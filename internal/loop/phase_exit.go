package loop

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// The ladder stages that end or repeat the attempt once its seam is settled.

// endOnInvokeError ends the attempt when the run itself failed rather than the session exiting: a
// cancelled context stops the run, a failed MCP preflight is retried once, anything else is an
// error.
func (r *phaseRun) endOnInvokeError(ctx context.Context, at *phaseAttempt) attemptVerdict {
	t := r.t
	drainNum := r.drainNum
	phaseIdx := r.phaseIdx
	ph := r.ph
	a := r.a
	if at.ierr != nil {
		if ctx.Err() != nil {
			return attemptVerdict{next: attemptReturn, stop: true}
		}
		// Count this attempt's spend before giving up on it.
		if at.res.Untrusted == "" {
			r.countTrustedSpend(at)
		}
		// CLA-703: an MCP preflight failure is infrastructure, not a verdict on the phase.
		if errors.Is(at.ierr, harness.ErrMCPNotConnected) {
			if !r.mcpRetried {
				r.mcpRetried = true
				log.Printf("%siteration %d: %s could not start its first turn: %v — retrying the %s phase once (MCP preflight failure, not a phase verdict)",
					labelOf(t), drainNum, a.Name(), at.ierr, ph.Label(phaseIdx))
				return attemptVerdict{next: attemptRetry}
			}
			return attemptVerdict{next: attemptReturn, err: fmt.Errorf("iteration %d: %w", drainNum, at.ierr)}
		}
		// A run failure that was not an exit status — a launch failure, or one of the deaths in Wait
		// above — ends the attempt without any retry classification.
		return attemptVerdict{next: attemptReturn, err: fmt.Errorf("invoke %s: %w (log: %s)", a.Name(), at.ierr, at.logPath)}
	}

	return attemptVerdict{}
}

// countAttemptSpend refuses a stream that could not be read whole, and otherwise counts the
// attempt's spend toward the budget breaker and the pass's session entry.
func (r *phaseRun) countAttemptSpend(ctx context.Context, at *phaseAttempt) attemptVerdict {
	d := r.d
	t := r.t
	tc := r.tc
	drainNum := r.drainNum
	ph := r.ph
	// A session whose stream could not be read whole. Everything below this line reads a figure parsed
	// out of that stream, so counting the spend, classifying the exit or retrying on the strength of
	// it are three ways to make a confident decision on data with a hole in it.
	if at.res.Untrusted != "" {
		// held is nil here by construction — an untrusted stream never holds a phase open — so the
		// sequence ends and nothing is carried forward.
		utokens, ucost, ustop, uerr := d.endUntrustedDrain(t, r.ti, drainNum, tc.HarnessFor(ph), at.logPath, at.res, r.tokens, r.cost)
		return attemptVerdict{next: attemptReturn, spendOverride: true, tokens: utokens, cost: ucost, stop: ustop, err: uerr}
	}

	// Count THIS attempt's spend toward the budget breaker regardless of how it ends — usage-limit,
	// transient, stop, or clean.
	r.countTrustedSpend(at)

	return attemptVerdict{}
}

// countTrustedSpend counts one attempt's spend toward the budget breaker and the pass's session
// entry. Callers hold the guard that the stream was read whole: an untrusted stream carries a
// floor, not a total, and is counted nowhere.
func (r *phaseRun) countTrustedSpend(at *phaseAttempt) {
	r.tokens += at.res.Tokens
	r.cost += at.res.CostUSD
	// The pass's own session entry takes the same figures, in the same breath: every attempt the loop
	// counts is money the record attributes to the pass, and none it refuses is.
	recordPassSpend(r.end.session, r.a, &r.pass, &at.res)
	r.d.charge(r.ti, r.tc.HarnessFor(r.ph), at.res.Tokens, at.res.CostUSD)
}

// usageLimitEnd reports whether the session that just ended died on the provider's usage limit -
// the same classification handleUsageLimit acts on, taken HERE because the handback runs first.
// DetectLimit covers a rolling-window cap and a hard stop; the turn stop reason covers a
// multi-turn session whose sequence a limit ended.
func usageLimitEnd(a harness.Adapter, res harness.Result) bool {
	return a.DetectLimit(res).Limited || res.TurnStopReason == "usage_limit"
}

// handleUsageLimit waits out a rolling usage limit and re-runs the session, or ends the phase on a
// hard stop or a limit the phase must not wait for.
func (r *phaseRun) handleUsageLimit(ctx context.Context, at *phaseAttempt) attemptVerdict {
	d := r.d
	t := r.t
	tc := r.tc
	drainNum := r.drainNum
	phaseIdx := r.phaseIdx
	ph := r.ph
	prev := r.prev
	prior := r.prior
	a := r.a
	panelOptional := r.shape.panelOptional
	// A usage limit. A rolling-window subscription cap is waited out and the session re-run; a hard
	// budget/credit exhaustion (Stop) has no reset to poll for, so the run stops cleanly and the
	// operator resumes it once they've topped up.
	if lim := a.DetectLimit(at.res); lim.Limited {
		if lim.Stop {
			// CLA-784: an OPTIONAL panel seat is a trial reviewer. A hard stop on its account is recorded
			// against that seat — never the run.
			if panelOptional {
				log.Printf("%siteration %d: the %s member is an OPTIONAL trial seat — its usage stop is recorded against that seat, not the run (log: %s)",
					labelOf(t), drainNum, ph.Label(phaseIdx), at.logPath)
				r.end.empty = true
				r.end.emptyWhy = "stopped on a usage limit (no reset to wait for) before recording"
				return attemptVerdict{next: attemptReturn}
			}
			line := limitStopLine(lim, limitStopRoute(at.res, at.inv.Model), limitStopMessage(a, at.res))
			log.Printf("iteration %d stopped: %s — no reset to wait for, stopping (resume once resolved) (log: %s)",
				drainNum, line, at.logPath)
			d.writeLimitStop(line)
			return attemptVerdict{next: attemptReturn, stop: true}
		}
		log.Printf("iteration %d hit a usage limit: %s", drainNum, limitReason(lim))
		// CLA-784: an OPTIONAL seat never waits and never ends the sequence, resumed or not.
		if panelOptional {
			log.Printf("%siteration %d: the OPTIONAL %s seat hit a usage limit (%s) — recording it against that seat and carrying on rather than waiting out a reset on a trial reviewer (log: %s)",
				labelOf(t), drainNum, ph.Label(phaseIdx), limitReason(lim), at.logPath)
			r.end.sequenceEnd = &phaseSequenceEnd{limitWait: &phaseLimitWait{lim: lim, ph: ph, a: a}}
			return attemptVerdict{next: attemptReturn}
		}
		// A RESUMED phase must not wait one out. The seam's handback has already ended the claim:
		// `release` puts a no-work claim straight back in the queue, and `expireLease` ends a pushed-work
		// claim's lease immediately, leaving the task an immediate takeover — so a supervised wait of
		// hours would only run against a task this sequence no longer holds, with a runId in its prompt
		// that no longer means anything.
		if prev != nil {
			log.Printf("iteration %d: a resumed phase hit a usage limit — ending the sequence rather than waiting out a reset on a task the handback has already returned; it comes back as an immediate takeover, and the run stays up to wait for the reset instead of exiting (log: %s)",
				drainNum, at.logPath)
			r.end.sequenceEnd = &phaseSequenceEnd{limitWait: &phaseLimitWait{lim: lim, ph: ph, a: a}}
			return attemptVerdict{next: attemptReturn}
		}
		// Whatever this attempt held open is not held open any more: every path below either waits or
		// retries with a FRESH session, which re-claims.
		r.end = d.releaseCarriedEnd(ctx, t, r.end, true)
		remaining, bounded := tc.Budget.Remaining(d.budgetElapsed(r.ti, time.Since(prior.start)))
		if until, over := waitPastBudget(lim.ResetAt, remaining, bounded); over {
			log.Printf("iteration %d: the limit resets %s, in %s — more than the %s left of this ceiling; not waiting, so start a fresh run after the reset (log: %s)",
				drainNum, formatReset(until), time.Until(until).Round(time.Minute), remaining.Round(time.Minute), at.logPath)
			d.exhaustForWait(r.ti, "the stated reset is past the remaining budget")
			return attemptVerdict{next: attemptReturn, stop: true}
		}
		// The wait's own probe spend lands in the SAME account as the sessions' (charge, inside the
		// wait), so it reaches the breaker at the top of this loop, the between-iterations sweep, and the
		// iteration's cost line — rather than a separate figure nothing is measured against.
		ptokens, pcost, pstop := d.supervisedWait(ctx, lim, a, ph, t, spend{
			start: prior.start,
			ti:    r.ti,
		})
		r.tokens += ptokens
		r.cost += pcost
		if pstop {
			return attemptVerdict{next: attemptReturn, stop: true}
		}
		return attemptVerdict{next: attemptRetry}
	}

	return attemptVerdict{}
}

// endOnExitKind counts a silent attempt, then ends the phase on the exits that are not a failure to
// retry: a tool-loop kill, a clean exit, a recovered exit and the turn, token and wall-clock caps.
func (r *phaseRun) endOnExitKind(ctx context.Context, at *phaseAttempt) attemptVerdict {
	t := r.t
	drainNum := r.drainNum
	phaseIdx := r.phaseIdx
	ph := r.ph
	// Below the limit branch, so a usage-limit attempt is counted NEITHER way.
	if at.res.UsageReported {
		r.noUsage = 0
	} else {
		r.noUsage++
	}

	// CLA-739: killed for repeating a failing tool call. Ahead of the exit status, because the session
	// can exit 0 in the instant before the kill lands.
	if at.toolLoop != "" {
		// claude reports dollars only on its terminal result event, which a killed stream never reaches -
		// so say the cost was not captured rather than print $0 as if it were measured.
		spent := fmt.Sprintf("tokens=%d cost=$%.4f", r.tokens, r.cost)
		if !at.res.CostReported {
			spent = fmt.Sprintf("tokens=%d; cost not captured - killed mid-stream", r.tokens)
		}
		log.Printf("%siteration %d: the %s phase was killed by the driver: %s — classified dead, no delivery (%s) (log: %s)",
			labelOf(t), drainNum, ph.Label(phaseIdx), at.toolLoop, spent, at.logPath)
		return attemptVerdict{next: attemptReturn}
	}

	if at.res.ExitCode == 0 {
		// A clean exit is the shape the CLA-398 quiet death wears, so the marker gets named here rather
		// than inferred downstream: a final step_finish with reason "unknown" and all-zero usage used to
		// read as "iteration done (tokens=0 cost=$0.00)" — indistinguishable from a cheap clean run,
		// which is exactly what made these deaths invisible.
		if at.zeroUsage {
			log.Printf("iteration %d: the session ended with the zero-usage-unknown signature (final step reason %q, all-zero usage) — a quiet death, not a clean run (log: %s)",
				drainNum, harness.FinishReasonUnknown, at.logPath)
		} else if finished := harness.RouteFinished(at.res); finished != "" && finished != at.inv.Model {
			// CLA-782: the spawn line names the route the session STARTED on; this line adds the route that
			// finished it when a usage limit moved it mid-session.
			log.Printf("iteration %d done (tokens=%d cost=$%.4f; finished on %s) (log: %s)", drainNum, r.tokens, r.cost, finished, at.logPath)
		} else {
			log.Printf("iteration %d done (tokens=%d cost=$%.4f) (log: %s)", drainNum, r.tokens, r.cost, at.logPath)
		}
		return attemptVerdict{next: attemptReturn}
	}

	// CLA-700: a session that recovered from its provider errors is not judged by its exit status —
	// the stream shows the harness took the error, retried and carried the session on, and 2.x exits
	// non-zero regardless (pinned by the conformance suite).
	if at.recoveredExit {
		log.Printf("iteration %d: the session's stream shows %d provider error event(s) it recovered from and no terminal one (exit %s) — judging this phase on its delivery, not on the exit status (log: %s)",
			drainNum, harness.RecoveredErrors(at.res), at.res.ExitString(), at.logPath)
		return attemptVerdict{next: attemptReturn}
	}

	// A turn-capped end is this PHASE ending, never the run failing.
	if at.capped {
		log.Printf("iteration %d: the session hit its turn cap (tokens=%d cost=$%.4f) — ending this phase; anything uncommitted was salvaged above (log: %s)",
			drainNum, r.tokens, r.cost, at.logPath)
		return attemptVerdict{next: attemptReturn}
	}

	// A session the ADAPTER killed for crossing its per-session token ceiling is the same shape as a
	// turn cap: an orderly end with the salvage's problem to handle, never a failure and never a
	// retry.
	if at.ceiling {
		log.Printf("iteration %d: the session crossed its per-session token ceiling (tokens=%d; cost not captured — killed mid-stream) — ending this phase; anything uncommitted was salvaged above (log: %s)",
			drainNum, r.tokens, at.logPath)
		return attemptVerdict{next: attemptReturn}
	}

	// A session the ADAPTER ended for outliving its wall-clock cap is the same shape again: the phase
	// ends, and neither a retry nor a failure is right - a retry would spend the same hours over again
	// and reach the same deadline, and failing would stop the run over a cap doing its job.
	if at.wallclock {
		if at.res.Claim.Held() {
			log.Printf("iteration %d: the session outlived its wall-clock cap (tokens=%d cost=$%.4f) — ending this phase; anything uncommitted was salvaged above (log: %s)",
				drainNum, r.tokens, r.cost, at.logPath)
		} else {
			log.Printf("iteration %d: the session outlived its wall-clock cap (tokens=%d cost=$%.4f) — ending this phase. NOTHING was salvaged: this harness does not observe the session's task claim, so whatever the session left uncommitted is still in the worktree (log: %s)",
				drainNum, r.tokens, r.cost, at.logPath)
		}
		return attemptVerdict{next: attemptReturn}
	}

	return attemptVerdict{}
}

// handleTransient backs off and retries a transient failure, or ends the phase where a retry is not
// allowed (a resumed phase, an optional seat, a bound hit).
func (r *phaseRun) handleTransient(ctx context.Context, at *phaseAttempt) attemptVerdict {
	d := r.d
	t := r.t
	drainNum := r.drainNum
	phaseIdx := r.phaseIdx
	ph := r.ph
	prev := r.prev
	a := r.a
	panelOptional := r.shape.panelOptional
	// Non-zero exit, not the usage cap: a transient server/network blip backs off and retries;
	// anything else is a genuine failure and stops.
	if a.IsTransient(at.res) {
		// A RESUMED phase does not retry, for the same reason it does not wait out a usage limit — and
		// this path is the more dangerous of the two, because MaxRetries defaults to 0, meaning never
		// give up.
		if panelOptional {
			log.Printf("%siteration %d: the OPTIONAL %s seat hit a transient failure (%s) — recording it against that seat and carrying on rather than retrying a task the panel is still working (log: %s)",
				labelOf(t), drainNum, ph.Label(phaseIdx), at.res.ExitString(), at.logPath)
			r.end.sequenceEnd = &phaseSequenceEnd{}
			return attemptVerdict{next: attemptReturn}
		}
		if prev != nil {
			log.Printf("iteration %d: a resumed phase hit a transient failure (%s) — ending the sequence rather than retrying a task the handback has already returned; it comes back as an immediate takeover, and the run stays up to poll again instead of exiting (log: %s)",
				drainNum, at.res.ExitString(), at.logPath)
			r.end.sequenceEnd = &phaseSequenceEnd{}
			return attemptVerdict{next: attemptReturn}
		}
		// A retry the adapter could not NAME is bounded on its own, because nothing else bounds it: the
		// dials are off by default and the zero-spend counter is reset by the usage this heuristic
		// requires.
		if a.IsUnclassifiedTransient(at.res) {
			r.unclassified++
			if r.unclassified > unclassifiedRetryBound {
				return attemptVerdict{next: attemptReturn, err: fmt.Errorf(
					"iteration %d: %w: %d retries on %s failures that matched no known-transient pattern - retried because the session had reported usage, but the same unrecognised failure keeps happening, so this is a real fault rather than a blip%s (log: %s)",
					drainNum, errUnclassifiedRetryLoop, r.unclassified-1, a.Name(), failureDetail(a.Diagnostic(at.res)), at.logPath)}
			}
		}
		// As in the limit branch: the retry is a fresh session that re-claims, so nothing may stay held
		// open across it.
		r.end = d.releaseCarriedEnd(ctx, t, r.end, false)
		r.retries++
		if d.cfg.MaxRetries > 0 && r.retries > d.cfg.MaxRetries {
			return attemptVerdict{next: attemptReturn, err: fmt.Errorf(
				"iteration %d: transient failures persisted after %d retries (check https://status.claude.com; rerun to resume) (log: %s)",
				drainNum, d.cfg.MaxRetries, at.logPath)}
		}
		wait := d.backoff(r.retries)
		log.Printf("iteration %d transient failure (%s) — %s in %s (a fresh session reclaims any half-done task) (log: %s)",
			drainNum, at.res.ExitString(), retryLabel(r.retries, d.cfg.MaxRetries), wait, at.logPath)
		if d.waitOrStop(ctx, wait) {
			return attemptVerdict{next: attemptReturn, stop: true}
		}
		return attemptVerdict{next: attemptRetry}
	}

	return attemptVerdict{}
}
