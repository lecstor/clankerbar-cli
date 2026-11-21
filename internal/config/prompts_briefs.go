package config

// noCodeReviewBrief is the review brief for a phase-1 exit evidenced by the PLANE'S RECORD instead
// of a branch: the task left `ready` (in_review / done / parked / blocked - its own session settled
// it), or it carries a declared no-code delivery. There is no branch because the task's correct
// delivery is no code, so the branch-shaped builtin review brief would misdirect its successor - it
// asserts a branch "the driver verified to exist" that does not exist, and its "empty branch field
// is a FAILED hand-off to report" instruction contradicts the checkpoint the driver just recorded.
const noCodeReviewBrief = "You are PHASE " + PhaseNumberPlaceholder + " of " + PhaseTotalPlaceholder + " on task " + PhaseTaskPlaceholder + ", whose implement phase's " +
	"exit was evidenced by the PLANE'S RECORD rather than a branch: the task left `ready` (in_review / done / " +
	"parked / blocked - its own session settled it) or carries a declared no-code delivery. There is NO " +
	"branch, because this task's correct delivery is no code - nothing to check out, no worktree to work " +
	"in, no PR to open. The empty branch field is NOT a failed hand-off: the driver recorded this " +
	"checkpoint on exactly that record. You are RESUMING that run, not starting a new one: do not call " +
	"next_task, and do not claim anything. Call heartbeat(\"" + PhaseRunPlaceholder + "\") to resume the " +
	"run." + driverRenewsLease + resumeHeartbeatStays +
	" If the plane REFUSES the heartbeat because the task already left in_progress, that refusal is " +
	"expected: the settlement IS the delivery, so proceed to the review below. Then get_task with " +
	"includeDecisions: true to re-read the bar and the standing decisions. Run the adversarial review gate " +
	"over the PLANE'S RECORD of what phase 1 actually did - the task-body edits, the outcome, the decisions " +
	"recorded on this task or its siblings: is the settlement honest, does the record carry what the task's " +
	"doneWhen required, does the no-code declaration match the work? Fix what the gate finds by amending " +
	"the record (record_progress, record_decision) - never by inventing code, a branch or a PR." + rerunGuidance +
	noCodeReviewTerminalStep + handoffGuidance

// unverifiedReviewBrief is the review brief for a checkpoint the driver could NOT verify against
// the origin remote: the implement phase recorded branch {{branch}} on the plane, but the driver's
// branch check did not complete — most often the remote could not be read (network, a GitHub
// outage, a credential problem), though a check that never reached the remote (no local branch to
// compare, an ambiguous repo, no git on PATH) lands in the same delivery.Unknown — so whether the
// branch was ever pushed — and whether phase 1 did anything at all — is unknown. The
// branch-shaped builtin brief would tell the successor "a branch the driver verified to exist on
// the origin remote with its tip reachable, so phase 1 really did implement, commit and push",
// which is exactly the claim this checkpoint cannot support.
const unverifiedReviewBrief = "You are PHASE " + PhaseNumberPlaceholder + " of " + PhaseTotalPlaceholder + " on task " + PhaseTaskPlaceholder + ", whose implement phase " +
	"recorded branch " + PhaseBranchPlaceholder + " on the plane — but the driver could NOT verify that branch on " +
	"the origin remote: its check did not complete — most often because the remote could not be read (network, a " +
	"GitHub outage, a credential problem), but a check that never reached the remote is possible too — so whether " +
	"phase 1 pushed, and whether the branch exists at all, is UNKNOWN. This checkpoint is UNVERIFIED, not a " +
	"verified hand-off: do NOT assume the branch exists, and do NOT assume phase 1 implemented, committed and " +
	"pushed. You are RESUMING that run, not starting a new one: do not call next_task, and do not claim anything. " +
	"Call heartbeat(\"" + PhaseRunPlaceholder + "\") to resume the run." + driverRenewsLease + resumeHeartbeatStays +
	" Then get_task with includeDecisions: true " +
	"to re-read the bar and the standing decisions. FIRST verify the hand-off yourself, before any review: read " +
	"branch " + PhaseBranchPlaceholder + " from the origin remote (git ls-remote origin " + PhaseBranchPlaceholder +
	", or fetch it) and confirm it carries phase 1's work. If the branch is not on the origin remote, phase 1 did " +
	"NOT deliver: report the failed hand-off in your outcome and hand the task over for a human decision — do not " +
	"review a phantom diff. If the remote still cannot be read, the hand-off could not be verified: say exactly " +
	"that in your outcome and hand the task over as the terminal step below requires — do not invent work. If the " +
	"branch is there, work in the worktree for branch " + PhaseBranchPlaceholder +
	" recorded on the task, and never commit to the repo's PR base branch (" + PhasePRBasePlaceholder + ") - a session whose cwd is already " +
	"the main checkout sitting on that branch is not a decision to commit where you are, it is this failure mode. Read " +
	"the diff on that branch (git diff " + PhaseBasePlaceholder + "..." + PhaseBranchPlaceholder + "). Then run the adversarial review gate, fix what it finds, and re-verify SCOPED to " +
	"those fixes: brief the follow-up reviewer with the findings you fixed, by name, and point it at the fix " +
	"commits (or, if not yet committed, the fix diff) and the regression surface they touch - not at the whole " +
	"diff, whose full pass already happened. A full second pass is the exception you state a reason for (a fix " +
	"that had to reach outside its own area), never the default." + rerunGuidance + reviewTerminalStep +
	handoffGuidance

// reviewGateScope defines what "run the adversarial review gate" means for a review-only phase, and
// what it does NOT mean. A reviewer's value is the reading, so a command is scoped to a PROBE of a
// defect it already suspects - evidence for a finding, never a second gate.
const reviewGateScope = " THE GATES ARE NOT YOURS TO RE-RUN. The implement phase self-verified at this tip and the fix " +
	"and verify phases run the gates again over what they change, so do NOT run the repo's full test suite, " +
	"typecheck, build or long checks (a PDF validator, a whole-browser e2e sweep - anything that takes minutes) " +
	"as a matter of course: each reviewer re-running them is another identical run of the same tree, bought with " +
	"wall time, machine load and a context full of test output. Spend the session READING instead - the diff " +
	"against the base, and the code it touches that the diff does not show; that is where a defect the " +
	"implementer's own gates missed is found. Run a command only as a TARGETED probe of a specific defect you " +
	"suspect: one test file, one package, one short repro script, never the repo's gate list. Name the suspected " +
	"defect the probe is for, and when it confirms one, the probe's output IS that finding's evidence - " +
	"`(gate): <the command and its failing output>` for a command that fails, `(failure): <the input and the " +
	"wrong output>` for behaviour you reproduced by hand. A probe must leave the worktree as it found it: if it " +
	"writes a report, coverage file or snapshot, restore or delete the artifact (`git checkout -- <path>` for a tracked " +
	"one) before you record. The panel shares this one worktree, so an artifact left behind is the next member's " +
	"- and the fix phase's - dirty tree. "

// reviewOnlyBrief is the review phase's brief when a LATER phase will apply its findings (the
// shipped [implement, review, fix] sequence). "Review only" is enforced as a scope, not a
// capability: the phase is told what it may not do, and the fix phase is told to treat any fix it
// did not make as its own job by ID.
var reviewOnlyBriefOpening = "You are PHASE " + PhaseNumberPlaceholder + " of " + PhaseTotalPlaceholder + " on task " +
	PhaseTaskPlaceholder + ", the REVIEW-ONLY gate. Its scope is the review and nothing else: you find issues " +
	"and specify fixes; the phase after you applies them. The implement phase (harness " +
	PhasePrevHarnessPlaceholder + ", model " + PhasePrevModelPlaceholder + ") recorded branch " +
	PhaseBranchPlaceholder + " on the plane — "

// reviewHandoffVerified is the claim only a checkpoint the driver read on the origin remote can
// make.
const reviewHandoffVerified = "a branch the driver verified to exist on the origin remote with its tip " +
	"reachable, so phase 1 really did implement, commit and push."

// reviewHandoffUnverified is the panel member's counterpart: the check did not complete, so the
// branch's existence and phase 1's push are UNKNOWN and the member must establish the tip itself.
const reviewHandoffUnverified = "but the driver could NOT verify that branch on the origin remote: its check did " +
	"not complete — most often because the driver could not read the remote (network, a GitHub outage, a " +
	"credential problem), but a check that never reached the remote is possible too — so whether phase 1 " +
	"pushed, and whether the branch exists at all, is UNKNOWN. This checkpoint is UNVERIFIED, not a verified " +
	"hand-off: the branch may not exist on the origin remote, and do NOT assume phase 1 implemented, committed " +
	"and pushed."

var reviewOnlyBriefResume = " You are RESUMING that run, not starting a new one: do not call next_task, and do " +
	"not claim anything. Call heartbeat(\"" + PhaseRunPlaceholder + "\") to resume the run." + driverRenewsLease +
	resumeHeartbeatStays + " Then get_task with " +
	"includeDecisions: true to re-read the bar and the standing decisions. An empty branch field on the task is " +
	"a FAILED hand-off to report in the record below, not work to silently adopt and implement."

// reviewHandoffUnverifiedCheck states the member's obligation; the record shape carries the
// mechanics and the failure arm for a branch that is absent, empty or unreadable.
const reviewHandoffUnverifiedCheck = " FIRST establish the tip yourself, before any review: read branch " +
	PhaseBranchPlaceholder + " from the origin remote - the record shape below tells you exactly how, and where " +
	"to record the tip - and confirm it carries phase 1's work. If the branch is absent, or exists but carries " +
	"none of phase 1's work, phase 1 did NOT deliver: record that failed hand-off in your durable record and end " +
	"holding the task — do not review a phantom diff."

var reviewOnlyBriefWorktreeAndGate = " Work in the worktree for branch " + PhaseBranchPlaceholder +
	" recorded on the task, and never commit to the repo's PR base branch (" + PhasePRBasePlaceholder + ") - a " +
	"session whose cwd is already the main checkout sitting on that branch is not a decision to commit where " +
	"you are, it is this failure mode. Read the diff against the PR base (git diff " + PhaseBasePlaceholder +
	"..." + PhaseBranchPlaceholder + ") and run the adversarial review gate." + reviewGateScope +
	"REVIEW ONLY: do not edit, create or delete a source file, do not commit, do not push, and do not open a PR " +
	"- not even to fix a typo. Do NOT fix what you find: the phase after you is responsible for applying it, " +
	"and a fix applied here is a fix it cannot attribute."

var reviewOnlyBriefCore = reviewOnlyBriefOpening + reviewHandoffVerified + reviewOnlyBriefResume +
	reviewOnlyBriefWorktreeAndGate

// reviewOnlyBrief is the single reviewer's brief: the core above, the two-half record shape, and
// the two-half ending.
var reviewOnlyBrief = reviewOnlyBriefCore +
	reviewOnlyRecordShape + rerunGuidance + reviewOnlyTerminalStep + handoffGuidance

// panelMemberReviewOnlyBrief is a PANEL MEMBER's brief: the same core, with the durable-record-only
// shape and an ending that forbids the outcome write.
var panelMemberReviewOnlyBrief = reviewOnlyBriefCore +
	panelMemberRecordShape + rerunGuidance + panelMemberTerminalStep + handoffGuidance

// panelMemberUnverifiedReviewOnlyBrief is panelMemberReviewOnlyBrief for a checkpoint the driver
// could not verify against the origin remote.
var panelMemberUnverifiedReviewOnlyBrief = reviewOnlyBriefOpening + reviewHandoffUnverified +
	reviewOnlyBriefResume + reviewHandoffUnverifiedCheck + reviewOnlyBriefWorktreeAndGate +
	panelMemberUnverifiedRecordShape + rerunGuidance + panelMemberTerminalStep + handoffGuidance

// unverifiedReviewOnlyBrief is reviewOnlyBrief for a checkpoint the driver could NOT verify against
// the origin remote. The verified claim above is exactly what such a checkpoint cannot support, so
// the session verifies the hand-off itself before reviewing anything.
var unverifiedReviewOnlyBrief = "You are PHASE " + PhaseNumberPlaceholder + " of " + PhaseTotalPlaceholder +
	" on task " + PhaseTaskPlaceholder + ", the REVIEW-ONLY gate. Its scope is the review and nothing else: " +
	"you find issues and specify fixes; the phase after you applies them. The implement phase (harness " +
	PhasePrevHarnessPlaceholder + ", model " + PhasePrevModelPlaceholder + ") recorded branch " +
	PhaseBranchPlaceholder + " on the plane — but the driver could NOT verify that branch on the origin " +
	"remote: its check did not complete — most often because the remote could not be read (network, a GitHub " +
	"outage, a credential problem), but a check that never reached the remote is possible too — so whether " +
	"phase 1 pushed, and whether the branch exists at all, is UNKNOWN. This checkpoint is UNVERIFIED, not a " +
	"verified hand-off: do NOT assume the branch exists, and do NOT assume phase 1 implemented, committed and " +
	"pushed. You are RESUMING that run, not starting a new one: do not call next_task, and do not claim " +
	"anything. Call heartbeat(\"" + PhaseRunPlaceholder + "\") to resume the run." + driverRenewsLease +
	resumeHeartbeatStays + " Then get_task with " +
	"includeDecisions: true to re-read the bar and the standing decisions. FIRST verify the hand-off yourself, " +
	"before any review: read branch " + PhaseBranchPlaceholder + " from the origin remote (git ls-remote " +
	"origin " + PhaseBranchPlaceholder + ", or fetch it) and confirm it carries phase 1's work. If the branch " +
	"is not on the origin remote, phase 1 did NOT deliver: record that failed hand-off in the review record " +
	"below and end holding the task — do not review a phantom diff. If the remote still cannot be read, say " +
	"exactly that in the record and end holding the task; do not invent work. If the branch is there, work in " +
	"the worktree for branch " + PhaseBranchPlaceholder + " recorded on the task, and never commit to the " +
	"repo's PR base branch (" + PhasePRBasePlaceholder + ") - a session whose cwd is already the main checkout " +
	"sitting on that branch is not a decision to commit where you are, it is this failure mode. Read the diff " +
	"against the PR base (git diff " + PhaseBasePlaceholder + "..." + PhaseBranchPlaceholder + ") and run " +
	"the adversarial review gate." + reviewGateScope +
	"REVIEW ONLY: do not edit, create or delete a source " +
	"file, do not commit, do not push, and do not open a PR - not even to fix a typo. Do NOT fix what you " +
	"find: the phase after you is responsible for applying it, and a fix you apply here is a fix it cannot " +
	"attribute." + unverifiedReviewOnlyRecordShape + rerunGuidance + reviewOnlyTerminalStep +
	handoffGuidance

// noCodeReviewOnlyTerminalStep ends a branchless review-only phase. The record here is the LEGACY
// block alone (operator decision on question 03a12578, option 1): the durable table takes writes
// only from the task's live holder — a settled task has none, and a still-held no-code task's
// write would seal a round this variant never closes — so attempting the append would be a
// refused call at best and a wedged run at worst, and the seam does not need it.
const noCodeReviewOnlyTerminalStep = " FINALLY, and this is the step that ENDS the phase: record the review ON THE " +
	"TASK with record_progress(taskId, runId, outcome: ...), in the shape above - the LEGACY `" + ReviewRecordHeading +
	"` block ONLY. Do NOT attempt a durable review record write here: a settled task has no live holder to accept it, " +
	"and on a still-held no-code task it would seal a round this variant never closes - the block IS this " +
	"variant's review record and the driver's seam gates on it. This " +
	"is a CHECKPOINT, not a " +
	"hand-off: do NOT change the task's status, delivery or body here, do NOT re-open it, do NOT invent code, a " +
	"branch or a PR, and do NOT move it to in_review. If phase 1 already settled it, leave it settled; the " +
	"phase after you decides whether a finding needs more than a record change. Ending this session after " +
	"recording the review is this phase FINISHING."

// noCodeReviewOnlyBrief is reviewOnlyBrief for a checkpoint evidenced by the PLANE'S RECORD rather
// than a branch: there is nothing to check out and nothing for the fix phase to edit, so the review
// is of the record itself.
var noCodeReviewOnlyBrief = "You are PHASE " + PhaseNumberPlaceholder + " of " + PhaseTotalPlaceholder + " on task " +
	PhaseTaskPlaceholder + ", the REVIEW-ONLY gate. Its scope is the review and nothing else: you find issues " +
	"and specify fixes; the phase after you applies them. The implement phase's exit was evidenced by the " +
	"PLANE'S RECORD rather than a branch: the task left `ready` (in_review / done / parked / blocked - its own " +
	"session settled it) or carries a declared no-code delivery. There is NO branch, because this task's " +
	"correct delivery is no code - nothing to check out, no worktree to work in, no PR to open. The empty " +
	"branch field is NOT a failed hand-off: the driver recorded this checkpoint on exactly that record. You " +
	"are RESUMING that run, not starting a new one: do not call next_task, and do not claim anything. Call " +
	"heartbeat(\"" + PhaseRunPlaceholder + "\") to resume the run." + driverRenewsLease + resumeHeartbeatStays +
	" If the plane REFUSES the heartbeat because " +
	"the task already left in_progress, that refusal is expected: the settlement IS the delivery, so proceed " +
	"to the review below. Then get_task with includeDecisions: true to re-read the bar and the standing " +
	"decisions. Review the PLANE'S RECORD of what phase 1 actually did - the task-body edits, the outcome, the " +
	"decisions recorded on this task or its siblings: is the settlement honest, does the record carry what the " +
	"task's doneWhen required, does the no-code declaration match the work? REVIEW ONLY: do not amend what " +
	"phase 1 wrote - the phase after you applies what you find - but DO append your legacy review block below " +
	"it so its delivery record survives (the block, not a durable record: see the shape below); and do not " +
	"invent code, a branch or a PR." +
	noCodeReviewOnlyRecordShape + rerunGuidance + noCodeReviewOnlyTerminalStep + handoffGuidance

// fixBrief is the phase that APPLIES a review's findings. The cost is one more session startup and
// a hand-off the fixer must read; the review record on the task is what makes that hand-off cheap
// and auditable.
const fixCore = "Read BOTH the durable record and the block: `reviews.records` carries every released record in the newest " +
	"round, each with its findings[] and the plane-assigned id (`<round>.<reviewer role>.<n>`) you update by; " +
	"the outcome's `" + ReviewRecordHeading + "` block is the legacy copy. On a later fix round (after a " +
	"verification round) the newest record carries only the verifier's NEW findings: the work list is every " +
	"MUST the plane still shows OPEN on ANY released record (get_task's `reviews.openMustIds`), which the driver " +
	"prepends when it can read it - an open MUST from an earlier round is yours to fix or refute too. If there is neither a released review " +
	"record nor a review block, that is a FAILED hand-off: park the task with settle_task(taskId, runId, disposition: \"parked\", " +
	"outcome: ...) naming the missing record - do NOT review the work yourself, do NOT invent findings, and do " +
	"NOT open a PR over unreviewed work. Read the record's reviewedTip and compare it with " +
	"`git rev-parse HEAD` in the worktree: if they differ, the branch moved after the review, so do NOT apply " +
	"stale findings - park the task with settle_task(taskId, runId, disposition: \"parked\", outcome: ...) naming the mismatch. Work in the worktree for branch " +
	PhaseBranchPlaceholder + " recorded on the task, and never commit to the repo's PR base branch (" +
	PhasePRBasePlaceholder + ") - a session whose cwd is already the main checkout sitting on that branch is " +
	"not a decision to commit where you are, it is this failure mode. Apply the review's findings, each by its " +
	"PLANE id from the durable record - the block's R-numbers are labels, not the keys you update by." +
	fixGradingRule +
	" A finding you neither fix nor answer is this phase failing. The review " +
	"ran checks, so the worktree may carry generated or cache files it did not commit: stage only the fixes " +
	"you made, never those. Re-verify " +
	"SCOPED to those fixes: run the gates the repo's own instructions name, and any check a finding says was " +
	"red, not the whole suite again for no reason."

// fixBrief is the phase that APPLIES a review's findings when it is the LAST phase of the sequence.
// The cost is one more session startup and a hand-off the fixer must read; the review record on the
// task is what makes that hand-off cheap and auditable.
var fixBrief = "You are PHASE " + PhaseNumberPlaceholder + " of " + PhaseTotalPlaceholder + " on task " +
	PhaseTaskPlaceholder + ", the FIX phase and the LAST phase of this task. The review phase reviewed branch " +
	PhaseBranchPlaceholder + " and wrote its findings as a sealed DURABLE review record; the driver has now " +
	"closed that round, so get_task releases the findings to you. It also left the legacy `" +
	ReviewRecordHeading + "` block in the task's outcome for one release — the LAST such block, because an " +
	"earlier `" + ReviewRecordHeading + "` block is an earlier round. It changed nothing. You are " +
	"RESUMING that run, not starting a new one: do not call next_task, and do not claim anything. Call " +
	"heartbeat(\"" + PhaseRunPlaceholder + "\") to resume the run." + driverRenewsLease + resumeHeartbeatStays +
	" Then get_task with includeDecisions: true. " +
	fixCore +
	" If the review found nothing " +
	"(verdict: pass), there is nothing to fix: still open the PR if none exists and hand over." +
	rerunGuidance + fixTerminalRecord + reviewTerminalStep + handoffGuidance

// fixVerifyBrief is the fix phase when a VERIFICATION round follows it. The rest of the contract is
// identical, because it reads the same record and applies the same findings.
var fixVerifyBrief = "You are PHASE " + PhaseNumberPlaceholder + " of " + PhaseTotalPlaceholder + " on task " +
	PhaseTaskPlaceholder + ", the FIX phase. The review phase reviewed branch " +
	PhaseBranchPlaceholder + " and wrote its findings as a sealed DURABLE review record; the driver has now " +
	"closed that round, so get_task releases the findings to you. It also left the legacy `" +
	ReviewRecordHeading + "` block in the task's outcome for one release — the LAST such block, because an " +
	"earlier `" + ReviewRecordHeading + "` block is an earlier round. It changed nothing. A VERIFICATION round " +
	"runs after you: it re-reviews your fix delta and adjudicates the prior open MUSTs, and IT opens the PR " +
	"and hands the task to in_review once no MUST remains open. You are RESUMING that run, not starting a " +
	"new one: do not call next_task, and do not claim anything. Call heartbeat(\"" + PhaseRunPlaceholder +
	"\") to resume the run." + driverRenewsLease + resumeHeartbeatStays +
	" Then get_task with includeDecisions: true. " +
	fixCore +
	" If the review found nothing (verdict: pass), there is nothing to fix: commit nothing, leave the branch " +
	"as it is, and end holding the task — the verification round still runs and carries the terminal step." +
	rerunGuidance + fixVerifyRecord + fixOnlyTerminalStep + handoffGuidance

// fixOnlyTerminalStep ends a fix phase that is NOT the sequence's last one: the verification round
// owns the PR and in_review hand-over.
const fixOnlyTerminalStep = " FINALLY, and this is the step that ENDS the phase: COMMIT and PUSH the fixes " +
	"you made, append an outcome with record_progress(taskId, runId, outcome: ...) that carries the fixes and a " +
	"**Tests** section saying what you actually verified — the plane requires that section on the task before " +
	"the verifier can hand it to in_review, and the verifier cannot write the outcome itself once its own " +
	"review record is sealed — then STOP still holding the task. Do NOT open a PR and do NOT move the task to " +
	"in_review: the VERIFICATION round after you carries that terminal step once it finds no open MUST. A PR " +
	"opened here would ship fixes the verifier never saw. Ending this session holding the task is the " +
	"checkpoint the verification round resumes from."

// noCodeFixOnlyTerminalStep is fixOnlyTerminalStep for a branchless fix: it still records an
// outcome the in_review guard can read, but there is nothing to commit or push.
const noCodeFixOnlyTerminalStep = " FINALLY, and this is the step that ENDS the phase: append an outcome with " +
	"record_progress(taskId, runId, outcome: ...) that carries what you amended and a **Tests** section (\"**Tests** " +
	"— none, no-code delivery.\" is honest and valid) — the plane reads that section on the in_review move the " +
	"verifier makes, and the verifier cannot write the outcome itself once its own review record is sealed. " +
	"Do NOT open a PR and do NOT move the task to in_review: the VERIFICATION round after you carries that " +
	"terminal step once it finds no open MUST. End holding the task."

// noCodeFixBrief is fixBrief for a branchless checkpoint: the review judged the plane's record, and
// the fix is an amendment to that record, never invented code.
var noCodeFixBrief = "You are PHASE " + PhaseNumberPlaceholder + " of " + PhaseTotalPlaceholder + " on task " +
	PhaseTaskPlaceholder + ", the FIX phase and the LAST phase of this task. The review phase reviewed the " +
	"PLANE'S RECORD of what the implement phase did - there is NO branch, because this task's correct " +
	"delivery is no code. You " +
	"are RESUMING that run, not starting a new one: do not call next_task, and do not claim anything. Call " +
	"heartbeat(\"" + PhaseRunPlaceholder + "\") to resume the run." + driverRenewsLease + resumeHeartbeatStays +
	" If the plane REFUSES the heartbeat because " +
	"the task already left in_progress, that refusal is expected: the settlement IS the delivery, so proceed. " +
	"Then get_task with includeDecisions: true and read the LAST `" + ReviewRecordHeading + "` block in the " +
	"outcome — an earlier one is an earlier round, so read past it. That block IS the round's record: the " +
	"branchless review writes no durable record (question 03a12578, option 1), so read and apply from the " +
	"block alone and do not treat an empty record set as a failed hand-off. If the outcome carries no `" +
	ReviewRecordHeading + "` block, that is a FAILED hand-off: say so " +
	"in your outcome, and park the task if it is still in_progress - if phase 1 already settled it, leave it " +
	"settled and end. Do not invent findings. Apply the findings by AMENDING THE RECORD " +
	"(record_progress, record_decision) - never by inventing code, a branch or a PR." + fixGradingRule +
	" A finding that needs CODE cannot be fixed here: file it as its own task " +
	"(create_task, with a real doneWhen) and say so in your outcome - for a MUST, that filed task is this " +
	"phase's fix of it (record the new task's ref in your outcome as the fix; a dispatched MUST is fixed this " +
	"way, not refuted, and not a failure) - do not re-open this task, do not rewrite " +
	"its settlement, and do not fake the code. If the review found nothing, say so." + rerunGuidance +
	noCodeFixTerminalRecord + noCodeReviewTerminalStep + handoffGuidance

// noCodeFixVerifyBrief is the branchless fix phase when a verification round follows: the amendment
// is to the plane's record, and the terminal step is the silent hold — the verification round
// carries the hand-over.
var noCodeFixVerifyBrief = "You are PHASE " + PhaseNumberPlaceholder + " of " + PhaseTotalPlaceholder + " on task " +
	PhaseTaskPlaceholder + ", the FIX phase. The review phase reviewed the PLANE'S RECORD of what the " +
	"implement phase did - there is NO branch, because this task's correct delivery is no code. A " +
	"VERIFICATION round runs after you and adjudicates the open MUSTs, so it, not you, decides the " +
	"hand-over. You are RESUMING that run, not starting a new one: do not call next_task, and do not claim " +
	"anything. Call heartbeat(\"" + PhaseRunPlaceholder + "\") to resume the run." + driverRenewsLease +
	resumeHeartbeatStays + " If the plane REFUSES the " +
	"heartbeat because the task already left in_progress, that refusal is expected: the settlement IS the " +
	"delivery, so proceed. Then get_task with includeDecisions: true and read the LAST `" + ReviewRecordHeading +
	"` block in the outcome — an earlier one is an earlier round, so read past it. That block IS the round's " +
	"record: the branchless review writes no durable record (question 03a12578, option 1), so read and apply " +
	"from the block alone and do not treat an empty record set as a failed hand-off. Apply the findings by " +
	"AMENDING THE RECORD (record_progress, record_decision) - never by inventing code, a branch or a PR." + fixGradingRule +
	" A finding that needs CODE cannot be fixed here: file it as its own task (create_task, with a real " +
	"doneWhen) and say so in your outcome - for a MUST, that filed task is this phase's fix of it. If the " +
	"review found nothing, change nothing." + rerunGuidance + noCodeFixVerifyRecord + noCodeFixOnlyTerminalStep +
	handoffGuidance

// verifyStoppingRule is the stopping rule's core: what counts as a MUST that is still open.
const verifyStoppingRule = " THE STOPPING RULE IS A BAR, NOT A FEELING. The gate is NO OPEN MUST. A MUST is open " +
	"until it is fixed, filed as its own task, or refuted with evidence — a prior MUST that is neither stays " +
	"open, and a refutation you reject also stays open. SHOULD and NIT findings are recorded, never gating."

// verifyDriverBounds is the id-based machinery the CODE-shaped verification round runs under: the
// driver's round cap and non-convergence breaker, both read from the plane's durable records.
const verifyDriverBounds = " The driver counts rounds: this is a hard cap of three non-void rounds per review " +
	"cycle (a human verdict starts a new cycle; a takeover or reclaim does not). At the cap, any open MUST is " +
	"the driver's park, never another round. Non-convergence is also the driver's: a MUST left unresolved " +
	"after two fix attempts, or an id a previous verifier marked resolved that you reopen, parks the task for " +
	"a human."

// verifyTerminalStep ends a verification round: the hand-over only when no MUST remains open,
// otherwise a silent hold for the driver to spawn another fix.
const verifyTerminalStep = " FINALLY, and this is the step that ENDS the phase. Two facts decide it: whether " +
	"any MUST is still open after your adjudication, and the round number the append returned.\n\n" +
	"If ANY MUST remains open: release this round with ONE call — record_review(taskId, runId, " +
	"{closeRound: <the round the append returned>}) — then do NOT open a PR, do NOT move the task to " +
	"in_review, and do NOT change its status. End holding the task. The driver spawns another fix against " +
	"the released findings, or parks the task at the round cap.\n\n" +
	"If NO MUST remains open: FIRST confirm the branch is pushed (push any fix commit that is still local) and " +
	"open a PR targeting the repo's PR base branch (" + PhasePRBasePlaceholder + ") if none exists for the branch." +
	worktreeReclaimStep + " THEN carry the hand-over in ONE call: submit_for_review(taskId, runId, closeRound: <the round the append " +
	"returned>, risk: { touches: [...] }). The close rides the hand-over's atomic write, which is the only order the plane accepts: once " +
	"the task leaves in_progress this run can no longer close the round. Do NOT send an outcome in that call: " +
	"your own sealed record makes the plane refuse a sealed holder's outcome write (review_sealed), and the fix " +
	"phase has already written the **Tests** section the in_review guard reads. The same call's risk declaration " +
	"uses touches: [] when it touches none of the project's risk classes, an omitted risk being UNDECLARED and " +
	"buying no per-batch auto-approval; the vocabulary and the judging rule are in the served `finishing` " +
	"reference and the project's risk_vocabulary (get_project_policy). Never decide to run one more " +
	"round yourself."

// noCodeVerifyTerminalStep ends a BRANCHLESS verification round: the no-code counterpart of
// verifyTerminalStep.
const noCodeVerifyTerminalStep = " FINALLY, and this is the step that ENDS the phase. Two facts decide it: whether " +
	"any MUST is still open after your adjudication, and whether the task still sits in_progress. The " +
	"id-based machinery is the driver's and needs plane finding ids this shape does not have, so this " +
	"terminal step is this shape's whole disposition.\n\n" +
	"Append YOUR OWN legacy `" + ReviewRecordHeading + "` block to the task's outcome with " +
	"record_progress(taskId, runId, outcome: ...) — append, never replace what sits above it: the block IS this " +
	"shape's round record, and the driver's checkpoint reads exactly it. There is no durable record to " +
	"append, no round to close and no PR to open for a no-code delivery.\n\n" +
	"If ANY MUST remains open: do NOT hand the task over — park it with settle_task(taskId, runId, " +
	"disposition: \"parked\", outcome: ...), carrying the open MUST(s) and why the amendment does not resolve them; the " +
	"same disposition the round cap gives the id-based shape. If the task already left in_progress, do NOT " +
	"re-open it: leave your block as the record and end.\n\n" +
	"If NO MUST remains open: hand the task over. If it still sits in_progress with its no-code delivery " +
	"declared, move it with submit_for_review(taskId, runId, risk: { touches: [...] }) — do NOT send an outcome in " +
	"that call; your block write already landed and the fix phase wrote the **Tests** section the in_review " +
	"guard reads. The same call's risk declaration uses touches: [] when it touches none of the project's risk " +
	"classes, an omitted risk being UNDECLARED and buying no per-batch auto-approval; the vocabulary and the " +
	"judging rule are in the served `finishing` reference and the project's risk_vocabulary " +
	"(get_project_policy). If it already left " +
	"in_progress, the hand-over already happened: verify the record reads " +
	"honestly and end. Never decide to run one more round yourself."

// verifyBrief is the fix-verification round: review the fix delta plus its regression surface,
// adjudicate every prior open MUST by its plane id, and gate on no open MUST. The driver prepends
// the prior open MUST ids (and their findings) to this brief: the list is plane state, not config,
// and the driver reads it from the released records before spawning.
var verifyBrief = "You are PHASE " + PhaseNumberPlaceholder + " of " + PhaseTotalPlaceholder + " on task " +
	PhaseTaskPlaceholder + ", the VERIFICATION round. A review round found issues and a fix round applied " +
	"them; your job is to verify the FIXES, not to review the whole diff again. The driver has closed the " +
	"previous round, so get_task releases the records. You are RESUMING that run, not starting a new one: do " +
	"not call next_task, and do not claim anything. Call heartbeat(\"" + PhaseRunPlaceholder + "\") to resume " +
	"the run." + driverRenewsLease + resumeHeartbeatStays +
	" Then get_task with includeDecisions: true. The driver has prepended the PRIOR OPEN MUST ids with " +
	"their findings, and every MUST of this review cycle already MARKED settled; if that block is missing, read " +
	"the released records yourself and list every finding with severity must, both those with status open and " +
	"those of this cycle marked fixed, refuted or filed. Work in the worktree for branch " + PhaseBranchPlaceholder +
	" recorded on the task, and never commit to the repo's PR base branch (" + PhasePRBasePlaceholder + ") - " +
	"a session whose cwd is already the main checkout sitting on that branch is not a decision to commit " +
	"where you are, it is this failure mode. " +
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
	"\"{{reviewerRole}}\", reviewedTip `git rev-parse HEAD`, scope kind \"fix-delta\" with base and range " +
	"(`<reviewedTip>..HEAD`), a verdict of pass or findings, and every new finding graded must/should/nit with " +
	"the evidence a MUST requires. The durable record is the driver's seam evidence; the block is the " +
	"one-release compatibility copy." +
	verifyStoppingRule + verifyDriverBounds + verifyTerminalStep + rerunGuidance + handoffGuidance

// noCodeVerifyBrief is verifyBrief for a branchless checkpoint: there is no code delta to
// re-review, so the verification is of the plane record and the fix's record amendments.
var noCodeVerifyBrief = "You are PHASE " + PhaseNumberPlaceholder + " of " + PhaseTotalPlaceholder + " on task " +
	PhaseTaskPlaceholder + ", the VERIFICATION round for a task whose correct delivery is NO CODE. There is " +
	"NO branch and no diff; the fix round amended the PLANE'S RECORD. The driver has prepended the LAST `" +
	ReviewRecordHeading + "` block of the task's outcome — that block, not a durable record, is this shape's " +
	"round record (question 03a12578, option 1). You are RESUMING that run, not starting a new one: do not " +
	"call next_task, and do not claim anything. Call heartbeat(\"" + PhaseRunPlaceholder + "\") to resume the " +
	"run." + driverRenewsLease + resumeHeartbeatStays + " If the plane REFUSES the heartbeat because the task " +
	"already left in_progress, that refusal is " +
	"expected: the settlement IS the delivery, so proceed. Then get_task with includeDecisions: true. Review " +
	"the amendment the fix made: does it resolve each prior open MUST, is it evidence rather than assertion, " +
	"does the task's record still match its doneWhen? ADJUDICATE EVERY PRIOR OPEN MUST in the BLOCK's own " +
	"terms — resolved, resolved by dispatch (the follow-up task's ref), refuted with evidence, or left open " +
	"with your reason — and check on the ground what the amendment claims where you can. There are no plane " +
	"finding ids to update on this shape, and do not treat the empty durable record set as a failed " +
	"hand-off. You may raise new MUSTs only about the record changes and their regression surface. Do NOT " +
	"invent code, a branch or a PR, and do NOT open a PR." +
	verifyStoppingRule + noCodeVerifyTerminalStep + rerunGuidance + handoffGuidance
