package loop

import (
	"context"
	"log"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// openReviewGate reads the review phase's freshness baseline before anything spawns. refused=true
// means the baseline could not be read and the phase must not spawn: end then carries the
// disposition the caller returns as it is.
func (d *Driver) openReviewGate(ctx context.Context, drainNum int, t Target, ph config.Phase, phaseIdx int, phases []config.Phase, prev *harness.Result, opts phaseRunOpts, panelOptional bool, review reviewGate, end phaseEnd) (reviewGate, phaseEnd, bool) {
	review.role = reviewerRoleFor(phases, phaseIdx)
	review.baseline, review.read = d.readReviewBaseline(ctx, t, prev)
	if !review.read {
		end.empty = true
		end.emptyWhy = "the task's outcome could not be read before the review phase spawned"
		switch {
		case opts.panelParallel:
			// CLA-697 review finding 1.review-3.1: this claim is the ONE the whole panel is working under.
			log.Printf("%siteration %d: refusing to spawn the %s phase: the task could not be read before it spawned, so this phase's record cannot be judged against the task it found — the sequence ends here without the review; this member leaves the SHARED claim to the panel's join, because a release here would expire the lease its siblings are still recording under",
				labelOf(t), drainNum, ph.Label(phaseIdx))
		case panelOptional:
			// CLA-784: an OPTIONAL seat's setup failure is that seat's own result.
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

// fixGradingFor is the text a FIX phase's brief is prepended with: the driver's structural grading
// of the review record, the merged panel list, and the open MUSTs a verification round left.
func (d *Driver) fixGradingFor(ctx context.Context, t Target, prev *harness.Result, captured config.Phase, phases []config.Phase, phaseIdx int, panel panelState) string {
	// CLA-635: the FIX phase's brief carries the driver's structural grading of the review record
	// (B5).
	fixGrading := ""
	if captured.Prompt == config.FixBrief() || captured.Prompt == config.FixVerifyBrief() {
		if outcome, read := d.readTaskOutcome(ctx, t, prev,
			"to grade the review record for the fix phase",
			"the fix brief will carry the raw record without the driver's structural grading"); read {
			fixGrading = reviewFindingsDigest(outcome)
		}
		// CLA-639: when the fix follows a review PANEL, the driver's list is the merged one (B11) —
		// every member's released record for the panel's round, MUSTs open from any member, SHOULD/NIT
		// annotated with who raised them, and the members that never recorded named as incomplete.
		if roles := panelRoles(phases, phaseIdx); len(roles) > 0 && panel.round > 0 && prev != nil && prev.Claim.TaskID != "" {
			if st, read := d.readTaskState(ctx, t, prev.Claim.TaskID); read {
				if merged := mergedFindingsDigest(st.Reviews, roles, panel.round, panel.failed); merged != "" {
					fixGrading = merged
				}
			}
		}
	}
	// CLA-671: on a fix round after a verification round, the newest record holds only the verifier's
	// NEW findings, so a MUST an earlier round left open is not in the block the grading above reads;
	// the fixer used to leave it untouched and the breaker parked the task as non-convergence.
	if captured.Prompt == config.FixVerifyBrief() && prev != nil && prev.Claim.TaskID != "" {
		if st, read := d.readTaskState(ctx, t, prev.Claim.TaskID); read {
			fixGrading = openMustFixDigest(st.Reviews, st.ReviewOpenMustIDs) + fixGrading
		}
	}
	return fixGrading
}

// verifyDigestFor is the text a VERIFICATION round's brief is prepended with. refused=true means
// the task's record could not be read: the round must not spawn, and end carries the disposition
// the caller returns as it is.
func (d *Driver) verifyDigestFor(ctx context.Context, drainNum int, t Target, ph config.Phase, phaseIdx int, prev *harness.Result, isVerify bool, end phaseEnd) (string, phaseEnd, bool) {
	// CLA-638: the VERIFICATION round's brief carries the prior open MUST ids and their findings, read
	// from the plane's released records before the session spawns.
	verifyDigest := ""
	if isVerify {
		taskID := ""
		if prev != nil {
			taskID = prev.Claim.TaskID
		}
		st, read := d.readTaskStateRetry(ctx, t, taskID, "before the verification round spawned")
		if !read {
			log.Printf("%siteration %d: refusing to spawn the %s phase: the task's record could not be read, so the round would not know what to adjudicate — the sequence ends here; the claim is released when it is releasable, else left to lapse into a takeover",
				labelOf(t), drainNum, ph.Label(phaseIdx))
			end.empty = true
			end.emptyWhy = "the task's record could not be read before the verification round spawned"
			if prev != nil {
				end.claim = prev
				end.released = prev.Claim.Held()
				end.releasedToQueue = d.releaseHeldClaim(ctx, t, *prev, false)
			}
			return "", end, true
		}
		if prev != nil && prev.Claim.Branch == "" {
			// CLA-638 × CLA-670: a BRANCHLESS verification round has no plane finding ids to adjudicate
			// (the branchless review writes no durable record), so the digest hands it the LAST legacy block
			// instead.
			verifyDigest = branchlessVerificationDigest(st.Outcome)
		} else {
			verifyDigest = verificationDigest(st.Reviews, st.ReviewOpenMustIDs, st.ReviewsTotal)
		}
	}
	return verifyDigest, end, false
}
