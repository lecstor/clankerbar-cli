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

// fixGradingFor is the text a FIX phase's brief is prepended with: the driver's
// structural grading of the review record, the merged panel list, and the open
// MUSTs a verification round left. Empty for every phase that is not a fix.
func (d *Driver) fixGradingFor(ctx context.Context, t Target, prev *harness.Result, captured config.Phase, phases []config.Phase, phaseIdx int, panel panelState) string {
	// CLA-635: the FIX phase's brief carries the driver's structural grading of
	// the review record (B5). Read once per phase, before the attempt ladder —
	// the record cannot change while this phase runs except by this phase
	// itself, and every attempt must see the same grade. A read that fails
	// leaves the digest empty and the brief falls back to the raw block, which
	// the fix phase reads with get_task anyway.
	//
	// Like requireReviewRecord above, the signal is the CAPTURED phase, not
	// ph.Prompt and not the live config: a handoff respawn replaces ph.Prompt
	// with the successor's continuation wholesale, and the successor of a
	// mid-fix handoff needs the same graded list its predecessor got — its
	// continuation (fixTerminalRecord) already holds it to the grade, so the
	// grade itself must be the one thing the respawn does not drop. The resolved
	// prompt is the builtin fix brief exactly when the operator left the phase's
	// own Prompt empty; a custom fix prompt owns its wording and gets no digest,
	// the same convention the variant swap in invocationFor follows.
	fixGrading := ""
	if captured.Prompt == config.FixBrief() || captured.Prompt == config.FixVerifyBrief() {
		if outcome, read := d.readTaskOutcome(ctx, t, prev,
			"to grade the review record for the fix phase",
			"the fix brief will carry the raw record without the driver's structural grading"); read {
			fixGrading = reviewFindingsDigest(outcome)
		}
		// CLA-639: when the fix follows a review PANEL, the driver's list is the
		// merged one (B11) — every member's released record for the panel's round,
		// MUSTs open from any member, SHOULD/NIT annotated with who raised them,
		// and the members that never recorded named as incomplete. It REPLACES the
		// legacy block's grading: the panel writes no block, so the legacy digest
		// above has nothing to grade, and the fixer must not be handed one
		// reviewer's list as though it were the panel's. The read is the same
		// get_task; a failed read leaves the legacy digest above in place, exactly
		// as before the panel existed.
		//
		// panel.round is the round the panel's records landed in (CLA-639 S1), and
		// it is the ONLY round the merged list may read: with round 0 the digest
		// would relist an earlier cycle's findings after a verify rewind and count
		// a member that recorded in an earlier round as having recorded this one.
		if roles := panelRoles(phases, phaseIdx); len(roles) > 0 && panel.round > 0 && prev != nil && prev.Claim.TaskID != "" {
			if st, read := d.readTaskState(ctx, t, prev.Claim.TaskID); read {
				if merged := mergedFindingsDigest(st.Reviews, roles, panel.round, panel.failed); merged != "" {
					fixGrading = merged
				}
			}
		}
	}
	// CLA-671: on a fix round after a verification round, the newest record
	// holds only the verifier's NEW findings, so a MUST an earlier round left
	// open is not in the block the grading above reads; the fixer used to leave
	// it untouched and the breaker parked the task as non-convergence. Prepend
	// the plane's own open list. Best-effort: an unreadable task state leaves
	// the brief's instruction to read reviews.openMustIds itself.
	if captured.Prompt == config.FixVerifyBrief() && prev != nil && prev.Claim.TaskID != "" {
		if st, read := d.readTaskState(ctx, t, prev.Claim.TaskID); read {
			fixGrading = openMustFixDigest(st.Reviews, st.ReviewOpenMustIDs) + fixGrading
		}
	}
	return fixGrading
}
