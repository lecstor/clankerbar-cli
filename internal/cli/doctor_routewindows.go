package cli

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// --- 15. route pricing windows ----------------------------------------------

// checkRouteWindows reports every route carrying a pricing schedule
// (CLA-947): its schedule's windows, whether it is blocking right now, and when
// it next will. It also WARNS on a `route_windows` key that names no route the
// config can run — a typo must be visible, but a config that lists a route from
// a paused experiment must still start, so this is not a config FAIL.
//
// The verdict is PASS (or WARN for an unmatched id) by design: a configured
// schedule is a setting, not a fault, and an invalid one never reaches here
// (config.Validate refuses it).
func checkRouteWindows(cfg *config.Config) check {
	return checkRouteWindowsAt(cfg, time.Now())
}

// checkRouteWindowsAt is the check with its clock injected, so a test can pin
// both sides of a window without waiting for one.
func checkRouteWindowsAt(cfg *config.Config, now time.Time) check {
	c := check{name: "route_windows"}
	if len(cfg.RouteWindows) == 0 {
		c.status = pass
		c.detail = "none configured"
		return c
	}

	c.status = pass
	c.detail = fmt.Sprintf("%d route(s) carry a schedule", len(cfg.RouteWindows))

	routes := make([]string, 0, len(cfg.RouteWindows))
	for route := range cfg.RouteWindows {
		routes = append(routes, route)
	}
	slices.Sort(routes)
	for _, route := range routes {
		name := strings.TrimSpace(cfg.RouteWindows[route])
		sched := cfg.RouteScheduleFor(route)
		if sched == nil {
			// Validate would have refused a missing schedule; a config nothing
			// validated still deserves a legible line rather than a skip.
			c.info = append(c.info, fmt.Sprintf("%s -> %s: no such schedule", route, name))
			continue
		}
		line := fmt.Sprintf("%s -> %s: %s", route, name, sched.Describe())
		loc := routeWindowZone(sched)
		if active, until := sched.ActiveAt(now); active {
			line += fmt.Sprintf(" — BLOCKING now until %s", until.In(loc).Format("Mon 15:04 MST"))
		} else if start, ok := sched.NextStart(now); ok {
			line += fmt.Sprintf(" — free now; next blocks %s", start.In(loc).Format("Mon 15:04 MST"))
		} else {
			line += " — free now"
		}
		c.info = append(c.info, line)
	}

	if unmatched := cfg.UnmatchedRouteWindows(); len(unmatched) > 0 {
		c.status = warn
		c.info = append(c.info, fmt.Sprintf(
			"no configured route matches: %s — a typo here silently applies no schedule",
			strings.Join(unmatched, ", ")))
	}
	// The two keys answer different questions and both may be set; say so once,
	// because an operator who expects a route schedule to quiet the machine will
	// otherwise be surprised that pause_windows still runs.
	if cfg.PauseWindows != nil && len(cfg.PauseWindows.Windows) > 0 {
		c.info = append(c.info, "pause_windows is also set and still stops EVERY route: a route schedule only filters the routes it names")
	}
	return c
}

// routeWindowZone is the schedule's own zone for rendering an instant in the
// operator's wall clock; an unloadable one falls back to UTC so the line still
// prints (Validate refuses a bad zone before doctor runs).
func routeWindowZone(sched *config.RouteSchedule) *time.Location {
	if loc, err := time.LoadLocation(strings.TrimSpace(sched.Timezone)); err == nil {
		return loc
	}
	return time.UTC
}
