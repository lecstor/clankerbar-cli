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

// codex drives OpenAI's Codex CLI (`codex exec`). Divergences from Claude Code, per the spike, that
// this adapter has to absorb: - Permissions are two axes: --sandbox {read-only|workspace-write|...}
// and --ask-for-approval {untrusted|on-request|never}.
type codex struct{}

func (codex) Name() string { return "codex" }

func (codex) MCPConfigUse() MCPConfigUse {
	return MCPConfigUse{
		Schema: MCPConfigUnused,
		Note: "the codex adapter does not pass mcp_config_path to its sessions - " +
			"their MCP servers come from [mcp_servers] in config.toml under the config dir (CODEX_HOME)",
	}
}

// codexArgs maps an Invocation to `codex exec`'s CLI dialect. The `--` terminator has two jobs: a
// prompt that looks like a flag would be parsed as one, and an exec subcommand (`resume`, …) would
// otherwise swallow the prompt as its argument.
func codexArgs(in Invocation) []string {
	if in.Probe {
		return []string{"exec", "--json", "--sandbox", "read-only", "--ask-for-approval", "never", "--", probePrompt}
	}
	// Invocation.ExtraDirs is deliberately NOT translated here: codex's sandbox has no per-invocation
	// flag for extra writable roots — its `workspace-write` scope is the cwd plus what
	// CODEX_HOME/config.toml's `[sandbox_workspace_write] writable_roots` names, which is
	// operator-owned local config like every other grant.
	args := []string{"exec", "--json", "--sandbox", "workspace-write", "--ask-for-approval", "never"}
	if m := in.ModelArg(); m != "" {
		args = append(args, "-m", m)
	}
	return append(args, "--", in.Prompt)
}

// env builds the child environment. Split out of Invoke - where it was three inline lines - so this
// adapter's environment is testable the way claude's and opencode's already were, which is what the
// CLA-441 PWD pin is asserted against: a fix "every adapter" has to carry needs every adapter's env
// to be reachable from a test.
func (c codex) env(in Invocation) []string {
	// Pin the child's PWD to the directory Invoke sets as cmd.Dir, dropping the daemon's inherited
	// value - see pinPWD.
	env := append(pinPWD(inheritEnv(os.Environ(), in.EnvAllowlist), in.WorkDir), in.Env...)
	if in.ConfigDir != "" {
		env = append(env, "CODEX_HOME="+in.ConfigDir)
	}
	return env
}

func (c codex) Invoke(ctx context.Context, in Invocation) (Result, error) {
	if err := refuseTurns("codex", in); err != nil {
		return Result{}, err
	}
	cmd, err := sessionCommand(ctx, in.Nice, "codex", codexArgs(in)...)
	if err != nil {
		return Result{}, err
	}
	if in.WorkDir != "" {
		cmd.Dir = in.WorkDir
	}
	cmd.Env = c.env(in)

	// Parse as the stream arrives, retain only a bounded tail of it for the text scans, and tee live
	// to the console when one is set (the JSONL event stream — a readable renderer is a TODO; raw is
	// honest live output). Parsing live rather than walking a saved copy afterwards is what makes the
	// cap safe: the usage figures are read out of events as they pass, so trimming the retained text
	// cannot cost the budget a single token.
	var p codexParse
	captured := newCapture(p.line)
	console := in.Console
	if in.Probe {
		console = nil
	}
	captured.attach(cmd, console)
	runErr := cmd.Run()

	// p.finish has already run, so res carries everything the stream announced before the failure —
	// the CLA-299 ordering: parse whatever arrived, THEN classify the run error.
	res := captured.result("codex")
	p.finish(&res)
	if ee, ok := runErr.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
		res.ExitSignal = exitSignal(ee)
	} else if runErr != nil {
		return res, runErr
	}
	return res, nil
}

// codexEvent is one line of the `codex exec --json` JSONL stream. The schema is
// experimental/underdocumented, so this captures only the fields we need and tolerates everything
// else.
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

// text returns the assistant message text this event carries, if any — captured best-effort,
// preferring items that look like agent messages over tool output.
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

// codexParse accumulates FinalMessage and Tokens (for the Budget) from the JSONL stream, ONE LINE
// AT A TIME as it arrives.
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
	// Recorded whatever the figures come to: the driver bounds attempts that reported NOTHING, and a
	// turn.completed carrying zeros still reported.
	res.UsageReported = p.sawUsage
	if p.sawUsage {
		// Exclude cached input (discounted reads); reasoning counts as output spend.
		res.Tokens = p.in + p.out + p.reason
		// The normalised breakdown. codex's counters are INCLUSIVE — cached_input_tokens is a subset of
		// input_tokens, reasoning_output_tokens a subset of output_tokens — so the subsets are
		// subtracted out to leave buckets that are disjoint (see Usage's own comment).
		res.Usage = TokenUsage{
			Input:     max(0, p.in-p.cached),
			Output:    max(0, p.out-p.reason),
			Reasoning: p.reason,
			CacheRead: p.cached,
		}
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

// codexErrorText collects the harness's OWN diagnostic text — all of stderr, the stdout lines
// that are not events at all, and the events that carry an `error` or announce a failure — so a
// limit or transient scan never reads the agent's narration. A backlog task whose body merely said
// "usage limit" therefore reported a subscription cap that never happened, and the loop paused and
// re-spawned the same paid session on a cycle no budget ceiling stopped.
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
		// The TYPE and the ERROR member, never the whole line: an event may carry a failure alongside a
		// sibling field holding the failed command's output, and that output is the agent's business.
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
	// The subscription usage cap (the long-pause case) only. An API-level 429 / "rate limit" / "too
	// many requests" is a transient blip handled by IsTransient, NOT this.
	blob := strings.ToLower(codexErrorText(res))
	for _, needle := range []string{"usage limit", "weekly limit", "session limit"} {
		if strings.Contains(blob, needle) {
			return Limit{Limited: true, Reason: "usage_limit"}
		}
	}
	return Limit{}
}

// codexTransientRe: API-level rate limits (429/too many requests) and server/ network blips are
// retryable; the subscription cap (DetectLimit) is not.
var codexTransientRe = regexp.MustCompile(`(?i)"status(code)?": ?(408|429|5\d\d)` +
	`|overloaded|too many requests|rate limit` +
	`|connection error|fetch failed|econnreset|econnrefused|etimedout|eai_again|socket hang up|network (error|timeout)`)

func (codex) IsTransient(res Result) bool {
	return codexTransientRe.MatchString(codexErrorText(res))
}

// Never a guess: pattern-driven throughout, so an unrecognised exit is a stop. See claude's for the
// reasoning; opencode is the only adapter that guesses.
func (codex) IsUnclassifiedTransient(Result) bool { return false }

// No turn cap: Invocation.MaxTurns never reaches the CLI, so no exit can be attributed to one.
func (codex) TurnCapped(Result) bool { return false }

// No mid-stream token ceiling: codex's turn.completed usage is cumulative but the adapter never
// kills the process, so no exit of its can be attributed to one.
func (codex) TokenCeilingHit(Result) bool { return false }

// No wall-clock cap: this adapter runs its child on the caller's context alone, so nothing here
// ends a session on elapsed time.
func (codex) WallClockCapped(Result) bool { return false }

// No zero-usage-unknown marker: the CLA-398 quiet-death signature is opencode's step_finish shape,
// and codex reports no per-step usage the parser could sum.
func (codex) ZeroUsageUnknown(Result) bool { return false }

// This adapter does not populate Result.Claim — it does not watch the session's clankerbar tool
// calls at all — so the driver's handback, salvage and delivery check are all inert under it, and
// `phases` is refused for it in config.Validate rather than half-working. ReportsCost is false for
// a harder reason than "not implemented yet": `codex exec --json` reports tokens, never money, so
// there is nothing for the adapter to read.
func (codex) Capabilities() Capabilities { return Capabilities{} }

// Diagnostic returns the same scoped text IsTransient judged. See the claude adapter's Diagnostic
// for why the scope must match exactly.
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
