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
	inv, err := d.invocationFor(d.targets[0], 1, phases[1], heldReviewClaim(), "", "staging", phases, runner)
	if err != nil {
		t.Fatalf("invocationFor: %v", err)
	}
	if want := "git diff " + testMergeBaseSHA + "...clanker/abc"; !strings.Contains(inv.Prompt, want) {
		t.Errorf("the review brief does not diffuse from the runner's merge-base SHA (%q):\n%s", want, inv.Prompt)
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
	inv, err := d.invocationFor(d.targets[0], 1, phases[1], heldReviewClaim(), "", "staging", phases, nil)
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
