package cli

// Stranded-commit reporting (CLA-533). Everything here runs against REAL git
// repositories and a real (bare) remote, deliberately: the check's contract is
// to read a git repository without changing it, and a mock of git would only
// assert that we called the commands we decided to call.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/delivery"
)

// realGitEnv is okEnv with the two git seams swapped for the real git: the
// check's contract is to read a git repository without changing it, and a mock
// of git would only assert that we called the commands we decided to call.
func realGitEnv(t *testing.T) doctorEnv {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	e := okEnv()
	e.repos = delivery.Repos
	e.gitRun = deployGitRun
	return e
}

// strandedEnv is the layout the per-task worktree convention produces: a bare
// remote, a checkout on main, and a per-task worktree.
type strandedEnv struct {
	root   string
	remote string
	repo   string
	wt     string
}

// strandedEnvOnBranch builds root/{remote.git, repo, repo-wt/abcd1234}: the
// same fixture shape internal/salvage uses, with a real remote to push to.
func strandedEnvOnBranch(t *testing.T, branch string) *strandedEnv {
	t.Helper()
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	e := &strandedEnv{
		root:   root,
		remote: filepath.Join(root, "remote.git"),
		repo:   filepath.Join(root, "repo"),
		wt:     filepath.Join(root, "repo-wt", "abcd1234"),
	}
	gitT(t, root, "init", "--bare", "-b", "main", e.remote)
	initRepoT(t, e.repo)
	gitT(t, e.repo, "remote", "add", "origin", e.remote)
	gitT(t, e.repo, "push", "-u", "origin", "main")
	gitT(t, e.repo, "worktree", "add", "-b", branch, e.wt)
	return e
}

func initRepoT(t *testing.T, dir string) {
	t.Helper()
	gitT(t, filepath.Dir(dir), "init", "-b", "main", dir)
	gitT(t, dir, "config", "user.email", "stranded@example.test")
	gitT(t, dir, "config", "user.name", "Stranded Test")
	gitT(t, dir, "config", "commit.gpgsign", "false")
	writeT(t, filepath.Join(dir, "tracked.txt"), "base")
	gitT(t, dir, "add", ".")
	gitT(t, dir, "commit", "-m", "base")
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeT(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A deliberately stranded commit — committed on a branch no remote has ever
// seen — must be reported, naming the ref, the count and the newest commit.
func TestStranded_FindsAStrandedBranchCommit(t *testing.T) {
	e := strandedEnvOnBranch(t, "clanker/abcd1234-a-session-killed-mid-task")
	gitT(t, e.wt, "commit", "-q", "--allow-empty", "-m", "stranded feature work")

	c := checkStranded(context.Background(), validCfgIn(t, e.root), realGitEnv(t))

	if c.status != warn {
		t.Fatalf("status = %v (%s), want WARN", c.status, c.detail)
	}
	if len(c.info) != 1 {
		t.Fatalf("got %d finding lines, want 1: %v", len(c.info), c.info)
	}
	line := c.info[0]
	for _, want := range []string{
		"clanker/abcd1234-a-session-killed-mid-task",
		"1 commit",
		`newest "stranded feature work"`,
		time.Now().Format("2006-01-02"),
	} {
		if !strings.Contains(line, want) {
			t.Errorf("finding %q is missing %q", line, want)
		}
	}
}

// A detached worktree HEAD is a tip no branch names, so the check must read it
// from the worktree listing and name the tree — the only handle that commit
// has.
func TestStranded_FindsADetachedWorktreeHead(t *testing.T) {
	e := strandedEnvOnBranch(t, "clanker/abcd1234-pushed-branch")
	gitT(t, e.wt, "push", "-u", "origin", "clanker/abcd1234-pushed-branch")
	detached := filepath.Join(e.root, "repo-wt", "detached")
	gitT(t, e.repo, "worktree", "add", "--detach", detached)
	gitT(t, detached, "commit", "-q", "--allow-empty", "-m", "work lost to a swept temp dir")

	c := checkStranded(context.Background(), validCfgIn(t, e.root), realGitEnv(t))

	if c.status != warn {
		t.Fatalf("status = %v (%s), want WARN", c.status, c.detail)
	}
	if len(c.info) != 1 {
		t.Fatalf("got %d finding lines, want 1: %v", len(c.info), c.info)
	}
	line := c.info[0]
	for _, want := range []string{
		"detached HEAD at " + detached,
		"1 commit",
		`newest "work lost to a swept temp dir"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("finding %q is missing %q", line, want)
		}
	}
}

// A repo where every commit is reachable from a remote-tracking ref says so
// and adds no noise.
func TestStranded_EverythingPushedIsQuiet(t *testing.T) {
	e := strandedEnvOnBranch(t, "clanker/abcd1234-pushed")
	gitT(t, e.wt, "commit", "-q", "--allow-empty", "-m", "pushed work")
	gitT(t, e.wt, "push", "-u", "origin", "clanker/abcd1234-pushed")

	c := checkStranded(context.Background(), validCfgIn(t, e.root), realGitEnv(t))

	if c.status != pass {
		t.Fatalf("status = %v (%s), want PASS", c.status, c.detail)
	}
	if !strings.Contains(c.detail, "no commit exists only on this machine") {
		t.Errorf("detail should say nothing is stranded, got %q", c.detail)
	}
	if len(c.info) != 0 {
		t.Errorf("a clean repo must add no lines, got %v", c.info)
	}
}

// The check writes nothing: no delete, move, push, rebase or prune. The whole
// repository tree (refs and all) must be byte-identical after a run that
// reports findings.
func TestStranded_LeavesTheRepoByteIdentical(t *testing.T) {
	e := strandedEnvOnBranch(t, "clanker/abcd1234-a-stranded-branch")
	gitT(t, e.wt, "commit", "-q", "--allow-empty", "-m", "stranded")
	detached := filepath.Join(e.root, "repo-wt", "detached")
	gitT(t, e.repo, "worktree", "add", "--detach", detached)
	gitT(t, detached, "commit", "-q", "--allow-empty", "-m", "also stranded")

	before := treeSnapshot(t, e.repo)
	c := checkStranded(context.Background(), validCfgIn(t, e.root), realGitEnv(t))
	after := treeSnapshot(t, e.repo)

	if c.status != warn || len(c.info) != 2 {
		t.Fatalf("the fixture must strand both tips for this test to mean anything: %v (%s)", c.info, c.detail)
	}
	if !maps.Equal(before, after) {
		for k, v := range before {
			if after[k] != v {
				t.Errorf("changed: %s %s -> %s", k, v, after[k])
			}
		}
		for k, v := range after {
			if _, ok := before[k]; !ok {
				t.Errorf("added: %s %s", k, v)
			}
		}
		t.Fatal("the repository tree changed while the check ran")
	}
}

// treeSnapshot fingerprints every entry under dir — path, mode and content
// hash — so a test can prove the check changed nothing, refs included.
func treeSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			out[rel] = fmt.Sprintf("d %o", info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			out[rel] = "l " + target
		default:
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out[rel] = fmt.Sprintf("f %o %x", info.Mode().Perm(), sha256.Sum256(data))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", dir, err)
	}
	return out
}

// A repo with no remote-tracking refs at all must not dump its whole history —
// one finding names the state.
func TestStranded_NoRemoteTrackingRefsIsOneFinding(t *testing.T) {
	e := strandedEnvOnBranch(t, "clanker/abcd1234-x")
	gitT(t, e.repo, "remote", "remove", "origin")
	gitT(t, e.wt, "commit", "-q", "--allow-empty", "-m", "local only")

	c := checkStranded(context.Background(), validCfgIn(t, e.root), realGitEnv(t))

	if c.status != warn {
		t.Fatalf("status = %v (%s), want WARN", c.status, c.detail)
	}
	if len(c.info) != 1 {
		t.Fatalf("got %d finding lines, want exactly one (not a per-commit dump): %v", len(c.info), c.info)
	}
	line := c.info[0]
	for _, want := range []string{"no remote-tracking refs", "2 commits", `newest "local only"`} {
		if !strings.Contains(line, want) {
			t.Errorf("finding %q is missing %q", line, want)
		}
	}
}

// Multi-project scoping: with a slug configured, only that project's repos are
// reported on, and every finding line names its repo when more than one is in
// scope — a stranded commit in an unrelated checkout is not this config's
// noise.
func TestStranded_ScopesToTheConfiguredProjectsRepos(t *testing.T) {
	e := strandedEnvOnBranch(t, "clanker/abcd1234-ours")
	gitT(t, e.wt, "commit", "-q", "--allow-empty", "-m", "ours stranded")

	other := filepath.Join(e.root, "other") // does not match the slug
	initRepoT(t, other)
	gitT(t, other, "commit", "-q", "--allow-empty", "-m", "not this project's")

	repoOther := filepath.Join(e.root, "repo-other") // matches the slug prefix
	initRepoT(t, repoOther)
	gitT(t, repoOther, "commit", "-q", "--allow-empty", "-m", "this project's too")

	cfg := validCfgIn(t, e.root)
	cfg.Projects = []config.Project{{Slug: "repo"}}

	c := checkStranded(context.Background(), cfg, realGitEnv(t))

	if c.status != warn {
		t.Fatalf("status = %v (%s), want WARN", c.status, c.detail)
	}
	if len(c.info) != 2 {
		t.Fatalf("got %d finding lines, want the two repos that match the slug: %v", len(c.info), c.info)
	}
	for _, want := range []string{e.repo + ": ", repoOther + ": "} {
		found := false
		for _, line := range c.info {
			if strings.HasPrefix(line, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("no finding line is prefixed with %q: %v", want, c.info)
		}
	}
	for _, line := range c.info {
		if strings.HasPrefix(line, other+": ") {
			t.Errorf("the unrelated repo leaked into the report: %q", line)
		}
	}
}

// A declared `repos` entry (CLA-437) names a checkout the project owns even
// when it sits outside the workdir; its stranded commits must be reported.
func TestStranded_CoversDeclaredReposOutsideTheWorkdir(t *testing.T) {
	e := strandedEnvOnBranch(t, "clanker/abcd1234-ours")
	gitT(t, e.wt, "commit", "-q", "--allow-empty", "-m", "declared but stranded")

	// A second checkout the config declares explicitly, NOT under the workdir.
	outside := t.TempDir()
	initRepoT(t, outside)
	gitT(t, outside, "commit", "-q", "--allow-empty", "-m", "outside the workdir")

	cfg := validCfgIn(t, e.root)
	cfg.Projects = []config.Project{{Slug: "repo", Repos: map[string]string{"repo-other": outside}}}

	c := checkStranded(context.Background(), cfg, realGitEnv(t))

	if c.status != warn {
		t.Fatalf("status = %v (%s), want WARN", c.status, c.detail)
	}
	var declared, outsideFound bool
	for _, line := range c.info {
		if strings.Contains(line, "declared but stranded") {
			declared = true
		}
		if strings.Contains(line, "outside the workdir") {
			outsideFound = true
		}
	}
	if !declared || !outsideFound {
		t.Errorf("the declared checkout's stranded commit is missing from: %v", c.info)
	}
}

// A repo whose git commands fail entirely is counted as UNCHECKED, never as one
// that holds stranded commits: its only "finding" is the error line, and the
// summary sentence must not read it as work on no remote.
func TestStranded_UncheckableRepoCountsAsUncheckedNotAsFindings(t *testing.T) {
	e := okEnv()
	e.repos = func(context.Context, string) []string {
		return []string{filepath.Join(t.TempDir(), "repo-a"), filepath.Join(t.TempDir(), "repo-b")}
	}
	e.gitRun = func(context.Context, string, ...string) (string, error) {
		return "", fmt.Errorf("boom")
	}

	c := checkStranded(context.Background(), validCfgIn(t, t.TempDir()), e)

	if c.status != warn {
		t.Fatalf("status = %v (%s), want WARN", c.status, c.detail)
	}
	if !strings.Contains(c.detail, "could not be fully checked") {
		t.Errorf("detail should say the repos could not be fully checked, got %q", c.detail)
	}
	if strings.Contains(c.detail, "hold commits") {
		t.Errorf("an uncheckable repo must not be reported as holding stranded commits: %q", c.detail)
	}
	if len(c.info) != 2 {
		t.Fatalf("got %d lines, want the two error lines: %v", len(c.info), c.info)
	}
}

// One repo that cannot be read and one that holds a stranded commit: the
// summary must count them separately — "X could not be fully checked, and Y
// hold commits" — never merging the error repo into the findings count.
func TestStranded_MixedUncheckableAndFindingRepos(t *testing.T) {
	good := filepath.Join(t.TempDir(), "repo-good")
	bad := filepath.Join(t.TempDir(), "repo-bad")
	e := okEnv()
	e.repos = func(context.Context, string) []string { return []string{good, bad} }
	e.gitRun = func(_ context.Context, dir string, args ...string) (string, error) {
		if dir == bad {
			return "", fmt.Errorf("boom")
		}
		switch args[0] {
		case "for-each-ref":
			// refs/heads listing: one branch tip; refs/remotes listing: one remote.
			if len(args) > 1 && args[len(args)-1] == "refs/remotes" {
				return "refs/remotes/origin/main", nil
			}
			return "main 1111111111111111111111111111111111111111", nil
		case "worktree":
			return "", nil
		case "rev-list":
			return "1111111111111111111111111111111111111111", nil
		case "show":
			return "stranded work\n2026-08-28", nil
		}
		return "", nil
	}

	c := checkStranded(context.Background(), validCfgIn(t, t.TempDir()), e)

	if c.status != warn {
		t.Fatalf("status = %v (%s), want WARN", c.status, c.detail)
	}
	for _, want := range []string{"1 of 2 repos could not be fully checked", "and 1 hold commits"} {
		if !strings.Contains(c.detail, want) {
			t.Errorf("detail %q is missing %q", c.detail, want)
		}
	}
	if len(c.info) != 2 {
		t.Fatalf("got %d lines, want the error line and the finding line: %v", len(c.info), c.info)
	}
}

// The same repository reachable by two paths — a declared `repos` entry
// pointing at a WORKTREE of a repo the workdir scan also found — must be
// checked ONCE: two trees of one repo share a ref database, so checking both
// would double-report every finding and double-count the repo in the summary.
func TestStranded_OneRepoReachableByTwoPathsIsCheckedOnce(t *testing.T) {
	e := strandedEnvOnBranch(t, "clanker/abcd1234-ours")
	gitT(t, e.wt, "commit", "-q", "--allow-empty", "-m", "stranded once, reported once")

	// The declared path is the WORKTREE, not the main checkout: a different
	// directory, the same repository.
	cfg := validCfgIn(t, e.root)
	cfg.Projects = []config.Project{{Slug: "repo", Repos: map[string]string{"owner/repo": e.wt}}}

	c := checkStranded(context.Background(), cfg, realGitEnv(t))

	if c.status != warn {
		t.Fatalf("status = %v (%s), want WARN", c.status, c.detail)
	}
	if !strings.Contains(c.detail, "1 of 1 repo") || !strings.Contains(c.detail, "hold commits") {
		t.Errorf("the repo must be counted once, got %q", c.detail)
	}
	if len(c.info) != 1 {
		t.Fatalf("got %d finding lines, want exactly one (the same commits reported once): %v", len(c.info), c.info)
	}
	if !strings.Contains(c.info[0], "clanker/abcd1234-ours") {
		t.Errorf("the one line should name the stranded branch: %q", c.info[0])
	}
}

// A repo whose per-tip comparison fails everywhere established no stranded
// commit: it counts as UNCHECKED ("could not be fully checked"), never as one
// that holds commits, and its error line names the repo even when it is the
// only one in scope — the remedy points at "the repo named above".
func TestStranded_RepoWhoseComparisonsAllFailCountsAsUnchecked(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo-a")
	e := okEnv()
	e.repos = func(context.Context, string) []string { return []string{repo} }
	e.gitRun = func(_ context.Context, dir string, args ...string) (string, error) {
		switch args[0] {
		case "rev-parse":
			return "/repo-a/.git", nil
		case "for-each-ref":
			if len(args) > 1 && args[len(args)-1] == "refs/remotes" {
				return "refs/remotes/origin/main", nil
			}
			return "main 1111111111111111111111111111111111111111", nil
		case "worktree":
			return "", nil
		case "rev-list":
			return "", errors.New("boom")
		}
		return "", nil
	}

	c := checkStranded(context.Background(), validCfgIn(t, t.TempDir()), e)

	if c.status != warn {
		t.Fatalf("status = %v (%s), want WARN", c.status, c.detail)
	}
	if !strings.Contains(c.detail, "1 of 1 repo could not be fully checked") {
		t.Errorf("detail should say the repo could not be fully checked, got %q", c.detail)
	}
	if strings.Contains(c.detail, "hold commits") {
		t.Errorf("a repo whose comparison failed must not be reported as holding stranded commits: %q", c.detail)
	}
	if len(c.info) != 1 {
		t.Fatalf("got %d lines, want the one error line: %v", len(c.info), c.info)
	}
	if !strings.HasPrefix(c.info[0], repo+": ") {
		t.Errorf("the error line must name the repo even in single-repo mode: %q", c.info[0])
	}
	if !strings.Contains(c.info[0], "could not compare against the remotes") {
		t.Errorf("the error line should say what failed: %q", c.info[0])
	}
}
