package config

import (
	"strings"
	"testing"
)

// conflictBriefInput is a fully-populated brief input: every field the supervisor
// resolves before rendering, so a test that varies one field is varying the only
// thing that could explain a difference.
func conflictBriefInput() ConflictBriefInput {
	return ConflictBriefInput{
		TaskID:   "task-1",
		TaskRef:  "CLA-1",
		RunID:    "run-1.secret",
		Branch:   "clanker/task-1",
		Base:     "staging",
		PR:       "#12",
		Repo:     "acme/widgets",
		WorkDir:  "/fleet/acme/widgets",
		Worktree: "/fleet/acme/worktrees/conflict-task-1",
		Paths:    []string{"a.txt", "b.txt"},
		Gate:     "go test ./...",
	}
}

func TestConflictBriefCarriesTheConflictOnlyContract(t *testing.T) {
	brief := ConflictBrief(conflictBriefInput())
	lower := strings.ToLower(brief)

	exact := []string{
		// the task, the claim and the branch it must merge into
		"CLA-1", "task-1", "run-1.secret", "clanker/task-1", "staging", "#12", "acme/widgets",
		// the conflicting paths, the gate, the worktree and the push
		"a.txt, b.txt", "go test ./...", "/fleet/acme/worktrees/conflict-task-1", "push origin HEAD:clanker/task-1",
		// the hand-back markers the supervisor classifies on
		"RESOLUTION: mechanical", "RESOLUTION: behavioural", "RESOLUTION: failed",
	}
	for _, want := range exact {
		if !strings.Contains(brief, want) {
			t.Errorf("the brief does not carry %q:\n%s", want, brief)
		}
	}
	// The contract phrases are prose, and prose is capitalised per sentence:
	// checked case-insensitively so a heading's capitals are not a failure.
	for _, want := range []string{
		"merge, never rebase",
		"only the conflicting hunks",
		"never force-push",
		"do not call claim_task",
		"do not call next_task",
		"never merge the pr",
	} {
		if !strings.Contains(lower, want) {
			t.Errorf("the brief does not say %q:\n%s", want, brief)
		}
	}
}

func TestConflictBriefFallsBackWhenPathsOrGateAreUnknown(t *testing.T) {
	in := conflictBriefInput()
	in.Paths = nil
	in.Gate = ""
	brief := ConflictBrief(in)

	if !strings.Contains(brief, "the files the merge reports as conflicting") {
		t.Errorf("an unknown path list does not fall back to the merge's own report:\n%s", brief)
	}
	if !strings.Contains(brief, "the repo's own documented full gate") {
		t.Errorf("an unknown gate does not fall back to the repo's documented gate:\n%s", brief)
	}
	if strings.Contains(brief, "``") {
		t.Errorf("the brief renders an empty command:\n%s", brief)
	}
}

func TestConflictStepDefaultsToTheImplementStep(t *testing.T) {
	c := defaults()
	c.Harness = "opencode"
	c.Model = "run-model"
	c.Models = map[string]string{"cheap": "cheap-model"}
	c.Phases = []Phase{
		{Name: ImplementPhaseName, Tier: "cheap"},
		{Name: ReviewPhaseName},
	}
	c.Conflict = nil

	step := c.ConflictStep()
	if step.Harness != "opencode" {
		t.Errorf("harness = %q, want the implement phase's opencode", step.Harness)
	}
	if step.Model != "cheap-model" {
		t.Errorf("model = %q, want the implement phase's tiered cheap-model", step.Model)
	}
}

func TestConflictStepHonoursItsOwnBlock(t *testing.T) {
	c := defaults()
	c.Harness = "opencode"
	c.Model = "run-model"
	c.Harnesses = map[string]HarnessConfig{"codex": {Model: "codex-model"}}
	c.Conflict = &ConflictStep{Harness: "codex", MaxTurns: 25}

	step := c.ConflictStep()
	if step.Harness != "codex" {
		t.Errorf("harness = %q, want the block's codex", step.Harness)
	}
	// A different harness is a different dialect: its own default model, never
	// the run-wide alias the other harness serves.
	if step.Model != "codex-model" {
		t.Errorf("model = %q, want codex's own codex-model (not the run-wide run-model)", step.Model)
	}
	if step.MaxTurns != 25 {
		t.Errorf("max turns = %d, want the block's 25", step.MaxTurns)
	}

	c.Conflict.Model = "pinned-model"
	if got := c.ConflictStep().Model; got != "pinned-model" {
		t.Errorf("model = %q, want the block's own pinned-model to win", got)
	}
}

func TestConflictResolutionReadsTheSessionMarker(t *testing.T) {
	cases := []struct {
		name    string
		outcome string
		kind    string
		detail  string
		ok      bool
	}{
		{
			name:    "mechanical",
			outcome: "RESOLUTION: mechanical\n\n**Tests**\n- go test ./...: pass",
			kind:    ConflictResolutionMechanical,
			ok:      true,
		},
		{
			name:    "behavioural with a note",
			outcome: "Some preamble.\n\nRESOLUTION: behavioural\n- a.txt: kept the new field and the old default",
			kind:    ConflictResolutionBehavioural,
			detail:  "a.txt: kept the new field and the old default",
			ok:      true,
		},
		{
			name:    "failed with a reason",
			outcome: "RESOLUTION: failed\n\n- b.txt cannot be resolved without guessing",
			kind:    ConflictResolutionFailed,
			detail:  "b.txt cannot be resolved without guessing",
			ok:      true,
		},
		{
			name:    "no marker",
			outcome: "I resolved the conflict.",
			ok:      false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, detail, ok := ConflictResolution(tc.outcome)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (outcome %q)", ok, tc.ok, tc.outcome)
			}
			if kind != tc.kind {
				t.Errorf("kind = %q, want %q", kind, tc.kind)
			}
			if detail != tc.detail {
				t.Errorf("detail = %q, want %q", detail, tc.detail)
			}
		})
	}
}

func TestValidateHoldsTheConflictStepToTheHarnessRules(t *testing.T) {
	t.Run("unknown harness", func(t *testing.T) {
		c := defaults()
		c.Conflict = &ConflictStep{Harness: "nope-not-a-harness"}
		err := c.Validate()
		if err == nil {
			t.Fatal("Validate() accepted an unregistered conflict harness")
		}
		if !strings.Contains(err.Error(), "conflict.harness") {
			t.Errorf("the refusal does not name the field: %v", err)
		}
	})

	t.Run("another harness with no block", func(t *testing.T) {
		c := defaults()
		c.Conflict = &ConflictStep{Harness: "codex"}
		err := c.Validate()
		if err == nil {
			t.Fatal("Validate() accepted a conflict harness with no harnesses block")
		}
		if !strings.Contains(err.Error(), "harnesses.codex") {
			t.Errorf("the refusal does not name the missing block: %v", err)
		}
	})

	t.Run("same harness as the run", func(t *testing.T) {
		c := defaults()
		c.Conflict = &ConflictStep{Harness: c.Harness, MaxTurns: 20}
		if err := c.Validate(); err != nil {
			t.Errorf("Validate() refused a conflict step on the run's own harness: %v", err)
		}
	})
}

func TestConflictHarnessIsSpawned(t *testing.T) {
	c := defaults()
	c.Harnesses = map[string]HarnessConfig{"codex": {Model: "codex-model"}}
	c.Conflict = &ConflictStep{Harness: "codex"}

	found := false
	for _, h := range c.SpawnedHarnesses() {
		if h == "codex" {
			found = true
		}
	}
	if !found {
		t.Errorf("SpawnedHarnesses() = %v, want the conflict step's codex (the supervisor really starts a session there)", c.SpawnedHarnesses())
	}
}
