package loop

import "log"

// endRunLost ends a phase whose session the renewer killed because the plane says
// the run no longer holds the task: nothing is owed a handback (the claim is not
// ours) and the drain ends.
func (r *phaseRun) endRunLost(at *phaseAttempt) attemptVerdict {
	d := r.d
	t := r.t
	drainNum := r.drainNum
	phaseIdx := r.phaseIdx
	ph := r.ph
	// The session ran and was cut short, so its spend is real and is counted as
	// countAttemptSpend counts a trusted stream. An untrusted stream is never
	// counted anywhere, and it cannot reach here: a session whose stream could not
	// be read whole is refused before the plane's answer matters.
	if at.res.Untrusted == "" {
		r.countTrustedSpend(at)
	}
	r.end.runLost = true
	// released says "nothing is owed": the claim is not this run's to hand back.
	r.end.released = true
	r.end.claim = nil
	r.end.dead = false
	r.end.empty = false
	r.end.producedNothing = false
	if r.end.session != nil {
		r.end.session.Cause = "run_lost"
	}
	// CURRENT_CLAIM is what the supervisor hands back for a child that died
	// holding a claim; this run must not ask it to.
	d.clearCurrentClaim()
	log.Printf("%siteration %d: the %s phase was ended: the plane says this run no longer holds the task (%v) - nothing salvaged, nothing handed back, ending the drain (log: %s)",
		labelOf(t), drainNum, ph.Label(phaseIdx), at.runLost, at.logPath)
	return attemptVerdict{next: attemptReturn}
}
