package loop

import (
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// panelState is what a phase following a review panel needs to know about it: the round the panel's
// members' records landed in, and — for the members that recorded NOTHING — why, keyed by
// reviewer role. Zero when the phase does not follow a panel, or when the panel never got a record
// into a round — which is exactly when there is no merged list to build.
type panelState struct {
	round  int
	failed map[string]panelMemberFailure
	// recorded reports that a REQUIRED member has already recorded in this panel.
	recorded bool
}

type panelMemberFailure struct {
	reason   string
	optional bool
}

type phaseEnd struct {
	claim *harness.Result

	released bool

	runLost bool

	checkpoint bool

	dead bool

	producedNothing bool

	deadBooked bool

	// session is the harness session this pass spawned, for the fleet record's sessions list: the
	// phase label, the resolved harness and the resolved model the spawn line named. Only the fleet
	// record reads it.
	session *fleet.Session

	// empty reports a held-but-empty exit — the 2026-08-24 fleet-incident shape: the phase ended
	// cleanly still holding the task with NO durable, origin-reachable branch evidence on the plane
	// record. Deliberately distinct from dead, which keys on the "unknown" finish reason — the
	// incident's sessions finished with reason "stop", so the dead signature alone did not catch them.
	empty bool

	emptyWhy string

	windowHeld bool

	windowWhy string

	branch string

	releasedToQueue bool

	handoff string

	panelMember bool
	panelLast   bool
	// panelOptional reports that this member is an OPTIONAL (trial) seat of the review panel.
	panelOptional  bool
	reviewRecorded bool

	noMCP bool

	toolLoop string

	reportResumeFailed string

	reviewCloseFailed bool

	reviewRound int

	verifyChecked bool
	// verifyFailure is the reason a verification seam could not be evaluated (no record this run
	// wrote, an unreadable task, a round that would not close). Non-empty means the driver must park
	// rather than hand over or loop: an unverifiable round cannot be the gate.
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
	// unreadable, invalid, or a write the plane refused. The seam gates append it to their own "no
	// record matched" reason, because a phase that wrote nothing looks identical either way.
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
// Named phases whose name is "review" are review phases; custom prompts with no name are never
// treated as review for escalation.
func isReviewPhase(ph config.Phase) bool {
	return ph.Name == config.ReviewPhaseName
}

func isVerifyPhase(ph config.Phase) bool {
	return config.VerifyShaped(ph.Prompt)
}
