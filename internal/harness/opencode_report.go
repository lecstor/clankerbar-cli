package harness

// opencode_report.go — the runner's done-report check, and its one resume.
//
// A runner-claimed session ends with a marker line and a JSON object the runner
// parses. A report the runner refuses normally ends the phase fail-closed and
// the session's work is wasted, so a refused report earns the same single
// steer resume the output-cap stall does: the turn is replayable and the
// transcript is intact, only the report needs re-emitting.

import (
	"context"
	"fmt"
	"time"
)

// opencodeReportWallClockMsg is the console line for the one place the report
// resume's wall-clock gate declines: the remaining budget is under the
// continuation floor, so a resume could only be cap-killed on arrival.
const opencodeReportWallClockMsg = "!! wall-clock budget exhausted before the report resume — leaving the report as it was\n"

// reportSteerPrompt is the steer the single report resume carries. It names the
// runner's own error, because that is the only statement of what to fix, and
// forbids tools so the resume cannot redo or re-run the work it is correcting.
func reportSteerPrompt(err error) string {
	return "Your done report was not accepted by the runner: " + err.Error() + ". " +
		"You are resumed in place and your transcript is intact. " +
		"Do not redo, re-run or change any work and do not call any tools. " +
		"Reply with ONLY the corrected report: a line that is exactly " +
		"=====CLANKERBAR DONE===== followed by the complete JSON object, " +
		"fixing the problem named above and keeping everything else the same."
}

// reportResumeSkipped reports whether this Result cannot take a report resume.
// It is resumeSkippedReason's yes/no form; see that function for the ends it
// names.
func (o opencode) reportResumeSkipped(ctx context.Context, in Invocation, res *Result) bool {
	return o.resumeSkippedReason(ctx, in, res) != ""
}

// resumeSkippedReason names the end that forbids resuming this session, or ""
// when it may be resumed. ONE list serves two callers: the done-report resume,
// which must not talk over an end of the adapter's own, and the multi-turn
// sequence, which must not send a later turn into a session that ended badly.
// The reasons are the Result.TurnStopReason vocabulary.
//
// A claim that is not held is one of those ends: the runner writes no record
// under a claim the session does not hold, so there is nothing to re-emit. A
// runner phase is seeded with in.ResumeClaim, so Held() is true whenever the
// runner would apply the report.
func (o opencode) resumeSkippedReason(ctx context.Context, in Invocation, res *Result) string {
	switch {
	case in.Probe:
		return "probe"
	case ctx.Err() != nil:
		return "cancelled"
	case !res.Claim.Held():
		return "claim_not_held"
	case res.Untrusted != "":
		return "untrusted_stream"
	case res.ToolLoop != "":
		return "tool_loop"
	case res.IdleKilled != "":
		return "idle_killed"
	case o.OutputCapNoOutput(*res):
		return "output_cap_stall"
	case o.ZeroUsageUnknown(*res):
		return "quiet_death"
	case o.DetectLimit(*res).Limited:
		return "usage_limit"
	case res.ExitCode != 0 && RecoveredErrors(*res) == 0:
		return "nonzero_exit"
	}
	if sid, _ := resumeTargets(*res); sid == "" {
		return "no_session"
	}
	return ""
}

// resumeBadReport runs the runner's report check on the session's final message
// and, when it refuses the report, resumes the session ONCE with the check's
// error as the steer. Exactly one resume, never a loop: a second refusal is the
// runner's to fail closed on, exactly as it was before this existed.
//
// mergeResume replaces FinalMessage with the continuation's, so a corrected
// report is what the runner reads off this Result.
func (o opencode) resumeBadReport(ctx context.Context, in Invocation, res *Result, deadline time.Time) {
	if in.CheckReport == nil || o.reportResumeSkipped(ctx, in, res) {
		return
	}
	checkErr := in.CheckReport(res.FinalMessage)
	if checkErr == nil {
		return
	}
	// The wall clock is checked only once a resume is owed: its line must not
	// describe a report that passed.
	if !deadline.IsZero() && time.Until(deadline) < opencodeResumeWallClockFloor {
		if in.Console != nil {
			fmt.Fprint(in.Console, opencodeReportWallClockMsg)
		}
		return
	}
	sid, _ := resumeTargets(*res)
	if in.Console != nil {
		fmt.Fprintf(in.Console, "\n!! done report refused (%v) — resuming session %s once to re-emit it\n", checkErr, sid)
	}
	contIn := in
	contIn.ResumeClaim = res.Claim
	if !deadline.IsZero() {
		contIn.MaxSessionWallClock = time.Until(deadline)
	}
	contRes, contErr := o.runSession(ctx, contIn, opencodeResumeArgs(in, sid, reportSteerPrompt(checkErr)))
	if contErr != nil {
		// Spend first, even on the error path: a resume re-sends the whole
		// transcript. mergeResume sums it on the success path instead.
		res.Tokens += contRes.Tokens
		res.CostUSD += contRes.CostUSD
		res.Usage.Add(contRes.Usage)
		res.UsageReported = res.UsageReported || contRes.UsageReported
		res.CostReported = res.CostReported || contRes.CostReported
		// An errored resume still ran a real turn: a claim or branch it
		// recorded describes state this session reached (same guard as the stall).
		if contRes.Untrusted == "" && contRes.Claim.Names(res.Claim.TaskID) {
			mergeObservation(res, contRes)
		}
		if in.Console != nil {
			fmt.Fprintf(in.Console, "!! report resume errored (%v) — leaving the report as it was\n", contErr)
		}
		return
	}
	mergeResume(res, contRes)
	verr := in.CheckReport(res.FinalMessage)
	// The driver applies a report only on an orderly end or a recovered one, so
	// a corrected report on any other end is text nothing will ever read.
	usable := res.ExitCode == 0 || RecoveredErrors(*res) > 0
	if in.Console == nil {
		return
	}
	switch {
	case verr != nil:
		fmt.Fprintf(in.Console, "!! report resume done — the report still fails the check (%v); the runner will fail closed\n", verr)
	case !usable:
		fmt.Fprintf(in.Console, "!! report resume done — the corrected report passes the check, but the resumed session ended %s, so the runner will not apply it\n", res.ExitString())
	default:
		fmt.Fprint(in.Console, "!! report resume done — the corrected report passes the check\n")
	}
}
