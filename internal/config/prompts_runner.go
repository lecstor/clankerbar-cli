package config

import "strings"

// The runner-claimed briefs. A runner-claimed session is started for ONE
// prepared task, in a worktree the runner created, on a branch the runner
// pushed and tracks, with the task already claimed and the lease held by the
// runner. Everything mechanical therefore leaves the brief: the session gets
// the context block (TaskContextBlock) above it and does only the phase's own
// work. It also writes no record: it ends with a done report
// (runnerDoneReport) and the runner turns that into every record. These
// constants are added beside the shipped briefs, not in place of them; the
// driver selects them in a later wiring step.

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
// page is the turn cost this design exists to remove.
//
// The PR clause is scoped to the brief's own terminal step because the fix and
// verify briefs still open the PR until the runner owns the close-out (B7/B8).
const runnerForbidden = " NEVER in this session: do NOT open a PR (unless this brief's own terminal step tells " +
	"you to), do NOT poll or sleep-wait on CI, do NOT call next_task or claim_task, do NOT call heartbeat (the " +
	"lease is the runner's), do NOT create or switch worktrees, and do NOT fetch clankerbar skill documents (curl " +
	"or webfetch of any page under clankerbar.com) - the runner owns all of that, and this brief plus the context " +
	"block carry everything the task needs." + runnerNoWrites

// runnerCloseOutForbidden is runnerForbidden for the close-out brief, and keeps
// its own write rules: the close-out still repairs a red check and hands the
// task back itself, so it must not carry runnerNoWrites. The PR already exists
// and a red check is the session's to repair, so neither the PR clause nor the
// CI-polling ban applies here.
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
// runner did that merge (or reported its conflicts in the context block), and it
// writes the park from the done report.
const runnerResumedRevalidate = " If the context block says Resumed, the branch already carried prior work, so " +
	"re-validate the task against the current merged tip BEFORE doing or verifying anything: a clean merge proves " +
	"nothing - a non-conflicting merge can silently revert a newer fix - so the check is the task's own bar (for a " +
	"bug: does it still reproduce?), not the absence of conflict markers. If the merged tip supersedes the task, " +
	"end the session with a done report whose `park` says why (name the commit or decision that supersedes the " +
	"task) and change nothing - the runner parks it, and a bare stop reads as checkpointed WIP and spends the " +
	"runner a whole phase rediscovering the staleness - or salvage only what still adds value and carry on with " +
	"the normal flow. Either way do NOT spend the run re-verifying and pushing stale work."

// runnerImplementCheckpoint is implementCommitEarlyRule's floor, shorter. The
// runner records the branch itself, so the push is the only thing that protects
// the work - and the runner only sees a branch that is on origin.
const runnerImplementCheckpoint = " COMMIT AND PUSH AS YOU GO: get a first commit down before the design is fully " +
	"settled - a skeleton, a failing test, a stub with the signatures, the first thing that compiles - push it to " +
	"origin " + PhaseBranchPlaceholder + ", then keep committing and pushing as you go rather than once at the " +
	"end. An uncommitted worktree and a commit nobody pushed are lost the same way when a session ends early. The " +
	"runner records the branch on the task itself once it is on origin, so pushing early is what protects the " +
	"work: a branch only in your worktree is invisible to the task and to the runner that resumes it. This is a " +
	"floor, not a ceiling: the terminal bar is still self-verify, then commit and push. Never rebase and never " +
	"force-push - the branch may already be pushed, and rewriting pushed history invalidates the runner's tip " +
	"checks."

// runnerImplementBrief is the implement phase's brief for a runner-claimed
// session: implementation only, in a worktree the runner prepared, ending at
// the pushed branch the runner continues from.
var runnerImplementBrief = "Work the task in the context block above. This session is PHASE " +
	PhaseNumberPlaceholder + " of " + PhaseTotalPlaceholder + ", and its scope is implementation ONLY: there is " +
	"no review gate and no in_review step in this session." + runnerTaskPrepared + runnerResumedRevalidate +
	runnerImplementCheckpoint +
	" Self-verify what you changed, then COMMIT and PUSH to origin " + PhaseBranchPlaceholder + ". Then end the " +
	"session with the done report: summary (what changed), tests (what you actually ran and saw), and park only " +
	"when the merged tip supersedes the task. Do NOT run the review gate and do NOT move the task to in_review - " +
	"the runner continues this task from the branch you pushed, so ending there is the task going to plan, not " +
	"the task being abandoned." +
	runnerDoneReport(runnerImplementReportFields) + runnerRerunGuidance + runnerForbidden + handoffGuidance + runnerReportReminder

// runnerCloseOutBrief is the close-out phase's brief for a runner-claimed
// session: the task is approved and awaiting merge, and the plane's own
// close-out instruction - which the context block carries, not this brief -
// owns the work after a red check is repaired. It must agree with that
// instruction: the plane tells an unclaimed session to "claim it", and here the
// runner already holds the claim, so the brief says so rather than leave the
// session claiming a task it already owns. This phase keeps its own writes: it
// hands the task back itself.
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
const runnerReviewResume = runnerResumeNoClaim + " An empty branch field on the task is a FAILED hand-off: " +
	"report it as a MUST finding of kind failure in your done report, not work to silently adopt and implement."

// runnerReviewWorktreeAndGate is reviewOnlyBriefWorktreeAndGate with the
// worktree hunt replaced: the session starts at the reviewed tip.
var runnerReviewWorktreeAndGate = " " + runnerWorktreeAtTip +
	" Read the diff against the PR base (git diff " + PhaseBasePlaceholder + "..." + PhaseBranchPlaceholder +
	") and run the adversarial review gate." + reviewGateScope +
	"REVIEW ONLY: do not edit, create or delete a source file, do not commit, do not push, and do not open a PR " +
	"- not even to fix a typo. Do NOT fix what you find: the phase after you is responsible for applying it, " +
	"and a fix applied here is a fix it cannot attribute."

// runnerReviewRecord replaces the shipped two-form record shape: the review is
// handed over in the done report alone, and the runner renders the legacy block
// and writes the sealed durable record from it. The grading rules are
// reviewMUSTBarGrades verbatim - they are the bar, not the write.
const runnerReviewRecord = " THE REVIEW IS YOUR DELIVERABLE, and the done report is where you put it: do NOT write " +
	"the `" + ReviewRecordHeading + "` block with record_progress, and do NOT call record_review. The runner " +
	"renders the block and writes the sealed durable record from your report, attributed to this session's " +
	"harness and model." + reviewMUSTBarGrades

// runnerReviewTerminalStep ends the review-only phase as a checkpoint: the
// runner records the review and the fix phase resumes the same run from the
// branch.
const runnerReviewTerminalStep = " This is a CHECKPOINT: keep holding, do NOT commit, push or open a PR. The runner " +
	"checkpoints on the branch the implement phase recorded, and the phase after you resumes this same run from " +
	"it. Ending this session after the report is this phase FINISHING, not the task being abandoned. The ONLY " +
	"exception is a declared handoff."

// runnerReviewOnlyBrief is the review-only gate's brief for a runner-claimed
// session: the shipped core, with the runner's resume and worktree facts in
// place of the claim and the worktree hunt, and the done report in place of the
// two record writes.
var runnerReviewOnlyBrief = reviewOnlyBriefOpening + reviewHandoffVerified + runnerReviewResume +
	runnerReviewWorktreeAndGate + runnerReviewRecord + runnerDoneReport(runnerReviewReportFields) +
	runnerReviewTerminalStep + runnerRerunGuidance + runnerForbidden + handoffGuidance + runnerReportReminder

// runnerFixCore is the fix phase's substance for a runner-claimed session,
// written fresh rather than derived: the record it reads is still the shipped
// one, but the park and the updates it used to write itself now leave in the
// done report.
var runnerFixCore = "Read BOTH the durable record and the block: `reviews.records` carries every released record " +
	"in the newest round, each with its findings[] and the plane-assigned id (`<round>.<reviewer role>.<n>`) you " +
	"report a findingUpdate by; the outcome's `" + ReviewRecordHeading + "` block is the legacy copy. On a later " +
	"fix round (after a verification round) the newest record carries only the verifier's NEW findings: the work " +
	"list is every MUST the plane still shows OPEN on ANY released record (get_task's `reviews.openMustIds`), " +
	"which the driver prepends when it can read it - an open MUST from an earlier round is yours to fix or " +
	"refute too. If there is neither a released review record nor a review block, that is a FAILED hand-off: end " +
	"the session with a done report whose `park` names the missing record, and change nothing - do NOT review the " +
	"work yourself, do NOT invent findings, and do NOT open a PR over unreviewed work. Read the record's " +
	"reviewedTip and compare it with `git rev-parse HEAD` in the worktree: if they differ, the branch moved after " +
	"the review, so do NOT apply stale findings - end the session with a done report whose `park` names the " +
	"mismatch, and change nothing. " + runnerWorktreeAtTip + " Apply the review's findings, each by its PLANE id " +
	"from the durable record - the block's R-numbers are labels, not the keys you update by." + runnerFixGradingRule +
	" A finding you neither fix nor answer is this phase failing. The review " +
	"ran checks, so the worktree may carry generated or cache files it did not commit: stage only the fixes " +
	"you made, never those. Re-verify " +
	"SCOPED to those fixes: run the gates the repo's own instructions name, and any check a finding says was " +
	"red, not the whole suite again for no reason."

// runnerFixUpdates puts the per-finding updates and the fix summary in the done
// report instead of a record_review call: the runner sends them and appends the
// `### Fixes` outcome itself.
const runnerFixUpdates = " THE FINDING UPDATES GO IN THE SAME REPORT, never with record_review: the report's " +
	"`findingUpdates` carries one entry per finding by its plane id, in the shape the report's field list gives. " +
	"The runner sends the updates and appends the `### Fixes` outcome itself, so do not write an outcome, and do " +
	"not preserve one - that is the runner's job now."

// runnerFixCreateTask is the one write a fix session may still make: a follow-up
// the review asked for stays a task of its own, and its ref travels back in the
// report rather than being recorded by the session.
const runnerFixCreateTask = " The ONE write that is yours in this session is create_task, for a follow-up a " +
	"finding deserves (with a real doneWhen): send the new task's ref back as that finding's `filed` update."

// runnerFixBrief is fixBrief for a runner-claimed session: the fix phase that
// still opens the PR, with the hand-over and the record writes owned by the
// runner.
var runnerFixBrief = "You are PHASE " + PhaseNumberPlaceholder + " of " + PhaseTotalPlaceholder + " on task " +
	PhaseTaskPlaceholder + ", the FIX phase and the LAST phase of this task. The review phase reviewed branch " +
	PhaseBranchPlaceholder + " and wrote its findings as a sealed DURABLE review record; the runner has now " +
	"closed that round, so get_task releases the findings to you. It also left the legacy `" +
	ReviewRecordHeading + "` block in the task's outcome for one release — the LAST such block, because an " +
	"earlier `" + ReviewRecordHeading + "` block is an earlier round. It changed nothing." + runnerResumeNoClaim +
	" " + runnerFixCore + runnerFixUpdates + runnerFixCreateTask +
	" If the review found nothing " +
	"(verdict: pass), there is nothing to fix: commit nothing and report an empty findingUpdates." +
	" FINALLY, and this is the step that ENDS the phase: COMMIT and PUSH the fixes, then open a PR targeting the " +
	"repo's PR base branch (" + PhasePRBasePlaceholder + ") if none exists for this branch - push, then the PR, " +
	"in that order." + worktreeReclaimStep + " Then end the session with the done report. The runner hands the " +
	"task over with submit_for_review itself." + runnerDoneReport(runnerFixLastReportFields) + runnerRerunGuidance +
	runnerForbidden + handoffGuidance + runnerReportReminder

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
	runnerFixUpdates + runnerFixCreateTask +
	" If the review found nothing (verdict: pass), there is nothing to fix: commit nothing, leave the branch " +
	"as it is, and report an empty findingUpdates - the verification round still runs and carries the terminal " +
	"step." +
	" FINALLY, and this is the step that ENDS the phase: COMMIT and PUSH the fixes you made, then end the session " +
	"with the done report. Do NOT open a PR and do NOT move the task to in_review: the VERIFICATION round after " +
	"you carries that terminal step once it finds no open MUST, and a PR opened here would ship fixes the " +
	"verifier never saw. Ending this session holding the task is the checkpoint the verification round resumes " +
	"from." + runnerDoneReport(runnerFixReportFields) + runnerRerunGuidance + runnerForbidden + handoffGuidance + runnerReportReminder

// runnerVerifyBrief is verifyBrief for a runner-claimed session: the review
// scope, the adjudication and the stopping rule are the shipped substance; the
// resume, the worktree, the polling rule and the record - a done report the
// runner writes from - differ.
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
	"in the report's notes. " +
	"ADJUDICATE EVERY PRIOR OPEN MUST BY ITS PLANE ID — exactly one disposition each, as an entry in the report's " +
	"`findingUpdates`, by the id the sealed record gave it: " +
	"(1) RESOLVED — the fix is present and correct: status \"fixed\" with fixCommit. " +
	"(2) RESOLVED BY DISPATCH — the fix is a follow-up task: status \"filed\" with filedTaskRef. " +
	"(3) REFUTATION ACCEPTED — the fixer's evidence shows the finding was wrong: status \"refuted\" with " +
	"refutedReason. " +
	"(4) UNRESOLVED or REFUTATION REJECTED — leave the finding's status \"open\" and say in the report's notes " +
	"why the fix or the refutation does not stand. " +
	"A MUST the fix phase already MARKED fixed, refuted or filed is not settled by that marking: the fixer " +
	"marks what it answers, and a refutation changes no code, so the delta will not show it. Check each one " +
	"the same way. Confirm it by leaving its status; reject it by setting its status back to \"open\" in " +
	"findingUpdates and saying why in notes — that is disposition (4), and the gate then sees it open. " +
	"You may raise NEW MUSTs ONLY within the fix delta and its regression surface; they go in the report's " +
	"`findings`, and the plane assigns their new ids. Do not re-raise an earlier finding as a new id: adjudicate " +
	"the existing id. " +
	"REVIEW ONLY: you do not change code — do not edit, create or delete a source file, do not commit, and do " +
	"not apply a fix yourself. The phase after you applies what you find, and a fix applied here is a fix it " +
	"cannot attribute. The one exception is the hand-over below: when no MUST remains open, opening the PR " +
	"(and pushing a fix commit that the fixer left local) is the terminal step, not a review change. " +
	" FINALLY, and this is the step that ENDS the phase. If ANY MUST is still open after your adjudication: " +
	"just end the session with the done report - do NOT open a PR, do NOT move the task to in_review and do NOT " +
	"change its status. The runner records the round, releases it, and spawns another fix or parks the task at " +
	"the round cap.\n\n" +
	"If NO MUST remains open: confirm the branch is pushed (push any fix commit the fixer left local), open a " +
	"PR targeting the repo's PR base branch (" + PhasePRBasePlaceholder + ") if none exists for the branch." +
	worktreeReclaimStep + " Then end with the report including " + runnerRiskField + ". The runner records the " +
	"round and hands the task to in_review. Never decide to run another round yourself." +
	verifyStoppingRule + verifyDriverBounds + runnerDoneReport(runnerVerifyReportFields) + runnerRerunGuidance +
	runnerForbidden + handoffGuidance + runnerReportReminder

// RunnerHandoffPreamble is HandoffPreamble for a runner-claimed session: the
// runner holds the lease, so no heartbeat, and the work stays in the worktree the
// runner started it in.
const RunnerHandoffPreamble = "You are resuming run " + PhaseRunPlaceholder + " on task " + PhaseTaskPlaceholder +
	", handed to you by the previous session on this task via a self-chosen handoff. The runner claimed the task " +
	"and holds its lease: do NOT call next_task, claim_task or heartbeat, and stay within this phase's scope and " +
	"in the worktree you were started in. What follows is the previous session's own continuation prompt:\n\n"

// RunnerHandoffContinuation adapts a shipped handoff continuation for a
// runner-claimed session: the waiting reference becomes the brief's own rule and
// the runner's prohibition - including the write ban - is appended.
func RunnerHandoffContinuation(continuation string) string {
	if continuation == "" {
		return ""
	}
	return strings.Replace(continuation, rerunGuidance, runnerRerunGuidance, -1) + runnerForbidden
}

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

// RunnerVerifyBrief returns the verification round's brief for a
// runner-claimed session.
func RunnerVerifyBrief() string { return runnerVerifyBrief }
