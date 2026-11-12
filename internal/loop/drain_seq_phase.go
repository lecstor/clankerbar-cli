package loop

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// The walk stages that set up, run and close out one phase pass.

// buildHandoffPrompt turns a pending handoff into this pass's prompt: the
// driver's resume preamble, the predecessor's own prompt, and the phase's own
// continuation.
func (s *drainSeq) buildHandoffPrompt(st *seqStep) seqNext {
	phases := s.phases
	rawPhases := s.rawPhases
	if st.isHandoff {
		// The respawn continues this phase's job under its knobs — turn cap
		// and tier still apply. The session's own prompt is framed by
		// config.HandoffPreamble, not handed over bare: the successor needs
		// the same "resume, don't claim" contract a phase resume gets from
		// its brief, and a session-authored prompt cannot be trusted to
		// carry that forward on its own. promptBytes is the session's own
		// prompt alone — what the size cap was measured against — not the
		// preamble-inflated total, so the log line stays comparable to it.
		//
		// config.HandoffContinuation (CLA-353) is appended after it for the
		// same reason: the phase's OWN brief — not only the preamble's
		// contract — is otherwise dropped on a handoff, since ph.Prompt is
		// replaced wholesale rather than framing the built-in text. For the
		// review phase that would silently drop the PR-then-hand-off
		// terminal step the moment a session hands off mid-review.
		//
		// Gated on the phase actually RUNNING the built-in brief (review
		// finding): HandoffContinuation matches on name alone, so an
		// operator who reuses the name "review" for a phase carrying their
		// own custom Prompt would otherwise have the shipped terminal step
		// force-injected into a handoff respawn of a brief that never
		// established that contract in the first place — clashing with
		// whatever terminal step their own prompt names, if any.
		// rawPhases (the configured sequence captured at drain start, not
		// EffectivePhases' output) is index-aligned with phases: a panel
		// member maps back to the `review` phase it expanded from, and every
		// other entry is its own configured phase. Never re-read live: a
		// RELOAD applied mid-drain must not reshape the in-flight sequence's
		// continuation (CLA-664) — the same rule the mode and the count follow.
		st.promptBytes = len(s.nextPrompt)
		continuation := ""
		if s.i < len(rawPhases) && rawPhases[s.i].Prompt == "" {
			// CLA-497: a handoff during a NO-CODE review must not append the
			// branch-shaped reviewTerminalStep, which tells the successor to
			// open a PR for a task that has no branch and no code.
			// branchlessCheckpoint records the checkpoint that opened this
			// phase (captured at the seam from the carried claim), so the
			// continuation matches the brief this phase's first session ran.
			//
			// The split adds the POSITION-dependent review mode: a handoff
			// during a review-only phase must carry the record-then-hold
			// ending, never the commit/push/PR/in_review terminal step. A
			// fix phase carries that terminal step by name.
			reviewOnly := st.ph.Name == config.ReviewPhaseName && !st.terminal
			// CLA-637: the checkpoint that OPENED this phase decides which
			// review-only variant the successor continues. A branchless
			// checkpoint's successor gets the no-tip shape; an UNVERIFIED
			// one has no driver-known tip to hand over either, so its
			// successor gets the variant that tells the reviewer to
			// establish the tip itself — the verified shape's {{reviewedTip}}
			// could not be filled and would leak as a literal.
			unverifiedCheckpoint := s.carried != nil && s.carried.CheckpointUnverified
			switch {
			case st.ph.Name == config.ReviewPhaseName && s.branchlessCheckpoint && reviewOnly:
				continuation = config.NoCodeReviewOnlyHandoffContinuation()
			case st.ph.Name == config.ReviewPhaseName && s.branchlessCheckpoint:
				continuation = config.NoCodeHandoffContinuation()
			case st.ph.Name == config.ReviewPhaseName && reviewOnly && unverifiedCheckpoint:
				continuation = config.UnverifiedReviewOnlyHandoffContinuation()
			case st.ph.Name == config.FixPhaseName && s.branchlessCheckpoint && s.i+1 < len(phases) && isVerifyPhase(phases[s.i+1]):
				// CLA-638: a BRANCHLESS fix whose verify round follows must
				// still stop before the hand-over — the legacy no-code fix
				// continuation would tell its successor to move the task to
				// in_review (noCodeReviewTerminalStep), which is the
				// verifier's move.
				continuation = config.NoCodeFixVerifyHandoffContinuation()
			case st.ph.Name == config.FixPhaseName && s.branchlessCheckpoint:
				continuation = config.NoCodeFixHandoffContinuation()
			case st.ph.Name == config.FixPhaseName && s.i+1 < len(phases) && isVerifyPhase(phases[s.i+1]):
				// CLA-638: the successor of a mid-fix handoff must still stop
				// before the PR; the verification round owns the hand-over.
				continuation = config.FixVerifyHandoffContinuation()
			default:
				continuation = config.HandoffContinuationFor(st.ph.Name, reviewOnly)
			}
		}
		if s.runnerClaimed {
			// The runner holds the lease and the worktree, so the successor must
			// not resume the run itself nor fetch a skill page. The report note
			// rides last because a successor's prompt is the predecessor's own
			// text, not a brief: without it the successor would write nothing.
			st.ph.Prompt = config.RunnerHandoffPreamble + s.nextPrompt + config.RunnerHandoffContinuation(continuation) + s.runnerDoneNote()
		} else {
			st.ph.Prompt = config.HandoffPreamble + s.nextPrompt + continuation
		}
		s.nextPrompt = ""
	}

	return seqFall
}

// runnerDoneNote is the done-report format the handoff successor of the phase at
// s.i must be told, "" for a phase whose runner brief writes no report (a
// close-out, a panel member, a custom prompt).
func (s *drainSeq) runnerDoneNote() string {
	if s.i < 0 || s.i >= len(s.phases) {
		return ""
	}
	ph := s.phases[s.i]
	rb := &runnerBriefing{closeOut: s.closeOut && s.i == s.start}
	return config.RunnerDoneReportFor(resolvedRunnerBrief(ph.Prompt, rb, ph.IsPanelMember(), s.carried))
}

// checkPhaseBoundary runs the budget breaker at a phase boundary, before a
// further session (or a handoff respawn) is spawned.
func (s *drainSeq) checkPhaseBoundary(st *seqStep) seqNext {
	d := s.d
	drainNum := s.drainNum
	prior := s.prior
	if s.spawned > 0 {
		// The breaker at a phase boundary — the third place a session ends and
		// the loop decides whether to spawn another, alongside between-drains
		// and between-attempts, and the finest granularity the ceiling has had.
		// It is not decoration: one measured task spent 92% of a whole run's
		// 75M ceiling inside a single session, which a between-drains check
		// cannot see coming and cannot interrupt. A handoff respawn gets the
		// same check, BEFORE the respawn: a session cannot spend its way past
		// the ceiling by handing off to itself.
		if dim := d.budgetTrip(prior.tokens+s.tokens, prior.cost+s.cost, time.Since(prior.start)); dim != "" {
			boundary := "the " + st.ph.Label(s.i) + " phase boundary"
			if st.isHandoff {
				boundary = "the handoff respawn"
			}
			log.Printf("iteration %d: budget reached at %s: %s — stopping (tokens=%d cost=$%.2f elapsed=%s)",
				drainNum, boundary, dim, prior.tokens+s.tokens, prior.cost+s.cost,
				time.Since(prior.start).Round(time.Second))
			s.stop = true
			return seqBreak
		}
	}

	return seqFall
}

// runPhase spawns one phase session and folds how it ended into the walk: the
// session entry, the claim the sequence now holds, and what is owed a handback.
func (s *drainSeq) runPhase(ctx context.Context, st *seqStep) seqNext {
	d := s.d
	t := s.t
	ti := s.ti
	drainNum := s.drainNum
	prior := s.prior
	phases := s.phases
	// Unphased runs keep their log names byte-identical: no phase tag. A
	// handoff respawn is tagged too, so the state dir shows the chain the way
	// the daemon log does.
	st.tag = ""
	if len(phases) > 1 {
		st.tag = "-p" + st.ph.Label(s.i)
	}
	if st.isHandoff {
		s.handoffs++
		s.chain++
		st.tag += fmt.Sprintf("-h%d", s.chain)
		log.Printf("%siteration %d: handoff respawn %d — spawning a fresh session on the predecessor's own %d-byte prompt, framed by the driver's resume preamble (counts as an iteration)",
			labelOf(t), drainNum, s.chain, st.promptBytes)
	}
	s.spawned++

	// CLA-466: announce the phase change NOW rather than at the next poll, so
	// the console sees which phase is running without waiting out an idle
	// interval. taskRef is empty until a claim has been observed (the first
	// phase claims inside drainPhase); from then on it rides every beacon.
	// CLA-510: a first-phase claim no longer waits for the next phase
	// boundary — the renewer's claim reflector (startLeaseRenewal) revises
	// the in-flight ref and beacons the moment the session's claim lands.
	ref := ""
	if s.carried != nil {
		ref = claimLabel(s.carried.Claim)
	}
	if len(s.phasesRun) == 0 || s.phasesRun[len(s.phasesRun)-1] != st.ph.Label(s.i) {
		s.phasesRun = append(s.phasesRun, st.ph.Label(s.i))
	}
	d.beginIteration(ti, t, drainNum, ref, st.ph.Label(s.i))

	// CLA-662: whether this session RESUMED a predecessor's claim — the
	// exact condition drainPhase's transient arm keys on (prev != nil), so
	// the streak follows the ends that arm produces. Captured BEFORE the
	// call: the seam below may replace `carried` with this phase's own
	// claim, which says nothing about how the phase was entered.
	st.phaseResumed = s.carried != nil
	opts := phaseRunOpts{taskDir: s.taskDir}
	if s.runnerClaimed && s.carried != nil {
		opts.runner = &runnerBriefing{task: s.claimedTask, runID: s.carried.Claim.RunID, start: s.i == s.start}
		if opts.runner.start {
			opts.runner.prep = s.prep
			opts.runner.closeOut = s.closeOut
		}
	}
	st.ptokens, st.pcost, st.pstop, st.end, st.perr = d.drainPhase(ctx, drainNum, ti, s.i, st.tag, st.ph, st.terminal, s.carried, t, spend{
		start:  prior.start,
		tokens: prior.tokens + s.tokens,
		cost:   prior.cost + s.cost,
	}, phases, panelState{round: s.panelRound, failed: s.panelFailed, recorded: s.panelRecorded}, opts)
	s.tokens += st.ptokens
	s.cost += st.pcost
	// CLA-630: one entry per session this pass spawned, in spawn order. nil
	// means the pass never launched a session (a harness or spawn-dir
	// refusal above), which is nothing to record.
	if st.end.session != nil {
		s.sessionsRun = append(s.sessionsRun, *st.end.session)
	}
	s.finalDead = st.end.dead
	s.lastProducedNothing = st.end.producedNothing
	// CLA-466: the record's task identity and claim disposition follow EVERY
	// claim the sequence observed — including one a dead phase already handed
	// back itself (end.released), which the switch below deliberately drops
	// from `carried`. The off-brief exception matches the switch's own first
	// arm: a claim on a DIFFERENT task does not re-attribute the iteration.
	if st.end.claim != nil && !(s.carried != nil && s.carried.Claim.TaskID != st.end.claim.Claim.TaskID) {
		s.seen = st.end.claim.Claim
		s.finalOutcome = claimDisposition(st.end.claim)
	}
	// What, if anything, is still owed a handback. Three cases, and getting any
	// of them wrong is a task either stranded on a dead lease or posted back to
	// the queue over work already in review:
	//
	//   - the phase handed the claim back itself → nothing is owed;
	//   - the phase observed claim state → that is now the authoritative view,
	//     including Settled and HasWIP, which releaseHeldClaim reads;
	//   - the phase observed nothing (it never launched) → KEEP the predecessor's,
	//     or a phase 2 that failed to start would silently drop the handback for
	//     the task phase 1 is still holding.
	//
	// A phase that went off-brief and claimed a DIFFERENT task is its own case.
	// noteClaimed replaces the observed claim wholesale, so the seam releases
	// that task and reports `released` — which would drop the predecessor's
	// still-live claim and hand back a task the sequence was never meant to
	// touch. Keep what we had: the task phase 1 is holding is the one owed a
	// handback, whatever its successor went and did.
	st.seamClaim = s.carried
	switch {
	case s.carried != nil && st.end.claim != nil && st.end.claim.Claim.TaskID != s.carried.Claim.TaskID:
		log.Printf("iteration %d: the %s phase claimed %s, which is NOT the task it was resuming (%s) — it was told not to claim at all; keeping the original for the handback",
			drainNum, st.ph.Label(s.i), st.end.claim.Claim.TaskID, s.carried.Claim.TaskID)
	case st.end.released:
		s.carried = nil
	case st.end.claim != nil:
		s.carried = st.end.claim
	}
	// CLA-497: whether the checkpoint that opened THIS phase was evidenced
	// by the plane's record (no verified branch). Read from the PRE-switch
	// claim - the one the seam advanced with: after the switch, carried is
	// this phase's OWN claim, whose branch says nothing about the checkpoint
	// that opened the phase, and the handoff continuation must match the
	// brief this phase's FIRST session ran, not what its last one recorded.
	s.branchlessCheckpoint = st.seamClaim != nil && st.seamClaim.Claim.Branch == ""

	// CLA-639 M4: a PANEL MEMBER that produced no record leaves the run
	// resumable — the next member, or the fix phase, picks up the SAME run
	// from the SAME verified checkpoint. Its own result is what carries the
	// claim forward (the switch above), and a session that was turned away or
	// died before reading the task reports no branch: without this, the next
	// member's brief would be handed an empty branch and read as the
	// BRANCHLESS variant, whose record is the legacy block the panel cannot
	// use. So the checkpoint's resumption facts ride forward from the claim
	// that opened this member, onto a claim that is still live and short of
	// them. Only a member: every other phase's claim is its own.
	if st.panelMember && !st.end.reviewRecorded && st.seamClaim != nil && s.carried != nil && s.carried.Claim.Held() {
		if s.carried.Claim.Branch == "" {
			s.carried.Claim.Branch = st.seamClaim.Claim.Branch
		}
		if s.carried.CheckpointTip == "" {
			s.carried.CheckpointTip = st.seamClaim.CheckpointTip
		}
		s.carried.CheckpointUnverified = st.seamClaim.CheckpointUnverified
	}

	// The marker a later claim's start choice reads rides the checkpoint this
	// phase already proved: the tip recorded here is the one origin holds.
	s.markPhaseDone(ctx, st)

	return seqFall
}

// judgeSequenceEnd ends the walk on a run-level stop, or on a resumed phase's
// sequence end (which owes the run a wait), and clears the transient streak
// for a resumed phase that ended any other way.
func (s *drainSeq) judgeSequenceEnd(st *seqStep) seqNext {
	d := s.d
	tc := s.tc
	ti := s.ti
	// A run-level stop wins over a dead phase. drainPhase returns stop=true
	// for a budget trip and credit exhaustion (lim.Stop) — and on those
	// paths end.dead can be set too (a credit-starved session is exactly the
	// shape that dies on reason "unknown" with no branch). Retrying a dead
	// phase against a stopped run burns another session into the same limit
	// and, on the second, parks a task for what was actually a run-wide
	// outage instead of stopping the daemon. A run stop is final; the dead
	// branch only decides retry vs park when the run is continuing. (A
	// resumed phase ending on a live lease is NOT one of those paths — see
	// the sequence-end break below, deliberately before the dead arm,
	// because that end leaves the run continuing.)
	if st.pstop {
		s.stop = true
		return seqBreak
	}

	// CLA-631/CLA-651/CLA-662: a RESUMED phase ended the sequence without
	// completing it — a rolling usage limit (CLA-631) or a transient failure
	// (CLA-651) — and the RUN must not end with it. The sequence stops
	// exactly like the pstop above — the claim is not handed back to the
	// queue, so the lease lapses into a takeover with its branch recorded —
	// but the run carries on: the wait the signal owes is taken below,
	// after the sequence's deferred handback, holding no lease. Broken here
	// rather than flowing into the checks below because those read a session
	// that merely got turned away: the dead/fleet counters would tick on a
	// blip's exit shape (or a limit's "unknown" finish reason), and none of
	// that machinery applies to a sequence end the run is ordered to survive.
	//
	// The transient arm ALSO extends its target's streak here (CLA-662):
	// nothing else bounds a provider that keeps blipping the resumed phase
	// while the implementing harness stays healthy — the drain is over, so
	// drainPhase's retry ladder was never entered, and the re-drain would
	// otherwise land the very next instant. A rolling limit extends no
	// streak: its wait is a reset, not a rung, and the next drain is
	// scheduled after it anyway.
	// CLA-639: a PANEL member's sequence-end is judged by the panel handler
	// below first — it records the member as failed, closes the round that the
	// earlier members wrote into, and only then lets the run-level wait take
	// over. Every other resumed phase keeps the shipped break here.
	if st.end.sequenceEnd != nil && !st.end.panelMember {
		s.limitWait = st.end.sequenceEnd.limitWait
		if s.limitWait == nil {
			s.transientKey = resumedTransientKey{target: ti, harness: tc.HarnessFor(st.ph)}
			s.transientRung = d.noteResumedTransient(s.transientKey)
		}
		return seqBreak
	}

	// CLA-662: a resumed phase that ended any OTHER way than the transient
	// sequence-end above — completed, settled, died — breaks its target's
	// streak: the next blip on that target+harness is not consecutive with
	// the last, and starts the ladder over. The other sequence-end arm, a
	// rolling limit, also breaks out above and so neither extends nor
	// clears: the reset the run waits out is its own bound. Reset on the
	// seam BETWEEN the arms, so "consecutive" means exactly what it says:
	// the arm above extends, this one clears.
	if st.phaseResumed {
		d.clearResumedTransient(resumedTransientKey{target: ti, harness: tc.HarnessFor(st.ph)})
	}

	return seqFall
}
