package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// The budget check where per-phase harness selection (CLA-366) meets the two
// features that landed beside it: per-harness budget breakers (CLA-367) and the
// per-session wall-clock cap (CLA-368). Both wrote their inert-dial reasoning
// against `cfg.Harness`, which was the whole truth about what a run spawns until
// a phase could name its own harness.
//
// The failure mode being pinned is not a crash. It is doctor printing a
// confident, wrong sentence — "INERT: this config runs the claude harness" over
// a ceiling that will fire, or silence about a dial that will not — which is
// exactly the reassuring falsehood the budget check exists to remove. Nothing
// about these call sites conflicts on merge, so only a test holds them.

// The sharpest case: a per_harness block for the harness the IMPLEMENT phase
// runs on. It is reachable, it will fire, and calling it inert is a false
// statement that would have an operator delete a working ceiling.
func TestBudget_APerHarnessBlockForAPhasesHarnessIsNotInert(t *testing.T) {
	cfg := mixedDoctorCfg(t)
	cfg.Budget = config.Budget{PerHarness: map[string]config.HarnessBudget{
		"opencode": {MaxTokens: 5_000_000},
	}}

	c := checkBudget(cfg)
	if strings.Contains(c.detail, "INERT") {
		t.Errorf("opencode runs this config's implement phase, so its block fires — doctor called it inert:\n%s", c.detail)
	}
	if !strings.Contains(c.detail, "per_harness[opencode]") {
		t.Errorf("the block is not reported at all: %s", c.detail)
	}
	// And it counts as a LIVE ceiling: a run whose only ceiling is that block is
	// bounded, so the "no effective ceiling" warning must not fire. (The check can
	// still be WARN on the unrelated guard dials — max_retries and friends — which
	// is why this asserts on the finding rather than on the status.)
	if strings.Contains(c.detail, "effective ceiling") {
		t.Errorf("the run is bounded by a block that fires; doctor said it has no ceiling:\n%s", c.detail)
	}
}

// The complement, so the check is not just permissive: a block for a harness no
// phase runs is still inert, and the annotation names what the run DOES spawn
// rather than the run-wide harness alone.
func TestBudget_APerHarnessBlockForAnUnspawnedHarnessIsStillInert(t *testing.T) {
	cfg := mixedDoctorCfg(t)
	cfg.Budget = config.Budget{
		MaxTokens: 50_000_000,
		PerHarness: map[string]config.HarnessBudget{
			"codex": {MaxTokens: 5_000_000},
		},
	}

	c := checkBudget(cfg)
	if !strings.Contains(c.detail, "per_harness[codex]") || !strings.Contains(c.detail, "INERT") {
		t.Fatalf("a block for a harness no phase runs must be named as inert: %s", c.detail)
	}
	// Both spawned harnesses are listed. Naming only cfg.Harness would tell an
	// operator their config "runs claude" while an opencode phase is what the
	// block was probably meant for.
	for _, want := range []string{"claude", "opencode"} {
		if !strings.Contains(c.detail, want) {
			t.Errorf("the inert annotation does not say the config runs %s: %s", want, c.detail)
		}
	}
}

// CLA-368's inert wall-clock warning, judged per phase-harness. opencode enforces
// a session wall clock and claude does not, so on a mixed sequence the same dial
// is live on one phase and dead on the other — and doctor has to say which.
func TestBudget_InertSessionWallClockIsJudgedPerPhaseHarness(t *testing.T) {
	t.Run("a phase dial on the harness that DOES enforce it is not reported", func(t *testing.T) {
		cfg := mixedDoctorCfg(t)
		cfg.Phases[0].MaxWallClock = config.Duration(30 * time.Minute) // implement, on opencode
		cfg.Budget = config.Budget{MaxTokens: 50_000_000}

		for _, line := range checkBudget(cfg).info {
			if strings.Contains(line, "max_wall_clock") && strings.Contains(line, "INERT") {
				t.Errorf("opencode enforces a session wall clock, so this phase's dial fires; doctor called it inert: %s", line)
			}
		}
	})

	t.Run("a phase dial on the harness that does NOT is reported, and names that harness", func(t *testing.T) {
		cfg := mixedDoctorCfg(t)
		cfg.Phases[1].MaxWallClock = config.Duration(30 * time.Minute) // review, on claude
		cfg.Budget = config.Budget{MaxTokens: 50_000_000}

		found := ""
		for _, line := range checkBudget(cfg).info {
			if strings.Contains(line, "max_wall_clock") && strings.Contains(line, "INERT") {
				found = line
			}
		}
		if found == "" {
			t.Fatalf("claude enforces no session wall clock, so the review phase's dial is inert and must be named: %q", checkBudget(cfg).info)
		}
		if !strings.Contains(found, `"claude"`) {
			t.Errorf("the note must name the harness the dial is inert ON, which is the phase's: %s", found)
		}
	})

	t.Run("the run-wide dial is not inert while ANY spawned harness enforces it", func(t *testing.T) {
		cfg := mixedDoctorCfg(t)
		cfg.MaxSessionWallClock = config.Duration(30 * time.Minute)
		cfg.Budget = config.Budget{MaxTokens: 50_000_000}

		for _, line := range checkBudget(cfg).info {
			if strings.Contains(line, "max_session_wall_clock") && strings.Contains(line, "INERT") {
				t.Errorf("the run-wide dial reaches every phase and opencode enforces it, so it fires somewhere: %s", line)
			}
		}
	})

	t.Run("the run-wide dial IS inert when nothing spawned enforces it", func(t *testing.T) {
		// The pre-existing single-harness reading, unchanged: claude alone.
		cfg := validCfg(t)
		cfg.MaxSessionWallClock = config.Duration(30 * time.Minute)

		found := false
		for _, line := range checkBudget(cfg).info {
			if strings.Contains(line, "max_session_wall_clock") && strings.Contains(line, "INERT") {
				found = true
			}
		}
		if !found {
			t.Errorf("a claude-only run enforces no session wall clock, so the dial is inert: %q", checkBudget(cfg).info)
		}
	})
}

// The per-session runaway token ceiling used to be reported behind a literal
// `cfg.Harness == "claude"`. On a mixed sequence that either hides a guard that
// is live on the claude phase, or claims one for a phase where it cannot fire.
func TestBudget_SessionTokenCeilingIsReportedPerSpawnedHarness(t *testing.T) {
	t.Run("mixed: reported for the harness that has one, and only that one", func(t *testing.T) {
		cfg := mixedDoctorCfg(t) // no ceilings at all, so the "no ceiling configured" branch runs

		line := ""
		for _, l := range checkBudget(cfg).info {
			if strings.Contains(l, "max_session_tokens") {
				line = l
			}
		}
		if line == "" {
			t.Fatalf("the claude review phase still has a mid-session ceiling; doctor said nothing: %q", checkBudget(cfg).info)
		}
		if !strings.Contains(line, "claude") {
			t.Errorf("the line does not name claude, the harness whose ceiling it describes: %s", line)
		}
		// opencode's TokenCeilingHit never fires, so listing a number for it would
		// announce a guard that does not exist — the ReportsCost reasoning exactly.
		if strings.Contains(line, "opencode") {
			t.Errorf("opencode has no mid-session token ceiling, so it must be omitted rather than given a number: %s", line)
		}
	})

	t.Run("single claude: the original wording is unchanged", func(t *testing.T) {
		cfg := validCfg(t)

		found := false
		for _, l := range checkBudget(cfg).info {
			if strings.Contains(l, "max_session_tokens=") && strings.Contains(l, "is the mid-session bound") {
				found = true
			}
		}
		if !found {
			t.Errorf("an unphased claude run must report the single number exactly as before: %q", checkBudget(cfg).info)
		}
	})
}

// The built-in turn cap bounds the claude phase of a mixed run exactly as it
// bounds a claude-only one. Gating the note on cfg.Harness == "claude" goes
// silent on a sequence whose run-wide harness is something else.
func TestBudget_DefaultTurnCapNoteFiresWhenClaudeRunsAnyPhase(t *testing.T) {
	cfg := mixedDoctorCfg(t)
	cfg.Harness = "opencode"
	cfg.Phases = []config.Phase{{Name: "implement"}, {Name: "review", Harness: "claude"}}
	cfg.Harnesses = map[string]config.HarnessConfig{"claude": {ConfigDir: t.TempDir()}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("fixture does not validate: %v", err)
	}

	found := false
	for _, line := range checkBudget(cfg).info {
		if strings.Contains(line, "max_turns") && strings.Contains(line, "built-in default") {
			found = true
		}
	}
	if !found {
		t.Errorf("claude runs the review phase and is bounded by the default turn cap there; the run-wide harness being opencode does not make that untrue: %q",
			checkBudget(cfg).info)
	}
}
