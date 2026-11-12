package config

import (
	"fmt"
	"strings"
	"testing"
)

// testQuestion is TaskContext.Questions' own element type (an anonymous struct)
// under a name, so a literal here assigns without a conversion.
type testQuestion = struct {
	Body, Answer string
	Blocking     bool
}

func question(body, answer string, blocking bool) testQuestion {
	return testQuestion{Body: body, Answer: answer, Blocking: blocking}
}

func fullTaskContext() TaskContext {
	return TaskContext{Ref: "CLA-1", Title: "Fix the thing", Detail: "The thing is broken.", DoneWhen: "The thing works.", PhaseNumber: 2, PhaseTotal: 4, PhaseName: "review", Worktree: "/tmp/wt/cla-1", Branch: "cla-1", PRBase: "staging", Tip: "deadbeef", Resumed: true, Merge: "merged", Decisions: []struct{ Question, Answer string }{{Question: "Which base?", Answer: "staging"}}, Questions: []testQuestion{question("Delete the old flag?", "yes", false)}, RunnerNext: "The runner opens the PR and waits for CI."}
}

func TestTaskContextBlockRendersTheFullContext(t *testing.T) {
	block := TaskContextBlock(fullTaskContext())
	for _, want := range []string{
		"TASK CLA-1 — Fix the thing",
		"Phase: 2 of 4 (review)",
		"Worktree: /tmp/wt/cla-1",
		"Branch: cla-1",
		"PR base: staging",
		"Tip: deadbeef",
		"Resumed: the branch already carried prior work.",
		"Merge: staging merged into cla-1",
		"## Detail\nThe thing is broken.",
		"## Done when\nThe thing works.",
		"## Standing decisions\n- Which base? — staging",
		"## Answered questions\n- Delete the old flag? — yes",
		"## Runner next\nThe runner opens the PR and waits for CI.",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the context block never says %q:\n%s", want, block)
		}
	}
	if strings.Contains(block, taskContextTruncationNote) {
		t.Errorf("the block was truncated without needing to be:\n%s", block)
	}
}

func TestTaskContextBlockOmitsEmptySections(t *testing.T) {
	// A minimal context must render as its header alone: a heading with nothing
	// under it reads as "the runner had something to say and lost it".
	block := TaskContextBlock(TaskContext{Ref: "CLA-2", Title: "Only a title"})
	if want := "TASK CLA-2 — Only a title"; block != want {
		t.Fatalf("minimal block = %q, want %q", block, want)
	}
	for _, absent := range []string{"## ", "Phase:", "Worktree:", "Branch:", "PR base:", "Tip:", "Resumed:", "Merge:", "Close-out:"} {
		if strings.Contains(block, absent) {
			t.Errorf("an empty context rendered %q:\n%s", absent, block)
		}
	}
}

func TestTaskContextBlockStatesTheAbortedMerge(t *testing.T) {
	block := TaskContextBlock(TaskContext{Ref: "CLA-3", Branch: "cla-3", PRBase: "staging", Merge: "merged", Conflicts: []string{"internal/config/phases.go", "docs/runner-owned-mechanics.md"}})
	for _, want := range []string{
		"The runner merged staging into cla-3 and the merge stopped with conflicts in:",
		"- internal/config/phases.go",
		"- docs/runner-owned-mechanics.md",
		"The runner aborted it.",
		"Your FIRST job is to redo that merge",
		"`git merge origin/staging`",
		"resolve them, and commit the result",
		"never rebase",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the conflicts block never says %q:\n%s", want, block)
		}
	}
}

func TestTaskContextBlockMarksACloseOut(t *testing.T) {
	block := TaskContextBlock(TaskContext{Ref: "CLA-4", Title: "Ship it", Branch: "cla-4", PRBase: "staging", CloseOut: true})
	if !strings.Contains(block, "Close-out: the task is approved and awaiting merge.") {
		t.Errorf("a close-out context does not say so:\n%s", block)
	}
}

// The plane's close-out instruction is the session's to follow verbatim, so it
// is a section of its own above the detail - and the shrink logic below only
// cuts the detail and the two lists, never this.
func TestTaskContextBlockRendersTheCloseOutInstructionFirst(t *testing.T) {
	hint := "Wait for the PR's checks to finish, repair a red one on the branch, then call release_task(checkGreen)."
	c := fullTaskContext()
	c.MergeHint = hint
	block := TaskContextBlock(c)
	section := "## Close-out instruction\n" + hint
	hintAt, detailAt := strings.Index(block, section), strings.Index(block, "## Detail")
	if hintAt < 0 {
		t.Fatalf("the close-out instruction is not rendered as its own section:\n%s", block)
	}
	if detailAt < 0 || hintAt > detailAt {
		t.Errorf("the close-out instruction is not before the detail:\n%s", block)
	}

	// A huge detail is the thing that must give: the instruction survives.
	c.Detail = strings.Repeat("detail ", 4000)
	c.Decisions, c.Questions = nil, nil
	block = TaskContextBlock(c)
	if len(block) > taskContextBlockMax {
		t.Fatalf("block is %d bytes, over the %d cap", len(block), taskContextBlockMax)
	}
	if !strings.Contains(block, section) {
		t.Errorf("truncating the detail lost the close-out instruction:\n%s", block)
	}
}

func TestTaskContextBlockTruncatesTheDetailFirst(t *testing.T) {
	c := fullTaskContext()
	c.Detail = strings.Repeat("detail ", 4000)
	c.Decisions = nil
	c.Questions = nil
	block := TaskContextBlock(c)
	if len(block) > taskContextBlockMax {
		t.Fatalf("block is %d bytes, over the %d cap", len(block), taskContextBlockMax)
	}
	if !strings.Contains(block, taskContextTruncationNote) {
		t.Errorf("a truncated block does not say where the full text is:\n%s", block)
	}
	// Only the detail is cut: everything else in the block survives.
	for _, want := range []string{"TASK CLA-1 — Fix the thing", "## Done when", "The runner opens the PR and waits for CI."} {
		if !strings.Contains(block, want) {
			t.Errorf("truncating the detail lost %q:\n%s", want, block)
		}
	}
	if strings.Count(block, "detail") >= 4000 {
		t.Errorf("the detail was not truncated:\n%s", block)
	}
}

// The plane serves decisions and questions newest-first: index 0 is the newest.
func newestFirstDecisions(n int) []struct{ Question, Answer string } {
	out := make([]struct{ Question, Answer string }, 0, n)
	for i := n - 1; i >= 0; i-- {
		out = append(out, struct{ Question, Answer string }{Question: fmt.Sprintf("decision-%03d", i), Answer: strings.Repeat("a", 100)})
	}
	return out
}

func newestFirstQuestions(n int) []testQuestion {
	out := make([]testQuestion, 0, n)
	for i := n - 1; i >= 0; i-- {
		out = append(out, question(fmt.Sprintf("question-%03d", i), strings.Repeat("b", 100), false))
	}
	return out
}

// blockingTailQuestion is the shape the plane serves a rideback in: an older
// live answered BLOCKER appended at the END of the newest-first list.
func blockingTailQuestion() testQuestion {
	return question("blocker-in-the-tail", "answered", true)
}

// detailLeavingSlack sizes the detail so a block carrying everything c already
// holds renders exactly `slack` bytes under the cap. With the slack below one
// bullet's width, every droppable entry must go while c's own entries and the
// detail stay - which is what pins the shrink order.
func detailLeavingSlack(t *testing.T, c TaskContext, slack int) string {
	t.Helper()
	c.Detail = "d"
	n := taskContextBlockMax - len(renderTaskContext(c, true)) - slack - 1
	if n <= 0 {
		t.Fatalf("the fixed parts of the block leave no room for a detail (n = %d)", n)
	}
	return strings.Repeat("d", n)
}

func TestTaskContextBlockDropsTheOldestListEntries(t *testing.T) {
	decisions := newestFirstDecisions(300)
	questions := newestFirstQuestions(300)
	for _, tc := range []struct {
		name, oldest, newest string
		c                    TaskContext
	}{
		{"decisions", "decision-000", "decision-299", TaskContext{Ref: "CLA-6", Detail: strings.Repeat("d", 4000), Decisions: decisions}},
		{"questions", "question-000", "question-299", TaskContext{Ref: "CLA-6", Detail: strings.Repeat("d", 4000), Questions: questions}},
	} {
		if !strings.Contains(TaskContextBlock(tc.c), strings.Repeat("d", 4000)) {
			t.Errorf("%s: the detail was truncated while list entries could still be dropped", tc.name)
		}
		block := TaskContextBlock(tc.c)
		if len(block) > taskContextBlockMax {
			t.Fatalf("%s: block is %d bytes, over the %d cap", tc.name, len(block), taskContextBlockMax)
		}
		if !strings.Contains(block, taskContextTruncationNote) {
			t.Errorf("%s: a truncated block does not say where the full text is:\n%s", tc.name, block)
		}
		if strings.Contains(block, tc.oldest) {
			t.Errorf("%s: the oldest entry survived while newer ones were dropped:\n%s", tc.name, block)
		}
		if !strings.Contains(block, tc.newest) {
			t.Errorf("%s: the newest entry was not kept:\n%s", tc.name, block)
		}
	}
}

// Pins the shrink order: NON-blocking questions go first, then decisions, then
// blocking questions, and the detail is only truncated once the lists are
// exhausted.
func TestTaskContextBlockShrinkOrder(t *testing.T) {
	detail := strings.Repeat("d", 3000)
	c := TaskContext{Ref: "CLA-6", Detail: detail, Decisions: newestFirstDecisions(30), Questions: newestFirstQuestions(300)}
	block := TaskContextBlock(c)
	if len(block) > taskContextBlockMax {
		t.Fatalf("block is %d bytes, over the %d cap", len(block), taskContextBlockMax)
	}
	if !strings.Contains(block, "decision-000") || !strings.Contains(block, "decision-029") {
		t.Errorf("a decision was dropped before the non-blocking questions were exhausted:\n%s", block)
	}
	if !strings.Contains(block, detail) {
		t.Errorf("the detail was truncated before the lists were exhausted")
	}
	if strings.Contains(block, "question-000") {
		t.Errorf("the oldest question survived while newer ones were dropped")
	}
	// A blocker at the tail outlives every decision: with the fixed parts sized
	// to the cap, the decisions go and the blocker stays.
	tailOnly := TaskContext{Ref: "CLA-6", Questions: []testQuestion{blockingTailQuestion()}}
	blockerDetail := detailLeavingSlack(t, tailOnly, 100)
	c = TaskContext{Ref: "CLA-6", Detail: blockerDetail, Decisions: newestFirstDecisions(300),
		Questions: []testQuestion{blockingTailQuestion()}}
	block = TaskContextBlock(c)
	if len(block) > taskContextBlockMax {
		t.Fatalf("block is %d bytes, over the %d cap", len(block), taskContextBlockMax)
	}
	if !strings.Contains(block, "blocker-in-the-tail") {
		t.Errorf("the blocking question at the tail was dropped while decisions could still go:\n%s", block)
	}
	if strings.Contains(block, "decision-") {
		t.Errorf("a decision outlived the blocking question at the tail:\n%s", block)
	}
	if !strings.Contains(block, blockerDetail) {
		t.Errorf("the detail was truncated before the blocking question was reached")
	}

	c = TaskContext{Ref: "CLA-6", Detail: strings.Repeat("e", 10000), Decisions: newestFirstDecisions(5), Questions: newestFirstQuestions(5)}
	block = TaskContextBlock(c)
	if len(block) > taskContextBlockMax {
		t.Fatalf("block is %d bytes, over the %d cap", len(block), taskContextBlockMax)
	}
	if strings.Contains(block, "decision-") || strings.Contains(block, "question-") {
		t.Errorf("list entries survived while the detail alone was over the cap")
	}
	if !strings.HasSuffix(block, taskContextTruncationNote) {
		t.Errorf("the truncated block does not end with the truncation note")
	}
}

// The plane appends older live answered BLOCKERS at the END of the newest-first
// list - a rideback is the reason the task stopped - so a blocking question at
// the tail must outlive every non-blocking question, not just the newest ones.
func TestTaskContextBlockKeepsATailBlockerOverEveryQuestion(t *testing.T) {
	probe := TaskContext{Ref: "CLA-6", Questions: []testQuestion{blockingTailQuestion()}}
	c := probe
	c.Detail = detailLeavingSlack(t, probe, 100)
	c.Questions = append(newestFirstQuestions(300), blockingTailQuestion())
	block := TaskContextBlock(c)
	if len(block) > taskContextBlockMax {
		t.Fatalf("block is %d bytes, over the %d cap", len(block), taskContextBlockMax)
	}
	if !strings.Contains(block, "blocker-in-the-tail") {
		t.Errorf("the blocking question at the tail was dropped:\n%s", block)
	}
	if strings.Contains(block, "question-") {
		t.Errorf("a non-blocking question outlived the tail blocker:\n%s", block)
	}
	if !strings.Contains(block, c.Detail) {
		t.Errorf("the detail was truncated while the tail blocker still had to be kept")
	}
}

// A blocking question is marked, so a session reads it as the reason the task
// stopped rather than as ordinary Q&A.
func TestTaskContextBlockMarksABlockingQuestion(t *testing.T) {
	block := TaskContextBlock(TaskContext{Ref: "CLA-8", Questions: []testQuestion{
		blockingTailQuestion(),
		question("Delete the old flag?", "yes", false),
	}})
	for _, want := range []string{
		"## Answered questions\n- (blocker) blocker-in-the-tail — answered",
		"- Delete the old flag? — yes",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the answered-questions section does not say %q:\n%s", want, block)
		}
	}
}

func TestTaskContextBlockCountsTheTruncationNoteAgainstTheCap(t *testing.T) {
	// The note itself counts against the cap: a block truncated to exactly the
	// cap with the note appended would be over it.
	c := TaskContext{Ref: "CLA-7", Detail: strings.Repeat("d", taskContextBlockMax)}
	block := TaskContextBlock(c)
	if len(block) > taskContextBlockMax {
		t.Fatalf("block is %d bytes, over the %d cap", len(block), taskContextBlockMax)
	}
	if !strings.HasSuffix(block, taskContextTruncationNote) {
		t.Errorf("the truncated block does not end with the truncation note:\n%s", block)
	}
}

func TestTaskContextBlockIsDeterministic(t *testing.T) {
	c := fullTaskContext()
	first := TaskContextBlock(c)
	for i := 0; i < 5; i++ {
		if got := TaskContextBlock(c); got != first {
			t.Fatalf("run %d rendered a different block:\nwant %q\ngot  %q", i, first, got)
		}
	}
}
