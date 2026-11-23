package config

// builtinPhasePrompts are the shipped briefs, selected by phase name. The review phase's brief is
// chosen by POSITION rather than a second name (see EffectivePhases): a review with a phase after
// it is the read-only gate (reviewOnlyBrief), and a FINAL review keeps this historical
// review-and-fix brief so an existing two-phase config still reaches in_review.
var builtinPhasePrompts = map[string]string{
	ImplementPhaseName: "Work the next backlog item. This session is PHASE " + PhaseNumberPlaceholder + " of " + PhaseTotalPlaceholder + ", and its scope is implementation ONLY (plus the resumed-work and close-out dispositions below): " +
		implementCloseOutRule + " " +
		implementResumedBranchRule +
		implementCommitEarlyRule +
		" Unless you parked above or served a close-out above, the rest of the flow is unchanged: claim the task, work it in a worktree, self-verify, then COMMIT, PUSH, and record the branch with " +
		"record_progress(taskId, runId, branch)." + driverRenewsLease + " Then STOP and end the session. Do NOT run the review gate, and do NOT " +
		"move the task to in_review — a second session resumes this same run from that checkpoint and does both. " +
		"Ending there is this task going to plan, not the task being abandoned." + rerunGuidance + handoffGuidance,

	ReviewPhaseName: "You are PHASE " + PhaseNumberPlaceholder + " of " + PhaseTotalPlaceholder + " on task " + PhaseTaskPlaceholder + ", whose implement phase recorded " +
		"branch " + PhaseBranchPlaceholder + " on the plane — a branch the driver verified to exist on the origin " +
		"remote with its tip reachable, so phase 1 really did implement, commit and push. You are RESUMING that " +
		"run, not starting a new one: do not call next_task, and do not claim anything. Call heartbeat(\"" +
		PhaseRunPlaceholder + "\") to resume the run." + driverRenewsLease + resumeHeartbeatStays +
		" Then get_task with includeDecisions: true to re-read the " +
		"bar and the standing decisions. An empty branch field on the task is a FAILED hand-off to report, not " +
		"work to silently adopt and implement. Work in the worktree for branch " + PhaseBranchPlaceholder +
		" recorded on the task, and never commit to the repo's PR base branch (" + PhasePRBasePlaceholder + ") - a session whose cwd is " +
		"already the main checkout sitting on that branch is not a decision to commit where you are, it is this " +
		"failure mode. Read the diff on that branch (git diff " + PhaseBasePlaceholder + "..." + PhaseBranchPlaceholder +
		"). Then run the adversarial review gate, fix what it finds, " +
		"and re-verify SCOPED to those fixes: brief the follow-up reviewer with the findings you fixed, by name, " +
		"and point it at the fix commits (or, if not yet committed, the fix diff) and the regression surface they " +
		"touch - not at the whole diff, whose full pass already happened. A full second pass is the exception you " +
		"state a reason for (a fix that had to reach outside its own area), never the default." + rerunGuidance +
		reviewTerminalStep + handoffGuidance,

	FixPhaseName: fixBrief,

	VerifyPhaseName: verifyBrief,
}

func BuiltinImplementBrief() string { return builtinPhasePrompts[ImplementPhaseName] }

// BuiltinReviewBrief returns the shipped review-and-fix brief, so the driver can tell whether a
// phase runs the built-in brief (vs an operator's custom prompt) when it must choose a variant: a
// checkpoint evidenced by the plane's record names no branch, and the branch-shaped brief would
// tell the successor its hand-off FAILED.
func BuiltinReviewBrief() string { return builtinPhasePrompts[ReviewPhaseName] }

// ReviewShaped reports whether a RESOLVED phase prompt is one of the review-only briefs whose
// deliverable is the durable review record.
func ReviewShaped(prompt string) bool {
	switch prompt {
	case reviewOnlyBrief, unverifiedReviewOnlyBrief, noCodeReviewOnlyBrief,
		panelMemberReviewOnlyBrief, panelMemberUnverifiedReviewOnlyBrief,
		// Callers pass captured config prompts today, so these two arms only matter if a resolved runner
		// prompt is ever checked.
		runnerReviewOnlyBrief, verifyBrief, noCodeVerifyBrief, runnerVerifyBrief:
		return true
	}
	return false
}

// VerifyShaped reports whether a RESOLVED prompt is a fix-verification round.
func VerifyShaped(prompt string) bool {
	switch prompt {
	case verifyBrief, noCodeVerifyBrief,
		// As in ReviewShaped: only reachable if a resolved runner prompt is checked.
		runnerVerifyBrief:
		return true
	}
	return false
}

// ReviewOnlyBrief returns the review-only brief a non-final review phase runs.
func ReviewOnlyBrief() string { return reviewOnlyBrief }

// NoCodeReviewOnlyBrief returns the review-only brief for a branchless checkpoint (a no-code
// delivery or an already-settled task).
func NoCodeReviewOnlyBrief() string { return noCodeReviewOnlyBrief }

// UnverifiedReviewOnlyBrief returns the review-only brief for a checkpoint the driver could not
// verify against the origin remote.
func UnverifiedReviewOnlyBrief() string { return unverifiedReviewOnlyBrief }

// PanelMemberReviewOnlyBrief returns the review-only brief a PANEL MEMBER writes: the durable
// record alone, with no outcome write.
func PanelMemberReviewOnlyBrief() string { return panelMemberReviewOnlyBrief }

func PanelMemberUnverifiedReviewOnlyBrief() string { return panelMemberUnverifiedReviewOnlyBrief }

// FixBrief returns the fix phase's shipped brief.
func FixBrief() string { return fixBrief }

func FixVerifyBrief() string { return fixVerifyBrief }

// VerifyBrief returns the verification round's shipped brief.
func VerifyBrief() string { return verifyBrief }

// NoCodeVerifyBrief returns the verification round's brief for a branchless checkpoint.
func NoCodeVerifyBrief() string { return noCodeVerifyBrief }

func NoCodeFixVerifyBrief() string { return noCodeFixVerifyBrief }

// NoCodeFixBrief returns the fix phase's brief for a branchless checkpoint.
func NoCodeFixBrief() string { return noCodeFixBrief }

func NoCodeReviewBrief() string { return noCodeReviewBrief }

func UnverifiedReviewBrief() string { return unverifiedReviewBrief }
