package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// RouteSchedule is a named pricing schedule that can be attached to model
// ROUTES (CLA-947). A route inside one of its windows is unavailable for a NEW
// session exactly as a route carrying a usage mark is; a running session is
// never interrupted.
//
// It reuses PauseWindows wholesale rather than reimplementing the day/clock
// arithmetic: `pause_windows` answers "start nothing anywhere" (quiet hours,
// machine load), while a route schedule answers "do not pay peak rates for THIS
// provider+model". Both may be set, and they are different questions.
//
// The schedule hangs off the route ID (the exact model string the driver
// iterates), never off the provider or the model alone: the same DeepSeek model
// reached through a flat-priced provider has no peak tier, so a model-keyed
// schedule would wrongly pause it.
type RouteSchedule struct {
	PauseWindows

	// WhenAllBlocked decides what a phase does when EVERY route in its list is
	// inside a window. "" and "wait" hold the spawn until the earliest window
	// ends (the pricing choice is respected); "run" starts on the first route
	// that is not usage-marked and accepts the peak price. It never overrides a
	// usage mark: only the window filter degrades.
	WhenAllBlocked RouteWhenAllBlocked `json:"when_all_blocked,omitempty"`
}

// RouteWhenAllBlocked is the all-routes-blocked policy of one schedule.
type RouteWhenAllBlocked string

const (
	// RouteWhenAllBlockedWait holds new work until the earliest blocking window
	// ends. It is the default because a schedule exists to avoid a price, and a
	// daemon that silently pays it has defeated the setting.
	RouteWhenAllBlockedWait RouteWhenAllBlocked = "wait"
	// RouteWhenAllBlockedRun starts on the first configured route anyway.
	RouteWhenAllBlockedRun RouteWhenAllBlocked = "run"
)

// Validate checks the schedule's windows (through PauseWindows) and its
// all-blocked policy. A nil schedule is valid: the map is optional.
func (s *RouteSchedule) Validate() error {
	if s == nil {
		return nil
	}
	// Address the embedded field explicitly so loc is cached on the value the
	// Config actually holds.
	if err := (&s.PauseWindows).Validate(); err != nil {
		return err
	}
	// Case-insensitive and trimmed, like the days spec: a hand-edited config
	// writing "Run" means what "run" means, and normalising it here keeps the
	// value Identity() hashes the canonical one.
	policy := RouteWhenAllBlocked(strings.ToLower(strings.TrimSpace(string(s.WhenAllBlocked))))
	switch policy {
	case "":
		return nil
	case RouteWhenAllBlockedWait, RouteWhenAllBlockedRun:
		s.WhenAllBlocked = policy
		return nil
	default:
		return fmt.Errorf("when_all_blocked %q: want %q (spawn nothing until the window ends) or %q (start on the first route anyway)",
			s.WhenAllBlocked, RouteWhenAllBlockedWait, RouteWhenAllBlockedRun)
	}
}

// allBlockedPolicy is Validate's accepted set resolved to its default.
func (s *RouteSchedule) allBlockedPolicy() RouteWhenAllBlocked {
	if s == nil || s.WhenAllBlocked == "" {
		return RouteWhenAllBlockedWait
	}
	return s.WhenAllBlocked
}

// RouteScheduleFor returns the schedule attached to route, or nil. A route with
// no entry, or an entry naming a schedule the config does not define, has no
// schedule: Validate refuses the latter, and reading it as "always available"
// keeps a half-loaded config from blocking every spawn.
func (c *Config) RouteScheduleFor(route string) *RouteSchedule {
	if c == nil || len(c.RouteWindows) == 0 {
		return nil
	}
	name := strings.TrimSpace(c.RouteWindows[strings.TrimSpace(route)])
	if name == "" {
		return nil
	}
	return c.RouteSchedules[name]
}

// RouteWindowAt reports whether route is inside one of its schedule's windows at
// now (or inside the window's lead), and when that window ends.
//
// It is a pure function of (route, now) — plus the config, which does not change
// under a drain — so a caller can drive it from a fixed clock.
func (c *Config) RouteWindowAt(route string, now time.Time) (active bool, until time.Time) {
	s := c.RouteScheduleFor(route)
	if s == nil {
		return false, time.Time{}
	}
	return s.ActiveAt(now)
}

// RouteChoice is the window filter's verdict on an ordered route list at one
// instant.
type RouteChoice struct {
	// Routes are the routes a new session may start on, in configured order.
	// Empty means nothing may start.
	Routes []string
	// WaitUntil is set only when Routes is empty because every route is inside
	// a window under a "wait" schedule: the instant the earliest window ends.
	// Zero when there is no window to wait for (no routes at all).
	WaitUntil time.Time
}

// RouteChoiceAt is the pricing-window filter over an ordered route list. It is
// shared by the loop (which layers usage marks on top) and the supervisor's
// conflict session, so both answer "which route may this new session start on?"
// the same way.
//
// A route with no active window is available. When every route is inside a
// window, the schedule's when_all_blocked decides: "run" makes those routes
// available again (peak price accepted), "wait" returns no routes and the
// earliest window end.
func (c *Config) RouteChoiceAt(now time.Time, routes []string) RouteChoice {
	if len(routes) == 0 {
		return RouteChoice{}
	}
	open := make([]string, 0, len(routes))
	var earliest time.Time
	for _, route := range routes {
		active, until := c.RouteWindowAt(route, now)
		if !active {
			open = append(open, route)
			continue
		}
		if earliest.IsZero() || until.Before(earliest) {
			earliest = until
		}
	}
	if len(open) > 0 {
		return RouteChoice{Routes: open}
	}
	// Every route is inside a window. A "run" schedule degrades only the window
	// filter, and only for the routes it governs — a sibling schedule set to
	// "wait" stays respected.
	runnable := make([]string, 0, len(routes))
	for _, route := range routes {
		if c.RouteScheduleFor(route).allBlockedPolicy() == RouteWhenAllBlockedRun {
			runnable = append(runnable, route)
		}
	}
	if len(runnable) > 0 {
		return RouteChoice{Routes: runnable}
	}
	return RouteChoice{WaitUntil: earliest}
}

// validateRouteWindows refuses a schedule that cannot be read and a route
// attachment naming a schedule that does not exist — either is a setting the
// loop would silently ignore. A route ID that matches no configured route is
// deliberately NOT refused here: doctor reports it, so a config listing a route
// from a paused experiment still starts.
//
// Names and routes are checked in sorted order so the message does not depend on
// map iteration.
func (c *Config) validateRouteWindows() error {
	for _, name := range slices.Sorted(maps.Keys(c.RouteSchedules)) {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("route_schedules: a schedule name must not be blank")
		}
		if strings.TrimSpace(name) != name {
			// Lookups trim, so a padded key would be attachable only by a value
			// spelling the padding too, and RouteScheduleFor could never find it:
			// a pause that looks configured and never pauses.
			return fmt.Errorf("route_schedules[%q]: a schedule name must not start or end with whitespace", name)
		}
		sched := c.RouteSchedules[name]
		if sched == nil {
			// A JSON null is "a name with no entry" spelled wrong: it would read
			// back as no schedule at all, so a route attached to it is always
			// available — the silent no-op this validation exists to refuse.
			return fmt.Errorf("route_schedules[%q]: must be an object, not null", name)
		}
		if err := sched.Validate(); err != nil {
			return fmt.Errorf("route_schedules[%q]: %w", name, err)
		}
	}
	for _, route := range slices.Sorted(maps.Keys(c.RouteWindows)) {
		if strings.TrimSpace(route) == "" {
			return fmt.Errorf("route_windows: a route id must not be blank")
		}
		name := strings.TrimSpace(c.RouteWindows[route])
		if name == "" {
			return fmt.Errorf("route_windows[%q]: names no schedule", route)
		}
		if _, ok := c.RouteSchedules[name]; !ok {
			return fmt.Errorf("route_windows[%q]: no route_schedules entry named %q", route, name)
		}
	}
	return nil
}

// ConfiguredRouteIDs is every route ID any phase, panel member, tier bucket or
// harness default in this config can name, deduplicated and sorted. Doctor uses
// it to tell an operator that a `route_windows` key matches nothing — a typo
// that would otherwise quietly never apply.
func (c *Config) ConfiguredRouteIDs() []string {
	seen := map[string]bool{}
	add := func(routes ...string) {
		for _, r := range routes {
			if r = strings.TrimSpace(r); r != "" {
				seen[r] = true
			}
		}
	}
	add(c.Model)
	for _, alias := range c.Models {
		add(alias)
	}
	for _, ph := range c.EffectivePhases() {
		add(ph.Model.List()...)
	}
	for _, member := range c.ReviewPanel {
		add(member.Model.List()...)
	}
	for _, hc := range c.Harnesses {
		add(hc.Model.List()...)
		for _, routes := range hc.Models {
			add(routes.List()...)
		}
	}
	out := slices.Sorted(maps.Keys(seen))
	return out
}

// UnmatchedRouteWindows returns the `route_windows` keys that name no route the
// config can actually run, sorted. MatchUnknownRouteWindow is doctor's WARN, not
// a Validate refusal.
func (c *Config) UnmatchedRouteWindows() []string {
	known := make(map[string]bool, len(c.RouteWindows))
	for _, route := range c.ConfiguredRouteIDs() {
		known[route] = true
	}
	var out []string
	for route := range c.RouteWindows {
		if !known[strings.TrimSpace(route)] {
			out = append(out, route)
		}
	}
	slices.Sort(out)
	return out
}
