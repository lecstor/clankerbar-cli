package config

import (
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/harness"
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
	// CLA-343, deliberately: the unphased phase used to carry MaxTurns 0 — the
	// zero value the turn-cap chain resolved to when nothing set one — and one
	// session ran 1093 turns / 285.9M tokens with no cap anywhere in the chain.
	// "0 = uncapped" is gone; the chain falls through to the built-in default.
	if got[0].MaxTurns != DefaultMaxTurns {
		t.Errorf("an unphased run resolved to a turn cap of %d, want the built-in default %d — a bare config must still bound its sessions", got[0].MaxTurns, DefaultMaxTurns)
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

// CLA-336: the post-fix pass is the expensive part paid twice. Phase 2's brief
// once ended at "re-verify", and the session read that as dispatching a second
// full review of the updated diff - eight minutes and most of a $12.64 phase,
// spent to check a handful of fixes the first pass had already localised. The
// brief must scope that pass to the fix commits and what they touch, name the
// findings being verified, and keep the full re-review as a stated exception.
func TestBuiltinReviewPhaseScopesThePostFixPassToTheFixes(t *testing.T) {
	brief := builtinPhasePrompts["review"]
	lower := strings.ToLower(brief)

	// The scope: named findings, the fix commits, and their regression surface.
	// Without the findings named, the re-reviewer reconstructs the list by
	// reading everything - the behaviour being removed.
	for _, want := range []string{"finding", "fix commit", "regression surface"} {
		if !strings.Contains(lower, want) {
			t.Errorf("the review brief never says %q; a post-fix pass it dispatches has no scope to hold to:\n%s", want, brief)
		}
	}

	// The escape hatch stays, and stays explicit: a full second pass is a
	// judgement the session states, not the default it falls into silently.
	for _, want := range []string{"exception", "default"} {
		if !strings.Contains(lower, want) {
			t.Errorf("the review brief never says %q; the full-pass escape hatch must be an explicit exception, not an unstated default:\n%s", want, brief)
		}
	}

	// And the wording being removed must not come back: a follow-up pointed at
	// the whole (updated) diff re-buys the full pass, however it is phrased.
	for _, banned := range []string{
		"review the updated diff", "over the updated diff", "review the whole diff",
		"re-review the diff", "re-review the whole", "read the diff again", "a fresh review",
	} {
		if strings.Contains(lower, banned) {
			t.Errorf("the review brief asks for %q - a whole-diff re-review, which is exactly the double-pay CLA-336 removed:\n%s", banned, brief)
		}
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

// Phases rest entirely on the adapter observing the session's claim. An adapter
// that does not would not FAIL — it would implement and push and then stop after
// phase 1 on every task, reported by a log line that reads like an ordinary early
// finish. Refusing at validation is the difference between a config error and a
// silent half-run every night.
func TestValidate_RefusesPhasesOnAHarnessThatCannotObserveAClaim(t *testing.T) {
	for _, name := range harness.Names() {
		caps, ok := harness.CapabilitiesOf(name)
		if !ok {
			t.Fatalf("registered harness %q has no capabilities", name)
		}
		c := defaults()
		c.Harness = name
		c.Prompt = ""
		c.Phases = []Phase{{Name: "implement"}, {Name: "review"}}

		err := c.Validate()
		if caps.TracksClaims {
			if err != nil {
				t.Errorf("Validate rejected phases on %q, which tracks claims: %v", name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("Validate accepted phases on %q, which does not observe a claim — every task would stop after phase 1", name)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the rejection for %q does not name the harness: %v", name, err)
		}
	}
}

// A single phase needs no hand-off, so it does not need claim tracking either —
// but it does need a brief that can finish a task. The phase carries its own
// prompt here for exactly that reason; see the test below.
func TestValidate_AllowsASinglePhaseOnAnyHarness(t *testing.T) {
	for _, name := range harness.Names() {
		c := defaults()
		c.Harness = name
		c.Prompt = ""
		c.Phases = []Phase{{Name: "solo", Prompt: "Work the next backlog item."}}

		if err := c.Validate(); err != nil {
			t.Errorf("Validate rejected a single self-contained phase on %q: %v — one phase hands off to nobody", name, err)
		}
	}
}

// `phases: [{"name":"implement"}]` validates as a shape and is a trap: the brief
// tells its session to stop at the checkpoint and leave in_review alone, and
// there is no successor to do the rest. Every task would stop half-implemented,
// every night, with nothing in the logs reading as an error.
func TestValidate_RefusesASequenceThatEndsOnTheImplementBrief(t *testing.T) {
	for _, phases := range [][]Phase{
		{{Name: implementPhaseName}},
		{{Name: reviewPhaseName, Prompt: "something"}, {Name: implementPhaseName}},
	} {
		c := defaults()
		c.Prompt = ""
		c.Phases = phases

		err := c.Validate()
		if err == nil {
			t.Errorf("Validate accepted a sequence ending on the %q brief (%d phases); nothing would ever hand a task to review", implementPhaseName, len(phases))
			continue
		}
		if !strings.Contains(err.Error(), reviewPhaseName) {
			t.Errorf("the rejection does not point at the fix: %v", err)
		}
	}
}

// The reverse: a resume brief cannot go FIRST, because there is no previous claim
// to fill its placeholders from — it would spend a session telling a model to
// heartbeat a literal "{{runId}}".
func TestValidate_RefusesAResumeBriefInFirstPosition(t *testing.T) {
	c := defaults()
	c.Prompt = ""
	c.Phases = []Phase{{Name: reviewPhaseName}, {Name: "closer", Prompt: "finish up"}}

	err := c.Validate()
	if err == nil {
		t.Fatal("Validate accepted a resume brief as phase 0, which has no previous claim to resume")
	}
	if !strings.Contains(err.Error(), PhaseRunPlaceholder) && !strings.Contains(err.Error(), PhaseTaskPlaceholder) {
		t.Errorf("the rejection does not name the placeholder that cannot be filled: %v", err)
	}
}

// The name becomes part of an iteration log's filename, and statedir refuses one
// it does not like — costing the phase the single artifact an operator has to
// debug the sequence with.
func TestValidate_RejectsAPhaseNameThatCannotBeAFilename(t *testing.T) {
	c := defaults()
	c.Prompt = ""
	c.Phases = []Phase{{Name: "impl/one", Prompt: "do the thing"}}

	err := c.Validate()
	if err == nil {
		t.Fatal("Validate accepted a phase name containing a path separator; its sessions would run with no iteration log at all")
	}
	if !strings.Contains(err.Error(), "impl/one") {
		t.Errorf("the rejection does not name the offending phase: %v", err)
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

// CLA-343: the turn-cap chain is phase → top-level → built-in default, and an
// unphased run — the default config — must never resolve to uncapped. This pins
// the two load-bearing readings: a bare config gets the default, and the
// operator's top-level cap overrides it.
func TestEffectivePhases_ResolvesTheTurnCapChain(t *testing.T) {
	t.Run("a bare unphased config resolves to the built-in default", func(t *testing.T) {
		c := defaults()
		c.Prompt = "work"
		got := c.EffectivePhases()[0]
		if got.MaxTurns != DefaultMaxTurns {
			t.Errorf("bare config resolved to MaxTurns %d, want the default %d", got.MaxTurns, DefaultMaxTurns)
		}
	})

	t.Run("the top-level cap overrides the default", func(t *testing.T) {
		c := defaults()
		c.Prompt = "work"
		c.MaxTurns = 50
		got := c.EffectivePhases()[0]
		if got.MaxTurns != 50 {
			t.Errorf("top-level MaxTurns 50 resolved to %d", got.MaxTurns)
		}
	})

	t.Run("a phase cap beats the top-level cap", func(t *testing.T) {
		c := defaults()
		c.Prompt = ""
		c.MaxTurns = 50
		c.Phases = []Phase{{Name: "implement", MaxTurns: 10}}
		got := c.EffectivePhases()[0]
		if got.MaxTurns != 10 {
			t.Errorf("phase MaxTurns 10 resolved to %d, want the phase's own cap to win", got.MaxTurns)
		}
	})

	t.Run("a phase without a cap inherits the top-level cap", func(t *testing.T) {
		c := defaults()
		c.Prompt = ""
		c.MaxTurns = 50
		c.Phases = []Phase{{Name: "implement"}, {Name: "review"}}
		got := c.EffectivePhases()
		for i, ph := range got {
			if ph.MaxTurns != 50 {
				t.Errorf("phase %d resolved to MaxTurns %d, want the top-level 50", i, ph.MaxTurns)
			}
		}
	})

	t.Run("resolution does not mutate the config it read", func(t *testing.T) {
		c := defaults()
		c.Prompt = "work"
		c.MaxTurns = 50
		c.EffectivePhases()
		if c.MaxTurns != 50 {
			t.Error("EffectivePhases wrote the resolved cap back into the config")
		}
	})
}

func TestValidate_RejectsANegativeTopLevelTurnCap(t *testing.T) {
	c := defaults()
	c.Prompt = "work"
	c.MaxTurns = -1

	if err := c.Validate(); err == nil {
		t.Fatal("Validate accepted a negative top-level max_turns")
	}
}

func TestValidate_RejectsANegativeSessionTokenCeiling(t *testing.T) {
	c := defaults()
	c.Prompt = "work"
	c.Budget = Budget{MaxSessionTokens: -1}

	if err := c.Validate(); err == nil {
		t.Fatal("Validate accepted a negative max_session_tokens")
	}
}

// CLA-343: the per-session token ceiling is the runaway detector, and it must
// exist even for a config that sets no budget dials at all — the whole defect
// was that nothing could stop the 285.9M session, so a ceiling that falls away
// by accident is the bug.
func TestBudgetSessionTokenCeiling(t *testing.T) {
	tests := []struct {
		name   string
		b      Budget
		want   int
		reason string
	}{
		{"the operator's own dial wins", Budget{MaxSessionTokens: 40_000_000}, 40_000_000, ""},
		{"2x max_tokens when no dial is set", Budget{MaxTokens: 75_000_000}, 150_000_000, ""},
		{"the documented floor with no budget at all", Budget{}, sessionTokenFloor, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.b.SessionTokenCeiling(); got != tc.want {
				t.Errorf("SessionTokenCeiling() = %d, want %d", got, tc.want)
			}
		})
	}
}

// CLA-352: the built-in briefs are where the pivot-trigger guidance lives — a
// session cannot measure its own context, so the trigger has to be event-shaped
// and it has to be TAUGHT, or the marker the driver watches for is never
// emitted. Pinned per brief so a rewrite cannot drop it from one silently.
func TestBuiltinPhaseBriefsCarryTheHandoffGuidance(t *testing.T) {
	for name, brief := range builtinPhasePrompts {
		if !strings.Contains(brief, HandoffMarker) {
			t.Errorf("the %q brief never names the handoff marker, so a session cannot use the mechanism", name)
		}
		if !strings.Contains(brief, "pivot") {
			t.Errorf("the %q brief lost the pivot trigger — without it the only rule left is a vibe about context size, which a session cannot measure", name)
		}
		if !strings.Contains(brief, "most tasks need zero") {
			t.Errorf("the %q brief must say most tasks need zero handoffs, or the exception becomes the habit", name)
		}
	}
}

// The review phase's terminal step is the one the phase exists to reach, and on
// 2026-08-19 three of four review phases in a single evening did all the work and
// then ended without it — the branch pushed, the task still `in_progress`, the
// next clanker paying to rediscover where the last one got to (CLA-384).
//
// The implement brief's equivalent step succeeded every time the same evening, and
// the difference between them was salience: implement names the call, its
// arguments and a stop condition in its own sentence; review said only "hand the
// task to in_review", a trailing clause at the end of a long paragraph about
// something else. So this pins the SHAPE that worked, not merely the topic.
//
// It pins wording, which is a weak test on its own — a brief can say all of this
// and still be ignored. Its partner is the driver-side tally in internal/loop,
// which counts the failures this brief is trying to prevent.
func TestReviewBriefStatesItsTerminalStep(t *testing.T) {
	brief, ok := builtinPhasePrompts[reviewPhaseName]
	if !ok {
		t.Fatalf("no built-in brief for phase %q", reviewPhaseName)
	}

	for _, want := range []string{
		// The call, by name, with the arguments the plane actually requires.
		"update_task(taskId, runId, status: \"in_review\", outcome: ...)",
		// The outcome requirement, which the brief omitted entirely before this —
		// a second way to end one call short, since the plane refuses an outcome
		// with no Tests section and a session can read that refusal as "done".
		"**Tests**",
		"REFUSES",
		// Ending while still holding the task is a failure, said as such.
		"FAILING, not finishing",
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("review brief does not state %q — the terminal step is what CLA-384 was about:\n%s", want, brief)
		}
	}

	// The step must not trail off the end of the re-verification sentence, which
	// is the shape that failed. Requiring it to open its own sentence is the
	// cheapest structural proxy available to a string test.
	if !strings.Contains(brief, "FINALLY, and this is the step that ENDS the phase:") {
		t.Errorf("the terminal step is not set apart as its own instruction:\n%s", brief)
	}

	// Presence is not the property that failed - POSITION is. The old brief
	// contained the instruction too; it was a trailing clause on a sentence about
	// something else. So pin the step to the LAST position before the shared
	// handoff guidance, which is exactly where the implement brief - the control in
	// this experiment, and the one that never failed - puts its own. Without this,
	// every assertion above still passes on a brief that buries the step in the
	// middle or softens it in a later sentence.
	if !strings.HasSuffix(brief, reviewTerminalStep+handoffGuidance) {
		t.Errorf("the terminal step is not the last thing the brief says before the handoff guidance:\n%s", brief)
	}
}
