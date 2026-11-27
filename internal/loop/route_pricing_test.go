package loop

// CLA-947: per-route pricing windows. A route inside one of its schedule's
// windows is unavailable for a NEW session exactly like a route carrying a usage
// mark; when every route is blocked the schedule's when_all_blocked decides
// whether the phase boundary holds the spawn or accepts the peak price.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/fleet"
)

const (
	routeZen       = "zen/free"
	routeGo        = "opencode-go/deepseek-v4.1-flash"
	routeSynthetic = "synthetic/hf:deepseek-ai/DeepSeek-V4.1-Flash-1017"
	routeCC        = "commandcode/deepseek/deepseek-v4.1-flash"
)

// peakRoutes attaches deepseek-peak to the routes given, through the real
// validation, so the fixture's loc is the parsed zone exactly as a loaded config
// has it.
func peakRoutes(t *testing.T, whenAllBlocked config.RouteWhenAllBlocked, routes ...string) (map[string]*config.RouteSchedule, map[string]string) {
	t.Helper()
	sched := &config.RouteSchedule{
		PauseWindows: config.PauseWindows{
			Timezone: "Australia/Brisbane",
			Windows: []config.PauseWindow{
				{Days: "mon-fri", From: "11:00", To: "14:00"},
				{Days: "mon-fri", From: "16:00", To: "20:00"},
			},
		},
		WhenAllBlocked: whenAllBlocked,
	}
	if err := sched.Validate(); err != nil {
		t.Fatalf("fixture schedule does not validate: %v", err)
	}
	windows := make(map[string]string, len(routes))
	for _, r := range routes {
		windows[r] = "deepseek-peak"
	}
	return map[string]*config.RouteSchedule{"deepseek-peak": sched}, windows
}

// TestRoutableRoutesAt_AppliesRouteWindowsAndMarks is the filter's table: a
// route inside a window never reaches the adapter, in configured order, under
// both policies and with usage marks layered on top. It covers a plain harness
// route list, a TIER list and a PHASE-PINNED route — all three resolve through
// ModelRoutesForPhase, so one table would miss which one was applied.
func TestRoutableRoutesAt_AppliesRouteWindowsAndMarks(t *testing.T) {
	// Monday 2026-11-09 12:00 is inside the peak schedule's first window; 09:00
	// is outside it.
	inside := brisbaneAt(t, 9, 12, 0)
	outside := brisbaneAt(t, 9, 9, 0)
	endOfWindow := brisbaneAt(t, 9, 14, 0)

	for _, tc := range []struct {
		name     string
		policy   config.RouteWhenAllBlocked
		windowed []string
		routes   []string
		ph       config.Phase                  // the phase's own route source
		tiers    map[string]config.ModelRoutes // the harness block's tier map
		dflt     config.ModelRoutes            // the harness block's default routes
		mark     []string                      // routes carrying a live usage mark
		now      time.Time
		want     []string
		wait     time.Time
	}{
		{
			name:   "outside the window nothing is filtered",
			policy: config.RouteWhenAllBlockedWait, windowed: []string{routeGo},
			dflt: config.ModelRoutes{routeZen, routeGo, routeSynthetic},
			now:  outside,
			want: []string{routeZen, routeGo, routeSynthetic},
		},
		{
			name:   "the peak route is skipped in configured order",
			policy: config.RouteWhenAllBlockedWait, windowed: []string{routeGo},
			dflt: config.ModelRoutes{routeZen, routeGo, routeSynthetic},
			now:  inside,
			want: []string{routeZen, routeSynthetic},
		},
		{
			name:   "a marked free route moves the start to synthetic, never Go",
			policy: config.RouteWhenAllBlockedWait, windowed: []string{routeGo},
			dflt: config.ModelRoutes{routeZen, routeGo, routeSynthetic},
			mark: []string{routeZen},
			now:  inside,
			want: []string{routeSynthetic},
		},
		{
			name:   "a TIER list is filtered the same way",
			policy: config.RouteWhenAllBlockedWait, windowed: []string{routeGo},
			ph:    config.Phase{Tier: "standard"},
			tiers: map[string]config.ModelRoutes{"standard": {routeZen, routeGo, routeSynthetic}},
			now:   inside,
			want:  []string{routeZen, routeSynthetic},
		},
		{
			name:   "a PHASE-PINNED route is the whole list",
			policy: config.RouteWhenAllBlockedWait, windowed: []string{routeGo, routeCC},
			ph:   config.Phase{Model: config.ModelRoutes{routeGo, routeCC}},
			now:  inside,
			want: nil,
			wait: endOfWindow,
		},
		{
			name:   "phase-pinned under run starts on the first route",
			policy: config.RouteWhenAllBlockedRun, windowed: []string{routeGo, routeCC},
			ph:   config.Phase{Model: config.ModelRoutes{routeGo, routeCC}},
			now:  inside,
			want: []string{routeGo, routeCC},
		},
		{
			name:   "every route windowed under wait reports the earliest end",
			policy: config.RouteWhenAllBlockedWait, windowed: []string{routeGo, routeCC},
			dflt: config.ModelRoutes{routeGo, routeCC},
			now:  inside,
			want: nil,
			wait: endOfWindow,
		},
		{
			name:   "under run a usage-marked route is still skipped",
			policy: config.RouteWhenAllBlockedRun, windowed: []string{routeGo, routeCC},
			dflt: config.ModelRoutes{routeGo, routeCC},
			mark: []string{routeGo},
			now:  inside,
			want: []string{routeCC},
		},
		{
			name:   "a marked route is never offered under wait either",
			policy: config.RouteWhenAllBlockedWait, windowed: []string{routeGo, routeZen},
			dflt: config.ModelRoutes{routeGo, routeZen},
			mark: []string{routeZen},
			now:  inside,
			want: nil,
			wait: endOfWindow,
		},
		// Review 1.review-1.3: a MARK only orders the candidates, a WINDOW removes
		// them. Every route marked and every one windowed under wait must hold —
		// starting on the marked route would pay the peak price the schedule
		// exists to avoid.
		{
			name:   "every route marked and windowed under wait holds for the window",
			policy: config.RouteWhenAllBlockedWait, windowed: []string{routeGo, routeCC},
			dflt: config.ModelRoutes{routeGo, routeCC},
			mark: []string{routeGo, routeCC},
			now:  inside,
			want: nil,
			wait: endOfWindow,
		},
		{
			name:   "every route marked: an open route wins over a windowed one",
			policy: config.RouteWhenAllBlockedWait, windowed: []string{routeGo},
			dflt: config.ModelRoutes{routeGo, routeSynthetic},
			mark: []string{routeGo, routeSynthetic},
			now:  inside,
			want: []string{routeSynthetic},
		},
		{
			name:   "every route marked and windowed under run still starts, on a marked route",
			policy: config.RouteWhenAllBlockedRun, windowed: []string{routeGo, routeCC},
			dflt: config.ModelRoutes{routeGo, routeCC},
			mark: []string{routeGo, routeCC},
			now:  inside,
			want: []string{routeGo},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := routedDriver(t, &fakeAdapter{}, nil)
			d.cfg.RouteSchedules, d.cfg.RouteWindows = peakRoutes(t, tc.policy, tc.windowed...)
			if tc.tiers != nil || tc.dflt != nil {
				d.cfg.Harnesses = map[string]config.HarnessConfig{
					"opencode": {Model: tc.dflt, Models: tc.tiers},
				}
			}
			routes, _ := d.cfg.ModelRoutesForPhase(tc.ph)
			for _, route := range tc.mark {
				d.markRouteLimited(route, tc.now.Add(time.Hour))
			}

			choice := d.routableRoutesAt(d.cfg, tc.now, routes)
			if len(choice.Routes) != len(tc.want) {
				t.Fatalf("routes = %v, want %v", choice.Routes, tc.want)
			}
			for i := range tc.want {
				if choice.Routes[i] != tc.want[i] {
					t.Fatalf("routes = %v, want %v", choice.Routes, tc.want)
				}
			}
			if tc.wait.IsZero() {
				if !choice.WaitUntil.IsZero() {
					t.Errorf("WaitUntil = %v, want none", choice.WaitUntil)
				}
			} else if !choice.WaitUntil.Equal(tc.wait) {
				t.Errorf("WaitUntil = %v, want %v", choice.WaitUntil, tc.wait)
			}
			// Under "wait" the blocked route is never in the answer, whatever
			// else happened; "run" is the one policy that offers it back.
			if tc.policy != config.RouteWhenAllBlockedRun {
				for _, route := range choice.Routes {
					if active, _ := d.cfg.RouteWindowAt(route, tc.now); active {
						t.Errorf("route %q is inside its window but was offered under wait", route)
					}
				}
			}
		})
	}
}

// A phase whose only routes are windowed under "wait" spawns NOTHING: the
// boundary holds, heartbeats any held claim, says when it resumes, and runs the
// phase when the clock passes the window.
func TestDrainPhases_RouteWindowHoldsTheSpawnUntilItEnds(t *testing.T) {
	clk := &pauseHoldClock{at: brisbaneAt(t, 9, 12, 0)}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)},
		{res: okResult(5, 0.05)},
	}}
	d := routedDriver(t, h, twoPhases(), routeGo, routeCC)
	d.cfg.RouteSchedules, d.cfg.RouteWindows = peakRoutes(t, config.RouteWhenAllBlockedWait, routeGo, routeCC)
	d.now = clk.Now
	d.leaseRenewalInterval = 2 * time.Millisecond
	// The two-phase fixture names no model, so the phase list is the harness
	// block's: window both routes.
	d.cfg.Harnesses = map[string]config.HarnessConfig{
		"opencode": {Model: config.ModelRoutes{routeGo, routeCC}},
	}
	logs := captureLogs(t)
	done := startPauseDrain(t, d)

	waitUntil(t, "the route-window hold at the implement boundary", func() bool {
		return strings.Contains(logs.String(), "route windows active until Mon 14:00 AEST")
	})
	if h.invokeCalls != 0 {
		t.Fatalf("a session spawned inside a pricing window: %d", h.invokeCalls)
	}
	out := logs.String()
	if !strings.Contains(out, "deepseek-peak mon-fri 11:00-14:00, mon-fri 16:00-20:00 (Australia/Brisbane)") {
		t.Errorf("the hold does not name the blocking schedule and its windows:\n%s", out)
	}
	if !strings.Contains(out, "holding the sequence at the implement boundary") {
		t.Errorf("the hold does not name the phase it is holding:\n%s", out)
	}

	clk.Set(brisbaneAt(t, 9, 14, 30))
	res := waitPauseDrain(t, done)
	if res.err != nil || res.stop {
		t.Fatalf("drain after the window: stop=%v err=%v", res.stop, res.err)
	}
	if h.invokeCalls != 2 {
		t.Fatalf("sessions after the window = %d, want 2", h.invokeCalls)
	}
	if got := h.invocations[0].Model; got != routeGo {
		t.Errorf("the first session started on %q, want %q", got, routeGo)
	}
	if !strings.Contains(logs.String(), "route windows ended — resuming the sequence at the implement phase") {
		t.Errorf("the leave line is missing:\n%s", logs.String())
	}
}

// when_all_blocked "run" starts on the first configured route and accepts the
// peak price — but a USAGE mark still takes a route out: only the window filter
// degrades.
func TestRouteWindows_RunStartsOnTheFirstRouteButHonoursMarks(t *testing.T) {
	at := brisbaneAt(t, 9, 12, 0)
	d := routedDriver(t, &fakeAdapter{steps: []invokeStep{{res: checkpointed(10, 0.1)}}}, nil, routeGo, routeCC)
	d.cfg.RouteSchedules, d.cfg.RouteWindows = peakRoutes(t, config.RouteWhenAllBlockedRun, routeGo, routeCC)
	d.now = func() time.Time { return at }

	choice := d.routableRoutesAt(d.cfg, at, []string{routeGo, routeCC})
	if len(choice.Routes) != 2 || choice.Routes[0] != routeGo {
		t.Fatalf("under run the windowed routes = %v, want both in order", choice.Routes)
	}

	d.markRouteLimited(routeGo, at.Add(time.Hour))
	choice = d.routableRoutesAt(d.cfg, at, []string{routeGo, routeCC})
	if len(choice.Routes) != 1 || choice.Routes[0] != routeCC {
		t.Fatalf("under run with the first route marked, routes = %v, want only %q — a window policy never overrides a mark", choice.Routes, routeCC)
	}
}

// A route with no schedule entry is always available, so adding a model cannot
// inherit a pause by accident.
func TestRoutableRoutesAt_UnattachedRouteIsAlwaysAvailable(t *testing.T) {
	at := brisbaneAt(t, 9, 12, 0)
	d := routedDriver(t, &fakeAdapter{}, nil)
	d.cfg.RouteSchedules, d.cfg.RouteWindows = peakRoutes(t, config.RouteWhenAllBlockedWait, routeGo)
	choice := d.routableRoutesAt(d.cfg, at, []string{routeGo, routeSynthetic})
	if len(choice.Routes) != 1 || choice.Routes[0] != routeSynthetic {
		t.Errorf("routes = %v, want only the unattached %q", choice.Routes, routeSynthetic)
	}
}

// The probe aims at the route the next session would start on, pricing windows
// applied — not at the windowed first route.
func TestProbeRouteFor_AppliesRouteWindows(t *testing.T) {
	at := brisbaneAt(t, 9, 12, 0)
	d := routedDriver(t, &fakeAdapter{}, twoPhases(), routeGo, routeSynthetic)
	d.cfg.RouteSchedules, d.cfg.RouteWindows = peakRoutes(t, config.RouteWhenAllBlockedWait, routeGo)
	d.now = func() time.Time { return at }
	if got := d.probeRouteFor(d.cfg, d.cfg.Phases[0]); got != routeSynthetic {
		t.Errorf("probe route = %q, want %q (the route this spawn would start on)", got, routeSynthetic)
	}
}

// Outside the window the same fixture is unchanged — the control that proves the
// window, not the fixture, moved the answer.
func TestRoutableRoutesAt_OutsideTheWindowKeepsTheFirstRoute(t *testing.T) {
	at := brisbaneAt(t, 9, 9, 0)
	d := routedDriver(t, &fakeAdapter{}, nil)
	d.cfg.RouteSchedules, d.cfg.RouteWindows = peakRoutes(t, config.RouteWhenAllBlockedWait, routeGo)
	if got := d.routableRoutesAt(d.cfg, at, []string{routeGo, routeSynthetic}); len(got.Routes) != 2 || got.Routes[0] != routeGo {
		t.Errorf("outside the window routes = %v, want both in order", got.Routes)
	}
}

// The invocation a phase builds names the first UNWINDOWED route.
func TestInvocationFor_StartsOnTheRouteOutsideItsWindow(t *testing.T) {
	at := brisbaneAt(t, 9, 12, 0)
	d := routedDriver(t, &fakeAdapter{}, nil, routeGo, routeSynthetic)
	d.cfg.RouteSchedules, d.cfg.RouteWindows = peakRoutes(t, config.RouteWhenAllBlockedWait, routeGo)
	d.now = func() time.Time { return at }
	phases := []config.Phase{{Name: "implement"}}
	inv, err := d.invocationFor(d.targets[0], 0, phases[0], nil, "", "", phases, nil, "")
	if err != nil {
		t.Fatalf("invocationFor: %v", err)
	}
	if inv.Model != routeSynthetic {
		t.Errorf("inv.Model = %q, want %q", inv.Model, routeSynthetic)
	}
	if len(inv.Routes) != 1 || inv.Routes[0] != routeSynthetic {
		t.Errorf("inv.Routes = %v, want only %q", inv.Routes, routeSynthetic)
	}
}

// And with every route windowed it refuses the spawn rather than starting on a
// route the operator paused.
func TestInvocationFor_RefusesWhenEveryRouteIsWindowed(t *testing.T) {
	at := brisbaneAt(t, 9, 12, 0)
	d := routedDriver(t, &fakeAdapter{}, nil, routeGo, routeCC)
	d.cfg.RouteSchedules, d.cfg.RouteWindows = peakRoutes(t, config.RouteWhenAllBlockedWait, routeGo, routeCC)
	d.now = func() time.Time { return at }
	phases := []config.Phase{{Name: "implement"}}
	_, err := d.invocationFor(d.targets[0], 0, phases[0], nil, "", "", phases, nil, "")
	if err == nil {
		t.Fatal("invocationFor built a spawn on a windowed route")
	}
	var blocked *routeWindowBlocked
	if !errors.As(err, &blocked) {
		t.Fatalf("error = %v, want *routeWindowBlocked", err)
	}
	if !blocked.until.Equal(brisbaneAt(t, 9, 14, 0)) {
		t.Errorf("blocked until %v, want 14:00", blocked.until)
	}
	if !strings.Contains(err.Error(), routeGo) {
		t.Errorf("the refusal does not name the blocked routes: %v", err)
	}
}

// Presence during a route-window hold names the end and the schedule's zone, as
// a daemon-wide schedule pause does — the console banner has one reason word for
// both, because both clear themselves.
func TestFleetState_RouteWindowHoldCarriesItsEndAndZone(t *testing.T) {
	d := routedDriver(t, &fakeAdapter{}, nil, routeGo)
	until := brisbaneAt(t, 9, 14, 0)
	d.schedPaused = true
	d.holdPause(0, "CB-7", "implement", until, "Australia/Brisbane", nil)

	st := d.fleetState(0)
	if st.Kind != fleet.StateDraining {
		t.Fatalf("state = %q, want draining", st.Kind)
	}
	if st.PauseReason != fleet.PauseReasonSchedule {
		t.Errorf("pause reason = %q, want %q", st.PauseReason, fleet.PauseReasonSchedule)
	}
	if !st.PauseUntil.Equal(until) {
		t.Errorf("pauseUntil = %v, want %v", st.PauseUntil, until)
	}
	if st.PauseZone != "Australia/Brisbane" {
		t.Errorf("pauseZone = %q, want the schedule's zone", st.PauseZone)
	}
	if st.TaskRef != "CB-7" || st.Phase != "implement" {
		t.Errorf("held state names %q at %q, want CB-7 at implement", st.TaskRef, st.Phase)
	}

	// A stale hold with no instant falls back to plain draining, so the row never
	// shows a reason with no resume time.
	d.holdPause(0, "CB-7", "implement", time.Time{}, "", nil)
	if st := d.fleetState(0); st.PauseReason != "" {
		t.Errorf("a hold with no instant reported reason %q, want plain draining", st.PauseReason)
	}
}

// CLA-947 review 1.review-1.1: the route-window hold re-reads the LIVE config at
// every wake, so a RELOAD that deletes the schedule ends the hold at once —
// without moving the clock and without waiting out a window the config no longer
// has. Pinning s.tc instead leaves the drain parked to the deleted window's end.
func TestDrainPhases_ReloadRemovingRouteWindowsEndsTheHold(t *testing.T) {
	clk := &pauseHoldClock{at: brisbaneAt(t, 9, 12, 0)} // inside 11:00-14:00
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)},
		{res: okResult(5, 0.05)},
	}}
	d := routedDriver(t, h, twoPhases(), routeGo, routeCC)
	d.cfg.RouteSchedules, d.cfg.RouteWindows = peakRoutes(t, config.RouteWhenAllBlockedWait, routeGo, routeCC)
	d.now = clk.Now
	d.leaseRenewalInterval = 2 * time.Millisecond
	d.SetReloader(func() (*config.Config, error) {
		fresh := d.cfg.Clone()
		fresh.RouteSchedules = nil
		fresh.RouteWindows = nil
		return fresh, nil
	})
	logs := captureLogs(t)
	done := startPauseDrain(t, d)

	waitUntil(t, "the route-window hold at the implement boundary", func() bool {
		return strings.Contains(logs.String(), "route windows active until Mon 14:00 AEST")
	})
	if h.invokeCalls != 0 {
		t.Fatalf("a session spawned inside a pricing window: %d", h.invokeCalls)
	}

	// The operator deletes the schedule and writes RELOAD. The clock does NOT
	// move: only the config changed, and that is enough to end the hold.
	writeMarker(t, d.state.Path(), MarkerReload, "")
	waitUntil(t, "the hold to end on the RELOAD alone", func() bool {
		return strings.Contains(logs.String(), "route windows ended — resuming the sequence at the implement phase")
	})

	res := waitPauseDrain(t, done)
	if res.err != nil || res.stop {
		t.Fatalf("drain after the RELOAD: stop=%v err=%v", res.stop, res.err)
	}
	if h.invokeCalls != 2 {
		t.Fatalf("sessions after the RELOAD = %d, want 2 (the schedule is gone, the window is not)", h.invokeCalls)
	}
}

// A probe is a paid session too: when every route of the phase is inside a "wait"
// window there is no route it may start on, so the probe is REFUSED rather than
// spawned on the phase's model — the peak price the schedule forbids (review
// 1.review-1.4).
func TestProbeInvocation_RefusesWhenEveryRouteIsWindowed(t *testing.T) {
	at := brisbaneAt(t, 9, 12, 0)
	d := routedDriver(t, &fakeAdapter{}, twoPhases(), routeGo, routeCC)
	d.cfg.RouteSchedules, d.cfg.RouteWindows = peakRoutes(t, config.RouteWhenAllBlockedWait, routeGo, routeCC)
	d.now = func() time.Time { return at }

	_, err := d.probeInvocation(d.targets[0], d.cfg, d.cfg.Phases[0], nil)
	var blocked *routeWindowBlocked
	if !errors.As(err, &blocked) {
		t.Fatalf("probeInvocation error = %v, want *routeWindowBlocked — a probe must not spawn on a windowed route", err)
	}
	if !blocked.until.Equal(brisbaneAt(t, 9, 14, 0)) {
		t.Errorf("probe refused until %v, want 14:00", blocked.until)
	}

	// A phase with ONE route gets the same answer: the single-route fallback used
	// to keep the model it always had, which here is the windowed route itself.
	single := routedDriver(t, &fakeAdapter{}, twoPhases(), routeGo)
	single.cfg.RouteSchedules, single.cfg.RouteWindows = peakRoutes(t, config.RouteWhenAllBlockedWait, routeGo)
	single.now = func() time.Time { return at }
	if _, err := single.probeInvocation(single.targets[0], single.cfg, single.cfg.Phases[0], nil); !errors.As(err, &blocked) {
		t.Errorf("single-route probe error = %v, want *routeWindowBlocked", err)
	}

	// Under "run" the peak price is accepted, so the probe runs on the route.
	run := routedDriver(t, &fakeAdapter{}, twoPhases(), routeGo, routeCC)
	run.cfg.RouteSchedules, run.cfg.RouteWindows = peakRoutes(t, config.RouteWhenAllBlockedRun, routeGo, routeCC)
	run.now = func() time.Time { return at }
	inv, err := run.probeInvocation(run.targets[0], run.cfg, run.cfg.Phases[0], nil)
	if err != nil {
		t.Fatalf("probe under run: %v, want a probe on the first route", err)
	}
	if inv.Model != routeGo {
		t.Errorf("probe under run aimed at %q, want %q", inv.Model, routeGo)
	}
}

// Review 1.review-1.13: the presence row's zone is the one whose schedule
// supplied the hold's instant, not the first active schedule's — with peaks in
// two zones the countdown must be rendered in the clock it counts to.
func TestRouteWindowHoldDetail_ZoneFollowsTheHoldInstant(t *testing.T) {
	now := brisbaneAt(t, 9, 12, 0) // Monday 02:00 UTC
	cfg := &config.Config{
		RouteSchedules: map[string]*config.RouteSchedule{},
		RouteWindows:   map[string]string{},
	}
	bris := &config.RouteSchedule{PauseWindows: config.PauseWindows{
		Timezone: "Australia/Brisbane",
		Windows:  []config.PauseWindow{{Days: "mon", From: "11:00", To: "14:00"}},
	}}
	if err := bris.Validate(); err != nil {
		t.Fatalf("fixture schedule: %v", err)
	}
	utc := &config.RouteSchedule{PauseWindows: config.PauseWindows{
		Timezone: "UTC",
		Windows:  []config.PauseWindow{{Days: "mon", From: "01:00", To: "05:00"}},
	}}
	if err := utc.Validate(); err != nil {
		t.Fatalf("fixture schedule: %v", err)
	}
	cfg.RouteSchedules["bris-peak"], cfg.RouteSchedules["utc-peak"] = bris, utc
	cfg.RouteWindows[routeGo], cfg.RouteWindows[routeCC] = "bris-peak", "utc-peak"
	lists := [][]string{{routeGo, routeCC}}

	// The Brisbane window ends first (14:00 AEST = 04:00 UTC), so it supplies the
	// hold's instant and its zone.
	detail, zone := routeWindowHoldDetail(cfg, lists, now, brisbaneAt(t, 9, 14, 0))
	if zone != "Australia/Brisbane" {
		t.Errorf("zone = %q, want the schedule whose end is the hold's instant (Australia/Brisbane)", zone)
	}
	for _, name := range []string{"bris-peak", "utc-peak"} {
		if !strings.Contains(detail, name) {
			t.Errorf("detail %q does not name the blocking schedule %q", detail, name)
		}
	}

	// The UTC window ends last (05:00 UTC = 15:00 AEST): its zone must win.
	if _, zone := routeWindowHoldDetail(cfg, lists, now, time.Date(2026, time.November, 9, 5, 0, 0, 0, time.UTC)); zone != "UTC" {
		t.Errorf("zone = %q, want UTC when the UTC window supplies the latest end", zone)
	}
}

// CLA-947 review 1.review-1.2: a PARALLEL panel whose windowed member is not the
// first holds the WHOLE panel. The per-phase boundary sees only member 1's open
// routes; without the fan-out hold, member 2 is refused inside invocationFor and
// booked as a reviewer that recorded nothing.
func TestDrainPhases_RouteWindowsHoldTheParallelPanelForALaterMember(t *testing.T) {
	repo, branch, tip := panelParallelRepo(t)
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented\n"}
	members := panelMembers() // three members: the fan-out really is parallel
	// The windowed member is the SECOND: the first member's routes are open, so
	// only a hold that looks at every member can see it.
	members[1].Model = config.ModelRoutes{routeGo}
	clk := &pauseHoldClock{at: brisbaneAt(t, 9, 12, 0)} // inside 11:00-14:00
	inner := newPanelParallelAdapter(rel, branch, tip, len(members))
	d, _ := panelParallelDriver(t, inner, rel, members, repo, tip, 0)
	d.cfg.RouteSchedules, d.cfg.RouteWindows = peakRoutes(t, config.RouteWhenAllBlockedWait, routeGo)
	d.now = clk.Now
	d.leaseRenewalInterval = 2 * time.Millisecond
	logs := captureLogs(t)

	done := startPauseDrain(t, d)
	waitUntil(t, "the hold at the panel boundary", func() bool {
		return strings.Contains(logs.String(), "holding t-1 at the review-1 boundary")
	})
	inner.mu.Lock()
	memberCalls := inner.reviewCalls
	inner.mu.Unlock()
	if memberCalls != 0 {
		t.Fatalf("the panel fan-out spawned %d member(s) while a later member was windowed", memberCalls)
	}
	if out := logs.String(); strings.Contains(out, "recording it as failed") {
		t.Errorf("a windowed member was booked as a failed reviewer:\n%s", out)
	}

	clk.Set(brisbaneAt(t, 9, 14, 30))
	res := waitPauseDrain(t, done)
	if res.err != nil || res.stop {
		t.Fatalf("drain after the window: stop=%v err=%v", res.stop, res.err)
	}
	inner.mu.Lock()
	memberCalls = inner.reviewCalls
	fixCalls := inner.fixCalls
	inner.mu.Unlock()
	if memberCalls != len(members) {
		t.Errorf("panel members run = %d, want %d after the window", memberCalls, len(members))
	}
	if fixCalls != 1 {
		t.Errorf("fix sessions = %d, want 1 after the panel", fixCalls)
	}
	if out := logs.String(); !strings.Contains(out, "route windows ended — resuming t-1 at the review-1 phase") {
		t.Errorf("the leave line is missing:\n%s", out)
	}
}

// CLA-947 review 1.review-1.6: the refusal a window opening between the boundary
// check and the spawn produces is its own outcome (windowHeld), never the
// evidence-gate `empty` shape that means a FAILED phase. A schedule must not be
// counted as a harness failure — that misclassification is what made a windowed
// panel member look like a broken reviewer.
func TestDrainPhase_RouteWindowRefusalIsHeldNotEmpty(t *testing.T) {
	at := brisbaneAt(t, 9, 12, 0)
	h := &fakeAdapter{}
	d := routedDriver(t, h, twoPhases(), routeGo, routeCC)
	d.cfg.RouteSchedules, d.cfg.RouteWindows = peakRoutes(t, config.RouteWhenAllBlockedWait, routeGo, routeCC)
	d.now = func() time.Time { return at }
	phases := d.cfg.EffectivePhases()

	_, _, _, end, err := d.drainPhase(context.Background(), 1, 0, 0, "", phases[0], true, nil, d.targets[0],
		spend{start: time.Now()}, phases, panelState{}, phaseRunOpts{})
	if err != nil {
		t.Fatalf("drainPhase: %v", err)
	}
	if h.invokeCalls != 0 {
		t.Fatalf("a session spawned on a windowed route: %d", h.invokeCalls)
	}
	if !end.windowHeld {
		t.Fatal("the refusal is not marked windowHeld")
	}
	if end.empty {
		t.Error("the refusal was booked as the evidence-gate empty shape (a FAILED phase)")
	}
	if end.dead || end.producedNothing {
		t.Errorf("the refusal was booked as a dead phase: dead=%v producedNothing=%v", end.dead, end.producedNothing)
	}
	if !strings.Contains(end.windowWhy, routeGo) {
		t.Errorf("windowWhy = %q, want it to name the blocked routes", end.windowWhy)
	}
}

// CLA-1026: the route-window hold publishes the same per-route breakdown a limit
// does, so the draining row names the pricing window that holds the routes WITH
// quota rather than only the usage limit that started the pause. A route shared
// by two panel members' lists appears once, in configured order.
func TestRouteWindowHoldState_CarriesPerRouteHolds(t *testing.T) {
	at := brisbaneAt(t, 9, 12, 0)
	d := routedDriver(t, &fakeAdapter{}, nil, routeGo, routeZen)
	d.cfg.RouteSchedules, d.cfg.RouteWindows = peakRoutes(t, config.RouteWhenAllBlockedWait, routeGo)
	d.now = func() time.Time { return at }
	reset := at.Add(time.Hour)
	d.markRouteLimited(routeZen, reset)

	seq := &drainSeq{d: d, t: d.targets[0], ti: 0}
	hs := seq.routeWindowHoldState([][]string{{routeGo, routeZen}, {routeGo}})
	if !hs.active {
		t.Fatal("the window hold is not active with every route withheld")
	}
	if want := brisbaneAt(t, 9, 14, 0); !hs.until.Equal(want) {
		t.Errorf("hold until = %s, want the window's end %s", hs.until, want)
	}
	if len(hs.routes) != 2 {
		t.Fatalf("hold routes = %+v, want the two configured routes once each", hs.routes)
	}
	if hs.routes[0].Route != routeGo || hs.routes[0].Reason != fleet.RouteHoldWindow || hs.routes[0].Schedule != "deepseek-peak" {
		t.Errorf("first hold = %+v, want the windowed %q with its schedule", hs.routes[0], routeGo)
	}
	if hs.routes[1].Route != routeZen || hs.routes[1].Reason != fleet.RouteHoldUsageLimit {
		t.Errorf("second hold = %+v, want the usage-limited %q", hs.routes[1], routeZen)
	}

	// The presence row is what the console reads: the hold state's routes ride the draining
	// beacon, and they are gone once the hold clears.
	d.schedPaused = true
	d.holdPause(0, "CB-7", "implement", hs.until, hs.zone, hs.routes)
	st := d.fleetState(0)
	if st.Kind != fleet.StateDraining || st.PauseReason != fleet.PauseReasonSchedule {
		t.Fatalf("state = %+v, want the schedule draining pause", st)
	}
	if len(st.Routes) != 2 || st.Routes[0].Route != routeGo || st.Routes[1].Route != routeZen {
		t.Errorf("draining state routes = %+v, want the hold's breakdown", st.Routes)
	}
	if st.TaskRef != "CB-7" || st.Phase != "implement" {
		t.Errorf("held state names %q at %q, want CB-7 at implement", st.TaskRef, st.Phase)
	}
	d.clearHoldPause(0)
	if st := d.fleetState(0); len(st.Routes) != 0 {
		t.Errorf("a cleared hold still reported routes: %+v", st.Routes)
	}
}

// review 1.review-1.2: a `when_all_blocked: run` schedule serves its routes AT PEAK PRICE, so a
// route inside its window can start and is not held. Reporting it as a window hold would replace
// the true "usage limit" sentence with a cause that is withholding nothing.
func TestRouteHoldsAt_RunWindowIsNotAHold(t *testing.T) {
	at := brisbaneAt(t, 9, 12, 0) // inside the 11:00-14:00 peak window
	d := routedDriver(t, &fakeAdapter{}, nil, routeGo, routeZen)
	d.cfg.RouteSchedules, d.cfg.RouteWindows = peakRoutes(t, config.RouteWhenAllBlockedRun, routeGo)
	d.now = func() time.Time { return at }
	d.markRouteLimited(routeZen, at.Add(time.Hour))

	holds := d.routeHoldsAt(d.cfg, at, []string{routeGo, routeZen})
	if len(holds) != 1 || holds[0].Route != routeZen || holds[0].Reason != fleet.RouteHoldUsageLimit {
		t.Fatalf("holds = %+v, want only %q usage_limit: the run-windowed %q can start at peak price", holds, routeZen, routeGo)
	}
}
