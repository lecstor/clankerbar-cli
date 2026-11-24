package loop

// CLA-986: a supervised limit wait must wake at the instant a route it could not use becomes
// usable — a pricing window ending, or a route mark's own reset — instead of sleeping out the rest
// of the poll interval with an open route sitting idle. It also pins what a window-blocked refusal
// NAMES: only the routes a window actually withheld, never a route the walk probed and marked.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

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
		h, d.cfg.Phases[0], d.targets[0], spend{start: start})
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
	gap := h.probeStarts[1].Sub(h.probeStarts[0])
	if gap >= interval/2 {
		t.Errorf("the second probe began %s after the first, want ~%s (the window's remaining time plus grace) — a full probe interval (%s) is the overrun this task fixes\n%s",
			gap.Round(time.Millisecond), (windowEnd - firstWake).Round(time.Millisecond), interval, logs.String())
	}
	if gap < windowEnd/3 {
		t.Errorf("the second probe began %s after the first; the wait must sleep out the window's remaining time, not re-probe at once\n%s",
			gap.Round(time.Millisecond), logs.String())
	}
	if elapsed >= interval {
		t.Errorf("the wait took %s, want the window's end at %s — not the interval deadline at %s\n%s",
			elapsed.Round(time.Millisecond), (windowEnd + grace).Round(time.Millisecond), interval, logs.String())
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
	_, _, verdict := d.probeWake(context.Background(), h, d.targets[0], d.cfg, d.cfg.Phases[0], spend{start: time.Now()})
	if verdict != wakeStillLimited {
		t.Fatalf("verdict = %v, want wakeStillLimited: the windowed route cannot lift inside its window", verdict)
	}
	if h.probeCalls != 1 || h.probeModels[0] != routeFreeAlias {
		t.Fatalf("probes = %v (%d calls), want the one limited probe on %s before the walk hits the window",
			h.probeModels, h.probeCalls, routeFreeAlias)
	}

	var line string
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "window-blocked") {
			line = l
			break
		}
	}
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
	_, err := d.probeInvocation(d.targets[0], d.cfg, d.cfg.Phases[0])
	var blocked *routeWindowBlocked
	if !errors.As(err, &blocked) {
		t.Fatalf("probeInvocation error = %v, want *routeWindowBlocked", err)
	}
	if len(blocked.routes) != 1 || blocked.routes[0] != routeGo {
		t.Errorf("routeWindowBlocked.routes = %v, want only the windowed %s", blocked.routes, routeGo)
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := routedDriver(t, &fakeAdapter{}, nil, tc.routes...)
			d.cfg.RouteSchedules, d.cfg.RouteWindows = peakRoutes(t, config.RouteWhenAllBlockedWait, tc.windowed...)
			d.now = func() time.Time { return at }
			d.waitGrace = grace
			for route, in := range tc.marks {
				d.markRouteLimited(route, at.Add(in))
			}

			got, ok := d.routeWakeHint(d.cfg, config.Phase{})
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (hint %s)", ok, tc.ok, got)
			}
			if ok && got != tc.want {
				t.Errorf("hint = %s, want %s", got, tc.want)
			}
		})
	}
}
