package loop

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/logfile"
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

// openAttemptLog opens this attempt's iteration log, points the invocation's
// console at it (and the terminal when echo is on), and logs the spawn line.
func (r *phaseRun) openAttemptLog(at *phaseAttempt) {
	d := r.d
	t := r.t
	drainNum := r.drainNum
	phaseIdx := r.phaseIdx
	tag := r.tag
	ph := r.ph
	a := r.a
	logName := fmt.Sprintf("iteration-%s-d%d%s-a%d-%s.log",
		time.Now().Format("20060102-150405"), drainNum, tag, r.retries, randomTail())
	f, ferr := d.state.Create(logName)
	at.logPath = filepath.Join(d.state.Path(), logName)
	// The console is where the adapter streams the session: the file, and
	// NOT the terminal unless the operator asked for the echo (CLA-701).
	//
	// The stream is the whole harness transcript — prompts, tool arguments,
	// tool output — and once it also went to the daemon's stderr it left only
	// the last few minutes of one session in the operator's scrollback, with
	// none of the daemon's own lines beside it. Those lines (spawn, exit,
	// classification, why the run ended) are the record of what the driver
	// DECIDED, and nothing re-reads a terminal; the file keeps the stream in
	// full, so echoing it only duplicates it over the top of the useful part.
	//
	// `echo_sessions: true` restores the old live echo for an operator
	// watching one session. What stays live either way is the daemon's own
	// logging (which also lands in daemon.log). Nothing harness-side is
	// echoed separately: claude's stderr is part of its stream and lands in
	// the iteration log with the rest of it, and a session worth watching
	// live is why the opt-in exists.
	var echo io.Writer
	if d.cfg.EchoSessions {
		echo = d.echoOut
		if echo == nil {
			echo = os.Stderr
		}
	}
	// sink names where the stream actually LANDS, on the spawn line the
	// operator joins the daemon log and the transcript by: whenever the
	// iteration log could not be opened (ferr != nil) there is no such file
	// to send a reader to, so the sink says so and names the fallback
	// instead of a path that was never created.
	sink := "log: " + at.logPath
	switch {
	case ferr == nil && echo != nil:
		// Both sinks get the stream, INDEPENDENTLY: a terminal that has gone
		// away must not cost the iteration log the chunk it would have
		// received — io.MultiWriter stops at the first failing writer, which
		// is the order that fails worst here (echo first, file second).
		at.inv.Console = logfile.Tee(echo, f)
	case ferr == nil:
		at.inv.Console = f
	case echo != nil:
		// No log file: keep the live view rather than swallow the session.
		at.inv.Console = echo
		sink = fmt.Sprintf("log: none (could not open %s: %v - the session's stream goes to the terminal)", at.logPath, ferr)
	default:
		// No file and no echo: the stream has nowhere to go. Say so once —
		// the daemon's own lines still land in daemon.log — rather than
		// failing a paid session over its transcript.
		at.inv.Console = io.Discard
		sink = fmt.Sprintf("log: none (could not open %s: %v - the session's stream is discarded; the daemon log still records its own lines)", at.logPath, ferr)
	}
	// The spawn line stamps what the session was actually spawned WITH:
	// phase and the resolved model id inv.Model carries (ModelForPhase's
	// output, never the tier alias). A token/cost line in an old log is then
	// attributable to the settings of THAT day rather than inferred from
	// whatever the config file says now (CLA-345). An empty model= is itself
	// evidence: no explicit model was configured at spawn time, and the
	// harness ran its own default.
	if r.retries == 0 {
		log.Printf("iteration %d %s- spawning %s (phase=%s model=%s %s)", drainNum, labelOf(t), a.Name(), ph.Label(phaseIdx), at.inv.Model, sink)
	} else {
		log.Printf("iteration %d %s- retry %d, spawning %s (phase=%s model=%s %s)", drainNum, labelOf(t), r.retries, a.Name(), ph.Label(phaseIdx), at.inv.Model, sink)
	}

	at.logFile = f
}

// spawnSession launches this attempt's session under lease renewal, records the
// routes it was denied, and closes the iteration log.
func (r *phaseRun) spawnSession(ctx context.Context, at *phaseAttempt) {
	d := r.d
	t := r.t
	ti := r.ti
	drainNum := r.drainNum
	opts := r.opts
	a := r.a
	// CLA-358: while THIS child runs, renew whatever task-lease it holds.
	// Started before Invoke so a resumed phase's seeded claim renews from
	// the first tick; stopped the moment Invoke returns, which is what makes
	// renewal pause whenever no child process is running - between attempts,
	// across supervised waits, after exit. Inside the window the adapter may
	// run its own resurrection machinery (opencode CLA-406): those processes
	// continue the SAME session on the SAME claim, so renewal spans them.
	renewer := d.startLeaseRenewal(ctx, ti, t, &at.inv, fmt.Sprintf("%siteration %d: ", labelOf(t), drainNum))
	// The launch boundary itself (CLA-738 review 3.2): a panel member releases
	// its successor here, so nothing this member did before its adapter call
	// (worktree, brief, budget check) can be overtaken by a later member's.
	if opts.onSessionLaunch != nil {
		opts.onSessionLaunch()
	}
	at.res, at.ierr = a.Invoke(ctx, at.inv)
	renewer.stop()
	// CLA-782: record the routes this session was denied and write each SWITCH
	// to both logs before the iteration log is closed. Runs before every
	// classification below, because a transient retry must already start on the
	// route that worked.
	d.noteRouteFallbacks(t, drainNum, at.inv, at.res)
	if at.logFile != nil {
		_ = at.logFile.Close()
	}
}
