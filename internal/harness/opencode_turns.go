package harness

// opencode_turns.go — planned multi-turn invocations in ONE opencode session.
//
// A reviewer that first takes notes and then writes findings needs the later
// turn to see everything the earlier ones read, and a config may want a
// different (stronger or cheaper) model per stage. Both are one opencode
// session: each turn is a `run -s <same id>` continuation, exactly the shape
// the resurrection, route-switch and report resumes already use, so the
// transcript survives and only the new turn's prompt is sent.

import (
	"context"
	"fmt"
	"time"
)

// runTurns runs the follow-up turns of a multi-turn invocation in the session
// its first turn created. The first turn has already run and its message is the
// sequence's first entry.
//
// It returns the invocation the LAST turn ran on and whether every turn ran.
// The caller's recovery passes and done-report check must use that invocation,
// not the first turn's: a final turn with its own model must be resumed and
// re-steered on that model, and must never be swapped onto the invocation's
// route list.
//
// A turn is followed by another only after it ends cleanly. The guards are
// resumeSkippedReason's own list - never a second copy - and a usage limit on a
// turn running the invocation's route list first follows the existing
// switchRoutesOnLimit rules. The first turn that fails a guard stops the
// sequence: Result.TurnStopped names it and no later turn runs.
func (o opencode) runTurns(ctx context.Context, in Invocation, res *Result, deadline time.Time) (Invocation, bool, error) {
	res.TurnMessages = nil

	// Turn 1 is a non-final turn whenever a follow-up follows it, so its end is
	// judged on exactly the terms a later turn's would be. Its message is
	// recorded AFTER that judgement: a route switch on this turn is a
	// continuation of it, so the switched answer is the turn's message.
	base, reason, err := o.afterNonFinalTurn(ctx, in, in, res, deadline)
	res.TurnMessages = append(res.TurnMessages, res.FinalMessage)
	if err != nil || reason != "" {
		o.stopTurns(in, res, 1, reason)
		return base, false, err
	}
	for i, turn := range in.Turns {
		turnNo := i + 2 // the first turn is 1
		if overBudget(deadline) {
			if in.Console != nil {
				fmt.Fprint(in.Console, opencodeTurnWallClockMsg)
			}
			o.stopTurns(in, res, turnNo, "wall_clock")
			return base, false, nil
		}
		sid, _ := resumeTargets(*res)
		if sid == "" {
			o.stopTurns(in, res, turnNo, "no_session")
			return base, false, nil
		}
		// On the BASE, never on the previous turn's invocation: a turn with a
		// per-turn model must not leak that model into a later turn that names
		// none (OnTurn only overrides when the turn names one).
		turnIn := o.turnInvocation(base.OnTurn(turn), res, deadline)
		if in.Console != nil {
			fmt.Fprintf(in.Console, "\n!! turn %d of %d — continuing session %s on %s\n",
				turnNo, len(in.Turns)+1, sid, modelLabel(turnIn))
		}
		// Blank the base first: mergeResume keeps the base message when a
		// continuation emits none, which would make an empty turn report its
		// predecessor's answer as its own.
		res.FinalMessage = ""
		turnRes, err := o.runSession(ctx, turnIn, opencodeResumeArgs(turnIn, sid, turn.Prompt))
		if err != nil {
			chargeTurn(res, turnRes)
			res.FinalMessage = turnRes.FinalMessage
			res.TurnMessages = append(res.TurnMessages, res.FinalMessage)
			o.stopTurns(in, res, turnNo, "run_error")
			if in.Console != nil {
				fmt.Fprintf(in.Console, "!! turn %d errored (%v) — stopping the turn sequence\n", turnNo, err)
			}
			return base, false, err
		}
		mergeResume(res, turnRes)
		if i == len(in.Turns)-1 {
			// The final turn IS the session's end: the caller's existing
			// recovery passes and done-report check judge it exactly as they
			// judge a single-turn session, on ITS model and route list.
			res.TurnMessages = append(res.TurnMessages, res.FinalMessage)
			return turnIn, true, nil
		}
		base, reason, err = o.afterNonFinalTurn(ctx, turnIn, base, res, deadline)
		res.TurnMessages = append(res.TurnMessages, res.FinalMessage)
		if err != nil || reason != "" {
			o.stopTurns(in, res, turnNo, reason)
			return base, false, err
		}
	}
	// Unreachable: runTurns is called only with a non-empty Turns.
	return base, true, nil
}

// afterNonFinalTurn judges a non-final turn's end and returns the invocation a
// later turn would run on.
//
// The turn's OWN invocation decides the route behaviour: a limit first follows
// switchRoutesOnLimit on the turn's list (a property of the stage's configured
// route), and a turn with its own model carries no list (Invocation.OnTurn), so
// its limit stands and stops the sequence - the stage's model is never silently
// swapped. Only a turn that ran the invocation's route list advances base onto
// the route that finished it. Every other bad end is resumeSkippedReason's
// decision, named with its own vocabulary.
func (o opencode) afterNonFinalTurn(ctx context.Context, turnIn, base Invocation, res *Result, deadline time.Time) (Invocation, string, error) {
	if o.DetectLimit(*res).Limited && res.Untrusted == "" {
		if err := o.switchRoutesOnLimit(ctx, turnIn, turnIn.RouteList(), res, deadline); err != nil {
			return base, "run_error", err
		}
		if o.DetectLimit(*res).Limited {
			return base, "usage_limit", nil
		}
		if finished := RouteFinished(*res); finished != "" {
			base = base.OnFinishedRoute(finished)
		}
	}
	return base, o.resumeSkippedReason(ctx, turnIn, res), nil
}

// turnInvocation seeds one follow-up turn: the live claim, so a settle or a
// branch record the turn makes is observed (a `run -s` streams only the new
// turn), and the REMAINING wall-clock budget, so the turns share the
// invocation's one cap instead of each getting a fresh full one.
func (o opencode) turnInvocation(in Invocation, res *Result, deadline time.Time) Invocation {
	in.ResumeClaim = res.Claim
	if !deadline.IsZero() {
		in.MaxSessionWallClock = time.Until(deadline)
	}
	return in
}

// stopTurns records which turn ended the sequence and names it for the operator.
func (o opencode) stopTurns(in Invocation, res *Result, turn int, reason string) {
	res.TurnStopped = turn
	res.TurnStopReason = reason
	if in.Console != nil {
		if reason == "" {
			reason = "an unreported end"
		}
		fmt.Fprintf(in.Console, "!! turn %d ended the sequence (%s) — no later turn will run\n", turn, reason)
	}
}

// chargeTurn puts an errored turn's spend on the Result. A resume re-sends the
// transcript, so even a failed turn is real money; its claim observation folds
// on the same terms as the other resume paths (see resumeBadReport).
func chargeTurn(res *Result, turnRes Result) {
	res.Tokens += turnRes.Tokens
	res.CostUSD += turnRes.CostUSD
	res.Usage.Add(turnRes.Usage)
	res.UsageReported = res.UsageReported || turnRes.UsageReported
	res.CostReported = res.CostReported || turnRes.CostReported
	if turnRes.Untrusted == "" && turnRes.Claim.Names(res.Claim.TaskID) {
		mergeObservation(res, turnRes)
	}
}

// opencodeTurnWallClockMsg is the console line for the one place the turn
// sequence declines to start a turn: the remaining wall-clock budget is under
// the continuation floor, so the turn could only be cap-killed on arrival.
const opencodeTurnWallClockMsg = "!! wall-clock budget exhausted before the next turn — stopping the turn sequence\n"

// overBudget reports whether the invocation's wall-clock budget has less than
// the continuation floor left. A zero deadline means no cap at all.
func overBudget(deadline time.Time) bool {
	return !deadline.IsZero() && time.Until(deadline) < opencodeResumeWallClockFloor
}

// modelLabel names the model for a console line.
func modelLabel(in Invocation) string {
	if m := in.ModelArg(); m != "" {
		return m
	}
	return "the harness default model"
}
