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
