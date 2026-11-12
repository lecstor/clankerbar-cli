package loop

// drain_start_test.go — the runner-claimed start, driven through a whole drain:
// which phase the walk begins at, the worktree it runs in, what a claim it
// cannot place is handed back to, and when a refusal is parked instead.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// startTip is the origin tip the start tests record and compare against.
const startTip = "b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0"

// phaseDoneCall is one RecordPhaseDone the fake saw.
type phaseDoneCall struct{ taskID, runID, phase, tip string }

// startReleaser is the claimer plus everything a runner-claimed drain touches
// besides claiming: the phase marker, and the park a deterministic refusal files.
// The plane's task state comes from the embedded claimingReleaser.
type startReleaser struct {
	claimingReleaser
	done      []phaseDoneCall
	doneErr   error
	parkErr   error
	parks     []parkCall
	questions []questionCall
}

func (r *startReleaser) RecordPhaseDone(_ context.Context, taskID, runID, phase, tip string) error {
	r.done = append(r.done, phaseDoneCall{taskID, runID, phase, tip})
	return r.doneErr
}

func (r *startReleaser) Park(_ context.Context, taskID, runID, outcome string) error {
	r.parks = append(r.parks, parkCall{taskID, runID, outcome})
	return r.parkErr
}

func (r *startReleaser) AskQuestion(_ context.Context, taskID, body string, options []string, blocking bool, kind string) error {
	r.questions = append(r.questions, questionCall{taskID, body, options, blocking, kind})
	return nil
}

func (r *startReleaser) AskProjectQuestion(_ context.Context, body string, options []string, kind string) error {
	return nil
}

// startFixture is one runner-claimed drain with the origin read and the
// worktree prep stubbed: the tests set origin/prep/prepErr/prepFn before driving
// it and read preps back afterwards. Every prep lands in its own directory, so a
// spawn dir can be attributed to the prep that produced it.
type startFixture struct {
	d       *Driver
	rel     *startReleaser
	h       *fakeAdapter
	brief   plane.TaskBrief
	origin  originBranchFacts
	prepDir string
	prep    worktreePrep
	prepErr error
	prepFn  func(call int) (worktreePrep, error)
	preps   []worktreeRequest
}

func newStartFixture(t *testing.T, brief plane.TaskBrief, state plane.TaskState, phases []config.Phase, steps []invokeStep) *startFixture {
	t.Helper()
	f := &startFixture{brief: brief, origin: originBranchFacts{Known: true}, prepDir: t.TempDir()}
	f.rel = &startReleaser{claimingReleaser: claimingReleaser{
		offers: []plane.Offer{{Next: &brief}},
		claimFn: func(string, bool) (plane.ClaimedTask, error) {
			return plane.ClaimedTask{Task: brief, RunID: "run-" + brief.ID}, nil
		},
		state: state,
	}}
	f.h = &fakeAdapter{steps: steps}
	f.d = phaseDriverWithReleaser(t, f.h, phases, f.rel)
	f.d.cfg.IdlePollInterval = config.Duration(time.Millisecond)
	f.prep = worktreePrep{Dir: f.prepDir, Created: true}
	f.d.originBranch = func(context.Context, string, string, string) originBranchFacts { return f.origin }
	f.d.prepareWorktree = func(_ context.Context, req worktreeRequest) (worktreePrep, error) {
		f.preps = append(f.preps, req)
		switch {
		case f.prepFn != nil:
			return f.prepFn(len(f.preps))
		case f.prepErr != nil:
			return worktreePrep{}, f.prepErr
		}
		return f.prep, nil
	}
	return f
}

// noWIPBrief is a fresh task: the plane names its branch but nothing is recorded.
func noWIPBrief(ref string) plane.TaskBrief {
	return plane.TaskBrief{ID: walkClaimTaskID, Ref: ref, Branch: "clanker/11111111-no-work"}
}

// wipBrief is a task with work recorded at startTip.
func wipBrief(ref string) plane.TaskBrief {
	return plane.TaskBrief{
		ID: walkClaimTaskID, Ref: ref, Branch: "clanker/11111111-recorded", HasWIP: true,
		PhasesDone: map[string]plane.PhaseDone{config.ImplementPhaseName: {Tip: startTip}},
	}
}

func TestPrepareStart_NoWIPStartsImplementInThePreparedWorktree(t *testing.T) {
	f := newStartFixture(t, noWIPBrief("CLA-910"), plane.TaskState{}, twoPhases(),
		[]invokeStep{{res: okResult(1, 0)}})

	if _, _, stop, err := drainPhasesOnce(t, f.d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if f.h.invokeCalls != 1 {
		t.Fatalf("spawned %d sessions, want the implement phase only", f.h.invokeCalls)
	}
	if !strings.Contains(f.h.invocations[0].Prompt, "PHASE 1") {
		t.Errorf("the first spawn is not the implement phase: %q", f.h.invocations[0].Prompt)
	}
	if got := f.h.invocations[0].WorkDir; got != f.prepDir {
		t.Errorf("spawn dir = %q, want the prepared worktree %q", got, f.prepDir)
	}
	if len(f.preps) != 1 {
		t.Fatalf("prepared the worktree %d time(s), want once", len(f.preps))
	}
	req := f.preps[0]
	if req.Resume || req.NoMerge {
		t.Errorf("worktree request = %+v, want a fresh branch with the PR base merged", req)
	}
	if req.Branch != f.brief.Branch || req.TaskID != f.brief.ID {
		t.Errorf("worktree request = %+v, want the task's own branch and id", req)
	}
}

func TestPrepareStart_ResumesReviewAtTheRecordedTip(t *testing.T) {
	brief := wipBrief("CLA-911")
	f := newStartFixture(t, brief, plane.TaskState{Status: "in_progress"}, twoPhases(),
		[]invokeStep{{res: okResult(1, 0)}})
	f.origin = originBranchFacts{Known: true, OnOrigin: true, Tip: startTip, DoneIsAncestor: true}

	if _, _, stop, err := drainPhasesOnce(t, f.d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if f.h.invokeCalls != 1 {
		t.Fatalf("spawned %d sessions, want the review phase only", f.h.invokeCalls)
	}
	inv := f.h.invocations[0]
	if !strings.Contains(inv.Prompt, "PHASE 2") {
		t.Errorf("the first spawn is not the review phase: %q", inv.Prompt)
	}
	if inv.ResumeClaim.Branch != brief.Branch || inv.ResumeClaim.RunID != "run-"+brief.ID {
		t.Errorf("review ResumeClaim = %+v, want the recorded branch of the runner's claim", inv.ResumeClaim)
	}
	if inv.WorkDir != f.prepDir {
		t.Errorf("spawn dir = %q, want the prepared worktree %q", inv.WorkDir, f.prepDir)
	}
	if len(f.preps) != 1 {
		t.Fatalf("prepared the worktree %d time(s), want once", len(f.preps))
	}
	// A review start must resume origin's tip as it is: merging the PR base would
	// move the revision under the reviewer.
	if !f.preps[0].NoMerge || !f.preps[0].Resume {
		t.Errorf("worktree request = %+v, want Resume with NoMerge", f.preps[0])
	}
}

func TestPrepareStart_UnreadableOriginHandsTheClaimBack(t *testing.T) {
	brief := wipBrief("CLA-912")
	f := newStartFixture(t, brief, plane.TaskState{}, twoPhases(), nil)
	// Known=false is the read failing, not the branch being absent.
	f.origin = originBranchFacts{}

	if _, _, stop, err := drainPhasesOnce(t, f.d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if f.h.invokeCalls != 0 {
		t.Errorf("spawned %d session(s) on an unplaceable claim, want none", f.h.invokeCalls)
	}
	if len(f.preps) != 0 {
		t.Errorf("prepared a worktree for a claim it could not place: %+v", f.preps)
	}
	if got := handbacks(&f.rel.claimingReleaser, brief.ID); got != 1 {
		t.Errorf("handed the task back %d time(s), want exactly 1", got)
	}
}

// A task that cannot be read is unplaceable even with no recorded work: a no-code
// delivery is a fact on that same record.
func TestPrepareStart_UnreadableTaskHandsTheClaimBack(t *testing.T) {
	brief := noWIPBrief("CLA-917")
	f := newStartFixture(t, brief, plane.TaskState{}, twoPhases(), nil)
	f.rel.stateErr = errors.New("the plane is not answering")

	if _, _, stop, err := drainPhasesOnce(t, f.d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if f.h.invokeCalls != 0 {
		t.Errorf("spawned %d session(s) without the task's record, want none", f.h.invokeCalls)
	}
	if got := handbacks(&f.rel.claimingReleaser, brief.ID); got != 1 {
		t.Errorf("handed the task back %d time(s), want exactly 1", got)
	}
}

func TestPrepareStart_UnpushedCommitsFallBackToImplement(t *testing.T) {
	brief := wipBrief("CLA-914")
	f := newStartFixture(t, brief, plane.TaskState{Status: "in_progress"}, twoPhases(),
		[]invokeStep{{res: okResult(1, 0)}})
	f.origin = originBranchFacts{Known: true, OnOrigin: true, Tip: startTip, DoneIsAncestor: true}
	// The tree holds a commit origin does not, so implement takes the tree as it
	// is and the PR base is merged into the reuse.
	f.prepFn = func(call int) (worktreePrep, error) {
		if call == 1 {
			return worktreePrep{Dir: f.prepDir, Ahead: 1}, nil
		}
		return worktreePrep{Dir: f.prepDir}, nil
	}

	if _, _, stop, err := drainPhasesOnce(t, f.d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if f.h.invokeCalls != 1 {
		t.Fatalf("spawned %d sessions, want the implement phase only", f.h.invokeCalls)
	}
	if !strings.Contains(f.h.invocations[0].Prompt, "PHASE 1") {
		t.Errorf("the fallback start is not the implement phase: %q", f.h.invocations[0].Prompt)
	}
	if got := f.h.invocations[0].WorkDir; got != f.prepDir {
		t.Errorf("spawn dir = %q, want the worktree the second prep settled on %q", got, f.prepDir)
	}
	if len(f.preps) != 2 {
		t.Fatalf("prepared the worktree %d time(s), want the first look and the implement re-prep", len(f.preps))
	}
	if !f.preps[0].NoMerge || f.preps[1].NoMerge {
		t.Errorf("NoMerge = %v/%v, want the re-prep to merge the PR base", f.preps[0].NoMerge, f.preps[1].NoMerge)
	}
}

// Origin moving between the fact read and the prep leaves the chosen checkpoint
// stale, so the claim goes back for a fresh start — and that is not a refusal.
func TestPrepareStart_OriginMovedHandsTheClaimBack(t *testing.T) {
	brief := wipBrief("CLA-918")
	f := newStartFixture(t, brief, plane.TaskState{Status: "in_progress"}, twoPhases(), nil)
	f.origin = originBranchFacts{Known: true, OnOrigin: true, Tip: startTip, DoneIsAncestor: true}
	f.prep = worktreePrep{Dir: f.prepDir, OriginTip: "c0ffee00"}

	if _, _, stop, err := drainPhasesOnce(t, f.d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if f.h.invokeCalls != 0 {
		t.Errorf("spawned %d session(s) at a stale checkpoint, want none", f.h.invokeCalls)
	}
	if got := handbacks(&f.rel.claimingReleaser, brief.ID); got != 1 {
		t.Errorf("handed the task back %d time(s), want exactly 1", got)
	}
	if got := f.d.startRefusals[brief.ID]; got != 0 {
		t.Errorf("counted the stale checkpoint as %d worktree refusal(s), want none", got)
	}
}

// A reclaim can land a different task. Nothing from the previous task's start may
// survive into it: task B is branchless, so B's session must not run in A's tree.
func TestPrepareStart_ReclaimToAnotherTaskDropsThePreviousWorktree(t *testing.T) {
	briefA := plane.TaskBrief{ID: walkClaimTaskID, Ref: "CLA-919", Branch: "clanker/11111111-a"}
	briefB := plane.TaskBrief{ID: "22222222-3333-4444-5555-666666666666", Ref: "CLA-920", Branch: "clanker/22222222-b"}
	f := newStartFixture(t, briefA, plane.TaskState{}, twoPhases(), []invokeStep{
		// Task A's implement dies producing nothing, so the dead-phase retry
		// re-claims — and by then A is gone.
		{res: held(deadResult(), harness.Claim{TaskID: briefA.ID, RunID: "run-a"})},
		{res: okResult(1, 0)},
	})
	f.rel.offers = []plane.Offer{{Next: &briefA}, {Next: &briefB}}
	f.rel.states = map[string]plane.TaskState{briefB.ID: {DeliveryNoCode: true}}
	aClaims := 0
	f.rel.claimFn = func(taskID string, _ bool) (plane.ClaimedTask, error) {
		if taskID == briefB.ID {
			return plane.ClaimedTask{Task: briefB, RunID: "run-b"}, nil
		}
		aClaims++
		if aClaims == 1 {
			return plane.ClaimedTask{Task: briefA, RunID: "run-a"}, nil
		}
		return plane.ClaimedTask{}, fmt.Errorf("%w: the dead phase gave it back", plane.ErrNotClaimable)
	}
	dirA := t.TempDir()
	f.d.prepareWorktree = func(_ context.Context, req worktreeRequest) (worktreePrep, error) {
		f.preps = append(f.preps, req)
		return worktreePrep{Dir: dirA, Created: true}, nil
	}

	if _, _, stop, err := drainPhasesOnce(t, f.d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if f.h.invokeCalls != 2 {
		t.Fatalf("spawned %d sessions, want task A's implement and task B's review", f.h.invokeCalls)
	}
	if got := f.h.invocations[0].WorkDir; got != dirA {
		t.Fatalf("task A's spawn dir = %q, want its prepared worktree %q", got, dirA)
	}
	if got := f.h.invocations[1].WorkDir; got == dirA {
		t.Fatalf("task B spawned in task A's worktree %q; a branchless start has no tree to run in", got)
	}
	if !strings.Contains(f.h.invocations[1].Prompt, "PHASE 2") {
		t.Errorf("task B did not start at the branchless review: %q", f.h.invocations[1].Prompt)
	}
	if len(f.preps) != 1 {
		t.Errorf("prepared %d worktrees, want only task A's (task B is branchless)", len(f.preps))
	}
}

// A dirty or diverged tree refuses every claim, so the second refusal parks the
// task for the operator instead of returning it to the queue a third time.
func TestPrepareStart_SecondRefusalParksTheTask(t *testing.T) {
	brief := noWIPBrief("CLA-921")
	f := newStartFixture(t, brief, plane.TaskState{}, twoPhases(), nil)
	f.rel.offers = []plane.Offer{{Next: &brief}, {Next: &brief}}
	f.prepErr = errors.New("worktree /repo-wt/11111111 has uncommitted changes (M a.txt) — refusing to reuse it")

	for i := 0; i < 2; i++ {
		if _, _, stop, err := drainPhasesOnce(t, f.d); err != nil || stop {
			t.Fatalf("drain %d: err=%v stop=%v", i+1, err, stop)
		}
	}
	if f.h.invokeCalls != 0 {
		t.Errorf("spawned %d session(s) with no worktree, want none", f.h.invokeCalls)
	}
	if len(f.rel.parks) != 1 {
		t.Fatalf("parks = %+v, want the second refusal to park the task once", f.rel.parks)
	}
	if got := f.rel.parks[0]; got.taskID != brief.ID || got.runID != "run-"+brief.ID {
		t.Errorf("parked %+v, want %s signed by its claim", got, brief.ID)
	}
	if len(f.rel.questions) != 1 {
		t.Errorf("filed %d question(s), want the park's one", len(f.rel.questions))
	}
	if got := handbacks(&f.rel.claimingReleaser, brief.ID); got != 1 {
		t.Errorf("handed the task back %d time(s), want only the first refusal (%+v / %+v)",
			got, f.rel.calls, f.rel.expireCalls)
	}
}

// A park the plane refuses must not swallow the claim: the task is NOT parked, so
// it is handed back and the next refusal retries the park immediately.
func TestPrepareStart_ParkFailureHandsBackAndRetriesNextRefusal(t *testing.T) {
	brief := noWIPBrief("CLA-924")
	f := newStartFixture(t, brief, plane.TaskState{}, twoPhases(), nil)
	f.rel.offers = []plane.Offer{{Next: &brief}, {Next: &brief}, {Next: &brief}}
	f.prepErr = errors.New("worktree /repo-wt/11111111 is detached, not on the branch — refusing to reuse it")
	f.rel.parkErr = errors.New("the plane refused the park")

	// Refusal 1 hands back; refusal 2 tries the park, which fails, so it hands
	// back too.
	for i := 0; i < 2; i++ {
		if _, _, stop, err := drainPhasesOnce(t, f.d); err != nil || stop {
			t.Fatalf("drain %d: err=%v stop=%v", i+1, err, stop)
		}
	}
	if got := handbacks(&f.rel.claimingReleaser, brief.ID); got != 2 {
		t.Fatalf("handed the task back %d time(s), want 2: a failed park leaves the claim live", got)
	}
	if len(f.rel.parks) != 1 {
		t.Fatalf("parks = %+v, want the park attempted on the second refusal", f.rel.parks)
	}

	// The third refusal retries the park, which now takes: parked, nothing to
	// hand back.
	f.rel.parkErr = nil
	if _, _, stop, err := drainPhasesOnce(t, f.d); err != nil || stop {
		t.Fatalf("drain 3: err=%v stop=%v", err, stop)
	}
	if len(f.rel.parks) != 2 {
		t.Errorf("parks = %+v, want the third refusal to retry the park", f.rel.parks)
	}
	if len(f.rel.questions) != 1 {
		t.Errorf("filed %d question(s), want the parked task's one", len(f.rel.questions))
	}
	if got := handbacks(&f.rel.claimingReleaser, brief.ID); got != 2 {
		t.Errorf("handed the task back %d time(s), want no handback for a parked task", got)
	}
}

// A fetch failure is transient: it never counts toward the park, however often
// it recurs.
func TestPrepareStart_FetchFailureIsHandedBackNotParked(t *testing.T) {
	brief := noWIPBrief("CLA-922")
	f := newStartFixture(t, brief, plane.TaskState{}, twoPhases(), nil)
	f.rel.offers = []plane.Offer{{Next: &brief}, {Next: &brief}}
	f.prepErr = fmt.Errorf("%w in %s: %v", errWorktreeFetch, "/repo", errors.New("no such remote"))

	for i := 0; i < 2; i++ {
		if _, _, stop, err := drainPhasesOnce(t, f.d); err != nil || stop {
			t.Fatalf("drain %d: err=%v stop=%v", i+1, err, stop)
		}
	}
	if len(f.rel.parks) != 0 {
		t.Errorf("parks = %+v, want a transient fetch failure never to park", f.rel.parks)
	}
	if got := handbacks(&f.rel.claimingReleaser, brief.ID); got != 2 {
		t.Errorf("handed the task back %d time(s), want once per claim", got)
	}
	if got := f.d.startRefusals[brief.ID]; got != 0 {
		t.Errorf("counted the fetch failure as %d worktree refusal(s), want none", got)
	}
}

func TestMarkPhaseDone_RecordsACheckpointedImplementPhase(t *testing.T) {
	f := newStartFixture(t, noWIPBrief("CLA-915"), plane.TaskState{}, twoPhases(),
		[]invokeStep{{res: checkpointed(1, 0)}, {res: okResult(1, 0)}})

	if _, _, stop, err := drainPhasesOnce(t, f.d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	// The checkpoint's claim is the SESSION's own (t-1/r-1), not the seeded one:
	// it is the run that actually produced the tip.
	want := phaseDoneCall{"t-1", "r-1", config.ImplementPhaseName, testCheckpointTip}
	if len(f.rel.done) != 1 || f.rel.done[0] != want {
		t.Fatalf("RecordPhaseDone calls = %+v, want exactly [%+v]", f.rel.done, want)
	}
}

func TestMarkPhaseDone_SkipsAPhaseThatNeverCheckpointed(t *testing.T) {
	f := newStartFixture(t, noWIPBrief("CLA-916"), plane.TaskState{}, twoPhases(),
		[]invokeStep{{res: okResult(1, 0)}})

	if _, _, stop, err := drainPhasesOnce(t, f.d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if len(f.rel.done) != 0 {
		t.Errorf("RecordPhaseDone calls = %+v, want none without a checkpoint", f.rel.done)
	}
}

// A marker the plane refuses costs the next claim its shortcut, nothing more: the
// sequence carries on to the next phase.
func TestMarkPhaseDone_FailureDoesNotEndTheWalk(t *testing.T) {
	f := newStartFixture(t, noWIPBrief("CLA-923"), plane.TaskState{}, twoPhases(),
		[]invokeStep{{res: checkpointed(1, 0)}, {res: okResult(1, 0)}})
	f.rel.doneErr = errors.New("the plane refused the marker")

	if _, _, stop, err := drainPhasesOnce(t, f.d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if f.h.invokeCalls != 2 {
		t.Errorf("spawned %d sessions, want the review phase to run anyway", f.h.invokeCalls)
	}
	if len(f.rel.done) != 1 {
		t.Errorf("RecordPhaseDone calls = %+v, want the marker attempted once", f.rel.done)
	}
}
