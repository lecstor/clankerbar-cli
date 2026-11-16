package supervisor

// The fleet listing's half of CLA-908: a live child whose own loop is stopped by
// an operator-configured pause window says so, with the window's end and any task
// held at a phase boundary. It reads the child's local BEACON — the same payload
// the plane POST carries — so the listing and the console's pause banner agree by
// construction rather than by two writers that could drift.

import (
	"fmt"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/fleet"
)

// schedulePauseText renders the schedule pause a child's local beacon states, or
// "" when it states none: "paused by schedule until 14:00 AEST", plus the held
// task when the daemon named one.
//
// Only a `draining` beacon carrying the schedule reason counts. A console, fleet,
// startup or budget pause names itself on the wire, but its wait has no stated
// end and is not this listing's sentence to write: the console shows it on the
// instance the operator paused, which this daemon-wide line cannot attribute.
// now is the reference for "today" in the schedule's own zone; the instant is
// rendered in the zone the daemon wrote beside it, which is the operator's
// configured wall clock, not this process's.
func schedulePauseText(dir string, now time.Time) string {
	if dir == "" {
		return ""
	}
	b := readLocalBeacon(dir)
	if b == nil || b.State != fleet.StateDraining || b.PauseReason != fleet.PauseReasonSchedule {
		return ""
	}
	// A stated end already past is a window that ENDED while the child's beacon
	// has not caught up: the plane's instance view treats that pause as over, so
	// the listing does too rather than showing a resume time in the past.
	if b.PauseUntil != nil && !b.PauseUntil.After(now) {
		return ""
	}
	text := "paused by schedule"
	if until := pauseUntilText(b.PauseUntil, b.PauseZone, now); until != "" {
		text += " until " + until
	}
	if b.TaskRef != "" {
		text += " (holding " + b.TaskRef
		if b.Phase != "" {
			text += " at the " + b.Phase + " phase"
		}
		text += ")"
	}
	return text
}

// pauseUntilText renders the window's end the way the operator configured it:
// the clock time and zone abbreviation, with the day named only when the end is
// not today in that zone (an overnight window would otherwise read as though it
// ended hours ago). Empty when the beacon stated no end — the daemon reports a
// reason alone when it cannot state a moment, and a guessed one would be worse
// than none.
func pauseUntilText(until *time.Time, zone string, now time.Time) string {
	if until == nil || until.IsZero() {
		return ""
	}
	at := until.UTC()
	if name := strings.TrimSpace(zone); name != "" {
		if loc, err := time.LoadLocation(name); err == nil {
			at = at.In(loc)
		}
	}
	clock := at.Format("15:04 MST")
	local := now.In(at.Location())
	if at.Year() == local.Year() && at.YearDay() == local.YearDay() {
		return clock
	}
	return fmt.Sprintf("%s %s", at.Format("Mon"), clock)
}
