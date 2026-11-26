package loop

import (
	"context"
	"errors"
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

// checkBreakers is the two bounds that stop the ladder spawning another paid session: the budget in
// force for the target, and the zero-spend attempt bound.
func (r *phaseRun) checkBreakers(ctx context.Context) attemptVerdict {
	d := r.d
	drainNum := r.drainNum
	prior := r.prior
	a := r.a
	// The breaker, from inside the drain. Every path that loops back here has just waited — a
	// supervised wait on a usage limit, an exponential backoff on a transient blip — and each of
	// those waits used to be unbounded by anything but the wall clock, which is only one of three
	// dials and the one an operator is least likely to have set.
	if v := d.budgetStop(r.ti, time.Since(prior.start)); v.dim != "" {
		log.Printf("iteration %d: budget reached mid-drain: %s — ending this drain (tokens=%d cost=$%.2f elapsed=%s)",
			drainNum, v.dim, v.tokens, v.cost, v.elapsed.Round(time.Second))
		return attemptVerdict{next: attemptReturn, stop: true}
	}

	// The breaker's blind spot, bounded. The check above can only stop spend it was told about; an
	// attempt that died before its harness reported usage added nothing to either accumulator, so a
	// ladder of those attempts leaves a token or cost ceiling permanently unreachable and only
	// max_wall_clock - the dial an operator is least likely to have set as a spend bound - can end the
	// run.
	if bound := d.cfg.ZeroSpendAttemptBound(); r.noUsage >= bound {
		return attemptVerdict{next: attemptReturn, err: fmt.Errorf(
			"iteration %d: %w: %d consecutive attempts died before %s reported any usage, so nothing the budget breaker reads was ever fed - the sessions are failing early rather than working (see the attempt logs in the state dir; raise max_zero_spend_attempts to allow more)",
			drainNum, errZeroSpendLoop, r.noUsage, a.Name())}
	}

	return attemptVerdict{}
}

// buildInvocation resolves this attempt's invocation and prepends the driver-read text (grading,
// verification digest, worktree note) to its prompt.
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
	at.inv, at.ierr = d.invocationFor(t, phaseIdx, ph, prev, spawnDir, prBase, phases, opts.runner, opts.baseSHA)
	if at.ierr != nil {
		// CLA-947: every route is inside a pricing window. windowHeld marks it as a scheduling condition
		// rather than a failed phase (review 1.review-1.6): the claim goes back through the ordinary
		// no-checkpoint path and the next drain's boundary holds the spawn until the window ends.
		var blocked *routeWindowBlocked
		if errors.As(at.ierr, &blocked) {
			log.Printf("%siteration %d: %v — the boundary will hold the next spawn until it ends", labelOf(t), drainNum, blocked)
			r.end.windowHeld = true
			r.end.windowWhy = blocked.Error()
			return attemptVerdict{next: attemptReturn, zeroSpend: true}
		}
		// The session env refused to resolve (a failing or empty-output fromCommand, an unreadable @path
		// file — the error names the variable).
		return attemptVerdict{next: attemptReturn, zeroSpend: true, err: fmt.Errorf("iteration %d: phase %q: refusing to spawn: %w", drainNum, ph.Label(phaseIdx), at.ierr)}
	}
	at.attr, at.role = d.doneAttribution(t, ph, phases, phaseIdx, at.inv)
	at.inv.CheckReport = r.reportCheck()
	// The graded list is PREPENDED after placeholder substitution, never run through the one-pass
	// replacer: it is driver-read text from the plane, and a finding quoting `{{model}}` must stay a
	// quote.
	if fixGrading != "" {
		at.inv.Prompt = fixGrading + at.inv.Prompt
	}
	if verifyDigest != "" {
		at.inv.Prompt = verifyDigest + at.inv.Prompt
	}
	// CLA-697: a parallel member's own checkout is a fact only the driver knows — the panel brief
	// still says "work in the worktree for branch X", which is the shared one.
	if opts.worktree != "" {
		at.inv.Prompt = panelWorktreeNote + at.inv.Prompt
	}
	if opts.runner != nil {
		at.inv.Prompt = r.runnerContextBlock(at) + "\n\n---\n\n" + at.inv.Prompt
	}
	plan, perr := r.mapsSessionPlanFor(at)
	if perr != nil {
		return attemptVerdict{next: attemptReturn, zeroSpend: true, err: fmt.Errorf("iteration %d: phase %q: %w", drainNum, ph.Label(phaseIdx), perr)}
	}
	r.applyMapsSession(at, plan)
	return attemptVerdict{}
}

// runnerContextBlock fills the context block from the attempt's resolved invocation: the worktree
// is where the session actually starts, and the tip is the checkpoint's when a predecessor recorded
// one, else what the runner's own prep left in the tree.
func (r *phaseRun) runnerContextBlock(at *phaseAttempt) string {
	tip := ""
	if r.prev != nil {
		tip = r.prev.CheckpointTip
	}
	if tip == "" {
		tip = r.opts.runner.prep.Tip
	}
	branch := ""
	if r.prev != nil {
		branch = verifiedBranch(r.prev)
	}
	if branch == "" {
		branch = r.opts.runner.task.Branch
	}
	return config.TaskContextBlock(runnerTaskContext(r.opts.runner, r.phases, r.phaseIdx, at.inv.WorkDir, branch, r.prBase, tip))
}

// openAttemptLog opens this attempt's iteration log, points the invocation's console at it (and the
// terminal when echo is on), and logs the spawn line.
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
	// The console is where the adapter streams the session: the file, and NOT the terminal unless the
	// operator asked for the echo. The stream is the whole harness transcript — prompts, tool
	// arguments, tool output — and once it also went to the daemon's stderr it left only the last
	// few minutes of one session in the operator's scrollback, with none of the daemon's own lines
	// beside it.
	var echo io.Writer
	if d.cfg.EchoSessions {
		echo = d.echoOut
		if echo == nil {
			echo = os.Stderr
		}
	}
	// sink names where the stream actually LANDS, on the spawn line the operator joins the daemon log
	// and the transcript by: whenever the iteration log could not be opened (ferr != nil) there is no
	// such file to send a reader to, so the sink says so and names the fallback instead of a path that
	// was never created.
	sink := "log: " + at.logPath
	switch {
	case ferr == nil && echo != nil:
		// Both sinks get the stream, INDEPENDENTLY: a terminal that has gone away must not cost the
		// iteration log the chunk it would have received — io.MultiWriter stops at the first failing
		// writer, which is the order that fails worst here (echo first, file second).
		at.inv.Console = logfile.Tee(echo, f)
	case ferr == nil:
		at.inv.Console = f
	case echo != nil:
		// No log file: keep the live view rather than swallow the session.
		at.inv.Console = echo
		sink = fmt.Sprintf("log: none (could not open %s: %v - the session's stream goes to the terminal)", at.logPath, ferr)
	default:
		// No file and no echo: the stream has nowhere to go. Say so once — the daemon's own lines still
		// land in daemon.log — rather than failing a paid session over its transcript.
		at.inv.Console = io.Discard
		sink = fmt.Sprintf("log: none (could not open %s: %v - the session's stream is discarded; the daemon log still records its own lines)", at.logPath, ferr)
	}
	// The spawn line stamps what the session was actually spawned WITH: phase and the resolved model
	// id inv.Model carries (ModelForPhase's output, never the tier alias).
	if r.retries == 0 {
		log.Printf("iteration %d %s- spawning %s (phase=%s model=%s %s)", drainNum, labelOf(t), a.Name(), ph.Label(phaseIdx), at.inv.Model, sink)
	} else {
		log.Printf("iteration %d %s- retry %d, spawning %s (phase=%s model=%s %s)", drainNum, labelOf(t), r.retries, a.Name(), ph.Label(phaseIdx), at.inv.Model, sink)
	}

	at.logFile = f
}

// spawnSession launches this attempt's session under lease renewal, records the routes it was
// denied, and closes the iteration log.
func (r *phaseRun) spawnSession(ctx context.Context, at *phaseAttempt) {
	d := r.d
	t := r.t
	ti := r.ti
	drainNum := r.drainNum
	opts := r.opts
	a := r.a
	// CLA-358: while THIS child runs, renew whatever task-lease it holds.
	sctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	loss := &leaseLoss{}
	renewer := d.startLeaseRenewal(ctx, ti, t, &at.inv, fmt.Sprintf("%siteration %d: ", labelOf(t), drainNum), func(err error) {
		loss.record(err)
		cancel(err)
	})
	if opts.onSessionLaunch != nil {
		opts.onSessionLaunch()
	}
	at.res, at.ierr = a.Invoke(sctx, at.inv)
	r.writeMapsNotes(at)
	joined := renewer.stop()
	// Only a session this run ended counts as lost: a cancelled parent is the operator stopping the
	// whole run.
	if ctx.Err() == nil {
		at.runLost = renewalCause(sctx, joined, loss.cause())
	}
	d.noteRouteFallbacks(t, drainNum, at.inv, at.res)
	if at.logFile != nil {
		_ = at.logFile.Close()
	}
}

// classifyEnd reads the adapter's orderly-cap and tool-loop classifications off the result.
func (r *phaseRun) classifyEnd(at *phaseAttempt) {
	a := r.a
	// The orderly-cap classifications, computed here (before the salvage) so the salvage can tell
	// whether a clean tree means "produced nothing" — a question the caps answer: a capped session
	// is an orderly end with its own marker, not a silent death, and an untrusted stream's finish
	// reason cannot be read at all.
	at.capped = a.TurnCapped(at.res)
	at.ceiling = a.TokenCeilingHit(at.res)
	at.wallclock = a.WallClockCapped(at.res)
	at.zeroUsage = a.ZeroUsageUnknown(at.res)
	// CLA-739: the adapter killed this session for making the same failing tool call over and over.
	at.toolLoop = at.res.ToolLoop
	if at.toolLoop == "" && at.res.IdleKilled != "" {
		// A silence kill is a driver kill like a tool loop: dead, never a checkpoint, retried by the dead-phase ladder.
		at.toolLoop = "idle: " + at.res.IdleKilled
	}
	at.producedNothing = at.res.Untrusted == "" && !at.capped && !at.ceiling && !at.wallclock && (deadPhase(at.res) || at.toolLoop != "")
}

// classifyExit decides, from the salvaged result, whether the session ended in a way that can be a
// checkpoint, and whether it is the dead signature.
func (r *phaseRun) classifyExit(at *phaseAttempt) {
	last := r.last
	at.orderlyExit = at.toolLoop == "" && (at.res.ExitCode == 0 || at.capped || at.ceiling || at.wallclock)
	at.recoveredExit = !at.orderlyExit && at.toolLoop == "" && at.res.Untrusted == "" && harness.RecoveredErrors(at.res) > 0
	at.checkpointable = at.toolLoop == "" && (at.res.ExitCode == 0 || ((at.capped || at.ceiling || at.wallclock) && at.res.Claim.HasWIP) || at.recoveredExit)
	// CLA-1033: a resume-only failure is retryable on the LAST phase too — the work was delivered and only
	// the report was lost, and the claim was kept for exactly this retry. The genuine-death signatures stay
	// non-retryable there, where there is no claim-staying retry to offer.
	at.dead = at.res.Untrusted == "" && at.res.Claim.Held() && !at.capped && !at.ceiling && !at.wallclock &&
		((!last && (deadPhase(at.res) || at.toolLoop != "")) || at.res.ReportResumeFailed != "")
}

// openPhaseEnd books the exit into the dead-phase tally and starts the attempt's phaseEnd: its
// panel identity, its claim and the fleet session entry.
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
	at.bookedDead = d.tallyDead(ph.Label(phaseIdx), tc.HarnessFor(ph), at.res, at.capped, at.ceiling, at.wallclock)
	r.end = phaseEnd{}
	r.end.deadBooked = at.bookedDead
	// CLA-639: the panel identity of this pass, for drainPhases' member handler.
	r.end.panelMember = panelMember
	r.end.panelOptional = panelOptional
	r.end.panelLast = panelLast
	if at.res.Claim.TaskID != "" {
		r.end.claim = &at.res
	}
	// Kept even when the session observed no claim: a runner-claimed session asks about the task
	// the RUNNER holds, which only the driver can match.
	r.end.blockingAsk = at.res.BlockingAsk
	// CLA-630: the session this pass actually spawned, in exactly the words the spawn line above used
	// — the phase's label, the phase's resolved harness, and the resolved model (empty when the
	// harness default applied; never guessed).
	r.end.session = &fleet.Session{
		Phase:   ph.Label(phaseIdx),
		Harness: a.Name(),
		Model:   at.inv.Model,
		RunID:   sessionRunID(at.res, prev),
	}
	if at.toolLoop != "" {
		r.end.session.Cause = at.toolLoop
	}
	r.end.toolLoop = at.toolLoop
	// The wider classification rides the end for the fleet record only — see
	// phaseEnd.producedNothing.
	r.end.producedNothing = at.producedNothing
}

// classifyHandoff detects a handoff block, refuses it where the phase does not chain, and works out
// whether the exit is a checkpoint contender.
func (r *phaseRun) classifyHandoff(at *phaseAttempt) {
	t := r.t
	drainNum := r.drainNum
	phaseIdx := r.phaseIdx
	ph := r.ph
	last := r.last
	isVerify := r.shape.isVerify
	panelMember := r.shape.panelMember
	panelLast := r.shape.panelLast
	// A handoff block ending the session's final message holds the task open exactly like a phase seam
	// — even on the LAST phase, where there is otherwise no successor to hold it for.
	at.handoff = detectHandoff(drainNum, t, at.res)
	// A handoff the session emitted is evidence it worked, even where the driver then REFUSES to chain
	// it (a panel member, a verification round).
	at.emittedHandoff = at.handoff != ""
	// CLA-638: a verification round does not chain via a handoff.
	if isVerify && at.handoff != "" {
		log.Printf("%siteration %d: the verification round emitted a handoff block; verification rounds do not chain — the driver judges the round's record and decides whether another fix runs", labelOf(t), drainNum)
		at.handoff = ""
	}
	if panelMember && at.handoff != "" {
		log.Printf("%siteration %d: the %s panel member emitted a handoff block; panel members do not chain — a successor would continue this reviewer's role, but the phase after it is the next member, and the member's record is its deliverable", labelOf(t), drainNum, ph.Label(phaseIdx))
		at.handoff = ""
	}
	// The seam contender: an orderly end still holding the task, minus the dead signature.
	at.canCheckpoint = at.res.Untrusted == "" && at.checkpointable && at.res.Claim.Held() && !at.dead
	// CLA-639: a panel member may be the LAST phase of the sequence when the operator wrote no fix
	// after the review, and its record gate must still run — that is how the round closes and how
	// the merged list is released.
	at.reviewable = (!last || panelLast) && at.canCheckpoint
}
