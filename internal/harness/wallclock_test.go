package harness

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// opencodeStub writes an executable `opencode` onto PATH and returns nothing:
// every test here needs the same shape, and the differences are all in the
// script. The stub emits real opencode 1.18.2 event lines so the usage summed
// before a kill is the honest figure, not a fixture.
func opencodeStub(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A session that spends its usage and then keeps running: the cap fires, the
// process is reaped, and Invoke returns an ORDERLY end — no error, a marker the
// driver can classify, and the spend seen before the kill still on the Result.
//
// The last of those is the one that would rot silently: p.finish rewrites Raw
// wholesale when the stream carried usage, so a marker written before it (or a
// marker that replaced Raw, as the claude ceiling kill does) would leave the
// budget looking at a session that cost nothing.
func TestOpencodeInvokeEndsASessionThatOutlivesItsWallClockCap(t *testing.T) {
	opencodeStub(t, `#!/bin/sh
echo '{"type":"step_finish","part":{"type":"step-finish","reason":"stop","tokens":{"total":1200,"input":200,"output":1000,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.5}}'
exec sleep 30
`)

	start := time.Now()
	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:              "work",
		MaxSessionWallClock: time.Second,
		Console:             io.Discard,
	})
	if err != nil {
		t.Fatalf("Invoke returned an error for our own cap: %v — the cap must read as an orderly end, not a launch failure", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("Invoke took %s — the cap did not end the session; it ran to the stub's own sleep", elapsed)
	}
	if !(opencode{}).WallClockCapped(res) {
		t.Fatal("the Result does not carry the wall-clock marker through Invoke, so the driver would read the kill as a plain failure and stop the run")
	}
	if res.ExitCode == 0 {
		t.Error("ExitCode = 0 — the child was not killed; the cap is inert")
	}
	if res.Tokens != 1200 || res.CostUSD != 0.5 {
		t.Errorf("Tokens = %d, CostUSD = %v; want 1200 / 0.5 — the spend up to the kill must still reach the budget", res.Tokens, res.CostUSD)
	}
	if res.Untrusted != "" {
		t.Errorf("Untrusted = %q: our own kill is not an unreadable stream", res.Untrusted)
	}
}

// The same seam driven the other way: a session that finishes inside its cap is
// an ordinary clean end, so the cap cannot fire on a session that already ended.
func TestOpencodeInvokeLeavesASessionInsideItsCapAlone(t *testing.T) {
	opencodeStub(t, `#!/bin/sh
echo '{"type":"text","part":{"type":"text","text":"done"}}'
echo '{"type":"step_finish","part":{"type":"step-finish","reason":"stop","tokens":{"total":10,"input":5,"output":5,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.01}}'
`)

	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:              "work",
		MaxSessionWallClock: 30 * time.Second,
		Console:             io.Discard,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if (opencode{}).WallClockCapped(res) {
		t.Error("a session that finished inside its cap was marked as capped")
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if res.FinalMessage != "done" {
		t.Errorf("FinalMessage = %q, want %q — the cap must not disturb ordinary parsing", res.FinalMessage, "done")
	}
}

// Off by default, and "off" has to mean NO deadline rather than a small one:
// a session that outlives what a default might plausibly have been still runs
// to its own end when the operator set nothing (CLA-368 ships the dial
// disabled, because no default number is honest across models and providers).
func TestOpencodeWallClockCapIsOffByDefault(t *testing.T) {
	opencodeStub(t, `#!/bin/sh
sleep 0.4
echo '{"type":"text","part":{"type":"text","text":"slow but finished"}}'
`)

	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:  "work",
		Console: io.Discard,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if (opencode{}).WallClockCapped(res) {
		t.Error("an uncapped session was marked as wall-clock capped — the zero value grew a deadline")
	}
	if res.ExitCode != 0 || res.FinalMessage != "slow but finished" {
		t.Errorf("ExitCode = %d, FinalMessage = %q — an uncapped session must run to its own end", res.ExitCode, res.FinalMessage)
	}
}

// A probe is a one-word liveness check, not a session doing work. Capping one
// would report a phase-shaped cap for a phase that never ran, so the adapter
// refuses to apply the cap there even when the field is set.
func TestOpencodeWallClockCapNeverAppliesToAProbe(t *testing.T) {
	opencodeStub(t, `#!/bin/sh
sleep 0.3
echo '{"type":"text","part":{"type":"text","text":"alive"}}'
`)

	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:              ".",
		Probe:               true,
		MaxSessionWallClock: 50 * time.Millisecond,
		Console:             io.Discard,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if (opencode{}).WallClockCapped(res) {
		t.Error("a probe was reported as a wall-clock-capped SESSION")
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0 — the probe was killed by a cap that is not meant to reach it", res.ExitCode)
	}
}

// The caller's own cancellation — Ctrl-C, SIGTERM, a supervised wait ending —
// is NOT this phase reaching its backstop. Reporting it as one would tell the
// driver to treat an interrupted run as an orderly phase end and carry on.
func TestOpencodeCallerCancellationIsNotAWallClockCap(t *testing.T) {
	opencodeStub(t, `#!/bin/sh
exec sleep 30
`)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	res, err := (opencode{}).Invoke(ctx, Invocation{
		Prompt:              "work",
		MaxSessionWallClock: 30 * time.Second,
		Console:             io.Discard,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if (opencode{}).WallClockCapped(res) {
		t.Error("a caller-cancelled session was reported as wall-clock capped — the driver would read an interrupted run as an orderly phase end")
	}
}

// The sibling of TestAdaptersWithoutATurnCapNeverReportOne: an adapter that
// enforces no wall-clock cap must never classify an exit as one, whatever a
// task's own text managed to put on the Result.
func TestAdaptersWithoutAWallClockCapNeverReportOne(t *testing.T) {
	res := Result{
		ExitCode: 1,
		Stdout:   "wall_clock_capped",
		Raw:      map[string]any{"terminal_reason": "wall_clock_capped"},
	}
	if (claude{}).WallClockCapped(res) {
		t.Error("claude reported a wall-clock cap it does not enforce")
	}
	if (codex{}).WallClockCapped(res) {
		t.Error("codex reported a wall-clock cap it does not enforce")
	}
}

// Capabilities is what doctor reads to tell an operator whether the dial they
// set can fire at all, so the pairing is pinned rather than left to drift: the
// harness with no turn flag is exactly the one that enforces the time cap.
func TestOnlyOpencodeHonoursTheSessionWallClock(t *testing.T) {
	for _, tc := range []struct {
		a          Adapter
		wantWall   bool
		wantTurns  bool
		nameOfCase string
	}{
		{a: opencode{}, wantWall: true, wantTurns: false, nameOfCase: "opencode: time cap, no turn flag"},
		{a: claude{}, wantWall: false, wantTurns: true, nameOfCase: "claude: turn flag, no time cap"},
		{a: codex{}, wantWall: false, wantTurns: false, nameOfCase: "codex: neither"},
	} {
		t.Run(tc.nameOfCase, func(t *testing.T) {
			caps := tc.a.Capabilities()
			if caps.HonoursSessionWallClock != tc.wantWall {
				t.Errorf("HonoursSessionWallClock = %v, want %v", caps.HonoursSessionWallClock, tc.wantWall)
			}
			if caps.HonoursMaxTurns != tc.wantTurns {
				t.Errorf("HonoursMaxTurns = %v, want %v", caps.HonoursMaxTurns, tc.wantTurns)
			}
		})
	}
}

// The kill has to end the WAIT, not merely the child. cmd.Stdout is an
// io.MultiWriter, so os/exec allocates its own pipe and cmd.Run blocks until
// every writer of it closes — and CommandContext signals the direct child only.
// A runaway session is exactly the one with a bash-tool grandchild (a build, a
// test run) holding the inherited fd, so without cmd.WaitDelay the cap would
// hang precisely where it is needed. The stub reproduces that shape: a
// backgrounded child that outlives its parent and keeps stdout open.
//
// It does NOT assert the orphan dies — it does not, today; killing the process
// GROUP is platform-specific and filed separately. What is pinned here is that
// the adapter returns rather than waiting on it.
func TestOpencodeWallClockKillDoesNotHangOnAGrandchildHoldingTheStream(t *testing.T) {
	opencodeStub(t, `#!/bin/sh
echo '{"type":"text","part":{"type":"text","text":"working"}}'
sleep 30 &
exec sleep 30
`)

	start := time.Now()
	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:              "work",
		MaxSessionWallClock: 500 * time.Millisecond,
		Console:             io.Discard,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if elapsed > 20*time.Second {
		t.Fatalf("Invoke blocked for %s: it waited on the grandchild's copy of the stream, so a capped runaway is not actually capped", elapsed)
	}
	if !(opencode{}).WallClockCapped(res) {
		t.Error("the session was not reported as wall-clock capped")
	}
}
