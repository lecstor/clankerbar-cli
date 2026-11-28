package loop

import (
	"context"
	"log"
	"sync"

	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/salvage"
)

// endRunLost ends a phase whose session the renewer killed because the plane says the run no longer
// holds the task: nothing is owed a handback (the claim is not ours) and the drain ends. The
// worktree was already salvaged to the task branch by salvageLostRunWork.
func (r *phaseRun) endRunLost(at *phaseAttempt) attemptVerdict {
	d := r.d
	t := r.t
	drainNum := r.drainNum
	phaseIdx := r.phaseIdx
	ph := r.ph
	// The session ran and was cut short, so its spend is real and is counted as countAttemptSpend
	// counts a trusted stream.
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
	// CURRENT_CLAIM is what the supervisor hands back for a child that died holding a claim; this run
	// must not ask it to.
	d.clearCurrentClaim()
	log.Printf("%siteration %d: the %s phase was ended: the plane says this run no longer holds the task (%v) - nothing handed back, ending the drain (log: %s)",
		labelOf(t), drainNum, ph.Label(phaseIdx), at.runLost, at.logPath)
	return attemptVerdict{next: attemptReturn}
}

// salvageLostRunWork commits and pushes what a run-lost session left uncommitted, onto the task's
// branch. The branch is still the task's even though the claim is not this run's, and a dirty tree
// makes the next claim refuse the worktree. It touches git only: recording the branch or handing
// the claim back would act on a run the plane has already ended.
func (d *Driver) salvageLostRunWork(ctx context.Context, t Target, c harness.Claim, workdir string) {
	if d.newSalvager == nil || c.TaskID == "" {
		return
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), salvageTimeout)
	defer cancel()
	out := d.newSalvager(workdir).Salvage(sctx, c.TaskID, c.Ref, c.Branch)
	label := claimLabel(c)
	switch out.Status {
	case salvage.Saved:
		log.Printf("%srun lost on %s: pushed its uncommitted work as %s on %s (the branch is not recorded on the task - this run no longer holds it): %s",
			labelOf(t), label, shortCommit(out.Commit), out.Branch, out.Detail)
	case salvage.Nothing:
		if out.Worktree != "" {
			log.Printf("%srun lost on %s: nothing to salvage: %s", labelOf(t), label, out.Detail)
		}
	case salvage.Untracked:
		log.Printf("%srun lost on %s: only untracked files left in %s, not committed: %s", labelOf(t), label, out.Worktree, out.Detail)
	case salvage.Refused:
		log.Printf("%srun lost on %s: STRANDED WORK LEFT AS IS: %s", labelOf(t), label, out.Detail)
	default:
		log.Printf("%srun lost on %s: SALVAGE FAILED, the worktree is still dirty: %s", labelOf(t), label, out.Detail)
	}
}

func shortCommit(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// lostRunClaimOf picks the claim a run-lost session held: the Result's when it carries one, else the
// last one the session was observed holding, else the claim the phase was seeded with.
func lostRunClaimOf(res, seen, seeded harness.Claim) harness.Claim {
	switch {
	case res.TaskID != "":
		return res
	case seen.TaskID != "":
		return seen
	}
	return seeded
}

// observedClaim keeps the latest claim snapshot an adapter reported. Adapters may call OnClaim from
// their own goroutines, hence the lock.
type observedClaim struct {
	mu sync.Mutex
	c  harness.Claim
}

func (o *observedClaim) set(c harness.Claim) {
	o.mu.Lock()
	o.c = c
	o.mu.Unlock()
}

func (o *observedClaim) get() harness.Claim {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.c
}
