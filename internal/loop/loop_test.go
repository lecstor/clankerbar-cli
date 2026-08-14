package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

// openTestStateDir gives a Driver a real, opened state dir in a temp directory —
// what Run does for itself. Tests that call drainWithRetries directly need it,
// because that path writes a per-iteration log.
func openTestStateDir(t *testing.T, d *Driver) {
	t.Helper()
	st, err := statedir.Open(t.TempDir())
	if err != nil {
		t.Fatalf("statedir.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	d.state = st
}

// --- Fakes for the two seams the loop depends on: harness.Adapter and
// backlog.Poller. Both are injected via loop.New. The fakes model the real
// contract: the loop calls Invoke, then interrogates the returned Result via
// DetectLimit / IsTransient — exactly as the real adapters parse their own
// output. We encode the intended classification in Result.Raw["kind"] and let
// the fake's DetectLimit/IsTransient decode it, so a scripted Result and its
// classification never drift apart.

type invokeStep struct {
	res harness.Result
	err error
}

func okResult(tokens int, cost float64) harness.Result {
	return harness.Result{ExitCode: 0, Tokens: tokens, CostUSD: cost, Raw: map[string]any{"kind": "ok"}}
}
func transientResult() harness.Result {
	return harness.Result{ExitCode: 1, Raw: map[string]any{"kind": "transient"}}
}
func nonRetryableResult() harness.Result {
	return harness.Result{ExitCode: 2, Raw: map[string]any{"kind": "fail"}}
}
func limitResult() harness.Result {
	return harness.Result{ExitCode: 1, Raw: map[string]any{"kind": "limit"}}
}

// turnCappedResult is a session cut off by Invocation.MaxTurns: a NON-ZERO exit
// that is nonetheless an orderly end, which is the whole reason it needs its own
// classification rather than falling through to "non-retryable".
func turnCappedResult() harness.Result {
	return harness.Result{ExitCode: 1, Raw: map[string]any{"kind": "turnCapped"}}
}

// tokenCeilingResult is a session the ADAPTER killed mid-stream for crossing its
// per-session token ceiling (CLA-343): the same orderly-end shape as a turn cap
// — non-zero exit, its own classification, nothing retried and nothing failed.
func tokenCeilingResult() harness.Result {
	return harness.Result{ExitCode: -1, Tokens: 90_000_000, Raw: map[string]any{"kind": "tokenCeiling"}}
}
func limitStopResult() harness.Result {
	return harness.Result{ExitCode: 1, Raw: map[string]any{"kind": "limitStop"}}
}

func kindOf(r harness.Result) string {
	if r.Raw == nil {
		return ""
	}
	k, _ := r.Raw["kind"].(string)
	return k
}

type fakeAdapter struct {
	steps        []invokeStep
	invokeCalls  int
	invocations  []harness.Invocation // every Invoke's argument, for asserting routing (CLA-142)
	probeResults []harness.Limit
	// probeErrs is scripted by call index and falls back to NO error once exhausted,
	// like fakePoller's errs. A constant error would make a probe loop unbounded, so
	// a regression in the wait's exit conditions would hang the package instead of
	// failing it.
	probeErrs  []error
	probeCalls int
	// probeTokens/probeCost are what EACH probe reports spending. A probe is a real
	// paid session (CLA-287), so the fake has to be able to charge for one; zero
	// keeps every pre-existing test's arithmetic exactly as it was.
	probeTokens  int
	probeCost    float64
	limitResetAt time.Time
	// caps overrides the fake's default capabilities, for the tests that drive an
	// adapter which cannot observe a claim.
	caps *harness.Capabilities
	// tokens is charged per session when the steps are exhausted (the okResult
	// path). Zero keeps every pre-existing test's arithmetic exactly as it was;
	// the no-progress breaker tests set it so a fruitless session costs a real
	// ~30M and the token threshold is actually reachable (CLA-343).
	tokens int
}

func (f *fakeAdapter) Name() string { return "fake" }

// The fake carries the MCPConfigPath through to f.invocations for the delivery
// tests to assert on, so the Claude reading is the honest one to declare.
func (f *fakeAdapter) MCPConfigUse() harness.MCPConfigUse {
	return harness.MCPConfigUse{Schema: harness.MCPConfigClaudeJSON}
}

func (f *fakeAdapter) Invoke(ctx context.Context, in harness.Invocation) (harness.Result, error) {
	f.invocations = append(f.invocations, in)
	i := f.invokeCalls
	f.invokeCalls++
	if i < len(f.steps) {
		return f.steps[i].res, f.steps[i].err
	}
	// Steps exhausted → a clean success, so a loop that keeps draining does not
	// panic and terminates via some other ceiling (max-iterations / budget).
	return okResult(f.tokens, 0), nil
}

func (f *fakeAdapter) DetectLimit(r harness.Result) harness.Limit {
	switch kindOf(r) {
	case "limit":
		return harness.Limit{Limited: true, ResetAt: f.limitResetAt}
	case "limitStop":
		return harness.Limit{Limited: true, Stop: true, Reason: "out of credits"}
	}
	return harness.Limit{}
}

func (f *fakeAdapter) IsTransient(r harness.Result) bool { return kindOf(r) == "transient" }

// The fake tracks claims — the loop tests script Result.Claim directly — so it
// stands in for a claim-observing adapter and `phases` is allowed on it.
func (f *fakeAdapter) Capabilities() harness.Capabilities {
	if f.caps != nil {
		return *f.caps
	}
	return harness.Capabilities{TracksClaims: true, HonoursMaxTurns: true}
}

// Encoded in Raw like every other classification, so a scripted Result and how
// the driver reads it cannot drift apart.
func (f *fakeAdapter) TurnCapped(r harness.Result) bool { return kindOf(r) == "turnCapped" }

func (f *fakeAdapter) TokenCeilingHit(r harness.Result) bool { return kindOf(r) == "tokenCeiling" }

// Diagnostic stands in for a real adapter's scoped text. Stderr is where every
// adapter's scope starts, so returning it keeps the fake honest about what the
// driver is allowed to quote.
func (f *fakeAdapter) Diagnostic(r harness.Result) string { return r.Stderr }

func (f *fakeAdapter) Probe(ctx context.Context, in harness.Invocation) (harness.ProbeResult, error) {
	i := f.probeCalls
	f.probeCalls++
	// Charged on every path, including the error one. That is the loop's contract
	// with an adapter — whatever a ProbeResult carries is counted — not a claim
	// about what the real adapters manage to parse from a failed run (CLA-299).
	out := harness.ProbeResult{Tokens: f.probeTokens, CostUSD: f.probeCost}
	if i < len(f.probeErrs) && f.probeErrs[i] != nil {
		return out, f.probeErrs[i]
	}
	if i < len(f.probeResults) {
		out.Limit = f.probeResults[i]
		return out, nil
	}
	out.Limit = harness.Limit{Limited: false} // default: the limit has lifted
	return out, nil
}

func (f *fakeAdapter) ReadUsage(ctx context.Context, in harness.Invocation) (harness.Usage, error) {
	return harness.Usage{}, harness.ErrUsageUnsupported
}

type fakePoller struct {
	sum    backlog.Summary   // constant fallback once the script (if any) is exhausted
	err    error             // constant fallback error
	sums   []backlog.Summary // optional per-call script, indexed by call number
	errs   []error           // optional per-call errors, aligned with sums
	calls  int
	onCall func(i int) // hook fired at the start of each Poll (e.g. cancel ctx)
}

func (p *fakePoller) Poll(ctx context.Context) (backlog.Summary, error) {
	i := p.calls
	p.calls++
	if p.onCall != nil {
		p.onCall(i)
	}
	if i < len(p.sums) {
		var e error
		if i < len(p.errs) {
			e = p.errs[i]
		}
		return p.sums[i], e
	}
	return p.sum, p.err
}

// fastCfg is a config whose every wait interval is sub-millisecond, so no test
// ever sleeps for real. Crucially, RetryCap=1ms makes backoff() return the cap
// for every retry n (the 30s base is always > the cap), so transient retries do
// not incur the real 30s/60s/... backoff.
func fastCfg() *config.Config {
	return &config.Config{
		Harness:          "claude",
		Prompt:           "Work the backlog.",
		IdlePollInterval: config.Duration(time.Millisecond),
		PollInterval:     config.Duration(time.Millisecond),
		RetryCap:         config.Duration(time.Millisecond),
	}
}

// runLoop runs Run under a safety timeout so a control-flow regression that fails
// to terminate fails the test fast instead of hanging the suite.
func runLoop(t *testing.T, cfg *config.Config, h harness.Adapter, p backlog.Poller) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return New(cfg, h, p).Run(ctx)
}

// drainOnce is runLoop's counterpart for the tests that drive drainWithRetries
// directly: the same safety timeout, so a regression in one of the drain's exit
// conditions fails the test instead of hanging the package. The waits inside a
// drain are unbounded by design — a supervised wait polls for as long as the cap
// lasts — so nothing but the context bounds them if a script stops terminating.
func drainOnce(t *testing.T, d *Driver) (int, float64, bool, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return d.drainWithRetries(ctx, 1, d.targets[0], spend{start: time.Now()})
}

// ---------------------------------------------------------------------------
// Gate decision: whether a cheap backlog read is enough to spend a session.

func TestRun_GateDecision(t *testing.T) {
	t.Run("wired claimable zero idles and spawns nothing", func(t *testing.T) {
		cfg := fastCfg()
		cfg.StateDir = t.TempDir()
		h := &fakeAdapter{}
		// The poller cancels the run's context on its second call, so the idle
		// loop terminates without ever reaching a spawn.
		p := &fakePoller{sum: backlog.Summary{Ready: 3, Claimable: 0}}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p.onCall = func(i int) {
			if i >= 1 {
				cancel()
			}
		}
		if err := New(cfg, h, p).Run(ctx); err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if h.invokeCalls != 0 {
			t.Errorf("claimable==0 must not spawn a session; got %d Invoke calls", h.invokeCalls)
		}
		if p.calls < 2 {
			t.Errorf("expected the loop to keep polling while idle; got %d polls", p.calls)
		}
	})

	t.Run("wired claimable positive spawns a session", func(t *testing.T) {
		cfg := fastCfg()
		cfg.StateDir = t.TempDir()
		cfg.MaxIterations = 1 // stop after one drain so the test terminates
		h := &fakeAdapter{steps: []invokeStep{{res: okResult(0, 0)}}}
		p := &fakePoller{sum: backlog.Summary{Ready: 1, Claimable: 1}}
		if err := runLoop(t, cfg, h, p); err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if h.invokeCalls != 1 {
			t.Errorf("claimable>0 must spawn exactly one session; got %d", h.invokeCalls)
		}
	})

	t.Run("idle loop wakes and spawns when work appears", func(t *testing.T) {
		// The loop's whole reason to idle rather than exit: an empty queue that
		// later gains claimable work must produce a spawn on the next poll.
		cfg := fastCfg()
		cfg.StateDir = t.TempDir()
		cfg.MaxIterations = 1
		h := &fakeAdapter{steps: []invokeStep{{res: okResult(0, 0)}}}
		p := &fakePoller{sums: []backlog.Summary{
			{Ready: 2, Claimable: 0}, // first poll: idle, no spawn
			{Ready: 2, Claimable: 1}, // second poll: work appeared → spawn
		}}
		if err := runLoop(t, cfg, h, p); err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if h.invokeCalls != 1 {
			t.Errorf("loop should spawn once work appears; got %d Invoke calls", h.invokeCalls)
		}
		if p.calls != 2 {
			t.Errorf("expected exactly two polls (idle then wake); got %d", p.calls)
		}
	})

	t.Run("generic poll error is retried, not treated as blind mode", func(t *testing.T) {
		// A non-ErrNotWired poll error must back off and retry the poll (never
		// spawn, never flip to blind mode). The hook cancels after two polls.
		cfg := fastCfg()
		cfg.StateDir = t.TempDir()
		h := &fakeAdapter{}
		p := &fakePoller{err: errors.New("boom")}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p.onCall = func(i int) {
			if i >= 1 {
				cancel()
			}
		}
		if err := New(cfg, h, p).Run(ctx); err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if h.invokeCalls != 0 {
			t.Errorf("a poll error must not spawn a session; got %d Invoke calls", h.invokeCalls)
		}
		if p.calls < 2 {
			t.Errorf("a poll error must be retried; got %d polls", p.calls)
		}
	})

	t.Run("ErrNotWired enters blind mode: spawns then idles without polling again", func(t *testing.T) {
		cfg := fastCfg()
		cfg.StateDir = t.TempDir()
		cfg.MaxIterations = 1
		h := &fakeAdapter{steps: []invokeStep{{res: okResult(0, 0)}}}
		p := &fakePoller{err: backlog.ErrNotWired}
		if err := runLoop(t, cfg, h, p); err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if h.invokeCalls != 1 {
			t.Errorf("blind mode must still spawn a drain session; got %d Invoke calls", h.invokeCalls)
		}
		if p.calls != 1 {
			t.Errorf("blind mode must stop polling after the not-wired signal; got %d polls", p.calls)
		}
	})

	t.Run("ErrUnauthorized hard-stops: returns a non-nil error and spawns nothing", func(t *testing.T) {
		// A 401/403 (ErrUnauthorized) is a bad API key the harness sessions share, so
		// the loop must NOT blind-drain or idle-retry — it hard-stops with a non-nil
		// error (non-zero exit) and spawns no session (CLA-132).
		cfg := fastCfg()
		cfg.StateDir = t.TempDir()
		h := &fakeAdapter{}
		p := &fakePoller{err: backlog.ErrUnauthorized}
		err := runLoop(t, cfg, h, p)
		if err == nil {
			t.Fatal("ErrUnauthorized must make Run return a non-nil error (loud hard stop), got nil")
		}
		if !errors.Is(err, backlog.ErrUnauthorized) {
			t.Errorf("returned error should wrap backlog.ErrUnauthorized; got %v", err)
		}
		if !strings.Contains(err.Error(), "CLANKERBAR_API_KEY") {
			t.Errorf("hard-stop error should name CLANKERBAR_API_KEY as the cause; got %v", err)
		}
		if h.invokeCalls != 0 {
			t.Errorf("auth failure must spawn NO harness session; got %d Invoke calls", h.invokeCalls)
		}
		if p.calls != 1 {
			t.Errorf("auth failure must stop after the first poll, not idle-retry; got %d polls", p.calls)
		}
	})

	t.Run("ErrProjectRequired hard-stops: returns a non-nil error and spawns nothing", func(t *testing.T) {
		// A 400 project_required (ErrProjectRequired) means the key is account-scoped but
		// a project-scoped key is required; the harness sessions share that account key,
		// so the loop must NOT blind-drain or idle-retry — it hard-stops with a non-nil
		// error (non-zero exit) and spawns no session (CLA-133).
		cfg := fastCfg()
		cfg.StateDir = t.TempDir()
		h := &fakeAdapter{}
		p := &fakePoller{err: backlog.ErrProjectRequired}
		err := runLoop(t, cfg, h, p)
		if err == nil {
			t.Fatal("ErrProjectRequired must make Run return a non-nil error (loud hard stop), got nil")
		}
		if !errors.Is(err, backlog.ErrProjectRequired) {
			t.Errorf("returned error should wrap backlog.ErrProjectRequired; got %v", err)
		}
		if !strings.Contains(err.Error(), "project-scoped") {
			t.Errorf("hard-stop error should tell the operator to use a project-scoped key; got %v", err)
		}
		if h.invokeCalls != 0 {
			t.Errorf("project_required must spawn NO harness session; got %d Invoke calls", h.invokeCalls)
		}
		if p.calls != 1 {
			t.Errorf("project_required must stop after the first poll, not idle-retry; got %d polls", p.calls)
		}
	})
}

// ---------------------------------------------------------------------------
// A reset that lands past the run's wall-clock ceiling must not be waited out.
// The budget breaker only runs BETWEEN drains and the supervised wait sits inside
// one, so waiting produces the worst possible shape: sleep through the window,
// spend one session on the freshly reset quota, then stop on the very next check
// — headroom saved all night and then declined. A real run did exactly that,
// stopping eight minutes after an 8am reset having waited 5h31m for it.
func TestDrainWithRetries_ResetPastBudgetStopsInsteadOfWaiting(t *testing.T) {
	cfg := fastCfg()
	h := &fakeAdapter{
		steps:        []invokeStep{{res: limitResult()}, {res: okResult(7, 0)}},
		limitResetAt: time.Now().Add(3 * time.Hour),
	}
	// A 30m ceiling with the run just started: 30m of budget left, reset in 3h.
	cfg.Budget = config.Budget{MaxWallClock: config.Duration(30 * time.Minute)}
	d := New(cfg, h, &fakePoller{})
	openTestStateDir(t, d)

	_, _, stop, err := d.drainWithRetries(context.Background(), 1, d.targets[0], spend{start: time.Now()})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !stop {
		t.Error("a reset past the ceiling must stop the run, not wait it out")
	}
	if h.probeCalls != 0 {
		t.Errorf("stopping early must not enter the supervised wait; got %d probes", h.probeCalls)
	}
	if h.invokeCalls != 1 {
		t.Errorf("the session must not be re-run; got %d invokes", h.invokeCalls)
	}
}

// The mirror case: a reset INSIDE the ceiling is still waited out, because the
// wait is what lets an overnight run survive a rolling-window cap at all.
func TestDrainWithRetries_ResetInsideBudgetStillWaits(t *testing.T) {
	cfg := fastCfg()
	h := &fakeAdapter{
		steps:        []invokeStep{{res: limitResult()}, {res: okResult(7, 0)}},
		limitResetAt: time.Now().Add(time.Minute),
	}
	// A 4h ceiling with the run just started: the 1m reset fits easily.
	cfg.Budget = config.Budget{MaxWallClock: config.Duration(4 * time.Hour)}
	d := New(cfg, h, &fakePoller{})
	openTestStateDir(t, d)

	tokens, _, stop, err := d.drainWithRetries(context.Background(), 1, d.targets[0], spend{start: time.Now()})

	if err != nil || stop {
		t.Fatalf("a reset inside the ceiling must be waited out: stop=%v err=%v", stop, err)
	}
	if tokens != 7 {
		t.Errorf("the session should have been re-run after the wait; got %d tokens", tokens)
	}
}

// Both halves must be known before an early stop is justified: an unknown reset
// is waited out because the supervised wait polls for an EARLY lift, and with no
// ceiling set there is nothing to be past.
func TestWaitPastBudget(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name      string
		resetAt   time.Time
		remaining time.Duration
		bounded   bool
		wantOver  bool
	}{
		{"reset costs more than is left", now.Add(2 * time.Hour), time.Hour, true, true},
		{"reset fits inside what is left", now.Add(time.Hour), 2 * time.Hour, true, false},
		{"unknown reset is waited out", time.Time{}, time.Hour, true, false},
		{"no ceiling means nothing to be past", now.Add(2 * time.Hour), 0, false, false},
		{"an already-spent ceiling stops on any wait", now.Add(time.Minute), -time.Hour, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := waitPastBudget(tc.resetAt, tc.remaining, tc.bounded); got != tc.wantOver {
				t.Errorf("waitPastBudget = %v, want %v", got, tc.wantOver)
			}
		})
	}
}

// The decision must be taken on the SAME clock the budget breaker uses.
//
// Budget.Deadline keeps start's monotonic reading, and ExceededBy counts
// monotonic elapsed; a suspended machine advances the wall clock and freezes the
// monotonic one. Comparing a wall-clock reset against a wall-clock deadline
// therefore stopped runs the breaker would have allowed to continue, and blamed
// a ceiling that had not been reached. This pins the divergence directly.
func TestWaitPastBudget_ignoresWallClockDrift(t *testing.T) {
	const ceiling = 8 * time.Hour

	// A run that began 8h ago by the WALL clock, but spent 5h30m suspended, so the
	// breaker has only seen 2h30m of monotonic elapsed.
	elapsedMonotonic := 2*time.Hour + 30*time.Minute
	remaining, bounded := config.Budget{MaxWallClock: config.Duration(ceiling)}.Remaining(elapsedMonotonic)
	if !bounded {
		t.Fatal("a configured ceiling must report bounded")
	}

	// The quota returns in an hour - comfortably inside the 5h30m still budgeted.
	resetAt := time.Now().Add(time.Hour)
	if _, over := waitPastBudget(resetAt, remaining, bounded); over {
		t.Errorf("stopped a run with %s of budget left for a %s wait; the wall clock is not the breaker's clock",
			remaining.Round(time.Minute), time.Until(resetAt).Round(time.Minute))
	}
}

// drainWithRetries: transient retry / usage-limit wait / hard stop / genuine
// failures. Exercised directly on the method — its retry loop is the unit that
// must NOT advance the outer drain count, and testing it in isolation makes that
// structural fact assertable through Invoke-call counts.

// CLA-343: the unphased path must carry the resolved turn cap to the harness —
// this is the exact line the task changed (drainWithRetries used to build the
// phase inline with a zero MaxTurns), and no other test reads MaxTurns off an
// invocation this path produced. A revert of the EffectivePhases wiring would
// fail this test and nothing else.
func TestDrainWithRetries_CarriesTheResolvedTurnCap(t *testing.T) {
	t.Run("a bare config gets the built-in default", func(t *testing.T) {
		cfg := fastCfg()
		cfg.StateDir = t.TempDir()
		h := &fakeAdapter{}
		d := New(cfg, h, &fakePoller{})
		openTestStateDir(t, d)

		if _, _, _, err := drainOnce(t, d); err != nil {
			t.Fatalf("drainWithRetries: %v", err)
		}
		if got := h.invocations[0].MaxTurns; got != config.DefaultMaxTurns {
			t.Errorf("unphased invocation carries MaxTurns %d, want the default %d — an unphased run must never reach the harness uncapped", got, config.DefaultMaxTurns)
		}
	})

	t.Run("the top-level cap wins over the default", func(t *testing.T) {
		cfg := fastCfg()
		cfg.StateDir = t.TempDir()
		cfg.MaxTurns = 25
		h := &fakeAdapter{}
		d := New(cfg, h, &fakePoller{})
		openTestStateDir(t, d)

		if _, _, _, err := drainOnce(t, d); err != nil {
			t.Fatalf("drainWithRetries: %v", err)
		}
		if got := h.invocations[0].MaxTurns; got != 25 {
			t.Errorf("unphased invocation carries MaxTurns %d, want the top-level 25", got)
		}
	})
}

func TestDrainWithRetries(t *testing.T) {
	tests := []struct {
		name         string
		steps        []invokeStep
		probeResults []harness.Limit
		maxRetries   int
		wantTokens   int
		wantCost     float64
		wantStop     bool
		wantErr      string // substring; "" means no error
		wantInvokes  int
		wantProbes   int
	}{
		{
			name:        "transient failure retries the same session then succeeds",
			steps:       []invokeStep{{res: transientResult()}, {res: okResult(5, 0.5)}},
			wantTokens:  5,
			wantCost:    0.5,
			wantInvokes: 2,
		},
		{
			// Regression for finding #3: a failed+retried attempt still burned tokens,
			// and the budget breaker must see that spend. The transient attempt's
			// 100 tokens / $1.00 are counted alongside the clean re-run's 5 / $0.50.
			name: "budget breaker sees spend from a failed+retried attempt",
			steps: []invokeStep{
				{res: harness.Result{ExitCode: 1, Tokens: 100, CostUSD: 1.0, Raw: map[string]any{"kind": "transient"}}},
				{res: okResult(5, 0.5)},
			},
			wantTokens:  105,
			wantCost:    1.5,
			wantInvokes: 2,
		},
		{
			// A usage-limit attempt also spends before the supervised re-run, so its
			// 20 tokens / $0.50 must be counted alongside the clean re-run's 7 / $0.25.
			name: "budget breaker sees spend from a usage-limited attempt",
			steps: []invokeStep{
				{res: harness.Result{ExitCode: 1, Tokens: 20, CostUSD: 0.5, Raw: map[string]any{"kind": "limit"}}},
				{res: okResult(7, 0.25)},
			},
			wantTokens:  27,
			wantCost:    0.75,
			wantInvokes: 2,
			wantProbes:  1,
		},
		{
			name:        "non-transient non-zero exit is a genuine failure",
			steps:       []invokeStep{{res: nonRetryableResult()}},
			wantErr:     "non-retryable",
			wantInvokes: 1,
		},
		{
			name:        "invoke launch error returns immediately",
			steps:       []invokeStep{{err: errors.New("bad PATH")}},
			wantErr:     "invoke",
			wantInvokes: 1,
		},
		{
			name:        "rolling-window limit waits then re-runs the session",
			steps:       []invokeStep{{res: limitResult()}, {res: okResult(7, 0)}},
			wantTokens:  7,
			wantInvokes: 2,
			wantProbes:  1, // one supervised probe sees the limit lifted
		},
		{
			name:        "hard Limit.Stop stops the run cleanly with no probe",
			steps:       []invokeStep{{res: limitStopResult()}},
			wantStop:    true,
			wantInvokes: 1,
			wantProbes:  0,
		},
		{
			name:        "transient failures exhaust MaxRetries and give up",
			steps:       []invokeStep{{res: transientResult()}, {res: transientResult()}, {res: transientResult()}},
			maxRetries:  2,
			wantErr:     "transient failures persisted",
			wantInvokes: 3, // initial attempt + 2 retries
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fastCfg()
			cfg.MaxRetries = tc.maxRetries
			h := &fakeAdapter{steps: tc.steps, probeResults: tc.probeResults}
			d := New(cfg, h, &fakePoller{})
			openTestStateDir(t, d) // drainWithRetries writes per-iteration logs here

			tokens, cost, stop, err := d.drainWithRetries(context.Background(), 1, d.targets[0], spend{start: time.Now()})

			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
			}
			if tokens != tc.wantTokens {
				t.Errorf("tokens = %d, want %d", tokens, tc.wantTokens)
			}
			if cost != tc.wantCost {
				t.Errorf("cost = %v, want %v", cost, tc.wantCost)
			}
			if stop != tc.wantStop {
				t.Errorf("stop = %v, want %v", stop, tc.wantStop)
			}
			if h.invokeCalls != tc.wantInvokes {
				t.Errorf("Invoke calls = %d, want %d", h.invokeCalls, tc.wantInvokes)
			}
			if h.probeCalls != tc.wantProbes {
				t.Errorf("Probe calls = %d, want %d", h.probeCalls, tc.wantProbes)
			}
		})
	}
}

// The rolling-window limit path must also honour the stated reset: once the
// reset time has passed, the loop resumes without needing a probe to say so.
func TestDrainWithRetries_StatedResetPassed(t *testing.T) {
	cfg := fastCfg()
	h := &fakeAdapter{
		steps:        []invokeStep{{res: limitResult()}, {res: okResult(3, 0)}},
		limitResetAt: time.Now().Add(-time.Hour), // already past
	}
	d := New(cfg, h, &fakePoller{})
	openTestStateDir(t, d)

	tokens, _, stop, err := d.drainWithRetries(context.Background(), 1, d.targets[0], spend{start: time.Now()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stop {
		t.Errorf("a passed reset should resume, not stop")
	}
	if tokens != 3 {
		t.Errorf("tokens = %d, want 3", tokens)
	}
	if h.probeCalls != 0 {
		t.Errorf("a passed stated reset should resume without probing; got %d probes", h.probeCalls)
	}
}

// End-to-end confirmation that a transient retry stays a SINGLE drain: with
// MaxIterations=1, a transient-then-success drain (two Invoke calls) must still
// be counted as one iteration and stop. If the retry loop ever leaked into the
// drain count, this would either stop early (one Invoke) or over-run.
func TestRun_TransientRetryIsOneDrain(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 1
	h := &fakeAdapter{steps: []invokeStep{{res: transientResult()}, {res: okResult(1, 0)}}}
	p := &fakePoller{sum: backlog.Summary{Claimable: 1}}
	if err := runLoop(t, cfg, h, p); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if h.invokeCalls != 2 {
		t.Errorf("one drain with a transient retry should Invoke twice; got %d", h.invokeCalls)
	}
	if p.calls != 1 {
		t.Errorf("the retry must not trigger a re-poll; expected 1 poll, got %d", p.calls)
	}
}

// A non-transient invoke error surfaced through Run stops the loop with the
// error, confirming drainWithRetries' error propagates rather than being
// swallowed.
func TestRun_NonRetryableFailurePropagates(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	h := &fakeAdapter{steps: []invokeStep{{res: nonRetryableResult()}}}
	p := &fakePoller{sum: backlog.Summary{Claimable: 1}}
	err := runLoop(t, cfg, h, p)
	if err == nil {
		t.Fatal("expected Run to return the non-retryable failure")
	}
	if !strings.Contains(err.Error(), "non-retryable") {
		t.Errorf("error %q does not mention non-retryable", err.Error())
	}
}

// Stopping the whole run has to say WHAT it stopped on (CLA-268). An operator
// reading the terminal at 8am needs to tell "the flag is wrong" from "the
// classifier does not know this blip yet"; "exited 2 (non-retryable)" cannot.
func TestRun_NonRetryableFailureNamesTheText(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	res := nonRetryableResult()
	res.Stderr = "API Error: Something Nobody Classified Yet"
	h := &fakeAdapter{steps: []invokeStep{{res: res}}}
	p := &fakePoller{sum: backlog.Summary{Claimable: 1}}

	err := runLoop(t, cfg, h, p)
	if err == nil {
		t.Fatal("expected Run to return the non-retryable failure")
	}
	if !strings.Contains(err.Error(), "API Error: Something Nobody Classified Yet") {
		t.Errorf("error %q does not name the text that was judged non-retryable", err.Error())
	}
}

func TestFailureDetail(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// Nothing to say leaves the caller's message untouched — no dangling colon.
		{"empty", "", ""},
		{"only whitespace", "   \n\t ", ""},
		{"collapses to one line", "API Error: 503\n  Service Unavailable\n", ": API Error: 503 Service Unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := failureDetail(tc.in); got != tc.want {
				t.Errorf("failureDetail(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	t.Run("keeps the tail, bounded", func(t *testing.T) {
		// The head of a real diagnostic is startup noise identical on every
		// session; the thing that killed this one is said last.
		got := failureDetail(strings.Repeat("x", 1000) + " THE ACTUAL FAILURE")
		if !strings.Contains(got, "THE ACTUAL FAILURE") {
			t.Errorf("truncation dropped the tail: %q", got)
		}
		// Exactly ": " (2) + "..." (3) of overhead — asserted tight, so an
		// off-by-one in the slice is caught rather than absorbed by slack.
		if n, want := len([]rune(got)), failureDetailMax+5; n != want {
			t.Errorf("detail is %d runes, want exactly %d", n, want)
		}
	})

	// The diagnostic is rendered to a TTY now, not just matched against. A harness
	// writes stderr through verbatim, so an ESC sequence in it would be EXECUTED
	// by the terminal rather than shown — repainting or clearing the one line the
	// operator came back to read.
	t.Run("strips control bytes but keeps the text", func(t *testing.T) {
		got := failureDetail("API Error: \x1b[2J\x1b[1;1Hnothing to see \x07here")
		if strings.ContainsAny(got, "\x1b\x07") {
			t.Errorf("control bytes reached the log line: %q", got)
		}
		for _, want := range []string{"API Error:", "nothing to see", "here"} {
			if !strings.Contains(got, want) {
				t.Errorf("stripping ate real text (%q missing): %q", want, got)
			}
		}
	})

	t.Run("does not split a rune", func(t *testing.T) {
		// The harness's own text carries · in every usage-limit notice.
		got := failureDetail(strings.Repeat("·", 1000))
		if strings.ContainsRune(got, '�') {
			t.Errorf("truncation cut a rune in half: %q", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Budget breaker: each dimension independently stops the loop.

func TestRun_BudgetBreaker(t *testing.T) {
	t.Run("cumulative tokens", func(t *testing.T) {
		cfg := fastCfg()
		cfg.StateDir = t.TempDir()
		cfg.Budget = config.Budget{MaxTokens: 10}
		h := &fakeAdapter{steps: []invokeStep{{res: okResult(100, 0)}}}
		p := &fakePoller{sum: backlog.Summary{Claimable: 1}}
		if err := runLoop(t, cfg, h, p); err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if h.invokeCalls != 1 {
			t.Errorf("token budget should stop after one over-budget drain; got %d Invoke calls", h.invokeCalls)
		}
	})

	t.Run("cumulative cost", func(t *testing.T) {
		cfg := fastCfg()
		cfg.StateDir = t.TempDir()
		cfg.Budget = config.Budget{MaxCostUSD: 1.0}
		h := &fakeAdapter{steps: []invokeStep{{res: okResult(0, 2.0)}}}
		p := &fakePoller{sum: backlog.Summary{Claimable: 1}}
		if err := runLoop(t, cfg, h, p); err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if h.invokeCalls != 1 {
			t.Errorf("cost budget should stop after one over-budget drain; got %d Invoke calls", h.invokeCalls)
		}
	})

	t.Run("wall clock", func(t *testing.T) {
		cfg := fastCfg()
		cfg.StateDir = t.TempDir()
		cfg.Budget = config.Budget{MaxWallClock: config.Duration(time.Nanosecond)}
		h := &fakeAdapter{}
		p := &fakePoller{sum: backlog.Summary{Claimable: 1}}
		if err := runLoop(t, cfg, h, p); err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		// The wall-clock check sits before the poll gate, so an already-elapsed
		// budget stops the loop before it spends or even polls.
		if h.invokeCalls != 0 {
			t.Errorf("wall-clock budget should stop before spending; got %d Invoke calls", h.invokeCalls)
		}
		if p.calls != 0 {
			t.Errorf("wall-clock budget should stop before polling; got %d polls", p.calls)
		}
	})
}

// ---------------------------------------------------------------------------
// The budget breaker has to be reachable from INSIDE a drain (CLA-258).
//
// `waitPastBudget` covers exactly one case: a stated reset measured against a
// wall-clock ceiling. Everything else about a drain was budget-blind — a limit
// with no stated reset (codex never states one), a cost-only or token-only
// ceiling, and the transient retry loop, which with the documented default
// `max_retries: 0` never gives up. Each of those could spin all night spending
// real money while a ceiling the operator set sat inert one stack frame up.

func TestDrainWithRetries_CostCeilingEndsASupervisedWait(t *testing.T) {
	cfg := fastCfg()
	// A cost-only ceiling and NO wall clock: waitPastBudget cannot fire (nothing
	// to be past), so this is the hole it never covered. The limited attempt spends
	// $2 against a $1 ceiling.
	cfg.Budget = config.Budget{MaxCostUSD: 1.0}
	h := &fakeAdapter{
		steps: []invokeStep{
			{res: harness.Result{ExitCode: 1, CostUSD: 2.0, Raw: map[string]any{"kind": "limit"}}},
			{res: okResult(0, 0)}, // must never be reached
		},
		// The limit does NOT lift. This is the stuck wait the bar names: without a
		// stated reset there is nothing for waitPastBudget to measure, so before
		// this the loop probed here for as long as the cap lasted, with the ceiling
		// one stack frame up. (Finite so a regression fails rather than hangs.)
		probeResults: []harness.Limit{{Limited: true}, {Limited: true}, {Limited: true}},
	}
	d := New(cfg, h, &fakePoller{})
	openTestStateDir(t, d)

	_, cost, stop, err := d.drainWithRetries(context.Background(), 1, d.targets[0], spend{start: time.Now()})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !stop {
		t.Error("a blown cost ceiling must end the drain, not wait out the limit")
	}
	if cost != 2.0 {
		t.Errorf("cost = %v, want the limited attempt's 2.0 reported back to Run", cost)
	}
	if h.invokeCalls != 1 {
		t.Errorf("the session must not be re-run past the ceiling; got %d invokes", h.invokeCalls)
	}
	if h.probeCalls != 0 {
		t.Errorf("a ceiling already reached must end the wait before it polls; got %d probes", h.probeCalls)
	}
}

// The mirror: a ceiling still in credit must not cut a legitimate pause short.
func TestDrainWithRetries_UnreachedCeilingStillWaitsOutTheLimit(t *testing.T) {
	cfg := fastCfg()
	cfg.Budget = config.Budget{MaxCostUSD: 10.0}
	h := &fakeAdapter{steps: []invokeStep{
		{res: harness.Result{ExitCode: 1, CostUSD: 1.0, Raw: map[string]any{"kind": "limit"}}},
		{res: okResult(4, 0)},
	}}
	d := New(cfg, h, &fakePoller{})
	openTestStateDir(t, d)

	tokens, _, stop, err := d.drainWithRetries(context.Background(), 1, d.targets[0], spend{start: time.Now()})

	if err != nil || stop {
		t.Fatalf("$1 of a $10 ceiling must still be waited out: stop=%v err=%v", stop, err)
	}
	if tokens != 4 {
		t.Errorf("the session should have been re-run after the wait; got %d tokens", tokens)
	}
	if h.probeCalls != 1 {
		t.Errorf("the supervised wait should have probed once; got %d", h.probeCalls)
	}
}

// ---------------------------------------------------------------------------
// A probe is a paid session, and its spend has to reach the accumulator the
// breaker reads (CLA-287).
//
// CLA-258 put the breaker inside the supervised wait, but the wait can only be
// ended by spend the breaker can SEE. Every adapter implements Probe as
// Invoke-then-DetectLimit — a real session against the harness binary — and threw
// away the tokens and cost it reported. So a cap that lasts a week, polled every
// 30 minutes, was ~336 sessions no ceiling could count, in the one loop with no
// other way out.
//
// Both cases below set NO wall clock, so waitPastBudget cannot fire, and the limit
// NEVER lifts. The limited attempt itself spends nothing: every dollar and every
// token in these tests arrives from probes alone.

func TestSupervisedWait_ProbeSpendCrossesACeilingAndEndsTheWait(t *testing.T) {
	t.Run("cost-only ceiling", func(t *testing.T) {
		cfg := fastCfg()
		cfg.Budget = config.Budget{MaxCostUSD: 0.5}
		h := &fakeAdapter{
			// The limited attempt is FREE, so nothing but probe spend can trip the
			// ceiling. The clean re-run behind it must never be reached.
			steps:     []invokeStep{{res: limitResult()}, {res: okResult(0, 0)}},
			probeCost: 0.25,
			// The limit never lifts. Finite so a regression fails rather than hangs,
			// and longer than the run needs so exhaustion cannot rescue the test by
			// falling through to the fake's "lifted" default.
			probeResults: []harness.Limit{
				{Limited: true}, {Limited: true}, {Limited: true},
				{Limited: true}, {Limited: true},
			},
		}
		d := New(cfg, h, &fakePoller{})
		openTestStateDir(t, d)

		_, cost, stop, err := drainOnce(t, d)

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !stop {
			t.Error("probe spend past the cost ceiling must end the wait, not poll on")
		}
		if h.probeCalls != 2 {
			t.Errorf("two $0.25 probes reach a $0.50 ceiling; got %d probes", h.probeCalls)
		}
		if cost != 0.5 {
			t.Errorf("cost = %v, want the probes' 0.5 reported back to Run's accumulator", cost)
		}
		if h.invokeCalls != 1 {
			t.Errorf("the session must not be re-run past the ceiling; got %d invokes", h.invokeCalls)
		}
	})

	t.Run("token-only ceiling", func(t *testing.T) {
		cfg := fastCfg()
		cfg.Budget = config.Budget{MaxTokens: 10}
		h := &fakeAdapter{
			steps:       []invokeStep{{res: limitResult()}, {res: okResult(0, 0)}},
			probeTokens: 5,
			probeResults: []harness.Limit{
				{Limited: true}, {Limited: true}, {Limited: true},
				{Limited: true}, {Limited: true},
			},
		}
		d := New(cfg, h, &fakePoller{})
		openTestStateDir(t, d)

		tokens, _, stop, err := drainOnce(t, d)

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !stop {
			t.Error("probe spend past the token ceiling must end the wait, not poll on")
		}
		if h.probeCalls != 2 {
			t.Errorf("two 5-token probes reach a 10-token ceiling; got %d probes", h.probeCalls)
		}
		if tokens != 10 {
			t.Errorf("tokens = %d, want the probes' 10 reported back to Run's accumulator", tokens)
		}
		if h.invokeCalls != 1 {
			t.Errorf("the session must not be re-run past the ceiling; got %d invokes", h.invokeCalls)
		}
	})
}

// The other half of "the SAME accumulator": a wait that ends normally still has to
// hand its probe spend back, or the ceiling between drains is under-counted by
// every probe the run ever made — including the runs that never trip one.
func TestSupervisedWait_ProbeSpendIsReportedEvenWhenTheLimitLifts(t *testing.T) {
	cfg := fastCfg()
	cfg.Budget = config.Budget{MaxCostUSD: 10.0} // nowhere near reached
	h := &fakeAdapter{
		steps:     []invokeStep{{res: limitResult()}, {res: okResult(0, 0.5)}},
		probeCost: 0.25,
		// Default (no script): the first probe sees the limit lifted.
	}
	d := New(cfg, h, &fakePoller{})
	openTestStateDir(t, d)

	_, cost, stop, err := drainOnce(t, d)

	if err != nil || stop {
		t.Fatalf("an unreached ceiling must resume: stop=%v err=%v", stop, err)
	}
	if h.probeCalls != 1 {
		t.Errorf("expected one probe to see the limit lift; got %d", h.probeCalls)
	}
	if cost != 0.75 {
		t.Errorf("cost = %v, want the probe's 0.25 plus the re-run's 0.50", cost)
	}
}

// The wait counts whatever a ProbeResult carries, error or not — so a harness
// failing every poll cannot be the cheapest-looking loop in the driver and the
// most expensive one to run.
//
// This pins the LOOP's side of the contract, deliberately, and not the adapters':
// today all three parse nothing before returning a run error, so what they report
// there is zero (CLA-299). This is what stops the loop from being the thing that
// drops it once they do.
func TestSupervisedWait_ReportedSpendCountsOnAFailedProbe(t *testing.T) {
	cfg := fastCfg()
	cfg.Budget = config.Budget{MaxCostUSD: 0.5}
	h := &fakeAdapter{
		steps:     []invokeStep{{res: limitResult()}, {res: okResult(0, 0)}},
		probeCost: 0.25,
		// Two failures reach the ceiling. The script then runs out, so a regression
		// falls through to the fake's "limit lifted" default and fails this test
		// rather than polling forever.
		probeErrs: []error{errors.New("harness exploded"), errors.New("harness exploded")},
	}
	d := New(cfg, h, &fakePoller{})
	openTestStateDir(t, d)

	_, cost, stop, err := drainOnce(t, d)

	if err != nil {
		t.Fatalf("a probe error is retried next interval, not returned: %v", err)
	}
	if !stop {
		t.Error("spend from FAILED probes must reach the ceiling too, not loop forever")
	}
	if h.probeCalls != 2 {
		t.Errorf("two $0.25 failed probes reach a $0.50 ceiling; got %d probes", h.probeCalls)
	}
	if cost != 0.5 {
		t.Errorf("cost = %v, want the failed probes' 0.5 counted", cost)
	}
}

func TestDrainWithRetries_TokenCeilingEndsATransientRetry(t *testing.T) {
	cfg := fastCfg()
	cfg.Budget = config.Budget{MaxTokens: 10} // no wall clock, no MaxRetries — unbounded before this
	h := &fakeAdapter{steps: []invokeStep{
		{res: harness.Result{ExitCode: 1, Tokens: 100, Raw: map[string]any{"kind": "transient"}}},
		{res: okResult(0, 0)}, // must never be reached
	}}
	d := New(cfg, h, &fakePoller{})
	openTestStateDir(t, d)

	_, _, stop, err := d.drainWithRetries(context.Background(), 1, d.targets[0], spend{start: time.Now()})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !stop {
		t.Error("a blown token ceiling must end the retry loop")
	}
	if h.invokeCalls != 1 {
		t.Errorf("the session must not be retried past the ceiling; got %d invokes", h.invokeCalls)
	}
}

// The ceilings are CUMULATIVE across the whole run, so a drain must be measured
// against what the run has already spent — not only against its own attempts.
// Spend that Run is holding is what stops the very first wait of a late drain.
func TestDrainWithRetries_CeilingCountsSpendFromEarlierDrains(t *testing.T) {
	cfg := fastCfg()
	cfg.Budget = config.Budget{MaxCostUSD: 10.0}
	h := &fakeAdapter{steps: []invokeStep{
		{res: harness.Result{ExitCode: 1, CostUSD: 1.0, Raw: map[string]any{"kind": "limit"}}},
		{res: okResult(0, 0)}, // must never be reached
	}}
	d := New(cfg, h, &fakePoller{})
	openTestStateDir(t, d)

	// $9.50 already spent by earlier drains; this one's $1 crosses the $10 ceiling.
	_, _, stop, err := d.drainWithRetries(context.Background(), 4, d.targets[0], spend{start: time.Now(), cost: 9.5})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !stop {
		t.Error("the drain must count the run's earlier spend against the ceiling")
	}
	if h.invokeCalls != 1 {
		t.Errorf("got %d invokes, want 1", h.invokeCalls)
	}
}

// End to end through Run: the wiring that hands a drain the run's ceilings, and
// takes its mid-drain stop as a graceful end of the run.
func TestRun_MidDrainBudgetStopEndsTheRun(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.Budget = config.Budget{MaxCostUSD: 1.0}
	h := &fakeAdapter{steps: []invokeStep{
		{res: harness.Result{ExitCode: 1, CostUSD: 5.0, Raw: map[string]any{"kind": "limit"}}},
	}}
	p := &fakePoller{sum: backlog.Summary{Claimable: 1}}
	if err := runLoop(t, cfg, h, p); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if h.invokeCalls != 1 {
		t.Errorf("the run must stop on the first over-budget drain; got %d invokes", h.invokeCalls)
	}
}

// ---------------------------------------------------------------------------
// Text that arrives FROM THE BACKLOG must not be readable as a usage limit.
//
// The adapter-level pinning lives in internal/harness/injection_test.go; this is
// the end-to-end shape it protects, driven through the REAL claude classifiers:
// a clean session that merely quoted a poisoned task body must finish the drain,
// not send the driver to sleep and re-spawn the same paid session.

// realClassifierAdapter scripts Invoke and Probe while leaving DetectLimit and
// IsTransient to the genuine adapter, so the loop's reading of a real stream is
// what is under test.
type realClassifierAdapter struct {
	harness.Adapter
	steps       []invokeStep
	invokeCalls int
	probeCalls  int
}

func (a *realClassifierAdapter) Invoke(context.Context, harness.Invocation) (harness.Result, error) {
	i := a.invokeCalls
	a.invokeCalls++
	if i < len(a.steps) {
		return a.steps[i].res, a.steps[i].err
	}
	return okResult(0, 0), nil
}

func (a *realClassifierAdapter) Probe(context.Context, harness.Invocation) (harness.ProbeResult, error) {
	a.probeCalls++
	return harness.ProbeResult{}, nil // the limit that never was has "lifted"
}

func TestDrainWithRetries_BacklogTextCannotFakeALimit(t *testing.T) {
	real, err := harness.Get("claude")
	if err != nil {
		t.Fatalf("harness.Get(claude): %v", err)
	}

	// A session that claimed CLA-258 and quoted its body back — the body says
	// "hit your", "usage limit" and "api error: 500", because that is what the
	// task is about — then finished cleanly.
	const poison = "You've hit your session limit · resets 9:40pm — `usage limit` and `api error: 500`"
	quoted, merr := json.Marshal(poison)
	if merr != nil {
		t.Fatalf("marshal: %v", merr)
	}
	stream := strings.Join([]string{
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":` + string(quoted) + `}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":` + string(quoted) + `}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":` + string(quoted) + `,"total_cost_usd":0.5}`,
	}, "\n") + "\n"

	h := &realClassifierAdapter{Adapter: real, steps: []invokeStep{
		{res: harness.Result{ExitCode: 0, Stdout: stream, CostUSD: 0.5, Raw: map[string]any{"terminal_reason": ""}}},
		{res: okResult(0, 0)}, // a re-spawn would land here — it must not happen
	}}
	d := New(fastCfg(), h, &fakePoller{})
	openTestStateDir(t, d)

	_, _, stop, err := d.drainWithRetries(context.Background(), 1, d.targets[0], spend{start: time.Now()})

	if err != nil || stop {
		t.Fatalf("a clean session must finish the drain: stop=%v err=%v", stop, err)
	}
	if h.invokeCalls != 1 {
		t.Errorf("the session was re-spawned %d times over a limit that never happened", h.invokeCalls-1)
	}
	if h.probeCalls != 0 {
		t.Errorf("the driver entered a supervised wait on faked backlog text; got %d probes", h.probeCalls)
	}
}

// ---------------------------------------------------------------------------
// Control markers and the max-iterations ceiling.

func TestRun_Markers(t *testing.T) {
	t.Run("STOP stops gracefully and is consumed", func(t *testing.T) {
		cfg := fastCfg()
		dir := t.TempDir()
		cfg.StateDir = dir
		writeMarker(t, dir, "STOP", "please stop")
		h := &fakeAdapter{}
		p := &fakePoller{sum: backlog.Summary{Claimable: 1}}
		if err := runLoop(t, cfg, h, p); err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if h.invokeCalls != 0 {
			t.Errorf("STOP should stop before spending; got %d Invoke calls", h.invokeCalls)
		}
		if _, err := os.Stat(filepath.Join(dir, "STOP")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("STOP marker should be removed after a graceful stop; stat err = %v", err)
		}
	})

	t.Run("HALT stops and is left in place", func(t *testing.T) {
		cfg := fastCfg()
		dir := t.TempDir()
		cfg.StateDir = dir
		writeMarker(t, dir, "HALT", "wedged — needs a human")
		h := &fakeAdapter{}
		p := &fakePoller{sum: backlog.Summary{Claimable: 1}}
		if err := runLoop(t, cfg, h, p); err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if h.invokeCalls != 0 {
			t.Errorf("HALT should stop before spending; got %d Invoke calls", h.invokeCalls)
		}
		if _, err := os.Stat(filepath.Join(dir, "HALT")); err != nil {
			t.Errorf("HALT marker must be left in place for the operator; stat err = %v", err)
		}
	})

	t.Run("max-iterations stops after N drains", func(t *testing.T) {
		cfg := fastCfg()
		cfg.StateDir = t.TempDir()
		cfg.MaxIterations = 3
		h := &fakeAdapter{} // steps exhausted → clean successes forever
		p := &fakePoller{sum: backlog.Summary{Claimable: 1}}
		if err := runLoop(t, cfg, h, p); err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if h.invokeCalls != 3 {
			t.Errorf("max-iterations=3 should spawn exactly 3 sessions; got %d", h.invokeCalls)
		}
	})
}

// ---------------------------------------------------------------------------
// Console pause (CLA-130): loopPaused on the cheap read stops the loop spawning
// new sessions WITHOUT exiting — it idle-polls until the flag clears, then resumes.
// Distinct from STOP (exits) and from an empty queue (pause holds even with work).

func TestRun_ConsolePause(t *testing.T) {
	t.Run("paused with claimable work spawns nothing and keeps polling", func(t *testing.T) {
		// The load-bearing behaviour: pause is ordered BEFORE the claimable gate, so a
		// paused loop never spawns even though there IS claimable work. The poller
		// cancels the ctx on its second poll so the idling loop terminates.
		cfg := fastCfg()
		cfg.StateDir = t.TempDir()
		h := &fakeAdapter{}
		p := &fakePoller{sum: backlog.Summary{Ready: 5, Claimable: 5, Paused: true}}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p.onCall = func(i int) {
			if i >= 1 {
				cancel()
			}
		}
		if err := New(cfg, h, p).Run(ctx); err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if h.invokeCalls != 0 {
			t.Errorf("a paused loop must not spawn even with claimable work; got %d Invoke calls", h.invokeCalls)
		}
		if p.calls < 2 {
			t.Errorf("a paused loop must keep idle-polling (not exit); got %d polls", p.calls)
		}
	})

	t.Run("clearing the pause resumes and spawns", func(t *testing.T) {
		cfg := fastCfg()
		cfg.StateDir = t.TempDir()
		cfg.MaxIterations = 1 // stop after the first spawn so the test terminates
		h := &fakeAdapter{steps: []invokeStep{{res: okResult(0, 0)}}}
		p := &fakePoller{sums: []backlog.Summary{
			{Ready: 1, Claimable: 1, Paused: true},  // paused: no spawn despite work
			{Ready: 1, Claimable: 1, Paused: false}, // resumed: spawn
		}}
		if err := runLoop(t, cfg, h, p); err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if h.invokeCalls != 1 {
			t.Errorf("clearing the pause must resume and spawn exactly once; got %d Invoke calls", h.invokeCalls)
		}
		if p.calls != 2 {
			t.Errorf("expected exactly two polls (paused then resumed); got %d", p.calls)
		}
	})
}

// The queue line is a bar clause in its own right: an operator watching the console
// has to be able to tell "spawning because there is fresh work" from "spawning to
// recover an abandoned branch". Before CLA-274 the second did not happen, so the
// line did not have to distinguish them; now it does, and a count that only reaches
// the gate would leave the two indistinguishable on screen.
func TestRun_QueueLineReportsTheStaleCount(t *testing.T) {
	var logged strings.Builder
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })

	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 1
	h := &fakeAdapter{steps: []invokeStep{{res: okResult(0, 0)}}}
	p := &fakePoller{sum: backlog.Summary{Ready: 0, Claimable: 0, InProgress: 2, StaleClaimable: 2}}

	if err := runLoop(t, cfg, h, p); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got := logged.String(); !strings.Contains(got, "claimable=0 stale_claimable=2") {
		t.Errorf("the queue line must report the stale count beside claimable; got:\n%s", got)
	}
}

// The deadlock CLA-274 closes, end to end through Run. A project whose ready queue
// has emptied while it holds an abandoned branch used to be wedged shut: the gate
// read `claimable == 0` and spawned nothing, nothing called next_task, and the
// sweep that would have released or offered that branch runs ONLY inside next_task.
// The gate prevented the one call that could clear it, so the branch was stranded
// for good.
func TestRun_SpawnsToRecoverAbandonedWIPWithAnEmptyReadyQueue(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 1
	h := &fakeAdapter{steps: []invokeStep{{res: okResult(0, 0)}}}
	// Nothing ready at all; one abandoned branch the plane would offer for takeover.
	p := &fakePoller{sum: backlog.Summary{Ready: 0, Claimable: 0, InProgress: 1, StaleClaimable: 1}}

	if err := runLoop(t, cfg, h, p); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if h.invokeCalls != 1 {
		t.Errorf("offerable abandoned WIP must earn a recovery session; got %d Invoke calls", h.invokeCalls)
	}
}

// The other side of it, and the reason the count is a separate field rather than
// added into `claimable`: a target with nothing ready AND nothing offerable is
// genuinely idle and must still cost nothing. `in_progress` on its own is not the
// signal — that counts work in flight as well as work abandoned, which is exactly
// why the sweep's own verdict is what the plane sends.
func TestRun_StillIdlesWhenThereIsNeitherReadyNorAbandonedWork(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	h := &fakeAdapter{}
	p := &fakePoller{sum: backlog.Summary{Ready: 0, Claimable: 0, InProgress: 2, StaleClaimable: 0}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p.onCall = func(i int) {
		if i >= 1 {
			cancel()
		}
	}

	if err := New(cfg, h, p).Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if h.invokeCalls != 0 {
		t.Errorf("an idle target must still spawn nothing; got %d Invoke calls", h.invokeCalls)
	}
	if p.calls < 2 {
		t.Errorf("and it must keep idle-polling rather than exiting; got %d polls", p.calls)
	}
}

// The console pause outranks the widened gate exactly as it outranks the original
// one. Pause is ordered BEFORE the gate, so this holds by construction — pinned
// because "recovery work is different" is precisely the argument someone would
// later make for letting it through, and the operator's pause must mean no
// sessions, not no ORDINARY sessions.
func TestRun_ConsolePauseOutranksAbandonedWIP(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	h := &fakeAdapter{}
	p := &fakePoller{sum: backlog.Summary{Ready: 0, Claimable: 0, StaleClaimable: 3, Paused: true}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p.onCall = func(i int) {
		if i >= 1 {
			cancel()
		}
	}

	if err := New(cfg, h, p).Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if h.invokeCalls != 0 {
		t.Errorf("a paused loop must not spawn a recovery session either; got %d Invoke calls", h.invokeCalls)
	}
}

// The judgeProgress interaction the task flagged, pinned rather than assumed. A
// target spawning purely to recover abandoned WIP is no longer IDLE — Spawnable()
// is what the breaker reads to mean idle, and it is now true — so the breaker
// charges those drains and backs the target off after enough fruitless spend
// (quietTokenThreshold, CLA-343), instead of forgetting the count every poll.
//
// This is the case where the branch STAYS offerable across the drains — a session
// that declined the takeover, or a second abandoned branch behind the first. There
// the strikes accumulate and the breaker bites, which is what this pins.
//
// It is deliberately not the whole story, and the sibling test below is the other
// half: once a session actually TAKES a branch over, the lease renews and the count
// does not climb. What bounds that is the plane, not this breaker.
func TestJudgeProgressChargesRecoveryDrainsRatherThanReadingThemAsIdle(t *testing.T) {
	d := New(fastCfg(), &fakeAdapter{}, &fakePoller{})
	recovering := backlog.Summary{Claimable: 0, InProgress: 1, StaleClaimable: 1}

	// Two fruitless recovery drains, then a settled poll with the branch still
	// abandoned. THIS is the discriminating step: before the widening the summary
	// read as not-spawnable, so the breaker forgot the count on every such poll and
	// an unfinishable branch could spawn for ever. Now the target is spawnable, so
	// the strikes stand. Each drain spends 10M, so two of them sit well under the
	// 80M threshold.
	for range 2 {
		d.pending[0] = true
		d.spent[0] = 10_000_000
		d.judgeProgress(0, recovering)
	}
	d.judgeProgress(0, recovering)
	if d.quietTokens[0] != 20_000_000 {
		t.Fatalf("a target still holding recoverable WIP is not idle, so its strikes must stand; quietTokens=%d, want 20000000", d.quietTokens[0])
	}

	// So a third fruitless drain big enough to cross the threshold backs it off,
	// rather than the count being reset to zero underneath it every poll.
	d.pending[0] = true
	d.spent[0] = 60_000_000
	d.judgeProgress(0, recovering)
	if !d.backedOff(0) {
		t.Error("an unfinishable abandoned branch must back the target off rather than spawning for ever")
	}

	// And once the branch is gone, the target is idle on the old terms again and
	// forgets, so the recovery episode does not follow it into later work.
	d.judgeProgress(0, backlog.Summary{})
	if d.quietTokens[0] != 0 || !d.skipUntil[0].IsZero() {
		t.Errorf("with the branch cleared the target is idle again; quietTokens=%d skipUntil=%v", d.quietTokens[0], d.skipUntil[0])
	}
}

// The half the local breaker does NOT bound, asserted rather than left to be
// discovered on a live run. A takeover RENEWS the branch's lease, so the task stops
// being offerable and `stale_claimable` drops to 0 — and the settled poll before
// the lease lapses again reads as idle and forgets the strike. So `quietTokens`
// oscillates and never reaches quietTokenThreshold, however many times the loop
// retries one unfinishable branch.
//
// That is not an oversight to fix here. The bound is the PLANE's: it counts
// hand-offs per task and parks a branch that has exhausted its allowance, raising a
// question instead of offering it again, at which point it leaves `stale_claimable`
// for good. A local bound would mean suppressing the idle reset while work is in
// progress, which is precisely the narrowing CLA-249 weighed and rejected — it
// reinstates an immortal `quietTokens` accumulator for every project holding
// abandoned WIP.
//
// This test exists so that if someone later "fixes" the oscillation locally, they
// have to delete a test whose comment tells them why not.
func TestJudgeProgressDoesNotAccumulateStrikesAcrossTakeoversOfOneBranch(t *testing.T) {
	d := New(fastCfg(), &fakeAdapter{}, &fakePoller{})
	offered := backlog.Summary{Claimable: 0, InProgress: 1, StaleClaimable: 1}
	// The lease is fresh again because a session took the branch over: still one
	// task in progress, but nothing offerable, so nothing to spawn for.
	takenOver := backlog.Summary{Claimable: 0, InProgress: 1, StaleClaimable: 0}

	for range quietThreshold + 2 {
		d.pending[0] = true
		d.spent[0] = 30_000_000       // each fruitless drain is a real 30M session
		d.judgeProgress(0, takenOver) // the verdict on a drain that settled nothing
		d.judgeProgress(0, takenOver) // settled poll, lease still live -> reads idle
		d.judgeProgress(0, offered)   // lease lapsed, offered again
	}

	if d.quietTokens[0] != 0 {
		t.Errorf("the strike is forgotten while the lease is live, so the accumulator cannot climb; quietTokens=%d, want 0", d.quietTokens[0])
	}
	if d.backedOff(0) {
		t.Error("and the local breaker therefore never bites here — the plane's reclaim bound is what stops this")
	}
}

func writeMarker(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("writing %s marker: %v", name, err)
	}
}

// ---------------------------------------------------------------------------
// Multi-project (CLA-142): one instance, one account key, many queues.

// runLoopMulti runs a multi-target Run under a safety timeout.
func runLoopMulti(t *testing.T, cfg *config.Config, h harness.Adapter, targets []Target) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return NewMulti(cfg, h, targets).Run(ctx)
}

func TestRun_MultiProject(t *testing.T) {
	t.Run("drains the claimable project in ITS workdir, not the paused one", func(t *testing.T) {
		// alpha has more claimable work but is console-paused; beta must be the one
		// drained, and the session must spawn in beta's workdir with beta's .mcp.json
		// — the pause is per project, never instance-wide.
		cfg := fastCfg()
		cfg.StateDir = t.TempDir()
		cfg.MaxIterations = 1
		h := &fakeAdapter{}
		targets := []Target{
			{Name: "alpha", Poller: &fakePoller{sum: backlog.Summary{Claimable: 5, Paused: true}}, WorkDir: "/repos/alpha", MCPConfigPath: "/repos/alpha/.mcp.json"},
			{Name: "beta", Poller: &fakePoller{sum: backlog.Summary{Claimable: 1}}, WorkDir: "/repos/beta", MCPConfigPath: "/repos/beta/.mcp.json"},
		}
		if err := runLoopMulti(t, cfg, h, targets); err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if h.invokeCalls != 1 {
			t.Fatalf("want exactly one session; got %d", h.invokeCalls)
		}
		if got := h.invocations[0].WorkDir; got != "/repos/beta" {
			t.Errorf("session ran in %q, want the claimable project's workdir /repos/beta", got)
		}
		if got := h.invocations[0].MCPConfigPath; got != "/repos/beta/.mcp.json" {
			t.Errorf("session got mcp config %q, want beta's", got)
		}
	})

	t.Run("round-robins across projects that both have claimable work", func(t *testing.T) {
		// Neither queue may starve the other: successive drains must alternate.
		cfg := fastCfg()
		cfg.StateDir = t.TempDir()
		cfg.MaxIterations = 4
		h := &fakeAdapter{}
		targets := []Target{
			{Name: "alpha", Poller: &fakePoller{sum: backlog.Summary{Claimable: 9}}, WorkDir: "/repos/alpha"},
			{Name: "beta", Poller: &fakePoller{sum: backlog.Summary{Claimable: 9}}, WorkDir: "/repos/beta"},
		}
		if err := runLoopMulti(t, cfg, h, targets); err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if h.invokeCalls != 4 {
			t.Fatalf("want 4 sessions; got %d", h.invokeCalls)
		}
		var seq []string
		for _, inv := range h.invocations {
			seq = append(seq, inv.WorkDir)
		}
		for i := 1; i < len(seq); i++ {
			if seq[i] == seq[i-1] {
				t.Fatalf("drains did not alternate between projects: %v", seq)
			}
		}
	})

	t.Run("all projects idle: no sessions, keeps polling every queue", func(t *testing.T) {
		cfg := fastCfg()
		cfg.StateDir = t.TempDir()
		h := &fakeAdapter{}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		alpha := &fakePoller{sum: backlog.Summary{Ready: 2, Claimable: 0}}
		beta := &fakePoller{sum: backlog.Summary{Claimable: 0}}
		// End the run once both queues have been polled at least twice.
		beta.onCall = func(i int) {
			if i >= 1 {
				cancel()
			}
		}
		targets := []Target{
			{Name: "alpha", Poller: alpha},
			{Name: "beta", Poller: beta},
		}
		if err := NewMulti(cfg, h, targets).Run(ctx); err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if h.invokeCalls != 0 {
			t.Errorf("idle queues must not spawn; got %d sessions", h.invokeCalls)
		}
		if alpha.calls < 2 || beta.calls < 2 {
			t.Errorf("every queue must keep being polled while idle; got alpha=%d beta=%d", alpha.calls, beta.calls)
		}
	})

	t.Run("a paused project resumes draining once the flag clears", func(t *testing.T) {
		cfg := fastCfg()
		cfg.StateDir = t.TempDir()
		cfg.MaxIterations = 1
		h := &fakeAdapter{}
		alpha := &fakePoller{
			sums: []backlog.Summary{{Claimable: 2, Paused: true}},
			sum:  backlog.Summary{Claimable: 2}, // pause cleared from the second poll on
		}
		targets := []Target{
			{Name: "alpha", Poller: alpha, WorkDir: "/repos/alpha"},
			{Name: "beta", Poller: &fakePoller{sum: backlog.Summary{Claimable: 0}}},
		}
		if err := runLoopMulti(t, cfg, h, targets); err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if h.invokeCalls != 1 {
			t.Fatalf("want one session after the pause cleared; got %d", h.invokeCalls)
		}
		if got := h.invocations[0].WorkDir; got != "/repos/alpha" {
			t.Errorf("session ran in %q, want /repos/alpha", got)
		}
	})

	t.Run("project_required from any queue is still a loud hard stop with the new remedy", func(t *testing.T) {
		cfg := fastCfg()
		cfg.StateDir = t.TempDir()
		h := &fakeAdapter{}
		targets := []Target{
			{Name: "alpha", Poller: &fakePoller{err: backlog.ErrProjectRequired}},
		}
		err := runLoopMulti(t, cfg, h, targets)
		if err == nil {
			t.Fatal("want a non-nil (non-zero-exit) error on project_required")
		}
		if !errors.Is(err, backlog.ErrProjectRequired) {
			t.Errorf("error should wrap ErrProjectRequired; got %v", err)
		}
		// The remedy is a project selector, never "switch to a project key"
		// (decision 2026-07-29).
		if !strings.Contains(err.Error(), "projects") || !strings.Contains(err.Error(), "/mcp/<slug>") {
			t.Errorf("error should point at the projects config / slug remedy; got %q", err.Error())
		}
		if strings.Contains(err.Error(), "set CLANKERBAR_API_KEY to a project key") {
			t.Errorf("error must not tell the operator to abandon the account key; got %q", err.Error())
		}
		if h.invokeCalls != 0 {
			t.Errorf("must not spawn doomed sessions; got %d", h.invokeCalls)
		}
	})
}

func TestRun_MultiProject_ThreeTargetRotation(t *testing.T) {
	// The scan must skip a non-candidate BETWEEN the cursor and the next claimable
	// target — with gamma idle, drains rotate alpha↔charlie without ever landing on
	// gamma or getting stuck.
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 4
	h := &fakeAdapter{}
	targets := []Target{
		{Name: "alpha", Poller: &fakePoller{sum: backlog.Summary{Claimable: 9}}, WorkDir: "/repos/alpha"},
		{Name: "gamma", Poller: &fakePoller{sum: backlog.Summary{Claimable: 0}}, WorkDir: "/repos/gamma"},
		{Name: "charlie", Poller: &fakePoller{sum: backlog.Summary{Claimable: 9}}, WorkDir: "/repos/charlie"},
	}
	if err := runLoopMulti(t, cfg, h, targets); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	var seq []string
	for _, inv := range h.invocations {
		seq = append(seq, inv.WorkDir)
	}
	want := []string{"/repos/charlie", "/repos/alpha", "/repos/charlie", "/repos/alpha"}
	if len(seq) != len(want) {
		t.Fatalf("drain sequence %v, want %v", seq, want)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("drain sequence %v, want %v (idle gamma must be skipped, never starve the rest)", seq, want)
		}
	}
}

func TestRun_MultiProject_PollErrorOnOneQueueStillDrainsSibling(t *testing.T) {
	// The multi-project win over the old single-poller flow: a transient poll error
	// on one queue must not idle the whole instance — a sibling with claimable work
	// is drained in the same cycle.
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 1
	h := &fakeAdapter{}
	targets := []Target{
		{Name: "alpha", Poller: &fakePoller{err: errors.New("boom: 502")}},
		{Name: "beta", Poller: &fakePoller{sum: backlog.Summary{Claimable: 1}}, WorkDir: "/repos/beta"},
	}
	if err := runLoopMulti(t, cfg, h, targets); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if h.invokeCalls != 1 {
		t.Fatalf("want the healthy queue drained despite the sibling's poll error; got %d sessions", h.invokeCalls)
	}
	if got := h.invocations[0].WorkDir; got != "/repos/beta" {
		t.Errorf("session ran in %q, want /repos/beta", got)
	}
}

// ---------------------------------------------------------------------------
// Handing back a claim the session was still holding (CLA-242).

type releaseCall struct{ taskID, runID string }

type fakeReleaser struct {
	calls  []releaseCall
	err    error
	onCall func() // fired inside Release, to observe what has NOT happened yet
}

func (f *fakeReleaser) Release(ctx context.Context, taskID, runID string) error {
	f.calls = append(f.calls, releaseCall{taskID, runID})
	if f.onCall != nil {
		f.onCall()
	}
	return f.err
}

// held is a scripted Result carrying the claim a session ended still holding.
func held(base harness.Result, c harness.Claim) harness.Result {
	base.Claim = c
	return base
}

func openClaim() harness.Claim { return harness.Claim{TaskID: "t-1", RunID: "r-1"} }

// runWithReleaser drives one unnamed target that has both a poller and a releaser.
func runWithReleaser(t *testing.T, cfg *config.Config, h harness.Adapter, p backlog.Poller, r *fakeReleaser) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return NewMulti(cfg, h, []Target{{Poller: p, Releaser: r}}).Run(ctx)
}

func busyPoller() *fakePoller {
	return &fakePoller{sum: backlog.Summary{Ready: 1, Claimable: 1}}
}

// The case this whole change exists for: the driver knows it is about to sleep
// for as long as the usage limit takes, so the claim must be handed back FIRST.
// A real run slept ninety minutes on a live lease and spent a reclaim for it.
func TestRun_ReleasesBeforeSleepingOutAUsageLimit(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 1

	h := &fakeAdapter{steps: []invokeStep{{res: held(limitResult(), openClaim())}}}
	rel := &fakeReleaser{}
	// Probing is the first thing supervisedWait does after its initial pause, so
	// zero probes at release time proves the handback came before the sleep.
	rel.onCall = func() {
		if h.probeCalls != 0 {
			t.Errorf("released AFTER the supervised wait began (%d probes already); the lease was unattended for the whole pause", h.probeCalls)
		}
	}

	if err := runWithReleaser(t, cfg, h, busyPoller(), rel); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(rel.calls) != 1 {
		t.Fatalf("expected exactly one release, got %d: %+v", len(rel.calls), rel.calls)
	}
	if rel.calls[0] != (releaseCall{"t-1", "r-1"}) {
		t.Errorf("released %+v, want {t-1 r-1}", rel.calls[0])
	}
}

func TestRun_ReleaseHeldClaim(t *testing.T) {
	tests := []struct {
		name string
		res  harness.Result
		want []releaseCall
	}{
		{
			name: "a session that ended still holding the task hands it back",
			res:  held(okResult(10, 0.1), openClaim()),
			want: []releaseCall{{"t-1", "r-1"}},
		},
		{
			name: "a session that handed its task to review releases nothing",
			res:  held(okResult(10, 0.1), harness.Claim{TaskID: "t-1", RunID: "r-1", Settled: true}),
			want: nil,
		},
		{
			name: "a session that claimed nothing releases nothing",
			res:  okResult(10, 0.1),
			want: nil,
		},
		{
			// Releasing to `ready` would discard requiresTakeover and strand the
			// pushed branch, so the driver takes the expiry instead.
			name: "a session holding PUSHED work is left to expire",
			res:  held(okResult(10, 0.1), harness.Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true}),
			want: nil,
		},
		{
			name: "a session that died non-retryably still hands its task back",
			res:  held(nonRetryableResult(), openClaim()),
			want: []releaseCall{{"t-1", "r-1"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := fastCfg()
			cfg.StateDir = t.TempDir()
			cfg.MaxIterations = 1

			h := &fakeAdapter{steps: []invokeStep{{res: tt.res}}}
			rel := &fakeReleaser{}
			// A non-retryable exit legitimately returns an error; the assertion
			// under test is what was released, not whether the run survived.
			_ = runWithReleaser(t, cfg, h, busyPoller(), rel)

			if len(rel.calls) != len(tt.want) {
				t.Fatalf("release calls = %+v, want %+v", rel.calls, tt.want)
			}
			for i := range tt.want {
				if rel.calls[i] != tt.want[i] {
					t.Errorf("release[%d] = %+v, want %+v", i, rel.calls[i], tt.want[i])
				}
			}
		})
	}
}

// No-progress breaker: `claimable > 0` claims work is AVAILABLE, not that it can
// be DONE. A task gated on an unanswered question is claimable and unworkable, so
// the gate spawns, the session correctly declines, and the operator pays for the
// same report every cycle — ten times, in the run this was written for.

func TestRun_BacksOffAfterFruitlessDrains(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 10
	// Every session succeeds cleanly, but each costs a real ~30M tokens...
	h := &fakeAdapter{tokens: 30_000_000}
	// ...and nothing ever reaches a reviewer or finishes, which is the shape of a
	// queue whose only claimable task is waiting on the operator.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lastInvokes := 0
	p := &fakePoller{
		sum: backlog.Summary{Ready: 1, Claimable: 1},
		onCall: func(i int) {
			// End the run once the back-off has stopped it from spawning: the
			// assertion is about the session count, and the 15-minute quiet
			// wait that follows is not what this test is for. Two consecutive
			// polls with no new session means the loop is backed off; in a
			// regression that keeps spawning, invokeCalls keeps advancing and
			// the run falls through to the 5s deadline, failing the assertion
			// below exactly as it would have today.
			if i > 0 && h.invokeCalls == lastInvokes {
				cancel()
			}
			lastInvokes = h.invokeCalls
		},
	}

	if err := New(cfg, h, p).Run(ctx); err != nil && ctx.Err() == nil {
		t.Fatalf("Run returned error: %v", err)
	}

	// Three sessions spend 90M, crossing the 80M token threshold; the 15-minute
	// back-off that follows cannot be skipped by any fast-config interval — so the
	// run cannot reach its 10 iterations (the poller's cancel above ends the run
	// while it is sitting the wait out). Same calibration as the old
	// three-fruitless-sessions rule at ~26M each (CLA-343).
	if h.invokeCalls != quietThreshold {
		t.Errorf("should stop spawning after %d fruitless drains; got %d sessions", quietThreshold, h.invokeCalls)
	}
}

// The mirror: a queue that keeps settling work must never be backed off, or the
// breaker would throttle exactly the runs that are working.
func TestRun_ProgressResetsTheBreaker(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 6
	h := &fakeAdapter{}
	// Settled climbs on every poll — each drain delivered something.
	p := &fakePoller{sums: []backlog.Summary{
		{Claimable: 1, Done: 0}, {Claimable: 1, Done: 1}, {Claimable: 1, Done: 2},
		{Claimable: 1, Done: 3}, {Claimable: 1, Done: 4}, {Claimable: 1, Done: 5},
	}, sum: backlog.Summary{Claimable: 1, Done: 6}}

	if err := runLoop(t, cfg, h, p); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if h.invokeCalls != 6 {
		t.Errorf("a productive queue must never be backed off; got %d of 6 sessions", h.invokeCalls)
	}
}

// Work settling while nothing is outstanding means someone else moved it — the
// operator merging a PR, another machine's loop. Whatever was stuck may now be
// unstuck, so the target comes straight back rather than serving out its wait.
func TestJudgeProgressClearsBackOffOnOutsideProgress(t *testing.T) {
	d := New(fastCfg(), &fakeAdapter{}, &fakePoller{})
	d.quietTokens[0] = quietTokenThreshold
	d.skipUntil[0] = time.Now().Add(time.Hour)
	d.baseline[0] = 4

	// Claimable stays > 0 so this exercises the outside-progress path and not the
	// idle reset below it — with nothing claimable the back-off would clear for a
	// different reason and this test would pass without testing anything.
	d.judgeProgress(0, backlog.Summary{Claimable: 1, Done: 5})

	if d.quietTokens[0] != 0 || !d.skipUntil[0].IsZero() {
		t.Errorf("outside progress must clear the back-off; quietTokens=%d skipUntil=%v", d.quietTokens[0], d.skipUntil[0])
	}
}

// The back-off's own message asks the operator to go and answer the question that
// is stopping the queue. Answering it takes the task `blocked -> ready`, which
// moves neither half of `Settled()`, so before CLA-248 the operator did exactly
// what they were told, within seconds, and the target still served out the wait.
//
// Driven on the SECOND of two targets, so a baseline that crossed indices would
// show up here rather than passing by luck on the only slice entry there is.
func TestJudgeProgressClearsBackOffWhenAQuestionIsAnswered(t *testing.T) {
	d := NewMulti(fastCfg(), &fakeAdapter{}, []Target{{Poller: &fakePoller{}}, {Poller: &fakePoller{}}})
	d.quietTokens[1] = quietTokenThreshold
	d.skipUntil[1] = time.Now().Add(time.Hour)
	d.baseline[1], d.openQs[1] = 4, 2

	// Nothing settled: Settled() is exactly what it was. The only thing that moved
	// is the answer the message asked for. Claimable stays > 0 so this exercises the
	// answered-question path rather than the idle reset above it — with nothing
	// claimable the back-off clears for a different reason entirely.
	d.judgeProgress(1, backlog.Summary{Claimable: 1, Done: 4, OpenQuestions: 1})

	if d.quietTokens[1] != 0 || !d.skipUntil[1].IsZero() {
		t.Errorf("an answered question must clear the back-off; quietTokens=%d skipUntil=%v", d.quietTokens[1], d.skipUntil[1])
	}
	if d.backedOff(1) {
		t.Error("the target must be eligible on that same poll, not once the remaining wait elapses")
	}
	if d.openQs[1] != 1 {
		t.Errorf("the open-question baseline must track the poll; got %d, want 1", d.openQs[1])
	}
	if d.openQs[0] != 0 || d.quietTokens[0] != 0 {
		t.Errorf("the sibling target must be untouched; openQs=%d quietTokens=%d", d.openQs[0], d.quietTokens[0])
	}
}

// The mirror. Only a FALL is the operator acting; anything else must leave a
// backed-off target sitting out, or the breaker clears itself on ordinary noise.
func TestJudgeProgressKeepsBackOffWhenQuestionsDoNotFall(t *testing.T) {
	for _, tc := range []struct {
		name    string
		wasOpen int
		sum     backlog.Summary
	}{
		// Claimable stays > 0 throughout. Not a tidy-up: once CLA-249's idle branch
		// landed, a poll with nothing claimable clears the back-off on its own
		// account, so at 0 all three cases assert the inverse of what now happens and
		// go RED. The summary has to be spawnable for "back-off must survive" to be a
		// claim about the question-fall path at all.
		{"a NEW question is not an answered one", 2, backlog.Summary{Claimable: 1, Done: 4, OpenQuestions: 3}},
		{"an unchanged count says nothing happened", 2, backlog.Summary{Claimable: 1, Done: 4, OpenQuestions: 2}},
		{"none open, none answered", 0, backlog.Summary{Claimable: 1, Done: 4, OpenQuestions: 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := New(fastCfg(), &fakeAdapter{}, &fakePoller{})
			until := time.Now().Add(time.Hour)
			d.quietTokens[0], d.skipUntil[0] = quietTokenThreshold, until
			d.baseline[0], d.openQs[0] = 4, tc.wasOpen

			d.judgeProgress(0, tc.sum)

			if d.quietTokens[0] != quietTokenThreshold || !d.skipUntil[0].Equal(until) {
				t.Errorf("back-off must survive; quietTokens=%d skipUntil=%v", d.quietTokens[0], d.skipUntil[0])
			}
		})
	}
}

// CLA-343: the back-off is denominated in TOKENS, so ONE huge fruitless drain
// trips it — the property the session count could never give. The old rule
// counted 3 fruitless SESSIONS (~78M at the measured ~26M each), so a single
// runaway drain could repeat that spend without ever climbing the count: the
// 285.9M session would have needed nine more like it before the first sit-out.
// A single fruitless drain at or above quietTokenThreshold must earn the base
// 15m on its first verdict. (Fails against the session-count behaviour, which
// would need three such drains.)
func TestJudgeProgressOneHugeFruitlessDrainTripsTheBackoff(t *testing.T) {
	d := New(fastCfg(), &fakeAdapter{}, &fakePoller{})
	d.pending[0] = true
	d.spent[0] = quietTokenThreshold

	d.judgeProgress(0, backlog.Summary{Claimable: 1})

	if !d.backedOff(0) {
		t.Fatal("one fruitless drain at the token threshold must back the target off — the old session count needed three of them")
	}
	if got := time.Until(d.skipUntil[0]).Round(time.Minute); got != 15*time.Minute {
		t.Errorf("backed off for %s, want the base 15m", got)
	}

	// And a second drain at the threshold escalates to the next rung, exactly as
	// the session-count ladder used to after a fourth fruitless drain.
	d.pending[0] = true
	d.spent[0] = quietTokenThreshold
	d.judgeProgress(0, backlog.Summary{Claimable: 1})
	if got := time.Until(d.skipUntil[0]).Round(time.Minute); got != 30*time.Minute {
		t.Errorf("second crossing backed off for %s, want the 30m rung", got)
	}
}

// A drain that settled nothing is fruitless even if questions fell while it ran:
// the verdict on a drain is what it SETTLED, and a session that answers its own
// question has not thereby delivered anything. The baseline still tracks, so the
// next poll judges the change since this one rather than re-reading it.
func TestJudgeProgressDrainThatSettlesNothingIsStillFruitless(t *testing.T) {
	d := New(fastCfg(), &fakeAdapter{}, &fakePoller{})
	d.pending[0] = true
	d.quietTokens[0], d.spent[0], d.baseline[0], d.openQs[0] = 10_000_000, 10_000_000, 4, 3

	d.judgeProgress(0, backlog.Summary{Done: 4, OpenQuestions: 1})

	if d.quietTokens[0] != 20_000_000 {
		t.Errorf("a drain that settled nothing must still count against the target; quietTokens=%d, want 20000000", d.quietTokens[0])
	}
	if d.openQs[0] != 1 {
		t.Errorf("the open-question baseline must track through a drain verdict too; got %d, want 1", d.openQs[0])
	}
}

// The window the fall is otherwise LOST in: the back-off elapses, a drain goes
// out, and the operator answers while it is running. That poll is the only one
// that will ever see the fall, because the baseline advances on it - so if it
// merely counts the strike, the target sits out the next rung of the ladder on a
// queue that is already unblocked, which is the CLA-248 bug again in a narrower
// window. It takes the strike and skips the sit-out: one immediate retry.
func TestJudgeProgressAnswerDuringADrainSkipsTheSitOut(t *testing.T) {
	d := New(fastCfg(), &fakeAdapter{}, &fakePoller{})
	d.pending[0] = true
	d.quietTokens[0], d.spent[0], d.baseline[0], d.openQs[0] = quietTokenThreshold, 10_000_000, 4, 1

	d.judgeProgress(0, backlog.Summary{Claimable: 1, Done: 4, OpenQuestions: 0})

	if d.quietTokens[0] != quietTokenThreshold+10_000_000 {
		t.Errorf("the drain settled nothing, so its spend is still charged; quietTokens=%d, want %d", d.quietTokens[0], quietTokenThreshold+10_000_000)
	}
	if d.backedOff(0) {
		t.Error("an answer that landed mid-drain must buy one immediate retry, not the next rung of the ladder")
	}
	// And the retry is ONE: a second fruitless drain, with nothing having moved,
	// backs the target off at the rung its spend has reached.
	d.pending[0] = true
	d.spent[0] = 10_000_000
	d.judgeProgress(0, backlog.Summary{Claimable: 1, Done: 4, OpenQuestions: 0})
	if !d.backedOff(0) {
		t.Error("the retry is one, not an exemption; a second fruitless drain must back the target off")
	}
}

// The reported sequence end to end: a session blocks on a question, three fruitless
// drains back the target off, the operator reads the message and answers, and the
// target is eligible again on the very next poll instead of after 15 minutes.
func TestJudgeProgressAnswerEndsTheBackOffOnTheNextPoll(t *testing.T) {
	d := New(fastCfg(), &fakeAdapter{}, &fakePoller{})
	blocked := backlog.Summary{Claimable: 1, OpenQuestions: 1}
	for n := 0; n < quietThreshold; n++ {
		d.pending[0] = true         // a drain went out...
		d.spent[0] = 30_000_000     // ...costing a real ~30M (3x crosses the 80M threshold)
		d.judgeProgress(0, blocked) // ...and settled nothing
	}
	if !d.backedOff(0) {
		t.Fatalf("%d fruitless drains must back the target off", quietThreshold)
	}

	d.judgeProgress(0, backlog.Summary{Claimable: 1, OpenQuestions: 0})

	if d.backedOff(0) {
		t.Error("the answer must end the back-off on the next poll, not after the full wait")
	}
}

// A target with nothing claimable is IDLE, not fruitless — there is nothing for
// the gate to spawn, so there is nothing it can have failed at. `quiet` used to
// survive that, and survive the blocker being parked (`parked` is not counted in
// Settled(), so nothing else cleared it), so a task filed a week later inherited
// the escalated wait on its first strike.
func TestJudgeProgressForgetsFruitlessDrainsOnceNothingIsClaimable(t *testing.T) {
	d := New(fastCfg(), &fakeAdapter{}, &fakePoller{})

	// Six fruitless drains of 60M each against a claimable-but-unworkable queue:
	// 360M crosses three thresholds, putting the target at the 2h cap.
	for range 6 {
		d.pending[0] = true
		d.spent[0] = 60_000_000
		d.judgeProgress(0, backlog.Summary{Claimable: 1})
	}
	if d.quietTokens[0] != 360_000_000 {
		t.Fatalf("setup: quietTokens = %d, want 360000000", d.quietTokens[0])
	}
	if got := time.Until(d.skipUntil[0]).Round(time.Minute); got != 2*time.Hour {
		t.Fatalf("setup: backed off for %s, want the escalated 2h", got)
	}

	// The operator parks the blocker: the queue is now empty.
	d.judgeProgress(0, backlog.Summary{Claimable: 0})
	if d.quietTokens[0] != 0 || !d.skipUntil[0].IsZero() {
		t.Fatalf("an idle poll must forget the fruitless spend and its wait; quietTokens=%d skipUntil=%v", d.quietTokens[0], d.skipUntil[0])
	}

	// A week later, unrelated work is filed and its first drains settle nothing —
	// a large task spanning sessions looks exactly like this. It must serve the
	// BASE wait, not inherit the one the parked blocker earned.
	for range quietThreshold {
		d.pending[0] = true
		d.spent[0] = 30_000_000
		d.judgeProgress(0, backlog.Summary{Claimable: 1})
	}
	if got := time.Until(d.skipUntil[0]).Round(time.Minute); got != 15*time.Minute {
		t.Errorf("new work backed off for %s, want the base 15m", got)
	}
}

// The other half of the rule: an idle poll must not CANCEL a verdict that is
// still outstanding. `claimable == 0` also means "everything ready is claimed
// right now", and the poll straight after a drain is exactly when that happens —
// a session that ends holding a task it pushed work on is deliberately not handed
// back. Forgetting there would let a target that always ends that way alternate
// spawn / forget forever and never back off at all.
func TestJudgeProgressStillJudgesADrainOutstandingWhenTheQueueGoesQuiet(t *testing.T) {
	d := New(fastCfg(), &fakeAdapter{}, &fakePoller{})

	// Each drain settles nothing and leaves the task claimed, so every verdict poll
	// sees an empty queue. The spend must still climb.
	for n := 1; n <= quietThreshold; n++ {
		d.pending[0] = true
		d.spent[0] = 30_000_000
		d.judgeProgress(0, backlog.Summary{Claimable: 0, InProgress: 1})
		if d.quietTokens[0] != n*30_000_000 {
			t.Fatalf("after %d fruitless drains quietTokens = %d, want %d — an outstanding verdict was cancelled", n, d.quietTokens[0], n*30_000_000)
		}
	}
	if d.skipUntil[0].IsZero() {
		t.Error("three fruitless drains must back the target off, whatever the queue looked like at verdict time")
	}

	// One more idle poll with nothing outstanding IS the idle signal, and forgets.
	d.judgeProgress(0, backlog.Summary{Claimable: 0, InProgress: 1})
	if d.quietTokens[0] != 0 || !d.skipUntil[0].IsZero() {
		t.Errorf("a settled, idle target must forget; quietTokens=%d skipUntil=%v", d.quietTokens[0], d.skipUntil[0])
	}
}

// The idle branch must advance the open-question baseline as every other path
// does. This is the one place the two rules meet: CLA-249's idle reset returns
// early, and CLA-248 added a second baseline that every other path advances
// beside the first, so the merge of the two left `openQs` behind.
//
// It is an INVARIANT violation, not a live bug, and the distinction is the point
// of this comment. The stale value is unreachable today: the idle branch zeroes
// `quiet` in the same statement, the `!pending` fall detector is gated on
// `quiet > 0`, and the drain-verdict path drops its `answered` flag below
// quietThreshold — so the first poll after an idle stretch always consumes the
// stale baseline somewhere it cannot change the outcome, and refreshes it. The
// fix is here because the invariant is what the doc comment above judgeProgress
// promises, and because three separate conditions currently have to hold for it
// not to matter: drop quietThreshold to 1, stop zeroing `quiet` in the idle
// branch, or add a consumer of the fall that is not gated on `quiet > 0`, and it
// becomes reachable.
//
// So the assertion that pins this is the baseline itself, below. The drains after
// it are a plain regression guard and pass with or without the fix; they are not
// a demonstration of the failure, because no reachable sequence is.
func TestJudgeProgressIdlePollAdvancesTheOpenQuestionBaseline(t *testing.T) {
	d := New(fastCfg(), &fakeAdapter{}, &fakePoller{})
	d.baseline[0], d.openQs[0] = 4, 3

	// An idle stretch during which the operator clears the queue's questions.
	d.judgeProgress(0, backlog.Summary{Claimable: 0, Done: 4, OpenQuestions: 1})

	if d.openQs[0] != 1 {
		t.Fatalf("an idle poll must track the open-question baseline like every other path; got %d, want 1", d.openQs[0])
	}

	// Guard, not a demonstration (see above): work is filed and its drains settle
	// nothing, and the third backs the target off. Green either way today — it is
	// here so that if the fall ever becomes readable while `quietTokens > 0`, an idle
	// stretch cannot hand a later drain a retry it did not earn.
	for range quietThreshold {
		d.pending[0] = true
		d.spent[0] = 30_000_000
		d.judgeProgress(0, backlog.Summary{Claimable: 1, Done: 4, OpenQuestions: 1})
	}
	if !d.backedOff(0) {
		t.Error("a fall consumed during an idle stretch must not buy a later drain a free retry")
	}
}

// The same thing through Run: a backed-off target whose queue then empties starts
// spawning again the moment work reappears, rather than serving out a wait no
// fast-config interval can skip.
func TestRun_IdleQueueLiftsTheNoProgressBackOff(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 4
	// Each fruitless session costs a real ~30M so three of them cross the 80M
	// token threshold (CLA-343).
	h := &fakeAdapter{tokens: 30_000_000}
	// Polls 1-3 each spawn a session that settles nothing; poll 4 is the verdict on
	// the third and earns a 15-minute wait no fast-config interval can skip. Poll 5
	// shows an empty queue, which lifts it, and the work that arrives after must
	// get a session rather than serve out somebody else's back-off.
	p := &fakePoller{sums: []backlog.Summary{
		{Ready: 1, Claimable: 1},
		{Ready: 1, Claimable: 1},
		{Ready: 1, Claimable: 1},
		{Ready: 1, Claimable: 1},
		{Ready: 0, Claimable: 0},
	}, sum: backlog.Summary{Ready: 1, Claimable: 1}}

	if err := runLoop(t, cfg, h, p); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if h.invokeCalls != 4 {
		t.Errorf("work arriving after an idle stretch must not inherit the back-off; got %d of 4 sessions", h.invokeCalls)
	}
}

func TestQuietBackoffEscalatesToACap(t *testing.T) {
	for _, tc := range []struct {
		quiet int
		want  time.Duration
	}{
		{3, 15 * time.Minute},
		{4, 30 * time.Minute},
		{5, time.Hour},
		{6, 2 * time.Hour},
		{99, 2 * time.Hour}, // never becomes "never" — the blocker is usually one answer away
	} {
		if got := quietBackoff(tc.quiet); got != tc.want {
			t.Errorf("quietBackoff(%d) = %s, want %s", tc.quiet, got, tc.want)
		}
	}
}

// A wait that takes far longer in real time than in timer time means the machine
// was suspended: Go's timers do not advance while it sleeps, so the loop freezes
// mid-wait and goes silent in a way indistinguishable from a hang.
func TestSleepStall(t *testing.T) {
	for _, tc := range []struct {
		name           string
		intended, wall time.Duration
		wantStalled    bool
	}{
		{"the real case: a 30m wait that took 2h28m", 30 * time.Minute, 2*time.Hour + 28*time.Minute, true},
		{"ordinary scheduling overshoot says nothing", 30 * time.Minute, 30*time.Minute + 2*time.Second, false},
		{"a short idle poll can be stalled too", time.Minute, 20 * time.Minute, true},
		{"just under the floor stays quiet", time.Minute, 5 * time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := sleepStall(tc.intended, tc.wall); got != tc.wantStalled {
				t.Errorf("sleepStall(%s, %s) stalled = %v, want %v", tc.intended, tc.wall, got, tc.wantStalled)
			}
		})
	}
}

// The handback is best-effort: the lease expiring is the old behaviour, so a
// plane that refuses the release must not take the whole run down with it.
func TestRun_ReleaseFailureDoesNotStopTheRun(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 1

	h := &fakeAdapter{steps: []invokeStep{{res: held(okResult(10, 0.1), openClaim())}}}
	rel := &fakeReleaser{err: errors.New("plane unreachable")}

	if err := runWithReleaser(t, cfg, h, busyPoller(), rel); err != nil {
		t.Fatalf("a failed release must not fail the run, got: %v", err)
	}
	if len(rel.calls) != 1 {
		t.Errorf("expected the release to have been attempted once, got %d", len(rel.calls))
	}
}

// A target with no releaser configured must behave exactly as before.
func TestRun_NoReleaserIsNotFatal(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 1

	h := &fakeAdapter{steps: []invokeStep{{res: held(okResult(10, 0.1), openClaim())}}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := NewMulti(cfg, h, []Target{{Poller: busyPoller()}}).Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// State-dir confinement (CLA-259). The daemon runs OUTSIDE the sandbox its own
// sessions run inside, so anything it writes on a path a session can influence
// is that sandbox defeated by proxy. These drive the behaviour rather than
// asserting a mode: a real drain, then look at what is on disk.

// A transcript records whatever the harness printed — prompts, tool arguments,
// occasionally a token. No other user on the machine may read one.
func TestDrainWritesOwnerOnlyTranscripts(t *testing.T) {
	cfg := fastCfg()
	dir := t.TempDir()
	cfg.StateDir = dir
	cfg.MaxIterations = 1
	if err := runLoop(t, cfg, &fakeAdapter{}, &fakePoller{sum: backlog.Summary{Claimable: 1}}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	logs := iterationLogs(t, dir)
	if len(logs) != 1 {
		t.Fatalf("got %d iteration logs, want 1: %v", len(logs), logs)
	}
	fi, err := os.Lstat(filepath.Join(dir, logs[0]))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("iteration log mode: got %04o, want 0600", got)
	}
	if di, err := os.Lstat(dir); err != nil {
		t.Fatal(err)
	} else if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("state dir mode: got %04o, want 0700", got)
	}
}

// The self-ignoring .gitignore, so a state dir an operator points back inside a
// repo can still never be staged by an unattended `git add -A`.
func TestDrainLeavesTheStateDirUnstageable(t *testing.T) {
	cfg := fastCfg()
	dir := t.TempDir()
	cfg.StateDir = dir
	cfg.MaxIterations = 1
	if err := runLoop(t, cfg, &fakeAdapter{}, &fakePoller{sum: backlog.Summary{Claimable: 1}}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "*\n" {
		t.Errorf(".gitignore body: got %q, want %q", b, "*\n")
	}
}

// The log name used to be derivable to within a second by the very session whose
// transcript it holds, which is all a pre-planted decoy needs.
func TestIterationLogNamesAreNotGuessable(t *testing.T) {
	cfg := fastCfg()
	dir := t.TempDir()
	cfg.StateDir = dir
	cfg.MaxIterations = 3
	if err := runLoop(t, cfg, &fakeAdapter{}, &fakePoller{sum: backlog.Summary{Claimable: 1}}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	logs := iterationLogs(t, dir)
	if len(logs) != 3 {
		t.Fatalf("got %d iteration logs, want 3: %v", len(logs), logs)
	}
	seen := map[string]bool{}
	for _, name := range logs {
		tail := strings.TrimSuffix(name[strings.LastIndexByte(name, '-')+1:], ".log")
		if len(tail) != 8 {
			t.Fatalf("log %q has no 8-hex random tail", name)
		}
		if seen[tail] {
			t.Errorf("log tail %q repeated — it is not random", tail)
		}
		seen[tail] = true
	}
}

// The escape this task closes: a session pre-plants a symlink at a path the
// daemon writes, and the daemon truncates a file the session cannot reach. The
// log name is unguessable now, so this drives the guarantee directly — every
// name is refused, and the victim survives.
func TestIterationLogNeverWritesThroughAPlantedSymlink(t *testing.T) {
	d := New(fastCfg(), &fakeAdapter{}, &fakePoller{})
	openTestStateDir(t, d)

	victim := filepath.Join(t.TempDir(), "authorized_keys")
	const body = "ssh-ed25519 AAAA... operator@laptop\n"
	if err := os.WriteFile(victim, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// Plant the link at every name a drain could pick this second, plus the whole
	// fan of attempt counters — the pre-CLA-259 attack, minus the guessing.
	stamp := time.Now().Format("20060102-150405")
	planted := 0
	for a := 0; a < 4; a++ {
		name := fmt.Sprintf("iteration-%s-d1-a%d-%s.log", stamp, a, randomTail())
		if err := os.Symlink(victim, filepath.Join(d.state.Path(), name)); err != nil {
			t.Fatal(err)
		}
		planted++
		// Prove the guard directly on the exact name, not only on the drain's pick.
		if f, err := d.state.Create(name); err == nil {
			f.Close()
			t.Fatalf("Create(%q) through a planted symlink succeeded", name)
		}
	}
	if planted == 0 {
		t.Fatal("planted nothing")
	}

	if _, _, _, err := d.drainWithRetries(context.Background(), 1, d.targets[0], spend{start: time.Now()}); err != nil {
		t.Fatalf("drain returned error: %v", err)
	}
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("the symlink target was written through: got %q, want %q", got, body)
	}
}

// A marker is the operator's switch. Following a symlink at STOP would let
// whoever planted it choose a file for the daemon to open and a line of it to
// echo into the log.
func TestMarkersAreNotReadThroughASymlink(t *testing.T) {
	d := New(fastCfg(), &fakeAdapter{}, &fakePoller{})
	openTestStateDir(t, d)

	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("sk-live-hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(d.state.Path(), "STOP")); err != nil {
		t.Fatal(err)
	}

	if present, msg := d.readMarker("STOP"); present {
		t.Errorf("a symlinked STOP was honoured, carrying %q", msg)
	}
}

// iterationLogs lists the per-iteration transcripts in a state dir.
func iterationLogs(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "iteration-") {
			out = append(out, e.Name())
		}
	}
	return out
}

// For an UNPHASED run — what this test drives, and what a config with no `phases`
// gets — the configured prompt is the ONLY thing bounding how much a session takes
// on, so the wording is the interface and it has to arrive at the harness
// unchanged. (A phased run also has config.Phase.MaxTurns, but that is a backstop
// under a boundary the prompt is meant to land on its own, not a second dial on
// scope — see TestDrainPhases_CarriesTheTurnCapToTheHarness.)
//
// Nothing asserted this before. `internal/config` pins what the default IS, but a
// change to Driver.invocation that dropped, defaulted or rewrote Prompt would have
// left every test in the repo green while every session silently got a different
// instruction. That is the same shape as the bug this all came from (CLA-281): a
// string nobody was watching decided how much work a session did.
func TestInvocationCarriesTheConfiguredPromptVerbatim(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 1
	// Deliberately not the default, and deliberately containing punctuation and
	// case: this asserts pass-through, not a match against the default constant.
	cfg.Prompt = "Work the next backlog item."
	h := &fakeAdapter{}
	targets := []Target{
		{Name: "solo", Poller: &fakePoller{sum: backlog.Summary{Claimable: 1}}, WorkDir: "/repos/solo", MCPConfigPath: "/repos/solo/.mcp.json"},
	}

	if err := runLoopMulti(t, cfg, h, targets); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if h.invokeCalls != 1 {
		t.Fatalf("want exactly one session; got %d", h.invokeCalls)
	}
	if got := h.invocations[0].Prompt; got != cfg.Prompt {
		t.Errorf("harness got prompt %q, want the configured %q verbatim", got, cfg.Prompt)
	}
}
