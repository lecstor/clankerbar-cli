package loop

// CLA-906: a scheduled pause window holds a phase sequence at its next boundary
// instead of letting the remaining phases run through peak hours. These tests
// drive the whole drain so the seam, the lease renewal and the stop path are the
// production ones; the clock is the same d.now seam the poll gate uses.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// pauseHoldClock is a clock the test moves from the test goroutine while the
// drain runs on another: the window can open mid-session without racing d.now.
type pauseHoldClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *pauseHoldClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *pauseHoldClock) Set(t time.Time) {
	c.mu.Lock()
	c.at = t
	c.mu.Unlock()
}

// beatReleaser is fakeReleaser plus the lease renewal the hold needs, recording
// every run id it was asked to renew. err, when set, is what each renewal
// answers — the seam the hold's failure branches are driven through.
type beatReleaser struct {
	fakeReleaser
	mu    sync.Mutex
	beats []string
	err   error
}

func (b *beatReleaser) Heartbeat(_ context.Context, runID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.beats = append(b.beats, runID)
	return b.err
}

func (b *beatReleaser) beatSnapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.beats...)
}

// pauseClaimReleaser is claimingReleaser plus Heartbeat: the shape a
// runner-claimed drain's hold beats on.
type pauseClaimReleaser struct {
	claimingReleaser
	mu    sync.Mutex
	beats []string
}

func (p *pauseClaimReleaser) Heartbeat(_ context.Context, runID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.beats = append(p.beats, runID)
	return nil
}

func (p *pauseClaimReleaser) beatSnapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.beats...)
}

// pauseBoundaryDriver is the two-phase fixture with a moveable clock and a
// renewal cadence a test can wait out. Two milliseconds keeps every wait and
// beat real (the underlying wait still sleeps real time) without making the
// suite slow.
func pauseBoundaryDriver(t *testing.T, h harness.Adapter, rel plane.Releaser, clk *pauseHoldClock, pw *config.PauseWindows) *Driver {
	t.Helper()
	d := phaseDriverWithReleaser(t, h, twoPhases(), rel)
	d.cfg.PauseWindows = pw
	d.now = clk.Now
	d.leaseRenewalInterval = 2 * time.Millisecond
	return d
}

type pauseDrainResult struct {
	stop bool
	err  error
}

func startPauseDrain(t *testing.T, d *Driver) <-chan pauseDrainResult {
	ch := make(chan pauseDrainResult, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	go func() {
		_, _, _, stop, err := d.drainPhases(ctx, 1, 0, d.targets[0], spend{start: time.Now()})
		ch <- pauseDrainResult{stop: stop, err: err}
	}()
	return ch
}

func waitPauseDrain(t *testing.T, ch <-chan pauseDrainResult) pauseDrainResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the drain")
		return pauseDrainResult{}
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The case the task is about: phase 1 is running when the window opens, finishes
// uninterrupted, and phase 2 is held at the seam — heartbeating the lease — until
// the window closes, then resumes at that same phase.
func TestDrainPhases_ScheduledPauseHoldsTheNextPhaseAndResumesIt(t *testing.T) {
	clk := &pauseHoldClock{at: brisbaneAt(t, 9, 10, 0)} // Monday 10:00, outside
	rel := &beatReleaser{}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)},
		{res: okResult(5, 0.05)},
	}}
	h.onInvoke = func(call int) {
		if call == 0 {
			// The window opens DURING the session: the running session must
			// still finish, and the pause lands at the boundary after it.
			clk.Set(brisbaneAt(t, 9, 11, 30))
		}
	}
	logs := captureLogs(t)
	d := pauseBoundaryDriver(t, h, rel, clk, peakSchedule(t))
	done := startPauseDrain(t, d)

	waitUntil(t, "the hold at the review boundary", func() bool {
		return strings.Contains(logs.String(), "holding t-1 at the review boundary")
	})
	if h.invokeCalls != 1 {
		t.Fatalf("phase 2 spawned inside the window: %d sessions", h.invokeCalls)
	}
	if len(rel.calls) != 0 {
		t.Errorf("the claim was handed back while holding: %+v", rel.calls)
	}
	waitUntil(t, "a heartbeat while held", func() bool { return len(rel.beatSnapshot()) > 0 })
	beats := rel.beatSnapshot()
	if beats[0] != "r-1" {
		t.Errorf("heartbeats = %v, want the held run r-1", beats)
	}
	if h.invokeCalls != 1 {
		t.Fatalf("phase 2 spawned while the window was still active")
	}

	clk.Set(brisbaneAt(t, 9, 14, 30)) // past 14:00
	res := waitPauseDrain(t, done)
	if res.err != nil || res.stop {
		t.Fatalf("drain after the window: stop=%v err=%v", res.stop, res.err)
	}
	if h.invokeCalls != 2 {
		t.Fatalf("sessions after the window = %d, want 2 (implement, then review)", h.invokeCalls)
	}
	if got := h.invocations[1].Prompt; !strings.Contains(got, "PHASE 2") {
		t.Errorf("the resumed session is not the review phase: %q", got)
	}
	out := logs.String()
	if !strings.Contains(out, "scheduled pause ended — resuming t-1 at the review phase") {
		t.Errorf("the leave line is missing or names the wrong phase:\n%s", out)
	}
}

// CLA-908: the hold's presence says what the console's pause banner needs — the
// schedule reason, the window's end and the task/phase waiting behind it — on
// the plane beacon AND in the local BEACON file, which the supervisor's fleet
// listing reads. Same state, two readers, no second writer to drift.
func TestDrainPhases_ScheduledPauseBeaconsTheReasonUntilAndHeldTask(t *testing.T) {
	clk := &pauseHoldClock{at: brisbaneAt(t, 9, 10, 0)}
	rel := &beatReleaser{}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)},
		{res: okResult(5, 0.05)},
	}}
	h.onInvoke = func(call int) {
		if call == 0 {
			clk.Set(brisbaneAt(t, 9, 11, 30))
		}
	}
	ff := &fakeFleet{}
	d := pauseBoundaryDriver(t, h, rel, clk, peakSchedule(t))
	d.targets[0].Fleet = ff
	done := startPauseDrain(t, d)

	waitUntil(t, "the held presence beacon", func() bool {
		sent, _ := ff.reports()
		if len(sent) == 0 {
			return false
		}
		last := sent[len(sent)-1]
		return last.State.Kind == fleet.StateDraining && last.State.PauseReason == fleet.PauseReasonSchedule
	})
	until := brisbaneAt(t, 9, 14, 0)
	sent, _ := ff.reports()
	last := sent[len(sent)-1]
	if last.State.Kind != fleet.StateDraining {
		t.Errorf("held beacon kind = %q, want draining (the console banner keys on it)", last.State.Kind)
	}
	if last.State.PauseReason != fleet.PauseReasonSchedule {
		t.Errorf("held beacon pause reason = %q, want %q", last.State.PauseReason, fleet.PauseReasonSchedule)
	}
	if !last.State.PauseUntil.Equal(until) {
		t.Errorf("held beacon pause until = %v, want %v", last.State.PauseUntil, until)
	}
	if last.State.TaskRef != "t-1" || last.State.Phase != "review" {
		t.Errorf("held beacon names %q at %q, want t-1 at the review phase", last.State.TaskRef, last.State.Phase)
	}

	data, err := os.ReadFile(filepath.Join(d.state.Path(), LocalBeaconName))
	if err != nil {
		t.Fatalf("no local beacon during the hold: %v", err)
	}
	var b LocalBeacon
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatalf("local beacon is not valid JSON: %v\n%s", err, data)
	}
	if b.State != fleet.StateDraining || b.PauseReason != fleet.PauseReasonSchedule {
		t.Errorf("local beacon = state %q reason %q, want draining/%s", b.State, b.PauseReason, fleet.PauseReasonSchedule)
	}
	if b.PauseUntil == nil || !b.PauseUntil.Equal(until) {
		t.Errorf("local beacon pauseUntil = %v, want %v", b.PauseUntil, until)
	}
	if b.PauseZone != "Australia/Brisbane" {
		t.Errorf("local beacon pauseZone = %q, want the schedule's zone so the listing can say AEST", b.PauseZone)
	}
	if b.TaskRef != "t-1" || b.Phase != "review" {
		t.Errorf("local beacon names %q at %q, want t-1 at the review phase", b.TaskRef, b.Phase)
	}

	clk.Set(brisbaneAt(t, 9, 14, 30))
	if res := waitPauseDrain(t, done); res.err != nil || res.stop {
		t.Fatalf("drain after the window: stop=%v err=%v", res.stop, res.err)
	}
	if h.invokeCalls != 2 {
		t.Errorf("sessions after the window = %d, want 2", h.invokeCalls)
	}
	// The hold marker is cleared on the way out: the next presence seat is the
	// resumed drain, not a pause left stuck on the row.
	if got := d.fleetState(0); got.Kind == fleet.StateDraining {
		t.Errorf("state after the hold = %+v, want the drain's own state (the hold marker must be clear)", got)
	}
	if got := d.holdAt(0); got.on {
		t.Errorf("hold marker survived the hold: %+v", got)
	}
}

// The local BEACON is ONE file for the whole daemon, so the running target must
// be its last writer: while a sequence is held, `supervise` has to read the held
// task, not whichever sibling project happens to be beaconed after it. The
// sibling still reaches the plane — its project's row must not go stale.
func TestBeaconSessionTargets_LocalBeaconCarriesTheHeldTarget(t *testing.T) {
	at := brisbaneAt(t, 9, 12, 0)
	d, _, _, _ := pauseDriver(t, peakSchedule(t), &at)
	sibling := &fakeFleet{}
	d.targets = append(d.targets, Target{Poller: busyPoller(), Fleet: sibling})
	d.schedPaused = true
	d.holdPause(0, "t-1", "review", time.Time{}, "", nil)

	d.beaconSessionTargets(0, d.targets[0])

	data, err := os.ReadFile(filepath.Join(d.state.Path(), LocalBeaconName))
	if err != nil {
		t.Fatalf("no local beacon after the presence lap: %v", err)
	}
	var b LocalBeacon
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatalf("local beacon is not valid JSON: %v\n%s", err, data)
	}
	if b.State != fleet.StateDraining || b.PauseReason != fleet.PauseReasonSchedule {
		t.Errorf("local beacon = state %q reason %q, want the held target's schedule pause", b.State, b.PauseReason)
	}
	if b.TaskRef != "t-1" || b.Phase != "review" {
		t.Errorf("local beacon names %q at %q, want the HELD target's t-1 at review (not the sibling beaconed last)", b.TaskRef, b.Phase)
	}

	sent, _ := sibling.reports()
	if len(sent) != 1 || sent[0].State.Kind != fleet.StateDraining {
		t.Errorf("the sibling target was not beaconed on the plane: %+v", sent)
	}
}

// A STOP marker during the hold takes the ordinary drain-stop path: the next
// phase never spawns and the deferred handback releases the claim from the queue.
func TestDrainPhases_StopDuringScheduledPauseTakesTheNormalStopPath(t *testing.T) {
	clk := &pauseHoldClock{at: brisbaneAt(t, 9, 10, 0)}
	rel := &beatReleaser{}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(1, 0)},
		{res: okResult(1, 0)},
	}}
	h.onInvoke = func(call int) {
		if call == 0 {
			clk.Set(brisbaneAt(t, 9, 11, 30))
		}
	}
	logs := captureLogs(t)
	d := pauseBoundaryDriver(t, h, rel, clk, peakSchedule(t))
	done := startPauseDrain(t, d)

	waitUntil(t, "the hold at the review boundary", func() bool {
		return strings.Contains(logs.String(), "holding t-1 at the review boundary")
	})
	if err := os.WriteFile(filepath.Join(d.state.Path(), "STOP"), []byte("stop during the pause"), 0o644); err != nil {
		t.Fatalf("planting STOP: %v", err)
	}
	res := waitPauseDrain(t, done)
	if res.err != nil || !res.stop {
		t.Fatalf("STOP during the hold: stop=%v err=%v, want a graceful stop", res.stop, res.err)
	}
	if h.invokeCalls != 1 {
		t.Errorf("the review phase spawned after STOP: %d sessions", h.invokeCalls)
	}
	if len(rel.calls) != 1 || rel.calls[0] != (releaseCall{"t-1", "r-1"}) {
		t.Errorf("the held claim was not handed back by the normal stop path: %+v", rel.calls)
	}
}

// No active window: the drain is byte-identical to today — no hold, no extra
// log, no beacon, nothing that could change a healthy sequence's timing.
func TestDrainPhases_NoActivePauseWindowChangesNothing(t *testing.T) {
	clk := &pauseHoldClock{at: brisbaneAt(t, 9, 9, 0)} // outside 11:00-14:00
	rel := &beatReleaser{}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(1, 0)},
		{res: okResult(1, 0)},
	}}
	logs := captureLogs(t)
	d := pauseBoundaryDriver(t, h, rel, clk, peakSchedule(t))

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: stop=%v err=%v", stop, err)
	}
	if h.invokeCalls != 2 {
		t.Errorf("sessions = %d, want 2 with no window active", h.invokeCalls)
	}
	if out := logs.String(); strings.Contains(out, "scheduled pause") {
		t.Errorf("a pause line was logged with no window active:\n%s", out)
	}
}

// A runner-claimed drain has already taken the claim when the walk begins, so a
// window active at that first boundary holds even the FIRST phase — the
// close-out shape observed on 2026-11-16 — and keeps heartbeating the claim it
// took rather than letting the lease lapse into a takeover.
func TestDrainPhases_ScheduledPauseHoldsAClaimedDrainsFirstPhase(t *testing.T) {
	b := walkClaimBrief("CLA-906")
	rel := &pauseClaimReleaser{claimingReleaser: claimingReleaser{
		offers: []plane.Offer{{Next: &b}},
		claimFn: func(string, bool) (plane.ClaimedTask, error) {
			return plane.ClaimedTask{Task: b, RunID: walkClaimRunID}, nil
		},
	}}
	claim := harness.Claim{TaskID: walkClaimTaskID, Ref: b.Ref, RunID: walkClaimRunID}
	h := &fakeAdapter{steps: []invokeStep{
		{res: reported(held(okResult(1, 0), claim), harness.Report{
			TaskID: claim.TaskID, Ref: claim.Ref, RunID: claim.RunID, Branch: "clanker/claimed",
		})},
		{res: okResult(1, 0)},
	}}
	clk := &pauseHoldClock{at: brisbaneAt(t, 9, 11, 30)}
	logs := captureLogs(t)
	d := pauseBoundaryDriver(t, h, rel, clk, peakSchedule(t))
	done := startPauseDrain(t, d)

	waitUntil(t, "the hold before the first phase", func() bool {
		return strings.Contains(logs.String(), "holding CLA-906 at the implement boundary")
	})
	if h.invokeCalls != 0 {
		t.Fatalf("%d session(s) spawned inside the window before the first phase", h.invokeCalls)
	}
	if len(rel.claimCalls) != 1 {
		t.Errorf("claim calls = %v, want the runner's single claim", rel.claimCalls)
	}
	if len(rel.calls) != 0 {
		t.Errorf("the runner's claim was handed back while holding: %+v", rel.calls)
	}
	waitUntil(t, "a heartbeat of the claimed run", func() bool { return len(rel.beatSnapshot()) > 0 })
	if beats := rel.beatSnapshot(); beats[0] != walkClaimRunID {
		t.Errorf("heartbeats = %v, want the claimed run %s", beats, walkClaimRunID)
	}

	clk.Set(brisbaneAt(t, 9, 14, 30))
	res := waitPauseDrain(t, done)
	if res.err != nil || res.stop {
		t.Fatalf("drain after the window: stop=%v err=%v", res.stop, res.err)
	}
	if h.invokeCalls != 2 {
		t.Fatalf("sessions after the window = %d, want 2", h.invokeCalls)
	}
	if got := h.invocations[0].ResumeClaim; got.TaskID != walkClaimTaskID || got.RunID != walkClaimRunID {
		t.Errorf("the first phase resumed %+v, want the claim the runner took (%s/%s)", got, walkClaimTaskID, walkClaimRunID)
	}
}

// holdClaimResult is the carried result a boundary hold renews: the task and
// run a preceding phase left in flight.
func holdClaimResult() *harness.Result {
	return &harness.Result{Claim: harness.Claim{TaskID: "t-1", Ref: "CLA-1", RunID: "r-1"}}
}

// holdSeq is the smallest drain state holdThroughPause needs, driven directly:
// nothing here spawns, so the hold's own branches are what the assertions read.
func holdSeq(t *testing.T, d *Driver, carried *harness.Result) (*drainSeq, *seqStep) {
	t.Helper()
	phases := d.cfg.EffectivePhases()
	s := &drainSeq{d: d, t: d.targets[0], ti: 0, i: len(phases) - 1, phases: phases, carried: carried}
	return s, &seqStep{ph: phases[len(phases)-1]}
}

func startHoldThroughPause(s *drainSeq, st *seqStep, ctx context.Context) <-chan pauseHold {
	ch := make(chan pauseHold, 1)
	go func() { ch <- s.holdThroughPause(ctx, st) }()
	return ch
}

func waitHoldOutcome(t *testing.T, ch <-chan pauseHold) pauseHold {
	t.Helper()
	select {
	case out := <-ch:
		return out
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the hold")
		return pauseHoldNone
	}
}

// CLA-906 review 1.review-1.3: ErrNotWired degrades the beat exactly as the
// in-session renewer does — one line, no further heartbeats — but the hold keeps
// waiting and never releases the claim.
func TestHoldThroughPause_NotWiredKeepsHoldingWithoutBeats(t *testing.T) {
	clk := &pauseHoldClock{at: brisbaneAt(t, 9, 11, 30)}
	rel := &beatReleaser{err: fmt.Errorf("%w: no api key", plane.ErrNotWired)}
	d := pauseBoundaryDriver(t, &fakeAdapter{}, rel, clk, peakSchedule(t))
	logs := captureLogs(t)
	s, st := holdSeq(t, d, holdClaimResult())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := startHoldThroughPause(s, st, ctx)
	waitUntil(t, "the lease stand-down", func() bool {
		return strings.Contains(logs.String(), "cannot renew a lease")
	})
	if beats := rel.beatSnapshot(); len(beats) != 1 {
		t.Fatalf("heartbeats = %v, want the single entry beat before standing down", beats)
	}
	clk.Set(brisbaneAt(t, 9, 14, 30))
	if out := waitHoldOutcome(t, done); out != pauseHoldResumed {
		t.Fatalf("hold outcome = %v, want a resume after the window", out)
	}
	if len(rel.calls) != 0 || len(rel.expireCalls) != 0 {
		t.Errorf("the claim was handed back during the hold: release %v expire %v", rel.calls, rel.expireCalls)
	}
	if d.schedPaused {
		t.Error("schedPaused must be clear once the hold resumes")
	}
}

// CLA-906 review 1.review-1.3: three consecutive generic renewal failures log
// the give-up and the hold continues without heartbeats — it must not end the
// drain.
func TestHoldThroughPause_GivesUpAfterThreeFailedRenewalsAndKeepsHolding(t *testing.T) {
	clk := &pauseHoldClock{at: brisbaneAt(t, 9, 11, 30)}
	rel := &beatReleaser{err: errors.New("plane unreachable")}
	d := pauseBoundaryDriver(t, &fakeAdapter{}, rel, clk, peakSchedule(t))
	logs := captureLogs(t)
	s, st := holdSeq(t, d, holdClaimResult())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := startHoldThroughPause(s, st, ctx)
	waitUntil(t, "the give-up line", func() bool {
		return strings.Contains(logs.String(), "giving up on lease renewal after 3 consecutive failures")
	})
	if beats := rel.beatSnapshot(); len(beats) != 3 {
		t.Fatalf("heartbeats = %v, want the three consecutive failures then no more", beats)
	}
	clk.Set(brisbaneAt(t, 9, 14, 30))
	if out := waitHoldOutcome(t, done); out != pauseHoldResumed {
		t.Fatalf("hold outcome = %v, want the sequence to resume", out)
	}
	if len(rel.calls) != 0 || len(rel.expireCalls) != 0 {
		t.Errorf("the claim was handed back during the hold: release %v expire %v", rel.calls, rel.expireCalls)
	}
}

// CLA-906 review 1.review-1.1/1.review-1.3: a lost run during the hold drops the
// claim (it is someone else's task) and reports the DRAIN ending, leaving the
// pause flag clear for the run's next poll.
func TestHoldThroughPause_RunLostDropsTheClaimAndEndsTheHold(t *testing.T) {
	clk := &pauseHoldClock{at: brisbaneAt(t, 9, 11, 30)}
	rel := &beatReleaser{err: fmt.Errorf("%w: run_superseded", plane.ErrRunLost)}
	d := pauseBoundaryDriver(t, &fakeAdapter{}, rel, clk, peakSchedule(t))
	logs := captureLogs(t)
	s, st := holdSeq(t, d, holdClaimResult())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if out := waitHoldOutcome(t, startHoldThroughPause(s, st, ctx)); out != pauseHoldRunLost {
		t.Fatalf("hold outcome = %v, want the run-lost drain ending", out)
	}
	if s.carried != nil {
		t.Errorf("the hold kept a claim that is no longer this run's: %+v", s.carried)
	}
	if d.schedPaused {
		t.Error("schedPaused must be cleared for the run's next poll")
	}
	if len(rel.calls) != 0 || len(rel.expireCalls) != 0 {
		t.Errorf("a lost run must not be handed back: release %v expire %v", rel.calls, rel.expireCalls)
	}
	if out := logs.String(); !strings.Contains(out, "is no longer this run's, ending the hold") {
		t.Errorf("the lost-run line is missing:\n%s", out)
	}
}

// The drain-level half of the run-lost disposition: the hold ends THIS drain
// without setting the run's stop flag, spawns no next phase and hands nothing
// back — the contract TestDrainPhases_RunLostEndsTheSessionAndTheDrain pins for
// a lost run mid-phase.
func TestDrainPhases_RunLostDuringScheduledPauseEndsTheDrainWithoutTheRun(t *testing.T) {
	clk := &pauseHoldClock{at: brisbaneAt(t, 9, 10, 0)}
	rel := &beatReleaser{err: fmt.Errorf("%w: run_superseded", plane.ErrRunLost)}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(1, 0)},
		{res: okResult(1, 0)},
	}}
	h.onInvoke = func(call int) {
		if call == 0 {
			clk.Set(brisbaneAt(t, 9, 11, 30))
		}
	}
	logs := captureLogs(t)
	d := pauseBoundaryDriver(t, h, rel, clk, peakSchedule(t))

	res := waitPauseDrain(t, startPauseDrain(t, d))
	if res.err != nil || res.stop {
		t.Fatalf("drainPhases: err=%v stop=%v, want a drain that ends without stopping the run", res.err, res.stop)
	}
	if h.invokeCalls != 1 {
		t.Errorf("sessions = %d, want 1: no phase may spawn after the run was refused", h.invokeCalls)
	}
	if len(rel.calls) != 0 || len(rel.expireCalls) != 0 {
		t.Errorf("handback calls = release %v expire %v; want none for a task this run no longer holds", rel.calls, rel.expireCalls)
	}
	if d.schedPaused {
		t.Error("schedPaused must be cleared so the next poll can announce the window again")
	}
	if out := logs.String(); !strings.Contains(out, "ending the drain: the task is no longer this run's") {
		t.Errorf("the drain-level lost-run line is missing:\n%s", out)
	}
}

// A cancelled context during the hold takes the ordinary stop path: the drain
// stops and the deferred handback releases the claim.
func TestDrainPhases_CancelledContextDuringScheduledPauseTakesTheNormalStopPath(t *testing.T) {
	clk := &pauseHoldClock{at: brisbaneAt(t, 9, 10, 0)}
	rel := &beatReleaser{}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(1, 0)},
		{res: okResult(1, 0)},
	}}
	h.onInvoke = func(call int) {
		if call == 0 {
			clk.Set(brisbaneAt(t, 9, 11, 30))
		}
	}
	logs := captureLogs(t)
	d := pauseBoundaryDriver(t, h, rel, clk, peakSchedule(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan pauseDrainResult, 1)
	go func() {
		_, _, _, stop, err := d.drainPhases(ctx, 1, 0, d.targets[0], spend{start: time.Now()})
		done <- pauseDrainResult{stop: stop, err: err}
	}()
	waitUntil(t, "the hold at the review boundary", func() bool {
		return strings.Contains(logs.String(), "holding t-1 at the review boundary")
	})
	cancel()

	res := waitPauseDrain(t, done)
	if res.err != nil || !res.stop {
		t.Fatalf("cancelled hold: stop=%v err=%v, want a graceful stop", res.stop, res.err)
	}
	if h.invokeCalls != 1 {
		t.Errorf("the review phase spawned after the cancel: %d sessions", h.invokeCalls)
	}
	if len(rel.calls) != 1 || rel.calls[0] != (releaseCall{"t-1", "r-1"}) {
		t.Errorf("the held claim was not handed back by the normal stop path: %+v", rel.calls)
	}
}

// CLA-906 review 1.review-1.5: a hold is idle time, not draining time, so it is
// not charged to max_wall_clock — a short ceiling must not end the sequence at
// the boundary the window held.
func TestDrainPhases_ScheduledPauseIsNotChargedToTheWallClockBudget(t *testing.T) {
	clk := &pauseHoldClock{at: brisbaneAt(t, 9, 10, 0)}
	rel := &beatReleaser{}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(1, 0)},
		{res: okResult(1, 0)},
	}}
	h.onInvoke = func(call int) {
		if call == 0 {
			clk.Set(brisbaneAt(t, 9, 11, 30))
		}
	}
	logs := captureLogs(t)
	d := pauseBoundaryDriver(t, h, rel, clk, peakSchedule(t))
	// The implement phase finishes well inside the ceiling; the held wall clock
	// then outruns it. A post-hold breaker that charged the hold would stop the
	// drain and never spawn the review.
	d.cfg.Budget.MaxWallClock = config.Duration(time.Second)

	done := startPauseDrain(t, d)
	waitUntil(t, "the hold at the review boundary", func() bool {
		return strings.Contains(logs.String(), "holding t-1 at the review boundary")
	})
	time.Sleep(1200 * time.Millisecond)
	clk.Set(brisbaneAt(t, 9, 14, 30))

	res := waitPauseDrain(t, done)
	if res.err != nil || res.stop {
		t.Fatalf("drain after a held window: stop=%v err=%v, want the sequence to resume", res.stop, res.err)
	}
	if h.invokeCalls != 2 {
		t.Errorf("sessions = %d, want 2: the review must run after the hold", h.invokeCalls)
	}
}

// CLA-906 review 1.review-1.2: the boundary check runs BEFORE the parallel
// panel's fan-out, so a window active at the panel boundary holds the WHOLE
// panel — no member starts — and all of them run when it ends. Moving the check
// back after runPanelFanOut lets every member spawn inside the window and fails
// this test.
type pausePanelAdapter struct {
	*panelParallelAdapter
	enterWindow func()
}

func (a *pausePanelAdapter) Invoke(ctx context.Context, in harness.Invocation) (harness.Result, error) {
	// The implement phase is the only call that is neither a panel review nor
	// the fix; moving the clock there puts the next boundary inside the window.
	if !strings.Contains(in.Prompt, "REVIEW-ONLY gate") && !strings.Contains(in.Prompt, "the FIX phase") {
		a.enterWindow()
	}
	return a.panelParallelAdapter.Invoke(ctx, in)
}

func TestDrainPhases_ScheduledPauseHoldsTheParallelPanelBeforeAnyMember(t *testing.T) {
	repo, branch, tip := panelParallelRepo(t)
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented\n"}
	members := panelMembers() // three members: the fan-out really is parallel
	clk := &pauseHoldClock{at: brisbaneAt(t, 9, 10, 0)}
	var once sync.Once
	inner := newPanelParallelAdapter(rel, branch, tip, len(members))
	a := &pausePanelAdapter{
		panelParallelAdapter: inner,
		enterWindow:          func() { once.Do(func() { clk.Set(brisbaneAt(t, 9, 11, 30)) }) },
	}
	d, _ := panelParallelDriver(t, a, rel, members, repo, tip, 0)
	d.cfg.PauseWindows = peakSchedule(t)
	d.now = clk.Now
	d.leaseRenewalInterval = 2 * time.Millisecond
	logs := captureLogs(t)

	done := startPauseDrain(t, d)
	waitUntil(t, "the hold at the panel boundary", func() bool {
		return strings.Contains(logs.String(), "holding t-1 at the review-1 boundary")
	})
	inner.mu.Lock()
	memberCalls := inner.reviewCalls
	inner.mu.Unlock()
	if memberCalls != 0 {
		t.Fatalf("the panel fan-out spawned %d member(s) inside the window", memberCalls)
	}
	if len(rel.calls) != 0 {
		t.Errorf("the panel's claim was handed back while holding: %+v", rel.calls)
	}

	clk.Set(brisbaneAt(t, 9, 14, 30))
	res := waitPauseDrain(t, done)
	if res.err != nil || res.stop {
		t.Fatalf("drain after the window: stop=%v err=%v", res.stop, res.err)
	}
	inner.mu.Lock()
	memberCalls = inner.reviewCalls
	maxInFlight := inner.maxInFlight
	fixCalls := inner.fixCalls
	inner.mu.Unlock()
	if memberCalls != len(members) {
		t.Errorf("panel members run = %d, want %d after the window", memberCalls, len(members))
	}
	if maxInFlight != len(members) {
		t.Errorf("max members in flight = %d, want %d — the panel must still fan out", maxInFlight, len(members))
	}
	if fixCalls != 1 {
		t.Errorf("fix sessions = %d, want 1 after the panel", fixCalls)
	}
}

// pollBeaconState is the poll lap's answer to "which project speaks for the
// daemon in the one local BEACON file". The schedule must survive an idle or
// console-paused sibling polled after it, and a held schedule must survive a
// sibling that is paused by the same window but holding nothing (CLA-908).
func TestPollBeaconState_Precedence(t *testing.T) {
	idle := fleet.State{Kind: fleet.StateIdle}
	sched := fleet.State{Kind: fleet.StateDraining, PauseReason: fleet.PauseReasonSchedule}
	held := fleet.State{Kind: fleet.StateDraining, PauseReason: fleet.PauseReasonSchedule, TaskRef: "t-1", Phase: "review"}
	console := fleet.State{Kind: fleet.StateDraining, PauseReason: fleet.PauseReasonConsole}

	cases := []struct {
		name      string
		cur, next fleet.State
		want      fleet.State
	}{
		{"no pause anywhere stays idle", idle, idle, idle},
		{"a schedule pause is taken over idle", idle, sched, sched},
		{"a later console pause cannot overwrite the schedule", sched, console, sched},
		{"a later idle sibling cannot overwrite the schedule", sched, idle, sched},
		{"a held schedule outranks a bare one", sched, held, held},
		{"a bare schedule cannot displace a held one", held, sched, held},
		{"an operator pause replaces idle", idle, console, console},
		{"an operator pause is kept over idle", console, idle, console},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pollBeaconState(tc.cur, tc.next); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("pollBeaconState(%+v, %+v) = %+v, want %+v", tc.cur, tc.next, got, tc.want)
			}
		})
	}
}

// The poll lap writes the ONE local BEACON file from a state chosen across the
// targets, not from whichever target is polled last: with the schedule active on
// one project and a console pause on a sibling polled after it, `supervise` must
// still print the schedule banner the console shows (CLA-908).
func TestPickTarget_LocalBeaconKeepsTheSchedulePause(t *testing.T) {
	at := brisbaneAt(t, 9, 12, 0) // inside the 11:00-14:00 window
	cfg := fastCfg()
	cfg.PauseWindows = peakSchedule(t)
	held := &fakeFleet{}
	sibling := &fakeFleet{}
	d := NewMulti(cfg, &fakeAdapter{}, []Target{
		{Poller: &fakePoller{sum: backlog.Summary{Ready: 1, Claimable: 1}}, Fleet: held},
		{Poller: &fakePoller{sum: backlog.Summary{Ready: 1, Claimable: 1, Paused: true}}, Fleet: sibling},
	})
	openTestStateDir(t, d)
	d.now = func() time.Time { return at }
	// The first target is the one a schedule holds; the sibling is paused from
	// the console and is polled after it.
	d.holdPause(0, "t-1", "review", time.Time{}, "", nil)

	pick(t, d)

	data, err := os.ReadFile(filepath.Join(d.state.Path(), LocalBeaconName))
	if err != nil {
		t.Fatalf("no local beacon after the poll lap: %v", err)
	}
	var b LocalBeacon
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatalf("local beacon is not valid JSON: %v\n%s", err, data)
	}
	if b.State != fleet.StateDraining || b.PauseReason != fleet.PauseReasonSchedule {
		t.Errorf("local beacon = state %q reason %q, want the schedule pause (the sibling polled last must not overwrite it)", b.State, b.PauseReason)
	}
	if b.TaskRef != "t-1" || b.Phase != "review" {
		t.Errorf("local beacon names %q at %q, want the held task t-1 at review", b.TaskRef, b.Phase)
	}

	// The sibling still reaches the plane: its own project row must say it is
	// console-paused, and only the daemon-wide file is deduplicated.
	sent, _ := sibling.reports()
	if len(sent) != 1 {
		t.Fatalf("sibling plane beacons = %d, want 1 per poll", len(sent))
	}
	if sent[0].State.Kind != fleet.StateDraining || sent[0].State.PauseReason != fleet.PauseReasonConsole {
		t.Errorf("sibling plane beacon = %+v, want its own console reason", sent[0].State)
	}
	if got, _ := held.reports(); len(got) != 1 {
		t.Errorf("held target plane beacons = %d, want 1 per poll", len(got))
	}
}
