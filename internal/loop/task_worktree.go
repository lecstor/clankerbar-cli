package loop

// task_worktree.go — the runner prepares the task's worktree before the session
// starts (runner-owned mechanics A4, docs/runner-owned-mechanics.md).
//
// The session is handed a branch that is already checked out at a deterministic
// path, based on the PR base, and — when resuming — already merged with the
// current base where git can merge it. Everything mechanical that used to be a
// prompt instruction and a full-context turn happens here instead.
//
// Two rules the code below keeps:
//   - never discard work. A reused worktree that is dirty, detached, on another
//     branch, or diverged from origin is REFUSED with a reason; only a
//     fast-forward is applied to a clean, matching tree.
//   - a conflict is not a failure. The merge is aborted, the conflicting paths
//     are returned, and the session starts clean at the pre-merge tip with the
//     conflict to resolve itself.
//
// Nothing here pushes or opens a PR; that is the close-out's job (B7).

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// worktreeRequest is one task's worktree to prepare. CloseOut marks an approved
// close-out: the branch is checked out but the PR base is NEVER merged into it,
// because a close-out publishes work that was already reviewed.
type worktreeRequest struct {
	Checkout string // the repo's main checkout (config.ResolveCheckout result)
	TaskID   string // UUID; dir name = first 8 chars
	Branch   string // the plane's branch name, e.g. clanker/1a2b3c4d-some-title
	PRBase   string // e.g. staging
	Resume   bool   // branch already exists on origin
	CloseOut bool   // approved close-out: check out, never merge PRBase
}

// worktreePrep is the prepared tree the caller briefs the session with.
type worktreePrep struct {
	Dir       string
	Tip       string   // HEAD after prep
	OriginTip string   // origin/<branch> after the fetch; empty when the branch is not on origin
	Ahead     int      // commits HEAD has that origin/<branch> lacks; 0 when OriginTip is empty
	Created   bool     // worktree newly added (vs reused)
	Merged    bool     // PRBase merged into a resumed branch, moving HEAD
	UpToDate  bool     // merge was a no-op
	Conflicts []string // merge stopped: conflicting paths; aborted, tree clean at the pre-merge tip
}

// registeredWorktree is one entry of `git worktree list --porcelain`.
type registeredWorktree struct {
	path     string
	branch   string // short name; empty when detached
	detached bool
	// prunable marks an entry whose directory is gone but whose registration
	// remains: git still refuses to add that path or branch until it is pruned.
	prunable bool
}

// prepareTaskWorktree makes the task's worktree ready for a session and reports
// where it is and what happened to it. An error from the fetch is transient: the
// caller should retry rather than treat the task as failed.
func prepareTaskWorktree(ctx context.Context, req worktreeRequest) (worktreePrep, error) {
	dir, err := taskWorktreeDir(req)
	if err != nil {
		return worktreePrep{}, err
	}
	prep := worktreePrep{Dir: dir}
	if _, err := taskWorktreeGit(ctx, req.Checkout, "fetch", "origin", "--prune"); err != nil {
		return prep, fmt.Errorf("fetching origin in %s: %w", req.Checkout, err)
	}
	registered, err := taskWorktreeWorktrees(ctx, req.Checkout)
	if err != nil {
		return prep, err
	}
	// A stale registration (directory gone, entry still listed) makes git refuse
	// the very add or checkout this session needs, so clear those first.
	prunable := false
	for _, wt := range registered {
		prunable = prunable || wt.prunable
	}
	if prunable {
		if _, err := taskWorktreeGit(ctx, req.Checkout, "worktree", "prune"); err != nil {
			return prep, err
		}
		if registered, err = taskWorktreeWorktrees(ctx, req.Checkout); err != nil {
			return prep, err
		}
	}
	for _, wt := range registered {
		if taskWorktreeSamePath(wt.path, dir) {
			return reuseTaskWorktree(ctx, req, prep, wt)
		}
	}
	// The branch may already be checked out elsewhere — a session can cut its
	// own `<repo>/.worktrees/<name>` for it, and git refuses to add it twice.
	// Only a tree under the task-worktree roots is provably this task's own;
	// anything else may be another drain instance's checkout.
	if wt, ok := taskWorktreeOnBranch(registered, req.Branch); ok {
		// The main worktree is listed first: the operator's checkout, never a
		// session's. req.Checkout itself may be a LINKED worktree (the fleet
		// layout), so it is another run's tree too.
		if len(registered) > 0 && taskWorktreeSamePath(wt.path, registered[0].path) {
			return prep, fmt.Errorf("the branch %s is checked out in the main checkout %s — refusing to run a session there", req.Branch, wt.path)
		}
		if taskWorktreeSamePath(wt.path, req.Checkout) {
			return prep, fmt.Errorf("the branch %s is checked out in %s — refusing to run a session there", req.Branch, wt.path)
		}
		roots := []string{panelWorktreeRoot(req.Checkout)}
		if len(registered) > 0 {
			roots = append(roots, filepath.Join(registered[0].path, ".worktrees"))
		}
		adoptable := false
		for _, root := range roots {
			adoptable = adoptable || taskWorktreeUnder(wt.path, root)
		}
		if !adoptable {
			return prep, fmt.Errorf("the branch %s is checked out in %s, which is not a task worktree — refusing to adopt it", req.Branch, wt.path)
		}
		prep.Dir = wt.path
		return reuseTaskWorktree(ctx, req, prep, wt)
	}
	return createTaskWorktree(ctx, req, prep)
}

// taskWorktreeDir validates the request and resolves the deterministic path:
// the fleet's worktree root for this checkout, then the task's id prefix.
func taskWorktreeDir(req worktreeRequest) (string, error) {
	if strings.TrimSpace(req.Checkout) == "" {
		return "", fmt.Errorf("no checkout to prepare a task worktree from")
	}
	id := strings.TrimSpace(req.TaskID)
	if len(id) < 8 || !isHex(id[:8]) {
		return "", fmt.Errorf("task id %q is not a UUID with at least 8 hex characters", req.TaskID)
	}
	if strings.TrimSpace(req.Branch) == "" {
		return "", fmt.Errorf("task %s has no branch to prepare", id[:8])
	}
	// A close-out checks out an existing branch and never branches from or
	// merges the base, so it is the one shape that needs no PR base.
	if !req.CloseOut && strings.TrimSpace(req.PRBase) == "" {
		return "", fmt.Errorf("task %s has no PR base to branch from", id[:8])
	}
	return filepath.Join(panelWorktreeRoot(req.Checkout), id[:8]), nil
}

func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// reuseTaskWorktree takes over a worktree that is already registered for this
// repo. It only adopts one that is demonstrably the task's own tree and clean;
// anything else is refused by name, because the alternative is discarding work.
func reuseTaskWorktree(ctx context.Context, req worktreeRequest, prep worktreePrep, wt registeredWorktree) (worktreePrep, error) {
	switch {
	case wt.detached:
		return prep, fmt.Errorf("worktree %s is detached, not on %s — refusing to reuse it", prep.Dir, req.Branch)
	case wt.branch != req.Branch:
		return prep, fmt.Errorf("worktree %s is on %s, not %s — refusing to reuse it", prep.Dir, wt.branch, req.Branch)
	}
	dirty, err := taskWorktreeDirty(ctx, prep.Dir)
	if err != nil {
		return prep, err
	}
	if dirty != "" {
		return prep, fmt.Errorf("worktree %s has uncommitted changes (%s) — refusing to reuse it", prep.Dir, dirty)
	}
	if req.Resume || req.CloseOut {
		if err := taskWorktreeSyncBranch(ctx, prep.Dir, req.Branch); err != nil {
			return prep, err
		}
	}
	if req.Resume && !req.CloseOut {
		merged, upToDate, conflicts, err := taskWorktreeMerge(ctx, prep.Dir, "origin/"+req.PRBase)
		if err != nil {
			return prep, err
		}
		prep.Merged, prep.UpToDate, prep.Conflicts = merged, upToDate, conflicts
	}
	return finishTaskWorktreePrep(ctx, req, prep)
}

// createTaskWorktree adds the task's worktree at the deterministic path. A path
// that exists without being a registered worktree is refused, never adopted:
// git itself refuses an existing path, and a directory nobody registered is
// somebody else's.
func createTaskWorktree(ctx context.Context, req worktreeRequest, prep worktreePrep) (worktreePrep, error) {
	if _, err := os.Stat(prep.Dir); err == nil {
		return prep, fmt.Errorf("%s already exists but is not a worktree of %s — refusing to reuse it", prep.Dir, req.Checkout)
	} else if !os.IsNotExist(err) {
		return prep, err
	}
	if err := os.MkdirAll(filepath.Dir(prep.Dir), 0o755); err != nil {
		return prep, err
	}
	var err error
	if req.Resume || req.CloseOut {
		err = taskWorktreeAddExisting(ctx, req, prep.Dir)
	} else {
		err = taskWorktreeAddNew(ctx, req, prep.Dir)
	}
	if err != nil {
		return prep, err
	}
	prep.Created = true
	if req.Resume && !req.CloseOut {
		merged, upToDate, conflicts, err := taskWorktreeMerge(ctx, prep.Dir, "origin/"+req.PRBase)
		if err != nil {
			return prep, err
		}
		prep.Merged, prep.UpToDate, prep.Conflicts = merged, upToDate, conflicts
	}
	return finishTaskWorktreePrep(ctx, req, prep)
}

// taskWorktreeAddExisting cuts a NEW worktree for a branch that is on origin.
// -B puts the local branch at the remote tip, which resets it, so an existing
// local branch may only be fast-forwarded: anything else is unpushed work.
func taskWorktreeAddExisting(ctx context.Context, req worktreeRequest, dir string) error {
	remote := "origin/" + req.Branch
	if !taskWorktreeCommitishExists(ctx, req.Checkout, remote) {
		return fmt.Errorf("the branch %s is not on origin", req.Branch)
	}
	if taskWorktreeCommitishExists(ctx, req.Checkout, "refs/heads/"+req.Branch) {
		if _, err := taskWorktreeGit(ctx, req.Checkout, "merge-base", "--is-ancestor", "refs/heads/"+req.Branch, remote); err != nil {
			return fmt.Errorf("the local branch %s has commits origin/%s does not have — refusing to reset it", req.Branch, req.Branch)
		}
	}
	_, err := taskWorktreeGit(ctx, req.Checkout, "worktree", "add", "-B", req.Branch, dir, remote)
	return err
}

// taskWorktreeAddNew cuts the first worktree for a task with no work yet. A
// local branch of the same name may only be reused when it carries nothing the
// PR base does not already have; otherwise it is somebody's commits and the
// request is refused rather than reset.
func taskWorktreeAddNew(ctx context.Context, req worktreeRequest, dir string) error {
	base := "origin/" + req.PRBase
	if !taskWorktreeCommitishExists(ctx, req.Checkout, base) {
		return fmt.Errorf("the PR base %s is not on origin", req.PRBase)
	}
	if !taskWorktreeCommitishExists(ctx, req.Checkout, "refs/heads/"+req.Branch) {
		_, err := taskWorktreeGit(ctx, req.Checkout, "worktree", "add", "-b", req.Branch, dir, base)
		return err
	}
	if _, err := taskWorktreeGit(ctx, req.Checkout, "merge-base", "--is-ancestor", "refs/heads/"+req.Branch, base); err != nil {
		return fmt.Errorf("the local branch %s already exists with commits %s does not have — refusing to reuse it", req.Branch, base)
	}
	_, err := taskWorktreeGit(ctx, req.Checkout, "worktree", "add", dir, req.Branch)
	return err
}

// taskWorktreeSyncBranch fast-forwards a reused worktree's branch to
// origin/<branch>. A branch that has DIVERGED is refused: it holds commits of
// its own AND origin moved, and deciding which side wins is not this function's
// call. Ahead-only is left alone — those are unpushed local commits, not a
// conflict with origin.
func taskWorktreeSyncBranch(ctx context.Context, dir, branch string) error {
	remote := "origin/" + branch
	if !taskWorktreeCommitishExists(ctx, dir, remote) {
		return fmt.Errorf("the branch %s is not on origin", branch)
	}
	out, err := taskWorktreeGit(ctx, dir, "rev-list", "--left-right", "--count", "HEAD..."+remote)
	if err != nil {
		return err
	}
	ahead, behind, err := taskWorktreeCounts(out)
	if err != nil {
		return err
	}
	if behind == 0 {
		return nil
	}
	if ahead > 0 {
		return fmt.Errorf("worktree %s has diverged from %s (%d local and %d remote commit(s)) — refusing to reset it",
			dir, remote, ahead, behind)
	}
	if _, err := taskWorktreeGit(ctx, dir, "merge", "--ff-only", remote); err != nil {
		return fmt.Errorf("fast-forwarding %s to %s: %w", dir, remote, err)
	}
	return nil
}

func taskWorktreeCounts(out string) (ahead, behind int, err error) {
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("git rev-list --left-right --count returned %q", out)
	}
	if ahead, err = strconv.Atoi(fields[0]); err != nil {
		return 0, 0, err
	}
	if behind, err = strconv.Atoi(fields[1]); err != nil {
		return 0, 0, err
	}
	return ahead, behind, nil
}

// taskWorktreeMerge merges base into HEAD with a merge commit, never a rebase.
// A conflict is reported as paths with a nil error, after aborting: the session
// starts clean at the pre-merge tip and does the merge itself.
func taskWorktreeMerge(ctx context.Context, dir, base string) (merged, upToDate bool, conflicts []string, err error) {
	before, err := taskWorktreeHead(ctx, dir)
	if err != nil {
		return false, false, nil, err
	}
	if _, merr := taskWorktreeGit(ctx, dir, "merge", "--no-edit", base); merr != nil {
		out, derr := taskWorktreeGit(ctx, dir, "diff", "--name-only", "--diff-filter=U")
		if derr == nil {
			if paths := taskWorktreeLines(out); len(paths) > 0 {
				if _, aerr := taskWorktreeGit(ctx, dir, "merge", "--abort"); aerr != nil {
					return false, false, nil, fmt.Errorf("aborting the conflicted merge of %s: %w", base, aerr)
				}
				return false, false, paths, nil
			}
		}
		// Nothing conflicted: a real failure. Abort anything half-started so the
		// tree is not left mid-merge, then surface the error.
		_, _ = taskWorktreeGit(ctx, dir, "merge", "--abort")
		return false, false, nil, merr
	}
	after, err := taskWorktreeHead(ctx, dir)
	if err != nil {
		return false, false, nil, err
	}
	return after != before, after == before, nil, nil
}

// finishTaskWorktreePrep records the tip HEAD afterwards, plus where origin's
// branch is: a reused tree ahead of origin keeps its unpushed commits, so its
// Tip is not "the tip on origin" and the caller must be able to tell.
func finishTaskWorktreePrep(ctx context.Context, req worktreeRequest, prep worktreePrep) (worktreePrep, error) {
	tip, err := taskWorktreeHead(ctx, prep.Dir)
	if err != nil {
		return prep, err
	}
	prep.Tip = tip
	remote := "origin/" + req.Branch
	if !taskWorktreeCommitishExists(ctx, prep.Dir, remote) {
		return prep, nil
	}
	if prep.OriginTip, err = taskWorktreeGit(ctx, prep.Dir, "rev-parse", remote); err != nil {
		return prep, err
	}
	out, err := taskWorktreeGit(ctx, prep.Dir, "rev-list", "--count", remote+"..HEAD")
	if err != nil {
		return prep, err
	}
	ahead, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return prep, fmt.Errorf("git rev-list --count returned %q: %w", out, err)
	}
	prep.Ahead = ahead
	return prep, nil
}

func taskWorktreeHead(ctx context.Context, dir string) (string, error) {
	return taskWorktreeGit(ctx, dir, "rev-parse", "HEAD")
}

// taskWorktreeWorktrees is every entry of `git worktree list --porcelain`.
func taskWorktreeWorktrees(ctx context.Context, checkout string) ([]registeredWorktree, error) {
	out, err := taskWorktreeGit(ctx, checkout, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var list []registeredWorktree
	var cur *registeredWorktree
	flush := func() {
		if cur != nil {
			list = append(list, *cur)
			cur = nil
		}
	}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			cur = &registeredWorktree{path: strings.TrimPrefix(line, "worktree ")}
		case line == "detached":
			if cur != nil {
				cur.detached = true
			}
		case line == "prunable", strings.HasPrefix(line, "prunable "):
			if cur != nil {
				cur.prunable = true
			}
		case strings.HasPrefix(line, "branch "):
			if cur != nil {
				cur.branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
			}
		}
	}
	flush()
	return list, nil
}

// taskWorktreeOnBranch finds the registered worktree sitting on branch.
func taskWorktreeOnBranch(list []registeredWorktree, branch string) (registeredWorktree, bool) {
	for _, wt := range list {
		if !wt.detached && wt.branch == branch {
			return wt, true
		}
	}
	return registeredWorktree{}, false
}

// taskWorktreeSamePath compares two paths that may differ only in how a symlink
// (macOS /var) was spelled by git versus by us.
func taskWorktreeSamePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, aerr := filepath.EvalSymlinks(a)
	rb, berr := filepath.EvalSymlinks(b)
	return aerr == nil && berr == nil && ra == rb
}

// taskWorktreeUnder reports whether path sits inside root, comparing paths the
// way taskWorktreeSamePath does: git may hand back a symlink-resolved spelling.
func taskWorktreeUnder(path, root string) bool {
	resolve := func(p string) string {
		clean := filepath.Clean(p)
		if resolved, err := filepath.EvalSymlinks(clean); err == nil {
			return resolved
		}
		return clean
	}
	rel, err := filepath.Rel(resolve(root), resolve(path))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// taskWorktreeDirty summarises uncommitted changes for a refusal message; ""
// means clean. Untracked files count: a session's leftovers are work too.
func taskWorktreeDirty(ctx context.Context, dir string) (string, error) {
	out, err := taskWorktreeGit(ctx, dir, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	lines := taskWorktreeLines(out)
	if len(lines) == 0 {
		return "", nil
	}
	if len(lines) > 3 {
		return strings.Join(lines[:3], "; ") + fmt.Sprintf("; and %d more", len(lines)-3), nil
	}
	return strings.Join(lines, "; "), nil
}

func taskWorktreeCommitishExists(ctx context.Context, dir, commitish string) bool {
	_, err := taskWorktreeGit(ctx, dir, "rev-parse", "--verify", "--quiet", commitish+"^{commit}")
	return err == nil
}

func taskWorktreeLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// taskWorktreeGit runs one git command in dir. The environment is the caller's
// minus the GIT_* variables that ADDRESS a repository: a driver started from a
// hook or a rebase can carry GIT_DIR/GIT_WORK_TREE, and `-C` would then address
// the wrong repository. The non-interactive settings mirror panelGit: an
// unattended run has no terminal, and git must fail rather than wait.
func taskWorktreeGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = taskWorktreeGitEnv()
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, firstLineOf(msg))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// taskWorktreeRepoEnvVars name a repository rather than configure git; an
// inherited one would override the -C that taskWorktreeGit passes.
var taskWorktreeRepoEnvVars = []string{
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_INDEX_FILE",
	"GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_COMMON_DIR",
	"GIT_NAMESPACE",
	"GIT_PREFIX",
}

// taskWorktreeGitEnvDefaults make git fail fast instead of prompting, but an
// inherited value wins: GIT_SSH_COMMAND, GIT_ASKPASS and SSH_ASKPASS may carry
// the operator's own auth.
var taskWorktreeGitEnvDefaults = []string{
	"GIT_ASKPASS=echo",
	"SSH_ASKPASS=echo",
	"GIT_SSH_COMMAND=ssh -oBatchMode=yes -oConnectTimeout=10",
}

// taskWorktreeGitEnv is the caller's environment with the repository-addressing
// variables dropped, the two prompt settings forced off, and defaults for the
// rest.
func taskWorktreeGitEnv() []string {
	env := os.Environ()
	kept := make([]string, 0, len(env)+5)
	present := make(map[string]bool, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		// These two must never prompt whatever the caller set.
		if name == "GIT_TERMINAL_PROMPT" || name == "GCM_INTERACTIVE" || taskWorktreeAddressesRepo(name) {
			continue
		}
		kept = append(kept, kv)
		present[name] = true
	}
	for _, kv := range taskWorktreeGitEnvDefaults {
		if name, _, _ := strings.Cut(kv, "="); !present[name] {
			kept = append(kept, kv)
		}
	}
	return append(kept, "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
}

func taskWorktreeAddressesRepo(name string) bool {
	for _, repoVar := range taskWorktreeRepoEnvVars {
		if name == repoVar {
			return true
		}
	}
	return false
}
