package loop

import (
	"errors"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// CLA-288: a ceiling can only stop spend it is told about. An attempt that dies
// before its harness reports usage adds nothing to either accumulator, so under
// `max_retries: 0` - the documented default, never give up - a token- or cost-only
// budget can never be reached and only max_wall_clock ends the run. The bound
// below is what makes that loop finite, and the reset is what keeps it from
// touching an ordinary retry ladder.

// silentTransientResult is the attempt at the heart of the bug: transient, and
// dead before any usage-bearing event arrived, so the budget sees nothing.
func silentTransientResult() harness.Result {
	return harness.Result{ExitCode: 1, Raw: map[string]any{"kind": "transient"}}
}

// reportingTransientResult is a transient failure whose session DID report - and
// reported zero. This is the case the bound must not count: the harness told us
// what it spent, and the honest answer happened to be nothing.
func reportingTransientResult() harness.Result {
	return harness.Result{ExitCode: 1, UsageReported: true, Raw: map[string]any{"kind": "transient"}}
}

func steps(results ...harness.Result) []invokeStep {
	out := make([]invokeStep, 0, len(results))
	for _, r := range results {
		out = append(out, invokeStep{res: r})
	}
	return out
}

func TestDrainPhase_ZeroSpendAttemptBound(t *testing.T) {
	t.Run("the bound fires once the default number of silent attempts is reached", func(t *testing.T) {
		cfg := fastCfg()
		cfg.StateDir = t.TempDir()
		// The exact configuration from the report: never give up, and a cost
		// ceiling that nothing feeds. Without the bound this script never ends.
		cfg.MaxRetries = 0
		cfg.Budget.MaxCostUSD = 10
		h := &fakeAdapter{steps: steps(
			silentTransientResult(), silentTransientResult(), silentTransientResult(),
			silentTransientResult(), silentTransientResult(),
		)}
		d := New(cfg, h, &fakePoller{})
		openTestStateDir(t, d)

		_, _, stop, err := drainOnce(t, d)
		if !errors.Is(err, errZeroSpendLoop) {
			t.Fatalf("drain ended with %v, want an error wrapping errZeroSpendLoop - the operator has to be able to tell this from a budget or wall-clock stop", err)
		}
		if stop {
			t.Error("the zero-spend bound is a failure, not a clean stop: stop must be false so the run reports an error rather than looking like a tidy finish")
		}
		// Three silent attempts, and the fourth spawn refused. The bound is
		// enforced on the decision to spawn AGAIN, so it costs exactly the
		// attempts it counted and not one more.
		if h.invokeCalls != config.DefaultMaxZeroSpendAttempts {
			t.Errorf("spawned %d sessions, want %d - the bound must refuse the next spawn, not run it first", h.invokeCalls, config.DefaultMaxZeroSpendAttempts)
		}
		// The message is the whole point of a distinct error: it has to name the
		// loop and the dial, or the operator is back to guessing.
		if msg := err.Error(); !strings.Contains(msg, "before fake reported any usage") || !strings.Contains(msg, "max_zero_spend_attempts") {
			t.Errorf("error message %q must name what happened and the dial that tunes it", msg)
		}
	})

	t.Run("an attempt that reports usage resets the count, including a report of zero", func(t *testing.T) {
		cfg := fastCfg()
		cfg.StateDir = t.TempDir()
		cfg.MaxRetries = 0
		// Two silent, one reporting zero, two silent, then a clean finish. Nothing
		// here is three consecutive silences, so the drain must complete - and the
		// reporting attempt in the middle is the reset doing its job, since without
		// it the fourth silent attempt would be counted as the third.
		h := &fakeAdapter{steps: steps(
			silentTransientResult(), silentTransientResult(),
			reportingTransientResult(),
			silentTransientResult(), silentTransientResult(),
			okResult(1_000, 0.5),
		)}
		d := New(cfg, h, &fakePoller{})
		openTestStateDir(t, d)

		_, _, _, err := drainOnce(t, d)
		if err != nil {
			t.Fatalf("drain failed with %v, want a clean finish: a session that reported zero spend still REPORTED, so it must not count toward the bound", err)
		}
		if h.invokeCalls != 6 {
			t.Errorf("spawned %d sessions, want all 6 of the script", h.invokeCalls)
		}
	})

	t.Run("a transient ladder below the bound is unaffected", func(t *testing.T) {
		cfg := fastCfg()
		cfg.StateDir = t.TempDir()
		cfg.MaxRetries = 0
		h := &fakeAdapter{steps: steps(
			silentTransientResult(), silentTransientResult(), okResult(2_000, 1),
		)}
		d := New(cfg, h, &fakePoller{})
		openTestStateDir(t, d)

		tokens, _, stop, err := drainOnce(t, d)
		if err != nil || stop {
			t.Fatalf("drain returned (stop=%v, err=%v), want a clean retry-then-succeed - the bound must not shorten the ladder an operator tuned retry_cap for", stop, err)
		}
		if h.invokeCalls != 3 {
			t.Errorf("spawned %d sessions, want 3", h.invokeCalls)
		}
		if tokens != 2_000 {
			t.Errorf("counted %d tokens, want 2000 from the successful attempt", tokens)
		}
	})

	// The regression the adversarial review caught. A session the subscription cap
	// turned away often reports nothing at all - under claude the notice can arrive
	// on stderr with no stream at all - so counting it as silence ends the run on
	// the ordinary overnight quota wait, and blames a harness that is starting
	// perfectly well. A usage limit is counted NEITHER way: it is a known cause
	// with its own breakers, and it must not reset the count either.
	t.Run("a usage-limit attempt is not counted as silence", func(t *testing.T) {
		cfg := fastCfg()
		cfg.StateDir = t.TempDir()
		cfg.MaxRetries = 0
		cfg.Budget.MaxCostUSD = 10
		// Four limited-at-spawn attempts, none reporting usage, then a clean
		// finish. Every probe reports the limit lifted, so each pause resumes.
		h := &fakeAdapter{steps: steps(
			limitResult(), limitResult(), limitResult(), limitResult(),
			okResult(1_000, 0.5),
		)}
		d := New(cfg, h, &fakePoller{})
		openTestStateDir(t, d)

		if _, _, _, err := drainOnce(t, d); err != nil {
			t.Fatalf("drain failed with %v - waiting out a quota is not a zero-spend loop, and the run must survive it", err)
		}
		if h.invokeCalls != 5 {
			t.Errorf("spawned %d sessions, want all 5 of the script", h.invokeCalls)
		}
	})

	t.Run("a usage limit does not reset the count either", func(t *testing.T) {
		cfg := fastCfg()
		cfg.StateDir = t.TempDir()
		cfg.MaxRetries = 0
		// Silent, silent, a limit in between, then silent. The limit is neither a
		// report nor a silence, so the third silent attempt is still the third and
		// the bound fires - otherwise a flapping quota would hide the loop.
		h := &fakeAdapter{steps: steps(
			silentTransientResult(), silentTransientResult(),
			limitResult(),
			silentTransientResult(), silentTransientResult(),
		)}
		d := New(cfg, h, &fakePoller{})
		openTestStateDir(t, d)

		if _, _, _, err := drainOnce(t, d); !errors.Is(err, errZeroSpendLoop) {
			t.Fatalf("drain ended with %v, want the zero-spend error: a limit between silences must not clear the count", err)
		}
		if h.invokeCalls != 4 {
			t.Errorf("spawned %d sessions, want 4 (three silent plus the limit)", h.invokeCalls)
		}
	})

	t.Run("the bound is config-overridable", func(t *testing.T) {
		cfg := fastCfg()
		cfg.StateDir = t.TempDir()
		cfg.MaxRetries = 0
		cfg.MaxZeroSpendAttempts = 5
		h := &fakeAdapter{steps: steps(
			silentTransientResult(), silentTransientResult(), silentTransientResult(),
			silentTransientResult(), silentTransientResult(), silentTransientResult(),
		)}
		d := New(cfg, h, &fakePoller{})
		openTestStateDir(t, d)

		if _, _, _, err := drainOnce(t, d); !errors.Is(err, errZeroSpendLoop) {
			t.Fatalf("drain ended with %v, want the zero-spend error at the configured bound", err)
		}
		if h.invokeCalls != 5 {
			t.Errorf("spawned %d sessions, want the configured 5", h.invokeCalls)
		}
	})
}
