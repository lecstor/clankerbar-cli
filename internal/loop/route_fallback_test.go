package loop

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// CLA-782: the driver's per-run route marks. The adapter switches routes inside a
// session; these tests pin the half that makes the switch STICKY — a later phase
// of the same run starts on the route that worked, and goes back to the free one
// only after the mark expires.

const (
	routeFreeAlias = "free/route"
	routePaidAlias = "paid/route"
)

// routedDriver is phaseDriver with an opencode harness block carrying the route
// list, which is where a route list lives: the top-level model fields stay plain
// strings (the plane's schema).
func routedDriver(t *testing.T, h harness.Adapter, phases []config.Phase, routes ...string) *Driver {
	t.Helper()
	d, _ := phaseDriver(t, h, phases)
	d.cfg.Harness = "opencode"
	d.cfg.Harnesses = map[string]config.HarnessConfig{
		"opencode": {Model: config.ModelRoutes(routes)},
	}
	return d
}

// switchedResult is a phase result that also reports the adapter's route switch:
// the free route was denied and the paid one finished the session.
func switchedResult(res harness.Result, from, to string, resetAt time.Time) harness.Result {
	if res.Raw == nil {
		res.Raw = map[string]any{}
	}
	res.Raw[harness.RouteStartedKey] = from
	res.Raw[harness.RouteFinishedKey] = to
	res.Raw[harness.RouteFallbacksKey] = []harness.RouteFallback{{
		Route: from, Next: to, Reason: "usage_limit", ResetAt: resetAt,
	}}
	return res
}

// The sticky half: phase 1 is denied on the free route and finishes on the paid
// one; phase 2 of the SAME run must start on the paid one, not pay for the same
// denial again.
func TestDrainPhases_ARunDeniedOnItsFreeRouteStartsTheNextPhasePaid(t *testing.T) {
	reset := time.Now().Add(time.Hour)
	h := &fakeAdapter{steps: []invokeStep{
		{res: switchedResult(checkpointed(1, 0), routeFreeAlias, routePaidAlias, reset)},
		{res: okResult(1, 0)},
	}}
	d := routedDriver(t, h, twoPhases(), routeFreeAlias, routePaidAlias)
	d.cfg.PollInterval = config.Duration(time.Hour)
	logs := captureLogs(t)

	if _, _, _, err := drainPhasesOnce(t, d); err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	if len(h.invocations) != 2 {
		t.Fatalf("ran %d sessions, want 2", len(h.invocations))
	}
	if h.invocations[0].Model != routeFreeAlias {
		t.Errorf("phase 1 started on %q, want the configured first route %q", h.invocations[0].Model, routeFreeAlias)
	}
	if h.invocations[1].Model != routePaidAlias {
		t.Errorf("phase 2 started on %q, want the route that worked %q — the mark did not stick", h.invocations[1].Model, routePaidAlias)
	}
	if got := h.invocations[1].Routes; len(got) != 1 || got[0] != routePaidAlias {
		t.Errorf("phase 2 routes = %v, want only the paid route (a marked route is never offered again)", got)
	}

	// Both logs carry the switch: from, to, reason and when the denied route is
	// worth trying again.
	line := logs.String()
	if !strings.Contains(line, routeFreeAlias+" hit a usage limit (usage_limit) — resuming the session on "+routePaidAlias) {
		t.Errorf("daemon log missing the switch line; got:\n%s", line)
	}
	if !regexp.MustCompile(`retrying ` + regexp.QuoteMeta(routeFreeAlias) + ` after \d\d:\d\d`).MatchString(line) {
		t.Errorf("daemon log missing the retry time; got:\n%s", line)
	}
	// The spawn line names the route the session STARTED on; the done line adds
	// the route that finished it.
	if !strings.Contains(line, "finished on "+routePaidAlias) {
		t.Errorf("daemon log's done line does not name the finishing route; got:\n%s", line)
	}
	if !strings.Contains(line, "model="+routeFreeAlias) {
		t.Errorf("spawn line does not name the starting route; got:\n%s", line)
	}

	// And the iteration log (the session's own file) carries it too.
	var found bool
	for _, name := range iterationLogs(t, d.state.Path()) {
		data, err := os.ReadFile(filepath.Join(d.state.Path(), name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(data), "resuming the session on "+routePaidAlias) {
			found = true
		}
	}
	if !found {
		t.Error("no iteration log carries the route switch")
	}
}

// The mark lifts: the same run's next drain returns to the free route once the
// fallback interval has passed, and then keeps using it.
func TestRouteMarks_ReturnToTheFreeRouteAfterTheMarkExpires(t *testing.T) {
	// A stated ResetAt already in the past is the same statement as an expired
	// mark: the provider said this route is available again.
	expired := time.Now().Add(-time.Second)
	h := &fakeAdapter{steps: []invokeStep{
		{res: switchedResult(checkpointed(1, 0), routeFreeAlias, routePaidAlias, expired)},
		{res: okResult(1, 0)},
	}}
	d := routedDriver(t, h, twoPhases(), routeFreeAlias, routePaidAlias)

	if _, _, _, err := drainPhasesOnce(t, d); err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	if len(h.invocations) != 2 {
		t.Fatalf("ran %d sessions, want 2", len(h.invocations))
	}
	if h.invocations[1].Model != routeFreeAlias {
		t.Errorf("phase 2 started on %q, want the free route: its stated reset is already past", h.invocations[1].Model)
	}
}

// With no stated reset the mark lasts the supervised wait's probe interval
// (config `poll_interval`, 30 minutes by default), and the run is back on the
// free route after it. Uses a short configured interval so the expiry is
// observable without waiting the real default.
func TestRouteMarks_UnstatedResetUsesTheProbeIntervalThenExpires(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: switchedResult(checkpointed(1, 0), routeFreeAlias, routePaidAlias, time.Time{})},
		{res: okResult(1, 0)},
		{res: checkpointed(1, 0)},
		{res: okResult(1, 0)},
	}}
	d := routedDriver(t, h, twoPhases(), routeFreeAlias, routePaidAlias)
	d.cfg.PollInterval = config.Duration(80 * time.Millisecond)

	if _, _, _, err := drainPhasesOnce(t, d); err != nil {
		t.Fatalf("first drain: %v", err)
	}
	if h.invocations[1].Model != routePaidAlias {
		t.Fatalf("phase 2 started on %q, want the paid route while the free route's mark holds", h.invocations[1].Model)
	}

	time.Sleep(120 * time.Millisecond)
	if _, _, _, err := drainPhasesOnce(t, d); err != nil {
		t.Fatalf("second drain: %v", err)
	}
	if h.invocations[2].Model != routeFreeAlias {
		t.Errorf("the next drain started on %q after the mark expired, want the free route", h.invocations[2].Model)
	}
	if got := h.invocations[2].Routes; len(got) != 2 || got[0] != routeFreeAlias {
		t.Errorf("the expired mark did not restore the fallback chain: %v", got)
	}
}

// The mark duration's default, through the real recording path: no stated reset
// and no configured poll interval means 30 minutes.
func TestRouteMarks_UnstatedResetDefaultsToThirtyMinutes(t *testing.T) {
	d := routedDriver(t, &fakeAdapter{}, twoPhases(), routeFreeAlias, routePaidAlias)
	d.cfg.PollInterval = 0
	res := harness.Result{ExitCode: 0, Raw: map[string]any{
		harness.RouteStartedKey:  routeFreeAlias,
		harness.RouteFinishedKey: routePaidAlias,
		harness.RouteFallbacksKey: []harness.RouteFallback{{
			Route: routeFreeAlias, Next: routePaidAlias, Reason: "usage_limit",
		}},
	}}
	d.noteRouteFallbacks(d.targets[0], 1, harness.Invocation{}, res)

	until, ok := d.routeMarkExpiry(routeFreeAlias)
	if !ok {
		t.Fatal("the fallback reported no mark for the denied route")
	}
	if left := time.Until(until); left < 29*time.Minute || left > 31*time.Minute {
		t.Errorf("mark expires in %s, want about 30 minutes", left)
	}
}

// Mark application, directly: unmarked routes keep configured order, a marked
// route is dropped, and when EVERY route is marked the one that lifts first is
// used (a phase must still start rather than wait on a mark).
func TestRoutableRoutes_AppliesMarks(t *testing.T) {
	d := routedDriver(t, &fakeAdapter{}, nil, routeFreeAlias, routePaidAlias)
	now := time.Now()

	if got := d.routableRoutes([]string{routeFreeAlias, routePaidAlias}); len(got) != 2 || got[0] != routeFreeAlias || got[1] != routePaidAlias {
		t.Fatalf("unmarked routes = %v, want both in order", got)
	}

	d.markRouteLimited(routeFreeAlias, now.Add(time.Hour))
	if got := d.routableRoutes([]string{routeFreeAlias, routePaidAlias}); len(got) != 1 || got[0] != routePaidAlias {
		t.Errorf("with the free route marked, routes = %v, want only the paid one", got)
	}

	// Every route marked: the earliest expiry wins, alone.
	d.markRouteLimited(routePaidAlias, now.Add(10*time.Minute))
	if got := d.routableRoutes([]string{routeFreeAlias, routePaidAlias}); len(got) != 1 || got[0] != routePaidAlias {
		t.Errorf("all routes marked = %v, want the earliest-expiring route alone", got)
	}

	// A later report wins over an earlier one: shortening a mark on a stale
	// report would send the next phase back into the same limit.
	d.markRouteLimited(routePaidAlias, now.Add(5*time.Minute))
	if until, _ := d.routeMarkExpiry(routePaidAlias); until.Sub(now) < 9*time.Minute {
		t.Errorf("a shorter mark replaced the later one: expires in %s", time.Until(until))
	}

	// An expired mark is no mark at all. A fresh driver, because the later-wins
	// rule above deliberately keeps the 1h mark on the same field.
	fresh := routedDriver(t, &fakeAdapter{}, nil, routeFreeAlias, routePaidAlias)
	fresh.markRouteLimited(routeFreeAlias, now.Add(-time.Minute))
	if got := fresh.routableRoutes([]string{routeFreeAlias, routePaidAlias}); len(got) != 2 || got[0] != routeFreeAlias {
		t.Errorf("an expired mark still excluded its route: %v", got)
	}
}

// A denied LAST route is marked (so the next phase does not waste a run on it)
// but is not logged as a switch — there was none, and the existing limit branch
// owns that session's ending.
func TestNoteRouteFallbacks_DeniedLastRouteIsMarkedButNotLoggedAsASwitch(t *testing.T) {
	d := routedDriver(t, &fakeAdapter{}, nil, routeFreeAlias, routePaidAlias)
	logs := captureLogs(t)
	res := harness.Result{Raw: map[string]any{
		harness.RouteStartedKey:  routeFreeAlias,
		harness.RouteFinishedKey: routePaidAlias,
		harness.RouteFallbacksKey: []harness.RouteFallback{
			{Route: routeFreeAlias, Next: routePaidAlias, Reason: "usage_limit"},
			{Route: routePaidAlias, Reason: "usage_limit"},
		},
	}}
	d.noteRouteFallbacks(d.targets[0], 1, harness.Invocation{}, res)

	if _, ok := d.routeMarkExpiry(routePaidAlias); !ok {
		t.Error("the denied last route was not marked; the next phase would retry it for nothing")
	}
	if got := logs.String(); strings.Count(got, "resuming the session") != 1 {
		t.Errorf("want exactly the one real switch logged, got:\n%s", got)
	}
}

// The brief's {{model}} names the route the session actually STARTS on, not the
// config's first route: a review's record block carries `model: "<alias>"`, and
// the plane would otherwise attribute a paid-route session to the free one.
func TestInvocationFor_BriefNamesTheRoutableStartRoute(t *testing.T) {
	d := routedDriver(t, &fakeAdapter{}, nil, routeFreeAlias, routePaidAlias)
	d.markRouteLimited(routeFreeAlias, time.Now().Add(time.Hour))

	prev := checkpointed(1, 0)
	ph := config.Phase{Name: config.ReviewPhaseName, Prompt: config.ReviewOnlyBrief()}
	phases := []config.Phase{{Name: "implement"}, ph, {Name: "fix"}}
	inv, err := d.invocationFor(d.targets[0], 1, ph, &prev, "", "", phases)
	if err != nil {
		t.Fatalf("invocationFor: %v", err)
	}
	if inv.Model != routePaidAlias {
		t.Fatalf("inv.Model = %q, want the routable route %q", inv.Model, routePaidAlias)
	}
	// The review's attribution lines read "reviewed by: harness <h>, model <m>";
	// THIS phase's line must name the route it actually starts on.
	if !strings.Contains(inv.Prompt, "reviewed by: harness opencode, model "+routePaidAlias) {
		t.Errorf("the brief's own attribution does not name the session's starting route %q", routePaidAlias)
	}
}

// The supervised-wait probe tests the route the next session would start on,
// marks applied - not the harness block's first route (review finding
// 1.review-1.3). A single-route phase leaves the probe's model alone.
func TestProbeRouteFor_AimsAtTheRouteTheNextSessionStartsOn(t *testing.T) {
	d := routedDriver(t, &fakeAdapter{}, twoPhases(), routeFreeAlias, routePaidAlias)
	ph := d.cfg.Phases[0]
	if got := d.probeRouteFor(d.cfg, ph); got != routeFreeAlias {
		t.Errorf("unmarked: probe route = %q, want %q", got, routeFreeAlias)
	}
	d.markRouteLimited(routeFreeAlias, time.Now().Add(2*time.Hour))
	d.markRouteLimited(routePaidAlias, time.Now().Add(time.Hour))
	if got := d.probeRouteFor(d.cfg, ph); got != routePaidAlias {
		t.Errorf("both marked: probe route = %q, want %q, the route whose mark lifts first", got, routePaidAlias)
	}

	single := routedDriver(t, &fakeAdapter{}, twoPhases(), routeFreeAlias)
	if got := single.probeRouteFor(single.cfg, single.cfg.Phases[0]); got != "" {
		t.Errorf("single route: probe route = %q, want \"\" (the probe keeps its historical model)", got)
	}
}

// The probe Invocation the supervised wait spawns is the one probeRouteFor aims:
// with every route marked it probes the route whose mark lifts first, and a
// single-route phase keeps the harness block's model.
func TestProbeInvocation_UsesTheProbeRoute(t *testing.T) {
	d := routedDriver(t, &fakeAdapter{}, twoPhases(), routeFreeAlias, routePaidAlias)
	d.markRouteLimited(routeFreeAlias, time.Now().Add(2*time.Hour))
	d.markRouteLimited(routePaidAlias, time.Now().Add(time.Hour))
	inv, err := d.probeInvocation(Target{}, d.cfg, d.cfg.Phases[0])
	if err != nil {
		t.Fatalf("probeInvocation: %v", err)
	}
	if !inv.Probe || inv.Model != routePaidAlias {
		t.Errorf("probe invocation = {Probe: %v, Model: %q}, want a probe on %q", inv.Probe, inv.Model, routePaidAlias)
	}

	single := routedDriver(t, &fakeAdapter{}, twoPhases(), routeFreeAlias)
	single.markRouteLimited(routeFreeAlias, time.Now().Add(time.Hour))
	inv, err = single.probeInvocation(Target{}, single.cfg, single.cfg.Phases[0])
	if err != nil {
		t.Fatalf("probeInvocation (single): %v", err)
	}
	if inv.Model != routeFreeAlias {
		t.Errorf("single route: probe model = %q, want the harness block's %q", inv.Model, routeFreeAlias)
	}
}
