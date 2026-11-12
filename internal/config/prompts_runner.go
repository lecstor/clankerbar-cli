package config

import "strings"

// The runner-claimed briefs. A runner-claimed session is started for ONE
// prepared task, in a worktree the runner created, on a branch the runner
// pushed and tracks, with the task already claimed and the lease held by the
// runner. Everything mechanical therefore leaves the brief: the session gets
// the context block (TaskContextBlock) above it and does only the phase's own
// work. These constants are added beside the shipped briefs, not in place of
// them; the driver selects them in a later wiring step.

// runnerWaitRule replaces rerunGuidance's pointer at the served waiting
// reference: a runner-claimed session is forbidden from fetching skill pages,
// so the polling discipline it needs is stated in the brief instead.
const runnerWaitRule = "Waiting on a background job: block inside ONE tool call with a bounded wait " +
	"instead of polling across turns, and never sleep-poll."

// runnerRerunGuidance is rerunGuidance's bound with that pointer replaced. It is
// derived rather than copied so the bound itself cannot drift between a
// session-claimed and a runner-claimed session.
var runnerRerunGuidance = strings.Replace(rerunGuidance,
	"Consecutive turns spent idling on a background job or re-polling its output are the "+
		"WAITING problem, not the rerun problem: read "+
		"https://clankerbar.com/skills/clankerbar/waiting.md before your next poll and take from it the polling "+
		"discipline while keeping hold of this task.",
	runnerWaitRule, 1)

// runnerForbidden is the shared prohibition every runner-claimed brief carries.
// The runner owns the lifecycle around the session, and a session doing any of
// this competes with it: a second claim or a heartbeat races the lease, a PR or
// a CI poll duplicates what the runner opens and waits for, and a fetched skill
// page is the turn cost this design exists to remove. The phaseDone marker is
// the runner's too: the plane does not check who writes it, so a session's copy
// silently stands in for the runner's own phase transition.
//
// The PR clause is scoped to the brief's own terminal step because the fix and
// verify briefs still open the PR until the runner owns the close-out (B7/B8).
const runnerForbidden = " NEVER in this session: do NOT open a PR (unless this brief's own terminal step tells " +
	"you to), do NOT poll or sleep-wait on CI, do NOT call next_task or claim_task, do NOT call heartbeat (the " +
	"lease is the runner's), do NOT create or switch worktrees, do NOT fetch clankerbar skill documents (curl or " +
	"webfetch of any page under clankerbar.com), and do NOT pass phaseDone to record_progress (the plane does " +
	"not check who writes it, and the runner writes that phase marker itself) - the runner owns all of that, and " +
	"this brief plus the context block carry everything the task needs."

// runnerCloseOutForbidden is runnerForbidden for the close-out brief. The PR
// already exists and a red check is the session's to repair, so neither the PR
// clause nor the CI-polling ban applies here.
const runnerCloseOutForbidden = " NEVER in this session: do NOT call next_task or claim_task, do NOT call " +
	"heartbeat (the lease is the runner's), do NOT create or switch worktrees, do NOT fetch clankerbar skill " +
	"documents (curl or webfetch of any page under clankerbar.com), and do NOT pass phaseDone to " +
	"record_progress (the plane does not check who writes it, and the runner writes that phase marker itself) " +
	"- the runner owns all of that, and this brief plus the context block carry everything the task needs."

// runnerWorktreeAtTip is the runner's replacement for every "work in the
// worktree for branch X" clause: the runner has already put the session there.
const runnerWorktreeAtTip = "You were started in the task worktree at tip " + ReviewedTipPlaceholder +
	" (the context block above names the worktree, the branch and the tip; confirm `git rev-parse HEAD` is that " +
	"tip), and never commit to the repo's PR base branch (" + PhasePRBasePlaceholder + ") - a session whose cwd " +
	"is already the main checkout sitting on that branch is not a decision to commit where you are, it is this " +
	"failure mode."

// runnerResumeNoClaim replaces the claim/heartbeat resume step. The session is
// already inside a held run, so a heartbeat of its own is a second holder.
const runnerResumeNoClaim = " You are RESUMING that run, not starting a new one: the runner claimed the task and " +
	"holds its lease, so do NOT claim anything and do NOT call heartbeat - the lease is the runner's. Call plain " +
	"get_task to re-read the bar: the context block above already carries the task, the phase, the branch and the " +
	"standing decisions."

// runnerTaskPrepared states the two facts the old brief spent turns
// establishing: the claim and the worktree already exist.
const runnerTaskPrepared = " The task is already claimed and your worktree is prepared: work in the directory and on " +
	"the branch the context block names, and do NOT claim anything or create, switch or re-create a worktree."

// runnerResumedRevalidate keeps the substance of implementResumedBranchRule's
// re-validation and supersession handling, without its merge instruction: the
// runner did that merge (or reported its conflicts in the context block).
const runnerResumedRevalidate = " If the context block says Resumed, the branch already carried prior work, so " +
	"re-validate the task against the current merged tip BEFORE doing or verifying anything: a clean merge proves " +
	"nothing - a non-conflicting merge can silently revert a newer fix - so the check is the task's own bar (for a " +
	"bug: does it still reproduce?), not the absence of conflict markers. If the merged tip supersedes the task, " +
	"record the decision with record_decision, then PARK the task with settle_task(taskId, runId, disposition: " +
	"\"parked\", outcome: ...) citing it - a bare stop reads as checkpointed WIP and spends the runner a whole " +
	"phase rediscovering the staleness - or salvage only what still adds value and carry on with the normal flow. " +
	"Either way do NOT spend the run re-verifying and pushing stale work."

// runnerImplementCheckpoint is implementCommitEarlyRule's floor, shorter: the
// evidence gate still reads a branch recorded on the task, so the first push and
// its record must land before the session is deep in the work.
const runnerImplementCheckpoint = " COMMIT AND PUSH AS YOU GO: get a first commit down before the design is fully " +
	"settled - a skeleton, a failing test, a stub with the signatures, the first thing that compiles - push it to " +
	"origin " + PhaseBranchPlaceholder + ", and record the branch with record_progress(taskId, runId, branch) as " +
	"soon as that first push lands; then keep committing and pushing as you go rather than once at the end. An " +
	"uncommitted worktree and a commit nobody pushed are lost the same way when a session ends early, and the " +
	"runner resumes this task from the branch recorded on it. This is a floor, not a ceiling: the terminal bar is " +
	"still self-verify, then commit, push and record. Never rebase and never force-push - the branch may already " +
	"be pushed, and rewriting pushed history invalidates the runner's tip checks."

// runnerImplementBrief is the implement phase's brief for a runner-claimed
// session: implementation only, in a worktree the runner prepared, ending at
// the pushed-and-recorded branch the runner continues from.
var runnerImplementBrief = "Work the task in the context block above. This session is PHASE " +
	PhaseNumberPlaceholder + " of " + PhaseTotalPlaceholder + ", and its scope is implementation ONLY: there is " +
	"no review gate and no in_review step in this session." + runnerTaskPrepared + runnerResumedRevalidate +
	runnerImplementCheckpoint +
	" Self-verify what you changed, then COMMIT, PUSH to origin " + PhaseBranchPlaceholder + ", record the branch " +
	"with record_progress(taskId, runId, branch) if that write has not already landed, and STOP and end the " +
	"session. Do NOT run the review gate and do NOT move the task to in_review - the runner continues this task " +
	"from the branch you pushed, so ending there is the task going to plan, not the task being abandoned." +
	runnerRerunGuidance + runnerForbidden + handoffGuidance

// runnerCloseOutBrief is the close-out phase's brief for a runner-claimed
// session: the task is approved and awaiting merge, and the plane's own
// close-out instruction - which the context block carries, not this brief -
// owns the work after a red check is repaired. It must agree with that
// instruction: the plane tells an unclaimed session to "claim it", and here the
// runner already holds the claim, so the brief says so rather than leave the
// session claiming a task it already owns.
var runnerCloseOutBrief = "This session is PHASE " + PhaseNumberPlaceholder + " of " + PhaseTotalPlaceholder +
	" on the task in the context block above, and it is a CLOSE-OUT: its implementation and review are done " +
	"and approved, and it is awaiting merge. The task is already claimed - the runner holds that claim and its " +
	"lease, so the `claim it` the close-out instruction mentions is already done: do NOT call claim_task. " +
	"Follow the `Close-out instruction` section of the context block exactly - it is the plane's instruction " +
	"for this task and it governs the close-out; this brief does not restate it. You MAY inspect the PR's CI " +
	"checks. " + runnerWaitRule + " If a check is RED, repair it on the branch in the prepared worktree, then " +
	"COMMIT and PUSH the repair to that branch - never rebase and never force-push - then finish exactly as the " +
	"close-out instruction says: hand the task back with `release_task`, declaring `checkGreen` honestly, and " +
	"STOP and end the session. Do NOT merge the PR base into the branch unless the red check itself requires it " +
	"- a conflict, or a required up-to-date check - because that merge is repair work the check asked for. If " +
	"you cannot repair the red check, record why with record_progress(taskId, runId, outcome: ...), hand the " +
	"task back with `release_task` and `checkGreen` false, then STOP and end the session." +
	runnerCloseOutForbidden + handoffGuidance

// runnerReviewResume is the review variant's resume step: the same
// no-claim, no-heartbeat rule plus the hand-off check only a branch-shaped
// phase can make.
const runnerReviewResume = runnerResumeNoClaim + " An empty branch field on the task is a FAILED hand-off to " +
	"report in the record below, not work to silently adopt and implement."

// runnerReviewWorktreeAndGate is reviewOnlyBriefWorktreeAndGate with the
// worktree hunt replaced: the session starts at the reviewed tip.
var runnerReviewWorktreeAndGate = " " + runnerWorktreeAtTip +
	" Read the diff against the PR base (git diff " + PhasePRBasePlaceholder + "..." + PhaseBranchPlaceholder +
	") and run the adversarial review gate." + reviewGateScope +
	"REVIEW ONLY: do not edit, create or delete a source file, do not commit, do not push, and do not open a PR " +
	"- not even to fix a typo. Do NOT fix what you find: the phase after you is responsible for applying it, " +
	"and a fix applied here is a fix it cannot attribute."

// runnerReviewOnlyBrief is the review-only gate's brief for a runner-claimed
// session: the shipped core and record shape, with the runner's resume and
// worktree facts in place of the claim and the worktree hunt.
var runnerReviewOnlyBrief = reviewOnlyBriefOpening + reviewHandoffVerified + runnerReviewResume +
	runnerReviewWorktreeAndGate + reviewOnlyRecordShape + runnerRerunGuidance + runnerForbidden +
	reviewOnlyTerminalStep + handoffGuidance

// fixWorktreeClause is fixCore's worktree sentence verbatim. The runner fix
// briefs swap it for runnerWorktreeAtTip, and a test pins that the swap lands,
// so an edit to fixCore cannot silently leave a runner session looking for a
// worktree it is already in.
const fixWorktreeClause = "Work in the worktree for branch " + PhaseBranchPlaceholder +
	" recorded on the task, and never commit to the repo's PR base branch (" + PhasePRBasePlaceholder + ") - a " +
	"session whose cwd is already the main checkout sitting on that branch is not a decision to commit where " +
	"you are, it is this failure mode."

// runnerFixCore is fixCore with that one sentence replaced. Everything else -
// reading both record forms, the reviewed-tip check, applying findings by plane
// id, the grading rule and the scoped re-verify - is the shipped text.
var runnerFixCore = strings.Replace(fixCore, fixWorktreeClause, runnerWorktreeAtTip, 1)

// runnerFixBrief is fixBrief for a runner-claimed session: the fix phase that
// still owns the PR and the in_review hand-over.
var runnerFixBrief = "You are PHASE " + PhaseNumberPlaceholder + " of " + PhaseTotalPlaceholder + " on task " +
	PhaseTaskPlaceholder + ", the FIX phase and the LAST phase of this task. The review phase reviewed branch " +
	PhaseBranchPlaceholder + " and wrote its findings as a sealed DURABLE review record; the runner has now " +
	"closed that round, so get_task releases the findings to you. It also left the legacy `" +
	ReviewRecordHeading + "` block in the task's outcome for one release — the LAST such block, because an " +
	"earlier `" + ReviewRecordHeading + "` block is an earlier round. It changed nothing." + runnerResumeNoClaim +
	" " + runnerFixCore +
	" If the review found nothing " +
	"(verdict: pass), there is nothing to fix: still open the PR if none exists and hand over." +
	runnerRerunGuidance + runnerForbidden + fixTerminalRecord + reviewTerminalStep + handoffGuidance

// runnerFixVerifyBrief is fixVerifyBrief for a runner-claimed session: a
// verification round follows, so the PR and the hand-over stay the verifier's.
var runnerFixVerifyBrief = "You are PHASE " + PhaseNumberPlaceholder + " of " + PhaseTotalPlaceholder +
	" on task " + PhaseTaskPlaceholder + ", the FIX phase. The review phase reviewed branch " +
	PhaseBranchPlaceholder + " and wrote its findings as a sealed DURABLE review record; the runner has now " +
	"closed that round, so get_task releases the findings to you. It also left the legacy `" +
	ReviewRecordHeading + "` block in the task's outcome for one release — the LAST such block, because an " +
	"earlier `" + ReviewRecordHeading + "` block is an earlier round. It changed nothing. A VERIFICATION round " +
	"runs after you: it re-reviews your fix delta and adjudicates the prior open MUSTs, and IT opens the PR " +
	"and hands the task to in_review once no MUST remains open." + runnerResumeNoClaim + " " + runnerFixCore +
	" If the review found nothing (verdict: pass), there is nothing to fix: commit nothing, leave the branch " +
	"as it is, and end holding the task — the verification round still runs and carries the terminal step." +
	runnerRerunGuidance + runnerForbidden + fixVerifyRecord + fixOnlyTerminalStep + handoffGuidance

// runnerVerifyBrief is verifyBrief for a runner-claimed session. The review
// scope, the adjudication and the record are the shipped text; only the resume,
// the worktree and the polling rule differ.
var runnerVerifyBrief = "You are PHASE " + PhaseNumberPlaceholder + " of " + PhaseTotalPlaceholder + " on task " +
	PhaseTaskPlaceholder + ", the VERIFICATION round. A review round found issues and a fix round applied " +
	"them; your job is to verify the FIXES, not to review the whole diff again. The driver has closed the " +
	"previous round, so get_task releases the records." + runnerResumeNoClaim +
	" The driver has prepended the PRIOR OPEN MUST ids with " +
	"their findings, and every MUST of this review cycle already MARKED settled; if that block is missing, read " +
	"the released records yourself and list every finding with severity must, both those with status open and " +
	"those of this cycle marked fixed, refuted or filed. " + runnerWorktreeAtTip + " " +
	"SCOPE: the fix delta is the reviewed tip the LAST released record names (`reviewedTip`) up to HEAD " +
	"(`git diff <reviewedTip>..HEAD`), PLUS the regression surface those fixes touch. Do not re-review the " +
	"whole diff, whose full pass already happened; a full second pass is the exception you state a reason for " +
	"in the record. " +
	"ADJUDICATE EVERY PRIOR OPEN MUST BY ITS PLANE ID — exactly one disposition each, recorded through " +
	"record_review(taskId, runId, {findingUpdates: [...]}) on the CLOSED record it belongs to: " +
	"(1) RESOLVED — the fix is present and correct: status \"fixed\" with fixCommit. " +
	"(2) RESOLVED BY DISPATCH — the fix is a follow-up task: status \"filed\" with filedTaskRef. " +
	"(3) REFUTATION ACCEPTED — the fixer's evidence shows the finding was wrong: status \"refuted\" with " +
	"refutedReason. " +
	"(4) UNRESOLVED or REFUTATION REJECTED — leave the finding's status \"open\" and record in this round's " +
	"record why the fix or the refutation does not stand. " +
	"A MUST the fix phase already MARKED fixed, refuted or filed is not settled by that marking: the fixer " +
	"marks what it answers, and a refutation changes no code, so the delta will not show it. Check each one " +
	"the same way. Confirm it by leaving its status; reject it by setting its status back to \"open\" with " +
	"findingUpdates and recording why — that is disposition (4), and the gate then sees it open. " +
	"You may raise NEW MUSTs ONLY within the fix delta and its regression surface; the plane assigns their " +
	"new ids. Do not re-raise an earlier finding as a new id: adjudicate the existing id. " +
	"REVIEW ONLY: you do not change code — do not edit, create or delete a source file, do not commit, and do " +
	"not apply a fix yourself. The phase after you applies what you find, and a fix applied here is a fix it " +
	"cannot attribute. The one exception is the hand-over below: when no MUST remains open, opening the PR " +
	"(and pushing a fix commit that the fixer left local) is the terminal step, not a review change. " +
	"RECORD THE ROUND IN BOTH FORMS: first append the legacy `" + ReviewRecordHeading + "` block to the task's " +
	"outcome with record_progress(taskId, runId, outcome: ...) — append, never replace what sits above it — then " +
	"append the sealed durable record with record_review(taskId, runId, {...}) naming reviewerRole " +
	"\"" + ReviewerRolePlaceholder + "\", reviewedTip `git rev-parse HEAD`, scope kind \"fix-delta\" with base and range " +
	"(`<reviewedTip>..HEAD`), a verdict of pass or findings, and every new finding graded must/should/nit with " +
	"the evidence a MUST requires. The durable record is the driver's seam evidence; the block is the " +
	"one-release compatibility copy." +
	verifyStoppingRule + verifyDriverBounds + verifyTerminalStep + runnerRerunGuidance + runnerForbidden +
	handoffGuidance

// RunnerImplementBrief returns the implement phase's brief for a runner-claimed
// session.
func RunnerImplementBrief() string { return runnerImplementBrief }

// RunnerCloseOutBrief returns the close-out brief for a runner-claimed
// approved-awaiting-merge task.
func RunnerCloseOutBrief() string { return runnerCloseOutBrief }

// RunnerReviewOnlyBrief returns the review-only gate's brief for a
// runner-claimed session.
func RunnerReviewOnlyBrief() string { return runnerReviewOnlyBrief }

// RunnerFixBrief returns the final fix phase's brief for a runner-claimed
// session.
func RunnerFixBrief() string { return runnerFixBrief }

// RunnerFixVerifyBrief returns the fix phase's brief when a verification round
// follows, for a runner-claimed session.
func RunnerFixVerifyBrief() string { return runnerFixVerifyBrief }

// RunnerVerifyBrief returns the verification round's brief for a runner-claimed
// session.
func RunnerVerifyBrief() string { return runnerVerifyBrief }
