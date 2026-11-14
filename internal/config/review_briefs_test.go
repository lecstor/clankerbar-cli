package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLA-702 review 1.review-3.1: the doneWhen requires the verified panel member
// brief and the single reviewer's two briefs to be UNCHANGED, and a seam check
// cannot establish that — an edit anywhere else in an eleven-kilobyte brief
// would pass it. The goldens are those briefs' fully resolved text as CLA-702
// pinned it; any unintended change anywhere in them fails here.
//
// A deliberate change to a shared fragment these briefs embed (the gate scope,
// the record shape, the handoff guidance, or CLA-865's {{baseSha}} diff line)
// means updating the matching goldens by hand — that friction is the point,
// because it is the only way an edit elsewhere in the brief cannot slip through.
// The panel member's UNVERIFIED brief is deliberately not pinned: CLA-702
// rewrites exactly that one.
func TestReviewBriefsUnchangedByTheUnverifiedSplit(t *testing.T) {
	for _, tc := range []struct{ name, brief, golden string }{
		{"the single reviewer's verified brief", reviewOnlyBrief, "single-reviewer-verified.txt"},
		{"the verified panel member brief", panelMemberReviewOnlyBrief, "panel-member-verified.txt"},
		{"the single reviewer's unverified brief", unverifiedReviewOnlyBrief, "single-reviewer-unverified.txt"},
	} {
		want, err := os.ReadFile(filepath.Join("testdata", "review-briefs", tc.golden))
		if err != nil {
			t.Fatalf("read the pre-split golden %s for %s: %v", tc.golden, tc.name, err)
		}
		// A file needs its closing newline; the brief itself does not carry one.
		wantBrief := strings.TrimSuffix(string(want), "\n")
		if tc.brief == wantBrief {
			continue
		}
		t.Errorf("%s changed from its pre-split golden (%s): %s",
			tc.name, tc.golden, firstBriefDifference(wantBrief, tc.brief))
	}
}

// firstBriefDifference reports where a brief first departs from its golden, so
// a whole-brief mismatch yields one sentence instead of two eleven-kilobyte
// texts. Runes, not bytes: the briefs carry em dashes.
func firstBriefDifference(want, got string) string {
	w, g := []rune(want), []rune(got)
	i := 0
	for i < len(w) && i < len(g) && w[i] == g[i] {
		i++
	}
	if i == len(w) && i == len(g) {
		return "no difference"
	}
	window := func(rs []rune) string {
		from, to := i-60, i+60
		if from < 0 {
			from = 0
		}
		if to > len(rs) {
			to = len(rs)
		}
		if from > len(rs) {
			from = len(rs)
		}
		return string(rs[from:to])
	}
	return fmt.Sprintf("first difference at rune %d: want …%s…, got …%s…", i, window(w), window(g))
}

// CLA-536: the session that takes the work off the machine reclaims the tree it
// worked in. Nobody else will - the close-out that merges is forbidden to touch
// a tree it did not create - so every FINAL phase (review, fix, verify) and both
// handoff continuations that can end a sequence must carry it, and every phase
// whose successor still works in the tree must not.
func TestFinalPhaseBriefsReclaimTheWorktreeAfterThePR(t *testing.T) {
	for _, want := range []string{
		"AFTER the PR is confirmed open",
		"git worktree remove",
		"git branch -d " + PhaseBranchPlaceholder,
		"git push -u origin " + PhaseBranchPlaceholder,
		"Never pass `--force` or `-D`",
		"this run's own tree",
		"never another task's worktree",
	} {
		if !strings.Contains(worktreeReclaimStep, want) {
			t.Errorf("worktreeReclaimStep does not say %q, so a final phase session leaves its tree behind:\n%s", want, worktreeReclaimStep)
		}
	}

	// Order: push -> PR -> remove -> branch delete -> the phase's final call. A
	// reclaim named before the PR could delete the only copy of work the PR
	// exists to publish.
	for _, tc := range []struct{ name, brief, push, pr, finalCall string }{
		{"review", builtinPhasePrompts[ReviewPhaseName], "COMMIT and PUSH", "Open a PR targeting",
			"submit_for_review(taskId, runId, outcome:"},
		{"fix", builtinPhasePrompts[FixPhaseName], "COMMIT and PUSH", "Open a PR targeting",
			"submit_for_review(taskId, runId, outcome:"},
		{"verify", builtinPhasePrompts[VerifyPhaseName], "confirm the branch is pushed", "open a PR targeting",
			"submit_for_review(taskId, runId, closeRound:"},
	} {
		assertReclaimSequence(t, tc.name, tc.brief, tc.push, tc.pr, tc.finalCall)
	}

	// A handoff replaces the brief with the predecessor's own prompt, so the
	// continuation is the only thing that carries the reclaim to a successor.
	for name, cont := range map[string]string{
		"review": HandoffContinuation(ReviewPhaseName),
		"fix":    HandoffContinuation(FixPhaseName),
	} {
		if !strings.Contains(cont, worktreeReclaimStep) {
			t.Errorf("the %s handoff continuation does not carry the reclaim step forward:\n%s", name, cont)
		}
	}

	// The phases that still need the tree must never reclaim it: implement ends
	// at a checkpoint with the review phase working in the same tree, and the
	// review-only and fix-verify shapes hand the tree to a phase after them.
	for name, text := range map[string]string{
		"implement brief":          builtinPhasePrompts[ImplementPhaseName],
		"implement continuation":   HandoffContinuation(ImplementPhaseName),
		"review-only brief":        reviewOnlyBrief,
		"review-only continuation": HandoffContinuationFor(ReviewPhaseName, true),
		"fix-verify brief":         fixVerifyBrief,
		"fix-verify continuation":  FixVerifyHandoffContinuation(),
	} {
		if strings.Contains(text, worktreeReclaimStep) {
			t.Errorf("the %s reclaims a tree a later phase still works in:\n%s", name, text)
		}
	}
}

// assertReclaimSequence pins the reclaim to its position in a final phase's
// terminal block: push, then the PR, then the removal.
func assertReclaimSequence(t *testing.T, name, brief, push, pr, finalCall string) {
	t.Helper()
	if !strings.Contains(brief, worktreeReclaimStep) {
		t.Errorf("the %s brief does not carry worktreeReclaimStep:\n%s", name, brief)
		return
	}
	idx := func(marker string) int {
		i := strings.Index(brief, marker)
		if i == -1 {
			t.Fatalf("the %s brief never says %q:\n%s", name, marker, brief)
		}
		return i
	}
	pushIdx, prIdx := idx(push), idx(pr)
	removeIdx, branchIdx := idx("git worktree remove"), idx("git branch -d ")
	finalIdx := idx(finalCall)
	if !(pushIdx < prIdx && prIdx < removeIdx && removeIdx < branchIdx && branchIdx < finalIdx) {
		t.Errorf("the %s terminal sequence is not push (%d) -> PR (%d) -> worktree remove (%d) -> branch delete (%d) -> final call (%d):\n%s",
			name, pushIdx, prIdx, removeIdx, branchIdx, finalIdx, brief)
	}
}
