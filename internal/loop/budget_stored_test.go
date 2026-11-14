package loop

// Stored run-config budget enforcement (CLA-480). A ratified document's budget
// governs its own project's spend: its per_harness blocks are live, a looser
// ceiling it sets actually raises that project's ceiling, and a project that
// exhausts its ceiling stops draining without taking its siblings down.

import (
	"context"
	"fmt"
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

// --- exhaustForWait: the limit-reset-past-ceiling arm (review 1.review-1.1) ---

// exhaustForWait is a state transition of its own: it must exhaust the account it
// NAMES, not every project and not none. The overlaid arm governs that one target
// and records the mid-drain stop so Run keeps the siblings going.
func TestExhaustForWait_OverlaidTargetExhaustsOnlyItself(t *testing.T) {
	cfg := fastCfg()
	d := NewMulti(cfg, &fakeAdapter{}, []Target{
		{Name: "alpha", WorkDir: "/repos/alpha"},
		{Name: "beta", WorkDir: "/repos/beta"},
	})
	d.applyRunConfig(0, &d.targets[0], &plane.RunConfigState{Version: 1, Config: []byte(
		`{"budget":{"max_wall_clock":3600}}`)})
	if !d.overlayBudgetAt(0) {
		t.Fatal("the stored budget was refused; want the overlay in force")
	}

	d.exhaustForWait(0, "the stated reset is past the remaining budget")

	if !d.exhaustedAt(0) || !d.budgetStoppedAt(0) {
		t.Errorf("overlaid target: exhausted=%t budgetStopped=%t, want true/true", d.exhaustedAt(0), d.budgetStoppedAt(0))
	}
	if d.exhaustedAt(1) || d.budgetStoppedAt(1) {
		t.Errorf("sibling: exhausted=%t budgetStopped=%t, want false/false — an overlaid target's wait must not exhaust the other project", d.exhaustedAt(1), d.budgetStoppedAt(1))
	}
}

// The local arm is the opposite scope: every target the local file governs is
// exhausted together, and a target whose document SET a budget keeps draining.
func TestExhaustForWait_LocalPoolExhaustsTheLocalTargetsOnly(t *testing.T) {
	cfg := fastCfg()
	d := NewMulti(cfg, &fakeAdapter{}, []Target{
		{Name: "alpha", WorkDir: "/repos/alpha"},
		{Name: "beta", WorkDir: "/repos/beta"},
		{Name: "gamma", WorkDir: "/repos/gamma"},
	})
	d.applyRunConfig(2, &d.targets[2], &plane.RunConfigState{Version: 1, Config: []byte(
		`{"budget":{"max_tokens":1000}}`)})
	if !d.overlayBudgetAt(2) {
		t.Fatal("the stored budget was refused; want the overlay in force")
	}

	d.exhaustForWait(0, "the stated reset is past the remaining budget")

	for i, want := range []bool{true, true, false} {
		if got := d.exhaustedAt(i); got != want {
			t.Errorf("target %d exhausted = %t, want %t", i, got, want)
		}
	}
	if !d.budgetStoppedAt(0) {
		t.Error("the local trip did not record that THIS drain ended on the budget")
	}
}

// The same arm end to end: a stored wall-clock reset landing past the target's
// remaining ceiling ends THAT project's drain, and Run keeps its sibling draining.
func TestRun_ExhaustForWait_OverlaidTargetLeavesTheSiblingDraining(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 4
	limited := okResult(0, 0)
	limited.Raw = map[string]any{"kind": "limit"}
	h := &fakeAdapter{
		// The first drain is beta (the rotation starts at the next target);
		// alpha's is the limited one this test is about.
		steps:        []invokeStep{{res: okResult(0, 0)}, {res: limited}},
		limitResetAt: time.Now().Add(3 * time.Hour),
	}
	targets := []Target{
		{Name: "alpha", WorkDir: "/repos/alpha", Poller: &fakePoller{sum: backlog.Summary{Claimable: 1, RunConfigVersion: 1}},
			RCfg: &fakeRCfg{docs: map[int]string{1: `{"budget":{"max_wall_clock":1800}}`}}},
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

	alpha, beta, _ := invocationsByWorkDir(t, h)
	if alpha != 1 || beta != 3 {
		t.Errorf("drains alpha=%d beta=%d, want 1 and 3 — the reset-past-ceiling arm must exhaust only alpha (max-iterations 4)", alpha, beta)
	}
	if !d.exhaustedAt(0) {
		t.Error("alpha was not marked exhausted")
	}
	if d.exhaustedAt(1) {
		t.Error("beta was exhausted; the overlaid target's wait must not take the sibling down")
	}
	if out := logs.String(); !strings.Contains(out, "the stated reset is past the remaining budget") {
		t.Errorf("the exhaust-for-wait arm was not logged:\n%s", out)
	}
}

// The local arm end to end: the trip exhausts every locally governed project,
// and a stored-budget sibling keeps draining.
func TestRun_ExhaustForWait_LocalPoolLeavesTheOverlaidSiblingDraining(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 4
	cfg.Budget = config.Budget{MaxWallClock: config.Duration(30 * time.Minute)} // the local account's clock
	limited := okResult(0, 0)
	limited.Raw = map[string]any{"kind": "limit"}
	h := &fakeAdapter{steps: []invokeStep{{res: limited}}, limitResetAt: time.Now().Add(3 * time.Hour)}
	targets := []Target{
		{Name: "alpha", WorkDir: "/repos/alpha", Poller: &fakePoller{sum: backlog.Summary{Claimable: 1, RunConfigVersion: 1}},
			RCfg: &fakeRCfg{docs: map[int]string{1: `{"budget":{"max_tokens":1000}}`}}},
		{Name: "beta", WorkDir: "/repos/beta", Poller: &fakePoller{sum: backlog.Summary{Claimable: 1}}},
		{Name: "gamma", WorkDir: "/repos/gamma", Poller: &fakePoller{sum: backlog.Summary{Claimable: 1}}},
	}
	logs := captureLogs(t)
	d := NewMulti(cfg, h, targets)
	openTestStateDir(t, d)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	alpha, beta, gamma := invocationsByWorkDir(t, h)
	if alpha != 3 || beta != 1 || gamma != 0 {
		t.Errorf("drains alpha=%d beta=%d gamma=%d, want 3/1/0 — the local arm exhausts beta and gamma together and leaves alpha draining", alpha, beta, gamma)
	}
	if !d.exhaustedAt(1) || !d.exhaustedAt(2) {
		t.Errorf("local targets exhausted = %t/%t, want true/true", d.exhaustedAt(1), d.exhaustedAt(2))
	}
	if d.exhaustedAt(0) {
		t.Error("the overlaid sibling was exhausted by the local account's wait")
	}
	if out := logs.String(); !strings.Contains(out, "the projects under the local budget are exhausted") {
		t.Errorf("the local exhaust-for-wait arm was not logged:\n%s", out)
	}
}

// --- local-pool scope in a mixed run (review 1.review-1.3) --------------------

// A local-pool trip must exhaust only the projects the local file governs, and
// leave an overlaid sibling draining. Before this test every local trip was
// single-target or all-local, so a regression that also exhausted the overlaid
// sibling would have passed.
func TestRun_LocalPoolTripLeavesAnOverlaidSiblingDraining(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 4
	cfg.Budget = config.Budget{MaxTokens: 10} // trips on alpha's first drain
	h := &fakeAdapter{name: "claude", tokens: 100}
	targets := []Target{
		{Name: "alpha", WorkDir: "/repos/alpha", Poller: &fakePoller{sum: backlog.Summary{Claimable: 1}}},
		{Name: "beta", WorkDir: "/repos/beta", Poller: &fakePoller{sum: backlog.Summary{Claimable: 1, RunConfigVersion: 1}},
			RCfg: &fakeRCfg{docs: map[int]string{1: `{"budget":{"max_tokens":1000}}`}}},
	}
	logs := captureLogs(t)
	d := NewMulti(cfg, h, targets)
	openTestStateDir(t, d)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	alpha, beta, _ := invocationsByWorkDir(t, h)
	// beta drains first (rotation), then alpha's first drain trips the pool.
	if alpha != 1 || beta != 3 {
		t.Errorf("drains alpha=%d beta=%d, want 1 and 3 — the local trip must exhaust alpha and leave the overlaid beta draining (max-iterations 4)", alpha, beta)
	}
	if !d.exhaustedAt(0) {
		t.Error("the local target was not exhausted")
	}
	if d.exhaustedAt(1) {
		t.Error("the overlaid sibling was exhausted by the local account's trip")
	}
	if out := logs.String(); strings.Contains(out, "every project's budget is exhausted") {
		t.Errorf("a local-pool trip was reported as a run-wide stop while the overlaid sibling had budget:\n%s", out)
	}
}

// The mirror: an overlaid target's trip leaves a locally governed sibling
// draining — this time with a local ceiling configured, so the local branch
// exists and must not fire.
func TestRun_OverlaidTripLeavesALocalSiblingDraining(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 4
	cfg.Budget = config.Budget{MaxTokens: 1000} // the local sibling has headroom
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

	alpha, beta, _ := invocationsByWorkDir(t, h)
	if alpha != 1 || beta != 3 {
		t.Errorf("drains alpha=%d beta=%d, want 1 and 3 — alpha's stored trip must leave the local sibling draining (max-iterations 4)", alpha, beta)
	}
	if !d.exhaustedAt(0) {
		t.Error("the overlaid target was not exhausted")
	}
	if d.exhaustedAt(1) {
		t.Error("the local sibling was exhausted by the overlaid target's trip")
	}
	if out := logs.String(); strings.Contains(out, "the local budget governs") {
		t.Errorf("the overlaid target's trip was reported as the local account's:\n%s", out)
	}
}

// --- a document that sets no budget stays in the local pool (review 1.review-1.2) ---

// A model-only document must not take its project out of the instance-wide
// pool: the README's exception is a document that sets a budget. Two projects
// spending 50 each against a local 75-token ceiling must trip it after the
// second drain, not be judged as two separate 75-token accounts.
func TestRun_ModelOnlyOverlayStaysInTheLocalPool(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 6
	cfg.Budget = config.Budget{MaxTokens: 75}
	h := &fakeAdapter{name: "claude", tokens: 50}
	targets := []Target{
		{Name: "alpha", WorkDir: "/repos/alpha", Poller: &fakePoller{sum: backlog.Summary{Claimable: 1, RunConfigVersion: 1}},
			RCfg: &fakeRCfg{docs: map[int]string{1: `{"model":"plane-1"}`}}},
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

	if d.targets[0].Cfg == nil {
		t.Fatal("the stored document was refused; want the overlay in force")
	}
	if d.overlayBudgetAt(0) {
		t.Error("a document that sets no budget was recorded as the target's own budget account")
	}
	if h.invokeCalls != 2 {
		t.Errorf("spawned %d sessions, want 2 — the two projects share the local 75-token pool, so 50+50 trips it (separate 75-token accounts would allow 6)", h.invokeCalls)
	}
	if !d.exhaustedAt(0) || !d.exhaustedAt(1) {
		t.Errorf("local-pool targets exhausted = %t/%t, want true/true", d.exhaustedAt(0), d.exhaustedAt(1))
	}
	if out := logs.String(); !strings.Contains(out, "every project's budget is exhausted") {
		t.Errorf("the pooled ceiling was not reported as exhausted:\n%s", out)
	}
}

// --- unmeasurable spend is account-scoped too (review 1.review-1.4) ----------

// A session stream that cannot be read breaks only the ceiling that was
// protecting it: alpha's stored budget, so alpha is exhausted and beta drains on.
func TestRun_UntrustedSessionExhaustsOnlyTheOverlaidTarget(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 4
	h := &fakeAdapter{steps: []invokeStep{{res: okResult(0, 0)}, {res: untrustedResult(10, 0.5)}}}
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

	alpha, beta, _ := invocationsByWorkDir(t, h)
	if alpha != 1 || beta != 3 {
		t.Errorf("drains alpha=%d beta=%d, want 1 and 3 — alpha's unmeasurable session must exhaust alpha only (max-iterations 4)", alpha, beta)
	}
	if !d.exhaustedAt(0) {
		t.Error("alpha was not exhausted after its unmeasurable session")
	}
	if d.exhaustedAt(1) {
		t.Error("beta was exhausted by alpha's unmeasurable session")
	}
	if out := logs.String(); !strings.Contains(out, "spend cannot be read") {
		t.Errorf("the account-scoped unmeasurable stop was not logged:\n%s", out)
	}
}

// The supervised wait's unreadable probe follows the same scope.
func TestRun_UnreadableProbeExhaustsOnlyTheOverlaidTarget(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 4
	limited := okResult(0, 0)
	limited.Raw = map[string]any{"kind": "limit"}
	h := &fakeAdapter{
		steps:        []invokeStep{{res: okResult(0, 0)}, {res: limited}},
		limitResetAt: time.Now().Add(3 * time.Hour),
		probeErrs:    []error{fmt.Errorf("%w: probe stdout could not be read to the end", harness.ErrUntrusted)},
	}
	targets := []Target{
		{Name: "alpha", WorkDir: "/repos/alpha", Poller: &fakePoller{sum: backlog.Summary{Claimable: 1, RunConfigVersion: 1}},
			RCfg: &fakeRCfg{docs: map[int]string{1: `{"budget":{"max_cost_usd":50}}`}}},
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

	alpha, beta, _ := invocationsByWorkDir(t, h)
	if alpha != 1 || beta != 3 {
		t.Errorf("drains alpha=%d beta=%d, want 1 and 3 — the unreadable probe must exhaust alpha only (max-iterations 4)", alpha, beta)
	}
	if !d.exhaustedAt(0) {
		t.Error("alpha was not exhausted after its unreadable probe")
	}
	if d.exhaustedAt(1) {
		t.Error("beta was exhausted by alpha's unreadable probe")
	}
	if out := logs.String(); !strings.Contains(out, "pause probe's spend cannot be read") {
		t.Errorf("the account-scoped probe stop was not logged:\n%s", out)
	}
}

// invocationsByWorkDir counts sessions per target workdir, failing on anything
// unexpected so a routing bug cannot read as a drain count.
func invocationsByWorkDir(t *testing.T, h *fakeAdapter) (alpha, beta, gamma int) {
	t.Helper()
	for _, inv := range h.invocations {
		switch inv.WorkDir {
		case "/repos/alpha":
			alpha++
		case "/repos/beta":
			beta++
		case "/repos/gamma":
			gamma++
		default:
			t.Fatalf("session ran in unexpected workdir %q", inv.WorkDir)
		}
	}
	return alpha, beta, gamma
}
