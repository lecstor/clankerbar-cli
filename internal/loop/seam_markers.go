package loop

import (
	"context"
	"log"
)

// Seam control (CLA-879). STOP and RESTART are honoured at a VERIFIED PHASE
// SEAM, not only between iterations: one iteration runs a task's WHOLE phase
// sequence, so a marker dropped while a four-phase sequence was in flight used to
// wait for implement -> review -> fix -> verify to finish (one real drain ran 2h+
// and was SIGKILLed by systemd's TimeoutStopSec 2.5 minutes into verify). The
// seam is always available: each phase checkpoints and pushes, and a later claim
// resumes at the next phase from `phasesDone`.
//
// The seam is a safe place to let go. The phase has already ended, its checkpoint
// is recorded and verified, and the run holds no live session — so there is no
// kill, no salvage and no lost work: releasing the claim hands the task to the
// next claim, which resumes the next phase from the checkpoint exactly as this
// daemon would have.
//
// Deliberately NOT read here:
//   - RELOAD and RESTART_NOW keep their existing boundaries. RESTART_NOW's whole
//     point is to cut a LIVE session short, and its watcher owns that; with no
//     session in flight it acts as a plain restart at the iteration boundary.
//   - HALT keeps its iteration-boundary and supervised-wait reads: it is the
//     operator's note and is left in place for the next start to report.
//   - PROJECT PAUSE (the console `sum.Paused` flag and `scheduledPause`) must
//     never stop a sequence. A pause only gates NEW claims; the task in flight
//     runs every remaining phase to the end. Pausing is how the operator gets
//     in-flight tasks completed (decision 6019ea26-1464-43a3-a152-3271510b8b42),
//     so pausing must not behave like a soft stop.
//
// It is the phase twin of checkMarkers (run.go), which reads the same markers at
// the iteration boundary.
func (s *drainSeq) stopAtSeamMarker(ctx context.Context, st *seqStep, nextIdx int) bool {
	d := s.d
	// Only a VERIFIED checkpoint is a resumable seam, and only when a next phase
	// would actually spawn on the held claim. A phase that ended without one keeps
	// today's handling: drainPhases has already ended the walk above (nothing for
	// a successor to resume), and the marker lands at the iteration boundary or
	// the next wait exactly as before.
	if d.state == nil || s.carried == nil || !st.end.checkpoint || nextIdx < 0 || nextIdx >= len(s.phases) {
		return false
	}
	// Read-then-remove is not atomic, and controlMu is the lock every other
	// marker consumer takes (CLA-697). Held only for the marker read: the claim
	// release below can reach the plane and must not block another consumer.
	d.controlMu.Lock()
	var marker, msg string
	if present, m := d.readMarker("STOP"); present {
		_ = d.state.Remove("STOP")
		marker, msg = "STOP", m
	} else if present, m := d.readMarker(MarkerRestart); present {
		_ = d.state.Remove(MarkerRestart)
		marker, msg = MarkerRestart, m
	}
	d.controlMu.Unlock()
	if marker == "" {
		return false
	}

	// Release exactly as the RESTART_NOW path does, through the drain's own
	// handback: a held claim with no pushed work goes straight back to the queue;
	// one with pushed work has its lease ended so the takeover hand-off survives.
	// There is no session to kill — the phase has ended.
	ref := claimLabel(s.carried.Claim)
	released := d.releaseHeldClaim(ctx, s.t, *s.carried, false)
	s.carried = nil
	if marker == MarkerRestart {
		d.restartRequested.Store(true)
	}
	s.stop = true

	after := s.phases[s.i].Label(s.i)
	next := s.phases[nextIdx].Label(nextIdx)
	note := ""
	if msg != "" {
		note = " (" + msg + ")"
	}
	action := "stopping"
	if marker == MarkerRestart {
		action = "re-executing"
	}
	if released {
		log.Printf("%siteration %d: %s at the phase seam after %s%s — released %s; %s, and the next phase (%s) resumes from its checkpoint on the next claim",
			labelOf(s.t), s.drainNum, marker, after, note, ref, action, next)
	} else {
		log.Printf("%siteration %d: %s at the phase seam after %s%s — could not hand %s back, so its lease will end on its own (pushed work keeps the takeover hand-off); %s, and the next phase (%s) resumes from its checkpoint on the next claim",
			labelOf(s.t), s.drainNum, marker, after, note, ref, action, next)
	}
	return true
}
