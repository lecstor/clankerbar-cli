package harness

// opencode_interrupt.go — the ONE resume a silent exit 130 earns.
//
// opencode 2.0.32 has been seen to exit 130 with no error event when a route's
// quota ran out mid-turn, so the stream carried no evidence for the route
// switch; resuming the same session is what surfaces the real error.

import (
	"context"
	"fmt"
	"time"
)

const opencodeInterruptExit = 130

// interruptResumePrompt tells the session what happened without asking it to
// prove anything: the transcript is intact, so only "do not redo the work"
// needs saying.
const interruptResumePrompt = "Your previous turn was interrupted: the opencode process exited 130 without an error. Your transcript is intact. Continue exactly where you left off; do not repeat work that is already done."

// interruptResumeSkipped reports whether this end cannot take an interrupt
// resume. The adapter's own kills set ExitCode -1, so 130 is opencode's own
// exit; a held claim means real work is at stake, not a session that died
// before doing anything.
func (o opencode) interruptResumeSkipped(ctx context.Context, in Invocation, res *Result) bool {
	switch {
	case in.Probe,
		ctx.Err() != nil,
		res.ExitCode != opencodeInterruptExit,
		res.ExitSignal != 0,
		res.Untrusted != "",
		res.ToolLoop != "",
		res.IdleKilled != "",
		o.DetectLimit(*res).Limited,
		!res.Claim.Held():
		return true
	}
	sid, _ := resumeTargets(*res)
	return sid == ""
}

// resumeInterrupted resumes a session that exited 130 silently, ONCE, on the
// route it already ran: the resume exists to surface the error the stream
// missed (a quota error), which the route switch then judges on evidence.
func (o opencode) resumeInterrupted(ctx context.Context, in Invocation, res *Result, deadline time.Time) {
	if o.interruptResumeSkipped(ctx, in, res) {
		return
	}
	if !deadline.IsZero() && time.Until(deadline) < opencodeResumeWallClockFloor {
		if in.Console != nil {
			fmt.Fprint(in.Console, opencodeWallClockExhaustedMsg)
		}
		return
	}
	sid, _ := resumeTargets(*res)
	if in.Console != nil {
		route := in.ModelArg()
		if route == "" {
			route = "the configured model"
		}
		fmt.Fprintf(in.Console, "\n!! opencode exited 130 with no error — resuming session %s once on %s\n", sid, route)
	}
	contIn := in
	contIn.ResumeClaim = res.Claim
	if !deadline.IsZero() {
		contIn.MaxSessionWallClock = time.Until(deadline)
	}
	contRes, contErr := o.runSession(ctx, contIn, opencodeResumeArgs(in, sid, interruptResumePrompt))
	if contErr != nil {
		// Spend first, even on the error path: a resume re-sends the transcript.
		res.Tokens += contRes.Tokens
		res.CostUSD += contRes.CostUSD
		res.Usage.Add(contRes.Usage)
		res.UsageReported = res.UsageReported || contRes.UsageReported
		res.CostReported = res.CostReported || contRes.CostReported
		// An errored resume still ran a real turn, so a claim it recorded
		// describes state this session reached (same guard as the stall).
		if contRes.Untrusted == "" && contRes.Claim.Names(res.Claim.TaskID) {
			mergeObservation(res, contRes)
		}
		if in.Console != nil {
			fmt.Fprintf(in.Console, "!! interrupt resume errored (%v) — leaving the session's end as it was\n", contErr)
		}
		return
	}
	mergeResume(res, contRes)
	if in.Console != nil {
		fmt.Fprintf(in.Console, "!! interrupt resume done — the session ended %s\n", res.ExitString())
	}
}
