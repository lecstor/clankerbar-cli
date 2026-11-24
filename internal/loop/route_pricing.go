package loop

// route_pricing.go — the loop's half of CLA-947: per-route pricing windows.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// routeWindowBlocked is invocationFor's refusal when every route a phase may start on is inside a
// pricing window and its schedule says wait.
type routeWindowBlocked struct {
	phase  string
	until  time.Time
	routes []string // only the routes an active window withheld, never the phase's whole list
}

func (e *routeWindowBlocked) Error() string {
	// until carries the schedule's own location, so it is rendered there rather than in the daemon's
	// zone: the operator reads the clock their config wrote.
	return fmt.Sprintf("every route open to the %s phase is inside a pricing window until %s (%s) — no session started",
		e.phase, e.until.Format("Mon 15:04 MST"), strings.Join(e.routes, ", "))
}

// holdThroughRouteWindows holds a phase sequence at its next boundary while every route that phase
// may start on is inside one of its pricing windows and the schedule says wait. A session already
// running is never interrupted — this runs only before the NEXT session spawns.
func (s *drainSeq) holdThroughRouteWindows(ctx context.Context, st *seqStep) pauseHold {
	return s.holdSequence(ctx, st, func() boundaryHoldState {
		return s.routeWindowHoldState([][]string{s.phaseRoutes(st.ph)})
	})
}

// holdThroughPanelRouteWindows is the same hold for a PARALLEL review panel's fan-out, over every
// member's route list rather than the first member's. checkPhaseBoundary runs once per fan-out and
// can only see the phase at s.i, so a later member whose own list is windowed would otherwise reach
// invocationFor and be refused there — booked as a reviewer that recorded nothing, with the
// failure streak and the dead rate charged for a scheduled peak.
func (s *drainSeq) holdThroughPanelRouteWindows(ctx context.Context, st *seqStep, end int) pauseHold {
	if end <= s.i+1 {
		return pauseHoldNone
	}
	lists := make([][]string, 0, end-s.i)
	for i := s.i; i < end; i++ {
		lists = append(lists, s.phaseRoutes(s.phases[i]))
	}
	return s.holdSequence(ctx, st, func() boundaryHoldState {
		return s.routeWindowHoldState(lists)
	})
}

// phaseRoutes is one phase's configured route list, read from the live config so the hold and the
// spawn it guards resolve the same routes.
func (s *drainSeq) phaseRoutes(ph config.Phase) []string {
	routes, _ := s.d.liveCfg(s.t).ModelRoutesForPhase(ph)
	return routes
}

// routeWindowHoldState is the hold's decision for a set of route lists: active only when EVERY list
// is blocked under a "wait" schedule, until the instant the last of them opens. A list with a route
// that may start is not held, and an empty list (a phase with no configured routes) never blocks.
func (s *drainSeq) routeWindowHoldState(lists [][]string) boundaryHoldState {
	d := s.d
	tc := d.liveCfg(s.t)
	now := d.clock()
	active, until := d.routeWindowHold(tc, now, lists)
	if !active {
		return boundaryHoldState{}
	}
	detail, zone := routeWindowHoldDetail(tc, lists, now, until)
	return boundaryHoldState{active: true, until: until, label: "route windows", detail: detail, zone: zone}
}

// routeWindowHold reports whether every route list is blocked under a "wait" schedule, and the
// instant the last blocked list opens.
func (d *Driver) routeWindowHold(tc *config.Config, now time.Time, lists [][]string) (active bool, until time.Time) {
	for _, routes := range lists {
		choice := d.routableRoutesAt(tc, now, routes)
		if len(choice.Routes) > 0 || choice.WaitUntil.IsZero() {
			continue
		}
		active = true
		if choice.WaitUntil.After(until) {
			until = choice.WaitUntil
		}
	}
	return active, until
}

// routeWindowHoldDetail names every blocking schedule and its windows for the hold's entry line, so
// the operator reads WHY these routes are held rather than only that something is.
func routeWindowHoldDetail(tc *config.Config, lists [][]string, now, until time.Time) (detail, zone string) {
	seen := map[string]bool{}
	var parts []string
	for _, routes := range lists {
		for _, route := range routes {
			sched := tc.RouteScheduleFor(route)
			if sched == nil {
				continue
			}
			active, end := sched.ActiveAt(now)
			if !active {
				continue
			}
			if zone == "" && end.Equal(until) {
				zone = sched.Timezone
			}
			name := strings.TrimSpace(tc.RouteWindows[route])
			if seen[name] {
				continue
			}
			seen[name] = true
			parts = append(parts, fmt.Sprintf("%s %s", name, sched.Describe()))
		}
	}
	return strings.Join(parts, "; "), zone
}
