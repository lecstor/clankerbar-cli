package supervisor

// The deterministic close-out's tests (CLA-758).
//
// They drive `reconcileCloseOuts` against a REAL git fixture and a REAL plane wire
// shape, because the two things this feature can get wrong are both invisible to a
// stub: whether the merge commit GitHub made is really an ancestor of the base on
// the remote (so the fixture has a bare remote, a fleet clone and a clone the fake
// `gh` merges in), and what the plane is actually told (so the fake plane records
// the `claim_task` / `complete_task` / `release_task` arguments it receives).
//
// The fake `gh` is a shell script on the verifier's own gh path: `pr list` and
// `pr view` answer from JSON files the test rewrites, and the merge endpoint
// performs a real `git merge --no-ff` in a second clone and pushes it, so the
// ancestry check has something true to verify. Its merge mode makes the merge fail,
// or return a sha that never landed, which is what the release-after-claim cases
// need.

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/delivery"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// closeOutTestRunCredential is what a real `claim_task` answers with: the run
// CREDENTIAL `<uuid>.<secret>`, never a bare uuid. The plane's own records
// (`claimedByRun`) hold the bare uuid, so the fixture pins BOTH shapes apart on
// purpose — comparing the two as strings is the defect CLA-759 review 1.review-1.1
// found, and a fixture that served a bare id hid it.
const closeOutTestRunUUID = "run-1"

const closeOutTestRunCredential = closeOutTestRunUUID + ".s3cr3t-session-credential"

const (
	closeOutTestSlug = "acme"
	closeOutTestRepo = "acme/widgets"
	closeOutTestBase = "staging"
	closeOutTestHead = "clanker/close-me"
	closeOutTestTask = "task-1"
	closeOutTestRef  = "CLA-1"
	closeOutTestPR   = 12
)

// closeOutFixture is one project's world: a bare remote, the fleet's clone of it at
// the path the workdir derivation produces, a second clone the fake gh merges and
// pushes from, and the fake gh itself.
type closeOutFixture struct {
	t      *testing.T
	root   string // the machine workdir root
	dir    string // <root>/<slug>/widgets — the fleet's clone
	seed   string // the clone the fake gh merges and pushes from
	remote string // the bare repository both clones share
	base   string // the remote's default branch: the PR base and the merge target
	ghBin  string
	ghDir  string

	plane *fakePlane
	srv   string // roster URL base

	// cacheDir is the state root the fixture's Supervisors share: it is where the
	// fleet's conflict-session slot lives (conflictlock.go), so two Supervisors
	// built from this fixture contend for the same slot exactly as two real ones do.
	cacheDir string

	headOID string // the task branch's tip, as the PR JSON advertises it

	// conflictMu guards the conflict-session record below; the session runs on
	// its own goroutine, so a test that reads it must wait for the session first
	// (f.run does: d.waitConflict()).
	conflictMu  sync.Mutex
	conflicts   []conflictCall
	conflictRun func(ctx context.Context, cfg *config.Config, slug string, req ConflictRunRequest, step config.ConflictStep) (harness.Result, error)
}

// conflictCall is one conflict-only session the fixture was asked to start.
type conflictCall struct {
	req  ConflictRunRequest
	step config.ConflictStep
}

// newCloseOutFixture builds the world with `staging` as the remote's default.
func newCloseOutFixture(t *testing.T, conflicting bool) *closeOutFixture {
	return newCloseOutFixtureOn(t, conflicting, closeOutTestBase)
}

// newCloseOutFixtureOn builds the world around a remote whose default branch is
// base: the branch the seed pushes, the fixture's PR targets, and the fake gh
// merges into. A repo whose PRs target a branch other than the config's
// integration branch needs the remote to carry that branch, so the default is a
// fixture input rather than a constant.
func newCloseOutFixtureOn(t *testing.T, conflicting bool, base string) *closeOutFixture {
	t.Helper()
	root := t.TempDir()
	// The bare remote's PATH carries the repo identity, because the fleet's
	// checkout is attributed to `acme/widgets` by its origin URL
	// (delivery.MatchesRepoSlug) — a bare name would not match and the close-out
	// would (correctly) refuse to work in it.
	remote := filepath.Join(root, "github.com", "acme", "widgets.git")
	mustGit(t, root, "init", "-q", "--bare", "-b", base, remote)

	seed := filepath.Join(root, "seed")
	mustGit(t, root, "clone", "-q", remote, seed)
	mustGit(t, seed, "config", "user.email", "test@example.com")
	mustGit(t, seed, "config", "user.name", "close-out test")
	writeFile(t, filepath.Join(seed, "a.txt"), "one\ntwo\nthree\n")
	writeFile(t, filepath.Join(seed, "b.txt"), "x\ny\n")
	writeFile(t, filepath.Join(seed, "keep.txt"), "keep\n")
	mustGit(t, seed, "add", "-A")
	mustGit(t, seed, "commit", "-q", "-m", "base")
	mustGit(t, seed, "push", "-q", "-u", "origin", base)

	mustGit(t, seed, "checkout", "-q", "-b", closeOutTestHead)
	if conflicting {
		writeFile(t, filepath.Join(seed, "a.txt"), "one\nTWO-feature\nthree\n")
		writeFile(t, filepath.Join(seed, "b.txt"), "x\nY-feature\n")
	} else {
		writeFile(t, filepath.Join(seed, "feature.txt"), "the work\n")
	}
	mustGit(t, seed, "add", "-A")
	mustGit(t, seed, "commit", "-q", "-m", "feature")
	mustGit(t, seed, "push", "-q", "-u", "origin", closeOutTestHead)

	if conflicting {
		// Move the base underneath the branch so the two conflict on BOTH files.
		mustGit(t, seed, "checkout", "-q", base)
		writeFile(t, filepath.Join(seed, "a.txt"), "one\nTWO-base\nthree\n")
		writeFile(t, filepath.Join(seed, "b.txt"), "x\nY-base\n")
		mustGit(t, seed, "add", "-A")
		mustGit(t, seed, "commit", "-q", "-m", "base moves on")
		mustGit(t, seed, "push", "-q", "origin", base)
		mustGit(t, seed, "checkout", "-q", closeOutTestHead)
	}

	dir := filepath.Join(root, closeOutTestSlug, "widgets")
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatalf("mkdir project root: %v", err)
	}
	mustGit(t, root, "clone", "-q", remote, dir)

	f := &closeOutFixture{
		t:        t,
		root:     root,
		dir:      dir,
		seed:     seed,
		remote:   remote,
		base:     base,
		ghDir:    filepath.Join(root, "fakegh"),
		cacheDir: filepath.Join(root, "state", "roster"),
	}
	f.headOID = strings.TrimSpace(mustGit(t, seed, "rev-parse", closeOutTestHead))
	if err := os.MkdirAll(f.ghDir, 0o755); err != nil {
		t.Fatalf("mkdir fake gh: %v", err)
	}
	f.writePR(base, "MERGEABLE", "CLEAN", `[{"__typename":"CheckRun","name":"ci","status":"COMPLETED","conclusion":"SUCCESS"}]`)
	f.writePRList(closeOutTestPR)
	f.setMergeMode("ok")
	f.writeGH()
	return f
}

// writeGH writes the fake gh. Its merge path is the one that does real work: it
// merges the requested head into the requested BASE in the seed clone and pushes,
// so the merge commit GitHub "made" really is on the remote — and a request that
// did not name the base, or went through the PR merge endpoint (which follows the
// PR's own retargetable base), is refused loudly.
func (f *closeOutFixture) writeGH() {
	f.ghBin = filepath.Join(f.ghDir, "gh")
	script := fmt.Sprintf(`#!/bin/sh
set -u
dir=%q
seed=%q
base=%q
head=%q
case "$1 $2" in
"pr list")
  # A test that drives TWO tasks in one repo gives each head branch its own list;
  # otherwise the one shared list answers. (The head is what gh filters on.)
  want=""; prev=""
  for a in "$@"; do
    if [ "$prev" = "--head" ]; then want=$a; fi
    prev=$a
  done
  f="$dir/prlist-$(printf '%%s' "$want" | tr '/' '-').json"
  [ -f "$f" ] || f="$dir/prlist.json"
  cat "$f"
  ;;
"pr view")
  sleep "$(cat "$dir/pr-view-delay" 2>/dev/null || echo 0)"
  f="$dir/prview-$3.json"
  [ -f "$f" ] || f="$dir/prview.json"
  cat "$f"
  ;;
"auth token")
  echo "no token for that account" >&2
  exit 1
  ;;
"api --method")
  # The close-out must never merge through the pull request: that endpoint follows
  # whatever base the PR carries when GitHub processes it.
  case " $* " in
  *"/pulls/"*) echo "refusing: the PR merge endpoint follows a retargetable base" >&2; exit 1 ;;
  esac
  mode=$(cat "$dir/merge-mode" 2>/dev/null || echo ok)
  printf '%%s\n' "$*" > "$dir/merge-args"
  if [ "$mode" = "fail" ]; then
    echo "merge is blocked by branch protection" >&2
    exit 1
  fi
  got_base=""; got_head=""
  for a in "$@"; do
    case "$a" in
    base=*) got_base=${a#base=} ;;
    head=*) got_head=${a#head=} ;;
    esac
  done
  if [ "$got_base" != "$base" ]; then
    echo "merge named base '$got_base', want '$base'" >&2
    exit 1
  fi
  if [ -z "$got_head" ]; then
    echo "merge named no head revision to pin" >&2
    exit 1
  fi
  git -C "$seed" fetch -q origin || exit 1
  git -C "$seed" checkout -q -f "$base" || exit 1
  git -C "$seed" merge -q --no-ff "$got_head" -m "Merge pull request #%d" || exit 1
  git -C "$seed" push -q origin "$base" || exit 1
  sha=$(git -C "$seed" rev-parse HEAD)
  if [ "$mode" = "bogus" ]; then
    sha=0123456789abcdef0123456789abcdef01234567
  fi
  printf '{"merged":true,"sha":"%%s","message":"Pull Request successfully merged"}' "$sha"
  ;;
*)
  echo "fake gh: unhandled: $*" >&2
  exit 9
  ;;
esac
`, f.ghDir, f.seed, f.base, closeOutTestHead, closeOutTestPR)
	if err := os.WriteFile(f.ghBin, []byte(script), 0o755); err != nil {
		f.t.Fatalf("write fake gh: %v", err)
	}
}

func (f *closeOutFixture) setMergeMode(mode string) {
	if err := os.WriteFile(filepath.Join(f.ghDir, "merge-mode"), []byte(mode), 0o600); err != nil {
		f.t.Fatalf("write merge mode: %v", err)
	}
}

// setViewDelay makes the fake gh's `pr view` take d, so a test can prove a pass
// does not hold the supervisor's control path while it polls.
func (f *closeOutFixture) setViewDelay(d time.Duration) {
	secs := fmt.Sprintf("%.3f", d.Seconds())
	if err := os.WriteFile(filepath.Join(f.ghDir, "pr-view-delay"), []byte(secs), 0o600); err != nil {
		f.t.Fatalf("write view delay: %v", err)
	}
}

// mergeArgs is the argument list the fake gh received for its merge call, empty
// when no merge call was made.
func (f *closeOutFixture) mergeArgs() string {
	b, err := os.ReadFile(filepath.Join(f.ghDir, "merge-args"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// writePR rewrites what `gh pr view` answers.
func (f *closeOutFixture) writePR(base, mergeable, state, rollup string) {
	body := fmt.Sprintf(`{"number":%d,"url":"https://github.com/%s/pull/%d","mergeable":%q,"mergeStateStatus":%q,"statusCheckRollup":%s,"baseRefName":%q,"headRefName":%q,"headRefOid":%q}`,
		closeOutTestPR, closeOutTestRepo, closeOutTestPR, mergeable, state, rollup, base, closeOutTestHead, f.headOID)
	if err := os.WriteFile(filepath.Join(f.ghDir, "prview.json"), []byte(body), 0o600); err != nil {
		f.t.Fatalf("write prview: %v", err)
	}
}

// writePRList rewrites what `gh pr list` answers: one entry per number given.
func (f *closeOutFixture) writePRList(numbers ...int) {
	entries := make([]string, 0, len(numbers))
	for _, n := range numbers {
		entries = append(entries, fmt.Sprintf(`{"number":%d,"url":"https://github.com/%s/pull/%d","baseRefName":%q,"headRefName":%q,"headRefOid":%q}`,
			n, closeOutTestRepo, n, f.base, closeOutTestHead, f.headOID))
	}
	body := "[" + strings.Join(entries, ",") + "]"
	if err := os.WriteFile(filepath.Join(f.ghDir, "prlist.json"), []byte(body), 0o600); err != nil {
		f.t.Fatalf("write prlist: %v", err)
	}
}

// addSecondBranch pushes a second feature branch of the same repo, with its own
// PR number (closeOutTestPR+1) and its own `gh` answers, so one pass can be
// driven with two approved tasks that share a repo — the shape the per-pass base
// cache exists for. It returns the branch and its PR number.
func (f *closeOutFixture) addSecondBranch(t *testing.T) (branch string, number int) {
	t.Helper()
	branch = closeOutTestHead + "-b"
	number = closeOutTestPR + 1
	mustGit(t, f.seed, "checkout", "-q", "-b", branch, closeOutTestHead)
	writeFile(t, filepath.Join(f.seed, "second.txt"), "the second work\n")
	mustGit(t, f.seed, "add", "-A")
	mustGit(t, f.seed, "commit", "-q", "-m", "second feature")
	mustGit(t, f.seed, "push", "-q", "-u", "origin", branch)
	headOID := strings.TrimSpace(mustGit(t, f.seed, "rev-parse", branch))

	const passing = `[{"__typename":"CheckRun","name":"ci","status":"COMPLETED","conclusion":"SUCCESS"}]`
	writeJSON := func(name, body string) {
		if err := os.WriteFile(filepath.Join(f.ghDir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	writeJSON(fmt.Sprintf("prview-%d.json", number), fmt.Sprintf(
		`{"number":%d,"url":"https://github.com/%s/pull/%d","mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","statusCheckRollup":%s,"baseRefName":%q,"headRefName":%q,"headRefOid":%q}`,
		number, closeOutTestRepo, number, passing, f.base, branch, headOID))
	writeJSON("prlist-"+strings.ReplaceAll(branch, "/", "-")+".json", fmt.Sprintf(
		`[{"number":%d,"url":"https://github.com/%s/pull/%d","baseRefName":%q,"headRefName":%q,"headRefOid":%q}]`,
		number, closeOutTestRepo, number, f.base, branch, headOID))
	return branch, number
}

// servePlane starts the fake plane and answers the close-out's reads: the policy
// dial, the approved queue, and the claim's run id. authority is the dial.
func (f *closeOutFixture) servePlane(t *testing.T, authority string) {
	t.Helper()
	f.plane = &fakePlane{}
	f.plane.serveTool("get_project_policy", fmt.Sprintf(`{"policy":{"authorityIntegrate":%q,"topology":"staging_then_promote"}}`, authority))
	f.plane.serveTool("list_tasks", fmt.Sprintf(
		`{"tasks":[{"id":%q,"ref":%q,"title":"a task","branch":%q,"repo":%q}],"count":1,"total":1,"view":"full"}`,
		closeOutTestTask, closeOutTestRef, closeOutTestHead, closeOutTestRepo))
	f.plane.serveTool("claim_task", `{"task":{"id":"`+closeOutTestTask+`"},"run":{"id":"`+closeOutTestRunCredential+`"}}`)
	// The post-session read (CLA-759): the default is the hand-back the conflict
	// brief asks for, so a test that does not care about the classification gets
	// the ordinary mechanical outcome. A test overrides this payload to drive
	// the behavioural, failed and still-held cases. `claimedByRun` is NULL here
	// because the session released its claim, exactly as the brief asks.
	f.plane.serveTool("get_task", fmt.Sprintf(
		`{"task":{"id":%q,"ref":%q,"status":"approved","claimedByRun":null,"branch":%q,"outcome":"RESOLUTION: mechanical\n\n**Tests**\n- `+closeOutTestBase+`: pass\n\n- a.txt: kept both sides\n- b.txt: kept both sides"}}`,
		closeOutTestTask, closeOutTestRef, closeOutTestHead))
	srv := f.plane.serve(t)
	f.srv = srv.URL
}

// supervisor builds a Supervisor holding one running instance for this project —
// the state reconcile would have produced — so the pass can be driven directly.
// pollWindow shrinks the PR judgement's poll budget (0 = production default).
func (f *closeOutFixture) supervisor(pollWindow time.Duration) *Supervisor {
	f.t.Helper()
	o := Options{
		RosterURL:   f.srv + "/api/daemon-roster",
		APIKey:      "test-key",
		WorkdirRoot: f.root,
		CloseOutVerifier: func(dir string) *delivery.Verifier {
			return delivery.New(dir, "").WithGHBin(f.ghBin).WithPollWindow(pollWindow, 20*time.Millisecond)
		},
		ConflictRun: func(ctx context.Context, cfg *config.Config, slug string, req ConflictRunRequest, step config.ConflictStep) (harness.Result, error) {
			f.conflictMu.Lock()
			f.conflicts = append(f.conflicts, conflictCall{req: req, step: step})
			hook := f.conflictRun
			f.conflictMu.Unlock()
			if hook != nil {
				return hook(ctx, cfg, slug, req, step)
			}
			return harness.Result{}, nil
		},
	}.withDefaults()
	d := &Supervisor{o: o, ctx: context.Background(), cacheDir: f.cacheDir}
	cfg := &config.Config{
		Harness: "opencode",
		Model:   "implement-model",
		Projects: []config.Project{{
			Slug:              closeOutTestSlug,
			PrimaryRepo:       closeOutTestRepo,
			IntegrationBranch: closeOutTestBase,
		}},
	}
	d.instances = []*Instance{{
		name:    "inst-1",
		entry:   &RosterEntry{Name: "inst-1", DesiredState: RosterDesiredRunning, Placement: RosterPlacementLocal},
		cfg:     cfg,
		desired: RosterDesiredRunning,
	}}
	return d
}

// conflictCalls returns the conflict sessions the fixture was asked to start.
func (f *closeOutFixture) conflictCalls() []conflictCall {
	f.conflictMu.Lock()
	defer f.conflictMu.Unlock()
	return append([]conflictCall(nil), f.conflicts...)
}

// setConflictRun installs a hook the fake session runner calls, so a test can
// block the session (the at-most-one case) or return a result/error.
func (f *closeOutFixture) setConflictRun(hook func(ctx context.Context, cfg *config.Config, slug string, req ConflictRunRequest, step config.ConflictStep) (harness.Result, error)) {
	f.conflictMu.Lock()
	f.conflictRun = hook
	f.conflictMu.Unlock()
}

// run drives the pass once with the log captured. It waits for any conflict-only
// session the pass starts, so assertions about the session's outcome see a
// completed hand-back rather than a goroutine still running.
func (f *closeOutFixture) run(d *Supervisor) string {
	f.t.Helper()
	return f.runWith(d, nil)
}

// runWith is run with a seam between the pass and the wait: a conflict session
// that blocks the pass's own progress can be released there, deterministically,
// without the sleep a "wait until the pass has moved on" test would need.
func (f *closeOutFixture) runWith(d *Supervisor, beforeWait func()) string {
	f.t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	d.reconcileCloseOuts()
	if beforeWait != nil {
		beforeWait()
	}
	d.waitConflict()
	return buf.String()
}

// callsFor returns the MCP calls the plane received for one tool, in order.
func (f *closeOutFixture) callsFor(tool string) []planeCall {
	var out []planeCall
	for _, c := range f.plane.planeCalls() {
		if c.tool == tool {
			out = append(out, c)
		}
	}
	return out
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, stderr.String())
	}
	return stdout.String()
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// --- the clean close-out -----------------------------------------------------

func TestCloseOutMergesACleanApprovedTask(t *testing.T) {
	f := newCloseOutFixture(t, false)
	f.servePlane(t, "clanker")
	d := f.supervisor(0)

	lines := f.run(d)

	claims := f.callsFor("claim_task")
	if len(claims) != 1 {
		t.Fatalf("claim_task calls = %d, want 1 (recorded calls: %v)", len(claims), f.plane.planeCalls())
	}
	if got := claims[0].args["taskId"]; got != closeOutTestTask {
		t.Errorf("claim_task taskId = %v, want %q", got, closeOutTestTask)
	}

	closedCalls := f.callsFor("complete_task")
	if len(closedCalls) != 1 {
		t.Fatalf("complete_task calls = %d, want 1 (the task was not closed). calls: %v", len(closedCalls), f.plane.planeCalls())
	}
	closed := closedCalls[0].args
	if got := closed["runId"]; got != closeOutTestRunCredential {
		t.Errorf("close runId = %v, want the claim's run credential (%s)", got, closeOutTestRunCredential)
	}
	delivery, ok := closed["delivery"].(map[string]any)
	if !ok {
		t.Fatalf("done call carries no delivery block: %v", closed)
	}
	if got := delivery["integrationBranch"]; got != closeOutTestBase {
		t.Errorf("delivery.integrationBranch = %v, want %q", got, closeOutTestBase)
	}
	if got := delivery["pr"]; got != fmt.Sprintf("#%d", closeOutTestPR) {
		t.Errorf("delivery.pr = %v, want #%d", got, closeOutTestPR)
	}
	if got := delivery["mergeVerified"]; got != true {
		t.Errorf("delivery.mergeVerified = %v, want true (the driver ran the ancestor check)", got)
	}
	mergeSHA, _ := delivery["commit"].(string)
	if len(mergeSHA) != 40 {
		t.Fatalf("delivery.commit = %q, want the merge commit's full sha", mergeSHA)
	}

	// The merge commit really landed on the remote base, and it is a MERGE commit
	// (two parents) — never a squash.
	parents := strings.Fields(strings.TrimSpace(mustGit(t, f.remote, "rev-list", "--parents", "-n", "1", mergeSHA)))
	if len(parents) != 3 {
		t.Errorf("merge commit %s has %d parents, want 2 (a merge commit, never a squash): %v", mergeSHA, len(parents)-1, parents)
	}
	mustGit(t, f.remote, "merge-base", "--is-ancestor", mergeSHA, "refs/heads/"+closeOutTestBase)

	// The merge named the base and the judged head itself: a retarget of the PR
	// after the judgement cannot redirect it, and a push that landed after it
	// cannot be what gets merged.
	args := f.mergeArgs()
	for _, want := range []string{"repos/" + closeOutTestRepo + "/merges", "base=" + closeOutTestBase, "head=" + f.headOID} {
		if !strings.Contains(args, want) {
			t.Errorf("the merge invocation does not carry %q: %q", want, args)
		}
	}
	for _, forbidden := range []string{"/pulls/", "merge_method="} {
		if strings.Contains(args, forbidden) {
			t.Errorf("the merge went through the PR endpoint (%q), which follows a retargetable base: %q", forbidden, args)
		}
	}

	if !strings.Contains(lines, "task closed done") {
		t.Errorf("the close-out did not report the close:\n%s", lines)
	}
}

// A repo whose origin carries no `staging`: its default branch is `integration`,
// which is exactly what a phase brief resolves for it (config.ResolvePRBase: no
// pr_base, no staging on origin, so origin's default). The close-out must merge
// into that same branch, not judge the PR against the config default - the
// disagreement that refused these correct deliveries as release-targeted.
func TestCloseOutMergesTheBaseTheBriefResolved(t *testing.T) {
	f := newCloseOutFixtureOn(t, false, "integration")
	f.servePlane(t, "clanker")
	d := f.supervisor(0)
	// Neither pr_base nor integration_branch configured: only the origin probe
	// can name this repo's merge target.
	d.instances[0].cfg.Projects[0].IntegrationBranch = ""
	d.instances[0].cfg.Projects[0].PRBase = nil

	lines := f.run(d)

	closedCalls := f.callsFor("complete_task")
	if len(closedCalls) != 1 {
		t.Fatalf("complete_task calls = %d, want 1 (the PR into integration is a correct delivery). lines:\n%s", len(closedCalls), lines)
	}
	decl, ok := closedCalls[0].args["delivery"].(map[string]any)
	if !ok {
		t.Fatalf("done call carries no delivery block: %v", closedCalls[0].args)
	}
	if got := decl["integrationBranch"]; got != "integration" {
		t.Errorf("delivery.integrationBranch = %v, want %q - the base the driver's prBaseFor resolves", got, "integration")
	}
	if got := f.mergeArgs(); !strings.Contains(got, "base=integration") {
		t.Errorf("the merge did not name base=integration: %q", got)
	}
	mergeSHA, _ := decl["commit"].(string)
	if len(mergeSHA) != 40 {
		t.Fatalf("delivery.commit = %q, want the merge commit's sha", mergeSHA)
	}
	mustGit(t, f.remote, "merge-base", "--is-ancestor", mergeSHA, "refs/heads/integration")
	if strings.Contains(lines, "release-targeted") {
		t.Errorf("a PR into the resolved merge target was called release-targeted:\n%s", lines)
	}
	if !strings.Contains(lines, `integrates on "integration"`) {
		t.Errorf("the close-out did not report the probed merge target:\n%s", lines)
	}
}

// A pass that works TWO approved tasks in one repo resolves that repo's merge
// target ONCE. The probe is three process spawns and a network round-trip, and the
// supervisor starts a pass on every roster poll, so a queue of N approved tasks in
// one repo must not pay it N times (CLA-833 review 1.review-2.2).
func TestCloseOutResolvesEachReposBaseOncePerPass(t *testing.T) {
	f := newCloseOutFixture(t, false)
	secondBranch, secondPR := f.addSecondBranch(t)
	f.servePlane(t, "clanker")
	f.plane.serveTool("list_tasks", fmt.Sprintf(
		`{"tasks":[{"id":%q,"ref":%q,"title":"one","branch":%q,"repo":%q},`+
			`{"id":"task-2","ref":"CLA-2","title":"two","branch":%q,"repo":%q}],"count":2,"total":2,"view":"full"}`,
		closeOutTestTask, closeOutTestRef, closeOutTestHead, closeOutTestRepo, secondBranch, closeOutTestRepo))
	d := f.supervisor(0)

	probes := 0
	old := closeOutOriginProbe
	closeOutOriginProbe = func(context.Context, string, string) (bool, string, error) {
		probes++
		return true, "", nil
	}
	defer func() { closeOutOriginProbe = old }()

	lines := f.run(d)

	if probes != 1 {
		t.Errorf("origin was probed %d times for two tasks in one repo, want 1:\n%s", probes, lines)
	}
	if got := len(f.callsFor("claim_task")); got != 2 {
		t.Errorf("claim_task calls = %d, want 2 (one per task):\n%s", got, lines)
	}
	closed := map[string]string{}
	for _, c := range f.callsFor("complete_task") {
		taskID, _ := c.args["taskId"].(string)
		decl, _ := c.args["delivery"].(map[string]any)
		pr, _ := decl["pr"].(string)
		closed[taskID] = pr
	}
	if len(closed) != 2 {
		t.Fatalf("complete_task closed %d task(s), want 2:\n%s", len(closed), lines)
	}
	if got := closed["task-2"]; got != fmt.Sprintf("#%d", secondPR) {
		t.Errorf("the second task's delivery.pr = %q, want #%d", got, secondPR)
	}
}

// The memo is keyed by repo AND integration branch: a second repo is a second
// question, and so is the same repo under a changed integration branch.
func TestCloseOutBasesProbeOncePerRepoAndIntegrationBranch(t *testing.T) {
	old := closeOutOriginProbe
	defer func() { closeOutOriginProbe = old }()
	var asked []string
	closeOutOriginProbe = func(_ context.Context, dir, integration string) (bool, string, error) {
		asked = append(asked, dir+" "+integration)
		return true, "", nil
	}
	cfg := &config.Config{Projects: []config.Project{{Slug: closeOutTestSlug, IntegrationBranch: closeOutTestBase}}}
	bases := newCloseOutBases()
	ctx := context.Background()

	first := bases.resolve(ctx, "/checkouts/widgets", cfg, closeOutTestSlug, closeOutTestRepo)
	second := bases.resolve(ctx, "/checkouts/widgets", cfg, closeOutTestSlug, closeOutTestRepo)
	if len(asked) != 1 {
		t.Fatalf("origin was probed %d times for one repo resolved twice, want 1: %v", len(asked), asked)
	}
	if first != second {
		t.Errorf("the memoised resolution changed between calls: %+v then %+v", first, second)
	}
	if first.Base != closeOutTestBase {
		t.Errorf("Base = %q, want the integration branch %q", first.Base, closeOutTestBase)
	}

	bases.resolve(ctx, "/checkouts/other", cfg, closeOutTestSlug, "acme/other")
	if len(asked) != 2 {
		t.Errorf("a task in a second repo did not probe: %v", asked)
	}

	cfg.Projects[0].IntegrationBranch = "integration"
	bases.resolve(ctx, "/checkouts/widgets", cfg, closeOutTestSlug, closeOutTestRepo)
	if len(asked) != 3 {
		t.Errorf("a changed integration branch reused the cached answer: %v", asked)
	}
}

// The close-out's probe carries its OWN short bound, not the driver's 30s one: it
// runs inside a task budget shared with the PR judgement, the claim and the merge
// check, and a stalled origin must not be able to cancel a merge mid-flight
// (CLA-833 review 1.review-2.1). A probe that cannot answer still yields the
// fail-closed integration-branch fallback.
func TestCloseOutBasesBoundsItsOriginProbe(t *testing.T) {
	oldProbe, oldTimeout := closeOutOriginProbe, closeOutBaseProbeTimeout
	defer func() {
		closeOutOriginProbe, closeOutBaseProbeTimeout = oldProbe, oldTimeout
	}()
	closeOutBaseProbeTimeout = 40 * time.Millisecond
	closeOutOriginProbe = func(ctx context.Context, dir, integration string) (bool, string, error) {
		<-ctx.Done()
		return false, "", ctx.Err()
	}
	cfg := &config.Config{Projects: []config.Project{{Slug: closeOutTestSlug, IntegrationBranch: closeOutTestBase}}}

	start := time.Now()
	res := newCloseOutBases().resolve(context.Background(), "/checkouts/widgets", cfg, closeOutTestSlug, closeOutTestRepo)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the probe was not bounded by the close-out's own timeout: resolve took %s, its bound is %s", elapsed, closeOutBaseProbeTimeout)
	}
	if res.Err == nil || res.Source != config.PRBaseIntegrationFallback || res.Base != closeOutTestBase {
		t.Errorf("a probe that could not answer resolved to %+v, want the integration-branch fallback %q with its error", res, closeOutTestBase)
	}
}

// A PR based on a branch that is neither the resolved merge target nor a release
// line is refused, and the line names BOTH bases without calling it a promotion.
func TestCloseOutNamesBothBasesOnANonPromotionMismatch(t *testing.T) {
	const passing = `[{"__typename":"CheckRun","name":"ci","status":"COMPLETED","conclusion":"SUCCESS"}]`
	f := newCloseOutFixture(t, false)
	f.servePlane(t, "clanker")
	f.writePR("release-2", "MERGEABLE", "CLEAN", passing)
	d := f.supervisor(0)

	lines := f.run(d)

	if got := f.callsFor("claim_task"); len(got) != 0 {
		t.Errorf("a PR based on another branch was claimed: %v", got)
	}
	if got := f.mergeArgs(); got != "" {
		t.Errorf("a PR based on another branch was merged: %q", got)
	}
	for _, want := range []string{`"release-2"`, `"staging"`} {
		if !strings.Contains(lines, want) {
			t.Errorf("the mismatch line does not name %s:\n%s", want, lines)
		}
	}
	if strings.Contains(lines, "release-targeted") {
		t.Errorf("a non-promotion base was called release-targeted:\n%s", lines)
	}
}

// --- stand-downs -------------------------------------------------------------

func TestCloseOutStandsDownWhenThePRIsNotReady(t *testing.T) {
	const passing = `[{"__typename":"CheckRun","name":"ci","status":"COMPLETED","conclusion":"SUCCESS"}]`
	cases := []struct {
		name      string
		mergeable string
		state     string
		rollup    string
		want      string
	}{
		{"failing check", "MERGEABLE", "CLEAN", `[{"__typename":"CheckRun","name":"ci","status":"COMPLETED","conclusion":"FAILURE"}]`, "FAILING checks"},
		{"empty rollup", "MERGEABLE", "CLEAN", `[]`, "NO checks"},
		{"unknown past the budget", "UNKNOWN", "UNKNOWN", passing, "did not report whether"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCloseOutFixture(t, false)
			f.servePlane(t, "clanker")
			f.writePR(closeOutTestBase, tc.mergeable, tc.state, tc.rollup)
			d := f.supervisor(150 * time.Millisecond)

			lines := f.run(d)

			if got := f.callsFor("claim_task"); len(got) != 0 {
				t.Errorf("claim_task was called for a PR that is not ready: %v", got)
			}
			if got := f.callsFor("complete_task"); len(got) != 0 {
				t.Errorf("complete_task was called for a PR that is not ready: %v", got)
			}
			if !strings.Contains(lines, tc.want) {
				t.Errorf("the stand-down line does not say %q:\n%s", tc.want, lines)
			}
		})
	}
}

func TestCloseOutConflictNamesEveryPathOncePerPair(t *testing.T) {
	f := newCloseOutFixture(t, true)
	f.servePlane(t, "clanker")
	f.writePR(closeOutTestBase, "CONFLICTING", "DIRTY", `[{"__typename":"CheckRun","name":"ci","status":"COMPLETED","conclusion":"SUCCESS"}]`)
	d := f.supervisor(0)

	first := f.run(d)

	for _, want := range []string{"CONFLICTING", "a.txt", "b.txt", closeOutTestRef} {
		if !strings.Contains(first, want) {
			t.Errorf("the conflict line does not name %q:\n%s", want, first)
		}
	}
	// The conflict stand-down now takes the claim itself (CLA-759): the claim is
	// the lock the conflict-only session works under, and is what makes "a task
	// already claimed gets no session" true rather than hoped for.
	if got := f.callsFor("claim_task"); len(got) != 1 {
		t.Errorf("claim_task calls = %d, want exactly the conflict session's claim: %v", len(got), got)
	}
	if got := f.conflictCalls(); len(got) != 1 {
		t.Fatalf("conflict sessions = %d, want exactly one", len(got))
	}

	// The claim moved the task out of the approved queue, so the next poll has
	// nothing to report for it: the evidence line is one per head/base pair, and
	// an unchanged conflict is silent.
	f.plane.serveTool("list_tasks", `{"tasks":[],"count":0,"total":0,"view":"full"}`)
	second := f.run(d)

	if strings.Contains(second, "CONFLICTING") {
		t.Errorf("an unchanged head/base pair was reported twice:\n%s", second)
	}
	if got := f.conflictCalls(); len(got) != 1 {
		t.Errorf("conflict sessions = %d after a poll with no approved task, want 1", len(got))
	}
}

func TestCloseOutNeverMergesAMainBasedPR(t *testing.T) {
	t.Run("the PR targets main", func(t *testing.T) {
		f := newCloseOutFixture(t, false)
		f.servePlane(t, "clanker")
		f.writePR("main", "MERGEABLE", "CLEAN", `[{"__typename":"CheckRun","name":"ci","status":"COMPLETED","conclusion":"SUCCESS"}]`)
		d := f.supervisor(0)

		lines := f.run(d)

		if got := f.callsFor("claim_task"); len(got) != 0 {
			t.Errorf("a main-based PR was claimed: %v", got)
		}
		if !strings.Contains(lines, `not this project's merge target "staging"`) {
			t.Errorf("the stand-down does not name the base mismatch:\n%s", lines)
		}
		if !strings.Contains(lines, "release-targeted") {
			t.Errorf("a promotion-base mismatch is not named as release-targeted:\n%s", lines)
		}
	})

	t.Run("the project integrates into main", func(t *testing.T) {
		f := newCloseOutFixture(t, false)
		f.servePlane(t, "clanker")
		f.writePR("main", "MERGEABLE", "CLEAN", `[{"__typename":"CheckRun","name":"ci","status":"COMPLETED","conclusion":"SUCCESS"}]`)
		// The configured integration branch is the base only when origin HAS it.
		mustGit(t, f.seed, "branch", "main", closeOutTestBase)
		mustGit(t, f.seed, "push", "-q", "origin", "main")
		d := f.supervisor(0)
		d.instances[0].cfg.Projects[0].IntegrationBranch = "main"

		lines := f.run(d)

		if got := f.callsFor("claim_task"); len(got) != 0 {
			t.Errorf("an approved task integrating into main was claimed: %v", got)
		}
		if !strings.Contains(lines, "promotion is the operator's") {
			t.Errorf("the stand-down does not name the promotion branch:\n%s", lines)
		}
	})
}

func TestCloseOutStandsDownWhenTheJudgedPRNamesNoBase(t *testing.T) {
	const passing = `[{"__typename":"CheckRun","name":"ci","status":"COMPLETED","conclusion":"SUCCESS"}]`
	f := newCloseOutFixture(t, false)
	f.servePlane(t, "clanker")
	// The judged response simply does not carry baseRefName.
	body := fmt.Sprintf(`{"number":%d,"url":"https://github.com/%s/pull/%d","mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","statusCheckRollup":%s,"headRefName":%q,"headRefOid":%q}`,
		closeOutTestPR, closeOutTestRepo, closeOutTestPR, passing, closeOutTestHead, f.headOID)
	if err := os.WriteFile(filepath.Join(f.ghDir, "prview.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write prview: %v", err)
	}
	d := f.supervisor(0)

	lines := f.run(d)

	if got := f.callsFor("claim_task"); len(got) != 0 {
		t.Errorf("a PR whose base the judgement did not name was claimed: %v", got)
	}
	if got := f.mergeArgs(); got != "" {
		t.Errorf("a PR whose base the judgement did not name was merged: %q", got)
	}
	if !strings.Contains(lines, "without a base branch") {
		t.Errorf("the stand-down does not say the base was missing:\n%s", lines)
	}
}

func TestCloseOutPassNeverHoldsTheReconcileLoop(t *testing.T) {
	f := newCloseOutFixture(t, false)
	f.servePlane(t, "clanker")
	f.setViewDelay(600 * time.Millisecond)
	d := f.supervisor(0)

	start := time.Now()
	d.startCloseOutPass()
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("starting the pass blocked the caller for %s: reconcile/select would have been held", elapsed)
	}
	// The next poll arrives while the pass is still working: it must not start a
	// second pass over the same queue.
	d.startCloseOutPass()
	d.closeOutWG.Wait()

	if got := len(f.callsFor("claim_task")); got != 1 {
		t.Errorf("claim_task calls = %d, want 1 (one pass at a time)", got)
	}
}

func TestCloseOutPassStopsWhenTheFleetStops(t *testing.T) {
	f := newCloseOutFixture(t, false)
	f.servePlane(t, "clanker")
	// The judgement takes two seconds; the fleet stops while it is in flight.
	f.setViewDelay(2 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	d := f.supervisor(0)
	d.ctx = ctx

	done := make(chan struct{})
	go func() {
		d.reconcileCloseOuts()
		close(done)
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled fleet did not abort the close-out pass: shutdown would wait out its budget")
	}
	if got := f.callsFor("claim_task"); len(got) != 0 {
		t.Errorf("a cancelled fleet still claimed a task: %v", got)
	}
	if got := f.mergeArgs(); got != "" {
		t.Errorf("a cancelled fleet still merged: %q", got)
	}
}

func TestCloseOutStandsDownWithoutOneIdentifiedPR(t *testing.T) {
	cases := []struct {
		name   string
		prs    []int
		says   string
		hasRef bool
	}{
		{"no open PR", nil, "no OPEN pull request", false},
		{"two open PRs", []int{12, 13}, "refusing to guess", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCloseOutFixture(t, false)
			f.servePlane(t, "clanker")
			f.writePRList(tc.prs...)
			d := f.supervisor(0)

			lines := f.run(d)

			if got := f.callsFor("claim_task"); len(got) != 0 {
				t.Errorf("a task was claimed with no single PR to merge: %v", got)
			}
			if !strings.Contains(lines, tc.says) {
				t.Errorf("the stand-down does not say %q:\n%s", tc.says, lines)
			}
			if tc.hasRef && !strings.Contains(lines, "#13") {
				t.Errorf("the stand-down does not name both PRs:\n%s", lines)
			}
		})
	}
}

func TestCloseOutUsesTheConfiguredCheckoutWithoutAMachineRoot(t *testing.T) {
	f := newCloseOutFixture(t, false)
	f.servePlane(t, "clanker")
	d := f.supervisor(0)
	// No machine root: the checkout comes from the operator's own wiring, exactly
	// as a session's spawn cwd does (repos/primary_repo conventions).
	d.o.WorkdirRoot = ""
	d.instances[0].cfg.WorkDir = filepath.Join(f.root, closeOutTestSlug)

	lines := f.run(d)

	if got := f.callsFor("complete_task"); len(got) == 0 {
		t.Fatalf("the close-out did not run on the configured checkout:\n%s", lines)
	}
	if !strings.Contains(lines, "task closed done") {
		t.Errorf("the configured checkout did not close the task:\n%s", lines)
	}
}

func TestCloseOutStandsDownUnderHumanIntegrate(t *testing.T) {
	f := newCloseOutFixture(t, false)
	f.servePlane(t, "human")
	d := f.supervisor(0)

	lines := f.run(d)

	if got := f.callsFor("list_tasks"); len(got) != 0 {
		t.Errorf("the approved queue was listed under authorityIntegrate: human: %v", got)
	}
	if got := f.callsFor("claim_task"); len(got) != 0 {
		t.Errorf("a task was claimed under authorityIntegrate: human: %v", got)
	}
	if !strings.Contains(lines, "authorityIntegrate") {
		t.Errorf("the stand-down does not name the dial:\n%s", lines)
	}
}

func TestCloseOutClaimRefusedDoesNotMerge(t *testing.T) {
	f := newCloseOutFixture(t, false)
	f.servePlane(t, "clanker")
	f.plane.refuseWhen = func(tool string, _ map[string]any) string {
		if tool == "claim_task" {
			return "claim_lost: the task changed hands after this call started"
		}
		return ""
	}
	before := strings.TrimSpace(mustGit(t, f.remote, "rev-parse", "refs/heads/"+closeOutTestBase))
	d := f.supervisor(0)

	lines := f.run(d)

	if got := append(f.callsFor("complete_task"), f.callsFor("release_task")...); len(got) != 0 {
		t.Errorf("the plane was written to after a refused claim: %v", got)
	}
	after := strings.TrimSpace(mustGit(t, f.remote, "rev-parse", "refs/heads/"+closeOutTestBase))
	if after != before {
		t.Errorf("the base moved (%s -> %s) after a refused claim: something merged anyway", before, after)
	}
	if !strings.Contains(lines, "claim was refused") {
		t.Errorf("a refused claim did not stop the close-out:\n%s", lines)
	}
}

func TestCloseOutReleasesTheClaimAfterAMergeFailure(t *testing.T) {
	f := newCloseOutFixture(t, false)
	f.servePlane(t, "clanker")
	f.setMergeMode("fail")
	d := f.supervisor(0)

	lines := f.run(d)

	releases := 0
	for _, c := range f.callsFor("release_task") {
		releases++
		if c.args["runId"] != closeOutTestRunCredential {
			t.Errorf("release runId = %v, want the claim's run credential (%s)", c.args["runId"], closeOutTestRunCredential)
		}
		// A failed MERGE is what the plane's repair ladder must bound: the
		// checks are still green, so attesting green here would leave a
		// green-but-unmergeable PR re-claimed and re-released forever.
		if c.args["checkGreen"] != false {
			t.Errorf("a merge failure released with checkGreen=%v, want false (the ladder must count it): %v", c.args["checkGreen"], c.args)
		}
	}
	if got := f.callsFor("complete_task"); len(got) != 0 {
		t.Errorf("the task was closed after a failed merge: %v", got)
	}
	if releases != 1 {
		t.Fatalf("release calls = %d, want 1 (the claim must never be left held). calls: %v", releases, f.plane.planeCalls())
	}
	if !strings.Contains(lines, "claim released") {
		t.Errorf("the release was not reported:\n%s", lines)
	}
}

func TestCloseOutClosesAMergeItCannotVerify(t *testing.T) {
	f := newCloseOutFixture(t, false)
	f.servePlane(t, "clanker")
	f.setMergeMode("bogus")
	// The verification retries within the verifier's poll window; shrink it so the
	// test does not wait out the production window.
	d := f.supervisor(150 * time.Millisecond)

	lines := f.run(d)

	if released := f.callsFor("release_task"); len(released) != 0 {
		t.Errorf("a merge that LANDED was released: the PR is no longer OPEN, so the task would be stranded on approved forever: %v", released)
	}
	closedCalls := f.callsFor("complete_task")
	if len(closedCalls) != 1 {
		t.Fatalf("a merge that landed but could not be verified was not closed. calls: %v", f.plane.planeCalls())
	}
	closed := closedCalls[0].args
	delivery, ok := closed["delivery"].(map[string]any)
	if !ok {
		t.Fatalf("done call carries no delivery block: %v", closed)
	}
	if got := delivery["mergeVerified"]; got != false {
		t.Errorf("delivery.mergeVerified = %v, want false (the merge landed, the ancestor check did not settle)", got)
	}
	if got, _ := delivery["commit"].(string); len(got) != 40 {
		t.Errorf("delivery.commit = %q, want the merge commit the API reported", got)
	}
	if !strings.Contains(lines, "mergeVerified:false") {
		t.Errorf("the close was not reported as unverified:\n%s", lines)
	}
}

func TestCloseOutReleasesTheClaimWhenThePlaneRefusesTheClose(t *testing.T) {
	f := newCloseOutFixture(t, false)
	f.servePlane(t, "clanker")
	f.plane.refuseWhen = func(tool string, args map[string]any) string {
		if tool == "complete_task" {
			return "done refused: evidence_required"
		}
		return ""
	}
	d := f.supervisor(0)

	lines := f.run(d)

	if releases := f.releases(); releases != 1 {
		t.Fatalf("release calls = %d, want 1 after the plane refused the close: %v", releases, f.plane.planeCalls())
	}
	if !strings.Contains(lines, "refused to close the task") {
		t.Errorf("the stand-down does not name the refused close:\n%s", lines)
	}
}

func TestCloseOutNotWiredIsSilentOnThePlane(t *testing.T) {
	f := newCloseOutFixture(t, false)
	f.servePlane(t, "clanker")
	d := f.supervisor(0)
	d.instances[0].cfg.Projects[0].Slug = "acme"

	// No API key: nothing can be claimed, so the pass must stand down before any
	// wire call rather than merge on a queue it could not read.
	d.o.APIKey = ""
	lines := f.run(d)

	if !strings.Contains(lines, "no plane endpoint") {
		t.Errorf("the pass did not say it had no plane to work against:\n%s", lines)
	}
	if got := f.callsFor("claim_task"); len(got) != 0 {
		t.Errorf("a task was claimed with no plane wired: %v", got)
	}
}

// releases counts the release calls the plane received.
func (f *closeOutFixture) releases() int {
	return len(f.callsFor("release_task"))
}
