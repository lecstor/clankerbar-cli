package loop

// CLA-947: per-route pricing windows. A route inside one of its schedule's
// windows is unavailable for a NEW session exactly like a route carrying a usage
// mark; when every route is blocked the schedule's when_all_blocked decides
// whether the phase boundary holds the spawn or accepts the peak price.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
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
