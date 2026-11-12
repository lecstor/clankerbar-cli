package config

import (
	"strings"
	"testing"
)

// runnerBriefs lists every runner-claimed brief by name, with the prohibition
// block it carries. Each is started by a runner that holds the claim and owns
// the lifecycle around it - so the prohibitions and the removed plumbing are the
// same for all of them, except that the close-out may inspect and repair CI and
// still writes its own close-out record.
func runnerBriefs() []struct {
	name, brief, forbidden string
	doneReport             bool
} {
	return []struct {
		name, brief, forbidden string
		doneReport             bool
	}{
		{"implement", RunnerImplementBrief(), runnerForbidden, true},
		{"close-out", RunnerCloseOutBrief(), runnerCloseOutForbidden, false},
		{"review-only", RunnerReviewOnlyBrief(), runnerForbidden, true},
		{"fix", RunnerFixBrief(), runnerForbidden, true},
		{"fix-verify", RunnerFixVerifyBrief(), runnerForbidden, true},
		{"verify", RunnerVerifyBrief(), runnerForbidden, true},
	}
}

func TestRunnerBriefsForbidTheRunnersMechanics(t *testing.T) {
	for _, tc := range runnerBriefs() {
		if !strings.Contains(tc.brief, tc.forbidden) {
			t.Errorf("the %s brief does not carry the runner's prohibition block", tc.name)
		}
		// None of these belong in a runner-claimed session: the runner holds and
		// renews the lease, the context block carries the decisions, no session
		// fetches a skill page, and the runner already put it in the worktree.
		for _, banned := range []string{
			"heartbeat(",
			"includeDecisions",
			"clankerbar.com/skills",
			"Work in the worktree for branch",
		} {
			if strings.Contains(tc.brief, banned) {
				t.Errorf("the %s brief still says %q; the runner owns that step", tc.name, banned)
			}
		}
		// next_task appears once, inside the prohibition: nowhere does the brief
		// send the session looking for other work.
		if n := strings.Count(tc.brief, "next_task"); n != 1 {
			t.Errorf("the %s brief mentions next_task %d times, want only the prohibition", tc.name, n)
		}
	}
}

// Every runner phase but the close-out records nothing: it ends with the done
// report and the runner writes the records from it. The close-out keeps its own
// release_task hand-back, so it is the one brief that may still instruct a
// write.
func TestRunnerBriefsEndWithADoneReportAndNoWrites(t *testing.T) {
	for _, tc := range runnerBriefs() {
		if !tc.doneReport {
			if strings.Contains(tc.brief, DoneMarker) {
				t.Errorf("the %s brief carries a done report; that phase still writes its own record", tc.name)
			}
			continue
		}
		if !strings.Contains(tc.brief, DoneMarker) {
			t.Errorf("the %s brief never names the done marker %q", tc.name, DoneMarker)
		}
		if !strings.Contains(tc.brief, runnerNoWrites) {
			t.Errorf("the %s brief does not carry the runner's write ban", tc.name)
		}
		// The ban itself names the calls it forbids, so it is removed before the
		// scan: what must not survive is a brief INSTRUCTING one of them.
		body := strings.Replace(tc.brief, runnerNoWrites, "", 1)
		for _, banned := range []string{
			"record_progress(taskId",
			"record_review(taskId",
			"settle_task(taskId",
			"submit_for_review(taskId",
			"record_decision",
		} {
			if strings.Contains(body, banned) {
				t.Errorf("the %s brief still instructs %q; the runner writes that record from the done report", tc.name, banned)
			}
		}
		// The only write a fix session keeps is the follow-up task it files.
		if strings.Contains(body, runnerFixCreateTask) && tc.name != "fix" && tc.name != "fix-verify" {
			t.Errorf("the %s brief allows create_task; only a fix phase files a follow-up", tc.name)
		}
	}
}

// The close-out is the exception: it repairs a red check and hands the task back
// itself, so it must keep release_task and checkGreen.
func TestRunnerCloseOutBriefKeepsItsHandBack(t *testing.T) {
	brief := RunnerCloseOutBrief()
	for _, want := range []string{"`release_task`", "`checkGreen`"} {
		if !strings.Contains(brief, want) {
			t.Errorf("the runner close-out brief no longer says %s:\n%s", want, brief)
		}
	}
	if strings.Contains(brief, runnerNoWrites) {
		t.Error("the runner close-out brief carries the write ban it must not: it writes its own close-out record")
	}
}

// Every brief still ends at handoffGuidance: a declared handoff is the one way a
// session ends without the phase's own ending.
func TestRunnerBriefsEndAtHandoffGuidance(t *testing.T) {
	for _, tc := range runnerBriefs() {
		if !strings.HasSuffix(tc.brief, handoffGuidance) {
			t.Errorf("the %s brief does not end with the handoff guidance", tc.name)
		}
	}
}

func TestRunnerPhaseBriefsStartAtAPreparedTip(t *testing.T) {
	want := "You were started in the task worktree at tip " + ReviewedTipPlaceholder
	for _, tc := range []struct{ name, brief string }{
		{"review-only", RunnerReviewOnlyBrief()},
		{"fix", RunnerFixBrief()},
		{"fix-verify", RunnerFixVerifyBrief()},
		{"verify", RunnerVerifyBrief()},
	} {
		if !strings.Contains(tc.brief, want) {
			t.Errorf("the %s brief does not tell the session which worktree and tip it starts at", tc.name)
		}
	}
}

// The review's findings are the deliverable, but the session no longer renders
// the block or seals the record: it reports, and the runner writes both.
func TestRunnerReviewOnlyBriefReportsInsteadOfRecording(t *testing.T) {
	brief := RunnerReviewOnlyBrief()
	for _, want := range []string{
		"verdict",
		"reviewedTip",
		"findings",
		"omitted",
		reviewMUSTBarGrades,
		"do NOT write the `" + ReviewRecordHeading + "` block",
		"do NOT call record_review",
		"CHECKPOINT: keep holding, do NOT commit, push or open a PR",
		"report it as a MUST finding of kind failure",
		DoneMarker,
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("the runner review-only brief never says %q:\n%s", want, brief)
		}
	}
	if strings.Contains(brief, reviewOnlyRecordShape) || strings.Contains(brief, reviewOnlyTerminalStep) {
		t.Error("the runner review-only brief still carries the shipped record shape or terminal step")
	}
}

// The fix phase's per-finding updates and its follow-up tasks leave in the
// report; the runner sends them and appends the outcome itself.
func TestRunnerFixBriefsReportTheirFindingUpdates(t *testing.T) {
	for _, tc := range []struct{ name, brief string }{
		{"fix", RunnerFixBrief()},
		{"fix-verify", RunnerFixVerifyBrief()},
	} {
		for _, want := range []string{
			"create_task",
			"findingUpdates",
			"findingId",
			"fixCommit",
			"refutedReason",
			"filedTaskRef",
			"### Fixes",
			DoneMarker,
		} {
			if !strings.Contains(tc.brief, want) {
				t.Errorf("the runner %s brief never says %q:\n%s", tc.name, want, tc.brief)
			}
		}
	}
	if strings.Contains(runnerFixCore, "settle_task(") || strings.Contains(runnerFixCore, "record_review(") {
		t.Errorf("runnerFixCore still writes its own records:\n%s", runnerFixCore)
	}
}

// The last fix phase opens the PR, and its report must carry the risk the runner
// hands the task over with.
func TestRunnerFixBriefDeclaresRiskAndOpensThePR(t *testing.T) {
	brief := RunnerFixBrief()
	for _, want := range []string{
		"open a PR targeting the repo's PR base branch",
		"risk { touches: [...] }",
		"risk_vocabulary",
		"The runner hands the task over with submit_for_review itself",
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("the runner fix brief never says %q:\n%s", want, brief)
		}
	}
	// The served `finishing` page is a skill page, and those are forbidden here.
	if strings.Contains(brief, "`finishing`") {
		t.Errorf("the runner fix brief points at the served finishing reference:\n%s", brief)
	}
}

// A verification round follows the fix-verify phase, so the PR stays the
// verifier's: the fixer commits, pushes and reports.
func TestRunnerFixVerifyBriefLeavesThePRToTheVerifier(t *testing.T) {
	brief := RunnerFixVerifyBrief()
	for _, want := range []string{
		"Do NOT open a PR",
		"do NOT move the task to in_review",
		"the VERIFICATION round after you carries that terminal step",
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("the runner fix-verify brief never says %q:\n%s", want, brief)
		}
	}
}

// The verification round adjudicates through the report's findingUpdates, and
// only its no-open-MUST arm hands the task over.
func TestRunnerVerifyBriefReportsTheRound(t *testing.T) {
	brief := RunnerVerifyBrief()
	for _, want := range []string{
		"findingUpdates",
		"verdict",
		"reviewedTip",
		"findings",
		"omitted",
		"notes",
		"risk { touches: [...] }",
		"The runner records the round and hands the task to in_review",
		"Never decide to run another round yourself",
		verifyStoppingRule,
		verifyDriverBounds,
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("the runner verify brief never says %q:\n%s", want, brief)
		}
	}
}

func TestRunnerImplementBriefEndsAtAPushedBranch(t *testing.T) {
	brief := RunnerImplementBrief()
	for _, want := range []string{
		"PHASE " + PhaseNumberPlaceholder + " of " + PhaseTotalPlaceholder,
		"implementation ONLY",
		"no review gate and no in_review",
		runnerTaskPrepared,
		"The runner records the branch on the task itself once it is on origin",
		"Never rebase and never force-push",
		"PUSH to origin " + PhaseBranchPlaceholder,
		"Then end the session with the done report",
		"do NOT move the task to in_review",
		DoneMarker,
		runnerRerunGuidance,
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("the runner implement brief never says %q:\n%s", want, brief)
		}
	}
	// Supersession is a park in the report, not a decision the session writes; the
	// runner does the merge or reports its conflicts in the context block.
	for _, banned := range []string{
		"record_progress(taskId",
		"settle_task(taskId",
		"merge the repo's PR base branch",
		"git merge origin/",
		"hasWip",
	} {
		if strings.Contains(brief, banned) {
			t.Errorf("the runner implement brief still says %q:\n%s", banned, brief)
		}
	}
	// The park is named as a report field, never as a call.
	if !strings.Contains(brief, "`park`") {
		t.Errorf("the runner implement brief never names the report's park field:\n%s", brief)
	}
}

// The plane's close-out hint tells an UNCLAIMED session to "claim it" and hands
// the task back with release_task/checkGreen, so the runner-claimed brief must
// say the claim is already held - and must not blanket-ban the base merge: a
// conflict or a required up-to-date check needs one.
func TestRunnerCloseOutBriefAgreesWithThePlanesInstruction(t *testing.T) {
	brief := RunnerCloseOutBrief()
	for _, want := range []string{
		"CLOSE-OUT",
		"`Close-out instruction` section of the context block",
		"the runner holds that claim",
		"do NOT call claim_task",
		"`release_task`",
		"`checkGreen`",
		"repair it on the branch in the prepared worktree",
		"never rebase and never force-push",
		"record_progress(taskId, runId, outcome: ...)",
		"Do NOT merge the PR base into the branch unless the red check itself requires it",
		"STOP and end the session",
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("the runner close-out brief never says %q:\n%s", want, brief)
		}
	}
	// The blanket ban and the reason it gave are gone: neither is true.
	for _, banned := range []string{
		"Do NOT merge the PR base into the branch:",
		"would rewrite",
		"git merge origin/",
		"merge " + PhasePRBasePlaceholder + " into ",
	} {
		if strings.Contains(brief, banned) {
			t.Errorf("the runner close-out brief still says %q:\n%s", banned, brief)
		}
	}
}

// The plane's close-out instruction has the session check CI and repair a red
// check, so this brief is the one runner variant that must NOT carry the CI
// ban; it points at the context block's section instead.
func TestRunnerCloseOutBriefMayWaitOnCI(t *testing.T) {
	brief := RunnerCloseOutBrief()
	if strings.Contains(brief, "do NOT poll or sleep-wait on CI") {
		t.Errorf("the runner close-out brief bans the CI wait the plane's close-out instruction requires:\n%s", brief)
	}
	if !strings.Contains(brief, "Close-out instruction") {
		t.Errorf("the runner close-out brief does not point at the close-out instruction section:\n%s", brief)
	}
	if !strings.Contains(brief, "repair it on the branch") {
		t.Errorf("the runner close-out brief does not let the session repair a red check:\n%s", brief)
	}
}

// phaseDone is the runner's phase marker, and it rides on record_progress: the
// phase briefs ban that write outright, so only the close-out needs the marker
// named.
func TestRunnerBriefsForbidThePhaseMarker(t *testing.T) {
	if !strings.Contains(runnerForbidden, runnerNoWrites) {
		t.Errorf("runnerForbidden does not carry the write ban that removes the phase marker:\n%s", runnerForbidden)
	}
	if !strings.Contains(runnerNoWrites, "phaseDone") {
		t.Errorf("the write ban does not name the phase marker among the record_progress writes:\n%s", runnerNoWrites)
	}
	if !strings.Contains(runnerCloseOutForbidden, "do NOT pass phaseDone to record_progress") {
		t.Errorf("the close-out prohibition does not keep the session off the runner's phase marker:\n%s", runnerCloseOutForbidden)
	}
}

// The lease is the runner's too: a session's heartbeat is a competing holder,
// so neither prohibition may leave heartbeat available.
func TestRunnerBriefsForbidHeartbeat(t *testing.T) {
	const ban = "do NOT call heartbeat (the lease is the runner's)"
	for _, tc := range []struct{ name, block string }{
		{"runner", runnerForbidden},
		{"close-out", runnerCloseOutForbidden},
	} {
		if !strings.Contains(tc.block, ban) {
			t.Errorf("the %s prohibition does not ban heartbeat:\n%s", tc.name, tc.block)
		}
	}
}

func TestRunnerRerunGuidanceInlinesTheWaitingRule(t *testing.T) {
	if runnerRerunGuidance == rerunGuidance {
		t.Fatal("runnerRerunGuidance is unchanged from rerunGuidance: the waiting.md substitution did not apply")
	}
	if !strings.Contains(runnerRerunGuidance, runnerWaitRule) {
		t.Errorf("runnerRerunGuidance does not carry the inline waiting rule:\n%s", runnerRerunGuidance)
	}
	if strings.Contains(runnerRerunGuidance, "clankerbar.com/skills") {
		t.Errorf("runnerRerunGuidance still points at a served skill page:\n%s", runnerRerunGuidance)
	}
	// Everything else about the bound is rerunGuidance's.
	if !strings.Contains(runnerRerunGuidance, "two consecutive reruns of the same command") {
		t.Errorf("runnerRerunGuidance lost the rerun bound:\n%s", runnerRerunGuidance)
	}
}

// A runner-claimed handoff respawn gets the runner's own preamble: no heartbeat,
// no claim, no skill page - and adapting a shipped continuation rewrites the
// waiting pointer without dropping the bound.
func TestRunnerHandoffPreambleLeavesTheLeaseAlone(t *testing.T) {
	if strings.Contains(RunnerHandoffPreamble, `heartbeat("`) {
		t.Errorf("RunnerHandoffPreamble still tells the successor to resume the run itself:\n%s", RunnerHandoffPreamble)
	}
	if !strings.Contains(RunnerHandoffPreamble, PhaseRunPlaceholder) || !strings.Contains(RunnerHandoffPreamble, PhaseTaskPlaceholder) {
		t.Errorf("RunnerHandoffPreamble does not name the run and task placeholders:\n%s", RunnerHandoffPreamble)
	}
}

func TestRunnerHandoffContinuationDropsTheServedSkillPointer(t *testing.T) {
	got := RunnerHandoffContinuation(HandoffContinuation(ImplementPhaseName))
	if got == "" {
		t.Fatal("RunnerHandoffContinuation dropped a shipped continuation")
	}
	if strings.Contains(got, "waiting.md") {
		t.Errorf("the runner continuation still points at a served skill page:\n%s", got)
	}
	if !strings.Contains(got, runnerWaitRule) {
		t.Errorf("the runner continuation does not carry the inline waiting rule:\n%s", got)
	}
	if !strings.Contains(got, runnerForbidden) {
		t.Errorf("the runner continuation does not carry the runner's prohibition:\n%s", got)
	}
	if !strings.Contains(got, runnerNoWrites) {
		t.Errorf("the runner continuation does not carry the write ban:\n%s", got)
	}
	if got := RunnerHandoffContinuation(""); got != "" {
		t.Errorf("RunnerHandoffContinuation(\"\") = %q, want empty", got)
	}
}

// runnerFixCore is written for the runner rather than derived from fixCore, so
// the worktree and the records are pinned here: the runner's prepared tree, and
// no write of its own.
func TestRunnerFixCoreStatesThePreparedWorktree(t *testing.T) {
	if !strings.Contains(runnerFixCore, runnerWorktreeAtTip) {
		t.Errorf("runnerFixCore does not state the prepared worktree and tip:\n%s", runnerFixCore)
	}
	if strings.Contains(runnerFixCore, "Work in the worktree for branch") {
		t.Errorf("runnerFixCore carries a worktree hunt:\n%s", runnerFixCore)
	}
	if strings.Contains(runnerFixCore, "record_progress(taskId") {
		t.Errorf("runnerFixCore still writes the task itself:\n%s", runnerFixCore)
	}
}

func TestRunnerFixGradingRulePointsAtTheReport(t *testing.T) {
	if runnerFixGradingRule == fixGradingRule {
		t.Fatal("runnerFixGradingRule is unchanged: a shipped fixGradingRule edit broke the replacements")
	}
	for _, b := range []string{runnerFixBrief, runnerFixVerifyBrief} {
		if strings.Contains(b, "in your outcome") {
			t.Errorf("runner fix brief still tells the session to write its outcome")
		}
	}
}
