package loop

// Scheduled pause windows (config.PauseWindows): during a configured local-time
// window the loop starts no new sessions — checked between iterations like the
// console pause, and at every phase boundary (CLA-906), so a running session
// always finishes while the next one waits. The poll-gate tests are here; the
// boundary hold is in pause_boundary_test.go.

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
	if len(sent) == 1 {
		// The reason and the resume instant ride the same beacon (CLA-908), so
		// the console can name the schedule instead of showing bare draining.
		if sent[0].State.PauseReason != fleet.PauseReasonSchedule {
			t.Errorf("poll beacon pause reason = %q, want %q", sent[0].State.PauseReason, fleet.PauseReasonSchedule)
		}
		if want := brisbaneAt(t, 9, 14, 0); !sent[0].State.PauseUntil.Equal(want) {
			t.Errorf("poll beacon pause until = %v, want the window's end %v", sent[0].State.PauseUntil, want)
		}
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

// The schedule's presence payload (CLA-908): a window-paused target says WHY and
// UNTIL WHEN, so the console's banner can name the resume time instead of showing
// the same bare `draining` a console or fleet pause shows. A pause the operator
// has to act on takes precedence, and nothing paused is unchanged.
func TestFleetState_ScheduledPauseCarriesReasonAndUntil(t *testing.T) {
	at := brisbaneAt(t, 9, 12, 0)
	d, _, _, _ := pauseDriver(t, peakSchedule(t), &at)
	until := brisbaneAt(t, 9, 14, 0)

	d.schedPaused = true
	got := d.fleetState(0)
	if got.Kind != fleet.StateDraining {
		t.Fatalf("scheduled-pause state = %q, want draining", got.Kind)
	}
	if got.PauseReason != fleet.PauseReasonSchedule {
		t.Errorf("pause reason = %q, want %q so the console can name the schedule", got.PauseReason, fleet.PauseReasonSchedule)
	}
	if !got.PauseUntil.Equal(until) {
		t.Errorf("pause until = %v, want the window's end %v", got.PauseUntil, until)
	}
	if got.PauseZone != "Australia/Brisbane" {
		t.Errorf("pause zone = %q, want the schedule's own zone Australia/Brisbane", got.PauseZone)
	}

	// A console pause is the cause the operator has to act on: it wins, and it
	// names itself so the console renders "paused by console" rather than the
	// schedule's window.
	d.paused[0] = true
	if got := d.fleetState(0); got.Kind != fleet.StateDraining || got.PauseReason != fleet.PauseReasonConsole || !got.PauseUntil.IsZero() {
		t.Errorf("console pause + schedule = %+v, want draining with reason %q and no end", got, fleet.PauseReasonConsole)
	}
	d.paused[0] = false

	// So does a fleet pause (a raised dead-phase question awaits an answer).
	d.fleetPaused[0] = true
	if got := d.fleetState(0); got.Kind != fleet.StateDraining || got.PauseReason != fleet.PauseReasonFleet {
		t.Errorf("fleet pause + schedule = %+v, want draining with reason %q", got, fleet.PauseReasonFleet)
	}
	d.fleetPaused[0] = false

	// A window that has ended leaves plain draining: schedPaused is bookkeeping
	// updated at the next gate, and a reason with no end would keep a banner up.
	at = brisbaneAt(t, 9, 14, 30)
	if got := d.fleetState(0); got.Kind != fleet.StateDraining || got.PauseReason != "" {
		t.Errorf("stale schedule flag = %+v, want plain draining once the window has ended", got)
	}

	// Nothing paused: unchanged.
	d.schedPaused = false
	if got := d.fleetState(0); got.Kind != fleet.StateIdle {
		t.Errorf("no pause state = %v, want idle", got)
	}

	// An in-flight session still outranks the window: the pause holds the NEXT
	// spawn, so the Fleet page keeps showing the iteration while work is running.
	at = brisbaneAt(t, 9, 12, 0)
	d.schedPaused = true
	d.iter = []iterState{{on: true, n: 3, ref: "CB-7", phase: "implement"}}
	got = d.fleetState(0)
	if got.Kind != fleet.StateIteration || got.N != 3 || got.TaskRef != "CB-7" || got.Phase != "implement" {
		t.Errorf("mid-drain during a window = %+v, want iteration{n:3 taskRef:CB-7 phase:implement}", got)
	}
}

// A target held at a phase boundary (CLA-906) reports the pause, not the open
// iteration: the walk's iteration names the phase that just ENDED, while the
// console needs the task and the phase waiting behind the window.
func TestFleetState_HeldPhaseOutranksTheIterationMarker(t *testing.T) {
	at := brisbaneAt(t, 9, 12, 0)
	d, _, _, _ := pauseDriver(t, peakSchedule(t), &at)
	d.schedPaused = true
	d.iter = []iterState{{on: true, n: 3, ref: "CB-7", phase: "implement"}}
	d.holdPause(0, "CB-7", "review", time.Time{}, "")

	got := d.fleetState(0)
	if got.Kind != fleet.StateDraining || got.PauseReason != fleet.PauseReasonSchedule {
		t.Fatalf("held state = %+v, want draining with the schedule reason", got)
	}
	if got.TaskRef != "CB-7" || got.Phase != "review" {
		t.Errorf("held state names %q at %q, want CB-7 at the HELD phase review", got.TaskRef, got.Phase)
	}

	// The clear is what the hold's defer does: the next beacon is the iteration
	// again, not a pause stuck on the row.
	d.clearHoldPause(0)
	if got := d.fleetState(0); got.Kind != fleet.StateIteration || got.Phase != "implement" {
		t.Errorf("after the hold = %+v, want the iteration back", got)
	}
}
