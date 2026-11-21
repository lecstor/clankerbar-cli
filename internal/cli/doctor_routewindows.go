package cli

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// --- 15. route pricing windows ----------------------------------------------

// checkRouteWindows reports every route carrying a pricing schedule: its schedule's windows,
// whether it is blocking right now, and when it next will. The verdict is PASS (or WARN for an
// unmatched id) by design: a configured schedule is a setting, not a fault, and an invalid one
// never reaches here (config.Validate refuses it).
func checkRouteWindows(cfg *config.Config) check {
	return checkRouteWindowsAt(cfg, time.Now())
}

// checkRouteWindowsAt is the check with its clock injected, so a test can pin both sides of a
// window without waiting for one.
func checkRouteWindowsAt(cfg *config.Config, now time.Time) check {
	c := check{name: "route_windows"}
	if len(cfg.RouteWindows) == 0 && len(cfg.RouteSchedules) == 0 {
		c.status = pass
		c.detail = "none configured"
		return c
	}

	c.status = pass
	if len(cfg.RouteWindows) == 0 {
		c.detail = "no route carries a schedule"
	} else {
		c.detail = fmt.Sprintf("%d route(s) carry a schedule", len(cfg.RouteWindows))
	}

	routes := make([]string, 0, len(cfg.RouteWindows))
	for route := range cfg.RouteWindows {
		routes = append(routes, route)
	}
	slices.Sort(routes)
	attached := make(map[string]bool, len(cfg.RouteWindows))
	for _, route := range routes {
		name := strings.TrimSpace(cfg.RouteWindows[route])
		sched := cfg.RouteScheduleFor(route)
		if sched == nil {
			// Validate refuses both a missing schedule and a null entry; a config nothing validated still
			// gets a WARN rather than a quiet skip — an attachment that resolves to no schedule reads as
			// "always available", the silent no-op this check exists to surface.
			c.status = warn
			c.info = append(c.info, fmt.Sprintf("%s -> %s: no such schedule (the route is always available) — check the name against route_schedules", route, name))
			continue
		}
		attached[name] = true
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

	// A schedule nobody attached is dead configuration, and exactly the kind an operator cannot see
	// from a listing of routes: name it, or "no route carries a schedule" reads as "nothing is
	// configured".
	if names := unattachedSchedules(cfg, attached); len(names) > 0 {
		c.info = append(c.info, fmt.Sprintf("defined but attached to no route: %s — an unattached schedule pauses nothing",
			strings.Join(names, ", ")))
	}

	if unmatched := cfg.UnmatchedRouteWindows(); len(unmatched) > 0 {
		c.status = warn
		c.info = append(c.info, fmt.Sprintf(
			"no configured route matches: %s — a typo here silently applies no schedule",
			strings.Join(unmatched, ", ")))
	}
	// The two keys answer different questions and both may be set; say so once, because an operator
	// who expects a route schedule to quiet the machine will otherwise be surprised that pause_windows
	// still runs.
	if cfg.PauseWindows != nil && len(cfg.PauseWindows.Windows) > 0 {
		c.info = append(c.info, "pause_windows is also set and still stops EVERY route: a route schedule only filters the routes it names")
	}
	return c
}

// unattachedSchedules is every route_schedules name no route_windows entry attached, sorted: the
// configuration doctor says out loud, because a schedule nothing names pauses nothing.
func unattachedSchedules(cfg *config.Config, attached map[string]bool) []string {
	var out []string
	for name := range cfg.RouteSchedules {
		if !attached[name] {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// routeWindowZone is the schedule's own zone for rendering an instant in the operator's wall clock;
// an unloadable one falls back to UTC so the line still prints (Validate refuses a bad zone before
// doctor runs).
func routeWindowZone(sched *config.RouteSchedule) *time.Location {
	if loc, err := time.LoadLocation(strings.TrimSpace(sched.Timezone)); err == nil {
		return loc
	}
	return time.UTC
}
