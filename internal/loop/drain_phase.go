package loop

import (
	"context"
	"fmt"
	"log"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// drainPhase runs ONE phase to a clean finish, absorbing usage-limit pauses (supervised wait) and
// transient blips (exponential backoff) by re-running the SAME session — neither costs a drain
// count. Returns the tokens/cost consumed on a clean finish; stop=true if a STOP/cancel landed
// during a wait; err only on a genuine, non-retryable failure (or exhausted retries).
func (d *Driver) drainPhase(ctx context.Context, drainNum int, ti int, phaseIdx int, tag string, ph config.Phase, last bool, prev *harness.Result, t Target, prior spend, phases []config.Phase, panel panelState, opts phaseRunOpts) (tokens int, cost float64, stop bool, end phaseEnd, err error) {
	tc := d.targetCfg(t)
	shape := phaseShapeOf(phases, phaseIdx, prev)
	captured, requireReviewRecord := shape.captured, shape.requireReviewRecord
	isVerify := shape.isVerify
	panelMember, panelOptional, panelLast := shape.panelMember, shape.panelOptional, shape.panelLast
	// The review seam's freshness baseline, read ONCE before the attempt ladder: the phase's outcome
	// as the review found it.
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
	// The adapter for THIS phase, resolved once: everything below — the spawn, the classification of
	// what came back, and the probe of any usage limit it hit — has to be the harness that actually
	// ran, not the run's default.
	a, aerr := d.adapterFor(ph)
	if aerr != nil {
		return 0, 0, false, end, fmt.Errorf("iteration %d: phase %q: %w", drainNum, ph.Label(phaseIdx), aerr)
	}
	// Where this phase's sessions run, resolved ONCE per phase: the task repo's checkout when it
	// resolves, the target's workdir when nothing does.
	spawnDir, briefedRepo, serr := d.sessionDir(ctx, t, prev)
	if serr != nil {
		return 0, 0, false, end, fmt.Errorf("iteration %d: phase %q: %w", drainNum, ph.Label(phaseIdx), serr)
	}
	// CLA-697: a parallel panel member starts in its OWN detached checkout at the reviewed tip.
	if opts.worktree != "" {
		spawnDir = opts.worktree
	}
	if opts.taskDir != "" && opts.worktree == "" {
		// The runner prepared the task's own worktree: run there, with no panel note, because the brief
		// already describes that tree.
		spawnDir = opts.taskDir
	}
	// The branch this task's repo PRs target, resolved ONCE per phase like the spawn directory above,
	// and for the same reason: it does not change between attempts.
	prBase := d.prBaseFor(ctx, t, spawnDir, briefedRepo)
	fixGrading := d.fixGradingFor(ctx, t, prev, captured, phases, phaseIdx, panel)
	verifyDigest, end, verifyRefused := d.verifyDigestFor(ctx, drainNum, t, ph, phaseIdx, prev, isVerify, end)
	if verifyRefused {
		return 0, 0, false, end, nil
	}
	r := &phaseRun{
		d: d, t: t, tc: tc, ti: ti, drainNum: drainNum, phaseIdx: phaseIdx, tag: tag, ph: ph,
		last: last, prev: prev, prior: prior, phases: phases, panel: panel, opts: opts,
		shape: shape, a: a, spawnDir: spawnDir, prBase: prBase,
		fixGrading: fixGrading, verifyDigest: verifyDigest, review: review, end: end,
		// From the CAPTURED sequence, so a handoff successor (whose ph.Prompt is the predecessor's
		// successor text) still expects its phase's report.
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
		// Rescue whatever the session left uncommitted, FIRST — before the handback, because a
		// successful salvage changes what the handback should do: a task with a branch recorded on it is
		// no longer safe to release.
		if !requireReviewRecord && at.runLost == nil {
			d.salvageStrandedWork(ctx, t, &at.res, at.producedNothing && at.toolLoop == "", at.inv.WorkDir)
		}
		r.classifyExit(at)
		r.openPhaseEnd(at)
		if at.runLost != nil {
			return r.result(r.endRunLost(at))
		}
		r.classifyHandoff(at)
		// The session made no plane writes: its done report becomes the task's records here, before the
		// evidence gate judges them.
		r.applyDoneReport(ctx, at)
		// classifyHandoff judged canCheckpoint on the claim as the session left it; a park or hand-over
		// the runner just wrote settled it, and a settled task must not be carried into the next phase.
		if !at.res.Claim.Held() {
			at.canCheckpoint = false
		}
		// CLA-457: held-ness alone must never advance the seam.
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
		// A handoff holds the seam OPEN even without evidence: its successor continues THIS phase's job,
		// so it is not the false-premise advance the gate exists to stop.
		at.verifyNeedsHold = isVerify && r.end.verifyChecked && r.end.verifyFailure == "" && len(r.end.verifyOpenMusts) > 0
		if ((!last || panelLast || at.handoff != "") && at.canCheckpoint && (at.handoff != "" || at.evidenceOK)) ||
			(at.verifyNeedsHold && at.canCheckpoint) {
			r.holdCheckpoint(ctx, at)
		} else {
			r.settleWithoutCheckpoint(ctx, at)
		}
		// CLA-703: a no-MCP end is an infrastructure miss, not a dead or empty phase.
		if r.end.noMCP {
			r.end.dead = false
			r.end.producedNothing = false
			r.end.empty = false
		}
		// Then check what it said it delivered. After the handback, because a dead lease is
		// time-sensitive and a git check is not; on every exit, because a session that pushed nothing and
		// died is exactly as likely to have recorded a branch as one that finished cleanly.
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
		// CLA-703 backstop: the primary fix is the adapter's MCP preflight, but a session can still end
		// with no clankerbar call and no record (an older build, a session that acted before its tools
		// were offered, a harness whose preflight does not exist).
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
		return r.tokens, r.cost, false, r.end, fmt.Errorf("iteration %d: %s %s (non-retryable) — stopping%s%s (log: %s)",
			drainNum, a.Name(), at.res.ExitString(), failureDetail(a.Diagnostic(at.res)), droppedNote(at.res.OutputDropped), at.logPath)
	}
}
