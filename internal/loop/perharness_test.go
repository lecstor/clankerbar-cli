package loop

import (
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/config"
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
