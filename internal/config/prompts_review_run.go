package config

// prompts_review_run.go — the brief for `clankerbar review run` (CLA-870).
//
// The command reviews a local repository's base..head outside any task, claim
// or plane: an eval runner scores the REAL driver, and a human uses the same
// entry point later. The brief is therefore the review-only brief's substance
// with every task-shaped fact removed — no task id, no done-when bar, no
// record, no terminal hand-over — while the reading discipline, the gate scope,
// the MUST bar and the done report stay the phase's own constants. That is what
// keeps the command's review the same review the phase runs.

import "strings"

// ReviewRunBriefOptions is the context a standalone review carries. Base and
// Head are resolved commits; Title and Description are the optional human
// context an eval PR carries in place of a task body.
type ReviewRunBriefOptions struct {
	Base        string
	Head        string
	Title       string
	Description string
}

// reviewRunMUSTBarGrades is reviewMUSTBarGrades for a review with no done-when
// bar. Two clauses name the task's doneWhen as an input this review does not
// have, so they are removed rather than left to reach a session as an
// instruction it cannot follow; every other grading rule — the evidence kinds,
// the seventh kind, and the not-a-MUST list — is the phase's verbatim. A test
// pins that both replacements landed, because a silently failed Replace would
// hand the session a bar that names a bar it was told does not exist.
var reviewRunMUSTBarGrades = strings.NewReplacer(
	"doneWhen - quote the clause of the task's doneWhen the change violates; ", "",
	"missing tests the doneWhen does not require (except untested)", "missing tests the change does not require (except untested)",
).Replace(reviewMUSTBarGrades)

// reviewRunGateScope is reviewGateScope for a review with no phase sequence.
// Three sentences name phases this command does not have - the implement
// phase's self-verification, the fix and verify phases, and the shared panel
// worktree - and left in they would tell a standalone session to rely on a
// checkpoint that does not exist. The discipline itself is the phase's verbatim
// (no full gate runs, targeted probes only, the probe's output as the finding's
// evidence), which is what keeps the command's review comparable. A test pins
// every replacement, for the same reason reviewRunMUSTBarGrades' does.
var reviewRunGateScope = strings.NewReplacer(
	"The implement phase self-verified at this tip and the fix and verify phases run the gates again over what they change, so do NOT run",
	"No earlier phase has vouched for this change's gates, and a review's value is the reading rather than a second gate run, so do NOT run",
	"that is where a defect the implementer's own gates missed is found",
	"that is where a defect a passing gate does not show is found",
	"The panel shares this one worktree, so an artifact left behind is the next member's - and the fix phase's - dirty tree.",
	"This worktree is temporary and this command's own, so a leftover artifact reaches no one else.",
).Replace(reviewGateScope)

// reviewRunDoneReport is the report paragraph for the standalone command. It
// reuses runnerReviewReportFields — the same fields the review phase's report
// takes, so ParseDone and ValidateReview read it unchanged — and says who
// consumes it: not the runner writing plane records, but the command writing
// JSON.
var reviewRunDoneReport = " END WITH A DONE REPORT: the LAST thing in your final message is a line that is " +
	"exactly `" + DoneMarker + "` - unindented and outside any code fence - followed by ONE JSON object and " +
	"nothing after it. Fields: " + runnerReviewReportFields + ". `reviewedTip` must be the head commit the " +
	"context above names. The command parses this report and writes it as JSON; a session that ends without a " +
	"valid report has produced nothing, and the command exits non-zero."

// ReviewRunBrief renders the brief for a standalone review of base..head.
//
// It carries no placeholders: the command does not run the phase driver's
// substitution pass, so a placeholder here would reach the session standing.
func ReviewRunBrief(o ReviewRunBriefOptions) string {
	var b strings.Builder
	b.WriteString("You are reviewing ONE change in the worktree you were started in. It is a detached checkout " +
		"at the head commit, on no branch. There is NO claimed task, NO done-when bar and NO plane record: the " +
		"change itself is the whole subject of this review.\n\n")
	b.WriteString("## The change\n")
	b.WriteString("- Base: `" + o.Base + "`\n")
	b.WriteString("- Head: `" + o.Head + "`\n")
	if title := strings.TrimSpace(o.Title); title != "" {
		b.WriteString("\n### Title\n" + title + "\n")
	}
	if description := strings.TrimSpace(o.Description); description != "" {
		b.WriteString("\n### Description\n" + description + "\n")
	}
	b.WriteString("\nRead the diff with `git diff " + o.Base + ".." + o.Head + "` (or " + o.Base +
		"..HEAD), read the code around every changed region in the worktree, and compare changed definitions " +
		"with their callers and with sibling code that does the same job. The head commit is checked out, so " +
		"`git rev-parse HEAD` is `" + o.Head + "`.\n")
	b.WriteString(reviewRunGateScope)
	b.WriteString("REVIEW ONLY: do not edit, create or delete a source file, do not commit, do not push, and do " +
		"not create or switch a branch or worktree - not even to fix a typo. Your findings are the deliverable; " +
		"nothing in this command applies them.")
	b.WriteString("\n\nA MUST here cannot cite a task's done-when bar, because there is none: a MUST is a " +
		"correctness or safety defect, a security problem, or a repo gate (a test, typecheck, lint, build or CI " +
		"check) that fails or would fail." + reviewRunMUSTBarGrades + "\n")
	b.WriteString(reviewRunDoneReport)
	return b.String()
}
