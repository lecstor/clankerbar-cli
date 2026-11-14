package loop

import (
	"context"
	"fmt"
	"log"
	"maps"
	"slices"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// spend is what the run had already consumed when this drain began, plus when it
// began — the budget breaker's whole view, handed down from Run.
//
// It is handed down because the ceilings are CUMULATIVE across the run while a
// drain only sees its own attempts, and because a drain can wait for hours
// (supervised wait, transient backoff) without ever returning to the breaker
// between drains. Measuring a mid-drain decision against this drain's spend alone
// would let a run walk past a ceiling it had already reached.
type spend struct {
	start  time.Time
	tokens int
	cost   float64
}

// harnessSpend is one harness's accumulated spend across a run, for the
// per-harness breakers (config.Budget.PerHarness, CLA-367).
type harnessSpend struct {
	tokens int
	cost   float64
}

// charge books a session's spend against the harness that ran it, in the ledger
// the per-harness breakers read.
//
// It is charged at the two places a session's spend enters a run — a drain
// attempt's result and a supervised wait's probe — which are the same two places
// the run-wide accumulators are added to, so the two accounts cannot drift. The
// run-wide ones stay exactly where they were, threaded through the drain as
// locals: the global dials have to keep behaving byte for byte as they always
// have, so this ledger is additive rather than a replacement for them.
//
// An UNTRUSTED session is charged to neither, for the reason endUntrustedDrain
// gives: a figure parsed out of a stream with a hole in it is not a measurement.
//
// The key is the phase's harness NAME AS THE CONFIG SPELLS IT (HarnessFor), never
// the adapter's own Name(). The two agree in production - an adapter is fetched
// from the registry by that same key - but only one of them is the currency the
// breaker spends: budgetTrip looks these keys up in Budget.PerHarness, which the
// operator keyed by config name. An adapter whose Name() diverged would silently
// disable its own per-harness ceiling rather than failing, so the ledger asks the
// config what a session IS and asks the adapter only behavioural questions.
//
// A run's phases can each name their own harness (CLA-366), so the caller charges
// the PHASE's harness, not the run's: on a mixed sequence the two call sites split
// the run's spend across one entry per harness, and on a single-harness run they
// both land on the same key and it tracks the run total exactly as before. That is
// the whole reason the ledger is keyed by name — passing d.h.Name() from a phase
// running on some other harness would bill opencode's tokens to claude's breaker,
// which is precisely the mis-accounting the per-harness ceilings exist to prevent.
func (d *Driver) charge(harnessName string, tokens int, cost float64) {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	if d.spentBy == nil {
		d.spentBy = make(map[string]harnessSpend)
	}
	s := d.spentBy[harnessName]
	s.tokens += tokens
	s.cost += cost
	d.spentBy[harnessName] = s
}

// budgetTrip names the ceiling that has stopped this run, or "" if none has: the
// run-wide dials first, measured against the run totals the caller threads down,
// then each harness's own block against that harness's own spend.
//
// Any trip stops the whole run, so the order only decides which ceiling gets
// named in the log, and the global dials are named first because a config that
// sets both wants the run-wide promise reported as the one that was broken.
// Harnesses are consulted in name order so a run with two blocks reports the same
// dial every time rather than whichever the map yielded first.
func (d *Driver) budgetTrip(tokens int, cost float64, elapsed time.Duration) string {
	if dim := d.cfg.Budget.ExceededBy(tokens, cost, elapsed); dim != "" {
		return dim
	}
	d.stateMu.Lock()
	spent := maps.Clone(d.spentBy)
	d.stateMu.Unlock()
	for _, name := range slices.Sorted(maps.Keys(spent)) {
		s := spent[name]
		if dim := d.cfg.Budget.ExceededByHarness(name, s.tokens, s.cost); dim != "" {
			// That harness's OWN totals ride along, because every caller prints
			// the RUN's figures after the reason: in a mixed-harness run two
			// dollar figures on one line read as a contradiction, which is the
			// defect Budget.ExceededBy's doc comment exists to prevent, one level
			// down.
			return fmt.Sprintf("%s (%s so far: tokens=%d cost=$%.2f)", dim, name, s.tokens, s.cost)
		}
	}
	return ""
}

// deadReason renders the finish reason a dead phase was classified on, for a log
// line. The classification fired on FinishReasonUnknown, so a nil claim — a
// session that observed no task — still has a reason to name.
//
// The adapter's OWN terminal_reason marker wins when present: ZeroUsageReason
// (CLA-398) names the quiet-death signature more precisely than the bare
// "unknown" it rides on, so the operator's log says which flavour of death this
// was instead of the driver re-inferring it from the zero usage downstream.
func deadReason(res *harness.Result) string {
	if res != nil {
		if r, ok := res.Raw[harness.TerminalReasonKey].(string); ok && r != "" {
			return r
		}
		if r, ok := res.Raw[harness.FinishReasonKey].(string); ok && r != "" {
			return r
		}
	}
	return harness.FinishReasonUnknown
}

// outputCapStallReasoning reports whether this session ended on the CLA-584
// output-cap stall, and the stalled final step's own reasoning-token count when
// it did. The question is asked of the adapter's own terminal_reason marker —
// the same channel deadReason reads — never re-derived from the finish reason,
// so a harness that cannot produce the stall simply never answers true.
//
// It exists so the hand-back can say WHY a session stopped holding a task it
// never moved on: the MAK-123 post-mortem found only "the phase finished but
// never moved the task on", with nothing in the daemon log saying the model had
// spent its whole output budget thinking (reasoning=32000) and emitted nothing.
func outputCapStallReasoning(res harness.Result) (int, bool) {
	if r, _ := res.Raw[harness.TerminalReasonKey].(string); r != harness.OutputCapReason {
		return 0, false
	}
	n, _ := res.Raw[harness.OutputCapReasoningKey].(int)
	return n, true
}

// fleetTrip pauses a target whose fleet dead-phase counter has reached
// fleetDeadBound (CLA-396): that many consecutive dead phases across tasks mean
// the harness or provider is broken right now, not the tasks, and every further
// session would pay the same death rate for nothing. The target stops spawning
// until the operator answers the raised project-level question (the open-question
// count falls back to what it was at the raise — see the poll gate in Run).
//
// The question is PROJECT-level — ask_question with no taskId — because a fleet
// trip is not one task's triage: pinning it to whichever task happened to be in
// flight would make the operator answer about a bystander. It is filed as a
// non-blocking `decision` — there is no task to block (the loop's pause is the
// enforcement), and the operator's ruling on a fleet incident is a project-level
// judgment worth standing in the decision log, unlike the park's one-task
// clarification.
//
// "Exactly one" is guaranteed by fleetRaised: while the pause is in force the
// flag is set, so a later trip in the same episode does not re-raise. The flag
// clears with the pause, so a NEW episode — the operator answered, the loop
// resumed, and the fleet died again — raises a fresh question.
//
// If the question cannot be filed, the pause is NOT set: a pause the operator
// can never be told about and can never resume is a permanent stall, which is
// worse than the fall-through (per-task parking still bounds each task). The
// dead-phase counter stays at the bound, so the next dead phase tries again —
// loudly.
func (d *Driver) fleetTrip(ctx context.Context, t Target, ti, drainNum int, res *harness.Result) {
	pk, ok := t.Releaser.(plane.ParkAPI)
	if !ok {
		log.Printf("%siteration %d: %d consecutive dead phases across tasks, but the releaser cannot file a project question — NOT pausing (the operator could never resume it); the per-task budget still bounds each individual task",
			d.prefix(ti), drainNum, d.fleetDead[ti])
		return
	}
	if !d.fleetRaised[ti] {
		d.fleetRaised[ti] = true
		// The baseline the resume signal is measured against: the open-question
		// count at the last poll, BEFORE the question below raises it by one.
		d.fleetOpenQ[ti] = d.openQs[ti]
		why := fmt.Sprintf("Each task died producing nothing (final step reason %q, no branch recorded) before this", deadReason(res))
		if res != nil && res.ReportResumeFailed != "" {
			why = "Each task's session delivered its work but its done report could not be re-emitted (the provider stream kept hanging) before this"
		}
		body := fmt.Sprintf(
			"**%d consecutive dead phases across tasks — the provider or harness looks broken, not the tasks.** %s, so the driver paused this project rather than burning more sessions against a provider that is already known to be failing.\n\nThe loop is PAUSED for this project until you answer. Answering resumes it; the next dead phase will pause it again and raise a fresh question. Iteration logs are in %s.",
			d.fleetDead[ti], why, d.state.Path())
		options := []string{
			"The provider recovered — resume draining this project",
			"Still investigating — I will answer when it is safe to resume",
			"Switch or restart the harness — try again",
			"Something else is wrong — stop the daemon",
		}
		pctx, pcancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer pcancel()
		if err := pk.AskProjectQuestion(pctx, body, options, "decision"); err != nil {
			log.Printf("%siteration %d: the fleet dead-phase question could not be filed: %v — NOT pausing (the operator could never resume it); the next dead phase will retry the raise",
				d.prefix(ti), drainNum, err)
			d.fleetRaised[ti] = false
			return
		}
	}
	d.fleetPaused[ti] = true
	log.Printf("%siteration %d: %d consecutive dead phases across tasks — the provider or harness looks broken, not the tasks; pausing this project until the operator answers the raised question",
		d.prefix(ti), drainNum, d.fleetDead[ti])
}

// startupTrip pauses a target whose harness-startup counter has reached
// startupFailBound (CLA-562): that many consecutive spawns failing before any
// usage report means the harness or provider is broken right now, not the
// tasks, and every further spawn would pay the same sideline-and-retry for
// nothing. Same shape as fleetTrip: the target stops spawning until the
// operator answers the raised project-level question (the open-question count
// falls back to what it was at the raise - see the poll gate in Run).
//
// The question is PROJECT-level - ask_question with no taskId - because a
// startup trip is not one task's triage: pinning it to whichever drain happened
// to be in flight would make the operator answer about a bystander. It is filed
// as a non-blocking `decision` - there is no task to block (the loop's pause is
// the enforcement), and the ruling is project-level judgment for the decision
// log.
//
// The body carries the harness exit signature (the sideline cause), as the
// sideline log line does: "provider looks broken" without the error string
// sends the operator grep-hunting through state-dir logs.
//
// "Exactly one" is guaranteed by startupRaised, cleared with the pause so a new
// episode raises fresh. If the question cannot be filed, the pause is NOT set
// and the counter stays at the bound, so the next startup failure retries the
// raise - loudly - while the sideline backoff keeps bounding each attempt.
func (d *Driver) startupTrip(ctx context.Context, t Target, ti, drainNum int, cause error) {
	pk, ok := t.Releaser.(plane.ParkAPI)
	if !ok {
		log.Printf("%siteration %d: %d consecutive harness-startup failures, but the releaser cannot file a project question - NOT pausing (the operator could never resume it); the sideline backoff still bounds each attempt",
			d.prefix(ti), drainNum, d.startupFails[ti])
		return
	}
	if !d.startupRaised[ti] {
		d.startupRaised[ti] = true
		// The baseline the resume signal is measured against: the open-question
		// count at the last poll, BEFORE the question below raises it by one.
		d.startupOpenQ[ti] = d.openQs[ti]
		body := fmt.Sprintf(
			"**%d consecutive harness-startup failures - the harness or provider looks broken, not the tasks.** Each spawn failed before reporting any usage (workdir %s), so the driver paused this project rather than sidelining and retrying forever against a harness that is already known to be failing.\n\nCause: %v\n\nThe loop is PAUSED for this project until you answer. Answering resumes it; the next startup failure will pause it again and raise a fresh question. Iteration logs are in %s.",
			d.startupFails[ti], d.workdirOf(t), cause, d.state.Path())
		options := []string{
			"The harness recovered - resume draining this project",
			"Still investigating - I will answer when it is safe to resume",
			"Switch or restart the harness - try again",
			"Something else is wrong - stop the daemon",
		}
		pctx, pcancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer pcancel()
		if err := pk.AskProjectQuestion(pctx, body, options, "decision"); err != nil {
			log.Printf("%siteration %d: the harness-startup question could not be filed: %v - NOT pausing (the operator could never resume it); the next startup failure will retry the raise",
				d.prefix(ti), drainNum, err)
			d.startupRaised[ti] = false
			return
		}
	}
	d.startupPaused[ti] = true
	log.Printf("%siteration %d: %d consecutive harness-startup failures - the harness looks broken, not the tasks; pausing this project until the operator answers the raised question. Cause: %v",
		d.prefix(ti), drainNum, d.startupFails[ti], cause)
}

// parkDeadPhase parks the task after four consecutive dead phases, filing an
// OPEN question so the park reaches the operator instead of vanishing into the
// Done tab (CLA-395). The question is a non-blocking `clarification` — the task
// is already parked, so there is nothing left to block, and a `decision` would
// read as project-wide standing law rather than one task's triage. Its body and
// options are modelled on the plane's own escalateExhausted shape: what
// happened, the dead-phase signature, how many sessions it cost, where the
// iteration logs are, and options covering retry / re-scope / leave parked /
// drop (CLA-386, per the 2026-08-20 operator decision: retry the implement
// phase four times, then park; the budget is per task).
//
// The phase is dead — its sessions are gone — so nobody is left to declare the
// failure; the driver must. The claim was released before this ran (the retry
// re-claimed, then died again), so the park is a plain status write signed by
// the dead phase's own run.
func (d *Driver) parkDeadPhase(ctx context.Context, t Target, phaseLabel string, res *harness.Result) {
	if res == nil || res.Claim.TaskID == "" {
		log.Printf("%sdead-phase park: no claim observed, so there is no task to park", labelOf(t))
		return
	}
	// How the sessions died. A CLA-739 tool-loop kill is dead whatever it
	// pushed, so "no branch recorded" may be false for it and the finish reason
	// says nothing: the cause names what happened instead. The cause is the
	// LAST attempt's, which is the one the operator is being asked about.
	// CLA-877: a report-resume failure delivered its work too — only the reply
	// that re-emits the done report kept failing — so it is named for what it
	// is rather than as a session that produced nothing.
	death := fmt.Sprintf("died producing nothing (final step reason %q, no branch recorded)", deadReason(res))
	switch {
	case res.ReportResumeFailed != "":
		death = fmt.Sprintf("delivered its work but could not re-emit its done report (%s)", res.ReportResumeFailed)
	case res.ToolLoop != "":
		death = fmt.Sprintf("was killed by the driver (%s)", res.ToolLoop)
	}
	pk, ok := t.Releaser.(plane.ParkAPI)
	if !ok {
		log.Printf("%sCANNOT PARK %s — four consecutive dead phases (the last %s), but the releaser cannot park or raise a question; the task stays in the queue and the next claim will retry it again",
			labelOf(t), claimLabel(res.Claim), death)
		return
	}
	ref := claimLabel(res.Claim)
	outcome := fmt.Sprintf(
		"Parked after four consecutive dead phases: the %s session %s on the last attempt, after dying on each of the three before it. Per the 2026-08-20 operator decision, a task that can kill four full sessions reaches the operator rather than a fifth; the retry budget is per task. Iteration logs are in %s.",
		phaseLabel, death, d.state.Path())
	body := fmt.Sprintf(
		"**%s has defeated four sessions and is now parked.** The %s phase died on four consecutive attempts; the last one %s, so the driver stopped rather than spending a fifth session on it.\n\n"+
			"The task is `parked` — nothing will pick it up until an operator decides what to do with it. The iteration logs are in %s.",
		ref, phaseLabel, death, d.state.Path())
	options := []string{
		"Set it back to `ready` — the blocker is gone, let the fleet try again",
		"Leave it parked — I will work it myself",
		"It needs re-scoping before anyone tries again",
		"Drop it — this is not worth another attempt",
	}

	pctx, pcancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer pcancel()
	if err := pk.Park(pctx, res.Claim.TaskID, res.Claim.RunID, outcome); err != nil {
		log.Printf("%spark failed for %s: %v — the task is left for the next claim to retry it again",
			labelOf(t), claimLabel(res.Claim), err)
		return
	}
	// Parked is a let-go: the record describes a run that no longer holds the
	// task, and a supervisor must not try to hand it back (CLA-708).
	d.clearCurrentClaim()
	if err := pk.AskQuestion(pctx, res.Claim.TaskID, body, options, false, "clarification"); err != nil {
		// The task IS parked — it will not be retried — so saying it was "left
		// for the next claim" would be the opposite of what happened. The park
		// outcome stands alone; the missing question is the failure the operator
		// needs to know about (review finding).
		log.Printf("%sparked %s — four consecutive dead phases, but the question for the operator could not be filed: %v",
			labelOf(t), claimLabel(res.Claim), err)
		return
	}
	log.Printf("%sparked %s — four consecutive dead phases, question filed for the operator",
		labelOf(t), claimLabel(res.Claim))
}

// parkStartRefusal parks a task whose worktree the runner refused on two
// consecutive claims, and reports whether the task IS parked: only a Park that
// took makes the claim safe to let go. The caller logs the outcome, because only
// it knows the refusal count and the cause. Unlike a dead phase the claim is LIVE
// here, so the park signs it with its own run and then lets it go: a dirty or
// diverged tree refuses every claim identically, so another claim buys nothing and
// only a human can clear it. The question is a non-blocking `clarification` for
// the same reason parkDeadPhase's is — the task is already parked, so there is
// nothing to block.
func (d *Driver) parkStartRefusal(ctx context.Context, t Target, res *harness.Result, why string) bool {
	if res == nil || res.Claim.TaskID == "" {
		log.Printf("%sstart-refusal park: no claim observed, so there is no task to park", labelOf(t))
		return false
	}
	pk, ok := t.Releaser.(plane.ParkAPI)
	if !ok {
		log.Printf("%sCANNOT PARK %s — two consecutive worktree refusals (%s), but the releaser cannot park or raise a question; the claim is handed back and the next refusal retries the park",
			labelOf(t), claimLabel(res.Claim), why)
		return false
	}
	ref := claimLabel(res.Claim)
	outcome := fmt.Sprintf(
		"Parked: the runner could not prepare this task's worktree on two consecutive claims (%s). The tree or branch needs a human: a dirty or diverged worktree, or a local branch with commits origin does not have. Iteration logs are in %s.",
		why, d.state.Path())
	body := fmt.Sprintf(
		"**%s is parked: its worktree could not be prepared.** The runner refused twice: %s\n\nClean up or remove the worktree/branch named above, then set the task back to `ready`.",
		ref, why)
	options := []string{
		"Set it back to `ready` — I fixed the worktree",
		"Leave it parked — I will work it myself",
		"Drop it",
	}

	pctx, pcancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer pcancel()
	if err := pk.Park(pctx, res.Claim.TaskID, res.Claim.RunID, outcome); err != nil {
		log.Printf("%spark failed for %s: %v — the claim is handed back and the next refusal retries the park",
			labelOf(t), claimLabel(res.Claim), err)
		return false
	}
	// Parked is a let-go: the record describes a run that no longer holds the
	// task, and a supervisor must not try to hand it back.
	d.clearCurrentClaim()
	if err := pk.AskQuestion(pctx, res.Claim.TaskID, body, options, false, "clarification"); err != nil {
		// The task IS parked — it will not be retried — so the park stands and the
		// missing question is the failure the operator needs to know about.
		log.Printf("%sparked %s — two consecutive worktree refusals, but the question for the operator could not be filed: %v",
			labelOf(t), claimLabel(res.Claim), err)
	}
	return true
}
