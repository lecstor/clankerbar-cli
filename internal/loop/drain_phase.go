package loop

import (
	"context"
	"fmt"
	"log"
	"time"

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
	spawnDir, briefedTask, briefedRepo, serr := d.sessionDir(ctx, t, prev)
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
		shape: shape, a: a, spawnDir: spawnDir, prBase: prBase, briefedTask: briefedTask,
		fixGrading: fixGrading, verifyDigest: verifyDigest, review: review, end: end,
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
		if !requireReviewRecord {
			d.salvageStrandedWork(ctx, t, &at.res, at.producedNothing && at.toolLoop == "", at.inv.WorkDir)
		}
		r.classifyExit(at)
		r.openPhaseEnd(at)
		r.classifyHandoff(at)
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
		// A session whose stream could not be read whole (CLA-262). Everything below
		// this line reads a figure parsed out of that stream, so counting the spend,
		// classifying the exit or retrying on the strength of it are three ways to
		// make a confident decision on data with a hole in it.
		if at.res.Untrusted != "" {
			// held is nil here by construction — an untrusted stream never holds a
			// phase open — so the sequence ends and nothing is carried forward.
			//
			// CLA-691: no spend figures were recorded on this pass's session entry
			// either (recordPassSpend runs only where the loop counts an attempt),
			// so the record says "unknown" instead of publishing a FLOOR from a
			// stream with a hole in it as if it were a report.
			utokens, ucost, ustop, uerr := d.endUntrustedDrain(t, drainNum, tc.HarnessFor(ph), at.logPath, at.res, r.tokens, r.cost)
			return utokens, ucost, ustop, r.end, uerr
		}

		// Count THIS attempt's spend toward the budget breaker regardless of how it
		// ends — usage-limit, transient, stop, or clean. A failed/retried attempt
		// still burned tokens, and a "leave headroom" breaker must err toward seeing
		// real spend, not under-counting it. Each attempt is a distinct session, so
		// summing per attempt (and returning the accumulator, not the final res)
		// counts every session exactly once.
		r.tokens += at.res.Tokens
		r.cost += at.res.CostUSD
		r.publishSpend()
		// The pass's own session entry takes the same figures, in the same breath
		// (CLA-691): every attempt the loop counts is money the record attributes
		// to the pass, and none it refuses is.
		recordPassSpend(r.end.session, a, &r.pass, &at.res)
		d.charge(tc.HarnessFor(ph), at.res.Tokens, at.res.CostUSD)

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

		// A usage limit. A rolling-window subscription cap is waited out and the
		// session re-run; a hard budget/credit exhaustion (Stop) has no reset to
		// poll for, so the run stops cleanly and the operator resumes it once
		// they've topped up.
		if lim := a.DetectLimit(at.res); lim.Limited {
			if lim.Stop {
				// CLA-784: an OPTIONAL panel seat is a trial reviewer. A hard
				// stop on its account is recorded against that seat — never the
				// run. Today's stop path would end the daemon for a reviewer we
				// were only evaluating, throwing away the required members' round
				// with it. The optional member's failure rides the usual
				// per-member bookkeeping: named INCOMPLETE with this reason, and
				// the panel moves on.
				if panelOptional {
					log.Printf("%siteration %d: the %s member is an OPTIONAL trial seat — its usage stop is recorded against that seat, not the run (log: %s)",
						labelOf(t), drainNum, ph.Label(phaseIdx), at.logPath)
					r.end.empty = true
					r.end.emptyWhy = "stopped on a usage limit (no reset to wait for) before recording"
					return r.tokens, r.cost, false, r.end, nil
				}
				log.Printf("iteration %d stopped: %s — no reset to wait for, stopping (resume once resolved) (log: %s)",
					drainNum, limitReason(lim), at.logPath)
				d.writeLimitStop(lim)
				return r.tokens, r.cost, true, r.end, nil
			}
			log.Printf("iteration %d hit a usage limit: %s", drainNum, limitReason(lim))
			// CLA-784: an OPTIONAL seat never waits and never ends the sequence,
			// resumed or not. The sequence-end signal is the member handler's
			// reason; for a trial seat it is inert there (no run-level wait, no
			// shared-claim handback).
			if panelOptional {
				log.Printf("%siteration %d: the OPTIONAL %s seat hit a usage limit (%s) — recording it against that seat and carrying on rather than waiting out a reset on a trial reviewer (log: %s)",
					labelOf(t), drainNum, ph.Label(phaseIdx), limitReason(lim), at.logPath)
				r.end.sequenceEnd = &phaseSequenceEnd{limitWait: &phaseLimitWait{lim: lim, ph: ph, a: a}}
				return r.tokens, r.cost, false, r.end, nil
			}
			// A RESUMED phase must not wait one out (CLA-631). The seam's handback
			// has already ended the claim: `release` puts a no-work claim straight
			// back in the queue, and `expireLease` ends a pushed-work claim's lease
			// immediately, leaving the task an immediate takeover — so a supervised
			// wait of hours would only run against a task this sequence no longer
			// holds, with a runId in its prompt that no longer means anything. End
			// the sequence instead. The RUN, though, must not end with it — exiting
			// the daemon left a whole fleet idle until an operator noticed — so the
			// wait is handed to drainPhases, which owes it only once the sequence's
			// handback has run and no lease is held.
			if prev != nil {
				log.Printf("iteration %d: a resumed phase hit a usage limit — ending the sequence rather than waiting out a reset on a task the handback has already returned; it comes back as an immediate takeover, and the run stays up to wait for the reset instead of exiting (log: %s)",
					drainNum, at.logPath)
				r.end.sequenceEnd = &phaseSequenceEnd{limitWait: &phaseLimitWait{lim: lim, ph: ph, a: a}}
				return r.tokens, r.cost, false, r.end, nil
			}
			// Whatever this attempt held open is not held open any more: every path
			// below either waits or retries with a FRESH session, which re-claims.
			// Pre-phases the handback ran on every attempt, so this could not arise.
			r.end = d.releaseCarriedEnd(ctx, t, r.end)
			// Waiting out a reset that lands past the wall-clock ceiling buys
			// nothing: the budget check runs between drains, so the loop would
			// sleep through the window, run one session on the freshly reset quota
			// and stop on the very next check — having spent the night waiting for
			// headroom it then declines to use. Stop now and say when the quota
			// returns, so the operator can start a fresh run against it.
			remaining, bounded := tc.Budget.Remaining(time.Since(prior.start))
			if until, over := waitPastBudget(lim.ResetAt, remaining, bounded); over {
				log.Printf("iteration %d: the limit resets %s, in %s — more than the %s left of this run's ceiling; stopping now rather than waiting, so start a fresh run after the reset (log: %s)",
					drainNum, formatReset(until), time.Until(until).Round(time.Minute), remaining.Round(time.Minute), at.logPath)
				return r.tokens, r.cost, true, r.end, nil
			}
			// The wait's own probe spend lands in the SAME accumulator as the
			// sessions', so it reaches the breaker at the top of this loop, the one in
			// Run between drains, and the iteration's cost line — rather than a
			// separate figure nothing is measured against (CLA-287).
			// Probed on the harness that hit the limit, in that harness's own
			// session shape: a usage limit is a property of one provider's account,
			// so probing the run's default harness would answer a question nobody
			// asked, and spend on it. The panel AGGREGATE rides `sofar` too (see
			// above): a sibling's counted spend counts against the same ceiling
			// this wait's probes are about to spend under.
			waitTokens, waitCost := r.sofar()
			ptokens, pcost, pstop := d.supervisedWait(ctx, lim, a, ph, t, spend{
				start:  prior.start,
				tokens: waitTokens,
				cost:   waitCost,
			})
			r.tokens += ptokens
			r.cost += pcost
			r.publishSpend()
			if pstop {
				return r.tokens, r.cost, true, r.end, nil
			}
			continue
		}

		// Below the limit branch, so a usage-limit attempt is counted NEITHER way.
		// A session the subscription cap turned away often reports nothing at all
		// - under claude the notice can arrive on stderr with no stream, and under
		// codex as free text - so counting it as silence would end the run on the
		// ordinary overnight quota wait, and blame a harness that is starting
		// perfectly well. It is not a reset either: the cap is a known cause with
		// its own breakers (the budget check inside supervisedWait, waitPastBudget,
		// and the probes' own spend), and clearing the count on it would let a
		// limit between silences hide the loop this bounds.
		//
		// Everything from here down is an attempt that RAN and ended on its own
		// terms, which is where "did it tell us anything?" is the right question -
		// a different question from what it told us, since a report of zero is
		// still a report (CLA-288). Counted here, enforced at the top of the loop,
		// so the bound fires on the decision to spawn again rather than on this
		// attempt's exit.
		if at.res.UsageReported {
			r.noUsage = 0
		} else {
			r.noUsage++
		}

		// CLA-739: killed for repeating a failing tool call. Ahead of the exit
		// status, because the session can exit 0 in the instant before the kill
		// lands. Never retried here as a transient (a fresh session would loop
		// the same way, and each call re-sends the whole context), and never a
		// run failure (the kill did its job). The dead classification above has
		// already decided what follows: drainPhases' dead-phase ladder, or the
		// panel's failed-member path.
		if at.toolLoop != "" {
			// claude reports dollars only on its terminal result event, which a
			// killed stream never reaches - so say the cost was not captured
			// rather than print $0 as if it were measured (the CLA-343 ceiling
			// branch's rule). opencode sums each step's cost up to the kill.
			spent := fmt.Sprintf("tokens=%d cost=$%.4f", r.tokens, r.cost)
			if !at.res.CostReported {
				spent = fmt.Sprintf("tokens=%d; cost not captured - killed mid-stream", r.tokens)
			}
			log.Printf("%siteration %d: the %s phase was killed by the driver: %s — classified dead, no delivery (%s) (log: %s)",
				labelOf(t), drainNum, ph.Label(phaseIdx), at.toolLoop, spent, at.logPath)
			return r.tokens, r.cost, false, r.end, nil
		}

		if at.res.ExitCode == 0 {
			// A clean exit is the shape the CLA-398 quiet death wears, so the
			// marker gets named here rather than inferred downstream: a final
			// step_finish with reason "unknown" and all-zero usage used to read
			// as "iteration done (tokens=0 cost=$0.00)" — indistinguishable from
			// a cheap clean run, which is exactly what made these deaths
			// invisible. The adapter's own marker is the only thing that can
			// tell them apart.
			if at.zeroUsage {
				log.Printf("iteration %d: the session ended with the zero-usage-unknown signature (final step reason %q, all-zero usage) — a quiet death, not a clean run (log: %s)",
					drainNum, harness.FinishReasonUnknown, at.logPath)
			} else if finished := harness.RouteFinished(at.res); finished != "" && finished != at.inv.Model {
				// CLA-782: the spawn line names the route the session STARTED on;
				// this line adds the route that finished it when a usage limit moved
				// it mid-session.
				log.Printf("iteration %d done (tokens=%d cost=$%.4f; finished on %s) (log: %s)", drainNum, r.tokens, r.cost, finished, at.logPath)
			} else {
				log.Printf("iteration %d done (tokens=%d cost=$%.4f) (log: %s)", drainNum, r.tokens, r.cost, at.logPath)
			}
			return r.tokens, r.cost, false, r.end, nil
		}

		// CLA-700: a session that recovered from its provider errors is not judged
		// by its exit status — the stream shows the harness took the error, retried
		// and carried the session on, and 2.x exits non-zero regardless (pinned by
		// the conformance suite). Everything this exit could have meant was settled
		// above by the DELIVERY question: either the phase checkpoints (its work is
		// verified on the origin remote, or its review record appended) and the
		// sequence advances, or it delivered nothing and ends the drain as the
		// held-but-empty shape. Falling through to the classifiers here would undo
		// both — the transient ladder retries a session whose work is already
		// pushed, and the non-retryable stop ends the run over an exit status that
		// names no failure.
		//
		// Deliberately HERE, after the untrusted and spend-accounting arms and
		// after DetectLimit: a usage limit or budget stop is still honoured (its
		// scan reads stderr and a TERMINAL error event, so a recovered blip cannot
		// trip it while a genuine account stop still can), and the cap branches
		// below are unreachable from here by construction — a capped exit is
		// `orderlyExit`, so it never sets recoveredExit.
		if at.recoveredExit {
			log.Printf("iteration %d: the session's stream shows %d provider error event(s) it recovered from and no terminal one (exit %s) — judging this phase on its delivery, not on the exit status (log: %s)",
				drainNum, harness.RecoveredErrors(at.res), at.res.ExitString(), at.logPath)
			return r.tokens, r.cost, false, r.end, nil
		}

		// A turn-capped end is this PHASE ending, never the run failing. It exits
		// non-zero and matches neither the limit scan nor the transient one, so
		// without this it would fall through to the non-retryable branch below and
		// stop the daemon — the backstop killing what it was added to protect.
		// Not retried either: the cap would fire again at the same place.
		if at.capped {
			log.Printf("iteration %d: the session hit its turn cap (tokens=%d cost=$%.4f) — ending this phase; anything uncommitted was salvaged above (log: %s)",
				drainNum, r.tokens, r.cost, at.logPath)
			return r.tokens, r.cost, false, r.end, nil
		}

		// A session the ADAPTER killed for crossing its per-session token ceiling
		// is the same shape as a turn cap: an orderly end with the salvage's
		// problem to handle, never a failure and never a retry. Retrying would
		// re-spend against the same runaway ceiling; failing would stop the run
		// over a kill that did its job (CLA-343).
		//
		// The cost is reported as unmeasured, never as a number: the result event
		// — the only carrier of total_cost_usd — is deliberately not parsed on a
		// killed stream, so res.CostUSD is 0 and printing it would under-report
		// the drain's spend as exactly nothing. The tokens are the honest figure.
		if at.ceiling {
			log.Printf("iteration %d: the session crossed its per-session token ceiling (tokens=%d; cost not captured — killed mid-stream) — ending this phase; anything uncommitted was salvaged above (log: %s)",
				drainNum, r.tokens, at.logPath)
			return r.tokens, r.cost, false, r.end, nil
		}

		// A session the ADAPTER ended for outliving its wall-clock cap is the same
		// shape again: the phase ends, and neither a retry nor a failure is right -
		// a retry would spend the same hours over again and reach the same deadline,
		// and failing would stop the run over a cap doing its job (CLA-368).
		//
		// Spend IS reported here, unlike the token-ceiling branch: opencode's usage
		// arrives per step_finish and is summed all the way to the kill, so the
		// figures are the honest cost of the session up to the moment it ended.
		//
		// Since CLA-365 opencode has BOTH HonoursSessionWallClock and
		// TracksClaims (Capabilities.TracksClaims: true), so a capped opencode
		// session holding its claim IS salvaged - the res.Claim.Held() arm above
		// is the one that fires today. The else branch (nothing salvaged) applies
		// only if an adapter enforcing this cap ever lacks claim observation
		// again; it is kept defensive, not descriptive of the present. The claim-held
		// wording was earned by CLA-365, not reserved for a future adapter.
		if at.wallclock {
			if at.res.Claim.Held() {
				log.Printf("iteration %d: the session outlived its wall-clock cap (tokens=%d cost=$%.4f) — ending this phase; anything uncommitted was salvaged above (log: %s)",
					drainNum, r.tokens, r.cost, at.logPath)
			} else {
				log.Printf("iteration %d: the session outlived its wall-clock cap (tokens=%d cost=$%.4f) — ending this phase. NOTHING was salvaged: this harness does not observe the session's task claim, so whatever the session left uncommitted is still in the worktree (log: %s)",
					drainNum, r.tokens, r.cost, at.logPath)
			}
			return r.tokens, r.cost, false, r.end, nil
		}

		// Non-zero exit, not the usage cap: a transient server/network blip backs
		// off and retries; anything else is a genuine failure and stops.
		if a.IsTransient(at.res) {
			// A RESUMED phase does not retry, for the same reason it does not wait
			// out a usage limit — and this path is the more dangerous of the two,
			// because MaxRetries defaults to 0, meaning never give up. Seeding the
			// resumed claim made the seam's handback live for phase 2, so a blip
			// here hands the task back mid-sequence at once: `release` for a claim
			// with nothing pushed, `expireLease` for pushed work, which ends that
			// claim's lease immediately and leaves the task an immediate takeover.
			// A retry would then be handed the very same heartbeat(runId) brief
			// against a run that no longer holds anything. End the sequence. The
			// RUN, though, must not end with it (CLA-651) — exiting the daemon on
			// one provider blip left the fleet idle until an operator noticed, the
			// same failure CLA-631 fixed for limits — so the sequence-end signal
			// goes to drainPhases, which owes it a wait (CLA-662): there is no
			// reset for a blip, so the run waits the target's growing backoff rung
			// after the handback and then polls again. The rung matters precisely
			// when this phase's harness is NOT the one the queue drains on: an
			// opencode implement start can never see the claude blip, so without
			// the run-level wait the same implement-and-lose loop would repeat with
			// nothing bounding it.
			// CLA-784: like a usage limit, an OPTIONAL seat's transient end is
			// its own result — never the sequence's, resumed or not.
			if panelOptional {
				log.Printf("%siteration %d: the OPTIONAL %s seat hit a transient failure (%s) — recording it against that seat and carrying on rather than retrying a task the panel is still working (log: %s)",
					labelOf(t), drainNum, ph.Label(phaseIdx), at.res.ExitString(), at.logPath)
				r.end.sequenceEnd = &phaseSequenceEnd{}
				return r.tokens, r.cost, false, r.end, nil
			}
			if prev != nil {
				log.Printf("iteration %d: a resumed phase hit a transient failure (%s) — ending the sequence rather than retrying a task the handback has already returned; it comes back as an immediate takeover, and the run stays up to poll again instead of exiting (log: %s)",
					drainNum, at.res.ExitString(), at.logPath)
				r.end.sequenceEnd = &phaseSequenceEnd{}
				return r.tokens, r.cost, false, r.end, nil
			}
			// A retry the adapter could not NAME is bounded on its own, because
			// nothing else bounds it: the dials are off by default and the
			// zero-spend counter is reset by the usage this heuristic requires. The
			// diagnostic goes in the error because it is the whole remedy - the
			// adapter has no pattern for this text, so an operator who can see it
			// can say whether one should be added (CLA-381).
			if a.IsUnclassifiedTransient(at.res) {
				r.unclassified++
				if r.unclassified > unclassifiedRetryBound {
					return r.tokens, r.cost, false, r.end, fmt.Errorf(
						"iteration %d: %w: %d retries on %s failures that matched no known-transient pattern - retried because the session had reported usage, but the same unrecognised failure keeps happening, so this is a real fault rather than a blip%s (log: %s)",
						drainNum, errUnclassifiedRetryLoop, r.unclassified-1, a.Name(), failureDetail(a.Diagnostic(at.res)), at.logPath)
				}
			}
			// As in the limit branch: the retry is a fresh session that re-claims,
			// so nothing may stay held open across it.
			r.end = d.releaseCarriedEnd(ctx, t, r.end)
			r.retries++
			if d.cfg.MaxRetries > 0 && r.retries > d.cfg.MaxRetries {
				return r.tokens, r.cost, false, r.end, fmt.Errorf(
					"iteration %d: transient failures persisted after %d retries (check https://status.claude.com; rerun to resume) (log: %s)",
					drainNum, d.cfg.MaxRetries, at.logPath)
			}
			wait := d.backoff(r.retries)
			log.Printf("iteration %d transient failure (%s) — %s in %s (a fresh session reclaims any half-done task) (log: %s)",
				drainNum, at.res.ExitString(), retryLabel(r.retries, d.cfg.MaxRetries), wait, at.logPath)
			if d.waitOrStop(ctx, wait) {
				return r.tokens, r.cost, true, r.end, nil
			}
			continue
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
