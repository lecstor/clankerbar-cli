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
	// pauseHoldStop: a STOP/RESTART/HALT marker or a cancelled context landed
	// during the hold — the ordinary drain-stop path.
	pauseHoldStop
	// pauseHoldRunLost: the plane no longer recognises this run. The DRAIN ends
	// (the claim is dropped, nothing is handed back) but the RUN continues to
	// its next poll, exactly as the mid-drain run-lost ending does
	// (drain_phases.go: "ending the drain: the task is no longer this run's").
	pauseHoldRunLost
)

// holdThroughPause holds a phase sequence at its next boundary while a scheduled
// pause window is active (CLA-906).
//
// A session already running is never interrupted: this runs only before the
// NEXT session spawns. The drain's claim is kept for the whole wait — the next
// phase resumes the same run — and its lease is renewed at the usual
// third-of-lease cadence, so the task is not re-offered as a takeover. Presence
// goes out on the same cadence whether or not the lease can still be renewed,
// to the held target and every sibling project the daemon serves, so a window
// hours long does not make the instance read dead.
//
// It reports how it ended, so the caller can take the ordinary stop path or the
// drain-level run-lost ending. A nil claim (a session-claimed drain that has not
// spawned yet) still waits: there is no lease to renew, but a window must not
// start a session either.
func (s *drainSeq) holdThroughPause(ctx context.Context, st *seqStep) pauseHold {
	d := s.d
	t := s.t
	active, until, pw := d.pauseState()
	if !active {
		return pauseHoldNone
	}

	interval := d.leaseRenewalInterval
	if interval <= 0 {
		interval = heartbeatInterval(planeLeaseDuration)
	}
	// A hold is idle time, not draining time: book it so the budget clocks do
	// not charge the wait (CLA-906 review 1.review-1.5). Clocked through d.now
	// so a test's jump past a window is the credit it measures.
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

	// The flag is the same one the poll gate sets, so presence reads `draining`
	// and the next poll does not repeat a generic entry line.
	d.schedPaused = true
	// Presence names what is waiting: the held claim and the phase that will run
	// when the window ends (CLA-908). Set before the first beacon below, and
	// cleared on every way out of the hold, so no stale hold outlives it.
	heldRef := ""
	if claim != nil && claim.Claim.Held() {
		heldRef = claimLabel(claim.Claim)
	}
	d.holdPause(s.ti, heldRef, st.ph.Label(s.i))
	defer d.clearHoldPause(s.ti)
	// until carries the window's own zone, so the line names the operator's
	// local resume time rather than the daemon's.
	log.Printf("%sscheduled pause active until %s (%s) — holding %s at the %s boundary; %s",
		labelOf(t), until.Format("Mon 15:04 MST"), pw.Describe(), subject, st.ph.Label(s.i), tail)
	// The poll that normally carries presence does not run mid-drain, and the
	// phase's own renewer stopped when its session ended: without this a window
	// hours long would make a healthy instance — and every sibling project this
	// daemon serves — read as dead. The held target's row is a `draining` state
	// carrying the schedule reason, the window's end and the held ref and phase,
	// which is what the console's pause banner reads.
	d.beaconSessionTargets(s.ti, t)

	leave := func() {
		d.schedPaused = false
		log.Printf("%sscheduled pause ended — resuming %s at the %s phase", labelOf(t), subject, st.ph.Label(s.i))
	}
	runID := ""
	if canBeat {
		runID = claim.Claim.RunID
	}
	failures := 0
	// beat renews the lease once. pauseHoldNone keeps the hold going; a stop or
	// a lost run ends it with that disposition.
	beat := func() pauseHold {
		bctx, cancel := context.WithTimeout(ctx, heartbeatBeatTimeout)
		err := hb.Heartbeat(bctx, runID)
		cancel()
		switch {
		case err == nil:
			failures = 0
			return pauseHoldNone
		case errors.Is(err, plane.ErrNotWired):
			// The same degradation the in-session renewer documents: the lease
			// may expire, but the hold itself is still the right answer — the
			// plane cannot release a session that has already ended.
			canBeat = false
			log.Printf("%sscheduled pause: the plane cannot renew a lease (%v) — holding %s without heartbeats; its lease may expire while the window lasts",
				labelOf(t), err, subject)
			return pauseHoldNone
		case ctx.Err() != nil:
			return pauseHoldStop
		case errors.Is(err, plane.ErrRunLost):
			// The task is someone else's now. Continuing to hold would only
			// spawn a session whose own heartbeat is refused; drop the claim so
			// the deferred handback does not try to release another run's task,
			// and leave the pause flag clear because the RUN continues to its
			// next poll rather than ending with this drain.
			log.Printf("%sscheduled pause: the plane refused the lease renewal (%v) — %s is no longer this run's, ending the hold",
				labelOf(t), err, subject)
			s.carried = nil
			d.schedPaused = false
			return pauseHoldRunLost
		default:
			// Retried on the next cadence; three consecutive failures means the
			// plane is unreachable for this window and the beat is hopeless.
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
	// Renew on entry as well as on the cadence: the claim may have been taken,
	// or last renewed, before a slow start prep.
	if canBeat {
		if out := beat(); out != pauseHoldNone {
			return out
		}
	}
	for {
		now := d.clock()
		active, until, _ = d.pauseState()
		if !active {
			leave()
			return pauseHoldResumed
		}
		// Wake at the next cadence, or when the window ends, whichever is first.
		wait := interval
		if remaining := until.Sub(now); remaining < wait {
			wait = remaining
		}
		if wait <= 0 {
			// A fake clock can sit exactly on the window's end while ActiveAt
			// still answers active; a zero wait would spin.
			wait = time.Millisecond
		}
		// waitOrStopHalt keeps STOP, RESTART, HALT and RELOAD responsive between
		// wakes: a hold can last hours, so an operator's shutdown must not wait
		// for the window to end.
		if d.waitOrStopHalt(ctx, wait) {
			return pauseHoldStop
		}
		// Presence goes out on the cadence whatever the lease does: after
		// ErrNotWired or heartbeatGiveUp the hold is still alive and holding.
		d.beaconSessionTargets(s.ti, t)
		if canBeat {
			if out := beat(); out != pauseHoldNone {
				return out
			}
		}
	}
}

// renewalCadence renders a lease-renewal interval the way an operator reads it:
// the production cadence is a third of the plane's thirty-minute lease, and
// Duration.String prints that as "10m0s".
func renewalCadence(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return d.String()
}
