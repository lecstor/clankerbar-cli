package config

// builtinPhasePrompts are the shipped briefs, selected by phase name.
//
// The split is implement, review, fix. The review phase's brief is chosen by
// POSITION rather than a second name (see EffectivePhases): a review with a
// phase after it is the read-only gate (reviewOnlyBrief), and a FINAL review
// keeps this historical review-and-fix brief so an existing two-phase config
// still reaches in_review. The fix phase applies the findings on a cheaper
// harness and owns the PR/in_review terminal step.
//
// An earlier version of this comment argued a fix phase was wrong because it
// "would hold neither the implementation nor the review context". That is still
// true, and it was never an argument about COST. The review record on the task
// (reviewOnlyRecordShape) is what replaces the shared context: the fix phase
// re-reads it from the plane, the way any resumed phase re-reads the bar.
var builtinPhasePrompts = map[string]string{
	ImplementPhaseName: "Work the next backlog item. This session is PHASE " + PhaseNumberPlaceholder + " of " + PhaseTotalPlaceholder + ", and its scope is implementation ONLY (plus the resumed-work and close-out dispositions below): " +
		implementCloseOutRule + " " +
		implementResumedBranchRule +
		implementCommitEarlyRule +
		" Unless you parked above or served a close-out above, the rest of the flow is unchanged: claim the task, work it in a worktree, self-verify, then COMMIT, PUSH, and record the branch with " +
		"record_progress(taskId, runId, branch). Then STOP and end the session. Do NOT run the review gate, and do NOT " +
		"move the task to in_review — a second session resumes this same run from that checkpoint and does both. " +
		"Ending there is this task going to plan, not the task being abandoned." + rerunGuidance + handoffGuidance,

	ReviewPhaseName: "You are PHASE " + PhaseNumberPlaceholder + " of " + PhaseTotalPlaceholder + " on task " + PhaseTaskPlaceholder + ", whose implement phase recorded " +
		"branch " + PhaseBranchPlaceholder + " on the plane — a branch the driver verified to exist on the origin " +
		"remote with its tip reachable, so phase 1 really did implement, commit and push. You are RESUMING that " +
		"run, not starting a new one: do not call next_task, and do not claim anything. Call heartbeat(\"" +
		PhaseRunPlaceholder + "\") to resume the run, then get_task with includeDecisions: true to re-read the " +
		"bar and the standing decisions. An empty branch field on the task is a FAILED hand-off to report, not " +
		"work to silently adopt and implement. Work in the worktree for branch " + PhaseBranchPlaceholder +
		" recorded on the task, and never commit to the repo's PR base branch (" + PhasePRBasePlaceholder + ") - a session whose cwd is " +
		"already the main checkout sitting on that branch is not a decision to commit where you are, it is this " +
		"failure mode. Read the diff on that branch. Then run the adversarial review gate, fix what it finds, " +
		"and re-verify SCOPED to those fixes: brief the follow-up reviewer with the findings you fixed, by name, " +
		"and point it at the fix commits (or, if not yet committed, the fix diff) and the regression surface they " +
		"touch - not at the whole diff, whose full pass already happened. A full second pass is the exception you " +
		"state a reason for (a fix that had to reach outside its own area), never the default." + rerunGuidance +
		reviewTerminalStep + handoffGuidance,

	FixPhaseName: fixBrief,

	VerifyPhaseName: verifyBrief,
}

// BuiltinImplementBrief returns the shipped implement brief, so the driver can
// tell a phase running it (and so swap in the runner-claimed variant) from one
// carrying an operator's custom prompt.
func BuiltinImplementBrief() string { return builtinPhasePrompts[ImplementPhaseName] }

// BuiltinReviewBrief returns the shipped review-and-fix brief, so the driver can
// tell whether a phase runs the built-in brief (vs an operator's custom prompt)
// when it must choose a variant (CLA-497): a checkpoint evidenced by the
// plane's record names no branch, and the branch-shaped brief would tell the
// successor its hand-off FAILED.
func BuiltinReviewBrief() string { return builtinPhasePrompts[ReviewPhaseName] }

// ReviewShaped reports whether a RESOLVED phase prompt is one of the review-only
// briefs whose deliverable is the durable review record (CLA-637). It is the
// driver's single "requires a review record" predicate: the seam gate must apply
// to every review-shaped brief, not only the exact built-in review-only prompt
// it was originally keyed to, so every variant — and, as later phases land,
// every verifier and panel brief — is added HERE rather than in a second
// condition that can drift.
//
// The resolved prompt, not the configured one: EffectivePhases swaps in
// reviewOnlyBrief by position, and invocationFor later swaps the branchless and
// unverified variants. A handoff continuation replaces the prompt wholesale, so
// the caller reads this from EffectivePhases before any of that.
func ReviewShaped(prompt string) bool {
	switch prompt {
	case reviewOnlyBrief, unverifiedReviewOnlyBrief, noCodeReviewOnlyBrief,
		panelMemberReviewOnlyBrief, panelMemberUnverifiedReviewOnlyBrief,
		// Callers pass captured config prompts today, so these two arms only
		// matter if a resolved runner prompt is ever checked.
		runnerReviewOnlyBrief, verifyBrief, noCodeVerifyBrief, runnerVerifyBrief:
		return true
	}
	return false
}

// VerifyShaped reports whether a RESOLVED prompt is a fix-verification round
// (CLA-638). It is deliberately narrower than ReviewShaped: a verifier writes a
// durable record and is gated the same way, but it also adjudicates prior
// findings and its terminal step depends on whether any MUST is still open, so
// the driver must not mistake an ordinary review for it.
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

// NoCodeReviewOnlyBrief returns the review-only brief for a branchless
// checkpoint (a no-code delivery or an already-settled task).
func NoCodeReviewOnlyBrief() string { return noCodeReviewOnlyBrief }

// UnverifiedReviewOnlyBrief returns the review-only brief for a checkpoint the
// driver could not verify against the origin remote.
func UnverifiedReviewOnlyBrief() string { return unverifiedReviewOnlyBrief }

// PanelMemberReviewOnlyBrief returns the review-only brief a PANEL MEMBER
// writes: the durable record alone, with no outcome write (CLA-639 M5). The
// driver selects it at spawn time for a panel member whose checkpoint carries a
// branch.
func PanelMemberReviewOnlyBrief() string { return panelMemberReviewOnlyBrief }

// PanelMemberUnverifiedReviewOnlyBrief returns the panel member's brief for a
// checkpoint the driver could not verify against the origin remote.
func PanelMemberUnverifiedReviewOnlyBrief() string { return panelMemberUnverifiedReviewOnlyBrief }

// FixBrief returns the fix phase's shipped brief.
func FixBrief() string { return fixBrief }

// FixVerifyBrief returns the fix phase's brief when a verification round
// follows it: it applies findings and stops holding the task; the verifier owns
// the PR and in_review hand-over.
func FixVerifyBrief() string { return fixVerifyBrief }

// VerifyBrief returns the verification round's shipped brief.
func VerifyBrief() string { return verifyBrief }

// NoCodeVerifyBrief returns the verification round's brief for a branchless
// checkpoint.
func NoCodeVerifyBrief() string { return noCodeVerifyBrief }

// NoCodeFixVerifyBrief returns the branchless fix phase's brief when a
// verification round follows it.
func NoCodeFixVerifyBrief() string { return noCodeFixVerifyBrief }

// NoCodeFixBrief returns the fix phase's brief for a branchless checkpoint.
func NoCodeFixBrief() string { return noCodeFixBrief }

// NoCodeReviewBrief returns the review brief for a checkpoint evidenced by the
// plane's record (CLA-497) - see noCodeReviewBrief.
func NoCodeReviewBrief() string { return noCodeReviewBrief }

// UnverifiedReviewBrief returns the review brief for a checkpoint the driver
// could not verify against the origin remote (CLA-576) - see
// unverifiedReviewBrief.
func UnverifiedReviewBrief() string { return unverifiedReviewBrief }
