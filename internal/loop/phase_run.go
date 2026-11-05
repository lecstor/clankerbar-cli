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
