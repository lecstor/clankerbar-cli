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
	go func() {
		time.Sleep(80 * time.Millisecond)
		if err := os.WriteFile(reloadPath, []byte("added an open route"), 0o644); err != nil {
			t.Errorf("planting RELOAD: %v", err)
		}
	}()

	logs := captureLogs(t)
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, stop := d.supervisedWait(ctx, harness.Limit{Limited: true}, h, d.cfg.Phases[0], routes, d.targets[0], spend{start: start})
	elapsed := time.Since(start)
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
