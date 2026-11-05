package loop

import (
	"context"
	"log"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// openReviewGate reads the review phase's freshness baseline before anything
// spawns. refused=true means the baseline could not be read and the phase must
// not spawn: end then carries the disposition the caller returns as it is.
func (d *Driver) openReviewGate(ctx context.Context, drainNum int, t Target, ph config.Phase, phaseIdx int, phases []config.Phase, prev *harness.Result, opts phaseRunOpts, panelOptional bool, review reviewGate, end phaseEnd) (reviewGate, phaseEnd, bool) {
	review.role = reviewerRoleFor(phases, phaseIdx)
	review.baseline, review.read = d.readReviewBaseline(ctx, t, prev)
	if !review.read {
		end.empty = true
		end.emptyWhy = "the task's outcome could not be read before the review phase spawned"
		switch {
		case opts.panelParallel:
			// CLA-697 review finding 1.review-3.1: this claim is the ONE the
			// whole panel is working under. Releasing it here would expire a
			// lease the other members' in-flight sessions are still appending
			// their records under — their durable writes would be
			// unauthorized and the panel would lose findings it had already
			// earned. Only the JOIN may dispose of it, and it does: the
			// panel's deferred handback runs once, in drainPhases, after every
			// member has ended.
			log.Printf("%siteration %d: refusing to spawn the %s phase: the task could not be read before it spawned, so this phase's record cannot be judged against the task it found — the sequence ends here without the review; this member leaves the SHARED claim to the panel's join, because a release here would expire the lease its siblings are still recording under",
				labelOf(t), drainNum, ph.Label(phaseIdx))
		case panelOptional:
			// CLA-784: an OPTIONAL seat's setup failure is that seat's own
			// result. Handing the shared claim back here would end the run
			// its required siblings are still recording under, which is
			// exactly what a trial seat must never do.
			log.Printf("%siteration %d: refusing to spawn the OPTIONAL %s seat: the task could not be read before it spawned — recording the seat as failed and leaving the SHARED claim to the panel, because a release here would hand back the run its required siblings are still recording under",
				labelOf(t), drainNum, ph.Label(phaseIdx))
		default:
			log.Printf("%siteration %d: refusing to spawn the %s phase: the task could not be read before it spawned, so this phase's record cannot be judged against the task it found — the sequence ends here without the review; the claim is released when it is releasable, else left to lapse into a takeover",
				labelOf(t), drainNum, ph.Label(phaseIdx))
		}
		if prev != nil {
			end.claim = prev
			if !opts.panelParallel && !panelOptional {
				end.released = prev.Claim.Held()
				end.releasedToQueue = d.releaseHeldClaim(ctx, t, *prev, false)
			}
		}
		return review, end, true
	}
	return review, end, false
}
