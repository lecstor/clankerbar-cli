package config

import (
	"strings"
	"testing"
	"unicode"

	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// A config with no phases must behave exactly as it always has. This is the
// back-compat guarantee that lets phases be opt-in rather than a migration.
func TestEffectivePhases_UnphasedIsOnePhaseCarryingThePrompt(t *testing.T) {
	c := defaults()
	c.Prompt = "Work the next backlog item."

	got := c.EffectivePhases()
	if len(got) != 1 {
		t.Fatalf("an unphased config resolved to %d phases, want exactly 1: %+v", len(got), got)
	}
	if got[0].Prompt != c.Prompt {
		t.Errorf("the single phase carries %q, want the configured prompt %q", got[0].Prompt, c.Prompt)
	}
	// CLA-343, deliberately: the unphased phase used to carry MaxTurns 0 — the
	// zero value the turn-cap chain resolved to when nothing set one — and one
	// session ran 1093 turns / 285.9M tokens with no cap anywhere in the chain.
	// "0 = uncapped" is gone; the chain falls through to the built-in default.
	if got[0].MaxTurns != DefaultMaxTurns {
		t.Errorf("an unphased run resolved to a turn cap of %d, want the built-in default %d — a bare config must still bound its sessions", got[0].MaxTurns, DefaultMaxTurns)
	}
}

func TestEffectivePhases_ResolvesBuiltinsByNameAndLetsAPromptOverride(t *testing.T) {
	c := defaults()
	c.Prompt = ""
	c.Phases = []Phase{
		{Name: "implement"},
		{Name: "review", Prompt: "my own brief"},
	}

	got := c.EffectivePhases()
	if len(got) != 2 {
		t.Fatalf("got %d phases, want 2", len(got))
	}
	if got[0].Prompt != builtinPhasePrompts["implement"] {
		t.Errorf("a named phase with no prompt did not take its built-in brief; got %q", got[0].Prompt)
	}
	if got[1].Prompt != "my own brief" {
		t.Errorf("an explicit prompt was overwritten by the built-in; got %q", got[1].Prompt)
	}
	// Resolution must not mutate the config it read.
	if c.Phases[0].Prompt != "" {
		t.Error("EffectivePhases wrote a resolved prompt back into the config")
	}
}

// The phase-1 brief is an interface, exactly as the default prompt is: it is read
// by an agent that has read the served protocol, and the whole mechanism is that
// the session STOPS at the checkpoint. A paraphrase that drops the stop turns the
// split back into one long session while every other test stays green.
func TestBuiltinImplementPhaseTellsTheSessionToStopAtTheCheckpoint(t *testing.T) {
	brief := builtinPhasePrompts["implement"]
	if brief == "" {
		t.Fatal("no built-in brief for the implement phase")
	}
	lower := strings.ToLower(brief)
	for _, want := range []string{"push", "stop"} {
		if !strings.Contains(lower, want) {
			t.Errorf("the implement brief never says %q; it must push its work and then stop:\n%s", want, brief)
		}
	}
	// The two things it must forbid, because doing either makes the second phase
	// pointless: reviewing its own work, and closing the task itself.
	if !strings.Contains(lower, "in_review") {
		t.Errorf("the implement brief does not tell the session to leave in_review alone:\n%s", brief)
	}
}

// CLA-378: a resumed branch is a liability until it has been synced and
// re-proven. EZY-199 was a recovered stale claim whose implement phase verified
// the found tip faithfully (build, unit suite, 60 e2e executions, push) without
// ever syncing staging in - staging had superseded the branch's whole fix a day
// earlier, and the review phase paid for the archaeology and salvage a cheap
// up-front check would have made unnecessary. The brief must carry all three
// parts of the rule: merge the branch the task's PRs will merge into (never
// rebase) - CLA-608 makes that a resolved per-repo fact, {{prBase}}, not the
// hard-wired integration branch - re-validate the task against the merged tip
// before working, and on supersession record the decision and park the task
// instead of re-verifying stale work.
//
// The content assertions are FULL directional phrases, not fragments: an
// earlier draft pinned "doing or verifying" alone, and a mutation flipping the
// rule's BEFORE to AFTER - the exact behaviour this rule exists to remove -
// shipped green because the fragment survived the inversion. Every phrase here
// fails when its direction or disposition is edited away.
func TestBuiltinImplementBriefPinsTheResumedBranchRule(t *testing.T) {
	brief, ok := builtinPhasePrompts[ImplementPhaseName]
	if !ok {
		t.Fatalf("no built-in brief for phase %q", ImplementPhaseName)
	}

	lower := strings.ToLower(brief)
	for _, want := range []string{
		"existing branch or worktree",
		"haswip",
		"your first step is to merge the repo's pr base branch ({{prbase}}) into it",
		"a merge, never a rebase",
		"phase-boundary check",
		"then re-validate the task against the merged tip before doing or verifying any of the found work",
		"can silently revert a newer fix",
		"the task's own bar",
		"not the absence of conflict markers",
		"if the merged tip supersedes the task",
		"record_decision",
		"park the task with an outcome citing it",
		"do not spend the run re-verifying and pushing stale work",
		"unless you parked above or served a close-out above, the rest of the flow is unchanged",
	} {
		if !strings.Contains(lower, want) {
			t.Errorf("the resumed-branch rule never says %q:\n%s", want, brief)
		}
	}

	// The join back into the normal flow must be NON-exclusive: an earlier
	// draft said "Otherwise, claim the task...", which read as exempting a
	// resumed-and-still-valid session from commit/push/record-branch/STOP -
	// the exact phase-1 contract phase 2 depends on.
	if strings.Contains(lower, "otherwise, claim") {
		t.Errorf("the resumed-branch rule excludes the found-work case from the normal flow with 'Otherwise, claim'; the join must carry every path into the checkpoint instructions:\n%s", brief)
	}

	// Order is part of the rule: sync, then re-prove, then the supersession
	// disposition ending in the stale-work prohibition. Presence alone would
	// pass a brief listing them in an order a session could read as
	// verify-first, the exact behaviour being removed.
	mergeIdx := strings.Index(lower, "your first step is to merge")
	revalidateIdx := strings.Index(lower, "before doing or verifying any of the found work")
	supersededIdx := strings.Index(lower, "supersedes the task")
	parkIdx := strings.Index(lower, "park the task with an outcome citing it")
	staleIdx := strings.Index(lower, "re-verifying and pushing stale work")
	joinIdx := strings.Index(lower, "unless you parked above or served a close-out above, the rest of the flow is unchanged")
	if mergeIdx == -1 || revalidateIdx == -1 || supersededIdx == -1 || parkIdx == -1 || staleIdx == -1 || joinIdx == -1 {
		t.Fatalf("merge (%d), re-validate (%d), supersession (%d), parking (%d), stale-work end (%d) or flow join (%d) missing from the brief:\n%s",
			mergeIdx, revalidateIdx, supersededIdx, parkIdx, staleIdx, joinIdx, brief)
	}
	if !(mergeIdx < revalidateIdx && revalidateIdx < supersededIdx && supersededIdx < parkIdx && parkIdx < staleIdx && staleIdx < joinIdx) {
		t.Errorf("the rule is not sync (%d) -> re-validate (%d) -> supersede/park (%d, %d) -> stale-work end (%d) -> flow join (%d) in that order:\n%s",
			mergeIdx, revalidateIdx, supersededIdx, parkIdx, staleIdx, joinIdx, brief)
	}

	// The rule is entry guidance, so it rides BEFORE the terminal checkpoint
	// instruction: this file's own field history (reviewTerminalStep) credits
	// the brief's emphatic, named LAST instruction for never failing, and the
	// first draft broke that shape by wedging the rule between STOP and the
	// handoff guidance.
	stopIdx := strings.Index(lower, "then stop and end the session")
	ruleIdx := strings.Index(lower, strings.ToLower(implementResumedBranchRule))
	if stopIdx == -1 || ruleIdx == -1 {
		t.Fatalf("the stop instruction (%d) or the resumed-branch rule (%d) is missing:\n%s", stopIdx, ruleIdx, brief)
	}
	if ruleIdx > stopIdx {
		t.Errorf("the resumed-branch rule (%d) sits after the terminal checkpoint instruction (%d); entry guidance must precede the work/stop sequence so the brief keeps its emphatic last instruction:\n%s", ruleIdx, stopIdx, brief)
	}
	if !strings.HasSuffix(brief, handoffGuidance) {
		t.Errorf("the implement brief does not end with the shared handoff guidance:\n%s", brief)
	}
	// The join must sit in front of the work sequence it introduces: a join
	// after the STOP instruction leaves the flow with no path statement
	// before it (mutation-proven escapable before this pin existed).
	if joinIdx > stopIdx {
		t.Errorf("the flow join (%d) sits after the STOP instruction (%d); it must introduce the work sequence, not follow it:\n%s", joinIdx, stopIdx, brief)
	}

	// Harness-neutral: one wording serves every harness's implement phase. The
	// registry, not a hardcoded list, is the source of truth for the name set.
	names := harness.Names()
	if len(names) == 0 {
		t.Fatal("no harnesses registered; the neutrality check would be vacuous")
	}
	for _, name := range names {
		if strings.Contains(lower, strings.ToLower(name)) {
			t.Errorf("the implement brief names harness %q; its wording must hold for every harness:\n%s", name, brief)
		}
	}
}

// CLA-540: a phased implement session is told to claim, work, commit, push and
// stop at the checkpoint — and an approved-awaiting-merge offer from next_task
// needs none of that: no commit, no branch, no worktree, its required act a
// status move the brief's letter forbids. Without an exception the session asks
// the operator about an offer that is exactly its job (observed twice on
// 2026-08-28: CLA-533 and CLA-526, both green and mergeable, both raised as
// blocking questions). The brief must admit the case as its own fork: the offer
// IS the session's job, the offer's own instructions are the brief for it, and
// ending after it is the task going to plan.
//
// The content assertions are FULL directional phrases, in the shape
// TestBuiltinImplementBriefPinsTheResumedBranchRule established: a fragment
// like "the offer" alone would survive a mutation that inverts the admission
// into "decline the offer and ask".
//
// Two routing guards pin the fork's exclusivity. "The resumed-work rule below
// is for found work, not this offer" preempts that rule's "existing branch"
// trigger, which a close-out claim's recorded branch would otherwise satisfy -
// applying its merge-and-revalidate sequence to an approved PR branch would
// rewrite the diff the approval and the green CI stand on. And "served a
// close-out above" exempts the close-out case from the join into the
// claim/worktree/commit flow, which otherwise reads as instructing the session
// to keep going after the close-out.
func TestBuiltinImplementBriefAdmitsTheCloseOutOffer(t *testing.T) {
	brief, ok := builtinPhasePrompts[ImplementPhaseName]
	if !ok {
		t.Fatalf("no built-in brief for phase %q", ImplementPhaseName)
	}
	if implementCloseOutRule == "" {
		t.Fatal("the implement brief has no close-out rule")
	}

	lower := strings.ToLower(brief)
	for _, want := range []string{
		"if next_task instead offers an approved-awaiting-merge task",
		"that offer is this session's job",
		"follow the offer's own instructions",
		"once the close-out is done stop and end the session",
		"the resumed-work rule below is for found work, not this offer",
		"unless you parked above or served a close-out above, the rest of the flow is unchanged",
		"going to plan",
	} {
		if !strings.Contains(lower, want) {
			t.Errorf("the implement brief never says %q; a close-out offer must be admitted as this session's job:\n%s", want, brief)
		}
	}

	// The fork comes FIRST: the offer replaces fresh work, so the close-out
	// disposition must precede the resumed-work rule and the checkpoint
	// instruction, which both concern a task this session claims for itself.
	closeIdx := strings.Index(lower, strings.ToLower(implementCloseOutRule))
	ruleIdx := strings.Index(lower, strings.ToLower(implementResumedBranchRule))
	stopIdx := strings.Index(lower, "then stop and end the session")
	if closeIdx == -1 || ruleIdx == -1 || stopIdx == -1 {
		t.Fatalf("close-out rule (%d), resumed-branch rule (%d) or terminal stop (%d) missing from the brief:\n%s",
			closeIdx, ruleIdx, stopIdx, brief)
	}
	if !(closeIdx < ruleIdx && ruleIdx < stopIdx) {
		t.Errorf("the close-out fork (%d) must precede the resumed-work rule (%d) and the terminal stop (%d):\n%s",
			closeIdx, ruleIdx, stopIdx, brief)
	}

	// What the clause must NOT carry (the plane's offer owns all of it, and a
	// second copy would drift — CLA-341): a placeholder, a named branch or merge
	// target, the red-CI repair rule, the decline door, the queue (ONE offer, not
	// several), or a worktree-reclaim step. "main" is banned as a whole word so
	// "remaining" is not caught, the way TestRerunGuidancePointsAtWaitingRather
	// ThanRestatingIt bans the waiting reference's mechanics.
	for _, banned := range []string{
		"{{", "branch", "worktree", "staging", "integration",
		"red", "decline", "cannot attempt", "queue",
	} {
		if strings.Contains(strings.ToLower(implementCloseOutRule), banned) {
			t.Errorf("the close-out rule says %q — it belongs on the offer, not in the brief:\n%s", banned, implementCloseOutRule)
		}
	}
	words := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(implementCloseOutRule), func(r rune) bool { return !unicode.IsLetter(r) }) {
		words[w] = true
	}
	if words["main"] {
		t.Errorf("the close-out rule names %q as a merge target — the offer's own instructions own the target, and a brief that names one is wrong for every other topology:\n%s", "main", implementCloseOutRule)
	}
}

// CLA-544: a wall-clock kill earns a checkpoint only with WIP, and WIP is a
// branch recorded on the TASK — the session's own update_task(branch), or the
// salvage recording one. The salvage refuses a clean worktree on purpose
// (salvage.go: "The one thing that must NOT happen here is recording a branch"),
// so a commit nobody pushed and recorded is as invisible to the plane as no
// commit at all. The sessions that killed CLA-529 and CLA-531 three times each
// made not one Edit, Write or Create call: they read and planned for their
// whole lives, the kill landed on a clean worktree, nothing was salvageable,
// and the no-progress ladder parked the task. The brief must therefore demand a
// first commit EARLY — before the design is fully settled — then push it and
// RECORD the branch, and commit and push as it goes, so a kill lands on a
// checkpoint instead of on nothing.
//
// The trigger is an EVENT, never a duration: a session cannot read its own
// wall clock (the same constraint that shaped handoffGuidance), so "commit
// within fifteen minutes" is not implementable — a skeleton, a failing test, a
// stub with the signatures are events it can recognise. And the clause is a
// FLOOR, not a ceiling: checkpoint pushes are WIP, not deliveries — the
// terminal step's bar (self-verify, then COMMIT, PUSH, record the branch) is
// unchanged, and a checkpoint push must not be passed off as the self-verified
// terminal delivery.
func TestBuiltinImplementBriefDemandsAnEarlyCommit(t *testing.T) {
	brief, ok := builtinPhasePrompts[ImplementPhaseName]
	if !ok {
		t.Fatalf("no built-in brief for phase %q", ImplementPhaseName)
	}
	if implementCommitEarlyRule == "" {
		t.Fatal("the implement brief has no commit-early rule")
	}

	lower := strings.ToLower(brief)
	for _, want := range []string{
		"first commit",
		"before the design is fully settled",
		"skeleton, a failing test, a stub with the signatures",
		"commit and push as you go rather than once at the end",
		"push it and record the branch",
		"update_task(taskid, runid, branch)",
		"record it as soon as the first commit is pushed",
		"not only at the end",
		"uncommitted worktree is lost",
		"a local commit nobody pushed and recorded is lost the same way",
		"the next phase resumes from the branch recorded on the task",
		"earlier floor",
		"does not lower the ceiling",
		"terminal step's bar is unchanged",
		"not licence to commit broken or half-considered code as a habit",
		"pass a checkpoint push off as the self-verified terminal delivery",
	} {
		if !strings.Contains(lower, want) {
			t.Errorf("the implement brief never says %q — the first commit must be demanded early, anchored to an event the session can recognise, and pushed and recorded so the plane can checkpoint it:\n%s", want, brief)
		}
	}

	// The anchor is an EVENT, not a duration: a session cannot measure its own
	// wall clock, so a time budget is a rule it cannot follow.
	for _, banned := range []string{"within 15", "within fifteen", "minute", "hour"} {
		if strings.Contains(strings.ToLower(implementCommitEarlyRule), banned) {
			t.Errorf("the commit-early rule anchors the first commit to a duration (%q) — a session cannot read its own wall clock:\n%s", banned, implementCommitEarlyRule)
		}
	}

	// The clause is entry guidance for the normal flow, so it rides BEFORE the
	// terminal checkpoint instruction — the same position pin the resumed-branch
	// rule carries. A commit-early clause wedged after STOP would read as
	// post-session advice nobody follows.
	ruleIdx := strings.Index(lower, strings.ToLower(implementCommitEarlyRule))
	stopIdx := strings.Index(lower, "then stop and end the session")
	if ruleIdx == -1 || stopIdx == -1 {
		t.Fatalf("commit-early rule (%d) or terminal stop (%d) missing from the brief:\n%s", ruleIdx, stopIdx, brief)
	}
	if ruleIdx > stopIdx {
		t.Errorf("the commit-early rule (%d) sits after the terminal checkpoint instruction (%d); it must precede the work/stop sequence so the brief keeps its emphatic last instruction:\n%s", ruleIdx, stopIdx, brief)
	}

	// Phase 0 refuses a first-phase prompt carrying the resume placeholders —
	// the implement brief IS phase 0, so the clause must never import them.
	for _, ph := range []string{PhaseTaskPlaceholder, PhaseRunPlaceholder} {
		if strings.Contains(implementCommitEarlyRule, ph) {
			t.Errorf("the commit-early rule carries %s — the implement brief is phase 0 and refuses resume placeholders:\n%s", ph, implementCommitEarlyRule)
		}
	}
}

// The resume brief is useless without the ids, and they are substituted by the
// driver — so the placeholders have to actually be in the shipped text.
func TestBuiltinReviewPhaseCarriesTheResumePlaceholders(t *testing.T) {
	brief := builtinPhasePrompts["review"]
	for _, ph := range []string{PhaseTaskPlaceholder, PhaseRunPlaceholder} {
		if !strings.Contains(brief, ph) {
			t.Errorf("the review brief is missing the %s placeholder, so a resuming session is never told which run it is on:\n%s", ph, brief)
		}
	}
	if !strings.Contains(strings.ToLower(brief), "heartbeat") {
		t.Errorf("the review brief does not tell the session to resume the run with heartbeat:\n%s", brief)
	}
}

// CLA-336: the post-fix pass is the expensive part paid twice. Phase 2's brief
// once ended at "re-verify", and the session read that as dispatching a second
// full review of the updated diff - eight minutes and most of a $12.64 phase,
// spent to check a handful of fixes the first pass had already localised. The
// brief must scope that pass to the fix commits and what they touch, name the
// findings being verified, and keep the full re-review as a stated exception.
func TestBuiltinReviewPhaseScopesThePostFixPassToTheFixes(t *testing.T) {
	brief := builtinPhasePrompts["review"]
	lower := strings.ToLower(brief)

	// The scope: named findings, the fix commits, and their regression surface.
	// Without the findings named, the re-reviewer reconstructs the list by
	// reading everything - the behaviour being removed.
	for _, want := range []string{"finding", "fix commit", "regression surface"} {
		if !strings.Contains(lower, want) {
			t.Errorf("the review brief never says %q; a post-fix pass it dispatches has no scope to hold to:\n%s", want, brief)
		}
	}

	// The escape hatch stays, and stays explicit: a full second pass is a
	// judgement the session states, not the default it falls into silently.
	for _, want := range []string{"exception", "default"} {
		if !strings.Contains(lower, want) {
			t.Errorf("the review brief never says %q; the full-pass escape hatch must be an explicit exception, not an unstated default:\n%s", want, brief)
		}
	}

	// And the wording being removed must not come back: a follow-up pointed at
	// the whole (updated) diff re-buys the full pass, however it is phrased.
	for _, banned := range []string{
		"review the updated diff", "over the updated diff", "review the whole diff",
		"re-review the diff", "re-review the whole", "read the diff again", "a fresh review",
	} {
		if strings.Contains(lower, banned) {
			t.Errorf("the review brief asks for %q - a whole-diff re-review, which is exactly the double-pay CLA-336 removed:\n%s", banned, brief)
		}
	}
}

func TestValidate_PhasesAndPromptAreMutuallyExclusive(t *testing.T) {
	c := defaults()
	c.Prompt = "something the operator wrote"
	c.Phases = []Phase{{Name: "implement"}, {Name: "review"}}

	err := c.Validate()
	if err == nil {
		t.Fatal("Validate accepted both `prompt` and `phases`; one of the two would then be silently ignored")
	}
	if !strings.Contains(err.Error(), "prompt") || !strings.Contains(err.Error(), "phases") {
		t.Errorf("the rejection names neither field, so it is not actionable: %v", err)
	}
}

// The residue of detecting "set" by comparing against the default: a prompt left
// untouched is not a conflict, because defaults() filled it before the operator's
// file was ever layered on top.
func TestValidate_AnUntouchedDefaultPromptDoesNotConflictWithPhases(t *testing.T) {
	c := defaults()
	c.Phases = []Phase{{Name: "implement"}, {Name: "review"}}

	if err := c.Validate(); err != nil {
		t.Fatalf("Validate rejected phases alongside the untouched default prompt: %v", err)
	}
}

func TestValidate_RejectsAPhaseWithNoPromptAndNoBuiltin(t *testing.T) {
	c := defaults()
	c.Prompt = ""
	c.Phases = []Phase{{Name: "implement"}, {Name: "not-a-real-phase"}}

	err := c.Validate()
	if err == nil {
		t.Fatal("Validate accepted a phase that resolves to an empty prompt; its session would be spawned with nothing to do")
	}
	if !strings.Contains(err.Error(), "not-a-real-phase") {
		t.Errorf("the rejection does not name the offending phase: %v", err)
	}
	for _, n := range BuiltinPhaseNames() {
		if !strings.Contains(err.Error(), n) {
			t.Errorf("the rejection does not list the built-in name %q, so an operator cannot see what they meant to type: %v", n, err)
		}
	}
}

// Phases rest entirely on the adapter observing the session's claim. An adapter
// that does not would not FAIL — it would implement and push and then stop after
// phase 1 on every task, reported by a log line that reads like an ordinary early
// finish. Refusing at validation is the difference between a config error and a
// silent half-run every night.
func TestValidate_RefusesPhasesOnAHarnessThatCannotObserveAClaim(t *testing.T) {
	for _, name := range harness.Names() {
		caps, ok := harness.CapabilitiesOf(name)
		if !ok {
			t.Fatalf("registered harness %q has no capabilities", name)
		}
		c := defaults()
		c.Harness = name
		c.Prompt = ""
		c.Phases = []Phase{{Name: "implement"}, {Name: "review"}}

		err := c.Validate()
		if caps.TracksClaims {
			if err != nil {
				t.Errorf("Validate rejected phases on %q, which tracks claims: %v", name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("Validate accepted phases on %q, which does not observe a claim — every task would stop after phase 1", name)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the rejection for %q does not name the harness: %v", name, err)
		}
	}
}

// A single phase needs no hand-off, so it does not need claim tracking either —
// but it does need a brief that can finish a task. The phase carries its own
// prompt here for exactly that reason; see the test below.
func TestValidate_AllowsASinglePhaseOnAnyHarness(t *testing.T) {
	for _, name := range harness.Names() {
		c := defaults()
		c.Harness = name
		c.Prompt = ""
		c.Phases = []Phase{{Name: "solo", Prompt: "Work the next backlog item."}}

		if err := c.Validate(); err != nil {
			t.Errorf("Validate rejected a single self-contained phase on %q: %v — one phase hands off to nobody", name, err)
		}
	}
}

// `phases: [{"name":"implement"}]` validates as a shape and is a trap: the brief
// tells its session to stop at the checkpoint and leave in_review alone, and
// there is no successor to do the rest. Every task would stop half-implemented,
// every night, with nothing in the logs reading as an error.
func TestValidate_RefusesASequenceThatEndsOnTheImplementBrief(t *testing.T) {
	for _, phases := range [][]Phase{
		{{Name: ImplementPhaseName}},
		{{Name: ReviewPhaseName, Prompt: "something"}, {Name: ImplementPhaseName}},
	} {
		c := defaults()
		c.Prompt = ""
		c.Phases = phases

		err := c.Validate()
		if err == nil {
			t.Errorf("Validate accepted a sequence ending on the %q brief (%d phases); nothing would ever hand a task to review", ImplementPhaseName, len(phases))
			continue
		}
		if !strings.Contains(err.Error(), ReviewPhaseName) {
			t.Errorf("the rejection does not point at the fix: %v", err)
		}
	}
}

// The reverse: a resume brief cannot go FIRST, because there is no previous claim
// to fill its placeholders from — it would spend a session telling a model to
// heartbeat a literal "{{runId}}".
func TestValidate_RefusesAResumeBriefInFirstPosition(t *testing.T) {
	c := defaults()
	c.Prompt = ""
	c.Phases = []Phase{{Name: ReviewPhaseName}, {Name: "closer", Prompt: "finish up"}}

	err := c.Validate()
	if err == nil {
		t.Fatal("Validate accepted a resume brief as phase 0, which has no previous claim to resume")
	}
	if !strings.Contains(err.Error(), PhaseRunPlaceholder) && !strings.Contains(err.Error(), PhaseTaskPlaceholder) {
		t.Errorf("the rejection does not name the placeholder that cannot be filled: %v", err)
	}
}

// The name becomes part of an iteration log's filename, and statedir refuses one
// it does not like — costing the phase the single artifact an operator has to
// debug the sequence with.
func TestValidate_RejectsAPhaseNameThatCannotBeAFilename(t *testing.T) {
	c := defaults()
	c.Prompt = ""
	c.Phases = []Phase{{Name: "impl/one", Prompt: "do the thing"}}

	err := c.Validate()
	if err == nil {
		t.Fatal("Validate accepted a phase name containing a path separator; its sessions would run with no iteration log at all")
	}
	if !strings.Contains(err.Error(), "impl/one") {
		t.Errorf("the rejection does not name the offending phase: %v", err)
	}
}

func TestValidate_RejectsANegativeTurnCap(t *testing.T) {
	c := defaults()
	c.Prompt = ""
	c.Phases = []Phase{{Name: "implement", MaxTurns: -1}}

	if err := c.Validate(); err == nil {
		t.Fatal("Validate accepted a negative max_turns")
	}
}

// An unphased config with an empty prompt is still the old error, unchanged: the
// new phases check must not have swallowed it.
func TestValidate_StillRejectsAnEmptyPromptWhenUnphased(t *testing.T) {
	c := defaults()
	c.Prompt = ""

	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "prompt is empty") {
		t.Fatalf("Validate() = %v, want the empty-prompt rejection", err)
	}
}

// CLA-343: the turn-cap chain is phase → top-level → built-in default, and an
// unphased run — the default config — must never resolve to uncapped. This pins
// the two load-bearing readings: a bare config gets the default, and the
// operator's top-level cap overrides it.
func TestEffectivePhases_ResolvesTheTurnCapChain(t *testing.T) {
	t.Run("a bare unphased config resolves to the built-in default", func(t *testing.T) {
		c := defaults()
		c.Prompt = "work"
		got := c.EffectivePhases()[0]
		if got.MaxTurns != DefaultMaxTurns {
			t.Errorf("bare config resolved to MaxTurns %d, want the default %d", got.MaxTurns, DefaultMaxTurns)
		}
	})

	t.Run("the top-level cap overrides the default", func(t *testing.T) {
		c := defaults()
		c.Prompt = "work"
		c.MaxTurns = 50
		got := c.EffectivePhases()[0]
		if got.MaxTurns != 50 {
			t.Errorf("top-level MaxTurns 50 resolved to %d", got.MaxTurns)
		}
	})

	t.Run("a phase cap beats the top-level cap", func(t *testing.T) {
		c := defaults()
		c.Prompt = ""
		c.MaxTurns = 50
		c.Phases = []Phase{{Name: "implement", MaxTurns: 10}}
		got := c.EffectivePhases()[0]
		if got.MaxTurns != 10 {
			t.Errorf("phase MaxTurns 10 resolved to %d, want the phase's own cap to win", got.MaxTurns)
		}
	})

	t.Run("a phase without a cap inherits the top-level cap", func(t *testing.T) {
		c := defaults()
		c.Prompt = ""
		c.MaxTurns = 50
		c.Phases = []Phase{{Name: "implement"}, {Name: "review"}}
		got := c.EffectivePhases()
		for i, ph := range got {
			if ph.MaxTurns != 50 {
				t.Errorf("phase %d resolved to MaxTurns %d, want the top-level 50", i, ph.MaxTurns)
			}
		}
	})

	t.Run("resolution does not mutate the config it read", func(t *testing.T) {
		c := defaults()
		c.Prompt = "work"
		c.MaxTurns = 50
		c.EffectivePhases()
		if c.MaxTurns != 50 {
			t.Error("EffectivePhases wrote the resolved cap back into the config")
		}
	})
}

func TestValidate_RejectsANegativeTopLevelTurnCap(t *testing.T) {
	c := defaults()
	c.Prompt = "work"
	c.MaxTurns = -1

	if err := c.Validate(); err == nil {
		t.Fatal("Validate accepted a negative top-level max_turns")
	}
}

func TestValidate_RejectsANegativeSessionTokenCeiling(t *testing.T) {
	c := defaults()
	c.Prompt = "work"
	c.Budget = Budget{MaxSessionTokens: -1}

	if err := c.Validate(); err == nil {
		t.Fatal("Validate accepted a negative max_session_tokens")
	}
}

// CLA-343: the per-session token ceiling is the runaway detector, and it must
// exist even for a config that sets no budget dials at all — the whole defect
// was that nothing could stop the 285.9M session, so a ceiling that falls away
// by accident is the bug.
func TestBudgetSessionTokenCeiling(t *testing.T) {
	tests := []struct {
		name   string
		b      Budget
		want   int
		reason string
	}{
		{"the operator's own dial wins", Budget{MaxSessionTokens: 40_000_000}, 40_000_000, ""},
		{"2x max_tokens when no dial is set", Budget{MaxTokens: 75_000_000}, 150_000_000, ""},
		{"the documented floor with no budget at all", Budget{}, sessionTokenFloor, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.b.SessionTokenCeiling(); got != tc.want {
				t.Errorf("SessionTokenCeiling() = %d, want %d", got, tc.want)
			}
		})
	}
}

// CLA-352: the built-in briefs are where the pivot-trigger guidance lives — a
// session cannot measure its own context, so the trigger has to be event-shaped
// and it has to be TAUGHT, or the marker the driver watches for is never
// emitted. Pinned per brief so a rewrite cannot drop it from one silently.
func TestBuiltinPhaseBriefsCarryTheHandoffGuidance(t *testing.T) {
	for name, brief := range builtinPhasePrompts {
		if !strings.Contains(brief, HandoffMarker) {
			t.Errorf("the %q brief never names the handoff marker, so a session cannot use the mechanism", name)
		}
		if !strings.Contains(brief, "pivot") {
			t.Errorf("the %q brief lost the pivot trigger — without it the only rule left is a vibe about context size, which a session cannot measure", name)
		}
		if !strings.Contains(brief, "most tasks need zero") {
			t.Errorf("the %q brief must say most tasks need zero handoffs, or the exception becomes the habit", name)
		}
	}
}

// The review phase's terminal step is the one the phase exists to reach, and on
// 2026-08-19 three of four review phases in a single evening did all the work and
// then ended without it — the branch pushed, the task still `in_progress`, the
// next clanker paying to rediscover where the last one got to (CLA-384).
//
// The implement brief's equivalent step succeeded every time the same evening, and
// the difference between them was salience: implement names the call, its
// arguments and a stop condition in its own sentence; review said only "hand the
// task to in_review", a trailing clause at the end of a long paragraph about
// something else. So this pins the SHAPE that worked, not merely the topic.
//
// It pins wording, which is a weak test on its own — a brief can say all of this
// and still be ignored. Its partner is the driver-side tally in internal/loop,
// which counts the failures this brief is trying to prevent.
func TestReviewBriefStatesItsTerminalStep(t *testing.T) {
	brief, ok := builtinPhasePrompts[ReviewPhaseName]
	if !ok {
		t.Fatalf("no built-in brief for phase %q", ReviewPhaseName)
	}

	for _, want := range []string{
		// The call, by name, with the arguments the plane actually requires.
		"update_task(taskId, runId, status: \"in_review\", outcome: ...)",
		// The outcome requirement, which the brief omitted entirely before this —
		// a second way to end one call short, since the plane refuses an outcome
		// with no Tests section and a session can read that refusal as "done".
		"**Tests**",
		"REFUSES",
		// Ending while still holding the task is a failure, said as such.
		"FAILING, not finishing",
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("review brief does not state %q — the terminal step is what CLA-384 was about:\n%s", want, brief)
		}
	}

	// The step must not trail off the end of the re-verification sentence, which
	// is the shape that failed. Requiring it to open its own sentence is the
	// cheapest structural proxy available to a string test.
	if !strings.Contains(brief, "FINALLY, and this is the step that ENDS the phase:") {
		t.Errorf("the terminal step is not set apart as its own instruction:\n%s", brief)
	}

	// Presence is not the property that failed - POSITION is. The old brief
	// contained the instruction too; it was a trailing clause on a sentence about
	// something else. So pin the step to the LAST position before the shared
	// handoff guidance, which is exactly where the implement brief - the control in
	// this experiment, and the one that never failed - puts its own. Without this,
	// every assertion above still passes on a brief that buries the step in the
	// middle or softens it in a later sentence.
	if !strings.HasSuffix(brief, reviewTerminalStep+handoffGuidance) {
		t.Errorf("the terminal step is not the last thing the brief says before the handoff guidance:\n%s", brief)
	}
}

// CLA-353: an overnight forensic drain found `gh pr create` appearing ZERO times
// across four review-phase transcripts, and two tasks committed straight onto
// `staging` because a session's cwd was already sitting there and nothing told it
// that was wrong. This pins the fix's three parts: the PR step itself, its
// position relative to the terminal update_task call (CLA-384 made that call
// emphatic and final, which pulls harder on anything mentioned before it), and
// the worktree/never-commit-to-the-base-branch rule that names the failure mode
// rather than leaving "commit where I am" looking like no decision at all.
//
// Since CLA-608 the target is the {{prBase}} placeholder rather than a literal
// branch name: the shipped text is shared by every repo of every project, and
// the branch is resolved per repo at spawn time. A literal name here (any name,
// "staging" included) would be the CLA-608 bug again - wrong for every repo it
// does not happen to match.
func TestReviewBriefStatesThePRStep(t *testing.T) {
	brief, ok := builtinPhasePrompts[ReviewPhaseName]
	if !ok {
		t.Fatalf("no built-in brief for phase %q", ReviewPhaseName)
	}

	prIdx := strings.Index(brief, "Open a PR")
	if prIdx == -1 {
		t.Fatalf("the review brief never mentions opening a PR:\n%s", brief)
	}
	if !strings.Contains(brief, "PR base branch ("+PhasePRBasePlaceholder+")") {
		t.Errorf("the review brief does not name the resolved PR base as the PR's target:\n%s", brief)
	}
	if !strings.Contains(brief, "if no PR exists") {
		t.Errorf("the review brief does not say the PR step is a no-op when a PR already exists for the branch:\n%s", brief)
	}

	// Position: push -> PR -> update_task, in that order, matching the doneWhen's
	// explicit sequencing. Presence alone would pass on a brief that mentions a
	// PR somewhere far from the terminal step, which is exactly the shape CLA-384
	// warned would happen to anything not folded into the same emphatic block.
	pushIdx := strings.Index(brief, "COMMIT and PUSH")
	updateTaskIdx := strings.Index(brief, "update_task(taskId, runId, status: \"in_review\"")
	if pushIdx == -1 || updateTaskIdx == -1 {
		t.Fatalf("push (%d) or update_task (%d) not found in the brief:\n%s", pushIdx, updateTaskIdx, brief)
	}
	if !(pushIdx < prIdx && prIdx < updateTaskIdx) {
		t.Errorf("the brief's terminal sequence is not push (%d) -> PR (%d) -> update_task (%d), in that order:\n%s",
			pushIdx, prIdx, updateTaskIdx, brief)
	}

	// The PR step lives in the same pinned terminal block as the update_task call,
	// not one paragraph earlier where CLA-384 showed salience is lost.
	if !strings.Contains(reviewTerminalStep, "PR") {
		t.Errorf("the PR step is not part of reviewTerminalStep, so it does not share the position pinned by TestReviewBriefStatesItsTerminalStep:\n%s", reviewTerminalStep)
	}
}

// The two failure modes named in the 2026-08-19/20 evidence: a session whose cwd
// is already the main checkout committing straight to staging because nothing
// told it that was wrong, and an empty branch field silently adopted as work to
// implement rather than reported as a failed hand-off.
func TestReviewBriefStatesTheWorktreeRuleAndTheEmptyBranchRule(t *testing.T) {
	brief, ok := builtinPhasePrompts[ReviewPhaseName]
	if !ok {
		t.Fatalf("no built-in brief for phase %q", ReviewPhaseName)
	}

	for _, want := range []string{
		// Since CLA-457 the branch is recomposed from plane state: the brief names
		// the verified branch ({{branch}}) rather than asserting an unverified
		// phase-1 success.
		"worktree for branch " + PhaseBranchPlaceholder + " recorded on the task",
		// CLA-608: the branch the session must not commit onto is the resolved PR
		// base, not a literal.
		"never commit to the repo's PR base branch (" + PhasePRBasePlaceholder + ")",
		"empty branch",
		"FAILED hand-off",
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("the review brief does not say %q:\n%s", want, brief)
		}
	}
}

// CLA-497: a checkpoint evidenced by the plane's record - the task left
// `ready` or declared a no-code delivery - names no branch, so the review
// successor gets the no-code brief: it must not assert a verified branch,
// must not call the empty branch field a failed hand-off, and must not tell
// the session to work in a worktree or open a PR. Its terminal step is the
// handover alone, pinned in the same shape TestReviewBriefStatesItsTerminalStep
// pins the branch-shaped one.
func TestNoCodeReviewBriefStatesItsShape(t *testing.T) {
	for _, want := range []string{
		// The record, not a branch, is what the checkpoint means.
		"evidenced by the PLANE'S RECORD",
		// The successor is not told to treat the empty branch as a failure.
		"NOT a failed hand-off",
		// The terminal step: the handover, with the outcome's Tests section.
		"update_task(taskId, runId, status: \"in_review\", outcome: ...)",
		"**Tests**",
		"REFUSES",
		// The form-(b) case where phase 1 already settled the task.
		"already settled the task",
	} {
		if !strings.Contains(noCodeReviewBrief, want) {
			t.Errorf("no-code review brief does not say %q:\n%s", want, noCodeReviewBrief)
		}
	}
	// The branch-shaped instructions must be ABSENT: this brief is selected
	// exactly when there is no branch, so each one would misdirect.
	for _, banned := range []string{
		"FAILED hand-off to report",
		"Work in the worktree",
		"Open a PR",
		"COMMIT and PUSH",
		PhaseBranchPlaceholder,
	} {
		if strings.Contains(noCodeReviewBrief, banned) {
			t.Errorf("no-code review brief still says %q - it is selected exactly when there is no branch to name:\n%s", banned, noCodeReviewBrief)
		}
	}
	// Position, like the branch-shaped brief: the no-code terminal step is the
	// last thing before the shared handoff guidance.
	if !strings.HasSuffix(noCodeReviewBrief, noCodeReviewTerminalStep+handoffGuidance) {
		t.Errorf("the no-code terminal step is not the last thing before the handoff guidance:\n%s", noCodeReviewBrief)
	}
	// The no-code handoff continuation carries the no-code terminal step and
	// the rerun bound, and never the PR step.
	cont := NoCodeHandoffContinuation()
	if !strings.Contains(cont, noCodeReviewTerminalStep) || !strings.Contains(cont, rerunGuidance) {
		t.Errorf("NoCodeHandoffContinuation does not carry the no-code terminal step and rerun bound:\n%s", cont)
	}
	if strings.Contains(cont, "Open a PR") {
		t.Errorf("NoCodeHandoffContinuation carries the branch-shaped PR step:\n%s", cont)
	}
}

// CLA-576: an unverified checkpoint - the implement phase recorded a branch,
// but the driver's read of the origin remote failed - must not reach its
// successor with the branch-shaped brief's "a branch the driver verified to
// exist on the origin remote ... so phase 1 really did implement, commit and
// push". The unverified brief states the checkpoint is unverified, tells the
// review phase to verify the hand-off itself first, and still carries the
// review contract: resume-don't-claim, the adversarial gate, the scoped
// re-verification, the staging rule and the pinned terminal step.
func TestUnverifiedReviewBriefStatesItsShape(t *testing.T) {
	for _, want := range []string{
		// The honest state of the checkpoint.
		"could NOT verify that branch on the origin remote",
		"UNKNOWN",
		"UNVERIFIED",
		"do NOT assume phase 1 implemented, committed and pushed",
		// The cause is the CHECK not completing, never a remote-read failure
		// asserted as fact: delivery.Unknown also covers a check that never
		// reached the remote (no local branch to compare, an ambiguous repo).
		"its check did not complete",
		"a check that never reached the remote is possible too",
		// The self-verification step, before any review.
		"FIRST verify the hand-off yourself",
		"git ls-remote origin " + PhaseBranchPlaceholder,
		// The absent-branch outcome: report it, never review a phantom diff.
		"do not review a phantom diff",
		// The still-unreadable arm names the hand-over rather than leaving the
		// session to end holding the task against the terminal step.
		"hand the task over as the terminal step below requires",
		// The resume contract and the review gate survive.
		"do not call next_task",
		"heartbeat(",
		"get_task with includeDecisions: true",
		"adversarial review gate",
		"re-verify SCOPED to those fixes",
		// The branch still names the worktree, with the resolved-PR-base rule.
		"worktree for branch " + PhaseBranchPlaceholder,
		"never commit to the repo's PR base branch (" + PhasePRBasePlaceholder + ")",
	} {
		if !strings.Contains(unverifiedReviewBrief, want) {
			t.Errorf("unverified review brief does not say %q:\n%s", want, unverifiedReviewBrief)
		}
	}
	// The verified claim itself must be ABSENT: it is exactly what this
	// checkpoint cannot support, and the trap CLA-576 names.
	if strings.Contains(unverifiedReviewBrief, "verified to exist on the origin remote") ||
		strings.Contains(unverifiedReviewBrief, "really did implement, commit and push") {
		t.Errorf("the unverified review brief asserts the verified-branch claim it cannot support:\n%s", unverifiedReviewBrief)
	}
	// The no-code brief's shape must not be reused either: there IS a branch,
	// and its empty-branch discriminator must stay the no-code form's alone.
	if strings.Contains(unverifiedReviewBrief, "There is NO branch") {
		t.Errorf("the unverified review brief tells the successor there is no branch:\n%s", unverifiedReviewBrief)
	}
	// Position, like the other built-in review briefs: the terminal step is the
	// last thing before the shared handoff guidance.
	if !strings.HasSuffix(unverifiedReviewBrief, reviewTerminalStep+handoffGuidance) {
		t.Errorf("the unverified terminal step is not the last thing before the handoff guidance:\n%s", unverifiedReviewBrief)
	}
	// And it is a variant, not the branch-shaped brief itself, so the driver's
	// selection is meaningful: an unverified checkpoint swaps one for the other.
	if unverifiedReviewBrief == builtinPhasePrompts[ReviewPhaseName] {
		t.Error("the unverified brief is the branch-shaped built-in brief")
	}
}

// CLA-637: the review-only brief is the CLI's whole lever on the durable
// record's shape — the SESSION writes the record through its own MCP client —
// so the tokens the plane and the seam gate read are pinned here: the
// update_task field names, the identity the driver fills in, the per-finding
// attribution the analytics need, and the append order the plane's channel
// seal forces. The NO-CODE variant is the exception the operator decided
// (question 03a12578, option 1): a settled task has no live holder, so the
// plane would refuse the durable write and this brief prescribes the legacy
// block ALONE — pinned both ways below.
func TestReviewOnlyBriefsPrescribeTheDurableRecord(t *testing.T) {
	for name, brief := range map[string]string{
		"review-only brief":             reviewOnlyBrief,
		"unverified review-only brief":  unverifiedReviewOnlyBrief,
		"panel member brief":            panelMemberReviewOnlyBrief,
		"unverified panel member brief": panelMemberUnverifiedReviewOnlyBrief,
	} {
		for _, want := range []string{
			"update_task(taskId, runId, review: {...})",
			`reviewerRole: "` + ReviewerRolePlaceholder + `"`,
			"reviewedTip",
			"raisedByModel",
			`scope: {kind: "full-diff", base: "` + PhasePRBasePlaceholder,
			"duplicate_reviewer",
		} {
			if !strings.Contains(brief, want) {
				t.Errorf("the %s does not prescribe %q, so the durable record the seam gates on is not what the session writes:\n%s", name, want, brief)
			}
		}
	}
	// A PANEL MEMBER writes the durable record and NOTHING ELSE on the task
	// (CLA-639 M5): member 1's outcome block would be readable by member 2
	// through its own get_task, and the plane refuses the write anyway once the
	// record is sealed. The legacy block, the outcome call and the ordering
	// sentence that explains it must all be ABSENT.
	for name, brief := range map[string]string{
		"panel member brief":            panelMemberReviewOnlyBrief,
		"unverified panel member brief": panelMemberUnverifiedReviewOnlyBrief,
	} {
		for _, banned := range []string{
			"update_task(taskId, runId, outcome: ...)",
			"Record the LEGACY block",
			"only after that outcome write has SUCCEEDED",
			ReviewRecordHeading,
			"`- reviewed by:`",
		} {
			if strings.Contains(brief, banned) {
				t.Errorf("the %s prescribes %q, but a panel member must not write the task's outcome at all:\n%s", name, banned, brief)
			}
		}
		if !strings.Contains(brief, panelMemberTerminalStep) {
			t.Errorf("the %s does not carry the panel member's record-only ending:\n%s", name, brief)
		}
	}
	// The two halves the SINGLE reviewer's shape prescribes, in that order: the
	// legacy block first, the durable record below it.
	block := strings.Index(reviewOnlyRecordShape, "Record the LEGACY block")
	durable := strings.Index(reviewOnlyRecordShape, "append THIS run's durable")
	if block < 0 || durable < 0 || block > durable {
		t.Errorf("the record shape does not put the legacy outcome block before the durable record (block at %d, durable at %d)", block, durable)
	}

	// The branchless variant prescribes the block ALONE: it must name the block
	// write, must NOT name the durable-record call (the plane would refuse it
	// for a settled task), and its terminal step must say so.
	if !strings.Contains(noCodeReviewOnlyBrief, "update_task(taskId, runId, outcome: ...)") {
		t.Errorf("the no-code review-only brief does not prescribe the legacy block write:\n%s", noCodeReviewOnlyBrief)
	}
	for _, banned := range []string{"update_task(taskId, runId, review: {...})", "THEN append THIS run's durable"} {
		if strings.Contains(noCodeReviewOnlyBrief, banned) {
			t.Errorf("the no-code review-only brief prescribes %q, but the plane refuses every review write on a settled task (question 03a12578, option 1):\n%s", banned, noCodeReviewOnlyBrief)
		}
	}
	if !strings.Contains(noCodeReviewOnlyTerminalStep, "Do NOT attempt update_task(review: ...) here") {
		t.Errorf("the no-code terminal step does not tell the session the durable write is not attempted:\n%s", noCodeReviewOnlyTerminalStep)
	}
}

// CLA-639 M5: a panel member's brief must not write the task's outcome at all
// — member 1's block would land in the task, member 2 would read it through its
// own get_task, and member 2's own outcome write would be refused once member
// 1's record sealed the round. The member records the durable record alone; the
// fix phase is what carries the panel's findings into the outcome.
func TestPanelMemberBriefWritesOnlyItsSealedRecord(t *testing.T) {
	for name, brief := range map[string]string{
		"panel member brief":            panelMemberReviewOnlyBrief,
		"unverified panel member brief": panelMemberUnverifiedReviewOnlyBrief,
	} {
		if !ReviewShaped(brief) {
			t.Errorf("the %s is not review-shaped, so the driver would not gate it on its durable record", name)
		}
		if !strings.Contains(brief, panelMemberTerminalStep) {
			t.Errorf("the %s does not end on the record-only terminal step:\n%s", name, brief)
		}
		if !strings.Contains(brief, "update_task(taskId, runId, review: {...})") {
			t.Errorf("the %s does not prescribe the sealed durable record:\n%s", name, brief)
		}
		// The member is told what its outcome write would do to the next member,
		// so a session that is tempted to write one knows why it must not.
		if !strings.Contains(brief, "the member after you reads the task") {
			t.Errorf("the %s does not say why the outcome write is forbidden:\n%s", name, brief)
		}
		// The panel context: a member knows it is one of several reviewers and
		// that its own findings are what it records.
		if !strings.Contains(brief, "ONE MEMBER of a review PANEL") {
			t.Errorf("the %s does not tell the session it is a panel member:\n%s", name, brief)
		}
		// Everything that makes it a review-only brief still holds.
		lower := strings.ToLower(brief)
		for _, want := range []string{"review-only gate", "review only", "do not commit", "do not push", "do not open a pr"} {
			if !strings.Contains(lower, want) {
				t.Errorf("the %s no longer states %q:\n%s", name, want, brief)
			}
		}
	}

	// The SINGLE reviewer's brief is unchanged: both halves, in the order the
	// plane's seal forces (this is the "single-reviewer configs keep today's
	// brief exactly" half of M5, pinned where the text lives).
	if !strings.Contains(reviewOnlyBrief, reviewOnlyRecordShape) || !strings.Contains(reviewOnlyBrief, reviewOnlyTerminalStep) {
		t.Errorf("the single reviewer's brief lost its two-half record or its ending:\n%s", reviewOnlyBrief)
	}
	if strings.Contains(reviewOnlyBrief, panelMemberRecordShape) || strings.Contains(reviewOnlyBrief, panelMemberTerminalStep) {
		t.Errorf("the single reviewer's brief picked up the panel member's record-only contract:\n%s", reviewOnlyBrief)
	}
}

// ReviewShaped is the driver's single "this phase owes a durable review
// record" predicate (CLA-637): true for every review-ONLY variant the briefs
// ship — the seam gate and the round close apply to all of them, not only the
// exact built-in prompt the gate was first keyed to — and false for everything
// else. The final combined review brief does its own fixing and hands the task
// over, so it records no durable round; an operator's custom prompt owns its
// own contract.
func TestReviewShaped(t *testing.T) {
	tests := []struct {
		name   string
		prompt string
		want   bool
	}{
		{"the review-only brief", reviewOnlyBrief, true},
		{"the unverified review-only brief", unverifiedReviewOnlyBrief, true},
		{"the panel member brief", panelMemberReviewOnlyBrief, true},
		{"the unverified panel member brief", panelMemberUnverifiedReviewOnlyBrief, true},
		{"the no-code review-only brief", noCodeReviewOnlyBrief, true},
		{"the verification brief", verifyBrief, true},
		{"the no-code verification brief", noCodeVerifyBrief, true},
		{"the final combined review brief", builtinPhasePrompts[ReviewPhaseName], false},
		{"the fix brief", fixBrief, false},
		{"the fix-with-verification brief", fixVerifyBrief, false},
		{"the implement brief", builtinPhasePrompts[ImplementPhaseName], false},
		{"an operator's custom prompt", "Do your own review-shaped job, whatever that means here.", false},
		{"an empty prompt", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReviewShaped(tc.prompt); got != tc.want {
				t.Errorf("ReviewShaped(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// CLA-353: a handoff respawn replaces ph.Prompt wholesale with
// config.HandoffPreamble plus the predecessor's self-authored prompt (CLA-352) —
// the built-in brief text, including reviewTerminalStep, is not otherwise part of
// it. Without HandoffContinuation a session that hands off mid-review drops the
// PR-then-update_task sequence for its successor, silently reintroducing the bug
// this task fixes on the one path a wording change in the brief cannot reach.
// Since CLA-391 the rerun bound rides forward too — for BOTH built-in phases,
// pinned below — so a handoff cannot be how unbounded rerunning arrives.
func TestHandoffContinuation_CarriesTheReviewTerminalStepForward(t *testing.T) {
	got := HandoffContinuation(ReviewPhaseName)
	if !strings.Contains(got, reviewTerminalStep) {
		t.Errorf("HandoffContinuation(%q) does not carry reviewTerminalStep forward:\n%s", ReviewPhaseName, got)
	}
	// And the rerun bound rides along (phase-2 review of CLA-391): the
	// successor inheriting a half-fixed finding list is the loopiest session
	// there is, and a handoff must not be how it arrives unbounded.
	if !strings.Contains(got, rerunGuidance) {
		t.Errorf("HandoffContinuation(%q) does not carry the rerun bound forward:\n%s", ReviewPhaseName, got)
	}

	// The implement phase has no terminal step to lose — it stops rather than
	// hands its task to in_review, and a later phase owns the resume brief — but
	// its handoffs are real, so the rerun bound rides forward for it too
	// (phase-2 review of CLA-391): its successor is mid-verification with none
	// of the predecessor's memory of the runs already paid for, which is exactly
	// the session that starts paying again. The terminal step, though, stays the
	// review phase's alone.
	if got := HandoffContinuation(ImplementPhaseName); !strings.Contains(got, rerunGuidance) {
		t.Errorf("HandoffContinuation(%q) does not carry the rerun bound forward:\n%s", ImplementPhaseName, got)
	} else if strings.Contains(got, reviewTerminalStep) {
		t.Errorf("HandoffContinuation(%q) carries reviewTerminalStep, which belongs to the review phase alone:\n%s", ImplementPhaseName, got)
	}
	if got := HandoffContinuation("not-a-real-phase"); got != "" {
		t.Errorf("HandoffContinuation of an unknown phase = %q, want empty", got)
	}
}

// CLA-391: repeated verification was the dominant measured waste of the
// 2026-08-19/20 drain - fourteen near-identical Playwright runs differing only
// in the grep filter, twelve identical `go test -race ./...` on a two-file
// diff. The built-in briefs bound consecutive reruns of the same command and
// demand a stated reason past the bound. Pinned per brief, in the shape
// TestBuiltinPhaseBriefsCarryTheHandoffGuidance established, so a rewrite
// cannot drop the bound from one brief silently.
func TestBuiltinPhaseBriefsCarryTheRerunBound(t *testing.T) {
	for name, brief := range builtinPhasePrompts {
		for _, want := range []string{
			// The bound itself, on consecutive reruns of one command.
			"two consecutive reruns of the same command",
			// The escape hatch has a price: a stated reason, which is what
			// keeps "I changed something, re-run it" open while a loop of
			// identical runs has nothing to say.
			"a third needs a stated reason",
			// Reading 1 the task called out: the bound is per command, not on
			// verification overall - typecheck, tests, lint is three commands
			// run once each.
			"per command, not on verification overall",
			// Reading 2: a narrowed rerun is still a rerun. The fourteen-run
			// case differed only by its filter, so a bound phrased only as
			// "the same command" would not have caught it.
			"different selector, filter or flag",
			// The CLAIM that a narrowed rerun counts, not just its vocabulary:
			// inverting the sentence around "different selector, filter or flag"
			// used to leave every pinned fragment verbatim intact while
			// asserting the opposite of the doneWhen (phase-2 review).
			"counts against the same command's bound",
			"not a change to what is under test",
			// Consecutiveness defined: other tool calls cannot reset the count,
			// or interleaved reads and greps defeat the bound entirely.
			"do not reset the count",
			// The arithmetic past a stated reason, which the first wording left
			// to the reader: a reason buys one run, not a fresh pair.
			"buys one run, not a fresh pair",
			// The idle-polling case points at the served waiting reference
			// rather than restating its rules.
			"clankerbar.com/skills/clankerbar/waiting.md",
			// ...and the pointer is scoped to KEEPING the task, because Rule 1
			// of that reference read unscoped tells a mid-phase session to
			// release.
			"keeping hold of this task",
		} {
			if !strings.Contains(brief, want) {
				t.Errorf("the %q brief never says %q; the rerun bound is not pinned in it:\n%s", name, want, brief)
			}
		}
	}
}

// The rerun guidance must not dilute the endings. CLA-384's lesson was
// position, not presence: an emphatic final block works, a trailing clause
// does not. So the bound rides BEFORE each brief's ending - the handoff
// guidance for the implement phase, the terminal step for the review phase -
// leaving the review brief's terminal-step-then-handoff suffix untouched
// (TestReviewBriefStatesItsTerminalStep pins that suffix exactly).
func TestRerunGuidanceSitsBeforeEachBriefsEnding(t *testing.T) {
	// Both directions of CLA-384's position lesson: LATE enough to ride with
	// the brief's tail (prefixing it ahead of the working instructions would
	// bury it at the front, which the first version of this test permitted),
	// and never displacing the emphatic ending each brief must keep.
	impl := builtinPhasePrompts[ImplementPhaseName]
	ci := strings.Index(impl, "update_task(taskId, runId, branch)")
	ri, hi := strings.Index(impl, rerunGuidance), strings.Index(impl, handoffGuidance)
	if ri == -1 || hi == -1 || ci == -1 || ri < ci || ri > hi {
		t.Errorf("the implement brief does not carry the rerun bound after its checkpoint instruction (%d) and before its handoff guidance (%d); got rerun at %d:\n%s", ci, hi, ri, impl)
	}

	rev := builtinPhasePrompts[ReviewPhaseName]
	gi := strings.Index(rev, "adversarial review gate")
	rr, ti := strings.Index(rev, rerunGuidance), strings.Index(rev, reviewTerminalStep)
	if rr == -1 || ti == -1 || gi == -1 || rr < gi || rr > ti {
		t.Errorf("the review brief does not carry the rerun bound after its gate paragraph (%d) and before its terminal step (%d); got rerun at %d:\n%s", gi, ti, rr, rev)
	}
}

// The waiting pointer must stay a pointer. The served waiting reference owns
// the mechanics - lease cadence, blocking inside one tool call - and a second
// copy in the brief would drift, the same way a test carrying its own copy of
// a string just moves the rot. Naming the problem and naming the address is
// the brief's whole obligation here.
func TestRerunGuidancePointsAtWaitingRatherThanRestatingIt(t *testing.T) {
	if !strings.Contains(rerunGuidance, "clankerbar.com/skills/clankerbar/waiting.md") {
		t.Errorf("rerunGuidance does not name the served waiting reference:\n%s", rerunGuidance)
	}
	// Whole words, not substrings: the substring match fired on "release"
	// containing "lease", failing a future edit that was doing the right
	// thing and pointing its author at the wrong repair. The ban remains a
	// heuristic against copying the reference's mechanics vocabulary - it
	// cannot prove a restatement absent, only make the cheap drifts loud.
	words := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(rerunGuidance), func(r rune) bool { return !unicode.IsLetter(r) }) {
		words[w] = true
	}
	for _, banned := range []string{"heartbeat", "lease", "sleep"} {
		if words[banned] {
			t.Errorf("rerunGuidance says %q - it is restating the waiting reference's mechanics, which live at the address it should point at and would drift here:\n%s", banned, rerunGuidance)
		}
	}
	// The pointer is scoped as well as pointed (phase-2 review): Rule 1 of the
	// waiting reference tells a claim-holder to let go, and read unscoped it
	// directs a mid-phase session to release - the exact failure the briefs'
	// own endings forbid. The session takes the polling discipline and keeps
	// the task.
	if !strings.Contains(rerunGuidance, "keeping hold of this task") {
		t.Errorf("rerunGuidance does not scope the waiting pointer to keeping the task; unscoped, Rule 1 reads as permission to release mid-phase:\n%s", rerunGuidance)
	}
}

// CLA-608: the branch that phase briefs name as the PR target is resolved per
// repo at spawn time, so every shipped brief that names one must carry the
// {{prBase}} placeholder — and none may carry a literal branch name, which is
// the bug being fixed (any literal is wrong for some repo in some project).
// The no-code brief names no branch at all and is deliberately exempt.
func TestBuiltinBriefsCarryThePRBasePlaceholder(t *testing.T) {
	for name, brief := range builtinPhasePrompts {
		if !strings.Contains(brief, PhasePRBasePlaceholder) {
			t.Errorf("the %q brief never carries %s, so its PR base is either absent or hard-coded:\n%s", name, PhasePRBasePlaceholder, brief)
		}
		if strings.Contains(brief, "(staging)") {
			t.Errorf("the %q brief still hard-codes the old (staging) literal:\n%s", name, brief)
		}
	}
	if !strings.Contains(unverifiedReviewBrief, PhasePRBasePlaceholder) {
		t.Errorf("the unverified review brief never carries %s:\n%s", PhasePRBasePlaceholder, unverifiedReviewBrief)
	}
	if !strings.Contains(reviewTerminalStep, PhasePRBasePlaceholder) {
		t.Errorf("reviewTerminalStep does not carry %s, so the PR step's target cannot be resolved:\n%s", PhasePRBasePlaceholder, reviewTerminalStep)
	}
	if !strings.Contains(implementResumedBranchRule, PhasePRBasePlaceholder) {
		t.Errorf("implementResumedBranchRule does not carry %s, so a resumed branch is synced against the wrong branch:\n%s", PhasePRBasePlaceholder, implementResumedBranchRule)
	}
	// The placeholder is unresolved text: a phase-0 prompt carrying the RESUME
	// placeholders is refused (see the presence check in Validate), and {{prBase}}
	// is deliberately not one of those — it needs no claim — so this pins that it
	// is NOT smuggled into that refusal set.
	if strings.Contains(implementCommitEarlyRule, PhasePRBasePlaceholder) {
		t.Errorf("the commit-early rule carries %s; it belongs where a branch is named, not in entry guidance:\n%s", PhasePRBasePlaceholder, implementCommitEarlyRule)
	}
}

// The split's mode is POSITIONAL: a review with a phase after it is the
// read-only gate; a final review keeps the historical review-and-fix brief so
// an existing two-phase config still reaches in_review. Pinning both arms is
// what stops a later edit from silently flipping every existing config into a
// sequence that stops at a checkpoint with no successor.
func TestEffectivePhases_ReviewIsOnlyWhenAPhaseFollowsIt(t *testing.T) {
	three := defaults()
	three.Prompt = ""
	three.Phases = []Phase{{Name: ImplementPhaseName}, {Name: ReviewPhaseName}, {Name: FixPhaseName}}

	got := three.EffectivePhases()
	if len(got) != 3 {
		t.Fatalf("got %d phases, want 3", len(got))
	}
	if got[1].Prompt != reviewOnlyBrief {
		t.Errorf("a non-final review did not resolve to the review-only brief:\n%s", got[1].Prompt)
	}
	if got[2].Prompt != fixBrief {
		t.Errorf("the fix phase did not resolve to the fix brief:\n%s", got[2].Prompt)
	}

	two := defaults()
	two.Prompt = ""
	two.Phases = []Phase{{Name: ImplementPhaseName}, {Name: ReviewPhaseName}}
	got = two.EffectivePhases()
	if got[1].Prompt != builtinPhasePrompts[ReviewPhaseName] {
		t.Errorf("a FINAL review lost the historical review-and-fix brief, so an existing two-phase config would stop at the checkpoint:\n%s", got[1].Prompt)
	}

	// An operator's own prompt is never rewritten by the positional rule.
	custom := defaults()
	custom.Prompt = ""
	custom.Phases = []Phase{{Name: ImplementPhaseName}, {Name: ReviewPhaseName, Prompt: "my gate"}, {Name: FixPhaseName}}
	if got := custom.EffectivePhases()[1].Prompt; got != "my gate" {
		t.Errorf("the positional rule overwrote a custom review prompt with %q", got)
	}
}

// The review-only brief is the whole cost argument: the expensive model reads
// and judges, and changes nothing. Pin the prohibitions and the record, not
// merely the presence of the word "review" — a brief that says "review only"
// and then asks it to fix what it finds is the thing being removed.
func TestBuiltinReviewOnlyBriefStatesItsScopeAndRecord(t *testing.T) {
	brief := reviewOnlyBrief
	lower := strings.ToLower(brief)

	for _, want := range []string{
		"review-only gate",
		"find issues",
		"specify fixes",
		"review only",
		"do not edit, create or delete a source file",
		"do not commit",
		"do not push",
		"do not open a pr",
		"reviewed tip",
		"verdict: pass",
		"verdict: findings",
		"r1 [must|should|nit]",
		"required fix",
		"failed hand-off",
		"heartbeat(",
		"get_task with includedecisions: true",
	} {
		if !strings.Contains(lower, want) {
			t.Errorf("the review-only brief never says %q:\n%s", want, brief)
		}
	}
	if !strings.Contains(brief, ReviewRecordHeading) {
		t.Errorf("the review-only brief never carries the record heading %q the seam gate looks for:\n%s", ReviewRecordHeading, brief)
	}
	// Placeholders are case-sensitive: the lowercased copy cannot carry them.
	for _, want := range []string{
		PhaseTaskPlaceholder,
		PhaseRunPlaceholder,
		PhaseBranchPlaceholder,
		PhaseHarnessPlaceholder,
		PhaseModelPlaceholder,
		PhasePrevHarnessPlaceholder,
		PhasePrevModelPlaceholder,
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("the review-only brief never carries %s:\n%s", want, brief)
		}
	}

	// The actions that belong to the fix phase must be ABSENT, not merely
	// contradicted somewhere else: a session reads the brief linearly, and a
	// dangling "Then COMMIT and PUSH" is an instruction whatever the paragraph
	// above said.
	for _, banned := range []string{
		"Open a PR", "COMMIT and PUSH", `status: "in_review"`, "fix what it finds",
	} {
		if strings.Contains(brief, banned) {
			t.Errorf("the review-only brief carries %q, which is the fix phase's job:\n%s", banned, brief)
		}
	}

	// Position: the record-then-hold ending is the last thing before the shared
	// handoff guidance, in the same emphatic shape CLA-384 pinned on the old
	// terminal step.
	if !strings.HasSuffix(brief, reviewOnlyTerminalStep+handoffGuidance) {
		t.Errorf("the review-only ending is not the last thing before the handoff guidance:\n%s", brief)
	}
	if !strings.Contains(reviewOnlyTerminalStep, "do NOT COMMIT, PUSH or open a PR") {
		t.Errorf("the review-only terminal step does not forbid the fix phase's actions:\n%s", reviewOnlyTerminalStep)
	}
}

// CLA-635: every review-only variant — the three briefs and the two handoff
// continuations that carry the record shape — states the accepted MUST bar: the
// seven evidence kinds with their payloads, and the not-a-MUST list, whose
// precedence over a repo hard rule about style, prose or doc wording is B6.
func TestReviewOnlyVariantsCarryTheMUSTBar(t *testing.T) {
	variants := map[string]string{
		"review-only brief":             reviewOnlyBrief,
		"no-code review-only brief":     noCodeReviewOnlyBrief,
		"unverified review-only brief":  unverifiedReviewOnlyBrief,
		"panel member brief":            panelMemberReviewOnlyBrief,
		"unverified panel member brief": panelMemberUnverifiedReviewOnlyBrief,
		"review-only handoff":           HandoffContinuationFor(ReviewPhaseName, true),
		"no-code review-only handoff":   NoCodeReviewOnlyHandoffContinuation(),
	}
	for name, brief := range variants {
		lower := strings.ToLower(brief)
		for _, want := range []string{
			"grade every finding",
			"must is the only gating grade",
			"donewhen - quote the clause",
			"gate - name a repo gate",
			"failure - a concrete input or state",
			"security - credential exposure",
			"decision - a recorded standing decision",
			"irreversible - an irreversible migration",
			"untested - a new or changed guard",
			"never a must, however confident",
			"missing tests the donewhen does not require (except untested)",
			"not-a-must list wins",
			"they need no evidence",
		} {
			if !strings.Contains(lower, want) {
				t.Errorf("the %s never states %q, so the bar it reviews against is unstated:\n%s", name, want, brief)
			}
		}
	}
}

// CLA-635: the record format the review fills in carries, per finding, its id,
// severity, evidence kind and evidence, file:line, summary and required fix;
// the block attributes the reviewer (harness/model) once, and every finding in
// it inherits that attribution.
func TestReviewOnlyRecordFormatCarriesTheGradedFindingFields(t *testing.T) {
	for _, want := range []string{
		"R1 [must|should|nit]",
		"(<kind>): <evidence>",
		"<file:line> - <what is wrong>",
		"Required fix:",
		"- reviewed by: harness " + PhaseHarnessPlaceholder + ", model " + PhaseModelPlaceholder,
	} {
		if !strings.Contains(reviewOnlyRecordShape, want) {
			t.Errorf("the review record shape does not carry %q, so the fix phase cannot read it back:\n%s", want, reviewOnlyRecordShape)
		}
	}
}

// CLA-635: the fix briefs hold the fixer to the grade (A1/B5): every MUST is
// fixed or refuted with evidence in the outcome, the fixer may not downgrade a
// MUST, a single reviewer's MUST is never dropped because it stands alone, and
// SHOULD/NIT get the filing rule.
func TestBuiltinFixBriefsHoldTheFixerToTheGrade(t *testing.T) {
	for name, brief := range map[string]string{
		"fix brief":         fixBrief,
		"no-code fix brief": noCodeFixBrief,
	} {
		lower := strings.ToLower(brief)
		for _, want := range []string{
			"every must is fixed, or refuted with evidence recorded in your outcome",
			"you may not downgrade a must",
			"a must raised by a single reviewer is never dropped because nobody else raised it",
			"listing a recorded must with no evidence kind or no evidence payload as a should",
			"record nits and the shoulds you do not fix in your outcome as not done, with the reason",
			"file a follow-up task only for a should worth doing",
		} {
			if !strings.Contains(lower, want) {
				t.Errorf("the %s never states %q, so the fix phase can drop or silently downgrade a MUST:\n%s", name, want, brief)
			}
		}
	}
	// The ### Fixes obligation keeps that promise in the durable record: a MUST
	// left unfixed must be a refutation carrying its evidence.
	if !strings.Contains(strings.ToLower(fixTerminalRecord), "for a must that reason must be a refutation carrying its evidence") {
		t.Errorf("the fix record requirement does not say an unfixed MUST must be a refutation with evidence:\n%s", fixTerminalRecord)
	}
	// The no-code brief's one unreachable case must not contradict the grade: a
	// code-needing MUST can be neither fixed nor refuted there, so dispatching
	// it to a real follow-up task has to count as the fix — and the record
	// requirement has to accept the task ref as that fix's evidence, or the
	// dispatched MUST still reads as "did not (with a reason)".
	if !strings.Contains(strings.ToLower(noCodeFixBrief), "for a must, that filed task is this phase's fix of it") {
		t.Errorf("the no-code fix brief leaves a dispatched MUST contradicting the fix-or-refute rule:\n%s", noCodeFixBrief)
	}
	if !strings.Contains(strings.ToLower(fixTerminalRecord), "the ref of the real follow-up task a code-needing finding was dispatched to") {
		t.Errorf("the fix record requirement does not name a dispatch as a fix's evidence:\n%s", fixTerminalRecord)
	}
}

// The fix brief is the one that must land the PR and the hand-off, and it must
// carry the review forward: once it overwrites the review's outcome, the review
// block in its own outcome is the only durable record of what was found and
// what was done about it.
func TestBuiltinFixBriefCarriesTheReviewForwardAndTheTerminalStep(t *testing.T) {
	brief := fixBrief
	lower := strings.ToLower(brief)

	for _, want := range []string{
		"the fix phase and the last phase",
		"## review",
		"each by its plane id",
		"reviewedtip",
		"git rev-parse head",
		"park the task",
		"### fixes",
		"reviews.records",
		"durable review record",
		"fixed by: harness",
		"failed hand-off",
		"do not review the work yourself",
	} {
		if !strings.Contains(lower, want) {
			t.Errorf("the fix brief never says %q:\n%s", want, brief)
		}
	}
	for _, want := range []string{
		PhaseTaskPlaceholder,
		PhaseRunPlaceholder,
		PhaseBranchPlaceholder,
		PhasePRBasePlaceholder,
		PhaseHarnessPlaceholder,
		PhaseModelPlaceholder,
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("the fix brief never carries %s:\n%s", want, brief)
		}
	}

	// The PR and in_review step is the shared terminal block, in its pinned
	// position: a fix phase that ends without it is the CLA-384 failure again,
	// with the added cost that the findings were recorded and then stranded.
	if !strings.HasSuffix(brief, reviewTerminalStep+handoffGuidance) {
		t.Errorf("the fix brief does not end on the shared terminal step:\n%s", brief)
	}
}

// The exemption's fix-side half (question 03a12578, option 1): the branchless
// review writes no durable record, so the fix brief that follows must read the
// legacy block alone. A brief that demanded `reviews.records`, a PLANE id or a
// `findingUpdates` call would be unexecutable on every branchless round — the
// exact dead-end the exemption removes, one phase later.
func TestNoCodeFixBriefReadsTheBlockAlone(t *testing.T) {
	for _, banned := range []string{
		"reviews.records",
		"PLANE ids",
		"findingUpdates",
		"durable review record",
		"update_task(taskId, runId, review: {...})",
	} {
		if strings.Contains(noCodeFixBrief, banned) {
			t.Errorf("the no-code fix brief prescribes %q, but a branchless round writes no durable record (question 03a12578, option 1):\n%s", banned, noCodeFixBrief)
		}
	}
	for _, want := range []string{
		"read the LAST `" + ReviewRecordHeading + "` block",
		"read and apply from the block alone",
		"AMENDING THE RECORD",
	} {
		if !strings.Contains(noCodeFixBrief, want) {
			t.Errorf("the no-code fix brief does not %q:\n%s", want, noCodeFixBrief)
		}
	}
	// Its terminal record is the outcome requirement alone — no findingUpdates
	// half — and the no-code handover ends the brief.
	if !strings.Contains(noCodeFixBrief, noCodeFixTerminalRecord) {
		t.Errorf("the no-code fix brief does not carry the outcome-only terminal record:\n%s", noCodeFixBrief)
	}
	if strings.Contains(noCodeFixTerminalRecord, "findingUpdates") {
		t.Errorf("the no-code terminal record demands a durable findingUpdates call:\n%s", noCodeFixTerminalRecord)
	}
	if !strings.HasSuffix(noCodeFixBrief, noCodeReviewTerminalStep+handoffGuidance) {
		t.Errorf("the no-code fix brief does not end on the no-code handover:\n%s", noCodeFixBrief)
	}
}

// A statusless update_task outcome REPLACES the whole field on the plane, so a
// review-only brief that writes only its own block erases phase 1's delivery
// record, and a fix brief that rebuilds the outcome from the review block plus
// ### Fixes erases it again. Both must state the append/keep-above contract,
// handoff continuations included — a respawned successor never saw the original
// brief, and it is the session most likely to have only its own block in hand.
func TestBuiltinReviewAndFixBriefsPreserveWhatSitsAboveTheBlock(t *testing.T) {
	reviewBriefs := map[string]string{
		"review-only brief":            reviewOnlyBrief,
		"no-code review-only brief":    noCodeReviewOnlyBrief,
		"unverified review-only brief": unverifiedReviewOnlyBrief,
		"review-only handoff":          HandoffContinuationFor(ReviewPhaseName, true),
		"no-code review-only handoff":  NoCodeReviewOnlyHandoffContinuation(),
	}
	for name, brief := range reviewBriefs {
		lower := strings.ToLower(brief)
		for _, want := range []string{"existing outcome", "appending", "unchanged"} {
			if !strings.Contains(lower, want) {
				t.Errorf("the %s never says %q, so a review session can erase the outcome it found:\n%s", name, want, brief)
			}
		}
	}

	fixBriefs := map[string]string{
		"fix brief":            fixBrief,
		"no-code fix brief":    noCodeFixBrief,
		"fix handoff":          NoCodeFixHandoffContinuation(),
		"fix handoff (branch)": HandoffContinuation(FixPhaseName),
	}
	for name, brief := range fixBriefs {
		lower := strings.ToLower(brief)
		for _, want := range []string{"above the last", "verbatim", "### fixes"} {
			if !strings.Contains(lower, want) {
				t.Errorf("the %s never says %q, so the final outcome can lose what sat above the review block:\n%s", name, want, brief)
			}
		}
	}
}

// The fix phase can face more than one `## Review` block: both the review and
// the fix APPEND, so a request_changes re-trip leaves round 1's block above
// round 2's. Every fix brief and every continuation that reaches a fix session
// must say which block is the round to apply — the LAST one — or the session
// applies an earlier round's findings, whose reviewed tip is stale by design.
func TestFixBriefsAndHandoffsPointAtTheLastReviewBlock(t *testing.T) {
	fixBriefs := map[string]string{
		"fix brief":            fixBrief,
		"no-code fix brief":    noCodeFixBrief,
		"fix handoff":          NoCodeFixHandoffContinuation(),
		"fix handoff (branch)": HandoffContinuation(FixPhaseName),
	}
	for name, brief := range fixBriefs {
		if !strings.Contains(brief, "LAST `"+ReviewRecordHeading+"`") {
			t.Errorf("the %s never points the session at the LAST %q block:\n%s", name, ReviewRecordHeading, brief)
		}
	}
}

func TestValidate_FixPhaseRules(t *testing.T) {
	tests := []struct {
		name   string
		phases []Phase
		want   string
	}{
		{
			name:   "fix must be last",
			phases: []Phase{{Name: ImplementPhaseName}, {Name: ReviewPhaseName}, {Name: FixPhaseName}, {Name: "closer", Prompt: "finish"}},
			want:   "LAST",
		},
		{
			name:   "fix needs a review before it",
			phases: []Phase{{Name: "builder", Prompt: "claim and build"}, {Name: FixPhaseName}},
			want:   ReviewPhaseName,
		},
		{
			name:   "a review-only phase must not be followed by another builtin that does not apply findings",
			phases: []Phase{{Name: ImplementPhaseName}, {Name: ReviewPhaseName}, {Name: ReviewPhaseName}},
			want:   FixPhaseName,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := defaults()
			c.Prompt = ""
			c.Phases = tc.phases
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %+v", tc.phases)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the rejection does not name %q: %v", tc.want, err)
			}
		})
	}

	// A phase carrying its OWN prompt is a legitimate applier — the operator owns
	// what runs after the review-only gate — so the rule must not refuse it.
	custom := defaults()
	custom.Prompt = ""
	custom.Phases = []Phase{{Name: ImplementPhaseName}, {Name: ReviewPhaseName}, {Name: "ship-it", Prompt: "apply the review's findings, push and hand over"}}
	if err := custom.Validate(); err != nil {
		t.Errorf("Validate refused a custom phase after the review-only gate: %v", err)
	}

	// The shipped sequence is the one that must validate.
	c := defaults()
	c.Prompt = ""
	c.Phases = []Phase{{Name: ImplementPhaseName}, {Name: ReviewPhaseName}, {Name: FixPhaseName}}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate refused the shipped implement/review/fix sequence: %v", err)
	}
}

// A first phase has no predecessor to name. The review brief's attribution
// placeholders are refused there for the same reason the resume placeholders
// are: a literal {{prevModel}} in a log is a misconfiguration announcing itself,
// but there is no reason to spend a session discovering it.
func TestValidate_RefusesPrevAttributionInFirstPosition(t *testing.T) {
	for _, ph := range []string{PhasePrevHarnessPlaceholder, PhasePrevModelPlaceholder} {
		c := defaults()
		c.Prompt = ""
		c.Phases = []Phase{{Name: "solo", Prompt: "do the thing " + ph}}
		err := c.Validate()
		if err == nil {
			t.Errorf("Validate accepted %s in phase 0; there is no previous phase to fill it from", ph)
			continue
		}
		if !strings.Contains(err.Error(), ph) {
			t.Errorf("the rejection does not name %s: %v", ph, err)
		}
	}
}

// A handoff must not be a way to lose the mode. Review-only carries the
// record-then-hold ending; the fix phase carries the PR/in_review terminal step;
// the branchless review-only carries neither the PR step nor the no-code
// handover, which belongs to the fix phase.
func TestHandoffContinuation_CarriesThePhaseModeForward(t *testing.T) {
	reviewOnly := HandoffContinuationFor(ReviewPhaseName, true)
	if !strings.Contains(reviewOnly, reviewOnlyTerminalStep) {
		t.Errorf("a review-only handoff does not carry the record-then-hold ending:\n%s", reviewOnly)
	}
	// The ending says "in the shape above"; a respawned successor never saw the
	// shape, and the seam gate refuses a record that does not carry the heading.
	if !strings.Contains(reviewOnly, reviewOnlyRecordShape) {
		t.Errorf("a review-only handoff does not carry the record shape its ending refers to:\n%s", reviewOnly)
	}
	if strings.Contains(reviewOnly, reviewTerminalStep) {
		t.Errorf("a review-only handoff carries the commit/push/PR/in_review terminal step:\n%s", reviewOnly)
	}

	combined := HandoffContinuationFor(ReviewPhaseName, false)
	if !strings.Contains(combined, reviewTerminalStep) {
		t.Errorf("a final review handoff lost the terminal step:\n%s", combined)
	}

	fix := HandoffContinuationFor(FixPhaseName, false)
	if !strings.Contains(fix, reviewTerminalStep) {
		t.Errorf("a fix-phase handoff lost the terminal step:\n%s", fix)
	}
	// The record requirement must ride too: a mid-fix handoff that dropped it
	// would overwrite the task's outcome without the review block, losing the
	// only durable record of the round.
	if !strings.Contains(fix, fixTerminalRecord) {
		t.Errorf("a fix-phase handoff lost the requirement to carry the review block forward:\n%s", fix)
	}

	noCode := NoCodeReviewOnlyHandoffContinuation()
	if !strings.Contains(noCode, noCodeReviewOnlyTerminalStep) {
		t.Errorf("a branchless review-only handoff does not carry its ending:\n%s", noCode)
	}
	if !strings.Contains(noCode, noCodeReviewOnlyRecordShape) {
		t.Errorf("a branchless review-only handoff does not carry the no-tip record shape:\n%s", noCode)
	}
	if strings.Contains(noCode, noCodeReviewTerminalStep) {
		t.Errorf("a branchless review-only handoff carries the no-code handover, which is the fix phase's:\n%s", noCode)
	}

	// An UNVERIFIED checkpoint has no driver-known tip either, so its successor
	// must get the shape that tells it to establish the revision itself; the
	// verified shape's {{reviewedTip}} could never be filled (CLA-576/637).
	unverified := UnverifiedReviewOnlyHandoffContinuation()
	if !strings.Contains(unverified, reviewOnlyTerminalStep) {
		t.Errorf("an unverified review-only handoff does not carry the record-then-hold ending:\n%s", unverified)
	}
	if !strings.Contains(unverified, unverifiedReviewOnlyRecordShape) {
		t.Errorf("an unverified review-only handoff does not carry the self-established-tip record shape:\n%s", unverified)
	}
	if strings.Contains(unverified, ReviewedTipPlaceholder) {
		t.Errorf("an unverified review-only handoff carries the verified tip placeholder, which nothing can fill:\n%s", unverified)
	}

	noCodeFix := NoCodeFixHandoffContinuation()
	if !strings.Contains(noCodeFix, noCodeReviewTerminalStep) || !strings.Contains(noCodeFix, noCodeFixTerminalRecord) {
		t.Errorf("a branchless fix handoff does not carry the no-code terminal step and the review block requirement:\n%s", noCodeFix)
	}
	if strings.Contains(noCodeFix, "Open a PR") {
		t.Errorf("a branchless fix handoff carries the branch-shaped PR step:\n%s", noCodeFix)
	}
	// The exemption's fix-side half (question 03a12578, option 1): the
	// branchless review writes no durable record, so a continuation that
	// demanded a findingUpdates call would fail every branchless round closed.
	if strings.Contains(noCodeFix, "findingUpdates") || strings.Contains(noCodeFix, "DURABLE review record") {
		t.Errorf("a branchless fix handoff demands the durable record the branchless review cannot write:\n%s", noCodeFix)
	}
}

// CLA-638: a fix followed by a verification round must not carry the PR terminal
// step; the verifier owns it. The legacy three-phase shape keeps the old brief.
func TestEffectivePhases_FixAndVerifyVariantsByPosition(t *testing.T) {
	four := defaults()
	four.Prompt = ""
	four.Phases = []Phase{
		{Name: ImplementPhaseName},
		{Name: ReviewPhaseName, Tier: "strong"},
		{Name: FixPhaseName, Tier: "cheap"},
		{Name: VerifyPhaseName},
	}
	got := four.EffectivePhases()
	if len(got) != 4 {
		t.Fatalf("got %d phases, want 4", len(got))
	}
	if got[2].Prompt != fixVerifyBrief {
		t.Errorf("a fix followed by a verification round did not resolve to the no-PR variant:\n%s", got[2].Prompt)
	}
	if got[3].Prompt != verifyBrief {
		t.Errorf("the named verify phase did not resolve to its built-in brief:\n%s", got[3].Prompt)
	}
	if strings.Contains(got[2].Prompt, "Open a PR targeting") {
		t.Errorf("the fix brief still carries the PR terminal step with a verifier after it:\n%s", got[2].Prompt)
	}

	three := defaults()
	three.Prompt = ""
	three.Phases = []Phase{{Name: ImplementPhaseName}, {Name: ReviewPhaseName}, {Name: FixPhaseName}}
	if got := three.EffectivePhases()[2].Prompt; got != fixBrief {
		t.Errorf("the legacy three-phase fix lost its terminal-step brief")
	}
}

// A5: the verifier defaults to the FIX phase's harness and tier — the cheap,
// non-raise side of the split — and an explicit choice always wins. This is what
// keeps a bare verify phase off the expensive reviewer's model and, in the
// shipped shape, off Claude.
func TestEffectivePhases_VerifyInheritsTheFixHarnessAndTier(t *testing.T) {
	c := defaults()
	c.Prompt = ""
	c.Harness = "claude"
	c.Harnesses = map[string]HarnessConfig{"opencode": {MCPConfigPath: "/tmp/oc.json", ConfigDir: "/tmp/oc"}}
	c.Phases = []Phase{
		{Name: ImplementPhaseName},
		{Name: ReviewPhaseName, Tier: "strong"},
		{Name: FixPhaseName, Harness: "opencode", Tier: "cheap"},
		{Name: VerifyPhaseName},
	}
	got := c.EffectivePhases()
	if got[3].Harness != "opencode" || got[3].Tier != "cheap" {
		t.Errorf("verify defaults = harness %q tier %q, want the fix phase's opencode/cheap", got[3].Harness, got[3].Tier)
	}

	c.Phases[3].Harness = "claude"
	c.Phases[3].Tier = "strong"
	got = c.EffectivePhases()
	if got[3].Harness != "claude" || got[3].Tier != "strong" {
		t.Errorf("an explicit verify harness/tier was overwritten: harness %q tier %q", got[3].Harness, got[3].Tier)
	}

	// A CUSTOM prompt under the name "verify" is the operator's own phase: the
	// inheritance keys on the built-in brief, so it keeps the run-wide harness
	// and tier instead of silently inheriting the fix phase's.
	c.Phases[3].Harness = ""
	c.Phases[3].Tier = ""
	c.Phases[3].Prompt = "Verify whatever you like, your own way."
	got = c.EffectivePhases()
	if got[3].Harness != "claude" || got[3].Tier != "" {
		t.Errorf("a custom-prompt verify phase inherited the fix phase's knobs: harness %q tier %q, want the run-wide harness and no tier", got[3].Harness, got[3].Tier)
	}
}

// CLA-638: the verification round's one legal position is last, directly after
// the built-in fix, with a review before it. A sequence that cannot make progress
// between verification rounds is refused at validation rather than paid for.
func TestValidate_VerifyPhaseRules(t *testing.T) {
	tests := []struct {
		name   string
		phases []Phase
		want   string
	}{
		{
			name:   "verify must be last",
			phases: []Phase{{Name: ImplementPhaseName}, {Name: ReviewPhaseName}, {Name: FixPhaseName}, {Name: VerifyPhaseName}, {Name: FixPhaseName}},
			want:   VerifyPhaseName,
		},
		{
			name:   "verify must directly follow fix",
			phases: []Phase{{Name: ImplementPhaseName}, {Name: ReviewPhaseName}, {Name: VerifyPhaseName}},
			want:   FixPhaseName,
		},
		{
			name:   "verify needs a review whose findings it adjudicates",
			phases: []Phase{{Name: ImplementPhaseName}, {Name: FixPhaseName}, {Name: VerifyPhaseName}},
			want:   ReviewPhaseName,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := defaults()
			c.Prompt = ""
			c.Phases = tc.phases
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %+v", tc.phases)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the rejection does not name %q: %v", tc.want, err)
			}
		})
	}

	c := defaults()
	c.Prompt = ""
	c.Phases = []Phase{{Name: ImplementPhaseName}, {Name: ReviewPhaseName}, {Name: FixPhaseName}, {Name: VerifyPhaseName}}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate refused the shipped implement/review/fix/verify sequence: %v", err)
	}
}

// The verify brief is an interface: it must prescribe the by-id adjudication,
// the delta scope, the new-MUST limit, the no-open-MUST gate and the conditional
// terminal step. A paraphrase that drops any of these reopens the loop the
// stopping rule exists to close.
func TestVerifyBriefsStateTheStoppingRuleAndContract(t *testing.T) {
	// The core bar is shared by both variants: the gate is no open MUST, and
	// SHOULD/NIT never gate.
	for name, brief := range map[string]string{
		"verify":         verifyBrief,
		"no-code verify": noCodeVerifyBrief,
	} {
		for _, want := range []string{
			"THE STOPPING RULE IS A BAR, NOT A FEELING",
			"NO OPEN MUST",
		} {
			if !strings.Contains(brief, want) {
				t.Errorf("%s brief does not carry %q", name, want)
			}
		}
	}
	// The ID-based machinery is the CODE shape's: plane ids to adjudicate by,
	// the driver's round cap, and the non-convergence breaker.
	for _, want := range []string{
		"ADJUDICATE EVERY PRIOR OPEN MUST BY ITS PLANE ID",
		"RESOLVED",
		"REFUTATION ACCEPTED",
		"UNRESOLVED",
		"findingUpdates",
		"three non-void rounds",
		"takeover or reclaim does not",
		"fix-delta",
		"ONLY within the fix delta",
		`status: "in_review"`,
		"closeRound",
		"review_sealed",
		"do NOT open a PR",
		"{{reviewerRole}}",
	} {
		if !strings.Contains(verifyBrief, want) {
			t.Errorf("the code verify brief does not carry %q", want)
		}
	}
	// The no-code variant is BLOCK-based (CLA-670, question 03a12578 option 1):
	// no plane ids, no durable round to close, and its own terminal carries the
	// hand-over (clean) or the operator park (open MUST) — the verifier is the
	// LAST phase, so nothing after it can carry the move the review-only ending
	// defers.
	for _, want := range []string{
		"ADJUDICATE EVERY PRIOR OPEN MUST",
		"the BLOCK's own terms",
		"no durable record",
		"no plane finding ids",
		"the LAST",
		`status: "in_review"`,
		`status: "parked"`,
		"do NOT open a PR",
	} {
		if !strings.Contains(noCodeVerifyBrief, want) {
			t.Errorf("the no-code verify brief does not carry %q", want)
		}
	}
	for _, unwanted := range []string{
		"closeRound",
		"review_sealed",
		"findingUpdates",
		"The phase after you decides",
		"COMMIT and PUSH",
	} {
		if strings.Contains(noCodeVerifyBrief, unwanted) {
			t.Errorf("the no-code verify brief carries %q — the branchless shape writes no durable round and has no plane ids", unwanted)
		}
	}
}

// The no-code fix variant that a verifier follows holds the task; the verifier
// decides the hand-over.
func TestNoCodeFixVerifyBriefStopsBeforeTheHandover(t *testing.T) {
	if !strings.Contains(noCodeFixVerifyBrief, "Do NOT open a PR") {
		t.Errorf("the no-code fix-verify brief does not forbid the PR:\n%s", noCodeFixVerifyBrief)
	}
	if strings.Contains(noCodeFixVerifyBrief, "Open a PR targeting") {
		t.Errorf("the no-code fix-verify brief carries the terminal step:\n%s", noCodeFixVerifyBrief)
	}
	if !strings.Contains(noCodeFixVerifyBrief, "**Tests**") {
		t.Errorf("the no-code fix-verify brief does not write the outcome the in_review guard reads:\n%s", noCodeFixVerifyBrief)
	}
	if strings.Contains(noCodeFixVerifyBrief, "COMMIT and PUSH") {
		t.Errorf("the no-code fix-verify brief tells a branchless fix to commit and push:\n%s", noCodeFixVerifyBrief)
	}
}

// CLA-686 D1: a fix phase whose terminal step belongs to the VERIFICATION round
// must not also be told to hand the task over. All four of those variants ended
// with fixOnlyTerminalStep — "Do NOT open a PR and do NOT move the task to
// in_review", because the verifier carries that move — while still embedding
// fixTerminalRecord, which says "THEN hand the task over" and names the call
// that moves the task to in_review. The same brief told the fixer both to hand
// the task over and not to, and a fixer that obeyed the first half shipped
// fixes no verification round had seen. The final-phase fix briefs, which DO own
// the hand-over, must keep it: the property is one-sided, and a test that only
// banned the phrase would be satisfied by a brief that says nothing at all.
func TestFixVerifyVariantsDoNotHandTheTaskOver(t *testing.T) {
	// The imperative hand-over forms only a fix phase that OWNS the sequence's
	// terminal step may carry. Spelled out rather than referencing
	// fixTerminalRecord: this test has to fail — and still COMPILE — against the
	// briefs as they were before the split.
	handOver := []string{
		"THEN hand the task over",
		"the call that moves the task to in_review",
	}
	verifyOnly := []struct {
		name string
		// brief is the whole prompt or continuation the phase spawns with.
		brief string
		// terminal is the step that ends the phase still holding the task.
		terminal string
		// record is the record requirement it must carry instead of
		// fixTerminalRecord.
		record string
		// durable is whether the durable findingUpdates call belongs in this
		// variant: only the BRANCH-shaped ones have a durable record to update,
		// because the branchless round writes none (question 03a12578, option 1).
		durable bool
	}{
		{"the fix-with-verification brief", fixVerifyBrief, fixOnlyTerminalStep, fixVerifyRecord, true},
		{"the no-code fix-with-verification brief", noCodeFixVerifyBrief, noCodeFixOnlyTerminalStep, noCodeFixVerifyRecord, false},
		{"the fix-verify handoff", FixVerifyHandoffContinuation(), fixOnlyTerminalStep, fixVerifyRecord, true},
		{"the branchless fix-verify handoff", NoCodeFixVerifyHandoffContinuation(), noCodeFixOnlyTerminalStep, noCodeFixVerifyRecord, false},
	}
	for _, tc := range verifyOnly {
		for _, banned := range handOver {
			if strings.Contains(tc.brief, banned) {
				t.Errorf("%s says %q, but its terminal step leaves the hand-over to the verification round:\n%s", tc.name, banned, tc.brief)
			}
		}
		// Dropping the hand-over must not drop the rest of the contract: the
		// terminal step that ends the phase, and the record requirement the
		// successor's own outcome depends on.
		if !strings.Contains(tc.brief, tc.terminal) {
			t.Errorf("%s lost the terminal step that stops it before the hand-over:\n%s", tc.name, tc.brief)
		}
		if !strings.Contains(tc.brief, tc.record) {
			t.Errorf("%s lost its record requirement:\n%s", tc.name, tc.brief)
		}
		// The durable finding updates are the seam's evidence; a variant that
		// cannot make the call must not be told to (the same exemption
		// noCodeFixTerminalRecord carries for the branchless final fix).
		if tc.durable && !strings.Contains(tc.brief, "findingUpdates") {
			t.Errorf("%s lost the durable finding updates the fix phase owes:\n%s", tc.name, tc.brief)
		}
		if !tc.durable && strings.Contains(tc.brief, "findingUpdates") {
			t.Errorf("%s demands the durable findingUpdates call a branchless round cannot make:\n%s", tc.name, tc.brief)
		}
	}
	// The other side of the property: the fix phase that IS the sequence's last
	// still hands the task over.
	if !strings.Contains(fixBrief, "THEN hand the task over") {
		t.Errorf("the final fix brief lost the hand-over it owns:\n%s", fixBrief)
	}
	if !strings.Contains(noCodeFixBrief, "When you hand the task over") {
		t.Errorf("the branchless final fix brief lost the hand-over it owns:\n%s", noCodeFixBrief)
	}
}

// VerifyShaped is deliberately narrower than ReviewShaped: every verifier is a
// review-shaped record writer, but an ordinary review must not be mistaken for
// the stopping-rule gate.
func TestVerifyShaped(t *testing.T) {
	tests := []struct {
		name   string
		prompt string
		want   bool
	}{
		{"the verification brief", verifyBrief, true},
		{"the no-code verification brief", noCodeVerifyBrief, true},
		{"the review-only brief", reviewOnlyBrief, false},
		{"the fix-with-verification brief", fixVerifyBrief, false},
		{"an operator's custom prompt", "verify whatever you like", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := VerifyShaped(tc.prompt); got != tc.want {
				t.Errorf("VerifyShaped(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// CLA-638: a fix session that hands off while a verifier follows must carry the
// no-PR terminal step forward — otherwise the successor opens the PR the fix
// phase was told not to open, and the verifier gates a task already in_review.
func TestFixVerifyHandoffContinuationStopsBeforeThePR(t *testing.T) {
	got := FixVerifyHandoffContinuation()
	if !strings.Contains(got, fixOnlyTerminalStep) {
		t.Errorf("the fix-verify handoff does not carry the no-PR terminal step:\n%s", got)
	}
	if strings.Contains(got, "Open a PR targeting") || strings.Contains(got, `status: "in_review"`) {
		t.Errorf("the fix-verify handoff carries the verifier's hand-over:\n%s", got)
	}
	legacy := HandoffContinuationFor(FixPhaseName, false)
	if !strings.Contains(legacy, reviewTerminalStep) {
		t.Errorf("the legacy fix handoff lost its PR terminal step:\n%s", legacy)
	}

	// The BRANCHLESS variant: the same B12 boundary, with the no-code terminal
	// step. The legacy branchless fix continuation carries noCodeReviewTerminalStep,
	// which DOES move the task to in_review — the exact move the verifier owns.
	noCodeGot := NoCodeFixVerifyHandoffContinuation()
	if !strings.Contains(noCodeGot, noCodeFixOnlyTerminalStep) {
		t.Errorf("the branchless fix-verify handoff does not carry the no-code hold terminal step:\n%s", noCodeGot)
	}
	if strings.Contains(noCodeGot, `status: "in_review"`) || strings.Contains(noCodeGot, "no-code delivery declared") {
		t.Errorf("the branchless fix-verify handoff carries the no-code hand-over the verifier owns:\n%s", noCodeGot)
	}
	if !strings.Contains(noCodeGot, "RERUN BOUND") {
		t.Errorf("the branchless fix-verify handoff lost the rerun bound:\n%s", noCodeGot)
	}
	noCodeLegacy := NoCodeFixHandoffContinuation()
	if !strings.Contains(noCodeLegacy, noCodeReviewTerminalStep) {
		t.Errorf("the legacy branchless fix handoff lost its no-code hand-over:\n%s", noCodeLegacy)
	}
}
