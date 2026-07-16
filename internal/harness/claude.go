package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"time"
)

func init() { Register(claude{}) }

// claude drives Claude Code (`claude -p`). Notable, per the capability spike:
//   - `.mcp.json` is NOT auto-discovered in -p mode; pass --mcp-config explicitly.
//   - --output-format json exposes a structured `terminal_reason:"usage_limit"`
//     on a limit hit — a cleaner signal than scraping stderr.
//   - CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=0 keeps the session alive while
//     delegated subagents are still running (the drain dispatches to subagents).
type claude struct{}

func (claude) Name() string { return "claude" }

func (c claude) Invoke(ctx context.Context, in Invocation) (Result, error) {
	var args []string
	if in.Probe {
		// Cheapest possible request: a trivial prompt, no tools. Still-limited
		// returns the limit message at ~zero cost; success means the gate lifted.
		args = []string{"-p", ".", "--output-format", "json", "--permission-mode", "dontAsk", "--allowedTools", ""}
	} else {
		args = []string{"-p", in.Prompt, "--output-format", "json", "--permission-mode", "acceptEdits"}
		if in.Model != "" {
			args = append(args, "--model", in.Model)
		}
		if in.MCPConfigPath != "" {
			args = append(args, "--mcp-config", in.MCPConfigPath, "--strict-mcp-config")
		}
	}

	cmd := exec.CommandContext(ctx, "claude", args...)
	if in.WorkDir != "" {
		cmd.Dir = in.WorkDir
	}
	cmd.Env = append(os.Environ(), in.Env...)
	// Never truncate the session while subagents/background work run.
	cmd.Env = append(cmd.Env, "CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=0")

	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if ee, ok := runErr.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
	} else if runErr != nil {
		return res, runErr // couldn't launch claude at all (not on PATH, etc.)
	}
	c.parse(&res)
	return res, nil
}

// parse reads `claude --output-format json` — a single JSON object.
// TODO: support --output-format stream-json for live progress.
func (claude) parse(res *Result) {
	var p struct {
		IsError        bool    `json:"is_error"`
		Result         string  `json:"result"`
		TerminalReason string  `json:"terminal_reason"`
		TotalCostUSD   float64 `json:"total_cost_usd"`
		Usage          struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &p); err != nil {
		return // not JSON (plain text / stream) — leave raw for the caller
	}
	res.FinalMessage = p.Result
	res.CostUSD = p.TotalCostUSD
	res.Tokens = p.Usage.InputTokens + p.Usage.OutputTokens
	res.Raw = map[string]any{"terminal_reason": p.TerminalReason, "is_error": p.IsError}
}

func (claude) DetectLimit(res Result) Limit {
	reason, _ := res.Raw["terminal_reason"].(string)
	if reason == "usage_limit" || strings.Contains(res.Stdout+res.Stderr, "hit your") {
		return Limit{
			Limited: true,
			ResetAt: parseClaudeReset(res.Stdout + res.Stderr),
			Reason:  "usage_limit",
		}
	}
	return Limit{}
}

func (c claude) Probe(ctx context.Context, in Invocation) (Limit, error) {
	in.Probe = true
	res, err := c.Invoke(ctx, in)
	if err != nil {
		return Limit{}, err
	}
	return c.DetectLimit(res), nil
}

func (claude) ReadUsage(context.Context, Invocation) (Usage, error) {
	// /usage is TTY-only; --output-format json carries no remaining quota; no
	// local file persists it (see the memo, and anthropics/claude-code#32796).
	return Usage{}, ErrUsageUnsupported
}

// parseClaudeReset extracts the reset time from a message like
// "You've hit your session limit · resets 9:40pm (Europe/Madrid)". The format is
// timezone- and locale-fragile, so this is deliberately best-effort: on any doubt
// it returns the zero time and the loop falls back to interval polling.
//
// TODO: parse the "9:40pm" clock time against the "(Europe/Madrid)" zone and
// roll to tomorrow when it's already past — mirroring loop.sh's date logic.
func parseClaudeReset(string) time.Time { return time.Time{} }
