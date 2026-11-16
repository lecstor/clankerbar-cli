package loop

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// holdThroughPause holds a phase sequence at its next boundary while a scheduled
// pause window is active (CLA-906).
//
// A session already running is never interrupted: this runs only before the
// NEXT session spawns. The drain's claim is kept for the whole wait — the next
// phase resumes the same run — and its lease is renewed at the usual
// third-of-lease cadence, so the task is not re-offered as a takeover.
//
// It reports held, so the caller can re-judge a wall-clock budget the wait may
// have outlasted, and stop when a STOP/RESTART marker or a cancelled context
// landed during the wait. A nil claim (a session-claimed drain that has not
// spawned yet) still waits: there is no lease to renew, but a window must not
// start a session either.
func (s *drainSeq) holdThroughPause(ctx context.Context, st *seqStep) (held, stop bool) {
	d := s.d
	t := s.t
	active, until, pw := d.pauseState()
	if !active {
		return false, false
	}
	held = true

	interval := d.leaseRenewalInterval
	if interval <= 0 {
		interval = heartbeatInterval(planeLeaseDuration)
	}
	hb, _ := t.Releaser.(plane.Heartbeat)
	claim := s.carried
	canBeat := hb != nil && claim != nil && claim.Claim.Held() && claim.Claim.RunID != ""
	subject := "the sequence"
	if claim != nil && claim.Claim.Held() {
		subject = claimLabel(claim.Claim)
	}
	renewal := "keeping the claim for the next phase"
	if canBeat {
		renewal = fmt.Sprintf("renewing the lease of %s every %s", subject, interval)
	}

	// The flag is the same one the poll gate sets, so presence reads `draining`
	// and the next poll does not repeat a generic entry line.
	d.schedPaused = true
	// until carries the window's own zone, so the line names the operator's
	// local resume time rather than the daemon's.
	log.Printf("%sscheduled pause active until %s (%s) — holding %s at the %s boundary; %s until the window ends",
		labelOf(t), until.Format("Mon 15:04 MST"), pw.Describe(), subject, st.ph.Label(s.i), renewal)
	// The poll that normally carries presence does not run mid-drain, and the
	// phase's own renewer stopped when its session ended: without this a window
	// hours long would make a healthy instance read as dead.
	d.beacon(s.ti, t, fleet.State{Kind: fleet.StateDraining})

	leave := func() {
		d.schedPaused = false
		log.Printf("%sscheduled pause ended — resuming %s at the %s phase", labelOf(t), subject, st.ph.Label(s.i))
	}
	runID := ""
	if canBeat {
		runID = claim.Claim.RunID
	}
	failures := 0
	for {
		now := d.clock()
		active, until, _ = d.pauseState()
		if !active {
			leave()
			return held, false
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
		// waitOrStop keeps STOP/RESTART/RELOAD responsive between wakes.
		if d.waitOrStop(ctx, wait) {
			return held, true
		}
		if !canBeat {
			continue
		}
		bctx, cancel := context.WithTimeout(ctx, heartbeatBeatTimeout)
		err := hb.Heartbeat(bctx, runID)
		cancel()
		switch {
		case err == nil:
			failures = 0
			d.beacon(s.ti, t, fleet.State{Kind: fleet.StateDraining})
		case errors.Is(err, plane.ErrNotWired):
			// The same degradation the in-session renewer documents: the lease
			// may expire, but the hold itself is still the right answer — the
			// plane cannot release a session that has already ended.
			canBeat = false
			log.Printf("%sscheduled pause: the plane cannot renew a lease (%v) — holding %s without heartbeats; its lease may expire while the window lasts",
				labelOf(t), err, subject)
		case ctx.Err() != nil:
			return held, true
		case errors.Is(err, plane.ErrRunLost):
			// The task is someone else's now. Continuing to hold would only
			// spawn a session whose own heartbeat is refused; drop the claim so
			// the deferred handback does not try to release another run's task.
			log.Printf("%sscheduled pause: the plane refused the lease renewal (%v) — %s is no longer this run's, ending the hold",
				labelOf(t), err, subject)
			s.carried = nil
			return held, true
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
		}
	}
}
