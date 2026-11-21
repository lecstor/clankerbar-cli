package loop

import (
	"context"
	"fmt"
	"log"

	"github.com/lecstor/clankerbar-cli/internal/fleet"
)

// judgeVerifyRound is the verification seam.
func (r *phaseRun) judgeVerifyRound(ctx context.Context, at *phaseAttempt) {
	d := r.d
	t := r.t
	// The verification seam runs even on the LAST phase.
	r.end.verifyChecked = true
	r.end.branch, at.evidenceTip, at.evidenceOK, at.evidenceUnverified, at.evidenceWhy = d.checkpointEvidence(ctx, t, at.res, at.inv.WorkDir, &r.review)
	if !at.evidenceOK {
		at.evidenceWhy = r.withReportProblem(at.evidenceWhy)
	}
	if r.review.state == "sealed" {
		r.end.reviewRound = r.review.round
	}
	if at.evidenceOK && r.review.required && r.review.round > 0 {
		if r.review.state == "sealed" {
			// The verifier did not release its own round; the driver does, exactly as for the review gate.
			if cerr := d.closeReviewRound(ctx, t, at.res, r.review.round); cerr != nil {
				at.evidenceOK = false
				at.evidenceWhy = fmt.Sprintf("verification round %d could not be closed (%v) — its findings would stay sealed and the next fix would have nothing to apply", r.review.round, cerr)
			}
		}
		if at.evidenceOK {
			r.end.reviewRound = 0
			r.end.verifyRound = r.review.round
			// Fail-closed, so retried like the spawn read: one blip here ends a sequence the verification
			// round has already been paid for.
			if st, read := d.readTaskStateRetry(ctx, t, at.res.Claim.TaskID, "after the verification round closed"); !read {
				at.evidenceOK = false
				at.evidenceWhy = "the task could not be read after the verification round closed, so its open MUSTs cannot be judged"
			} else {
				r.end.verifyOpenMusts = append([]string(nil), st.ReviewOpenMustIDs...)
				r.end.verifyOpenDetails = openMustDetails(st.ReviewOpenMustIDs, st.Reviews)
				r.end.verifyCounted = st.ReviewCounted
				r.end.verifyResolvedIDs = resolvedFindingIDs(st.Reviews)
				r.end.verifyStatus = st.Status
				r.handOverVerifiedRound(ctx, at, st)
			}
		}
	} else if at.evidenceOK && r.review.required {
		// CLA-638 × CLA-670: the checkpoint rested on the legacy `## Review` block (the branchless arm
		// of the gate), so there is no durable round to release and no plane open-MUST set to judge —
		// the block is this shape's record.
		r.end.verifyRound = 0
		if st, read := d.readTaskStateRetry(ctx, t, at.res.Claim.TaskID, "after the branchless verification round"); !read {
			at.evidenceOK = false
			at.evidenceWhy = "the task could not be read after the branchless verification round, so its hand-over cannot be judged"
		} else {
			r.end.verifyStatus = st.Status
		}
	}
	if !at.evidenceOK {
		r.end.verifyFailure = at.evidenceWhy
	}
	// Fold the verified branch/tip onto the claim exactly as the generic checkpoint path below does,
	// so a fix spawned after an open-MUST round resumes with the verified hand-off.
	if r.end.claim != nil && at.evidenceOK {
		if r.end.branch != "" {
			r.end.claim.Claim.Branch = r.end.branch
		} else {
			r.end.claim.Claim.Branch = ""
		}
		r.end.claim.CheckpointUnverified = at.evidenceUnverified
		r.end.claim.CheckpointTip = at.evidenceTip
	}
}

// judgeReviewRecord is the record gate for a checkpoint contender.
func (r *phaseRun) judgeReviewRecord(ctx context.Context, at *phaseAttempt) {
	d := r.d
	t := r.t
	panel := r.panel
	opts := r.opts
	nextReviewShaped := r.shape.nextReviewShaped
	panelOptional := r.shape.panelOptional
	r.end.branch, at.evidenceTip, at.evidenceOK, at.evidenceUnverified, at.evidenceWhy = d.checkpointEvidence(ctx, t, at.res, at.inv.WorkDir, &r.review)
	if !at.evidenceOK {
		at.evidenceWhy = r.withReportProblem(at.evidenceWhy)
	}
	// The matched round rides the end: drainPhases closes it if the handoff guards refuse the respawn
	// this detected handoff asked for — the one advance path drainPhase cannot close on itself.
	if r.review.state == "sealed" {
		r.end.reviewRound = r.review.round
	}
	// CLA-637: the review phase's durable record is SEALED — the plane withholds its findings from
	// every reader until the driver closes the round.
	optionalOnly := panelOptional && !panel.recorded
	if at.evidenceOK && r.review.required && r.review.round > 0 && r.review.state == "sealed" && at.handoff == "" && !nextReviewShaped && !opts.panelParallel && !optionalOnly {
		if cerr := d.closeReviewRound(ctx, t, at.res, r.review.round); cerr != nil {
			at.evidenceOK = false
			r.end.reviewCloseFailed = true
			at.evidenceWhy = fmt.Sprintf("review round %d could not be closed (%v) — refusing the checkpoint, because the fix phase would be spawned against findings the plane still has sealed", r.review.round, cerr)
		}
	}
}

// classifyEmpty turns a refused evidence gate into the held-but-empty shape, and recognises the
// no-MCP infrastructure miss (retrying it once).
func (r *phaseRun) classifyEmpty(at *phaseAttempt) {
	d := r.d
	tc := r.tc
	phaseIdx := r.phaseIdx
	ph := r.ph
	a := r.a
	reviewOnly := r.shape.reviewOnly
	// A held-but-empty exit (2026-08-24): a would-be checkpoint refused by the evidence gate — not
	// by any of the older vetoes (dead, untrusted, not checkpointable) — so this is a FAILED phase,
	// not a completed one.
	at.empty = at.reviewable && !at.evidenceOK
	r.end.empty = at.empty
	r.end.emptyWhy = at.evidenceWhy
	// CLA-639: whether this pass left a review RECORD behind, for the panel's member handler.
	r.end.reviewRecorded = at.reviewable && r.review.required && at.evidenceOK
	// CLA-703: did this review-only session ever reach the backlog?
	at.noMCPNow = r.kind == reportNone && reviewOnly && !r.end.reviewRecorded && !r.end.reviewCloseFailed && at.res.ClankerbarCalls == 0 &&
		!at.res.Claim.Settled && !at.emittedHandoff &&
		at.res.Untrusted == "" && at.res.ExitCode == 0 && !at.capped && !at.ceiling && !at.wallclock &&
		!at.producedNothing && a.Capabilities().TracksClaims
	at.retryNoMCP = at.noMCPNow && !r.mcpRetried
	if at.noMCPNow && r.mcpRetried {
		// The retry ended the same way: this is the final classification.
		r.end.noMCP = true
		// The generic dead booking (if the retry happened to look like a death) and the held-but-empty
		// booking below both belong to dead/ empty, not to an infrastructure miss.
		if at.bookedDead {
			d.unbookDead(ph.Label(phaseIdx), tc.HarnessFor(ph))
		}
		r.end.deadBooked = true
		if r.end.session != nil {
			r.end.session.Cause = fleet.CauseNoMCP
		}
	}
}

// holdCheckpoint keeps the lease for the successor: it folds the verified branch and tip onto the
// carried claim and says why the seam advances.
func (r *phaseRun) holdCheckpoint(ctx context.Context, at *phaseAttempt) {
	t := r.t
	drainNum := r.drainNum
	prev := r.prev
	r.end.checkpoint = true
	r.end.handoff = at.handoff
	if r.end.claim != nil {
		// The invariant "the carried claim's branch is the VERIFIED one" is held by construction,
		// whatever form the evidence took: the branch form folds the verified branch on, and the
		// plane-record forms clear whatever the session recorded - a recorded branch that FAILED origin
		// verification must not ride into the successor's brief, which names {{branch}} as verified on
		// the origin remote, and the no-code brief is selected exactly on an empty claim branch.
		if r.end.branch != "" {
			r.end.claim.Claim.Branch = r.end.branch
		} else {
			r.end.claim.Claim.Branch = ""
		}
		r.end.claim.CheckpointUnverified = at.evidenceUnverified
		// CLA-637: the origin tip the verified branch check read.
		r.end.claim.CheckpointTip = at.evidenceTip
		// CLA-685: a HANDOFF is the one checkpoint this session's own exit did not earn — it holds only
		// because the successor continues THIS phase, which the claim carried in as `prev` opened.
		if at.handoff != "" && !at.evidenceOK && prev != nil && r.end.claim.CheckpointTip == "" {
			r.end.claim.CheckpointUnverified = prev.CheckpointUnverified
			if r.end.claim.Claim.Branch == prev.Claim.Branch {
				r.end.claim.CheckpointTip = prev.CheckpointTip
			}
		}
	}
	if at.handoff != "" {
		log.Printf("%siteration %d: the session ended its final message with a handoff block, still holding %s — keeping the lease for its successor",
			labelOf(t), drainNum, at.res.Claim.TaskID)
	} else if r.end.branch != "" && at.evidenceUnverified {
		// The cause is the CHECK not completing, named by evidenceWhy (the Unknown check's own detail)
		// — never "the origin remote could not be read" asserted as fact: delivery.Unknown also covers
		// a check that never reached the remote (a branch no local repository has, an ambiguous repo, no
		// git on PATH), and the log must not contradict the detail it prints.
		log.Printf("%siteration %d: phase reached an UNVERIFIED checkpoint holding %s (branch %s recorded, but it could not be verified on the origin remote: %s) — keeping the lease for the next phase; the review phase must verify the branch itself",
			labelOf(t), drainNum, at.res.Claim.TaskID, r.end.branch, at.evidenceWhy)
	} else if r.end.branch != "" {
		log.Printf("%siteration %d: phase reached its checkpoint holding %s (branch %s verified on the origin remote) — keeping the lease for the next phase",
			labelOf(t), drainNum, at.res.Claim.TaskID, r.end.branch)
	} else {
		// The plane-record forms of exit evidence: a no-code or already-settled task checkpoints with no
		// branch to name.
		log.Printf("%siteration %d: phase reached its checkpoint holding %s (exit evidenced by the plane's record — task left ready or declared a no-code delivery; no branch) — keeping the lease for the next phase",
			labelOf(t), drainNum, at.res.Claim.TaskID)
	}
}

// settleWithoutCheckpoint is the exit that is not a checkpoint: it hands the claim back and books
// the empty phase.
func (r *phaseRun) settleWithoutCheckpoint(ctx context.Context, at *phaseAttempt) {
	d := r.d
	t := r.t
	tc := r.tc
	drainNum := r.drainNum
	phaseIdx := r.phaseIdx
	ph := r.ph
	last := r.last
	panelMember := r.shape.panelMember
	// `released` records only a handback that could actually have happened: a zero Result from a
	// failed launch releases nothing, and reporting it as released would erase a predecessor's claim
	// that is still live.
	resumeOnly := at.res.ReportResumeFailed != "" && at.res.Claim.Held()
	// CLA-703: a retrying no-MCP attempt is not an ending — keep the claim (the retry resumes the
	// same run) and book nothing yet.
	if !panelMember && !at.retryNoMCP && !resumeOnly {
		r.end.released = at.res.Claim.Held()
	}
	r.end.reportResumeFailed = at.res.ReportResumeFailed
	r.end.dead = at.dead
	if r.end.empty && !r.end.noMCP && !at.retryNoMCP {
		log.Printf("%siteration %d: the %s phase ended holding %s with no recorded branch on the origin remote — not a checkpoint (%s), so no review phase spawns",
			labelOf(t), drainNum, ph.Label(phaseIdx), claimLabel(at.res.Claim), at.evidenceWhy)
		// Book it into the dead-phase tally: this is the "produced nothing" class the rate exists to
		// measure, and a fleet-wide silent EOT is exactly the incident in question.
		if !at.bookedDead && !(panelMember && r.end.reviewCloseFailed) {
			d.tallyEmpty(ph.Label(phaseIdx), tc.HarnessFor(ph))
			// CLA-639 M4: this exit is booked, so the panel's member handler must not book it again when it
			// records the member as failed.
			r.end.deadBooked = true
		}
	}
	// last && a clean exit is the shape CLA-384 is about: the phase ran to completion, pushed, and
	// simply did not declare.
	if !panelMember && !at.retryNoMCP && !resumeOnly {
		// A tool-loop kill is never the undeclared hand-off CLA-384 counts, even when the process exited
		// 0 an instant before the kill landed: it was killed failing to move the task on.
		r.end.releasedToQueue = d.releaseHeldClaim(ctx, t, at.res, last && at.res.ExitCode == 0 && at.toolLoop == "")
	}
}
