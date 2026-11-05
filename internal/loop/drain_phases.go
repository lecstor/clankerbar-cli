package loop

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// drainPhases runs ONE drain as its configured sequence of phase sessions —
// each a separate harness process, so the context resets between them. With no
// phases configured that sequence is one session and this is exactly the old
// behaviour; see config.Phase for why the split exists and config.EffectivePhases
// for why "unphased" is expressed as a single phase rather than a second path.
//
// Two things have to be true across a seam, and they are the whole design:
//
//  1. The claim is NOT handed back. The next phase resumes the same run with
//     heartbeat(runId) rather than claiming, so the lease has to survive the gap.
//     A handback here would put the task in the queue while we are still on it.
//  2. If the next phase will never run, the handback skipped at the seam happens
//     here instead — otherwise a budget stop leaves the task leased to nobody.
func (d *Driver) drainPhases(ctx context.Context, drainNum, ti int, t Target, prior spend) (tokens int, cost float64, handoffs int, stop bool, err error) {
	// The target's own effective config (CLA-410): a stored run-config overlay
	// changes harness/model/budget here, not on the shared base.
	tc := d.targetCfg(t)
	phases := tc.EffectivePhases()
	// rawPhases is the same sequence with every EXPANSION collapsed: index-
	// aligned 1:1 with phases, but each review-panel member maps back to the one
	// `review` phase the operator wrote (CLA-639), so a member is recognised as
	// running the resolved built-in brief its configured phase asked for rather
	// than a prompt of its own. The handoff branch below reads it to tell a
	// built-in-brief phase from an operator's custom prompt, and it MUST be this
	// captured copy (CLA-664): a live read would let a RELOAD applied mid-drain
	// reshape the continuation of the sequence in flight — a 3 -> 2 reload drops
	// the in-flight fix successor's terminal step, and a custom-prompt phase
	// later covered by a built-in gains one.
	rawPhases := make([]config.Phase, len(phases))
	for i := range phases {
		rawPhases[i] = phases[i]
		if src := phases[i].ConfiguredIndex(); src < len(tc.Phases) {
			rawPhases[i] = tc.Phases[src]
		}
	}
	// CLA-638: where the fix→verify pair lives, when the sequence has one. The
	// verification round is last by validation, and the driver repeats the pair
	// by rewinding to fixIdx after a round with open MUSTs. Zero when the
	// sequence has no verification round (the legacy fix-is-terminal shape).
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
	// CLA-466: one iteration record posts at this drain's boundary, whatever way
	// it ends — hence the defer, and hence state the deferred record reads.
	defer s.recordIteration()
walk:
	for s.i = 0; s.i < len(phases); {
		st := &seqStep{}
		st.ph = phases[s.i]
		// CLA-639: whether this position is one reviewer of a review panel. The
		// members are consecutive review-shaped phases, so "before the next
		// configured phase" is the same test the shipped sequence already uses.
		// Captured BEFORE the handoff branch replaces ph.Prompt with a
		// continuation: a panel member never honours a handoff, but the
		// continuation's own review position must still be read from the phase
		// identity, not from a prompt that is no longer a brief.
		st.panelMember = st.ph.IsPanelMember()
		st.last = s.i == len(phases)-1
		switch s.skipBranchlessSeat(st) {
		case seqContinue:
			continue
		case seqBreak:
			break walk
		}
		// CLA-639: the panel's last member is the last REVIEW of the sequence even
		// when a fix follows it, and the driver's plain-review seam must run for it
		// (that is the seam that carries the record gate). Only a PANEL member gets
		// this treatment: a shipped `review` before a fix is not "terminal", and
		// passing that word over would tell it there is nothing to checkpoint for.
		st.terminal = st.last
		if st.panelMember && !st.last && !config.ReviewShaped(phases[s.i+1].Prompt) {
			st.terminal = true
		}
		st.isHandoff = s.nextPrompt != ""
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
		switch s.checkPhaseBoundary(st) {
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
		// CLA-703: a LONE review-only phase (no panel) that never reached the
		// backlog is an infrastructure miss, not a dead phase and not a review
		// verdict. It gets its own drained ending: the claim was handed back (or
		// left to lapse) by drainPhase, the tally books the miss separately, and
		// the LOG names it so the operator does not read a review as having been
		// attempted when the tools were absent.
		//
		// CLA-738: the name is a log line only. The iteration Outcome stays the
		// four-word enum the plane accepts — prose there makes marshalReport
		// refuse the whole report, presence refresh included.
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
		// CLA-784: an optional (trial) seat's invocation error is that seat's
		// result, not the drain's — the same rule the parallel join applies with
		// its `!m.optional` guard. A member that recorded and THEN hit a harness
		// error has already delivered its record; ending the drain on it would
		// throw away the required members' round and the fix, the exact harm the
		// trial-seat contract exists to prevent. A REQUIRED member's error still
		// ends the drain exactly as before.
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
		// A held-but-empty exit is a FAILED phase (2026-08-24), not a checkpoint
		// and not an ordinary "ended without holding the task" end: the claim was
		// held to the end, it just carried no durable, origin-reachable branch
		// evidence. It gets ONE bounded retry, and only when the claim was ACTUALLY
		// released back to the queue — a fresh claiming session can then pick the
		// task straight back up. A recorded-but-unreachable branch was left to the
		// takeover path by the handback (never released), and retrying would seed a
		// session against a live lease nothing here renews, so such an exit ends the
		// drain. This also covers a handoff the guards REFUSED above (chain cap,
		// max-iterations): the handoff exemption ends the moment the successor is
		// not going to run, so the exit must be re-judged as the bare exit it really
		// is. Deliberately OUTSIDE the dead-phase ladder: the dead shape
		// retries-then-parks a task that can kill four sessions, while the empty
		// shape returns to the queue for the next drain — the tally measures it
		// without tripping the park or fleet machinery, which is how a fleet-wide
		// silent EOT shows up in the rate the operator watches.
		if st.end.empty {
			if !st.last && s.i == 0 && s.emptyRetries < 1 && st.end.releasedToQueue {
				s.emptyRetries++
				continue
			}
			break
		}

		// A non-final phase that did not reach its checkpoint leaves the next one
		// nothing to resume: the session settled the task itself (worked past its
		// brief and finished it), its stream could not be trusted to say either way,
		// or the adapter does not observe claims at all. The sequence is over, and
		// none of those is a failure — a task that reached in_review in one session
		// is a task that got done.
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
		// A phase that ended dead was retried above and, on the retry, either
		// parked or advanced — either way this phase's dead-phase budget is spent.
		// Resetting on the advance keeps the retry per-PHASE rather than per-run:
		// two dead phases separated by a healthy one are not "consecutive", and
		// each phase earns its own retry ladder (CLA-386).
		s.deadRetries = 0
		s.deadTask = ""
		s.emptyRetries = 0
		// CLA-396: a successful IMPLEMENT phase resets the FLEET-wide dead-phase
		// counter. Only the implement phase runs the harness that can exhibit the
		// zero-usage death — the review phase runs on the claude harness, which
		// cannot — so a claude success must not vouch for the opencode gateway:
		// letting any success reset the counter would hide a fleet fault from the
		// detector that exists to find it. Reaching here means this phase did not
		// die and did not fail, so for the implement phase it is the "successful
		// implement" the reset is keyed to.
		if st.ph.Name == config.ImplementPhaseName {
			d.fleetDead[ti] = 0
		}
		// CLA-639: a member that DID record leaves its round open for the next
		// member (the close belongs to the panel's last phase, not to each
		// reviewer), so remember it — a later member that fails must still be able
		// to release what the earlier ones wrote.
		if st.panelMember && st.end.reviewRound > 0 {
			s.panelRound = st.end.reviewRound
		}
		s.i++
	}

	// The deferred handback (2, above). Skipped at every seam that led to another
	// phase; owed at the one that did not — including a final-phase handoff the
	// guards above refused, whose claim was held for a successor that never ran.
	s.carried = d.releaseCarried(ctx, t, s.carried)

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
	return s.tokens, s.cost, s.handoffs, s.stop, s.err
}

// drainWithRetries runs a single unphased drain — the whole task in one session.
// It is what a config with no `phases` gets, and it is kept as its own entry
// point because that is the shape every existing test drives.
//
// The phase comes from EffectivePhases, not an inline literal, so the unphased
// path carries the same resolution every other path does — the top-level or
// default turn cap (CLA-343), which is precisely what the inline zero-value
// version failed to do.
func (d *Driver) drainWithRetries(ctx context.Context, drainNum int, t Target, prior spend) (tokens int, cost float64, stop bool, err error) {
	phases := d.targetCfg(t).EffectivePhases()
	ph := phases[0]
	tokens, cost, stop, _, err = d.drainPhase(ctx, drainNum, 0, 0, "", ph, true, nil, t, prior, phases, panelState{}, phaseRunOpts{})
	return tokens, cost, stop, err
}

// phaseRunOpts are the per-spawn facts a panel's parallel fan-out adds to
// drainPhase (CLA-697). The zero value is every other caller: the phase runs in
// the checkout sessionDir resolved, and drainPhase itself closes the review
// round when the review stage is over.
type phaseRunOpts struct {
	// worktree, when non-empty, is the session's start directory instead of the
	// one sessionDir resolved: the member's own DETACHED checkout at the reviewed
	// tip. The brief tells the session it is there, so a member cannot wander
	// into the shared task worktree.
	worktree string

	// panelParallel says the JOIN that spawned this member owns the review
	// round's close. A member must not close it itself: its siblings may still be
	// recording, and the close is what releases the findings they must not see.
	panelParallel bool

	// panelSpend, when non-nil, is this member's handle on its panel's shared
	// spend ledger (CLA-697 review finding 1.review-1.1). Every attempt's counted
	// spend is published to it the moment the ladder counts it, and the ladder's
	// pre-spawn budget check measures the run's totals PLUS the siblings' counted
	// spend. Without it each member compared the same prior snapshot plus its own
	// local spend against a run-wide ceiling none of them could see the others
	// approaching. Nil for every caller that is not a parallel panel member.
	panelSpend *panelSpendHandle

	// onSessionLaunch, when non-nil, is called immediately before the adapter is
	// invoked — the first moment this phase's session launch has actually BEGUN —
	// on every attempt the ladder makes. A parallel panel member uses it to
	// release the NEXT member's start: signalling when its worktree was ready let
	// a later member's own setup and launch overtake an earlier member's, so the
	// scheduler's list-order guarantee (CLA-697) did not extend to the adapters
	// (CLA-738 review 3.2). Idempotent on the panel side, so a retry's repeat
	// call is harmless. Nil for every other caller.
	onSessionLaunch func()
}

// panelWorktreeNote is prepended to a PARALLEL panel member's brief (CLA-697).
// The shipped brief says to work in "the worktree for branch X", which is the
// task's shared one; giving each member its own detached checkout would be
// contradicted by that sentence, so the driver states the fact first.
const panelWorktreeNote = "YOUR CHECKOUT: you are one member of a review PANEL running in PARALLEL " +
	"with its siblings. The driver created a DETACHED worktree of the task's repository at the reviewed tip " +
	"and started this session IN it; work here for the whole session. Do NOT switch to, edit, or leave " +
	"artifacts in the task's shared worktree — another member and the fix phase use that one. This checkout " +
	"is discarded when you end and is never salvaged, so put nothing you need in it. Nothing you do here can " +
	"affect a sibling's checkout, so a probe is yours alone. "
