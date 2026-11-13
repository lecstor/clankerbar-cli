package config

import (
	"strings"
	"testing"
	"time"
)

// mustPause validates a hand-built schedule, so every fixture below is a
// schedule the loader would accept (and has its zone parsed).
func mustPause(t *testing.T, p *PauseWindows) *PauseWindows {
	t.Helper()
	if err := p.Validate(); err != nil {
		t.Fatalf("fixture pause windows do not validate: %v", err)
	}
	return p
}

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load zone %s: %v", name, err)
	}
	return loc
}

func wall(loc *time.Location, y int, m time.Month, d, hh, mm int) time.Time {
	return time.Date(y, m, d, hh, mm, 0, 0, loc)
}

func TestPauseWindowsValidate(t *testing.T) {
	peak := func() []PauseWindow {
		return []PauseWindow{{Days: "mon-fri", From: "11:00", To: "14:00"}}
	}
	win := func(w PauseWindow) []PauseWindow { return []PauseWindow{w} }

	// want is a substring the error must carry; "" means the schedule is valid.
	for _, tc := range []struct {
		name string
		p    *PauseWindows
		want string
	}{
		{"nil", nil, ""},
		{"no windows", &PauseWindows{}, ""},
		{"valid peak", &PauseWindows{Timezone: "Australia/Brisbane", Windows: peak()}, ""},
		{"valid overnight", &PauseWindows{Timezone: "Australia/Brisbane", Windows: win(PauseWindow{Days: "fri", From: "22:00", To: "06:00"})}, ""},
		{"valid lead", &PauseWindows{Timezone: "Australia/Brisbane", Lead: Duration(30 * time.Minute), Windows: peak()}, ""},
		{"valid lead at the 24h cap", &PauseWindows{Timezone: "Australia/Brisbane", Lead: Duration(24 * time.Hour), Windows: peak()}, ""},
		{"valid 24-hour window", &PauseWindows{Timezone: "Australia/Brisbane", Windows: win(PauseWindow{Days: "sat", From: "00:00", To: "00:00"})}, ""},
		{"missing timezone", &PauseWindows{Windows: peak()}, "timezone"},
		{"unknown zone", &PauseWindows{Timezone: "Mars/Olympus", Windows: peak()}, "timezone"},
		{"negative lead", &PauseWindows{Timezone: "Australia/Brisbane", Lead: Duration(-time.Minute), Windows: peak()}, "lead"},
		{"lead over 24h", &PauseWindows{Timezone: "Australia/Brisbane", Lead: Duration(24*time.Hour + time.Minute), Windows: peak()}, "must not exceed 24h"},
		{"empty days", &PauseWindows{Timezone: "Australia/Brisbane", Windows: win(PauseWindow{From: "11:00", To: "14:00"})}, "windows[0].days"},
		{"unknown day", &PauseWindows{Timezone: "Australia/Brisbane", Windows: win(PauseWindow{Days: "funday", From: "11:00", To: "14:00"})}, `"funday"`},
		{"wrapping range", &PauseWindows{Timezone: "Australia/Brisbane", Windows: win(PauseWindow{Days: "fri-mon", From: "11:00", To: "14:00"})}, "may not wrap"},
		{"hour out of range", &PauseWindows{Timezone: "Australia/Brisbane", Windows: win(PauseWindow{Days: "mon", From: "25:00", To: "14:00"})}, "windows[0].from"},
		{"minute out of range", &PauseWindows{Timezone: "Australia/Brisbane", Windows: win(PauseWindow{Days: "mon", From: "11:00", To: "11:60"})}, "windows[0].to"},
		{"not HH:MM", &PauseWindows{Timezone: "Australia/Brisbane", Windows: win(PauseWindow{Days: "mon", From: "9:30", To: "14:00"})}, "windows[0].from"},
		{"second window is named", &PauseWindows{
			Timezone: "Australia/Brisbane",
			Windows: []PauseWindow{
				{Days: "daily", From: "11:00", To: "14:00"},
				{Days: "mon-fri", From: "16:00", To: "25:00"},
			},
		}, "windows[1]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.p.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate: %v, want valid", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate accepted it, want an error carrying %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not carry %q", err, tc.want)
			}
		})
	}
}

// TestPauseWindowsValidateIsWiredIntoConfig pins the hook: a bad schedule is a
// config load failure, not a setting the loop silently ignores.
func TestPauseWindowsValidateIsWiredIntoConfig(t *testing.T) {
	c := &Config{Harness: "claude", Prompt: "Work the backlog.", WorkDir: t.TempDir()}
	c.PauseWindows = &PauseWindows{Windows: []PauseWindow{{Days: "mon-fri", From: "11:00", To: "14:00"}}}
	err := c.Validate()
	if err == nil {
		t.Fatal("Config.Validate accepted pause_windows with no timezone")
	}
	if !strings.HasPrefix(err.Error(), "pause_windows: ") {
		t.Errorf("error %q does not name the config key", err)
	}
}

func TestPauseWindowsActiveAt(t *testing.T) {
	bne := mustZone(t, "Australia/Brisbane")
	peak := func(lead time.Duration) *PauseWindows {
		return mustPause(t, &PauseWindows{
			Timezone: "Australia/Brisbane",
			Lead:     Duration(lead),
			Windows:  []PauseWindow{{Days: "mon-fri", From: "11:00", To: "14:00"}},
		})
	}
	overnight := mustPause(t, &PauseWindows{
		Timezone: "Australia/Brisbane",
		Windows:  []PauseWindow{{Days: "fri", From: "22:00", To: "06:00"}},
	})
	abutting := mustPause(t, &PauseWindows{
		Timezone: "Australia/Brisbane",
		Windows: []PauseWindow{
			{Days: "mon-fri", From: "11:00", To: "14:00"},
			{Days: "mon-fri", From: "14:00", To: "15:00"},
		},
	})

	// 2026-11-09 is a Monday, 2026-11-13 a Friday, 2026-11-14 a Saturday and
	// 2026-11-15 a Sunday.
	for _, tc := range []struct {
		name   string
		p      *PauseWindows
		now    time.Time
		active bool
		until  time.Time
	}{
		{"inside a weekday window", peak(0), wall(bne, 2026, time.November, 9, 12, 0), true, wall(bne, 2026, time.November, 9, 14, 0)},
		{"same clock time on Saturday", peak(0), wall(bne, 2026, time.November, 14, 12, 0), false, time.Time{}},
		{"exactly at From", peak(0), wall(bne, 2026, time.November, 9, 11, 0), true, wall(bne, 2026, time.November, 9, 14, 0)},
		{"exactly at To", peak(0), wall(bne, 2026, time.November, 9, 14, 0), false, time.Time{}},
		{"inside the lead", peak(30 * time.Minute), wall(bne, 2026, time.November, 9, 10, 31), true, wall(bne, 2026, time.November, 9, 14, 0)},
		{"before the lead", peak(30 * time.Minute), wall(bne, 2026, time.November, 9, 10, 29), false, time.Time{}},
		{"exactly at the lead boundary", peak(30 * time.Minute), wall(bne, 2026, time.November, 9, 10, 30), true, wall(bne, 2026, time.November, 9, 14, 0)},
		{"overnight window, Saturday small hours", overnight, wall(bne, 2026, time.November, 14, 3, 0), true, wall(bne, 2026, time.November, 14, 6, 0)},
		{"overnight window, Sunday small hours", overnight, wall(bne, 2026, time.November, 15, 3, 0), false, time.Time{}},
		{"abutting windows merge", abutting, wall(bne, 2026, time.November, 9, 11, 30), true, wall(bne, 2026, time.November, 9, 15, 0)},
		// The same instant given in UTC must be judged in the configured zone.
		{"now in UTC", peak(0), wall(bne, 2026, time.November, 9, 12, 0).UTC(), true, wall(bne, 2026, time.November, 9, 14, 0)},
		{"nil receiver", nil, wall(bne, 2026, time.November, 9, 12, 0), false, time.Time{}},
		{"no windows", &PauseWindows{Timezone: "Australia/Brisbane"}, wall(bne, 2026, time.November, 9, 12, 0), false, time.Time{}},
		{"unloadable zone is inactive", &PauseWindows{Timezone: "Mars/Olympus", Windows: []PauseWindow{{Days: "daily", From: "00:00", To: "23:59"}}}, wall(bne, 2026, time.November, 9, 12, 0), false, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			active, until := tc.p.ActiveAt(tc.now)
			if active != tc.active {
				t.Fatalf("active = %v, want %v (until %v)", active, tc.active, until)
			}
			if active && !until.Equal(tc.until) {
				t.Errorf("until = %v, want %v", until, tc.until)
			}
		})
	}
}

// TestPauseWindowsWallClockAcrossDST: a window is wall-clock time, so 11:00-14:00
// must still mean 11:00-14:00 on either side of a DST change — the trap a
// fixed-offset implementation would fall into.
func TestPauseWindowsWallClockAcrossDST(t *testing.T) {
	syd := mustZone(t, "Australia/Sydney")
	p := mustPause(t, &PauseWindows{
		Timezone: "Australia/Sydney",
		Windows:  []PauseWindow{{Days: "mon-fri", From: "11:00", To: "14:00"}},
	})

	// Sydney leaves DST on Sunday 2026-04-05, so the Friday before and the
	// Monday after are on opposite sides of the change.
	fri := wall(syd, 2026, time.April, 3, 12, 0)
	mon := wall(syd, 2026, time.April, 6, 12, 0)

	friActive, friUntil := p.ActiveAt(fri)
	monActive, monUntil := p.ActiveAt(mon)
	if !friActive || !monActive {
		t.Fatalf("active = %v/%v, want both inside the window", friActive, monActive)
	}
	if want := wall(syd, 2026, time.April, 3, 14, 0); !friUntil.Equal(want) {
		t.Errorf("Friday until = %v, want wall-clock %v", friUntil, want)
	}
	if want := wall(syd, 2026, time.April, 6, 14, 0); !monUntil.Equal(want) {
		t.Errorf("Monday until = %v, want wall-clock %v", monUntil, want)
	}
	_, friOffset := friUntil.Zone()
	_, monOffset := monUntil.Zone()
	if friOffset == monOffset {
		t.Fatalf("the fixture does not straddle the DST change (both offsets %d)", friOffset)
	}
}

// TestPauseWindowsFromEqualsTo: equal ends are a whole 24 hours, not a
// zero-length window.
func TestPauseWindowsFromEqualsTo(t *testing.T) {
	bne := mustZone(t, "Australia/Brisbane")
	p := mustPause(t, &PauseWindows{
		Timezone: "Australia/Brisbane",
		Windows:  []PauseWindow{{Days: "sat", From: "00:00", To: "00:00"}},
	})

	for _, tc := range []struct {
		name   string
		now    time.Time
		active bool
	}{
		{"Saturday midnight starts it", wall(bne, 2026, time.November, 14, 0, 0), true},
		{"Saturday nearly midnight", wall(bne, 2026, time.November, 14, 23, 59), true},
		{"Sunday midnight ends it", wall(bne, 2026, time.November, 15, 0, 0), false},
		{"Friday just before the lead-in", wall(bne, 2026, time.November, 13, 23, 59), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if active, until := p.ActiveAt(tc.now); active != tc.active {
				t.Errorf("active = %v, want %v (until %v)", active, tc.active, until)
			}
		})
	}
	if active, until := p.ActiveAt(wall(bne, 2026, time.November, 14, 12, 0)); !active || !until.Equal(wall(bne, 2026, time.November, 15, 0, 0)) {
		t.Errorf("active/until = %v/%v, want true and the next midnight", active, until)
	}
}

// TestPauseWindowsDSTAtMidnight: Chile starts DST at local midnight, so
// time.Date(y, m, d, 0, 0, ...) normalises to 23:00 the previous day and would
// match Sunday's window against Saturday. Day markers are built at noon instead.
func TestPauseWindowsDSTAtMidnight(t *testing.T) {
	sc := mustZone(t, "America/Santiago")
	p := mustPause(t, &PauseWindows{
		Timezone: "America/Santiago",
		Windows:  []PauseWindow{{Days: "sun", From: "02:30", To: "04:00"}},
	})

	for _, tc := range []struct {
		name string
		now  time.Time
	}{
		{"Sunday whose DST starts at midnight", wall(sc, 2026, time.September, 6, 3, 0)},
		{"an ordinary Sunday in DST", wall(sc, 2026, time.November, 8, 3, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if active, until := p.ActiveAt(tc.now); !active || !until.Equal(wall(sc, 2026, tc.now.Month(), tc.now.Day(), 4, 0)) {
				t.Errorf("active/until = %v/%v, want true and 04:00", active, until)
			}
		})
	}

	start, ok := p.NextStart(wall(sc, 2026, time.September, 5, 12, 0))
	if want := wall(sc, 2026, time.September, 6, 2, 30); !ok || !start.Equal(want) {
		t.Errorf("NextStart = %v/%v, want %v", start, ok, want)
	}
}

func TestPauseWindowsNextStart(t *testing.T) {
	bne := mustZone(t, "Australia/Brisbane")
	peak := func(lead time.Duration) *PauseWindows {
		return mustPause(t, &PauseWindows{
			Timezone: "Australia/Brisbane",
			Lead:     Duration(lead),
			Windows:  []PauseWindow{{Days: "mon-fri", From: "11:00", To: "14:00"}},
		})
	}

	for _, tc := range []struct {
		name string
		p    *PauseWindows
		now  time.Time
		want time.Time
		ok   bool
	}{
		{"Friday night skips the weekend", peak(30 * time.Minute), wall(bne, 2026, time.November, 13, 21, 0), wall(bne, 2026, time.November, 16, 10, 30), true},
		{"Saturday skips to Monday", peak(30 * time.Minute), wall(bne, 2026, time.November, 14, 9, 0), wall(bne, 2026, time.November, 16, 10, 30), true},
		{"earlier the same weekday", peak(30 * time.Minute), wall(bne, 2026, time.November, 13, 10, 0), wall(bne, 2026, time.November, 13, 10, 30), true},
		{"nil receiver", nil, wall(bne, 2026, time.November, 13, 21, 0), time.Time{}, false},
		{"no windows", &PauseWindows{Timezone: "Australia/Brisbane"}, wall(bne, 2026, time.November, 13, 21, 0), time.Time{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.p.NextStart(tc.now)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (start %v)", ok, tc.ok, got)
			}
			if ok && !got.Equal(tc.want) {
				t.Errorf("start = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDescribeLeadDropsZeroFields(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Minute:           "30m",
		time.Hour:                  "1h",
		90 * time.Minute:           "1h30m",
		45 * time.Second:           "45s",
		time.Hour + 30*time.Second: "1h0m30s",
	} {
		if got := describeLead(d); got != want {
			t.Errorf("describeLead(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestPauseWindowsDescribe(t *testing.T) {
	p := mustPause(t, &PauseWindows{
		Timezone: "Australia/Brisbane",
		Lead:     Duration(30 * time.Minute),
		Windows: []PauseWindow{
			{Days: "mon-fri", From: "11:00", To: "14:00"},
			{Days: "mon-fri", From: "16:00", To: "20:00"},
		},
	})
	if got, want := p.Describe(), "mon-fri 11:00-14:00, mon-fri 16:00-20:00 (Australia/Brisbane, lead 30m)"; got != want {
		t.Errorf("Describe = %q, want %q", got, want)
	}

	if got := (*PauseWindows)(nil).Describe(); got != "" {
		t.Errorf("nil Describe = %q, want empty", got)
	}
	if got := (&PauseWindows{Timezone: "Australia/Brisbane"}).Describe(); got != "" {
		t.Errorf("no-windows Describe = %q, want empty", got)
	}
}
