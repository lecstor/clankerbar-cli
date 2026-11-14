package harness

// opencode_report.go — the runner's done-report check, and its bounded resume.
//
// A runner-claimed session ends with a marker line and a JSON object the runner
// parses. A report the runner refuses normally ends the phase fail-closed and
// the session's work is wasted, so a refused report earns the same steer resume
// the output-cap stall does: the turn is replayable and the transcript is
// intact, only the report needs re-emitting.

import (
	"context"
	"fmt"
	"time"
)

const (
	// opencodeReportWallClockMsg is the console line for the one place the report
	// resume's wall-clock gate declines: the remaining budget is under the
	// continuation floor, so a resume could only be cap-killed on arrival.
	opencodeReportWallClockMsg = "!! wall-clock budget exhausted before the report resume — leaving the report as it was\n"

	// opencodeReportResumeAttempts bounds the resume processes ONE refused report
	// earns. A silent provider hang is usually transient, so a stalled resume is
	// worth ONE fresh attempt; a second stall is the runner's to fail closed on,
	// exactly as a single refusal was before CLA-877.
	opencodeReportResumeAttempts = 2
)

// opencodeReportResumeIdle bounds ONE report-resume process for silence. The
// resume is a no-tools reply that re-emits one JSON object, so it should answer
// in seconds; the phase's own idle limit is tuned for a working session and was
// 15 minutes in the CLA-868 incident, so it let a hung resume hold the whole
// phase open until the watchdog killed it — the resume's silence then read as a
// dead session. Three minutes leaves room for a slow provider without ever
// being the phase's own bound.
//
// A var purely so the tests can shrink it: the production value is what they
// test against.
var opencodeReportResumeIdle = 3 * time.Minute

// reportResumeIdle resolves the no-output bound for one report resume. An
// operator's SHORTER phase idle is honoured (their stricter dial wins); a phase
// watchdog that is disabled for the session does not disable this one, because
// the resume is not the session doing work — a silent reply here is exactly the
// failure the bound exists for.
func reportResumeIdle(phaseIdle time.Duration) time.Duration {
	if phaseIdle > 0 && phaseIdle < opencodeReportResumeIdle {
		return phaseIdle
	}
	return opencodeReportResumeIdle
}

// reportSteerPrompt is the steer every report resume carries. It names the
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

// resumeSkippedReason names the end that forbids resuming this session, or ""
// when it may be resumed. ONE list serves two callers: the done-report resume,
// which must not talk over an end of the adapter's own, and the multi-turn
// sequence, which must not send a later turn into a session that ended badly.
// The reasons are the Result.TurnStopReason vocabulary, plus "probe", which
// only the report resume can see (a probe never reaches the turn sequence).
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
	case o.WallClockCapped(*res):
		// The adapter's own cap killed the session, so the remaining budget is
		// not enough to resume into - and the turn sequence must name the kill
		// rather than the exit status it leaves behind.
		return "wall_clock"
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
// and, when it refuses the report, resumes the session with the check's error as
// the steer. A resume that stalls on a silent provider earns ONE fresh attempt
// with the same steer (CLA-877): a hang is usually transient, and the alternative
// is losing the phase — and every finished, pushed commit in it — to a bound
// tuned for a working session. A second stall fails closed: the report stands
// refused, and the session's OWN end is left untouched with
// Result.ReportResumeStalled naming what really failed.
//
// mergeResume replaces FinalMessage with the continuation's, so a corrected
// report is what the runner reads off this Result.
func (o opencode) resumeBadReport(ctx context.Context, in Invocation, res *Result, deadline time.Time) {
	if in.CheckReport == nil {
		return
	}
	// The shared guard names a wall-clock kill before the budget check below, so
	// its line would otherwise be skipped here. Say why the refused report was
	// left alone; only when a resume was actually owed, so the line never
	// describes a report that passed.
	if reason := o.resumeSkippedReason(ctx, in, res); reason != "" {
		if reason == "wall_clock" && in.CheckReport(res.FinalMessage) != nil && in.Console != nil {
			fmt.Fprint(in.Console, opencodeReportWallClockMsg)
		}
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
		fmt.Fprintf(in.Console, "\n!! done report refused (%v) — resuming session %s up to %d times to re-emit it (idle bound %s each)\n",
			checkErr, sid, opencodeReportResumeAttempts, reportResumeIdle(in.MaxSessionIdle))
	}
	for attempt := 1; attempt <= opencodeReportResumeAttempts; attempt++ {
		if !deadline.IsZero() && time.Until(deadline) < opencodeResumeWallClockFloor {
			if in.Console != nil {
				fmt.Fprint(in.Console, opencodeReportWallClockMsg)
			}
			return
		}
		contIn := in
		contIn.ResumeClaim = res.Claim
		contIn.MaxSessionIdle = reportResumeIdle(in.MaxSessionIdle)
		if !deadline.IsZero() {
			contIn.MaxSessionWallClock = time.Until(deadline)
		}
		contRes, contErr := o.runSession(ctx, contIn, opencodeResumeArgs(in, sid, reportSteerPrompt(checkErr)))
		if contErr != nil {
			chargeReportResume(res, contRes)
			if in.Console != nil {
				fmt.Fprintf(in.Console, "!! report resume errored (%v) — leaving the report as it was\n", contErr)
			}
			return
		}
		if contRes.IdleKilled != "" {
			// The resume produced nothing before its own short idle bound. Do
			// NOT merge its process outcome: the session's run did not end this
			// way, and adopting it would brand the whole phase dead (CLA-868).
			chargeReportResume(res, contRes)
			if attempt < opencodeReportResumeAttempts {
				if in.Console != nil {
					fmt.Fprintf(in.Console, "!! report resume stalled (%s) — retrying it with the same steer\n", contRes.IdleKilled)
				}
				continue
			}
			res.ReportResumeStalled = contRes.IdleKilled
			if in.Console != nil {
				fmt.Fprintf(in.Console, "!! report resume stalled %d times (%s) — leaving the report refused; the session's own end stands\n",
					attempt, contRes.IdleKilled)
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
		return
	}
}

// chargeReportResume charges one report-resume process's spend and folds its
// CLAIM observation, without adopting its process outcome. A stalled or errored
// resume re-sends the whole transcript, so its spend is real even when it helped
// nothing (mergeResume sums the success path instead), and a turn that ran may
// have touched the claim — under the same trusted-capture, same-task guard the
// stall and errored paths use.
func chargeReportResume(res *Result, contRes Result) {
	res.Tokens += contRes.Tokens
	res.CostUSD += contRes.CostUSD
	res.Usage.Add(contRes.Usage)
	res.UsageReported = res.UsageReported || contRes.UsageReported
	res.CostReported = res.CostReported || contRes.CostReported
	if contRes.Untrusted == "" && contRes.Claim.Names(res.Claim.TaskID) {
		mergeObservation(res, contRes)
	}
}
