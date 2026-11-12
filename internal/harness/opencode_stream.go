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

// runSession executes ONE `opencode run` process end-to-end and parses its
// stream. Split out of Invoke so the CLA-406 resurrection loop can run the
// probe and the continuation through exactly the same exec/parse machinery as
// the original session — same env, same permission policy, same capture — and
// merge the results, rather than maintaining a second code path that would
// drift the first time either half changed.
func (o opencode) runSession(ctx context.Context, in Invocation, args []string) (Result, error) {

	// The per-session wall-clock cap, enforced on the exec context so the kill
	// needs nothing from the stream: opencode reports no turn count and takes no
	// turn flag, so elapsed time is the only backstop this adapter can offer a
	// phase (CLA-368). The derived context is ALWAYS created, cap or no cap, so
	// there is one cancellation path rather than two shapes of ctx below.
	//
	// Never on a PROBE: a probe is a one-word liveness check, not a session doing
	// work, and a capped probe would report a phase-shaped cap for something no
	// phase ran. The driver never sets the field on a probe either — this is the
	// belt to that braces, so a future caller reusing an Invocation cannot turn a
	// probe into a capped session by accident.
	sctx := ctx
	cancel := context.CancelFunc(func() {})
	if in.MaxSessionWallClock > 0 && !in.Probe {
		sctx, cancel = context.WithTimeout(ctx, in.MaxSessionWallClock)
	}
	defer cancel()

	// One canonical directory for the permission policy, the server process,
	// the status query and the run: the server keys its MCP state by the
	// resolved path, so a raw `/var/...` query would poll a location no session
	// uses (see opencodeWorkDir). The policy's absolute paths must agree with
	// the directory the session actually runs in, or an in-tree write can look
	// external on a symlinked temp dir.
	workDir := opencodeWorkDir(in.WorkDir)
	if workDir != "" {
		in.WorkDir = workDir
	}
	env, err := o.env(in)
	if err != nil {
		// Nothing has been spawned yet, so this is a clean refusal to start
		// rather than a session to classify: an empty Result, and the error the
		// caller reports. See env's fail-closed note.
		return Result{}, err
	}

	// CLA-703: a working (non-probe) session gets a private server of the
	// adapter's own, and does not run its first model turn until that server
	// reports the clankerbar MCP connection established. The OLD shape —
	// `run --standalone`, which spawns its own private server and submits the
	// prompt immediately — is exactly the race: opencode 2.x connects MCP in the
	// background, so a fast model could take its first turn with no clankerbar
	// tools at all.
	//
	// A PROBE keeps `--standalone`: it is a one-word liveness check of an
	// already-created session and does not use the backlog, so waiting for MCP
	// would only eat its time box.
	runArgs := args
	runEnv := env
	var srv *opencodeServer
	if !in.Probe {
		// Only a session with a clankerbar server gets the preflight. That
		// server is resolved across the config layers opencode actually loads:
		// the named file when there is one, otherwise the global config dir and
		// the project-level files discovered from the instance directory. A
		// layer that exists but cannot be read fails CLOSED — an unknown
		// effective config is not evidence that no backlog server is declared —
		// and a session that does declare one never gets its first turn until
		// that server reports connected (CLA-703 review, findings 1.1 and 3.1).
		names, err := opencodeEffectiveMCPServers(in, env)
		if err != nil {
			return mcpPreflightFailure(sctx, ctx, err)
		}
		if len(names) > 0 {
			srv, err = startOpencodeServer(sctx, env, workDir)
			if err != nil {
				return mcpPreflightFailure(sctx, ctx, err)
			}
			defer srv.stop()
			if err := srv.waitForMCP(sctx, names, workDir); err != nil {
				return mcpPreflightFailure(sctx, ctx, err)
			}
			runArgs = withOpencodeServer(args, srv.url)
			// The run must authenticate to the server the adapter started,
			// whatever an inherited or configured OPENCODE_PASSWORD says.
			// Appended last so it wins over env's own tail (in.Env).
			runEnv = append(append([]string{}, env...), "OPENCODE_PASSWORD="+srv.password)
		}
	}

	// CLA-739: the repeated-failing-tool-call kill cancels THIS context, a child
	// of sctx, so the kill reaches the run alone - never the server above, which
	// the deferred stop tears down, and never the caller. Created even when the
	// guard is off, so there is one shape of exec context below.
	lctx, loopKill := context.WithCancel(sctx)
	defer loopKill()
	cmd := exec.CommandContext(lctx, "opencode", runArgs...)
	setupProcessGroup(cmd)
	if workDir != "" {
		cmd.Dir = workDir
	}
	cmd.Env = runEnv
	// A cap that can hang is not a cap. The capture points Stdout/Stderr at
	// io.MultiWriter values, so os/exec makes its own pipes and cmd.Run waits for
	// every WRITER of them to close — and CommandContext kills the direct child
	// only. A runaway session is exactly the case with a bash-tool grandchild (a
	// build, a test run) or an MCP server holding the inherited fd, which survives
	// the SIGKILL and keeps the pipe open: without a delay, Invoke blocks past its
	// own deadline in the one scenario the cap exists for. WaitDelay force-closes
	// the pipes and lets Wait return.
	// The orphan itself is killed by the process-group Cancel below; this is
	// the backstop BEHIND that kill, for a descendant that escaped the group -
	// a daemonised child (setsid) lands in a group of its own that no signal
	// of ours reaches (the same escapee claude's session path has no backstop
	// for at all, CLA-423). Set whenever the exec context carries ANY deadline -
	// one this function created, or one a caller handed in (the CLA-406
	// probe's 30s box arrives that way, and the old `sctx != ctx` test missed
	// exactly that case): an fd holder would otherwise keep the pipe open past
	// the kill and hang Run past its own box. An ordinary uncapped session's
	// I/O is never cut short by it.
	//
	// CLA-739 arms it too whenever the repeated-failure guard is armed, because
	// that guard is a cancellation with no deadline, and a kill that can hang is
	// not a kill either. The idle watchdog is the same shape of cancellation, so
	// it arms it too. What that changes for a session neither guard fires on: a
	// grandchild still holding the pipes after opencode exits on its own now has
	// Run return exec.ErrWaitDelay 5s later instead of blocking until the
	// grandchild lets go - the CLA-414 branch below already reads that as the
	// clean end it is.
	if _, hasDeadline := sctx.Deadline(); hasDeadline || ((in.MaxRepeatedToolFailures > 0 || in.MaxSessionIdle > 0) && !in.Probe) {
		cmd.WaitDelay = 5 * time.Second
	}

	// Kill the whole process group on ANY cancellation of sctx - the
	// wall-clock cap, or the caller's own cancellation (a Ctrl-C that stopped
	// reaching the session subtree through the tty the moment Setpgid moved
	// it out of the foreground group). Through exec.Cmd.Cancel, not a monitor
	// goroutine: os/exec calls Cancel only while it still considers the
	// process live, so the post-reap window shrinks from "every capped
	// session" to the instant between Process.Wait returning and the
	// watchdog's handshake, where os/exec's own Kill reads os.ErrProcessDone.
	// killProcessGroup's raw syscall.Kill narrows the recycled-pid hazard by
	// orders of magnitude without eliminating it - unlike Process.Kill it
	// enjoys no pidfd/reaped-status protection, so a group id reused inside
	// that window would be signalled in error. Accepted rather than hardened:
	// skipping the group kill whenever the direct child looks already-reaped
	// could strand exactly the in-group descendants this file exists to kill,
	// trading the bar away for a microseconds-wide hazard. The direct child
	// is killed too (the trailing Kill mirrors CommandContext's own default
	// Cancel), and a Kill on an already-dead child reads as os.ErrProcessDone,
	// which os/exec treats as nothing to report.
	cmd.Cancel = func() error {
		killProcessGroup(cmd)
		return cmd.Process.Kill()
	}

	// Parse as the stream arrives, retain only a bounded tail of it for the text
	// scans, and tee live to the console when one is set (the JSON event stream —
	// a readable renderer is a TODO; raw is honest live output).
	//
	// Live parsing is load-bearing here rather than merely tidy: this adapter SUMS
	// per-step usage across the whole session, so a capped copy of stdout walked
	// afterwards would silently drop the early steps and under-count the budget by
	// however much of the stream had scrolled away (CLA-262).
	console := in.Console
	if in.Probe {
		console = nil
	}
	// Through the constructor, not a struct literal: it carries the resumed-phase
	// claim seed, and the tests build the parser the same way so that dropping it
	// here turns them red.
	p := newOpencodeParse(in, console)
	p.kill = loopKill
	// The no-output watchdog, on the same cancellation the tool-loop kill uses.
	// Armed after p.kill so its callback has the kill switch; a probe leaves it
	// disabled.
	p.armIdle(in)
	captured := newCapture(p.line)
	captured.attach(cmd, console)
	// Taken before the spawn, so every message this process adds to the session
	// is stamped after it: applyExport attributes export figures to THIS run by
	// that window (a resumed session's export also carries the earlier runs).
	started := time.Now()
	runErr := cmd.Run()

	// Disarmed BEFORE the verdict is read: a timer that lands as the process
	// exits must not report a kill for a session that had already finished.
	p.idle.stop()
	res := captured.result("opencode")
	p.finish(&res)
	// A clean exit beat the kill: the session finished, so it was not a hang.
	if runErr == nil {
		res.IdleKilled = ""
	} else {
		res.IdleKilled = p.idleCause()
	}

	// Our own deadline, not the caller's: ctx.Err() distinguishes a session we
	// cut off from a run-wide Ctrl-C / SIGTERM / supervised-wait cancellation,
	// which cancels the parent too and is NOT this phase reaching its backstop.
	// Marked after p.finish, which rewrites Raw wholesale when it saw usage.
	//
	// runErr is the third conjunct because the deadline is read AFTER Run
	// returned: a session that exited cleanly a hair before it, with the deadline
	// passing in the window while the output was parsed, would otherwise be
	// marked as capped. Nothing would misbehave today — the driver takes its
	// exit-0 branch first — but the Result would be lying about how the session
	// ended, and the next reader of this marker inherits the lie.
	timedOut := runErr != nil && sctx.Err() == context.DeadlineExceeded && ctx.Err() == nil
	if timedOut {
		res.markWallClockCapped()
		if console != nil {
			fmt.Fprintf(console, "\n!! session outlived its wall-clock cap (%s) — ending it here; whatever it wrote is still in the worktree, uncommitted\n", in.MaxSessionWallClock)
		}
	}
	// CLA-739: the guard's verdict rides the Result whether or not the kill
	// landed before the process ended on its own - the session WAS looping, and
	// that is what the driver classifies. loopKilled is the narrower fact the
	// exit handling below needs: Run's error is the kill's, not the session's.
	res.ToolLoop = p.observed.toolLoop.tripped()
	loopKilled := runErr != nil && res.ToolLoop != ""
	idleKilled := res.IdleKilled != ""

	if ee, ok := runErr.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
		res.ExitSignal = exitSignal(ee)
	} else if errors.Is(runErr, exec.ErrWaitDelay) && !timedOut && !loopKilled && !idleKilled {
		// The process exited cleanly on its own — ExitCode 0 — but a grandchild
		// (a backgrounded build, a test run) inherited the output pipes and held
		// them open past cmd.WaitDelay, so os/exec force-closed them and Run
		// returned exec.ErrWaitDelay instead of an *exec.ExitError. p.finish has
		// already run (the CLA-299 ordering: parse whatever arrived, THEN
		// classify), and every event the session emitted was consumed by the live
		// parse as it arrived — only grandchild bytes written after the parent's
		// exit are lost, and those were never this session's output. This is a
		// clean end, not a failure: returning it as an error failed a completed
		// attempt in drainPhase with no retry classification (CLA-414). Residual
		// risk, accepted: force-closed pipes mean this cannot be distinguished
		// from a parent killed while pretending to succeed — ExitCode 0 plus a
		// fully parsed live stream is the strongest evidence available either way.
		// The same holds for a run-wide cancellation landing inside the window:
		// sctx reads Canceled, not DeadlineExceeded, so !timedOut is true and the
		// attempt ends "clean" during teardown. Nothing durable turns on it —
		// salvage, handback and spend are computed from res before any error
		// check either way, and the cancelled driver ctx stops the run one spawn
		// later — so the noise is left standing rather than guarded against.
		o.settleUsage(ctx, in, &res, started, isResumeArgs(args))
		return res, nil
	} else if runErr != nil && !timedOut && !loopKilled && !idleKilled {
		// A launch failure (bad PATH, unreadable config): nothing was emitted,
		// so res is an honest zero, and the failure is real.
		return res, runErr
	} else if runErr != nil {
		// Our own kill (the wall-clock cap, the CLA-739 repeated failing tool
		// call, or the idle watchdog): the child died on the cancel, so its error
		// is the kill's and not a verdict. The marker above is the verdict.
		res.ExitCode = -1
	}
	o.settleUsage(ctx, in, &res, started, isResumeArgs(args))
	return res, nil
}

// opencodeEvent is one line of `opencode run --format json`. The stream is a
// sequence of typed events, each wrapping a message part; this captures only the
// fields the loop needs and tolerates the rest.
//
// Shapes, recorded against opencode 2.0.32 (every event also carries a
// top-level `timestamp`):
//
//	{"type":"step_start","sessionID":"ses_...","part":{"id":"prt_...","messageID":"msg_...","type":"step-start"}}
//	{"type":"text","sessionID":"ses_...","part":{"id":"prt_...","type":"text","text":"DONE",...}}
//	{"type":"step_finish","sessionID":"ses_...","part":{"type":"step-finish","reason":"tool-calls",
//	   "cost":0.055563,"tokens":{"input":18511,"output":2,"reasoning":0,"cache":{"read":0,"write":0}}}}
//	{"type":"error","sessionID":"ses_...","error":{"type":"provider.auth","message":"...","status":401,...}}
//
// step_finish is emitted for TOOL-CALL steps only: the step that ends the
// session with a plain answer emits none, so neither the final reason nor the
// final step's usage is on the stream. That is what the session export after
// exit is for (opencode_export.go); the stream figures are the fallback.
type opencodeEvent struct {
	Type  string              `json:"type"`
	Part  *opencodePart       `json:"part"`
	Error *opencodeErrorEvent `json:"error"`

	// SessionID is the opencode session every event of one run belongs to. It
	// rides at the TOP level of each event, not on the part, and is constant for
	// a run — captured once and used as the handle a follow-up `opencode run -s
	// <id>` continues and `opencode session export` reads.
	SessionID string `json:"sessionID"`
}

// opencodeErrorEvent is the typed error 2.x puts on a {"type":"error"} event.
// The type is a stable dotted name ("provider.auth", "provider.rate-limit",
// "provider.no-route", "provider.invalid-output"), which classification keys on
// before it falls back to the word classes.
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
	// A POINTER for the same reason Tokens is one: a step reporting cost 0 has
	// REPORTED, and the driver's zero-spend bound counts silence, not zeroes
	// (CLA-288).
	Cost *float64 `json:"cost"`

	// Tool parts, which arrive on a "tool_use" event. On 2.x the call id is
	// `id` (1.x called it `callID`) and the part's own id moves to `partID`.
	// The call id is the PROVIDER's, so it is not unique across steps - the
	// fake provider used in the conformance suite numbers every step's calls
	// from call_0 - which is why replays are recognised by partID.
	Tool   string             `json:"tool"`
	ID     string             `json:"id"`
	PartID string             `json:"partID"`
	State  *opencodeToolState `json:"state"`
}

// opencodeToolState is a tool call's lifecycle slot on its part. Where claude
// splits a call across an `assistant` tool_use block and a later `user`
// tool_result block, opencode emits the part once it has settled and carries
// the arguments and the result TOGETHER (recorded against opencode 2.0.32 with
// the MCP server out of Code Mode):
//
//	{"type":"tool_use","part":{"partID":"prt_...","type":"tool","id":"call_0",
//	  "tool":"clankerbar_claim_task","state":{"status":"completed",
//	    "input":{"taskId":"T-9"},
//	    "output":"{\"task\": {\"id\": \"T-9\", \"ref\": \"CLA-1\"}, \"run\": {\"id\": \"run-1\"}}",
//	    "title":"clankerbar_claim_task","metadata":{...}}}}
//
// A refusal takes the other terminal status and puts the error where the output
// would have been. There is no `is_error` flag to read — the STATUS is the flag,
// which is why a status this adapter does not recognise must never be treated as
// success:
//
//	{"state":{"status":"error","input":{...},
//	  "error":"No tool named \"clankerbar_claim_task\" is currently available. ..."}}
//
// `output` is a STRING, not an object: an MCP tool's result arrives flattened to
// its text content, which is the shape noteClaimed already parses.
//
// A large result is TRUNCATED on 2.x, the same failure Claude Code's spill
// caused on the phase seam (CLA-330, docs/large-tool-results.md). Verified
// against 2.0.32: an MCP result above 51,200 bytes arrives as
//
//	"output":"[showing 0 lines of 1; full output saved to <data>/opencode/tool-output/tool_...]",
//	"metadata":{"metadata":{"truncated":true,"outputPath":"<the same path>"},...}
//
// and the full text is in that file. Plane payloads reach that size (a
// claim_task carrying decisions, a get_task at 56 KB). opencodeTruncatedOutput
// rehydrates it from the file, under the guards that function states.
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

// opencodeParse accumulates FinalMessage, Tokens and CostUSD from the event
// stream, ONE LINE AT A TIME as it arrives.
//
// Tokens/cost are SUMMED across every step-finish part: opencode reports usage
// per step (one step = one LLM turn), not cumulatively, so a multi-turn drain
// emits several step-finish parts that each cover their own turn. FinalMessage is
// the last non-empty text part — the final assistant answer, after any
// intermediate per-step messages.
//
// Because it sums, it has to see EVERY step — which is exactly why it runs on the
// live stream instead of over the retained tail (see Invoke).
type opencodeParse struct {
	lastText string
	// lastReason is the reason of the MOST RECENT step_finish — the session's
	// final step, once the stream ends. opencode writes "stop" when the session
	// produced a final answer and "unknown" when it died without one (the
	// CLA-386 dead-phase marker); the driver reads it off Result.Raw via
	// FinishReasonKey.
	lastReason string
	// lastStepZeroUsage reports that the MOST RECENT step_finish's own usage
	// summed to zero — no tokens block, an all-zero tokens block, or no cost
	// are all the same quiet end. Reset for every step: the CLA-398
	// quiet-death signature is a FINAL step whose OWN usage is all-zero — a
	// session can do hours of real paid work and then die with an "unknown"
	// final step that reported nothing, so the session total (p.total/p.cost)
	// is NOT the discriminator. Only the final step's own usage is.
	lastStepZeroUsage bool
	// lastStepOut and lastStepReason are the MOST RECENT step_finish's OWN
	// output and reasoning token counts (CLA-584). lastStepOut is the
	// discriminator for the output-cap stall — a final "length" step with ZERO
	// output — and it is deliberately not the all-zero test above: the recorded
	// MAK-123 stall carried reasoning=32000 and a nonzero total, so only the
	// output figure sees it. lastStepReason rides onto the Result for the
	// daemon's named log line. Both move only with a step_finish that carried a
	// reason, so they always describe the same event as lastReason (deadscan's
	// rule, kept in step so the two classifiers cannot disagree).
	lastStepOut, lastStepReason           int
	total, in, out, reason, cWrite, cRead int
	cost                                  float64
	sawUsage                              bool
	// sawCost reports that a step_finish actually carried a cost figure
	// (CLA-691). Deliberately separate from sawUsage: opencode reports a step's
	// tokens and its cost as independent siblings, so a session can account for
	// tokens and never for money, and the durable record carries the two
	// questions apart.
	sawCost bool

	// sessionID is the opencode session this stream belongs to, taken from the
	// top-level sessionID field every event carries. Constant for a run; the
	// CLA-406 resurrection path needs it as the handle a follow-up
	// `opencode run --session <id>` continues into.
	sessionID string

	// observed carries ONLY the claim/report observation state, which finish
	// copies onto the real Result. It cannot be the real Result: that one is built
	// by capture.result() after the process exits, long after the claim event went
	// past, so anything written into it during the stream would be discarded.
	observed Result
	console  io.Writer
	// settledCalls is the tool parts already acted on (by partID), so a
	// re-delivered terminal event cannot rebuild a claim that a later call has
	// since settled.
	settledCalls map[string]bool

	// toolOutputDir is where this session's opencode saves truncated tool
	// output: <data home>/opencode/tool-output, resolved from the session's own
	// environment. The only directory a truncated result is read back from.
	toolOutputDir string

	// errorType is the type of the last typed {"type":"error"} event
	// ("provider.auth", ...), recorded on Result.Raw for diagnostics and the
	// conformance suite. Classification itself reads the event text.
	errorType string

	// recoveredErrors counts the typed error (or 2.2.x retry) events the stream
	// did NOT end on:
	// an event that is followed by any later typed event is one the session
	// recovered from, because 2.x retries a provider failure itself and carries
	// on (CLA-700). lastEventWasError is whether the most recent typed event was
	// itself an error — exactly the question buildOpencodeErrorText asks of the
	// finished stream, tracked here as the stream arrives so the two cannot
	// disagree about how the session ended.
	recoveredErrors   int
	lastEventWasError bool

	// kill ends the session's process, for the CLA-739 repeated-failure guard
	// (armed on observed.toolLoop by newSessionResult). Set by runSession to the
	// cancel of the run's own exec context; nil in the parser tests, where the
	// guard still records its verdict and nothing is killed.
	kill func()

	// idle is the no-output watchdog, armed by runSession from the Invocation.
	// nil in the parser tests; a probe gets a disabled guard, where every method
	// is a no-op.
	idle *idleGuard
}

// newOpencodeParse builds the parser a session streams into. It exists so the
// CLAIM SEED is exercised by tests: a resumed phase never calls claim_task, so
// without newSessionResult here its Claim would be the zero value and
// Claim.Held() — the gate on the handback, the salvage and the delivery check —
// would be false for the phase that pushes the branch and opens the PR. Assigned
// inline in Invoke, deleting it would leave the whole suite green.
func newOpencodeParse(in Invocation, console io.Writer) opencodeParse {
	return opencodeParse{observed: newSessionResult(in), console: console, toolOutputDir: opencodeToolOutputDir(in)}
}

// diag is where a claim diagnostic goes. The zero opencodeParse has no console (a
// probe passes none, and the parser tests build one directly), and noteClaimed
// writes unconditionally — by design, since a claim that silently stops being
// tracked is indistinguishable from a session that never claimed.
func (p *opencodeParse) diag() io.Writer {
	if p.console == nil {
		return io.Discard
	}
	return p.console
}

// armIdle starts the no-output watchdog for this session. A probe is exempt: it
// is a one-word liveness check, not a session doing work, and killing one for
// silence would report a session-shaped verdict for something no phase ran. The
// guard is always non-nil so stop can be deferred unconditionally; a zero bound
// leaves it disabled.
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
	// Every event names its session; the first one seen wins (they all agree
	// within a run). Taken before the Part check, because an error event carries
	// no part and can be the only event a failed run emits.
	if p.sessionID == "" && ev.SessionID != "" {
		p.sessionID = ev.SessionID
	}
	if ev.Type == "error" && ev.Error != nil && ev.Error.Type != "" {
		p.errorType = ev.Error.Type
	}
	// CLA-700: an error event the stream moves past is one the session
	// recovered from. Counted here for every typed event, before the Part check,
	// because an error event carries no part and can be the last thing a failed
	// run emits.
	if ev.Type != "" {
		if p.lastEventWasError {
			// The PREVIOUS error event was followed by this one, so it was
			// recovered from (whether or not this event is also an error).
			p.recoveredErrors++
		}
		// 2.2.x reports a recovery it retries as a `retry` event carrying the
		// error; it counts as recovered once something follows it.
		p.lastEventWasError = ev.Type == "error" || (ev.Type == "retry" && ev.Error != nil)
	}
	if ev.Part == nil {
		return
	}
	switch ev.Type {
	case "step_start":
		// A new step has begun, so the previous step is no longer the final one.
		// The final step of a 2.x session (the plain answer) emits no
		// step_finish, so without this reset the last TOOL step's "tool-calls"
		// would be reported as how the session ended. With it, a session whose
		// last step never finished on the stream reports no streamed reason, and
		// the export supplies the real one.
		p.lastReason = ""
		p.lastStepOut, p.lastStepReason = 0, 0
		p.lastStepZeroUsage = false
	case "text":
		if t := strings.TrimSpace(ev.Part.Text); t != "" {
			p.lastText = t
		}
	case "step_finish":
		// The reason rides on the same part as the usage. The LAST one wins: it
		// is the step the session's stream ended on, which is the step that says
		// whether a final answer was produced.
		if ev.Part.Reason != "" {
			p.lastReason = ev.Part.Reason
			// CLA-584: the stalled step's own figures move WITH the reason, so
			// the pair always describes ONE event. A reason-less step_finish is
			// skipped for this pair exactly as deadscan skips it for its own —
			// otherwise the previous step's "length" would be paired with THIS
			// event's zero output (or its output paired with the previous
			// reason), and the live adapter would resume a session the
			// retrospective scan reads as a mere long answer.
			p.lastStepOut, p.lastStepReason = 0, 0
		}
		// tokens and cost are siblings on the part; count each independently so
		// a step that reports one without the other still lands in the budget.
		//
		// The LAST step's OWN usage is tracked separately from the session sum:
		// the CLA-398 quiet-death signature is a final step that reported
		// NOTHING (all-zero), whatever the earlier steps cost — so the sums that
		// feed the budget are not the discriminator, the last step's own block
		// is. Reset per step; the final value is the one that survives.
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

// noteTool feeds one tool event into the shared clankerbar observation, which is
// what populates Result.Claim and the delivery Reports.
//
// Only a TERMINAL state is acted on for that observation, and that is
// FUTURE-PROOFING rather than a filter this stream needs today: `opencode run
// --format json` forwards a tool part only once it has settled (true of 1.x
// source, cli/cmd/run.ts, and of every 2.0.32 stream recorded for this adapter),
// which is why the recorded fixtures carry exactly one event per tool part. The
// check is here because the status is the ONLY error flag
// opencode gives — there is no is_error sibling — so a version that started
// forwarding in-flight parts would otherwise have us read a call that had not
// returned as a success. An unrecognised status is ignored for the same reason:
// the cost of being wrong that way is an unobserved claim (the driver hands the
// task back — one expiring lease), against a claim recorded for a call that
// failed, which is a task the driver believes it holds and does not.
//
// A part still in flight is not a claim, but it DOES open a tool call for the
// idle watchdog: the session is working rather than hanging, and the call's own
// cap bounds it (see idleGuard).
//
// A tool part is acted on ONCE. noteClaimed rebuilds Result.Claim wholesale, so
// a re-delivered terminal event for a claim already settled by a later
// a status write would resurrect it — and the driver would post `ready` over a task
// that is in review. This is insurance, not an observed bug.
//
// # What this shape cannot do, and what it costs
//
// claude sets Claim.HasWIP on the REQUEST (see noteToolUse), deliberately: a
// session killed between issuing `record_progress(branch: …)` and its result has
// still pushed work, and erring towards "there is WIP" only costs the reclaim an
// expiring lease already costs. opencode gives no request-side event at all — the
// arguments arrive fused to the result — so that arm cannot fire early here. A
// session killed mid-call (SIGTERM, a cancelled context) therefore records no
// WIP, and releaseHeldClaim posts `ready` over a branch that was pushed. The
// truncated-stream variant of this is caught by the capture's untrusted mark; the
// killed-mid-call one is not, and it is a property of opencode's stream rather
// than something this adapter can fix.
func (p *opencodeParse) noteTool(part *opencodePart) {
	st := part.State
	// part.Type ("tool") is deliberately not checked: only a tool part carries a
	// call id and a state, so the fields below are the discriminator.
	if st == nil || part.ID == "" {
		return
	}
	// The call's identity for the replay guard. Replays are recognised by the
	// part id, not the call id: the call id is the provider's and repeats across
	// steps (see opencodePart), so keying on it would drop a later, genuine call.
	key := part.PartID
	if key == "" {
		key = part.ID
	}
	// A part still in flight opens a tool call for the idle watchdog: the
	// session is working rather than hanging, and the call's own cap bounds it.
	// Keyed like the replay guard, so the running and settled events for one
	// call always name the same id.
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
	// CLA-739: the STATUS is the failure flag (see opencodeToolState), and the
	// error text rides where the output would have been.
	if p.observed.toolLoop.observe(part.Tool, st.Status == "error", st.Error) && p.kill != nil {
		fmt.Fprintf(p.diag(), "\n!! %s - killing the session here instead of letting it retry the same failing call\n", p.observed.toolLoop.tripped())
		p.kill()
	}
	name := opencodeClankerbarTool(part.Tool)
	if name == "" {
		return
	}

	noteToolUse(name, part.ID, st.Input, &p.observed)
	// The result text is handed over as JSON so it reaches noteClaimed by the same
	// road claude's string-shaped tool_result content does. Marshalling a string
	// cannot fail (invalid UTF-8 is replaced, not rejected), and the error is
	// swallowed rather than returned on purpose: bailing here would strand the
	// delivery Report noteToolUse just armed, unresolved and silently dropped.
	output := st.Output
	if full, ok, err := opencodeTruncatedOutput(st, p.toolOutputDir); err != nil {
		// Fail safe: the envelope goes on as the result, which no consumer can
		// parse, so a claim is NOT tracked - the same outcome as any unreadable
		// claim result, with the reason said first.
		fmt.Fprintf(p.diag(), "  · %s result was truncated by opencode and could not be read back: %v\n", part.Tool, err)
	} else if ok {
		output = full
	}
	content, _ := json.Marshal(output)
	noteToolResult(&p.observed, part.ID, st.Status == "error", content, p.diag())
}
