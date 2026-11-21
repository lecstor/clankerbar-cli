package config

import (
	"fmt"
	"time"
)

// Budget is the "leave headroom / don't run away" circuit breaker.
type Budget struct {
	MaxTokens    int      `json:"max_tokens"`     // cumulative tokens across iterations
	MaxCostUSD   float64  `json:"max_cost_usd"`   // cumulative $ across iterations
	MaxWallClock Duration `json:"max_wall_clock"` // stop after this much elapsed

	// MaxSessionTokens kills a single SESSION in flight when its own cumulative usage crosses this
	// ceiling — the runaway detector, distinct from the run-wide breaker above, which only checks
	// BETWEEN sessions and so cannot see a single huge session coming.
	MaxSessionTokens int `json:"max_session_tokens"`

	// PerHarness are optional per-harness spend ceilings, keyed by harness name, each counted over
	// ONLY that harness's own sessions. Wall clock is deliberately absent: it measures the run, not a
	// harness, and a run has one clock however many harnesses it spends it on.
	PerHarness map[string]HarnessBudget `json:"per_harness"`
}

type HarnessBudget struct {
	MaxTokens  int     `json:"max_tokens"`   // cumulative tokens across THIS harness's sessions
	MaxCostUSD float64 `json:"max_cost_usd"` // cumulative $ across THIS harness's sessions
}

func (h HarnessBudget) CountsSpend() bool { return h.MaxTokens > 0 || h.MaxCostUSD > 0 }

// ExceededBy names the dimension of this block that tripped, or "" if none has.
func (h HarnessBudget) ExceededBy(harness string, tokens int, costUSD float64) string {
	switch {
	case h.MaxTokens > 0 && tokens >= h.MaxTokens:
		return fmt.Sprintf("%s tokens %d ≥ %d", harness, tokens, h.MaxTokens)
	case h.MaxCostUSD > 0 && costUSD >= h.MaxCostUSD:
		return fmt.Sprintf("%s cost $%.2f ≥ $%.2f", harness, costUSD, h.MaxCostUSD)
	}
	return ""
}

// ForHarness is the block configured for a harness, and whether there is one.
func (b Budget) ForHarness(name string) (HarnessBudget, bool) {
	hb, ok := b.PerHarness[name]
	return hb, ok
}

func (b Budget) ExceededByHarness(name string, tokens int, costUSD float64) string {
	hb, ok := b.PerHarness[name]
	if !ok {
		return ""
	}
	return hb.ExceededBy(name, tokens, costUSD)
}

// CountsSpendFor reports whether a session run on this harness is under a spend ceiling — its own
// block's, or the run-wide one.
func (b Budget) CountsSpendFor(name string) bool {
	if b.CountsSpend() {
		return true
	}
	hb, _ := b.ForHarness(name)
	return hb.CountsSpend()
}

const (
	// sessionTokenCeilingMultiplier derives the ceiling from budget.max_tokens when the operator set
	// one. 2x is defensible because the runaway this exists to stop ran 3.8x its run's ceiling: a
	// detector at 2x catches that class of runaway while sitting far above what a large task can
	// plausibly cost.
	sessionTokenCeilingMultiplier = 2

	sessionTokenFloor = 150_000_000
)

// SessionTokenCeilingFor resolves the per-session runaway ceiling for a session on this harness:
// the operator's own dial, else 2x the run's max_tokens, else 2x that harness's own per_harness
// max_tokens, else the floor. The per-harness rung exists because CLA-367 tells an operator to move
// claude's token ceiling out of the global dial and into its own block — and without it, doing
// exactly that would silently LOOSEN the runaway detector the global dial was deriving:
// per_harness.claude.max_tokens=20M would give a 150M floor rather than the 40M the old shape gave,
// 7.5x the run's own ceiling.
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

// SessionTokenCeiling resolves the per-session runaway ceiling: the operator's own dial, else 2x
// the run's max_tokens, else the floor. There is deliberately no "disabled": the whole point of
// CLA-343 is that nothing was able to stop the 285.9M session, so a ceiling that can be left unset
// by accident is the bug.
func (b Budget) SessionTokenCeiling() int {
	if b.MaxSessionTokens > 0 {
		return b.MaxSessionTokens
	}
	if b.MaxTokens > 0 {
		return sessionTokenCeilingMultiplier * b.MaxTokens
	}
	return sessionTokenFloor
}

// CountsSpend reports whether this budget bounds SPEND — tokens or dollars — as opposed to only
// the wall clock. The distinction matters when a session's spend cannot be measured at all: a run
// under a spend ceiling has been promised something the driver can no longer deliver, while a run
// under a wall-clock ceiling alone still has its ceiling intact, because the clock does not depend
// on anything the child said.
func (b Budget) CountsSpend() bool { return b.MaxTokens > 0 || b.MaxCostUSD > 0 }

// Exceeded reports whether any enabled budget dimension has been reached.
func (b Budget) Exceeded(tokens int, costUSD float64, elapsed time.Duration) bool {
	return b.ExceededBy(tokens, costUSD, elapsed) != ""
}

// ExceededBy names the dimension that tripped, or "" if none has.
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

// Deadline is when the wall-clock ceiling will be reached for a run that began at start, or the
// zero time if no wall-clock ceiling is set.
func (b Budget) Deadline(start time.Time) time.Time {
	if b.MaxWallClock <= 0 {
		return time.Time{}
	}
	return start.Add(b.MaxWallClock.Duration())
}

// Remaining is how much of the wall-clock ceiling is left after elapsed, and whether a ceiling is
// set at all. Callers must pass the SAME elapsed the breaker is given (ExceededBy), so a decision
// taken mid-drain cannot disagree with the breaker's own verdict between drains.
func (b Budget) Remaining(elapsed time.Duration) (time.Duration, bool) {
	if b.MaxWallClock <= 0 {
		return 0, false
	}
	return b.MaxWallClock.Duration() - elapsed, true
}

func (c *Config) resolveMaxTurns(phase int) int {
	if phase > 0 {
		return phase
	}
	if c.MaxTurns > 0 {
		return c.MaxTurns
	}
	return DefaultMaxTurns
}

// resolveWallClock is the session wall-clock chain: the phase's own cap wins, then the run-wide
// one, then nothing. Unlike resolveMaxTurns this CAN resolve to zero, and zero means off — the
// dial ships disabled because no default number would be honest across harnesses (see
// Phase.MaxWallClock).
func (c *Config) resolveWallClock(phase Duration) Duration {
	if phase > 0 {
		return phase
	}
	return c.MaxSessionWallClock
}

// ZeroSpendAttemptBound is the effective bound on consecutive no-usage attempts: the operator's
// own, else the built-in default.
func (c *Config) ZeroSpendAttemptBound() int {
	if c.MaxZeroSpendAttempts > 0 {
		return c.MaxZeroSpendAttempts
	}
	return DefaultMaxZeroSpendAttempts
}

// RepeatedToolFailureBound is the effective repeated-failing-tool-call threshold: the operator's
// own, else the built-in default.
func (c *Config) RepeatedToolFailureBound() int {
	if c.MaxRepeatedToolFailures != nil {
		return *c.MaxRepeatedToolFailures
	}
	return DefaultMaxRepeatedToolFailures
}
