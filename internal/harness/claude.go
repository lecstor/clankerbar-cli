package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strconv"
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
//
//	You've hit your session limit · resets 9:40pm (Europe/Madrid)
//	You've hit your weekly limit · resets Sunday 12:00am
//
// The reset is only an upper bound for the supervised wait (the loop still polls
// for an early reset), so this is deliberately best-effort: on any doubt it
// returns the zero time and the loop falls back to interval polling.
func parseClaudeReset(s string) time.Time { return parseClaudeResetAt(s, time.Now()) }

// resetRe captures: [weekday] hour [:minute] [am/pm] [(timezone)].
var resetRe = regexp.MustCompile(`(?i)resets\s+(?:(mon|tue|wed|thu|fri|sat|sun)[a-z]*\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?(?:\s*\(([^)]+)\))?`)

// parseClaudeResetAt is parseClaudeReset with an injectable "now" for testing.
func parseClaudeResetAt(s string, now time.Time) time.Time {
	m := resetRe.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}
	}
	hour, err := strconv.Atoi(m[2])
	if err != nil {
		return time.Time{}
	}
	minute := 0
	if m[3] != "" {
		minute, _ = strconv.Atoi(m[3])
	}
	switch strings.ToLower(m[4]) {
	case "pm":
		if hour != 12 {
			hour += 12
		}
	case "am":
		if hour == 12 {
			hour = 0
		}
	}
	if hour > 23 || minute > 59 {
		return time.Time{}
	}

	// Interpret the clock time in the stated zone when present, else locally.
	loc := time.Local
	if tz := strings.TrimSpace(m[5]); tz != "" {
		if l, lerr := time.LoadLocation(tz); lerr == nil {
			loc = l
		}
	}
	now = now.In(loc)
	target := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, loc)

	if wd := strings.ToLower(m[1]); wd != "" {
		// Weekly: advance to the next occurrence of the named weekday in the future.
		want, ok := weekdayNum[wd]
		if !ok {
			return time.Time{}
		}
		for i := 0; i < 8; i++ {
			if target.Weekday() == want && target.After(now) {
				return target
			}
			target = target.AddDate(0, 0, 1)
		}
		return time.Time{}
	}
	// Session: a clock time already past today means tomorrow.
	if !target.After(now) {
		target = target.AddDate(0, 0, 1)
	}
	return target
}

var weekdayNum = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}
