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

	i     int
	start int
	// runnerClaimed reports that the RUNNER, not a session, holds this drain's claim, so a retry must
	// re-claim before it respawns.
	runnerClaimed bool
	claimedTask   plane.TaskBrief
	taskDir       string
	prep          worktreePrep
	closeOut      bool
	// preClaimOrigin is the origin read the pre-claim worktree check made, keyed to
	// preClaimTask/preClaimCheckout. prepareStart consumes it ONCE: a retry after a long session must
	// re-derive origin rather than resume on a stale read.
	preClaimOrigin   originBranchFacts
	preClaimTask     string
	preClaimCheckout string

	tokens   int
	cost     float64
	handoffs int
	stop     bool
	err      error

	// MUST ids a verifier has already marked resolved in THIS drain. In-memory on purpose: it is a
	// courtesy layer over the durable round-prefix check, which survives a takeover on its own because
	// it reads the plane's round numbers.
	verifyResolved map[string]bool
	// Verification rounds this drain has actually closed.
	verifyRoundsRun int
	// CLA-639: the panel's members that wrote no record this round, and the round the panel's released
	// records landed in. A dead, capped or otherwise record-less member does not stop the panel: it is
	// listed here, the round still closes, and the fix phase is told the panel was incomplete.
	panelIncomplete []string
	panelRound      int
	// CLA-639 M4: whether any REQUIRED member of this panel recorded, and why each member that did NOT
	// failed. The first decides whether a failed LAST member hands the round to the fix phase (there
	// is something to apply) or ends the drain; the second rides the fix brief, so "the panel was
	// incomplete" names the members AND what happened to them.
	panelRecorded        bool
	panelFailed          map[string]panelMemberFailure
	branchlessPanelNoted bool
	// CLA-784: the branchless panel's ONE reviewer is chosen from the REQUIRED seats, never the first
	// configured member blindly.
	branchlessPanelRan    bool
	branchlessPanelChosen string
	panelFanOutOff        bool
	// The claim a phase deliberately left held for its successor, and the source of the task/run ids
	// that successor's prompt needs.
	carried    *harness.Result
	nextPrompt string
	chain      int // consecutive handoff respawns, against handoffChainCap
	spawned    int // sessions this drain has launched, phase or respawn
	// Consecutive dead phases this TASK has produced. deadTask names which task the counter counts
	// for, because the retry is a fresh claiming session (the dead claim was handed back): if
	// next_task hands it a DIFFERENT task, that task has died once, not twice, and must not be parked
	// for a second death it did not have (review finding).
	deadRetries  int
	deadTask     string
	emptyRetries int
	// Whether the checkpoint that opened the CURRENT phase was evidenced by the plane's record rather
	// than a verified branch. Set at each seam from the claim carried INTO the phase; a handoff
	// respawn continues the same phase and must keep the value, so it is never recomputed at the loop
	// head (where `carried` has already become the phase's own claim).
	branchlessCheckpoint bool
	drainStart           time.Time
	phasesRun            []string // distinct phase labels attempted, in order
	sessionsRun          []fleet.Session
	seen                 harness.Claim
	parkFired            bool // the per-task dead budget parked the task
	tripFired            bool // the fleet dead counter tripped and paused this target
	// finalOutcome is the CLAIM's disposition at the drain boundary: one of the fleet.Outcome*
	// constants, or "" when nothing was observed (no claim state at all, or claimDisposition's nil
	// return). Never append prose to it: marshalReport refuses the WHOLE report on a word it does not
	// know, so free text here loses the iteration record AND the presence refresh riding the same
	// report.
	finalOutcome  string
	limitWait     *phaseLimitWait
	transientRung int
	transientKey  resumedTransientKey
	// finalDead / lastProducedNothing are the FINAL attempt's dead classifications (the seam's, and
	// the tally's wider one): a death that was retried past must not brand the record, and a
	// LAST-phase death never sets end.dead at all (no retry/park applies) yet the record must still
	// read `dead` — see phaseEnd.producedNothing.
	finalDead           bool
	lastProducedNothing bool
}

func (s *drainSeq) recordIteration() {
	d := s.d
	t := s.t
	ti := s.ti
	drainNum := s.drainNum
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
		CostUSD:         s.cost,
	})
}

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

type seqNext int

const (
	seqFall     seqNext = iota // carry on to the next stage
	seqContinue                // take the next pass without advancing past this one
	seqBreak                   // end the walk
)
