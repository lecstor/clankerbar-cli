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
// It is read BETWEEN iterations and at every phase boundary (CLA-906). A session
// already running is never interrupted; a window holds the NEXT spawn — the next
// phase, a handoff respawn, or a drain's first session — and holdThroughPause is
// what keeps the claim alive while that waits.
func (d *Driver) scheduledPause() bool {
	active, until, pw := d.pauseState()
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

// pauseState is scheduledPause's read without its transition logging: whether a
// window is in force now, the instant it ends, and its schedule for a log line.
// A nil config or schedule is inactive.
//
// d.cfg is read under controlMu because a RELOAD swaps it on another goroutine (a
// panel member parked in waitResponsive), and since CLA-908 this is reachable
// from the lease renewer's presence lap as well as the loop. The returned
// schedule is immutable: a reload installs a new config rather than editing this
// one, so reading it after the unlock is safe.
func (d *Driver) pauseState() (active bool, until time.Time, pw *config.PauseWindows) {
	d.controlMu.Lock()
	if d.cfg != nil {
		pw = d.cfg.PauseWindows
	}
	d.controlMu.Unlock()
	active, until = pw.ActiveAt(d.clock())
	return active, until, pw
}

// idleThroughPause is the blind-mode answer to an active window: pick no target
// and idle-poll, the same shape the wired path takes with no candidate.
func (d *Driver) idleThroughPause(runCtx context.Context, idle time.Duration) runNext {
	if d.waitOrStop(runCtx, idle) {
		return runStop
	}
	return runContinue
}
