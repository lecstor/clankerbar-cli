package config

// HandoffMarker is the exact line a session puts in its FINAL message to hand the rest of its job
// to a fresh successor session. The syntax had to be improbable in ordinary prose and easy to pin
// in a test, and it must not resemble tool-call framing markup: the web repo's
// docs/text-integrity.md records agents destroying their own output merely by EMITTING such tags,
// so an angle-bracket or tag-shaped marker would put the defect this feature rides on into the
// feature itself.
const HandoffMarker = "=====CLANKERBAR HANDOFF====="

// HandoffPromptMax bounds the successor prompt a handoff block may carry, in bytes. A
// session-authored respawn prompt is self-directed prompt injection, and the size cap is one of the
// non-negotiable guards on it: an over-cap prompt is refused (logged, normal path) rather than
// truncated, because a truncated brief is a successor confidently working half an instruction.
const HandoffPromptMax = 4096

// HandoffPreamble is driver-authored framing prepended to a handoff respawn's prompt — the
// successor never gets the emitting session's own prompt bare. A phase resume (reviewPhaseName,
// above) works only because its brief spells out "do not claim, call heartbeat first": a
// session-authored prompt cannot be trusted to carry that instruction forward on its own, and
// without it a successor calls next_task/claim_task on a fresh task, or never re-enters its run —
// the single heartbeat is the resume/settlement check, while the run's lease itself is renewed by
// the driver from Go while the session runs.
const HandoffPreamble = "You are resuming run " + PhaseRunPlaceholder + " on task " + PhaseTaskPlaceholder +
	", handed to you by the previous session on this task via a self-chosen handoff. Do NOT call next_task, and " +
	"do NOT claim anything: call heartbeat(\"" + PhaseRunPlaceholder + "\") first to resume the run." +
	driverRenewsLease + resumeHeartbeatStays + " Then stay within this phase's scope. What follows is the " +
	"previous session's own continuation prompt:\n\n"

func HandoffContinuation(phaseName string) string {
	switch phaseName {
	case ReviewPhaseName:
		return "\n\nThe phase's rerun bound and terminal step are unchanged by handing off:" + rerunGuidance + reviewTerminalStep
	case ImplementPhaseName:
		return "\n\nThe phase's rerun bound is unchanged by handing off:" + rerunGuidance
	case FixPhaseName:
		return "\n\nThe phase's rerun bound, record requirement and terminal step are unchanged by handing off:" +
			rerunGuidance + fixTerminalRecord + reviewTerminalStep
	}
	return ""
}

// HandoffContinuationFor is HandoffContinuation for a review phase whose mode is POSITION-dependent
// (see EffectivePhases): a review-only phase has no terminal step to carry, only the
// record-then-hold ending, and appending the shared reviewTerminalStep would tell its successor to
// commit, push, open a PR and hand the task to in_review — the exact actions the review-only mode
// forbids. The driver's review→fix gate matches the DURABLE record, so a successor that invented
// its own shape (or skipped the append) would fail the seam.
func HandoffContinuationFor(phaseName string, reviewOnly bool) string {
	if phaseName == ReviewPhaseName && reviewOnly {
		return "\n\nThe phase's rerun bound, record shape and review-only ending are unchanged by handing off:" +
			rerunGuidance + reviewOnlyRecordShape + reviewOnlyTerminalStep
	}
	return HandoffContinuation(phaseName)
}

// NoCodeReviewOnlyHandoffContinuation is the branchless review-only counterpart: the record shape
// and record-then-hold ending without the branch-shaped PR step or the no-code handover (which
// belongs to the fix phase).
func NoCodeReviewOnlyHandoffContinuation() string {
	return "\n\nThe phase's rerun bound, record shape and review-only ending are unchanged by handing off:" +
		rerunGuidance + noCodeReviewOnlyRecordShape + noCodeReviewOnlyTerminalStep
}

// UnverifiedReviewOnlyHandoffContinuation is the review-only handoff for a checkpoint the driver
// could NOT verify on the origin remote: the successor must establish the tip itself, exactly as
// the first session was told to, or its durable record cannot satisfy the seam.
func UnverifiedReviewOnlyHandoffContinuation() string {
	return "\n\nThe phase's rerun bound, record shape and review-only ending are unchanged by handing off:" +
		rerunGuidance + unverifiedReviewOnlyRecordShape + reviewOnlyTerminalStep
}

// NoCodeFixHandoffContinuation is the fix phase's continuation for a branchless checkpoint: the
// outcome requirement and the no-code handover, and never the branch-shaped PR step.
func NoCodeFixHandoffContinuation() string {
	return "\n\nThe phase's rerun bound, record requirement and terminal step are unchanged by handing off:" +
		rerunGuidance + noCodeFixTerminalRecord + noCodeReviewTerminalStep
}

// FixVerifyHandoffContinuation is the fix phase's continuation when a verification round follows:
// the successor must still answer every finding, commit and push, and then STOP holding the task.
// Carrying the legacy reviewTerminalStep here would tell it to open the PR and move the task to
// in_review — the terminal actions the verifier owns, and precisely what B12 says the fix phase
// must not do — and carrying fixTerminalRecord would do the same thing in prose: the record
// requirement here is fixVerifyRecord, which keeps the finding updates and drops the hand-over.
func FixVerifyHandoffContinuation() string {
	return "\n\nThe phase's rerun bound, record requirement and terminal step are unchanged by handing off:" +
		rerunGuidance + fixVerifyRecord + fixOnlyTerminalStep
}

func NoCodeFixVerifyHandoffContinuation() string {
	return "\n\nThe phase's rerun bound, record requirement and terminal step are unchanged by handing off:" +
		rerunGuidance + noCodeFixVerifyRecord + noCodeFixOnlyTerminalStep
}

const driverRenewsLease = " The driver renews the run's lease from Go every third of the lease while this " +
	"session runs, so do NOT call heartbeat for renewal."

const resumeHeartbeatStays = " That first heartbeat(runId) is the resume/settlement check and stays."

const handoffGuidance = " HANDOFF (most tasks need zero): if you reach a genuine pivot mid-session - " +
	"exploration finished and implementation about to start, or one distinct sub-goal landed and the next " +
	"beginning - you may hand the REST of this session's job to a fresh successor session that starts without " +
	"your accumulated context. First put durable state where it belongs: discovered constraints in repo docs, " +
	"commit/branch state on the task record. Then end your FINAL message with a line that is exactly " +
	HandoffMarker + " followed by the successor's starting prompt, written the way an operator's continuation " +
	"prompt reads: decisions made, state verified, exact next steps - nothing about the journey, nothing that " +
	"already lives in the repo or on the task, and under 4KB. The successor resumes this same run under this " +
	"same brief's scope, so do NOT settle, release or hand back the task first."

// rerunGuidance rides on every built-in phase brief.
const rerunGuidance = " RERUN BOUND: every rerun of a verification command is another turn over everything the " +
	"session holds, so bound them - two consecutive reruns of the same command is the ceiling, and a third " +
	"needs a stated reason first: one line saying either what changed in the code or environment UNDER TEST " +
	"since the last run, or why the last result may not reproduce (flakiness, timing, state outside this " +
	"checkout). A narrowed rerun is still a rerun: re-running the same suite with a different selector, filter " +
	"or flag counts against the same command's bound, because a different selector is not a change to what is " +
	"under test. Tool calls in between do not reset the count either, and a stated reason buys one run, not a " +
	"fresh pair - the count starts over only when what the command tests actually changed, which for a " +
	"fix-and-reverify loop is every fix landing, so honest re-verification never reaches the bound. Nothing " +
	"changed means the result cannot differ - read the previous output instead of paying for it again. The " +
	"bound is per command, not on verification overall - typecheck, then tests, then lint is three commands " +
	"run once each. Consecutive turns spent idling on a background job or re-polling its output are the " +
	"WAITING problem, not the rerun problem: read " +
	"https://clankerbar.com/skills/clankerbar/waiting.md before your next poll and take from it the polling " +
	"discipline while keeping hold of this task. This bound governs every verification command this phase runs."

const implementResumedBranchRule = "RESUMED WORK comes first when you find it: if the claim hands you an EXISTING branch or " +
	"worktree for this task (a recovered stale claim, a prior session's WIP - check the repo yourself whatever " +
	"the claim's hasWip flag said), your FIRST step is to merge the repo's PR base branch (" + PhasePRBasePlaceholder + ") into it - a " +
	"merge, NEVER a rebase: the branch may already be pushed, and rewriting pushed history invalidates the " +
	"daemon's phase-boundary check, which compares the pushed origin tip to your local tip. THEN re-validate " +
	"the task against the merged tip BEFORE doing or verifying any of the found work: a clean merge is not " +
	"proof the task still needs doing - a non-conflicting merge can silently revert a newer fix - so the " +
	"check is the task's own bar (for a bug: does it still reproduce?), not the absence of conflict markers. " +
	"If the merged tip supersedes the task, record the decision (record_decision), then PARK the task with an " +
	"outcome citing it (settle_task(taskId, runId, disposition: \"parked\", outcome: ...)) - task.branch is already set on a resumed branch, so a bare " +
	"stop reads as checkpointed WIP and spends a phase 2 rediscovering the staleness - or salvage only what " +
	"still adds value and carry on with the normal flow; either way do NOT spend the run re-verifying and " +
	"pushing stale work."

const implementCloseOutRule = "If next_task instead offers an approved-awaiting-merge task - implementation and " +
	"review already done, only the close-out remaining - that offer IS this session's job: follow the offer's " +
	"own instructions, and once the close-out is done STOP and end the session. The resumed-work rule below is " +
	"for found work, not this offer. Ending that way is the task going to plan, not the task being abandoned."

const implementCommitEarlyRule = " COMMIT EARLY, PUSH EARLY, RECORD EARLY: get a first commit down before the design is fully " +
	"settled - a skeleton, a failing test, a stub with the signatures, the first thing that compiles - then " +
	"push it and record the branch with record_progress(taskId, runId, branch), and commit and push as you go " +
	"rather than once at the end. A checkpoint is a branch the plane was told about: an uncommitted " +
	"worktree is lost when a session ends early, and a local commit nobody pushed and recorded is lost the " +
	"same way - the next phase resumes from the branch recorded on the task, so record it as soon as the " +
	"first commit is pushed, not only at the end. This adds an earlier floor, it does not lower the ceiling: " +
	"the terminal step's bar is unchanged - self-verify, then COMMIT, PUSH, and record the branch - and " +
	"checkpoint commits are not licence to commit broken or half-considered code as a habit, nor to pass a " +
	"checkpoint push off as the self-verified terminal delivery."

func NoCodeHandoffContinuation() string {
	return "\n\nThe phase's rerun bound and terminal step are unchanged by handing off:" + rerunGuidance + noCodeReviewTerminalStep
}
