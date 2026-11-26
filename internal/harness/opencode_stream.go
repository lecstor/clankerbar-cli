package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// runSession executes ONE `opencode run` process end-to-end and parses its stream. A process with a
// private server attaches it for the life of the process, so a message is only ever posted to the
// server that is actually running.
func (o opencode) runSession(ctx context.Context, in Invocation, args []string, delivery *opencodeInbox) (Result, error) {

	// The per-session wall-clock cap, enforced on the exec context so the kill needs nothing from the
	// stream: opencode reports no turn count and takes no turn flag, so elapsed time is the only
	// backstop this adapter can offer a phase.
	sctx := ctx
	cancel := context.CancelFunc(func() {})
	if in.MaxSessionWallClock > 0 && !in.Probe {
		sctx, cancel = context.WithTimeout(ctx, in.MaxSessionWallClock)
	}
	defer cancel()

	workDir := opencodeWorkDir(in.WorkDir)
	if workDir != "" {
		in.WorkDir = workDir
	}
	env, err := o.env(in)
	if err != nil {
		return Result{}, err
	}

	runArgs := args
	runEnv := env
	var srv *opencodeServer
	// CLA-927: a NoBacklogMCP session has its clankerbar servers switched off in
	// the content layer, so there is nothing to wait for and no tools to lose a
	// race against. It keeps `--standalone` and starts immediately.
	if !in.Probe && !in.NoBacklogMCP {
		// Only a session with a clankerbar server gets the preflight. That server is resolved across the
		// config layers opencode actually loads: the named file when there is one, otherwise the global
		// config dir and the project-level files discovered from the instance directory.
		names, err := opencodeEffectiveMCPServers(in, env)
		if err != nil {
			return mcpPreflightFailure(sctx, ctx, err)
		}
		if len(names) > 0 {
			srv, err = startOpencodeServer(sctx, in.Nice, env, workDir)
			if err != nil {
				return mcpPreflightFailure(sctx, ctx, err)
			}
			defer srv.stop()
			if err := srv.waitForMCP(sctx, names, workDir); err != nil {
				return mcpPreflightFailure(sctx, ctx, err)
			}
			// The delivery target is this process's own server. Attach AFTER the MCP preflight so the target
			// is live only for a session whose tools are actually there, and defer the detach behind
			// srv.stop's own defer: LIFO runs detach FIRST, so the target is cleared before the server is
			// killed.
			if delivery != nil {
				delivery.attach(srv)
				defer delivery.detach(srv)
			}
			runArgs = withOpencodeServer(args, srv.url)
			// The run must authenticate to the server the adapter started, whatever an inherited or
			// configured OPENCODE_PASSWORD says.
			runEnv = append(append([]string{}, env...), "OPENCODE_PASSWORD="+srv.password)
		}
	}

	// CLA-739: the repeated-failing-tool-call kill cancels THIS context, a child of sctx, so the kill
	// reaches the run alone - never the server above, which the deferred stop tears down, and never
	// the caller.
	lctx, loopKill := context.WithCancel(sctx)
	defer loopKill()
	cmd, err := sessionCommand(lctx, in.Nice, "opencode", runArgs...)
	if err != nil {
		return Result{}, err
	}
	setupProcessGroup(cmd)
	if workDir != "" {
		cmd.Dir = workDir
	}
	cmd.Env = runEnv
	// A cap that can hang is not a cap. The capture points Stdout/Stderr at io.MultiWriter values, so
	// os/exec makes its own pipes and cmd.Run waits for every WRITER of them to close — and
	// CommandContext kills the direct child only.
	if _, hasDeadline := sctx.Deadline(); hasDeadline || ((in.MaxRepeatedToolFailures > 0 || in.MaxSessionIdle > 0) && !in.Probe) {
		cmd.WaitDelay = 5 * time.Second
	}

	cmd.Cancel = func() error {
		killProcessGroup(cmd)
		return cmd.Process.Kill()
	}

	// Parse as the stream arrives, retain only a bounded tail of it for the text scans, and tee live
	// to the console when one is set (the JSON event stream — a readable renderer is a TODO; raw is
	// honest live output).
	console := in.Console
	if in.Probe {
		console = nil
	}
	p := newOpencodeParse(in, console)
	defer p.closeEvents()
	p.kill = loopKill
	p.inbox = delivery
	p.armIdle(in)
	captured := newCapture(p.line)
	captured.attach(cmd, console)
	started := time.Now()
	runErr := cmd.Run()

	// The process is over, so its server must stop being a delivery target NOW: settleUsage below
	// reads the session export over HTTP (and can take seconds), and a message polled in that window
	// would be ACKed into a session with no process left to act on it.
	if delivery != nil && srv != nil {
		delivery.detach(srv)
	}

	// Disarmed BEFORE the verdict is read: a timer that lands as the process exits must not report a
	// kill for a session that had already finished.
	p.idle.stop()
	res := captured.result("opencode")
	p.finish(&res)
	if runErr == nil {
		res.IdleKilled = ""
	} else {
		res.IdleKilled = p.idleCause()
		res.IdleToolCall = res.IdleKilled != "" && p.idle.trippedWithOpenCall()
	}

	timedOut := runErr != nil && sctx.Err() == context.DeadlineExceeded && ctx.Err() == nil
	if timedOut {
		res.markWallClockCapped()
		if console != nil {
			fmt.Fprintf(console, "\n!! session outlived its wall-clock cap (%s) — ending it here; whatever it wrote is still in the worktree, uncommitted\n", in.MaxSessionWallClock)
		}
	}
	res.ToolLoop = p.observed.toolLoop.tripped()
	loopKilled := runErr != nil && res.ToolLoop != ""
	idleKilled := res.IdleKilled != ""

	if ee, ok := runErr.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
		res.ExitSignal = exitSignal(ee)
	} else if errors.Is(runErr, exec.ErrWaitDelay) && !timedOut && !loopKilled && !idleKilled {
		o.settleUsage(ctx, in, &res, started, in.ResumeSession)
		return res, nil
	} else if runErr != nil && !timedOut && !loopKilled && !idleKilled {
		return res, runErr
	} else if runErr != nil {
		res.ExitCode = -1
	}
	o.settleUsage(ctx, in, &res, started, in.ResumeSession)
	return res, nil
}

// opencodeEvent is one line of `opencode run --format json`. The stream is a sequence of typed
// events, each wrapping a message part; this captures only the fields the loop needs and tolerates
// the rest.
type opencodeEvent struct {
	Type  string              `json:"type"`
	Part  *opencodePart       `json:"part"`
	Error *opencodeErrorEvent `json:"error"`

	// SessionID is the opencode session every event of one run belongs to.
	SessionID string `json:"sessionID"`
}

// opencodeErrorEvent is the typed error 2.x puts on a {"type":"error"} event. The type is a stable
// dotted name ("provider.auth", "provider.rate-limit", "provider.no-route",
// "provider.invalid-output"), which classification keys on before it falls back to the word
// classes.
type opencodeErrorEvent struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Status  int    `json:"status"`
}

type opencodePart struct {
	Type   string          `json:"type"`
	Text   string          `json:"text"`
	Reason string          `json:"reason"`
	Tokens *opencodeTokens `json:"tokens"`
	// A POINTER for the same reason Tokens is one: a step reporting cost 0 has REPORTED, and the
	// driver's zero-spend bound counts silence, not zeroes.
	Cost *float64 `json:"cost"`

	// Tool parts, which arrive on a "tool_use" event. On 2.x the call id is `id` (1.x called it
	// `callID`) and the part's own id moves to `partID`.
	Tool   string             `json:"tool"`
	ID     string             `json:"id"`
	PartID string             `json:"partID"`
	State  *opencodeToolState `json:"state"`
}

// opencodeToolState is a tool call's lifecycle slot on its part. There is no `is_error` flag to
// read — the STATUS is the flag, which is why a status this adapter does not recognise must never
// be treated as success: {"state":{"status":"error","input":{...}, "error":"No tool named
// \"clankerbar_claim_task\" is currently available.
type opencodeToolState struct {
	Status   string          `json:"status"`
	Input    json.RawMessage `json:"input"`
	Output   string          `json:"output"`
	Error    string          `json:"error"`
	Metadata *struct {
		Metadata *struct {
			Truncated  bool   `json:"truncated"`
			OutputPath string `json:"outputPath"`
		} `json:"metadata"`
	} `json:"metadata"`
}

type opencodeTokens struct {
	Total     int `json:"total"`
	Input     int `json:"input"`
	Output    int `json:"output"`
	Reasoning int `json:"reasoning"`
	Cache     struct {
		Write int `json:"write"`
		Read  int `json:"read"`
	} `json:"cache"`
}

// opencodeParse accumulates FinalMessage, Tokens and CostUSD from the event stream, ONE LINE AT A
// TIME as it arrives. Because it sums, it has to see EVERY step — which is exactly why it runs on
// the live stream instead of over the retained tail (see Invoke).
type opencodeParse struct {
	lastText string
	// lastReason is the reason of the MOST RECENT step_finish — the session's final step, once the
	// stream ends. opencode writes "stop" when the session produced a final answer and "unknown" when
	// it died without one; the driver reads it off Result.Raw via FinishReasonKey.
	lastReason string
	// lastStepZeroUsage reports that the MOST RECENT step_finish's own usage summed to zero — no
	// tokens block, an all-zero tokens block, or no cost are all the same quiet end. Only the final
	// step's own usage is.
	lastStepZeroUsage bool
	// lastStepOut and lastStepReason are the MOST RECENT step_finish's OWN output and reasoning token
	// counts. lastStepOut is the discriminator for the output-cap stall — a final "length" step with
	// ZERO output — and it is deliberately not the all-zero test above: the recorded MAK-123 stall
	// carried reasoning=32000 and a nonzero total, so only the output figure sees it.
	lastStepOut, lastStepReason           int
	total, in, out, reason, cWrite, cRead int
	cost                                  float64
	sawUsage                              bool
	// sawCost reports that a step_finish actually carried a cost figure. Deliberately separate from
	// sawUsage: opencode reports a step's tokens and its cost as independent siblings, so a session
	// can account for tokens and never for money, and the durable record carries the two questions
	// apart.
	sawCost bool

	// sessionID is the opencode session this stream belongs to, taken from the top-level sessionID
	// field every event carries. Constant for a run; the CLA-406 resurrection path needs it as the
	// handle a follow-up `opencode run --session <id>` continues into.
	sessionID string

	// observed carries ONLY the claim/report observation state, which finish copies onto the real
	// Result.
	observed Result
	console  io.Writer
	// inbox is where this session's console -> agent target is announced, or nil when the adapter is
	// not delivering messages (see opencode_inbox.go).
	inbox *opencodeInbox
	// settledCalls is the tool parts already acted on (by partID), so a re-delivered terminal event
	// cannot rebuild a claim that a later call has since settled.
	settledCalls map[string]bool

	// toolOutputDir is where this session's opencode saves truncated tool output: <data
	// home>/opencode/tool-output, resolved from the session's own environment. The only directory a
	// truncated result is read back from.
	toolOutputDir string

	// errorType is the type of the last typed {"type":"error"} event ("provider.auth", ...), recorded
	// on Result.Raw for diagnostics and the conformance suite.
	errorType string

	// recoveredErrors counts the typed error (or 2.2.x retry) events the stream did NOT end on: an
	// event that is followed by any later typed event is one the session recovered from, because 2.x
	// retries a provider failure itself and carries on. lastEventWasError is whether the most recent
	// typed event was itself an error — exactly the question buildOpencodeErrorText asks of the
	// finished stream, tracked here as the stream arrives so the two cannot disagree about how the
	// session ended.
	recoveredErrors   int
	lastEventWasError bool

	// kill ends the session's process, for the CLA-739 repeated-failure guard (armed on
	// observed.toolLoop by newSessionResult).
	kill func()

	// idle is the no-output watchdog, armed by runSession from the Invocation. nil in the parser
	// tests; a probe gets a disabled guard, where every method is a no-op.
	idle *idleGuard
}

// newOpencodeParse builds the parser a session streams into.
func newOpencodeParse(in Invocation, console io.Writer) opencodeParse {
	return opencodeParse{observed: newSessionResult(in), console: console, toolOutputDir: opencodeToolOutputDir(in)}
}

// diag is where a claim diagnostic goes. The zero opencodeParse has no console (a probe passes
// none, and the parser tests build one directly), and noteClaimed writes unconditionally — by
// design, since a claim that silently stops being tracked is indistinguishable from a session that
// never claimed.
func (p *opencodeParse) diag() io.Writer {
	if p.console == nil {
		return io.Discard
	}
	return p.console
}

// armIdle starts the no-output watchdog for this session. A probe is exempt: it is a one-word
// liveness check, not a session doing work, and killing one for silence would report a
// session-shaped verdict for something no phase ran.
func (p *opencodeParse) armIdle(in Invocation) {
	idle := in.MaxSessionIdle
	if in.Probe {
		idle = 0
	}
	p.idle = newIdleGuard(idle, in.MaxToolCallDuration, func(cause string) {
		fmt.Fprintf(p.diag(), "\n!! %s - killing the session here instead of waiting on a silent child\n", cause)
		if p.kill != nil {
			p.kill()
		}
	})
}

// idleCause is the watchdog's verdict, "" unless it fired.
func (p *opencodeParse) idleCause() string { return p.idle.tripped() }

// idleStepCall is the synthetic tool call an opencode STEP opens in the idle guard: the step IS the
// call window this stream exposes. opencode forwards a tool part only once it has settled and gives
// no request-side tool event at all (see noteTool), so a long call's silence sits somewhere between
// the step's step_start and its step_finish.
const idleStepCall = "step_start"

func (p *opencodeParse) line(line []byte) {
	p.idle.seen()
	trimmed := strings.TrimSpace(string(line))
	if !strings.HasPrefix(trimmed, "{") {
		return
	}
	var ev opencodeEvent
	if json.Unmarshal([]byte(trimmed), &ev) != nil {
		return // partial/non-JSON line — skip
	}
	if p.sessionID == "" && ev.SessionID != "" {
		p.sessionID = ev.SessionID
		if p.inbox != nil {
			p.inbox.setSession(p.sessionID)
		}
	}
	if ev.Type == "error" && ev.Error != nil && ev.Error.Type != "" {
		p.errorType = ev.Error.Type
	}
	if ev.Type != "" {
		if p.lastEventWasError {
			p.recoveredErrors++
		}
		p.lastEventWasError = ev.Type == "error" || (ev.Type == "retry" && ev.Error != nil)
	}
	// CLA-900: the step is the call window this stream exposes. step_start opens it - the last line
	// before the step's tools run, because a tool's own part arrives only after it returned - and
	// step_finish closes it.
	switch ev.Type {
	case "step_start":
		p.idle.toolHandoff(idleStepCall)
	case "step_finish":
		p.idle.toolFinished(idleStepCall)
	}
	if ev.Part == nil {
		return
	}
	switch ev.Type {
	case "step_start":
		p.lastReason = ""
		p.lastStepOut, p.lastStepReason = 0, 0
		p.lastStepZeroUsage = false
	case "text":
		if t := strings.TrimSpace(ev.Part.Text); t != "" {
			p.lastText = t
		}
		noteAssistantText(p.observed.events, ev.Part.Text)
	case "step_finish":
		if ev.Part.Reason != "" {
			p.lastReason = ev.Part.Reason
			p.lastStepOut, p.lastStepReason = 0, 0
		}
		p.lastStepZeroUsage = true
		if tk := ev.Part.Tokens; tk != nil {
			p.total += tk.Total
			p.in += tk.Input
			p.out += tk.Output
			p.reason += tk.Reasoning
			p.cWrite += tk.Cache.Write
			p.cRead += tk.Cache.Read
			if ev.Part.Reason != "" {
				p.lastStepOut = tk.Output
				p.lastStepReason = tk.Reasoning
			}
			if tk.Total != 0 {
				p.lastStepZeroUsage = false
			}
			p.sawUsage = true
		}
		if c := ev.Part.Cost; c != nil {
			p.cost += *c
			if *c != 0 {
				p.lastStepZeroUsage = false
			}
			p.sawUsage = true
			p.sawCost = true
		}
	case "tool_use":
		p.noteTool(ev.Part)
	}
}

// closeEvents flushes and stops this session's agent-event sink. The parser's observation holds the
// sink because the real Result is only built after the process exits; it is nil when the channel is
// off.
func (p *opencodeParse) closeEvents() { p.observed.closeEvents() }

// noteTool feeds one tool event into the shared clankerbar observation, which is what populates
// Result.Claim and the delivery Reports. Only a TERMINAL state is acted on for that observation,
// and that is FUTURE-PROOFING rather than a filter this stream needs today: `opencode run --format
// json` forwards a tool part only once it has settled (true of 1.x source, cli/cmd/run.ts, and of
// every 2.0.32 stream recorded for this adapter), which is why the recorded fixtures carry exactly
// one event per tool part.
func (p *opencodeParse) noteTool(part *opencodePart) {
	st := part.State
	// part.Type ("tool") is deliberately not checked: only a tool part carries a call id and a state,
	// so the fields below are the discriminator.
	if st == nil || part.ID == "" {
		return
	}
	key := part.PartID
	if key == "" {
		key = part.ID
	}
	if st.Status == "running" {
		p.idle.toolStarted(key)
		return
	}
	if st.Status != "completed" && st.Status != "error" {
		return
	}
	p.idle.toolFinished(key)
	if p.settledCalls[key] {
		return
	}
	if p.settledCalls == nil {
		p.settledCalls = map[string]bool{}
	}
	p.settledCalls[key] = true
	noteOpencodeToolEvent(p.observed.events, part.Tool, part.ID, st.Input)
	if p.observed.toolLoop.observe(part.Tool, st.Status == "error", st.Error) && p.kill != nil {
		fmt.Fprintf(p.diag(), "\n!! %s - killing the session here instead of letting it retry the same failing call\n", p.observed.toolLoop.tripped())
		p.kill()
	}
	name := opencodeClankerbarTool(part.Tool)
	transcript := p.observed.events != nil && p.observed.events.TranscriptEnabled()
	// The `.env*` verdict is transcript-only; computing it once keeps the spilled output from being
	// re-read only to be withheld.
	dropOutput := transcript && envReadTarget(part.Tool, toolTarget(part.Tool, st.Input))
	// The result is rehydrated when EITHER consumer needs it: the clankerbar observation (a claim
	// refused inside a spilled payload) and the transcript tier (the model saw the output, not the
	// pointer).
	rehydrated, rehydratedOK := "", false
	if name != "" || (transcript && !dropOutput) {
		var err error
		if rehydrated, rehydratedOK, err = opencodeTruncatedOutput(st, p.toolOutputDir); err != nil {
			fmt.Fprintf(p.diag(), "  · %s result was truncated by opencode and could not be read back: %v\n", part.Tool, err)
		}
	}
	if transcript {
		output := st.Output
		if st.Status == "error" {
			output = st.Error
		} else if rehydratedOK {
			output = rehydrated
		}
		noteOpencodeToolResult(p.observed.events, part.Tool, part.ID, st.Input, st.Status != "error", output)
	}
	if name == "" {
		return
	}

	noteToolUse(name, part.ID, st.Input, &p.observed)
	output := st.Output
	if rehydratedOK {
		output = rehydrated
	}
	content, _ := json.Marshal(output)
	noteToolResult(&p.observed, part.ID, st.Status == "error", content, p.diag())
}

// finish writes what the stream added up to onto res.
func (p *opencodeParse) finish(res *Result) {
	res.FinalMessage = p.lastText
	// What the session did with the backlog. Copied rather than accumulated in place because res does
	// not exist until the process has exited; see the `observed` field.
	res.Claim = p.observed.Claim
	res.BlockingAsk = p.observed.BlockingAsk
	res.Reports = p.observed.Reports
	res.ClankerbarCalls = p.observed.ClankerbarCalls
	res.UsageReported = p.sawUsage
	// The cost figure is its own question: a stream whose steps carried tokens but no cost reported no
	// money, and the durable record must say so rather than reporting 0.
	res.CostReported = p.sawCost
	if p.sawUsage {
		res.Tokens = p.total
		res.CostUSD = p.cost
		res.Usage = TokenUsage{
			Input:      p.in,
			Output:     p.out,
			Reasoning:  p.reason,
			CacheRead:  p.cRead,
			CacheWrite: p.cWrite,
		}
		res.Raw = map[string]any{
			"input_tokens": p.in, "output_tokens": p.out, "reasoning_tokens": p.reason,
			"cache_write_tokens": p.cWrite, "cache_read_tokens": p.cRead,
		}
	}
	// The finish reason is recorded even when the step carried no usage: a dead session's final step
	// can report zero tokens, and that reason is exactly the CLA-386 signal the driver needs to refuse
	// to hand the phase on.
	if p.lastReason != "" {
		if res.Raw == nil {
			res.Raw = map[string]any{}
		}
		res.Raw[FinishReasonKey] = p.lastReason
		// The CLA-398 quiet-death signature: a FINAL "unknown" step whose OWN usage was all-zero.
		if p.lastReason == FinishReasonUnknown && p.lastStepZeroUsage {
			res.Raw[TerminalReasonKey] = ZeroUsageReason
		}
		// CLA-584: the output-cap stall — a FINAL step that hit the output token limit with ZERO
		// output.
		if p.lastReason == FinishReasonLength && p.lastStepOut == 0 {
			res.Raw[TerminalReasonKey] = OutputCapReason
			res.Raw[OutputCapReasoningKey] = p.lastStepReason
		}
	}
	// The opencode session id, when the stream named one: the CLA-406 resurrection path resumes INTO
	// this session via `opencode run --session`, and needs the handle to survive onto the Result.
	if p.sessionID != "" {
		if res.Raw == nil {
			res.Raw = map[string]any{}
		}
		res.Raw[opencodeSessionIDKey] = p.sessionID
		res.SessionRef = p.sessionID
	}
	if p.errorType != "" {
		if res.Raw == nil {
			res.Raw = map[string]any{}
		}
		res.Raw[OpencodeErrorTypeKey] = p.errorType
	}
	// CLA-700: the events this session RECOVERED from, with no error event ending the stream.
	if p.recoveredErrors > 0 && !p.lastEventWasError {
		if res.Raw == nil {
			res.Raw = map[string]any{}
		}
		res.Raw[RecoveredErrorsKey] = p.recoveredErrors
	}
}
