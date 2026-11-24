package loop

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// The walk stages that set up, run and close out one phase pass.

// buildHandoffPrompt turns a pending handoff into this pass's prompt: the driver's resume preamble,
// the predecessor's own prompt, and the phase's own continuation.
func (s *drainSeq) buildHandoffPrompt(st *seqStep) seqNext {
	phases := s.phases
	rawPhases := s.rawPhases
	if st.isHandoff {
		// The respawn continues this phase's job under its knobs — turn cap and tier still apply.
		st.promptBytes = len(s.nextPrompt)
		continuation := ""
		if s.i < len(rawPhases) && rawPhases[s.i].Prompt == "" {
			// CLA-497: a handoff during a NO-CODE review must not append the branch-shaped
			// reviewTerminalStep, which tells the successor to open a PR for a task that has no branch and
			// no code.
			reviewOnly := st.ph.Name == config.ReviewPhaseName && !st.terminal
			// CLA-637: the checkpoint that OPENED this phase decides which review-only variant the successor
			// continues.
			unverifiedCheckpoint := s.carried != nil && s.carried.CheckpointUnverified
			switch {
			case st.ph.Name == config.ReviewPhaseName && s.branchlessCheckpoint && reviewOnly:
				continuation = config.NoCodeReviewOnlyHandoffContinuation()
			case st.ph.Name == config.ReviewPhaseName && s.branchlessCheckpoint:
				continuation = config.NoCodeHandoffContinuation()
			case st.ph.Name == config.ReviewPhaseName && reviewOnly && unverifiedCheckpoint:
				continuation = config.UnverifiedReviewOnlyHandoffContinuation()
			case st.ph.Name == config.FixPhaseName && s.branchlessCheckpoint && s.i+1 < len(phases) && isVerifyPhase(phases[s.i+1]):
				// CLA-638: a BRANCHLESS fix whose verify round follows must still stop before the hand-over —
				// the legacy no-code fix continuation would tell its successor to move the task to in_review
				// (noCodeReviewTerminalStep), which is the verifier's move.
				continuation = config.NoCodeFixVerifyHandoffContinuation()
			case st.ph.Name == config.FixPhaseName && s.branchlessCheckpoint:
				continuation = config.NoCodeFixHandoffContinuation()
			case st.ph.Name == config.FixPhaseName && s.i+1 < len(phases) && isVerifyPhase(phases[s.i+1]):
				// CLA-638: the successor of a mid-fix handoff must still stop before the PR; the verification
				// round owns the hand-over.
				continuation = config.FixVerifyHandoffContinuation()
			default:
				continuation = config.HandoffContinuationFor(st.ph.Name, reviewOnly)
			}
		}
		if s.runnerClaimed {
			// The runner holds the lease and the worktree, so the successor must not resume the run itself
			// nor fetch a skill page.
			st.ph.Prompt = config.RunnerHandoffPreamble + s.nextPrompt + config.RunnerHandoffContinuation(continuation) + s.runnerDoneNote()
		} else {
			st.ph.Prompt = config.HandoffPreamble + s.nextPrompt + continuation
		}
		s.nextPrompt = ""
	}

	return seqFall
}

// runnerDoneNote is the done-report format the handoff successor of the phase at s.i must be told,
// "" for a phase whose runner brief writes no report (a close-out, a panel member, a custom
// prompt).
func (s *drainSeq) runnerDoneNote() string {
	if s.i < 0 || s.i >= len(s.phases) {
		return ""
	}
	ph := s.phases[s.i]
	rb := &runnerBriefing{closeOut: s.closeOut && s.i == s.start}
	return config.RunnerDoneReportFor(resolvedRunnerBrief(ph.Prompt, rb, ph.IsPanelMember(), s.carried))
}

// checkPhaseBoundary runs the budget breaker and the scheduled-pause hold at a phase boundary,
// before a further session (or a handoff respawn) is spawned.
func (s *drainSeq) checkPhaseBoundary(ctx context.Context, st *seqStep) seqNext {
	// The breaker at a phase boundary — the third place a session ends and the loop decides whether
	// to spawn another, alongside between-drains and between-attempts.
	if s.spawned > 0 && s.budgetStopAtBoundary(st) {
		return seqBreak
	}
	// CLA-906: a window holds the next spawn, never a running session.
	return s.takeBoundaryHolds(ctx, st, s.holdThroughPause, s.holdThroughRouteWindows)
}

// boundaryHold is one reason a phase boundary may not spawn yet: the daemon-wide scheduled pause,
// or the pricing windows of the routes a phase may start on.
type boundaryHold func(context.Context, *seqStep) pauseHold

// takeBoundaryHolds runs the boundary holds in order and folds the two endings that are not
// "resume" into the walk: a stop, and a lost run.
func (s *drainSeq) takeBoundaryHolds(ctx context.Context, st *seqStep, holds ...boundaryHold) seqNext {
	for _, hold := range holds {
		switch out := hold(ctx, st); out {
		case pauseHoldStop:
			s.stop = true
			return seqBreak
		case pauseHoldRunLost:
			// A lost run ends THIS drain, not the daemon — the same ending the mid-drain runLost arm takes
			// (drain_phases.go).
			log.Printf("%siteration %d: ending the drain: the task is no longer this run's", labelOf(s.t), s.drainNum)
			return seqBreak
		}
	}
	return seqFall
}

// budgetStopAtBoundary is checkPhaseBoundary's breaker pass: it logs and stops the drain when the
// target's budget is spent.
func (s *drainSeq) budgetStopAtBoundary(st *seqStep) bool {
	d := s.d
	drainNum := s.drainNum
	prior := s.prior
	if v := d.budgetStop(s.ti, time.Since(prior.start)); v.dim != "" {
		boundary := "the " + st.ph.Label(s.i) + " phase boundary"
		if st.isHandoff {
			boundary = "the handoff respawn"
		}
		log.Printf("iteration %d: budget reached at %s: %s — ending this drain (tokens=%d cost=$%.2f elapsed=%s)",
			drainNum, boundary, v.dim, v.tokens, v.cost, v.elapsed.Round(time.Second))
		s.stop = true
		return true
	}
	return false
}

// runPhase spawns one phase session and folds how it ended into the walk: the session entry, the
// claim the sequence now holds, and what is owed a handback.
func (s *drainSeq) runPhase(ctx context.Context, st *seqStep) seqNext {
	d := s.d
	t := s.t
	ti := s.ti
	drainNum := s.drainNum
	prior := s.prior
	phases := s.phases
	// Unphased runs keep their log names byte-identical: no phase tag.
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

	// CLA-466: announce the phase change NOW rather than at the next poll, so the console sees which
	// phase is running without waiting out an idle interval.
	ref := ""
	if s.carried != nil {
		ref = claimLabel(s.carried.Claim)
	}
	if len(s.phasesRun) == 0 || s.phasesRun[len(s.phasesRun)-1] != st.ph.Label(s.i) {
		s.phasesRun = append(s.phasesRun, st.ph.Label(s.i))
	}
	d.beginIteration(ti, t, drainNum, ref, st.ph.Label(s.i))

	// CLA-662: whether this session RESUMED a predecessor's claim — the exact condition drainPhase's
	// transient arm keys on (prev != nil), so the streak follows the ends that arm produces.
	st.phaseResumed = s.carried != nil
	opts := phaseRunOpts{taskDir: s.taskDir}
	if s.runnerClaimed && s.carried != nil {
		opts.runner = &runnerBriefing{task: s.claimedTask, runID: s.carried.Claim.RunID, start: s.i == s.start, baseSHA: s.prep.BaseSHA}
		if opts.runner.start {
			opts.runner.prep = s.prep
			opts.runner.closeOut = s.closeOut
		}
	}
	st.ptokens, st.pcost, st.pstop, st.end, st.perr = d.drainPhase(ctx, drainNum, ti, s.i, st.tag, st.ph, st.terminal, s.carried, t, spend{
		start: prior.start,
		ti:    s.ti,
	}, phases, panelState{round: s.panelRound, failed: s.panelFailed, recorded: s.panelRecorded}, opts)
	s.tokens += st.ptokens
	s.cost += st.pcost
	// CLA-630: one entry per session this pass spawned, in spawn order.
	if st.end.session != nil {
		s.sessionsRun = append(s.sessionsRun, *st.end.session)
	}
	s.finalDead = st.end.dead
	s.lastProducedNothing = st.end.producedNothing
	// CLA-466: the record's task identity and claim disposition follow EVERY claim the sequence
	// observed — including one a dead phase already handed back itself (end.released), which the
	// switch below deliberately drops from `carried`.
	if st.end.claim != nil && !(s.carried != nil && s.carried.Claim.TaskID != st.end.claim.Claim.TaskID) {
		s.seen = st.end.claim.Claim
		s.finalOutcome = claimDisposition(st.end.claim)
	}
	// CLA-535: a blocking ask about the drain's own task is what the closing beacon reports as
	// waiting. The ref is resolved HERE, while s.seen still names the task the phase asked about.
	if st.end.blockingAsk != "" && s.seen.Names(st.end.blockingAsk) {
		s.askedRef = claimLabel(s.seen)
	}
	// What, if anything, is still owed a handback. Three cases, and getting any of them wrong is a
	// task either stranded on a dead lease or posted back to the queue over work already in review: -
	// the phase handed the claim back itself → nothing is owed; - the phase observed claim state →
	// that is now the authoritative view, including Settled and HasWIP, which releaseHeldClaim reads;
	// - the phase observed nothing (it never launched) → KEEP the predecessor's, or a phase 2 that
	// failed to start would silently drop the handback for the task phase 1 is still holding.
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
	// CLA-497: whether the checkpoint that opened THIS phase was evidenced by the plane's record (no
	// verified branch).
	s.branchlessCheckpoint = st.seamClaim != nil && st.seamClaim.Claim.Branch == ""

	// CLA-639 M4: a PANEL MEMBER that produced no record leaves the run resumable — the next member,
	// or the fix phase, picks up the SAME run from the SAME verified checkpoint.
	if st.panelMember && !st.end.reviewRecorded && st.seamClaim != nil && s.carried != nil && s.carried.Claim.Held() {
		if s.carried.Claim.Branch == "" {
			s.carried.Claim.Branch = st.seamClaim.Claim.Branch
		}
		if s.carried.CheckpointTip == "" {
			s.carried.CheckpointTip = st.seamClaim.CheckpointTip
		}
		s.carried.CheckpointUnverified = st.seamClaim.CheckpointUnverified
	}

	// The marker a later claim's start choice reads rides the checkpoint this phase already proved:
	// the tip recorded here is the one origin holds.
	s.markPhaseDone(ctx, st)

	return seqFall
}

// judgeSequenceEnd ends the walk on a run-level stop, or on a resumed phase's sequence end (which
// owes the run a wait), and clears the transient streak for a resumed phase that ended any other
// way.
func (s *drainSeq) judgeSequenceEnd(st *seqStep) seqNext {
	d := s.d
	tc := s.tc
	ti := s.ti
	// A run-level stop wins over a dead phase. drainPhase returns stop=true for a budget trip and
	// credit exhaustion (lim.Stop) — and on those paths end.dead can be set too (a credit-starved
	// session is exactly the shape that dies on reason "unknown" with no branch).
	if st.pstop {
		s.stop = true
		return seqBreak
	}

	// CLA-631/CLA-651/CLA-662: a RESUMED phase ended the sequence without completing it — a rolling
	// usage limit or a transient failure — and the RUN must not end with it.
	if st.end.sequenceEnd != nil && !st.end.panelMember {
		s.limitWait = st.end.sequenceEnd.limitWait
		if s.limitWait == nil {
			s.transientKey = resumedTransientKey{target: ti, harness: tc.HarnessFor(st.ph)}
			s.transientRung = d.noteResumedTransient(s.transientKey)
		}
		return seqBreak
	}

	if st.phaseResumed {
		d.clearResumedTransient(resumedTransientKey{target: ti, harness: tc.HarnessFor(st.ph)})
	}

	return seqFall
}
