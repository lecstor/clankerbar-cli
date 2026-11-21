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
	// ONLY that harness's own sessions.
	PerHarness map[string]HarnessBudget `json:"per_harness"`
}

// HarnessBudget is one harness's own spend ceiling inside a run's Budget — see Budget.PerHarness
// for why a mixed-harness run needs one per side.
type HarnessBudget struct {
	MaxTokens  int     `json:"max_tokens"`   // cumulative tokens across THIS harness's sessions
	MaxCostUSD float64 `json:"max_cost_usd"` // cumulative $ across THIS harness's sessions
}

// CountsSpend reports whether this block bounds spend at all — the question Budget.CountsSpend
// answers, asked of one harness's block.
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

// ForHarness is the block configured for a harness, and whether there is one. A missing block is
// the zero HarnessBudget, disabled in every dimension, so a caller that does not care which it got
// may use the value and drop the flag.
func (b Budget) ForHarness(name string) (HarnessBudget, bool) {
	hb, ok := b.PerHarness[name]
	return hb, ok
}

// ExceededByHarness names the per-harness dimension that tripped for one harness's own accumulated
// spend, or "" if none has.
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

// The per-session ceiling's defaults, in the order the resolution chain falls through
// (SessionTokenCeiling).
const (
	// sessionTokenCeilingMultiplier derives the ceiling from budget.max_tokens when the operator set
	// one.
	sessionTokenCeilingMultiplier = 2

	// sessionTokenFloor is the ceiling when the operator set no budget ceiling at all.
	sessionTokenFloor = 150_000_000
)

// SessionTokenCeilingFor resolves the per-session runaway ceiling for a session on this harness:
// the operator's own dial, else 2x the run's max_tokens, else 2x that harness's own per_harness
// max_tokens, else the floor.
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
// the run's max_tokens, else the floor.
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
// the wall clock.
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
// set at all.
func (b Budget) Remaining(elapsed time.Duration) (time.Duration, bool) {
	if b.MaxWallClock <= 0 {
		return 0, false
	}
	return b.MaxWallClock.Duration() - elapsed, true
}

// resolveMaxTurns is the turn-cap chain: the phase's own cap wins, then the top-level cap, then the
// built-in default. Nothing resolves to zero — see Phase.MaxTurns and DefaultMaxTurns for why "0
// = uncapped" is gone.
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
// one, then nothing.
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
