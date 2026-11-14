package loop

// Stored run-config budget enforcement (CLA-480). A ratified document's budget
// governs its own project's spend: its per_harness blocks are live, a looser
// ceiling it sets actually raises that project's ceiling, and a project that
// exhausts its ceiling stops draining without taking its siblings down.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// A stored per_harness block must trip on the target's own sessions. Before
// CLA-480 this was accepted by the overlay and reported by doctor, but the
// breaker only ever read the base config's map.
func TestRun_StoredPerHarnessCeilingTrips(t *testing.T) {
	cfg := fastCfg()
	cfg.Harness = "claude"
	h := &fakeAdapter{name: "claude", tokens: 100}
	rc := &fakeRCfg{docs: map[int]string{
		1: `{"budget":{"per_harness":{"claude":{"max_tokens":10}}}}`,
	}}
	logs := captureLogs(t)

	d := rcRunDriver(t, cfg, []backlog.Summary{
		{Claimable: 1, RunConfigVersion: 1},
		{Claimable: 1, RunConfigVersion: 1},
		{Claimable: 1, RunConfigVersion: 1},
	}, rc, h)

	if d.targets[0].Cfg == nil {
		t.Fatal("the stored budget was refused; want the overlay in force")
	}
	if h.invokeCalls != 1 {
		t.Errorf("the stored per_harness ceiling should stop this project after one over-budget drain; got %d Invoke calls", h.invokeCalls)
	}
	if out := logs.String(); !strings.Contains(out, "claude tokens") {
		t.Errorf("the trip did not name the stored per-harness ceiling:\n%s", out)
	}
}

// A ratified ceiling LOOSER than the local file's must be the one in force for
// its project: the local dial cannot keep acting as a second, tighter bound.
func TestRun_StoredLooserCeilingRaisesTheEffectiveCeiling(t *testing.T) {
	cfg := fastCfg()
	cfg.Budget = config.Budget{MaxTokens: 10} // local file's ceiling
	h := &fakeAdapter{name: "claude", tokens: 100}
	rc := &fakeRCfg{docs: map[int]string{
		1: `{"budget":{"max_tokens":1000}}`, // ratified, deliberately looser
	}}

	d := rcRunDriver(t, cfg, []backlog.Summary{
		{Claimable: 1, RunConfigVersion: 1},
		{Claimable: 1, RunConfigVersion: 1},
	}, rc, h)

	if d.targets[0].Cfg == nil {
		t.Fatal("the stored budget was refused; want the overlay in force")
	}
	if h.invokeCalls != 2 {
		t.Errorf("a stored looser ceiling must raise the project's ceiling: got %d Invoke calls, want the 2 max_iterations allows (the local 10-token dial stopped it)", h.invokeCalls)
	}
}

// The other direction: the stored ceiling is still a ceiling. A tighter ratified
// dial must trip where the local one would have allowed the spend.
func TestRun_StoredTighterCeilingStillTrips(t *testing.T) {
	cfg := fastCfg()
	cfg.Budget = config.Budget{MaxTokens: 1000} // local file would allow this
	h := &fakeAdapter{name: "claude", tokens: 100}
	rc := &fakeRCfg{docs: map[int]string{
		1: `{"budget":{"max_tokens":50}}`, // ratified, deliberately tighter
	}}

	d := rcRunDriver(t, cfg, []backlog.Summary{
		{Claimable: 1, RunConfigVersion: 1},
		{Claimable: 1, RunConfigVersion: 1},
	}, rc, h)

	if d.targets[0].Cfg == nil {
		t.Fatal("the stored budget was refused; want the overlay in force")
	}
	if h.invokeCalls != 1 {
		t.Errorf("the stored tighter ceiling must stop this project after one drain; got %d Invoke calls", h.invokeCalls)
	}
}

// A project that exhausts its ratified ceiling must stop draining while its
// siblings carry on. Before CLA-480 the per-target breaker returned out of Run,
// so one project's ceiling took the whole daemon down.
func TestRun_OneTargetBudgetTripLeavesTheOtherDraining(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 4 // generous: only the budget can end this earlier
	h := &fakeAdapter{name: "claude", tokens: 100}
	targets := []Target{
		{Name: "alpha", WorkDir: "/repos/alpha", Poller: &fakePoller{sum: backlog.Summary{Claimable: 1, RunConfigVersion: 1}},
			RCfg: &fakeRCfg{docs: map[int]string{1: `{"budget":{"max_tokens":10}}`}}},
		{Name: "beta", WorkDir: "/repos/beta", Poller: &fakePoller{sum: backlog.Summary{Claimable: 1}}},
	}
	logs := captureLogs(t)
	d := NewMulti(cfg, h, targets)
	openTestStateDir(t, d)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	alpha, beta := 0, 0
	for _, inv := range h.invocations {
		switch inv.WorkDir {
		case "/repos/alpha":
			alpha++
		case "/repos/beta":
			beta++
		default:
			t.Fatalf("session ran in unexpected workdir %q", inv.WorkDir)
		}
	}
	if alpha != 1 {
		t.Errorf("alpha (over its stored ceiling) drained %d times, want 1", alpha)
	}
	if beta != 3 {
		t.Errorf("beta drained %d times, want 3 - the sibling must keep draining after alpha's trip (total invokes %d, max-iterations 4)", beta, len(h.invocations))
	}
	if out := logs.String(); strings.Contains(out, "every project's budget is exhausted") {
		t.Errorf("alpha's trip was reported as a run-wide stop while beta still had budget:\n%s", out)
	}
}

// The same scope on the MID-drain path: when a retry loop's next pre-spawn check
// finds this target over its ceiling, the drain ends but the sibling keeps
// draining. This is the path the naive fix misses (the drain itself returns
// stop), so it is pinned separately from the between-drains sweep above.
func TestRun_MidDrainBudgetTripLeavesTheOtherDraining(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 4
	h := &fakeAdapter{name: "claude", steps: []invokeStep{
		{res: okResult(0, 0)}, // beta's first drain
		// alpha's session spends past its stored ceiling and ends transient, so
		// the retry ladder comes back through the pre-spawn breaker and trips.
		{res: harness.Result{ExitCode: 1, Tokens: 100, Raw: map[string]any{"kind": "transient"}}},
	}}
	targets := []Target{
		{Name: "alpha", WorkDir: "/repos/alpha", Poller: &fakePoller{sum: backlog.Summary{Claimable: 1, RunConfigVersion: 1}},
			RCfg: &fakeRCfg{docs: map[int]string{1: `{"budget":{"max_tokens":10}}`}}},
		{Name: "beta", WorkDir: "/repos/beta", Poller: &fakePoller{sum: backlog.Summary{Claimable: 1}}},
	}
	logs := captureLogs(t)
	d := NewMulti(cfg, h, targets)
	openTestStateDir(t, d)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	alpha, beta := 0, 0
	for _, inv := range h.invocations {
		switch inv.WorkDir {
		case "/repos/alpha":
			alpha++
		case "/repos/beta":
			beta++
		}
	}
	if alpha != 1 || beta != 3 {
		t.Errorf("drains alpha=%d beta=%d, want 1 and 3 - a mid-drain trip on alpha must not end the run (max-iterations 4)", alpha, beta)
	}
	if out := logs.String(); !strings.Contains(out, "budget exhausted mid-drain") {
		t.Errorf("the mid-drain trip was not reported as this project's:\n%s", out)
	}
}

// "Stop only when every target is exhausted": once the last target reaches its
// ceiling the run ends, without waiting for max_iterations.
func TestRun_AllTargetsExhaustedStops(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 10
	h := &fakeAdapter{name: "claude", tokens: 100}
	targets := []Target{
		{Name: "alpha", WorkDir: "/repos/alpha", Poller: &fakePoller{sum: backlog.Summary{Claimable: 1, RunConfigVersion: 1}},
			RCfg: &fakeRCfg{docs: map[int]string{1: `{"budget":{"max_tokens":10}}`}}},
		{Name: "beta", WorkDir: "/repos/beta", Poller: &fakePoller{sum: backlog.Summary{Claimable: 1, RunConfigVersion: 1}},
			RCfg: &fakeRCfg{docs: map[int]string{1: `{"budget":{"max_tokens":10}}`}}},
	}
	logs := captureLogs(t)
	d := NewMulti(cfg, h, targets)
	openTestStateDir(t, d)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if h.invokeCalls != 2 {
		t.Errorf("both projects should drain once and stop; got %d Invoke calls", h.invokeCalls)
	}
	if out := logs.String(); !strings.Contains(out, "every project's budget is exhausted") {
		t.Errorf("the run did not report that every project was exhausted:\n%s", out)
	}
}

// The wall-clock arm of a stored ceiling runs on the TARGET's clock: a project
// pooled into the same daemon must not inherit the run's elapsed time as its
// own. (The defect: time.Since(start) was the run's clock.)
func TestTargetBudgetTrip_UsesTargetElapsedNotRunElapsed(t *testing.T) {
	cfg := fastCfg()
	d := New(cfg, &fakeAdapter{}, busyPoller())
	d.applyRunConfig(0, &d.targets[0], &plane.RunConfigState{Version: 1, Config: []byte(
		`{"budget":{"max_wall_clock":3600}}`)})
	if d.targets[0].Cfg == nil {
		t.Fatal("the stored budget was refused; want the overlay in force")
	}

	// The target started a moment ago, while the RUN has been up for two hours
	// (its first drain came late). The stored ceiling is an hour.
	d.tgtStart[0] = time.Now()
	if v := d.targetBudgetTrip(0, 2*time.Hour); v.dim != "" {
		t.Errorf("targetBudgetTrip = %q, want no trip: the target's own clock is seconds old", v.dim)
	}

	d.tgtStart[0] = time.Now().Add(-2 * time.Hour)
	v := d.targetBudgetTrip(0, 2*time.Hour)
	if !strings.Contains(v.dim, "wall clock") {
		t.Errorf("targetBudgetTrip = %q, want the stored wall-clock ceiling named once the TARGET elapsed it", v.dim)
	}
}
