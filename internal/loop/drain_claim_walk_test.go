package loop

// The runner's claim, observed from the DRAIN rather than the helpers: it is
// planted before the walk, so phase 0 must resume it without claiming, and the
// deferred handback must hand it back exactly once — including after a dead
// phase's re-claim.

import (
	"fmt"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

const (
	walkClaimTaskID = "11111111-2222-3333-4444-555555555555"
	walkClaimRunID  = "run-uuid.secret"
)

// walkClaimBrief is the one task the claimer offers the tests below.
func walkClaimBrief(ref string) plane.TaskBrief {
	return plane.TaskBrief{ID: walkClaimTaskID, Ref: ref}
}

// handbacks counts every handback the fake recorded for one task, whichever
// disposition the claim earned (Release for a ready task, ExpireLease for one
// with pushed work).
func handbacks(rel *claimingReleaser, taskID string) int {
	n := 0
	for _, c := range rel.calls {
		if c.taskID == taskID {
			n++
		}
	}
	for _, c := range rel.expireCalls {
		if c.taskID == taskID {
			n++
		}
	}
	return n
}

// The claim the runner takes seeds phase 0 directly: that session was never
// told to claim, so ResumeClaim is the only way the salvage, the handback and
// the delivery checks see a task at all. And the one handback the drain owes is
// taken at its end, not by the phase that ended without holding.
func TestRunnerClaim_SeedsPhaseZeroAndHandsBackOnce(t *testing.T) {
	b := walkClaimBrief("CLA-901")
	f := &claimingReleaser{
		offers: []plane.Offer{{Next: &b}},
		claimFn: func(string, bool) (plane.ClaimedTask, error) {
			return plane.ClaimedTask{Task: b, RunID: walkClaimRunID}, nil
		},
	}
	// Phase 0 exits cleanly holding NOTHING. A non-final phase with no
	// checkpoint leaves the next phase nothing to resume, so the walk ends
	// there — and the planted claim is still owed the deferred handback.
	h := &fakeAdapter{steps: []invokeStep{{res: okResult(1, 0)}}}
	d := phaseDriverWithReleaser(t, h, twoPhases(), f)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 1 {
		t.Fatalf("spawned %d sessions, want 1: the run's claim is seeded, so the walk ends after the phase that carried it", h.invokeCalls)
	}
	if got := h.invocations[0].ResumeClaim; got.TaskID != walkClaimTaskID || got.RunID != walkClaimRunID {
		t.Errorf("phase 0 ResumeClaim = %+v, want %s/%s — the session never claimed, so the seed is all it has",
			got, walkClaimTaskID, walkClaimRunID)
	}
	if len(f.claimCalls) != 1 {
		t.Errorf("claim calls = %v, want the runner's single fresh claim", f.claimCalls)
	}
	if got := handbacks(f, walkClaimTaskID); got != 1 {
		t.Errorf("handed the task back %d time(s), want exactly 1 across the whole drain: %+v / %+v",
			got, f.calls, f.expireCalls)
	}
}

// A dead phase 0 hands its claim back, so the retry has to re-claim the very
// task it is about to respawn — and the retry must resume THAT claim, not the
// one the dead session reported.
func TestRunnerClaim_DeadPhaseZeroReclaimsBeforeRetry(t *testing.T) {
	b := walkClaimBrief("CLA-902")
	// The retry's own run id, so the second invocation proves the takeover's
	// claim is what reached the walk.
	const retryRunID = "run-uuid.retry"
	plain := 0
	f := &claimingReleaser{
		offers: []plane.Offer{{Next: &b}},
		claimFn: func(_ string, takeover bool) (plane.ClaimedTask, error) {
			if takeover {
				return plane.ClaimedTask{Task: b, RunID: retryRunID}, nil
			}
			plain++
			if plain == 1 {
				return plane.ClaimedTask{Task: b, RunID: walkClaimRunID}, nil
			}
			return plane.ClaimedTask{}, fmt.Errorf("%w: the dead phase's handback lapsed the lease", plane.ErrClaimLost)
		},
	}
	// The existing dead fixture: exit 0 with the final step reason "unknown" and
	// no branch recorded, still holding the task.
	h := &fakeAdapter{steps: []invokeStep{
		{res: held(deadResult(), harness.Claim{TaskID: walkClaimTaskID, RunID: walkClaimRunID})},
		{res: okResult(1, 0)},
	}}
	d := phaseDriverWithReleaser(t, h, twoPhases(), f)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 2 {
		t.Fatalf("spawned %d sessions, want 2 (the dead phase, then its retry)", h.invokeCalls)
	}
	want := []claimCall{
		// the drain's own fresh claim
		{walkClaimTaskID, false},
		// the retry's plain re-claim
		{walkClaimTaskID, false},
		// the lease is gone, so the takeover
		{walkClaimTaskID, true},
	}
	if len(f.claimCalls) != len(want) {
		t.Fatalf("claim calls = %v, want %v", f.claimCalls, want)
	}
	for i := range want {
		if f.claimCalls[i] != want[i] {
			t.Errorf("claim call %d = %v, want %v", i, f.claimCalls[i], want[i])
		}
	}
	if got := h.invocations[1].ResumeClaim; got.TaskID != walkClaimTaskID || got.RunID != retryRunID {
		t.Errorf("the retry's ResumeClaim = %+v, want %s/%s — the re-claim must be planted before the respawn",
			got, walkClaimTaskID, retryRunID)
	}
}

// Nothing offered is not a session: the drain ends without spawning, and it
// ends promptly rather than sitting out an idle interval.
func TestRunnerClaim_NothingOfferedSpawnsNothing(t *testing.T) {
	f := &claimingReleaser{offers: []plane.Offer{{}}}
	h := &fakeAdapter{}
	d := phaseDriverWithReleaser(t, h, twoPhases(), f)
	d.cfg.IdlePollInterval = config.Duration(time.Millisecond)

	start := time.Now()
	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 0 {
		t.Errorf("spawned %d session(s) on an empty offer, want none", h.invokeCalls)
	}
	if f.offerCalls != 1 {
		t.Errorf("read the queue %d time(s), want once", f.offerCalls)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("the idle wait took %s for a 1ms poll interval", elapsed)
	}
}

// A releaser that cannot claim keeps the old path: the first session claims for
// itself, so the driver seeds nothing.
func TestRunnerClaim_IneligibleKeepsTheSessionPath(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{{res: okResult(1, 0)}}}
	d, _ := phaseDriver(t, h, twoPhases())

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 1 {
		t.Fatalf("spawned %d sessions, want 1", h.invokeCalls)
	}
	if got := h.invocations[0].ResumeClaim; got != (harness.Claim{}) {
		t.Errorf("phase 0 ResumeClaim = %+v, want zero — a releaser without the claimer keeps the session's own claim", got)
	}
}
