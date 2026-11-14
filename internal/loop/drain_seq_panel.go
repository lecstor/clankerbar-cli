package loop

import (
	"context"
	"log"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// The walk stages that belong to a review panel.

// skipBranchlessSeat applies the branchless panel's one-reviewer rule: it skips an
// optional seat, and every member after the one required reviewer that runs.
func (s *drainSeq) skipBranchlessSeat(st *seqStep) seqNext {
	t := s.t
	drainNum := s.drainNum
	// CLA-639 M5: a BRANCHLESS task cannot run a panel AT ALL, so its panel
	// is one reviewer. This shape's review record is the legacy `## Review`
	// block written to the task's outcome — the plane accepts a durable
	// record only from the task's live holder, and a settled task has none
	// (question 03a12578, option 1) — so a second member could record nothing
	// a durable round could hold, and the block it would append is the FIRST
	// member's own record.
	//
	// CLA-784: that ONE reviewer is the first REQUIRED member, never blindly
	// the first configured one. An optional (trial) seat is skipped here: its
	// failure is that seat's own result, and if it consumed the single review
	// slot a branchless task would be left with no required review at all
	// (review 1.review-2.1 / 1.review-4.1). Validate refuses an all-optional
	// panel, so a required seat always exists; the members after the chosen
	// one are skipped, with one log line.
	//
	// The test is the claim CARRIED INTO this phase (`carried`), not
	// `branchlessCheckpoint`: that flag is set at the seam from the claim the
	// PREVIOUS phase was entered with, so it is still false at the FIRST
	// panel member — exactly the position where the trial seat sits in the
	// shape this finding names. `carried` is what `drainPhase` itself reads
	// for `branchlessPanel` (`prev.Claim.Branch == ""`), so the two agree.
	if st.panelMember && s.carried != nil && s.carried.Claim.Branch == "" {
		if st.ph.IsOptionalPanelMember() {
			log.Printf("%siteration %d: this task's checkpoint is BRANCHLESS (its review record is the legacy %q block, not a durable one), so its review panel runs ONE reviewer — skipping the OPTIONAL (trial) seat %s: the required seat is the one that must record",
				labelOf(t), drainNum, config.ReviewRecordHeading, st.ph.Label(s.i))
			s.i++
			return seqContinue
		}
		if s.branchlessPanelRan {
			if !s.branchlessPanelNoted {
				log.Printf("%siteration %d: this task's checkpoint is BRANCHLESS (its review record is the legacy %q block, not a durable one), so the review panel runs ONE reviewer: %s",
					labelOf(t), drainNum, config.ReviewRecordHeading, s.branchlessPanelChosen)
				s.branchlessPanelNoted = true
			}
			s.i++
			return seqContinue
		}
		s.branchlessPanelRan = true
		s.branchlessPanelChosen = st.ph.Label(s.i)
	}
	return seqFall
}

// runPanelFanOut runs a panel's contiguous members concurrently when the panel
// concurrency allows it, folding their joined result into the walk. It does
// nothing at concurrency 1, and the sequential path then runs as before.
func (s *drainSeq) runPanelFanOut(ctx context.Context, st *seqStep) seqNext {
	d := s.d
	t := s.t
	tc := s.tc
	ti := s.ti
	drainNum := s.drainNum
	prior := s.prior
	phases := s.phases
	// CLA-697: fan the panel's contiguous members out concurrently, each in
	// its own detached worktree. A handoff respawn continues ONE phase (never
	// a panel run), and a branchless checkpoint runs one reviewer by design —
	// both take the shipped sequential path below.
	//
	// The join owns everything the sequential member handler would have done
	// one member at a time: the incomplete list, the round close, the run-level
	// wait a limited member owes, and the decision not to spawn a fix when
	// nobody recorded. With cap = 1 (or a single member) this block does
	// nothing at all and the old path runs byte for byte.
	if !st.panelMember {
		s.panelFanOutOff = false
	}
	if st.panelMember && !st.isHandoff && !s.branchlessCheckpoint && !s.panelFanOutOff {
		runEnd := s.i + 1
		for runEnd < len(phases) && phases[runEnd].IsPanelMember() {
			runEnd++
		}
		if n, conc := runEnd-s.i, tc.EffectivePanelConcurrency(runEnd-s.i); n > 1 && conc > 1 {
			// The breaker at the boundary BEFORE the fan-out, exactly as the
			// sequential path's per-spawn check would have stopped before the
			// first member it could not afford. The target's ledger already
			// carries every counted member (CLA-480).
			if s.spawned > 0 {
				if v := d.budgetStop(s.ti, time.Since(prior.start)); v.dim != "" {
					log.Printf("iteration %d: budget reached at the %s phase boundary: %s — ending this drain (tokens=%d cost=$%.2f elapsed=%s)",
						drainNum, st.ph.Label(s.i), v.dim, v.tokens, v.cost, v.elapsed.Round(time.Second))
					s.stop = true
					return seqBreak
				}
			}
			if out, ok := d.runPanelInParallel(ctx, drainNum, ti, t, phases, s.i, runEnd, conc, s.carried, spend{
				start: prior.start,
				ti:    s.ti,
			}, s.prep.BaseSHA); ok {
				s.tokens += out.tokens
				s.cost += out.cost
				s.spawned += n
				for k, m := range out.members {
					if m.end.session != nil {
						s.sessionsRun = append(s.sessionsRun, *m.end.session)
					}
					label := phases[s.i+k].Label(s.i + k)
					if len(s.phasesRun) == 0 || s.phasesRun[len(s.phasesRun)-1] != label {
						s.phasesRun = append(s.phasesRun, label)
					}
				}
				// The member claims are the same run the panel resumed; keep
				// the CARRIED claim (with its verified branch and tip) rather
				// than adopting one member's copy. A member that recorded
				// nothing can end holding a claim that never had the tip
				// folded onto it, and adopting that one would hand the fix
				// phase an unverified-looking checkpoint.
				if out.claim != nil && (s.carried == nil || out.claim.Claim.TaskID == s.carried.Claim.TaskID) {
					s.seen = out.claim.Claim
					s.finalOutcome = out.disposition
				}
				s.finalDead = out.finalDead
				s.lastProducedNothing = out.lastProducedNothing
				s.panelRecorded = s.panelRecorded || out.recorded
				s.panelRound = max(s.panelRound, out.round)
				s.panelIncomplete = append(s.panelIncomplete, out.incomplete...)
				for role, why := range out.failed {
					s.panelFailed[role] = why
				}
				// The run-level endings, in the order the sequential handler
				// checks them: a close that failed is fail-closed and stops the
				// drain; a member's budget stop ends the run; a limit or
				// transient end hands the drain its wait (the claim is left to
				// lapse into a takeover, never released); no record at all means
				// no fix.
				switch {
				case out.runLost:
					// The task is no longer this run's: nothing is owed a handback and no later
					// phase runs. First, so a failed close cannot leave the claim to be released.
					s.carried = nil
					log.Printf("%siteration %d: ending the drain: the task is no longer this run's", labelOf(s.t), s.drainNum)
					return seqBreak
				case out.closeFailed:
					if s.limitWait == nil && out.sequenceEnded && out.limitWait == nil {
						s.transientKey = out.transientKey
						s.transientRung = d.noteResumedTransient(s.transientKey)
					}
				case out.stop:
					s.stop = true
				case out.err != nil:
					s.err = out.err
				case out.sequenceEnded:
					s.limitWait = out.limitWait
					if s.limitWait == nil {
						s.transientKey = out.transientKey
						s.transientRung = d.noteResumedTransient(s.transientKey)
					}
				}
				if s.stop || s.err != nil || out.closeFailed || out.sequenceEnded || !out.recorded || runEnd >= len(phases) {
					return seqBreak
				}
				s.i = runEnd
				return seqContinue
			}
		}
	}
	return seqFall
}

// handlePanelMember records a panel member that wrote no record as failed and
// keeps the panel going, or closes the round and hands over when the panel is
// over.
func (s *drainSeq) handlePanelMember(ctx context.Context, st *seqStep) seqNext {
	d := s.d
	t := s.t
	tc := s.tc
	ti := s.ti
	drainNum := s.drainNum
	phases := s.phases
	// CLA-639: the panel's members are skippable where a lone review phase is
	// not. A member that recorded nothing — dead, capped before its record
	// write, empty under the evidence gate, or turned away by a non-retryable
	// error — is RECORDED AS FAILED and the panel moves on, instead of ending
	// the drain with three reviewers still unrun. That is the panel's version
	// of the doneWhen's "recorded or been recorded as failed/capped".
	//
	// Deliberately BEFORE the dead/fleet arms: those exist for a phase whose
	// death leaves the task going nowhere, but a panel member's death is one
	// reviewer lost, not the task lost. Counting it toward the fleet pause
	// would let a flaky reviewer bench the whole target.
	//
	// The round is NOT closed while another member follows: closing it
	// would release the earlier members' findings to that member, which is
	// exactly the panel independence the seal exists to protect. It is closed
	// when the panel is over, whichever way the last member ends.
	if st.end.panelMember {
		if round := st.end.reviewRound; round > 0 {
			s.panelRound = round
		}
		// The member's reviewer ROLE is positional (CLA-639): the durable
		// record and the seam gate are keyed on it, and the operator's `name`
		// is only the log label. The incomplete list and the fix brief must
		// therefore speak roles, or a named member would be reported missing
		// while its role-keyed record is sitting right there.
		role := reviewerRoleFor(phases, s.i)
		member := st.ph.Label(s.i)
		// CLA-639 M4: whether the NEXT member, or the fix phase after the
		// panel, could still resume this run. The member's own claim is the
		// live one when it observed any; a member that observed NOTHING (a
		// launch failure) leaves the predecessor's carried claim in place, and
		// that is still the run the panel is working. A claim the session
		// settled itself, or one already handed back, is not live: nothing may
		// be resumed from it, and only the live holder may close a round.
		live := s.carried != nil && (st.end.claim == nil || st.end.claim.Claim.Held())
		// A member ends the PANEL's stage when it recorded (its record passed
		// the seam gate) or when no next member can run — the last member, a
		// claim the failed member took with it, or a harness sequence-end.
		// Only a failed member with a live claim and a member after it is
		// skipped past.
		//
		// A member whose own round close failed DID record: the seam matched
		// a record and the close is a later, separate step. Counting that as
		// "recorded nothing" would name a reviewer INCOMPLETE and charge its
		// failure streak for the plane's failure to release the round.
		recorded := st.end.reviewRecorded || st.end.reviewCloseFailed
		seqEnd := st.end.sequenceEnd
		// A member that writes its durable record and only THEN ends the
		// sequence (a usage limit or a transient failure) returns before the
		// seam evaluates the record gate. Ask the plane the same question the
		// seam asks, so the member is counted as recorded instead of unnamed
		// INCOMPLETE and charged a failure for writing exactly what it owed
		// (CLA-639 fix pass 4). Only the seam's own matcher inputs qualify: a
		// verified tip (match it exactly) or the unverified mark (demand a
		// tip). A carried claim with neither — a branch with no tip and no
		// unverified mark, the shape the checkpoint-fact copy cannot fill
		// when the claim is not held — would make the matcher accept any
		// record by run and role, looser than the seam ever is, so it is left
		// to the failure arm (CLA-639 fix pass 5).
		if !recorded && seqEnd != nil && s.carried != nil && (s.carried.CheckpointTip != "" || s.carried.CheckpointUnverified) {
			if rec, matched := d.panelMemberRecordMatch(ctx, t, *s.carried, role); matched {
				recorded = true
				if rec.Round > 0 {
					s.panelRound = rec.Round
				}
			}
		}
		// endSequenceEnd is the shared tail of the panel's run-level endings:
		// a usage limit or transient failure owes the run its wait after the
		// sequence's handback, exactly as the single-review seam does.
		endSequenceEnd := func() {
			if seqEnd == nil {
				return
			}
			// CLA-784: an OPTIONAL seat's end is that seat's own result. It
			// may not end the sequence with a run-level wait; the panel
			// carries on, and the fix spawns from the required members'
			// records exactly as for any incomplete panel.
			if st.end.panelOptional {
				return
			}
			s.limitWait = seqEnd.limitWait
			if s.limitWait == nil {
				s.transientKey = resumedTransientKey{target: ti, harness: tc.HarnessFor(st.ph)}
				s.transientRung = d.noteResumedTransient(s.transientKey)
			}
		}
		if recorded {
			// The member recorded, so its failure streak is over (CLA-639
			// M4): the next failure, if any, is not consecutive with the last.
			d.panelMemberRecorded(member)
			// CLA-784: only a REQUIRED member's record is a review. An
			// optional (trial) seat's record is that seat's result — it counts
			// toward its own assessment, but a round where only optional
			// seats recorded is no review at all (see the panel-last check
			// below).
			if !st.end.panelOptional {
				s.panelRecorded = true
			}
		}
		// A round that could not be closed is the single-review seam's
		// fail-closed shape, for the panel: the member recorded, but the fix
		// must not spawn against findings the plane still holds sealed. End
		// the drain here (the deferred handback releases the claim exactly as
		// the single-review close-failure path does) and say why.
		if st.end.reviewCloseFailed {
			log.Printf("%siteration %d: the %s panel member recorded, but the panel's round %d could not be closed (%s) — ending the drain without spawning the fix: a fix spawned now would run against findings the plane still has sealed",
				labelOf(t), drainNum, member, s.panelRound, st.end.emptyWhy)
			endSequenceEnd()
			return seqBreak
		}
		if !recorded {
			reason := st.end.emptyWhy
			switch {
			case st.end.noMCP:
				// CLA-703: an infrastructure miss, not a reviewer's verdict.
				// The `no_mcp` token is stable so the fixer's INCOMPLETE list
				// and the analytics can exclude it without parsing prose.
				reason = "no_mcp: no successful clankerbar MCP call and no review record"
			case st.end.toolLoop != "":
				reason = st.end.toolLoop
			case st.end.dead:
				reason = "died producing nothing"
			case seqEnd != nil && seqEnd.limitWait != nil:
				reason = "hit a usage limit before recording"
			case seqEnd != nil:
				reason = "ended on a transient failure before recording"
			case st.perr != nil:
				reason = failureDetail(st.perr.Error())
			case st.end.empty:
				reason = st.end.emptyWhy
			case reason == "":
				reason = "recorded no durable review record"
			}
			s.panelIncomplete = append(s.panelIncomplete, panelIncompleteRole(role, st.end.panelOptional)+": "+reason)
			s.panelFailed[role] = panelMemberFailure{reason: reason, optional: st.end.panelOptional}
			log.Printf("%siteration %d: the %s panel member recorded nothing (%s) — recording it as failed", labelOf(t), drainNum, member, reason)
			// CLA-639 M4: the member is a failure the operator has to be able
			// to see, so it is counted in the run's per-harness dead-phase
			// tally (CLA-402) — the rate that exists for exactly this shape —
			// unless the seam's own classification already booked this exit
			// (a dead session, or a held-but-empty one). And a member that
			// keeps failing task after task is a fault in that member rather
			// than bad luck: the streak is counted across tasks and said
			// loudly once it reaches panelMemberFailBound. Deliberately NOT a
			// park: one reviewer of several failing is not a reason to strand
			// the panel's real findings.
			//
			// CLA-703: a no_mcp miss belongs to NEITHER counter. It is not a
			// dead phase (no work was attempted), and it is not the member's
			// fault — charging its streak would make an MCP outage look like a
			// broken reviewer. It gets its own tally cell and its own log line.
			if st.end.noMCP {
				d.tallyNoMCP(member, tc.HarnessFor(st.ph))
				log.Printf("%siteration %d: the %s panel member's failure is no_mcp (infrastructure), not a review failure — not counted against the member's streak or the dead rate",
					labelOf(t), drainNum, member)
			} else {
				if !st.end.deadBooked {
					d.tallyEmpty(member, tc.HarnessFor(st.ph))
				}
				if n := d.panelMemberFailed(member); n >= panelMemberFailBound {
					model, _ := tc.ModelForPhase(st.ph)
					log.Printf("%siteration %d: WARNING — the %s review-panel member has recorded nothing on %d consecutive tasks (harness %s, model %s); the panel runs on and the fix applies what the other members found, but this member's harness, model or brief is not producing reviews — check its iteration logs",
						labelOf(t), drainNum, member, n, tc.HarnessFor(st.ph), attributionOr(model, "harness default"))
				}
			}
			if live && !st.end.panelLast {
				// The next member resumes this claim: keep the panel going, and
				// keep the round sealed — a close here would release the earlier
				// members' findings to the next reviewer. ADVANCE, never re-run
				// this member: the loop has no post statement, so a `continue`
				// without the increment would re-spawn index i forever.
				s.i++
				return seqContinue
			}
			// The panel is over: nothing follows this member, or the claim it
			// would have resumed is gone, or the harness ended the sequence.
			// Release what the earlier members wrote rather than leave it
			// sealed behind a lease that will lapse — BEFORE any handback,
			// because the close is legal only while we are still the holder.
			if s.panelRecorded && s.panelRound > 0 && live {
				if cerr := d.closeReviewRound(ctx, t, *s.carried, s.panelRound); cerr != nil {
					log.Printf("%siteration %d: the panel's round %d could not be closed after %s failed (%v) — ending the drain without spawning the fix: a fix spawned now would run against findings the plane still has sealed",
						labelOf(t), drainNum, s.panelRound, member, cerr)
					endSequenceEnd()
					return seqBreak
				}
			}
			switch {
			case live && s.panelRecorded && s.i+1 < len(phases):
				// Earlier members DID record, so the panel hands over rather
				// than ending: the phase after it (the fix) applies the merged
				// list, which names this member as INCOMPLETE, and resumes the
				// same run. Checked BEFORE the sequence-end arm on purpose — a
				// member turned away by a usage limit is exactly the case the
				// bar names ("a usage-limited or dead member does not stop the
				// round"), and the sequence is not ending here: if the phase
				// that follows hits the same limit it says so itself, with the
				// wait that belongs to ITS end.
				s.i++
				return seqContinue
			case seqEnd != nil:
				// The run-level sequence-end break above was deferred for this
				// member so the round could be released first; end the attempt
				// here, with the run's wait machinery to follow.
				endSequenceEnd()
			}
			return seqBreak
		}
		// CLA-784: a panel where ONLY optional seats recorded has no review
		// to apply — an optional seat's record is that seat's result, not a
		// review the fix was waiting for. Treat it exactly as a panel where
		// nobody recorded: leave the round sealed (no close) and spawn no
		// fix. Reached only at the panel's LAST member; an earlier optional
		// seat's record is kept while a required member may still record.
		if st.end.panelLast && !s.panelRecorded {
			log.Printf("%siteration %d: no REQUIRED member of the review panel recorded — the round is not closed and no fix spawns", labelOf(t), drainNum)
			return seqBreak
		}
		// This member recorded. A recorded mid-panel member whose session was
		// then turned away by a run-level limit or transient failure continues
		// the panel exactly as a failed one does: its deliverable is in the
		// round, and the wait belongs to the end that actually stops the
		// sequence.
		//
		// The LAST member is the panel's close, so its sequence-end must do
		// what the failed last member's arm does: release the round while the
		// holder is still live (a sealed round would be voided by the
		// takeover, losing every member's record), then hand over to the fix
		// that applies the merged list. A close that fails is fail-closed —
		// the drain ends with the wait and no fix runs against a sealed
		// round.
		if seqEnd != nil {
			if live && !st.end.panelLast && s.i+1 < len(phases) {
				s.i++
				return seqContinue
			}
			// Close only when the seam did not already close the round for
			// this member. A session the adapter ALSO classifies as a limit
			// or transient can exit cleanly (or be capped with WIP), so the
			// seam runs first, matches the record and closes the round; the
			// sequence-end is then read by the arm below. A second close is
			// the plane's no_open_round, and failing closed on it would skip
			// the fix against a round that is already released.
			if live && s.panelRecorded && s.panelRound > 0 && !st.end.reviewRecorded {
				if cerr := d.closeReviewRound(ctx, t, *s.carried, s.panelRound); cerr != nil {
					log.Printf("%siteration %d: the panel's round %d could not be closed after %s recorded but ended the sequence (%v) — ending the drain without spawning the fix: a fix spawned now would run against findings the plane still has sealed",
						labelOf(t), drainNum, s.panelRound, member, cerr)
					endSequenceEnd()
					return seqBreak
				}
			}
			if live && s.panelRecorded && s.i+1 < len(phases) {
				s.i++
				return seqContinue
			}
			endSequenceEnd()
			return seqBreak
		}
	}

	return seqFall
}
