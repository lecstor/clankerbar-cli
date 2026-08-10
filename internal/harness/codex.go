package harness

import (
	"context"
	"encoding/json"
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
type codex struct{}

func (codex) Name() string { return "codex" }

func (c codex) Invoke(ctx context.Context, in Invocation) (Result, error) {
	var args []string
	if in.Probe {
		args = []string{"exec", ".", "--json", "--sandbox", "read-only", "--ask-for-approval", "never"}
	} else {
		args = []string{"exec", in.Prompt, "--json", "--sandbox", "workspace-write", "--ask-for-approval", "never"}
		if in.Model != "" {
			args = append(args, "-m", in.Model)
		}
	}

	cmd := exec.CommandContext(ctx, "codex", args...)
	if in.WorkDir != "" {
		cmd.Dir = in.WorkDir
	}
	cmd.Env = append(os.Environ(), in.Env...)
	if in.ConfigDir != "" {
		cmd.Env = append(cmd.Env, "CODEX_HOME="+in.ConfigDir)
	}

	// Parse as the stream arrives, retain only a bounded tail of it for the text
	// scans, and tee live to the console when one is set (the JSONL event stream —
	// a readable renderer is a TODO; raw is honest live output).
	//
	// Parsing live rather than walking a saved copy afterwards is what makes the
	// cap safe: the usage figures are read out of events as they pass, so trimming
	// the retained text cannot cost the budget a single token (CLA-262).
	var p codexParse
	captured := newCapture(p.line)
	console := in.Console
	if in.Probe {
		console = nil
	}
	captured.attach(cmd, console)
	runErr := cmd.Run()

	res := captured.result("codex")
	p.finish(&res)
	if ee, ok := runErr.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
	} else if runErr != nil {
		return res, runErr
	}
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

// codexParse accumulates FinalMessage and Tokens (for the Budget) from the JSONL
// stream, ONE LINE AT A TIME as it arrives.
//
// Incremental rather than a walk over saved output, so the retained text can be
// capped without the token count depending on how much of the stream survived —
// see output.go.
type codexParse struct {
	lastText                string
	in, cached, out, reason int
	sawUsage                bool
}

func (p *codexParse) line(line []byte) {
	trimmed := strings.TrimSpace(string(line))
	if !strings.HasPrefix(trimmed, "{") {
		return
	}
	var ev codexEvent
	if json.Unmarshal([]byte(trimmed), &ev) != nil {
		return // partial/non-JSON line — skip
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
		p.in, p.cached, p.out, p.reason = ev.Usage.InputTokens, ev.Usage.CachedInput, ev.Usage.OutputTokens, ev.Usage.ReasoningTokens
		p.sawUsage = true
	case ev.InputTokens != nil || ev.OutputTokens != nil:
		p.in, p.cached, p.out, p.reason = deref(ev.InputTokens), deref(ev.CachedInput), deref(ev.OutputTokens), deref(ev.ReasoningTokens)
		p.sawUsage = true
	}
	if t := ev.text(); t != "" {
		p.lastText = t
	}
}

// finish writes what the stream added up to onto res.
func (p *codexParse) finish(res *Result) {
	res.FinalMessage = p.lastText
	if p.sawUsage {
		// Exclude cached input (discounted reads); reasoning counts as output spend.
		res.Tokens = p.in + p.out + p.reason
		res.Raw = map[string]any{
			"input_tokens": p.in, "cached_input_tokens": p.cached,
			"output_tokens": p.out, "reasoning_output_tokens": p.reason,
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
	return res.scan(scanErrorText, func() string { return buildCodexErrorText(res) })
}

func buildCodexErrorText(res Result) string {
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
	return probeVerdict(out, res, c.DetectLimit)
}

func (codex) ReadUsage(context.Context, Invocation) (Usage, error) {
	// exec --json emits rate_limits: null; /status is TUI-only (openai/codex#14728).
	return Usage{}, ErrUsageUnsupported
}
