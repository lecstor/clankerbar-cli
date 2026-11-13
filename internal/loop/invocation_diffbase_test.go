package loop

// invocation_diffbase_test.go — CLA-865: a brief's `git diff` starts from the
// merge-base SHA the runner computed during worktree prep, or origin/<prBase>
// when no SHA is available. A bare local base branch can be far behind origin,
// and the diff from it then carries unrelated files.

import (
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

const testMergeBaseSHA = "0123456789abcdef0123456789abcdef01234567"

// diffBaseDriver is the shipped-shaped implement/review sequence the
// substitution is read off.
func diffBaseDriver(t *testing.T) (*Driver, []config.Phase) {
	t.Helper()
	h := &fakeAdapter{steps: []invokeStep{{res: okResult(1, 0)}}}
	d, _ := phaseDriver(t, h, []config.Phase{
		{Name: config.ImplementPhaseName},
		{Name: config.ReviewPhaseName},
		{Name: config.FixPhaseName},
	})
	return d, d.cfg.EffectivePhases()
}

func heldReviewClaim() *harness.Result {
	return &harness.Result{
		Claim:         harness.Claim{TaskID: "t-1", RunID: "r-1", Branch: "clanker/abc"},
		CheckpointTip: testCheckpointTip,
	}
}

// The runner's prep SHA reaches the review brief, so `git diff` names the commit
// the runner computed rather than the PR base's branch name.
func TestInvocationForRendersTheRunnerMergeBaseSHA(t *testing.T) {
	d, phases := diffBaseDriver(t)
	runner := &runnerBriefing{
		task:    plane.TaskBrief{ID: "t-1", Ref: "CLA-1", Branch: "clanker/abc"},
		runID:   "r-1",
		start:   true,
		baseSHA: testMergeBaseSHA,
	}
	inv, err := d.invocationFor(d.targets[0], 1, phases[1], heldReviewClaim(), "", "staging", phases, runner, "")
	if err != nil {
		t.Fatalf("invocationFor: %v", err)
	}
	if want := "git diff " + testMergeBaseSHA + "...clanker/abc"; !strings.Contains(inv.Prompt, want) {
		t.Errorf("the review brief does not diff from the runner's merge-base SHA (%q):\n%s", want, inv.Prompt)
	}
	if strings.Contains(inv.Prompt, "git diff staging...") {
		t.Errorf("the review brief still diffs from the bare local PR base:\n%s", inv.Prompt)
	}
	if strings.Contains(inv.Prompt, config.PhaseBasePlaceholder) {
		t.Errorf("the review brief carries an unfilled %s placeholder:\n%s", config.PhaseBasePlaceholder, inv.Prompt)
	}
}

// Without a prep SHA (a session-claimed drain, or a prep whose merge-base could
// not be computed) the fallback is origin/<prBase>: still a remote-tracking ref,
// never the bare local branch.
func TestInvocationForFallsBackToOriginBase(t *testing.T) {
	d, phases := diffBaseDriver(t)
	inv, err := d.invocationFor(d.targets[0], 1, phases[1], heldReviewClaim(), "", "staging", phases, nil, "")
	if err != nil {
		t.Fatalf("invocationFor: %v", err)
	}
	if want := "git diff origin/staging...clanker/abc"; !strings.Contains(inv.Prompt, want) {
		t.Errorf("the review brief does not diff from %q:\n%s", want, inv.Prompt)
	}
	if strings.Contains(inv.Prompt, "git diff staging...") {
		t.Errorf("the review brief still diffs from the bare local PR base:\n%s", inv.Prompt)
	}
}

// The resolved base is exposed to every runner phase, not only the one the walk
// started at: a later phase inherits the start prep's SHA in its context block.
func TestRunnerTaskContextCarriesTheDiffBaseOnEveryPhase(t *testing.T) {
	rb := &runnerBriefing{
		task:    plane.TaskBrief{ID: "t-1", Branch: "clanker/abc"},
		runID:   "r-1",
		baseSHA: testMergeBaseSHA,
	}
	for idx := range runnerTestPhases() {
		c := runnerTaskContext(rb, runnerTestPhases(), idx, "/wt", "clanker/abc", "staging", testCheckpointTip)
		if c.DiffBase != testMergeBaseSHA {
			t.Errorf("phase %d's context DiffBase = %q, want the runner's merge-base %q", idx+1, c.DiffBase, testMergeBaseSHA)
		}
	}
	// And the block renders it, which is what a maps-building review reads.
	block := config.TaskContextBlock(runnerTaskContext(rb, runnerTestPhases(), 1, "/wt", "clanker/abc", "staging", testCheckpointTip))
	if want := "Diff base: " + testMergeBaseSHA; !strings.Contains(block, want) {
		t.Errorf("the context block does not say %q:\n%s", want, block)
	}
	// A briefing with no SHA falls back to origin/<prBase> in the same field.
	fallback := runnerTaskContext(&runnerBriefing{task: rb.task, runID: rb.runID}, runnerTestPhases(), 1, "/wt", "clanker/abc", "staging", testCheckpointTip)
	if fallback.DiffBase != "origin/staging" {
		t.Errorf("DiffBase without a prep SHA = %q, want origin/staging", fallback.DiffBase)
	}
}

// The prep-to-prompt link (CLA-865 review 1.review-1.3): a runner-claimed drain
// whose worktree prep computed a merge-base spawns its review phase with that
// SHA in the diff command AND in the context block. Every other briefing test
// either drives the session-claim path (origin/<base> fallback) or hand-builds a
// runnerBriefing, so deleting baseSHA: s.prep.BaseSHA from drain_seq_phase.go
// would leave them all green. This one drives the production wiring end to end.
func TestDrainPhases_ReviewSpawnsWithThePreparedMergeBase(t *testing.T) {
	brief := wipBrief("CLA-924")
	f := newStartFixture(t, brief, plane.TaskState{Status: "in_progress"}, twoPhases(),
		[]invokeStep{{res: okResult(1, 0)}})
	f.origin = originBranchFacts{Known: true, OnOrigin: true, Tip: startTip, DoneIsAncestor: true}
	f.prep = worktreePrep{Dir: f.prepDir, Created: true, BaseSHA: testMergeBaseSHA}

	if _, _, stop, err := drainPhasesOnce(t, f.d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if f.h.invokeCalls != 1 {
		t.Fatalf("spawned %d sessions, want the review phase only", f.h.invokeCalls)
	}
	inv := f.h.invocations[0]
	if !strings.Contains(inv.Prompt, "PHASE 2") {
		t.Fatalf("the spawn is not the review phase: %q", inv.Prompt)
	}
	if want := "git diff " + testMergeBaseSHA + "..." + brief.Branch; !strings.Contains(inv.Prompt, want) {
		t.Errorf("the review brief does not carry %q, so the prep's merge-base never reached the prompt:\n%s", want, inv.Prompt)
	}
	if want := "Diff base: " + testMergeBaseSHA; !strings.Contains(inv.Prompt, want) {
		t.Errorf("the context block does not carry %q:\n%s", want, inv.Prompt)
	}
	if strings.Contains(inv.Prompt, "git diff origin/") || strings.Contains(inv.Prompt, "git diff staging...") {
		t.Errorf("the review brief fell back to a branch base although the prep computed a SHA:\n%s", inv.Prompt)
	}
}
