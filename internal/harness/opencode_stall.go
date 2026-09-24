package harness

import (
	"context"
	"fmt"
	"time"
)

// opencode_stall.go — CLA-584: the output-cap stall, and its one resume.
//
// MAK-123 (2026-09-24, makespdf) is the measured failure this answers: asked to
// embed a screenshot in a review deck, the model tried to type a 7,076-char
// base64 string into a tool argument, spent its whole output budget reasoning,
// and emitted nothing. Its final step_finish carried reason "length" with
// output 0 and reasoning 32000; opencode ended the turn and the session stopped
// 11 seconds later without ever moving the task on. All the daemon logged was
// its generic "the phase finished but never moved the task on", which names no
// cause.
//
// This is a DIFFERENT end from the CLA-398 quiet death that CLA-406 resurrects
// (reason "unknown", final step all-zero usage, the stream dropped
// mid-generation) and from opencode's own loop-exit bug. Here the harness ran
// to a normal turn end and reported real usage: the model hit its output
// ceiling. So the response is a resume too, but on its own terms:
//
//   - NO coherence probe. Nothing was lost — the session is alive and its
//     transcript is intact — so probing would only spend the turn the resume
//     exists to save.
//   - ONE resume, never a loop. A plain retry would re-spend the same output
//     budget the same way, so the resume carries a STEER that names what
//     happened and redirects the approach.
//   - A second consecutive "length"/zero-output stop ends the session exactly
//     as it did before this existed. The adapter writes the stall marker
//     (OutputCapReason) onto the Result and the driver's existing paths take
//     over; the marker is what lets the daemon name the end instead of logging
//     a generic hand-off failure.

// opencodeStallSteerPrompt is the steer the single resume carries: what
// happened, do not repeat it, and the specific redirect the MAK-123 shape
// needs. Kept short because the session keeps its whole transcript — everything
// else it needs is already there.
const opencodeStallSteerPrompt = `Your previous step hit the output token limit while thinking and produced no output; ` +
	`you are resumed in place and your transcript is intact. Do not retry the same approach. If you were ` +
	`about to transcribe file contents (for example base64 image data) into a tool argument, do not: write ` +
	`the file to disk and upload it by path instead. Then continue where you left off.`

// opencodeStallWallClockMsg is the console line for the one place the wall-clock
// gate can decline the stall resume: the remaining budget is under the
// continuation floor, so a resume could only be cap-killed on arrival. The
// stall then stands, exactly as if the resume had never existed.
const opencodeStallWallClockMsg = "!! wall-clock budget exhausted before the stall resume - leaving the stall\n"

// outputCapStallReasoning reads the stalled final step's own reasoning-token
// count off a Result, 0 when absent — for the console line and the driver's
// named log line.
func outputCapStallReasoning(res Result) int {
	if res.Raw == nil {
		return 0
	}
	n, _ := res.Raw[OutputCapReasoningKey].(int)
	return n
}

// resumeOutputCapStall performs the one resume the stall earns. It mirrors the
// CLA-406 continuation's rules where they apply and drops the ones that do not:
//
//   - never on an unreadable stream: the marker itself was parsed from the
//     bytes that may have been dropped whole (CLA-262), and the same gate keeps
//     the adapter and the driver's classifications agreeing about what ended;
//   - no session id, no resume — a stream that died before its first event has
//     nothing on opencode's side to continue;
//   - the wall-clock budget is re-checked first, then INHERITED by the resume
//     (MaxSessionWallClock = what remains), so a stall cannot hand the session a
//     fresh full cap the original never had;
//   - the parser is seeded with the dead run's live claim, so any
//     update_task/heartbeat the resumed turn makes is observed on the same
//     terms as the session it belongs to;
//   - the resume's spend is never lost: mergeResume sums it on the success
//     path, and an errored resume charges it explicitly — a resume re-sends the
//     whole transcript, so it is real money even when it fails to help;
//   - a clean resume is merged with mergeResume, so the driver sees ONE session
//     that was stalled and carried on rather than two.
//
// A resume that itself ends "length"/zero-output leaves the merged Result
// carrying the marker (mergeResume recomputes terminal_reason from the
// continuation), and NO further resume is attempted — the second consecutive
// stall is terminal, as designed.
func (o opencode) resumeOutputCapStall(ctx context.Context, in Invocation, res *Result, deadline time.Time) {
	if res.Untrusted != "" {
		return
	}
	sid, _ := resumeTargets(*res)
	if sid == "" {
		return
	}
	if !deadline.IsZero() && time.Until(deadline) < opencodeResumeWallClockFloor {
		if in.Console != nil {
			fmt.Fprint(in.Console, opencodeStallWallClockMsg)
		}
		return
	}
	if in.Console != nil {
		fmt.Fprintf(in.Console, "\n!! output-cap stall detected — resuming session %s once with a steer (reasoning=%d, output=0)\n",
			sid, outputCapStallReasoning(*res))
	}
	contIn := in
	contIn.ResumeClaim = res.Claim
	if !deadline.IsZero() {
		contIn.MaxSessionWallClock = time.Until(deadline)
	}
	contRes, contErr := o.runSession(ctx, contIn, opencodeResumeArgs(in, sid, opencodeStallSteerPrompt))
	if contErr != nil {
		// Spend first, even on the error path — a resume re-sends the whole
		// transcript, so it is real money even when the continuation fails.
		// (On the success path mergeResume sums the spend; charging it here too
		// would count the resume twice.)
		res.Tokens += contRes.Tokens
		res.CostUSD += contRes.CostUSD
		res.UsageReported = res.UsageReported || contRes.UsageReported
		if in.Console != nil {
			fmt.Fprintf(in.Console, "!! stall resume errored (%v) — leaving the stall\n", contErr)
		}
		return
	}
	mergeResume(res, contRes)
	if in.Console != nil {
		fmt.Fprint(in.Console, "!! stall resume done — session continued in place\n")
	}
}
