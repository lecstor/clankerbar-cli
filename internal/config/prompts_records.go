package config

// reviewTerminalStep is the review phase's LAST instruction, kept as a constant so
// its position can be pinned by a test and not only its wording.
//
// Position is the property that failed (CLA-384). The old brief said "Then push,
// and hand the task to in_review" - the instruction was PRESENT, as a trailing
// clause on a long sentence about scoping the follow-up re-verification. Three of
// four review phases in one evening did the work and ended without it. The
// implement brief, whose equivalent step is emphatic, names its call and its
// arguments, and comes last, did not fail once over the same evening. So this is
// written in that shape and pinned to that place.
//
// CLA-353: the terminal sequence is push, then open a PR, then the hand-off - in
// that order, and now all three live here rather than "push" sitting one sentence
// earlier. An overnight forensic drain found `gh pr create` appearing ZERO times
// across four review-phase transcripts, and two tasks committed straight onto
// `staging` because a session's cwd was already sitting there and nothing told it
// that was wrong - so a task closed at in_review with no PR, and the close-out
// flow's "merge its PR into staging" had nothing to merge. CLA-384 made
// the hand-off the phase's emphatic final call, which if the PR step were bolted on
// earlier would make it LESS likely to survive, not more - a step mentioned before
// an emphatic final call loses to it. So the PR step is folded into this same
// terminal block, before the hand-off sentence, not left in the paragraph above.
//
// CLA-608: the target was the literal "(staging)" here and in the never-commit
// rule below. Neither read the config nor the task's repo, so every task in a
// repo with no staging branch (the makesPDF plugins; lecstor/makespdf, whose
// PRs target integration) was told to open its PR against a branch that does
// not exist. The target is now {{prBase}}, resolved per repo by the driver.
const reviewTerminalStep = " Then COMMIT and PUSH the fixes. Open a PR targeting the repo's PR base " +
	"branch (" + PhasePRBasePlaceholder + ") if no PR exists yet for this branch - push, then the PR, in that order, before the " +
	"hand-off below. FINALLY, and this is the step that ENDS the phase: hand the task over with " +
	"submit_for_review(taskId, runId, outcome: ..., risk: { touches: [...] }), where the outcome MUST carry a " +
	"**Tests** section saying what you actually verified - without one the plane REFUSES the call, so a " +
	"session that leaves it out has not handed anything over. The same call's risk declaration uses touches: [] " +
	"when it touches none of the project's risk classes, an omitted risk being UNDECLARED and buying no " +
	"per-batch auto-approval; the vocabulary and the judging rule are in the served `finishing` reference and " +
	"the project's risk_vocabulary (get_project_policy). Ending this session while you still hold the " +
	"task is this phase FAILING, not finishing: the work you pushed then gets rediscovered and paid for a " +
	"second time by whoever takes it over. The ONLY exception is a declared handoff."

// reviewOnlyTerminalStep ends the review-only phase. It is the counterpart of
// reviewTerminalStep for a phase that must NOT fix, commit, push, open a PR or
// hand the task to in_review: the review is a checkpoint, and the next (fix)
// phase resumes the same run from the branch the implement phase recorded.
//
// The review's FINDINGS are the deliverable, and they are recorded on the task
// with record_progress(outcome: ...) — the plane's one owner of the statusless
// outcome write. That keeps the record durable across the seam, readable by the
// fix phase's get_task, and attributed to the run that wrote it. A file in the
// worktree was rejected: it is invisible to the plane, dies with the worktree,
// and cannot be read from another machine.
const reviewOnlyTerminalStep = " FINALLY, and this is the step that ENDS the phase: record the review ON THE TASK " +
	"in BOTH forms the shape above prescribes, in that order - FIRST the legacy outcome block with " +
	"record_progress(taskId, runId, outcome: ...), THEN the sealed durable record with " +
	"record_review(taskId, runId, {...}). The durable record is the driver's seam evidence; the block is " +
	"the one-release compatibility copy. This is a CHECKPOINT, not a hand-off: " +
	"keep holding the task - do NOT release it, do NOT move it to in_review, and do NOT COMMIT, PUSH or open a " +
	"PR. The driver checkpoints on the branch the implement phase recorded, and the phase after you resumes " +
	"this same run from it. Ending this session after recording the review is this phase " +
	"FINISHING, not the task being abandoned. The ONLY exception is a declared handoff."

// panelMemberTerminalStep ends a PANEL MEMBER's phase (CLA-639 M5). It is
// reviewOnlyTerminalStep with the legacy outcome write REMOVED, not moved: the
// member's only task write is the durable record, and the member after it must
// not be able to read this member's findings through the task's outcome. A
// session told to write both would have its outcome write refused by the plane
// (the round is sealed the moment the record lands, CLA-636 B3) and could
// easily read that refusal as the phase failing.
const panelMemberTerminalStep = " FINALLY, and this is the step that ENDS the phase: record the review ON THE " +
	"TASK in the ONE form the shape above prescribes - the sealed durable record with " +
	"record_review(taskId, runId, {...}) - and write NOTHING ELSE on the task: do NOT call record_progress with an outcome, do not write " +
	"an outcome of your own, and do not touch the task's status or body. That is a CHECKPOINT, not a hand-off: " +
	"keep holding the task - do NOT release it, do NOT move it to in_review, and do NOT COMMIT, PUSH or open a " +
	"PR. The driver checkpoints on the branch the implement phase recorded, and the phase after you (the next " +
	"panel member, or the fix phase) resumes this same run from it. Ending this session after recording the " +
	"review is this phase FINISHING, not the task being abandoned. The ONLY exception is a declared handoff."

// reviewRecordOutcomeShape is the LEGACY half of the review record: the
// `## Review` block appended to the task's outcome. It is kept for one release
// (CLA-637), so older planes and readers still find a review where they expect
// one, while the durable per-reviewer record below is what the driver's seam
// gate reads.
//
// It is a SHAPE, not a grade: the fix phase reads it back by finding ID, and
// the driver still parses it (reviewFindingsDigest in internal/loop) when a
// durable record is unavailable, so the severity bracket and the
// `(<kind>): <evidence>` prefix after it are load-bearing.
//
// It APPENDS below the outcome as it stands. The plane's statusless outcome is
// a full replacement, and the outcome may already carry phase 1's delivery
// record (or, on a request_changes re-trip or a takeover, an earlier trip's
// round); writing a fresh block over it would lose that record. The fix phase
// keeps everything above the block for the same reason.
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

// reviewRecordTipVerified tells the reviewer which commit the driver verified
// and requires that exact tip in both halves of the record. Handing the SHA
// over is not a shortcut around the check: the session must fetch and confirm
// HEAD is that commit before reviewing, and the seam matches the recorded tip
// against the origin tip the driver read.
const reviewRecordTipVerified = " The driver verified the origin remote's tip for branch " + PhaseBranchPlaceholder +
	" as `" + ReviewedTipPlaceholder + "`. Fetch and confirm the worktree is at that commit (`git rev-parse HEAD`) " +
	"before you review, and record that SHA as `reviewedTip` in the durable record and on the block's " +
	"`- reviewed tip:` line."

// reviewRecordTipUnverified is the counterpart for a checkpoint the driver could
// NOT verify against the origin remote (CLA-576): there is no driver-known tip,
// so the reviewer establishes it itself, and the seam requires the record to
// carry a non-empty tip rather than matching one it never had.
const reviewRecordTipUnverified = " The driver could NOT verify the origin remote's tip, so there is no " +
	"driver-verified tip to record: establish it yourself. Read the branch from the origin remote " +
	"(`git ls-remote origin " + PhaseBranchPlaceholder + "`, or fetch it), check out the commit it names, review " +
	"that revision, and record that SHA as `reviewedTip` in the durable record and on the block's " +
	"`- reviewed tip:` line. If the branch is absent or the remote cannot be read, record the failed hand-off " +
	"and end holding the task - do not invent a tip and do not review a phantom diff."

// reviewRecordTipNone is the branchless counterpart: there is no branch and no
// commit to review, and — per the operator's decision on question 03a12578,
// option 1 — no durable record either. The plane accepts review writes only
// from the task's LIVE holder: a settled task (phase 1's own session settled
// it) refuses the append outright, and on a still-held no-code task it would
// seal a round this variant's driver never closes, blocking the run's own
// outcome writes. The legacy block IS this variant's record, and the driver's
// seam gates on the block.
const reviewRecordTipNone = " There is NO branch and NO tip to review: this record is about the PLANE'S RECORD of " +
	"what phase 1 did. Write `- reviewed tip: none (no branch - the plane record was reviewed)` on the block's " +
	"`- reviewed tip:` line. Do NOT append a durable review record: a settled task has no live holder to accept " +
	"it, and on a still-held no-code task it would seal a round this variant never closes - the legacy block IS " +
	"this variant's review record, and the driver gates the seam on it."

// reviewRecordDurableShape is the durable half (CLA-637): one sealed
// per-reviewer record per run, written through record_review. It is the
// driver's seam evidence — round, role, run and reviewed tip are what the gate
// matches — and per-finding `raisedByModel` is what the analytics need.
//
// ORDER IS LOAD-BEARING. While a run has a sealed record in an open round the
// plane refuses that run's outcome, body and decision writes (CLA-636 B3), so
// the legacy outcome block must be written FIRST. The driver closes the round
// before spawning the fix phase, which releases the findings to it.
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

// reviewOnlyRecordShape is the verified review-only phase's full record
// contract: the legacy outcome block, then the durable record. The driver
// injected the phases' RESOLVED harness/model and the verified round, role and
// tip, because a session cannot reliably name its own model, cannot see its
// predecessor's, and cannot see whether the branch tip it read is the one the
// driver verified.
const reviewOnlyRecordShape = reviewRecordOutcomeShape + reviewRecordTipVerified +
	reviewRecordDurableShape + reviewMUSTBar

// unverifiedReviewOnlyRecordShape is reviewOnlyRecordShape for a checkpoint the
// driver could not verify on the origin remote (CLA-576): the reviewer
// establishes the tip itself, and the seam demands a non-empty one.
const unverifiedReviewOnlyRecordShape = reviewRecordOutcomeShape + reviewRecordTipUnverified +
	reviewRecordDurableShape + reviewMUSTBar

// noCodeReviewOnlyRecordShape is the branchless review-only phase's record
// contract: the LEGACY outcome block alone, with no durable-record half —
// the plane accepts review writes only from the task's LIVE holder and a
// settled task has none (operator decision on question 03a12578, option 1), so
// the seam gates this variant on the block (changed since the spawn, heading on
// its own line, and a last block the baseline did not carry).
const noCodeReviewOnlyRecordShape = reviewRecordOutcomeShape + reviewRecordTipNone + reviewMUSTBar

// panelMemberRecordIntro and panelMemberRecordBody are the two halves of a
// PANEL MEMBER's record contract (CLA-639 M5): the tip paragraph sits between
// them, so the verified and unverified variants share everything else.
//
// A panel member writes the DURABLE record and NOTHING ELSE on the task. The
// legacy outcome block reviewOnlyRecordShape prescribes is not merely deferred
// here, it is forbidden: member 1's block would be readable by member 2 through
// its own get_task — the exact cross-member leak the seal exists to prevent —
// and the plane refuses a run's outcome writes while its record sits sealed in
// the open round the panel writes into (CLA-636 B3), so the write would be
// refused anyway. The panel's findings reach the task's outcome through the FIX
// phase, from the merged list the driver releases to it (mergedFindingsDigest).
//
// The field list below is deliberately a second copy of the durable record's
// rather than a shared constant: the two differ wherever the legacy block is
// named ("one object per finding in the block", "write `- none` under
// Findings"), and one string cannot say both. Both copies are pinned by
// TestReviewOnlyBriefsPrescribeTheDurableRecord, so they cannot drift silently.
//
// No branchless variant exists: a branchless task writes no durable record (a
// settled task has no live holder to accept one), so a panel cannot run that
// way at all — the driver skips every member after the first, whose brief is
// the shipped no-code one.
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

// panelMemberRecordTipVerified is the panel member's counterpart of
// reviewRecordTipVerified: the same driver-verified tip, recorded in the
// durable record alone.
const panelMemberRecordTipVerified = " The driver verified the origin remote's tip for branch " +
	PhaseBranchPlaceholder + " as `" + ReviewedTipPlaceholder + "`. Fetch and confirm the worktree is at that " +
	"commit (`git rev-parse HEAD`) before you review, and record that SHA as `reviewedTip` in your durable " +
	"record."

// panelMemberRecordTipUnverified is the panel member's counterpart of
// reviewRecordTipUnverified (CLA-576): the member establishes the tip itself.
const panelMemberRecordTipUnverified = " The driver could NOT verify the origin remote's tip, so there is no " +
	"driver-verified tip to record: establish it yourself. Read the branch from the origin remote " +
	"(`git ls-remote origin " + PhaseBranchPlaceholder + "`, or fetch it), check out the commit it names, review " +
	"that revision, and record that SHA as `reviewedTip` in your durable record. If the branch is absent or the " +
	"remote cannot be read, record that failed hand-off IN YOUR DURABLE RECORD and end holding the task — do not " +
	"write an outcome or touch the task's status, do not invent a tip, and do not review a phantom diff."

// panelMemberRecordShape is the verified panel member's record contract: the
// panel context, the verified tip, and the durable record alone.
var panelMemberRecordShape = panelMemberRecordIntro + panelMemberRecordTipVerified +
	panelMemberRecordBody + panelMemberMUSTBar

// panelMemberUnverifiedRecordShape is panelMemberRecordShape for a checkpoint
// the driver could not verify on the origin remote (CLA-576).
var panelMemberUnverifiedRecordShape = panelMemberRecordIntro + panelMemberRecordTipUnverified +
	panelMemberRecordBody + panelMemberMUSTBar

// reviewMUSTBarGrades is the accepted bar for a MUST (task CLA-635; the
// review-panel design's section 14 A2 as amended by B5/B6, decisions 3a57db6a
// and 60ed0748 superseding it): the evidence a MUST must cite, the seventh kind
// the operator added after the second review, and the things that are never a
// MUST however confident the reviewer is.
//
// Only PRESENCE is checked structurally - here as the reviewer's own
// obligation, and by the driver when it parses the block back
// (loop.reviewFindingsDigest). The VALIDITY of the evidence is judged by a
// verifier in the next round, never by the fixer: a fixer that disagrees with a
// MUST refutes it with evidence, it does not downgrade it. Where a repo hard
// rule concerns style, prose or documentation wording, the not-a-MUST list wins
// over it (B6); missing tests stay not-a-MUST except the untested-guard case.
//
// The GRADING rules are one constant because they are the same for every
// reviewer; the attribution sentence that closes the bar is not, so it is
// separate (CLA-639) — see reviewMUSTBar and panelMemberMUSTBar.
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

// reviewMUSTBarAttribution closes the bar for the SINGLE reviewer, whose record
// has two halves: the legacy outcome block and the durable record. Both carry
// the attribution, and the block's own line is what a human reading the task
// sees.
const reviewMUSTBarAttribution = "Every finding in the block is " +
	"attributed by the block's own `- reviewed by:` line, and the durable record carries `raisedByModel` on every " +
	"finding so the review is attributable per finding, not only per block."

// reviewMUSTBar is the bar as the single reviewer's record shape carries it:
// the grading rules, then the attribution sentence. Split into two constants
// (CLA-639) so a PANEL MEMBER — whose record is the durable one alone — gets
// the same grading rules with an attribution sentence that names no block.
const reviewMUSTBar = reviewMUSTBarGrades + reviewMUSTBarAttribution

// panelMemberMUSTBarAttribution is reviewMUSTBarAttribution for a panel member:
// there is no block, so the record's own `raisedByModel` is the whole
// attribution.
const panelMemberMUSTBarAttribution = "Every finding is attributed in your durable " +
	"record by its `raisedByModel` field, so the review is attributable per finding."

// panelMemberMUSTBar is the panel member's bar: the same grading rules, the
// record-only attribution.
const panelMemberMUSTBar = reviewMUSTBarGrades + panelMemberMUSTBarAttribution

// fixOutcomeRecord is the fix phase's outcome requirement, shared by both fix
// variants. It carries the review forward: the final outcome is the only
// durable task-record the round has (the ledgers excepted), so dropping the
// review block here would lose the very record the split exists to keep. And it
// must keep everything ABOVE the block: that is phase 1's delivery record (or
// an earlier round's), and the statusless outcome replaces the field wholesale,
// so a final outcome built only from the review block and ### Fixes would erase
// it.
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

// fixFindingUpdates is the durable half of the fix phase's record requirement:
// the per-finding status updates, which must go out while this run is still the
// task's HOLDER because the plane refuses them once it is not. It is stated
// without reference to WHO hands the task over — the last fix of a sequence
// does, and a fix with a VERIFICATION round after it does not — so both
// terminals can embed it without one contradicting the other (CLA-686 D1). The
// ordering rule that DOES name the hand-over lives in fixTerminalRecord, where
// the phase making that call is the one reading it.
const fixFindingUpdates = " While you still hold the task, update every finding's " +
	"status on the DURABLE review record with record_review(taskId, runId, {findingUpdates: [...]}) — one " +
	"entry per finding, by the plane-assigned `id` the record gave you: {findingId, status: \"fixed\", " +
	"fixCommit: <the commit SHA>} for a fixed finding; {findingId, status: \"refuted\", refutedReason: <the " +
	"evidence that refutes it>} for a MUST you did not fix; {findingId, status: \"filed\", filedTaskRef: <the " +
	"follow-up task's ref>} for a code-needing finding dispatched instead. The batch is all-or-nothing: an " +
	"unknown id or a still-sealed round refuses it, so it must go out while this run is the holder. An empty " +
	"array is refused; if the review found nothing, skip the call."

// fixTerminalRecord is the BRANCH fix phase's record requirement when this fix
// OWNS the sequence's terminal step, stated BEFORE the shared reviewTerminalStep
// so the emphatic final block still ends the brief (the CLA-384 property): the
// durable finding updates go out before the call that ends the run and clears
// the holder, then the hand-over carries the outcome requirement
// (fixOutcomeRecord) forward.
//
// The ordering sentence is here rather than in fixFindingUpdates because it is
// advice about THIS phase's own hand-over call: the plane refuses the batch
// after that call, even when the same call carries the outcome.
const fixTerminalRecord = fixFindingUpdates + " It must go out BEFORE the hand-over call - before the call that " +
	"moves the task to in_review, even when that same call carries your outcome. THEN hand the task over: " +
	fixOutcomeRecord

// fixVerifyRecord is the BRANCH fix phase's record requirement when a
// VERIFICATION round FOLLOWS it (CLA-686 D1). fixVerifyBrief and its handoff
// continuation end with fixOnlyTerminalStep — "do NOT move the task to
// in_review", because the verifier owns that move — so they must not carry
// fixTerminalRecord's "THEN hand the task over" as well: that is one brief
// telling the fixer both to hand the task over and not to, and a fixer that
// obeys the first half ships fixes no verification round has seen. Everything
// the RECORD owes is unchanged; only the hand-over sentence is dropped, because
// the hand-over is not this phase's.
const fixVerifyRecord = fixFindingUpdates + " When you end the phase, " + fixOutcomeRecord

// noCodeFixTerminalRecord is the BRANCHLESS fix phase's counterpart (question
// 03a12578, option 1): the branchless review writes no durable record — a
// settled task has no live holder to accept one — so there is no findingUpdates
// call to make and no plane-assigned id to send. The record requirement is the
// outcome alone, exactly as fixTerminalRecord's second half states it.
const noCodeFixTerminalRecord = " When you hand the task over, " + fixOutcomeRecord

// noCodeFixVerifyRecord is fixVerifyRecord for a BRANCHLESS fix (CLA-686 D1):
// the outcome requirement alone, with no findingUpdates half — the branchless
// round writes no durable record, so there is no plane-assigned id to send, the
// same exemption noCodeFixTerminalRecord carries — and no hand-over, which the
// verification round owns. Its terminal step is noCodeFixOnlyTerminalStep.
const noCodeFixVerifyRecord = " When you end the phase, " + fixOutcomeRecord

// fixGradingRule is the fix briefs' shared obligation for acting on the graded
// review record (CLA-635): every MUST fixed or refuted with evidence, never
// downgraded by the fixer, and SHOULD/NIT handled by the filing rule. B5's
// structural check runs driver-side, so a MUST that reaches this brief has
// already been judged to carry evidence; the fixer may not second-guess the
// grade - it refutes the finding with evidence of its own, or fixes it. A1 is
// restated because the interim record may hold a single reviewer: disagreement
// is not a second opinion, and a lone reviewer's MUST still gates.
const fixGradingRule = " Every MUST is fixed, or REFUTED with evidence " +
	"recorded in your outcome - a refutation must actually refute the finding, and a MUST you neither fix nor " +
	"refute is this phase failing. You may not downgrade a MUST: the driver has already applied its structural " +
	"check, listing a recorded MUST with no evidence kind or no evidence payload as a SHOULD, and a MUST raised " +
	"by a single reviewer is never dropped because nobody else raised it. Fix the SHOULDs that are cheap; record " +
	"NITs and the SHOULDs you do not fix in your outcome as not done, with the reason, and file a follow-up task " +
	"only for a SHOULD worth doing."

// noCodeReviewTerminalStep is the no-code review brief's LAST instruction
// (CLA-497), the counterpart of reviewTerminalStep for a phase-1 exit that was
// evidenced by the plane's record rather than a branch. There is nothing to
// commit and no PR to open - the task's correct delivery is no code - so the
// step is the handover alone, with the same emphatic, final-call shape
// CLA-384 pinned on the branch-shaped one. It also covers the form-(b) case
// where phase 1 ALREADY settled the task (in_review / done / parked /
// blocked): the handover happened before this phase ran, and the session's job
// is to verify the record reads honestly and stop - re-opening or releasing a
// settled task would restart the release loop this task exists to kill.
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
