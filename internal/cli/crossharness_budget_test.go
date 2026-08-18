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

// The between-sessions clause used to append sessionTokenBounds unguarded. That
// function is empty for every harness with no mid-session ceiling - which is every
// harness except claude - so an unphased opencode or codex run with max_tokens set
// printed a bare "()": a broken sentence in the surface whose entire job is not
// making sloppy or false statements, and a straight regression against staging.
func TestBudget_NoEmptyParentheticalWhenTheHarnessHasNoMidSessionCeiling(t *testing.T) {
	for _, harnessName := range []string{"claude", "opencode", "codex"} {
		t.Run(harnessName, func(t *testing.T) {
			cfg := validCfg(t)
			cfg.Harness = harnessName
			cfg.Budget = config.Budget{MaxTokens: 1_000_000}

			d := checkBudget(cfg).detail
			if strings.Contains(d, "()") {
				t.Errorf("empty parenthetical in the budget detail: %s", d)
			}
			if !strings.Contains(d, "enforced BETWEEN sessions") {
				t.Fatalf("the between-sessions warning is missing entirely: %s", d)
			}
			// And the sentence has to be TRUE either way: claude names its bound,
			// the others say plainly that there is none rather than implying one.
			if harnessName == "claude" {
				if !strings.Contains(d, "max_session_tokens=") {
					t.Errorf("claude has a mid-session bound and must name the number: %s", d)
				}
			} else {
				if strings.Contains(d, "max_session_tokens=") {
					t.Errorf("%s has no mid-session ceiling, so quoting a number claims a guard that cannot fire: %s", harnessName, d)
				}
				if !strings.Contains(d, "no mid-session token ceiling") {
					t.Errorf("%s: the absence of a bound is the sharper finding and must be stated: %s", harnessName, d)
				}
			}
		})
	}
}

// A config whose phases ALL override the harness declares a run-wide `harness`
// that never starts a session. Validate does not refuse that shape, and both
// spawn sites in the driver are phase-driven, so a budget check reasoning from
// the DECLARED harness makes three separate false statements about it.
//
// This is why doctor's budget check asks SpawnedHarnesses and not PhaseHarnesses:
// the latter seeds the declared harness unconditionally, which is right for
// "is every configured harness usable" and wrong for "what will this run do".
func TestBudget_ADeclaredHarnessNoPhaseRunsIsNotTreatedAsSpawned(t *testing.T) {
	// Every phase on claude; the run-wide harness is one that never spawns.
	unspawned := func(t *testing.T, runHarness string) *config.Config {
		t.Helper()
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		cfg := &config.Config{
			Harness: runHarness,
			WorkDir: t.TempDir(),
			Phases: []config.Phase{
				{Name: "implement", Harness: "claude"},
				{Name: "review", Harness: "claude"},
			},
			Harnesses: map[string]config.HarnessConfig{"claude": {ConfigDir: t.TempDir()}},
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("fixture does not validate: %v", err)
		}
		return cfg
	}

	t.Run("a live cost ceiling is not called INERT by a cost-blind harness that never runs", func(t *testing.T) {
		cfg := unspawned(t, "codex")
		cfg.Budget = config.Budget{MaxCostUSD: 5}

		c := checkBudget(cfg)
		if strings.Contains(c.detail, "INERT") || strings.Contains(c.detail, "NO effective ceiling") {
			t.Errorf("every session runs on claude, which reports cost, so max_cost_usd fires:\n%s -> %s", c.detail, c.remedy)
		}
		if strings.Contains(c.remedy, "codex") {
			t.Errorf("the remedy points at a harness no session runs on: %s", c.remedy)
		}
	})

	t.Run("an unreachable per_harness block for it is still INERT", func(t *testing.T) {
		cfg := unspawned(t, "codex")
		cfg.Budget = config.Budget{PerHarness: map[string]config.HarnessBudget{
			"codex": {MaxTokens: 100_000},
		}}

		c := checkBudget(cfg)
		if !strings.Contains(c.detail, "INERT") {
			t.Errorf("no phase runs codex, so its block can never fire and must be named inert:\n%s", c.detail)
		}
		// And it must not count as the run's live ceiling, which is the inverse
		// error: the run would then look bounded while having no ceiling at all.
		if !strings.Contains(c.detail, "effective ceiling") {
			t.Errorf("the only ceiling is unreachable, so the run has none — doctor did not say so:\n%s -> %s", c.detail, c.remedy)
		}
	})

	t.Run("an inert wall-clock dial is not excused by a harness that never runs", func(t *testing.T) {
		// opencode honours a session wall clock and claude does not. Declaring
		// opencode and running every phase on claude leaves the dial dead on every
		// session the run actually spawns.
		cfg := unspawned(t, "opencode")
		cfg.MaxSessionWallClock = config.Duration(30 * time.Minute)
		cfg.Budget = config.Budget{MaxTokens: 50_000_000}

		found := false
		for _, line := range checkBudget(cfg).info {
			if strings.Contains(line, "max_session_wall_clock") && strings.Contains(line, "INERT") {
				found = true
			}
		}
		if !found {
			t.Errorf("no phase runs opencode, so nothing enforces the dial and it must be reported inert: %q",
				checkBudget(cfg).info)
		}
	})
}
