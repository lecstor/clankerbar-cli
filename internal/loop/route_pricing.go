package loop

// route_windows.go — the loop's half of CLA-947: per-route pricing windows.
//
// `pause_windows` is the operator's daemon-wide "start no new work" schedule.
// A route schedule answers a narrower question — "do not pay THIS provider's
// peak rate" — and is therefore a filter on route SELECTION (routableRoutes),
// not a gate on the driver. Only when every route in a phase's list is inside a
// window does it become a gate, and then the schedule's when_all_blocked decides
// whether to hold the spawn or accept the peak price.
//
// The filter is a pure function of (route, now): the boundary hold below and
// invocationFor both read it through a clock the driver can pin in tests.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// routeWindowBlocked is invocationFor's refusal when every route a phase may
// start on is inside a pricing window and its schedule says wait.
//
// It is not a failure: the phase boundary holds the spawn until the earliest
// window ends (holdThroughRouteWindows). Reaching here means the window opened
// in the instant between the boundary check and the spawn, so the ladder ends
// the phase without spending rather than starting on a route the operator
// paused.
type routeWindowBlocked struct {
	phase  string
	until  time.Time
	routes []string
}

func (e *routeWindowBlocked) Error() string {
	return fmt.Sprintf("every route for the %s phase is inside a pricing window until %s (%s) — no session started",
		e.phase, formatReset(e.until), strings.Join(e.routes, ", "))
}

// holdThroughRouteWindows holds a phase sequence at its next boundary while
// every route that phase may start on is inside one of its pricing windows and
// the schedule says wait.
//
// It is the same hold as a scheduled pause (CLA-906): the claim is kept and its
// lease renewed, presence goes out on the same cadence, and STOP / RESTART /
// HALT / RELOAD stay responsive. A session already running is never interrupted
// — this runs only before the NEXT session spawns.
func (s *drainSeq) holdThroughRouteWindows(ctx context.Context, st *seqStep) pauseHold {
	return s.holdSequence(ctx, st, func() boundaryHoldState {
		d := s.d
		tc := s.tc
		routes, _ := tc.ModelRoutesForPhase(st.ph)
		now := d.clock()
		choice := d.routableRoutesAt(tc, now, routes)
		if len(choice.Routes) > 0 || choice.WaitUntil.IsZero() {
			return boundaryHoldState{}
		}
		return boundaryHoldState{
			active: true,
			until:  choice.WaitUntil,
			label:  "route windows",
			detail: routeWindowDetail(tc, routes, now),
		}
	})
}

// routeWindowDetail names every blocking schedule and its windows for the hold's
// entry line, so the operator reads WHY these routes are held rather than only
// that something is.
func routeWindowDetail(tc *config.Config, routes []string, now time.Time) string {
	seen := map[string]bool{}
	var parts []string
	for _, route := range routes {
		sched := tc.RouteScheduleFor(route)
		if sched == nil {
			continue
		}
		if active, _ := sched.ActiveAt(now); !active {
			continue
		}
		name := strings.TrimSpace(tc.RouteWindows[route])
		if seen[name] {
			continue
		}
		seen[name] = true
		parts = append(parts, fmt.Sprintf("%s %s", name, sched.Describe()))
	}
	return strings.Join(parts, "; ")
}
