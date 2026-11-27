package loop

// CLA-1024: two things the wait used to ignore. A daemon-wide pause_windows window is a "spawn
// nothing" schedule, and a probe is a paid session: the wait spawns none for the window's duration.
// And a RELOAD is the operator saying the config changed, so the wait re-reads it and re-aims its
// deadline at once instead of sleeping out the interval the old config set.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// pauseUntilWindow is a validated daemon-wide pause window, named so the fixture reads as the
// schedule it is rather than as a bare struct.
func pauseUntilWindow(t *testing.T, from, to string) *config.PauseWindows {
	t.Helper()
	pw := &config.PauseWindows{
		Timezone: "Australia/Brisbane",
		Windows:  []config.PauseWindow{{Days: "mon-fri", From: from, To: to}},
	}
	if err := pw.Validate(); err != nil {
		t.Fatalf("fixture pause windows do not validate: %v", err)
	}
	return pw
}

// The wait is entered inside an 11:00-12:00 pause window that ends 400ms later, with a 40ms poll
// interval. A wait that probed on its ordinary cadence would spawn ~10 paid sessions inside the
// window; the probe must land only once the window has ended.
func TestSupervisedWait_DaemonPauseWindowSpawnsNoProbesAndResumesAfter(t *testing.T) {
	const (
		pollInterval = 40 * time.Millisecond
		windowEnd    = 400 * time.Millisecond // from the wait's start
	)
	pw := pauseUntilWindow(t, "11:00", "12:00")
	start := time.Now()
	base := brisbaneAt(t, 9, 12, 0).Add(-windowEnd) // inside the window, ending `windowEnd` from now

	cfg := fastCfg()
	cfg.Phases = []config.Phase{{Name: config.ImplementPhaseName, Model: config.ModelRoutes{routeZen}, Prompt: "Work."}}
	cfg.Prompt = ""
	cfg.PollInterval = config.Duration(pollInterval)
	cfg.PauseWindows = pw
	h := &fakeAdapter{probeLimitFor: func(int, string) harness.Limit { return harness.Limit{Limited: false} }}
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller()}})
	openTestStateDir(t, d)
	d.now = func() time.Time { return base.Add(time.Since(start)) }

	routes, _ := cfg.ModelRoutesForPhase(cfg.Phases[0])
	logs := captureLogs(t)
	_, _, stop := d.supervisedWait(context.Background(), harness.Limit{Limited: true}, h, cfg.Phases[0], routes, d.targets[0], spend{start: start})
	if stop {
		t.Fatalf("the pause ended and the probe lifted the limit; the wait should resume\n%s", logs.String())
	}
	if h.probeCalls != 1 {
		t.Fatalf("got %d probes, want the single one after the window ended\n%s", h.probeCalls, logs.String())
	}
	first := h.probeStarts[0].Sub(start)
	if first < windowEnd-50*time.Millisecond {
		t.Errorf("a probe spawned %s into the pause window: %d poll intervals were paid for during a schedule that says spawn nothing\n%s",
			first.Round(time.Millisecond), int(first/pollInterval)+1, logs.String())
	}
	if !strings.Contains(logs.String(), "spawning no probes until it ends") {
		t.Errorf("the pause holding the probes is not said out loud:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "scheduled pause ended — resuming limit probes") {
		t.Errorf("the pause ending is not said out loud:\n%s", logs.String())
	}
}

// Gap 5: the wait is parked on a marked route with a 10s interval, the operator adds an unblocked
// route and writes RELOAD. The wait must re-read the live config and probe the new route at once —
// not sleep out the old timer, and not spend a probe on the route it already knows is limited.
func TestSupervisedWait_ReloadUnblocksTheWaitWithoutTheOldInterval(t *testing.T) {
	const pollInterval = 10 * time.Second
	h := &fakeAdapter{probeLimitFor: func(_ int, model string) harness.Limit {
		if model == routeGo {
			return harness.Limit{Limited: false}
		}
		return harness.Limit{Limited: true}
	}}
	d := routedDriver(t, h, []config.Phase{{Name: config.ImplementPhaseName}}, routeZen)
	d.cfg.PollInterval = config.Duration(pollInterval)
	d.waitChunk = 5 * time.Millisecond
	d.waitGrace = 10 * time.Millisecond
	// The only route is limited for hours: without a RELOAD the wait's next deadline is the interval.
	d.markRouteLimited(routeZen, time.Now().Add(4*time.Hour))

	// The operator's edit: a second, open route, in the same harness block the phase resolves from.
	next := d.cfg.Clone()
	hc := next.Harnesses["opencode"]
	hc.Model = config.ModelRoutes{routeZen, routeGo}
	next.Harnesses["opencode"] = hc
	d.reloadConfig = func() (*config.Config, error) { return next, nil }

	routes, _ := d.cfg.ModelRoutesForPhase(d.cfg.Phases[0])
	reloadPath := filepath.Join(d.state.Path(), MarkerReload)
	writeErr := make(chan error, 1)
	go func() {
		time.Sleep(80 * time.Millisecond)
		// Report through the channel, not t.Errorf: a failed assertion returns before this write,
		// and a t.Errorf after the test has finished panics as a log-after-test.
		writeErr <- os.WriteFile(reloadPath, []byte("added an open route"), 0o644)
	}()

	logs := captureLogs(t)
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, stop := d.supervisedWait(ctx, harness.Limit{Limited: true}, h, d.cfg.Phases[0], routes, d.targets[0], spend{start: start})
	elapsed := time.Since(start)
	if err := <-writeErr; err != nil {
		t.Fatalf("planting RELOAD: %v", err)
	}
	if stop {
		t.Fatalf("the new route probed open; the wait should resume, not stop\n%s", logs.String())
	}
	if elapsed >= 3*time.Second {
		t.Errorf("the wait took %s — the RELOAD must re-aim the deadline, not sleep out the %s interval", elapsed.Round(time.Millisecond), pollInterval)
	}
	if want := []string{routeGo}; !slices.Equal(h.probeModels, want) {
		t.Errorf("probes ran on %v, want %v: the reloaded config's open route, and no probe against the marked one\n%s",
			h.probeModels, want, logs.String())
	}
	if out := logs.String(); !strings.Contains(out, "RELOAD requested during wait") {
		t.Errorf("the wait did not act on the RELOAD marker:\n%s", out)
	}
}

// review 1.review-1.4: a RELOAD that lands while the wait is parked inside a daemon-wide pause must
// end the pause chunk and re-aim the deadline at once. The new config removes the window, so nothing
// may sleep out the old poll interval before obeying the operator's edit.
func TestSupervisedWait_ReloadInsideAPauseEndsItAndReAimsTheDeadline(t *testing.T) {
	const pollInterval = 10 * time.Second
	start := time.Now()
	at := brisbaneAt(t, 9, 11, 30) // inside 11:00-12:00, an hour of fake time from its end

	cfg := fastCfg()
	cfg.Phases = []config.Phase{{Name: config.ImplementPhaseName, Model: config.ModelRoutes{routeZen}, Prompt: "Work."}}
	cfg.Prompt = ""
	cfg.PollInterval = config.Duration(pollInterval)
	cfg.PauseWindows = pauseUntilWindow(t, "11:00", "12:00")
	h := &fakeAdapter{probeLimitFor: func(int, string) harness.Limit { return harness.Limit{Limited: false} }}
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller()}})
	openTestStateDir(t, d)
	d.now = func() time.Time { return at.Add(time.Since(start)) }
	d.waitChunk = 5 * time.Millisecond

	// The operator's edit: the pause is gone. The wait is certainly parked here, so it is the
	// pause-gate chunk that must notice the RELOAD.
	next := cfg.Clone()
	next.PauseWindows = nil
	d.reloadConfig = func() (*config.Config, error) { return next, nil }

	reloadPath := filepath.Join(d.state.Path(), MarkerReload)
	writeErr := make(chan error, 1)
	go func() {
		time.Sleep(80 * time.Millisecond)
		writeErr <- os.WriteFile(reloadPath, []byte("pause removed"), 0o644)
	}()

	routes, _ := cfg.ModelRoutesForPhase(cfg.Phases[0])
	logs := captureLogs(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, stop := d.supervisedWait(ctx, harness.Limit{Limited: true}, h, cfg.Phases[0], routes, d.targets[0], spend{start: start})
	elapsed := time.Since(start)
	if err := <-writeErr; err != nil {
		t.Fatalf("planting RELOAD: %v", err)
	}
	if stop {
		t.Fatalf("the reloaded config removed the pause and the probe lifted; the wait should resume, not stop\n%s", logs.String())
	}
	if elapsed >= 3*time.Second {
		t.Errorf("the wait took %s — a RELOAD inside the pause must re-aim the deadline, not sleep out the %s interval", elapsed.Round(time.Millisecond), pollInterval)
	}
	if want := []string{routeZen}; !slices.Equal(h.probeModels, want) {
		t.Errorf("probes ran on %v, want %v", h.probeModels, want)
	}
	out := logs.String()
	if !strings.Contains(out, "RELOAD requested during wait") {
		t.Errorf("the wait did not act on the RELOAD marker:\n%s", out)
	}
	if !strings.Contains(out, "scheduled pause ended — resuming limit probes") {
		t.Errorf("the reloaded-away pause is not reported as ended:\n%s", out)
	}
}

// review 1.review-1.5: Gap 4's "advances into" shape. The wait starts just BEFORE a pause window
// opens and its first walk probe runs across the opening; the walk's mid-walk pause guard must stop
// it rather than spawn a second paid probe inside the window. The window end then lets the wait
// probe the remaining route. Without the guard the second probe starts inside the window.
func TestSupervisedWait_AdvancesIntoAPauseWindowAndSpawnsNoProbeInside(t *testing.T) {
	const pollInterval = 20 * time.Millisecond
	windowStart := brisbaneAt(t, 9, 12, 0)
	windowEnd := brisbaneAt(t, 9, 13, 0)
	base := windowStart.Add(-500 * time.Millisecond)
	var clockNanos atomic.Int64
	clockNanos.Store(base.UnixNano())

	cfg := fastCfg()
	cfg.Phases = []config.Phase{{Name: config.ImplementPhaseName, Model: config.ModelRoutes{routeZen, routeGo}, Prompt: "Work."}}
	cfg.Prompt = ""
	cfg.PollInterval = config.Duration(pollInterval)
	cfg.PauseWindows = pauseUntilWindow(t, "12:00", "13:00")
	h := &fakeAdapter{
		probeDelay: 50 * time.Millisecond,
		probeLimitFor: func(_ int, model string) harness.Limit {
			if model == routeZen {
				// A stated reset far out: the mark must outlast the pause, so the second wake
				// probes the other route rather than coming back to this one.
				return harness.Limit{Limited: true, ResetAt: time.Now().Add(time.Hour)}
			}
			return harness.Limit{Limited: false}
		},
	}
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller()}})
	openTestStateDir(t, d)
	d.now = func() time.Time { return time.Unix(0, clockNanos.Load()) }
	d.waitGrace = 10 * time.Millisecond
	d.waitChunk = 5 * time.Millisecond

	var probeClocks []time.Time
	var pausedAtProbe []bool
	h.onProbe = func(int) {
		probeClocks = append(probeClocks, d.now())
		paused, _, _ := d.pauseState()
		pausedAtProbe = append(pausedAtProbe, paused)
	}

	routes, _ := cfg.ModelRoutesForPhase(cfg.Phases[0])
	// The window opens while the first probe is running, and ends once the walk has given up and
	// the pause gate is holding.
	go func() {
		time.Sleep(10 * time.Millisecond)
		clockNanos.Store(windowStart.Add(time.Second).UnixNano())
		time.Sleep(140 * time.Millisecond)
		clockNanos.Store(windowEnd.UnixNano())
	}()

	logs := captureLogs(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, stop := d.supervisedWait(ctx, harness.Limit{Limited: true}, h, cfg.Phases[0], routes, d.targets[0], spend{start: time.Now()})
	if stop {
		t.Fatalf("the second probe lifted the limit; the wait should resume, not stop\n%s", logs.String())
	}
	if want := []string{routeZen, routeGo}; !slices.Equal(h.probeModels, want) {
		t.Fatalf("probes ran on %v, want %v: the walk must stop at the window's opening and resume after it\n%s",
			h.probeModels, want, logs.String())
	}
	if len(probeClocks) != 2 {
		t.Fatalf("recorded %d probe starts, want 2", len(probeClocks))
	}
	if !probeClocks[0].Before(windowStart) {
		t.Errorf("the first probe started at %s, want before the window opens at %s", probeClocks[0].Format(time.RFC3339), windowStart.Format(time.RFC3339))
	}
	if probeClocks[1].Before(windowEnd) {
		t.Errorf("the second probe started at %s, INSIDE the pause window (%s-%s): the mid-walk pause guard did not hold it\n%s",
			probeClocks[1].Format(time.RFC3339), windowStart.Format(time.RFC3339), windowEnd.Format(time.RFC3339), logs.String())
	}
	for i, paused := range pausedAtProbe {
		if paused {
			t.Errorf("probe %d spawned while a daemon-wide pause held", i+1)
		}
	}
	if !strings.Contains(logs.String(), "scheduled pause active — not probing further in this wake") {
		t.Errorf("the mid-walk pause guard is not said out loud:\n%s", logs.String())
	}
}
