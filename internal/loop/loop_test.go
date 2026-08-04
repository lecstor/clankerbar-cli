package loop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

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
	probeErr     error
	probeCalls   int
	limitResetAt time.Time
}

func (f *fakeAdapter) Name() string { return "fake" }

func (f *fakeAdapter) Invoke(ctx context.Context, in harness.Invocation) (harness.Result, error) {
	f.invocations = append(f.invocations, in)
	i := f.invokeCalls
	f.invokeCalls++
	if i < len(f.steps) {
		return f.steps[i].res, f.steps[i].err
	}
	// Steps exhausted → a clean success, so a loop that keeps draining does not
	// panic and terminates via some other ceiling (max-iterations / budget).
	return okResult(0, 0), nil
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

func (f *fakeAdapter) Probe(ctx context.Context, in harness.Invocation) (harness.Limit, error) {
	i := f.probeCalls
	f.probeCalls++
	if f.probeErr != nil {
		return harness.Limit{}, f.probeErr
	}
	if i < len(f.probeResults) {
		return f.probeResults[i], nil
	}
	return harness.Limit{Limited: false}, nil // default: the limit has lifted
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
func TestDrainWithRetries_ResetPastDeadlineStopsInsteadOfWaiting(t *testing.T) {
	cfg := fastCfg()
	h := &fakeAdapter{
		steps:        []invokeStep{{res: limitResult()}, {res: okResult(7, 0)}},
		limitResetAt: time.Now().Add(3 * time.Hour),
	}
	d := New(cfg, h, &fakePoller{})
	d.stateDir = t.TempDir()

	deadline := time.Now().Add(30 * time.Minute) // reset lands well past it
	_, _, stop, err := d.drainWithRetries(context.Background(), 1, d.targets[0], deadline)

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
func TestDrainWithRetries_ResetInsideDeadlineStillWaits(t *testing.T) {
	cfg := fastCfg()
	h := &fakeAdapter{
		steps:        []invokeStep{{res: limitResult()}, {res: okResult(7, 0)}},
		limitResetAt: time.Now().Add(time.Minute),
	}
	d := New(cfg, h, &fakePoller{})
	d.stateDir = t.TempDir()

	deadline := time.Now().Add(4 * time.Hour)
	tokens, _, stop, err := d.drainWithRetries(context.Background(), 1, d.targets[0], deadline)

	if err != nil || stop {
		t.Fatalf("a reset inside the ceiling must be waited out: stop=%v err=%v", stop, err)
	}
	if tokens != 7 {
		t.Errorf("the session should have been re-run after the wait; got %d tokens", tokens)
	}
}

// Both halves must be known before an early stop is justified: an unknown reset
// is waited out because the supervised wait polls for an EARLY lift, and with no
// wall-clock ceiling there is nothing to be past.
func TestWaitPastDeadline(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name             string
		resetAt          time.Time
		deadline         time.Time
		wantOverDeadline bool
	}{
		{"reset past deadline", now.Add(2 * time.Hour), now.Add(time.Hour), true},
		{"reset inside deadline", now.Add(time.Hour), now.Add(2 * time.Hour), false},
		{"unknown reset is waited out", time.Time{}, now.Add(time.Hour), false},
		{"no ceiling means nothing to be past", now.Add(2 * time.Hour), time.Time{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := waitPastDeadline(tc.resetAt, tc.deadline); got != tc.wantOverDeadline {
				t.Errorf("waitPastDeadline = %v, want %v", got, tc.wantOverDeadline)
			}
		})
	}
}

// drainWithRetries: transient retry / usage-limit wait / hard stop / genuine
// failures. Exercised directly on the method — its retry loop is the unit that
// must NOT advance the outer drain count, and testing it in isolation makes that
// structural fact assertable through Invoke-call counts.

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
			d.stateDir = t.TempDir() // drainWithRetries writes per-iteration logs here

			tokens, cost, stop, err := d.drainWithRetries(context.Background(), 1, d.targets[0], time.Time{})

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
	d.stateDir = t.TempDir()

	tokens, _, stop, err := d.drainWithRetries(context.Background(), 1, d.targets[0], time.Time{})
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
	h := &fakeAdapter{} // every session succeeds cleanly...
	// ...but nothing ever reaches a reviewer or finishes, which is the shape of a
	// queue whose only claimable task is waiting on the operator.
	p := &fakePoller{sum: backlog.Summary{Ready: 1, Claimable: 1}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := New(cfg, h, p).Run(ctx); err != nil && ctx.Err() == nil {
		t.Fatalf("Run returned error: %v", err)
	}

	// Three sessions to reach the threshold, then a 15-minute back-off that no
	// fast-config interval can skip — so the run cannot reach its 10 iterations.
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
	d.quiet[0] = quietThreshold
	d.skipUntil[0] = time.Now().Add(time.Hour)
	d.baseline[0] = 4

	d.judgeProgress(0, backlog.Summary{Done: 5})

	if d.quiet[0] != 0 || !d.skipUntil[0].IsZero() {
		t.Errorf("outside progress must clear the back-off; quiet=%d skipUntil=%v", d.quiet[0], d.skipUntil[0])
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
