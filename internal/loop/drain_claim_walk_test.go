package loop

// The runner's claim, observed from the DRAIN rather than the helpers: it is
// planted before the walk, so phase 0 must resume it without claiming, and the
// deferred handback must hand it back exactly once — including after a dead
// phase's re-claim.

import (
	"context"
	"fmt"
	"strings"
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

// CLA-877: a phase whose ONLY failure is its report resume stalling delivered
// its work, so the phase is retried inside the SAME claim. No handback, no
// ExpireLease and no re-claim — the retry resumes the run already held.
func TestRunnerClaim_ReportResumeStallRetriesInTheSameClaim(t *testing.T) {
	b := walkClaimBrief("CLA-877")
	f := &claimingReleaser{
		offers: []plane.Offer{{Next: &b}},
		claimFn: func(string, bool) (plane.ClaimedTask, error) {
			return plane.ClaimedTask{Task: b, RunID: walkClaimRunID}, nil
		},
	}
	walkClaim := harness.Claim{
		TaskID: walkClaimTaskID, Ref: "CLA-877", RunID: walkClaimRunID,
		HasWIP: true, Branch: "clanker/walk",
	}
	// The session's own end is clean and its branch is pushed; only the report
	// resume went silent, and the adapter says so on the marker.
	stalled := held(okResult(1, 0), walkClaim)
	stalled.ReportResumeFailed = "no output for 3m0s"
	retried := reported(held(okResult(1, 0), walkClaim), harness.Report{
		TaskID: walkClaimTaskID, Ref: "CLA-877", RunID: walkClaimRunID, Branch: "clanker/walk",
	})
	h := &fakeAdapter{steps: []invokeStep{
		{res: stalled},
		{res: retried},
		// The review hands the task on and settles it, so the only handback this
		// drain could owe is the one the stalled phase must NOT take.
		{res: held(okResult(1, 0), harness.Claim{TaskID: walkClaimTaskID, RunID: walkClaimRunID, Settled: true})},
	}}
	logged := captureLogs(t)
	d := phaseDriverWithReleaser(t, h, twoPhases(), f)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 3 {
		t.Fatalf("spawned %d sessions, want 3 (stalled phase, its retry, then review)", h.invokeCalls)
	}
	if len(f.claimCalls) != 1 {
		t.Errorf("claim calls = %v, want only the runner's initial fresh claim — the retry must not re-claim a claim it still holds", f.claimCalls)
	}
	if got := handbacks(f, walkClaimTaskID); got != 0 {
		t.Errorf("handed the task back %d time(s), want none — a resume-only failure keeps the claim: %+v / %+v",
			got, f.calls, f.expireCalls)
	}
	if got := h.invocations[1].ResumeClaim; got.TaskID != walkClaimTaskID || got.RunID != walkClaimRunID {
		t.Errorf("the retry's ResumeClaim = %+v, want the SAME %s/%s the runner holds, not a re-claimed one",
			got, walkClaimTaskID, walkClaimRunID)
	}
	if !strings.Contains(h.invocations[1].Prompt, "PHASE 1") {
		t.Errorf("the retry was not a phase-1 session: %q", h.invocations[1].Prompt)
	}
	if !strings.Contains(h.invocations[1].Prompt, "Resumed: the branch already carried prior work") {
		t.Errorf("the retry's context block does not say the branch already carries the stalled phase's work: %q", h.invocations[1].Prompt)
	}
	if !strings.Contains(h.invocations[2].Prompt, "PHASE 2") {
		t.Errorf("the review phase did not run after the healthy retry: %q", h.invocations[2].Prompt)
	}
	if out := logged.String(); !strings.Contains(out, "SAME claim") {
		t.Errorf("the retry was not named as running inside the held claim:\n%s", out)
	}
}

// CLA-877 review 1.review-1.2: the same-claim retry is not limited to the phase
// the walk started at. A resume-only failure keeps the claim, so `prev` is still
// set and a later phase's retry is seeded exactly as its failed attempt was — so
// a fix/review position that delivered its work and only lost its report is
// retried in place too, without ending the lease or burning a reclaim.
func TestRunnerClaim_ReportResumeFailureInALaterPhaseRetriesInTheSameClaim(t *testing.T) {
	b := walkClaimBrief("CLA-877")
	f := &claimingReleaser{
		offers: []plane.Offer{{Next: &b}},
		claimFn: func(string, bool) (plane.ClaimedTask, error) {
			return plane.ClaimedTask{Task: b, RunID: walkClaimRunID}, nil
		},
	}
	walkClaim := harness.Claim{
		TaskID: walkClaimTaskID, Ref: "CLA-877", RunID: walkClaimRunID,
		HasWIP: true, Branch: "clanker/walk",
	}
	implemented := reported(held(okResult(1, 0), walkClaim), harness.Report{
		TaskID: walkClaimTaskID, Ref: "CLA-877", RunID: walkClaimRunID, Branch: "clanker/walk",
	})
	// The review phase (index 1 of 3, not the phase the walk started at) ends
	// with its run clean and its branch already recorded; only its report
	// resume went silent.
	stalled := held(okResult(1, 0), walkClaim)
	stalled.ReportResumeFailed = "no output for 3m0s"
	retried := held(okResult(1, 0), harness.Claim{TaskID: walkClaimTaskID, RunID: walkClaimRunID, Settled: true})
	h := &fakeAdapter{steps: []invokeStep{
		{res: implemented},
		{res: stalled},
		{res: retried},
	}}
	logged := captureLogs(t)
	phases := []config.Phase{{Name: "implement"}, {Name: "review"}, {Name: "fix"}}
	d := phaseDriverWithReleaser(t, h, phases, f)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 3 {
		t.Fatalf("spawned %d sessions, want 3 (implement, the stalled review, its in-place retry)", h.invokeCalls)
	}
	if len(f.claimCalls) != 1 {
		t.Errorf("claim calls = %v, want only the runner's initial fresh claim — a later phase's retry must not re-claim either", f.claimCalls)
	}
	if got := handbacks(f, walkClaimTaskID); got != 0 {
		t.Errorf("handed the task back %d time(s), want none — the resume-only failure keeps the claim: %+v / %+v",
			got, f.calls, f.expireCalls)
	}
	if !strings.Contains(h.invocations[1].Prompt, "PHASE 2 of 3") || !strings.Contains(h.invocations[2].Prompt, "PHASE 2 of 3") {
		t.Errorf("the retry did not re-run the review position:\nfirst: %q\nretry: %q", h.invocations[1].Prompt, h.invocations[2].Prompt)
	}
	if got := h.invocations[2].ResumeClaim; got.TaskID != walkClaimTaskID || got.RunID != walkClaimRunID {
		t.Errorf("the retry's ResumeClaim = %+v, want the SAME %s/%s the runner holds", got, walkClaimTaskID, walkClaimRunID)
	}
	if out := logged.String(); !strings.Contains(out, "SAME claim") {
		t.Errorf("the retry was not named as running inside the held claim:\n%s", out)
	}
}

// CLA-877 review 1.review-1.4: the start prep's Tip is the revision the tree held
// BEFORE the stalled phase pushed. A same-claim retry must not hand it over as
// the current tip; with no re-claim to refresh it, the context block drops it.
func TestRunnerClaim_ReportResumeRetryDropsTheStalePrepTip(t *testing.T) {
	b := walkClaimBrief("CLA-877")
	f := &claimingReleaser{
		offers: []plane.Offer{{Next: &b}},
		claimFn: func(string, bool) (plane.ClaimedTask, error) {
			return plane.ClaimedTask{Task: b, RunID: walkClaimRunID}, nil
		},
	}
	walkClaim := harness.Claim{
		TaskID: walkClaimTaskID, Ref: "CLA-877", RunID: walkClaimRunID,
		HasWIP: true, Branch: "clanker/walk",
	}
	stalled := held(okResult(1, 0), walkClaim)
	stalled.ReportResumeFailed = "no output for 3m0s"
	retried := reported(held(okResult(1, 0), walkClaim), harness.Report{
		TaskID: walkClaimTaskID, Ref: "CLA-877", RunID: walkClaimRunID, Branch: "clanker/walk",
	})
	h := &fakeAdapter{steps: []invokeStep{
		{res: stalled},
		{res: retried},
		{res: held(okResult(1, 0), harness.Claim{TaskID: walkClaimTaskID, RunID: walkClaimRunID, Settled: true})},
	}}
	d := phaseDriverWithReleaser(t, h, twoPhases(), f)
	const staleTip = "1111111111111111111111111111111111111111"
	d.prepareWorktree = func(context.Context, worktreeRequest) (worktreePrep, error) {
		return worktreePrep{Dir: d.cfg.WorkDir, Created: true, Tip: staleTip}, nil
	}

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 3 {
		t.Fatalf("spawned %d sessions, want 3 (stalled phase, its retry, then review)", h.invokeCalls)
	}
	if !strings.Contains(h.invocations[0].Prompt, "Tip: "+staleTip) {
		t.Fatalf("test precondition: the first attempt's block does not name the prep tip: %q", h.invocations[0].Prompt)
	}
	if strings.Contains(h.invocations[1].Prompt, staleTip) {
		t.Errorf("the retry's block still names the pre-phase tip %s as the current revision:\n%q", staleTip, h.invocations[1].Prompt)
	}
	if !strings.Contains(h.invocations[1].Prompt, "Resumed: the branch already carried prior work") {
		t.Errorf("the retry's block does not say the branch already carries the stalled phase's work: %q", h.invocations[1].Prompt)
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
