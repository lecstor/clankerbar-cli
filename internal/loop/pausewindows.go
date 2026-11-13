package loop

import (
	"context"
	"log"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// clock is the driver's clock, falling back to time.Now so a Driver built by
// hand (a test's &Driver{}) needs no clock of its own.
func (d *Driver) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

// scheduledPause reports whether an operator-configured pause window is active
// (config.PauseWindows), logging the entry and the exit once each so a window
// hours long does not repeat the line on every poll.
//
// Like the console pause it is read BETWEEN iterations, so a window opening or
// closing never interrupts a session or a phase sequence already in flight.
func (d *Driver) scheduledPause() bool {
	var pw *config.PauseWindows
	if d.cfg != nil {
		pw = d.cfg.PauseWindows
	}
	active, until := pw.ActiveAt(d.clock())
	switch {
	case active && !d.schedPaused:
		d.schedPaused = true
		// until carries the window's own zone, so the line names the operator's
		// local resume time rather than the daemon's.
		log.Printf("scheduled pause active until %s (%s) — not spawning new sessions; idle-polling until it ends",
			until.Format("Mon 15:04 MST"), pw.Describe())
	case !active && d.schedPaused:
		d.schedPaused = false
		log.Print("scheduled pause ended — resuming")
	}
	return active
}

// idleThroughPause is the blind-mode answer to an active window: pick no target
// and idle-poll, the same shape the wired path takes with no candidate.
func (d *Driver) idleThroughPause(runCtx context.Context, idle time.Duration) runNext {
	if d.waitOrStop(runCtx, idle) {
		return runStop
	}
	return runContinue
}
