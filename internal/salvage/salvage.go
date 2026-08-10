// Package salvage rescues the work a harness session leaves uncommitted when it
// is killed mid-task.
//
// # Why
//
// A usage limit kills a session mid-turn. There is no graceful shutdown, so the
// protocol's "commit, push, record the branch before you let go" never runs -
// there is no *before*. The uncommitted work survives in the worktree and nowhere
// else: invisible to the plane, invisible to another machine, invisible to the
// next session. The lease then expires with no branch recorded, the plane
// correctly returns the task to `ready`, and the next clanker starts from nothing
// - redoing hours of work sitting intact on the same disk. Measured on
// 2026-08-10, one such task cost 112.7M tokens, a third of the day's spend, for
// work that was done twice (CLA-314).
//
// The driver is the only party that can close this. It outlives the session, it
// is already local and in the git tree, and it already watches the stream for the
// claim (CLA-242) and for delivery claims (CLA-253).
//
// # What it will and will not touch
//
// A mistaken claim is refused by an atomic write; a mistaken cleanup has no such
// backstop. So the rules here are narrow and none of them has an override:
//
//   - It acts on ONE worktree: the one whose checked-out branch carries this
//     task's id prefix. Never a worktree it cannot tie to the task, never a
//     detached HEAD, never `main` or `staging` - the `clanker/<id>` prefix is
//     what makes the tie, and there is no path that commits to a branch without
//     it.
//   - It matches on the TASK-ID prefix, not the whole derived branch name. The
//     slug half comes from the title, so a task retitled mid-flight computes a
//     name that no longer matches the worktree that exists - a silent miss.
//   - Two worktrees answering to the same task is an ambiguity, not a coin toss:
//     it is reported and nothing is touched.
//   - Nothing is ever forced. No `push --force`, no `checkout`, no branch
//     creation, no worktree removal. A push the remote refuses is reported as a
//     failure and leaves the branch UNRECORDED, because a recorded branch nobody
//     can fetch is worse than none at all.
package salvage

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/delivery"
)

// Status is what became of one salvage attempt.
type Status string

const (
	// Nothing: there was nothing to save. No worktree for this task, or a clean
	// one. NOT a failure, and the ordinary outcome for a session that finished
	// tidily.
	Nothing Status = "nothing"

	// Saved: uncommitted work was committed and pushed. Outcome.Branch names
	// where, and it is the only status that sets it.
	Saved Status = "saved"

	// Refused: there IS work, and it was deliberately left alone - an ambiguous
	// match, or a worktree in the middle of a merge or rebase. The detail says
	// where to look; a human or the next clanker decides.
	Refused Status = "refused"

	// Failed: it was attempted and did not finish - git missing, a commit that
	// would not run, a push the remote rejected. The work may still be committed
	// locally; the detail says what happened.
	Failed Status = "failed"
)

// Outcome is what one salvage attempt did, written to be read in a log by
// somebody who was asleep when it happened.
type Outcome struct {
	Status Status

	// Branch is set ONLY when the commit reached the remote, because it is what
	// the driver records on the task as the hand-off - and the hand-off is a claim
	// that fetchable work exists. A local commit is not a hand-off.
	Branch string

	// Commit is the salvage commit, when one was made.
	Commit string

	// Worktree is the tree acted on (or declined), when one was identified.
	Worktree string

	// Detail is the human-readable account. Always set except on the empty
	// Nothing.
	Detail string
}

// Saved reports whether there is a pushed branch to record on the task.
func (o Outcome) Saved() bool { return o.Status == Saved && o.Branch != "" }

// branchIDLen is how many characters of the task id the plane puts in a derived
// branch name: `clanker/${taskId.slice(0, BRANCH_ID_LEN)}-${slug(title)}`. It is
// duplicated here rather than communicated, exactly as the plane intends - the
// name is a pure function of the task, which is what makes it free to derive.
const branchIDLen = 8

// Salvager commits and pushes stranded work under a workdir. The zero value is
// not usable; build one with New.
type Salvager struct {
	workdir string
	gitBin  string
	remote  string
}

// New builds a Salvager rooted at a session's workdir. remote is the git remote
// to push to; empty means "origin, or the only remote there is" - the same rule
// the delivery checks judge against, taken from there rather than restated.
func New(workdir, remote string) *Salvager {
	if remote == "" {
		remote = "origin"
	}
	return &Salvager{workdir: workdir, gitBin: "git", remote: remote}
}

// Salvage finds the worktree this task was being worked in and, if it holds
// uncommitted work, commits and pushes it. label is the task's human-readable
// ref, used in the commit message and the detail; it is cosmetic.
//
// It never returns an error: every way this can go wrong is an Outcome the caller
// logs and carries on from. The caller must not treat a non-Saved outcome as a
// reason to stop the run - the session is already over, and this is a rescue.
func (s *Salvager) Salvage(ctx context.Context, taskID, label string) Outcome {
	prefix, ok := branchPrefix(taskID)
	if !ok {
		return Outcome{Status: Nothing, Detail: fmt.Sprintf(
			"task id %q does not spell a derived branch prefix, so no worktree can be tied to it", taskID)}
	}
	if _, err := exec.LookPath(s.gitBin); err != nil {
		return Outcome{Status: Failed, Detail: "git is not on PATH, so stranded work cannot be committed"}
	}

	repos := delivery.Repos(ctx, s.workdir)
	if len(repos) == 0 {
		return Outcome{Status: Nothing, Detail: fmt.Sprintf(
			"%s is not a git repository and none was found below it", s.workdir)}
	}

	found := s.findWorktrees(ctx, repos, prefix)
	switch len(found) {
	case 0:
		return Outcome{Status: Nothing, Detail: fmt.Sprintf(
			"no worktree at or below %s has a branch starting %q checked out", s.workdir, prefix)}
	case 1:
	default:
		return Outcome{Status: Refused, Detail: fmt.Sprintf(
			"%d worktrees below %s have a %q branch checked out (%s) - which one the session worked in cannot be told apart, and committing in the wrong one is worse than not committing",
			len(found), s.workdir, prefix, joinPaths(found))}
	}
	wt := found[0]

	if op := s.inProgressOp(ctx, wt.path); op != "" {
		// See the package doc: `add -A && commit` here does not preserve a state,
		// it invents one - a conflict "resolved" by committing its markers, or a
		// rebase's intermediate step recorded as though it were the work.
		return Outcome{Status: Refused, Worktree: wt.path, Detail: fmt.Sprintf(
			"%s is in the middle of a %s - committing it would record a state nobody chose, so it is left exactly as it is for a human to look at",
			wt.path, op)}
	}

	dirty, err := s.run(ctx, wt.path, "status", "--porcelain")
	if err != nil {
		return Outcome{Status: Failed, Worktree: wt.path, Detail: fmt.Sprintf(
			"could not read the state of %s: %v", wt.path, err)}
	}
	if dirty == "" {
		// The one thing that must NOT happen here is recording a branch: an empty
		// hand-off sends the next clanker - routinely on another machine - to fetch
		// nothing, which the protocol calls out as worse than recording none.
		return Outcome{Status: Nothing, Worktree: wt.path, Detail: fmt.Sprintf(
			"%s is clean, so there is nothing to salvage and no branch to record", wt.path)}
	}

	return s.commitAndPush(ctx, wt, taskID, label)
}

// commitAndPush is the only mutating path in this package. Everything above it is
// a guard.
func (s *Salvager) commitAndPush(ctx context.Context, wt worktree, taskID, label string) Outcome {
	out := Outcome{Worktree: wt.path}

	if _, err := s.run(ctx, wt.path, "add", "-A"); err != nil {
		out.Status, out.Detail = Failed, fmt.Sprintf("could not stage the work in %s: %v", wt.path, err)
		return out
	}
	subject, body := commitMessage(label, taskID, wt)
	// --no-verify: a pre-commit hook that lints or tests would reject exactly the
	// half-finished tree this exists to save. -c commit.gpgsign=false: a signing
	// key with a passphrase has nobody to ask at 3am and would hang the run.
	if _, err := s.run(ctx, wt.path, "-c", "commit.gpgsign=false", "commit", "--no-verify", "-m", subject, "-m", body); err != nil {
		out.Status, out.Detail = Failed, fmt.Sprintf("could not commit the work in %s: %v", wt.path, err)
		return out
	}
	sha, err := s.run(ctx, wt.path, "rev-parse", "HEAD")
	if err != nil {
		out.Status, out.Detail = Failed, fmt.Sprintf("committed in %s but could not read the commit back: %v", wt.path, err)
		return out
	}
	out.Commit = sha

	remote := delivery.Remote(ctx, wt.path, s.remote)
	ref := "refs/heads/" + wt.branch
	// No --force, and no lease variant either: this branch belongs to the run that
	// created it, and a push the remote refuses is a fact to report, never one to
	// overrule. --no-verify for the same reason the commit has it.
	if _, err := s.run(ctx, wt.path, "push", "--no-verify", "--", remote, ref+":"+ref); err != nil {
		out.Status, out.Detail = Failed, fmt.Sprintf(
			"committed %s in %s, but pushing %s to %s failed: %v - the work is safe on this machine and NOT reachable from another, so no branch was recorded",
			short(sha), wt.path, wt.branch, remote, err)
		return out
	}

	out.Status, out.Branch = Saved, wt.branch
	out.Detail = fmt.Sprintf("committed the uncommitted work in %s as %s and pushed it to %s/%s",
		wt.path, short(sha), remote, wt.branch)
	return out
}

// commitMessage writes the commit a later reader has to be able to judge. It says
// what this is in the subject, because a one-line log is all most readers see,
// and says plainly in the body that nothing here was reviewed - a salvage of a
// half-applied refactor looks exactly like a salvage of good work.
func commitMessage(label, taskID string, wt worktree) (subject, body string) {
	name := label
	if name == "" {
		name = taskID
	}
	subject = fmt.Sprintf("WIP salvage: %s (unreviewed, may not build)", name)
	body = strings.Join([]string{
		"The session working this task ended without committing - killed by a usage",
		"limit, a crash, or a stream that could not be read. The driver committed its",
		"worktree verbatim so the work is recoverable from another machine instead of",
		"being redone from nothing.",
		"",
		"Nothing here has been reviewed, built or tested, and no human or agent chose",
		"this as a stopping point. Read it before building on it, and discard it if it",
		"is a half-applied change.",
		"",
		"Task: " + taskID,
		"Worktree: " + wt.path,
		"Salvaged: " + time.Now().Format(time.RFC3339),
	}, "\n")
	return subject, body
}

// worktree is one checked-out tree and the branch it is on.
type worktree struct {
	path   string
	branch string
}

// findWorktrees collects the trees whose checked-out branch carries the task's
// id prefix, across every candidate repository, deduplicated by path.
//
// Deduplication matters because linked worktrees SHARE a ref database: the same
// tree is listed by every repository it belongs to, and two listings of one tree
// must not read as an ambiguity.
func (s *Salvager) findWorktrees(ctx context.Context, repos []string, prefix string) []worktree {
	seen := map[string]bool{}
	var out []worktree
	for _, r := range repos {
		listing, err := s.run(ctx, r, "worktree", "list", "--porcelain")
		if err != nil {
			continue
		}
		for _, wt := range parseWorktrees(listing) {
			if !matchesPrefix(wt.branch, prefix) {
				continue
			}
			key := realPath(wt.path)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, wt)
		}
	}
	return out
}

// parseWorktrees reads `git worktree list --porcelain`: blank-line-separated
// blocks, each led by `worktree <path>`, with `branch refs/heads/<name>` when one
// is checked out. A `detached` or `bare` block simply has no branch line, and is
// therefore never matched - which is the behaviour we want and not a special
// case.
func parseWorktrees(listing string) []worktree {
	var out []worktree
	var cur worktree
	flush := func() {
		if cur.path != "" && cur.branch != "" {
			out = append(out, cur)
		}
		cur = worktree{}
	}
	for _, line := range strings.Split(listing, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.TrimSpace(line) == "":
			flush()
		case strings.HasPrefix(line, "worktree "):
			flush()
			cur.path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch refs/heads/"):
			cur.branch = strings.TrimPrefix(line, "branch refs/heads/")
		}
	}
	flush()
	return out
}

// branchPrefix derives `clanker/<first 8 of task id>` - the part of the branch
// name that identifies the TASK rather than its title.
//
// The id must spell hex, which is what a task id is. Without that check a
// degenerate id (empty, a ref like "CLA-314", a path fragment) would still
// produce a prefix, and a prefix is what decides which tree gets committed in.
func branchPrefix(taskID string) (string, bool) {
	if len(taskID) < branchIDLen {
		return "", false
	}
	head := taskID[:branchIDLen]
	for _, r := range head {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return "", false
		}
	}
	return "clanker/" + head, true
}

// matchesPrefix reports whether a branch belongs to the task the prefix names.
// The character after the id must be the separator the plane puts there (or
// nothing at all), so `clanker/cc561415aa-...` - a different task whose id merely
// starts the same way - is not swept up.
func matchesPrefix(branch, prefix string) bool {
	rest, ok := strings.CutPrefix(branch, prefix)
	return ok && (rest == "" || strings.HasPrefix(rest, "-"))
}

// inProgressOp names the git operation a worktree is in the middle of, or "" if
// it is not in one. Read from the per-worktree git dir, which is where git keeps
// these - a linked worktree's merge state is its own, not the repository's.
func (s *Salvager) inProgressOp(ctx context.Context, dir string) string {
	gitDir, err := s.run(ctx, dir, "rev-parse", "--absolute-git-dir")
	if err != nil || gitDir == "" {
		return ""
	}
	for _, probe := range []struct{ path, name string }{
		{"MERGE_HEAD", "merge"},
		{"rebase-merge", "rebase"},
		{"rebase-apply", "rebase"},
		{"CHERRY_PICK_HEAD", "cherry-pick"},
		{"REVERT_HEAD", "revert"},
		{"BISECT_LOG", "bisect"},
	} {
		if _, err := os.Stat(filepath.Join(gitDir, probe.path)); err == nil {
			return probe.name
		}
	}
	return ""
}

// run executes one git command in dir and returns its trimmed stdout.
//
// The environment is deliberately hostile to interaction, and the WaitDelay is
// not decoration: `git push` spawns helpers (ssh, git-remote-https, a credential
// helper) that inherit these pipes, and os/exec's Wait blocks until every
// inheritor closes its write end - so a killed git can still leave the call
// sitting there. Same reasoning, and the same numbers, as internal/delivery.
func (s *Salvager) run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, s.gitBin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=echo",
		"SSH_ASKPASS=echo",
		"GIT_SSH_COMMAND=ssh -oBatchMode=yes -oConnectTimeout=10",
		"GCM_INTERACTIVE=never",
	)
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("git %s: %w: %s", args[0], err, firstLine(msg))
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func realPath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

func joinPaths(w []worktree) string {
	paths := make([]string, 0, len(w))
	for _, one := range w {
		paths = append(paths, one.path)
	}
	return strings.Join(paths, ", ")
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
