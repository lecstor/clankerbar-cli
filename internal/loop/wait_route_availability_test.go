package loop

// CLA-1024: the supervised wait's entry sleep takes the route wake hint, and its beacon reports what
// the wait is actually waiting for. Both were successor-only or static before: a wait entered at 13:45
// with another route windowed until 14:00 slept a full poll interval, and the console showed
// "usage limit - probing for a reset" while the truth was "the peak window ends at 14:00".

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// The incident's entry shape: the unwindowed route is limited with no stated reset, another route
// is windowed until T (T well inside the poll interval), and the wait is entered at 13:45 with NO
// stated reset of its own. The first probe must land at T + grace, on the route that just opened —
// not an interval later on a dead route (Gap 1).
func TestSupervisedWait_EntrySleepTakesTheRouteWakeHint(t *testing.T) {
	const (
		interval  = 2500 * time.Millisecond
		grace     = 20 * time.Millisecond
		windowEnd = 400 * time.Millisecond // from the wait's start
	)
	windowUntil := brisbaneAt(t, 9, 14, 0) // the peak schedule's 11:00-14:00 window
	start := time.Now()
	base := windowUntil.Add(-windowEnd)

	h := &fakeAdapter{probeLimitFor: func(_ int, model string) harness.Limit {
		if model == routeGo {
			return harness.Limit{Limited: false}
		}
		return harness.Limit{Limited: true}
	}}
	d := routedDriver(t, h, twoPhases(), routeFreeAlias, routeGo)
	d.cfg.RouteSchedules, d.cfg.RouteWindows = peakRoutes(t, config.RouteWhenAllBlockedWait, routeGo)
	d.cfg.PollInterval = config.Duration(interval)
	d.now = func() time.Time { return base.Add(time.Since(start)) }
	d.waitGrace = grace
	// The unwindowed route is marked with no stated reset: the mark's own interval (30m from the real
	// clock) is far beyond the poll interval, so only the Go route's window end can pull the wake in.
	d.markRouteLimited(routeFreeAlias, base.Add(time.Hour))

	logs := captureLogs(t)
	_, _, stop := d.supervisedWait(context.Background(),
		harness.Limit{Limited: true}, h, d.cfg.Phases[0], nil, d.targets[0], spend{start: start})
	elapsed := time.Since(start)

	if stop {
		t.Fatalf("the windowed route probed open at its window's end; the wait should resume, not stop\n%s", logs.String())
	}
	if want := []string{routeGo}; !slices.Equal(h.probeModels, want) {
		t.Fatalf("probes ran on %v, want %v: the first wake must probe the route that just opened, not the marked one\n%s",
			h.probeModels, want, logs.String())
	}
	first := h.probeStarts[0].Sub(start)
	if first > windowEnd+300*time.Millisecond {
		t.Errorf("the first probe began %s after entry, want ~%s (the route window's end) — the entry sleep ignored the route wake hint\n%s",
			first.Round(time.Millisecond), windowEnd, logs.String())
	}
	if first < windowEnd-50*time.Millisecond {
		t.Errorf("the first probe began %s after entry, before the window's end at %s: the wait must sleep out the window, not probe early\n%s",
			first.Round(time.Millisecond), windowEnd, logs.String())
	}
	if elapsed >= interval {
		t.Errorf("the wait took %s, want ~%s — the entry sleep used the %s poll interval instead of the window's end\n%s",
			elapsed.Round(time.Millisecond), (windowEnd + grace).Round(time.Millisecond), interval, logs.String())
	}
}

// withRouteMarks marks routes at the driver's own clock and returns that clock's instant, so a
// fixture's marks and its windows share one timeline (a mark stamped from the real clock would read
// as already expired to a fake one).
func withRouteMarks(t *testing.T, d *Driver, at time.Time, marks map[string]time.Duration) {
	t.Helper()
	for route, in := range marks {
		d.markRouteLimited(route, at.Add(in))
	}
}

// Gap 2: every route is limited with no stated reset of its own, and the earliest thing that can
// change is a route PRICING window ending. That is a schedule pause with a known resume instant, and
// the console must show it (draining/schedule/pauseUntil+zone) rather than an open-ended limit probe.
func TestSupervisedWait_BeaconsDrainingWhenTheEarliestWakeIsARouteWindowEnd(t *testing.T) {
	at := brisbaneAt(t, 9, 12, 0) // inside the 11:00-14:00 peak window
	windowEnd := brisbaneAt(t, 9, 14, 0)
	cfg := fastCfg()
	cfg.Phases = []config.Phase{{Name: "implement", Model: config.ModelRoutes{routeZen, routeGo}, Prompt: "Work."}}
	cfg.Prompt = ""
	cfg.PollInterval = config.Duration(20 * time.Millisecond)
	cfg.RouteSchedules, cfg.RouteWindows = peakRoutes(t, config.RouteWhenAllBlockedWait, routeGo)
	h := &fakeAdapter{probeLimitFor: func(int, string) harness.Limit { return harness.Limit{Limited: true} }}
	ff := &fakeFleet{}
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Fleet: ff}})
	openTestStateDir(t, d)
	d.now = func() time.Time { return at }
	d.limitBeaconInterval = time.Hour // only the entry beacon is in scope
	// The Go route's window is what lifts LAST for it (mark sooner, window later), so its clear
	// instant is a WINDOW end. Zen's mark outlasts that window end, so the Go window is the earliest
	// unblocking event of the whole phase.
	withRouteMarks(t, d, at, map[string]time.Duration{routeGo: 30 * time.Minute, routeZen: 4 * time.Hour})

	routes, _ := cfg.ModelRoutesForPhase(cfg.Phases[0])
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	_, _, stop := d.supervisedWait(ctx, harness.Limit{Limited: true}, h, cfg.Phases[0], routes, d.targets[0], spend{start: time.Now()})
	if !stop {
		t.Fatal("the context bounded the wait; it should report stop")
	}

	sent, _ := ff.reports()
	if len(sent) == 0 {
		t.Fatal("the wait sent no beacon")
	}
	entry := sent[0].State
	if entry.Kind != fleet.StateDraining {
		t.Fatalf("entry beacon kind = %q, want %q — the earliest unblocking event is a route window's end, so this is a schedule pause\n%+v",
			entry.Kind, fleet.StateDraining, entry)
	}
	if entry.PauseReason != fleet.PauseReasonSchedule {
		t.Errorf("entry beacon pauseReason = %q, want %q", entry.PauseReason, fleet.PauseReasonSchedule)
	}
	if !entry.PauseUntil.Equal(windowEnd) {
		t.Errorf("entry beacon pauseUntil = %s, want the window's end %s", entry.PauseUntil.Format(time.RFC3339), windowEnd.Format(time.RFC3339))
	}
	if entry.PauseZone != "Australia/Brisbane" {
		t.Errorf("entry beacon pauseZone = %q, want the schedule's zone so the console renders the operator's clock", entry.PauseZone)
	}
	if len(entry.Routes) != 2 {
		t.Errorf("entry beacon routes = %+v, want both held routes (CLA-1026 breakdown rides along)", entry.Routes)
	}
}

// Gap 2(a), the other half: a daemon-wide pause_windows window is in force when the wait starts. No
// probe spawns during it, so the beacon must report the schedule's resume instant, not a limit probe.
func TestSupervisedWait_BeaconsDrainingDuringADaemonWidePause(t *testing.T) {
	at := brisbaneAt(t, 9, 12, 0) // inside 11:00-14:00
	cfg := fastCfg()
	cfg.Phases = []config.Phase{{Name: "implement", Model: config.ModelRoutes{routeZen}, Prompt: "Work."}}
	cfg.Prompt = ""
	cfg.PollInterval = config.Duration(20 * time.Millisecond)
	cfg.PauseWindows = peakSchedule(t)
	h := &fakeAdapter{probeLimitFor: func(int, string) harness.Limit { return harness.Limit{Limited: true} }}
	ff := &fakeFleet{}
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Fleet: ff}})
	openTestStateDir(t, d)
	d.now = func() time.Time { return at }
	d.limitBeaconInterval = time.Hour

	routes, _ := cfg.ModelRoutesForPhase(cfg.Phases[0])
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	if _, _, stop := d.supervisedWait(ctx, harness.Limit{Limited: true}, h, cfg.Phases[0], routes, d.targets[0], spend{start: time.Now()}); !stop {
		t.Fatal("the context bounded the wait; it should report stop")
	}

	sent, _ := ff.reports()
	if len(sent) == 0 {
		t.Fatal("the wait sent no beacon")
	}
	entry := sent[0].State
	if entry.Kind != fleet.StateDraining || entry.PauseReason != fleet.PauseReasonSchedule {
		t.Fatalf("entry beacon = %+v, want draining with the schedule reason while a daemon-wide pause holds", entry)
	}
	if want := brisbaneAt(t, 9, 14, 0); !entry.PauseUntil.Equal(want) {
		t.Errorf("entry beacon pauseUntil = %s, want the pause window's end %s", entry.PauseUntil.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if entry.PauseZone != "Australia/Brisbane" {
		t.Errorf("entry beacon pauseZone = %q, want the pause schedule's zone", entry.PauseZone)
	}
}

// Gap 2(b): with every route rolling-limited and no window in play, the beacon is `limited` carrying
// the EARLIEST concrete mark, and it refreshes when that instant changes across wakes. The entry
// beacon also proves the zero case: a route with no hold at all is "genuinely probing", so resetAt is
// the wait's own stated reset (here: none, so zero).
func TestSupervisedWait_BeaconsLimitedWithTheEarliestConcreteReset(t *testing.T) {
	cfg := fastCfg()
	cfg.Phases = []config.Phase{{Name: "implement", Model: config.ModelRoutes{routeZen}, Prompt: "Work."}}
	cfg.Prompt = ""
	cfg.PollInterval = config.Duration(60 * time.Millisecond)
	h := &fakeAdapter{probeLimitFor: func(int, string) harness.Limit {
		// Every probe states a NEW reset an interval out: the beacon must follow the mark the probe
		// sets, not a snapshot taken at entry.
		return harness.Limit{Limited: true, ResetAt: time.Now().Add(time.Second)}
	}}
	ff := &fakeFleet{}
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Fleet: ff}})
	openTestStateDir(t, d)
	d.limitBeaconInterval = 5 * time.Millisecond

	routes, _ := cfg.ModelRoutesForPhase(cfg.Phases[0])
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	defer cancel()
	if _, _, stop := d.supervisedWait(ctx, harness.Limit{Limited: true}, h, cfg.Phases[0], routes, d.targets[0], spend{start: start}); !stop {
		t.Fatal("the context bounded the wait; it should report stop")
	}

	sent, _ := ff.reports()
	if len(sent) < 2 {
		t.Fatalf("got %d beacons, want the entry one plus a refresh after the first wake", len(sent))
	}
	entry := sent[0].State
	if entry.Kind != fleet.StateLimited {
		t.Fatalf("entry beacon kind = %q, want %q: the one route is open (no mark, no window) so the wait is probing it\n%+v",
			entry.Kind, fleet.StateLimited, entry)
	}
	if !entry.ResetAt.IsZero() {
		t.Errorf("entry beacon resetAt = %s, want zero — the wait stated no reset and the route carries no hold yet",
			entry.ResetAt.Format(time.RFC3339))
	}

	// After the first probe the route carries a mark whose expiry is the provider's stated reset
	// (about a second out); a refresh must carry that mark instead of the entry snapshot.
	var refreshed fleet.State
	for _, r := range sent[1:] {
		if r.State.Kind == fleet.StateLimited && !r.State.ResetAt.IsZero() {
			refreshed = r.State
		}
	}
	if refreshed.Kind == "" {
		t.Fatalf("no refresh carried a concrete reset after the probe marked the route: %+v", sent)
	}
	if until := time.Until(refreshed.ResetAt); until < 500*time.Millisecond {
		t.Errorf("refreshed beacon resetAt = %s (%s out), want the mark the probe set (~1s out) — the beacon kept its entry snapshot",
			refreshed.ResetAt.Format(time.RFC3339), until.Round(time.Millisecond))
	}
}

// review 1.review-1.9: two routes reaching the SAME clear instant — one by a usage mark, one by a
// withholding window — must read as a schedule pause, whatever order the routes came in. Before the
// tie-break the answer depended on which route was visited first.
func TestWaitAvailability_AnEqualInstantFromAWindowIsASchedulePause(t *testing.T) {
	at := brisbaneAt(t, 9, 12, 0)
	windowEnd := brisbaneAt(t, 9, 14, 0)
	d := routedDriver(t, &fakeAdapter{}, []config.Phase{{Name: "implement"}}, routeZen, routeGo)
	d.cfg.RouteSchedules, d.cfg.RouteWindows = peakRoutes(t, config.RouteWhenAllBlockedWait, routeGo)
	d.now = func() time.Time { return at }
	// The mark and the window end at the same instant; zen is FIRST in the list, and carries no window.
	d.markRouteLimited(routeZen, windowEnd)
	d.markRouteLimited(routeGo, windowEnd.Add(-time.Minute))

	av := d.waitAvailabilityAt(d.cfg, at, []string{routeZen, routeGo})
	if av.open {
		t.Fatalf("availability = %+v, want blocked: both routes carry a future instant", av)
	}
	if av.scheduleAt.IsZero() || !av.scheduleAt.Equal(windowEnd) {
		t.Errorf("scheduleAt = %s, want the shared instant %s — an equal instant reached as a window end is a schedule pause",
			av.scheduleAt.Format(time.RFC3339), windowEnd.Format(time.RFC3339))
	}
	if av.scheduleZone != "Australia/Brisbane" {
		t.Errorf("scheduleZone = %q, want the withholding schedule's zone", av.scheduleZone)
	}
}
