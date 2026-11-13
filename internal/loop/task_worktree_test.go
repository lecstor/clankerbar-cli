package loop

// task_worktree_test.go — task_worktree.go over REAL repositories: a bare
// "origin", a clone as the checkout, and a `<checkout>-wt` directory so the
// fleet root is the beside-the-checkout area.
//
// The tests pin the behaviour the runner depends on: the branch and path are
// deterministic, a resumed branch is merged (or reported conflicted and left
// clean), a close-out never merges, and no tree that might hold work is ever
// adopted or reset.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	taskWorktreeID     = "1a2b3c4d-0000-4000-8000-000000000000"
	taskWorktreeBranch = "clanker/1a2b3c4d-some-title"
)

// taskWorktreeRepo is the fixture: origin is bare and is the only remote the
// checkout has, so every branch the runner needs is on "origin".
type taskWorktreeRepo struct {
	root     string
	seed     string
	origin   string
	checkout string
}

func newTaskWorktreeRepo(t *testing.T) *taskWorktreeRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	r := &taskWorktreeRepo{root: t.TempDir()}
	r.seed = filepath.Join(r.root, "seed")
	r.origin = filepath.Join(r.root, "origin.git")
	r.checkout = filepath.Join(r.root, "repo")
	gitRun(t, r.root, "init", "--bare", "-b", "staging", r.origin)
	gitRun(t, r.root, "init", "-b", "staging", r.seed)
	writeTestFile(t, filepath.Join(r.seed, "a.txt"), "base")
	gitRun(t, r.seed, "add", ".")
	gitRun(t, r.seed, "commit", "-m", "base")
	gitRun(t, r.seed, "remote", "add", "origin", r.origin)
	gitRun(t, r.seed, "push", "-u", "origin", "staging")
	gitRun(t, r.root, "clone", r.origin, r.checkout)
	// The merge commits prepareTaskWorktree makes need an identity; local config
	// is the one place guaranteed to reach it, and it also overrides a
	// developer's global commit.gpgsign.
	gitRun(t, r.checkout, "config", "user.name", "Driver Test")
	gitRun(t, r.checkout, "config", "user.email", "driver@example.test")
	gitRun(t, r.checkout, "config", "commit.gpgsign", "false")
	// fleetWorktreeRoot prefers <project>/worktrees, then <checkout>-wt; only
	// the latter exists here.
	if err := os.MkdirAll(r.checkout+"-wt", 0o755); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *taskWorktreeRepo) request() worktreeRequest {
	return worktreeRequest{Checkout: r.checkout, TaskID: taskWorktreeID, Branch: taskWorktreeBranch, PRBase: "staging"}
}

// withoutFleetRoot removes the only fleet worktree area this fixture has, so a
// prepare has to pick a fallback root.
func (r *taskWorktreeRepo) withoutFleetRoot(t *testing.T) {
	t.Helper()
	if err := os.RemoveAll(r.checkout + "-wt"); err != nil {
		t.Fatal(err)
	}
}

// commitOn (re)cuts branch at from ("" means its current origin tip), commits
// one file change, pushes it to origin and returns the new tip.
func (r *taskWorktreeRepo) commitOn(t *testing.T, branch, from, file, body, msg string) string {
	t.Helper()
	gitRun(t, r.seed, "fetch", "origin")
	if from == "" {
		from = "origin/" + branch
	}
	gitRun(t, r.seed, "checkout", "-B", branch, from)
	writeTestFile(t, filepath.Join(r.seed, file), body)
	gitRun(t, r.seed, "add", "-A")
	gitRun(t, r.seed, "commit", "-m", msg)
	tip := gitOut(t, r.seed, "rev-parse", "HEAD")
	gitRun(t, r.seed, "push", "origin", branch)
	gitRun(t, r.seed, "fetch", "origin")
	return tip
}

func (r *taskWorktreeRepo) prepare(t *testing.T, req worktreeRequest) (worktreePrep, error) {
	t.Helper()
	return prepareTaskWorktree(context.Background(), req)
}

// taskRefExists reports whether ref resolves in dir, without failing the test.
func taskRefExists(t *testing.T, dir, ref string) bool {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", ref)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "HOME="+t.TempDir())
	return cmd.Run() == nil
}

// taskWorktreeListedPaths is the `worktree <path>` line of each porcelain block.
// The paths may be symlink-resolved by git, so compare them with
// taskWorktreeSamePath rather than literally.
func taskWorktreeListedPaths(t *testing.T, checkout string) []string {
	t.Helper()
	var paths []string
	for _, line := range strings.Split(gitOut(t, checkout, "worktree", "list", "--porcelain"), "\n") {
		if path, ok := strings.CutPrefix(line, "worktree "); ok {
			paths = append(paths, path)
		}
	}
	return paths
}

func taskWorktreeParentCount(t *testing.T, dir string) int {
	t.Helper()
	return len(strings.Fields(gitOut(t, dir, "rev-list", "--parents", "-n", "1", "HEAD")))
}

func TestTaskWorktree_FreshCreateFromBase(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	baseTip := gitOut(t, f.origin, "rev-parse", "staging")

	prep, err := f.prepare(t, f.request())
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	wantDir := filepath.Join(f.checkout+"-wt", taskWorktreeID[:8])
	if prep.Dir != wantDir {
		t.Errorf("Dir = %q, want %q", prep.Dir, wantDir)
	}
	if !prep.Created || prep.Merged || prep.UpToDate {
		t.Errorf("Created/Merged/UpToDate = %v/%v/%v, want true/false/false", prep.Created, prep.Merged, prep.UpToDate)
	}
	if prep.Tip != baseTip {
		t.Errorf("Tip = %s, want the base tip %s", prep.Tip, baseTip)
	}
	if prep.BaseSHA != baseTip {
		t.Errorf("BaseSHA = %s, want the base tip %s (the merge-base of origin/staging and the tip)", prep.BaseSHA, baseTip)
	}
	if branch := gitOut(t, prep.Dir, "rev-parse", "--abbrev-ref", "HEAD"); branch != taskWorktreeBranch {
		t.Errorf("worktree is on %q, want %q", branch, taskWorktreeBranch)
	}
	if dirty := gitOut(t, prep.Dir, "status", "--porcelain"); dirty != "" {
		t.Errorf("a fresh worktree should be clean, got:\n%s", dirty)
	}
	listed := false
	for _, path := range taskWorktreeListedPaths(t, f.checkout) {
		listed = listed || taskWorktreeSamePath(path, prep.Dir)
	}
	if !listed {
		t.Errorf("%s is not registered as a worktree of %s", prep.Dir, f.checkout)
	}
	if taskRefExists(t, f.origin, "refs/heads/"+taskWorktreeBranch) {
		t.Errorf("prepareTaskWorktree pushed the new branch to origin; it must never push")
	}
}

func TestTaskWorktree_ReusesCleanWorktree(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	first, err := f.prepare(t, f.request())
	if err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	second, err := f.prepare(t, f.request())
	if err != nil {
		t.Fatalf("second prepare: %v", err)
	}
	if second.Created {
		t.Errorf("the second prepare reported Created; it reused %s", second.Dir)
	}
	if second.Dir != first.Dir || second.Tip != first.Tip {
		t.Errorf("reuse returned %s@%s, want %s@%s", second.Dir, second.Tip, first.Dir, first.Tip)
	}
}

// A worktree whose directory is gone is still registered until pruned, and git
// refuses to add that path again while the stale entry stands.
func TestTaskWorktree_PrunesStaleEntryAndRecreates(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	first, err := f.prepare(t, f.request())
	if err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	if err := os.RemoveAll(first.Dir); err != nil {
		t.Fatal(err)
	}

	prep, err := f.prepare(t, f.request())
	if err != nil {
		t.Fatalf("prepare after the worktree directory was removed: %v", err)
	}
	if !prep.Created {
		t.Errorf("prepare did not recreate the worktree at %s (Dir = %s)", first.Dir, prep.Dir)
	}
	if !taskWorktreeSamePath(prep.Dir, first.Dir) {
		t.Errorf("Dir = %q, want the deterministic path %q", prep.Dir, first.Dir)
	}
	if _, err := os.Stat(filepath.Join(prep.Dir, "a.txt")); err != nil {
		t.Errorf("the recreated worktree is missing the base file: %v", err)
	}
}

func TestTaskWorktree_ResumeMergesBase(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	f.commitOn(t, taskWorktreeBranch, "origin/staging", "branch.txt", "branch work", "the work")
	mergeBaseTip := f.commitOn(t, "staging", "", "base.txt", "base moved", "base moved")
	req := f.request()
	req.Resume = true

	prep, err := f.prepare(t, req)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !prep.Created || !prep.Merged || prep.UpToDate {
		t.Errorf("Created/Merged/UpToDate = %v/%v/%v, want true/true/false", prep.Created, prep.Merged, prep.UpToDate)
	}
	if len(prep.Conflicts) != 0 {
		t.Errorf("Conflicts = %v, want none", prep.Conflicts)
	}
	if got := taskWorktreeParentCount(t, prep.Dir); got != 3 {
		t.Errorf("HEAD has %d parents, want a merge commit (3 fields incl. itself)", got)
	}
	if got := gitOut(t, prep.Dir, "rev-parse", "HEAD^2"); got != mergeBaseTip {
		t.Errorf("the merge's second parent is %s, want the base tip %s", got, mergeBaseTip)
	}
	// The prep merged the base into the branch, so the commit a review's diff
	// must start from is the merged base tip, not the old fork point: anything
	// earlier would re-show the base's own commits.
	if prep.BaseSHA != mergeBaseTip {
		t.Errorf("BaseSHA = %s, want the merged base tip %s", prep.BaseSHA, mergeBaseTip)
	}
	for _, file := range []string{"branch.txt", "base.txt"} {
		if _, err := os.Stat(filepath.Join(prep.Dir, file)); err != nil {
			t.Errorf("the merged worktree is missing %s: %v", file, err)
		}
	}
	if dirty := gitOut(t, prep.Dir, "status", "--porcelain"); dirty != "" {
		t.Errorf("the merged worktree should be clean, got:\n%s", dirty)
	}
}

func TestTaskWorktree_ResumeAlreadyUpToDate(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	branchTip := f.commitOn(t, taskWorktreeBranch, "origin/staging", "branch.txt", "branch work", "the work")
	req := f.request()
	req.Resume = true

	prep, err := f.prepare(t, req)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if prep.Merged || !prep.UpToDate {
		t.Errorf("Merged/UpToDate = %v/%v, want false/true", prep.Merged, prep.UpToDate)
	}
	if prep.Tip != branchTip {
		t.Errorf("Tip = %s, want the branch tip %s unchanged", prep.Tip, branchTip)
	}
}

func TestTaskWorktree_ResumeConflictIsReportedAndAborted(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	branchTip := f.commitOn(t, taskWorktreeBranch, "origin/staging", "a.txt", "branch side", "branch side")
	f.commitOn(t, "staging", "", "a.txt", "base side", "base side")
	req := f.request()
	req.Resume = true

	prep, err := f.prepare(t, req)
	if err != nil {
		t.Fatalf("a conflict must not be an error, got: %v", err)
	}
	if len(prep.Conflicts) != 1 || prep.Conflicts[0] != "a.txt" {
		t.Fatalf("Conflicts = %v, want [a.txt]", prep.Conflicts)
	}
	if prep.Merged || prep.UpToDate {
		t.Errorf("Merged/UpToDate = %v/%v, want both false", prep.Merged, prep.UpToDate)
	}
	if prep.Tip != branchTip {
		t.Errorf("Tip = %s, want the pre-merge tip %s", prep.Tip, branchTip)
	}
	if dirty := gitOut(t, prep.Dir, "status", "--porcelain"); dirty != "" {
		t.Errorf("the aborted merge left the tree dirty:\n%s", dirty)
	}
	if taskRefExists(t, prep.Dir, "MERGE_HEAD") {
		t.Errorf("the merge was not aborted: MERGE_HEAD still exists")
	}
}

func TestTaskWorktree_CloseOutNeverMerges(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	branchTip := f.commitOn(t, taskWorktreeBranch, "origin/staging", "a.txt", "branch side", "branch side")
	f.commitOn(t, "staging", "", "a.txt", "base side", "base side")
	req := f.request()
	req.Resume = true
	req.CloseOut = true
	req.PRBase = "" // a close-out needs no base at all

	prep, err := f.prepare(t, req)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if prep.Merged || prep.UpToDate || len(prep.Conflicts) != 0 {
		t.Errorf("Merged/UpToDate/Conflicts = %v/%v/%v, want a checked-out branch and no merge",
			prep.Merged, prep.UpToDate, prep.Conflicts)
	}
	if prep.Tip != branchTip {
		t.Errorf("Tip = %s, want the branch tip %s unchanged", prep.Tip, branchTip)
	}
	if body, err := os.ReadFile(filepath.Join(prep.Dir, "a.txt")); err != nil || strings.TrimSpace(string(body)) != "branch side" {
		t.Errorf("a.txt in the close-out worktree = %q (%v), want the branch side: the base must never be merged", body, err)
	}
	if got := taskWorktreeParentCount(t, prep.Dir); got != 2 {
		t.Errorf("HEAD has %d parent fields, want an ordinary commit", got)
	}
}

func TestTaskWorktree_DivergedLocalBranchRefused(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	f.commitOn(t, taskWorktreeBranch, "origin/staging", "branch.txt", "branch work", "the work")
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
	f.commitOn(t, taskWorktreeBranch, "", "remote.txt", "remote commit", "remote moved")

	if _, err := f.prepare(t, req); err == nil {
		t.Fatalf("a diverged local branch must be refused")
	} else if !strings.Contains(err.Error(), "diverged") {
		t.Errorf("error %q does not name the divergence", err)
	}
	if got := gitOut(t, first.Dir, "rev-parse", "HEAD"); got != localTip {
		t.Errorf("HEAD = %s, want the local commit %s to survive the refusal", got, localTip)
	}
}

// A resumed branch carrying a commit origin never got: -B would reset it and
// drop that work, so prepare must refuse and leave the ref where it is.
func TestTaskWorktree_ResumeRefusesToResetUnpushedLocalBranch(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	f.commitOn(t, taskWorktreeBranch, "origin/staging", "branch.txt", "branch work", "the work")
	gitRun(t, f.checkout, "fetch", "origin")
	gitRun(t, f.checkout, "checkout", "-b", taskWorktreeBranch, "origin/"+taskWorktreeBranch)
	writeTestFile(t, filepath.Join(f.checkout, "unpushed.txt"), "unpushed work")
	gitRun(t, f.checkout, "add", "-A")
	gitRun(t, f.checkout, "commit", "-m", "unpushed")
	localTip := gitOut(t, f.checkout, "rev-parse", "HEAD")
	gitRun(t, f.checkout, "checkout", "staging")
	req := f.request()
	req.Resume = true

	if _, err := f.prepare(t, req); err == nil {
		t.Fatalf("a local branch ahead of origin must be refused, not reset")
	} else if !strings.Contains(err.Error(), "refusing to reset it") {
		t.Errorf("error %q does not say why the branch was refused", err)
	}
	if got := gitOut(t, f.checkout, "rev-parse", "refs/heads/"+taskWorktreeBranch); got != localTip {
		t.Errorf("the local branch moved to %s, want %s: the refusal must not reset it", got, localTip)
	}
}

func TestTaskWorktree_FetchFailureErrors(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	gitRun(t, f.checkout, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))

	if _, err := f.prepare(t, f.request()); err == nil {
		t.Fatalf("an unreachable origin must fail the fetch")
	} else if !strings.Contains(err.Error(), "fetching origin") {
		t.Errorf("error %q is not reported as a fetch failure the caller can retry", err)
	}
}

// Fetched is the caller saying it already ran the one fetch: a start that read
// origin must not pay for a second round-trip, and must still work when the
// remote has gone away in between.
func TestTaskWorktree_FetchedSkipsTheFetch(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	f.commitOn(t, taskWorktreeBranch, "origin/staging", "a.txt", "work", "work")
	// The caller's fetch: the refs are local from here on, and only then does the
	// remote go away.
	gitRun(t, f.checkout, "fetch", "origin")
	gitRun(t, f.checkout, "remote", "set-url", "origin", filepath.Join(f.root, "gone.git"))

	req := f.request()
	req.Resume = true
	req.Fetched = true
	if _, err := f.prepare(t, req); err != nil {
		t.Fatalf("prepare with Fetched = true: %v", err)
	}
	req.Fetched = false
	if _, err := f.prepare(t, req); !errors.Is(err, errWorktreeFetch) {
		t.Fatalf("prepare with Fetched = false gave %v, want errWorktreeFetch", err)
	}
}

func TestTaskWorktree_ForeignDirectoryRefused(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	dir := filepath.Join(f.checkout+"-wt", taskWorktreeID[:8])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "mine.txt")
	writeTestFile(t, keep, "not the runner's")

	if _, err := f.prepare(t, f.request()); err == nil {
		t.Fatalf("a directory that is not a registered worktree must be refused")
	} else if !strings.Contains(err.Error(), "not a worktree") {
		t.Errorf("error %q does not say the directory is foreign", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("the refusal touched the foreign directory: %v", err)
	}
}

// A session may cut its own worktree for the branch before the runner gets
// there; git refuses to add that branch twice, so the clean tree is adopted.
func TestTaskWorktree_AdoptsCleanWorktreeOnBranch(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	branchTip := f.commitOn(t, taskWorktreeBranch, "origin/staging", "branch.txt", "branch work", "the work")
	gitRun(t, f.checkout, "fetch", "origin")
	elsewhere := filepath.Join(f.checkout, ".worktrees", "session-wt")
	gitRun(t, f.checkout, "worktree", "add", "-b", taskWorktreeBranch, elsewhere, "origin/"+taskWorktreeBranch)
	req := f.request()
	req.Resume = true

	prep, err := f.prepare(t, req)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !taskWorktreeSamePath(prep.Dir, elsewhere) {
		t.Errorf("Dir = %q, want the existing worktree %q", prep.Dir, elsewhere)
	}
	if prep.Created {
		t.Errorf("prepare reported Created; it adopted %s", elsewhere)
	}
	if prep.Tip != branchTip || !prep.UpToDate {
		t.Errorf("Tip/UpToDate = %s/%v, want %s/true: the adopted tree is still synced and merged",
			prep.Tip, prep.UpToDate, branchTip)
	}
	deterministic := filepath.Join(f.checkout+"-wt", taskWorktreeID[:8])
	if _, err := os.Stat(deterministic); !os.IsNotExist(err) {
		t.Errorf("the deterministic dir %s was created as well (err %v)", deterministic, err)
	}
}

// The fleet's own worktrees directory is the other root a task worktree may
// live under.
func TestTaskWorktree_AdoptsWorktreeInFleetRoot(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	branchTip := f.commitOn(t, taskWorktreeBranch, "origin/staging", "branch.txt", "branch work", "the work")
	gitRun(t, f.checkout, "fetch", "origin")
	elsewhere := filepath.Join(f.root, "worktrees", "session-a")
	gitRun(t, f.checkout, "worktree", "add", "-b", taskWorktreeBranch, elsewhere, "origin/"+taskWorktreeBranch)
	req := f.request()
	req.Resume = true

	prep, err := f.prepare(t, req)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !taskWorktreeSamePath(prep.Dir, elsewhere) {
		t.Errorf("Dir = %q, want the existing worktree %q", prep.Dir, elsewhere)
	}
	if prep.Created || prep.Tip != branchTip {
		t.Errorf("Created/Tip = %v/%s, want false/%s: the fleet tree is adopted", prep.Created, prep.Tip, branchTip)
	}
}

// A tree outside the task-worktree roots is still adopted when it carries
// nothing origin does not: a clean tree at origin's tip holds no unpushed work,
// and refusing it would burn the task's claim for a tree the next session could
// safely use.
func TestTaskWorktree_AdoptsCleanWorktreeOutsideTheRoots(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	branchTip := f.commitOn(t, taskWorktreeBranch, "origin/staging", "branch.txt", "branch work", "the work")
	gitRun(t, f.checkout, "fetch", "origin")
	elsewhere := filepath.Join(f.root, "session-wt")
	gitRun(t, f.checkout, "worktree", "add", "-b", taskWorktreeBranch, elsewhere, "origin/"+taskWorktreeBranch)
	req := f.request()
	req.Resume = true

	prep, err := f.prepare(t, req)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !taskWorktreeSamePath(prep.Dir, elsewhere) {
		t.Errorf("Dir = %q, want the existing worktree %q", prep.Dir, elsewhere)
	}
	if !prep.AdoptedOutside || prep.Created {
		t.Errorf("AdoptedOutside/Created = %v/%v, want true/false", prep.AdoptedOutside, prep.Created)
	}
	if prep.Tip != branchTip {
		t.Errorf("Tip = %s, want %s", prep.Tip, branchTip)
	}
}

// A task worktree outlives the session and holds installed dependencies, so
// with no fleet area it belongs in the main checkout's own .worktrees — never
// the OS temp dir, which is a tmpfs on many hosts.
func TestTaskWorktree_CreatesUnderMainWorktreesDir(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	f.withoutFleetRoot(t)
	dotWorktrees := filepath.Join(f.checkout, ".worktrees")
	if err := os.MkdirAll(dotWorktrees, 0o755); err != nil {
		t.Fatal(err)
	}

	prep, err := f.prepare(t, f.request())
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// git spells the main worktree symlink-resolved (/private/tmp), so compare
	// the way the rest of this file does.
	if want := filepath.Join(dotWorktrees, taskWorktreeID[:8]); !taskWorktreeSamePath(prep.Dir, want) {
		t.Errorf("Dir = %q, want %q", prep.Dir, want)
	}
}

// With neither a fleet area nor a .worktrees directory, the older -wt sibling
// outside the repo is used: still durable, unlike the temp dir.
func TestTaskWorktree_CreatesBesideCheckoutWithoutFleetArea(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	f.withoutFleetRoot(t)

	prep, err := f.prepare(t, f.request())
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// The exact path, not a temp-dir prefix: t.TempDir() is itself under the OS
	// temp dir, so only the exact expectation proves the root rule.
	if want := filepath.Join(f.checkout+"-wt", taskWorktreeID[:8]); prep.Dir != want {
		t.Errorf("Dir = %q, want %q", prep.Dir, want)
	}
}

// The fleet's area wins over the checkout's .worktrees: it is the layout the
// panel and the doctor already agree on.
func TestTaskWorktree_FleetRootWinsOverMainWorktreesDir(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	if err := os.MkdirAll(filepath.Join(f.checkout, ".worktrees"), 0o755); err != nil {
		t.Fatal(err)
	}

	prep, err := f.prepare(t, f.request())
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if want := filepath.Join(f.checkout+"-wt", taskWorktreeID[:8]); prep.Dir != want {
		t.Errorf("Dir = %q, want the fleet root's %q", prep.Dir, want)
	}
}

// A session in the repo cuts `<main>/.claNNN` worktrees. A clean one at origin's
// tip is the task's tree and is adopted rather than refused.
func TestTaskWorktree_AdoptsCleanSiblingWorktree(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	branchTip := f.commitOn(t, taskWorktreeBranch, "origin/staging", "branch.txt", "branch work", "the work")
	gitRun(t, f.checkout, "fetch", "origin")
	sibling := filepath.Join(f.checkout, ".cla848")
	gitRun(t, f.checkout, "worktree", "add", "-b", taskWorktreeBranch, sibling, "origin/"+taskWorktreeBranch)
	req := f.request()
	req.Resume = true

	prep, err := f.prepare(t, req)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !taskWorktreeSamePath(prep.Dir, sibling) {
		t.Errorf("Dir = %q, want the existing worktree %q", prep.Dir, sibling)
	}
	if !prep.AdoptedOutside || prep.Created {
		t.Errorf("AdoptedOutside/Created = %v/%v, want true/false", prep.AdoptedOutside, prep.Created)
	}
	if prep.Tip != branchTip {
		t.Errorf("Tip = %s, want %s", prep.Tip, branchTip)
	}
}

// A sibling tree behind origin holds nothing unpushed, so it is adopted and the
// resumed branch fast-forwards to origin's tip.
func TestTaskWorktree_AdoptsSiblingWorktreeBehindOrigin(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	f.commitOn(t, taskWorktreeBranch, "origin/staging", "branch.txt", "branch work", "the work")
	gitRun(t, f.checkout, "fetch", "origin")
	sibling := filepath.Join(f.checkout, ".cla848")
	gitRun(t, f.checkout, "worktree", "add", "-b", taskWorktreeBranch, sibling, "origin/"+taskWorktreeBranch)
	originTip := f.commitOn(t, taskWorktreeBranch, "", "more.txt", "more work", "more work")
	req := f.request()
	req.Resume = true

	prep, err := f.prepare(t, req)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !prep.AdoptedOutside {
		t.Errorf("AdoptedOutside = false, want true for %s", prep.Dir)
	}
	if prep.Tip != originTip {
		t.Errorf("Tip = %s, want the fast-forwarded origin tip %s", prep.Tip, originTip)
	}
}

func TestTaskWorktree_SiblingWorktreeAheadOfOriginRefused(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	f.commitOn(t, taskWorktreeBranch, "origin/staging", "branch.txt", "branch work", "the work")
	gitRun(t, f.checkout, "fetch", "origin")
	sibling := filepath.Join(f.checkout, ".cla848")
	gitRun(t, f.checkout, "worktree", "add", "-b", taskWorktreeBranch, sibling, "origin/"+taskWorktreeBranch)
	writeTestFile(t, filepath.Join(sibling, "local.txt"), "local commit")
	gitRun(t, sibling, "add", "-A")
	gitRun(t, sibling, "commit", "-m", "unpushed work")
	req := f.request()
	req.Resume = true

	_, err := f.prepare(t, req)
	if err == nil {
		t.Fatalf("a sibling ahead of origin must be refused, not adopted")
	}
	if !strings.Contains(err.Error(), "does not have") {
		t.Errorf("error %q does not say the tree holds commits origin lacks", err)
	}
}

// A fresh task that has pushed nothing yet: its branch is not on origin, but a
// clean sibling at the PR base holds exactly what the runner would cut, so it is
// adopted rather than refused and parked.
func TestTaskWorktree_AdoptsFreshSiblingAtBaseNotOnOrigin(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	gitRun(t, f.checkout, "fetch", "origin")
	baseTip := gitOut(t, f.checkout, "rev-parse", "origin/staging")
	sibling := filepath.Join(f.checkout, ".cla848")
	gitRun(t, f.checkout, "worktree", "add", "-b", taskWorktreeBranch, sibling, "origin/staging")

	prep, err := f.prepare(t, f.request())
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !taskWorktreeSamePath(prep.Dir, sibling) {
		t.Errorf("Dir = %q, want the existing worktree %q", prep.Dir, sibling)
	}
	if !prep.AdoptedOutside || prep.Created {
		t.Errorf("AdoptedOutside/Created = %v/%v, want true/false", prep.AdoptedOutside, prep.Created)
	}
	if prep.Tip != baseTip {
		t.Errorf("Tip = %s, want the base tip %s", prep.Tip, baseTip)
	}
	if prep.OriginTip != "" || prep.Ahead != 0 {
		t.Errorf("OriginTip/Ahead = %q/%d, want empty/0: the branch was never pushed", prep.OriginTip, prep.Ahead)
	}
}

// The same fresh shape, but the sibling holds a commit the base does not: that
// is unpushed work, so it is refused and left untouched.
func TestTaskWorktree_FreshSiblingWithLocalCommitRefused(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	gitRun(t, f.checkout, "fetch", "origin")
	sibling := filepath.Join(f.checkout, ".cla848")
	gitRun(t, f.checkout, "worktree", "add", "-b", taskWorktreeBranch, sibling, "origin/staging")
	writeTestFile(t, filepath.Join(sibling, "local.txt"), "local commit")
	gitRun(t, sibling, "add", "-A")
	gitRun(t, sibling, "commit", "-m", "unpushed work")
	localTip := gitOut(t, sibling, "rev-parse", "HEAD")

	_, err := f.prepare(t, f.request())
	if err == nil {
		t.Fatalf("a sibling with commits the base does not have must be refused, not adopted")
	}
	if !strings.Contains(err.Error(), "holds commits origin/") {
		t.Errorf("error %q does not say the tree holds commits origin lacks", err)
	}
	if got := gitOut(t, sibling, "rev-parse", "HEAD"); got != localTip {
		t.Errorf("HEAD = %s, want the local commit %s to survive the refusal", got, localTip)
	}
}

// The fresh-tree relaxation is for a start, never a resume: a resumed branch
// that is not on origin still has nothing to prove it was published.
func TestTaskWorktree_ResumeRefusesBranchNotOnOriginOutsideTheRoots(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	gitRun(t, f.checkout, "fetch", "origin")
	sibling := filepath.Join(f.checkout, ".cla848")
	gitRun(t, f.checkout, "worktree", "add", "-b", taskWorktreeBranch, sibling, "origin/staging")
	req := f.request()
	req.Resume = true

	_, err := f.prepare(t, req)
	if err == nil {
		t.Fatalf("a resumed branch that is not on origin must be refused, not adopted")
	}
	if !strings.Contains(err.Error(), "not on origin") {
		t.Errorf("error %q does not say the branch is not on origin", err)
	}
}

// The panel's root rule is unchanged: a throwaway member tree falls back to the
// OS temp dir when no known layout exists.
func TestPanelWorktreeRootFallsBackToTempDir(t *testing.T) {
	checkout := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := panelWorktreeRoot(checkout); got != os.TempDir() {
		t.Errorf("panelWorktreeRoot = %q, want the temp dir %q", got, os.TempDir())
	}
	if err := os.MkdirAll(checkout+"-wt", 0o755); err != nil {
		t.Fatal(err)
	}
	if got := panelWorktreeRoot(checkout); got != checkout+"-wt" {
		t.Errorf("panelWorktreeRoot = %q, want %q", got, checkout+"-wt")
	}
}

// A drain's own checkout is a LINKED worktree in the fleet layout. Its being on
// the branch is another run's session, never this task's tree.
func TestTaskWorktree_OwnLinkedCheckoutOnBranchRefused(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	f.commitOn(t, taskWorktreeBranch, "origin/staging", "branch.txt", "branch work", "the work")
	gitRun(t, f.checkout, "fetch", "origin")
	linked := filepath.Join(f.root, "worktrees", "drain-a")
	gitRun(t, f.checkout, "worktree", "add", "-b", taskWorktreeBranch, linked, "origin/"+taskWorktreeBranch)
	req := f.request()
	req.Checkout = linked
	req.Resume = true

	if _, err := f.prepare(t, req); err == nil {
		t.Fatalf("the run's own checkout on the branch must be refused")
	} else if !strings.Contains(err.Error(), "drain-a") || !strings.Contains(err.Error(), "refusing") {
		t.Errorf("error %q does not name the refusing checkout", err)
	}
}

func TestTaskWorktree_MainCheckoutOnBranchRefused(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	f.commitOn(t, taskWorktreeBranch, "origin/staging", "branch.txt", "branch work", "the work")
	gitRun(t, f.checkout, "fetch", "origin")
	gitRun(t, f.checkout, "checkout", "-b", taskWorktreeBranch, "origin/"+taskWorktreeBranch)
	req := f.request()
	req.Resume = true

	_, err := f.prepare(t, req)
	if err == nil || !strings.Contains(err.Error(), "main checkout") {
		t.Fatalf("prepare err = %v, want a refusal naming the main checkout", err)
	}
}

func TestTaskWorktree_ReuseReportsUnpushedCommits(t *testing.T) {
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

	prep, err := f.prepare(t, req)
	if err != nil {
		t.Fatalf("second prepare: %v", err)
	}
	if prep.Tip != localTip {
		t.Errorf("Tip = %s, want the unpushed local commit %s kept", prep.Tip, localTip)
	}
	if prep.OriginTip != branchTip {
		t.Errorf("OriginTip = %s, want %s", prep.OriginTip, branchTip)
	}
	if prep.Tip == prep.OriginTip {
		t.Errorf("Tip and OriginTip are both %s; origin has not got the local commit", prep.Tip)
	}
	if prep.Ahead != 1 {
		t.Errorf("Ahead = %d, want 1", prep.Ahead)
	}
}

func TestTaskWorktree_ExistingLocalBranchWithCommitsRefused(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	gitRun(t, f.checkout, "checkout", "-b", taskWorktreeBranch)
	writeTestFile(t, filepath.Join(f.checkout, "local.txt"), "local work")
	gitRun(t, f.checkout, "add", "-A")
	gitRun(t, f.checkout, "commit", "-m", "local work")
	localTip := gitOut(t, f.checkout, "rev-parse", "HEAD")
	gitRun(t, f.checkout, "checkout", "staging")

	if _, err := f.prepare(t, f.request()); err == nil {
		t.Fatalf("a local branch carrying commits the base does not have must be refused")
	} else if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error %q does not name the existing branch", err)
	}
	if got := gitOut(t, f.checkout, "rev-parse", taskWorktreeBranch); got != localTip {
		t.Errorf("the local branch moved to %s, want %s: the refusal must not reset it", got, localTip)
	}
}

func TestTaskWorktree_ExistingLocalBranchAtBaseIsReused(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	baseTip := gitOut(t, f.origin, "rev-parse", "staging")
	gitRun(t, f.checkout, "branch", taskWorktreeBranch, "origin/staging")

	prep, err := f.prepare(t, f.request())
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !prep.Created || prep.Tip != baseTip {
		t.Errorf("Created/Tip = %v/%s, want true/%s", prep.Created, prep.Tip, baseTip)
	}
	if branch := gitOut(t, prep.Dir, "rev-parse", "--abbrev-ref", "HEAD"); branch != taskWorktreeBranch {
		t.Errorf("worktree is on %q, want the existing local branch", branch)
	}
}

func TestTaskWorktree_RejectsBadRequests(t *testing.T) {
	checkout := t.TempDir()
	cases := []struct {
		name string
		req  worktreeRequest
	}{
		{"no checkout", worktreeRequest{TaskID: taskWorktreeID, Branch: taskWorktreeBranch, PRBase: "staging"}},
		{"short task id", worktreeRequest{Checkout: checkout, TaskID: "1a2b3c4", Branch: taskWorktreeBranch, PRBase: "staging"}},
		{"non-hex task id", worktreeRequest{Checkout: checkout, TaskID: "zzzzzzzz-0000", Branch: taskWorktreeBranch, PRBase: "staging"}},
		{"empty task id", worktreeRequest{Checkout: checkout, Branch: taskWorktreeBranch, PRBase: "staging"}},
		{"no branch", worktreeRequest{Checkout: checkout, TaskID: taskWorktreeID, PRBase: "staging"}},
		{"no PR base", worktreeRequest{Checkout: checkout, TaskID: taskWorktreeID, Branch: taskWorktreeBranch}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := prepareTaskWorktree(context.Background(), tc.req); err == nil {
				t.Fatalf("prepareTaskWorktree(%+v) returned no error", tc.req)
			}
		})
	}
}

// Only the variables that ADDRESS a repository may be stripped: the operator's
// own auth travels in GIT_SSH_COMMAND/GIT_ASKPASS and must reach git.
func TestTaskWorktreeGitEnvKeepsOperatorAuth(t *testing.T) {
	t.Setenv("GIT_AUTHOR_NAME", "x")
	t.Setenv("GIT_DIR", "/nope")
	t.Setenv("GIT_SSH_COMMAND", "custom")

	env := taskWorktreeGitEnv()
	for _, want := range []string{"GIT_AUTHOR_NAME=x", "GIT_SSH_COMMAND=custom", "GIT_TERMINAL_PROMPT=0"} {
		if got := taskWorktreeEnvCount(env, want); got != 1 {
			t.Errorf("%s appears %d time(s), want exactly once", want, got)
		}
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_DIR=") {
			t.Errorf("GIT_DIR survived into the git env: %s", kv)
		}
	}
}

func taskWorktreeEnvCount(env []string, kv string) int {
	n := 0
	for _, got := range env {
		if got == kv {
			n++
		}
	}
	return n
}
