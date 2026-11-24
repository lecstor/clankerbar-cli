package loop

// CLA-535: the daemon's reporting half of the plane's `waiting` presence state. A drain that ends
// by filing a blocking question on its own task leaves nothing claimable and no session running, so
// before this the poll beacon said `idle` — indistinguishable from a daemon with nothing to do.
// These tests pin the beacon, the flip back once the answer lands, the reclaim path (iteration
// outranks waiting and a new drain clears it), and that the marker is reporting only.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

// askedResult is a session that ended by filing a blocking question on claim: the plane set the
// task `blocked` and released the run, which the adapter observed as a settled claim.
func askedResult(claim harness.Claim) harness.Result {
	res := held(okResult(5, 0.05), claim)
	res.BlockingAsk = claim.TaskID
	return res
}

// beaconKinds lists every plane beacon's state kind in send order.
func beaconKinds(ff *fakeFleet) []string {
	sent, _ := ff.reports()
	kinds := make([]string, 0, len(sent))
	for _, r := range sent {
		kinds = append(kinds, r.State.Kind)
	}
	return kinds
}

// TestFleetState_WaitsOnItsOwnBlockingQuestion pins the mapping and its rank: waiting replaces the
// idle a quiet target reports, but a pause that names why spawns are closed and a running iteration
// both outrank it.
func TestFleetState_WaitsOnItsOwnBlockingQuestion(t *testing.T) {
	d := NewMulti(fastCfg(), &fakeAdapter{}, []Target{{Poller: busyPoller()}})
	if got := d.fleetState(0); got.Kind != fleet.StateIdle {
		t.Fatalf("fresh target state = %+v, want idle", got)
	}
	d.noteWaiting(0, "CLA-535")
	got := d.fleetState(0)
	if got.Kind != fleet.StateWaiting || got.TaskRef != "CLA-535" {
		t.Fatalf("waiting state = %+v, want waiting on CLA-535", got)
	}
	// A running session outranks it: the iteration says what the daemon is doing now.
	d.iter[0] = iterState{on: true, n: 2, ref: "CLA-535", phase: "implement"}
	if got := d.fleetState(0); got.Kind != fleet.StateIteration {
		t.Errorf("state while draining = %+v, want iteration over waiting", got)
	}
	d.iter[0] = iterState{}
	// A deliberate pause still wins: draining names the cause the operator can clear.
	d.paused[0] = true
	if got := d.fleetState(0); got.Kind != fleet.StateDraining || got.PauseReason != fleet.PauseReasonConsole {
		t.Errorf("state while console-paused = %+v, want draining over waiting", got)
	}
	d.paused[0] = false
	// The answer landing clears the marker; the same target then reports idle.
	d.clearAnsweredWaiting(0, backlog.Summary{OpenQuestions: 0})
	if got := d.fleetState(0); got.Kind != fleet.StateIdle {
		t.Errorf("state after the answer = %+v, want idle", got)
	}
	// A target with no ask is untouched by the marker (the other assertions above start from it).
	d2 := NewMulti(fastCfg(), &fakeAdapter{}, []Target{{Poller: busyPoller()}})
	if got := d2.fleetState(0); got.Kind != fleet.StateIdle {
		t.Errorf("a target with no outstanding ask = %+v, want idle", got)
	}
}

// TestRun_BeaconsWaitingAfterItsOwnBlockingAsk is the whole path through the loop: the drain's
// closing beacon and every poll after it say waiting with the task ref, and the poll where the
// open-question count falls back says idle again.
func TestRun_BeaconsWaitingAfterItsOwnBlockingAsk(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	claim := harness.Claim{TaskID: "t-1", Ref: "CLA-535", RunID: "r-1", Settled: true}
	h := &fakeAdapter{steps: []invokeStep{{res: askedResult(claim)}}}

	p := &fakePoller{
		sums: []backlog.Summary{
			{Ready: 1, Claimable: 1, OpenQuestions: 0}, // the drain that asks
			{OpenQuestions: 1},                         // the question is open
			{OpenQuestions: 0},                         // the answer landed
		},
		sum: backlog.Summary{},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p.onCall = func(i int) {
		if i >= 2 {
			cancel()
		}
	}
	d, ff := fleetDriver(t, cfg, h)
	d.targets[0].Poller = p

	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	// The poll before the drain is idle; the drain announces its phase, closes on waiting, then
	// both following polls are waiting until the answer flips the last one back to idle.
	want := []string{
		fleet.StateIdle,
		fleet.StateIteration,
		fleet.StateWaiting,
		fleet.StateWaiting,
		fleet.StateIdle,
	}
	if got := beaconKinds(ff); len(got) != len(want) {
		t.Fatalf("beacon states = %v, want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("beacon states = %v, want %v", got, want)
			}
		}
	}
	for i, r := range mustReports(t, ff) {
		if r.State.Kind != fleet.StateWaiting {
			continue
		}
		if r.State.TaskRef != "CLA-535" {
			t.Errorf("waiting beacon %d names taskRef %q, want CLA-535", i, r.State.TaskRef)
		}
	}
}

// TestRun_AnOpenQuestionNeverGatesTheNextDrain is the reporting-only guard: while the question is
// still open, a claimable queue still spawns. The new drain reports iteration, and starting it
// clears the stale marker so the drain's own end reports what IT did.
func TestRun_AnOpenQuestionNeverGatesTheNextDrain(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	claim := harness.Claim{TaskID: "t-1", Ref: "CLA-535", RunID: "r-1", Settled: true}
	h := &fakeAdapter{steps: []invokeStep{
		{res: askedResult(claim)},
		{res: okResult(1, 0)},
	}}

	p := &fakePoller{
		sums: []backlog.Summary{
			{Ready: 1, Claimable: 1, OpenQuestions: 0},
			// The question is STILL open, and another task is claimable: the waiting marker must not
			// withhold the drain.
			{Ready: 1, Claimable: 1, OpenQuestions: 1},
		},
		sum: backlog.Summary{OpenQuestions: 1},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p.onCall = func(i int) {
		if i >= 2 {
			cancel()
		}
	}
	d, ff := fleetDriver(t, cfg, h)
	d.targets[0].Poller = p

	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if h.invokeCalls != 2 {
		t.Fatalf("sessions spawned = %d, want 2 — an outstanding question must never gate a drain", h.invokeCalls)
	}
	want := []string{
		fleet.StateIdle,      // poll 0
		fleet.StateIteration, // drain 1 begins
		fleet.StateWaiting,   // drain 1 closes after asking
		fleet.StateWaiting,   // poll 1: still open, but still claimable
		fleet.StateIteration, // drain 2 begins: the marker is cleared, iteration says what it is doing
		fleet.StateIdle,      // drain 2 closes without asking
		fleet.StateIdle,      // the cancelled poll's beacon, before the loop stops
	}
	got := beaconKinds(ff)
	if len(got) != len(want) {
		t.Fatalf("beacon states = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("beacon states = %v, want %v", got, want)
		}
	}
	if d.waitingAt(0).on {
		t.Error("the waiting marker survived a drain that started — a reclaimed task must not keep reporting the old ask")
	}
}

// TestRun_APollFailureDoesNotAnswerTheQuestion: a failed poll returns a ZERO summary, and zero
// open questions is exactly what an answer looks like. The clear runs only on the poll's success
// branch, so an outage must leave the beacon waiting.
func TestRun_APollFailureDoesNotAnswerTheQuestion(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	claim := harness.Claim{TaskID: "t-1", Ref: "CLA-535", RunID: "r-1", Settled: true}
	h := &fakeAdapter{steps: []invokeStep{{res: askedResult(claim)}}}

	p := &fakePoller{
		sums: []backlog.Summary{{Ready: 1, Claimable: 1}},
		errs: []error{nil, errors.New("plane blip")},
		sum:  backlog.Summary{OpenQuestions: 1},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p.onCall = func(i int) {
		if i >= 2 {
			cancel()
		}
	}
	d, ff := fleetDriver(t, cfg, h)
	d.targets[0].Poller = p

	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	waitingSeen := false
	for _, r := range mustReports(t, ff) {
		if r.State.Kind == fleet.StateWaiting {
			waitingSeen = true
			continue
		}
		if waitingSeen && r.State.Kind == fleet.StateIdle {
			t.Fatalf("beacon went idle on a poll failure — its zero summary was read as an answer: %s", r.State.Kind)
		}
	}
	if !waitingSeen {
		t.Error("no waiting beacon after the drain asked")
	}
}

// TestDrainPhases_AWaitOnTheRunnerHeldTask: with a phased sequence the RUNNER holds the claim, so
// the session that asks never observes a claim of its own. The ask is still the daemon's own — it
// names the task the runner seeded — and must produce the waiting state.
func TestDrainPhases_AWaitOnTheRunnerHeldTask(t *testing.T) {
	b := walkClaimBrief("CLA-901")
	f := &claimingReleaser{
		offers:  []plane.Offer{{Next: &b}},
		claimFn: claimedBrief(b),
	}
	res := okResult(1, 0) // no Claim: a runner-claimed session is told not to claim
	res.BlockingAsk = walkClaimTaskID
	h := &fakeAdapter{steps: []invokeStep{{res: res}}}
	d := phaseDriverWithReleaser(t, h, twoPhases(), f)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	got := d.fleetState(0)
	if got.Kind != fleet.StateWaiting || got.TaskRef != "CLA-901" {
		t.Fatalf("state after the runner-claimed session asked = %+v, want waiting on CLA-901", got)
	}
}

// TestWaiting_SurvivesARestart: the summary cannot say WHICH task a question belongs to, so the
// daemon saves the ref in its state dir; a restarted process (the supervisor's roll) must come
// back reporting waiting on the same task rather than idle.
func TestWaiting_SurvivesARestart(t *testing.T) {
	st, err := statedir.Open(t.TempDir())
	if err != nil {
		t.Fatalf("statedir.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	targets := func() []Target {
		return []Target{
			{Name: "alpha", Poller: busyPoller()},
			{Name: "beta", Poller: busyPoller()},
		}
	}
	first := NewMulti(fastCfg(), &fakeAdapter{}, targets())
	first.state = st
	first.noteWaiting(1, "CLA-535")

	restarted := NewMulti(fastCfg(), &fakeAdapter{}, targets())
	restarted.state = st
	restarted.loadWaiting()
	if got := restarted.fleetState(0); got.Kind != fleet.StateIdle {
		t.Errorf("target alpha after restart = %+v, want idle (it never asked)", got)
	}
	got := restarted.fleetState(1)
	if got.Kind != fleet.StateWaiting || got.TaskRef != "CLA-535" {
		t.Errorf("target beta after restart = %+v, want waiting on CLA-535", got)
	}
	// The answer clears the marker in the new process, and takes the saved record with it.
	restarted.clearAnsweredWaiting(1, backlog.Summary{OpenQuestions: 0})
	if restarted.waitingAt(1).on {
		t.Error("the restored marker survived its answer")
	}
	if st.Exists(statedir.WaitingName) {
		t.Error("the saved waiting record survived its answer")
	}
}

func mustReports(t *testing.T, ff *fakeFleet) []fleet.Report {
	t.Helper()
	sent, _ := ff.reports()
	return sent
}
