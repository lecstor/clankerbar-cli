package loop

// route_fallback.go — the driver's half of CLA-782's model route fallback.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

// defaultRouteMarkInterval is how long a denied route with no stated reset is treated as limited.
const defaultRouteMarkInterval = 30 * time.Minute

// routeMarkInterval is the configured supervised-wait probe interval, or the built-in default when
// the config names none. Read per call: a RELOAD landing mid-run applies to the next mark without a
// restart.
func (d *Driver) routeMarkInterval() time.Duration {
	return d.cfg.PollInterval.OrDefault(defaultRouteMarkInterval)
}

// markRouteLimited records that route is denied until the given instant. A later expiry wins over
// an earlier one: the provider's own stated reset is a better estimate than a fallback interval
// computed earlier, and shortening a mark on a stale report would send the next phase back into the
// same limit.
func (d *Driver) markRouteLimited(route string, until time.Time) {
	route = strings.TrimSpace(route)
	if route == "" || until.IsZero() {
		return
	}
	d.routeMarksMu.Lock()
	defer d.routeMarksMu.Unlock()
	if d.routeMarks == nil {
		d.routeMarks = map[string]time.Time{}
	}
	if current, ok := d.routeMarks[route]; !ok || until.After(current) {
		d.routeMarks[route] = until
		d.persistRouteMarksLocked()
	}
}

// persistRouteMarksLocked writes the live marks to the state dir so a restarted process keeps them.
// Caller holds routeMarksMu.
func (d *Driver) persistRouteMarksLocked() {
	if d.state == nil {
		return
	}
	now := time.Now()
	out := make(map[string]time.Time, len(d.routeMarks))
	for route, until := range d.routeMarks {
		if until.After(now) {
			out[route] = until
		}
	}
	b, err := json.Marshal(out)
	if err == nil {
		err = d.state.WriteFileAtomic(statedir.RouteMarksName, b)
	}
	if err != nil {
		log.Printf("could not save route marks to the state dir (a restart will forget them): %v", err)
	}
}

// loadRouteMarks reads the marks a previous process saved, dropping any that have already expired.
// A missing file is the normal first start; an unreadable or corrupt one is logged and ignored,
// never fatal.
func (d *Driver) loadRouteMarks() {
	if d.state == nil {
		return
	}
	b, err := d.state.ReadFile(statedir.RouteMarksName)
	if errors.Is(err, os.ErrNotExist) {
		log.Printf("no saved route marks in %s (first start, or none were set)", d.state.Path())
		return
	}
	if err != nil {
		log.Printf("ignoring saved route marks: %v", err)
		return
	}
	var saved map[string]time.Time
	if err := json.Unmarshal(b, &saved); err != nil {
		log.Printf("ignoring corrupt saved route marks: %v", err)
		return
	}
	now := time.Now()
	d.routeMarksMu.Lock()
	defer d.routeMarksMu.Unlock()
	for route, until := range saved {
		route = strings.TrimSpace(route)
		if route == "" || !until.After(now) {
			continue
		}
		if d.routeMarks == nil {
			d.routeMarks = map[string]time.Time{}
		}
		if current, ok := d.routeMarks[route]; !ok || until.After(current) {
			d.routeMarks[route] = until
			log.Printf("restored route mark: %s stays off until %s", route, routeRetryLabel(until))
		}
	}
}

// routeMarkExpiry returns when route's mark lifts, and whether it has one.
func (d *Driver) routeMarkExpiry(route string) (time.Time, bool) {
	d.routeMarksMu.Lock()
	defer d.routeMarksMu.Unlock()
	until, ok := d.routeMarks[route]
	if !ok {
		return time.Time{}, false
	}
	return until, true
}

// routableRoutes applies this instance's marks AND the config's route pricing windows to one
// phase's configured route list, at the driver's clock. A route inside one of its schedule's
// windows is unavailable exactly as a marked one is; the window is a filter on SELECTION, never a
// gate on the driver, so a route with no schedule (or an empty one) is always available.
func (d *Driver) routableRoutes(tc *config.Config, routes []string) config.RouteChoice {
	return d.routableRoutesAt(tc, d.clock(), routes)
}

// routableRoutesAt is routableRoutes with its clock injected, so tests can pin both sides of a
// window. The window arithmetic itself is config's and pure.
func (d *Driver) routableRoutesAt(tc *config.Config, now time.Time, routes []string) config.RouteChoice {
	if len(routes) == 0 {
		return config.RouteChoice{}
	}
	unmarked := make([]string, 0, len(routes))
	marked := make([]string, 0, len(routes))
	var earliestMarked string
	var earliestUntil time.Time
	for _, route := range routes {
		until, limited := d.routeMarkExpiry(route)
		if !limited || !now.Before(until) {
			unmarked = append(unmarked, route)
			continue
		}
		marked = append(marked, route)
		if earliestMarked == "" || until.Before(earliestUntil) {
			earliestMarked, earliestUntil = route, until
		}
	}
	if len(unmarked) > 0 {
		choice := tc.RouteChoiceAt(now, unmarked)
		if len(choice.Routes) > 0 || !choice.WaitUntil.IsZero() {
			return choice
		}
		// Unreachable while RouteChoiceAt answers "open" or "wait" for every route; keep the unmarked
		// list rather than dropping every route if that ever stops being true.
		return config.RouteChoice{Routes: unmarked}
	}
	if earliestMarked == "" {
		return config.RouteChoice{}
	}
	choice := tc.RouteChoiceAt(now, marked)
	if len(choice.Routes) == 0 && !choice.WaitUntil.IsZero() {
		// Every marked route is inside a "wait" window: hold rather than pay the peak price.
		return choice
	}
	// The soonest-resetting marked route among those the window filter left open.
	soonest := ""
	var soonestUntil time.Time
	for _, route := range choice.Routes {
		until, _ := d.routeMarkExpiry(route)
		if soonest == "" || until.Before(soonestUntil) {
			soonest, soonestUntil = route, until
		}
	}
	if soonest == "" {
		soonest = earliestMarked
	}
	return config.RouteChoice{Routes: []string{soonest}}
}

// noteRouteFallbacks records the marks an adapter reported and writes each SWITCH to the daemon log
// and the iteration log.
func (d *Driver) noteRouteFallbacks(t Target, drainNum int, inv harness.Invocation, res harness.Result) {
	_, _, fallbacks := harness.RouteFallbacks(res)
	if len(fallbacks) == 0 {
		return
	}
	now := time.Now()
	interval := d.routeMarkInterval()
	for _, fb := range fallbacks {
		until := fb.ResetAt
		if until.IsZero() {
			until = now.Add(interval)
		}
		d.markRouteLimited(fb.Route, until)
		if fb.Next == "" {
			continue
		}
		reason := fb.Reason
		if reason == "" {
			reason = "usage_limit"
		}
		line := fmt.Sprintf("%siteration %d: %s hit a usage limit (%s) — resuming the session on %s; retrying %s after %s",
			labelOf(t), drainNum, fb.Route, reason, fb.Next, fb.Route, routeRetryLabel(until))
		log.Printf("%s", line)
		if inv.Console != nil {
			fmt.Fprintf(inv.Console, "\n%s\n", line)
		}
	}
}

// routeRetryLabel renders a mark's expiry the way an operator reads a clock: a bare local time for
// something later today, and the date too when the wait runs past a day (a weekly cap's reset) so
// "after 12:40" cannot be misread as hours away when it is days.
func routeRetryLabel(until time.Time) string {
	if until.IsZero() {
		return "the reset"
	}
	if time.Until(until) > 24*time.Hour {
		return formatReset(until)
	}
	return until.Local().Format("15:04 MST")
}

// routeRoutesFor is the one place the driver turns a phase into the routes its session will offer
// the adapter, marks and pricing windows applied. A choice with no routes and a WaitUntil is not
// "no model": it means every route is inside a window under a "wait" schedule, and invocationFor
// refuses the spawn rather than running on a route the operator paused.
func (d *Driver) routeRoutesFor(tc *config.Config, ph config.Phase) config.RouteChoice {
	routes, _ := tc.ModelRoutesForPhase(ph)
	return d.routableRoutes(tc, routes)
}

// probeRouteFor is the route a supervised-wait probe should test for a phase with a route list
// (review finding 1.review-1.3): the route its next session would start on, marks and pricing
// windows applied. A wait is entered only once every route is spent, so that is the route whose
// mark lifts first - not the harness block's first route, which may be a route this run has already
// moved off.
func (d *Driver) probeRouteFor(tc *config.Config, ph config.Phase) string {
	routes, _ := tc.ModelRoutesForPhase(ph)
	if len(routes) < 2 {
		return ""
	}
	choice := d.routableRoutes(tc, routes)
	if len(choice.Routes) > 0 {
		return choice.Routes[0]
	}
	return ""
}

// probeInvocation is the supervised-wait probe's Invocation: the phase harness's probe invocation,
// aimed at probeRouteFor's route when the phase has a route list. A phase whose every route is
// inside a "wait" pricing window has NO route a probe may start on, so it refuses the probe rather
// than letting the phase's model through: the probe is a paid session, and paying the peak price to
// learn whether a limit lifted is exactly what the schedule exists to prevent.
func (d *Driver) probeInvocation(t Target, tc *config.Config, ph config.Phase) (harness.Invocation, error) {
	inv, err := d.invocationOn(t, tc.HarnessFor(ph), true)
	if err != nil {
		return inv, err
	}
	routes, _ := tc.ModelRoutesForPhase(ph)
	if len(routes) == 0 {
		return inv, nil
	}
	choice := d.routableRoutes(tc, routes)
	switch {
	case len(choice.Routes) > 0:
		inv.Model = choice.Routes[0]
	case !choice.WaitUntil.IsZero():
		return inv, &routeWindowBlocked{phase: ph.Label(0), until: choice.WaitUntil, routes: choice.WindowBlocked}
	}
	return inv, nil
}
