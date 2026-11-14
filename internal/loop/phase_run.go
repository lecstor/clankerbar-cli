package loop

import (
	"os"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// phaseRun is one phase's attempt ladder: what drainPhase resolved once before
// the first spawn, and the state the ladder's attempts accumulate. The stage
// methods read and write it so each can be read without the rest of drainPhase.
type phaseRun struct {
	// Resolved once per phase, never written by an attempt.
	d            *Driver
	t            Target
	tc           *config.Config
	ti           int
	drainNum     int
	phaseIdx     int
	tag          string
	ph           config.Phase
	last         bool
	prev         *harness.Result
	prior        spend
	phases       []config.Phase
	panel        panelState
	opts         phaseRunOpts
	shape        phaseShape
	a            harness.Adapter
	spawnDir     string
	prBase       string
	briefedTask  string
	fixGrading   string
	verifyDigest string

	// The done-report contract of a runner-claimed phase: which report the phase
	// expects, and the one-shot note for a plane client that cannot write.
	kind              reportKind
	reportWriterNoted bool

	// Spend the ladder has counted so far, and the phase's disposition.
	tokens int
	cost   float64
	end    phaseEnd
	review reviewGate

	// CLA-703: the ONE retry a phase gets for an infrastructure miss. TWO arms
	// spend this single budget, deliberately: an adapter preflight failure
	// (ErrMCPNotConnected, any phase) and a review-only session that made no
	// successful clankerbar MCP call and wrote no review record (the no_mcp
	// backstop). They are not independent one-retry lanes — a preflight failure
	// followed by a no-record end closes the round INCOMPLETE with no second
	// backstop. Deliberately a separate counter from `retries`: this is not the
	// transient/limit ladder, and its bound is one regardless of max_retries.
	mcpRetried bool
	// Consecutive attempts that ended without the harness reporting any usage -
	// the spend the budget breaker above can never see (CLA-288).
	//
	// Deliberately scoped to THIS phase's attempt ladder, which is where the
	// unbounded loop lives: every re-spawn of an attempt comes back through the
	// top of this loop, and it is the ladder - not the phase sequence - that has
	// no other bound under `max_retries: 0`. A phased run therefore allows the
	// bound per phase, and a handoff respawn (which needs a clean exit and a held
	// claim, so a silently dying attempt cannot produce one) starts a fresh count
	// under drainPhases' own chain cap.
	noUsage int
	// Retries this ladder has taken on a failure the adapter judged retryable
	// WITHOUT recognising it (harness.Adapter.IsUnclassifiedTransient) - see
	// errUnclassifiedRetryLoop. Scoped per phase like noUsage, and for the same
	// reason: the ladder is what has no other bound.
	//
	// Never reset. A classified retry in between does not make the next guess any
	// better informed, and resetting would let a failure that alternates between a
	// recognised blip and an unrecognised one loop forever - which is the exact
	// shape of a provider degrading: real 5xx blips interleaved with stream drops
	// nothing has a pattern for.
	unclassified int
	// The spend of THIS phase pass, summed across the ladder's attempts
	// (CLA-691). One fleet-record entry is published per pass, and a pass that
	// retried is several paid sessions, so every attempt the ladder spawned is
	// folded in here where the entry is built.
	pass    sessionSpend
	retries int
}

// result is the drainPhase return a verdict ends the phase with.
func (r *phaseRun) result(v attemptVerdict) (int, float64, bool, phaseEnd, error) {
	if v.zeroSpend {
		return 0, 0, v.stop, r.end, v.err
	}
	if v.spendOverride {
		return v.tokens, v.cost, v.stop, r.end, v.err
	}
	return r.tokens, r.cost, v.stop, r.end, v.err
}

// attemptNext says what the ladder does after a stage.
type attemptNext int

const (
	attemptFall   attemptNext = iota // carry on to the next stage
	attemptRetry                     // go round the ladder again
	attemptReturn                    // end the phase with the verdict's stop and err
)

// attemptVerdict is a stage's decision about the ladder.
type attemptVerdict struct {
	next attemptNext
	stop bool
	err  error
	// zeroSpend ends the phase reporting no spend, as the pre-spawn refusals do.
	zeroSpend bool
	// spendOverride ends the phase reporting tokens and cost instead of the ladder's totals.
	spendOverride bool
	tokens        int
	cost          float64
}

// phaseAttempt is one pass through the ladder: the session it spawned, how that
// session ended, and the seam's verdict on it. A fresh one is made per attempt.
// res is shared by pointer with phaseEnd.claim, so it lives here rather than
// being passed by value between stages. runLost is non-nil when the renewer
// ended the session because the plane says the run no longer holds the task.
type phaseAttempt struct {
	inv                harness.Invocation
	res                harness.Result
	ierr               error
	runLost            error
	logPath            string
	logFile            *os.File
	capped             bool
	ceiling            bool
	wallclock          bool
	zeroUsage          bool
	toolLoop           string
	producedNothing    bool
	orderlyExit        bool
	recoveredExit      bool
	checkpointable     bool
	dead               bool
	bookedDead         bool
	handoff            string
	emittedHandoff     bool
	canCheckpoint      bool
	reviewable         bool
	evidenceOK         bool
	evidenceUnverified bool
	evidenceWhy        string
	evidenceTip        string
	attr               config.Attribution
	role               string
	empty              bool
	noMCPNow           bool
	retryNoMCP         bool
	verifyNeedsHold    bool
	// mapsNotes is how many NOTES turns a maps-session invocation planned (turn 1
	// included). The replies before that index go to the iteration log; the turn
	// after them is the findings turn whose report the runner reads.
	mapsNotes int
}
