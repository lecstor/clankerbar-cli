package delivery

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests run against REAL temporary git repositories, with a real (local,
// bare) remote, deliberately: the whole point of the check is that it agrees with
// git, and a mock would only assert that the code agrees with itself. Every case
// below is a failure that actually happened or could happen on an overnight run.

func TestBranchPushedAndCommitMerged_Passes(t *testing.T) {
	env := newEnv(t)
	env.commit("main", "base.txt", "base")
	env.push("main")

	env.checkout("clanker/feature")
	env.commit("clanker/feature", "work.txt", "work")
	env.push("clanker/feature")
	sha := env.head()

	// Land it on the integration branch the way a merged PR would.
	env.checkout("main")
	env.git("merge", "--no-ff", "-m", "merge feature", "clanker/feature")
	env.push("main")

	rep := verify(t, env.work, Claim{
		Label: "CLA-253", Branch: "clanker/feature",
		Commit: sha, IntegrationBranch: "main",
	})

	if rep.Failed() {
		t.Fatalf("pushed and merged should pass, got %s", render(rep))
	}
	mustStatus(t, rep, BranchPushed, Pass)
	mustStatus(t, rep, CommitMerged, Pass)

	verified, ran := rep.MergeVerified()
	if !ran || !verified {
		t.Fatalf("MergeVerified() = (%v, %v), want (true, true)", verified, ran)
	}
}

func TestCommittedButUnpushed_Fails(t *testing.T) {
	env := newEnv(t)
	env.commit("main", "base.txt", "base")
	env.push("main")

	env.checkout("clanker/feature")
	env.commit("clanker/feature", "first.txt", "first")
	env.push("clanker/feature")

	// The CLA-134 shape exactly: the branch IS on the remote, the handover record
	// is truthful about its existence, and the work that matters never left the
	// laptop.
	env.commit("clanker/feature", "second.txt", "second")
	env.commit("clanker/feature", "third.txt", "third")

	rep := verify(t, env.work, Claim{Label: "CLA-134", Branch: "clanker/feature"})

	if !rep.Failed() {
		t.Fatalf("unpushed local commits should fail, got %s", render(rep))
	}
	c := mustStatus(t, rep, BranchPushed, Fail)
	if !strings.Contains(c.Detail, "2 commits ahead") {
		t.Errorf("detail should name how much is unpushed, got %q", c.Detail)
	}
	if !strings.Contains(c.Detail, "UNPUSHED") {
		t.Errorf("detail should say the work is unpushed, got %q", c.Detail)
	}
}

func TestBranchAbsentOnRemote_Fails(t *testing.T) {
	env := newEnv(t)
	env.commit("main", "base.txt", "base")
	env.push("main")

	env.checkout("clanker/feature")
	env.commit("clanker/feature", "work.txt", "work")
	// Never pushed at all.

	rep := verify(t, env.work, Claim{Label: "CLA-253", Branch: "clanker/feature"})

	if !rep.Failed() {
		t.Fatalf("a branch that was never pushed should fail, got %s", render(rep))
	}
	c := mustStatus(t, rep, BranchPushed, Fail)
	if !strings.Contains(c.Detail, "NOT on") {
		t.Errorf("detail should say the branch is not on the remote, got %q", c.Detail)
	}
}

func TestNonGitWorkdir_DegradesToCannotCheck(t *testing.T) {
	requireGit(t)
	// Not a repository, and nothing below it is either. The distinction that
	// matters: this must NOT read as a pass.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "notes", "deeper"), 0o755); err != nil {
		t.Fatal(err)
	}

	rep := verify(t, dir, Claim{
		Label: "CLA-253", Branch: "clanker/feature",
		Commit: "deadbeef", IntegrationBranch: "main",
	})

	if rep.Failed() {
		t.Fatalf("an unverifiable workdir must not be reported as a failure, got %s", render(rep))
	}
	mustStatus(t, rep, BranchPushed, Unknown)
	mustStatus(t, rep, CommitMerged, Unknown)

	if _, ran := rep.MergeVerified(); ran {
		t.Error("MergeVerified() must report ran=false when nothing was checked — attesting here would be a false pass")
	}
	c := mustStatus(t, rep, BranchPushed, Unknown)
	if !strings.Contains(c.Detail, "not a git repository") {
		t.Errorf("detail should say why it could not check, got %q", c.Detail)
	}
}

func TestCommitNotMerged_Fails(t *testing.T) {
	env := newEnv(t)
	env.commit("main", "base.txt", "base")
	env.push("main")

	env.checkout("clanker/feature")
	env.commit("clanker/feature", "work.txt", "work")
	env.push("clanker/feature")
	sha := env.head()

	// The branch is pushed but never merged, and the session declared it landed.
	rep := verify(t, env.work, Claim{
		Label: "CLA-253", Branch: "clanker/feature",
		Commit: sha, IntegrationBranch: "main",
	})

	mustStatus(t, rep, BranchPushed, Pass)
	c := mustStatus(t, rep, CommitMerged, Fail)
	if !strings.Contains(c.Detail, "NOT an ancestor") {
		t.Errorf("detail should say the commit did not land, got %q", c.Detail)
	}

	verified, ran := rep.MergeVerified()
	if !ran || verified {
		t.Fatalf("MergeVerified() = (%v, %v), want (false, true) — the check ran and said no", verified, ran)
	}
}

// The integration branch is judged against the REMOTE tip, not a stale local one:
// a driver whose local `main` is tens of commits behind must not report a merged
// delivery as unmerged.
func TestMergedRemotelyButLocalIntegrationBranchIsStale_Passes(t *testing.T) {
	env := newEnv(t)
	env.commit("main", "base.txt", "base")
	env.push("main")

	env.checkout("clanker/feature")
	env.commit("clanker/feature", "work.txt", "work")
	env.push("clanker/feature")
	sha := env.head()

	// Land it on the remote from a SEPARATE clone, so the first clone's local
	// `main` never learns about the merge.
	other := filepath.Join(env.root, "other")
	run(t, env.root, "git", "clone", env.remote, other)
	configure(t, other)
	run(t, other, "git", "fetch", "origin", "clanker/feature")
	run(t, other, "git", "merge", "--no-ff", "-m", "merge feature", "FETCH_HEAD")
	run(t, other, "git", "push", "origin", "main")

	rep := verify(t, env.work, Claim{Label: "CLA-253", Commit: sha, IntegrationBranch: "main"})

	if rep.Failed() {
		t.Fatalf("a delivery merged on the remote should pass despite a stale local main, got %s", render(rep))
	}
	mustStatus(t, rep, CommitMerged, Pass)
}

// The driver's workdir is routinely a multi-repo parent (`~/dev`) that is not a
// repository, with the work in a per-task worktree it did not create. Both must
// resolve to the repository whose refs carry the branch.
func TestResolvesRepoFromMultiRepoParentAndLinkedWorktree(t *testing.T) {
	env := newEnv(t)
	env.commit("main", "base.txt", "base")
	env.push("main")

	// A decoy sibling repository with no such branch, to prove the search picks by
	// ref rather than by taking the first repo it trips over.
	decoy := filepath.Join(env.root, "aaa-decoy")
	run(t, env.root, "git", "init", "-b", "main", decoy)
	configure(t, decoy)
	writeFile(t, filepath.Join(decoy, "x.txt"), "x")
	run(t, decoy, "git", "add", ".")
	run(t, decoy, "git", "commit", "-m", "x")

	// The per-task worktree: created off the main checkout, two levels below the
	// parent, exactly like ~/dev/<repo>-wt/<task-id>.
	wt := filepath.Join(env.root, "work-wt", "c143f64b")
	run(t, env.work, "git", "worktree", "add", "-b", "clanker/feature", wt, "main")
	writeFile(t, filepath.Join(wt, "work.txt"), "work")
	run(t, wt, "git", "add", ".")
	run(t, wt, "git", "commit", "-m", "work in the worktree")

	// From the multi-repo parent, with nothing pushed: found, and correctly failed.
	rep := verify(t, env.root, Claim{Label: "CLA-253", Branch: "clanker/feature"})
	if rep.Repo == "" {
		t.Fatalf("expected a repository to be resolved from the parent dir, got %s", render(rep))
	}
	mustStatus(t, rep, BranchPushed, Fail)

	// And once it is pushed from the worktree, the same claim passes.
	run(t, wt, "git", "push", "origin", "clanker/feature")
	rep = verify(t, env.root, Claim{Label: "CLA-253", Branch: "clanker/feature"})
	mustStatus(t, rep, BranchPushed, Pass)
}

// A branch nobody has locally is not a failure — it is an absence of knowledge.
// Reporting it as unpushed would cry wolf at every session that worked in a tree
// the driver cannot see.
func TestUnknownBranch_DegradesToCannotCheck(t *testing.T) {
	env := newEnv(t)
	env.commit("main", "base.txt", "base")
	env.push("main")

	rep := verify(t, env.work, Claim{Label: "CLA-253", Branch: "clanker/never-existed"})

	if rep.Failed() {
		t.Fatalf("an unknown branch must not be reported as a failure, got %s", render(rep))
	}
	mustStatus(t, rep, BranchPushed, Unknown)
}

func TestEmptyClaimChecksNothing(t *testing.T) {
	requireGit(t)
	rep := verify(t, t.TempDir(), Claim{Label: "CLA-253"})
	if len(rep.Checks) != 0 {
		t.Fatalf("nothing claimed, nothing to check, got %s", render(rep))
	}
	if rep.Failed() {
		t.Error("an empty claim cannot fail")
	}
}

// A commit declared merged that this repository has never seen cannot be traced,
// and must not be guessed at either way.
func TestUnknownCommit_DegradesToCannotCheck(t *testing.T) {
	env := newEnv(t)
	env.commit("main", "base.txt", "base")
	env.push("main")

	rep := verify(t, env.work, Claim{
		Label: "CLA-253", Commit: "0123456789abcdef0123456789abcdef01234567", IntegrationBranch: "main",
	})

	if rep.Failed() {
		t.Fatalf("an untraceable commit must not be reported as a failure, got %s", render(rep))
	}
	mustStatus(t, rep, CommitMerged, Unknown)
	if _, ran := rep.MergeVerified(); ran {
		t.Error("nothing was checked, so nothing may be attested")
	}
}

// --- harness ---------------------------------------------------------------

type env struct {
	t      *testing.T
	root   string
	work   string
	remote string
}

// newEnv builds root/{remote.git, work}: a bare remote and one clone-like working
// repository wired to it.
func newEnv(t *testing.T) *env {
	t.Helper()
	requireGit(t)

	root := t.TempDir()
	// macOS temp dirs are symlinked (/var -> /private/var); resolve so paths
	// compared against git's own output match.
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}

	remote := filepath.Join(root, "remote.git")
	run(t, root, "git", "init", "--bare", "-b", "main", remote)

	work := filepath.Join(root, "work")
	run(t, root, "git", "init", "-b", "main", work)
	configure(t, work)
	run(t, work, "git", "remote", "add", "origin", remote)

	return &env{t: t, root: root, work: work, remote: remote}
}

func (e *env) git(args ...string) string {
	e.t.Helper()
	return run(e.t, e.work, append([]string{"git"}, args...)...)
}

func (e *env) checkout(branch string) {
	e.t.Helper()
	if out, err := exec.Command("git", "-C", e.work, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).Output(); err == nil && len(out) > 0 {
		e.git("checkout", branch)
		return
	}
	e.git("checkout", "-b", branch)
}

func (e *env) commit(branch, name, body string) {
	e.t.Helper()
	writeFile(e.t, filepath.Join(e.work, name), body)
	e.git("add", ".")
	e.git("commit", "-m", "add "+name+" on "+branch)
}

func (e *env) push(branch string) {
	e.t.Helper()
	e.git("push", "origin", branch)
}

func (e *env) head() string { return e.git("rev-parse", "HEAD") }

func verify(t *testing.T, workdir string, c Claim) Report {
	t.Helper()
	return New(workdir, "origin").Verify(context.Background(), c)
}

func mustStatus(t *testing.T, rep Report, kind Kind, want Status) Check {
	t.Helper()
	for _, c := range rep.Checks {
		if c.Kind == kind {
			if c.Status != want {
				t.Fatalf("%s check: got %s (%s), want %s", kind, c.Status, c.Detail, want)
			}
			return c
		}
	}
	t.Fatalf("no %s check in report: %s", kind, render(rep))
	return Check{}
}

func render(rep Report) string {
	var b strings.Builder
	b.WriteString("repo=" + rep.Repo)
	for _, c := range rep.Checks {
		b.WriteString("; " + string(c.Kind) + "=" + string(c.Status) + " (" + c.Detail + ")")
	}
	return b.String()
}

func configure(t *testing.T, dir string) {
	t.Helper()
	run(t, dir, "git", "config", "user.email", "driver@example.test")
	run(t, dir, "git", "config", "user.name", "Driver Test")
	run(t, dir, "git", "config", "commit.gpgsign", "false")
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME="+t.TempDir(),
		"GIT_AUTHOR_NAME=Driver Test", "GIT_AUTHOR_EMAIL=driver@example.test",
		"GIT_COMMITTER_NAME=Driver Test", "GIT_COMMITTER_EMAIL=driver@example.test",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
}
