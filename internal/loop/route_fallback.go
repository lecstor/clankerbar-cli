package loop

// route_fallback.go — the driver's half of CLA-782's model route fallback.
//
// The opencode adapter switches routes INSIDE one session: a usage limit on the
// free route resumes the same session on the paid route and reports the switch on
// Result.Raw (see harness.RouteFallback). This file is what makes the switch
// STICKY across the run: it records a mark per denied route, keeps later phases
// off a marked route until the mark lifts, and writes the switch to both logs
// with the route, the reason and when the denied route is worth trying again.
//
// The mark's length is the provider's stated reset when it gave one, else the
// same default interval the supervised wait probes at (config `poll_interval`,
// 30 minutes). Nothing here waits: a marked route is simply not offered at the
// next phase's spawn, so the run keeps working on the route that worked.

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// defaultRouteMarkInterval is how long a denied route with no stated reset is
// treated as limited. It matches supervisedWait's default probe interval: that
// is how often the driver would otherwise ask whether the limit lifted, so a
// marked route is retried on the same cadence rather than on a second, invented
// one.
const defaultRouteMarkInterval = 30 * time.Minute

// routeMarkInterval is the configured supervised-wait probe interval, or the
// built-in default when the config names none. Read per call: a RELOAD landing
// mid-run applies to the next mark without a restart.
func (d *Driver) routeMarkInterval() time.Duration {
	return d.cfg.PollInterval.OrDefault(defaultRouteMarkInterval)
}

// markRouteLimited records that route is denied until the given instant. A later
// expiry wins over an earlier one: the provider's own stated reset is a better
// estimate than a fallback interval computed earlier, and shortening a mark on a
// stale report would send the next phase back into the same limit.
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

// routableRoutes applies this run's marks to one phase's configured route list.
//
// Unmarked routes are kept in configured order. When EVERY route is marked — a
// phase that starts while the whole chain is still in its reset window — the
// route whose mark lifts FIRST is used, alone: it is the soonest the free route
// can be tested again, and a phase must still start rather than wait on a mark.
// A marked route is never offered as a fallback to an unmarked one; the point of
// the mark is to stop paying for the same denial twice.
func (d *Driver) routableRoutes(routes []string) []string {
	if len(routes) == 0 {
		return nil
	}
	now := time.Now()
	available := make([]string, 0, len(routes))
	var earliest string
	var earliestUntil time.Time
	for _, route := range routes {
		until, marked := d.routeMarkExpiry(route)
		if !marked || !now.Before(until) {
			available = append(available, route)
			continue
		}
		if earliest == "" || until.Before(earliestUntil) {
			earliest, earliestUntil = route, until
		}
	}
	if len(available) > 0 {
		return available
	}
	if earliest != "" {
		return []string{earliest}
	}
	return nil
}

// noteRouteFallbacks records the marks an adapter reported and writes each
// SWITCH to the daemon log and the iteration log. It runs on every attempt,
// before any classification: a switch is a fact about the session that happened
// whichever way the attempt then ended, and the next attempt (a transient retry)
// must already start on the route that worked.
//
// A denied LAST route is marked but not logged here: there was no switch, and the
// existing limit branch already says what happened to the session.
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

// routeRetryLabel renders a mark's expiry the way an operator reads a clock: a
// bare time for something later today, and the weekday too when the wait runs
// past a day (a weekly cap's reset) so "after 12:40" cannot be misread as hours
// away when it is days.
func routeRetryLabel(until time.Time) string {
	if until.IsZero() {
		return "the reset"
	}
	if time.Until(until) > 24*time.Hour {
		return until.Format("Mon 15:04")
	}
	return until.Format("15:04")
}

// routeRoutesFor is the one place the driver turns a phase into the routes its
// session will offer the adapter, marks applied. It keeps ModelForPhase's answer
// authoritative when the config names no model at all (the harness default), and
// otherwise starts on the first ROUTABLE route.
func (d *Driver) routeRoutesFor(tc *config.Config, ph config.Phase) []string {
	routes, _ := tc.ModelRoutesForPhase(ph)
	return d.routableRoutes(routes)
}

// probeRouteFor is the route a supervised-wait probe should test for a phase
// with a route list (review finding 1.review-1.3): the route its next session
// would start on, marks applied. A wait is entered only once every route is
// spent, so that is the route whose mark lifts first - not the harness block's
// first route, which may be a route this run has already moved off. It returns
// "" for a phase with one route or none, so a single-route probe keeps the
// model it always had.
func (d *Driver) probeRouteFor(tc *config.Config, ph config.Phase) string {
	routes, _ := tc.ModelRoutesForPhase(ph)
	if len(routes) < 2 {
		return ""
	}
	if routable := d.routableRoutes(routes); len(routable) > 0 {
		return routable[0]
	}
	return ""
}
