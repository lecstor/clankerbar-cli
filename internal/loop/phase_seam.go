package loop

import (
	"context"
	"fmt"
	"log"

	"github.com/lecstor/clankerbar-cli/internal/fleet"
)

// The ladder stages that decide what the attempt's exit means for the claim: the
// verification and review record gates, the checkpoint, and the handback.

// judgeVerifyRound is the verification seam. It runs even on the LAST phase: its
// evidence is the durable record and the open-MUST set, not a checkpoint.
func (r *phaseRun) judgeVerifyRound(ctx context.Context, at *phaseAttempt) {
	d := r.d
	t := r.t
	// The verification seam runs even on the LAST phase. Unlike every
	// other terminal phase its evidence is not a checkpoint (nothing
	// follows to resume) but the durable record plus the open-MUST set:
	// that set is what decides whether ANOTHER fix is spawned, whether
	// the sequence is done, or whether the round cap parks the task
	// (CLA-638, B12).
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
			// The verifier did not release its own round; the driver does,
			// exactly as for the review gate.
			if cerr := d.closeReviewRound(ctx, t, at.res, r.review.round); cerr != nil {
				at.evidenceOK = false
				at.evidenceWhy = fmt.Sprintf("verification round %d could not be closed (%v) — its findings would stay sealed and the next fix would have nothing to apply", r.review.round, cerr)
			}
		}
		if at.evidenceOK {
			// Closed (or already released by the verifier's terminal call);
			// nothing downstream may try to close it again.
			r.end.reviewRound = 0
			r.end.verifyRound = r.review.round
			// Fail-closed, so retried like the spawn read (CLA-686 D3):
			// one blip here ends a sequence the verification round has
			// already been paid for.
			if st, read := d.readTaskStateRetry(ctx, t, at.res.Claim.TaskID, "after the verification round closed"); !read {
				at.evidenceOK = false
				at.evidenceWhy = "the task could not be read after the verification round closed, so its open MUSTs cannot be judged"
			} else {
				r.end.verifyOpenMusts = append([]string(nil), st.ReviewOpenMustIDs...)
				r.end.verifyOpenDetails = openMustDetails(st.ReviewOpenMustIDs, st.Reviews)
				r.end.verifyCounted = st.ReviewCounted
				r.end.verifyResolvedIDs = resolvedFindingIDs(st.Reviews)
				r.end.verifyStatus = st.Status
				// A runner-claimed verifier writes no record and makes no
				// hand-over: with the round closed clean and the task still
				// in_progress, the runner is the one that moves it to review.
				r.handOverVerifiedRound(ctx, at, st)
			}
		}
	} else if at.evidenceOK && r.review.required {
		// CLA-638 × CLA-670: the checkpoint rested on the legacy `## Review`
		// block (the branchless arm of the gate), so there is no durable
		// round to release and no plane open-MUST set to judge — the block
		// is this shape's record. Read the task back anyway, for the status
		// the hand-over log needs: a branchless verifier that recorded its
		// pass but left the task in_progress must be named loudly, exactly
		// as the id-based shape names it. A failed read ends the sequence
		// fail-closed, like every other unreadable seam — so it retries
		// like them too (CLA-686 D3).
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
	// Fold the verified branch/tip onto the claim exactly as the generic
	// checkpoint path below does, so a fix spawned after an open-MUST
	// round resumes with the verified hand-off.
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

// judgeReviewRecord is the record gate for a checkpoint contender. It leaves the
// matched review round sealed, closing it only when the review stage is over.
func (r *phaseRun) judgeReviewRecord(ctx context.Context, at *phaseAttempt) {
	d := r.d
	t := r.t
	panel := r.panel
	opts := r.opts
	nextReviewShaped := r.shape.nextReviewShaped
	panelOptional := r.shape.panelOptional
	r.end.branch, at.evidenceTip, at.evidenceOK, at.evidenceUnverified, at.evidenceWhy = d.checkpointEvidence(ctx, t, at.res, at.inv.WorkDir, &r.review)
	if !at.evidenceOK {
		// The reason the record did not match, plus what the session's own
		// report did — the two together are what the operator acts on.
		at.evidenceWhy = r.withReportProblem(at.evidenceWhy)
	}
	// The matched round rides the end (CLA-637): drainPhases closes it if
	// the handoff guards refuse the respawn this detected handoff asked
	// for — the one advance path drainPhase cannot close on itself. Only a
	// still-sealed round is ours to close: a verifier closes its own in the
	// terminal call (CLA-638).
	if r.review.state == "sealed" {
		r.end.reviewRound = r.review.round
	}
	// CLA-637: the review phase's durable record is SEALED — the plane
	// withholds its findings from every reader until the driver closes
	// the round. The close is the driver's, made when the review stage
	// is OVER: not while the session hands off to a successor reviewer
	// (the successor continues this round), and not while the next
	// configured phase is another review-shaped one (a panel member,
	// whose independence depends on not reading the earlier member's
	// findings). A close that fails REFUSES the checkpoint, fail-closed:
	// the alternative is a fix phase spawned against findings the plane
	// still has sealed, which can only park the task.
	//
	// CLA-639: for a panel, the stage is the whole panel. Every member
	// but the last leaves the round sealed here; the last member's close
	// is what releases them all to the fix phase, and it happens in the
	// SAME condition below because nextReviewShaped is false only for
	// the last member (the panel's last phase, when no fix follows).
	// CLA-784: an OPTIONAL seat's record alone must not release the
	// round. If no required member has recorded, this trial seat's
	// record is kept sealed — the panel is treated as one where nobody
	// recorded, so no fix runs against a round that is no review.
	optionalOnly := panelOptional && !panel.recorded
	if at.evidenceOK && r.review.required && r.review.round > 0 && r.review.state == "sealed" && at.handoff == "" && !nextReviewShaped && !opts.panelParallel && !optionalOnly {
		if cerr := d.closeReviewRound(ctx, t, at.res, r.review.round); cerr != nil {
			at.evidenceOK = false
			r.end.reviewCloseFailed = true
			at.evidenceWhy = fmt.Sprintf("review round %d could not be closed (%v) — refusing the checkpoint, because the fix phase would be spawned against findings the plane still has sealed", r.review.round, cerr)
		}
	}
}

// classifyEmpty turns a refused evidence gate into the held-but-empty shape, and
// recognises the no-MCP infrastructure miss (retrying it once).
func (r *phaseRun) classifyEmpty(at *phaseAttempt) {
	d := r.d
	tc := r.tc
	phaseIdx := r.phaseIdx
	ph := r.ph
	a := r.a
	reviewOnly := r.shape.reviewOnly
	// A held-but-empty exit (2026-08-24): a would-be checkpoint refused by the
	// evidence gate — not by any of the older vetoes (dead, untrusted, not
	// checkpointable) — so this is a FAILED phase, not a completed one. It is
	// carried on the result in BOTH branches on purpose: for a bare exit it is
	// the classification of record, and for a handoff it is what lets
	// drainPhases downgrade the exit when the handoff is refused. An honored
	// handoff never reaches that downgrade — the phase continues first.
	//
	// CLA-639: a panel member is not special-cased here. A member whose
	// review record did not pass the gate is a FAILED member, and the panel
	// handler in drainPhases is what records it as failed and keeps the round
	// going; the flag is shared with the shipped single-review path, which
	// still refuses to advance on it.
	at.empty = at.reviewable && !at.evidenceOK
	r.end.empty = at.empty
	r.end.emptyWhy = at.evidenceWhy
	// CLA-639: whether this pass left a review RECORD behind, for the panel's
	// member handler. It is the seam gate's verdict, not the round number: a
	// member whose envelope was sealed and closed still recorded.
	r.end.reviewRecorded = at.reviewable && r.review.required && at.evidenceOK
	// CLA-703: did this review-only session ever reach the backlog? A clean
	// end with no successful clankerbar call and no review record is the
	// missing-MCP shape — the tools the brief tells it to use were not
	// offered, so "recorded nothing" is not a reviewer's verdict.
	//
	// The dead signature is EXCLUDED: a silent death is already classified
	// as dead and retried by the dead-phase ladder, and relabelling it no_mcp
	// here would hide a real death behind an infrastructure excuse. A
	// non-zero exit is excluded for the same reason — the transient/
	// non-retryable classification owns it. Caps and untrusted streams are
	// excluded because neither can be read as "the tools were missing".
	//
	// A runner-claimed phase is excluded too: its session is TOLD to write
	// nothing, so no successful clankerbar call is its correct shape. Its
	// missing record is a failed report, never a missing MCP server.
	at.noMCPNow = r.kind == reportNone && reviewOnly && !r.end.reviewRecorded && !r.end.reviewCloseFailed && at.res.ClankerbarCalls == 0 &&
		!at.res.Claim.Settled && !at.emittedHandoff &&
		at.res.Untrusted == "" && at.res.ExitCode == 0 && !at.capped && !at.ceiling && !at.wallclock &&
		!at.producedNothing && a.Capabilities().TracksClaims
	at.retryNoMCP = at.noMCPNow && !r.mcpRetried
	if at.noMCPNow && r.mcpRetried {
		// The retry ended the same way: this is the final classification.
		r.end.noMCP = true
		// The generic dead booking (if the retry happened to look like a
		// death) and the held-but-empty booking below both belong to dead/
		// empty, not to an infrastructure miss. Unbook the first and stop the
		// second from firing; the handler books the no-MCP cell instead.
		if at.bookedDead {
			d.unbookDead(ph.Label(phaseIdx), tc.HarnessFor(ph))
		}
		r.end.deadBooked = true
		if r.end.session != nil {
			r.end.session.Cause = fleet.CauseNoMCP
		}
	}
}

// holdCheckpoint keeps the lease for the successor: it folds the verified branch
// and tip onto the carried claim and says why the seam advances.
func (r *phaseRun) holdCheckpoint(ctx context.Context, at *phaseAttempt) {
	t := r.t
	drainNum := r.drainNum
	prev := r.prev
	r.end.checkpoint = true
	r.end.handoff = at.handoff
	if r.end.claim != nil {
		// The invariant "the carried claim's branch is the VERIFIED one"
		// is held by construction, whatever form the evidence took
		// (CLA-497): the branch form folds the verified branch on, and
		// the plane-record forms clear whatever the session recorded -
		// a recorded branch that FAILED origin verification must not
		// ride into the successor's brief, which names {{branch}} as
		// verified on the origin remote (CLA-457), and the no-code
		// brief is selected exactly on an empty claim branch.
		//
		// CLA-576: an UNVERIFIED checkpoint is the one exception, and it
		// is marked rather than hidden. Its branch rides the claim (the
		// review phase needs the name to verify it) with
		// CheckpointUnverified set, so the successor's brief is the
		// unverified variant - never the verified wording, which this
		// checkpoint cannot support.
		if r.end.branch != "" {
			r.end.claim.Claim.Branch = r.end.branch
		} else {
			r.end.claim.Claim.Branch = ""
		}
		r.end.claim.CheckpointUnverified = at.evidenceUnverified
		// CLA-637: the origin tip the verified branch check read. The
		// review brief hands it to the reviewer as the revision to
		// check out and record, and the seam matches the record's
		// reviewedTip against it. Empty exactly when no tip could be
		// read (branchless or unverified), which is when no tip can be
		// demanded of the reviewer either.
		r.end.claim.CheckpointTip = at.evidenceTip
		// CLA-685: a HANDOFF is the one checkpoint this session's own
		// exit did not earn — it holds only because the successor
		// continues THIS phase, which the claim carried in as `prev`
		// opened. So when the exit evidenced NOTHING (evidenceOK false)
		// the verdict just folded above is its REFUSAL's, not a
		// checkpoint's: the record gate zeroes the unverified mark even
		// though the branch leg had established it. The successor then
		// reads carried.CheckpointUnverified as false, is handed the
		// VERIFIED continuation - "The driver verified the origin
		// remote's tip ... as {{reviewedTip}}" - for a tip this driver
		// never read, and the placeholder stays literal, since it is
		// filled only when the tip is non-empty.
		//
		// The exit's verdict about the checkpoint is empty exactly when
		// it read no tip of its own: an evidenced branch leg always
		// brings a tip (the verified form), and a non-empty tip implies
		// unverified=false — the verified continuation, whose
		// {{reviewedTip}} that tip then fills. So the opening
		// checkpoint's mark is the only true one there, and it rides
		// forward, as the panel-member block in drainPhases rides a
		// claim's resumption facts onto the claim it hands on.
		//
		// The tip is carried only for the branch it was read for — it
		// is `prev`'s, and a claim that now names another one must not
		// be handed a tip that never belonged to it. The BRANCH stays as
		// folded: it is the live truth about where the work is. An exit
		// that WAS evidenced keeps its own verdict untouched, so the
		// verified path is unaffected.
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
		// The cause is the CHECK not completing, named by evidenceWhy
		// (the Unknown check's own detail) — never "the origin remote
		// could not be read" asserted as fact: delivery.Unknown also
		// covers a check that never reached the remote (a branch no
		// local repository has, an ambiguous repo, no git on PATH), and
		// the log must not contradict the detail it prints.
		log.Printf("%siteration %d: phase reached an UNVERIFIED checkpoint holding %s (branch %s recorded, but it could not be verified on the origin remote: %s) — keeping the lease for the next phase; the review phase must verify the branch itself",
			labelOf(t), drainNum, at.res.Claim.TaskID, r.end.branch, at.evidenceWhy)
	} else if r.end.branch != "" {
		log.Printf("%siteration %d: phase reached its checkpoint holding %s (branch %s verified on the origin remote) — keeping the lease for the next phase",
			labelOf(t), drainNum, at.res.Claim.TaskID, r.end.branch)
	} else {
		// The plane-record forms of exit evidence (CLA-497): a no-code or
		// already-settled task checkpoints with no branch to name.
		log.Printf("%siteration %d: phase reached its checkpoint holding %s (exit evidenced by the plane's record — task left ready or declared a no-code delivery; no branch) — keeping the lease for the next phase",
			labelOf(t), drainNum, at.res.Claim.TaskID)
	}
}

// settleWithoutCheckpoint is the exit that is not a checkpoint: it hands the claim
// back, books the empty phase, and asks the plane whether a claim the stream
// never showed is held.
func (r *phaseRun) settleWithoutCheckpoint(ctx context.Context, at *phaseAttempt) {
	d := r.d
	t := r.t
	tc := r.tc
	drainNum := r.drainNum
	phaseIdx := r.phaseIdx
	ph := r.ph
	last := r.last
	panelMember := r.shape.panelMember
	// `released` records only a handback that could actually have happened:
	// a zero Result from a failed launch releases nothing, and reporting it
	// as released would erase a predecessor's claim that is still live.
	//
	// CLA-639 M4: a PANEL MEMBER that ended without a record does NOT hand
	// its claim back here. Everything the panel handler must do about a
	// failed member needs the claim to still be live: closing the round (only
	// the LIVE holder may, and a failed close would leave the earlier
	// members' findings sealed behind a lease that is already gone) and
	// handing the next member — or the fix phase — the same run to resume.
	// A member that genuinely lost the claim is not special-cased: Held() is
	// false, so there is nothing to release either way. The handler owns the
	// handback for this shape, and takes it AFTER the close (see
	// drainPhases).
	// CLA-877: a report-resume failure is the one dead end whose claim is KEPT.
	// The session's own run succeeded — its commits are pushed — and only the
	// reply that re-emits the done report went silent, so the phase retry
	// resumes this very run. Handing the claim back here would expire the lease
	// on pushed work and cost a reclaim before the same phase could run again.
	resumeOnly := at.res.ReportResumeFailed != "" && at.res.Claim.Held()
	// CLA-703: a retrying no-MCP attempt is not an ending — keep the claim
	// (the retry resumes the same run) and book nothing yet. The final
	// attempt goes through the normal handback.
	if !panelMember && !at.retryNoMCP && !resumeOnly {
		r.end.released = at.res.Claim.Held()
	}
	r.end.reportResumeFailed = at.res.ReportResumeFailed
	r.end.dead = at.dead
	if r.end.empty && !r.end.noMCP && !at.retryNoMCP {
		log.Printf("%siteration %d: the %s phase ended holding %s with no recorded branch on the origin remote — not a checkpoint (%s), so no review phase spawns",
			labelOf(t), drainNum, ph.Label(phaseIdx), claimLabel(at.res.Claim), at.evidenceWhy)
		// Book it into the dead-phase tally (CLA-457): this is the
		// "produced nothing" class the rate exists to measure, and a
		// fleet-wide silent EOT is exactly the incident in question.
		//
		// Unless the session's exit was already booked as the CLA-402
		// SILENT death: a LAST phase can wear both shapes at once (its
		// finish reason is "unknown" AND its evidence gate failed), and
		// one session may not read as two dead ones out of one run —
		// CLA-639 M4 needs this booking to be exactly one per failed
		// panel member, and the operator's rate needs it to be honest.
		//
		// And unless this is a panel member whose review close failed
		// (CLA-639 fix pass 3): a member that WROTE its record and whose
		// round the plane then refused to release produced a review — the
		// empty shape here would report it as a session that died
		// producing nothing, which is the opposite of what happened. The
		// skip is PANEL-ONLY: the plain single review keeps the untaxed
		// CLA-457 booking it has had since the base commit, so an
		// operator's dead-phase rate reads the same with no panel
		// (CLA-639 fix pass 4).
		if !at.bookedDead && !(panelMember && r.end.reviewCloseFailed) {
			d.tallyEmpty(ph.Label(phaseIdx), tc.HarnessFor(ph))
			// CLA-639 M4: this exit is booked, so the panel's member
			// handler must not book it again when it records the member
			// as failed.
			r.end.deadBooked = true
		}
	}
	// last && a clean exit is the shape CLA-384 is about: the phase ran to
	// completion, pushed, and simply did not declare. A non-zero exit reached
	// the same place by crashing, which is a different failure with its own
	// reporting, and an earlier phase reaching here did not have declaring as
	// its job at all.
	//
	// The return tells whether the claim is actually back in the ready queue
	// — the thing a bounded retry can re-claim. A release the plane refused
	// (or that never ran) leaves the lease live, so a retry session would
	// only be refused and burned.
	if !panelMember && !at.retryNoMCP && !resumeOnly {
		// A tool-loop kill is never the undeclared hand-off CLA-384
		// counts, even when the process exited 0 an instant before the
		// kill landed: it was killed failing to move the task on.
		r.end.releasedToQueue = d.releaseHeldClaim(ctx, t, at.res, last && at.res.ExitCode == 0 && at.toolLoop == "")
	}

	r.recoverUnobservedClaim(ctx, at)
}

// recoverUnobservedClaim asks the plane about the one task this driver briefed
// when the stream showed no claim at all, and treats a held task with a recorded
// branch as the phase's checkpoint if the branch verifies on the origin remote.
func (r *phaseRun) recoverUnobservedClaim(ctx context.Context, at *phaseAttempt) {
	d := r.d
	t := r.t
	drainNum := r.drainNum
	phaseIdx := r.phaseIdx
	ph := r.ph
	last := r.last
	panel := r.panel
	opts := r.opts
	briefedTask := r.briefedTask
	spawnHold := r.briefedHold
	nextReviewShaped := r.shape.nextReviewShaped
	panelOptional := r.shape.panelOptional
	// CLA-451: the blind-spot convergence. Everything above learns about
	// claims by parsing clankerbar tool calls out of the stream; a session
	// that claimed by another channel (a raw API call with the same key,
	// a harness quirk, a renamed tool) ends checkpointable with NO observed
	// claim — and used to end the sequence here, stranding its pushed work
	// behind an expiring lease until a takeover recovered it at a reclaim's
	// cost. Before declaring the phase empty, ask the plane about the one
	// task this driver can name: the task it BRIEFED. If that task is held,
	// with a branch recorded, by some run of this project's key, treat it
	// as the phase's checkpoint exactly as an observed claim would be.
	//
	// Every conjunct below keeps an existing guard absolute:
	//
	//   - end.claim == nil && !res.Claim.Held() — an OBSERVED claim,
	//     settled or not, stays the authoritative view; the peek never
	//     overwrites or second-guesses one.
	//   - res.Untrusted == "" — a stream that could not be read whole is
	//     never read for claim state, and the plane is not asked to fill
	//     the hole either: the settle we never saw may be in the missing
	//     bytes (CLA-262).
	//   - !producedNothing — the dead-phase veto (CLA-386): a final step
	//     that finished "unknown" having produced nothing is not a
	//     checkpoint no matter what the plane says, so it is not even
	//     consulted.
	//   - the orderly-end disjunct mirrors `checkpointable` exactly, with
	//     the plane's recorded branch standing in for the HasWIP the cap
	//     path needs — a branch the PLANE confirms is stronger evidence
	//     than one read off a stream.
	//   - !last — a last phase has no successor to hold the lease for;
	//     a handoff cannot widen this either, because detectHandoff
	//     refuses on no observed claim before we get here.
	//
	// A claim the peek recovers then faces the SAME evidence gate as an
	// observed one: the recorded branch must really be on the origin
	// remote before the seam advances (CLA-457), because a plane record
	// alone cannot tell a pushed branch from a phantom one. Evidence that
	// fails keeps today's behavior — the drain ends, and the live lease
	// expires with its recorded branch intact as the takeover hand-off.
	// The peek itself is a READ of the briefed task (get_task), never a
	// claim and never a heartbeat: it cannot race a session out of work,
	// and a peek that fails or comes back not-held simply falls through
	// to the standard ending below.
	if r.end.claim == nil && !last && at.res.Untrusted == "" && !at.producedNothing &&
		!at.res.Claim.Held() && (at.res.ExitCode == 0 || at.capped || at.ceiling || at.wallclock || at.recoveredExit) {
		if claim := d.peekCheckpointClaim(ctx, t, drainNum, ph.Label(phaseIdx), briefedTask, spawnHold); claim != nil {
			r.end.claim = claim
			var peekTip string
			r.end.branch, peekTip, at.evidenceOK, at.evidenceUnverified, at.evidenceWhy = d.checkpointEvidence(ctx, t, *claim, at.inv.WorkDir, &r.review)
			r.end.reviewRound = r.review.round
			// The review round's close rides this path too (CLA-637): a
			// recovered review checkpoint whose round stayed sealed would
			// spawn the fix phase against findings it cannot read. No
			// handoff is possible here (detectHandoff refuses an
			// unobserved claim), so the only skip is a following
			// review-shaped phase.
			//
			// CLA-784: the same "an optional seat's record alone must not
			// release the round" rule as the observed-claim close above.
			// A trial seat that records here and is then recovered through
			// the plane's peek must leave the round sealed unless a
			// REQUIRED member has recorded; otherwise this close releases
			// a round that is no review at all, before the panel handler's
			// own "no required member recorded" check can see it.
			optionalOnly := panelOptional && !panel.recorded
			if at.evidenceOK && r.review.required && r.review.round > 0 && r.review.state == "sealed" && !nextReviewShaped && !opts.panelParallel && !optionalOnly {
				if cerr := d.closeReviewRound(ctx, t, *claim, r.review.round); cerr != nil {
					at.evidenceOK = false
					at.evidenceWhy = fmt.Sprintf("review round %d could not be closed (%v) — refusing the checkpoint, because the fix phase would be spawned against findings the plane still has sealed", r.review.round, cerr)
				}
			}
			if at.evidenceOK {
				// Fold the VERIFIED branch onto the carried claim, exactly as
				// the observed-claim checkpoint above does: a recovered claim
				// carries the plane's recorded branch and checkpointEvidence
				// re-verified it, so the two already agree today — but the
				// invariant "the carried claim's branch is the verified one"
				// is what verifiedBranch and the takeover hand-off depend on,
				// so hold it by construction rather than by coincidence of a
				// single-source read. CLA-576: an unverified recovery rides
				// the same way, marked so its brief is the unverified variant.
				r.end.claim.Claim.Branch = r.end.branch
				r.end.claim.CheckpointUnverified = at.evidenceUnverified
				r.end.claim.CheckpointTip = peekTip
				r.end.checkpoint = true
				if at.evidenceUnverified {
					log.Printf("%siteration %d: %s ended with no claim observed in its stream, but the plane holds %s (run %s) with branch %s recorded — it could not be verified on the origin remote (%s), so this is an UNVERIFIED checkpoint: keeping the lease for the next phase; the review phase must verify the branch itself",
						labelOf(t), drainNum, ph.Label(phaseIdx), claim.Claim.TaskID, claim.Claim.RunID, r.end.branch, at.evidenceWhy)
				} else {
					log.Printf("%siteration %d: %s ended with no claim observed in its stream, but the plane holds %s (run %s) with branch %s verified on the origin remote — treating it as the phase checkpoint and keeping the lease for the next phase",
						labelOf(t), drainNum, ph.Label(phaseIdx), claim.Claim.TaskID, claim.Claim.RunID, r.end.branch)
				}
			} else {
				log.Printf("%siteration %d: the plane holds %s (run %s) with branch %q recorded, but that branch could not be verified on the origin remote (%s) — not treating it as a checkpoint; the lease expires with its hand-off intact",
					labelOf(t), drainNum, claim.Claim.TaskID, claim.Claim.RunID, r.end.branch, at.evidenceWhy)
			}
		}
	}
}
