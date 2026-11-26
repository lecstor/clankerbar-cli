package loop

// CLA-1024: a Stop-class limit (budget, credits, 402, a monthly cap) has no reset to poll for, so the
// route it was reported on is out until the operator acts. In a multi-route phase that sidelines the
// route and the wait carries on with the routes still in play; it ends the run only when NOTHING is
// left, and a single-route phase keeps the immediate LIMIT_STOP it always had.

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// stopLimit is the Limit a Stop-class report carries, for the fixtures that sideline a route directly.
func stopLimit() harness.Limit {
	return harness.Limit{Limited: true, Stop: true, Reason: "out of credits"}
}

// The incident shape: routes [zen, go], go inside its 11:00-14:00 pricing window, so the session runs
// on zen and zen says the account behind it is out of credits. That is ONE route's answer: it must be
// sidelined (never re-probed) while the wait aims at go's window end, and the phase must resume on go
// when the window opens — with no LIMIT_STOP in between.
func TestDrainPhase_StopOnOneRouteSidelinesItAndWaitsForTheWindowedOne(t *testing.T) {
	const (
		pollInterval = 60 * time.Millisecond
		windowEnd    = 400 * time.Millisecond // from the drain's start
		grace        = 20 * time.Millisecond
	)
	windowUntil := brisbaneAt(t, 9, 14, 0) // the peak schedule's 11:00-14:00 window
	start := time.Now()
	base := windowUntil.Add(-windowEnd)

	h := &fakeAdapter{
		steps: []invokeStep{{res: limitStopResult()}, {res: okResult(5, 0.05)}},
		probeLimitFor: func(_ int, model string) harness.Limit {
			if model == routeGo {
				return harness.Limit{Limited: false}
			}
			return harness.Limit{Limited: true}
		},
	}
	d := routedDriver(t, h, []config.Phase{{Name: config.ImplementPhaseName}}, routeZen, routeGo)
	d.cfg.RouteSchedules, d.cfg.RouteWindows = peakRoutes(t, config.RouteWhenAllBlockedWait, routeGo)
	d.cfg.PollInterval = config.Duration(pollInterval)
	d.now = func() time.Time { return base.Add(time.Since(start)) }
	d.waitGrace = grace

	logs := captureLogs(t)
	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("a Stop on one of two routes must not end the run: stop=%v err=%v\n%s", stop, err, logs.String())
	}

	if len(h.invocations) != 2 {
		t.Fatalf("spawned %d sessions, want the stopped one and the respawn after the window opened", len(h.invocations))
	}
	if got := h.invocations[0].Model; got != routeZen {
		t.Fatalf("the first session ran on %q, want %q (the route outside its window)", got, routeZen)
	}
	if got := h.invocations[1].Model; got != routeGo {
		t.Errorf("the phase resumed on %q, want %q — the wait must resume on the route whose window opened", got, routeGo)
	}

	if present, reason := d.readMarker(LimitStopName); present {
		t.Errorf("%s was written (%q) while a route was still windowed: one route's Stop is not the phase's verdict", LimitStopName, reason)
	}
	if _, stopped := d.routeStopped(routeZen); !stopped {
		t.Error("the stopped route was not sidelined: it will be spawned on or probed again")
	}
	if slices.Contains(h.probeModels, routeZen) {
		t.Errorf("probes ran on %v — a Stop-blocked route must never be re-probed before a RELOAD/restart", h.probeModels)
	}
	if len(h.probeModels) != 1 || h.probeModels[0] != routeGo {
		t.Errorf("probes ran on %v, want exactly one on %q once its window opened", h.probeModels, routeGo)
	}
	first := h.probeStarts[0].Sub(start)
	if first < windowEnd-50*time.Millisecond {
		t.Errorf("the probe ran %s after the drain began, before the window's end at %s", first.Round(time.Millisecond), windowEnd)
	}
	if first > windowEnd+300*time.Millisecond {
		t.Errorf("the probe ran %s after the drain began, want ~%s (the window's end) — not the %s interval grid\n%s",
			first.Round(time.Millisecond), windowEnd, pollInterval, logs.String())
	}
	if !strings.Contains(logs.String(), "sidelining that route until RELOAD or restart") {
		t.Errorf("the sidelining is not said out loud, so an operator cannot tell why the route is not used:\n%s", logs.String())
	}
}

// Every configured route answers Stop (none windowed, none rolling-limited): there is nothing left to
// wait for, so the wait writes LIMIT_STOP and reports stop — once per route, never in a loop.
func TestSupervisedWait_EveryRouteStopBlockedWritesLimitStop(t *testing.T) {
	cfg := fastCfg()
	cfg.Phases = []config.Phase{{Name: config.ImplementPhaseName, Model: config.ModelRoutes{routeZen, routeGo}, Prompt: "Work."}}
	cfg.Prompt = ""
	cfg.PollInterval = config.Duration(10 * time.Millisecond)
	h := &fakeAdapter{probeLimitFor: func(int, string) harness.Limit { return stopLimit() }}
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller()}})
	openTestStateDir(t, d)

	routes, _ := cfg.ModelRoutesForPhase(cfg.Phases[0])
	start := time.Now()
	logs := captureLogs(t)
	_, _, stop := d.supervisedWait(context.Background(), harness.Limit{Limited: true}, h, cfg.Phases[0], routes, d.targets[0], spend{start: start})
	if !stop {
		t.Fatalf("every route is Stop-blocked; the wait must report stop\n%s", logs.String())
	}
	present, reason := d.readMarker(LimitStopName)
	if !present {
		t.Fatalf("%s was not written for a phase whose every route is Stop-blocked\n%s", LimitStopName, logs.String())
	}
	if !strings.Contains(reason, "out of credits") || !strings.Contains(reason, routeZen) {
		t.Errorf("marker line = %q, want the provider reason and the route that reported it", reason)
	}
	if h.probeCalls != 2 {
		t.Errorf("got %d probes, want exactly 2 — one per route: a Stop-blocked route must never be re-probed\n%s", h.probeCalls, logs.String())
	}
	for _, route := range []string{routeZen, routeGo} {
		if _, ok := d.routeStopped(route); !ok {
			t.Errorf("%s carries no Stop sideline after its Stop verdict", route)
		}
	}
}

// probeWake's own half: with no sleep between wakes, a walk over two Stop-blocked routes ends the wake
// as a stop rather than reporting "still limited" for a wait with nothing to wait for.
func TestProbeWake_EveryRouteStopBlockedEndsTheWake(t *testing.T) {
	h := &fakeAdapter{probeLimitFor: func(int, string) harness.Limit { return stopLimit() }}
	d := routedDriver(t, h, []config.Phase{{Name: config.ImplementPhaseName}}, routeZen, routeGo)

	_, _, verdict, _ := d.probeWake(context.Background(), h, d.targets[0], d.cfg, d.cfg.Phases[0], spend{start: time.Now()})
	if verdict != wakeStop {
		t.Fatalf("verdict = %v, want wakeStop: no configured route can lift a budget exhaustion", verdict)
	}
	if present, _ := d.readMarker(LimitStopName); !present {
		t.Errorf("%s was not written by the wake that found every route Stop-blocked", LimitStopName)
	}
	if h.probeCalls != 2 {
		t.Errorf("got %d probes, want one per route and no repeats", h.probeCalls)
	}
}

// The single-route phase keeps its old ending: one route returning Stop writes LIMIT_STOP and stops
// the run at once — no wait, no probe, and no attempt to poll a dead subscription.
func TestDrainPhase_SingleRouteStopStillWritesLimitStopImmediately(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{{res: limitStopResult()}}}
	d := routedDriver(t, h, []config.Phase{{Name: config.ImplementPhaseName}}, routeZen)
	logs := captureLogs(t)

	start := time.Now()
	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || !stop {
		t.Fatalf("a single-route Stop must end the run immediately: stop=%v err=%v\n%s", stop, err, logs.String())
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("the stop took %s — a single-route Stop must not wait for anything", elapsed.Round(time.Millisecond))
	}
	present, reason := d.readMarker(LimitStopName)
	if !present {
		t.Fatalf("%s was not written for the single-route Stop\n%s", LimitStopName, logs.String())
	}
	if !strings.Contains(reason, routeZen) || !strings.Contains(reason, "out of credits") {
		t.Errorf("marker line = %q, want the route and the provider's reason", reason)
	}
	if h.probeCalls != 0 {
		t.Errorf("got %d probes, want none: there was no reset to wait for", h.probeCalls)
	}
}

// The same shape one level down: a supervised wait entered on a single-route phase whose probe reports
// Stop must stop, write the marker and not keep polling.
func TestSupervisedWait_SingleRouteStopProbeWritesLimitStop(t *testing.T) {
	cfg := fastCfg()
	cfg.Phases = []config.Phase{{Name: config.ImplementPhaseName, Model: config.ModelRoutes{routeZen}, Prompt: "Work."}}
	cfg.Prompt = ""
	cfg.PollInterval = config.Duration(10 * time.Millisecond)
	h := &fakeAdapter{probeLimitFor: func(int, string) harness.Limit { return stopLimit() }}
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller()}})
	openTestStateDir(t, d)

	routes, _ := cfg.ModelRoutesForPhase(cfg.Phases[0])
	logs := captureLogs(t)
	_, _, stop := d.supervisedWait(context.Background(), harness.Limit{Limited: true}, h, cfg.Phases[0], routes, d.targets[0], spend{start: time.Now()})
	if !stop {
		t.Fatalf("the only route is Stop-blocked; the wait must report stop\n%s", logs.String())
	}
	if present, _ := d.readMarker(LimitStopName); !present {
		t.Fatalf("%s was not written for the single-route probe Stop\n%s", LimitStopName, logs.String())
	}
	if h.probeCalls != 1 {
		t.Errorf("got %d probes, want exactly 1 — the wait must not come back to a dead subscription", h.probeCalls)
	}
}

// A RELOAD is the operator's retry: the Stop sideline it carried is cleared, so the next probe or
// spawn may try the route again. Marks survive, because a config edit does not lift the provider's own
// timed statement.
func TestApplyReload_ClearsStopSidelinesButKeepsMarks(t *testing.T) {
	d := New(fastCfg(), &fakeAdapter{}, &fakePoller{})
	openTestStateDir(t, d)
	d.stopRoute(routeZen, stopLimit())
	d.markRouteLimited(routeGo, time.Now().Add(time.Hour))
	fresh := fastCfg()
	d.reloadConfig = func() (*config.Config, error) { return fresh, nil }

	logs := captureLogs(t)
	d.applyReload()

	if _, stopped := d.routeStopped(routeZen); stopped {
		t.Error("the RELOAD did not clear the Stop sideline: the operator's retry would be ignored")
	}
	if _, marked := d.routeMarkExpiry(routeGo); !marked {
		t.Error("the RELOAD cleared a usage MARK: a config edit does not lift the provider's timed statement")
	}
	if out := logs.String(); !strings.Contains(out, "RELOAD cleared the Stop sideline on "+routeZen) {
		t.Errorf("the cleared sideline is not named in the log:\n%s", out)
	}
}
