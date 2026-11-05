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
	"github.com/lecstor/clankerbar-cli/internal/fleet"
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

// classifyEnd reads the adapter's orderly-cap and tool-loop classifications off the
// result. It runs before the salvage, which needs them.
func (r *phaseRun) classifyEnd(at *phaseAttempt) {
	a := r.a
	// The orderly-cap classifications, computed here (before the salvage) so
	// the salvage can tell whether a clean tree means "produced nothing" — a
	// question the caps answer: a capped session is an orderly end with its
	// own marker, not a silent death, and an untrusted stream's finish reason
	// cannot be read at all. Purely adapter reads of `res`, so moving them up
	// changes nothing else (review finding).
	at.capped = a.TurnCapped(at.res)
	at.ceiling = a.TokenCeilingHit(at.res)
	at.wallclock = a.WallClockCapped(at.res)
	at.zeroUsage = a.ZeroUsageUnknown(at.res)
	// CLA-739: the adapter killed this session for making the same failing
	// tool call over and over. That is a DEAD session - no delivery, whatever
	// it pushed before it started looping - and it joins the dead signature
	// everywhere below: the seam veto, the dead-phase ladder, the tally, the
	// panel's failed-member path and the fleet record. Not a cap: a cap is an
	// orderly end of a session doing its job, and this one had stopped.
	at.toolLoop = at.res.ToolLoop
	at.producedNothing = at.res.Untrusted == "" && !at.capped && !at.ceiling && !at.wallclock && (deadPhase(at.res) || at.toolLoop != "")
}

// classifyExit decides, from the salvaged result, whether the session ended in a
// way that can be a checkpoint, and whether it is the dead signature.
func (r *phaseRun) classifyExit(at *phaseAttempt) {
	last := r.last
	// Hand back anything the session was still holding, BEFORE deciding what to
	// do next — every branch below either waits, retries or returns, and all of
	// them leave the lease unattended. Above the ierr check too: on every
	// adapter, an Invoke whose run failed with something other than an exit
	// status returns whatever the stream had announced by then, fully parsed,
	// alongside the error (CLA-299) — claude's drain path parses its stream as
	// it arrives; codex and opencode run their parsers to completion before
	// classifying the failure — so a claim observed there is real and must not
	// be dropped just because the process died untidily. A launch failure
	// yields a zero Result, which releases nothing.
	//
	// UNLESS this phase reached its checkpoint with another phase still to run.
	// Then the sequence is not over: the next phase resumes THIS run with
	// heartbeat(runId) instead of claiming, so handing the lease back here
	// would post the task to the queue while we are still working it, and the
	// next phase would find someone else's task or none at all. drainPhases
	// owns the handback in that case and performs it the moment the sequence
	// ends for any other reason.
	//
	// Only an ORDERLY end holds it open, and each conjunct is load-bearing: a
	// retry deliberately re-claims a half-done task with a fresh session
	// (which is why an ordinary non-zero exit still releases), and an untrusted
	// stream must not be read for claim state at all, because the settle we
	// never saw may be in the bytes that never arrived (CLA-262).
	//
	// A turn-capped end CAN be orderly — but only if something durable came out
	// of it. The salvage above is not a guarantee: it commits nothing on a
	// clean worktree, and refuses outright on a diverged remote or a tree in
	// the middle of a merge. A phase 1 that burned its turns reading and
	// planning without writing therefore leaves no branch at all, and calling
	// that a checkpoint spawns a session told "an earlier session has already
	// implemented, committed and pushed" and pointed at a branch that is not
	// there — a paid session that can only fail, or worse, move the task to
	// review over no work.
	//
	// HasWIP is exactly that signal: it is set by the only two things that make
	// a checkpoint real — the phase recording its own branch, or the salvage
	// recording one for it and the PLANE accepting the record.
	// A session the adapter ended on its wall-clock cap is the third member of
	// the same family: an orderly cut-off mid-thought, whose survivability rests
	// on the salvage exactly as a turn cap's does — so it earns a checkpoint on
	// the same terms, HasWIP and all (CLA-368). Classified by the PHASE's adapter
	// (`a`), never the run's: "did this session end on its own wall clock" is a
	// question only the harness that RAN it can answer, and under per-phase
	// harnesses (CLA-366) that harness is not necessarily d.h.
	// CLA-700: the exit status, and the ONE exception it does not decide. A
	// stream that shows the session took a provider error and then RECOVERED
	// from it — retried and carried on — ends on something other than the
	// error, and opencode 2.x exits non-zero anyway (pinned by the
	// conformance suite). Judging that exit by its status is what stranded two
	// delivered phases on 2026-10-19: the driver read the status as "failed
	// attempt" and either retried (a resumed phase ending the sequence, its
	// lease left to lapse into a takeover) or stopped, with the pushed work
	// already on the origin remote and the task held mid-sequence.
	//
	// So a recovered exit is asked the DELIVERY question instead, through the
	// same evidence gate an orderly checkpoint uses, read once below. It is
	// deliberately narrow: it needs the adapter's own marker (a stream whose
	// events include a recovered error and no terminal one), so a plain exit 1
	// from a harness with no such marker — or from a session that really did
	// stop on an error — keeps every shipped classification. A recovered exit
	// is eligible for a checkpoint on the evidence alone; when the evidence
	// says it delivered nothing, it is the existing held-but-empty shape
	// (retry once, then hand the task back), never a run-ending stop over an
	// exit status the harness itself explains.
	//
	// CLA-739: a tool-loop kill is none of these. The process usually reads
	// exit -1 from the kill, but the session can exit on its own in the
	// instant before the kill lands, and a zero exit must not turn a looping
	// session into a checkpoint - nor can its stream's recovered provider
	// errors (the incident's own log carries one).
	at.orderlyExit = at.toolLoop == "" && (at.res.ExitCode == 0 || at.capped || at.ceiling || at.wallclock)
	at.recoveredExit = !at.orderlyExit && at.toolLoop == "" && at.res.Untrusted == "" && harness.RecoveredErrors(at.res) > 0
	at.checkpointable = at.toolLoop == "" && (at.res.ExitCode == 0 || ((at.capped || at.ceiling || at.wallclock) && at.res.Claim.HasWIP) || at.recoveredExit)
	// CLA-386: a session whose final step finished with reason "unknown" and
	// left no branch recorded produced NOTHING — a dead phase, not a completed
	// one. It must not earn a checkpoint even on a zero exit, because a
	// checkpoint is what hands the next phase a brief that claims "an earlier
	// session has already implemented, committed and pushed". Two real
	// implement phases died exactly this way and the seam advanced to review
	// on the false premise; the signal has to veto the checkpoint.
	//
	// A capped session is excluded even when its stream carries "unknown": the
	// wall-clock kill can land mid-turn, but a cap is an orderly end with its
	// own marker — retrying it would re-spend against the same cap and parking
	// it would read a doing-its-job backstop as a silent death.
	at.dead = !last && at.res.Untrusted == "" && at.res.Claim.Held() && !at.capped && !at.ceiling && !at.wallclock && (deadPhase(at.res) || at.toolLoop != "")
}

// openPhaseEnd books the exit into the dead-phase tally and starts the
// attempt's phaseEnd: its panel identity, its claim and the fleet session entry.
func (r *phaseRun) openPhaseEnd(at *phaseAttempt) {
	d := r.d
	tc := r.tc
	phaseIdx := r.phaseIdx
	ph := r.ph
	prev := r.prev
	a := r.a
	panelMember := r.shape.panelMember
	panelOptional := r.shape.panelOptional
	panelLast := r.shape.panelLast
	// CLA-402: book this session into the run's dead-phase tally, per phase
	// label and harness, whatever the seam decided about retrying it. The
	// tally's dead classification deliberately drops the `!last` conjunct
	// that guards the checkpoint veto — a dead LAST phase is still a dead
	// phase, and the rate is a measurement, not a seam decision (see
	// deadtally.go). The report rides the end (CLA-639): the panel's member
	// handler books a failed member into the same tally, and must not count
	// an exit this call already booked.
	at.bookedDead = d.tallyDead(ph.Label(phaseIdx), tc.HarnessFor(ph), at.res, at.capped, at.ceiling, at.wallclock)
	r.end = phaseEnd{}
	r.end.deadBooked = at.bookedDead
	// CLA-639: the panel identity of this pass, for drainPhases' member
	// handler. Set from the CAPTURED phase (a handoff respawn replaced
	// ph.Prompt, never its identity), so a skipped member is recorded against
	// the reviewer that was meant to run.
	r.end.panelMember = panelMember
	r.end.panelOptional = panelOptional
	r.end.panelLast = panelLast
	if at.res.Claim.TaskID != "" {
		r.end.claim = &at.res
	}
	// CLA-630: the session this pass actually spawned, in exactly the words
	// the spawn line above used — the phase's label, the phase's resolved
	// harness, and the resolved model (empty when the harness default
	// applied; never guessed). Set on every attempt, so the LAST attempt's
	// values are the ones that ride: the entry describes the PASS, and a
	// retry within the ladder cannot change what the pass ran — only a
	// reload landing mid-ladder can.
	//
	// CLA-691: its spend is filled in later, at the point the loop counts the
	// attempt's spend toward the iteration (recordPassSpend), so the two can
	// never disagree about which attempts were counted. The pass sums every
	// counted attempt, because the loop counts each one as its own paid
	// session: an entry reporting only the surviving attempt's money would
	// not add up to the iteration total.
	r.end.session = &fleet.Session{
		Phase:   ph.Label(phaseIdx),
		Harness: a.Name(),
		Model:   at.inv.Model,
		RunID:   sessionRunID(at.res, prev),
	}
	// CLA-739: the tool-loop kill's cause rides the session entry on the
	// fleet report, as CLA-703's no_mcp does - it is why this session
	// produced nothing.
	if at.toolLoop != "" {
		r.end.session.Cause = at.toolLoop
	}
	r.end.toolLoop = at.toolLoop
	// The wider classification rides the end for the fleet record only — see
	// phaseEnd.producedNothing.
	r.end.producedNothing = at.producedNothing
}

// classifyHandoff detects a handoff block, refuses it where the phase does not
// chain, and works out whether the exit is a checkpoint contender.
func (r *phaseRun) classifyHandoff(at *phaseAttempt) {
	t := r.t
	drainNum := r.drainNum
	phaseIdx := r.phaseIdx
	ph := r.ph
	last := r.last
	isVerify := r.shape.isVerify
	panelMember := r.shape.panelMember
	panelLast := r.shape.panelLast
	// A handoff block ending the session's final message (CLA-352) holds the
	// task open exactly like a phase seam — even on the LAST phase, where
	// there is otherwise no successor to hold it for. The remaining guards
	// live in drainPhases; if they refuse, the deferred handback there
	// releases what this held.
	at.handoff = detectHandoff(drainNum, t, at.res)
	// A handoff the session emitted is evidence it worked, even where the
	// driver then REFUSES to chain it (a panel member, a verification round).
	// CLA-703's no-MCP backstop must not fire on such a session: it reached a
	// conclusion and wrote a successor prompt, which a session with no tools
	// does not do.
	at.emittedHandoff = at.handoff != ""
	// CLA-638: a verification round does not chain via a handoff. Its record
	// is the deliverable, and the DRIVER decides what follows (another fix, a
	// park, or nothing) from the open MUSTs the record releases; a successor
	// prompt here would have nothing to continue that this round has not
	// already written, and honoring it would defer the close/evaluation the
	// driver owes the round.
	if isVerify && at.handoff != "" {
		log.Printf("%siteration %d: the verification round emitted a handoff block; verification rounds do not chain — the driver judges the round's record and decides whether another fix runs", labelOf(t), drainNum)
		at.handoff = ""
	}
	if panelMember && at.handoff != "" {
		log.Printf("%siteration %d: the %s panel member emitted a handoff block; panel members do not chain — a successor would continue this reviewer's role, but the phase after it is the next member, and the member's record is its deliverable", labelOf(t), drainNum, ph.Label(phaseIdx))
		at.handoff = ""
	}
	// The seam contender: an orderly end still holding the task, minus the dead
	// signature. This is the gate the incident put in question — see below.
	at.canCheckpoint = at.res.Untrusted == "" && at.checkpointable && at.res.Claim.Held() && !at.dead
	// CLA-639: a panel member may be the LAST phase of the sequence when the
	// operator wrote no fix after the review, and its record gate must still
	// run — that is how the round closes and how the merged list is released.
	// `panelLast` (rather than !last) admits it, and only its review record
	// gate: a settled or non-checkpointable exit is excluded exactly as before.
	at.reviewable = (!last || panelLast) && at.canCheckpoint
}

// judgeVerifyRound is the verification seam. It runs even on the LAST phase: its
// evidence is the durable record and the open-MUST set, not a checkpoint.
func (r *phaseRun) judgeVerifyRound(ctx context.Context, at *phaseAttempt) {
	d := r.d
	t := r.t
	// The verification seam runs even on the LAST phase. Unlike every
	// other terminal phase its evidence is not a checkpoint (nothing
	// follows to resume) but the durable record plus the open-MUST set:
	// that set is what decides whether ANOTHER fix is spawned, whether
	// the sequence is done, or whether the round cap parks the task
	// (CLA-638, B12).
	r.end.verifyChecked = true
	r.end.branch, at.evidenceTip, at.evidenceOK, at.evidenceUnverified, at.evidenceWhy = d.checkpointEvidence(ctx, t, at.res, at.inv.WorkDir, &r.review)
	if r.review.state == "sealed" {
		r.end.reviewRound = r.review.round
	}
	if at.evidenceOK && r.review.required && r.review.round > 0 {
		if r.review.state == "sealed" {
			// The verifier did not release its own round; the driver does,
			// exactly as for the review gate.
			if cerr := d.closeReviewRound(ctx, t, at.res, r.review.round); cerr != nil {
				at.evidenceOK = false
				at.evidenceWhy = fmt.Sprintf("verification round %d could not be closed (%v) — its findings would stay sealed and the next fix would have nothing to apply", r.review.round, cerr)
			}
		}
		if at.evidenceOK {
			// Closed (or already released by the verifier's terminal call);
			// nothing downstream may try to close it again.
			r.end.reviewRound = 0
			r.end.verifyRound = r.review.round
			// Fail-closed, so retried like the spawn read (CLA-686 D3):
			// one blip here ends a sequence the verification round has
			// already been paid for.
			if st, read := d.readTaskStateRetry(ctx, t, at.res.Claim.TaskID, "after the verification round closed"); !read {
				at.evidenceOK = false
				at.evidenceWhy = "the task could not be read after the verification round closed, so its open MUSTs cannot be judged"
			} else {
				r.end.verifyOpenMusts = append([]string(nil), st.ReviewOpenMustIDs...)
				r.end.verifyOpenDetails = openMustDetails(st.ReviewOpenMustIDs, st.Reviews)
				r.end.verifyCounted = st.ReviewCounted
				r.end.verifyResolvedIDs = resolvedFindingIDs(st.Reviews)
				r.end.verifyStatus = st.Status
			}
		}
	} else if at.evidenceOK && r.review.required {
		// CLA-638 × CLA-670: the checkpoint rested on the legacy `## Review`
		// block (the branchless arm of the gate), so there is no durable
		// round to release and no plane open-MUST set to judge — the block
		// is this shape's record. Read the task back anyway, for the status
		// the hand-over log needs: a branchless verifier that recorded its
		// pass but left the task in_progress must be named loudly, exactly
		// as the id-based shape names it. A failed read ends the sequence
		// fail-closed, like every other unreadable seam — so it retries
		// like them too (CLA-686 D3).
		r.end.verifyRound = 0
		if st, read := d.readTaskStateRetry(ctx, t, at.res.Claim.TaskID, "after the branchless verification round"); !read {
			at.evidenceOK = false
			at.evidenceWhy = "the task could not be read after the branchless verification round, so its hand-over cannot be judged"
		} else {
			r.end.verifyStatus = st.Status
		}
	}
	if !at.evidenceOK {
		r.end.verifyFailure = at.evidenceWhy
	}
	// Fold the verified branch/tip onto the claim exactly as the generic
	// checkpoint path below does, so a fix spawned after an open-MUST
	// round resumes with the verified hand-off.
	if r.end.claim != nil && at.evidenceOK {
		if r.end.branch != "" {
			r.end.claim.Claim.Branch = r.end.branch
		} else {
			r.end.claim.Claim.Branch = ""
		}
		r.end.claim.CheckpointUnverified = at.evidenceUnverified
		r.end.claim.CheckpointTip = at.evidenceTip
	}
}

// judgeReviewRecord is the record gate for a checkpoint contender. It leaves the
// matched review round sealed, closing it only when the review stage is over.
func (r *phaseRun) judgeReviewRecord(ctx context.Context, at *phaseAttempt) {
	d := r.d
	t := r.t
	panel := r.panel
	opts := r.opts
	nextReviewShaped := r.shape.nextReviewShaped
	panelOptional := r.shape.panelOptional
	r.end.branch, at.evidenceTip, at.evidenceOK, at.evidenceUnverified, at.evidenceWhy = d.checkpointEvidence(ctx, t, at.res, at.inv.WorkDir, &r.review)
	// The matched round rides the end (CLA-637): drainPhases closes it if
	// the handoff guards refuse the respawn this detected handoff asked
	// for — the one advance path drainPhase cannot close on itself. Only a
	// still-sealed round is ours to close: a verifier closes its own in the
	// terminal call (CLA-638).
	if r.review.state == "sealed" {
		r.end.reviewRound = r.review.round
	}
	// CLA-637: the review phase's durable record is SEALED — the plane
	// withholds its findings from every reader until the driver closes
	// the round. The close is the driver's, made when the review stage
	// is OVER: not while the session hands off to a successor reviewer
	// (the successor continues this round), and not while the next
	// configured phase is another review-shaped one (a panel member,
	// whose independence depends on not reading the earlier member's
	// findings). A close that fails REFUSES the checkpoint, fail-closed:
	// the alternative is a fix phase spawned against findings the plane
	// still has sealed, which can only park the task.
	//
	// CLA-639: for a panel, the stage is the whole panel. Every member
	// but the last leaves the round sealed here; the last member's close
	// is what releases them all to the fix phase, and it happens in the
	// SAME condition below because nextReviewShaped is false only for
	// the last member (the panel's last phase, when no fix follows).
	// CLA-784: an OPTIONAL seat's record alone must not release the
	// round. If no required member has recorded, this trial seat's
	// record is kept sealed — the panel is treated as one where nobody
	// recorded, so no fix runs against a round that is no review.
	optionalOnly := panelOptional && !panel.recorded
	if at.evidenceOK && r.review.required && r.review.round > 0 && r.review.state == "sealed" && at.handoff == "" && !nextReviewShaped && !opts.panelParallel && !optionalOnly {
		if cerr := d.closeReviewRound(ctx, t, at.res, r.review.round); cerr != nil {
			at.evidenceOK = false
			r.end.reviewCloseFailed = true
			at.evidenceWhy = fmt.Sprintf("review round %d could not be closed (%v) — refusing the checkpoint, because the fix phase would be spawned against findings the plane still has sealed", r.review.round, cerr)
		}
	}
}

// classifyEmpty turns a refused evidence gate into the held-but-empty shape, and
// recognises the no-MCP infrastructure miss (retrying it once).
func (r *phaseRun) classifyEmpty(at *phaseAttempt) {
	d := r.d
	tc := r.tc
	phaseIdx := r.phaseIdx
	ph := r.ph
	a := r.a
	reviewOnly := r.shape.reviewOnly
	// A held-but-empty exit (2026-08-24): a would-be checkpoint refused by the
	// evidence gate — not by any of the older vetoes (dead, untrusted, not
	// checkpointable) — so this is a FAILED phase, not a completed one. It is
	// carried on the result in BOTH branches on purpose: for a bare exit it is
	// the classification of record, and for a handoff it is what lets
	// drainPhases downgrade the exit when the handoff is refused. An honored
	// handoff never reaches that downgrade — the phase continues first.
	//
	// CLA-639: a panel member is not special-cased here. A member whose
	// review record did not pass the gate is a FAILED member, and the panel
	// handler in drainPhases is what records it as failed and keeps the round
	// going; the flag is shared with the shipped single-review path, which
	// still refuses to advance on it.
	at.empty = at.reviewable && !at.evidenceOK
	r.end.empty = at.empty
	r.end.emptyWhy = at.evidenceWhy
	// CLA-639: whether this pass left a review RECORD behind, for the panel's
	// member handler. It is the seam gate's verdict, not the round number: a
	// member whose envelope was sealed and closed still recorded.
	r.end.reviewRecorded = at.reviewable && r.review.required && at.evidenceOK
	// CLA-703: did this review-only session ever reach the backlog? A clean
	// end with no successful clankerbar call and no review record is the
	// missing-MCP shape — the tools the brief tells it to use were not
	// offered, so "recorded nothing" is not a reviewer's verdict.
	//
	// The dead signature is EXCLUDED: a silent death is already classified
	// as dead and retried by the dead-phase ladder, and relabelling it no_mcp
	// here would hide a real death behind an infrastructure excuse. A
	// non-zero exit is excluded for the same reason — the transient/
	// non-retryable classification owns it. Caps and untrusted streams are
	// excluded because neither can be read as "the tools were missing".
	at.noMCPNow = reviewOnly && !r.end.reviewRecorded && !r.end.reviewCloseFailed && at.res.ClankerbarCalls == 0 &&
		!at.res.Claim.Settled && !at.emittedHandoff &&
		at.res.Untrusted == "" && at.res.ExitCode == 0 && !at.capped && !at.ceiling && !at.wallclock &&
		!at.producedNothing && a.Capabilities().TracksClaims
	at.retryNoMCP = at.noMCPNow && !r.mcpRetried
	if at.noMCPNow && r.mcpRetried {
		// The retry ended the same way: this is the final classification.
		r.end.noMCP = true
		// The generic dead booking (if the retry happened to look like a
		// death) and the held-but-empty booking below both belong to dead/
		// empty, not to an infrastructure miss. Unbook the first and stop the
		// second from firing; the handler books the no-MCP cell instead.
		if at.bookedDead {
			d.unbookDead(ph.Label(phaseIdx), tc.HarnessFor(ph))
		}
		r.end.deadBooked = true
		if r.end.session != nil {
			r.end.session.Cause = fleet.CauseNoMCP
		}
	}
}

// holdCheckpoint keeps the lease for the successor: it folds the verified branch
// and tip onto the carried claim and says why the seam advances.
func (r *phaseRun) holdCheckpoint(ctx context.Context, at *phaseAttempt) {
	t := r.t
	drainNum := r.drainNum
	prev := r.prev
	r.end.checkpoint = true
	r.end.handoff = at.handoff
	if r.end.claim != nil {
		// The invariant "the carried claim's branch is the VERIFIED one"
		// is held by construction, whatever form the evidence took
		// (CLA-497): the branch form folds the verified branch on, and
		// the plane-record forms clear whatever the session recorded -
		// a recorded branch that FAILED origin verification must not
		// ride into the successor's brief, which names {{branch}} as
		// verified on the origin remote (CLA-457), and the no-code
		// brief is selected exactly on an empty claim branch.
		//
		// CLA-576: an UNVERIFIED checkpoint is the one exception, and it
		// is marked rather than hidden. Its branch rides the claim (the
		// review phase needs the name to verify it) with
		// CheckpointUnverified set, so the successor's brief is the
		// unverified variant - never the verified wording, which this
		// checkpoint cannot support.
		if r.end.branch != "" {
			r.end.claim.Claim.Branch = r.end.branch
		} else {
			r.end.claim.Claim.Branch = ""
		}
		r.end.claim.CheckpointUnverified = at.evidenceUnverified
		// CLA-637: the origin tip the verified branch check read. The
		// review brief hands it to the reviewer as the revision to
		// check out and record, and the seam matches the record's
		// reviewedTip against it. Empty exactly when no tip could be
		// read (branchless or unverified), which is when no tip can be
		// demanded of the reviewer either.
		r.end.claim.CheckpointTip = at.evidenceTip
		// CLA-685: a HANDOFF is the one checkpoint this session's own
		// exit did not earn — it holds only because the successor
		// continues THIS phase, which the claim carried in as `prev`
		// opened. So when the exit evidenced NOTHING (evidenceOK false)
		// the verdict just folded above is its REFUSAL's, not a
		// checkpoint's: the record gate zeroes the unverified mark even
		// though the branch leg had established it. The successor then
		// reads carried.CheckpointUnverified as false, is handed the
		// VERIFIED continuation - "The driver verified the origin
		// remote's tip ... as {{reviewedTip}}" - for a tip this driver
		// never read, and the placeholder stays literal, since it is
		// filled only when the tip is non-empty.
		//
		// The exit's verdict about the checkpoint is empty exactly when
		// it read no tip of its own: an evidenced branch leg always
		// brings a tip (the verified form), and a non-empty tip implies
		// unverified=false — the verified continuation, whose
		// {{reviewedTip}} that tip then fills. So the opening
		// checkpoint's mark is the only true one there, and it rides
		// forward, as the panel-member block in drainPhases rides a
		// claim's resumption facts onto the claim it hands on.
		//
		// The tip is carried only for the branch it was read for — it
		// is `prev`'s, and a claim that now names another one must not
		// be handed a tip that never belonged to it. The BRANCH stays as
		// folded: it is the live truth about where the work is. An exit
		// that WAS evidenced keeps its own verdict untouched, so the
		// verified path is unaffected.
		if at.handoff != "" && !at.evidenceOK && prev != nil && r.end.claim.CheckpointTip == "" {
			r.end.claim.CheckpointUnverified = prev.CheckpointUnverified
			if r.end.claim.Claim.Branch == prev.Claim.Branch {
				r.end.claim.CheckpointTip = prev.CheckpointTip
			}
		}
	}
	if at.handoff != "" {
		log.Printf("%siteration %d: the session ended its final message with a handoff block, still holding %s — keeping the lease for its successor",
			labelOf(t), drainNum, at.res.Claim.TaskID)
	} else if r.end.branch != "" && at.evidenceUnverified {
		// The cause is the CHECK not completing, named by evidenceWhy
		// (the Unknown check's own detail) — never "the origin remote
		// could not be read" asserted as fact: delivery.Unknown also
		// covers a check that never reached the remote (a branch no
		// local repository has, an ambiguous repo, no git on PATH), and
		// the log must not contradict the detail it prints.
		log.Printf("%siteration %d: phase reached an UNVERIFIED checkpoint holding %s (branch %s recorded, but it could not be verified on the origin remote: %s) — keeping the lease for the next phase; the review phase must verify the branch itself",
			labelOf(t), drainNum, at.res.Claim.TaskID, r.end.branch, at.evidenceWhy)
	} else if r.end.branch != "" {
		log.Printf("%siteration %d: phase reached its checkpoint holding %s (branch %s verified on the origin remote) — keeping the lease for the next phase",
			labelOf(t), drainNum, at.res.Claim.TaskID, r.end.branch)
	} else {
		// The plane-record forms of exit evidence (CLA-497): a no-code or
		// already-settled task checkpoints with no branch to name.
		log.Printf("%siteration %d: phase reached its checkpoint holding %s (exit evidenced by the plane's record — task left ready or declared a no-code delivery; no branch) — keeping the lease for the next phase",
			labelOf(t), drainNum, at.res.Claim.TaskID)
	}
}

// settleWithoutCheckpoint is the exit that is not a checkpoint: it hands the claim
// back, books the empty phase, and asks the plane whether a claim the stream
// never showed is held.
func (r *phaseRun) settleWithoutCheckpoint(ctx context.Context, at *phaseAttempt) {
	d := r.d
	t := r.t
	tc := r.tc
	drainNum := r.drainNum
	phaseIdx := r.phaseIdx
	ph := r.ph
	last := r.last
	panelMember := r.shape.panelMember
	// `released` records only a handback that could actually have happened:
	// a zero Result from a failed launch releases nothing, and reporting it
	// as released would erase a predecessor's claim that is still live.
	//
	// CLA-639 M4: a PANEL MEMBER that ended without a record does NOT hand
	// its claim back here. Everything the panel handler must do about a
	// failed member needs the claim to still be live: closing the round (only
	// the LIVE holder may, and a failed close would leave the earlier
	// members' findings sealed behind a lease that is already gone) and
	// handing the next member — or the fix phase — the same run to resume.
	// A member that genuinely lost the claim is not special-cased: Held() is
	// false, so there is nothing to release either way. The handler owns the
	// handback for this shape, and takes it AFTER the close (see
	// drainPhases).
	// CLA-703: a retrying no-MCP attempt is not an ending — keep the claim
	// (the retry resumes the same run) and book nothing yet. The final
	// attempt goes through the normal handback.
	if !panelMember && !at.retryNoMCP {
		r.end.released = at.res.Claim.Held()
	}
	r.end.dead = at.dead
	if r.end.empty && !r.end.noMCP && !at.retryNoMCP {
		log.Printf("%siteration %d: the %s phase ended holding %s with no recorded branch on the origin remote — not a checkpoint (%s), so no review phase spawns",
			labelOf(t), drainNum, ph.Label(phaseIdx), claimLabel(at.res.Claim), at.evidenceWhy)
		// Book it into the dead-phase tally (CLA-457): this is the
		// "produced nothing" class the rate exists to measure, and a
		// fleet-wide silent EOT is exactly the incident in question.
		//
		// Unless the session's exit was already booked as the CLA-402
		// SILENT death: a LAST phase can wear both shapes at once (its
		// finish reason is "unknown" AND its evidence gate failed), and
		// one session may not read as two dead ones out of one run —
		// CLA-639 M4 needs this booking to be exactly one per failed
		// panel member, and the operator's rate needs it to be honest.
		//
		// And unless this is a panel member whose review close failed
		// (CLA-639 fix pass 3): a member that WROTE its record and whose
		// round the plane then refused to release produced a review — the
		// empty shape here would report it as a session that died
		// producing nothing, which is the opposite of what happened. The
		// skip is PANEL-ONLY: the plain single review keeps the untaxed
		// CLA-457 booking it has had since the base commit, so an
		// operator's dead-phase rate reads the same with no panel
		// (CLA-639 fix pass 4).
		if !at.bookedDead && !(panelMember && r.end.reviewCloseFailed) {
			d.tallyEmpty(ph.Label(phaseIdx), tc.HarnessFor(ph))
			// CLA-639 M4: this exit is booked, so the panel's member
			// handler must not book it again when it records the member
			// as failed.
			r.end.deadBooked = true
		}
	}
	// last && a clean exit is the shape CLA-384 is about: the phase ran to
	// completion, pushed, and simply did not declare. A non-zero exit reached
	// the same place by crashing, which is a different failure with its own
	// reporting, and an earlier phase reaching here did not have declaring as
	// its job at all.
	//
	// The return tells whether the claim is actually back in the ready queue
	// — the thing a bounded retry can re-claim. A release the plane refused
	// (or that never ran) leaves the lease live, so a retry session would
	// only be refused and burned.
	if !panelMember && !at.retryNoMCP {
		// A tool-loop kill is never the undeclared hand-off CLA-384
		// counts, even when the process exited 0 an instant before the
		// kill landed: it was killed failing to move the task on.
		r.end.releasedToQueue = d.releaseHeldClaim(ctx, t, at.res, last && at.res.ExitCode == 0 && at.toolLoop == "")
	}

	r.recoverUnobservedClaim(ctx, at)
}

// recoverUnobservedClaim asks the plane about the one task this driver briefed
// when the stream showed no claim at all, and treats a held task with a recorded
// branch as the phase's checkpoint if the branch verifies on the origin remote.
func (r *phaseRun) recoverUnobservedClaim(ctx context.Context, at *phaseAttempt) {
	d := r.d
	t := r.t
	drainNum := r.drainNum
	phaseIdx := r.phaseIdx
	ph := r.ph
	last := r.last
	panel := r.panel
	opts := r.opts
	briefedTask := r.briefedTask
	nextReviewShaped := r.shape.nextReviewShaped
	panelOptional := r.shape.panelOptional
	// CLA-451: the blind-spot convergence. Everything above learns about
	// claims by parsing clankerbar tool calls out of the stream; a session
	// that claimed by another channel (a raw API call with the same key,
	// a harness quirk, a renamed tool) ends checkpointable with NO observed
	// claim — and used to end the sequence here, stranding its pushed work
	// behind an expiring lease until a takeover recovered it at a reclaim's
	// cost. Before declaring the phase empty, ask the plane about the one
	// task this driver can name: the task it BRIEFED. If that task is held,
	// with a branch recorded, by some run of this project's key, treat it
	// as the phase's checkpoint exactly as an observed claim would be.
	//
	// Every conjunct below keeps an existing guard absolute:
	//
	//   - end.claim == nil && !res.Claim.Held() — an OBSERVED claim,
	//     settled or not, stays the authoritative view; the peek never
	//     overwrites or second-guesses one.
	//   - res.Untrusted == "" — a stream that could not be read whole is
	//     never read for claim state, and the plane is not asked to fill
	//     the hole either: the settle we never saw may be in the missing
	//     bytes (CLA-262).
	//   - !producedNothing — the dead-phase veto (CLA-386): a final step
	//     that finished "unknown" having produced nothing is not a
	//     checkpoint no matter what the plane says, so it is not even
	//     consulted.
	//   - the orderly-end disjunct mirrors `checkpointable` exactly, with
	//     the plane's recorded branch standing in for the HasWIP the cap
	//     path needs — a branch the PLANE confirms is stronger evidence
	//     than one read off a stream.
	//   - !last — a last phase has no successor to hold the lease for;
	//     a handoff cannot widen this either, because detectHandoff
	//     refuses on no observed claim before we get here.
	//
	// A claim the peek recovers then faces the SAME evidence gate as an
	// observed one: the recorded branch must really be on the origin
	// remote before the seam advances (CLA-457), because a plane record
	// alone cannot tell a pushed branch from a phantom one. Evidence that
	// fails keeps today's behavior — the drain ends, and the live lease
	// expires with its recorded branch intact as the takeover hand-off.
	// The peek itself is a READ of the briefed task (get_task), never a
	// claim and never a heartbeat: it cannot race a session out of work,
	// and a peek that fails or comes back not-held simply falls through
	// to the standard ending below.
	if r.end.claim == nil && !last && at.res.Untrusted == "" && !at.producedNothing &&
		!at.res.Claim.Held() && (at.res.ExitCode == 0 || at.capped || at.ceiling || at.wallclock || at.recoveredExit) {
		if claim := d.peekCheckpointClaim(ctx, t, drainNum, ph.Label(phaseIdx), briefedTask); claim != nil {
			r.end.claim = claim
			var peekTip string
			r.end.branch, peekTip, at.evidenceOK, at.evidenceUnverified, at.evidenceWhy = d.checkpointEvidence(ctx, t, *claim, at.inv.WorkDir, &r.review)
			r.end.reviewRound = r.review.round
			// The review round's close rides this path too (CLA-637): a
			// recovered review checkpoint whose round stayed sealed would
			// spawn the fix phase against findings it cannot read. No
			// handoff is possible here (detectHandoff refuses an
			// unobserved claim), so the only skip is a following
			// review-shaped phase.
			//
			// CLA-784: the same "an optional seat's record alone must not
			// release the round" rule as the observed-claim close above.
			// A trial seat that records here and is then recovered through
			// the plane's peek must leave the round sealed unless a
			// REQUIRED member has recorded; otherwise this close releases
			// a round that is no review at all, before the panel handler's
			// own "no required member recorded" check can see it.
			optionalOnly := panelOptional && !panel.recorded
			if at.evidenceOK && r.review.required && r.review.round > 0 && r.review.state == "sealed" && !nextReviewShaped && !opts.panelParallel && !optionalOnly {
				if cerr := d.closeReviewRound(ctx, t, *claim, r.review.round); cerr != nil {
					at.evidenceOK = false
					at.evidenceWhy = fmt.Sprintf("review round %d could not be closed (%v) — refusing the checkpoint, because the fix phase would be spawned against findings the plane still has sealed", r.review.round, cerr)
				}
			}
			if at.evidenceOK {
				// Fold the VERIFIED branch onto the carried claim, exactly as
				// the observed-claim checkpoint above does: a recovered claim
				// carries the plane's recorded branch and checkpointEvidence
				// re-verified it, so the two already agree today — but the
				// invariant "the carried claim's branch is the verified one"
				// is what verifiedBranch and the takeover hand-off depend on,
				// so hold it by construction rather than by coincidence of a
				// single-source read. CLA-576: an unverified recovery rides
				// the same way, marked so its brief is the unverified variant.
				r.end.claim.Claim.Branch = r.end.branch
				r.end.claim.CheckpointUnverified = at.evidenceUnverified
				r.end.claim.CheckpointTip = peekTip
				r.end.checkpoint = true
				if at.evidenceUnverified {
					log.Printf("%siteration %d: %s ended with no claim observed in its stream, but the plane holds %s (run %s) with branch %s recorded — it could not be verified on the origin remote (%s), so this is an UNVERIFIED checkpoint: keeping the lease for the next phase; the review phase must verify the branch itself",
						labelOf(t), drainNum, ph.Label(phaseIdx), claim.Claim.TaskID, claim.Claim.RunID, r.end.branch, at.evidenceWhy)
				} else {
					log.Printf("%siteration %d: %s ended with no claim observed in its stream, but the plane holds %s (run %s) with branch %s verified on the origin remote — treating it as the phase checkpoint and keeping the lease for the next phase",
						labelOf(t), drainNum, ph.Label(phaseIdx), claim.Claim.TaskID, claim.Claim.RunID, r.end.branch)
				}
			} else {
				log.Printf("%siteration %d: the plane holds %s (run %s) with branch %q recorded, but that branch could not be verified on the origin remote (%s) — not treating it as a checkpoint; the lease expires with its hand-off intact",
					labelOf(t), drainNum, claim.Claim.TaskID, claim.Claim.RunID, r.end.branch, at.evidenceWhy)
			}
		}
	}
}
