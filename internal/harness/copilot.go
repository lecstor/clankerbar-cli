package harness

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

func init() { Register(copilot{}) }

// copilot drives GitHub Copilot CLI (`copilot -p`) headlessly. Everything below
// is recorded against GitHub Copilot CLI 1.0.112 (the GA line; see
// docs/copilot-cli.md for the flags and event shapes this adapter relies on):
//
//   - `-p/--prompt` runs one non-interactive session and exits. `--output-format
//     json` turns stdout into a JSONL event stream (one object per line); the
//     final answer rides an `assistant.message` event and the session's usage a
//     `session.usage_checkpoint`.
//   - In `-p` mode a tool permission request that would prompt is DENIED, so the
//     posture is fail-closed before any rule is added; `--allow-tool` /
//     `--deny-tool` are what open the working set the driver needs. Deny rules
//     win over every allow, including --allow-all.
//   - `--additional-mcp-config @<file>` registers the operator's `mcpServers`
//     file for one session, which is why this adapter reads `mcp_config_path` in
//     Claude's schema: the shapes are the same object.
//   - COPILOT_HOME is the config/state dir (the COPILOT_CONFIG_DIR analogue);
//     auth lives in the OS keychain or an env token, so a pinned home is not a
//     pinned login.
//   - `--usage-output-file` also exists and reports AI credits; the JSONL
//     checkpoint carries the same figures, so no temp file is written.
//
// Billing is METERED since 2026-06-01: a monthly AI-credit allowance, then
// per-token overage. There is no rolling window to wait out, so every limit this
// adapter recognises is a Limit.Stop; the budget breaker is the control.
type copilot struct{}

func (copilot) Name() string { return "copilot" }

// copilot reads mcp_config_path in Claude's `mcpServers` schema, handed over
// with --additional-mcp-config rather than auto-discovered. A workspace
// .mcp.json is only auto-loaded once the folder is trusted, and this adapter
// never trusts a folder (that would also approve its skills, plugins and hooks);
// passing the file explicitly is what gives every session its clankerbar tools.
func (copilot) MCPConfigUse() MCPConfigUse { return MCPConfigUse{Schema: MCPConfigClaudeJSON} }

// copilotArgs maps an Invocation to the CLI dialect. Split out so the argument
// shape - and the fail-closed permission posture below - is unit-testable
// without executing anything.
func copilotArgs(in Invocation) []string {
	args := []string{"--output-format", "json", "--no-ask-user", "--no-auto-update", "--no-color",
		// The built-in GitHub MCP server is not in this adapter's allow policy,
		// so in -p mode every one of its tools would be offered to the model and
		// then denied. Drop it: a session's MCP servers are the operator's
		// mcp_config_path, and nothing else.
		"--disable-builtin-mcps",
	}
	if m := in.ModelArg(); m != "" {
		args = append(args, "--model", m)
	}
	if in.MCPConfigPath != "" {
		// `@path` is the documented file form. An absolute path is what
		// config.ResolveMCPConfig hands over; a relative one resolves against
		// the session working directory, which cmd.Dir pins.
		args = append(args, "--additional-mcp-config", "@"+in.MCPConfigPath)
	}
	for _, dir := range in.ExtraDirs {
		// Repeatable, one directory per flag: the CLI documents --add-dir as a
		// single-directory flag, and path verification (not the permission
		// rules) is what keeps file access to the workdir and these roots.
		args = append(args, "--add-dir", dir)
	}
	if in.Probe {
		// A probe is read-only by construction: no write and no shell grant,
		// plus explicit denies, because read-only shell commands normally run
		// without asking and a probe must do neither. Its one grant is the
		// cheap clankerbar read, so a probe also proves the MCP wiring.
		args = append(args, "--deny-tool", "write", "--deny-tool", "shell")
		if in.MCPConfigPath != "" {
			args = append(args, "--allow-tool", "clankerbar(get_backlog_summary)")
		}
		return append(args, "-p", copilotPrompt(in))
	}
	// The working set. Path-scoped kind(...) rules are optional-argument forms,
	// and a bare kind means "every request of this kind": the file boundary is
	// the CLI's own path verification (workdir + temp + --add-dir), which in -p
	// mode cannot prompt and so fails closed. `clankerbar` is the MCP server
	// name; a server the operator adds under another name stays unapproved.
	for _, rule := range []string{"read", "write", "shell", "clankerbar"} {
		args = append(args, "--allow-tool", rule)
	}
	// The prompt is the VALUE of -p, never a positional: a prompt that begins
	// with "-" cannot be read as a flag, and there is no subcommand position it
	// could be mistaken for.
	return append(args, "-p", in.Prompt)
}

// env builds the child environment, with the same PWD pin as every other
// adapter (CLA-441) and COPILOT_HOME as the config-dir override.
func (c copilot) env(in Invocation) []string {
	env := append(pinPWD(inheritEnv(os.Environ(), in.EnvAllowlist), in.WorkDir), in.Env...)
	if in.ConfigDir != "" {
		env = append(env, "COPILOT_HOME="+in.ConfigDir)
	}
	return env
}

func (c copilot) Invoke(ctx context.Context, in Invocation) (Result, error) {
	if err := refuseTurns("copilot", in); err != nil {
		return Result{}, err
	}
	// The wall-clock cap, on the exec context: Copilot has no turn flag, so
	// elapsed time is the only phase backstop this adapter can offer. Never on a
	// probe (a one-word liveness check is not a phase doing work).
	sctx := ctx
	cancel := context.CancelFunc(func() {})
	if in.MaxSessionWallClock > 0 && !in.Probe {
		sctx, cancel = context.WithTimeout(ctx, in.MaxSessionWallClock)
	}
	defer cancel()

	cmd := exec.CommandContext(sctx, "copilot", copilotArgs(in)...)
	setupProcessGroup(cmd)
	if in.WorkDir != "" {
		cmd.Dir = in.WorkDir
	}
	cmd.Env = c.env(in)
	// A cap that can hang is not a cap: os/exec waits for every writer of its
	// own pipe, and a bash-tool grandchild (or an MCP server) inheriting stdout
	// survives a direct-child kill. WaitDelay force-closes the pipes so Invoke
	// returns; the process-group Cancel below kills the descendants we can reach.
	if _, hasDeadline := sctx.Deadline(); hasDeadline {
		cmd.WaitDelay = 5 * time.Second
	}
	cmd.Cancel = func() error {
		killProcessGroup(cmd)
		return cmd.Process.Kill()
	}

	console := in.Console
	if console == nil {
		console = os.Stderr
	}
	tee := console
	if in.Probe {
		// Nothing is watching a probe; io.Discard rather than nil because the
		// shared claim observer writes diagnostics to this writer, and a nil
		// io.Writer panics.
		console, tee = io.Discard, nil
	}

	res := newSessionResult(in)
	var p copilotParse
	captured := newCapture(func(line []byte) { p.line(line, &res, console) })
	captured.attach(cmd, tee)
	runErr := cmd.Run()

	res.Stdout = captured.stdout.String()
	res.Stderr = captured.stderr.String()
	res.OutputDropped = captured.stdout.Dropped() + captured.stderr.Dropped()
	if captured.sink.Overran() {
		res.markUntrusted("a copilot event was longer than the line cap and was discarded, " +
			"so an unknown number of events never reached the parser: this session's token and cost " +
			"figures are incomplete")
	}
	p.finish(&res)

	// Our own deadline, not the caller's: a run-wide cancellation must not be
	// reported as the phase reaching its backstop. runErr conjunct because the
	// deadline is read after Run returned - a session that exited a hair before
	// it is not capped.
	timedOut := runErr != nil && sctx.Err() == context.DeadlineExceeded && ctx.Err() == nil
	if timedOut {
		res.markWallClockCapped()
		if console != nil {
			io.WriteString(console, "\n!! session outlived its wall-clock cap - ending it here; whatever it wrote is still in the worktree, uncommitted\n")
		}
	}

	if ee, ok := runErr.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
		res.ExitSignal = exitSignal(ee)
	} else if errors.Is(runErr, exec.ErrWaitDelay) && !timedOut {
		// The session exited on its own and a grandchild held the pipe open past
		// WaitDelay. Every event it emitted was consumed live, so this is a clean
		// end (the same arm opencode has, CLA-414).
	} else if runErr != nil && timedOut {
		res.ExitCode = -1
	} else if runErr != nil {
		return res, runErr
	}
	return res, nil
}

// copilotEvent is one line of `copilot --output-format json`. One flat struct
// covers every event type: the envelope is {type, data, id, timestamp} and any
// field a given type does not carry is simply absent.
type copilotEvent struct {
	Type string           `json:"type"`
	Data copilotEventData `json:"data"`
}

type copilotEventData struct {
	// session.start
	SessionID string `json:"sessionId"`

	// assistant.message
	Content      string               `json:"content"`
	Phase        string               `json:"phase"`
	ToolRequests []copilotToolRequest `json:"toolRequests"`

	// session.error / session.warning
	ErrorType string `json:"errorType"`
	Message   string `json:"message"`

	// tool.execution_start / tool.execution_complete
	ToolCallID    string             `json:"toolCallId"`
	MCPServerName string             `json:"mcpServerName"`
	MCPToolName   string             `json:"mcpToolName"`
	Arguments     json.RawMessage    `json:"arguments"`
	Success       *bool              `json:"success"`
	Error         json.RawMessage    `json:"error"`
	Result        *copilotToolResult `json:"result"`

	// session.mcp_server_status_changed
	ServerName string `json:"serverName"`
	Status     string `json:"status"`

	// session.usage_checkpoint
	TotalNanoAiu  int64              `json:"totalNanoAiu"`
	TotalPremiums int                `json:"totalPremiumRequests"`
	Accounting    *copilotAccounting `json:"accountingSnapshot"`
}

type copilotToolRequest struct {
	ToolCallID    string          `json:"toolCallId"`
	Name          string          `json:"name"`
	Arguments     json.RawMessage `json:"arguments"`
	MCPServerName string          `json:"mcpServerName"`
	MCPToolName   string          `json:"mcpToolName"`
}

type copilotToolResult struct {
	Content string `json:"content"`
}

// copilotAccounting is the session's cumulative usage snapshot. The counters are
// INCLUSIVE by construction in one place only: `tokenDetails` is already
// disjoint (its input excludes cache reads and writes - a real session reported
// input 15811 + cache_read 29824 against model usage.inputTokens 45635), so the
// four buckets are copied and nothing is subtracted.
type copilotAccounting struct {
	TotalNanoAiu int64          `json:"totalNanoAiu"`
	TokenDetails *copilotTokens `json:"tokenDetails"`
	ModelMetrics map[string]struct {
		Usage struct {
			InputTokens      int `json:"inputTokens"`
			OutputTokens     int `json:"outputTokens"`
			CacheReadTokens  int `json:"cacheReadTokens"`
			CacheWriteTokens int `json:"cacheWriteTokens"`
			ReasoningTokens  int `json:"reasoningTokens"`
		} `json:"usage"`
	} `json:"modelMetrics"`
}

type copilotTokens struct {
	Input      struct{ TokenCount int } `json:"input"`
	Output     struct{ TokenCount int } `json:"output"`
	CacheRead  struct{ TokenCount int } `json:"cache_read"`
	CacheWrite struct{ TokenCount int } `json:"cache_write"`
}

// copilotParse accumulates the session's final message, usage and clankerbar
// observation as the stream arrives. Live rather than a walk over saved output,
// so trimming the retained text cannot cost the budget a token (CLA-262).
type copilotParse struct {
	lastText  string
	finalText string

	sawUsage bool
	tokens   TokenUsage
	nanoAiu  int64
	premium  int
	reason   int

	toolNames map[string]string
	mcp       map[string]string
}

func (p *copilotParse) line(line []byte, res *Result, console io.Writer) {
	trimmed := strings.TrimSpace(string(line))
	if !strings.HasPrefix(trimmed, "{") {
		return
	}
	var ev copilotEvent
	if json.Unmarshal([]byte(trimmed), &ev) != nil {
		return // partial/non-JSON line - skip
	}
	d := ev.Data
	switch {
	case ev.Type == "session.start":
		if d.SessionID != "" {
			p.raw(res)["session_id"] = d.SessionID
		}
	case ev.Type == "assistant.message":
		if d.Content != "" {
			p.lastText = d.Content
			if d.Phase == "final_answer" {
				p.finalText = d.Content
			}
		}
		for _, req := range d.ToolRequests {
			p.noteRequest(req.ToolCallID, req.Name, req.Arguments, req.MCPServerName, req.MCPToolName, res)
		}
	case ev.Type == "tool.execution_start":
		p.noteRequest(d.ToolCallID, "", d.Arguments, d.MCPServerName, d.MCPToolName, res)
	case ev.Type == "tool.execution_complete":
		p.noteResult(d, res, console)
	case ev.Type == "session.mcp_server_status_changed":
		if d.ServerName != "" {
			if p.mcp == nil {
				p.mcp = map[string]string{}
			}
			status := d.Status
			if status == "" {
				status = "unknown"
			}
			p.mcp[d.ServerName] = status
		}
	case ev.Type == "session.usage_checkpoint":
		p.noteUsage(d)
	}
}

// noteUsage keeps the LAST checkpoint, never a sum: the CLI's counters are
// cumulative session totals (a real session's two model calls both appear in one
// final snapshot), so summing would double-count. A checkpoint that carries no
// accounting snapshot still counts as a REPORT of zero - UsageReported answers
// "did the CLI report?", not "was the report large?".
func (p *copilotParse) noteUsage(d copilotEventData) {
	p.sawUsage = true
	if d.TotalNanoAiu != 0 {
		p.nanoAiu = d.TotalNanoAiu
	}
	p.premium = d.TotalPremiums
	a := d.Accounting
	if a == nil {
		return
	}
	if a.TotalNanoAiu != 0 {
		p.nanoAiu = a.TotalNanoAiu
	}
	p.reason = 0
	for _, m := range a.ModelMetrics {
		p.reason += m.Usage.ReasoningTokens
	}
	if a.TokenDetails != nil {
		p.tokens = TokenUsage{
			Input:      a.TokenDetails.Input.TokenCount,
			Output:     a.TokenDetails.Output.TokenCount,
			CacheRead:  a.TokenDetails.CacheRead.TokenCount,
			CacheWrite: a.TokenDetails.CacheWrite.TokenCount,
		}
		return
	}
	// Older/partial snapshots carry only modelMetrics, whose inputTokens is the
	// INCLUSIVE figure (fresh + cache). Subtract the cache buckets so the four
	// buckets stay disjoint, as codex's are mapped.
	var in, out, cr, cw int
	for _, m := range a.ModelMetrics {
		in += m.Usage.InputTokens
		out += m.Usage.OutputTokens
		cr += m.Usage.CacheReadTokens
		cw += m.Usage.CacheWriteTokens
	}
	p.tokens = TokenUsage{
		Input:      max(0, in-cr-cw),
		Output:     out,
		CacheRead:  cr,
		CacheWrite: cw,
	}
}

// noteRequest feeds one tool REQUEST to the shared observer under the canonical
// clankerbar name, so claim/settle/delivery observation is the same code path
// claude and opencode use. Copilot names an MCP tool `clankerbar-<tool>` and
// reports its server and tool separately; `mcp__clankerbar__<tool>` is the
// namespace the shared observer keys on.
func (p *copilotParse) noteRequest(id, name string, args json.RawMessage, mcpServer, mcpTool string, res *Result) {
	if id == "" {
		return
	}
	canonical := ""
	if mcpServer == "clankerbar" && mcpTool != "" {
		canonical = "mcp__clankerbar__" + mcpTool
	} else if mcpServer == "" && mcpTool == "" && isClankerbarTool(name) {
		// Defensive: a future CLI that reports the canonical name directly.
		canonical = name
	}
	if canonical == "" {
		return
	}
	if p.toolNames == nil {
		p.toolNames = map[string]string{}
	}
	p.toolNames[id] = canonical
	noteToolUse(canonical, id, args, res)
}

// noteResult resolves the request's meaning on its result. A missing `success`
// with no error is read as success: the field is documented on failures, and
// treating an absent flag as failure would drop every claim the plane accepted.
func (p *copilotParse) noteResult(d copilotEventData, res *Result, console io.Writer) {
	if d.ToolCallID == "" {
		return
	}
	// A start event normally armed this already; if it did not, nothing about
	// this call is known and there is no claim to settle.
	if _, known := p.toolNames[d.ToolCallID]; !known {
		return
	}
	delete(p.toolNames, d.ToolCallID)
	isError := (d.Success != nil && !*d.Success) || (len(d.Error) > 0 && string(d.Error) != "null")
	var content json.RawMessage
	if d.Result != nil && d.Result.Content != "" {
		content, _ = json.Marshal(d.Result.Content)
	}
	noteToolResult(res, d.ToolCallID, isError, content, console)
}

func (p *copilotParse) raw(res *Result) map[string]any {
	if res.Raw == nil {
		res.Raw = map[string]any{}
	}
	return res.Raw
}

// finish writes what the stream added up to onto res, PRESERVING the claim and
// reports the shared observer recorded while it ran.
func (p *copilotParse) finish(res *Result) {
	if p.finalText != "" {
		res.FinalMessage = p.finalText
	} else {
		res.FinalMessage = p.lastText
	}
	if len(p.mcp) > 0 {
		p.raw(res)["mcp_servers"] = p.mcp
	}
	if !p.sawUsage {
		return
	}
	res.UsageReported = true
	res.Usage = p.tokens
	res.Tokens = p.tokens.Total()
	raw := p.raw(res)
	raw["ai_credits"] = float64(p.nanoAiu) / 1e9
	raw["premium_requests"] = p.premium
	raw["reasoning_tokens"] = p.reason
}

// copilotErrorText collects the CLI's OWN diagnostic text: all of stderr, the
// stdout lines that are not events, and the events that carry an error. The
// agent's narration (assistant.message content) and any tool output are
// EXCLUDED: a backlog task body quoting "Plan: 100% used" must not stop the run
// on a limit that never happened (the CLA-258 class). warnings are excluded too,
// deliberately - the CLI's 75%-used quota notice is a warning, and it is not a
// limit.
func copilotErrorText(res Result) string {
	return res.scan(scanErrorText, func() string { return buildCopilotErrorText(res) })
}

func buildCopilotErrorText(res Result) string {
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
		var ev copilotEvent
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if !strings.Contains(ev.Type, "error") && len(ev.Data.Error) == 0 {
			continue
		}
		b.WriteByte('\n')
		b.WriteString(ev.Type)
		if ev.Data.Message != "" {
			b.WriteByte('\n')
			b.WriteString(ev.Data.Message)
		}
		if len(ev.Data.Error) > 0 && string(ev.Data.Error) != "null" {
			b.WriteByte('\n')
			b.Write(ev.Data.Error)
		}
	}
	return b.String()
}

// copilotLimitRe recognises the CLI's documented exhaustion statements
// (`copilot help billing`, `copilot help limits`): the plan's monthly credit
// allowance running out, the session AI-credit cap, and the account-level
// session/weekly/model rate limits.
//
// Deliberately NOT the bare word "usage limit" or "quota": the CLI's warning at
// 75% of a weekly allowance contains both and is not a limit.
var copilotLimitRe = regexp.MustCompile(`(?i)` +
	`plan:\s*100%\s*used` +
	`|out of (ai )?credits|no (ai )?credits (left|remaining)|(ai )?credits? (exhausted|have run out)` +
	`|(ai )?credit limit (reached|exceeded)` +
	`|quota (exceeded|exhausted)` +
	`|session limit reached` +
	`|reached your weekly rate limit|hit your session rate limit|hit the rate limit for this model|hit your rate limit`)

// copilotRateLimitRe names the window a rate-limit statement refers to, for the
// log line only: every recognised limit is a Stop here (metered billing, no
// window to wait out).
var copilotRateLimitRe = regexp.MustCompile(`(?i)weekly rate limit`)

func (copilot) DetectLimit(res Result) Limit {
	text := copilotErrorText(res)
	if !copilotLimitRe.MatchString(text) {
		return Limit{}
	}
	// Metered billing (GitHub moved Copilot to per-token AI-credit billing on
	// 2026-06-01): the allowance refills monthly and overage is paid, so there is
	// no rolling window the supervised wait could poll for. Stop cleanly and let
	// the operator top up / resume, which is docs/limit-stop.md's flow.
	reason := "budget_exhausted"
	window := "monthly"
	if copilotRateLimitRe.MatchString(text) {
		reason = "usage_limit"
		window = "weekly"
	}
	return Limit{Limited: true, Stop: true, Reason: reason, Window: window}
}

// copilotTransientRe: API-level rate limits (429/too many requests) and server/
// network blips are retryable. The subscription/credit cap (DetectLimit) is not,
// and DetectLimit is consulted first by the driver.
var copilotTransientRe = regexp.MustCompile(`(?i)` +
	`\b(?:status|code|http)[ :=]*(408|429|5\d\d)\b` +
	`|\b429\b|too many requests|rate limit` +
	`|overloaded|server error|internal server error|service unavailable|bad gateway|gateway timeout` +
	`|connection reset|econnreset|econnrefused|etimedout|eai_again|socket hang up|network (error|timeout|is unreachable)` +
	`|fetch failed|timed out|temporarily unavailable|try again later`)

func (copilot) IsTransient(res Result) bool {
	return copilotTransientRe.MatchString(copilotErrorText(res))
}

// Never a guess: pattern-driven throughout, so an unrecognised exit is a stop.
func (copilot) IsUnclassifiedTransient(Result) bool { return false }

// No turn cap: Copilot takes no turn flag, so Invocation.MaxTurns never reaches
// it. The wall-clock cap below is the phase backstop instead.
func (copilot) TurnCapped(Result) bool { return false }

// No mid-stream token ceiling: nothing here kills the process on cumulative
// usage, so no exit can be attributed to one.
func (copilot) TokenCeilingHit(Result) bool { return false }

func (copilot) WallClockCapped(res Result) bool {
	if res.Raw == nil {
		return false
	}
	r, _ := res.Raw[TerminalReasonKey].(string)
	return r == wallClockReason
}

// No zero-usage-unknown signature: it is opencode's step_finish shape, and
// copilot's checkpoint reports the session total rather than per-step figures.
func (copilot) ZeroUsageUnknown(Result) bool { return false }

// copilot observes the session's clankerbar tool calls through the shared
// observer (noteToolUse/noteToolResult), so Result.Claim, Reports, Settled and
// ClankerbarCalls are all populated and a phased config is allowed.
//
// ReportsCost is false for a harder reason than "not implemented": Copilot
// reports AI CREDITS (and premium requests), never money. One credit's dollar
// value depends on the plan and on whether the call is inside the included
// allowance, so mapping it to CostUSD would invent a price. `budget.max_cost_usd`
// is therefore inert under copilot, and `budget.max_tokens` - fed by the
// checkpoint's token counts - is the breaker; doctor says so (CLA-288).
func (copilot) Capabilities() Capabilities {
	return Capabilities{
		TracksClaims:            true,
		HonoursSessionWallClock: true,
	}
}

func (copilot) Diagnostic(res Result) string { return copilotErrorText(res) }

func (c copilot) Probe(ctx context.Context, in Invocation) (ProbeResult, error) {
	in.Probe = true
	res, err := c.Invoke(ctx, in)
	out := ProbeResult{Tokens: res.Tokens, CostUSD: res.CostUSD}
	if err != nil {
		return out, err
	}
	return probeVerdict(out, res, c.DetectLimit)
}

func (copilot) ReadUsage(context.Context, Invocation) (Usage, error) {
	// /usage is TUI-only; the JSONL checkpoint reports this session, not the
	// account's window.
	return Usage{}, ErrUsageUnsupported
}

// copilotPrompt is the probe's prompt, kept beside the args builder so the
// read-only shape is asserted through the same path production uses. A probe
// with an MCP config asks for the cheap clankerbar read: it proves the custom
// server really connected in a headless run, which is the one part of the wiring
// no unit test can.
func copilotPrompt(in Invocation) string {
	if in.Probe {
		if in.MCPConfigPath != "" {
			return "Call the clankerbar MCP tool get_backlog_summary once and reply with only its contents. Do not call any other tool."
		}
		return "."
	}
	return in.Prompt
}
