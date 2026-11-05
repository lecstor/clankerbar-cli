package config

// HandoffMarker is the exact line a session puts in its FINAL message to hand
// the rest of its job to a fresh successor session (CLA-352). Everything after
// the marker line is the successor's prompt; the driver respawns on it with the
// same run-continuity rules as a phase resume.
//
// The syntax had to be improbable in ordinary prose and easy to pin in a test,
// and it must not resemble tool-call framing markup: the web repo's
// docs/text-integrity.md records agents destroying their own output merely by
// EMITTING such tags, so an angle-bracket or tag-shaped marker would put the
// defect this feature rides on into the feature itself. A plain punctuated
// uppercase line does neither.
const HandoffMarker = "=====CLANKERBAR HANDOFF====="

// HandoffPromptMax bounds the successor prompt a handoff block may carry, in
// bytes. A session-authored respawn prompt is self-directed prompt injection,
// and the size cap is one of the non-negotiable guards on it: an over-cap
// prompt is refused (logged, normal path) rather than truncated, because a
// truncated brief is a successor confidently working half an instruction. "A
// few KB" is the spec; a genuine continuation prompt — decisions made, state
// verified, exact next steps — fits in far less.
const HandoffPromptMax = 4096

// HandoffPreamble is driver-authored framing prepended to a handoff respawn's
// prompt (CLA-352) — the successor never gets the emitting session's own
// prompt bare. A phase resume (reviewPhaseName, above) works only because its
// brief spells out "do not claim, call heartbeat first": a session-authored
// prompt cannot be trusted to carry that instruction forward on its own, and
// without it a successor calls next_task/claim_task on a fresh task, or its
// run's lease lapses because nothing tells it to hold it. Placed FIRST, not
// appended, so the driver's own constraints bound the prompt that follows
// rather than being one more thing a self-authored prompt could bury or
// contradict — the same reasoning behind refusing to truncate an over-cap
// prompt rather than editing it.
const HandoffPreamble = "You are resuming run " + PhaseRunPlaceholder + " on task " + PhaseTaskPlaceholder +
	", handed to you by the previous session on this task via a self-chosen handoff. Do NOT call next_task, and " +
	"do NOT claim anything: call heartbeat(\"" + PhaseRunPlaceholder + "\") first to resume the run, then stay " +
	"within this phase's scope. What follows is the previous session's own continuation prompt:\n\n"

// HandoffContinuation is appended, by the driver, to a handoff respawn's prompt
// (CLA-353) so a phase's own contract survives a session-authored hand-off.
// HandoffPreamble carries the "resume, don't claim" contract forward on every
// handoff; the ORIGINAL phase brief's own instructions do not — a handoff
// respawn's prompt is HandoffPreamble plus the predecessor's self-authored
// nextPrompt alone, so anything the built-in brief said is otherwise gone the
// moment a session hands off instead of finishing itself.
//
// Two things ride, and the first rides for BOTH built-in phases. The rerun
// bound (CLA-391) rides because a handoff must not be how unbounded rerunning
// arrives: the review successor inheriting a half-fixed finding list is the
// loopiest session there is, but the implement successor mid-verification is
// the same shape — fresh context, no memory of the runs already paid for, and
// every reason to start paying again — so bounding only the review respawn
// would leave the asymmetry as the hole. The review phase ADDITIONALLY carries
// reviewTerminalStep — the PR-then-hand-off sequence CLA-353 exists to make
// land — so a handoff mid-review must not be a way to lose it. The implement
// phase carries the bound alone: it has no terminal step, because it stops
// rather than hands its task to in_review.
func HandoffContinuation(phaseName string) string {
	switch phaseName {
	case ReviewPhaseName:
		return "\n\nThe phase's rerun bound and terminal step are unchanged by handing off:" + rerunGuidance + reviewTerminalStep
	case ImplementPhaseName:
		return "\n\nThe phase's rerun bound is unchanged by handing off:" + rerunGuidance
	case FixPhaseName:
		// The fix phase owns the branch-shaped terminal step, exactly as a final
		// review did before the split. A handoff mid-fix must not be a way to
		// lose the commit/push/PR/in_review sequence, NOR the record contract:
		// fixTerminalRecord is what makes the successor keep the review block
		// verbatim and add the ### Fixes list. Dropping it here loses the only
		// durable record of the round, which is the exact loss the split exists
		// to prevent.
		return "\n\nThe phase's rerun bound, record requirement and terminal step are unchanged by handing off:" +
			rerunGuidance + fixTerminalRecord + reviewTerminalStep
	}
	return ""
}

// HandoffContinuationFor is HandoffContinuation for a review phase whose mode is
// POSITION-dependent (see EffectivePhases): a review-only phase has no terminal
// step to carry, only the record-then-hold ending, and appending the shared
// reviewTerminalStep would tell its successor to commit, push, open a PR and
// hand the task to in_review — the exact actions the review-only mode forbids.
//
// It carries reviewOnlyRecordShape as well as the ending: the ending says
// "in the shape above", and a respawned successor never saw the shape. The
// driver's review→fix gate matches the DURABLE record, so a successor that
// invented its own shape (or skipped the append) would fail the seam.
//
// The flag is only meaningful for ReviewPhaseName; every other phase ignores it.
func HandoffContinuationFor(phaseName string, reviewOnly bool) string {
	if phaseName == ReviewPhaseName && reviewOnly {
		return "\n\nThe phase's rerun bound, record shape and review-only ending are unchanged by handing off:" +
			rerunGuidance + reviewOnlyRecordShape + reviewOnlyTerminalStep
	}
	return HandoffContinuation(phaseName)
}

// NoCodeReviewOnlyHandoffContinuation is the branchless review-only counterpart:
// the record shape and record-then-hold ending without the branch-shaped PR step
// or the no-code handover (which belongs to the fix phase).
func NoCodeReviewOnlyHandoffContinuation() string {
	return "\n\nThe phase's rerun bound, record shape and review-only ending are unchanged by handing off:" +
		rerunGuidance + noCodeReviewOnlyRecordShape + noCodeReviewOnlyTerminalStep
}

// UnverifiedReviewOnlyHandoffContinuation is the review-only handoff for a
// checkpoint the driver could NOT verify on the origin remote (CLA-576): the
// successor must establish the tip itself, exactly as the first session was
// told to, or its durable record cannot satisfy the seam.
func UnverifiedReviewOnlyHandoffContinuation() string {
	return "\n\nThe phase's rerun bound, record shape and review-only ending are unchanged by handing off:" +
		rerunGuidance + unverifiedReviewOnlyRecordShape + reviewOnlyTerminalStep
}

// NoCodeFixHandoffContinuation is the fix phase's continuation for a branchless
// checkpoint: the outcome requirement and the no-code handover, and never the
// branch-shaped PR step. There is no durable record for this variant (question
// 03a12578, option 1 — the branchless review writes none), so the continuation
// carries noCodeFixTerminalRecord, not the branch fix's findingUpdates demand.
func NoCodeFixHandoffContinuation() string {
	return "\n\nThe phase's rerun bound, record requirement and terminal step are unchanged by handing off:" +
		rerunGuidance + noCodeFixTerminalRecord + noCodeReviewTerminalStep
}

// FixVerifyHandoffContinuation is the fix phase's continuation when a
// verification round follows (CLA-638): the successor must still answer every
// finding, commit and push, and then STOP holding the task. Carrying the
// legacy reviewTerminalStep here would tell it to open the PR and move the task
// to in_review — the terminal actions the verifier owns, and precisely what
// B12 says the fix phase must not do — and carrying fixTerminalRecord would do
// the same thing in prose (CLA-686 D1): the record requirement here is
// fixVerifyRecord, which keeps the finding updates and drops the hand-over.
func FixVerifyHandoffContinuation() string {
	return "\n\nThe phase's rerun bound, record requirement and terminal step are unchanged by handing off:" +
		rerunGuidance + fixVerifyRecord + fixOnlyTerminalStep
}

// NoCodeFixVerifyHandoffContinuation is FixVerifyHandoffContinuation for a
// BRANCHLESS fix: the amendment is to the plane's record, so there is nothing
// to commit or push, but the terminal step still belongs to the verifier.
// NoCodeFixHandoffContinuation would carry the legacy no-code hand-over and tell
// the successor to move the task to in_review before the verification round has
// seen the amendment — the same B12 leak the branch-shaped variant exists to
// close.
func NoCodeFixVerifyHandoffContinuation() string {
	return "\n\nThe phase's rerun bound, record requirement and terminal step are unchanged by handing off:" +
		rerunGuidance + noCodeFixVerifyRecord + noCodeFixOnlyTerminalStep
}

// handoffGuidance rides on every built-in phase brief (CLA-352): when a session
// may hand the rest of its job to a fresh successor, and how. The trigger is
// deliberately EVENT-shaped — a session cannot measure its own context (there
// is no token counter in its view), so "hand off when you feel big" is not
// implementable; a pivot is an event the session can recognise.
const handoffGuidance = " HANDOFF (most tasks need zero): if you reach a genuine pivot mid-session - " +
	"exploration finished and implementation about to start, or one distinct sub-goal landed and the next " +
	"beginning - you may hand the REST of this session's job to a fresh successor session that starts without " +
	"your accumulated context. First put durable state where it belongs: discovered constraints in repo docs, " +
	"commit/branch state on the task record. Then end your FINAL message with a line that is exactly " +
	HandoffMarker + " followed by the successor's starting prompt, written the way an operator's continuation " +
	"prompt reads: decisions made, state verified, exact next steps - nothing about the journey, nothing that " +
	"already lives in the repo or on the task, and under 4KB. The successor resumes this same run under this " +
	"same brief's scope, so do NOT settle, release or hand back the task first."

// rerunGuidance rides on every built-in phase brief (CLA-391). Repeated
// verification was the dominant measured waste of the 2026-08-19/20 drain: one
// review phase ran the same Playwright spec fourteen times differing only in
// the grep filter; another ran twelve identical `go test -race ./...` on a
// two-file diff; a third managed seven full-suite runs, eight typechecks and
// ten lints inside one phase. Cost is turns times context, and every rerun is
// another turn over everything the session holds, so the brief bounds
// CONSECUTIVE reruns of the same command and demands a stated reason past the
// bound.
//
// The phase-2 adversarial review found three ways the first wording still let
// the fourteen-run loop through, and the wording closes each:
//
//   - A reason phrased as "name what changed since the last run" was satisfied
//     by the selector change ITSELF - "narrowed the grep" is a change, and it
//     does make the output differ, so fourteen compliant runs each carried a
//     truthful-looking reason line. The reason must name a change to what is
//     UNDER TEST, or say why the last result may not reproduce; a narrower
//     selector is neither.
//   - "Consecutive" was undefined, so any interleaved call (read, grep, diff -
//     the shape real sessions actually produce) reset the count every turn and
//     the ceiling never engaged. Only a change to what the command tests
//     resets it now; other tool calls do not.
//   - The arithmetic past a reason was unstated: a reason buying a fresh pair
//     makes fourteen runs four cheap lines, while an absolute ceiling collides
//     with the review brief's own fix-and-reverify demand. A reason buys ONE
//     run, and the count restarts only when the thing under test changed -
//     which is exactly what happens each time a fix lands, so honest
//     re-verification never reaches the bound at all.
//
// Two readings the task called out stay pinned in the wording: the bound is
// per command, not on verification overall (typecheck, then tests, then lint
// is three commands run once each); and a narrowed rerun is still a rerun
// (same suite, different selector or filter - the fourteen-run case differed
// ONLY by its filter). The sibling failure, consecutive turns idling on a
// background job and re-polling its output, is the waiting problem the served
// skill already carries a reference for - so the brief POINTS at it rather
// than restating rules that live there and would drift here. That pointer is
// SCOPED as well as pointed: waiting.md's Rule 1 tells a claim-holder to let
// go, and read unscoped it directs a mid-phase session to release - the exact
// failure the briefs' own endings forbid - so the session takes the polling
// discipline and keeps hold of the task.
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

// implementResumedBranchRule is the implement brief's rule for resuming an EXISTING branch or
// worktree (CLA-378). EZY-199 was a recovered stale claim whose implement phase followed the
// rest of this brief faithfully - it verified the found tip with a build, the unit suite and
// 60 e2e executions, then pushed - and never synced against staging, which had superseded the
// branch's whole fix a day earlier. The review phase then paid $11.70 for the archaeology,
// merge and salvage that an up-front check costing cents would have made unnecessary.
//
// Three parts, each load-bearing. SYNC first, as a merge and never a rebase: the branch may
// already be pushed, and the daemon's phase-boundary check compares the pushed origin tip to
// the local tip, which a rebase invalidates. RE-VALIDATE against the MERGED tip before doing
// or verifying anything: a clean merge proves nothing - it can silently revert a newer fix -
// so the check is the task's own bar, not conflict markers. On SUPERSESSION, record the
// decision and PARK the task with an outcome citing it - a bare stop leaves the recorded
// branch reading as checkpointed WIP, which spends a review phase rediscovering the stale
// tip - or salvage only what still adds value; never re-verify and push stale work.
//
// Harness-neutral on purpose: one wording serves every harness's implement phase.
//
// CLA-608: the branch to merge in is the repo's PR BASE ({{prBase}}), resolved
// per repo by the driver, not necessarily the project's integration_branch. The
// resumed branch forked from its PR base and will merge back into it, so the
// superseding commits to sync against land there; for a repo whose PRs target
// main (the makesPDF plugins) the integration_branch would name a branch that
// does not exist, and for lecstor/makespdf it would sync against the deploy
// branch while the PR merges elsewhere.
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

// implementCloseOutRule is the implement brief's disposition for the offer the
// plane boosts ahead of fresh work (CLA-540): an approved-awaiting-merge task —
// implementation and review already done, only the close-out remaining — which
// next_task serves FIRST and which the phase-1 brief must not refuse by default.
// A phased implement session is told to claim, work, commit, push and stop at
// the checkpoint; a close-out has no commit, no branch and no worktree, and its
// required act is a status move the brief's letter forbids — so without an
// exception the session asks the operator about an offer that is exactly its
// job (observed twice on 2026-08-28: CLA-533 and CLA-526, both green and
// mergeable, both raised as blocking questions).
//
// Everything the plane already says on the offer stays there: the merge target,
// the red-CI repair rule and the decline door all ride on the offer's mergeHint,
// and the CLA-341 one-representation rule keeps them in that one place. The
// clause also names ONE offer, never the queue — several close-outs pending
// means this session serves the one it was offered and ends, not all of them on
// one accumulating context, which is the failure the unphased prompt's
// drain-vocabulary ban exists to stop.
//
// The ending deliberately does NOT reuse the checkpoint's "Then STOP and end the
// session" sentence: TestBuiltinImplementBriefPinsTheResumedBranchRule locates
// the terminal stop by that exact phrase, and this clause sits before the
// resumed-work rule, so reusing it would move the pin's anchor.
//
// The fork also preempts the resumed-work rule below: a close-out claim carries
// a recorded branch, so that rule's "existing branch" trigger would otherwise
// match an offer it must never apply to - merging the integration branch into an
// approved PR branch would rewrite the diff the approval and the green CI stand
// on. "For found work, not this offer" says the boundary in the brief itself.
const implementCloseOutRule = "If next_task instead offers an approved-awaiting-merge task - implementation and " +
	"review already done, only the close-out remaining - that offer IS this session's job: follow the offer's " +
	"own instructions, and once the close-out is done STOP and end the session. The resumed-work rule below is " +
	"for found work, not this offer. Ending that way is the task going to plan, not the task being abandoned."

// implementCommitEarlyRule is the implement brief's checkpoint floor (CLA-544):
// get a first commit down EARLY, before the design is fully settled - and push
// it and RECORD the branch on the task as soon as that first commit is down -
// then commit and push as you go rather than once at the end. CLA-529 and
// CLA-531 were each handed back empty three times on 2026-08-28: the four
// heaviest sessions contained not one Edit, Write or Create tool call - each
// read and planned for its whole life, a wall-clock kill ended it with a clean
// worktree, and nothing was salvageable.
//
// The mechanism is the driver's checkpoint line (loop.go): a wall-clock-killed
// session earns a checkpoint only with HasWIP, and HasWIP means a branch
// recorded on the TASK - the session's own record_progress(branch), or the salvage
// recording one. The salvage refuses a clean worktree on purpose (salvage.go:
// "The one thing that must NOT happen here is recording a branch"), so a commit
// nobody pushed and recorded is as invisible to the plane as no commit at all.
// The clause therefore names all three acts - commit, push, record - because
// the record is the checkpoint, and it must land as soon as the first commit is
// pushed, not only at the terminal step.
//
// Two constraints shaped the wording. The trigger is an EVENT, never a
// duration: a session cannot read its own wall clock (the constraint that
// shaped handoffGuidance), so "commit within fifteen minutes" is not
// implementable; a skeleton, a failing test, a stub with the signatures are
// events it can recognise. And the clause is a FLOOR, not a ceiling: checkpoint
// pushes are WIP, not deliveries - the terminal step's bar (self-verify, then
// commit, push, record the branch) is unchanged, and a checkpoint push must not
// be passed off as the self-verified terminal delivery.
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

// NoCodeHandoffContinuation is HandoffContinuation for a no-code review
// handoff (CLA-497): the branch-shaped reviewTerminalStep would tell the
// successor to open a PR for a task that has no branch and no code.
func NoCodeHandoffContinuation() string {
	return "\n\nThe phase's rerun bound and terminal step are unchanged by handing off:" + rerunGuidance + noCodeReviewTerminalStep
}
