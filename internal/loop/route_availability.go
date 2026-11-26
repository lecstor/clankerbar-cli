// route_availability.go — what each of a phase's routes can do right now, shared by the supervised
// wait's deadlines and its beacon: a usage mark, a pricing window, and a Stop-class sideline
// (budget/credits exhausted) are three different answers to "can this route start a session"
// (CLA-1024).

package loop

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// routeStopBlocked is the refusal when every route a phase may start on is Stop-blocked: a
// budget/credit exhaustion has no reset to poll for, so no probe or session may run on any of them
// until the operator resolves the account and writes RELOAD (or restarts).
type routeStopBlocked struct {
	phase  string
	route  string        // the route whose stop is reported, in configured order
	limit  harness.Limit // that route's recorded Stop, for the LIMIT_STOP line
	routes []string      // every configured route, all Stop-blocked
}

func (e *routeStopBlocked) Error() string {
	return fmt.Sprintf("no route for the %s phase may start: every configured route is Stop-blocked (%s) — resolve the account and write RELOAD (or restart): %s",
		e.phase, oneLine(e.limit.Reason), strings.Join(e.routes, ", "))
}

// stopRoute sidelines a route that returned a Stop-class limit until RELOAD/restart. A Stop limit has
// no reset to wait out, so re-probing it every poll interval only pays a dead subscription a visit;
// the sideline is the local record that the route is out until a human acts.
func (d *Driver) stopRoute(route string, lim harness.Limit) {
	route = strings.TrimSpace(route)
	if route == "" || !lim.Stop {
		return
	}
	d.routeStopsMu.Lock()
	defer d.routeStopsMu.Unlock()
	if d.routeStops == nil {
		d.routeStops = map[string]harness.Limit{}
	}
	// First record wins: the reason does not change between stops, and the first is the one the
	// LIMIT_STOP line (and the operator's diagnosis) was written from.
	if _, ok := d.routeStops[route]; !ok {
		d.routeStops[route] = lim
	}
}

// routeStopped reports the Stop recorded for route, and whether it carries one.
func (d *Driver) routeStopped(route string) (harness.Limit, bool) {
	d.routeStopsMu.Lock()
	defer d.routeStopsMu.Unlock()
	lim, ok := d.routeStops[route]
	return lim, ok
}

// clearStoppedRoutes drops every Stop sideline and returns the routes it cleared. Called on RELOAD:
// the operator is saying the config — and with it, presumably the account — changed, so the dead
// routes are worth one more probe. Marks are NOT cleared: a mark is the provider's own timed
// statement and a config edit does not lift it.
func (d *Driver) clearStoppedRoutes() []string {
	d.routeStopsMu.Lock()
	defer d.routeStopsMu.Unlock()
	if len(d.routeStops) == 0 {
		return nil
	}
	routes := make([]string, 0, len(d.routeStops))
	for route := range d.routeStops {
		routes = append(routes, route)
	}
	d.routeStops = nil
	return routes
}

// allRoutesStopped reports whether routes is a NON-EMPTY list that every route of has a Stop
// sideline. A phase with no configured route list is not "all stopped": it is the legacy
// single-harness shape, whose one route the caller knows by name.
func (d *Driver) allRoutesStopped(routes []string) bool {
	if len(routes) == 0 {
		return false
	}
	for _, route := range routes {
		if _, stopped := d.routeStopped(route); !stopped {
			return false
		}
	}
	return true
}

// stopRefusalFor is the phase-wide Stop verdict over an explicit route list: nil while any route may
// still start (open, windowed, or rolling-limited), and a *routeStopBlocked naming every route when
// all of them are Stop-blocked.
func (d *Driver) stopRefusalFor(routes []string, phaseLabel string) *routeStopBlocked {
	if !d.allRoutesStopped(routes) {
		return nil
	}
	refusal := &routeStopBlocked{phase: phaseLabel, routes: routes}
	for _, route := range routes {
		if lim, ok := d.routeStopped(route); ok {
			refusal.route, refusal.limit = route, lim
			break
		}
	}
	return refusal
}

// routeClearAt is when route is usable again: the later of its usage mark and a withholding pricing
// window, or a time not after now when it is already clear. window reports whether that instant is
// the WINDOW's end, which is what lets the beacon call an all-routes-limited wait a schedule pause
// rather than an open-ended limit. ok is false for a Stop-blocked route: nothing short of a RELOAD
// clears it, so it has no instant and must not pull a wake earlier.
func (d *Driver) routeClearAt(tc *config.Config, now time.Time, route string) (at time.Time, window, ok bool) {
	if _, stopped := d.routeStopped(route); stopped {
		return time.Time{}, false, false
	}
	at = now
	// Ask the filter rather than RouteWindowAt: a "run" schedule offers its route back at peak,
	// so its window never withholds anything and its mark is the only real wait.
	if choice := tc.RouteChoiceAt(now, []string{route}); len(choice.Routes) == 0 && !choice.WaitUntil.IsZero() {
		at, window = choice.WaitUntil, true
	}
	if until, marked := d.routeMarkExpiry(route); marked && until.After(at) {
		at, window = until, false
	}
	return at, window, true
}

// routeWakeHint is how long a still-limited wait should sleep when a route the walk left blocked
// clears sooner than the poll interval: the soonest instant any of the phase's routes is outside
// both its usage mark and a withholding pricing window (CLA-986). ok=false keeps the caller on the
// interval: a phase with no route list, a Stop-blocked set with no future instant, or one with a
// route already clear, has no future instant.
func (d *Driver) routeWakeHint(tc *config.Config, ph config.Phase) (time.Duration, bool) {
	routes, _ := tc.ModelRoutesForPhase(ph)
	return d.routeWakeHintFor(tc, routes)
}

// routeWakeHintFor is routeWakeHint over an explicit route list, for the callers holding the
// resolved (escalation-applied) list rather than the phase's configured one.
func (d *Driver) routeWakeHintFor(tc *config.Config, routes []string) (time.Duration, bool) {
	if len(routes) == 0 {
		return 0, false
	}
	now := d.clock()
	var earliest time.Time
	for _, route := range routes {
		at, _, ok := d.routeClearAt(tc, now, route)
		if !ok {
			// Stop-blocked: no instant to wake at. A wait with only these left stops instead of
			// polling, which the caller decides.
			continue
		}
		if !at.After(now) {
			return 0, false
		}
		if earliest.IsZero() || at.Before(earliest) {
			earliest = at
		}
	}
	if earliest.IsZero() {
		return 0, false
	}
	// The same grace a stated-reset wake takes: a probe that lands a hair before the edge pays a
	// whole interval for its timing.
	return earliest.Sub(now) + d.waitGrace, true
}

// routeClearNow reports whether one of the phase's routes is usable RIGHT NOW. routeWakeHint only
// answers with a future instant, which is not the same as no route being clear: a mark can expire
// while a later route's probe is still running, and the walk then stops at tried[route] without
// re-probing it (CLA-988). A Stop-blocked route never clears now.
func (d *Driver) routeClearNow(tc *config.Config, ph config.Phase) bool {
	routes, _ := tc.ModelRoutesForPhase(ph)
	return d.routeClearNowFor(tc, routes)
}

// routeClearNowFor is routeClearNow over an explicit route list.
func (d *Driver) routeClearNowFor(tc *config.Config, routes []string) bool {
	now := d.clock()
	for _, route := range routes {
		if at, _, ok := d.routeClearAt(tc, now, route); ok && !at.After(now) {
			return true
		}
	}
	return false
}

// waitAvailability is one phase's route verdict at one instant, computed live from the marks, the
// pricing windows and the Stop sidelines.
type waitAvailability struct {
	// open: some configured route is usable now, so the wait's next probe can lift it.
	open bool
	// allStopped: every configured route is Stop-blocked, so no probe can lift anything.
	allStopped bool
	// earliest: the soonest instant a blocked route lifts. Zero when a route is open, no route is
	// configured, or every configured route is stopped.
	earliest time.Time
	// scheduleAt is earliest when that instant is a route WINDOW's end: the console then reports a
	// schedule pause with its resume time instead of "usage limit - probing for a reset".
	scheduleAt   time.Time
	scheduleZone string
	holds        []fleet.RouteHold
}

// waitAvailabilityAt computes the verdict for routes. holds is the per-route breakdown the beacon
// carries (every route held now, and why).
func (d *Driver) waitAvailabilityAt(tc *config.Config, now time.Time, routes []string) waitAvailability {
	av := waitAvailability{holds: d.routeHoldsAt(tc, now, routes)}
	if len(routes) == 0 {
		return av
	}
	stopped := 0
	for _, route := range routes {
		at, window, ok := d.routeClearAt(tc, now, route)
		if !ok {
			stopped++
			continue
		}
		if !at.After(now) {
			// A route is usable now: the wait's next probe is what decides, so no schedule pause and no
			// future instant apply — whatever an earlier route in the list contributed.
			av.open = true
			av.earliest, av.scheduleAt, av.scheduleZone = time.Time{}, time.Time{}, ""
			return av
		}
		if av.earliest.IsZero() || at.Before(av.earliest) {
			av.earliest = at
			av.scheduleAt = time.Time{}
			if window {
				av.scheduleAt = at
			}
		}
	}
	av.allStopped = stopped == len(routes)
	if !av.scheduleAt.IsZero() {
		av.scheduleZone = d.routeWindowZoneAt(tc, now, routes, av.scheduleAt)
	}
	return av
}

// routeWindowZoneAt names the IANA zone of the schedule whose window ends at until, so the beacon's
// resume instant renders in the operator's own clock rather than the daemon's.
func (d *Driver) routeWindowZoneAt(tc *config.Config, now time.Time, routes []string, until time.Time) string {
	for _, route := range routes {
		sched := tc.RouteScheduleFor(route)
		if sched == nil {
			continue
		}
		if active, end := sched.ActiveAt(now); active && end.Equal(until) {
			return sched.Timezone
		}
	}
	return ""
}

// waitBeaconState is the presence a supervised wait publishes: the daemon-wide pause window when one
// is in force, else a route-window schedule pause when the earliest unblocking event is one, else
// `limited` carrying the earliest concrete instant any route lifts (resetAt stays the caller's
// stated reset — or zero — when there is none). routes are the phase's resolved route list.
func (d *Driver) waitBeaconState(tc *config.Config, routes []string, statedReset time.Time) fleet.State {
	av := d.waitAvailabilityAt(tc, d.clock(), routes)
	// A daemon-wide pause_windows window outranks everything: no probe may spawn while it holds, so
	// the console must show its resume time rather than a limit probe that is not happening.
	if active, until, pw := d.pauseState(); active {
		st := fleet.State{Kind: fleet.StateDraining, PauseReason: fleet.PauseReasonSchedule, PauseUntil: until, Routes: av.holds}
		if pw != nil {
			st.PauseZone = pw.Timezone
		}
		return st
	}
	if !av.scheduleAt.IsZero() {
		// CLA-1024: every route is limited or windowed and the earliest thing that can change is a
		// route window ending. That is a schedule pause with a known resume instant, and saying
		// "usage limit - probing for a reset" hides it.
		return fleet.State{Kind: fleet.StateDraining, PauseReason: fleet.PauseReasonSchedule,
			PauseUntil: av.scheduleAt, PauseZone: av.scheduleZone, Routes: av.holds}
	}
	reset := statedReset
	if !av.earliest.IsZero() {
		reset = av.earliest
	}
	return fleet.State{Kind: fleet.StateLimited, ResetAt: reset, Routes: av.holds}
}

// logStopBlocked says, once per occurrence, that no configured route can start: the operator's move
// is a RELOAD after resolving the account, not another wait.
func logStopBlocked(ph config.Phase, refusal *routeStopBlocked) {
	log.Printf("every configured route for the %s phase is Stop-blocked (%v) — writing %s and ending the wait: a budget/credit exhaustion has no reset to poll for, so resolve the account and write RELOAD (or restart)",
		ph.Label(0), strings.Join(refusal.routes, ", "), LimitStopName)
}

// writeStopBlocked records the LIMIT_STOP marker for a phase-wide stop. The line names the reason
// the first stopped route reported and every route that is out.
func (d *Driver) writeStopBlocked(refusal *routeStopBlocked) {
	d.writeLimitStop(limitStopLine(refusal.limit, refusal.route, refusal.Error()))
}
