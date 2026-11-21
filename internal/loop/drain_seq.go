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

	// i is the index of the phase being run.
	i int
	// start is the index the walk begins at.
	start int
	// runnerClaimed reports that the RUNNER, not a session, holds this drain's claim, so a retry must
	// re-claim before it respawns.
	runnerClaimed bool
	// claimedTask is the brief behind a runner-claimed drain's claim, kept for the start-phase wiring.
	claimedTask plane.TaskBrief
	taskDir     string
	// prep is what preparing taskDir did (merge result, conflicts), for the brief.
	prep worktreePrep
	// closeOut marks a close-out claim, which takes the close-out brief.
	closeOut bool
	// preClaimOrigin is the origin read the pre-claim worktree check made, keyed to
	// preClaimTask/preClaimCheckout.
	preClaimOrigin   originBranchFacts
	preClaimTask     string
	preClaimCheckout string

	// What the drain returns.
	tokens   int
	cost     float64
	handoffs int
	stop     bool
	err      error

	// MUST ids a verifier has already marked resolved in THIS drain.
	verifyResolved map[string]bool
	// Verification rounds this drain has actually closed.
	verifyRoundsRun int
	// CLA-639: the panel's members that wrote no record this round, and the round the panel's released
	// records landed in.
	panelIncomplete []string
	panelRound      int
	// CLA-639 M4: whether any REQUIRED member of this panel recorded, and why each member that did NOT
	// failed.
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
	carried *harness.Result
	// A session-authored successor prompt, pending from the last session's handoff block.
	nextPrompt string
	chain      int // consecutive handoff respawns, against handoffChainCap
	spawned    int // sessions this drain has launched, phase or respawn
	// Consecutive dead phases this TASK has produced.
	deadRetries int
	deadTask    string
	// Consecutive held-but-empty implement exits in this drain.
	emptyRetries int
	// Whether the checkpoint that opened the CURRENT phase was evidenced by the plane's record rather
	// than a verified branch.
	branchlessCheckpoint bool
	drainStart           time.Time
	phasesRun            []string // distinct phase labels attempted, in order
	sessionsRun          []fleet.Session
	seen                 harness.Claim
	parkFired            bool // the per-task dead budget parked the task
	tripFired            bool // the fleet dead counter tripped and paused this target
	// finalOutcome is the CLAIM's disposition at the drain boundary: one of the fleet.Outcome*
	// constants, or "" when nothing was observed (no claim state at all, or claimDisposition's nil
	// return).
	finalOutcome string
	// CLA-631/CLA-651/CLA-662: the wait a RESUMED phase's sequence-end signal asks the run for.
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

// recordIteration posts the one iteration record for this drain, whatever way it ended, and logs an
// incomplete review panel.
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

// seqStep is one pass through the walk: the phase it ran and how that phase ended.
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
