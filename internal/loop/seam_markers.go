package loop

import (
	"context"
	"log"
)

// STOP and RESTART act at a VERIFIED PHASE SEAM as well as between iterations
// (CLA-879). Releasing a checkpointed claim at the seam lets the next claim
// resume at the following phase from `phasesDone`, so a stop or roll waits one
// phase session instead of a whole task sequence.
//
// A seam is only safe where the resume mechanism actually exists: a
// runner-claimed drain whose checkpoint is verified. PROJECT PAUSE is
// deliberately not read here — it gates new claims and lets the in-flight task
// finish every phase (decision 6019ea26-1464-43a3-a152-3271510b8b42). RELOAD,
// RESTART_NOW and HALT keep their existing boundaries.
func (s *drainSeq) stopAtSeamMarker(ctx context.Context, st *seqStep, nextIdx int, nextPhase string) bool {
	d := s.d
	// Only a runner-claimed sequence writes `phasesDone`, and only a checkpoint
	// with a next phase would resume anywhere. Everything else keeps today's
	// iteration-boundary handling.
	if d.state == nil || s.carried == nil || !s.runnerClaimed || !st.end.checkpoint ||
		nextIdx < 0 || nextIdx >= len(s.phases) {
		return false
	}
	// Verified checkpoints only. The CLA-576 unverified form writes no
	// `phasesDone` marker (markPhaseDone skips it), and a HANDOFF checkpoint
	// writes none either — markPhaseDone cannot know yet whether the handoff
	// will be accepted, so it skips them all. Acting on either would let the
	// next claim re-run this phase instead of resuming the next.
	if s.carried.CheckpointUnverified || st.end.handoff != "" {
		return false
	}
	// Read-then-remove is not atomic; controlMu serialises it against a
	// concurrent waitOrStop consumer of the same markers. Held only for the
	// read — the claim release below reaches the plane and must not block it.
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
	// handback: a held claim with no pushed work goes straight back to the
	// queue; one with pushed work has its lease ended so the takeover hand-off
	// survives. There is no session to kill — the phase has already ended.
	ref := claimLabel(s.carried.Claim)
	pushed := !s.carried.Claim.Releasable()
	handed, _ := d.handBackClaim(ctx, s.t, *s.carried, false)
	// A FAILED handback keeps the claim: the deferred handback at the end of the
	// drain is a free retry before the daemon exits.
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
