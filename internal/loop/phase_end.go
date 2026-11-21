package loop

import (
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

type panelState struct {
	round int
	// failed maps a member's role to the reason it recorded nothing, so the fix brief names the
	// incomplete members AND what happened to them, not only that the list is short.
	failed map[string]panelMemberFailure
	// recorded reports that a REQUIRED member has already recorded in this panel.
	recorded bool
}

type panelMemberFailure struct {
	reason   string
	optional bool
}

// phaseEnd is how a phase reports what it left behind, so drainPhases can decide whether to run the
// next one and what is still owed a handback. The three fields are deliberately independent.
type phaseEnd struct {
	claim *harness.Result

	released bool

	runLost bool

	checkpoint bool

	dead bool

	producedNothing bool

	deadBooked bool

	// session is the harness session this pass spawned, for the fleet record's sessions list: the
	// phase label, the resolved harness and the resolved model the spawn line named.
	session *fleet.Session

	empty bool

	emptyWhy string

	// windowHeld reports that the spawn was REFUSED by a pricing window that opened between the
	// boundary check and the spawn, so no session ran and nothing was spent.
	windowHeld bool

	windowWhy string

	branch string

	releasedToQueue bool

	handoff string

	// panelMember reports that this pass was one reviewer of a review panel, and panelLast that it was
	// the panel's last member.
	panelMember bool
	panelLast   bool
	// panelOptional reports that this member is an OPTIONAL (trial) seat of the review panel.
	panelOptional  bool
	reviewRecorded bool

	noMCP bool

	toolLoop string

	reportResumeFailed string

	// reviewCloseFailed reports that this pass's review seam gate passed far enough to find a SEALED
	// round, but the driver's own close of that round failed.
	reviewCloseFailed bool

	// reviewRound is the review round the seam gate MATCHED this pass, zero when the phase is not
	// review-shaped or no record matched.
	reviewRound int

	verifyChecked bool
	// verifyFailure is the reason a verification seam could not be evaluated (no record this run
	// wrote, an unreadable task, a round that would not close).
	verifyFailure string
	verifyRound   int
	// verifyOpenMusts is every open MUST on a RELEASED record, by plane finding id, as of AFTER this
	// verification round closed.
	verifyOpenMusts []string
	// verifyOpenDetails is the same list with each finding's summary appended, for the park's
	// question: "carrying the records" means the operator reads WHAT is open, not only its id.
	verifyOpenDetails []string
	verifyCounted     int
	// verifyResolvedIDs are the MUST ids on released records a verifier has already marked resolved
	// (fixed/filed/refuted).
	verifyResolvedIDs []string
	verifyStatus      string

	// reportProblem is why a runner-claimed session's done report became no records: missing,
	// unreadable, invalid, or a write the plane refused.
	reportProblem string
	doneReport    *config.DoneReport

	// sequenceEnd, when non-nil, reports that this RESUMED phase ended the sequence without completing
	// it, and the RUN must not end with it.
	sequenceEnd *phaseSequenceEnd
}

// phaseSequenceEnd is the signal the sequenceEnd field carries: a resumed phase could not continue
// its sequence, but the RUN must.
type phaseSequenceEnd struct {
	limitWait *phaseLimitWait
}

type phaseLimitWait struct {
	lim harness.Limit   // the rolling limit, for the reset and its reason
	ph  config.Phase    // the phase whose harness was turned away
	a   harness.Adapter // that harness, so the probes run on the account that is limited
}

func deadPhase(res harness.Result) bool {
	r, _ := res.Raw[harness.FinishReasonKey].(string)
	return r == harness.FinishReasonUnknown && !res.Claim.HasWIP
}

// isReviewPhase reports whether a phase is a review-phase (the gate where escalation applies).
func isReviewPhase(ph config.Phase) bool {
	return ph.Name == config.ReviewPhaseName
}

func isVerifyPhase(ph config.Phase) bool {
	return config.VerifyShaped(ph.Prompt)
}
