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

// CLA-900: `opencode run --format json` emits no request-side tool event - a
// tool part arrives only once it has SETTLED - so the synthetic open call a
// step_finish(tool-calls) handoff creates is the only thing that pauses the idle
// clock for a long call. Silence past the idle bound but under the tool cap must
// NOT be killed: that is a CI wait or a test run, exactly what used to die.
func TestOpencodeStepFinishHandoffPausesTheIdleClock(t *testing.T) {
	opencodeStub(t, `#!/bin/sh
echo '{"type":"step_finish","sessionID":"ses_handoff","part":{"type":"step-finish","reason":"tool-calls","tokens":{"total":10,"input":5,"output":5,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.01}}'
sleep 4
echo '{"type":"step_finish","sessionID":"ses_handoff","part":{"type":"step-finish","reason":"stop","tokens":{"total":10,"input":5,"output":5,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.01}}'
`)

	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:              "work",
		MaxSessionIdle:      3 * time.Second,
		MaxToolCallDuration: 15 * time.Second,
		Console:             io.Discard,
	})
	if err != nil {
		t.Fatalf("Invoke returned an error for a session that was not killed: %v", err)
	}
	if res.IdleKilled != "" {
		t.Errorf("IdleKilled = %q - a step_finish(tool-calls) handoff did not pause the idle clock, so an honest wait was killed as a hang", res.IdleKilled)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0 - the session should have run to its own exit", res.ExitCode)
	}
}

// CLA-900: the handoff is still BOUNDED - a call that never returns must be
// killed by the tool cap, with a cause that names the cap rather than silence.
func TestOpencodeStepFinishHandoffIsBoundedByTheToolCap(t *testing.T) {
	opencodeStub(t, `#!/bin/sh
echo '{"type":"step_finish","sessionID":"ses_handoff","part":{"type":"step-finish","reason":"tool-calls","tokens":{"total":10,"input":5,"output":5,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.01}}'
exec sleep 30
`)

	start := time.Now()
	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:              "work",
		MaxSessionIdle:      5 * time.Second, // wide: the stub must reach its tool line before this under -race load
		MaxToolCallDuration: 2 * time.Second,
		Console:             io.Discard,
	})
	if err != nil {
		t.Fatalf("Invoke returned an error for our own kill: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("Invoke took %s - the tool cap did not end the session", elapsed)
	}
	if res.IdleKilled != "tool call "+idleHandoffCall+" open for 2s" {
		t.Errorf("IdleKilled = %q, want the tool-cap cause naming the synthetic call", res.IdleKilled)
	}
	if !res.IdleToolCall {
		t.Error("IdleToolCall = false - a tool-cap kill must be marked as landing on an open call")
	}
	if res.ExitCode == 0 {
		t.Error("ExitCode = 0 - the child was not killed")
	}
}

// CLA-900: output after the handoff closes the synthetic call and restarts the
// idle clock, so a session that is alive but then goes quiet is still killed for
// idle rather than waiting out the much longer tool cap.
func TestOpencodeOutputAfterTheHandoffResumesTheIdleClock(t *testing.T) {
	opencodeStub(t, `#!/bin/sh
echo '{"type":"step_finish","sessionID":"ses_handoff","part":{"type":"step-finish","reason":"tool-calls","tokens":{"total":10,"input":5,"output":5,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.01}}'
sleep 0.5
echo '{"type":"text","sessionID":"ses_handoff","part":{"type":"text","text":"still working"}}'
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
		t.Fatalf("Invoke took %s - the idle clock did not resume after the output; the synthetic call stayed open until the tool cap", elapsed)
	}
	if res.IdleKilled != "no output for 2s" {
		t.Errorf("IdleKilled = %q, want the idle cause - output after the handoff must close the synthetic call", res.IdleKilled)
	}
	if res.FinalMessage != "still working" {
		t.Errorf("FinalMessage = %q - the text event never parsed, so the kill landed before the output this test is about", res.FinalMessage)
	}
	if res.IdleToolCall {
		t.Error("IdleToolCall = true - this kill was the idle clock after the call was closed, not the tool cap")
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
