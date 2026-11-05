package config

import (
	"fmt"
	"time"
)

// Budget is the "leave headroom / don't run away" circuit breaker. No harness
// exposes headless quota introspection (see the design memo), so this is a
// self-accounted, operator-tuned cap — simple and reliable over
// percentage-accurate, by design. Zero-valued fields are disabled.
type Budget struct {
	MaxTokens    int      `json:"max_tokens"`     // cumulative tokens across iterations
	MaxCostUSD   float64  `json:"max_cost_usd"`   // cumulative $ across iterations
	MaxWallClock Duration `json:"max_wall_clock"` // stop after this much elapsed

	// MaxSessionTokens kills a single SESSION in flight when its own cumulative
	// usage crosses this ceiling — the runaway detector, distinct from the
	// run-wide breaker above, which only checks BETWEEN sessions and so cannot
	// see a single huge session coming (CLA-343: the 285.9M runaway was 3.8x
	// its run's whole 75M ceiling). 0 = defer to SessionTokenCeiling's default.
	MaxSessionTokens int `json:"max_session_tokens"`

	// PerHarness are optional per-harness spend ceilings, keyed by harness name,
	// each counted over ONLY that harness's own sessions (CLA-367).
	//
	// The dials above are one ceiling over every session a run spends, which is
	// the right shape while a run drives one harness and the wrong one the moment
	// it drives two. `max_tokens` is calibrated against a subscription plan — 75M
	// is a sane week of Claude and roughly $2 on a DeepSeek-class backend, which
	// would end a drain after a task or two. `max_cost_usd` is the meaningful dial
	// for a metered backend and a meaningless one for a session billed to a
	// subscription, which reports a price nobody is charged (CLA-289). Neither
	// dial can hold a number that means the same thing on both sides, so a
	// mixed-harness run gets one block per harness, each measured in the unit that
	// harness understands.
	//
	// Any block that trips stops the WHOLE run. These are circuit breakers, not
	// per-harness quotas to be juggled: a run that carried on with one harness
	// switched off would be a shape nobody configured, and the phase that needed
	// it would fail every iteration.
	//
	// The dials above keep working exactly as they always have, over the run's
	// whole spend, and a config setting none of these behaves byte for byte as
	// before. Set both and both apply — the global ceiling over everything, the
	// per-harness one over its own sessions, whichever is reached first.
	//
	// Wall clock is deliberately absent: it measures the run, not a harness, and a
	// run has one clock however many harnesses it spends it on. So is
	// max_session_tokens, which is a runaway detector on a single session rather
	// than an accumulating ceiling.
	PerHarness map[string]HarnessBudget `json:"per_harness"`
}

// HarnessBudget is one harness's own spend ceiling inside a run's Budget — see
// Budget.PerHarness for why a mixed-harness run needs one per side. Zero-valued
// fields are disabled, as in Budget.
type HarnessBudget struct {
	MaxTokens  int     `json:"max_tokens"`   // cumulative tokens across THIS harness's sessions
	MaxCostUSD float64 `json:"max_cost_usd"` // cumulative $ across THIS harness's sessions
}

// CountsSpend reports whether this block bounds spend at all — the question
// Budget.CountsSpend answers, asked of one harness's block.
func (h HarnessBudget) CountsSpend() bool { return h.MaxTokens > 0 || h.MaxCostUSD > 0 }

// ExceededBy names the dimension of this block that tripped, or "" if none has.
//
// The harness name leads the string for the reason Budget.ExceededBy names the
// dimension at all: in a run carrying two ceilings, "cost $2.05 ≥ $2.00" does not
// say which side stopped it.
func (h HarnessBudget) ExceededBy(harness string, tokens int, costUSD float64) string {
	switch {
	case h.MaxTokens > 0 && tokens >= h.MaxTokens:
		return fmt.Sprintf("%s tokens %d ≥ %d", harness, tokens, h.MaxTokens)
	case h.MaxCostUSD > 0 && costUSD >= h.MaxCostUSD:
		return fmt.Sprintf("%s cost $%.2f ≥ $%.2f", harness, costUSD, h.MaxCostUSD)
	}
	return ""
}

// ForHarness is the block configured for a harness, and whether there is one. A
// missing block is the zero HarnessBudget, disabled in every dimension, so a
// caller that does not care which it got may use the value and drop the flag.
func (b Budget) ForHarness(name string) (HarnessBudget, bool) {
	hb, ok := b.PerHarness[name]
	return hb, ok
}

// ExceededByHarness names the per-harness dimension that tripped for one
// harness's own accumulated spend, or "" if none has. It consults ONLY that
// harness's block — the dials above are Budget.ExceededBy's business, and a
// caller enforcing both asks both.
func (b Budget) ExceededByHarness(name string, tokens int, costUSD float64) string {
	hb, ok := b.PerHarness[name]
	if !ok {
		return ""
	}
	return hb.ExceededBy(name, tokens, costUSD)
}

// CountsSpendFor reports whether a session run on this harness is under a spend
// ceiling — its own block's, or the run-wide one.
//
// This is the question CLA-262's side effects turn on (see CountsSpend): a
// session whose spend cannot be measured breaks a promise only where a promise
// was made. In a mixed-harness run the promise follows the harness whose breaker
// is set, so an unreadable opencode session stops a run carrying an opencode
// block while an unreadable claude session does not — unless a global dial covers
// them both, which is what every pre-CLA-367 config has.
func (b Budget) CountsSpendFor(name string) bool {
	if b.CountsSpend() {
		return true
	}
	hb, _ := b.ForHarness(name)
	return hb.CountsSpend()
}

// The per-session ceiling's defaults, in the order the resolution chain falls
// through (SessionTokenCeiling).
const (
	// sessionTokenCeilingMultiplier derives the ceiling from budget.max_tokens
	// when the operator set one. 2x is defensible because the runaway this
	// exists to stop ran 3.8x its run's ceiling: a detector at 2x catches that
	// class of runaway while sitting far above what a large task can plausibly
	// cost (~66M measured, CLA-309). It is a DETECTOR, deliberately not a
	// tighter budget.
	sessionTokenCeilingMultiplier = 2

	// sessionTokenFloor is the ceiling when the operator set no budget ceiling
	// at all. Anchors (CLA-343): the largest measured legitimate session was
	// 66.2M tokens (370 turns, CLA-309); the runaway was 285.9M. 150M is ~2.3x
	// the largest legitimate session and would have stopped the runaway at just
	// over half its spend.
	sessionTokenFloor = 150_000_000
)

// SessionTokenCeilingFor resolves the per-session runaway ceiling for a session
// on this harness: the operator's own dial, else 2x the run's max_tokens, else
// 2x that harness's own per_harness max_tokens, else the floor.
//
// The per-harness rung exists because CLA-367 tells an operator to move claude's
// token ceiling out of the global dial and into its own block — and without it,
// doing exactly that would silently LOOSEN the runaway detector the global dial
// was deriving: per_harness.claude.max_tokens=20M would give a 150M floor rather
// than the 40M the old shape gave, 7.5x the run's own ceiling. A detector that
// gets weaker when you follow the documented migration is the CLA-343 bug
// reappearing by another door.
//
// The global dial still wins over the per-harness one where both are set: it
// bounds the whole run, so it is the tighter promise about any one session.
func (b Budget) SessionTokenCeilingFor(harness string) int {
	if b.MaxSessionTokens > 0 {
		return b.MaxSessionTokens
	}
	if b.MaxTokens > 0 {
		return sessionTokenCeilingMultiplier * b.MaxTokens
	}
	if hb, ok := b.ForHarness(harness); ok && hb.MaxTokens > 0 {
		return sessionTokenCeilingMultiplier * hb.MaxTokens
	}
	return sessionTokenFloor
}

// SessionTokenCeiling resolves the per-session runaway ceiling: the operator's
// own dial, else 2x the run's max_tokens, else the floor. There is deliberately
// no "disabled": the whole point of CLA-343 is that nothing was able to stop the
// 285.9M session, so a ceiling that can be left unset by accident is the bug.
//
// This is the harness-blind form, kept for callers with no harness in hand; a
// caller that knows which harness the session runs on asks
// SessionTokenCeilingFor, which can also see a per-harness token ceiling.
func (b Budget) SessionTokenCeiling() int {
	if b.MaxSessionTokens > 0 {
		return b.MaxSessionTokens
	}
	if b.MaxTokens > 0 {
		return sessionTokenCeilingMultiplier * b.MaxTokens
	}
	return sessionTokenFloor
}

// CountsSpend reports whether this budget bounds SPEND — tokens or dollars — as
// opposed to only the wall clock.
//
// The distinction matters when a session's spend cannot be measured at all (a
// stream the supervisor could not read whole, CLA-262): a run under a spend
// ceiling has been promised something the driver can no longer deliver, while a
// run under a wall-clock ceiling alone still has its ceiling intact, because the
// clock does not depend on anything the child said.
func (b Budget) CountsSpend() bool { return b.MaxTokens > 0 || b.MaxCostUSD > 0 }

// Exceeded reports whether any enabled budget dimension has been reached.
func (b Budget) Exceeded(tokens int, costUSD float64, elapsed time.Duration) bool {
	return b.ExceededBy(tokens, costUSD, elapsed) != ""
}

// ExceededBy names the dimension that tripped, or "" if none has.
//
// Which dial stopped a run is the first thing an operator asks, and reporting
// all three figures side by side answers it wrongly: a run under a wall-clock
// ceiling alone still prints a token count and a dollar figure, which read as the
// cause. Naming the dimension — and the ceiling it crossed — is the difference
// between "why did it stop at $148" and "it ran for 10h23m against an 8h cap".
func (b Budget) ExceededBy(tokens int, costUSD float64, elapsed time.Duration) string {
	switch {
	case b.MaxTokens > 0 && tokens >= b.MaxTokens:
		return fmt.Sprintf("tokens %d ≥ %d", tokens, b.MaxTokens)
	case b.MaxCostUSD > 0 && costUSD >= b.MaxCostUSD:
		return fmt.Sprintf("cost $%.2f ≥ $%.2f", costUSD, b.MaxCostUSD)
	case b.MaxWallClock > 0 && elapsed >= b.MaxWallClock.Duration():
		return fmt.Sprintf("wall clock %s ≥ %s", elapsed.Round(time.Second), b.MaxWallClock.Duration())
	}
	return ""
}

// Deadline is when the wall-clock ceiling will be reached for a run that began at
// start, or the zero time if no wall-clock ceiling is set.
func (b Budget) Deadline(start time.Time) time.Time {
	if b.MaxWallClock <= 0 {
		return time.Time{}
	}
	return start.Add(b.MaxWallClock.Duration())
}

// Remaining is how much of the wall-clock ceiling is left after elapsed, and
// whether a ceiling is set at all.
//
// Callers must pass the SAME elapsed the breaker is given (ExceededBy), so a
// decision taken mid-drain cannot disagree with the breaker's own verdict
// between drains. Deriving a wall-clock deadline is what let them disagree:
// Deadline keeps start's monotonic reading while ExceededBy counts monotonic
// elapsed, and a suspended machine advances the one and freezes the other.
func (b Budget) Remaining(elapsed time.Duration) (time.Duration, bool) {
	if b.MaxWallClock <= 0 {
		return 0, false
	}
	return b.MaxWallClock.Duration() - elapsed, true
}

// resolveMaxTurns is the turn-cap chain: the phase's own cap wins, then the
// top-level cap, then the built-in default. Nothing resolves to zero — see
// Phase.MaxTurns and DefaultMaxTurns for why "0 = uncapped" is gone.
func (c *Config) resolveMaxTurns(phase int) int {
	if phase > 0 {
		return phase
	}
	if c.MaxTurns > 0 {
		return c.MaxTurns
	}
	return DefaultMaxTurns
}

// resolveWallClock is the session wall-clock chain: the phase's own cap wins,
// then the run-wide one, then nothing. Unlike resolveMaxTurns this CAN resolve
// to zero, and zero means off — the dial ships disabled because no default
// number would be honest across harnesses (see Phase.MaxWallClock).
func (c *Config) resolveWallClock(phase Duration) Duration {
	if phase > 0 {
		return phase
	}
	return c.MaxSessionWallClock
}

// ZeroSpendAttemptBound is the effective bound on consecutive no-usage attempts:
// the operator's own, else the built-in default. Like the turn cap, nothing
// resolves to zero - the bound is a backstop, and "off" is not a value it takes.
func (c *Config) ZeroSpendAttemptBound() int {
	if c.MaxZeroSpendAttempts > 0 {
		return c.MaxZeroSpendAttempts
	}
	return DefaultMaxZeroSpendAttempts
}

// RepeatedToolFailureBound is the effective repeated-failing-tool-call
// threshold: the operator's own, else the built-in default. Unlike
// ZeroSpendAttemptBound this one resolves to zero when the operator set zero,
// which is how the check is turned off.
func (c *Config) RepeatedToolFailureBound() int {
	if c.MaxRepeatedToolFailures != nil {
		return *c.MaxRepeatedToolFailures
	}
	return DefaultMaxRepeatedToolFailures
}
