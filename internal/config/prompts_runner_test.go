package config

import (
	"strings"
	"testing"
)

// runnerBriefs lists every runner-claimed brief by name, with the prohibition
// block it carries. Each is started by a runner that holds the claim and owns
// the lifecycle around it - so the prohibitions and the removed plumbing are the
// same for all of them, except that the close-out may inspect and repair CI.
func runnerBriefs() []struct{ name, brief, forbidden string } {
	return []struct{ name, brief, forbidden string }{
		{"implement", RunnerImplementBrief(), runnerForbidden},
		{"close-out", RunnerCloseOutBrief(), runnerCloseOutForbidden},
		{"review-only", RunnerReviewOnlyBrief(), runnerForbidden},
		{"fix", RunnerFixBrief(), runnerForbidden},
		{"fix-verify", RunnerFixVerifyBrief(), runnerForbidden},
		{"verify", RunnerVerifyBrief(), runnerForbidden},
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

// The record, PR and hand-over steps stay in the fix and verify briefs until the
// runner owns the close-out (B7/B8): the runner variants must not quietly drop
// the phase's own contract.
func TestRunnerBriefsKeepTheirTerminalSteps(t *testing.T) {
	for _, tc := range []struct{ name, brief, step string }{
		{"review-only", RunnerReviewOnlyBrief(), reviewOnlyTerminalStep},
		{"fix", RunnerFixBrief(), reviewTerminalStep},
		{"fix", RunnerFixBrief(), fixTerminalRecord},
		{"fix-verify", RunnerFixVerifyBrief(), fixOnlyTerminalStep},
		{"fix-verify", RunnerFixVerifyBrief(), fixVerifyRecord},
		{"verify", RunnerVerifyBrief(), verifyTerminalStep},
	} {
		if !strings.Contains(tc.brief, tc.step) {
			t.Errorf("the %s brief lost its terminal/record step", tc.name)
		}
	}
}

func TestRunnerImplementBriefEndsAtAPushedRecordedBranch(t *testing.T) {
	brief := RunnerImplementBrief()
	for _, want := range []string{
		"PHASE " + PhaseNumberPlaceholder + " of " + PhaseTotalPlaceholder,
		"implementation ONLY",
		"no review gate and no in_review",
		runnerTaskPrepared,
		"record_decision",
		"settle_task(taskId, runId, disposition: \"parked\", outcome: ...)",
		"Never rebase and never force-push",
		"PUSH to origin " + PhaseBranchPlaceholder,
		"record_progress(taskId, runId, branch)",
		"STOP and end the session",
		runnerRerunGuidance,
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("the runner implement brief never says %q:\n%s", want, brief)
		}
	}
	// The runner does the merge (or reports its conflicts in the context block),
	// so the brief must not send the session to merge or claim anything itself.
	for _, banned := range []string{"merge the repo's PR base branch", "git merge origin/", "hasWip"} {
		if strings.Contains(brief, banned) {
			t.Errorf("the runner implement brief still says %q:\n%s", banned, brief)
		}
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

// phaseDone is the runner's phase marker: the plane does not check who writes
// it, so every runner brief must keep the session off it.
func TestRunnerBriefsForbidThePhaseMarker(t *testing.T) {
	for _, tc := range []struct{ name, block string }{
		{"runner", runnerForbidden},
		{"close-out", runnerCloseOutForbidden},
	} {
		if !strings.Contains(tc.block, "do NOT pass phaseDone to record_progress") {
			t.Errorf("the %s prohibition does not keep the session off the runner's phase marker:\n%s", tc.name, tc.block)
		}
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
	if got := RunnerHandoffContinuation(""); got != "" {
		t.Errorf("RunnerHandoffContinuation(\"\") = %q, want empty", got)
	}
}

func TestRunnerFixCoreReplacesTheWorktreeHunt(t *testing.T) {
	if strings.Contains(runnerFixCore, fixWorktreeClause) {
		t.Fatal("runnerFixCore still carries fixCore's worktree sentence: the substitution did not apply")
	}
	if !strings.Contains(runnerFixCore, runnerWorktreeAtTip) {
		t.Errorf("runnerFixCore does not state the prepared worktree and tip:\n%s", runnerFixCore)
	}
}
