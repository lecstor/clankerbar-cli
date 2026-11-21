package cli

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// --- 12. budget --------------------------------------------------------------

func checkBudget(cfg *config.Config) check {
	c := check{name: "budget"}
	b := cfg.Budget

	// A negative ceiling is never what anyone meant, and Budget.Exceeded ignores it (its guards are
	// all `> 0`) — so it reads as "set" while doing nothing at all.
	var bad []string
	if b.MaxTokens < 0 {
		bad = append(bad, "max_tokens")
	}
	if b.MaxCostUSD < 0 {
		bad = append(bad, "max_cost_usd")
	}
	if b.MaxWallClock < 0 {
		bad = append(bad, "max_wall_clock")
	}
	for _, name := range slices.Sorted(maps.Keys(b.PerHarness)) {
		hb := b.PerHarness[name]
		if hb.MaxTokens < 0 {
			bad = append(bad, fmt.Sprintf("per_harness[%s].max_tokens", name))
		}
		if hb.MaxCostUSD < 0 {
			bad = append(bad, fmt.Sprintf("per_harness[%s].max_cost_usd", name))
		}
	}
	if len(bad) > 0 {
		c.status = fail
		c.detail = "negative budget " + plural(len(bad), "value", "values") + ": " + strings.Join(bad, ", ")
		c.remedy = "use a positive ceiling; 0 disables that dimension"
		return c
	}

	// A cost ceiling on a harness that never reports cost is not a weak ceiling but an ABSENT one: no
	// code path can reach it, because nothing populates the figure it compares against (codex exec
	// reports tokens, not money).
	costBlind := costBlindHarnesses(cfg)
	inertCost := b.MaxCostUSD > 0 && len(costBlind) > 0
	costLive := b.MaxCostUSD > 0 && !inertCost

	var set []string
	if b.MaxTokens > 0 {
		set = append(set, fmt.Sprintf("max_tokens=%d", b.MaxTokens))
	}
	if b.MaxCostUSD > 0 {
		entry := fmt.Sprintf("max_cost_usd=%g", b.MaxCostUSD)
		if inertCost {
			// Annotated wherever it appears, not only when it is the last ceiling standing: a dial that does
			// nothing is worth saying out loud even when something else is holding the line.
			entry += fmt.Sprintf(" (INERT: the %s harness never reports cost)", strings.Join(costBlind, "/"))
		}
		set = append(set, entry)
	}
	if b.MaxWallClock > 0 {
		set = append(set, "max_wall_clock="+b.MaxWallClock.Duration().String())
	}

	// Per-harness blocks, read the same way: what can FIRE, not what is written down.
	spawned := cfg.SpawnedHarnesses()
	// The spawned harness whose block sets a cost dial and NOTHING else - under a harness that never
	// reports cost that block is a ceiling in name only, and the remedy has to name the unit rather
	// than the placement.
	costOnlyInertHarness := ""
	spawnedBlockTokens := false
	for _, name := range spawned {
		hb, _ := b.ForHarness(name)
		if hb.MaxTokens > 0 {
			spawnedBlockTokens = true
		}
		if costOnlyInertHarness == "" && hb.MaxCostUSD > 0 && hb.MaxTokens == 0 && !harnessReportsCost(name) {
			costOnlyInertHarness = name
		}
	}
	perHarnessLive := false
	for _, name := range slices.Sorted(maps.Keys(b.PerHarness)) {
		hb := b.PerHarness[name]
		if !hb.CountsSpend() {
			set = append(set, fmt.Sprintf("per_harness[%s]: no ceiling set (INERT)", name))
			continue
		}
		var dials []string
		tokensLive := hb.MaxTokens > 0
		costFires := hb.MaxCostUSD > 0 && harnessReportsCost(name)
		if hb.MaxTokens > 0 {
			dials = append(dials, fmt.Sprintf("max_tokens=%d", hb.MaxTokens))
		}
		if hb.MaxCostUSD > 0 {
			dial := fmt.Sprintf("max_cost_usd=%g", hb.MaxCostUSD)
			if !costFires {
				dial += " (INERT: never reports cost)"
			}
			dials = append(dials, dial)
		}
		entry := fmt.Sprintf("per_harness[%s]: %s", name, strings.Join(dials, ", "))
		runs := runsHarness(cfg, name)
		if !runs {
			entry += fmt.Sprintf(" (INERT: this config runs %s)", strings.Join(spawned, " / "))
		}
		set = append(set, entry)
		if runs && (tokensLive || costFires) {
			perHarnessLive = true
		}
	}
	// Every ceiling this run has that can actually fire. The branches below ask this rather than "is
	// anything configured", because a config whose only ceilings are inert has none at all - the
	// CLA-288 reading, now including the two ways a per-harness block can be inert.
	anyLive := b.MaxTokens > 0 || costLive || b.MaxWallClock > 0 || perHarnessLive

	// The guards that were silently absent from the config that ran a 285.9M single session: doctor's
	// job is preflight, so the dials most likely to be missing are the ones it should name.
	var guardNotes []string
	var guardDials []string
	// Turn cap: since CLA-343 the effective CLAUDE config ALWAYS resolves one — the operator's, else
	// the built-in default — so "no turn cap" cannot happen; what can happen is that the cap is the
	// DEFAULT, a runaway detector tuned to the largest measured session, not a budget the operator
	// chose.
	if runsHarness(cfg, "claude") && anyPhaseRunsTheDefaultTurnCap(cfg) {
		guardNotes = append(guardNotes, fmt.Sprintf(
			"max_turns: at the built-in default (%d turns): a runaway detector, not a budget, so a deep task that reaches it is cut off at the phase boundary (the salvage commits what it left); set max_turns (or a phase's) to tune",
			config.DefaultMaxTurns))
		guardDials = append(guardDials, "max_turns")
	}
	// A session wall-clock cap the configured harness never enforces is the same defect
	// max_cost_usd-under-codex is: a dial the operator set as their backstop, doing nothing,
	// discoverable only from a session that ran all night.
	if dial, on := inertSessionWallClock(cfg); dial != "" {
		// The advice rides in the NOTE, not in c.remedy: the remedy line is one per check and the guard
		// block's generic "set <dials>" usually claims it, so advice parked there is advice an operator
		// routinely never sees.
		backstop := "under " + on + " the turn cap is the backstop that does fire"
		if !harnessHonoursMaxTurns(on) {
			backstop = "under " + on + " there is NO per-session backstop at all - bound the run with max_iterations / max_tokens instead"
		}
		guardNotes = append(guardNotes, fmt.Sprintf(
			"%s: set, but harness %q does not enforce a per-session wall-clock cap, so it is INERT there - drop it or run the harness that enforces it (opencode); %s",
			dial, on, backstop))
	}
	if on := noSessionBackstop(cfg); on != "" {
		guardNotes = append(guardNotes, fmt.Sprintf(
			"max_session_wall_clock: unset, and harness %q has no turn cap and no mid-session token ceiling - so nothing bounds a single session there; a run that works past its brief is stopped only by a run-level ceiling",
			on))
		guardDials = append(guardDials, "max_session_wall_clock")
	}
	if cfg.MaxRetries == 0 {
		guardNotes = append(guardNotes,
			fmt.Sprintf("max_retries: 0 - transient failures are retried forever (backoff capped at retry_cap), each retry a fresh paid session redoing the task; set a positive max_retries to bound a run window. Only a run of attempts reporting NO usage is bounded regardless, at max_zero_spend_attempts=%d", cfg.ZeroSpendAttemptBound()))
		guardDials = append(guardDials, "max_retries")
	}
	if cfg.MaxIterations == 0 {
		guardNotes = append(guardNotes,
			"max_iterations: 0 — no session cap: the loop stops only on a STOP/HALT marker, a signal or a budget ceiling (a dry backlog idle-polls rather than exiting); set max_iterations to bound a run window")
		guardDials = append(guardDials, "max_iterations")
	}
	if runsHarness(cfg, "copilot") {
		if cfg.MaxRepeatedToolFailures != nil && *cfg.MaxRepeatedToolFailures > 0 {
			guardNotes = append(guardNotes, fmt.Sprintf(
				"max_repeated_tool_failures: %d is INERT under copilot - its adapter does not arm the tool-loop guard", *cfg.MaxRepeatedToolFailures))
		}
		if b, ok := cfg.SessionIdle["copilot"]; ok && ((b.MaxIdle != nil && b.MaxIdle.Duration() > 0) || (b.MaxToolCall != nil && b.MaxToolCall.Duration() > 0)) {
			guardNotes = append(guardNotes, "session_idle[copilot] is INERT - copilot's adapter arms no idle watchdog")
		}
	}

	c.status = pass
	if len(set) == 0 {
		// The wording names what actually STOPS the loop, because the obvious sentence — "runs until
		// the backlog is dry" — is false: a dry backlog idle-polls by design, so the daemon can react
		// to answered questions, promotions and newly filed work.
		c.detail = "no ceiling configured — the loop stops on a STOP/HALT marker or signal; a dry backlog idle-polls rather than exiting"
		// The per-session runaway ceiling still applies whatever the run-level dials say, and the
		// operator should know the number.
		if bounds := sessionTokenBounds(cfg); bounds != "" {
			c.info = append(c.info, "per-session runaway ceiling still active: "+bounds+" (the operator's own, else 2x a token ceiling when set, else the floor)")
		}
	} else if b.MaxWallClock > 0 && !costLive && b.MaxTokens == 0 && !perHarnessLive {
		// Wall clock is the weakest proxy for spend of the three, because it counts the hours a run
		// spends WAITING OUT a usage limit — time in which nothing is billed.
		c.status = warn
		c.detail = strings.Join(set, ", ") + " - wall clock is the only ceiling that can fire, and it counts time spent waiting out usage limits"
		// The usual advice is "add max_cost_usd" - which under a harness that cannot report cost is
		// advice to set a dial that does nothing, and it would be FOLLOWED, landing the operator in the
		// cost-only case the sibling branch below warns about.
		if len(costBlind) > 0 {
			c.remedy = "add max_tokens as the real ceiling; under " + strings.Join(costBlind, "/") + " max_cost_usd cannot fire, and max_wall_clock is only the outer bound on how late a run may finish"
		} else {
			c.remedy = "add max_cost_usd as the real ceiling; keep max_wall_clock as the outer bound on how late a run may finish"
		}
	} else if inertCost && b.MaxTokens == 0 && b.MaxWallClock == 0 && !perHarnessLive {
		// The sibling of the wall-clock-only warning, and the sharper case of the two: wall clock at
		// least measures something, whereas this run's every configured ceiling is unreachable, so it has
		// none at all.
		c.status = warn
		c.detail = strings.Join(set, ", ") + " - so this run has NO effective ceiling"
		// costBlind, not cfg.Harness: on a mixed sequence it is every harness the run spawns that cannot
		// report cost which makes the global dial inert, so naming the run-wide harness alone would point
		// the operator at the wrong one.
		c.remedy = "set max_tokens (or max_wall_clock) beside it - under " + strings.Join(costBlind, "/") + " those are the dials that can fire"
	} else if !anyLive {
		// The same finding reached the other way: every ceiling written down is a per-harness block this
		// run cannot reach (a harness it never drives, or a cost dial that harness never reports), so the
		// config LOOKS bounded and is not.
		c.status = warn
		c.detail = strings.Join(set, ", ") + " - none of them can fire, so this run has NO effective ceiling"
		// A remedy will be FOLLOWED, so it must not point at something the operator has already done.
		if costOnlyInertHarness != "" {
			c.remedy = "set per_harness[" + costOnlyInertHarness + "].max_tokens - " + costOnlyInertHarness + " reports tokens, never cost"
		} else {
			c.remedy = "put the ceiling where the run will reach it: a per_harness block for " + strings.Join(spawned, " or ") + ", or the run-wide max_tokens / max_wall_clock"
		}
	} else {
		c.detail = strings.Join(set, ", ")
		// The run-wide breaker is checked BETWEEN sessions: it cannot see a single huge session coming,
		// which is exactly what happened to the 285.9M run under max_tokens=75M.
		if b.MaxTokens > 0 || spawnedBlockTokens {
			c.detail += " — the token ceiling is enforced BETWEEN sessions, so one session can overrun it"
			// sessionTokenBounds is EMPTY when no spawned harness has a mid-session ceiling, which is every
			// opencode-only and codex-only run.
			if bounds := sessionTokenBounds(cfg); bounds != "" {
				c.detail += " (" + bounds + ")"
			} else {
				// The singular verb is safe because this branch implies exactly ONE spawned harness today:
				// claude is the only harness with a mid-session ceiling, so reaching here means claude is not
				// among the spawned.
				c.detail += " by any amount — " + strings.Join(spawned, "/") + " enforces no mid-session token ceiling"
			}
		}
	}

	// A missing guard turns the verdict WARN (a FAIL above stays a FAIL — a broken budget is worse
	// than a bare one).
	if len(guardNotes) > 0 {
		if c.status == pass {
			c.status = warn
		}
		if c.remedy == "" && len(guardDials) > 0 {
			// Named from the dials actually warned on, so a codex run (no max_turns note) is not told to set
			// a dial that does nothing there.
			c.remedy = "set " + strings.Join(guardDials, ", ") + " (see the guard lines), or accept the defaults"
		}
		c.info = append(c.info, guardNotes...)
	}
	return c
}

// harnessReportsCost asks the registry whether this harness ever populates Result.CostUSD. An
// UNKNOWN harness is treated as reporting cost - i.e. the warning is withheld - because
// config.Validate has already refused that name, so the only way to get here is a doctor run over a
// config too broken to run at all, and warning about an inert dial on a harness that does not exist
// would bury the real finding under a speculative one.
func harnessReportsCost(name string) bool {
	caps, ok := harness.CapabilitiesOf(name)
	if !ok {
		return true
	}
	return caps.ReportsCost
}

// inertSessionWallClock names the wall-clock dial this config sets that will not be enforced, and
// the harness it is inert ON - two empty strings when there is nothing to say. Judged PER
// PHASE-HARNESS since CLA-366, because that is the level at which the question has an answer.
func inertSessionWallClock(cfg *config.Config) (dial, on string) {
	// Phase dials first, so the operator is pointed at the line they wrote - and read off cfg.Phases,
	// NOT EffectivePhases, for exactly that reason: the effective phases have the run-wide dial
	// resolved into every one of them, so iterating those would report an operator's
	// `max_session_wall_clock` back to them as `max_wall_clock (phases)`, a line they never wrote.
	for _, ph := range cfg.Phases {
		if ph.MaxWallClock == 0 {
			continue
		}
		h := cfg.HarnessFor(ph)
		caps, ok := harness.CapabilitiesOf(h)
		if ok && !caps.HonoursSessionWallClock {
			return "max_wall_clock (phases)", h
		}
	}
	if cfg.MaxSessionWallClock == 0 {
		return "", ""
	}
	// The run-wide dial: inert only if NOTHING this run spawns enforces it.
	named := ""
	for _, h := range cfg.SpawnedHarnesses() {
		caps, ok := harness.CapabilitiesOf(h)
		if !ok {
			return "", ""
		}
		if caps.HonoursSessionWallClock {
			return "", ""
		}
		if named == "" {
			named = h
		}
	}
	if named == "" {
		return "", ""
	}
	return "max_session_wall_clock", named
}

// harnessHonoursMaxTurns asks the registry whether this harness takes a turn flag at all. An
// UNKNOWN harness is treated as honouring one — the withheld advice, for the reason
// harnessReportsCost gives.
func harnessHonoursMaxTurns(name string) bool {
	caps, ok := harness.CapabilitiesOf(name)
	if !ok {
		return true
	}
	return caps.HonoursMaxTurns
}

// noSessionBackstop names a harness whose sessions in THIS run are bounded by NOTHING - it takes no
// turn flag and kills on no token ceiling, and the phases it runs leave unset the one dial that
// could bound it. Per phase-harness since CLA-366, and per phase within it: an unbounded opencode
// implement phase is exactly as unbounded when a claude review phase runs after it, so asking only
// about cfg.Harness would go silent on the very sequence this warning is for.
func noSessionBackstop(cfg *config.Config) string {
	for _, ph := range cfg.EffectivePhases() {
		if ph.MaxWallClock > 0 {
			continue
		}
		caps, ok := harness.CapabilitiesOf(ph.Harness)
		if !ok || caps.HonoursMaxTurns || !caps.HonoursSessionWallClock {
			continue
		}
		return ph.Harness
	}
	return ""
}

// sessionTokenBounds renders the per-session runaway token ceiling for every harness this run
// spawns that actually HAS one, or "" when none does. Per harness for two reasons since CLA-366.
// The ceiling itself is resolved per harness, so one number cannot describe a mixed run; and
// whether the ceiling exists at all is a capability - codex has no mid-session ceiling, so listing
// it with a number would claim a guard that never fires.
func sessionTokenBounds(cfg *config.Config) string {
	var parts, only []string
	for _, h := range cfg.SpawnedHarnesses() {
		caps, ok := harness.CapabilitiesOf(h)
		if !ok || !caps.HasSessionTokenCeiling {
			continue
		}
		only = append(only, h)
		parts = append(parts, fmt.Sprintf("%s=%d", h, cfg.Budget.SessionTokenCeilingFor(h)))
	}
	if len(parts) == 0 {
		return ""
	}
	// Gated on the run spawning ONE harness, not on one harness having a ceiling.
	if len(cfg.SpawnedHarnesses()) == 1 {
		return fmt.Sprintf("max_session_tokens=%d is the mid-session bound", cfg.Budget.SessionTokenCeilingFor(only[0]))
	}
	return "max_session_tokens is the mid-session bound, per harness: " + strings.Join(parts, ", ")
}

// anyPhaseRunsTheDefaultTurnCap reports whether the effective config bounds any session by the
// built-in default rather than an operator-chosen cap — i.e. whether CLA-343's default is
// load-bearing for this run. The effective phases resolve phase → top-level → default, so a
// phase carrying DefaultMaxTurns is exactly a phase the operator never capped (an operator who set
// the same number explicitly is indistinguishable, and the statement "the default applies" is still
// true for them).
func anyPhaseRunsTheDefaultTurnCap(cfg *config.Config) bool {
	for _, ph := range cfg.EffectivePhases() {
		if ph.MaxTurns == config.DefaultMaxTurns {
			return true
		}
	}
	return false
}
