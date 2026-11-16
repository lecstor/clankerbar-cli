package loop

// CLA-906: a scheduled pause window holds a phase sequence at its next boundary
// instead of letting the remaining phases run through peak hours. These tests
// drive the whole drain so the seam, the lease renewal and the stop path are the
// production ones; the clock is the same d.now seam the poll gate uses.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
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
// every run id it was asked to renew.
type beatReleaser struct {
	fakeReleaser
	mu    sync.Mutex
	beats []string
}

func (b *beatReleaser) Heartbeat(_ context.Context, runID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.beats = append(b.beats, runID)
	return nil
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
