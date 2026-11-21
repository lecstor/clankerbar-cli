package loop

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// pauseHold says how a scheduled-pause hold ended (CLA-906).
type pauseHold int

const (
	// pauseHoldNone: no window was active, nothing was held.
	pauseHoldNone pauseHold = iota
	// pauseHoldResumed: the window ended and the next session may spawn.
	pauseHoldResumed
	// pauseHoldStop: a STOP/RESTART/HALT marker or a cancelled context landed during the hold — the
	// ordinary drain-stop path.
	pauseHoldStop
	// pauseHoldRunLost: the plane no longer recognises this run. The DRAIN ends (the claim is dropped,
	// nothing is handed back) but the RUN continues to its next poll, exactly as the mid-drain
	// run-lost ending does (drain_phases.go: "ending the drain: the task is no longer this run's").
	pauseHoldRunLost
)

// holdThroughPause holds a phase sequence at its next boundary while a scheduled pause window is
// active. A session already running is never interrupted: this runs only before the NEXT session
// spawns.
func (s *drainSeq) holdThroughPause(ctx context.Context, st *seqStep) pauseHold {
	return s.holdSequence(ctx, st, func() boundaryHoldState {
		active, until, pw := s.d.pauseState()
		if !active {
			return boundaryHoldState{}
		}
		return boundaryHoldState{active: true, until: until, label: "scheduled pause", detail: pw.Describe(), zone: pw.Timezone}
	})
}

// boundaryHoldState is why a boundary hold is waiting, re-read at every wake so a RELOAD that
// removes or shortens the cause ends the hold early.
type boundaryHoldState struct {
	active bool
	until  time.Time
	label  string // "scheduled pause" / "route windows", for the log lines
	detail string // the schedule's own description, in the entry line's parens
	zone   string // IANA zone the instant is rendered in (the presence row)
}

// holdSequence holds the sequence at its next boundary while decide reports a cause.
func (s *drainSeq) holdSequence(ctx context.Context, st *seqStep, decide func() boundaryHoldState) pauseHold {
	d := s.d
	t := s.t
	hs := decide()
	if !hs.active {
		return pauseHoldNone
	}
	label := hs.label
	until := hs.until

	interval := d.leaseRenewalInterval
	if interval <= 0 {
		interval = heartbeatInterval(planeLeaseDuration)
	}
	// A hold is idle time, not draining time: book it so the budget clocks do not charge the wait.
	holdStart := d.clock()
	defer func() { d.notePauseHeld(s.ti, d.clock().Sub(holdStart)) }()
	hb, _ := t.Releaser.(plane.Heartbeat)
	claim := s.carried
	canBeat := hb != nil && claim != nil && claim.Claim.Held() && claim.Claim.RunID != ""
	subject := "the sequence"
	if claim != nil && claim.Claim.Held() {
		subject = claimLabel(claim.Claim)
	}
	tail := "the sequence resumes at that phase when the window ends"
	if claim != nil && claim.Claim.Held() {
		if canBeat {
			tail = fmt.Sprintf("renewing its lease every %s; resuming at that phase when the window ends",
				renewalCadence(interval))
		} else {
			tail = "keeping its claim for the next phase; resuming when the window ends"
		}
	}

	// The flag is the same one the poll gate sets, so presence reads `draining` and the next poll does
	// not repeat a generic entry line.
	d.schedPaused = true
	// Presence names what is waiting: the held claim and the phase that will run when the window ends.
	heldRef := ""
	if claim != nil && claim.Claim.Held() {
		heldRef = claimLabel(claim.Claim)
	}
	d.holdPause(s.ti, heldRef, st.ph.Label(s.i), until, hs.zone)
	defer d.clearHoldPause(s.ti)
	// until carries the window's own zone, so the line names the operator's local resume time rather
	// than the daemon's.
	log.Printf("%s%s active until %s (%s) — holding %s at the %s boundary; %s",
		labelOf(t), label, until.Format("Mon 15:04 MST"), hs.detail, subject, st.ph.Label(s.i), tail)
	// The poll that normally carries presence does not run mid-drain, and the phase's own renewer
	// stopped when its session ended: without this a window hours long would make a healthy instance
	// — and every sibling project this daemon serves — read as dead.
	d.beaconSessionTargets(s.ti, t)

	leave := func() {
		d.schedPaused = false
		log.Printf("%s%s ended — resuming %s at the %s phase", labelOf(t), label, subject, st.ph.Label(s.i))
	}
	runID := ""
	if canBeat {
		runID = claim.Claim.RunID
	}
	failures := 0
	// beat renews the lease once. pauseHoldNone keeps the hold going; a stop or a lost run ends it
	// with that disposition.
	beat := func() pauseHold {
		if !canBeat {
			return pauseHoldNone
		}
		bctx, cancel := context.WithTimeout(ctx, heartbeatBeatTimeout)
		err := hb.Heartbeat(bctx, runID)
		cancel()
		switch {
		case err == nil:
			failures = 0
			return pauseHoldNone
		case errors.Is(err, plane.ErrNotWired):
			// The same degradation the in-session renewer documents: the lease may expire, but the hold
			// itself is still the right answer — the plane cannot release a session that has already
			// ended.
			canBeat = false
			log.Printf("%sscheduled pause: the plane cannot renew a lease (%v) — holding %s without heartbeats; its lease may expire while the window lasts",
				labelOf(t), err, subject)
			return pauseHoldNone
		case ctx.Err() != nil:
			return pauseHoldStop
		case errors.Is(err, plane.ErrRunLost):
			// The task is someone else's now. Continuing to hold would only spawn a session whose own
			// heartbeat is refused; drop the claim so the deferred handback does not try to release another
			// run's task, and leave the pause flag clear because the RUN continues to its next poll rather
			// than ending with this drain.
			log.Printf("%sscheduled pause: the plane refused the lease renewal (%v) — %s is no longer this run's, ending the hold",
				labelOf(t), err, subject)
			s.carried = nil
			d.schedPaused = false
			return pauseHoldRunLost
		default:
			// Retried on the next cadence; three consecutive failures means the plane is unreachable for
			// this window and the beat is hopeless.
			failures++
			log.Printf("%sscheduled pause: lease renewal failed (%d in a row): %v", labelOf(t), failures, err)
			if failures >= heartbeatGiveUp {
				canBeat = false
				log.Printf("%sscheduled pause: giving up on lease renewal after %d consecutive failures — holding %s without heartbeats; its lease may expire while the window lasts",
					labelOf(t), failures, subject)
			}
			return pauseHoldNone
		}
	}
	// Renew on entry as well as on the cadence: the claim may have been taken, or last renewed, before
	// a slow start prep.
	if canBeat {
		if out := beat(); out != pauseHoldNone {
			return out
		}
	}
	return s.holdWakeLoop(ctx, st, decide, interval, heldRef, beat, leave, until)
}

// holdWakeLoop is the hold's wait proper: sleep to the next renewal cadence or the window's end,
// re-decide, beacon, renew, and report how the hold ended.
func (s *drainSeq) holdWakeLoop(ctx context.Context, st *seqStep, decide func() boundaryHoldState,
	interval time.Duration, heldRef string, beat func() pauseHold, leave func(), until time.Time) pauseHold {
	d := s.d
	t := s.t
	for {
		now := d.clock()
		hs := decide()
		if !hs.active {
			leave()
			return pauseHoldResumed
		}
		until = hs.until
		// Re-publish the hold with the fresh instant: a RELOAD can shorten or move the window mid-hold,
		// and the presence row must not count down to an instant the config no longer says.
		d.holdPause(s.ti, heldRef, st.ph.Label(s.i), until, hs.zone)
		// Wake at the next cadence, or when the window ends, whichever is first.
		wait := interval
		if remaining := until.Sub(now); remaining < wait {
			wait = remaining
		}
		if wait <= 0 {
			// A fake clock can sit exactly on the window's end while ActiveAt still answers active; a zero
			// wait would spin.
			wait = time.Millisecond
		}
		// waitOrStopHalt keeps STOP, RESTART, HALT and RELOAD responsive between wakes: a hold can last
		// hours, so an operator's shutdown must not wait for the window to end.
		if d.waitOrStopHalt(ctx, wait) {
			return pauseHoldStop
		}
		// Presence goes out on the cadence whatever the lease does: after ErrNotWired or heartbeatGiveUp
		// the hold is still alive and holding.
		d.beaconSessionTargets(s.ti, t)
		if out := beat(); out != pauseHoldNone {
			return out
		}
	}
}

// renewalCadence renders a lease-renewal interval the way an operator reads it: the production
// cadence is a third of the plane's thirty-minute lease, and Duration.String prints that as
// "10m0s".
func renewalCadence(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return d.String()
}
