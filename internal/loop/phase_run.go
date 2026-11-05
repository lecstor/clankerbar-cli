package loop

import (
	"context"
	"fmt"
	"log"
	"time"

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

// The budget breaker's view of this decision, beside the run's own totals
// (CLA-697 review finding 1.review-1.1). For a PARALLEL panel member the
// siblings' published spend is part of it: the members share the run's
// ceilings, and a check that measured only the shared `prior` plus this
// member's own local spend could spawn an attempt the panel's AGGREGATE had
// already made unaffordable. Nil handle — every other caller — returns
// exactly the arithmetic the ladder has always done.
func (r *phaseRun) sofar() (int, float64) {
	tokensSoFar, costSoFar := r.prior.tokens+r.tokens, r.prior.cost+r.cost
	if r.opts.panelSpend != nil {
		peersTokens, peersCost := r.opts.panelSpend.peers()
		tokensSoFar += peersTokens
		costSoFar += peersCost
	}
	return tokensSoFar, costSoFar
}

// Every attempt's spend reaches the ledger the moment the ladder counts it,
// so a sibling's next pre-spawn check sees a member that has already paid.
func (r *phaseRun) publishSpend() {
	if r.opts.panelSpend != nil {
		r.opts.panelSpend.publish(r.tokens, r.cost)
	}
}

// result is the drainPhase return a verdict ends the phase with.
func (r *phaseRun) result(v attemptVerdict) (int, float64, bool, phaseEnd, error) {
	if v.zeroSpend {
		return 0, 0, v.stop, r.end, v.err
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
}

// phaseAttempt is one pass through the ladder: the session it spawned, how that
// session ended, and the seam's verdict on it. A fresh one is made per attempt.
// res is shared by pointer with phaseEnd.claim, so it lives here rather than
// being passed by value between stages.
type phaseAttempt struct {
	inv                harness.Invocation
	res                harness.Result
	ierr               error
	logPath            string
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
	empty              bool
	noMCPNow           bool
	retryNoMCP         bool
	verifyNeedsHold    bool
}

// checkBreakers is the two bounds that stop the ladder spawning another paid
// session: the run-wide budget breaker, and the zero-spend attempt bound.
func (r *phaseRun) checkBreakers(ctx context.Context) attemptVerdict {
	d := r.d
	drainNum := r.drainNum
	prior := r.prior
	a := r.a
	// The breaker, from inside the drain. Every path that loops back here has
	// just waited — a supervised wait on a usage limit, an exponential backoff
	// on a transient blip — and each of those waits used to be unbounded by
	// anything but the wall clock, which is only one of three dials and the one
	// an operator is least likely to have set. A drain that re-spawns a paid
	// session on a loop is the expensive failure this stops, whatever put it
	// there (CLA-258).
	spentTokens, spentCost := r.sofar()
	if dim := d.budgetTrip(spentTokens, spentCost, time.Since(prior.start)); dim != "" {
		log.Printf("iteration %d: budget reached mid-drain: %s — stopping (tokens=%d cost=$%.2f elapsed=%s)",
			drainNum, dim, spentTokens, spentCost, time.Since(prior.start).Round(time.Second))
		return attemptVerdict{next: attemptReturn, stop: true}
	}

	// The breaker's blind spot, bounded. The check above can only stop spend it
	// was told about; an attempt that died before its harness reported usage
	// added nothing to either accumulator, so a ladder of those attempts leaves
	// a token or cost ceiling permanently unreachable and only max_wall_clock -
	// the dial an operator is least likely to have set as a spend bound - can
	// end the run. Reached only on an attempt re-spawn (the first pass has
	// counted nothing), so a single silent attempt that ends the phase some
	// other way never trips it.
	if bound := d.cfg.ZeroSpendAttemptBound(); r.noUsage >= bound {
		return attemptVerdict{next: attemptReturn, err: fmt.Errorf(
			"iteration %d: %w: %d consecutive attempts died before %s reported any usage, so nothing the budget breaker reads was ever fed - the sessions are failing early rather than working (see the attempt logs in the state dir; raise max_zero_spend_attempts to allow more)",
			drainNum, errZeroSpendLoop, r.noUsage, a.Name())}
	}

	return attemptVerdict{}
}

// buildInvocation resolves this attempt's invocation and prepends the driver-read
// text (grading, verification digest, worktree note) to its prompt. A refusal
// ends the phase before anything is spawned.
func (r *phaseRun) buildInvocation(at *phaseAttempt) attemptVerdict {
	d := r.d
	t := r.t
	drainNum := r.drainNum
	phaseIdx := r.phaseIdx
	ph := r.ph
	prev := r.prev
	phases := r.phases
	opts := r.opts
	spawnDir := r.spawnDir
	prBase := r.prBase
	fixGrading := r.fixGrading
	verifyDigest := r.verifyDigest
	// Each attempt streams live to the terminal and to its own logfile. The name
	// carries the drain number and attempt counter as well as the timestamp: two
	// attempts in the same second (a sub-second backoff) would otherwise share a
	// name, and a name already taken is REFUSED rather than truncated.
	//
	// The random tail is not decoration. This log carries the session's whole
	// output — prompts, tool arguments, tool output, occasionally a token — and
	// the rest of the name is derivable to within a second by the session that
	// produced it. Unguessable names mean a session that CAN write to the state
	// dir (an operator who pointed state_dir back inside the workdir) still
	// cannot pre-plant a decoy at the name we are about to use. statedir.Create
	// refuses an existing path either way; this stops it turning into a denial
	// of the log itself.
	at.inv, at.ierr = d.invocationFor(t, phaseIdx, ph, prev, spawnDir, prBase, phases)
	if at.ierr != nil {
		// The session env refused to resolve (a failing or empty-output
		// fromCommand, an unreadable @path file — the error names the
		// variable). REFUSING THE SPAWN is the fail-closed contract: no
		// session starts without its declared env (CLA-462). This is not a
		// retryable failure — nothing was spawned and the cause is config,
		// not a blip — so it fails the phase like a bad spawnDir does.
		return attemptVerdict{next: attemptReturn, zeroSpend: true, err: fmt.Errorf("iteration %d: phase %q: refusing to spawn: %w", drainNum, ph.Label(phaseIdx), at.ierr)}
	}
	// The graded list is PREPENDED after placeholder substitution, never
	// run through the one-pass replacer: it is driver-read text from the
	// plane, and a finding quoting `{{model}}` must stay a quote.
	if fixGrading != "" {
		at.inv.Prompt = fixGrading + at.inv.Prompt
	}
	// The verification digest is the same contract for the verify phase
	// (CLA-638): plane-read ids and findings, prepended verbatim.
	if verifyDigest != "" {
		at.inv.Prompt = verifyDigest + at.inv.Prompt
	}
	// CLA-697: a parallel member's own checkout is a fact only the driver
	// knows — the panel brief still says "work in the worktree for branch X",
	// which is the shared one. State it first, so the session's cwd and its
	// brief agree.
	if opts.worktree != "" {
		at.inv.Prompt = panelWorktreeNote + at.inv.Prompt
	}
	return attemptVerdict{}
}
