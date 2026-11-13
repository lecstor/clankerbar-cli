package loop

import (
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// runnerTestPhases is a shipped-shaped sequence, so a phase name has something
// to be read off.
func runnerTestPhases() []config.Phase {
	return []config.Phase{
		{Name: config.ImplementPhaseName},
		{Name: config.ReviewPhaseName},
		{Name: config.FixPhaseName},
	}
}

func TestRunnerPromptSelectsTheRunnerBriefs(t *testing.T) {
	rb := &runnerBriefing{task: plane.TaskBrief{ID: "t-1", Branch: "clanker/abc"}, runID: "r-1"}
	closeOut := &runnerBriefing{task: rb.task, runID: rb.runID, start: true, closeOut: true}
	for _, tc := range []struct {
		name        string
		prompt      string
		rb          *runnerBriefing
		panelMember bool
		want        string
	}{
		{"implement", config.BuiltinImplementBrief(), rb, false, config.RunnerImplementBrief()},
		{"close-out", config.BuiltinImplementBrief(), closeOut, false, config.RunnerCloseOutBrief()},
		{"review only", config.ReviewOnlyBrief(), rb, false, config.RunnerReviewOnlyBrief()},
		{"fix", config.FixBrief(), rb, false, config.RunnerFixBrief()},
		{"fix verify", config.FixVerifyBrief(), rb, false, config.RunnerFixVerifyBrief()},
		{"verify", config.VerifyBrief(), rb, false, config.RunnerVerifyBrief()},
		{"no-code variant unchanged", config.NoCodeReviewOnlyBrief(), rb, false, config.NoCodeReviewOnlyBrief()},
		{"custom prompt unchanged", "do the thing", rb, false, "do the thing"},
		{"no briefing unchanged", config.BuiltinImplementBrief(), nil, false, config.BuiltinImplementBrief()},
		{"panel member unchanged", config.ReviewOnlyBrief(), rb, true, config.ReviewOnlyBrief()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := runnerPrompt(tc.prompt, tc.rb, tc.panelMember); got != tc.want {
				t.Errorf("runnerPrompt = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRunnerTaskContextFillsTheStartPhase(t *testing.T) {
	rb := &runnerBriefing{
		task: plane.TaskBrief{
			ID: "t-1", Ref: "CLA-1", Title: "Fix it", Detail: "broken", DoneWhen: "works",
			Branch: "clanker/abc", HasWIP: true, MergeHint: "release it",
			Decisions: []plane.Decision{{Question: "base?", Answer: "staging"}, {Question: "test?", Answer: "yes"}},
			Questions: []plane.AnsweredQuestion{
				{Body: "delete?", Answer: "yes", Blocking: true},
				{Body: "rename?", Answer: "no"},
			},
		},
		runID:    "r-1",
		prep:     worktreePrep{Tip: "abc1234", Merged: true, Conflicts: []string{"a.go"}},
		start:    true,
		closeOut: true,
	}
	c := runnerTaskContext(rb, runnerTestPhases(), 0, "/wt", "clanker/abc", "staging", "abc1234")
	for _, check := range []struct {
		name      string
		got, want any
	}{
		{"ref", c.Ref, "CLA-1"},
		{"title", c.Title, "Fix it"},
		{"task id", c.TaskID, "t-1"},
		{"run id", c.RunID, "r-1"},
		{"detail", c.Detail, "broken"},
		{"done when", c.DoneWhen, "works"},
		{"phase number", c.PhaseNumber, 1},
		{"phase total", c.PhaseTotal, 3},
		{"phase name", c.PhaseName, config.ImplementPhaseName},
		{"worktree", c.Worktree, "/wt"},
		{"branch", c.Branch, "clanker/abc"},
		{"pr base", c.PRBase, "staging"},
		{"tip", c.Tip, "abc1234"},
		{"resumed", c.Resumed, true},
		{"merge", c.Merge, "merged"},
		{"close out", c.CloseOut, true},
		{"merge hint", c.MergeHint, "release it"},
		{"runner next", c.RunnerNext, "When this session ends, the close-out is over and the runner ends the run."},
	} {
		if check.got != check.want {
			t.Errorf("%s = %v, want %v", check.name, check.got, check.want)
		}
	}
	if len(c.Conflicts) != 1 || c.Conflicts[0] != "a.go" {
		t.Errorf("conflicts = %v, want [a.go]", c.Conflicts)
	}
	// The plane serves both lists newest-first and the block renders them as
	// served: reordering here would put an older ruling before the one that
	// superseded it.
	if len(c.Decisions) != 2 || c.Decisions[0].Question != "base?" || c.Decisions[1].Question != "test?" {
		t.Errorf("decision order changed: %v", c.Decisions)
	}
	if len(c.Questions) != 2 || c.Questions[0].Body != "delete?" || !c.Questions[0].Blocking || c.Questions[1].Body != "rename?" {
		t.Errorf("question order or shape changed: %v", c.Questions)
	}
}

// The prep facts belong to the phase the walk started at: a later phase did not
// see the runner's merge and must not be told one happened.
func TestRunnerTaskContextOmitsThePrepOffTheStartPhase(t *testing.T) {
	rb := &runnerBriefing{
		task: plane.TaskBrief{
			ID: "t-1", Branch: "clanker/abc", HasWIP: true, MergeHint: "release it",
		},
		runID: "r-1",
		prep:  worktreePrep{Tip: "abc1234", UpToDate: true, Conflicts: []string{"a.go"}},
	}
	c := runnerTaskContext(rb, runnerTestPhases(), 1, "/wt", "clanker/abc", "staging", "abc1234")
	if c.Resumed || c.Merge != "" || len(c.Conflicts) != 0 {
		t.Errorf("a non-start phase carries the start phase's prep: resumed=%v merge=%q conflicts=%v", c.Resumed, c.Merge, c.Conflicts)
	}
	if c.MergeHint != "" || c.CloseOut {
		t.Errorf("a non-close-out phase carries the close-out facts: hint=%q closeOut=%v", c.MergeHint, c.CloseOut)
	}
	if want := "When this session ends, the runner checks this phase's evidence (the pushed branch, or the durable review record) and then starts phase 3 of 3 (fix)."; c.RunnerNext != want {
		t.Errorf("RunnerNext = %q, want %q", c.RunnerNext, want)
	}
	if c.PhaseNumber != 2 || c.PhaseName != config.ReviewPhaseName {
		t.Errorf("phase = %d (%q), want 2 (%q)", c.PhaseNumber, c.PhaseName, config.ReviewPhaseName)
	}

	last := runnerTaskContext(rb, runnerTestPhases(), 2, "/wt", "clanker/abc", "staging", "abc1234")
	if want := "This is the last phase: when this session ends, the runner checks the hand-over and the run ends."; last.RunnerNext != want {
		t.Errorf("RunnerNext for the last phase = %q, want %q", last.RunnerNext, want)
	}

	// An out-of-range index must not panic on the label lookup.
	outOfRange := runnerTaskContext(rb, runnerTestPhases(), 5, "/wt", "clanker/abc", "staging", "abc1234")
	if outOfRange.PhaseName != "" {
		t.Errorf("an out-of-range phase named %q", outOfRange.PhaseName)
	}
}

// A fresh runner claim records no branch yet, so the runner implement brief must
// be filled from the plane's own branch name - and the shipped brief's claim
// step must be gone.
func TestInvocationForAppliesTheRunnerImplementBrief(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{{res: okResult(1, 0)}}}
	d, _ := phaseDriver(t, h, []config.Phase{
		{Name: config.ImplementPhaseName},
		{Name: config.ReviewPhaseName},
	})
	phases := d.cfg.EffectivePhases()
	rb := &runnerBriefing{
		task:  plane.TaskBrief{ID: "t-1", Ref: "CLA-1", Title: "Fix it", Branch: "clanker/abc-1-title"},
		runID: "r-1",
		start: true,
	}
	prev := &harness.Result{Claim: harness.Claim{TaskID: "t-1", RunID: "r-1"}}
	inv, err := d.invocationFor(d.targets[0], 0, phases[0], prev, "", "staging", phases, rb, "")
	if err != nil {
		t.Fatalf("invocationFor: %v", err)
	}
	if !strings.Contains(inv.Prompt, "The task is already claimed and your worktree is prepared") {
		t.Errorf("the runner implement brief was not selected:\n%s", inv.Prompt)
	}
	if strings.Contains(inv.Prompt, "claim the task") {
		t.Errorf("the shipped claim step survived into the runner brief:\n%s", inv.Prompt)
	}
	if !strings.Contains(inv.Prompt, rb.task.Branch) {
		t.Errorf("the runner brief does not name the plane's branch %q:\n%s", rb.task.Branch, inv.Prompt)
	}
	if strings.Contains(inv.Prompt, config.PhaseBranchPlaceholder) {
		t.Errorf("the runner brief carries an unresolved branch placeholder:\n%s", inv.Prompt)
	}
}
