package loop

// CLA-491: a daemon that exits through a path which never reads the STOP marker
// - a non-retryable phase failure, a budget trip, a SIGTERM-shaped context
// cancel - used to leave the operator's soft-stop marker sitting in the state
// dir, and the NEXT start read it as fresh and stopped itself seconds into its
// first cycle. Three behaviours are pinned here:
//
//  1. shutdown paths leave NO pending STOP marker behind (Run's exit sweep, so
//     the guarantee covers paths these cases do not name);
//  2. the sweep beside a HALT (and on the one in-drain failure whose real-world
//     window no end-to-end drive can reach) clears a pending STOP;
//  3. a soft stop dropped against a RUNNING daemon still works end to end, and
//     the relaunch after it is not stopped by what the first run consumed.
//
// WHERE each case plants its marker is the substance: a marker that predates the
// process is consumed with a warning at start-up (the other half of CLA-491,
// pinned in TestRun_Markers), so only a drop against a LIVE daemon can be
// pending at exit. The loop reads markers at every wait (idle polls, retry
// backoffs, supervised waits), so for most exits the only honest plant is during
// a live session, which is what fakeAdapter.onInvoke is for.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// markerPresent reports whether a marker file is still sitting in dir.
func markerPresent(t *testing.T, dir, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, name))
	if err == nil {
		return true
	}
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	t.Fatalf("stat %s: %v", filepath.Join(dir, name), err)
	return false
}

// limitedResultSpending is a usage-limit turn-away whose session DID report
// spend: the ladder charges what it burned whatever the verdict (the accumulator
// comment above the attempt seam), so this is spend the mid-drain breakers see.
func limitedResultSpending(cost float64) harness.Result {
	return harness.Result{ExitCode: 1, CostUSD: cost, Raw: map[string]any{"kind": "limit"}}
}

// TestRun_ShutdownPathsSweepPendingStopMarker drives one shutdown path per case,
// each with a STOP marker dropped on the daemon while it was live. Every path
// must leave the state dir without it.
//
// The budget case trips the breaker ATOP the paused wait rather than between
// drains on purpose: at an iteration boundary a fresh STOP is READ and honoured
// first (soft stop outranks the ceiling; TestRun_SoftStopThenRelaunch pins
// that), and every transient-retry backoff is a read too. Inside the entry to a
// supervised wait there is no read before the breaker, which is exactly the
// exposure the sweep exists for.
func TestRun_ShutdownPathsSweepPendingStopMarker(t *testing.T) {
	cases := []struct {
		name string
		// mutate tweaks the fast config; steps script the harness.
		mutate func(cfg *config.Config)
		steps  []invokeStep
		// plant is where the soft stop lands against the live daemon:
		// "poll" drops it during the first queue poll; "invoke" drops it
		// during the FIRST live session, the window some paths alone have.
		plant string
		// cancelAfterPolls schedules a context cancel (what SIGTERM/SIGINT do
		// via signal.NotifyContext in main) once Poll has been called this many
		// times; negative schedules none.
		cancelAfterPolls int
		// wantErr / wantErrText pin HOW the path ends: an errors.Is match or a
		// message fragment (the non-retryable failure has no sentinel).
		wantErr     error
		wantErrText string
		// wantInvokes is the session count the scenario spends before ending.
		wantInvokes int
	}{
		{
			name: "non-retryable phase failure",
			steps: []invokeStep{
				{res: nonRetryableResult()},
				{res: okResult(0, 0)}, // a retry would land here; it must not
			},
			plant:       "poll",
			wantErrText: "non-retryable",
			wantInvokes: 1,
		},
		{
			name: "budget trip atop the paused wait",
			mutate: func(cfg *config.Config) {
				cfg.Budget.MaxCostUSD = 0.01
			},
			steps:       []invokeStep{{res: limitedResultSpending(0.05)}},
			plant:       "invoke",
			wantInvokes: 1,
		},
		{
			name:             "context cancel (the SIGTERM/SIGINT path)",
			plant:            "poll",
			cancelAfterPolls: 0,
			steps:            []invokeStep{{res: okResult(0, 0)}},
			wantInvokes:      1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fastCfg()
			dir := t.TempDir()
			cfg.StateDir = dir
			if tc.mutate != nil {
				tc.mutate(cfg)
			}
			h := &fakeAdapter{steps: tc.steps}
			p := &fakePoller{sum: backlog.Summary{Claimable: 1}}
			logs := captureLogs(t)

			plant := func() {
				if err := os.WriteFile(filepath.Join(dir, "STOP"), []byte("stop after this"), 0o644); err != nil {
					t.Errorf("planting STOP: %v", err)
				}
			}
			switch tc.plant {
			case "poll":
				p.onCall = func(i int) {
					if i == 0 {
						plant()
					}
				}
			case "invoke":
				h.onInvoke = func(i int) {
					if i == 0 {
						plant()
					}
				}
			default:
				t.Fatalf("case %q must say where the stop lands", tc.name)
			}

			// A bounded context, exactly as runLoop uses: the safety net if a
			// control-flow regression fails to terminate, and the carrier for the
			// signal-shaped cancel when the case wants one.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if tc.cancelAfterPolls >= 0 {
				after := tc.cancelAfterPolls
				prev := p.onCall
				p.onCall = func(i int) {
					if prev != nil {
						prev(i)
					}
					if i >= after {
						cancel()
					}
				}
			}

			err := New(cfg, h, p).Run(ctx)
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Run ended with %v, want an error wrapping %v; log was:\n%s", err, tc.wantErr, logs.String())
				}
			case tc.wantErrText != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErrText) {
					t.Fatalf("Run ended with %v, want an error naming %q; log was:\n%s", err, tc.wantErrText, logs.String())
				}
			default:
				if err != nil {
					t.Fatalf("Run returned an unexpected error: %v; log was:\n%s", err, logs.String())
				}
			}
			if h.invokeCalls != tc.wantInvokes {
				t.Errorf("spawned %d sessions, want %d", h.invokeCalls, tc.wantInvokes)
			}
			if markerPresent(t, dir, "STOP") {
				t.Errorf("this shutdown path left the pending STOP marker behind; the next start would read it as fresh and stop itself")
			}
			if out := logs.String(); !strings.Contains(out, "without consuming the STOP marker") {
				t.Errorf("the sweep must say what it did and why the run ended; log was:\n%s", out)
			}
		})
	}
}

// TestRun_SweepCoversTheExitsNoLiveWindowCanReach drives the sweep at the seam
// for two shapes whose end-to-end pending-marker window does not exist: the
// zero-spend bound (every retry backoff between attempts READS markers, so a
// drop never survives to that error) and HALT beside STOP (HALT is read at the
// same boundary, before any poll could land a fresh drop). Both still owe the
// guarantee: the sweep is registered unconditionally precisely so no exit path
// has to argue its own reachability.
//
// The third subtest pins the sweep's closing line on an error exit with NO
// marker to sweep — the exact "run ended: <why>" line the 2026-08-25 incident's
// truncated log lacked, and the one branch of the sweep the end-to-end cases
// cannot reach (they all plant a marker by construction).
func TestRun_SweepCoversTheExitsNoLiveWindowCanReach(t *testing.T) {
	t.Run("zero-spend bound trip", func(t *testing.T) {
		d := New(fastCfg(), &fakeAdapter{}, &fakePoller{})
		openTestStateDir(t, d)
		writeMarker(t, d.state.Path(), "STOP", "dropped against the live daemon")
		logs := captureLogs(t)
		// The exact shape drainPhase returns when the bound fires.
		err := fmt.Errorf("iteration 4: %w: 3 consecutive attempts died before fake reported any usage", errZeroSpendLoop)
		d.sweepPendingStop(err)
		if markerPresent(t, d.state.Path(), "STOP") {
			t.Error("the zero-spend exit must not leave a pending STOP marker behind")
		}
		out := logs.String()
		if !strings.Contains(out, "without consuming the STOP marker") || !strings.Contains(out, errZeroSpendLoop.Error()) {
			t.Errorf("the sweep line must name both the removal and the reason; log was:\n%s", out)
		}
	})

	t.Run("HALT beside STOP", func(t *testing.T) {
		d := New(fastCfg(), &fakeAdapter{}, &fakePoller{})
		openTestStateDir(t, d)
		writeMarker(t, d.state.Path(), "HALT", "wedged — needs a human")
		writeMarker(t, d.state.Path(), "STOP", "stop too")
		logs := captureLogs(t)
		d.sweepPendingStop(nil)
		if !markerPresent(t, d.state.Path(), "HALT") {
			t.Error("HALT must be left in place for the operator")
		}
		if markerPresent(t, d.state.Path(), "STOP") {
			t.Error("the pending STOP beside the HALT must be swept")
		}
		if out := logs.String(); !strings.Contains(out, "without consuming the STOP marker") {
			t.Errorf("the sweep must still say what it did beside a HALT; log was:\n%s", out)
		}
	})

	t.Run("error exit with no marker still closes the log with why", func(t *testing.T) {
		d := New(fastCfg(), &fakeAdapter{}, &fakePoller{})
		openTestStateDir(t, d)
		logs := captureLogs(t)
		// Any error return shape; what matters is the closing line.
		err := fmt.Errorf("iteration 4: %w: 3 consecutive attempts died before fake reported any usage", errZeroSpendLoop)
		d.sweepPendingStop(err)
		out := logs.String()
		if !strings.Contains(out, "run ended: ") || !strings.Contains(out, errZeroSpendLoop.Error()) {
			t.Errorf("the sweep must close the log with the exit reason; log was:\n%s", out)
		}
	})
}

// TestRun_SoftStopThenRelaunchDoesNotSelfStop is the incident, end to end: a
// daemon running with work queued has a STOP dropped on it mid-flight, consumes
// it at the next boundary, exits; the relaunch against the SAME state dir then
// does real work instead of reading a ghost.
func TestRun_SoftStopThenRelaunchDoesNotSelfStop(t *testing.T) {
	dir := t.TempDir()

	// Run 1: the soft stop. The poller plants a FRESH marker while the daemon is
	// live (between polls), so only the boundary read can see it.
	first := fastCfg()
	first.StateDir = dir
	first.MaxIterations = 3 // would keep working if the stop did not land
	h1 := &fakeAdapter{}    // steps exhausted -> clean successes
	p1 := &fakePoller{sum: backlog.Summary{Claimable: 5}}
	logs1 := captureLogs(t)
	p1.onCall = func(i int) {
		if i == 0 {
			if err := os.WriteFile(filepath.Join(dir, "STOP"), []byte("fleet soft-stop"), 0o644); err != nil {
				t.Errorf("planting STOP: %v", err)
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := New(first, h1, p1).Run(ctx); err != nil {
		t.Fatalf("run 1 returned error: %v", err)
	}
	if h1.invokeCalls != 1 {
		t.Errorf("the soft stop should end run 1 at the very next boundary; spawned %d sessions", h1.invokeCalls)
	}
	if markerPresent(t, dir, "STOP") {
		t.Fatal("run 1 consumed the fresh marker; a leftover here means the stop path changed")
	}
	if out := logs1.String(); !strings.Contains(out, "STOP requested") {
		t.Errorf("run 1 must log the stop as a stop; log was:\n%s", out)
	}

	// Run 2: the relaunch (the new clanker1). Nothing is planted; the state dir
	// holds no marker because run 1 was properly stopped.
	second := fastCfg()
	second.StateDir = dir
	second.MaxIterations = 1
	h2 := &fakeAdapter{}
	p2 := &fakePoller{sum: backlog.Summary{Claimable: 4}}
	logs2 := captureLogs(t)
	if err := New(second, h2, p2).Run(ctx); err != nil {
		t.Fatalf("relaunch returned error: %v", err)
	}
	if h2.invokeCalls != 1 {
		t.Errorf("the relaunched daemon self-stopped after %d sessions: the DOA is back", h2.invokeCalls)
	}
	out := logs2.String()
	if strings.Contains(out, "left over from a previous run") {
		t.Errorf("a properly consumed soft stop must not warn the relaunch; log was:\n%s", out)
	}
	if strings.Contains(out, "STOP requested") {
		t.Errorf("the relaunched daemon acted on a ghost stop; log was:\n%s", out)
	}
}
