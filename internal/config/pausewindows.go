package config

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// PauseWindows is the operator's recurring "start no new work" schedule: local
// wall-clock windows in which the loop claims nothing new, checked between
// iterations exactly like the console pause, so an in-flight session or phase
// sequence always finishes.
//
// It exists for pricing and quiet hours - "do not spend on peak rates" - which
// is why the windows are wall-clock times in an operator-named zone rather than
// a UTC offset that would drift across a DST change.
type PauseWindows struct {
	// Timezone is the IANA name the windows' wall-clock times are in, e.g.
	// "Australia/Brisbane". Required when Windows is non-empty.
	Timezone string        `json:"timezone"`
	Lead     Duration      `json:"lead,omitempty"` // also pause this long BEFORE each window starts; 0 = none
	Windows  []PauseWindow `json:"windows"`

	// loc caches Timezone once Validate has parsed it: ActiveAt and NextStart
	// are called every iteration and LoadLocation re-reads the zone database.
	// Nil until then, so the methods stay correct when nothing validated this.
	loc *time.Location
}

// PauseWindow is one recurring window: days are the day it STARTS in the
// configured zone, which is what makes an overnight window readable as
// "fri 22:00-06:00" rather than as two entries.
type PauseWindow struct {
	Days string `json:"days"` // "mon-fri", "sat,sun", "mon,wed-fri", "daily"; case-insensitive three-letter names; ranges may not wrap
	From string `json:"from"` // "HH:MM", 24h
	To   string `json:"to"`   // "HH:MM"; To <= From runs past midnight into the NEXT day, so From == To is a whole 24 hours
}

// pauseDays is a weekday set, bit 0 = Sunday to match time.Weekday.
type pauseDays uint8

var pauseDayNames = [7]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

const pauseEveryDay = pauseDays(0x7f)

func (d pauseDays) has(w time.Weekday) bool { return d&(pauseDays(1)<<uint(w)) != 0 }

// Validate reports the first problem, naming the window index and field.
//
// A nil schedule, and one with no windows, are valid: the scheduler is optional,
// and an absent one must never be read as "paused".
func (p *PauseWindows) Validate() error {
	if p == nil || len(p.Windows) == 0 {
		return nil
	}
	tz := strings.TrimSpace(p.Timezone)
	if tz == "" {
		return errors.New("timezone: required when windows are set (an IANA name, e.g. \"Australia/Brisbane\")")
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return fmt.Errorf("timezone %q: %v", tz, err)
	}
	if p.Lead < 0 {
		return errors.New("lead: must not be negative")
	}
	if p.Lead.Duration() > 24*time.Hour {
		return errors.New("lead: must not exceed 24h (it is how long BEFORE a window new work stops)")
	}
	for i, w := range p.Windows {
		if _, err := w.dayMask(); err != nil {
			return fmt.Errorf("windows[%d].days: %v", i, err)
		}
		if _, _, err := parseClock(w.From); err != nil {
			return fmt.Errorf("windows[%d].from: %v", i, err)
		}
		if _, _, err := parseClock(w.To); err != nil {
			return fmt.Errorf("windows[%d].to: %v", i, err)
		}
	}
	p.loc = loc
	return nil
}

// validatePauseWindows is Validate's labelled hook: the config key is prepended
// so the error names the line an operator edits.
func (c *Config) validatePauseWindows() error {
	if err := c.PauseWindows.Validate(); err != nil {
		return fmt.Errorf("pause_windows: %w", err)
	}
	return nil
}

// ActiveAt reports whether now falls inside a window (or its Lead), and if so
// when the pause ends (the end of the window, as an absolute time; windows that
// overlap or abut are merged so the returned end is the true resume time).
func (p *PauseWindows) ActiveAt(now time.Time) (active bool, until time.Time) {
	loc := p.zone()
	if loc == nil || len(p.Windows) == 0 {
		return false, time.Time{}
	}
	lead := p.Lead.Duration()
	insts := p.instancesAround(now.In(loc), loc, 1)

	for _, in := range insts {
		if now.Before(in.start.Add(-lead)) || !now.Before(in.end) {
			continue
		}
		active = true
		if in.end.After(until) {
			until = in.end
		}
	}
	if !active {
		return false, time.Time{}
	}
	// Extend across every window that begins at or before the current end: an
	// abutting or overlapping window (or its lead) is the same pause, and its
	// end is the time work actually resumes.
	for extended := true; extended; {
		extended = false
		for _, in := range insts {
			if in.end.After(until) && !in.start.Add(-lead).After(until) {
				until = in.end
				extended = true
			}
		}
	}
	return true, until
}

// NextStart is when the next pause begins (lead included) strictly after now;
// ok=false when there are no windows.
func (p *PauseWindows) NextStart(now time.Time) (start time.Time, ok bool) {
	loc := p.zone()
	if loc == nil || len(p.Windows) == 0 {
		return time.Time{}, false
	}
	lead := p.Lead.Duration()
	// A week plus a day either side is enough for any weekday mask to recur,
	// however the window straddles midnight.
	insts := p.instancesAround(now.In(loc), loc, 8)

	for _, in := range insts {
		s := in.start.Add(-lead)
		if !s.After(now) {
			continue
		}
		if !ok || s.Before(start) {
			start, ok = s, true
		}
	}
	return start, ok
}

// Describe is a one-line human summary, e.g.
// "mon-fri 11:00-14:00, mon-fri 16:00-20:00 (Australia/Brisbane, lead 30m)".
// An unset schedule describes as empty.
func (p *PauseWindows) Describe() string {
	if p == nil || len(p.Windows) == 0 {
		return ""
	}
	parts := make([]string, len(p.Windows))
	for i, w := range p.Windows {
		parts[i] = fmt.Sprintf("%s %s-%s",
			strings.ToLower(strings.TrimSpace(w.Days)),
			strings.TrimSpace(w.From),
			strings.TrimSpace(w.To))
	}
	inside := strings.TrimSpace(p.Timezone)
	if lead := p.Lead.Duration(); lead > 0 {
		inside += ", lead " + describeLead(lead)
	}
	return fmt.Sprintf("%s (%s)", strings.Join(parts, ", "), inside)
}

// describeLead renders a lead the way an operator writes it ("30m", "1h30m"),
// dropping the zero seconds field time.Duration always prints.
func describeLead(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// zone resolves the configured location, or nil when it cannot be: callers then
// report "not paused", which is the only safe answer for a schedule whose times
// cannot be placed on the clock.
func (p *PauseWindows) zone() *time.Location {
	if p == nil {
		return nil
	}
	tz := strings.TrimSpace(p.Timezone)
	if tz == "" {
		return nil
	}
	if p.loc != nil {
		return p.loc
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil
	}
	return loc
}

// pauseInstance is one window's occurrence on one local calendar day; the pause
// runs from Lead before start until end.
type pauseInstance struct{ start, end time.Time }

// instance builds this window's occurrence on day, in loc. Both ends are built
// with time.Date in the ZONE, so a window means wall-clock time on either side
// of a DST change rather than a fixed offset.
func (w PauseWindow) instance(day time.Time, loc *time.Location) (pauseInstance, bool) {
	fromH, fromM, err := parseClock(w.From)
	if err != nil {
		return pauseInstance{}, false
	}
	toH, toM, err := parseClock(w.To)
	if err != nil {
		return pauseInstance{}, false
	}
	// A From inside a spring-forward gap normalises past the gap, so that day's
	// window can be shorter or empty: the wall-clock window does not exist that day.
	start := time.Date(day.Year(), day.Month(), day.Day(), fromH, fromM, 0, 0, loc)
	endDay := day.Day()
	if toH*60+toM <= fromH*60+fromM {
		endDay++ // To <= From runs past midnight into the NEXT day.
	}
	end := time.Date(day.Year(), day.Month(), endDay, toH, toM, 0, 0, loc)
	return pauseInstance{start: start, end: end}, true
}

// instancesAround returns every window occurrence that starts within days local
// calendar days of now, either side. Days are matched against the day the
// window STARTS on, which is the documented meaning of a window's days.
func (p *PauseWindows) instancesAround(now time.Time, loc *time.Location, days int) []pauseInstance {
	y, m, d := now.Date()
	var out []pauseInstance
	for off := -days; off <= days; off++ {
		// Noon, not midnight: a DST change at local midnight normalises 00:00 to
		// 23:00 of the previous day, which drops that day's window. No zone
		// transitions happen at noon.
		day := time.Date(y, m, d+off, 12, 0, 0, 0, loc)
		for _, w := range p.Windows {
			mask, err := w.dayMask()
			if err != nil || !mask.has(day.Weekday()) {
				continue
			}
			if in, ok := w.instance(day, loc); ok {
				out = append(out, in)
			}
		}
	}
	return out
}

// dayMask parses the days spec: "daily", or comma-separated three-letter names
// and "lo-hi" ranges.
func (w PauseWindow) dayMask() (pauseDays, error) {
	spec := strings.ToLower(strings.TrimSpace(w.Days))
	if spec == "" {
		return 0, errors.New("empty — name the days (\"mon-fri\", \"sat,sun\", \"daily\")")
	}
	if spec == "daily" {
		return pauseEveryDay, nil
	}
	var mask pauseDays
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		lo, hi := part, part
		if i := strings.IndexByte(part, '-'); i >= 0 {
			lo, hi = strings.TrimSpace(part[:i]), strings.TrimSpace(part[i+1:])
		}
		loIdx, ok := pauseDayIndex(lo)
		if !ok {
			return 0, fmt.Errorf("%q is not a day (want mon, tue, wed, thu, fri, sat, sun, or daily)", lo)
		}
		hiIdx, ok := pauseDayIndex(hi)
		if !ok {
			return 0, fmt.Errorf("%q is not a day (want mon, tue, wed, thu, fri, sat, sun, or daily)", hi)
		}
		if loIdx > hiIdx {
			return 0, fmt.Errorf("%q runs backwards — ranges may not wrap; write the days after it out (\"fri,sat,sun,mon\")", part)
		}
		for i := loIdx; i <= hiIdx; i++ {
			mask |= pauseDays(1) << uint(i)
		}
	}
	return mask, nil
}

func pauseDayIndex(name string) (int, bool) {
	for i, day := range pauseDayNames {
		if name == day {
			return i, true
		}
	}
	return 0, false
}

// parseClock parses "HH:MM" (24h). Strict on purpose: "9:30" and "09:30:00"
// are the two shapes a hand-edited config gets wrong, and a permissive parser
// would accept one and misread the other.
func parseClock(s string) (hour, minute int, err error) {
	s = strings.TrimSpace(s)
	if len(s) != 5 || s[2] != ':' || !isDigit(s[0]) || !isDigit(s[1]) || !isDigit(s[3]) || !isDigit(s[4]) {
		return 0, 0, fmt.Errorf("%q is not HH:MM (24h, e.g. 09:30)", s)
	}
	hour = int(s[0]-'0')*10 + int(s[1]-'0')
	minute = int(s[3]-'0')*10 + int(s[4]-'0')
	if hour > 23 {
		return 0, 0, fmt.Errorf("%q is not HH:MM (24h) — the hour must be 00-23", s)
	}
	if minute > 59 {
		return 0, 0, fmt.Errorf("%q is not HH:MM (24h) — the minute must be 00-59", s)
	}
	return hour, minute, nil
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }
