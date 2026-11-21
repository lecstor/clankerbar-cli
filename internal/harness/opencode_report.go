package harness

// opencode_report.go — the runner's done-report check, and its bounded resume.

import (
	"context"
	"fmt"
	"time"
)

const (
	// opencodeReportWallClockMsg is the console line for the two places the report resume's wall-clock
	// gate declines: the remaining budget is under the continuation floor, so a resume could only be
	// cap-killed on arrival. The report stays refused and the marker makes the phase fail closed.
	opencodeReportWallClockMsg = "!! wall-clock budget exhausted before the report resume — leaving the report refused\n"

	// opencodeReportResumeAttempts bounds the resume processes ONE refused report earns.
	opencodeReportResumeAttempts = 2
)

// opencodeReportResumeIdle bounds ONE report-resume process for silence. The phase bound is long
// partly because opencode streams a tool part only once it SETTLES, so a running build emits
// nothing; the steer forbids tools here, which is what makes a short bound safe.
var opencodeReportResumeIdle = 3 * time.Minute

// reportResumeIdle resolves the no-output bound for one report resume. An operator's SHORTER phase
// idle is honoured (their stricter dial wins); a phase watchdog that is disabled for the session
// does not disable this one, because the resume is not the session doing work — a silent reply
// here is exactly the failure the bound exists for.
func reportResumeIdle(phaseIdle time.Duration) time.Duration {
	if phaseIdle > 0 && phaseIdle < opencodeReportResumeIdle {
		return phaseIdle
	}
	return opencodeReportResumeIdle
}

// reportSteerPrompt is the steer every report resume carries.
func reportSteerPrompt(err error) string {
	return "Your done report was not accepted by the runner: " + err.Error() + ". " +
		"You are resumed in place and your transcript is intact. " +
		"Do not redo, re-run or change any work and do not call any tools. " +
		"Reply with ONLY the corrected report: a line that is exactly " +
		"=====CLANKERBAR DONE===== followed by the complete JSON object, " +
		"fixing the problem named above and keeping everything else the same."
}

// resumeSkippedReason names the end that forbids resuming this session, or "" when it may be
// resumed. ONE list serves two callers: the done-report resume, which must not talk over an end of
// the adapter's own, and the multi-turn sequence, which must not send a later turn into a session
// that ended badly.
func (o opencode) resumeSkippedReason(ctx context.Context, in Invocation, res *Result) string {
	switch {
	case in.Probe:
		return "probe"
	case ctx.Err() != nil:
		return "cancelled"
	case !res.Claim.Held() && !in.AllowUnclaimed:
		return "claim_not_held"
	case res.Untrusted != "":
		return "untrusted_stream"
	case res.ToolLoop != "":
		return "tool_loop"
	case res.IdleKilled != "":
		return "idle_killed"
	case o.WallClockCapped(*res):
		// The adapter's own cap killed the session, so the remaining budget is not enough to resume into
		// - and the turn sequence must name the kill rather than the exit status it leaves behind.
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

// resumeBadReport runs the runner's report check on the session's final message and, when it
// refuses the report, resumes the session with the check's error as the steer. A second stall, a
// resume process that errors, a reply the runner still cannot apply, or a wall clock with no room
// left to resume all fail closed: the report stands refused, the session's OWN end is left
// untouched, and Result.ReportResumeFailed names what really failed.
func (o opencode) resumeBadReport(ctx context.Context, in Invocation, res *Result, deadline time.Time, delivery *opencodeInbox) {
	if in.CheckReport == nil {
		return
	}
	// The shared guard names a wall-clock kill before the budget check below, so its line would
	// otherwise be skipped here.
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
	// The wall clock is checked only once a resume is owed: its line must not describe a report that
	// passed.
	if !deadline.IsZero() && time.Until(deadline) < opencodeResumeWallClockFloor {
		res.ReportResumeFailed = reportResumeWallClockCause(time.Until(deadline))
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
	// stalled is the first attempt's idle cause, made STICKY: once a resume has gone silent, every
	// later return that leaves the report refused must fail closed on that stall.
	stalled := ""
	for attempt := 1; attempt <= opencodeReportResumeAttempts; attempt++ {
		if !deadline.IsZero() && time.Until(deadline) < opencodeResumeWallClockFloor {
			if stalled != "" {
				res.ReportResumeFailed = stalled
				if in.Console != nil {
					fmt.Fprintf(in.Console, "!! report resume stalled (%s) and the wall-clock budget is now exhausted — failing closed on the stall; the session's own end stands\n", stalled)
				}
			} else {
				res.ReportResumeFailed = reportResumeWallClockCause(time.Until(deadline))
				if in.Console != nil {
					fmt.Fprint(in.Console, opencodeReportWallClockMsg)
				}
			}
			return
		}
		contIn := in
		contIn.ResumeClaim = res.Claim
		contIn.MaxSessionIdle = reportResumeIdle(in.MaxSessionIdle)
		if !deadline.IsZero() {
			contIn.MaxSessionWallClock = time.Until(deadline)
		}
		contRes, contErr := o.runSession(ctx, contIn, opencodeResumeArgs(in, sid, reportSteerPrompt(checkErr)), delivery)
		if contErr != nil {
			chargeReportResume(res, contRes)
			// The resume never ran a full reply, so the report is still the refused one while the session's
			// own end is intact.
			res.ReportResumeFailed = "the report resume errored: " + contErr.Error()
			if in.Console != nil {
				fmt.Fprintf(in.Console, "!! report resume errored (%v) — leaving the report as it was; the session's own end stands\n", contErr)
			}
			return
		}
		if contRes.IdleKilled != "" {
			// The resume produced nothing before its own short idle bound.
			chargeReportResume(res, contRes)
			if attempt < opencodeReportResumeAttempts {
				stalled = contRes.IdleKilled
				if in.Console != nil {
					fmt.Fprintf(in.Console, "!! report resume stalled (%s) — retrying it with the same steer\n", contRes.IdleKilled)
				}
				continue
			}
			res.ReportResumeFailed = contRes.IdleKilled
			if in.Console != nil {
				fmt.Fprintf(in.Console, "!! report resume stalled %d times (%s) — leaving the report refused; the session's own end stands\n",
					attempt, contRes.IdleKilled)
			}
			return
		}
		// The resume ran. Judge BEFORE merging: the runner applies a report only on an orderly end or a
		// recovered one, so a reply that still fails the check, or an end no report is ever read on (a
		// cap kill, a non-zero exit with nothing recovered, a tool loop, an untrusted capture), produced
		// nothing the runner can act on.
		verr := in.CheckReport(contRes.FinalMessage)
		if verr != nil || !reportResumeUsable(contRes) {
			chargeReportResume(res, contRes)
			res.ReportResumeFailed = reportResumeUnusable(contRes, verr)
			if in.Console != nil {
				fmt.Fprintf(in.Console, "!! report resume done — %s; leaving the report as it was (the session's own end stands)\n",
					res.ReportResumeFailed)
			}
			return
		}
		mergeResume(res, contRes)
		if in.Console != nil {
			fmt.Fprint(in.Console, "!! report resume done — the corrected report passes the check\n")
		}
		return
	}
}

// reportResumeUsable reports whether the runner would APPLY a completed resume's report: it must
// end the way a report is read on (a clean exit, or a non-zero one the harness itself recovered
// from) and its capture must be trustworthy.
func reportResumeUsable(contRes Result) bool {
	if contRes.Untrusted != "" || contRes.ToolLoop != "" || contRes.IdleKilled != "" {
		return false
	}
	return contRes.ExitCode == 0 || RecoveredErrors(contRes) > 0
}

// reportResumeUnusable names why a completed resume left no report the runner can act on. The cause
// rides Result.ReportResumeFailed, which the driver prints as "failed re-emitting its done report
// (<cause>)".
func reportResumeUnusable(contRes Result, verr error) string {
	cause := "the report resume produced no report the runner can apply"
	if verr != nil {
		cause += " (the report still fails the check: " + verr.Error() + ")"
	}
	switch {
	case contRes.Untrusted != "":
		cause += " (its capture was untrusted)"
	case contRes.ToolLoop != "":
		cause += " (it was killed for repeating a failing tool call)"
	case contRes.ExitCode != 0 && RecoveredErrors(contRes) == 0:
		cause += " (it ended " + contRes.ExitString() + ")"
	}
	return cause
}

// reportResumeWallClockCause is the marker for a refused report whose resume the phase's wall clock
// left no room to start. No process ran, but the report is still refused, so the phase must fail
// closed and be retried under the claim rather than advance on it.
func reportResumeWallClockCause(remaining time.Duration) string {
	return fmt.Sprintf("the phase's wall-clock budget left %s before the report resume, under the %s floor",
		remaining.Round(time.Second), opencodeResumeWallClockFloor)
}

// chargeReportResume charges one report-resume process's spend and folds its CLAIM observation,
// without adopting its process outcome.
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
