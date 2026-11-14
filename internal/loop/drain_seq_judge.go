package loop

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// The walk stages that judge how a phase ended: dead, handed off, or verified.

// judgeDeadPhase retries a dead first phase up to the per-task bound, then parks
// the task; a fleet-wide run of deaths pauses the target instead.
func (s *drainSeq) judgeDeadPhase(ctx context.Context, st *seqStep) seqNext {
	d := s.d
	t := s.t
	ti := s.ti
	drainNum := s.drainNum
	// CLA-386: a dead phase is a FAILED phase, not a completed one, and it is
	// not "ended without holding the task" either — it never finished the job
	// it was spawned to do. The claim was released back in drainPhase (a retry
	// re-claims, exactly like a transient one), so the only question left is
	// retry vs park. Consecutive dead phases on the SAME task: retry up to the
	// per-task budget, then park — a task that can kill four full sessions
	// reaches the operator rather than a fifth (CLA-396, raised from two by
	// the 2026-08-20 operator decision).
	//
	// Deliberately BEFORE the error break below: the dead classification
	// names a phase that produced nothing whatever its exit code, so a dead
	// phase is retried-then-parked, never a run-failing error — the non-zero
	// exit that would otherwise stop the daemon is itself part of the silent
	// death, not a verdict.
	//
	// A retry cannot re-seed a resumed phase after the dead handback: invocationFor
	// only substitutes the task/run placeholders and sets ResumeClaim when the
	// predecessor claim is present, which the handback cleared. Retrying a dead
	// RESUMED phase would spawn a session whose brief still carries the literal
	// {{taskId}}/{{runId}} and whose handback, salvage and delivery checks are
	// all switched off. So the dead retry is scoped to the phase the walk
	// STARTED at (`i == start`). A session-claimed start re-claims in its own
	// session; a runner-claimed start is re-claimed by the runner before the
	// retry. A resumed phase's dead signature still vetoes its checkpoint below
	// (the false-premise guard applies to every phase); it just is not retried or
	// parked here — the drain ends and the task returns to the queue, where a
	// fresh claiming session retries it with a valid seed (review finding). The
	// operator decision's "retry the implement phase N times, then park" is
	// exactly `i == start` in the shipped [implement, review] sequence. A
	// resume-only failure is the exception (see below): it KEEPS the claim, so
	// `prev` re-seeds it and it may retry at any non-last position.
	//
	// CLA-396: a dead phase feeds BOTH counters, but the per-task park is
	// checked and taken FIRST — before the fleet counter's trip is even
	// consulted. The fleet counter persists on the Driver across drains and
	// only resets on a successful implement phase, so it can carry a residual
	// count left over from an earlier, unrelated task's death into THIS
	// task's own run of deaths. Checking fleet first would let that leftover
	// tip the fleet bound one death before this task reaches its own —
	// pre-empting a park this task legitimately earned with a fleet-wide
	// pause that blames the provider for what is, this time, actually the
	// task. Per-task takes priority: a task that has itself earned a park is
	// parked, full stop; only once that is ruled out does a fleet-wide run
	// of deaths across DIFFERENT tasks get to trip the fleet pause. A session
	// that never got past its claim is excluded from both by construction:
	// `end.dead` requires `res.Claim.Held()`, and a refused claim (lease_live,
	// a lost race) observes no task id, so its death counts toward neither
	// counter. This is the discriminator CLA-402's retrospective scan must
	// agree with.
	if st.end.dead {
		taskID := ""
		if st.end.claim != nil {
			taskID = st.end.claim.Claim.TaskID
		}
		// CLA-877: a resume-only failure KEPT its claim on purpose (see
		// settleWithoutCheckpoint), so the retry resumes that same run without a
		// re-claim. Its retry is safe in ANY phase position, not only the phase
		// the walk started at: the kept claim is still `prev`, so invocationFor
		// seeds the retry's brief exactly as it seeded the failed attempt's. Only
		// the start phase carries the prep facts (s.prep), and those it keeps —
		// minus the stale tip, dropped below.
		resumeOnly := st.end.reportResumeFailed != "" && st.end.claim != nil && st.end.claim.Claim.Held()
		retryable := !st.last && (s.i == s.start || resumeOnly)

		if retryable && taskID != "" && taskID == s.deadTask && s.deadRetries >= perTaskDeadBound-1 {
			how := "died producing nothing"
			switch {
			case st.end.reportResumeFailed != "":
				how = "failed re-emitting its done report (" + st.end.reportResumeFailed + ")"
			case st.end.toolLoop != "":
				how = "was killed (" + st.end.toolLoop + "), dead"
			}
			log.Printf("%siteration %d: the %s phase %s for the %dth consecutive time on this task — parking it per the 2026-08-20 decision (%d consecutive dead phases, then park)",
				labelOf(t), drainNum, st.ph.Label(s.i), how, s.deadRetries+1, perTaskDeadBound)
			d.parkDeadPhase(ctx, t, st.ph.Label(s.i), st.end.claim)
			s.parkFired = true // CLA-466: the record says parked, not dead
			// The task is parked for the operator and this drain is over; the
			// daemon carries on with the next task. Not a stop: the run did
			// not fail, one task reached a human.
			return seqBreak
		}

		d.fleetDead[ti]++
		if d.fleetDead[ti] >= fleetDeadBound {
			d.fleetTrip(ctx, t, ti, drainNum, st.end.claim)
			s.tripFired = true // CLA-466: a fleet trip ends this drain on a death
			// The fleet trip pauses this target and this drain is over; the
			// in-flight task was already released with the dead phase. Not a
			// stop: the run itself did not fail.
			return seqBreak
		}

		if retryable {
			// A runner-claimed start must re-claim before the respawn when the
			// dead phase handed the task back. The reclaim can land a DIFFERENT
			// task when this one is gone, so the counters below follow what is
			// actually held now, not what died. A report-resume failure handed
			// nothing back: its claim is still held, and re-claiming it would be
			// a plane round trip against a live lease (and could only be refused).
			if s.runnerClaimed && !resumeOnly {
				if taskID == "" {
					taskID = s.seen.TaskID
				}
				if !s.reclaimForRetry(ctx, taskID) {
					return seqBreak
				}
				taskID = s.seen.TaskID
			}
			// Either the first dead phase this task produced, or a dead phase
			// on a task other than the one counted above: that task has died
			// once, and earns its own retry ladder rather than being parked
			// for deaths it did not have.
			if taskID != s.deadTask {
				s.deadRetries = 1
				s.deadTask = taskID
			} else {
				s.deadRetries++
			}
			switch {
			case resumeOnly:
				// The brief was captured before this phase pushed anything. The
				// retry's context block reads WIP/branch off it, and a re-claim
				// would have refreshed both from the plane; do the same here so
				// the session is told the branch already carries prior work.
				if s.claimedTask.ID != "" {
					s.claimedTask.HasWIP = st.end.claim.Claim.HasWIP
					if b := st.end.claim.Claim.Branch; b != "" {
						s.claimedTask.Branch = b
					}
				}
				// The prep TIP is the revision the tree held BEFORE this session
				// pushed, so handing it to the retry would name a stale Tip (the
				// phase never held a checkpoint, so there is no fresher one). A
				// re-claim would have refreshed it; without one, drop it rather
				// than misdirect the retried session.
				s.prep.Tip = ""
				log.Printf("%siteration %d: the %s phase delivered its work but failed re-emitting its done report (%s) — retrying it within the SAME claim %s, no handback and no reclaim (%d of %d)",
					labelOf(t), drainNum, st.ph.Label(s.i), st.end.reportResumeFailed, claimLabel(st.end.claim.Claim), s.deadRetries, perTaskDeadBound)
			case st.end.toolLoop != "":
				log.Printf("%siteration %d: the %s phase was killed (%s) — retrying it (%d of %d) before any review brief sees the task",
					labelOf(t), drainNum, st.ph.Label(s.i), st.end.toolLoop, s.deadRetries, perTaskDeadBound)
			default:
				log.Printf("%siteration %d: the %s phase died producing nothing (final step reason %q, no branch recorded) — retrying it (%d of %d) before any review brief sees the task",
					labelOf(t), drainNum, st.ph.Label(s.i), deadReason(st.end.claim), s.deadRetries, perTaskDeadBound)
			}
			return seqContinue
		}
	}

	return seqFall
}

// judgeHandoff respawns the same phase on an accepted handoff, or re-judges the
// exit as a bare one when the chain cap or max-iterations refuses it.
func (s *drainSeq) judgeHandoff(ctx context.Context, st *seqStep) seqNext {
	d := s.d
	t := s.t
	tc := s.tc
	drainNum := s.drainNum
	phases := s.phases
	// A handoff the session emitted and drainPhase judged safe (clean exit,
	// trusted stream, task still held, prompt within the size cap). Two guards
	// remain, and both refuse toward the STANDARD path — the next configured
	// brief, or the drain ending — never toward truncating or rewriting the
	// session's prompt (CLA-352).
	if st.end.handoff != "" {
		switch {
		case s.chain >= handoffChainCap:
			log.Printf("%siteration %d: handoff refused — this task has already chained %d consecutive handoff respawns, the cap; falling back to the standard brief so a session cannot chain itself indefinitely",
				labelOf(t), drainNum, s.chain)
		case d.cfg.MaxIterations > 0 && drainNum+s.handoffs >= d.cfg.MaxIterations:
			log.Printf("%siteration %d: handoff refused — a respawn counts as an iteration and max-iterations (%d) is already spent; falling back to the normal path",
				labelOf(t), drainNum, d.cfg.MaxIterations)
		default:
			// Same phase index: the successor continues this phase's job. The
			// budget breaker at the top of the loop runs before the spawn.
			s.nextPrompt = st.end.handoff
			return seqContinue
		}
		// The handoff was REFUSED, so its exemption from the evidence gate ends
		// here: the exit is re-judged as the bare exit it really is (CLA-457).
		// With no durable, origin-reachable branch evidence that is a FAILED
		// empty phase — logged, tallied, never handed to the review brief.
		//
		// CLA-637: the review stage is over after all. drainPhase deliberately
		// left the matched review round SEALED when it detected the handoff —
		// the successor was going to close it at its own seam — but the refused
		// respawn is the last session that continuation was ever going to get,
		// so the round must be released HERE, before the checkpoint advances: a
		// fix phase spawned against a sealed round can neither read nor answer
		// (finding_sealed) the findings it was told to apply. A close that
		// fails refuses the checkpoint, exactly as the seam's own close does.
		nextReviewShaped := s.i+1 < len(phases) && config.ReviewShaped(phases[s.i+1].Prompt)
		if st.end.reviewRound > 0 && st.end.checkpoint && st.end.claim != nil && !nextReviewShaped {
			if cerr := d.closeReviewRound(ctx, t, *st.end.claim, st.end.reviewRound); cerr != nil {
				log.Printf("%siteration %d: the %s phase's review round %d could not be closed after its handoff was refused (%v) — refusing the checkpoint, because the next phase would be spawned against findings the plane still has sealed",
					labelOf(t), drainNum, st.ph.Label(s.i), st.end.reviewRound, cerr)
				d.tallyEmpty(st.ph.Label(s.i), tc.HarnessFor(st.ph))
				return seqBreak
			}
		}
		if !st.last && st.end.empty {
			log.Printf("%siteration %d: the %s phase ended holding a task with no recorded branch on the origin remote — not a checkpoint (%s), so no review phase spawns (its handoff was refused)",
				labelOf(t), drainNum, st.ph.Label(s.i), st.end.emptyWhy)
			d.tallyEmpty(st.ph.Label(s.i), tc.HarnessFor(st.ph))
			return seqBreak
		}
	}
	// This session ended without a respawn being accepted, so any chain is
	// broken: a later handoff starts counting from a standard-brief session.
	s.chain = 0

	return seqFall
}

// judgeVerifyGate is the verification round's stopping rule: it ends the walk
// when the gate passed or could not be judged, spawns another fix against open
// MUSTs, or parks the task at the round cap or on non-convergence.
func (s *drainSeq) judgeVerifyGate(ctx context.Context, st *seqStep) seqNext {
	d := s.d
	t := s.t
	drainNum := s.drainNum
	phases := s.phases
	fixIdx := s.fixIdx
	// CLA-638: the verification round is the stopping rule's gate. It is the
	// LAST configured phase, so the generic `!last` branches above never
	// applied to it; what follows is decided HERE from the durable records
	// the round just released:
	//
	//   - no open MUST → the gate passed. The verifier carried the terminal
	//     step (push check, PR, in_review) itself; the sequence is done.
	//   - open MUST(s) → another fix is spawned, unless the round cap or the
	//     non-convergence breaker parks the task first. Never one more round
	//     on the driver's own initiative.
	if isVerifyPhase(st.ph) {
		vres := harness.Result{}
		if st.end.claim != nil {
			vres = *st.end.claim
		}
		if st.end.verifyFailure != "" {
			// The seam could not judge the round: no durable record this run
			// wrote, a round that would not close, or a task that could not
			// be read back. That is the same fail-closed shape CLA-637 gives
			// a refused review checkpoint — the drain ends without the
			// hand-over and the claim is released/lapses, so a takeover can
			// retry it. It is not the stopping rule parking a disagreement.
			log.Printf("%siteration %d: the %s phase's round could not be judged (%s) — refusing the sequence: no hand-over, and no PR was accepted from this round",
				labelOf(t), drainNum, st.ph.Label(s.i), st.end.verifyFailure)
			return seqBreak
		}
		if !st.end.verifyChecked {
			// A verification session that died or produced no judgeable exit.
			// End the sequence; the normal takeover path retries it.
			log.Printf("%siteration %d: the %s phase produced no judgeable verification round — ending the sequence without a hand-over",
				labelOf(t), drainNum, st.ph.Label(s.i))
			return seqBreak
		}
		if len(st.end.verifyOpenMusts) == 0 {
			if st.end.verifyStatus == "in_progress" || st.end.verifyStatus == "ready" {
				log.Printf("%siteration %d: the %s phase closed round %d with NO open MUST but left the task %q — it recorded a pass but did not carry the terminal step (push check, PR, in_review); the sequence ends without a hand-over for the operator to see",
					labelOf(t), drainNum, st.ph.Label(s.i), st.end.verifyRound, st.end.verifyStatus)
			}
			if st.end.verifyRound == 0 {
				// CLA-638 × CLA-670: a BRANCHLESS round. Its record is the
				// legacy `## Review` block, so there is no plane open-MUST set
				// for the id-based gate to read — the verifier's own block
				// carried the verdict, and the sequence ends without claiming
				// the id-based stopping rule ran (it cannot, on this shape).
				log.Printf("iteration %d: the %s phase's verification round rests on the legacy %q block (this shape writes no durable record) — no plane open-MUST set to judge; ending the sequence",
					drainNum, st.ph.Label(s.i), config.ReviewRecordHeading)
				return seqBreak
			}
			log.Printf("iteration %d: the %s phase closed round %d with NO open MUST — the stopping rule is satisfied and the sequence is done",
				drainNum, st.ph.Label(s.i), st.end.verifyRound)
			return seqBreak
		}
		s.verifyRoundsRun++
		reason := nonConvergenceReason(st.end.verifyOpenMusts, st.end.verifyRound, s.verifyResolved)
		// The local fallback counts the same ROUNDS the plane does (CLA-686
		// D2). A review cycle is one review round plus the verification
		// rounds that follow it, and the plane's `counted` includes the
		// review round — so comparing verification rounds ALONE against the
		// cap allowed a cycle one round longer than the counted path, and
		// the park then read "Round 4 of the 3-round cap". `+1` is the
		// review round that opened this cycle.
		localRounds := s.verifyRoundsRun + 1
		capHit := st.end.verifyCounted >= config.ReviewRoundCap || localRounds >= config.ReviewRoundCap
		if reason == "" && !capHit && fixIdx >= 0 && st.end.checkpoint && s.carried != nil {
			// CLA-879: the rewind spawns the fix phase on the same verified
			// checkpoint, so it is a phase seam too: a STOP or RESTART dropped
			// during the verification round must act here, not after the fix
			// round it would otherwise start. The start table may resume at
			// verify instead of fix (open MUSTs not at the tip), so the log
			// names "a later phase" rather than promising the fix.
			if s.stopAtSeamMarker(ctx, st, fixIdx, "a later phase") {
				return seqBreak
			}
			for _, id := range st.end.verifyResolvedIDs {
				s.verifyResolved[id] = true
			}
			log.Printf("iteration %d: the %s phase closed round %d (%d non-void round(s) this cycle) with open MUST(s) [%s] — spawning another %s round against them",
				drainNum, st.ph.Label(s.i), st.end.verifyRound, st.end.verifyCounted,
				strings.Join(st.end.verifyOpenMusts, ", "), phases[fixIdx].Label(fixIdx))
			// CLA-639: the merged panel list belongs to the FIRST fix after
			// the panel, before any verification round has run. A verify
			// rewind is a later fix round: its grading is the verifier's own
			// record (plus the plane's open-MUST list), and reprinting the
			// panel's list would replace the verifier's findings with a
			// stale round. Clear the panel round so drainPhase falls back to
			// the shipped single-review path unchanged.
			s.panelRound = 0
			s.i = fixIdx
			return seqContinue
		}
		if reason == "" {
			switch {
			case capHit:
				reason = fmt.Sprintf("the %d-round cap for this review cycle is reached", config.ReviewRoundCap)
			case fixIdx < 0:
				reason = "the sequence has no fix phase to run between verification rounds"
			default:
				reason = "the round ended without holding the task, so no fix can be spawned against its open MUSTs"
			}
		}
		d.parkVerification(ctx, t, vres, reason, st.end.verifyRound, st.end.verifyCounted, st.end.verifyOpenMusts, st.end.verifyOpenDetails)
		s.parkFired = true
		s.carried = nil
		return seqBreak
	}
	return seqFall
}

// payOwedWaits takes the waits a resumed phase's sequence end owes the run -- a
// transient backoff and a usage-limit reset -- after the handback, with no lease
// held.
func (s *drainSeq) payOwedWaits(ctx context.Context) {
	d := s.d
	t := s.t
	tc := s.tc
	drainNum := s.drainNum
	prior := s.prior
	// CLA-662: the transient arm's wait, in the same place and for the same
	// reason as the limit wait below — after the sequence's handback, with NO
	// lease held. The rung is the target's streak of consecutive transient
	// ends, so a provider that keeps blipping the resumed phase is slowed
	// between drains instead of being re-drained the instant the last sequence
	// died (and, with implement and the resumed phase on different harnesses,
	// the only momentum the first-phase backoff could ever have was this).
	// The wait is bounded by retry_cap like every backoff(n), and STOP /
	// RESTART / RELOAD are honoured through waitOrStop exactly as they are
	// during the limit wait; the drain returns when it ends and Run polls again.
	//
	// Deliberately NOT fed into sidelineTarget, which the task that added this
	// asked us to consider: a sideline says "this target's harness is broken,
	// bench it" — its counters and log line are built for a drain that FAILED
	// to run a session — and this end returns err == nil. Run's success branch
	// would immediately clear the failure counter it read while leaving the
	// 15m+ skipUntil in place, so the log would report "the harness succeeded
	// again — rejoining the rotation" against a target still benched by a
	// sit-out nothing would then account for. Doing it properly means
	// redefining what a successful drain proves about the harness, well beyond
	// this bound's need: the ladder's growth, capped by retry_cap, plus
	// max_iterations is the brake the un-waited re-drain was missing.
	if s.transientRung > 0 {
		wait := d.resumedTransientDuration(s.transientRung)
		log.Printf("iteration %d: the sequence is over and no lease is held — waiting %s before polling again (transient failure %d in a row on %s; the run stays up)",
			drainNum, wait, s.transientRung, s.transientKey.harness)
		if d.waitOrStop(ctx, wait) {
			s.stop = true
		}
	}

	// CLA-631: the sequence ended on a rolling usage limit and the run is owed
	// its wait — after the handback above, deliberately, so the wait is taken
	// with NO lease held: the claim is back in the queue or, with pushed work,
	// left to lapse into a takeover exactly as the old stop path left it. The
	// wait itself is the first-phase path's own machinery, reused: the same
	// budget breaker inside supervisedWait, the same STOP/RESTART/RELOAD reads
	// in waitOrStop, the same probes on the harness that hit the limit, and the
	// same stated-reset handling — the difference is only that there is no
	// session left to re-run when it lifts, so the drain returns and Run polls
	// again. waitPastBudget is consulted first, exactly as on the first-phase
	// path: a reset beyond the run's remaining ceiling is not worth sleeping
	// through, so the run stops and says when to start a fresh one.
	if s.limitWait != nil {
		remaining, bounded := tc.Budget.Remaining(time.Since(prior.start))
		if until, over := waitPastBudget(s.limitWait.lim.ResetAt, remaining, bounded); over {
			log.Printf("iteration %d: the limit resets %s, in %s — more than the %s left of this run's ceiling; stopping now rather than waiting, so start a fresh run after the reset",
				drainNum, formatReset(until), time.Until(until).Round(time.Minute), remaining.Round(time.Minute))
			s.stop = true
		} else {
			log.Printf("iteration %d: the sequence is over and no lease is held — waiting for the reset%s before polling again (the run stays up)",
				drainNum, resetSuffix(s.limitWait.lim.ResetAt))
			ptokens, pcost, pstop := d.supervisedWait(ctx, s.limitWait.lim, s.limitWait.a, s.limitWait.ph, t, spend{
				start:  prior.start,
				tokens: prior.tokens + s.tokens,
				cost:   prior.cost + s.cost,
			})
			s.tokens += ptokens
			s.cost += pcost
			if pstop {
				s.stop = true
			}
		}
	}
}
