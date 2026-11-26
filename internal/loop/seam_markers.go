package loop

import (
	"context"
	"log"
)

// STOP and RESTART act at a VERIFIED PHASE SEAM as well as between iterations. A seam is only safe
// where the resume mechanism actually exists: a runner-claimed drain whose checkpoint is verified.
func (s *drainSeq) stopAtSeamMarker(ctx context.Context, st *seqStep, nextIdx int, nextPhase string) bool {
	d := s.d
	// Only a runner-claimed sequence writes `phasesDone`, and only a checkpoint with a next phase
	// would resume anywhere.
	if d.state == nil || s.carried == nil || !s.runnerClaimed || !st.end.checkpoint ||
		nextIdx < 0 || nextIdx >= len(s.phases) {
		return false
	}
	// Verified checkpoints only. The CLA-576 unverified form writes no `phasesDone` marker
	// (markPhaseDone skips it), and a HANDOFF checkpoint writes none either — markPhaseDone cannot
	// know yet whether the handoff will be accepted, so it skips them all.
	if s.carried.CheckpointUnverified || st.end.handoff != "" {
		return false
	}
	// Read-then-remove is not atomic; controlMu serialises it against a concurrent waitOrStop consumer
	// of the same markers.
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

	// Release exactly as the RESTART_NOW path does, through the drain's own handback: a held claim
	// with no pushed work goes straight back to the queue; one with pushed work has its lease ended so
	// the takeover hand-off survives.
	ref := claimLabel(s.carried.Claim)
	pushed := !s.carried.Claim.Releasable()
	handed, _ := d.handBackClaim(ctx, s.t, *s.carried, false, false)
	// A FAILED handback keeps the claim: the deferred handback at the end of the drain is a free retry
	// before the daemon exits.
	if handed {
		s.carried = nil
	}
	if marker == MarkerRestart {
		d.restartRequested.Store(true)
	}
	s.stop = true

	after := s.phases[s.i].Label(s.i)
	next := nextPhase
	if next == "" {
		next = s.phases[nextIdx].Label(nextIdx)
	}
	note := ""
	if msg != "" {
		note = " (" + msg + ")"
	}
	action := "stopping"
	if marker == MarkerRestart {
		action = "re-executing"
	}
	if handed {
		log.Printf("%siteration %d: %s at the phase seam after %s%s — released %s; %s, and the next claim resumes at %s from its checkpoint",
			labelOf(s.t), s.drainNum, marker, after, note, ref, action, next)
	} else {
		tail := ""
		if pushed {
			tail = " (pushed work keeps the takeover hand-off)"
		}
		log.Printf("%siteration %d: %s at the phase seam after %s%s — could not hand %s back, so its lease will end on its own%s; %s, and the next claim resumes at %s from its checkpoint",
			labelOf(s.t), s.drainNum, marker, after, note, ref, tail, action, next)
	}
	return true
}
