package loop

import (
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// A session whose output the adapter could not read whole reports figures with a
// hole in them, and the driver's contract is to act on NONE of them (CLA-262).
//
// The stream that produces this is a single stream-json line above the adapter's
// line cap — one tool_result carrying a large file read. Everything after it is
// lost: the `result` event that carries a claude session's entire tokens-and-cost
// total, and any settle that released the task.

// untrustedResult is what an adapter hands back after a truncated stream: whatever
// it managed to parse before the break, plus the reason none of it can be believed.
// The spend figure is deliberately NON-ZERO — a partial sum is the shape that
// tempts a caller to count it.
func untrustedResult(tokens int, cost float64) harness.Result {
	return harness.Result{
		ExitCode:  0,
		Tokens:    tokens,
		CostUSD:   cost,
		Untrusted: "claude's stdout could not be read to the end (bufio.Scanner: token too long)",
		Raw:       map[string]any{"kind": "ok"},
	}
}

// The spend parsed off a truncated stream is a floor of unknown looseness, never a
// measurement: claude's whole-session total arrives in the LAST event, so a stream
// cut short reports near-zero for a session that may have cost hundreds. Feeding
// that to the accumulator would put a made-up number in front of the breaker and
// in the iteration's cost line.
func TestDrain_UntrustedSpendIsNotCounted(t *testing.T) {
	logs := captureLogs(t)
	cfg := fastCfg()
	d := New(cfg, &fakeAdapter{steps: []invokeStep{{res: untrustedResult(5000, 12.34)}}}, busyPoller())
	openTestStateDir(t, d)

	tokens, cost, stop, err := drainOnce(t, d)
	if err != nil {
		t.Fatalf("drain returned error: %v", err)
	}
	if stop {
		t.Error("stop = true with no spend ceiling set; there is no ceiling to break, so the run should carry on")
	}
	if tokens != 0 || cost != 0 {
		t.Errorf("counted %d tokens / $%v off a truncated stream; a partial figure is not a measurement", tokens, cost)
	}
	if out := logs.String(); !strings.Contains(out, "UNTRUSTED") || !strings.Contains(out, "not counting this session's parsed spend") {
		t.Errorf("the truncation was not said out loud:\n%s", out)
	}
}

// The ceiling is a promise, and an unmeasurable session is the driver saying it can
// no longer keep it. An operator who set max_tokens/max_cost_usd asked to be
// protected from exactly this: sessions whose cost nothing can see. One is
// survivable; a night of them against an inert ceiling is the failure CLA-287 and
// CLA-258 were both about.
func TestDrain_UntrustedStopsTheRunWhenASpendCeilingIsSet(t *testing.T) {
	for _, tc := range []struct {
		name     string
		budget   config.Budget
		wantStop bool
	}{
		{"a token ceiling can no longer be honoured", config.Budget{MaxTokens: 1_000_000}, true},
		{"a cost ceiling can no longer be honoured", config.Budget{MaxCostUSD: 50}, true},
		{
			// The wall clock does not depend on anything the child said, so that
			// ceiling is still intact and there is nothing to stop for.
			"a wall-clock ceiling alone is unaffected",
			config.Budget{MaxWallClock: config.Duration(8 * time.Hour)},
			false,
		},
		{"no ceiling at all: nothing to break", config.Budget{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			cfg := fastCfg()
			cfg.Budget = tc.budget
			d := New(cfg, &fakeAdapter{steps: []invokeStep{{res: untrustedResult(10, 0.5)}}}, busyPoller())
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

// A truncated stream tells us which calls we SAW, never which the session made:
// the settle that released the task may be in the bytes that never arrived.
// Handing it back on that reading posts `ready` over work already in review, which
// is worse than any lease expiring.
func TestDrain_UntrustedDoesNotHandBackTheClaim(t *testing.T) {
	logs := captureLogs(t)
	cfg := fastCfg()
	h := &fakeAdapter{steps: []invokeStep{{res: held(untrustedResult(0, 0), openClaim())}}}
	rel := &fakeReleaser{}
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: rel}})
	openTestStateDir(t, d)

	if _, _, _, err := drainOnce(t, d); err != nil {
		t.Fatalf("drain returned error: %v", err)
	}
	if len(rel.calls) != 0 {
		t.Errorf("released %+v off a truncated stream; the task may already be settled", rel.calls)
	}
	if out := logs.String(); !strings.Contains(out, "leaving the lease to expire") {
		t.Errorf("the refusal to release was not explained:\n%s", out)
	}
}

// The exit code of a session whose stream broke is not evidence either — the
// adapter drains the pipe precisely so the code is not an EPIPE artefact, but the
// TEXT the classifiers read is still full of holes. So neither the transient arm
// nor the non-retryable stop may fire on it: a retry re-spawns a paid session on a
// reading we already know is incomplete, and stopping the whole run blames the
// child for our own truncated read.
func TestDrain_UntrustedIsNotClassified(t *testing.T) {
	captureLogs(t)
	cfg := fastCfg()
	cfg.MaxRetries = 3

	res := untrustedResult(0, 0)
	res.ExitCode = 1
	res.Raw = map[string]any{"kind": "transient"} // the fake would retry this all day
	h := &fakeAdapter{steps: []invokeStep{{res: res}, {res: okResult(10, 0.1)}}}
	d := New(cfg, h, busyPoller())
	openTestStateDir(t, d)

	_, _, stop, err := drainOnce(t, d)
	if err != nil {
		t.Fatalf("drain returned error: %v", err)
	}
	if stop {
		t.Error("stop = true with no spend ceiling set")
	}
	if h.invokeCalls != 1 {
		t.Errorf("Invoke called %d times, want 1 — a truncated stream must not be retried as a transient blip", h.invokeCalls)
	}
}

// A limit is not read off a broken stream either: DetectLimit reads the same text,
// and a supervised wait entered on a phantom cap sleeps for as long as the stated
// reset while the operator waits for progress that is not coming.
func TestDrain_UntrustedDoesNotEnterASupervisedWait(t *testing.T) {
	captureLogs(t)
	cfg := fastCfg()

	res := untrustedResult(0, 0)
	res.ExitCode = 1
	res.Raw = map[string]any{"kind": "limit"}
	h := &fakeAdapter{steps: []invokeStep{{res: res}}}
	d := New(cfg, h, busyPoller())
	openTestStateDir(t, d)

	if _, _, _, err := drainOnce(t, d); err != nil {
		t.Fatalf("drain returned error: %v", err)
	}
	if h.probeCalls != 0 {
		t.Errorf("probed %d times: the drain entered a supervised wait on a limit read out of a truncated stream", h.probeCalls)
	}
}

// What a truncated stream DID say about delivery still stands, and must: a report
// only exists here because the plane accepted the call that made it, which is a
// fact about the plane rather than a reading of the stream. The check is local git,
// and its answer is as good as ever.
func TestDrain_UntrustedStillVerifiesAcceptedDeliveries(t *testing.T) {
	captureLogs(t)
	cfg := fastCfg()

	res := untrustedResult(0, 0)
	res.Reports = []harness.Report{{TaskID: "t-1", Ref: "CLA-262", RunID: "r-1", Branch: "clanker/x", Status: "in_review"}}
	h := &fakeAdapter{steps: []invokeStep{{res: res}}}
	d := New(cfg, h, busyPoller())
	openTestStateDir(t, d)

	v := &fakeVerifier{}
	d.newVerifier = func(string) deliveryVerifier { return v }

	if _, _, _, err := drainOnce(t, d); err != nil {
		t.Fatalf("drain returned error: %v", err)
	}
	if len(v.claims) != 1 {
		t.Errorf("verified %d claims, want 1 — an ACCEPTED delivery claim is not in doubt just because the stream broke afterwards", len(v.claims))
	}
}

// And the run keeps going: the loop's own no-progress back-off is what bounds a
// target that produces these repeatedly, so an untrusted drain does not need to
// end the daemon when there is no spend ceiling to protect.
func TestRun_CarriesOnAfterAnUntrustedDrain(t *testing.T) {
	captureLogs(t)
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 2

	h := &fakeAdapter{steps: []invokeStep{
		{res: untrustedResult(1, 0.01)},
		{res: okResult(100, 1)},
	}}
	p := &fakePoller{sum: backlog.Summary{Ready: 1, Claimable: 1}}
	if err := runLoop(t, cfg, h, p); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if h.invokeCalls != 2 {
		t.Errorf("Invoke called %d times, want 2 — the run should reach its max-iterations, not stop on the untrusted drain", h.invokeCalls)
	}
}
