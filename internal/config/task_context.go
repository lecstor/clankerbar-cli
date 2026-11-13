package config

import (
	"fmt"
	"strings"
)

// taskContextBlockMax bounds the context block a runner-claimed session starts
// with, in bytes. The block is mechanical plumbing, not the task: the plane is
// always the fuller record, so a block that would cost more than this is
// truncated and says where the rest lives.
const taskContextBlockMax = 8 * 1024

// taskContextTruncationNote replaces whatever was cut. It names the tool rather
// than the task because the session may not know the ref the runner gave it.
const taskContextTruncationNote = "(truncated — the full text is on the task: get_task)"

// TaskContext is everything the runner hands a claimed session up front, so the
// session does not spend its first turns on get_task, git plumbing and
// archaeology. The runner fills it; the session reads the rendered block and
// starts work.
//
// Every empty field is omitted from the block: a session must be able to tell
// "the runner attempted no merge" from "the merge produced nothing to say".
//
// Resumed means the branch already carried prior work, so the session
// re-validates the task against the merged tip before anything else. Merge is
// "merged", "up-to-date" or "" (not attempted). Conflicts are the paths the
// runner's merge of PRBase stopped on before the runner aborted it. MergeHint
// is the plane's own close-out instruction: the session must be able to follow
// it verbatim, so it is rendered as a section of its own and never shrunk.
// DiffBase is the commit a diff in this phase starts from — the runner's
// merge-base SHA, or origin/PRBase when no SHA was available — so a later phase
// (a maps-building review) can reuse the base the runner resolved.
// Questions and Decisions are newest-first except for the blocking questions,
// which the plane appends at the END. RunnerNext is one sentence naming what
// the runner does after this session ends.
//
// TaskID and RunID are the ids every task write must be signed with. A
// runner-claimed session never claimed, so these are the only place it learns
// them: the runner briefs name record_progress(taskId, runId) but carry no
// placeholder for either.
type TaskContext struct {
	Ref         string
	Title       string
	TaskID      string
	RunID       string
	Detail      string
	DoneWhen    string
	PhaseNumber int
	PhaseTotal  int
	PhaseName   string
	Worktree    string
	Branch      string
	PRBase      string
	DiffBase    string
	Tip         string
	Resumed     bool
	Merge       string
	Conflicts   []string
	CloseOut    bool
	MergeHint   string
	Decisions   []struct{ Question, Answer string }
	Questions   []struct {
		Body, Answer string
		Blocking     bool
	}
	RunnerNext string
}

// TaskContextBlock renders the block above the brief. It is deterministic (no
// map iteration, input order preserved) and never exceeds taskContextBlockMax
// bytes. Decisions and questions arrive newest-first, so the oldest (the tail)
// go first: non-blocking questions, then decisions, then blocking questions,
// and only then is the detail truncated; the truncation note says where the
// full text is. Blocking questions go last because the plane appends older
// live blockers at the END of the list and one of them is the reason the task
// stopped - trimming the tail blindly would drop exactly that. The close-out
// instruction is header-like and is never shrunk: dropping the plane's own
// instruction would leave the session inventing one.
func TaskContextBlock(c TaskContext) string {
	if block := renderTaskContext(c, false); len(block) <= taskContextBlockMax {
		return block
	}
	shrunk := c
	for len(shrunk.Questions) > 0 && !taskContextFits(shrunk) {
		questions, dropped := dropTrailingQuestion(shrunk.Questions, false)
		if !dropped {
			break
		}
		shrunk.Questions = questions
	}
	for len(shrunk.Decisions) > 0 && !taskContextFits(shrunk) {
		shrunk.Decisions = shrunk.Decisions[:len(shrunk.Decisions)-1]
	}
	for len(shrunk.Questions) > 0 && !taskContextFits(shrunk) {
		questions, dropped := dropTrailingQuestion(shrunk.Questions, true)
		if !dropped {
			break
		}
		shrunk.Questions = questions
	}
	if !taskContextFits(shrunk) {
		shrunk.Detail = truncateTaskContextDetail(shrunk)
	}
	block := renderTaskContext(shrunk, true)
	if len(block) <= taskContextBlockMax {
		return block
	}
	// Only a header field (a long ref, title, runner-next sentence or close-out
	// instruction) can still be over the cap here, and those are not the
	// runner's to drop silently.
	return clampTaskContextBlock(block)
}

// taskContextFits judges the TRUNCATED render, because that is the block the
// shrinkage is buying: the note is not optional once anything was cut.
func taskContextFits(c TaskContext) bool {
	return len(renderTaskContext(c, true)) <= taskContextBlockMax
}

// dropTrailingQuestion removes the LAST question of the given class (blocking
// or not), preserving the order of the rest, and reports whether it removed
// one. It copies rather than reslices in place: callers re-render the same
// input, and an in-place append would reorder the caller's own list.
func dropTrailingQuestion(
	questions []struct {
		Body, Answer string
		Blocking     bool
	},
	blocking bool,
) ([]struct {
	Body, Answer string
	Blocking     bool
}, bool) {
	for i := len(questions) - 1; i >= 0; i-- {
		if questions[i].Blocking != blocking {
			continue
		}
		out := make([]struct {
			Body, Answer string
			Blocking     bool
		}, 0, len(questions)-1)
		out = append(out, questions[:i]...)
		out = append(out, questions[i+1:]...)
		return out, true
	}
	return questions, false
}

// truncateTaskContextDetail returns the longest rune-boundary prefix of the
// detail for which the block with everything else unchanged is inside the cap.
func truncateTaskContextDetail(c TaskContext) string {
	runes := []rune(c.Detail)
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		probe := c
		probe.Detail = string(runes[:mid])
		if taskContextFits(probe) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return string(runes[:lo])
}

// clampTaskContextBlock is the last resort: cut the rendered block on a rune
// boundary rather than hand a session a block over the cap.
func clampTaskContextBlock(block string) string {
	if len(block) <= taskContextBlockMax {
		return block
	}
	runes := []rune(block)
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if len(string(runes[:mid])) <= taskContextBlockMax {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return string(runes[:lo])
}

func renderTaskContext(c TaskContext, truncated bool) string {
	var parts []string
	if header := taskContextHeader(c); header != "" {
		parts = append(parts, header)
	}
	if len(c.Conflicts) > 0 {
		parts = append(parts, taskContextConflicts(c))
	}
	// The plane's close-out instruction sits above the fields the shrink logic
	// may cut: the session follows it verbatim.
	if hint := strings.TrimSpace(c.MergeHint); hint != "" {
		parts = append(parts, "## Close-out instruction\n"+hint)
	}
	for _, section := range []struct{ heading, body string }{
		{"Detail", c.Detail},
		{"Done when", c.DoneWhen},
		{"Standing decisions", taskContextDecisions(c.Decisions)},
		{"Answered questions", taskContextQuestions(c.Questions)},
		{"Runner next", strings.TrimSpace(c.RunnerNext)},
	} {
		if strings.TrimSpace(section.body) == "" {
			continue
		}
		parts = append(parts, "## "+section.heading+"\n"+strings.TrimSpace(section.body))
	}
	if truncated {
		parts = append(parts, taskContextTruncationNote)
	}
	return strings.Join(parts, "\n\n")
}

func taskContextHeader(c TaskContext) string {
	var lines []string
	ref, title := strings.TrimSpace(c.Ref), strings.TrimSpace(c.Title)
	switch {
	case ref != "" && title != "":
		lines = append(lines, "TASK "+ref+" — "+title)
	case ref != "":
		lines = append(lines, "TASK "+ref)
	case title != "":
		lines = append(lines, "TASK — "+title)
	}
	if phase := taskContextPhase(c); phase != "" {
		lines = append(lines, phase)
	}
	// The ids come right after the phase so they are read before the work, and
	// the run line states what to do with them: a session that never claimed
	// has nowhere else to learn them.
	if id := strings.TrimSpace(c.TaskID); id != "" {
		lines = append(lines, "Task id: "+id)
	}
	if id := strings.TrimSpace(c.RunID); id != "" {
		lines = append(lines, "Run id: "+id+" (sign every task write - record_progress, record_review, settle_task, release_task - with this taskId and runId)")
	}
	for _, field := range []struct{ label, value string }{
		{"Worktree", c.Worktree},
		{"Branch", c.Branch},
		{"PR base", c.PRBase},
		{"Diff base", c.DiffBase},
		{"Tip", c.Tip},
	} {
		if value := strings.TrimSpace(field.value); value != "" {
			lines = append(lines, field.label+": "+value)
		}
	}
	if c.Resumed {
		lines = append(lines, "Resumed: the branch already carried prior work.")
	}
	if merge := taskContextMerge(c); merge != "" {
		lines = append(lines, "Merge: "+merge)
	}
	if c.CloseOut {
		lines = append(lines, "Close-out: the task is approved and awaiting merge.")
	}
	return strings.Join(lines, "\n")
}

func taskContextPhase(c TaskContext) string {
	name := strings.TrimSpace(c.PhaseName)
	switch {
	case c.PhaseNumber > 0 && c.PhaseTotal > 0 && name != "":
		return fmt.Sprintf("Phase: %d of %d (%s)", c.PhaseNumber, c.PhaseTotal, name)
	case c.PhaseNumber > 0 && c.PhaseTotal > 0:
		return fmt.Sprintf("Phase: %d of %d", c.PhaseNumber, c.PhaseTotal)
	case name != "":
		return "Phase: " + name
	}
	return ""
}

func taskContextMerge(c TaskContext) string {
	base, branch := strings.TrimSpace(c.PRBase), strings.TrimSpace(c.Branch)
	switch strings.TrimSpace(c.Merge) {
	case "":
		return ""
	case "merged":
		if base != "" && branch != "" {
			return base + " merged into " + branch
		}
		return "merged"
	case "up-to-date":
		if base != "" && branch != "" {
			return base + " already up to date in " + branch
		}
		return "up to date"
	}
	return strings.TrimSpace(c.Merge)
}

// taskContextConflicts states the aborted merge and the session's first job.
// The runner never asks a session to resolve a conflict it did not create, so
// the redo command is spelled out rather than left to be re-derived.
func taskContextConflicts(c TaskContext) string {
	base, branch := strings.TrimSpace(c.PRBase), strings.TrimSpace(c.Branch)
	if base == "" {
		base = "the PR base branch"
	}
	var lines []string
	if branch != "" {
		lines = append(lines, fmt.Sprintf("The runner merged %s into %s and the merge stopped with conflicts in:", base, branch))
	} else {
		lines = append(lines, fmt.Sprintf("The runner merged %s in and the merge stopped with conflicts in:", base))
	}
	for _, path := range c.Conflicts {
		if path = strings.TrimSpace(path); path != "" {
			lines = append(lines, "- "+path)
		}
	}
	redo := "Your FIRST job is to redo that merge"
	if prBase := strings.TrimSpace(c.PRBase); prBase != "" {
		redo += " (`git merge origin/" + prBase + "`)"
	}
	redo += ", resolve them, and commit the result - never rebase. The rest of this brief applies from that commit."
	lines = append(lines, "", "The runner aborted it. "+redo)
	return "## Merge conflicts\n" + strings.Join(lines, "\n")
}

func taskContextDecisions(items []struct{ Question, Answer string }) string {
	var lines []string
	for _, item := range items {
		if line := taskContextPair(item.Question, item.Answer); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// taskContextQuestions renders the answered questions, marking a blocker so the
// session can tell the reason the task stopped from ordinary Q&A.
func taskContextQuestions(items []struct {
	Body, Answer string
	Blocking     bool
}) string {
	var lines []string
	for _, item := range items {
		body := strings.TrimSpace(item.Body)
		if item.Blocking && body != "" {
			body = "(blocker) " + body
		}
		if line := taskContextPair(body, item.Answer); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// taskContextPair renders one question/answer bullet, keeping a half-answered
// item readable rather than dropping it.
func taskContextPair(question, answer string) string {
	question, answer = strings.TrimSpace(question), strings.TrimSpace(answer)
	switch {
	case question == "" && answer == "":
		return ""
	case question == "":
		return "- " + answer
	case answer == "":
		return "- " + question
	}
	return "- " + question + " — " + answer
}
