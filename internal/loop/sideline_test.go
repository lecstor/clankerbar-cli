package loop

// CLA-507: a drain that returns an ERROR failed while trying to run ONE target's
// session (a malformed opencode.json in its workdir, a missing checkout, a
// harness that will not start). That failure sidelines that target on the quiet
// ladder - escalating while it keeps failing, rejoining once it succeeds - while
// every other project drains on. The run ends only when NOTHING is left to
// drive, and then names every benched project and its cause.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
)

func TestFailureBackoffFollowsTheQuietLadder(t *testing.T) {
	cases := []struct {
		fails int
		want  time.Duration
	}{
		{1, 15 * time.Minute},
		{2, 30 * time.Minute},
		{3, time.Hour},
		{4, 2 * time.Hour},
		{9, 2 * time.Hour}, // capped, like the quiet ladder it rides on
	}
	for _, tc := range cases {
		if got := failureBackoff(tc.fails); got != tc.want {
			t.Errorf("failureBackoff(%d) = %s, want %s", tc.fails, got, tc.want)
		}
	}
}

// Two projects, one broken: alpha's first session dies a non-retryable death,
// beta stays healthy. The run must NOT return, beta must keep draining, and
// alpha must sit out its back-off instead of being retried every poll.
func TestRun_HarnessFailureSidelinesOneTargetOnly(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 4
	logs := captureLogs(t)
	h := &fakeAdapter{steps: []invokeStep{
		{res: okResult(0, 0)},       // beta, iteration 1
		{res: nonRetryableResult()}, // alpha, iteration 2 - the broken config
		// steps exhausted -> every later session is a clean success
	}}
	targets := []Target{
		{Name: "alpha", Poller: &fakePoller{sum: backlog.Summary{Claimable: 2}}, WorkDir: "/repos/alpha"},
		{Name: "beta", Poller: &fakePoller{sum: backlog.Summary{Claimable: 2}}, WorkDir: "/repos/beta"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d := NewMulti(cfg, h, targets)
	if err := d.Run(ctx); err != nil {
		t.Fatalf("a sibling's harness failure must not end the run; got %v", err)
	}

	var seq []string
	for _, inv := range h.invocations {
		seq = append(seq, inv.WorkDir)
	}
	want := []string{"/repos/beta", "/repos/alpha", "/repos/beta", "/repos/beta"}
	if len(seq) != len(want) {
		t.Fatalf("drained %d times, want %d: %v", len(seq), len(want), seq)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("drain %d ran in %s, want %s (full sequence %v)", i, seq[i], want[i], seq)
		}
	}
	if d.harnessFails[0] != 1 || d.harnessFails[1] != 0 {
		t.Errorf("only alpha should carry a failure; alpha=%d beta=%d", d.harnessFails[0], d.harnessFails[1])
	}
	if d.skipUntil[0].IsZero() || !time.Now().Before(d.skipUntil[0]) {
		t.Errorf("alpha should still be sitting out its back-off; skipUntil=%v", d.skipUntil[0])
	}
	for _, want := range []string{"sidelining this project", "/repos/alpha", "the OTHER projects keep draining", "non-retryable"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log line lost %q - the operator diagnoses this incident from the log alone:\n%s", want, logs.String())
		}
	}
}

// The SAME failure repeating must climb the ladder rather than retry every
// poll: the second consecutive bench is the 30m band, not another 15m and not
// an immediate respawn. The first back-off is force-elapsed mid-run (the hook
// mutates skipUntil on the loop's own goroutine, between passes) so the test
// never sleeps real time.
func TestRun_RepeatedHarnessFailureEscalatesTheLadder(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 5
	h := &fakeAdapter{steps: []invokeStep{
		{res: okResult(0, 0)},       // beta
		{res: nonRetryableResult()}, // alpha, failure 1 -> 15m
		{res: okResult(0, 0)},       // beta (alpha sitting out)
		{res: nonRetryableResult()}, // alpha again, failure 2 -> 30m
		{res: okResult(0, 0)},       // beta
	}}
	beta := &fakePoller{sum: backlog.Summary{Claimable: 2}}
	targets := []Target{
		{Name: "alpha", Poller: &fakePoller{sum: backlog.Summary{Claimable: 2}}, WorkDir: "/repos/alpha"},
		{Name: "beta", Poller: beta, WorkDir: "/repos/beta"},
	}
	d := NewMulti(cfg, h, targets)
	var started bool
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = started
	beta.onCall = func(i int) {
		// Fires during iteration 3's poll scan, before that pass selects a
		// target: alpha's 15m sit-out is declared over, so iteration 4 retries
		// it and earns failure #2.
		if i == 2 && d.skipUntil[0].After(time.Now()) {
			d.skipUntil[0] = time.Now().Add(-time.Millisecond)
		}
	}
	if err := d.Run(ctx); err != nil {
		t.Fatalf("escalation run returned an error: %v", err)
	}

	var seq []string
	for _, inv := range h.invocations {
		seq = append(seq, inv.WorkDir)
	}
	want := []string{"/repos/beta", "/repos/alpha", "/repos/beta", "/repos/alpha", "/repos/beta"}
	if strings.Join(seq, ",") != strings.Join(want, ",") {
		t.Fatalf("drains went %v, want %v", seq, want)
	}
	if d.harnessFails[0] != 2 {
		t.Fatalf("want two consecutive failures recorded; got %d", d.harnessFails[0])
	}
	until := time.Until(d.skipUntil[0])
	if until <= 25*time.Minute || until >= 31*time.Minute {
		t.Errorf("second failure should sit out the ~30m band; got %s", until.Round(time.Second))
	}
}

// Self-healing: after the operator fixes the config, the next attempt after the
// sit-out succeeds and the failure count resets - the target is back in the
// rotation for good, not one more try.
func TestRun_FixedHarnessRejoinsTheRotationAndResets(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 4
	logs := captureLogs(t)
	h := &fakeAdapter{steps: []invokeStep{
		{res: okResult(0, 0)},       // beta
		{res: nonRetryableResult()}, // alpha fails once
		{res: okResult(0, 0)},       // beta (alpha sitting out)
		{res: okResult(0, 0)},       // alpha, config fixed -> success
	}}
	beta := &fakePoller{sum: backlog.Summary{Claimable: 2}}
	targets := []Target{
		{Name: "alpha", Poller: &fakePoller{sum: backlog.Summary{Claimable: 2}}, WorkDir: "/repos/alpha"},
		{Name: "beta", Poller: beta, WorkDir: "/repos/beta"},
	}
	d := NewMulti(cfg, h, targets)
	beta.onCall = func(i int) {
		if i == 2 && d.skipUntil[0].After(time.Now()) {
			d.skipUntil[0] = time.Now().Add(-time.Millisecond)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("run returned an error: %v", err)
	}

	var seq []string
	for _, inv := range h.invocations {
		seq = append(seq, inv.WorkDir)
	}
	want := []string{"/repos/beta", "/repos/alpha", "/repos/beta", "/repos/alpha"}
	if strings.Join(seq, ",") != strings.Join(want, ",") {
		t.Fatalf("drains went %v, want %v", seq, want)
	}
	if d.harnessFails[0] != 0 || d.harnessErrs[0] != nil {
		t.Errorf("success must reset the failure state; fails=%d err=%v", d.harnessFails[0], d.harnessErrs[0])
	}
	if !strings.Contains(logs.String(), "rejoining the rotation") {
		t.Errorf("the rejoin should be visible in the log:\n%s", logs.String())
	}
}

// Every target broken: nothing left to drive, so the run exits saying so, and
// names EVERY project, its workdir and the cause underneath.
func TestRun_AllTargetsSidelinedEndsTheRunNamingEveryOne(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	h := &fakeAdapter{steps: []invokeStep{
		{res: nonRetryableResult()}, // beta first (rotation starts one past the cursor)
		{res: nonRetryableResult()}, // then alpha - nothing left
	}}
	targets := []Target{
		{Name: "alpha", Poller: &fakePoller{sum: backlog.Summary{Claimable: 2}}, WorkDir: "/repos/alpha"},
		{Name: "beta", Poller: &fakePoller{sum: backlog.Summary{Claimable: 2}}, WorkDir: "/repos/beta"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := NewMulti(cfg, h, targets).Run(ctx)
	if err == nil {
		t.Fatal("a fleet where every harness is failing must end the run")
	}
	for _, want := range []string{"nothing left to drive", "[alpha]", "[beta]", "/repos/alpha", "/repos/beta", "non-retryable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("run-wide error lost %q; got:\n%v", want, err)
		}
	}
	if h.invokeCalls != 2 {
		t.Errorf("sidelined targets must not be retried inside the run; spawned %d sessions", h.invokeCalls)
	}
}

// Regression for the classification itself: a drain error carrying one of the
// run-wide sentinels still hard-stops the WHOLE run with the sentinel intact -
// it must not be absorbed into a one-target sideline (whose aggregate message
// would lose the errors.Is chain).
func TestRun_RunWideSentinelsFromADrainStillHardStop(t *testing.T) {
	t.Run("ErrUnauthorized", func(t *testing.T) {
		cfg := fastCfg()
		cfg.StateDir = t.TempDir()
		h := &fakeAdapter{steps: []invokeStep{{err: fmt.Errorf("invoke claude: %w", backlog.ErrUnauthorized)}}}
		p := &fakePoller{sum: backlog.Summary{Claimable: 1}}
		err := runLoop(t, cfg, h, p)
		if err == nil {
			t.Fatal("expected the run to stop loudly")
		}
		if !errors.Is(err, backlog.ErrUnauthorized) {
			t.Errorf("sentinel must survive to the caller unwrapped-away; got %v", err)
		}
	})
	t.Run("ErrProjectRequired", func(t *testing.T) {
		cfg := fastCfg()
		cfg.StateDir = t.TempDir()
		h := &fakeAdapter{steps: []invokeStep{{err: fmt.Errorf("invoke claude: %w", backlog.ErrProjectRequired)}}}
		p := &fakePoller{sum: backlog.Summary{Claimable: 1}}
		err := runLoop(t, cfg, h, p)
		if err == nil {
			t.Fatal("expected the run to stop loudly")
		}
		if !errors.Is(err, backlog.ErrProjectRequired) {
			t.Errorf("sentinel must survive to the caller unwrapped-away; got %v", err)
		}
	})
}

// Blind mode has no polls to gate candidacy, so the rotation itself must skip a
// target sitting out its failure back-off - otherwise "sidelined" means nothing
// on a run without a wired poller.
func TestRun_BlindModeSkipsASidelinedTarget(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 4
	h := &fakeAdapter{steps: []invokeStep{
		{res: okResult(0, 0)},       // beta
		{res: nonRetryableResult()}, // alpha fails -> benched
		{res: okResult(0, 0)},       // beta
		{res: okResult(0, 0)},       // beta AGAIN: rotation skipped the sitting alpha
	}}
	targets := []Target{
		{Name: "alpha", Poller: &fakePoller{err: backlog.ErrNotWired}, WorkDir: "/repos/alpha"},
		{Name: "beta", Poller: &fakePoller{err: backlog.ErrNotWired}, WorkDir: "/repos/beta"},
	}
	d := NewMulti(cfg, h, targets)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("blind run returned an error: %v", err)
	}
	var seq []string
	for _, inv := range h.invocations {
		seq = append(seq, inv.WorkDir)
	}
	want := []string{"/repos/beta", "/repos/alpha", "/repos/beta", "/repos/beta"}
	if strings.Join(seq, ",") != strings.Join(want, ",") {
		t.Fatalf("blind rotation drained %v, want %v", seq, want)
	}
	if d.harnessFails[0] != 1 || d.harnessFails[1] != 0 {
		t.Errorf("failure state wrong: alpha=%d beta=%d", d.harnessFails[0], d.harnessFails[1])
	}
}
