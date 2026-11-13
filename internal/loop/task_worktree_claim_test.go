package loop

// task_worktree_claim_test.go — the pre-claim verdict: it must decide a
// deterministic refusal WITHOUT touching the tree or claiming the task.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// No tree exists, so the create path is feasible once origin's PR base is
// proven: the check fetches and returns the facts the prep reuses.
func TestClaimWorktreeRefusal_NoWorktreeIsFeasible(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	req := f.request()

	reason, facts, err := claimWorktreeRefusal(context.Background(), req, "")
	if err != nil {
		t.Fatalf("claimWorktreeRefusal: %v", err)
	}
	if reason != "" {
		t.Errorf("reason = %q, want none: the create path's preconditions hold", reason)
	}
	if !facts.Known || facts.OnOrigin {
		t.Errorf("facts = %+v, want a fetched read of a branch origin does not have", facts)
	}
}

func TestClaimWorktreeRefusal_TrackedDirtyRefused(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	first, err := f.prepare(t, f.request())
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	stray := filepath.Join(first.Dir, "stray.txt")
	writeTestFile(t, filepath.Join(first.Dir, "a.txt"), "edited")
	writeTestFile(t, stray, "leftover")

	reason, _, err := claimWorktreeRefusal(context.Background(), f.request(), "")
	if err != nil {
		t.Fatalf("claimWorktreeRefusal: %v", err)
	}
	if !strings.Contains(reason, "uncommitted changes") {
		t.Errorf("reason = %q, want it to name the tracked changes", reason)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Errorf("the check moved or deleted %s: %v", stray, err)
	}
	if body, err := os.ReadFile(filepath.Join(first.Dir, "a.txt")); err != nil || strings.TrimSpace(string(body)) != "edited" {
		t.Errorf("the check touched the tracked change: %q (%v)", body, err)
	}
}

func TestClaimWorktreeRefusal_UntrackedScratchIsFeasible(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	branchTip := f.commitOn(t, taskWorktreeBranch, "origin/staging", "branch.txt", "branch work", "the work")
	req := f.request()
	req.Resume = true
	first, err := f.prepare(t, req)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	scratch := filepath.Join(first.Dir, ".scratch", "notes.txt")
	if err := os.MkdirAll(filepath.Dir(scratch), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, scratch, "reference")

	reason, facts, err := claimWorktreeRefusal(context.Background(), req, "")
	if err != nil {
		t.Fatalf("claimWorktreeRefusal: %v", err)
	}
	if reason != "" {
		t.Errorf("reason = %q, want none: untracked-only dirt is cleared by the prep", reason)
	}
	if !facts.Known || !facts.OnOrigin || facts.Tip != branchTip {
		t.Errorf("facts = %+v, want the fetched origin tip %s", facts, branchTip)
	}
	if _, err := os.Stat(scratch); err != nil {
		t.Errorf("the check moved %s; a pre-claim check must be read-only: %v", scratch, err)
	}
}

// A local commit plus untracked files: HEAD holds work origin lacks, so the
// move-aside must not happen and the check refuses before a claim is spent.
func TestClaimWorktreeRefusal_LocalOnlyCommitWithUntrackedRefused(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	f.commitOn(t, taskWorktreeBranch, "origin/staging", "branch.txt", "branch work", "the work")
	req := f.request()
	req.Resume = true
	first, err := f.prepare(t, req)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	writeTestFile(t, filepath.Join(first.Dir, "local.txt"), "local commit")
	gitRun(t, first.Dir, "add", "-A")
	gitRun(t, first.Dir, "commit", "-m", "local work")
	scratch := filepath.Join(first.Dir, "scratch.txt")
	writeTestFile(t, scratch, "leftover")

	reason, _, err := claimWorktreeRefusal(context.Background(), req, "")
	if err != nil {
		t.Fatalf("claimWorktreeRefusal: %v", err)
	}
	if !strings.Contains(reason, "untracked files") || !strings.Contains(reason, "does not have") {
		t.Errorf("reason = %q, want it to say HEAD holds commits origin does not", reason)
	}
	if _, err := os.Stat(scratch); err != nil {
		t.Errorf("the check moved %s: %v", scratch, err)
	}
}

// A fresh tree at the PR base with untracked files is feasible: the pre-check
// carries the resolved PR base, so HEAD is proven to be at origin/<base>.
func TestClaimWorktreeRefusal_FreshUntrackedAtBaseIsFeasible(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	first, err := f.prepare(t, f.request())
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	writeTestFile(t, filepath.Join(first.Dir, "stray.txt"), "leftover")

	reason, _, err := claimWorktreeRefusal(context.Background(), f.request(), "")
	if err != nil {
		t.Fatalf("claimWorktreeRefusal: %v", err)
	}
	if reason != "" {
		t.Errorf("reason = %q, want none: the prep can prove HEAD is at the PR base", reason)
	}
}

func TestClaimWorktreeRefusal_DetachedRefused(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	f.commitOn(t, taskWorktreeBranch, "origin/staging", "branch.txt", "branch work", "the work")
	req := f.request()
	req.Resume = true
	first, err := f.prepare(t, req)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	gitRun(t, first.Dir, "checkout", "--detach", "HEAD")

	reason, _, err := claimWorktreeRefusal(context.Background(), req, "")
	if err != nil {
		t.Fatalf("claimWorktreeRefusal: %v", err)
	}
	if !strings.Contains(reason, "detached") {
		t.Errorf("reason = %q, want the detached tree named", reason)
	}
}

func TestClaimWorktreeRefusal_MainCheckoutOnBranchRefused(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	f.commitOn(t, taskWorktreeBranch, "origin/staging", "branch.txt", "branch work", "the work")
	gitRun(t, f.checkout, "fetch", "origin")
	gitRun(t, f.checkout, "checkout", "-b", taskWorktreeBranch, "origin/"+taskWorktreeBranch)
	req := f.request()
	req.Resume = true

	reason, _, err := claimWorktreeRefusal(context.Background(), req, "")
	if err != nil {
		t.Fatalf("claimWorktreeRefusal: %v", err)
	}
	if !strings.Contains(reason, "main checkout") {
		t.Errorf("reason = %q, want the main checkout named", reason)
	}
}

// The create path's refusals are proven before the claim too. A resume whose
// branch is not on origin would fail taskWorktreeAddExisting after the claim.
func TestClaimWorktreeRefusal_CreateResumeBranchNotOnOriginRefused(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	req := f.request()
	req.Resume = true // no requirement to have pushed this branch first

	reason, facts, err := claimWorktreeRefusal(context.Background(), req, "")
	if err != nil {
		t.Fatalf("claimWorktreeRefusal: %v", err)
	}
	if !strings.Contains(reason, "not on origin") {
		t.Errorf("reason = %q, want the missing origin branch named", reason)
	}
	if !facts.Known {
		t.Errorf("facts = %+v, want the fetch recorded for the prep", facts)
	}
}

// A directory at the deterministic task path that is not a registered worktree
// makes the create refuse; a pre-claim check must find that before a claim.
func TestClaimWorktreeRefusal_CreatePathIsNotAWorktreeRefused(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	req := f.request()
	registered, err := taskWorktreeWorktrees(context.Background(), f.checkout)
	if err != nil {
		t.Fatalf("taskWorktreeWorktrees: %v", err)
	}
	var main string
	if len(registered) > 0 {
		main = registered[0].path
	}
	dir := taskWorktreeDir(req, main)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	reason, _, err := claimWorktreeRefusal(context.Background(), req, "")
	if err != nil {
		t.Fatalf("claimWorktreeRefusal: %v", err)
	}
	if !strings.Contains(reason, "already exists but is not a worktree") {
		t.Errorf("reason = %q, want the unregistered directory named", reason)
	}
}

// A fresh task whose resolved PR base is not on origin is refused before the
// claim: the create would fail at taskWorktreeAddNew.
func TestClaimWorktreeRefusal_CreateMissingPRBaseRefused(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	req := f.request()
	req.PRBase = "no-such-base"

	reason, facts, err := claimWorktreeRefusal(context.Background(), req, "")
	if err != nil {
		t.Fatalf("claimWorktreeRefusal: %v", err)
	}
	if !strings.Contains(reason, "the PR base no-such-base is not on origin") {
		t.Errorf("reason = %q, want the missing PR base named", reason)
	}
	if !facts.Known {
		t.Errorf("facts = %+v, want the fetch recorded for the prep", facts)
	}
}
