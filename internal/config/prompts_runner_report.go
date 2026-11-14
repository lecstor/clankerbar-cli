package config

import (
	"strconv"
	"strings"
)

// The done report is how a runner-claimed session hands its work over: it writes
// no plane record, and ends its final message with one JSON object after
// DoneMarker. The runner turns that object into the task's records and
// attributes them to the session's harness and model. A session that ends
// without a valid report has recorded nothing.

// runnerNoWrites is the write half of the runner's ownership, appended to
// runnerForbidden. The plane does not check who writes a record, so a session's
// write silently stands in for the runner's own - and the runner attributes
// every record from the done report instead.
const runnerNoWrites = " NEVER write to the task: no record_progress (phaseDone included), no record_review, no " +
	"record_decision, no settle_task, no submit_for_review and no release_task. The runner writes every record " +
	"from your done report and attributes it to this session's harness and model. Reading is fine: get_task and " +
	"get_project_policy are yours to call."

// runnerDoneReport renders the report's format paragraph. fields names only the
// fields this phase's report uses, so no brief asks for an answer its phase has
// no business giving.
func runnerDoneReport(fields string) string {
	return " END WITH A DONE REPORT and record nothing yourself: the LAST thing in your final message is a line " +
		"that is exactly `" + DoneMarker + "` - unindented and outside any code fence - followed by ONE JSON " +
		"object and nothing after it. Fields: " + fields + ". The runner writes every record from this report and " +
		"attributes it to this session's harness and model. A session that ends without a valid report has " +
		"recorded nothing, and the runner treats the phase as failed. A declared handoff (below) replaces the " +
		"report: end with one or the other, never both."
}

// runnerReportReminder repeats the report contract as the brief's LAST line.
// The full paragraph sits mid-brief; a session that reads the whole brief and
// then writes a prose summary (the CLA-868 trigger) misses it, so the one
// instruction it cannot complete the phase without gets the last word. It
// defers to a declared handoff, which replaces the report.
const runnerReportReminder = " ONE LAST TIME: unless you are declaring a handoff, the LAST thing in your final " +
	"message is the `" + DoneMarker + "` line and its ONE JSON object, with nothing after them. A session that " +
	"ends without them has recorded nothing."

// runnerDoneFindingShape is the finding object the review and verify reports
// carry, in the same shape the plane's durable record takes. The limits are the
// plane's bounds: it refuses an over-limit report whole, so a session cannot
// discover them by trying.
var runnerDoneFindingShape = "{severity: \"must\" | \"should\" | \"nit\", summary, evidenceKind, evidence, file, " +
	"line, kind, requiredFix} - each a plain string within its limit, counted as the plane counts them, where a " +
	"character outside the basic multilingual plane (an emoji, say) counts as two: summary at most " +
	strconv.Itoa(boundFindingSummary) + ", requiredFix at most " + strconv.Itoa(boundFindingRequiredFix) +
	", evidence at most " + strconv.Itoa(boundFindingEvidence) + ", evidenceKind " +
	strconv.Itoa(boundFindingEvidenceKind) + ", file " + strconv.Itoa(boundFindingFile) + ", kind " +
	strconv.Itoa(boundFindingKind) + "; a finding that needs more says the rest in evidence, never past a limit, " +
	"because an over-limit report is refused whole"

// runnerFindingUpdatesField is the per-finding update list the fix and verify
// reports carry in place of the record_review/findingUpdates call. The ids are
// the ones the sealed record already assigned.
var runnerFindingUpdatesField = "findingUpdates (one entry per finding, by the plane-assigned id the record gave " +
	"you: {findingId, status: \"fixed\", fixCommit: <the commit SHA>} for a fixed finding; {findingId, status: " +
	"\"refuted\", refutedReason: <the evidence that refutes it, at most " +
	strconv.Itoa(boundFindingRefutedReason) + " characters>} for a MUST you did not fix; {findingId, status: " +
	"\"filed\", filedTaskRef: <the follow-up's ref alone, at most " +
	strconv.Itoa(boundFindingFiledTaskRef) + " characters>} for a code-needing finding you dispatched after " +
	"create_task; an empty array when there is nothing to update)"

// runnerRiskField is the risk declaration the runner needs to hand the task over
// with submit_for_review: an omitted risk is UNDECLARED and buys no per-batch
// auto-approval.
const runnerRiskField = "risk { touches: [...] } - the project's risk classes the change touches, from " +
	"get_project_policy's risk_vocabulary, and [] when none; an omitted risk is UNDECLARED and buys no per-batch " +
	"auto-approval"

// The per-phase field lists. Each names only what that phase decides.
const runnerImplementReportFields = "summary (what changed), tests (what you actually ran and saw), and park " +
	"(only when the merged tip supersedes the task: why, naming the commit or the decision that supersedes it)"

var runnerReviewReportFields = "verdict (\"pass\" with no findings, or \"findings\"), reviewedTip (the SHA " +
	"`git rev-parse HEAD` shows, which must be the tip the context block names), findings (at most 20: keep every " +
	"MUST, then the most important of the rest, and put the count you left out in omitted - each finding is " +
	runnerDoneFindingShape + ", and a MUST carries evidenceKind, one of the bar's kinds, and evidence), " +
	"omitted (an integer count, 0 when you left nothing out - never a list), " +
	"and notes (optional: anything the operator should read beside the findings)"

var runnerFixReportFields = "summary (what changed), tests (what you actually ran and saw), " +
	runnerFindingUpdatesField + ", notes (the reason for any SHOULD or NIT you did not fix), and park (only " +
	"when the review record is missing or its reviewedTip is not HEAD: what is wrong, with nothing changed)"

var runnerFixLastReportFields = runnerFixReportFields + ", and " + runnerRiskField

var runnerVerifyReportFields = "verdict (\"pass\" when this round raises no new findings, or \"findings\" when it " +
	"does), reviewedTip (the SHA `git rev-parse HEAD` shows), findings (the NEW MUSTs this round raises and " +
	"nothing else - only within the fix delta and its regression surface, at most 20, each one " +
	runnerDoneFindingShape + " carrying evidenceKind and evidence, and the count you left out in omitted), " +
	"omitted (an integer count, 0 when you left nothing out - never a list), " + runnerFindingUpdatesField +
	", notes (why any finding is still open, and the reason you " +
	"state for a full second pass), and tests (what you actually ran and saw while verifying)"

// RunnerDoneReportFor is the report-format paragraph of a runner brief, and ""
// for anything else. A handoff successor is handed the predecessor's own
// successor text instead of a brief, so this is the only way the report contract
// reaches it.
func RunnerDoneReportFor(brief string) string {
	switch brief {
	case RunnerImplementBrief():
		return runnerDoneReport(runnerImplementReportFields)
	case RunnerReviewOnlyBrief():
		return runnerDoneReport(runnerReviewReportFields)
	case RunnerFixBrief():
		return runnerDoneReport(runnerFixLastReportFields)
	case RunnerFixVerifyBrief():
		return runnerDoneReport(runnerFixReportFields)
	case RunnerVerifyBrief():
		return runnerDoneReport(runnerVerifyReportFields)
	}
	return ""
}

// runnerFixGradingRule is fixGradingRule with "in your outcome" pointed at the
// done report: a runner-claimed fixer writes no outcome, so a refutation or a
// not-done reason it put anywhere else would never reach the task.
var runnerFixGradingRule = strings.NewReplacer(
	"evidence recorded in your outcome", "evidence in the finding's refutedReason",
	"record NITs and the SHOULDs you do not fix in your outcome as not done", "put NITs and the SHOULDs you do not fix in the report's notes as not done",
).Replace(fixGradingRule)
