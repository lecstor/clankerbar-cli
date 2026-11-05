package loop

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// drainSeq is one drain's walk through its phase sequence: what drainPhases
// resolved once, and the state the walk accumulates. The stage methods read and
// write it, so each can be read without the rest of drainPhases.
type drainSeq struct {
	// Resolved once per drain, never written by the walk.
	d         *Driver
	t         Target
	tc        *config.Config
	ti        int
	drainNum  int
	prior     spend
	phases    []config.Phase
	rawPhases []config.Phase
	fixIdx    int

	// i is the index of the phase being run. The walk has no post statement:
	// a stage that moves past a phase says so by writing it.
	i int

	// What the drain returns.
	tokens   int
	cost     float64
	handoffs int
	stop     bool
	err      error

	// MUST ids a verifier has already marked resolved in THIS drain. An id open
	// again is non-convergence (A6). In-memory on purpose: it is a courtesy
	// layer over the durable round-prefix check, which survives a takeover on
	// its own because it reads the plane's round numbers.
	verifyResolved map[string]bool
	// Verification rounds this drain has actually closed. The plane's counted is
	// the cap of record (it survives takeovers and ignores void rounds); this
	// local count is the last-resort bound that keeps the loop finite even if a
	// plane read serves no count at all, which would otherwise make "counted >=
	// 3" unreachable and the fix→verify pair unbounded.
	verifyRoundsRun int
	// CLA-639: the panel's members that wrote no record this round, and the round
	// the panel's released records landed in. A dead, capped or otherwise
	// record-less member does not stop the panel: it is listed here, the round
	// still closes, and the fix phase is told the panel was incomplete. The
	// round number is the one the seam MATCHED on a member that did record, so a
	// later member's failure can still release the records already written.
	panelIncomplete []string
	panelRound      int
	// CLA-639 M4: whether any REQUIRED member of this panel recorded, and why
	// each member that did NOT failed. The first decides whether a failed LAST
	// member hands the round to the fix phase (there is something to apply) or
	// ends the drain; the second rides the fix brief, so "the panel was
	// incomplete" names the members AND what happened to them.
	//
	// CLA-784: an OPTIONAL (trial) seat's record deliberately does NOT set this.
	// A round where only optional seats recorded is no review at all — there is
	// no required member's findings for a fix to work — and is treated exactly
	// as a panel where nobody recorded.
	panelRecorded bool
	panelFailed   map[string]panelMemberFailure
	// CLA-639 M5: the branchless-panel skip says its piece once, not once per
	// skipped member — N-1 identical lines would read as N problems.
	branchlessPanelNoted bool
	// CLA-784: the branchless panel's ONE reviewer is chosen from the REQUIRED
	// seats, never the first configured member blindly. branchlessPanelRan says
	// the chosen seat has already run (every later member is skipped);
	// branchlessPanelChosen is its label, for the skip log line.
	branchlessPanelRan    bool
	branchlessPanelChosen string
	// CLA-697: a panel run whose fan-out could not be set up (no repo, no
	// resolvable tip) falls back to the sequential members for THIS panel; the
	// flag stops every later member re-running the same failing probes.
	panelFanOutOff bool
	// The claim a phase deliberately left held for its successor, and the source
	// of the task/run ids that successor's prompt needs.
	carried *harness.Result
	// A session-authored successor prompt, pending from the last session's
	// handoff block (CLA-352). Non-empty means the next session is a handoff
	// respawn continuing the SAME phase, not the next configured one.
	nextPrompt string
	chain      int // consecutive handoff respawns, against handoffChainCap
	spawned    int // sessions this drain has launched, phase or respawn
	// Consecutive dead phases this TASK has produced (CLA-386). The retry budget
	// is per task, not per run: a single silent death is plausibly transient, a
	// second on the same task is not, and the same task reached fresh in a later
	// run earns its retry again. deadTask names which task the counter counts for,
	// because the retry is a fresh claiming session (the dead claim was handed
	// back): if next_task hands it a DIFFERENT task, that task has died once, not
	// twice, and must not be parked for a second death it did not have (review
	// finding). A dead phase always holds a claim, so deadTask is set whenever the
	// classification fires.
	deadRetries int
	deadTask    string
	// Consecutive held-but-empty implement exits in this drain (CLA-457). Bounded
	// at ONE retry per the 2026-08-24 decision, deliberately smaller than the
	// dead-phase ladder: the empty shape returns to the queue rather than
	// parking, so the retry is a courtesy to a plausibly-transient silent EOT,
	// not a budget (see the end.empty branch below).
	emptyRetries int
	// Whether the checkpoint that opened the CURRENT phase was evidenced by the
	// plane's record rather than a verified branch (CLA-497). Set at each seam
	// from the claim carried INTO the phase; a handoff respawn continues the
	// same phase and must keep the value, so it is never recomputed at the loop
	// head (where `carried` has already become the phase's own claim).
	branchlessCheckpoint bool
	drainStart           time.Time
	phasesRun            []string // distinct phase labels attempted, in order
	// CLA-630: one entry per session this drain spawned, in spawn order —
	// every pass through the loop below (a configured phase, a handoff
	// respawn, a dead- or empty-retry re-run), with the harness and model
	// each actually spawned with. A retry ladder INSIDE one phase is one
	// session re-run, not a new one; see drainPhase's phaseEnd.session.
	sessionsRun []fleet.Session
	seen        harness.Claim
	parkFired   bool // the per-task dead budget parked the task
	tripFired   bool // the fleet dead counter tripped and paused this target
	// finalOutcome is the CLAIM's disposition at the drain boundary: one of
	// the fleet.Outcome* constants, or "" when nothing was observed (no
	// claim state at all, or claimDisposition's nil return). The deferred
	// switch below is what makes the RECORDED outcome one of the four — it
	// maps "" to released and the drain's own verdicts (parked/dead) over
	// the disposition — so the variable itself is not always safe to hand
	// on (CLA-738). Never append prose to it: marshalReport refuses the
	// WHOLE report on a word it does not know, so free text here loses the
	// iteration record AND the presence refresh riding the same report.
	// Free-text notes belong in the log.
	finalOutcome string
	// CLA-631/CLA-651/CLA-662: the wait a RESUMED phase's sequence-end
	// signal asks the run for. A rolling limit (CLA-631) hands its reset
	// down here; the transient arm (CLA-662) hands its backoff rung down as
	// transientRung/transientKey instead. Both waits are owed AFTER the
	// sequence's handback, so both are taken holding no lease.
	limitWait *phaseLimitWait
	// CLA-662: the streak rung a transient sequence-end reached, and the
	// target it belongs to — zero when no transient end happened.
	transientRung int
	transientKey  resumedTransientKey
	// finalDead / lastProducedNothing are the FINAL attempt's dead
	// classifications (the seam's, and the tally's wider one): a death that
	// was retried past must not brand the record, and a LAST-phase death
	// never sets end.dead at all (no retry/park applies) yet the record must
	// still read `dead` — see phaseEnd.producedNothing.
	finalDead           bool
	lastProducedNothing bool
}

// recordIteration posts the one iteration record for this drain, whatever way it
// ended, and logs an incomplete review panel.
func (s *drainSeq) recordIteration() {
	d := s.d
	t := s.t
	ti := s.ti
	drainNum := s.drainNum
	// CLA-639: the LOG says the panel was incomplete, by member, whether the
	// drain then reached the fix phase or stopped before it. The reviewers
	// that DID record are what the fix phase receives; this names the ones
	// that did not, so "the panel" never reads as the full panel when it was
	// not.
	//
	// CLA-738: the note is a log line and NOTHING more. It must never be
	// folded into the iteration Outcome, which is one of the four
	// fleet.Outcome* words: the pre-CLA-738 code appended it to the
	// disposition, and marshalReport then refused the whole report on the
	// free text — dropping the iteration record and the presence refresh for
	// exactly the drains the operator most needs to see. The per-member
	// failures already ride each session's `cause`.
	if len(s.panelIncomplete) > 0 {
		log.Printf("%siteration %d: review panel INCOMPLETE: %s", labelOf(t), drainNum, strings.Join(s.panelIncomplete, "; "))
	}
	outcome := s.finalOutcome
	switch {
	case s.parkFired:
		outcome = fleet.OutcomeParked
	case s.tripFired || s.finalDead || s.lastProducedNothing || s.err != nil:
		outcome = fleet.OutcomeDead
	case outcome == "":
		// Nothing was ever observed — a launch failure with no claim state,
		// say. The record still posts (exactly one per iteration); its task
		// fields are simply empty and the outcome reads as released.
		outcome = fleet.OutcomeReleased
	}
	d.endIteration(ti, t, fleet.Iteration{
		TaskID:          s.seen.TaskID,
		TaskRef:         s.seen.Ref,
		Phases:          s.phasesRun,
		Sessions:        s.sessionsRun,
		Outcome:         outcome,
		DurationSeconds: time.Since(s.drainStart).Seconds(),
		Tokens:          s.tokens,
		// CLA-677: the drain's dollar spend, from the same accumulator the
		// budget breaker reads. It rides the iteration record so the plane
		// can show what each drain cost.
		CostUSD: s.cost,
	})
}

// seqStep is one pass through the walk: the phase it ran and how that phase
// ended. A fresh one is made per pass.
type seqStep struct {
	ph           config.Phase
	panelMember  bool
	last         bool
	terminal     bool
	isHandoff    bool
	promptBytes  int
	tag          string
	phaseResumed bool
	seamClaim    *harness.Result
	ptokens      int
	pcost        float64
	pstop        bool
	end          phaseEnd
	perr         error
}

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

// seqNext says what the walk does after a stage.
type seqNext int

const (
	seqFall     seqNext = iota // carry on to the next stage
	seqContinue                // take the next pass without advancing past this one
	seqBreak                   // end the walk
)

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
			// first member it could not afford.
			if s.spawned > 0 {
				if dim := d.budgetTrip(prior.tokens+s.tokens, prior.cost+s.cost, time.Since(prior.start)); dim != "" {
					log.Printf("iteration %d: budget reached at the %s phase boundary: %s — stopping (tokens=%d cost=$%.2f elapsed=%s)",
						drainNum, st.ph.Label(s.i), dim, prior.tokens+s.tokens, prior.cost+s.cost,
						time.Since(prior.start).Round(time.Second))
					s.stop = true
					return seqBreak
				}
			}
			if out, ok := d.runPanelInParallel(ctx, drainNum, ti, t, phases, s.i, runEnd, conc, s.carried, spend{
				start:  prior.start,
				tokens: prior.tokens + s.tokens,
				cost:   prior.cost + s.cost,
			}); ok {
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
		st.ph.Prompt = config.HandoffPreamble + s.nextPrompt + continuation
		s.nextPrompt = ""
	}

	return seqFall
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
	st.ptokens, st.pcost, st.pstop, st.end, st.perr = d.drainPhase(ctx, drainNum, ti, s.i, st.tag, st.ph, st.terminal, s.carried, t, spend{
		start:  prior.start,
		tokens: prior.tokens + s.tokens,
		cost:   prior.cost + s.cost,
	}, phases, panelState{round: s.panelRound, failed: s.panelFailed, recorded: s.panelRecorded}, phaseRunOpts{})
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
	// The retry is scoped to the FIRST phase (`i == 0`), the one that claimed
	// its own task: a retry cannot re-seed a resumed phase — invocationFor
	// only substitutes the task/run placeholders and sets ResumeClaim when the
	// predecessor claim is present, which the dead handback cleared — so
	// retrying a dead RESUMED phase would spawn a session whose brief still
	// carries the literal {{taskId}}/{{runId}} and whose handback, salvage and
	// delivery checks are all switched off. A resumed phase's dead signature
	// still vetoes its checkpoint below (the false-premise guard applies to
	// every phase); it just is not retried or parked here — the drain ends and
	// the task returns to the queue, where a fresh claiming session retries it
	// with a valid seed (review finding). The operator decision's "retry the
	// implement phase N times, then park" is exactly `i == 0` in the shipped
	// [implement, review] sequence.
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

		if !st.last && s.i == 0 && taskID != "" && taskID == s.deadTask && s.deadRetries >= perTaskDeadBound-1 {
			how := "died producing nothing"
			if st.end.toolLoop != "" {
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

		if !st.last && s.i == 0 {
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
			if st.end.toolLoop != "" {
				log.Printf("%siteration %d: the %s phase was killed (%s) — retrying it (%d of %d) before any review brief sees the task",
					labelOf(t), drainNum, st.ph.Label(s.i), st.end.toolLoop, s.deadRetries, perTaskDeadBound)
			} else {
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
