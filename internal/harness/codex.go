package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

func init() { Register(codex{}) }

// codex drives OpenAI's Codex CLI (`codex exec`). Divergences from Claude Code,
// per the spike, that this adapter has to absorb:
//   - Permissions are two axes: --sandbox {read-only|workspace-write|...} and
//     --ask-for-approval {untrusted|on-request|never}. "edits auto, shell gated"
//     ≈ `-s workspace-write -a never`.
//   - Output convention inverts: final message on stdout, events on stderr,
//     unless --json (then a JSONL event stream on stdout).
//   - No stable limit exit code and rate_limits is null in exec --json, so limit
//     detection is fuzzy text-matching and there is nothing to introspect.
//   - Invocation.MCPConfigPath is DELIBERATELY UNUSED. codex has no per-run MCP
//     flag and no reader for Claude's `.mcp.json` schema; it takes its servers
//     from `[mcp_servers]` in config.toml under CODEX_HOME, which ConfigDir
//     already pins. Saying so here because the field is on every Invocation and
//     silently dropping it read as wiring that was never there: `doctor`'s
//     workdir check passed a codex workdir green on the strength of an .mcp.json
//     no codex session would ever see (CLA-263). doctor now states the exclusion
//     rather than implying the wiring - see cli.mcpConfigReachesSession.
type codex struct{}

func (codex) Name() string { return "codex" }

// codexArgs maps an Invocation to `codex exec`'s CLI dialect. Split out so the
// argument shape is unit-testable without executing anything - the parity with
// opencodeArgs.
//
// The prompt goes LAST, behind a `--` terminator. It is a bare positional, so a
// prompt that is itself a flag token would be parsed as a flag and the session
// would run with no message at all (or read one from stdin). Nothing from the
// BACKLOG can reach argv - Invocation.Prompt is set once from the config's
// `prompt` (loop.go), there is no --prompt flag, and the backlog client decodes
// counts rather than strings - so this hardens the config-file path (the CLA-260
// class), not a live injection hole.
//
// Putting every flag AHEAD of the terminator is the other half. The pinned
// posture used to sit after the positional and survive only by last-wins; now
// nothing follows the prompt, so there is no ordering left to reason about.
func codexArgs(in Invocation) []string {
	if in.Probe {
		return []string{"exec", "--json", "--sandbox", "read-only", "--ask-for-approval", "never", "--", "."}
	}
	args := []string{"exec", "--json", "--sandbox", "workspace-write", "--ask-for-approval", "never"}
	if in.Model != "" {
		args = append(args, "-m", in.Model)
	}
	return append(args, "--", in.Prompt)
}

func (c codex) Invoke(ctx context.Context, in Invocation) (Result, error) {
	cmd := exec.CommandContext(ctx, "codex", codexArgs(in)...)
	if in.WorkDir != "" {
		cmd.Dir = in.WorkDir
	}
	cmd.Env = append(os.Environ(), in.Env...)
	if in.ConfigDir != "" {
		cmd.Env = append(cmd.Env, "CODEX_HOME="+in.ConfigDir)
	}

	// Capture for parsing, and tee live to the console when one is set (the JSONL
	// event stream — a readable renderer is a TODO; raw is honest live output).
	var stdout, stderr bytes.Buffer
	if in.Console != nil && !in.Probe {
		cmd.Stdout = io.MultiWriter(&stdout, in.Console)
		cmd.Stderr = io.MultiWriter(&stderr, in.Console)
	} else {
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
	}
	runErr := cmd.Run()

	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if ee, ok := runErr.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
	} else if runErr != nil {
		return res, runErr
	}
	c.parse(&res)
	return res, nil
}

// codexEvent is one line of the `codex exec --json` JSONL stream. The schema is
// experimental/underdocumented, so this captures only the fields we need and
// tolerates everything else. Field names follow the documented shape (the
// capability spike); unknown/missing fields are simply skipped.
type codexEvent struct {
	Type            string      `json:"type"`
	Usage           *codexUsage `json:"usage"` // on turn.completed
	InputTokens     *int        `json:"input_tokens"`
	CachedInput     *int        `json:"cached_input_tokens"`
	OutputTokens    *int        `json:"output_tokens"`
	ReasoningTokens *int        `json:"reasoning_output_tokens"`
	Text            string      `json:"text"`
	Item            *codexItem  `json:"item"`
}

type codexUsage struct {
	InputTokens     int `json:"input_tokens"`
	CachedInput     int `json:"cached_input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	ReasoningTokens int `json:"reasoning_output_tokens"`
}

type codexItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// text returns the assistant message text this event carries, if any — captured
// best-effort, preferring items that look like agent messages over tool output.
func (ev codexEvent) text() string {
	if ev.Item != nil && ev.Item.Text != "" {
		if ev.Item.Type == "" || strings.Contains(ev.Item.Type, "message") || strings.Contains(ev.Item.Type, "agent") {
			return ev.Item.Text
		}
	}
	if ev.Text != "" && strings.Contains(ev.Type, "message") {
		return ev.Text
	}
	return ""
}

// parse walks the JSONL stream to fill FinalMessage and Tokens (for the Budget).
func (codex) parse(res *Result) {
	var (
		lastText                string
		in, cached, out, reason int
		sawUsage                bool
	)
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev codexEvent
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue // partial/non-JSON line — skip
		}
		// Take the LAST usage-bearing event as the session total — never sum.
		// Confirmed against codex 0.144.6 (rust-v0.144.6): `codex exec --json`
		// emits one `turn.completed` per turn whose top-level `usage` is the
		// CUMULATIVE session running total (built from `total_token_usage`), not a
		// per-turn delta — so summing would double-count. Unlike the opencode
		// adapter, which sums per-step deltas, here we keep only the final total.
		// (`token_count` is a separate rollout/app-server stream not on --json
		// stdout; its top-level fallback below is harmless defensive parsing.)
		switch {
		case ev.Usage != nil:
			in, cached, out, reason = ev.Usage.InputTokens, ev.Usage.CachedInput, ev.Usage.OutputTokens, ev.Usage.ReasoningTokens
			sawUsage = true
		case ev.InputTokens != nil || ev.OutputTokens != nil:
			in, cached, out, reason = deref(ev.InputTokens), deref(ev.CachedInput), deref(ev.OutputTokens), deref(ev.ReasoningTokens)
			sawUsage = true
		}
		if t := ev.text(); t != "" {
			lastText = t
		}
	}
	res.FinalMessage = lastText
	if sawUsage {
		// Exclude cached input (discounted reads); reasoning counts as output spend.
		res.Tokens = in + out + reason
		res.Raw = map[string]any{
			"input_tokens": in, "cached_input_tokens": cached,
			"output_tokens": out, "reasoning_output_tokens": reason,
		}
	}
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// codexErrorText collects the harness's OWN diagnostic text — all of stderr, the
// stdout lines that are not events at all, and the events that carry an `error` or
// announce a failure — so a limit or transient scan never reads the agent's
// narration.
//
// Both scans used to run over the whole of Stdout+Stderr, and under `--json`
// stdout is the event stream: the agent's messages, and its tool output, which
// includes the verbatim MCP response to `claim_task`. A backlog task whose body
// merely said "usage limit" therefore reported a subscription cap that never
// happened, and the loop paused and re-spawned the same paid session on a cycle no
// budget ceiling stopped (CLA-258). Same defect, same fix, as opencodeErrorText.
//
// Non-JSON stdout lines are kept deliberately: every word the agent says arrives
// inside an event, so a bare line is the CLI talking — which is how codex reports a
// cap it has no typed event for.
func codexErrorText(res Result) string {
	var b strings.Builder
	b.WriteString(res.Stderr)
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "{") {
			b.WriteByte('\n')
			b.WriteString(line)
			continue
		}
		var ev struct {
			Type  string          `json:"type"`
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		hasError := len(ev.Error) > 0 && string(ev.Error) != "null"
		if !hasError && !strings.Contains(ev.Type, "error") && !strings.Contains(ev.Type, "failed") {
			continue
		}
		// The TYPE and the ERROR member, never the whole line: an event may carry a
		// failure alongside a sibling field holding the failed command's output, and
		// that output is the agent's business. codexTransientRe matches a bare "rate
		// limit", so there is no anchoring here to fall back on.
		b.WriteByte('\n')
		b.WriteString(ev.Type)
		if hasError {
			b.WriteByte('\n')
			b.Write(ev.Error)
		}
	}
	return b.String()
}

func (codex) DetectLimit(res Result) Limit {
	// The subscription usage cap (the long-pause case) only. An API-level 429 /
	// "rate limit" / "too many requests" is a transient blip handled by IsTransient,
	// NOT this. Codex exposes no structured reset, so ResetAt stays zero and the
	// loop leans on interval probing.
	blob := strings.ToLower(codexErrorText(res))
	for _, needle := range []string{"usage limit", "weekly limit", "session limit"} {
		if strings.Contains(blob, needle) {
			return Limit{Limited: true, Reason: "usage_limit"}
		}
	}
	return Limit{}
}

// codexTransientRe: API-level rate limits (429/too many requests) and server/
// network blips are retryable; the subscription cap (DetectLimit) is not.
var codexTransientRe = regexp.MustCompile(`(?i)"status(code)?": ?(408|429|5\d\d)` +
	`|overloaded|too many requests|rate limit` +
	`|connection error|fetch failed|econnreset|econnrefused|etimedout|eai_again|socket hang up|network (error|timeout)`)

func (codex) IsTransient(res Result) bool {
	return codexTransientRe.MatchString(codexErrorText(res))
}

// Diagnostic returns the same scoped text IsTransient judged. See the claude
// adapter's Diagnostic for why the scope must match exactly.
func (codex) Diagnostic(res Result) string { return codexErrorText(res) }

func (c codex) Probe(ctx context.Context, in Invocation) (ProbeResult, error) {
	in.Probe = true
	res, err := c.Invoke(ctx, in)
	// Spend is taken off the Result on every path — see claude.Probe for why.
	out := ProbeResult{Tokens: res.Tokens, CostUSD: res.CostUSD}
	if err != nil {
		return out, err
	}
	out.Limit = c.DetectLimit(res)
	return out, nil
}

func (codex) ReadUsage(context.Context, Invocation) (Usage, error) {
	// exec --json emits rate_limits: null; /status is TUI-only (openai/codex#14728).
	return Usage{}, ErrUsageUnsupported
}
