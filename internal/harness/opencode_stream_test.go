package harness

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// A session that emits nothing at all is a silent hang: the watchdog kills it
// and the cause reaches the Result.
func TestOpencodeIdleKillsASilentSession(t *testing.T) {
	opencodeStub(t, "#!/bin/sh\nexec sleep 30\n")

	start := time.Now()
	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:         "work",
		MaxSessionIdle: 200 * time.Millisecond,
		Console:        io.Discard,
	})
	if err != nil {
		t.Fatalf("Invoke returned an error for our own kill: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("Invoke took %s - the watchdog did not end the session; it ran to the stub's own sleep", elapsed)
	}
	if !strings.HasPrefix(res.IdleKilled, "no output for ") {
		t.Errorf("IdleKilled = %q, want the no-output cause", res.IdleKilled)
	}
	if res.ExitCode == 0 {
		t.Error("ExitCode = 0 - the child was not killed")
	}
}

// A long open tool call pauses the idle clock, so the TOOL CAP ends the session
// and the cause names the call, not silence.
func TestOpencodeIdlePausesForAToolCallAndTheToolCapKills(t *testing.T) {
	opencodeStub(t, `#!/bin/sh
echo '{"type":"tool_use","part":{"type":"tool","id":"call_1","tool":"bash","state":{"status":"running","input":{}}}}'
exec sleep 30
`)

	start := time.Now()
	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:              "work",
		MaxSessionIdle:      3 * time.Second, // wide: the stub must reach its tool line before this under -race load
		MaxToolCallDuration: 4 * time.Second,
		Console:             io.Discard,
	})
	if err != nil {
		t.Fatalf("Invoke returned an error for our own kill: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("Invoke took %s - the tool cap did not end the session", elapsed)
	}
	if res.IdleKilled != "tool call call_1 open for 4s" {
		t.Errorf("IdleKilled = %q, want the tool-cap cause naming the open call", res.IdleKilled)
	}
	if res.ExitCode == 0 {
		t.Error("ExitCode = 0 - the child was not killed")
	}
}

// The real order of a tool-bearing step, recorded against 2.0.32: step_start ->
// text (the model's message for the step, emitted when it ends) -> the tool runs
// -> tool_use(settled, with its own state.time start/end) -> step_finish
// (tool-calls). opencode emits no request-side tool event, so the silence of a
// long call sits between the step's early lines and its settled part - which is
// why the window spans the whole step, step_start until step_finish (CLA-900).
const (
	opencodeStepStartLine = `{"type":"step_start","sessionID":"ses_step","part":{"id":"prt_1","messageID":"msg_1","type":"step-start"}}`
	opencodeStepTextLine  = `{"type":"text","sessionID":"ses_step","part":{"type":"text","text":"Re-run triggered. Blocking on it with a bounded wait:"}}`
	opencodeSettledTool   = `{"type":"tool_use","sessionID":"ses_step","part":{"partID":"prt_2","type":"tool","id":"call_1","tool":"bash","state":{"status":"completed","input":{},"output":"ok"}}}`
	opencodeToolStepEnd   = `{"type":"step_finish","sessionID":"ses_step","part":{"type":"step-finish","reason":"tool-calls","tokens":{"total":10,"input":5,"output":5,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.01}}`
)

// CLA-900: a step is silent for as long as its tool runs, and the incident's
// stream emitted a text line ("Blocking on it with a bounded wait:") before the
// silence. Silence past the idle bound but under the tool cap must NOT be killed:
// that is the CI wait this task is about, and neither the text nor any other line
// inside the step closes the window - only step_finish does.
func TestOpencodeStepStartPausesTheIdleClock(t *testing.T) {
	opencodeStub(t, `#!/bin/sh
echo '`+opencodeStepStartLine+`'
sleep 0.2
echo '`+opencodeStepTextLine+`'
sleep 4
echo '`+opencodeSettledTool+`'
echo '`+opencodeToolStepEnd+`'
`)

	start := time.Now()
	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:              "work",
		MaxSessionIdle:      3 * time.Second, // past this, and below the tool cap
		MaxToolCallDuration: 15 * time.Second,
		Console:             io.Discard,
	})
	if err != nil {
		t.Fatalf("Invoke returned an error for a session that was not killed: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 3*time.Second {
		t.Fatalf("Invoke took %s - it never reached the silence this test is about", elapsed)
	}
	if res.IdleKilled != "" {
		t.Errorf("IdleKilled = %q - the step window did not pause the idle clock, so an honest wait inside one tool call was killed as a hang", res.IdleKilled)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0 - the session should have run to its own exit", res.ExitCode)
	}
	if got := strings.TrimSpace(res.FinalMessage); got != "Re-run triggered. Blocking on it with a bounded wait:" {
		t.Errorf("FinalMessage = %q - the step text was never parsed, so the kill landed before the output this test is about", got)
	}
}

// CLA-900: the step window is still BOUNDED - a tool that never returns must be
// killed by the tool cap, with a cause that names the cap rather than silence,
// even after the step's text line has been emitted.
func TestOpencodeStepStartWindowIsBoundedByTheToolCap(t *testing.T) {
	opencodeStub(t, `#!/bin/sh
echo '`+opencodeStepStartLine+`'
echo '`+opencodeStepTextLine+`'
exec sleep 30
`)

	start := time.Now()
	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:              "work",
		MaxSessionIdle:      5 * time.Second, // wide: the stub must reach its step line before this under -race load
		MaxToolCallDuration: 2 * time.Second,
		Console:             io.Discard,
	})
	if err != nil {
		t.Fatalf("Invoke returned an error for our own kill: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("Invoke took %s - the tool cap did not end the session", elapsed)
	}
	if want := "tool call " + idleStepCall + " open for 2s"; res.IdleKilled != want {
		t.Errorf("IdleKilled = %q, want the tool-cap cause %q naming the open step", res.IdleKilled, want)
	}
	if !res.IdleToolCall {
		t.Error("IdleToolCall = false - a tool-cap kill must be marked as landing on an open call")
	}
	if res.ExitCode == 0 {
		t.Error("ExitCode = 0 - the child was not killed")
	}
}

// CLA-900: step_finish closes the window and restarts the idle clock, so a
// session that ends a step and then goes quiet is killed for idle rather than
// waiting out the much longer tool cap. That silence is the model's next request
// - a provider question, never a tool-cap one.
func TestOpencodeOutputAfterTheStepResumesTheIdleClock(t *testing.T) {
	opencodeStub(t, `#!/bin/sh
echo '`+opencodeStepStartLine+`'
sleep 0.5
echo '`+opencodeStepTextLine+`'
echo '`+opencodeToolStepEnd+`'
exec sleep 30
`)

	start := time.Now()
	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:              "work",
		MaxSessionIdle:      2 * time.Second,
		MaxToolCallDuration: 20 * time.Second,
		Console:             io.Discard,
	})
	if err != nil {
		t.Fatalf("Invoke returned an error for our own kill: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Invoke took %s - the idle clock did not resume after step_finish; the step window stayed open until the tool cap", elapsed)
	}
	if res.IdleKilled != "no output for 2s" {
		t.Errorf("IdleKilled = %q, want the idle cause - step_finish must close the window", res.IdleKilled)
	}
	if got := strings.TrimSpace(res.FinalMessage); got != "Re-run triggered. Blocking on it with a bounded wait:" {
		t.Errorf("FinalMessage = %q - the text event never parsed, so the kill landed before the output this test is about", got)
	}
	if res.IdleToolCall {
		t.Error("IdleToolCall = true - this kill was the idle clock after the step window was closed, not the tool cap")
	}
}

// CLA-900: the gap AFTER step_finish is the model's next request, not a tool.
// step_finish must not open a window, or a provider that stops answering is
// filed as a stuck tool call - and with a cap wider than the idle bound the kill
// would not even happen.
func TestOpencodeStepFinishLeavesTheIdleClockArmed(t *testing.T) {
	opencodeStub(t, `#!/bin/sh
echo '`+opencodeStepStartLine+`'
echo '`+opencodeSettledTool+`'
echo '`+opencodeToolStepEnd+`'
exec sleep 30
`)

	start := time.Now()
	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:              "work",
		MaxSessionIdle:      2 * time.Second,
		MaxToolCallDuration: 20 * time.Second,
		Console:             io.Discard,
	})
	if err != nil {
		t.Fatalf("Invoke returned an error for our own kill: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Invoke took %s - step_finish opened a window and left the session on the tool cap", elapsed)
	}
	if res.IdleKilled != "no output for 2s" {
		t.Errorf("IdleKilled = %q, want the idle cause: a death after step_finish is the model wait", res.IdleKilled)
	}
	if res.IdleToolCall {
		t.Error("IdleToolCall = true - step_finish must not open a tool window; no tool was running")
	}
}

// A session that finishes on its own is unaffected by the watchdog.
func TestOpencodeIdleLeavesAWorkingSessionAlone(t *testing.T) {
	opencodeStub(t, `#!/bin/sh
echo '{"type":"text","part":{"type":"text","text":"done"}}'
echo '{"type":"step_finish","part":{"type":"step-finish","reason":"stop","tokens":{"total":10,"input":5,"output":5,"reasoning":0,"cache":{"write":0,"read":0}}}}'
`)

	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:         "work",
		MaxSessionIdle: 2 * time.Second,
		Console:        io.Discard,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.IdleKilled != "" {
		t.Errorf("IdleKilled = %q on a working session, want none", res.IdleKilled)
	}
	if res.ExitCode != 0 || res.FinalMessage != "done" {
		t.Errorf("ExitCode = %d, FinalMessage = %q, want 0 / %q", res.ExitCode, res.FinalMessage, "done")
	}
}

// A probe is a one-word liveness check, not a session doing work: it never arms
// the watchdog even when the invocation carries the bounds.
func TestOpencodeIdleNeverArmsOnAProbe(t *testing.T) {
	opencodeStub(t, `#!/bin/sh
sleep 0.4
echo '{"type":"text","part":{"type":"text","text":"alive"}}'
`)

	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:         ".",
		Probe:          true,
		MaxSessionIdle: 50 * time.Millisecond,
		Console:        io.Discard,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.IdleKilled != "" {
		t.Errorf("IdleKilled = %q - a probe was killed by a watchdog it must never arm", res.IdleKilled)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
}
