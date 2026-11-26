package loop

// invocation_test.go — CLA-1032: {{taskRef}} carries the task's qualified ref into a terminal
// phase's brief, so the PR it opens names the Clankerbar task in its title.

import (
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// taskRefPhases is a sequence whose second phase carries {{taskRef}}, the shape every PR-opening
// terminal brief has.
func taskRefPhases() []config.Phase {
	return []config.Phase{
		{Name: config.ImplementPhaseName},
		{Name: config.ReviewPhaseName, Prompt: "Open the PR as " + config.PhaseTaskRefPlaceholder},
	}
}

func taskRefPrompt(t *testing.T, claim harness.Claim, runner *runnerBriefing) string {
	t.Helper()
	d, _ := phaseDriver(t, &fakeAdapter{}, taskRefPhases())
	phases := d.cfg.EffectivePhases()
	prev := &harness.Result{Claim: claim}
	inv, err := d.invocationFor(d.targets[0], 1, phases[1], prev, "", "staging", phases, runner, "")
	if err != nil {
		t.Fatalf("invocationFor: %v", err)
	}
	return inv.Prompt
}

// The claim's own ref is the first source: it is the identifier the session itself wrote on the
// plane, so a ref and a UUID can never disagree about which task the PR belongs to. The runner's
// ref differs on purpose, so the test pins the ORDER, not merely that some ref was found.
func TestInvocationForPrefersTheClaimsOwnRef(t *testing.T) {
	got := taskRefPrompt(t,
		harness.Claim{TaskID: "t-1", Ref: "CLA-1032", RunID: "r-1", Branch: "clanker/x"},
		&runnerBriefing{task: plane.TaskBrief{ID: "t-1", Ref: "CLA-777"}, runID: "r-1"})
	if want := "Open the PR as CLA-1032"; !strings.Contains(got, want) {
		t.Errorf("the brief does not carry %q:\n%s", want, got)
	}
	if strings.Contains(got, "CLA-777") {
		t.Errorf("the brief took the runner's ref over the claim's own:\n%s", got)
	}
	if strings.Contains(got, config.PhaseTaskRefPlaceholder) {
		t.Errorf("the brief carries an unfilled %s placeholder:\n%s", config.PhaseTaskRefPlaceholder, got)
	}
}

// A runner-claimed session's observed claim does not always carry the ref; the runner's own task
// brief does, and a UUID in a PR title would not match the task back.
func TestInvocationForFallsBackToTheRunnersTaskRef(t *testing.T) {
	got := taskRefPrompt(t,
		harness.Claim{TaskID: "t-1", RunID: "r-1", Branch: "clanker/x"},
		&runnerBriefing{task: plane.TaskBrief{ID: "t-1", Ref: "CLA-777"}, runID: "r-1"})
	if want := "Open the PR as CLA-777"; !strings.Contains(got, want) {
		t.Errorf("the brief does not carry the runner's task ref %q:\n%s", want, got)
	}
	if strings.Contains(got, config.PhaseTaskRefPlaceholder) {
		t.Errorf("the brief carries an unfilled %s placeholder:\n%s", config.PhaseTaskRefPlaceholder, got)
	}
}

// With no ref anywhere the fallback is the UUID - the same last resort claimLabel uses, so a
// title still names the task rather than leaving a literal placeholder.
func TestInvocationForFallsBackToTheTaskIDWhenNoRefExists(t *testing.T) {
	got := taskRefPrompt(t, harness.Claim{TaskID: "t-1", RunID: "r-1", Branch: "clanker/x"}, nil)
	if want := "Open the PR as t-1"; !strings.Contains(got, want) {
		t.Errorf("the brief does not fall back to the task ID %q:\n%s", want, got)
	}
	if strings.Contains(got, config.PhaseTaskRefPlaceholder) {
		t.Errorf("the brief carries an unfilled %s placeholder:\n%s", config.PhaseTaskRefPlaceholder, got)
	}
}
