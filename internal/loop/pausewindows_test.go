package loop

// Scheduled pause windows (config.PauseWindows): during a configured local-time
// window the loop starts no new sessions — checked between iterations exactly
// like the console pause, so an in-flight session or sequence always finishes.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/fleet"
)

// peakSchedule is a validated window in a zone without DST, so a pinned wall
// clock means one thing all year: mon-fri 11:00-14:00 local.
func peakSchedule(t *testing.T) *config.PauseWindows {
	t.Helper()
	pw := &config.PauseWindows{
		Timezone: "Australia/Brisbane",
		Windows:  []config.PauseWindow{{Days: "mon-fri", From: "11:00", To: "14:00"}},
	}
	if err := pw.Validate(); err != nil {
		t.Fatalf("fixture pause windows do not validate: %v", err)
	}
	return pw
}

// brisbaneAt is a wall-clock instant in the window's zone. 2026-11-09 is a
// Monday, so 12:00 is inside peakSchedule's window and 15:30 is outside it.
func brisbaneAt(t *testing.T, d, hh, mm int) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("Australia/Brisbane")
	if err != nil {
		t.Fatalf("load Australia/Brisbane: %v", err)
	}
	return time.Date(2026, time.November, d, hh, mm, 0, 0, loc)
}

// pauseDriver is the smallest Driver the pause gate needs: one polled target
// with work to spawn, a fake fleet to see the beacons, a real state dir for the
// idle wait's marker reads, and a clock the test moves by hand.
func pauseDriver(t *testing.T, pw *config.PauseWindows, at *time.Time) (*Driver, *fakeAdapter, *fakePoller, *fakeFleet) {
	t.Helper()
	cfg := fastCfg()
	cfg.PauseWindows = pw
	h := &fakeAdapter{}
	p := &fakePoller{sum: backlog.Summary{Ready: 2, Claimable: 2}}
	ff := &fakeFleet{}
	d := NewMulti(cfg, h, []Target{{Poller: p, Fleet: ff}})
	openTestStateDir(t, d)
	d.now = func() time.Time { return *at }
	return d, h, p, ff
}

func pick(t *testing.T, d *Driver) (Target, runNext) {
	t.Helper()
	target, next, err := d.pickTarget(context.Background(), time.Millisecond)
	if err != nil {
		t.Fatalf("pickTarget: %v", err)
	}
	return target, next
}

func TestPickTarget_ScheduledPauseHoldsSpawnsInsideAWindow(t *testing.T) {
	at := brisbaneAt(t, 9, 12, 0)
	d, h, p, ff := pauseDriver(t, peakSchedule(t), &at)
	logs := captureLogs(t)

	if _, next := pick(t, d); next != runContinue {
		t.Errorf("inside a window pickTarget = %v, want runContinue (idle-poll, no session)", next)
	}
	if h.invokeCalls != 0 {
		t.Errorf("a scheduled pause must spawn nothing; got %d Invoke calls", h.invokeCalls)
	}
	if p.calls != 1 {
		t.Errorf("a scheduled pause must still poll the queue; got %d polls", p.calls)
	}
	sent, _ := ff.reports()
	if len(sent) != 1 || sent[0].State.Kind != fleet.StateDraining {
		t.Errorf("the poll must still beacon, and report draining while the window holds spawns; got %+v", sent)
	}
	if !d.schedPaused {
		t.Error("schedPaused should be set while a window is active")
	}
	// The resume time is the window's END, on the operator's local clock.
	want := "scheduled pause active until Mon 14:00 AEST (mon-fri 11:00-14:00 (Australia/Brisbane))"
	if out := logs.String(); !strings.Contains(out, want) {
		t.Errorf("entry line missing %q; log was:\n%s", want, out)
	}
}

func TestPickTarget_ScheduledPauseLetsTheSameQueueThroughAfterTheWindow(t *testing.T) {
	at := brisbaneAt(t, 9, 12, 0)
	d, _, p, _ := pauseDriver(t, peakSchedule(t), &at)
	if _, next := pick(t, d); next != runContinue {
		t.Fatalf("inside the window pickTarget = %v, want runContinue", next)
	}

	at = brisbaneAt(t, 9, 15, 30) // past 14:00
	if _, next := pick(t, d); next != runFall {
		t.Errorf("after the window the same spawnable queue must be picked; got %v, want runFall", next)
	}
	if p.calls != 2 {
		t.Errorf("expected one poll per pass; got %d", p.calls)
	}
	if d.schedPaused {
		t.Error("schedPaused should be clear once the window has ended")
	}
}

func TestScheduledPause_LogsEachTransitionOnce(t *testing.T) {
	at := brisbaneAt(t, 9, 12, 0)
	d, _, _, _ := pauseDriver(t, peakSchedule(t), &at)
	logs := captureLogs(t)

	pick(t, d) // enter
	pick(t, d) // still inside: nothing new to say
	at = brisbaneAt(t, 9, 15, 30)
	pick(t, d) // exit
	pick(t, d) // still outside: nothing new to say

	out := logs.String()
	if got := strings.Count(out, "scheduled pause active until"); got != 1 {
		t.Errorf("entry logged %d times, want once; log was:\n%s", got, out)
	}
	if got := strings.Count(out, "scheduled pause ended — resuming"); got != 1 {
		t.Errorf("exit logged %d times, want once; log was:\n%s", got, out)
	}
}

func TestPickTarget_NoPauseWindowsLeavesTheGateUnchanged(t *testing.T) {
	at := brisbaneAt(t, 9, 12, 0)
	d, _, _, _ := pauseDriver(t, nil, &at)
	logs := captureLogs(t)

	if _, next := pick(t, d); next != runFall {
		t.Errorf("with no pause_windows the spawnable queue must be picked; got %v, want runFall", next)
	}
	if d.schedPaused {
		t.Error("schedPaused must stay clear with no schedule configured")
	}
	if out := logs.String(); strings.Contains(out, "scheduled pause") {
		t.Errorf("no schedule, no pause line; log was:\n%s", out)
	}
}

func TestPickTarget_BlindScheduledPauseChoosesNoTarget(t *testing.T) {
	at := brisbaneAt(t, 9, 12, 0)
	d, _, p, _ := pauseDriver(t, peakSchedule(t), &at)
	d.blind = true

	target, next := pick(t, d)
	if next != runContinue {
		t.Errorf("blind inside a window pickTarget = %v, want runContinue", next)
	}
	if target.Poller != nil {
		t.Error("a scheduled pause must choose no target in blind mode")
	}

	at = brisbaneAt(t, 9, 15, 30)
	target, next = pick(t, d)
	if next != runFall || target.Poller != p {
		t.Errorf("after the window blind mode must rotate to the polled target; got %v, target poller %v", next, target.Poller)
	}
}

// TestPickTarget_ScheduledPauseDefersTheDrainVerdict pins the guard around
// judgeProgress. A poll taken while the loop has stopped spawning cannot tell
// "our last drain settled nothing" from "nothing happened because we stopped",
// so the verdict has to survive the window and land on the first live poll.
func TestPickTarget_ScheduledPauseDefersTheDrainVerdict(t *testing.T) {
	at := brisbaneAt(t, 9, 12, 0)
	d, _, _, _ := pauseDriver(t, peakSchedule(t), &at)

	// A drain just finished, settled nothing, and spent 90M — over
	// quietTokenThreshold on its own, so judging it now would back the target off.
	d.pending[0] = true
	d.spent[0] = 90_000_000

	if _, next := pick(t, d); next != runContinue {
		t.Fatalf("inside a window pickTarget = %v, want runContinue (idle-poll)", next)
	}
	if !d.pending[0] {
		t.Error("the pause judged the last drain; the verdict must wait for a live poll")
	}
	if d.quietTokens[0] != 0 || !d.skipUntil[0].IsZero() {
		t.Errorf("nothing may be charged while paused; quietTokens=%d skipUntil=%v", d.quietTokens[0], d.skipUntil[0])
	}
	if d.spent[0] != 90_000_000 {
		t.Errorf("the drain's spend must survive for its verdict; spent=%d", d.spent[0])
	}

	at = brisbaneAt(t, 9, 15, 30) // past the window's 14:00 end
	pick(t, d)
	if d.pending[0] {
		t.Error("the first live poll must land the outstanding verdict")
	}
	if d.quietTokens[0] != 90_000_000 {
		t.Errorf("the deferred drain's spend must be charged now; quietTokens=%d, want 90000000", d.quietTokens[0])
	}
	if !d.backedOff(0) {
		t.Error("90M of fruitless spend must back the target off on the poll that finally judges it")
	}
}

func TestFleetState_ReportsDrainingDuringAScheduledPause(t *testing.T) {
	at := brisbaneAt(t, 9, 12, 0)
	d, _, _, _ := pauseDriver(t, peakSchedule(t), &at)

	d.schedPaused = true
	if got := d.fleetState(0); got.Kind != fleet.StateDraining {
		t.Errorf("scheduled-pause state = %v, want draining", got)
	}
	// An in-flight session outranks the window: the pause holds the NEXT spawn.
	d.iter = []iterState{{on: true, n: 3, ref: "CB-7", phase: "implement"}}
	got := d.fleetState(0)
	if got.Kind != fleet.StateIteration || got.N != 3 || got.TaskRef != "CB-7" || got.Phase != "implement" {
		t.Errorf("mid-drain during a window = %+v, want iteration{n:3 taskRef:CB-7 phase:implement}", got)
	}
}
