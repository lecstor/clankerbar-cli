package loop

import (
	"log"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
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
	// start is the index the walk begins at. 0 until the start-phase wiring
	// chooses one; a retry is scoped to the phase the walk STARTED at.
	start int
	// runnerClaimed reports that the RUNNER, not a session, holds this drain's
	// claim, so a retry must re-claim before it respawns.
	runnerClaimed bool
	// claimedTask is the brief behind a runner-claimed drain's claim, kept for
	// the start-phase wiring.
	claimedTask plane.TaskBrief
	// taskDir is the runner-prepared worktree every phase of this drain runs in;
	// empty when the start is branchless or the drain is not runner-claimed.
	taskDir string
	// prep is what preparing taskDir did (merge result, conflicts), for the brief.
	prep worktreePrep
	// closeOut marks a close-out claim, which takes the close-out brief.
	closeOut bool
	// preClaimOrigin is the origin read the pre-claim worktree check made, keyed
	// to preClaimTask/preClaimCheckout. prepareStart consumes it ONCE: a retry
	// after a long session must re-derive origin rather than resume on a stale
	// read. An unknown (zero) originBranchFacts means there is none to reuse.
	preClaimOrigin   originBranchFacts
	preClaimTask     string
	preClaimCheckout string

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

// seqNext says what the walk does after a stage.
type seqNext int

const (
	seqFall     seqNext = iota // carry on to the next stage
	seqContinue                // take the next pass without advancing past this one
	seqBreak                   // end the walk
)
