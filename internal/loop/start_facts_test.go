package loop

// start_facts_test.go — readOriginBranch over REAL repositories: the bare
// origin plus clone fixture from task_worktree_test.go, so the fetch, the
// rev-parse and the ancestry check all run against git.

import (
	"context"
	"testing"
)

func TestReadOriginBranch(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	// Two commits on the task branch, so the older one is an ancestor of the tip
	// while another branch's commit is in neither's history.
	oldTip := f.commitOn(t, taskWorktreeBranch, "origin/staging", "a.txt", "one", "one")
	tip := f.commitOn(t, taskWorktreeBranch, "", "a.txt", "two", "two")
	otherTip := f.commitOn(t, "unrelated", "origin/staging", "b.txt", "other", "other")

	cases := []struct {
		name             string
		checkout, branch string
		doneTip          string
		want             originBranchFacts
	}{
		{
			name: "the fetch fails", checkout: t.TempDir(), branch: taskWorktreeBranch,
		},
		{
			name: "the branch is not on origin", checkout: f.checkout, branch: "clanker/never-pushed",
			want: originBranchFacts{Known: true},
		},
		{
			name: "the branch is on origin", checkout: f.checkout, branch: taskWorktreeBranch,
			want: originBranchFacts{Known: true, OnOrigin: true, Tip: tip},
		},
		{
			name: "the done tip is origin's tip", checkout: f.checkout, branch: taskWorktreeBranch, doneTip: tip,
			want: originBranchFacts{Known: true, OnOrigin: true, Tip: tip, DoneIsAncestor: true},
		},
		{
			name: "the done tip is an older commit on the branch", checkout: f.checkout, branch: taskWorktreeBranch, doneTip: oldTip,
			want: originBranchFacts{Known: true, OnOrigin: true, Tip: tip, DoneIsAncestor: true},
		},
		{
			name: "the done tip is not in the branch's history", checkout: f.checkout, branch: taskWorktreeBranch, doneTip: otherTip,
			want: originBranchFacts{Known: true, OnOrigin: true, Tip: tip},
		},
		{
			// A well-formed SHA git cannot resolve is not the answer "not an
			// ancestor": nothing about this branch is known.
			name: "the done tip does not exist", checkout: f.checkout, branch: taskWorktreeBranch, doneTip: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			want: originBranchFacts{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := readOriginBranch(context.Background(), tc.checkout, tc.branch, tc.doneTip)
			if got != tc.want {
				t.Fatalf("readOriginBranch = %+v, want %+v", got, tc.want)
			}
		})
	}
}
