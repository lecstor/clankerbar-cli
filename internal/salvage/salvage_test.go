package salvage

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Rescuing the work a killed session left uncommitted (CLA-314). Everything here
// runs against REAL git repositories and a real (bare) remote, deliberately: this
// package's whole job is to leave a git repository in a particular state, and a
// mock of git would only assert that we called the commands we decided to call.

const (
	// A task id shaped like the plane's: the first 8 characters are what a derived
	// branch name carries.
	taskID = "abcd1234-1111-2222-3333-444455556666"
	prefix = "clanker/abcd1234"
)

func TestSalvage_CommitsAndPushesADirtyWorktree(t *testing.T) {
	e := newEnv(t)
	// Both shapes of uncommitted work: an edit to a tracked file, and a file git
	// has never seen. The second is the one an `add -u` would silently drop, and it
	// is routinely the new module a session was halfway through writing.
	write(t, filepath.Join(e.wt, "tracked.txt"), "edited by the session")
	write(t, filepath.Join(e.wt, "brand-new.go"), "package new")

	out := New(e.root, "").Salvage(context.Background(), taskID, "CLA-314")

	if out.Status != Saved {
		t.Fatalf("status = %q (%s), want saved", out.Status, out.Detail)
	}
	if out.Branch != e.branch {
		t.Errorf("recorded branch %q, want %q", out.Branch, e.branch)
	}
	if out.Commit == "" {
		t.Fatal("no commit id reported")
	}
	if !out.Saved() {
		t.Error("Saved() is false on a saved outcome, so the driver would record nothing")
	}

	// The point of the push: another machine can fetch it.
	if remote := lsRemote(t, e.repo, e.branch); remote != out.Commit {
		t.Errorf("origin/%s is at %q, want the salvage commit %q", e.branch, remote, out.Commit)
	}
	// Both files, and only through the salvage commit.
	files := git(t, e.wt, "show", "--pretty=format:", "--name-only", "HEAD")
	if !strings.Contains(files, "tracked.txt") || !strings.Contains(files, "brand-new.go") {
		t.Errorf("the salvage commit does not carry both files:\n%s", files)
	}
	// A reader has to be able to tell instantly that this was not a considered
	// stopping point.
	msg := git(t, e.wt, "log", "-1", "--pretty=format:%s%n%b", "HEAD")
	for _, want := range []string{"WIP salvage", "CLA-314", "unreviewed", "discard it", taskID} {
		if !strings.Contains(msg, want) {
			t.Errorf("commit message is missing %q:\n%s", want, msg)
		}
	}
	if left := git(t, e.wt, "status", "--porcelain"); left != "" {
		t.Errorf("worktree still dirty after salvage:\n%s", left)
	}
}

// A clean tree must record NOTHING. An empty hand-off sends the next clanker -
// routinely on another machine - to fetch a branch with nothing on it, which the
// protocol calls out as worse than recording none at all.
func TestSalvage_CleanWorktreeRecordsNoBranch(t *testing.T) {
	e := newEnv(t)
	before := git(t, e.wt, "rev-parse", "HEAD")

	out := New(e.root, "").Salvage(context.Background(), taskID, "CLA-314")

	if out.Status != Nothing {
		t.Fatalf("status = %q (%s), want nothing", out.Status, out.Detail)
	}
	if out.Branch != "" || out.Commit != "" {
		t.Errorf("a clean tree produced branch=%q commit=%q; both must be empty", out.Branch, out.Commit)
	}
	if after := git(t, e.wt, "rev-parse", "HEAD"); after != before {
		t.Errorf("HEAD moved on a clean worktree: %s -> %s", before, after)
	}
	if lsRemote(t, e.repo, e.branch) != "" {
		t.Error("a clean tree pushed a branch to the remote")
	}
}

// The slug half of a derived branch name comes from the TITLE, so a task
// retitled mid-flight computes a name that no longer matches the worktree that
// exists. Matching on the id prefix is what stops that being a silent miss - and
// the API takes no branch name at all, which is what makes it structural.
func TestSalvage_MatchesARetitledTaskOnTheIDPrefix(t *testing.T) {
	e := newEnvOnBranch(t, prefix+"-the-title-it-had-when-the-worktree-was-cut")
	write(t, filepath.Join(e.wt, "work.txt"), "hours of it")

	out := New(e.root, "").Salvage(context.Background(), taskID, "CLA-314")

	if out.Status != Saved || out.Branch != e.branch {
		t.Fatalf("a retitled task was missed: status=%q branch=%q (%s)", out.Status, out.Branch, out.Detail)
	}
}

// The one place this can destroy work: committing in a tree that belongs to
// somebody else. Neither a sibling task's worktree nor the checkout sitting on
// `main` may be touched, however dirty they are.
func TestSalvage_LeavesTreesItCannotTieToTheTask(t *testing.T) {
	e := newEnvOnBranch(t, "clanker/deadbeef-a-different-task")
	write(t, filepath.Join(e.wt, "someone-elses.txt"), "not ours")
	write(t, filepath.Join(e.repo, "on-main.txt"), "not ours either")
	wtHead, repoHead := git(t, e.wt, "rev-parse", "HEAD"), git(t, e.repo, "rev-parse", "HEAD")

	out := New(e.root, "").Salvage(context.Background(), taskID, "CLA-314")

	if out.Status != Nothing {
		t.Fatalf("status = %q (%s), want nothing - no tree here belongs to this task", out.Status, out.Detail)
	}
	if got := git(t, e.wt, "rev-parse", "HEAD"); got != wtHead {
		t.Errorf("committed in another task's worktree: HEAD %s -> %s", wtHead, got)
	}
	if got := git(t, e.repo, "rev-parse", "HEAD"); got != repoHead {
		t.Errorf("committed on main: HEAD %s -> %s", repoHead, got)
	}
	if git(t, e.wt, "status", "--porcelain") == "" || git(t, e.repo, "status", "--porcelain") == "" {
		t.Error("someone else's uncommitted work was staged or committed")
	}
}

// A tree mid-merge is not a state to preserve, it is a state to leave alone:
// `add -A && commit` there would record conflict markers as a resolution nobody
// chose.
func TestSalvage_RefusesAWorktreeInTheMiddleOfAMerge(t *testing.T) {
	e := newEnv(t)
	// Divergent edits to one file: the worktree's branch and main.
	write(t, filepath.Join(e.wt, "tracked.txt"), "the branch version")
	git(t, e.wt, "commit", "-am", "branch edit")
	write(t, filepath.Join(e.repo, "tracked.txt"), "the main version")
	git(t, e.repo, "commit", "-am", "main edit")
	if _, err := gitErr(t, e.wt, "merge", "main"); err == nil {
		t.Fatal("the merge was expected to conflict; the fixture no longer sets this case up")
	}
	before := git(t, e.wt, "rev-parse", "HEAD")

	out := New(e.root, "").Salvage(context.Background(), taskID, "CLA-314")

	if out.Status != Refused {
		t.Fatalf("status = %q (%s), want refused", out.Status, out.Detail)
	}
	if !strings.Contains(out.Detail, "merge") || !strings.Contains(out.Detail, e.wt) {
		t.Errorf("the refusal must name the operation and the tree to look at, got: %s", out.Detail)
	}
	if out.Branch != "" {
		t.Errorf("a refused salvage recorded branch %q", out.Branch)
	}
	if after := git(t, e.wt, "rev-parse", "HEAD"); after != before {
		t.Errorf("a conflicted merge was committed: %s -> %s", before, after)
	}
}

// Two trees answering to one task is an ambiguity, not a coin toss: committing in
// the wrong one is worse than committing in neither.
func TestSalvage_RefusesWhenTwoTreesClaimTheTask(t *testing.T) {
	e := newEnv(t)
	// A second, independent repository under the same workdir - a review clone
	// beside the session's tree - carrying a branch with the same task prefix.
	other := filepath.Join(e.root, "other")
	initRepo(t, other)
	otherWT := filepath.Join(e.root, "other-wt", "abcd1234")
	git(t, other, "worktree", "add", "-b", prefix+"-same-task", otherWT)
	write(t, filepath.Join(e.wt, "ours.txt"), "x")
	before := git(t, e.wt, "rev-parse", "HEAD")

	out := New(e.root, "").Salvage(context.Background(), taskID, "CLA-314")

	if out.Status != Refused {
		t.Fatalf("status = %q (%s), want refused", out.Status, out.Detail)
	}
	if !strings.Contains(out.Detail, e.wt) || !strings.Contains(out.Detail, otherWT) {
		t.Errorf("the refusal must name both candidates, got: %s", out.Detail)
	}
	if after := git(t, e.wt, "rev-parse", "HEAD"); after != before {
		t.Errorf("committed in one of two ambiguous trees: %s -> %s", before, after)
	}
}

// A push that fails leaves the commit on this machine and NOTHING on the task: a
// recorded branch is a promise that another machine can fetch it.
func TestSalvage_APushThatFailsRecordsNoBranch(t *testing.T) {
	e := newEnv(t)
	git(t, e.repo, "remote", "set-url", "origin", filepath.Join(e.root, "not-a-remote.git"))
	write(t, filepath.Join(e.wt, "work.txt"), "hours of it")
	before := git(t, e.wt, "rev-parse", "HEAD")

	out := New(e.root, "").Salvage(context.Background(), taskID, "CLA-314")

	if out.Status != Failed {
		t.Fatalf("status = %q (%s), want failed", out.Status, out.Detail)
	}
	if out.Branch != "" {
		t.Errorf("recorded branch %q for work that never reached the remote", out.Branch)
	}
	// The commit still happened, and the detail has to say so - the work is
	// recoverable on this machine, and a reader who thinks it was lost will redo it.
	if out.Commit == "" {
		t.Error("no commit reported; the local commit is the half that did work")
	}
	if after := git(t, e.wt, "rev-parse", "HEAD"); after == before {
		t.Error("nothing was committed locally, so the work is still only in the working tree")
	}
	if !strings.Contains(out.Detail, "no branch was recorded") {
		t.Errorf("the failure must say the hand-off was not recorded, got: %s", out.Detail)
	}
}

// A task id that does not spell one cannot select a tree. Without this, a
// degenerate id would still produce a prefix - and the prefix is what decides
// which tree gets committed in.
func TestBranchPrefix(t *testing.T) {
	tests := []struct {
		id   string
		want string
		ok   bool
	}{
		{taskID, prefix, true},
		{"abcd1234", prefix, true},
		{"ABCD1234-1111", "clanker/ABCD1234", true},
		{"abcd123", "", false},       // too short to identify anything
		{"", "", false},              // no claim was observed
		{"CLA-314", "", false},       // a ref, not an id
		{"../../etc", "", false},     // not hex, and not a name we would ever build
		{"abcd123z-1111", "", false}, // one character off hex
	}
	for _, tc := range tests {
		got, ok := branchPrefix(tc.id)
		if got != tc.want || ok != tc.ok {
			t.Errorf("branchPrefix(%q) = (%q, %t), want (%q, %t)", tc.id, got, ok, tc.want, tc.ok)
		}
	}
}

func TestMatchesPrefix(t *testing.T) {
	tests := []struct {
		branch string
		want   bool
	}{
		{prefix + "-a-title", true},
		{prefix, true},                        // no slug at all
		{prefix + "abcd-another-task", false}, // a longer id that merely starts the same
		{"clanker/deadbeef-other", false},
		{"main", false},
		{"staging", false},
		{"feature/" + prefix, false},
	}
	for _, tc := range tests {
		if got := matchesPrefix(tc.branch, prefix); got != tc.want {
			t.Errorf("matchesPrefix(%q) = %t, want %t", tc.branch, got, tc.want)
		}
	}
}

// A detached or bare entry carries no branch line, so it can never be matched -
// and that is worth pinning, because a detached HEAD is exactly the state a
// half-done rebase leaves behind.
func TestParseWorktrees(t *testing.T) {
	listing := strings.Join([]string{
		"worktree /repo",
		"HEAD 1111111111111111111111111111111111111111",
		"branch refs/heads/main",
		"",
		"worktree /repo-wt/abcd1234",
		"HEAD 2222222222222222222222222222222222222222",
		"branch refs/heads/" + prefix + "-a-title",
		"",
		"worktree /repo-wt/detached",
		"HEAD 3333333333333333333333333333333333333333",
		"detached",
		"",
		"worktree /mirror.git",
		"bare",
		"",
	}, "\n")

	got := parseWorktrees(listing)
	want := []worktree{
		{path: "/repo", branch: "main"},
		{path: "/repo-wt/abcd1234", branch: prefix + "-a-title"},
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d worktrees, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("worktree %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// --- fixtures ---------------------------------------------------------------

type env struct {
	root   string // the driver's workdir: a parent holding several repositories
	remote string // a bare repository standing in for the git host
	repo   string // the checkout, on main
	wt     string // the per-task worktree the session worked in
	branch string
}

func newEnv(t *testing.T) *env { return newEnvOnBranch(t, prefix+"-a-session-killed-mid-task") }

// newEnvOnBranch builds root/{remote.git, repo, repo-wt/abcd1234}: the layout the
// per-task worktree convention produces, with a real remote to push to.
func newEnvOnBranch(t *testing.T, branch string) *env {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	e := &env{
		root:   root,
		remote: filepath.Join(root, "remote.git"),
		repo:   filepath.Join(root, "repo"),
		wt:     filepath.Join(root, "repo-wt", "abcd1234"),
		branch: branch,
	}
	git(t, root, "init", "--bare", "-b", "main", e.remote)
	initRepo(t, e.repo)
	git(t, e.repo, "remote", "add", "origin", e.remote)
	git(t, e.repo, "push", "origin", "main")
	git(t, e.repo, "worktree", "add", "-b", branch, e.wt)
	return e
}

// initRepo makes a checkout with one commit on main, configured so a commit never
// depends on the developer's global git identity.
func initRepo(t *testing.T, dir string) {
	t.Helper()
	git(t, filepath.Dir(dir), "init", "-b", "main", dir)
	git(t, dir, "config", "user.email", "salvage@example.test")
	git(t, dir, "config", "user.name", "Salvage Test")
	git(t, dir, "config", "commit.gpgsign", "false")
	write(t, filepath.Join(dir, "tracked.txt"), "base")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "base")
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitErr(t, dir, args...)
	if err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return out
}

// gitErr runs git and hands the caller the failure, for the fixtures that need a
// command to fail (setting up a conflicted merge).
//
// The environment is insulated from the developer's own git config: a system-wide
// `commit.gpgsign = true` would otherwise fail these tests on their machine and
// nowhere else.
func gitErr(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME="+t.TempDir(),
		"GIT_AUTHOR_NAME=Salvage Test", "GIT_AUTHOR_EMAIL=salvage@example.test",
		"GIT_COMMITTER_NAME=Salvage Test", "GIT_COMMITTER_EMAIL=salvage@example.test",
	)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// lsRemote returns the remote's tip for a branch, or "" when it has none.
func lsRemote(t *testing.T, repo, branch string) string {
	t.Helper()
	out := git(t, repo, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	fields := strings.Fields(out)
	if len(fields) < 2 {
		return ""
	}
	return fields[0]
}

func write(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
