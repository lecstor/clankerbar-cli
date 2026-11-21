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

// runSession executes ONE `opencode run` process end-to-end and parses its stream.
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
		// Nothing has been spawned yet, so this is a clean refusal to start rather than a session to
		// classify: an empty Result, and the error the caller reports.
		return Result{}, err
	}

	// CLA-703: a working (non-probe) session gets a private server of the adapter's own, and does not
	// run its first model turn until that server reports the clankerbar MCP connection established.
	runArgs := args
	runEnv := env
	var srv *opencodeServer
	if !in.Probe {
		// Only a session with a clankerbar server gets the preflight.
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
			// The delivery target is this process's own server.
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
	// A cap that can hang is not a cap.
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
	// Through the constructor, not a struct literal: it carries the resumed-phase claim seed, and the
	// tests build the parser the same way so that dropping it here turns them red.
	p := newOpencodeParse(in, console)
	defer p.closeEvents()
	p.kill = loopKill
	// The parser is where the session id first appears; it hands the id to the delivery target (see
	// opencodeParse.line).
	p.inbox = delivery
	// The no-output watchdog, on the same cancellation the tool-loop kill uses.
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
	// A clean exit beat the kill: the session finished, so it was not a hang.
	if runErr == nil {
		res.IdleKilled = ""
	} else {
		res.IdleKilled = p.idleCause()
		res.IdleToolCall = res.IdleKilled != "" && p.idle.trippedWithOpenCall()
	}

	// Our own deadline, not the caller's: ctx.Err() distinguishes a session we cut off from a run-wide
	// Ctrl-C / SIGTERM / supervised-wait cancellation, which cancels the parent too and is NOT this
	// phase reaching its backstop.
	timedOut := runErr != nil && sctx.Err() == context.DeadlineExceeded && ctx.Err() == nil
	if timedOut {
		res.markWallClockCapped()
		if console != nil {
			fmt.Fprintf(console, "\n!! session outlived its wall-clock cap (%s) — ending it here; whatever it wrote is still in the worktree, uncommitted\n", in.MaxSessionWallClock)
		}
	}
	// CLA-739: the guard's verdict rides the Result whether or not the kill landed before the process
	// ended on its own - the session WAS looping, and that is what the driver classifies.
	res.ToolLoop = p.observed.toolLoop.tripped()
	loopKilled := runErr != nil && res.ToolLoop != ""
	idleKilled := res.IdleKilled != ""

	if ee, ok := runErr.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
		res.ExitSignal = exitSignal(ee)
	} else if errors.Is(runErr, exec.ErrWaitDelay) && !timedOut && !loopKilled && !idleKilled {
		// The process exited cleanly on its own — ExitCode 0 — but a grandchild (a backgrounded
		// build, a test run) inherited the output pipes and held them open past cmd.WaitDelay, so os/exec
		// force-closed them and Run returned exec.ErrWaitDelay instead of an *exec.ExitError.
		o.settleUsage(ctx, in, &res, started, in.ResumeSession)
		return res, nil
	} else if runErr != nil && !timedOut && !loopKilled && !idleKilled {
		// A launch failure (bad PATH, unreadable config): nothing was emitted, so res is an honest zero,
		// and the failure is real.
		return res, runErr
	} else if runErr != nil {
		// Our own kill: the child died on the cancel, so its error is the kill's and not a verdict.
		res.ExitCode = -1
	}
	o.settleUsage(ctx, in, &res, started, in.ResumeSession)
	return res, nil
}

// opencodeEvent is one line of `opencode run --format json`.
type opencodeEvent struct {
	Type  string              `json:"type"`
	Part  *opencodePart       `json:"part"`
	Error *opencodeErrorEvent `json:"error"`

	// SessionID is the opencode session every event of one run belongs to.
	SessionID string `json:"sessionID"`
}

// opencodeErrorEvent is the typed error 2.x puts on a {"type":"error"} event.
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

	// Tool parts, which arrive on a "tool_use" event.
	Tool   string             `json:"tool"`
	ID     string             `json:"id"`
	PartID string             `json:"partID"`
	State  *opencodeToolState `json:"state"`
}

// opencodeToolState is a tool call's lifecycle slot on its part.
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
// TIME as it arrives.
type opencodeParse struct {
	lastText string
	// lastReason is the reason of the MOST RECENT step_finish — the session's final step, once the
	// stream ends.
	lastReason string
	// lastStepZeroUsage reports that the MOST RECENT step_finish's own usage summed to zero — no
	// tokens block, an all-zero tokens block, or no cost are all the same quiet end.
	lastStepZeroUsage bool
	// lastStepOut and lastStepReason are the MOST RECENT step_finish's OWN output and reasoning token
	// counts.
	lastStepOut, lastStepReason           int
	total, in, out, reason, cWrite, cRead int
	cost                                  float64
	sawUsage                              bool
	// sawCost reports that a step_finish actually carried a cost figure.
	sawCost bool

	// sessionID is the opencode session this stream belongs to, taken from the top-level sessionID
	// field every event carries.
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
	// home>/opencode/tool-output, resolved from the session's own environment.
	toolOutputDir string

	// errorType is the type of the last typed {"type":"error"} event ("provider.auth", ...), recorded
	// on Result.Raw for diagnostics and the conformance suite.
	errorType string

	// recoveredErrors counts the typed error (or 2.2.x retry) events the stream did NOT end on: an
	// event that is followed by any later typed event is one the session recovered from, because 2.x
	// retries a provider failure itself and carries on.
	recoveredErrors   int
	lastEventWasError bool

	// kill ends the session's process, for the CLA-739 repeated-failure guard (armed on
	// observed.toolLoop by newSessionResult).
	kill func()

	// idle is the no-output watchdog, armed by runSession from the Invocation.
	idle *idleGuard
}

// newOpencodeParse builds the parser a session streams into.
func newOpencodeParse(in Invocation, console io.Writer) opencodeParse {
	return opencodeParse{observed: newSessionResult(in), console: console, toolOutputDir: opencodeToolOutputDir(in)}
}

// diag is where a claim diagnostic goes.
func (p *opencodeParse) diag() io.Writer {
	if p.console == nil {
		return io.Discard
	}
	return p.console
}

// armIdle starts the no-output watchdog for this session.
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
// call window this stream exposes.
const idleStepCall = "step_start"

func (p *opencodeParse) line(line []byte) {
	// Output restarts the idle clock unless a step window is open.
	p.idle.seen()
	trimmed := strings.TrimSpace(string(line))
	if !strings.HasPrefix(trimmed, "{") {
		return
	}
	var ev opencodeEvent
	if json.Unmarshal([]byte(trimmed), &ev) != nil {
		return // partial/non-JSON line — skip
	}
	// Every event names its session; the first one seen wins (they all agree within a run).
	if p.sessionID == "" && ev.SessionID != "" {
		p.sessionID = ev.SessionID
		if p.inbox != nil {
			p.inbox.setSession(p.sessionID)
		}
	}
	if ev.Type == "error" && ev.Error != nil && ev.Error.Type != "" {
		p.errorType = ev.Error.Type
	}
	// CLA-700: an error event the stream moves past is one the session recovered from.
	if ev.Type != "" {
		if p.lastEventWasError {
			// The PREVIOUS error event was followed by this one, so it was recovered from (whether or not
			// this event is also an error).
			p.recoveredErrors++
		}
		// 2.2.x reports a recovery it retries as a `retry` event carrying the error; it counts as
		// recovered once something follows it.
		p.lastEventWasError = ev.Type == "error" || (ev.Type == "retry" && ev.Error != nil)
	}
	// CLA-900: the step is the call window this stream exposes.
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
		// A new step has begun, so the previous step is no longer the final one.
		p.lastReason = ""
		p.lastStepOut, p.lastStepReason = 0, 0
		p.lastStepZeroUsage = false
	case "text":
		if t := strings.TrimSpace(ev.Part.Text); t != "" {
			p.lastText = t
		}
		// Transcript tier (CLA-887): one event per text part.
		noteAssistantText(p.observed.events, ev.Part.Text)
	case "step_finish":
		// CLA-900: the step window is CLOSED above, before the Part check.
		if ev.Part.Reason != "" {
			p.lastReason = ev.Part.Reason
			// CLA-584: the stalled step's own figures move WITH the reason, so the pair always describes ONE
			// event.
			p.lastStepOut, p.lastStepReason = 0, 0
		}
		// tokens and cost are siblings on the part; count each independently so a step that reports one
		// without the other still lands in the budget.
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

// closeEvents flushes and stops this session's agent-event sink.
func (p *opencodeParse) closeEvents() { p.observed.closeEvents() }

// noteTool feeds one tool event into the shared clankerbar observation, which is what populates
// Result.Claim and the delivery Reports.
func (p *opencodeParse) noteTool(part *opencodePart) {
	st := part.State
	// part.Type ("tool") is deliberately not checked: only a tool part carries a call id and a state,
	// so the fields below are the discriminator.
	if st == nil || part.ID == "" {
		return
	}
	// The call's identity for the replay guard.
	key := part.PartID
	if key == "" {
		key = part.ID
	}
	// A part still in flight opens a tool call for the idle watchdog: the session is working rather
	// than hanging, and the call's own cap bounds it.
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
	// The channel's activity tier hears the call, once, before the clankerbar observation consumes it.
	noteOpencodeToolEvent(p.observed.events, part.Tool, part.ID, st.Input)
	// CLA-739: the STATUS is the failure flag (see opencodeToolState), and the error text rides where
	// the output would have been.
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
			// Fail safe: the envelope goes on as the result, which no consumer can parse, so a claim is NOT
			// tracked - the same outcome as any unreadable claim result, with the reason said first.
			fmt.Fprintf(p.diag(), "  · %s result was truncated by opencode and could not be read back: %v\n", part.Tool, err)
		}
	}
	if transcript {
		// Transcript tier: the status is the ok flag, and the error text stands in for the output on a
		// failed call.
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
	// The result text is handed over as JSON so it reaches noteClaimed by the same road claude's
	// string-shaped tool_result content does.
	output := st.Output
	if rehydratedOK {
		output = rehydrated
	}
	content, _ := json.Marshal(output)
	noteToolResult(&p.observed, part.ID, st.Status == "error", content, p.diag())
}
