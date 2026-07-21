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
	probeResults []harness.Limit
	probeErr     error
	probeCalls   int
	limitResetAt time.Time
}

func (f *fakeAdapter) Name() string { return "fake" }

func (f *fakeAdapter) Invoke(ctx context.Context, in harness.Invocation) (harness.Result, error) {
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
}

// ---------------------------------------------------------------------------
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

			tokens, cost, stop, err := d.drainWithRetries(context.Background(), 1)

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

	tokens, _, stop, err := d.drainWithRetries(context.Background(), 1)
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

func writeMarker(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("writing %s marker: %v", name, err)
	}
}
