package config

import (
	"strings"
	"testing"
)

// A config with no phases must behave exactly as it always has. This is the
// back-compat guarantee that lets phases be opt-in rather than a migration.
func TestEffectivePhases_UnphasedIsOnePhaseCarryingThePrompt(t *testing.T) {
	c := defaults()
	c.Prompt = "Work the next backlog item."

	got := c.EffectivePhases()
	if len(got) != 1 {
		t.Fatalf("an unphased config resolved to %d phases, want exactly 1: %+v", len(got), got)
	}
	if got[0].Prompt != c.Prompt {
		t.Errorf("the single phase carries %q, want the configured prompt %q", got[0].Prompt, c.Prompt)
	}
	if got[0].MaxTurns != 0 {
		t.Errorf("an unphased run picked up a turn cap of %d; nothing should bound it but its prompt", got[0].MaxTurns)
	}
}

func TestEffectivePhases_ResolvesBuiltinsByNameAndLetsAPromptOverride(t *testing.T) {
	c := defaults()
	c.Prompt = ""
	c.Phases = []Phase{
		{Name: "implement"},
		{Name: "review", Prompt: "my own brief"},
	}

	got := c.EffectivePhases()
	if len(got) != 2 {
		t.Fatalf("got %d phases, want 2", len(got))
	}
	if got[0].Prompt != builtinPhasePrompts["implement"] {
		t.Errorf("a named phase with no prompt did not take its built-in brief; got %q", got[0].Prompt)
	}
	if got[1].Prompt != "my own brief" {
		t.Errorf("an explicit prompt was overwritten by the built-in; got %q", got[1].Prompt)
	}
	// Resolution must not mutate the config it read.
	if c.Phases[0].Prompt != "" {
		t.Error("EffectivePhases wrote a resolved prompt back into the config")
	}
}

// The phase-1 brief is an interface, exactly as the default prompt is: it is read
// by an agent that has read the served protocol, and the whole mechanism is that
// the session STOPS at the checkpoint. A paraphrase that drops the stop turns the
// split back into one long session while every other test stays green.
func TestBuiltinImplementPhaseTellsTheSessionToStopAtTheCheckpoint(t *testing.T) {
	brief := builtinPhasePrompts["implement"]
	if brief == "" {
		t.Fatal("no built-in brief for the implement phase")
	}
	lower := strings.ToLower(brief)
	for _, want := range []string{"push", "stop"} {
		if !strings.Contains(lower, want) {
			t.Errorf("the implement brief never says %q; it must push its work and then stop:\n%s", want, brief)
		}
	}
	// The two things it must forbid, because doing either makes the second phase
	// pointless: reviewing its own work, and closing the task itself.
	if !strings.Contains(lower, "in_review") {
		t.Errorf("the implement brief does not tell the session to leave in_review alone:\n%s", brief)
	}
}

// The resume brief is useless without the ids, and they are substituted by the
// driver — so the placeholders have to actually be in the shipped text.
func TestBuiltinReviewPhaseCarriesTheResumePlaceholders(t *testing.T) {
	brief := builtinPhasePrompts["review"]
	for _, ph := range []string{PhaseTaskPlaceholder, PhaseRunPlaceholder} {
		if !strings.Contains(brief, ph) {
			t.Errorf("the review brief is missing the %s placeholder, so a resuming session is never told which run it is on:\n%s", ph, brief)
		}
	}
	if !strings.Contains(strings.ToLower(brief), "heartbeat") {
		t.Errorf("the review brief does not tell the session to resume the run with heartbeat:\n%s", brief)
	}
}

func TestValidate_PhasesAndPromptAreMutuallyExclusive(t *testing.T) {
	c := defaults()
	c.Prompt = "something the operator wrote"
	c.Phases = []Phase{{Name: "implement"}, {Name: "review"}}

	err := c.Validate()
	if err == nil {
		t.Fatal("Validate accepted both `prompt` and `phases`; one of the two would then be silently ignored")
	}
	if !strings.Contains(err.Error(), "prompt") || !strings.Contains(err.Error(), "phases") {
		t.Errorf("the rejection names neither field, so it is not actionable: %v", err)
	}
}

// The residue of detecting "set" by comparing against the default: a prompt left
// untouched is not a conflict, because defaults() filled it before the operator's
// file was ever layered on top.
func TestValidate_AnUntouchedDefaultPromptDoesNotConflictWithPhases(t *testing.T) {
	c := defaults()
	c.Phases = []Phase{{Name: "implement"}, {Name: "review"}}

	if err := c.Validate(); err != nil {
		t.Fatalf("Validate rejected phases alongside the untouched default prompt: %v", err)
	}
}

func TestValidate_RejectsAPhaseWithNoPromptAndNoBuiltin(t *testing.T) {
	c := defaults()
	c.Prompt = ""
	c.Phases = []Phase{{Name: "implement"}, {Name: "not-a-real-phase"}}

	err := c.Validate()
	if err == nil {
		t.Fatal("Validate accepted a phase that resolves to an empty prompt; its session would be spawned with nothing to do")
	}
	if !strings.Contains(err.Error(), "not-a-real-phase") {
		t.Errorf("the rejection does not name the offending phase: %v", err)
	}
	for _, n := range BuiltinPhaseNames() {
		if !strings.Contains(err.Error(), n) {
			t.Errorf("the rejection does not list the built-in name %q, so an operator cannot see what they meant to type: %v", n, err)
		}
	}
}

func TestValidate_RejectsANegativeTurnCap(t *testing.T) {
	c := defaults()
	c.Prompt = ""
	c.Phases = []Phase{{Name: "implement", MaxTurns: -1}}

	if err := c.Validate(); err == nil {
		t.Fatal("Validate accepted a negative max_turns")
	}
}

// An unphased config with an empty prompt is still the old error, unchanged: the
// new phases check must not have swallowed it.
func TestValidate_StillRejectsAnEmptyPromptWhenUnphased(t *testing.T) {
	c := defaults()
	c.Prompt = ""

	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "prompt is empty") {
		t.Fatalf("Validate() = %v, want the empty-prompt rejection", err)
	}
}
