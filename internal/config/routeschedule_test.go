package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// CLA-947: named pricing schedules attached to route IDs. The window arithmetic
// is PauseWindows'; these tests pin the ATTACHMENT (which route is blocked when)
// and the all-routes-blocked policy.

func peakSchedule(whenAllBlocked RouteWhenAllBlocked) *RouteSchedule {
	return &RouteSchedule{
		PauseWindows: PauseWindows{
			Timezone: "Australia/Brisbane",
			Windows: []PauseWindow{
				{Days: "mon-fri", From: "11:00", To: "14:00"},
				{Days: "mon-fri", From: "16:00", To: "20:00"},
			},
		},
		WhenAllBlocked: whenAllBlocked,
	}
}

func scheduledConfig(whenAllBlocked RouteWhenAllBlocked, routes ...string) *Config {
	c := &Config{
		RouteSchedules: map[string]*RouteSchedule{"deepseek-peak": peakSchedule(whenAllBlocked)},
		RouteWindows:   map[string]string{},
	}
	for _, r := range routes {
		c.RouteWindows[r] = "deepseek-peak"
	}
	return c
}

func TestRouteScheduleValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    *RouteSchedule
		want string
	}{
		{"nil", nil, ""},
		{"valid default policy", peakSchedule(""), ""},
		{"valid wait", peakSchedule(RouteWhenAllBlockedWait), ""},
		{"valid run", peakSchedule(RouteWhenAllBlockedRun), ""},
		{"run with mixed case normalises", peakSchedule("Run"), ""},
		{"no windows is valid", &RouteSchedule{}, ""},
		{"missing timezone", &RouteSchedule{PauseWindows: PauseWindows{Windows: []PauseWindow{{Days: "mon", From: "11:00", To: "14:00"}}}}, "timezone"},
		{"bad day", &RouteSchedule{PauseWindows: PauseWindows{Timezone: "Australia/Brisbane", Windows: []PauseWindow{{Days: "funday", From: "11:00", To: "14:00"}}}}, "windows[0].days"},
		{"bad clock", &RouteSchedule{PauseWindows: PauseWindows{Timezone: "Australia/Brisbane", Windows: []PauseWindow{{Days: "mon", From: "9:30", To: "14:00"}}}}, "windows[0].from"},
		{"unknown policy", peakSchedule("skip"), "when_all_blocked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.s.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate: %v, want valid", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want an error carrying %q", err, tc.want)
			}
		})
	}
}

// Validate is the config load's gate: an attachment naming no schedule must be
// refused, naming the route and the missing schedule.
func TestConfigValidate_RouteWindows(t *testing.T) {
	valid := func() *Config {
		c := &Config{Harness: "opencode", Prompt: "Work the backlog.", WorkDir: t.TempDir()}
		c.RouteSchedules = map[string]*RouteSchedule{"deepseek-peak": peakSchedule("")}
		c.RouteWindows = map[string]string{"opencode-go/deepseek-v4.1-flash": "deepseek-peak"}
		return c
	}
	if err := valid().Validate(); err != nil {
		t.Fatalf("a valid route schedule was refused: %v", err)
	}

	// No keys at all is the historical config: valid, and it must marshal
	// without either key.
	plain := &Config{Harness: "opencode", Prompt: "Work the backlog.", WorkDir: t.TempDir()}
	if err := plain.Validate(); err != nil {
		t.Fatalf("a config with no route schedule was refused: %v", err)
	}
	b, err := json.Marshal(plain)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"route_windows", "route_schedules"} {
		if strings.Contains(string(b), `"`+key+`"`) {
			t.Errorf("identity bytes carry %q for a config that set nothing: %s", key, b)
		}
	}

	for _, tc := range []struct {
		name string
		mut  func(*Config)
		want []string // substrings the error must carry
	}{
		{"missing schedule", func(c *Config) {
			c.RouteWindows["opencode-go/deepseek-v4.1-flash"] = "nope"
		}, []string{"route_windows", "opencode-go/deepseek-v4.1-flash", `"nope"`}},
		{"blank schedule name", func(c *Config) {
			c.RouteWindows["opencode-go/deepseek-v4.1-flash"] = "  "
		}, []string{"route_windows", "names no schedule"}},
		{"blank route id", func(c *Config) {
			c.RouteWindows[" "] = "deepseek-peak"
		}, []string{"route_windows", "must not be blank"}},
		{"blank schedule key", func(c *Config) {
			c.RouteSchedules[""] = peakSchedule("")
		}, []string{"route_schedules", "must not be blank"}},
		{"bad schedule is named", func(c *Config) {
			c.RouteSchedules["deepseek-peak"] = &RouteSchedule{
				PauseWindows:   PauseWindows{Windows: []PauseWindow{{Days: "mon", From: "11:00", To: "14:00"}}},
				WhenAllBlocked: RouteWhenAllBlockedRun,
			}
		}, []string{`route_schedules["deepseek-peak"]`, "timezone"}},
		{"bad policy is named", func(c *Config) {
			c.RouteSchedules["deepseek-peak"] = peakSchedule("sometimes")
		}, []string{`route_schedules["deepseek-peak"]`, "when_all_blocked"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := valid()
			tc.mut(c)
			err := c.Validate()
			if err == nil {
				t.Fatal("Validate accepted it")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not carry %q", err, want)
				}
			}
		})
	}
}

// TestRouteWindows_UnmatchedRouteIsADoctorWarnNotAValidateError pins the settled
// decision: a config listing a route from a paused experiment must still start.
func TestRouteWindows_UnmatchedRouteIsADoctorWarnNotAValidateError(t *testing.T) {
	c := &Config{
		Harness:        "opencode",
		Prompt:         "Work the backlog.",
		WorkDir:        t.TempDir(),
		Model:          "zen-free",
		RouteSchedules: map[string]*RouteSchedule{"deepseek-peak": peakSchedule("")},
		RouteWindows: map[string]string{
			"zen-free":              "deepseek-peak",
			"a/route/that/is/old":   "deepseek-peak",
			"another/unmatched/one": "deepseek-peak",
		},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate refused an unmatched route id: %v", err)
	}
	got := c.UnmatchedRouteWindows()
	want := []string{"a/route/that/is/old", "another/unmatched/one"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("UnmatchedRouteWindows = %v, want %v", got, want)
	}
}

func TestConfiguredRouteIDs(t *testing.T) {
	c := &Config{
		Harness: "opencode",
		Model:   "run-wide",
		Models:  map[string]string{"strong": "run-wide-strong"},
		Harnesses: map[string]HarnessConfig{
			"opencode": {
				Model:  ModelRoutes{"block-default", "block-fallback"},
				Models: map[string]ModelRoutes{"cheap": {"block-cheap"}},
			},
		},
		Phases: []Phase{{Name: "implement", Model: ModelRoutes{"phase-pinned"}}},
		ReviewPanel: []ReviewPanelMember{
			{Model: ModelRoutes{"panel-one"}},
		},
	}
	got := c.ConfiguredRouteIDs()
	want := []string{"block-cheap", "block-default", "block-fallback", "panel-one", "phase-pinned", "run-wide", "run-wide-strong"}
	if len(got) != len(want) {
		t.Fatalf("ConfiguredRouteIDs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ConfiguredRouteIDs = %v, want %v", got, want)
		}
	}
}

// TestRouteChoiceAt is the pure filter the loop and the conflict session share:
// a route inside its window is unavailable, in configured order, with the
// all-blocked policy applied.
func TestRouteChoiceAt(t *testing.T) {
	bne := mustZone(t, "Australia/Brisbane")
	// 2026-11-09 is a Monday, so 12:00 sits inside the peak schedule's first
	// window and 09:00 outside it.
	inside := wall(bne, 2026, time.November, 9, 12, 0)
	outside := wall(bne, 2026, time.November, 9, 9, 0)
	endOfWindow := wall(bne, 2026, time.November, 9, 14, 0)

	const (
		zen       = "zen/free"
		goPeak    = "opencode-go/deepseek-v4.1-flash"
		synthetic = "synthetic/hf:deepseek-ai/DeepSeek-V4.1-Flash-1017"
		cc        = "commandcode/deepseek/deepseek-v4.1-flash"
	)

	for _, tc := range []struct {
		name   string
		c      *Config
		routes []string
		now    time.Time
		want   []string
		wait   time.Time
	}{
		{
			name:   "no schedule keeps every route",
			c:      &Config{},
			routes: []string{zen, goPeak, synthetic},
			now:    inside,
			want:   []string{zen, goPeak, synthetic},
		},
		{
			name:   "outside the window every route is open",
			c:      scheduledConfig("", goPeak, cc),
			routes: []string{zen, goPeak, synthetic},
			now:    outside,
			want:   []string{zen, goPeak, synthetic},
		},
		{
			name:   "a route inside its window is skipped, order kept",
			c:      scheduledConfig("", goPeak, cc),
			routes: []string{zen, goPeak, synthetic},
			now:    inside,
			want:   []string{zen, synthetic},
		},
		{
			name:   "the same model through a flat-price provider stays available",
			c:      scheduledConfig("", goPeak),
			routes: []string{goPeak, synthetic},
			now:    inside,
			want:   []string{synthetic},
		},
		{
			name:   "every route blocked under wait returns none and the window end",
			c:      scheduledConfig(RouteWhenAllBlockedWait, goPeak, cc),
			routes: []string{goPeak, cc},
			now:    inside,
			want:   nil,
			wait:   endOfWindow,
		},
		{
			name:   "every route blocked under run returns them",
			c:      scheduledConfig(RouteWhenAllBlockedRun, goPeak, cc),
			routes: []string{goPeak, cc},
			now:    inside,
			want:   []string{goPeak, cc},
		},
		{
			name:   "the default policy is wait",
			c:      scheduledConfig("", goPeak),
			routes: []string{goPeak},
			now:    inside,
			want:   nil,
			wait:   endOfWindow,
		},
		{
			name: "only the run schedule degrades when schedules disagree",
			c: func() *Config {
				c := scheduledConfig(RouteWhenAllBlockedWait, goPeak)
				c.RouteSchedules["cheap-peak"] = peakSchedule(RouteWhenAllBlockedRun)
				c.RouteWindows[cc] = "cheap-peak"
				return c
			}(),
			routes: []string{goPeak, cc},
			now:    inside,
			want:   []string{cc},
		},
		{
			name:   "no routes at all",
			c:      scheduledConfig("", goPeak),
			routes: nil,
			now:    inside,
			want:   nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			choice := tc.c.RouteChoiceAt(tc.now, tc.routes)
			if len(choice.Routes) != len(tc.want) {
				t.Fatalf("routes = %v, want %v", choice.Routes, tc.want)
			}
			for i := range tc.want {
				if choice.Routes[i] != tc.want[i] {
					t.Fatalf("routes = %v, want %v", choice.Routes, tc.want)
				}
			}
			if tc.wait.IsZero() {
				if !choice.WaitUntil.IsZero() {
					t.Errorf("WaitUntil = %v, want none", choice.WaitUntil)
				}
				return
			}
			if !choice.WaitUntil.Equal(tc.wait) {
				t.Errorf("WaitUntil = %v, want %v", choice.WaitUntil, tc.wait)
			}
		})
	}
}

// An overnight window is `from > to`, and the route must stay blocked past
// midnight — the trap a naive hour bound gets wrong.
func TestRouteWindowAt_OvernightWindow(t *testing.T) {
	bne := mustZone(t, "Australia/Brisbane")
	c := &Config{
		RouteSchedules: map[string]*RouteSchedule{"night": {
			PauseWindows: PauseWindows{
				Timezone: "Australia/Brisbane",
				Windows:  []PauseWindow{{Days: "fri", From: "22:00", To: "06:00"}},
			},
		}},
		RouteWindows: map[string]string{"go/route": "night"},
	}
	// Saturday 2026-11-14 03:00 is inside Friday's overnight window.
	active, until := c.RouteWindowAt("go/route", wall(bne, 2026, time.November, 14, 3, 0))
	if !active || !until.Equal(wall(bne, 2026, time.November, 14, 6, 0)) {
		t.Errorf("active/until = %v/%v, want true and Saturday 06:00", active, until)
	}
	// Sunday 03:00 is not: the window belongs to Friday's start day.
	if active, _ := c.RouteWindowAt("go/route", wall(bne, 2026, time.November, 15, 3, 0)); active {
		t.Error("Sunday small hours are inside the window; the days name the START day")
	}
	// A route with no attachment is never blocked.
	if active, _ := c.RouteWindowAt("other/route", wall(bne, 2026, time.November, 14, 3, 0)); active {
		t.Error("an unattached route reported blocked")
	}
}

// A schedule's lead pauses a route BEFORE its window, exactly as pause_windows
// does, because a phase that starts just before peak must not run into it.
func TestRouteWindowAt_Lead(t *testing.T) {
	bne := mustZone(t, "Australia/Brisbane")
	c := &Config{
		RouteSchedules: map[string]*RouteSchedule{"peak": {
			PauseWindows: PauseWindows{
				Timezone: "Australia/Brisbane",
				Lead:     Duration(30 * time.Minute),
				Windows:  []PauseWindow{{Days: "mon-fri", From: "11:00", To: "14:00"}},
			},
		}},
		RouteWindows: map[string]string{"go/route": "peak"},
	}
	if active, _ := c.RouteWindowAt("go/route", wall(bne, 2026, time.November, 9, 10, 45)); !active {
		t.Error("the lead does not block the route")
	}
	if active, _ := c.RouteWindowAt("go/route", wall(bne, 2026, time.November, 9, 10, 29)); active {
		t.Error("the route is blocked before the lead")
	}
}

// The attachment survives the config round trip: the key is the route id and the
// value names a schedule.
func TestRouteWindows_RoundTrip(t *testing.T) {
	c := &Config{}
	raw := `{"route_schedules":{"deepseek-peak":{"timezone":"Australia/Brisbane","windows":[{"days":"mon-fri","from":"11:00","to":"14:00"}],"when_all_blocked":"run"}},"route_windows":{"opencode-go/deepseek-v4.1-flash":"deepseek-peak"}}`
	if err := json.Unmarshal([]byte(raw), c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	s := c.RouteScheduleFor("opencode-go/deepseek-v4.1-flash")
	if s == nil {
		t.Fatal("the route lost its schedule in the round trip")
	}
	if s.WhenAllBlocked != RouteWhenAllBlockedRun {
		t.Errorf("when_all_blocked = %q, want run", s.WhenAllBlocked)
	}
	if got, want := s.Describe(), "mon-fri 11:00-14:00 (Australia/Brisbane)"; got != want {
		t.Errorf("Describe = %q, want %q", got, want)
	}
}

// A hand-edited policy is normalised, so Identity() hashes the canonical value
// and the loop's comparison need not repeat the leniency.
func TestRouteScheduleNormalisesThePolicy(t *testing.T) {
	s := peakSchedule("  RUN ")
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if s.WhenAllBlocked != RouteWhenAllBlockedRun {
		t.Errorf("policy = %q, want %q", s.WhenAllBlocked, RouteWhenAllBlockedRun)
	}
}
