package cli

// The route_windows preflight (CLA-947): every route carrying a pricing
// schedule, its windows and whether it is blocking right now, plus a WARN for a
// route id that matches nothing the config can run.

import (
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

const (
	doctorZenRoute = "zen/free"
	doctorGoRoute  = "opencode-go/deepseek-v4.1-flash"
)

// routeWindowCfg is a config with one schedule attached to doctorGoRoute.
func routeWindowCfg(t *testing.T, extra map[string]string) *config.Config {
	t.Helper()
	sched := &config.RouteSchedule{
		PauseWindows: config.PauseWindows{
			Timezone: "Australia/Brisbane",
			Windows: []config.PauseWindow{
				{Days: "mon-fri", From: "11:00", To: "14:00"},
				{Days: "mon-fri", From: "16:00", To: "20:00"},
			},
		},
	}
	if err := sched.Validate(); err != nil {
		t.Fatalf("fixture schedule does not validate: %v", err)
	}
	windows := map[string]string{doctorGoRoute: "deepseek-peak"}
	for route, name := range extra {
		windows[route] = name
	}
	return &config.Config{
		Harness: "opencode",
		Model:   doctorZenRoute,
		Harnesses: map[string]config.HarnessConfig{
			"opencode": {Model: config.ModelRoutes{doctorZenRoute, doctorGoRoute}},
		},
		RouteSchedules: map[string]*config.RouteSchedule{"deepseek-peak": sched},
		RouteWindows:   windows,
	}
}

func TestCheckRouteWindowsUnset(t *testing.T) {
	c := checkRouteWindowsAt(&config.Config{}, time.Now())
	if c.status != pass {
		t.Fatalf("status = %v, want pass", c.status)
	}
	if !strings.Contains(c.detail, "none configured") {
		t.Errorf("detail = %q, want it to say none is configured", c.detail)
	}
}

func TestCheckRouteWindowsReportsEachRouteAndTheMoment(t *testing.T) {
	loc, err := time.LoadLocation("Australia/Brisbane")
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}
	cfg := routeWindowCfg(t, nil)

	// Monday 2026-11-09 12:00 local: inside the 11:00-14:00 window.
	blocked := checkRouteWindowsAt(cfg, time.Date(2026, 11, 9, 12, 0, 0, 0, loc))
	if blocked.status != pass {
		t.Fatalf("status = %v, want pass (a blocking schedule is not a fault)", blocked.status)
	}
	if !strings.Contains(blocked.detail, "1 route") {
		t.Errorf("detail = %q, want it to count the routes", blocked.detail)
	}
	var line string
	for _, info := range blocked.info {
		if strings.Contains(info, doctorGoRoute) {
			line = info
		}
	}
	if line == "" {
		t.Fatalf("info = %v, want a line for %s", blocked.info, doctorGoRoute)
	}
	for _, want := range []string{"deepseek-peak", "mon-fri 11:00-14:00", "BLOCKING now until Mon 14:00 AEST"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q does not carry %q", line, want)
		}
	}

	// Monday 09:00 local: free, and the next block is 11:00.
	free := checkRouteWindowsAt(cfg, time.Date(2026, 11, 9, 9, 0, 0, 0, loc))
	line = ""
	for _, info := range free.info {
		if strings.Contains(info, doctorGoRoute) {
			line = info
		}
	}
	if !strings.Contains(line, "free now; next blocks Mon 11:00 AEST") {
		t.Errorf("line %q does not name the next blocking instant", line)
	}
}

// A route id nothing matches is a WARN, not a FAIL: the config still starts.
func TestCheckRouteWindowsUnmatchedRouteIsAWarn(t *testing.T) {
	cfg := routeWindowCfg(t, map[string]string{"a/route/from/a/paused/experiment": "deepseek-peak"})
	c := checkRouteWindowsAt(cfg, time.Now())
	if c.status != warn {
		t.Fatalf("status = %v, want warn", c.status)
	}
	joined := strings.Join(c.info, "\n")
	if !strings.Contains(joined, "a/route/from/a/paused/experiment") || !strings.Contains(joined, "no configured route matches") {
		t.Errorf("info = %v, want the unmatched route named", c.info)
	}
}

// Both keys may be set, and doctor says so: a route schedule does not quiet the
// daemon the way pause_windows does.
func TestCheckRouteWindowsSaysPauseWindowsStillApplies(t *testing.T) {
	cfg := routeWindowCfg(t, nil)
	cfg.PauseWindows = &config.PauseWindows{
		Timezone: "Australia/Brisbane",
		Windows:  []config.PauseWindow{{Days: "sat,sun", From: "00:00", To: "23:59"}},
	}
	c := checkRouteWindowsAt(cfg, time.Now())
	joined := strings.Join(c.info, "\n")
	if !strings.Contains(joined, "pause_windows is also set and still stops EVERY route") {
		t.Errorf("info = %v, want the both-keys note", c.info)
	}
}
