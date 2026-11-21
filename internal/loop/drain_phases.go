package loop

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// drainPhases runs ONE drain as its configured sequence of phase sessions — each a separate
// harness process, so the context resets between them. 2. If the next phase will never run, the
// handback skipped at the seam happens here instead — otherwise a budget stop leaves the task
// leased to nobody.
func (d *Driver) drainPhases(ctx context.Context, drainNum, ti int, t Target, prior spend) (tokens int, cost float64, handoffs int, stop bool, err error) {
	tc := d.targetCfg(t)
	phases := tc.EffectivePhases()
	rawPhases := make([]config.Phase, len(phases))
	for i := range phases {
		rawPhases[i] = phases[i]
		if src := phases[i].ConfiguredIndex(); src < len(tc.Phases) {
			rawPhases[i] = tc.Phases[src]
		}
	}
	// CLA-638: where the fix→verify pair lives, when the sequence has one.
	fixIdx := -1
	for i := range phases {
		if i+1 < len(phases) && phases[i].Name == config.FixPhaseName && isVerifyPhase(phases[i+1]) {
			fixIdx = i
		}
	}
	s := &drainSeq{
		d: d, t: t, tc: tc, ti: ti, drainNum: drainNum, prior: prior, phases: phases, rawPhases: rawPhases, fixIdx: fixIdx,
		verifyResolved: map[string]bool{}, panelFailed: map[string]panelMemberFailure{}, drainStart: time.Now(),
	}
	// CLA-466: one iteration record posts at this drain's boundary, whatever way it ends — hence the
	// defer, and hence state the deferred record reads.
	defer s.recordIteration()
	// The runner takes the claim before the walk so every phase, including the first, resumes a task
	// the driver holds.
	if _, ok := s.runnerClaimEligible(); ok {
		claim := s.claimForDrain(ctx)
		if claim == claimHeld && s.prepareStart(ctx) != startReady {
			s.carried = d.releaseCarried(ctx, t, s.carried)
			claim = claimNothing
		}
		if claim == claimNothing {
			// Wired mode loops straight back to the poll, and a zero-spend drain never trips the quiet
			// breaker: idle here, or a poll that says "spawnable" while next_task offers nothing spins
			// against the plane.
			s.stop = d.waitOrStop(ctx, d.cfg.IdlePollInterval.OrDefault(60*time.Second))
			return s.tokens, s.cost, s.handoffs, s.stop, s.err
		}
	}
walk:
	for s.i = s.start; s.i < len(phases); {
		st := &seqStep{}
		st.ph = phases[s.i]
		// CLA-639: whether this position is one reviewer of a review panel.
		st.panelMember = st.ph.IsPanelMember()
		st.last = s.i == len(phases)-1
		switch s.skipBranchlessSeat(st) {
		case seqContinue:
			continue
		case seqBreak:
			break walk
		}
		// CLA-639: the panel's last member is the last REVIEW of the sequence even when a fix follows it,
		// and the driver's plain-review seam must run for it (that is the seam that carries the record
		// gate).
		st.terminal = st.last
		if st.panelMember && !st.last && !config.ReviewShaped(phases[s.i+1].Prompt) {
			st.terminal = true
		}
		st.isHandoff = s.nextPrompt != ""
		// CLA-906: the boundary check runs BEFORE the panel fan-out. A review panel is one phase boundary
		// that starts N sessions at once, so the pause must hold the whole fan-out — checking per
		// member would either run the first member inside the window or split the panel across it.
		switch s.checkPhaseBoundary(ctx, st) {
		case seqContinue:
			continue
		case seqBreak:
			break walk
		}
		switch s.runPanelFanOut(ctx, st) {
		case seqContinue:
			continue
		case seqBreak:
			break walk
		}
		st.promptBytes = 0
		switch s.buildHandoffPrompt(st) {
		case seqContinue:
			continue
		case seqBreak:
			break walk
		}
		switch s.runPhase(ctx, st) {
		case seqContinue:
			continue
		case seqBreak:
			break walk
		}
		if st.end.runLost {
			log.Printf("%siteration %d: ending the drain: the task is no longer this run's", labelOf(t), drainNum)
			break
		}
		switch s.judgeSequenceEnd(st) {
		case seqContinue:
			continue
		case seqBreak:
			break walk
		}
		switch s.handlePanelMember(ctx, st) {
		case seqContinue:
			continue
		case seqBreak:
			break walk
		}
		// CLA-703: a LONE review-only phase (no panel) that never reached the backlog is an
		// infrastructure miss, not a dead phase and not a review verdict.
		if st.end.noMCP && !st.end.panelMember {
			d.tallyNoMCP(st.ph.Label(s.i), tc.HarnessFor(st.ph))
			note := fmt.Sprintf("no_mcp: the %s session made no successful clankerbar MCP call and recorded no review record", st.ph.Label(s.i))
			log.Printf("%siteration %d: %s — ending the drain; this is an infrastructure miss, not a review result", labelOf(t), drainNum, note)
			break
		}

		switch s.judgeDeadPhase(ctx, st) {
		case seqContinue:
			continue
		case seqBreak:
			break walk
		}
		// CLA-784: an optional (trial) seat's invocation error is that seat's result, not the drain's —
		// the same rule the parallel join applies with its `!m.optional` guard.
		if st.perr != nil && !(st.end.panelMember && st.end.panelOptional) {
			s.err = st.perr
			break
		}

		switch s.judgeHandoff(ctx, st) {
		case seqContinue:
			continue
		case seqBreak:
			break walk
		}
		// CLA-947 review 1.review-1.6: a pricing window opened between the boundary check and the spawn,
		// so the spawn was refused and nothing was spent.
		if st.end.windowHeld {
			log.Printf("%siteration %d: the %s phase did not spawn — %s; the next drain's boundary holds until it ends",
				labelOf(t), drainNum, st.ph.Label(s.i), st.end.windowWhy)
			break
		}
		// A held-but-empty exit is a FAILED phase (2026-08-24), not a checkpoint and not an ordinary
		// "ended without holding the task" end: the claim was held to the end, it just carried no
		// durable, origin-reachable branch evidence.
		if st.end.empty {
			if !st.last && s.i == s.start && s.emptyRetries < 1 && st.end.releasedToQueue {
				if s.runnerClaimed {
					taskID := s.seen.TaskID
					if st.end.claim != nil {
						taskID = st.end.claim.Claim.TaskID
					}
					if !s.reclaimForRetry(ctx, taskID) {
						break
					}
				}
				s.emptyRetries++
				continue
			}
			break
		}

		if !st.last && !st.end.checkpoint {
			log.Printf("iteration %d: the %s phase ended without holding the task — nothing for the next phase to resume, so this drain ends here",
				drainNum, st.ph.Label(s.i))
			break
		}
		switch s.judgeVerifyGate(ctx, st) {
		case seqContinue:
			continue
		case seqBreak:
			break walk
		}
		// A phase that ended dead was retried above and, on the retry, either parked or advanced —
		// either way this phase's dead-phase budget is spent.
		s.deadRetries = 0
		s.deadTask = ""
		s.emptyRetries = 0
		// CLA-396: a successful IMPLEMENT phase resets the FLEET-wide dead-phase counter.
		if st.ph.Name == config.ImplementPhaseName {
			d.fleetDead[ti] = 0
		}
		// CLA-639: a member that DID record leaves its round open for the next member (the close belongs
		// to the panel's last phase, not to each reviewer), so remember it — a later member that fails
		// must still be able to release what the earlier ones wrote.
		if st.panelMember && st.end.reviewRound > 0 {
			s.panelRound = st.end.reviewRound
		}
		// CLA-879: STOP and RESTART act HERE, on the seam a verified checkpoint has just opened, rather
		// than waiting for the whole sequence to finish.
		if s.stopAtSeamMarker(ctx, st, s.i+1, "") {
			break walk
		}
		s.i++
	}

	// The deferred handback (2, above). Skipped at every seam that led to another phase; owed at the
	// one that did not — including a final-phase handoff the guards above refused, whose claim was
	// held for a successor that never ran.
	s.carried = d.releaseCarried(ctx, t, s.carried)

	s.payOwedWaits(ctx)
	return s.tokens, s.cost, s.handoffs, s.stop, s.err
}

// drainWithRetries runs a single unphased drain — the whole task in one session. ti is the
// target's index, taken explicitly rather than from prior: a caller that passes spend{start: ...}
// without a ti must not have its spend silently booked to target 0 and judged by target 0's stored
// budget.
func (d *Driver) drainWithRetries(ctx context.Context, drainNum, ti int, t Target, prior spend) (tokens int, cost float64, stop bool, err error) {
	phases := d.targetCfg(t).EffectivePhases()
	ph := phases[0]
	tokens, cost, stop, _, err = d.drainPhase(ctx, drainNum, ti, 0, "", ph, true, nil, t, prior, phases, panelState{}, phaseRunOpts{})
	return tokens, cost, stop, err
}

// phaseRunOpts are the per-spawn facts a panel's parallel fan-out adds to drainPhase. The zero
// value is every other caller: the phase runs in the checkout sessionDir resolved, and drainPhase
// itself closes the review round when the review stage is over.
type phaseRunOpts struct {
	// worktree, when non-empty, is the session's start directory instead of the one sessionDir
	// resolved: the member's own DETACHED checkout at the reviewed tip. The brief tells the session it
	// is there, so a member cannot wander into the shared task worktree.
	worktree string

	// taskDir, when non-empty, is the runner-prepared task worktree: the spawn dir, with no panel
	// note, because the brief already describes that tree.
	taskDir string

	// panelParallel says the JOIN that spawned this member owns the review round's close. A member
	// must not close it itself: its siblings may still be recording, and the close is what releases
	// the findings they must not see.
	panelParallel bool

	// onSessionLaunch, when non-nil, is called immediately before the adapter is invoked — the first
	// moment this phase's session launch has actually BEGUN — on every attempt the ladder makes.
	onSessionLaunch func()

	// runner, when non-nil, briefs this phase as a session of a drain the RUNNER claimed: the
	// runner-claimed briefs and the task context block.
	runner *runnerBriefing

	// baseSHA is the runner's prep merge-base for a spawn that has no runner briefing to carry it —
	// a parallel panel member, which is spawned by the join with no briefing because it writes no
	// report.
	baseSHA string
}

// panelWorktreeNote is prepended to a PARALLEL panel member's brief. The shipped brief says to work
// in "the worktree for branch X", which is the task's shared one; giving each member its own
// detached checkout would be contradicted by that sentence, so the driver states the fact first.
const panelWorktreeNote = "YOUR CHECKOUT: you are one member of a review PANEL running in PARALLEL " +
	"with its siblings. The driver created a DETACHED worktree of the task's repository at the reviewed tip " +
	"and started this session IN it; work here for the whole session. Do NOT switch to, edit, or leave " +
	"artifacts in the task's shared worktree — another member and the fix phase use that one. This checkout " +
	"is discarded when you end and is never salvaged, so put nothing you need in it. Nothing you do here can " +
	"affect a sibling's checkout, so a probe is yours alone. "
