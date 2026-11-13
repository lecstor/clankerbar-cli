package loop

// task_worktree_scratch.go — what a reused worktree does with its uncommitted
// state: tracked changes refuse it, and untracked files are moved aside (never
// deleted) once HEAD is proven to be at or behind a ref origin has. The cleared
// tree is then reusable, because untracked files cannot hold committed work.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// taskWorktreeTrackedSummary summarises the TRACKED changes for a refusal
// message: staged, unstaged or conflicted. "" means the tracked tree is clean.
// Untracked files are deliberately excluded — they cannot hold committed work,
// so reuse clears them rather than refusing.
func taskWorktreeTrackedSummary(ctx context.Context, dir string) (string, error) {
	out, err := taskWorktreeGit(ctx, dir, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return "", err
	}
	return taskWorktreeSummary(taskWorktreeLines(out)), nil
}

// taskWorktreeUntracked lists the untracked, not-ignored paths in dir. A wholly
// untracked directory comes back as one "dir/" entry, so a large scratch tree
// is one move, not thousands.
func taskWorktreeUntracked(ctx context.Context, dir string) ([]string, error) {
	out, err := taskWorktreeGit(ctx, dir, "ls-files", "--others", "--exclude-standard", "--directory", "-z")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	var paths []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// taskWorktreeSummary renders up to three status lines for a message.
func taskWorktreeSummary(lines []string) string {
	switch {
	case len(lines) == 0:
		return ""
	case len(lines) > 3:
		return strings.Join(lines[:3], "; ") + fmt.Sprintf("; and %d more", len(lines)-3)
	}
	return strings.Join(lines, "; ")
}

// taskWorktreeClearedRef is the ref that proves a tree whose only dirt is
// untracked files holds nothing origin lacks: HEAD must be that ref or an
// ancestor of it. origin/<branch> is preferred; a task's first session may cut
// its branch from the PR base before anything is pushed, so a non-resume
// request also accepts origin/<PRBase>. "" means no ref proves it.
func taskWorktreeClearedRef(ctx context.Context, req worktreeRequest, dir string) (string, error) {
	candidates := []string{"origin/" + req.Branch}
	if !req.Resume && !req.CloseOut && req.PRBase != "" {
		candidates = append(candidates, "origin/"+req.PRBase)
	}
	for _, ref := range candidates {
		if !taskWorktreeCommitishExists(ctx, dir, ref) {
			continue
		}
		if _, err := taskWorktreeGit(ctx, dir, "merge-base", "--is-ancestor", "HEAD", ref); err == nil {
			return ref, nil
		} else if !negativeFact(err) {
			return "", err
		}
	}
	return "", nil
}

// taskWorktreeUnclearableReason says why no ref proves a tree with untracked
// files safe to clear: HEAD either has commits the branch's origin tip lacks, or
// the branch is not on origin at all. Callers use it only after
// taskWorktreeClearedRef found no proving ref.
func taskWorktreeUnclearableReason(ctx context.Context, req worktreeRequest, dir string) string {
	remote := "origin/" + req.Branch
	if taskWorktreeCommitishExists(ctx, dir, remote) {
		return fmt.Sprintf("HEAD holds commits %s does not have", remote)
	}
	return "HEAD is not at or behind a ref origin has"
}

// taskWorktreeMoveUntracked moves the listed untracked paths out of dir into a
// fresh directory under scratchRoot, preserving each path relative to dir, and
// reports where they went. Nothing is deleted: a path leaves the tree only after
// its content is at the destination.
func taskWorktreeMoveUntracked(ctx context.Context, dir, scratchRoot, taskID string, paths []string) (string, error) {
	if len(paths) == 0 {
		return "", nil
	}
	if got, err := taskWorktreeUntracked(ctx, dir); err != nil {
		return "", err
	} else if strings.Join(got, "\x00") != strings.Join(paths, "\x00") {
		// The caller decided on an earlier listing; a tree that changed under it
		// is not this function's to clear blindly.
		return "", fmt.Errorf("the untracked set changed while it was being moved aside")
	}
	dest, err := taskWorktreeScratchDir(dir, scratchRoot, taskID)
	if err != nil {
		return "", err
	}
	for _, rel := range paths {
		clean := strings.TrimSuffix(rel, "/")
		if !filepath.IsLocal(clean) {
			return dest, fmt.Errorf("untracked path %q escapes %s", rel, dir)
		}
		src := filepath.Join(dir, clean)
		dst := filepath.Join(dest, clean)
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return dest, err
		}
		if err := taskWorktreeMovePath(src, dst); err != nil {
			return dest, fmt.Errorf("moving %s aside: %w", rel, err)
		}
	}
	return dest, nil
}

// taskWorktreeScratchDir makes the timestamped directory cleared files go to.
// It must sit OUTSIDE the worktree: the caller passes the runner's state dir;
// with none, a sibling of the task-worktree root is used rather than the OS temp
// dir, and the same fallback applies if the state dir itself is inside the tree.
func taskWorktreeScratchDir(dir, scratchRoot, taskID string) (string, error) {
	if !filepath.IsAbs(scratchRoot) || taskWorktreeUnder(scratchRoot, dir) {
		scratchRoot = filepath.Join(filepath.Dir(dir), ".scratch-aside")
	}
	id := strings.TrimSpace(taskID)
	if len(id) > 8 {
		id = id[:8]
	}
	if err := os.MkdirAll(scratchRoot, 0o700); err != nil {
		return "", err
	}
	base := filepath.Join(scratchRoot, id+"-"+time.Now().UTC().Format("20060102T150405.000Z"))
	for n := 0; ; n++ {
		dest := base
		if n > 0 {
			dest = fmt.Sprintf("%s-%d", base, n)
		}
		err := os.Mkdir(dest, 0o700)
		if err == nil {
			return dest, nil
		}
		if !os.IsExist(err) {
			return "", err
		}
	}
}

// taskWorktreeMovePath moves one path. A rename is atomic; across filesystems it
// falls back to a copy followed by removing the source, so the source is never
// gone before the destination holds it.
func taskWorktreeMovePath(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if err := taskWorktreeCopyPath(src, dst, info); err != nil {
		return err
	}
	return os.RemoveAll(src)
}

func taskWorktreeCopyPath(src, dst string, info os.FileInfo) error {
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	case info.IsDir():
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
			return err
		}
		for _, e := range entries {
			child := filepath.Join(src, e.Name())
			childInfo, err := os.Lstat(child)
			if err != nil {
				return err
			}
			if err := taskWorktreeCopyPath(child, filepath.Join(dst, e.Name()), childInfo); err != nil {
				return err
			}
		}
		return nil
	default:
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
		if err != nil {
			in.Close()
			return err
		}
		_, cerr := io.Copy(out, in)
		in.Close()
		if cerr != nil {
			out.Close()
			return cerr
		}
		return out.Close()
	}
}
