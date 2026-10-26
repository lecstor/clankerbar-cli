package loop

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// Where per-phase harness selection (CLA-366) meets the two features that landed
// beside it: per-harness budget breakers (CLA-367) and the per-session wall-clock
// cap (CLA-368). Both were written against a driver holding ONE adapter, so every
// one of them asked `d.h` — the RUN's harness — a question that is really about
// the harness of the phase that just ran.
//
// These tests exist because that mistake is textually invisible. Merging the two
// branches produced no conflict at any of these call sites: `d.h.Name()` compiles,
// runs, and returns a plausible harness name, so a revert of any fix below is a
// green build that bills opencode's tokens to claude's breaker, hands an opencode
// session a ceiling derived from claude's block, or drops a checkpoint because it
// asked the wrong adapter to classify somebody else's exit. Every assertion here
// is arranged so the two harnesses DISAGREE — a driver that consults d.h gets the
// wrong answer, rather than the right one by coincidence.

// mixedBudgetDriver is mixedDriver with the two harnesses named and given
// budgets, so the ledger and the ceilings can be told apart per harness.
func mixedBudgetDriver(t *testing.T, impl, review *fakeAdapter, b config.Budget) *Driver {
	t.Helper()
	review.name = "claude"
	d := mixedDriver(t, impl, review)
	d.cfg.Budget = b
	return d
}

// The ledger CLA-367 keys by harness name is only worth keying if the two phases
// of one drain land in different entries.
func TestPerPhaseHarness_SpendIsChargedToThePhasesOwnHarness(t *testing.T) {
	impl := &fakeAdapter{steps: []invokeStep{{res: checkpointed(10_000, 0.10)}}}
	review := &fakeAdapter{steps: []invokeStep{{res: okResult(5_000, 0.05)}}}
	d := mixedBudgetDriver(t, impl, review, config.Budget{PerHarness: map[string]config.HarnessBudget{
		"opencode": {MaxTokens: 1_000_000},
		"claude":   {MaxTokens: 1_000_000},
	}})

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}

	if got := d.spentBy["opencode"].tokens; got != 10_000 {
		t.Errorf("opencode's ledger entry = %d tokens, want the 10,000 its own session spent", got)
	}
	if got := d.spentBy["claude"].tokens; got != 5_000 {
		t.Errorf("claude's ledger entry = %d tokens, want the 5,000 its own session spent", got)
	}
	// The failure this pins: charging d.h.Name() puts the WHOLE drain — both
	// phases — on the run's harness, so opencode's breaker never sees a token it
	// spent and claude's trips on spend that was never its.
	if len(d.spentBy) != 2 {
		t.Errorf("the drain's spend landed in %d ledger entries (%v), want one per harness — a single entry means every phase was charged to the run's harness",
			len(d.spentBy), d.spentBy)
	}
}

// The per-session runaway ceiling is compiled INTO the invocation, so asking the
// wrong harness for it is not a reporting error: it is the number that will
// actually kill the session.
func TestPerPhaseHarness_SessionTokenCeilingComesFromThePhasesOwnHarness(t *testing.T) {
	impl := &fakeAdapter{steps: []invokeStep{{res: checkpointed(10, 0.10)}}}
	review := &fakeAdapter{steps: []invokeStep{{res: okResult(5, 0.05)}}}
	// Deliberately far apart: a run whose implement phase is a cheap backend and
	// whose review phase is not is exactly the shape CLA-366 exists for, and the
	// two ceilings it derives have no business being the same number.
	b := config.Budget{PerHarness: map[string]config.HarnessBudget{
		"opencode": {MaxTokens: 5_000_000},
		"claude":   {MaxTokens: 60_000_000},
	}}
	d := mixedBudgetDriver(t, impl, review, b)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}

	wantOC := b.SessionTokenCeilingFor("opencode")
	wantCl := b.SessionTokenCeilingFor("claude")
	if wantOC == wantCl {
		t.Fatal("the fixture's two ceilings resolve to the same number, so this test could not tell the harnesses apart")
	}
	if got := impl.invocations[0].MaxSessionTokens; got != wantOC {
		t.Errorf("the opencode phase got MaxSessionTokens=%d, want %d from opencode's own block — %d is claude's",
			got, wantOC, wantCl)
	}
	if got := review.invocations[0].MaxSessionTokens; got != wantCl {
		t.Errorf("the claude phase got MaxSessionTokens=%d, want %d from claude's own block", got, wantCl)
	}
}

// wallClockBlind is an adapter that never classifies anything as wall-clock
// capped — an honest fake of a harness with no session cap (claude has none).
// Used as the RUN's adapter so that consulting d.h loses the classification.
type wallClockBlind struct{ *fakeAdapter }

func (wallClockBlind) WallClockCapped(harness.Result) bool { return false }

// CLA-368 made a wall-clock-capped session with WIP a checkpoint. Which sessions
// those ARE is a question only the harness that ran them can answer.
func TestPerPhaseHarness_WallClockCapIsClassifiedByThePhasesOwnAdapter(t *testing.T) {
	// The implement session ends on ITS OWN wall clock, holding a claim with WIP:
	// under CLA-368 that is a checkpoint, so the review phase runs. The WIP claim
	// names the salvaged branch, which the evidence gate (CLA-457) verifies via
	// the stub verifier mixedDriver installs.
	wip := harness.Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true, Branch: "clanker/x"}
	impl := &fakeAdapter{steps: []invokeStep{{res: held(wallClockResult(), wip)}}}
	blind := &fakeAdapter{steps: []invokeStep{{res: okResult(5, 0.05)}}}
	blind.name = "claude"
	d := mixedDriver(t, impl, blind)
	// The run's adapter is the blind one: if the driver asks IT whether the
	// opencode session hit a wall-clock cap, the answer is a confident "no", the
	// session is not checkpointable, and the sequence ends after phase 1.
	d.h = wallClockBlind{blind}

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}

	if blind.invokeCalls != 1 {
		t.Fatalf("the review phase ran %d session(s), want 1 — the implement session ended on its own harness's wall-clock cap holding WIP, which CLA-368 makes a checkpoint; asking the RUN's adapter to classify it loses that and strands the task after phase 1",
			blind.invokeCalls)
	}
}

// A session whose output could not be read is stopped on when a spend ceiling is
// set, because the ceiling can no longer be honoured (CLA-262). Since CLA-367
// "is a ceiling set" is a per-harness question — and the harness that must be
// asked is the one whose session went unreadable, not the run's.
func TestPerPhaseHarness_UntrustedBreakerAsksThePhasesOwnHarness(t *testing.T) {
	for _, tc := range []struct {
		name     string
		perHarn  map[string]config.HarnessBudget
		wantStop bool
	}{
		{
			// opencode's own ceiling is live, and it is opencode's session that
			// went unreadable: the promise this run made cannot be kept, so it
			// stops.
			name:     "the unreadable session's own harness has a ceiling",
			perHarn:  map[string]config.HarnessBudget{"opencode": {MaxTokens: 5_000_000}},
			wantStop: true,
		},
		{
			// The only ceiling belongs to claude, whose session has not run and is
			// not the one in question. Nothing promised about the opencode session
			// has been broken, so the drain does not stop — and a driver asking
			// d.h ("claude") here would stop a run it had no reason to.
			name:     "only the OTHER harness has a ceiling",
			perHarn:  map[string]config.HarnessBudget{"claude": {MaxTokens: 5_000_000}},
			wantStop: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			impl := &fakeAdapter{steps: []invokeStep{{res: untrustedResult(9_000, 0.09)}}}
			review := &fakeAdapter{steps: []invokeStep{{res: okResult(5, 0.05)}}}
			d := mixedBudgetDriver(t, impl, review, config.Budget{PerHarness: tc.perHarn})

			_, _, stop, err := drainPhasesOnce(t, d)
			if err != nil {
				t.Fatalf("drainPhases: %v", err)
			}
			if stop != tc.wantStop {
				t.Errorf("stop = %t, want %t — the breaker asked the wrong harness whether a ceiling is set\n%s",
					stop, tc.wantStop, logs.String())
			}
			if !strings.Contains(logs.String(), "UNTRUSTED") {
				t.Fatalf("the drain did not take the untrusted path at all:\n%s", logs.String())
			}
		})
	}
}

// The supervised-wait loop is the fifth and sixth reconciled call site, and the
// two the first mutation check missed: reverting them alone left the package
// green, because the four tests above are reddened by the OTHER mutations and
// none of them ever enters this loop. A call site whose only evidence is that
// some other test fails has no evidence at all.
//
// Both are about a phase that hit a usage limit and is being waited out. The
// probes it spawns are real paid sessions on THAT phase's harness, so their spend
// is that harness's, and whether a ceiling exists to be honoured is that
// harness's question too.
func TestPerPhaseHarness_SupervisedWaitChargesTheWaitingPhasesHarness(t *testing.T) {
	impl := &fakeAdapter{
		// One probe that reports the limit still live, then the wait is cut short
		// by the context. What matters is that the probe was charged at all.
		probeResults: []harness.Limit{{Limited: true}},
		probeTokens:  7_000,
		probeCost:    0.07,
	}
	review := &fakeAdapter{}
	d := mixedBudgetDriver(t, impl, review, config.Budget{})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	implPhase := d.cfg.EffectivePhases()[0]
	d.supervisedWait(ctx, harness.Limit{Limited: true}, namedFake{impl, "opencode"}, implPhase, d.targets[0], spend{start: time.Now()})

	if got := d.spentBy["opencode"].tokens; got == 0 {
		t.Fatalf("the opencode phase's probes spent nothing on opencode's ledger (%v) — a probe is a paid session on the harness that RAN it, and charging d.h bills it to claude",
			d.spentBy)
	}
	if _, billed := d.spentBy["claude"]; billed {
		t.Errorf("claude was billed for a wait on the opencode phase: %v", d.spentBy)
	}
}

// The sibling: an unreadable PROBE stops the run only when a ceiling it can no
// longer honour is actually set — and that is a per-harness question about the
// harness being probed, not the run's.
func TestPerPhaseHarness_SupervisedWaitUntrustedProbeAsksThePhasesOwnHarness(t *testing.T) {
	for _, tc := range []struct {
		name     string
		perHarn  map[string]config.HarnessBudget
		wantStop bool
		wantLog  string
	}{
		{
			name:     "the probed harness has a ceiling",
			perHarn:  map[string]config.HarnessBudget{"opencode": {MaxTokens: 5_000_000}},
			wantStop: true,
			wantLog:  "stopping rather than polling on",
		},
		{
			// The ceiling belongs to claude, which is not what is being probed.
			// Nothing promised about these probes has been broken, so the wait
			// continues — and a driver asking d.h would abandon the run instead.
			name:     "only the OTHER harness has a ceiling",
			perHarn:  map[string]config.HarnessBudget{"claude": {MaxTokens: 5_000_000}},
			wantStop: false,
			wantLog:  "will retry next interval",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			impl := &fakeAdapter{probeErrs: []error{
				fmt.Errorf("%w: the harness's stdout could not be read to the end", harness.ErrUntrusted),
			}}
			d := mixedBudgetDriver(t, impl, &fakeAdapter{}, config.Budget{PerHarness: tc.perHarn})

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			implPhase := d.cfg.EffectivePhases()[0]
			_, _, stop := d.supervisedWait(ctx, harness.Limit{Limited: true}, namedFake{impl, "opencode"}, implPhase, d.targets[0], spend{start: time.Now()})

			if stop != tc.wantStop {
				t.Errorf("stop = %t, want %t — the breaker asked the wrong harness whether a ceiling is set\n%s",
					stop, tc.wantStop, logs.String())
			}
			if !strings.Contains(logs.String(), tc.wantLog) {
				t.Errorf("logs do not contain %q:\n%s", tc.wantLog, logs.String())
			}
		})
	}
}
