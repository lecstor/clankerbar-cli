package loop

// task_worktree_scratch_test.go — reuse over a real repository when the tree's
// only uncommitted state is untracked files: cleared to a logged directory
// outside the tree, never deleted, and refused when HEAD could hold work origin
// lacks.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A tracked modification could be uncommitted work: it still refuses, and
// nothing in the tree is touched.
func TestTaskWorktree_TrackedDirtyReuseRefused(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	first, err := f.prepare(t, f.request())
	if err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	writeTestFile(t, filepath.Join(first.Dir, "a.txt"), "edited but never committed")

	if _, err := f.prepare(t, f.request()); err == nil {
		t.Fatalf("a tracked modification must be refused")
	} else if !strings.Contains(err.Error(), "uncommitted changes") {
		t.Errorf("error %q does not say why the worktree was refused", err)
	}
	if body, err := os.ReadFile(filepath.Join(first.Dir, "a.txt")); err != nil || strings.TrimSpace(string(body)) != "edited but never committed" {
		t.Errorf("the refusal discarded the tracked change: %q (%v)", body, err)
	}
}

// Untracked session scratch cannot hold committed work when HEAD is at the PR
// base, so the tree is reused after the files are moved to a logged directory
// outside it. Nothing is deleted.

// Untracked session scratch cannot hold committed work when HEAD is at the PR
// base, so the tree is reused after the files are moved to a logged directory
// outside it. Nothing is deleted.
func TestTaskWorktree_UntrackedScratchMovedAsideAndReused(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	first, err := f.prepare(t, f.request())
	if err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	scratch := filepath.Join(first.Dir, ".scratch", "reference.txt")
	if err := os.MkdirAll(filepath.Dir(scratch), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, scratch, "an unpacked tarball a session used as reference")
	scratchRoot := t.TempDir()
	req := f.request()
	req.ScratchRoot = scratchRoot

	second, err := f.prepare(t, req)
	if err != nil {
		t.Fatalf("untracked-only dirt must be moved aside, not refused: %v", err)
	}
	if second.Created {
		t.Errorf("the second prepare reported Created; it reused %s", second.Dir)
	}
	if second.ScratchAside == "" {
		t.Fatal("ScratchAside is empty: the caller cannot log where the files went")
	}
	if !taskWorktreeUnder(second.ScratchAside, scratchRoot) {
		t.Errorf("ScratchAside = %q, want it under the requested root %q", second.ScratchAside, scratchRoot)
	}
	if taskWorktreeUnder(second.ScratchAside, second.Dir) {
		t.Errorf("ScratchAside = %q is INSIDE the worktree %q", second.ScratchAside, second.Dir)
	}
	moved := filepath.Join(second.ScratchAside, ".scratch", "reference.txt")
	if body, err := os.ReadFile(moved); err != nil || strings.TrimSpace(string(body)) != "an unpacked tarball a session used as reference" {
		t.Errorf("the moved scratch is not preserved at %s: %q (%v)", moved, body, err)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("%s still exists in the reused tree (err %v)", scratch, err)
	}
	if dirty := gitOut(t, second.Dir, "status", "--porcelain"); dirty != "" {
		t.Errorf("the reused worktree is not clean:\n%s", dirty)
	}
	if second.Tip != first.Tip {
		t.Errorf("Tip = %s, want the unchanged tip %s", second.Tip, first.Tip)
	}
}

// The same scratch on a RESUMED branch at origin's tip: the incident shape that
// parked CLA-847.

// The same scratch on a RESUMED branch at origin's tip: the incident shape that
// parked CLA-847.
func TestTaskWorktree_UntrackedScratchAtOriginTipMovedAside(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	branchTip := f.commitOn(t, taskWorktreeBranch, "origin/staging", "branch.txt", "branch work", "the work")
	req := f.request()
	req.Resume = true
	first, err := f.prepare(t, req)
	if err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	if first.Tip != branchTip {
		t.Fatalf("first prepare Tip = %s, want the origin tip %s", first.Tip, branchTip)
	}
	scratch := filepath.Join(first.Dir, ".tools", "repro.sh")
	if err := os.MkdirAll(filepath.Dir(scratch), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, scratch, "#!/bin/sh\ntrue")
	req.ScratchRoot = t.TempDir()

	second, err := f.prepare(t, req)
	if err != nil {
		t.Fatalf("untracked-only dirt at the origin tip must be reused, not refused: %v", err)
	}
	if second.ScratchAside == "" {
		t.Fatal("the untracked files were not reported as moved")
	}
	moved := filepath.Join(second.ScratchAside, ".tools", "repro.sh")
	if body, err := os.ReadFile(moved); err != nil || !strings.Contains(string(body), "true") {
		t.Errorf("the moved scratch is not preserved at %s: %q (%v)", moved, body, err)
	}
	if second.Tip != branchTip {
		t.Errorf("Tip = %s, want the origin tip %s", second.Tip, branchTip)
	}
	if dirty := gitOut(t, second.Dir, "status", "--porcelain"); dirty != "" {
		t.Errorf("the reused worktree is not clean:\n%s", dirty)
	}
}

// HEAD behind origin's tip is also proven to hold nothing origin lacks: the
// scratch is cleared and the branch fast-forwards.

// HEAD behind origin's tip is also proven to hold nothing origin lacks: the
// scratch is cleared and the branch fast-forwards.
func TestTaskWorktree_UntrackedScratchBehindOriginMovedAside(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	f.commitOn(t, taskWorktreeBranch, "origin/staging", "branch.txt", "branch work", "the work")
	req := f.request()
	req.Resume = true
	first, err := f.prepare(t, req)
	if err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	originTip := f.commitOn(t, taskWorktreeBranch, "", "more.txt", "more work", "more work")
	scratch := filepath.Join(first.Dir, ".scratch", "notes.txt")
	if err := os.MkdirAll(filepath.Dir(scratch), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, scratch, "reference")
	req.ScratchRoot = t.TempDir()

	second, err := f.prepare(t, req)
	if err != nil {
		t.Fatalf("untracked-only dirt behind origin must be reused: %v", err)
	}
	if second.ScratchAside == "" {
		t.Fatal("the untracked files were not reported as moved")
	}
	if _, err := os.Stat(filepath.Join(second.ScratchAside, ".scratch", "notes.txt")); err != nil {
		t.Errorf("the scratch is not preserved at %s: %v", second.ScratchAside, err)
	}
	if second.Tip != originTip {
		t.Errorf("Tip = %s, want the fast-forwarded origin tip %s", second.Tip, originTip)
	}
}

// HEAD holding a local commit is work origin lacks, so the untracked files are
// NOT cleared: the refusal leaves everything where it was.

// HEAD holding a local commit is work origin lacks, so the untracked files are
// NOT cleared: the refusal leaves everything where it was.
func TestTaskWorktree_LocalOnlyCommitWithUntrackedRefused(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	branchTip := f.commitOn(t, taskWorktreeBranch, "origin/staging", "branch.txt", "branch work", "the work")
	req := f.request()
	req.Resume = true
	first, err := f.prepare(t, req)
	if err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	writeTestFile(t, filepath.Join(first.Dir, "local.txt"), "local commit")
	gitRun(t, first.Dir, "add", "-A")
	gitRun(t, first.Dir, "commit", "-m", "local work")
	localTip := gitOut(t, first.Dir, "rev-parse", "HEAD")
	scratch := filepath.Join(first.Dir, "scratch.txt")
	writeTestFile(t, scratch, "do not lose me")
	req.ScratchRoot = t.TempDir()

	if _, err := f.prepare(t, req); err == nil {
		t.Fatalf("a tree whose HEAD holds a local commit must not have its untracked files cleared")
	} else if !strings.Contains(err.Error(), "untracked files") || !strings.Contains(err.Error(), "does not have") {
		t.Errorf("error %q does not say HEAD holds work origin lacks", err)
	}
	if got := gitOut(t, first.Dir, "rev-parse", "HEAD"); got != localTip {
		t.Errorf("HEAD = %s, want the local commit %s to survive", got, localTip)
	}
	if _, err := os.Stat(scratch); err != nil {
		t.Errorf("the refusal moved or deleted %s: %v", scratch, err)
	}
	if got := gitOut(t, first.Dir, "rev-parse", "origin/"+taskWorktreeBranch); got != branchTip {
		t.Errorf("origin ref = %s, want %s", got, branchTip)
	}
}

func TestTaskWorktree_ScratchAsideFallsBackOutsideTheTree(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	first, err := f.prepare(t, f.request())
	if err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	writeTestFile(t, filepath.Join(first.Dir, "stray.txt"), "leftover")

	second, err := f.prepare(t, f.request()) // no ScratchRoot: the fallback applies
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if second.ScratchAside == "" {
		t.Fatal("ScratchAside is empty")
	}
	if taskWorktreeUnder(second.ScratchAside, second.Dir) {
		t.Errorf("ScratchAside = %q is inside the worktree %q", second.ScratchAside, second.Dir)
	}
	if _, err := os.Stat(filepath.Join(second.ScratchAside, "stray.txt")); err != nil {
		t.Errorf("the scratch is not preserved at the fallback: %v", err)
	}
}

// An untracked sibling at origin's tip holds no committed work: its scratch is
// moved aside and the tree is adopted, exactly like a task tree under the roots.
func TestTaskWorktree_UntrackedScratchInSiblingMovedAsideAndAdopted(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	f.commitOn(t, taskWorktreeBranch, "origin/staging", "branch.txt", "branch work", "the work")
	gitRun(t, f.checkout, "fetch", "origin")
	sibling := filepath.Join(f.checkout, ".cla848")
	gitRun(t, f.checkout, "worktree", "add", "-b", taskWorktreeBranch, sibling, "origin/"+taskWorktreeBranch)
	scratch := filepath.Join(sibling, ".scratch", "notes.txt")
	if err := os.MkdirAll(filepath.Dir(scratch), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, scratch, "reference")
	req := f.request()
	req.Resume = true
	req.ScratchRoot = t.TempDir()

	prep, err := f.prepare(t, req)
	if err != nil {
		t.Fatalf("a sibling whose only dirt is untracked must be adopted: %v", err)
	}
	if !taskWorktreeSamePath(prep.Dir, sibling) || !prep.AdoptedOutside {
		t.Errorf("Dir/AdoptedOutside = %q/%v, want the sibling %q adopted", prep.Dir, prep.AdoptedOutside, sibling)
	}
	if prep.ScratchAside == "" {
		t.Fatal("ScratchAside is empty: the adopted tree's scratch was not reported")
	}
	if _, err := os.Stat(filepath.Join(prep.ScratchAside, ".scratch", "notes.txt")); err != nil {
		t.Errorf("the sibling's scratch is not preserved at %s: %v", prep.ScratchAside, err)
	}
	if dirty := gitOut(t, sibling, "status", "--porcelain"); dirty != "" {
		t.Errorf("the adopted sibling is not clean:\n%s", dirty)
	}
}

// A tracked change in the sibling could be uncommitted work: it still refuses.

// A tracked change in the sibling could be uncommitted work: it still refuses.
func TestTaskWorktree_TrackedDirtySiblingWorktreeRefused(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	f.commitOn(t, taskWorktreeBranch, "origin/staging", "branch.txt", "branch work", "the work")
	gitRun(t, f.checkout, "fetch", "origin")
	sibling := filepath.Join(f.checkout, ".cla848")
	gitRun(t, f.checkout, "worktree", "add", "-b", taskWorktreeBranch, sibling, "origin/"+taskWorktreeBranch)
	writeTestFile(t, filepath.Join(sibling, "branch.txt"), "edited")
	req := f.request()
	req.Resume = true

	_, err := f.prepare(t, req)
	if err == nil {
		t.Fatalf("a sibling with tracked changes must be refused, not adopted")
	}
	if !strings.Contains(err.Error(), ".cla848") || !strings.Contains(err.Error(), "uncommitted") {
		t.Errorf("error %q does not name the sibling and its dirty state", err)
	}
}

// A sibling holding a commit origin never got is refused: adopting it would make
// that work look like the task's own.

// A tree on the branch with untracked-only dirt is this task's tree: its
// scratch is moved aside and the tree reused.
func TestTaskWorktree_UntrackedScratchInWorktreeOnBranchMovedAside(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	f.commitOn(t, taskWorktreeBranch, "origin/staging", "branch.txt", "branch work", "the work")
	gitRun(t, f.checkout, "fetch", "origin")
	elsewhere := filepath.Join(f.checkout, ".worktrees", "session-wt")
	gitRun(t, f.checkout, "worktree", "add", "-b", taskWorktreeBranch, elsewhere, "origin/"+taskWorktreeBranch)
	scratch := filepath.Join(elsewhere, "stray.txt")
	writeTestFile(t, scratch, "work in progress")
	req := f.request()
	req.Resume = true
	req.ScratchRoot = t.TempDir()

	prep, err := f.prepare(t, req)
	if err != nil {
		t.Fatalf("untracked-only dirt in the branch's worktree must be reused: %v", err)
	}
	if !taskWorktreeSamePath(prep.Dir, elsewhere) {
		t.Errorf("Dir = %q, want the existing worktree %q", prep.Dir, elsewhere)
	}
	if prep.ScratchAside == "" {
		t.Fatal("ScratchAside is empty: the moved scratch was not reported")
	}
	if _, err := os.Stat(filepath.Join(prep.ScratchAside, "stray.txt")); err != nil {
		t.Errorf("the scratch is not preserved at the logged path: %v", err)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("%s still exists in the reused tree (err %v)", scratch, err)
	}
}

// A reused tree keeps its unpushed commits, so its Tip is not origin's; the
// caller has to be told both, or it reports the wrong tip on origin.

// The cross-filesystem fallback copies first: the source is still intact when
// the copy completes, and a symlink is copied as a link, never followed.
func TestTaskWorktreeCopyPathPreservesContentAndSymlinks(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(filepath.Join(src, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(src, "nested", "notes.txt"), "reference")
	if err := os.Symlink("../target", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, "dst")
	info, err := os.Lstat(src)
	if err != nil {
		t.Fatal(err)
	}

	if err := taskWorktreeCopyPath(src, dst, info); err != nil {
		t.Fatalf("taskWorktreeCopyPath: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(dst, "nested", "notes.txt")); err != nil || strings.TrimSpace(string(body)) != "reference" {
		t.Errorf("copied file = %q (%v), want the original content", body, err)
	}
	if target, err := os.Readlink(filepath.Join(dst, "link")); err != nil || target != "../target" {
		t.Errorf("copied symlink = %q (%v), want it copied as a link", target, err)
	}
	if _, err := os.Stat(filepath.Join(src, "nested", "notes.txt")); err != nil {
		t.Errorf("the copy removed the source: %v", err)
	}
}
