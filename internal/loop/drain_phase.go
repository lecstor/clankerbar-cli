package loop

import (
	"context"
	"fmt"
	"log"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// drainPhase runs ONE phase to a clean finish, absorbing usage-limit pauses
// (supervised wait) and transient blips (exponential backoff) by re-running the
// SAME session — neither costs a drain count. Returns the tokens/cost consumed on
// a clean finish; stop=true if a STOP/cancel landed during a wait; err only on a
// genuine, non-retryable failure (or exhausted retries).
//
// held is non-nil only when this phase ended cleanly, is not the last, and left
// the task's claim open for its successor — see drainPhases, which either carries
// it to the next phase or hands it back.
//
// prev is the claim the PREVIOUS phase left held, and is what fills the task/run
// placeholders in this phase's prompt. nil for the first phase, which has no run
// to resume and claims one of its own.
//
// prior carries the run's ceilings down into the drain, because the budget breaker
// in Run only runs BETWEEN drains and every wait in here happens inside one — see
// the check at the top of the loop, and waitPastBudget.
// phaseIdx is this phase's 0-based position in the sequence, carried only so a
// phase with no name can still be NAMED in a log line — ph.Label(phaseIdx) falls
// back to "phase 2". An unphased drain passes 0 and never reaches those lines.
// ti is the target's index, for the fleet claim reflector wired into the lease
// renewer below (CLA-510).
//
// phases is the sequence as CAPTURED when the drain started — the same slice
// drainPhases iterates. It is what this phase's IDENTITY is read from (its
// built-in brief, its position in the sequence), never the live config
// (CLA-664): a RELOAD marker is applied inside waitOrStop, which runs during a
// first-phase limit wait or a transient backoff in THIS drain, so the live
// config can be a DIFFERENT sequence than the one in flight. Reading it here
// would let the two disagree — the in-flight review gated by the reloaded
// position, the "PHASE i of N" text rewritten — while drainPhases still walks
// the captured slice. The rule the capture enforces: a RELOAD takes effect at
// the next drain, never mid-sequence.
//
// panel is what the panel stage this phase follows left behind (CLA-639): the
// round its members' records landed in, zero when no panel ran or no member
// recorded. It is the fix phase's grading input (mergedFindingsDigest reads
// that round and no other); the unphased drain and a phase that does not follow
// a panel pass the zero value.
func (d *Driver) drainPhase(ctx context.Context, drainNum int, ti int, phaseIdx int, tag string, ph config.Phase, last bool, prev *harness.Result, t Target, prior spend, phases []config.Phase, panel panelState, opts phaseRunOpts) (tokens int, cost float64, stop bool, end phaseEnd, err error) {
	tc := d.targetCfg(t)
	shape := phaseShapeOf(phases, phaseIdx, prev)
	captured, requireReviewRecord := shape.captured, shape.requireReviewRecord
	isVerify := shape.isVerify
	panelMember, panelOptional, panelLast := shape.panelMember, shape.panelOptional, shape.panelLast
	// The review seam's freshness baseline, read ONCE before the attempt ladder:
	// the phase's outcome as the review found it. A handoff respawn re-enters
	// drainPhase with the same carried claim and takes a fresh baseline, so a
	// successor must still write the record its own session is responsible for.
	//
	// CLA-663: an unreadable baseline refuses the phase BEFORE it spawns. The
	// seam cannot prove a record is this trip's from a baseline it never read —
	// so the old order spawned the full review session and then threw its
	// checkpoint away, and the claim (HasWIP) lapsed into a takeover that
	// re-ran implement AND review. Refusing here pays the read — already
	// retried by readReviewBaseline — and nothing else, and leaves the claim
	// with the same disposition a seam refusal leaves it: released when it is
	// releasable, left to lapse into a takeover when it carries pushed work.
	//
	// The reviewer role is the other half of the gate's key (CLA-637): one
	// name, read from the same resolved config the brief is built from, so the
	// session records under exactly the role the seam will match.
	review := reviewGate{required: requireReviewRecord}
	end.panelMember = panelMember
	end.panelOptional = panelOptional
	end.panelLast = panelLast
	if review.required {
		var refused bool
		review, end, refused = d.openReviewGate(ctx, drainNum, t, ph, phaseIdx, phases, prev, opts, panelOptional, review, end)
		if refused {
			return 0, 0, false, end, nil
		}
	}
	// The adapter for THIS phase, resolved once: everything below — the spawn, the
	// classification of what came back, and the probe of any usage limit it hit —
	// has to be the harness that actually ran, not the run's default. Resolved
	// here rather than per attempt so a retry ladder cannot straddle two harnesses.
	a, aerr := d.adapterFor(ph)
	if aerr != nil {
		return 0, 0, false, end, fmt.Errorf("iteration %d: phase %q: %w", drainNum, ph.Label(phaseIdx), aerr)
	}
	// Where this phase's sessions run, resolved ONCE per phase (CLA-437): the
	// task repo's checkout when it resolves, the target's workdir when nothing
	// does. Before the attempt ladder, so a repo that resolves to nothing fails
	// the phase with repo_not_found before anything is spawned — a session that
	// starts in the multi-repo parent reads a sibling's rule files and, on a
	// cwd-scoped grant, hits the wall this replaces. The same directory then
	// serves every retry: the task does not change between attempts.
	//
	// The second value is the task this phase was briefed on (CLA-451): exact for
	// a resumed phase, the spawn-time queue-head peek for a fresh one. Held onto
	// here because the seam needs it AFTER the session ends — it is the one task
	// the driver can ask the plane about when the stream carried no claim state.
	// The fourth is that task's custody at spawn (CLA-476): the peek uses it to
	// refuse a hold that was already there before the session started.
	spawnDir, briefedTask, briefedRepo, spawnHold, serr := d.sessionDir(ctx, t, prev)
	if serr != nil {
		return 0, 0, false, end, fmt.Errorf("iteration %d: phase %q: %w", drainNum, ph.Label(phaseIdx), serr)
	}
	// CLA-697: a parallel panel member starts in its OWN detached checkout at
	// the reviewed tip. The repo's resolution above still ran, so the briefed
	// task/repo and the PR-base probe are the ones it would have had anyway; only
	// the directory the session runs in (and therefore the tree it can dirty)
	// differs.
	if opts.worktree != "" {
		spawnDir = opts.worktree
	}
	if opts.taskDir != "" && opts.worktree == "" {
		// The runner prepared the task's own worktree: run there, with no panel
		// note, because the brief already describes that tree.
		spawnDir = opts.taskDir
	}
	// The branch this task's repo PRs target, resolved ONCE per phase like the
	// spawn directory above, and for the same reason: it does not change between
	// attempts. Filled into the prompt's {{prBase}} placeholder by invocationFor
	// (CLA-608). Never fails here: an unresolvable probe falls back to the
	// configured integration branch, which is the text the briefs carried
	// before this feature existed.
	prBase := d.prBaseFor(ctx, t, spawnDir, briefedRepo)
	fixGrading := d.fixGradingFor(ctx, t, prev, captured, phases, phaseIdx, panel)
	verifyDigest, end, verifyRefused := d.verifyDigestFor(ctx, drainNum, t, ph, phaseIdx, prev, isVerify, end)
	if verifyRefused {
		return 0, 0, false, end, nil
	}
	r := &phaseRun{
		d: d, t: t, tc: tc, ti: ti, drainNum: drainNum, phaseIdx: phaseIdx, tag: tag, ph: ph,
		last: last, prev: prev, prior: prior, phases: phases, panel: panel, opts: opts,
		shape: shape, a: a, spawnDir: spawnDir, prBase: prBase, briefedTask: briefedTask, briefedHold: spawnHold,
		fixGrading: fixGrading, verifyDigest: verifyDigest, review: review, end: end,
		// From the CAPTURED sequence, so a handoff successor (whose ph.Prompt is
		// the predecessor's successor text) still expects its phase's report.
		kind: reportKindFor(phases, phaseIdx, opts.runner, panelMember, prev),
	}
	for {
		at := &phaseAttempt{}
		switch v := r.checkBreakers(ctx); v.next {
		case attemptRetry:
			continue
		case attemptReturn:
			return r.result(v)
		}
		switch v := r.buildInvocation(at); v.next {
		case attemptRetry:
			continue
		case attemptReturn:
			return r.result(v)
		}
		r.openAttemptLog(at)
		r.spawnSession(ctx, at)
		r.classifyEnd(at)
		// Rescue whatever the session left uncommitted, FIRST — before the handback,
		// because a successful salvage changes what the handback should do: a task
		// with a branch recorded on it is no longer safe to release (CLA-314).
		//
		// NOT for the review-only gate (CLA-split): its deliverable is the record
		// on the PLANE, not the tree, and anything uncommitted in its worktree is
		// generated output from running checks — which the brief forbids it to
		// commit. Salvaging it would (a) commit the unattributable change the
		// review's own record forbids, and (b) push a new tip AFTER the reviewed
		// tip it just recorded, so the fix phase's tip comparison would fail and
		// it would park a healthy task for a mismatch the DRIVER created. A
		// review that genuinely dies leaving source edits loses them, which is
		// exactly what "review only, change nothing" means.
		//
		// A tool-loop kill is salvaged like any other end, but not with the
		// emphatic "produced nothing" line: a clean tree there may well be work
		// the session committed and pushed before it started looping.
		//
		// A run the plane says is gone is not salvaged at all: the task is
		// someone else's now, and the plane told this run not to commit or push.
		if !requireReviewRecord && at.runLost == nil {
			d.salvageStrandedWork(ctx, t, &at.res, at.producedNothing && at.toolLoop == "", at.inv.WorkDir)
		}
		r.classifyExit(at)
		r.openPhaseEnd(at)
		if at.runLost != nil {
			return r.result(r.endRunLost(at))
		}
		r.classifyHandoff(at)
		// The session made no plane writes: its done report becomes the task's
		// records here, before the evidence gate judges them. A successor that
		// takes over from a handoff does the same for its own report.
		r.applyDoneReport(ctx, at)
		// classifyHandoff judged canCheckpoint on the claim as the session left
		// it; a park or hand-over the runner just wrote settled it, and a settled
		// task must not be carried into the next phase.
		if !at.res.Claim.Held() {
			at.canCheckpoint = false
		}
		// CLA-457: held-ness alone must never advance the seam. Since CLA-358 the
		// driver's lease renewer renews a claim for a session's whole lifetime, so
		// ANY session that claimed ends held — "still holding" is a constant, not
		// a checkpoint signal. A non-final phase reaches its checkpoint ONLY when
		// the plane's record carries the phase's exit evidence: a branch recorded
		// by this run whose tip really exists on the origin remote (the implement
		// phase in the shipped sequence). The evidence is computed even on a
		// handoff: an HONORED handoff continues the same phase regardless (its
		// successor has nothing to falsely resume), but a REFUSED handoff must be
		// re-judged as the bare exit it really is, and this is the only place the
		// verification can run.
		at.evidenceOK = true
		at.evidenceUnverified = false
		at.evidenceWhy = ""
		at.evidenceTip = ""
		if isVerify && at.res.Untrusted == "" && (at.orderlyExit || at.recoveredExit) {
			r.judgeVerifyRound(ctx, at)
		} else if at.reviewable {
			r.judgeReviewRecord(ctx, at)
		}
		r.classifyEmpty(at)
		// A handoff holds the seam OPEN even without evidence: its successor
		// continues THIS phase's job, so it is not the false-premise advance the
		// gate exists to stop. The evidence gate governs only the bare seam
		// advance to the NEXT configured phase (handoff == ""); a refused handoff
		// is downgraded to that bare judgement in drainPhases.
		//
		// CLA-638: a verification round that left an OPEN MUST holds the seam
		// even though it is the last phase, because the driver spawns another
		// fix against it. A clean round does not hold.
		at.verifyNeedsHold = isVerify && r.end.verifyChecked && r.end.verifyFailure == "" && len(r.end.verifyOpenMusts) > 0
		if ((!last || panelLast || at.handoff != "") && at.canCheckpoint && (at.handoff != "" || at.evidenceOK)) ||
			(at.verifyNeedsHold && at.canCheckpoint) {
			r.holdCheckpoint(ctx, at)
		} else {
			r.settleWithoutCheckpoint(ctx, at)
		}
		// CLA-703: a no-MCP end is an infrastructure miss, not a dead or empty
		// phase. It must not reach the dead-phase ladder (which would retry/park
		// a task whose reviewer never got its tools) or the empty-retry arm.
		// The class is carried on end.noMCP for the panel handler and the
		// drain-level booking.
		if r.end.noMCP {
			r.end.dead = false
			r.end.producedNothing = false
			r.end.empty = false
		}
		// Then check what it said it delivered. After the handback, because a dead
		// lease is time-sensitive and a git check is not; on every exit, because a
		// session that pushed nothing and died is exactly as likely to have recorded
		// a branch as one that finished cleanly.
		d.verifyDeliveries(ctx, t, at.res, at.inv.WorkDir)

		switch v := r.endOnInvokeError(ctx, at); v.next {
		case attemptRetry:
			continue
		case attemptReturn:
			return r.result(v)
		}
		switch v := r.countAttemptSpend(ctx, at); v.next {
		case attemptRetry:
			continue
		case attemptReturn:
			return r.result(v)
		}
		// CLA-703 backstop: the primary fix is the adapter's MCP preflight, but
		// a session can still end with no clankerbar call and no record (an
		// older build, a session that acted before its tools were offered, a
		// harness whose preflight does not exist). Re-run it ONCE with the same
		// brief before the panel records the member as failed. The claim was
		// deliberately kept above, so a resumed phase retries the same run; the
		// spend is already counted, so the retry is visible to the budget.
		if at.retryNoMCP {
			r.mcpRetried = true
			log.Printf("%siteration %d: the %s phase made no successful clankerbar MCP call and recorded no review record — retrying it once (no_mcp: the backlog tools were not available to the session)",
				labelOf(t), drainNum, ph.Label(phaseIdx))
			continue
		}

		switch v := r.handleUsageLimit(ctx, at); v.next {
		case attemptRetry:
			continue
		case attemptReturn:
			return r.result(v)
		}
		switch v := r.endOnExitKind(ctx, at); v.next {
		case attemptRetry:
			continue
		case attemptReturn:
			return r.result(v)
		}
		switch v := r.handleTransient(ctx, at); v.next {
		case attemptRetry:
			continue
		case attemptReturn:
			return r.result(v)
		}
		// Stopping here ends the whole run, so say WHAT was judged non-retryable.
		// Without it the operator gets "exited 1 (non-retryable)" and no way to
		// tell a genuine failure from a blip the classifier merely does not know
		// yet — which is the one thing they need in order to report the gap. The
		// text is the harness's own diagnostic scope, never the raw stream, so
		// the agent's narration is not quoted back at them (CLA-258).
		return r.tokens, r.cost, false, r.end, fmt.Errorf("iteration %d: %s %s (non-retryable) — stopping%s%s (log: %s)",
			drainNum, a.Name(), at.res.ExitString(), failureDetail(a.Diagnostic(at.res)), droppedNote(at.res.OutputDropped), at.logPath)
	}
}
