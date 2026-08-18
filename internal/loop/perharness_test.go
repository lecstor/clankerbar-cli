package loop

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// ---------------------------------------------------------------------------
// Per-harness budget breakers (CLA-367).
//
// One ceiling over every session is the right shape while a run drives one
// harness and the wrong one the moment it drives two: `max_tokens` is calibrated
// against a subscription plan (75M is a sane week of Claude and roughly $2 on a
// DeepSeek-class backend), and `max_cost_usd` is meaningless for a session billed
// to a subscription. So each harness carries its own block, over its own
// sessions, and any trip stops the whole run.

// The metered side: dollars, summed off the CostUSD the adapter reports.
func TestRun_PerHarnessBudgetBreaker_MeteredCost(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.Harness = "opencode"
	cfg.Budget = config.Budget{PerHarness: map[string]config.HarnessBudget{
		"opencode": {MaxCostUSD: 1.0},
	}}
	h := &fakeAdapter{name: "opencode", steps: []invokeStep{{res: okResult(0, 2.0)}}}
	p := &fakePoller{sum: backlog.Summary{Claimable: 1}}
	if err := runLoop(t, cfg, h, p); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if h.invokeCalls != 1 {
		t.Errorf("the opencode cost block should stop the run after one over-budget drain; got %d Invoke calls", h.invokeCalls)
	}
}

// The claude side: plan-calibrated tokens, the semantics the global dial has
// always had, now counted over claude's own sessions.
func TestRun_PerHarnessBudgetBreaker_ClaudeTokens(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.Budget = config.Budget{PerHarness: map[string]config.HarnessBudget{
		"claude": {MaxTokens: 10},
	}}
	h := &fakeAdapter{name: "claude", steps: []invokeStep{{res: okResult(100, 0)}}}
	p := &fakePoller{sum: backlog.Summary{Claimable: 1}}
	if err := runLoop(t, cfg, h, p); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if h.invokeCalls != 1 {
		t.Errorf("the claude token block should stop the run after one over-budget drain; got %d Invoke calls", h.invokeCalls)
	}
}

// Independence is the whole point: a block belongs to its harness, so an
// opencode run is not stopped by claude's token ceiling however many tokens it
// burns — a cheap backend's tokens are not the claude plan's tokens.
func TestRun_PerHarnessBudgetIsNotChargedToAnotherHarness(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.Harness = "opencode"
	cfg.MaxIterations = 2 // the only thing that should end this run
	cfg.Budget = config.Budget{PerHarness: map[string]config.HarnessBudget{
		"claude": {MaxTokens: 10},
	}}
	h := &fakeAdapter{name: "opencode", tokens: 1_000_000}
	p := &fakePoller{sum: backlog.Summary{Claimable: 1}}
	if err := runLoop(t, cfg, h, p); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if h.invokeCalls != 2 {
		t.Errorf("claude's block stopped an opencode run: got %d Invoke calls, want the 2 max_iterations allows", h.invokeCalls)
	}
}

// Every pre-CLA-367 config: the global dials, no blocks, byte-for-byte the old
// behaviour — and a per-harness ledger that exists but bounds nothing.
func TestRun_GlobalBudgetUnchangedWithoutPerHarnessBlocks(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.Budget = config.Budget{MaxTokens: 10}
	h := &fakeAdapter{steps: []invokeStep{{res: okResult(100, 0)}}}
	p := &fakePoller{sum: backlog.Summary{Claimable: 1}}
	if err := runLoop(t, cfg, h, p); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if h.invokeCalls != 1 {
		t.Errorf("the global token dial should still stop after one over-budget drain; got %d Invoke calls", h.invokeCalls)
	}
}

// The mixed-harness case the task is about, at the level where it will actually
// arise once a phase can name its own harness (CLA-366): one run, two harnesses,
// two ledger entries, and either ceiling able to stop it. The ledger is charged
// directly here because the driver still drives one adapter per run.
func TestBudgetTrip_MixedHarnessLedger(t *testing.T) {
	newDriver := func() *Driver {
		cfg := fastCfg()
		cfg.Budget = config.Budget{PerHarness: map[string]config.HarnessBudget{
			"claude":   {MaxTokens: 75_000_000},
			"opencode": {MaxCostUSD: 2},
		}}
		return New(cfg, &fakeAdapter{}, busyPoller())
	}

	t.Run("neither side over: nothing trips", func(t *testing.T) {
		d := newDriver()
		d.charge("claude", 60_000_000, 0)
		d.charge("opencode", 900_000, 1.5)
		if dim := d.budgetTrip(60_900_000, 1.5, time.Minute); dim != "" {
			t.Errorf("budgetTrip = %q, want no trip", dim)
		}
	})

	t.Run("the metered side trips on its dollars", func(t *testing.T) {
		d := newDriver()
		d.charge("claude", 60_000_000, 0)
		d.charge("opencode", 900_000, 2.5)
		dim := d.budgetTrip(60_900_000, 2.5, time.Minute)
		if !strings.Contains(dim, "opencode cost") {
			t.Errorf("budgetTrip = %q, want the opencode cost ceiling named", dim)
		}
	})

	t.Run("the claude side trips on its tokens", func(t *testing.T) {
		d := newDriver()
		d.charge("claude", 80_000_000, 0)
		d.charge("opencode", 900_000, 1.0)
		dim := d.budgetTrip(80_900_000, 1.0, time.Minute)
		if !strings.Contains(dim, "claude tokens") {
			t.Errorf("budgetTrip = %q, want the claude token ceiling named", dim)
		}
	})

	t.Run("spend on one side is never charged to the other", func(t *testing.T) {
		// 200M tokens on opencode would have ended claude's side twice over; it
		// reaches opencode's block, which is measured in dollars, and stops
		// nothing. This is the failure the split exists to prevent — the reason
		// the opencode config carried no breaker at all before.
		d := newDriver()
		d.charge("opencode", 200_000_000, 0.5)
		if dim := d.budgetTrip(200_000_000, 0.5, time.Minute); dim != "" {
			t.Errorf("budgetTrip = %q, want no trip: opencode's spend is not claude's", dim)
		}
	})

	t.Run("a global dial is still reported first, over everything", func(t *testing.T) {
		d := newDriver()
		d.cfg.Budget.MaxWallClock = config.Duration(time.Nanosecond)
		d.charge("opencode", 0, 2.5)
		if dim := d.budgetTrip(0, 2.5, time.Hour); !strings.Contains(dim, "wall clock") {
			t.Errorf("budgetTrip = %q, want the run-wide dial named when it is the one broken", dim)
		}
	})
}

// A probe is a paid session too (CLA-287), so its spend has to reach the
// per-harness ledger, not only the run-wide accumulator.
func TestPerHarnessLedgerCountsSessionsAndProbes(t *testing.T) {
	cfg := fastCfg()
	cfg.Harness = "opencode"
	d := New(cfg, &fakeAdapter{name: "opencode"}, busyPoller())
	d.charge("opencode", 10, 0.25)
	d.charge("opencode", 5, 0.75)
	if got := d.spentBy["opencode"]; got.tokens != 15 || got.cost != 1.0 {
		t.Errorf("ledger = %+v, want tokens=15 cost=1", got)
	}
	if _, ok := d.spentBy["claude"]; ok {
		t.Error("a harness that never ran must not appear in the ledger")
	}
}

// CLA-262's side effect, per side: a session whose stream could not be read whole
// stops the run when a spend ceiling was promised over it, and only then. With
// per-harness blocks that promise follows the harness whose breaker is set.
func TestDrain_UntrustedFollowsThePerHarnessBreaker(t *testing.T) {
	for _, tc := range []struct {
		name     string
		harness  string
		budget   config.Budget
		wantStop bool
	}{
		{
			name:    "the running harness's own cost block can no longer be honoured",
			harness: "opencode",
			budget: config.Budget{PerHarness: map[string]config.HarnessBudget{
				"opencode": {MaxCostUSD: 2},
			}},
			wantStop: true,
		},
		{
			name:    "the running harness's own token block can no longer be honoured",
			harness: "claude",
			budget: config.Budget{PerHarness: map[string]config.HarnessBudget{
				"claude": {MaxTokens: 75_000_000},
			}},
			wantStop: true,
		},
		{
			// The block belongs to a harness this session is not: nothing that
			// was promised about this session has been broken, so the run goes on
			// exactly as it does with no ceiling at all.
			name:    "another harness's block promises nothing about this session",
			harness: "opencode",
			budget: config.Budget{PerHarness: map[string]config.HarnessBudget{
				"claude": {MaxTokens: 75_000_000},
			}},
			wantStop: false,
		},
		{
			// The clock does not depend on anything the child said, so that
			// ceiling is intact whichever harness produced the unreadable stream.
			name:     "a wall-clock ceiling alone is unaffected, as before",
			harness:  "opencode",
			budget:   config.Budget{MaxWallClock: config.Duration(8 * time.Hour)},
			wantStop: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			cfg := fastCfg()
			cfg.Harness = tc.harness
			cfg.Budget = tc.budget
			h := &fakeAdapter{name: tc.harness, steps: []invokeStep{{res: untrustedResult(10, 0.5)}}}
			d := New(cfg, h, busyPoller())
			openTestStateDir(t, d)

			_, _, stop, err := drainOnce(t, d)
			if err != nil {
				t.Fatalf("drain returned error: %v", err)
			}
			if stop != tc.wantStop {
				t.Errorf("stop = %t, want %t", stop, tc.wantStop)
			}
			if tc.wantStop && !strings.Contains(logs.String(), "ceiling can no longer be honoured") {
				t.Errorf("stopped without saying why:\n%s", logs.String())
			}
		})
	}
}

// The other half of the CLA-262 interaction, and the one the suite would not
// have noticed being reverted: the supervised wait's probe. A probe is a paid
// session, and one whose own output cannot be read reports no verdict AND no
// spend — so polling on it under a spend ceiling re-spawns a paid session every
// interval against a ceiling that can never see the cost. With per-harness
// blocks, that promise follows the harness whose breaker is set.
func TestSupervisedWait_UnreadableProbeFollowsThePerHarnessBreaker(t *testing.T) {
	for _, tc := range []struct {
		name     string
		harness  string
		budget   config.Budget
		wantStop bool
		wantLog  string
	}{
		{
			name:    "the running harness's own cost block cannot survive an unreadable probe",
			harness: "opencode",
			budget: config.Budget{PerHarness: map[string]config.HarnessBudget{
				"opencode": {MaxCostUSD: 2},
			}},
			wantStop: true,
			wantLog:  "cannot be counted",
		},
		{
			// The block belongs to a harness these probes are not, so nothing
			// promised about them has been broken: wait on, as with no ceiling.
			name:    "another harness's block promises nothing about these probes",
			harness: "opencode",
			budget: config.Budget{PerHarness: map[string]config.HarnessBudget{
				"claude": {MaxTokens: 75_000_000},
			}},
			wantStop: false,
			wantLog:  "will retry next interval",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			cfg := fastCfg()
			cfg.Harness = tc.harness
			cfg.Budget = tc.budget

			h := &fakeAdapter{name: tc.harness, probeErrs: []error{
				fmt.Errorf("%w: the harness's stdout could not be read to the end", harness.ErrUntrusted),
			}}
			d := New(cfg, h, busyPoller())
			openTestStateDir(t, d)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// The adapter and phase are the ones the WAITING phase runs on - which
			// is what supervisedWait now charges and asks its ceiling of, rather
			// than the run's d.h (CLA-366). Here they are the same, so the case
			// this table describes is unchanged; the two
			// TestPerPhaseHarness_SupervisedWait* tests in crossharness_test.go are
			// where they differ.
			_, _, stop := d.supervisedWait(ctx, harness.Limit{Limited: true}, h, cfg.EffectivePhases()[0], d.targets[0], spend{start: time.Now()})

			if stop != tc.wantStop {
				t.Errorf("stop = %t, want %t", stop, tc.wantStop)
			}
			if !strings.Contains(logs.String(), tc.wantLog) {
				t.Errorf("logs do not contain %q:\n%s", tc.wantLog, logs.String())
			}
		})
	}
}

// The breaker has to be reachable from INSIDE a drain, not only between drains
// (CLA-258): every path that loops back has just waited, and those waits can run
// for hours. A per-harness block is checked at the same site, which this drives
// directly — the retry ladder, with the block already over its ceiling.
func TestDrainMidDrain_PerHarnessBlockStopsTheRetryLadder(t *testing.T) {
	logs := captureLogs(t)
	cfg := fastCfg()
	cfg.Harness = "opencode"
	cfg.Budget = config.Budget{PerHarness: map[string]config.HarnessBudget{
		"opencode": {MaxCostUSD: 2},
	}}
	// A transient failure that still SPENT: without a ceiling this retries forever
	// (max_retries defaults to 0), so the mid-drain check on the way round the
	// ladder is the only thing that can end it. A failed attempt's spend counts —
	// a "leave headroom" breaker must err toward seeing real spend.
	spendyFailure := harness.Result{ExitCode: 1, CostUSD: 2.5, Raw: map[string]any{"kind": "transient"}}
	h := &fakeAdapter{name: "opencode", steps: []invokeStep{{res: spendyFailure}}}
	d := New(cfg, h, busyPoller())
	openTestStateDir(t, d)

	_, _, stop, err := drainOnce(t, d)
	if err != nil {
		t.Fatalf("drain returned error: %v", err)
	}
	if !stop {
		t.Error("stop = false: an over-budget per-harness block must end the drain rather than re-spawn a paid session")
	}
	if out := logs.String(); !strings.Contains(out, "opencode cost") {
		t.Errorf("the trip did not name the harness whose block stopped it:\n%s", out)
	}
}

// A run-wide figure printed after a per-harness reason reads as a contradiction
// in a mixed-harness run ("opencode cost $2.05 >= $2.00 ... cost=$4.50"), so the
// tripped harness's own totals ride along with the reason.
func TestBudgetTrip_NamesTheTrippedHarnessesOwnSpend(t *testing.T) {
	cfg := fastCfg()
	cfg.Budget = config.Budget{PerHarness: map[string]config.HarnessBudget{
		"opencode": {MaxCostUSD: 2},
	}}
	d := New(cfg, &fakeAdapter{}, busyPoller())
	d.charge("claude", 60_000_000, 0)
	d.charge("opencode", 900_000, 2.5)

	dim := d.budgetTrip(60_900_000, 2.5, time.Minute)
	for _, want := range []string{"opencode cost", "opencode so far", "tokens=900000", "cost=$2.50"} {
		if !strings.Contains(dim, want) {
			t.Errorf("budgetTrip = %q, want it to carry %q", dim, want)
		}
	}
}

// Moving a token ceiling out of the global dial and into a per-harness block is
// exactly what CLA-367 recommends, and it must not loosen the per-session
// runaway detector CLA-343 exists to be: the derivation follows the ceiling.
func TestInvocationDerivesTheSessionCeilingFromThePerHarnessBlock(t *testing.T) {
	cfg := fastCfg()
	cfg.Harness = "opencode"
	cfg.MaxIterations = 1
	cfg.StateDir = t.TempDir()
	cfg.Budget = config.Budget{PerHarness: map[string]config.HarnessBudget{
		"opencode": {MaxTokens: 20_000_000},
	}}
	h := &fakeAdapter{name: "opencode"}
	if err := runLoop(t, cfg, h, &fakePoller{sum: backlog.Summary{Claimable: 1}}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(h.invocations) == 0 {
		t.Fatal("no session was spawned")
	}
	if got := h.invocations[0].MaxSessionTokens; got != 40_000_000 {
		t.Errorf("MaxSessionTokens = %d, want 40000000 (2x the harness's own token ceiling), not the 150M floor", got)
	}
}
