package loop

// CLA-986: a supervised limit wait must wake at the instant a route it could not use becomes
// usable — a pricing window ending, or a route mark's own reset — instead of sleeping out the rest
// of the poll interval with an open route sitting idle. It also pins what a window-blocked refusal
// NAMES: only the routes a window actually withheld, never a route the walk probed and marked.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// logLineContaining returns the first log line carrying want, or "" when none does. The log
// assertions below read one line each out of a captured stream.
func logLineContaining(logs, want string) string {
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, want) {
			return l
		}
	}
	return ""
}

// The incident (headless clanker2, 2026-11-24): Synthetic hit a limit, Command Code and Go were
// inside the deepseek-peak window until 14:00, and the wake that found them windowed slept a full
// 30-minute interval. The window ended at 14:00 and the next probe was 14:29, so a usable route
// sat idle for 29 minutes. Here the window ends at T, the limited route keeps probing limited, and
// the wait must come back at T (plus grace) — not at the interval deadline — and resume on the
// windowed route once T has passed.
func TestSupervisedWait_WakesAtTheRouteWindowEndNotTheProbeInterval(t *testing.T) {
	const (
		interval  = 2500 * time.Millisecond
		grace     = 20 * time.Millisecond
		firstWake = 150 * time.Millisecond // a stated reset that gets the wait to its first wake
		windowEnd = 900 * time.Millisecond // the window's end, measured from the wait's start
	)
	windowUntil := brisbaneAt(t, 9, 14, 0) // the peak schedule's 11:00-14:00 window
	start := time.Now()
	base := windowUntil.Add(-windowEnd)

	h := &fakeAdapter{probeLimitFor: func(_ int, model string) harness.Limit {
		if model == routeFreeAlias {
			return harness.Limit{Limited: true}
		}
		return harness.Limit{Limited: false}
	}}
	d := routedDriver(t, h, twoPhases(), routeFreeAlias, routeGo)
	d.cfg.RouteSchedules, d.cfg.RouteWindows = peakRoutes(t, config.RouteWhenAllBlockedWait, routeGo)
	d.cfg.PollInterval = config.Duration(interval)
	// The driver's clock tracks real elapsed time from a point `windowEnd` before the window's end,
	// so the window filter, the walk and the wake hint all see the same edge the sleep must aim at.
	d.now = func() time.Time { return base.Add(time.Since(start)) }
	d.waitGrace = grace

	logs := captureLogs(t)
	_, _, stop := d.supervisedWait(context.Background(),
		harness.Limit{Limited: true, ResetAt: start.Add(firstWake)},
		h, d.cfg.Phases[0], nil, d.targets[0], spend{start: start})
	elapsed := time.Since(start)

	if stop {
		t.Fatalf("the window opened and the route probed open; the wait should resume, not stop\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "window-blocked") {
		t.Fatalf("the fixture never reached the window-blocked walk this test is about:\n%s", logs.String())
	}
	if len(h.probeModels) != 2 || h.probeModels[0] != routeFreeAlias || h.probeModels[1] != routeGo {
		t.Fatalf("probes ran on %v, want [%s %s]: the wait must resume on the windowed route once its window ends\n%s",
			h.probeModels, routeFreeAlias, routeGo, logs.String())
	}
	// The successor deadline is the window's end, not the interval measured from the last attempt.
	// Probe 1 lands at ~firstWake+grace and probe 2 at ~windowEnd+grace, so the gap between the two
	// is windowEnd - firstWake; 250ms either side is scheduling headroom on a loaded -race machine,
	// and the buggy interval is 2.5s away.
	gap := h.probeStarts[1].Sub(h.probeStarts[0])
	want := windowEnd - firstWake
	if gap > want+250*time.Millisecond {
		t.Errorf("the second probe began %s after the first, want ~%s (the window's end, T=%s from the start) — not the interval deadline at %s\n%s",
			gap.Round(time.Millisecond), want.Round(time.Millisecond), windowEnd, interval, logs.String())
	}
	if gap < want-250*time.Millisecond {
		t.Errorf("the second probe began %s after the first, want ~%s: the wait must sleep out the window's remaining time, not re-probe early\n%s",
			gap.Round(time.Millisecond), want.Round(time.Millisecond), logs.String())
	}
	if elapsed >= interval {
		t.Errorf("the wait took %s, want the window's end at %s — not the interval deadline at %s\n%s",
			elapsed.Round(time.Millisecond), (windowEnd + grace).Round(time.Millisecond), interval, logs.String())
	}
}

// A walk can end with a route already clear: route A's short mark expires while route B's probe is
// still running, so the next probeInvocation aims at A again and the walk stops at tried[A]. The
// wake hint only answers with a FUTURE instant, so the wait used to fall back to the whole interval
// with A routable now. It must wake one grace out and resume on A (CLA-988, review 1.review-1.1).
func TestSupervisedWait_WakesAtGraceWhenTheWalkLeavesARouteClear(t *testing.T) {
	const (
		interval   = 3 * time.Second
		grace      = 200 * time.Millisecond
		firstWake  = 100 * time.Millisecond
		probeDelay = 150 * time.Millisecond
	)
	h := &fakeAdapter{
		probeDelay: probeDelay,
		probeLimitFor: func(call int, _ string) harness.Limit {
			switch call {
			case 0: // A: a short stated reset, which expires while B is being probed
				return harness.Limit{Limited: true, ResetAt: time.Now().Add(probeDelay / 2)}
			case 1: // B: limited with no stated reset
				return harness.Limit{Limited: true}
			default: // A again: it cleared while the walk moved on
				return harness.Limit{Limited: false}
			}
		},
	}
	d := routedDriver(t, h, twoPhases(), routeFreeAlias, routeGo)
	d.cfg.PollInterval = config.Duration(interval)
	d.waitGrace = grace

	logs := captureLogs(t)
	start := time.Now()
	_, _, stop := d.supervisedWait(context.Background(),
		harness.Limit{Limited: true, ResetAt: start.Add(firstWake)},
		h, d.cfg.Phases[0], nil, d.targets[0], spend{start: start})
	elapsed := time.Since(start)

	if stop {
		t.Fatalf("the cleared route probed open; the wait should resume, not stop\n%s", logs.String())
	}
	if want := []string{routeFreeAlias, routeGo, routeFreeAlias}; !slices.Equal(h.probeModels, want) {
		t.Fatalf("probes ran on %v, want %v: once A's mark expired mid-wake the wait must resume on A\n%s",
			h.probeModels, want, logs.String())
	}
	// The successor deadline is one grace after the wake's last attempt (B's probe) — not the
	// interval. 250ms of scheduling headroom above; the lower bound catches a re-probe that fires
	// before the grace sleep has elapsed. The buggy interval is 3s away.
	gap := h.probeStarts[2].Sub(h.probeStarts[1])
	want := probeDelay + grace
	if gap > want+250*time.Millisecond {
		t.Errorf("the probe after a cleared route began %s after the wake's last attempt started, want ~%s — a route clear NOW must not wait the %s interval\n%s",
			gap.Round(time.Millisecond), want.Round(time.Millisecond), interval, logs.String())
	}
	if gap < want-50*time.Millisecond {
		t.Errorf("the probe after a cleared route began %s after the wake's last attempt started, want ~%s — it re-probed before the grace sleep elapsed\n%s",
			gap.Round(time.Millisecond), want.Round(time.Millisecond), logs.String())
	}
	if elapsed >= interval {
		t.Errorf("the wait took %s, want ~%s — the cleared route must be re-probed one grace out, not an interval later\n%s",
			elapsed.Round(time.Millisecond), (firstWake + 3*probeDelay + grace).Round(time.Millisecond), logs.String())
	}
}

// A wake ending on a probe error marks nothing, so its route can still be clear NOW. Waking at grace
// there would re-spawn the failing probe in a tight loop: an error keeps the successor deadline at
// the full interval (CLA-988).
func TestSupervisedWait_ProbeErrorKeepsTheFullInterval(t *testing.T) {
	const (
		interval = 400 * time.Millisecond
		grace    = 20 * time.Millisecond
	)
	h := &fakeAdapter{probeErrs: []error{errors.New("probe spawn refused")}}
	d := routedDriver(t, h, twoPhases(), routeFreeAlias, routeGo)
	d.cfg.PollInterval = config.Duration(interval)
	d.waitGrace = grace

	logs := captureLogs(t)
	start := time.Now()
	_, _, stop := d.supervisedWait(context.Background(),
		harness.Limit{Limited: true},
		h, d.cfg.Phases[0], nil, d.targets[0], spend{start: start})

	if stop {
		t.Fatalf("a probe error falls back to polling; it must not stop the wait\n%s", logs.String())
	}
	if h.probeCalls != 2 {
		t.Fatalf("got %d probes, want 2 (the failed lap, then the lift)\n%s", h.probeCalls, logs.String())
	}
	gap := h.probeStarts[1].Sub(h.probeStarts[0])
	if gap < interval/2 {
		t.Errorf("the probe after a probe error began %s after the failed one, want the full %s interval — a failing probe must not tight-loop\n%s",
			gap.Round(time.Millisecond), interval, logs.String())
	}
}

// The window-blocked log line names only the routes a window withheld. A route the walk probed and
// marked as limited is not inside its schedule (Synthetic in the incident), and naming it sends an
// operator looking at a window that is not holding it.
func TestProbeWake_WindowBlockedLogNamesOnlyWindowBlockedRoutes(t *testing.T) {
	inside := brisbaneAt(t, 9, 12, 0) // inside the peak schedule's first window
	h := &fakeAdapter{probeLimitFor: func(_ int, model string) harness.Limit {
		if model == routeFreeAlias {
			return harness.Limit{Limited: true}
		}
		return harness.Limit{Limited: false}
	}}
	d := routedDriver(t, h, twoPhases(), routeFreeAlias, routeGo)
	d.cfg.RouteSchedules, d.cfg.RouteWindows = peakRoutes(t, config.RouteWhenAllBlockedWait, routeGo)
	d.now = func() time.Time { return inside }

	logs := captureLogs(t)
	_, _, verdict, _ := d.probeWake(context.Background(), h, d.targets[0], d.cfg, d.cfg.Phases[0], nil, spend{start: time.Now()})
	if verdict != wakeStillLimited {
		t.Fatalf("verdict = %v, want wakeStillLimited: the windowed route cannot lift inside its window", verdict)
	}
	if h.probeCalls != 1 || h.probeModels[0] != routeFreeAlias {
		t.Fatalf("probes = %v (%d calls), want the one limited probe on %s before the walk hits the window",
			h.probeModels, h.probeCalls, routeFreeAlias)
	}

	line := logLineContaining(logs.String(), "window-blocked")
	if line == "" {
		t.Fatalf("no window-blocked line in the wake's log:\n%s", logs.String())
	}
	if !strings.Contains(line, "still limited on "+routeFreeAlias) {
		t.Errorf("the line does not name the route the walk probed limited: %q", line)
	}
	// Everything after "window-blocked (" is the refusal's own text; only window-blocked routes may
	// appear there, or the parenthetical is back to naming routes the schedule does not hold.
	at := strings.Index(line, "window-blocked (")
	if at < 0 {
		t.Fatalf("the window-blocked line carries no parenthetical: %q", line)
	}
	detail := line[at:]
	if !strings.Contains(detail, routeGo) {
		t.Errorf("the window-blocked detail does not name the windowed route %s: %q", routeGo, line)
	}
	if strings.Contains(detail, routeFreeAlias) {
		t.Errorf("the window-blocked detail names %s, which the walk marked as usage-limited, not windowed: %q",
			routeFreeAlias, line)
	}
	if !strings.Contains(detail, "Mon 14:00 AEST") {
		t.Errorf("the window-blocked detail does not name the window's end: %q", line)
	}

	// The refusal itself carries the same narrow list, so the phase-boundary evidence path ("the
	// next drain's boundary holds until it ends") cannot name a usage-limited route either.
	_, err := d.probeInvocation(d.targets[0], d.cfg, d.cfg.Phases[0], nil)
	var blocked *routeWindowBlocked
	if !errors.As(err, &blocked) {
		t.Fatalf("probeInvocation error = %v, want *routeWindowBlocked", err)
	}
	if len(blocked.routes) != 1 || blocked.routes[0] != routeGo {
		t.Errorf("routeWindowBlocked.routes = %v, want only the windowed %s", blocked.routes, routeGo)
	}
}

// A wake that starts with every route windowed probes nothing. That is a schedule condition, not a
// probe error — the wait is aimed at the window's end, so the line must say the schedule holds the
// phase rather than reading like a harness failure.
func TestProbeWake_AllRoutesWindowedAtWakeStartIsNotAProbeError(t *testing.T) {
	inside := brisbaneAt(t, 9, 12, 0)
	h := &fakeAdapter{}
	d := routedDriver(t, h, twoPhases(), routeFreeAlias, routeGo)
	d.cfg.RouteSchedules, d.cfg.RouteWindows = peakRoutes(t, config.RouteWhenAllBlockedWait, routeFreeAlias, routeGo)
	d.now = func() time.Time { return inside }

	logs := captureLogs(t)
	_, _, verdict, _ := d.probeWake(context.Background(), h, d.targets[0], d.cfg, d.cfg.Phases[0], nil, spend{start: time.Now()})
	if verdict != wakeStillLimited {
		t.Fatalf("verdict = %v, want wakeStillLimited", verdict)
	}
	if h.probeCalls != 0 {
		t.Fatalf("got %d probes, want 0: every route is inside its window", h.probeCalls)
	}
	out := logs.String()
	if !strings.Contains(out, "each is usage-marked or window-blocked") {
		t.Errorf("the wake does not say the schedule holds every route it could probe:\n%s", out)
	}
	if strings.Contains(out, "probe error") {
		t.Errorf("a window-blocked wake was booked as a probe error:\n%s", out)
	}
}

// A usage mark from an EARLIER wake holds a route too, so "every route for the phase is
// window-blocked" was false whenever a live mark sat beside a window: routableRoutesAt drops
// marked routes before the window filter. The line must claim only the disjunction, and the
// refusal's own text must keep naming just the routes a window withheld (CLA-988).
func TestProbeWake_WindowBlockedLogDoesNotClaimEveryHeldRouteIsWindowed(t *testing.T) {
	inside := brisbaneAt(t, 9, 12, 0) // inside the peak schedule's first window
	h := &fakeAdapter{}
	d := routedDriver(t, h, twoPhases(), routeFreeAlias, routeGo)
	d.cfg.RouteSchedules, d.cfg.RouteWindows = peakRoutes(t, config.RouteWhenAllBlockedWait, routeGo)
	d.now = func() time.Time { return inside }
	// A mark from an earlier wake holds the free route; only the Go route's window is refusing now.
	d.markRouteLimited(routeFreeAlias, inside.Add(5*time.Minute))

	logs := captureLogs(t)
	_, _, verdict, _ := d.probeWake(context.Background(), h, d.targets[0], d.cfg, d.cfg.Phases[0], nil, spend{start: time.Now()})
	if verdict != wakeStillLimited {
		t.Fatalf("verdict = %v, want wakeStillLimited", verdict)
	}
	if h.probeCalls != 0 {
		t.Fatalf("got %d probes, want 0: the only unmarked route is inside its window", h.probeCalls)
	}

	line := logLineContaining(logs.String(), "window-blocked")
	if line == "" {
		t.Fatalf("no window-blocked line in the wake's log:\n%s", logs.String())
	}
	if strings.Contains(line, "every route for the phase is window-blocked") {
		t.Errorf("the line claims every route is windowed while %s carries a usage mark: %q", routeFreeAlias, line)
	}
	if !strings.Contains(line, "usage-marked or window-blocked") {
		t.Errorf("the line does not say a route can be held by a usage mark: %q", line)
	}
	// The parenthetical is the refusal's own text: only the routes a window withheld may appear.
	at := strings.Index(line, "window-blocked (")
	if at < 0 {
		t.Fatalf("the window-blocked line carries no refusal detail: %q", line)
	}
	detail := line[at:]
	if !strings.Contains(detail, routeGo) {
		t.Errorf("the refusal detail does not name the windowed route %s: %q", routeGo, line)
	}
	if strings.Contains(detail, routeFreeAlias) {
		t.Errorf("the refusal detail names %s, which a usage mark holds, not a window: %q", routeFreeAlias, line)
	}
}

// The other shape of the same claim: a walk that probed a route LIMITED and then met a window
// refusal names the tried route and the disjunction, never "every other route is window-blocked".
// A third route held by a mark from an earlier wake makes the over-claim reachable, so a revert of
// the tried>0 line to the old wording fails here (CLA-988 review 1.review-1.1).
func TestProbeWake_WindowBlockedLogAfterALimitedProbeDoesNotClaimEveryHeldRouteIsWindowed(t *testing.T) {
	inside := brisbaneAt(t, 9, 12, 0) // inside the peak schedule's first window
	h := &fakeAdapter{probeLimitFor: func(_ int, model string) harness.Limit {
		if model == routeFreeAlias {
			return harness.Limit{Limited: true}
		}
		return harness.Limit{Limited: false}
	}}
	d := routedDriver(t, h, twoPhases(), routeFreeAlias, routePaidAlias, routeGo)
	d.cfg.RouteSchedules, d.cfg.RouteWindows = peakRoutes(t, config.RouteWhenAllBlockedWait, routeGo)
	d.now = func() time.Time { return inside }
	// A mark from an earlier wake holds the paid route, so the walk never probes it; only the Go
	// route's window refuses the rest.
	d.markRouteLimited(routePaidAlias, inside.Add(5*time.Minute))

	logs := captureLogs(t)
	_, _, verdict, end := d.probeWake(context.Background(), h, d.targets[0], d.cfg, d.cfg.Phases[0], nil, spend{start: time.Now()})
	if verdict != wakeStillLimited || end != wakeExhausted {
		t.Fatalf("verdict, end = %v, %v, want wakeStillLimited, wakeExhausted", verdict, end)
	}
	if h.probeCalls != 1 || h.probeModels[0] != routeFreeAlias {
		t.Fatalf("probes = %v (%d calls), want the one limited probe on %s before the walk hits the window",
			h.probeModels, h.probeCalls, routeFreeAlias)
	}

	line := logLineContaining(logs.String(), "still limited on "+routeFreeAlias)
	if line == "" {
		t.Fatalf("no line naming the route the walk probed limited:\n%s", logs.String())
	}
	if !strings.Contains(line, "usage-marked or window-blocked") {
		t.Errorf("the line claims every other route is windowed while %s carries a usage mark: %q", routePaidAlias, line)
	}
	if strings.Contains(line, "every other route is window-blocked") {
		t.Errorf("the line still carries the old over-claim: %q", line)
	}
	// The parenthetical is the refusal's own text: only the routes a window withheld may appear.
	at := strings.Index(line, "window-blocked (")
	if at < 0 {
		t.Fatalf("the window-blocked line carries no refusal detail: %q", line)
	}
	detail := line[at:]
	if !strings.Contains(detail, routeGo) {
		t.Errorf("the refusal detail does not name the windowed route %s: %q", routeGo, line)
	}
	for _, held := range []string{routeFreeAlias, routePaidAlias} {
		if strings.Contains(detail, held) {
			t.Errorf("the refusal detail names %s, which is not windowed here: %q", held, line)
		}
	}
}

// routeWakeHint is the deadline the wait uses when a route it left blocked clears before the poll
// interval: the soonest instant ANY route is outside both its usage mark and its pricing window.
// A route with neither must not pull the wake earlier.
func TestRouteWakeHint(t *testing.T) {
	at := brisbaneAt(t, 9, 12, 0) // inside the peak window
	const grace = 30 * time.Second
	for _, tc := range []struct {
		name     string
		routes   []string
		windowed []string // routes attached to the 11:00-14:00 peak schedule
		policy   config.RouteWhenAllBlocked
		marks    map[string]time.Duration
		want     time.Duration
		ok       bool
	}{
		{
			name:   "no route list uses the interval",
			routes: nil,
		},
		{
			name:   "an open route list uses the interval",
			routes: []string{routeFreeAlias, routeGo},
		},
		{
			name:     "the last open route's window end is the wake",
			routes:   []string{routeGo},
			windowed: []string{routeGo},
			want:     2*time.Hour + grace,
			ok:       true,
		},
		{
			name:   "a marked route wakes at its own reset",
			routes: []string{routeFreeAlias},
			marks:  map[string]time.Duration{routeFreeAlias: 5 * time.Minute},
			want:   5*time.Minute + grace,
			ok:     true,
		},
		{
			name:     "a route under both wakes when the LATER of the two lifts",
			routes:   []string{routeGo},
			windowed: []string{routeGo},
			marks:    map[string]time.Duration{routeGo: 5 * time.Minute},
			want:     2*time.Hour + grace,
			ok:       true,
		},
		{
			name:     "the soonest route wins across the list",
			routes:   []string{routeFreeAlias, routeGo},
			windowed: []string{routeGo},
			marks:    map[string]time.Duration{routeFreeAlias: 5 * time.Minute},
			want:     5*time.Minute + grace,
			ok:       true,
		},
		{
			name:     "a run-schedule route waits for its mark, not the window's end",
			routes:   []string{routeGo},
			windowed: []string{routeGo},
			policy:   config.RouteWhenAllBlockedRun,
			marks:    map[string]time.Duration{routeGo: 5 * time.Minute},
			want:     5*time.Minute + grace,
			ok:       true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := tc.policy
			if policy == "" {
				policy = config.RouteWhenAllBlockedWait
			}
			d := routedDriver(t, &fakeAdapter{}, nil, tc.routes...)
			d.cfg.RouteSchedules, d.cfg.RouteWindows = peakRoutes(t, policy, tc.windowed...)
			d.now = func() time.Time { return at }
			d.waitGrace = grace
			for route, in := range tc.marks {
				d.markRouteLimited(route, at.Add(in))
			}

			got, ok := d.routeWakeHintFor(d.cfg, tc.routes)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (hint %s)", ok, tc.ok, got)
			}
			if ok && got != tc.want {
				t.Errorf("hint = %s, want %s", got, tc.want)
			}
		})
	}
}
