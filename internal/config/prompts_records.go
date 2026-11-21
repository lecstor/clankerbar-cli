package config

// worktreeReclaimStep is the reclaim every FINAL phase step carries: the session that takes the
// work off the machine removes the tree it worked in, because the close-out that merges may not
// touch a tree it did not create.
const worktreeReclaimStep = " AFTER the PR is confirmed open - and before this session's final call - " +
	"reclaim the tree this run worked in, in ONE shell invocation started from the repo's main checkout (the " +
	"first path `git worktree list` prints, so the shell is never left in a deleted directory): " +
	"`git worktree remove <this run's worktree>` - the path `git worktree list` shows for branch " +
	PhaseBranchPlaceholder + " - then `git branch -d " + PhaseBranchPlaceholder + "`. Before the delete, point " +
	"the branch at its own remote with `git push -u origin " + PhaseBranchPlaceholder + "` (or " +
	"`git branch --set-upstream-to=origin/" + PhaseBranchPlaceholder + " " + PhaseBranchPlaceholder + "`): `-d` " +
	"deletes only a branch fully merged in its upstream, and a fresh task tree tracks the PR base, so without " +
	"this it refuses a pushed, unmerged branch. Never pass `--force` or `-D`: the expected refusals are that " +
	"unmerged-base upstream and untracked files you generated or cached - delete only files you created (never " +
	"a tracked modification) and retry, or leave the tree and report it; a refusal is not evidence of unpushed " +
	"work. Expect gitignored local state to go with the tree: collateral, not work. Reclaim only this run's own " +
	"tree, never another task's worktree."

// reviewTerminalStep is the review phase's LAST instruction, kept as a constant so its position can
// be pinned by a test and not only its wording.
const reviewTerminalStep = " Then COMMIT and PUSH the fixes. Open a PR targeting the repo's PR base " +
	"branch (" + PhasePRBasePlaceholder + ") if no PR exists yet for this branch - push, then the PR, in that order, before the " +
	"hand-off below." + worktreeReclaimStep +
	" FINALLY, and this is the step that ENDS the phase: hand the task over with " +
	"submit_for_review(taskId, runId, outcome: ..., risk: { touches: [...] }), where the outcome MUST carry a " +
	"**Tests** section saying what you actually verified - without one the plane REFUSES the call, so a " +
	"session that leaves it out has not handed anything over. The same call's risk declaration uses touches: [] " +
	"when it touches none of the project's risk classes, an omitted risk being UNDECLARED and buying no " +
	"per-batch auto-approval; the vocabulary and the judging rule are in the served `finishing` reference and " +
	"the project's risk_vocabulary (get_project_policy). Ending this session while you still hold the " +
	"task is this phase FAILING, not finishing: the work you pushed then gets rediscovered and paid for a " +
	"second time by whoever takes it over. The ONLY exception is a declared handoff."

// reviewOnlyTerminalStep ends the review-only phase.
const reviewOnlyTerminalStep = " FINALLY, and this is the step that ENDS the phase: record the review ON THE TASK " +
	"in BOTH forms the shape above prescribes, in that order - FIRST the legacy outcome block with " +
	"record_progress(taskId, runId, outcome: ...), THEN the sealed durable record with " +
	"record_review(taskId, runId, {...}). The durable record is the driver's seam evidence; the block is " +
	"the one-release compatibility copy. This is a CHECKPOINT, not a hand-off: " +
	"keep holding the task - do NOT release it, do NOT move it to in_review, and do NOT COMMIT, PUSH or open a " +
	"PR. The driver checkpoints on the branch the implement phase recorded, and the phase after you resumes " +
	"this same run from it. Ending this session after recording the review is this phase " +
	"FINISHING, not the task being abandoned. The ONLY exception is a declared handoff."

// panelMemberTerminalStep ends a PANEL MEMBER's phase.
const panelMemberTerminalStep = " FINALLY, and this is the step that ENDS the phase: record the review ON THE " +
	"TASK in the ONE form the shape above prescribes - the sealed durable record with " +
	"record_review(taskId, runId, {...}) - and write NOTHING ELSE on the task: do NOT call record_progress with an outcome, do not write " +
	"an outcome of your own, and do not touch the task's status or body. That is a CHECKPOINT, not a hand-off: " +
	"keep holding the task - do NOT release it, do NOT move it to in_review, and do NOT COMMIT, PUSH or open a " +
	"PR. The driver checkpoints on the branch the implement phase recorded, and the phase after you (the next " +
	"panel member, or the fix phase) resumes this same run from it. Ending this session after recording the " +
	"review is this phase FINISHING, not the task being abandoned. The ONLY exception is a declared handoff."

// reviewRecordOutcomeShape is the LEGACY half of the review record: the `## Review` block appended
// to the task's outcome.
const reviewRecordOutcomeShape = " THE REVIEW RECORD IS THE DELIVERABLE. First get_task and read the outcome as it " +
	"stands: a statusless record_progress outcome replaces the WHOLE field, so preserve it. Record the LEGACY block " +
	"with record_progress(taskId, runId, outcome: ...) by copying the existing outcome's text unchanged, then " +
	"APPENDING, below it, a compact block in exactly this shape: a `" + ReviewRecordHeading + "` heading; then " +
	"`- reviewed tip: <the SHA you actually reviewed>`; `- implemented by: harness " + PhasePrevHarnessPlaceholder +
	", model " + PhasePrevModelPlaceholder + "`; `- reviewed by: harness " + PhaseHarnessPlaceholder + ", model " +
	PhaseModelPlaceholder + "`; `- verdict: pass` or `- verdict: findings`; then a `### Findings` heading with " +
	"one line per finding: `- R1 [must|should|nit]` followed, for a MUST, by ` (<kind>): <evidence>`, then " +
	"` <file:line> - <what is wrong>. Required fix: <the smallest change that makes it right>.` Give every " +
	"finding its own ID. If there are none, write `- none` under " +
	"Findings and `verdict: pass`. Do not pad the list, and do not fix anything yourself - not even a typo: a " +
	"fix applied here is a fix the next phase cannot attribute and may undo."

// reviewRecordTipVerified tells the reviewer which commit the driver verified and requires that
// exact tip in both halves of the record.
const reviewRecordTipVerified = " The driver verified the origin remote's tip for branch " + PhaseBranchPlaceholder +
	" as `" + ReviewedTipPlaceholder + "`. Fetch and confirm the worktree is at that commit (`git rev-parse HEAD`) " +
	"before you review, and record that SHA as `reviewedTip` in the durable record and on the block's " +
	"`- reviewed tip:` line."

// reviewRecordTipUnverified is the counterpart for a checkpoint the driver could NOT verify against
// the origin remote: there is no driver-known tip, so the reviewer establishes it itself, and the
// seam requires the record to carry a non-empty tip rather than matching one it never had.
const reviewRecordTipUnverified = " The driver could NOT verify the origin remote's tip, so there is no " +
	"driver-verified tip to record: establish it yourself. Read the branch from the origin remote " +
	"(`git ls-remote origin " + PhaseBranchPlaceholder + "`, or fetch it), check out the commit it names, review " +
	"that revision, and record that SHA as `reviewedTip` in the durable record and on the block's " +
	"`- reviewed tip:` line. If the branch is absent or the remote cannot be read, record the failed hand-off " +
	"and end holding the task - do not invent a tip and do not review a phantom diff."

// reviewRecordTipNone is the branchless counterpart: there is no branch and no commit to review,
// and — per the operator's decision on question 03a12578, option 1 — no durable record either.
const reviewRecordTipNone = " There is NO branch and NO tip to review: this record is about the PLANE'S RECORD of " +
	"what phase 1 did. Write `- reviewed tip: none (no branch - the plane record was reviewed)` on the block's " +
	"`- reviewed tip:` line. Do NOT append a durable review record: a settled task has no live holder to accept " +
	"it, and on a still-held no-code task it would seal a round this variant never closes - the legacy block IS " +
	"this variant's review record, and the driver gates the seam on it."

// reviewRecordDurableShape is the durable half: one sealed per-reviewer record per run, written
// through record_review.
const reviewRecordDurableShape = " THEN, and only after that outcome write has SUCCEEDED, append THIS run's durable " +
	"review record with record_review(taskId, runId, {...}). This order is load-bearing: the plane seals the " +
	"record's round, and a sealed record makes this run's outcome, body and decision writes illegal - so the legacy " +
	"block above must already be on the task. The durable record is what the driver's review->fix seam checks; a " +
	"missing or mismatched one means the fix phase does not spawn. Send exactly: reviewerRole: \"" +
	ReviewerRolePlaceholder + "\" (this phase's role; the plane assigns the round and cycle from it), verdict: " +
	"\"pass\" or \"findings\", harness: \"" + PhaseHarnessPlaceholder + "\", model: \"" + PhaseModelPlaceholder +
	"\", branch: \"" + PhaseBranchPlaceholder + "\", reviewedTip as above, scope: {kind: \"full-diff\", base: \"" +
	PhasePRBasePlaceholder + "\", range: \"" + PhasePRBasePlaceholder + "..<reviewedTip>\"} (the plane caps range at 100 " +
	"characters, so never put the branch name there), and findings: one object per finding in the block, in " +
	"the same order, at most 20 (the plane refuses a record with more: keep every MUST, then the most important " +
	"of the rest, and say in the block how many you left out): {severity: \"must\" | \"should\" | \"nit\", " +
	"evidenceKind: <the MUST's kind token>, evidence: <its payload>, file: <path>, line: <number>, kind: <short category>, summary: <what is " +
	"wrong>, requiredFix: <the smallest fix>, raisedByModel: \"" + PhaseModelPlaceholder + "\"}. The plane assigns " +
	"each finding's id (`<round>.<reviewer role>.<n>`); do NOT invent one - the fix phase updates by that id. A " +
	"RETRY proves itself here: get_task before writing, and if a record for your reviewer role and THIS run is " +
	"already there (records show the run id WITHOUT its secret: compare only the part of " + PhaseRunPlaceholder +
	" before its first dot) in an open round, the record is written and its block landed before " +
	"it (the plane seals the outcome once the record exists) - skip both writes and end. If instead the plane " +
	"refuses the append with duplicate_reviewer, the same follows: a record for this role and round is already " +
	"written, that refusal is the idempotency signal, and retrying it changes nothing. If there are no findings, " +
	"send no findings (or an empty array), write `- none` under Findings, and verdict: pass."

// reviewOnlyRecordShape is the verified review-only phase's full record contract: the legacy
// outcome block, then the durable record.
const reviewOnlyRecordShape = reviewRecordOutcomeShape + reviewRecordTipVerified +
	reviewRecordDurableShape + reviewMUSTBar

// unverifiedReviewOnlyRecordShape is reviewOnlyRecordShape for a checkpoint the driver could not
// verify on the origin remote: the reviewer establishes the tip itself, and the seam demands a
// non-empty one.
const unverifiedReviewOnlyRecordShape = reviewRecordOutcomeShape + reviewRecordTipUnverified +
	reviewRecordDurableShape + reviewMUSTBar

// noCodeReviewOnlyRecordShape is the branchless review-only phase's record contract: the LEGACY
// outcome block alone, with no durable-record half — the plane accepts review writes only from
// the task's LIVE holder and a settled task has none (operator decision on question 03a12578,
// option 1), so the seam gates this variant on the block (changed since the spawn, heading on its
// own line, and a last block the baseline did not carry).
const noCodeReviewOnlyRecordShape = reviewRecordOutcomeShape + reviewRecordTipNone + reviewMUSTBar

// panelMemberRecordIntro and panelMemberRecordBody are the two halves of a PANEL MEMBER's record
// contract: the tip paragraph sits between them, so the verified and unverified variants share
// everything else.
const panelMemberRecordIntro = " You are ONE MEMBER of a review PANEL: several reviewers, possibly on " +
	"different models, review the SAME revision independently, and the driver merges every member's findings " +
	"into the fix phase's work list afterwards. Do not look for, wait for or defer to another member's " +
	"findings - review the revision yourself and record your own."

const panelMemberRecordBody = " THE RECORD IS YOUR DELIVERABLE, and it is the ONLY thing you write on the " +
	"task: record it with record_review(taskId, runId, {...}). Do NOT write the task's outcome - do not " +
	"call record_progress with an outcome, and do not hand the task over: the fix phase carries the " +
	"panel's findings into the task's outcome, and the member after you reads the task, so an outcome write here " +
	"would leak this review to a reviewer that must not see it. Send exactly: reviewerRole: \"" +
	ReviewerRolePlaceholder + "\" (this member's role; the plane assigns the round and cycle from it), verdict: " +
	"\"pass\" or \"findings\", harness: \"" + PhaseHarnessPlaceholder + "\", model: \"" + PhaseModelPlaceholder +
	"\", branch: \"" + PhaseBranchPlaceholder + "\", reviewedTip as above, scope: {kind: \"full-diff\", base: \"" +
	PhasePRBasePlaceholder + "\", range: \"" + PhasePRBasePlaceholder + "..<reviewedTip>\"} (the plane caps range at 100 " +
	"characters, so never put the branch name there), and findings: one object per finding in YOUR review, in " +
	"the same order you report them, at most 20 (the plane refuses a record with more: keep every MUST, then the " +
	"most important of the rest, and say in your final message how many you left out): {severity: \"must\" | " +
	"\"should\" | \"nit\", evidenceKind: <the MUST's kind token>, evidence: <its payload>, file: <path>, line: " +
	"<number>, kind: <short category>, summary: <what is wrong>, requiredFix: <the smallest fix>, raisedByModel: \"" +
	PhaseModelPlaceholder + "\"}. The plane assigns each finding's id (`<round>.<reviewer role>.<n>`); do NOT " +
	"invent one - the fix phase updates by that id. A RETRY proves itself here: get_task before writing, and if " +
	"a record for your reviewer role and THIS run is already there (records show the run id WITHOUT its secret: " +
	"compare only the part of " + PhaseRunPlaceholder + " before its first dot) in an open round, the record is " +
	"already written - skip the write and end. If instead the plane refuses the append with duplicate_reviewer, " +
	"the same follows: a record for this role and round is already written, that refusal is the idempotency " +
	"signal, and retrying it changes nothing. If there are no findings, send no findings (or an empty array), " +
	"and verdict: pass."

// panelMemberRecordTipVerified is the panel member's counterpart of reviewRecordTipVerified: the
// same driver-verified tip, recorded in the durable record alone.
const panelMemberRecordTipVerified = " The driver verified the origin remote's tip for branch " +
	PhaseBranchPlaceholder + " as `" + ReviewedTipPlaceholder + "`. Fetch and confirm the worktree is at that " +
	"commit (`git rev-parse HEAD`) before you review, and record that SHA as `reviewedTip` in your durable " +
	"record."

// panelMemberRecordTipUnverified is the panel member's counterpart of reviewRecordTipUnverified:
// the member establishes the tip itself.
const panelMemberRecordTipUnverified = " The driver could NOT verify the origin remote's tip, so there is no " +
	"driver-verified tip to record: establish it yourself. Read the branch from the origin remote " +
	"(`git ls-remote origin " + PhaseBranchPlaceholder + "`, or fetch it), check out the commit it names, review " +
	"that revision, and record that SHA as `reviewedTip` in your durable record. If the branch is absent or the " +
	"remote cannot be read, record that failed hand-off IN YOUR DURABLE RECORD and end holding the task — do not " +
	"write an outcome or touch the task's status, do not invent a tip, and do not review a phantom diff."

// panelMemberRecordShape is the verified panel member's record contract: the panel context, the
// verified tip, and the durable record alone.
var panelMemberRecordShape = panelMemberRecordIntro + panelMemberRecordTipVerified +
	panelMemberRecordBody + panelMemberMUSTBar

// panelMemberUnverifiedRecordShape is panelMemberRecordShape for a checkpoint the driver could not
// verify on the origin remote.
var panelMemberUnverifiedRecordShape = panelMemberRecordIntro + panelMemberRecordTipUnverified +
	panelMemberRecordBody + panelMemberMUSTBar

// reviewMUSTBarGrades is the accepted bar for a MUST: the evidence a MUST must cite, the seventh
// kind the operator added after the second review, and the things that are never a MUST however
// confident the reviewer is.
const reviewMUSTBarGrades = " GRADE EVERY FINDING. A severity in brackets is `[must]`, `[should]` or `[nit]`; " +
	"MUST is the only gating grade and the only one that must carry evidence. A MUST must be followed by " +
	"`(<kind>): <evidence>`, where <kind> is one of: doneWhen - quote the clause of the task's doneWhen the change " +
	"violates; gate - name a repo gate (a test, typecheck, lint, build, CI check, or a check the repo's agent " +
	"instructions require) that fails or would fail, with the command and the failing output or the reason it " +
	"would fail; failure - a concrete input or state and the wrong output, crash, data loss or stuck state it " +
	"produces in changed code or its direct regression surface; security - credential exposure, an authorization " +
	"bypass, injection, or a widened permission, with the path; decision - a recorded standing decision it " +
	"contradicts (cite its id) or a hard rule in the repo's agent instructions (cite the file and the rule); " +
	"irreversible - an irreversible migration, deletion or publish step without the guard the repo requires; " +
	"untested - a new or changed guard, gate or state transition with no test that pins it (cite its file:line " +
	"and the missing test). NEVER a MUST, however confident you are: style, naming, a design you would prefer, a " +
	"performance concern without a measurement, missing tests the doneWhen does not require (except untested), or " +
	"documentation wording - and where a repo hard rule concerns style, prose or documentation wording, this " +
	"not-a-MUST list wins. Write those as SHOULD or NIT; they need no evidence. "

// reviewMUSTBarAttribution closes the bar for the SINGLE reviewer, whose record has two halves: the
// legacy outcome block and the durable record.
const reviewMUSTBarAttribution = "Every finding in the block is " +
	"attributed by the block's own `- reviewed by:` line, and the durable record carries `raisedByModel` on every " +
	"finding so the review is attributable per finding, not only per block."

// reviewMUSTBar is the bar as the single reviewer's record shape carries it: the grading rules,
// then the attribution sentence.
const reviewMUSTBar = reviewMUSTBarGrades + reviewMUSTBarAttribution

// panelMemberMUSTBarAttribution is reviewMUSTBarAttribution for a panel member: there is no block,
// so the record's own `raisedByModel` is the whole attribution.
const panelMemberMUSTBarAttribution = "Every finding is attributed in your durable " +
	"record by its `raisedByModel` field, so the review is attributable per finding."

// panelMemberMUSTBar is the panel member's bar: the same grading rules, the record-only
// attribution.
const panelMemberMUSTBar = reviewMUSTBarGrades + panelMemberMUSTBarAttribution

// fixOutcomeRecord is the fix phase's outcome requirement, shared by both fix variants.
const fixOutcomeRecord = "the outcome MUST keep everything the existing outcome carries ABOVE the LAST `" +
	ReviewRecordHeading + "` block " +
	"(phase 1's delivery record plus any earlier rounds, their `" + ReviewRecordHeading + "` blocks included) " +
	"unchanged, keep that LAST block VERBATIM " +
	"(verdict, every finding by ID, its required fix, and the attribution it already carries — do not restate " +
	"that attribution from this brief), and add a `### Fixes` list saying, for every finding, whether you fixed " +
	"it (with the commit SHA, or for a no-code task the record change you made, or the ref of the real follow-up " +
	"task a code-needing finding was dispatched to) or did not (with the reason — for a MUST that reason must be " +
	"a refutation carrying its evidence, because a MUST is never downgraded), plus 'fixed by: harness " +
	PhaseHarnessPlaceholder + ", model " + PhaseModelPlaceholder + "'. A finding you neither fix nor answer is " +
	"this phase failing."

// fixFindingUpdates is the durable half of the fix phase's record requirement: the per-finding
// status updates, which must go out while this run is still the task's HOLDER because the plane
// refuses them once it is not.
const fixFindingUpdates = " While you still hold the task, update every finding's " +
	"status on the DURABLE review record with record_review(taskId, runId, {findingUpdates: [...]}) — one " +
	"entry per finding, by the plane-assigned `id` the record gave you: {findingId, status: \"fixed\", " +
	"fixCommit: <the commit SHA>} for a fixed finding; {findingId, status: \"refuted\", refutedReason: <the " +
	"evidence that refutes it>} for a MUST you did not fix; {findingId, status: \"filed\", filedTaskRef: <the " +
	"follow-up task's ref>} for a code-needing finding dispatched instead. The batch is all-or-nothing: an " +
	"unknown id or a still-sealed round refuses it, so it must go out while this run is the holder. An empty " +
	"array is refused; if the review found nothing, skip the call."

// fixTerminalRecord is the BRANCH fix phase's record requirement when this fix OWNS the sequence's
// terminal step, stated BEFORE the shared reviewTerminalStep so the emphatic final block still ends
// the brief: the durable finding updates go out before the call that ends the run and clears the
// holder, then the hand-over carries the outcome requirement (fixOutcomeRecord) forward.
const fixTerminalRecord = fixFindingUpdates + " It must go out BEFORE the hand-over call - before the call that " +
	"moves the task to in_review, even when that same call carries your outcome. THEN hand the task over: " +
	fixOutcomeRecord

// fixVerifyRecord is the BRANCH fix phase's record requirement when a VERIFICATION round FOLLOWS
// it.
const fixVerifyRecord = fixFindingUpdates + " When you end the phase, " + fixOutcomeRecord

// noCodeFixTerminalRecord is the BRANCHLESS fix phase's counterpart (question 03a12578, option 1):
// the branchless review writes no durable record — a settled task has no live holder to accept
// one — so there is no findingUpdates call to make and no plane-assigned id to send.
const noCodeFixTerminalRecord = " When you hand the task over, " + fixOutcomeRecord

// noCodeFixVerifyRecord is fixVerifyRecord for a BRANCHLESS fix: the outcome requirement alone,
// with no findingUpdates half — the branchless round writes no durable record, so there is no
// plane-assigned id to send, the same exemption noCodeFixTerminalRecord carries — and no
// hand-over, which the verification round owns.
const noCodeFixVerifyRecord = " When you end the phase, " + fixOutcomeRecord

// fixGradingRule is the fix briefs' shared obligation for acting on the graded review record: every
// MUST fixed or refuted with evidence, never downgraded by the fixer, and SHOULD/NIT handled by the
// filing rule.
const fixGradingRule = " Every MUST is fixed, or REFUTED with evidence " +
	"recorded in your outcome - a refutation must actually refute the finding, and a MUST you neither fix nor " +
	"refute is this phase failing. You may not downgrade a MUST: the driver has already applied its structural " +
	"check, listing a recorded MUST with no evidence kind or no evidence payload as a SHOULD, and a MUST raised " +
	"by a single reviewer is never dropped because nobody else raised it. Fix the SHOULDs that are cheap; record " +
	"NITs and the SHOULDs you do not fix in your outcome as not done, with the reason, and file a follow-up task " +
	"only for a SHOULD worth doing."

// noCodeReviewTerminalStep is the no-code review brief's LAST instruction, the counterpart of
// reviewTerminalStep for a phase-1 exit that was evidenced by the plane's record rather than a
// branch.
const noCodeReviewTerminalStep = " FINALLY, and this is the step that ENDS the phase: if the task still sits in " +
	"in_progress with its no-code delivery declared, hand it over with submit_for_review(taskId, runId, " +
	"outcome: ..., risk: { touches: [...] }), where the outcome MUST carry a **Tests** section saying what you actually " +
	"verified - without one the plane REFUSES the call, so a session that leaves it out has not handed " +
	"anything over. The same call's risk declaration uses touches: [] when it touches none of the project's risk " +
	"classes, an omitted risk being UNDECLARED and buying no per-batch auto-approval; the vocabulary and the " +
	"judging rule are in the served `finishing` reference and the project's risk_vocabulary (get_project_policy). " +
	"If phase 1 already settled the task (it left in_progress before this phase ran), the " +
	"handover already happened: verify the record reads honestly and end the session - do NOT re-open, " +
	"release or re-settle the task. Ending this session while the task still needs the handover is this " +
	"phase FAILING, not finishing. The ONLY exception is a declared handoff."
