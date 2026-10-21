package supervisor

// The deterministic close-out's tests (CLA-758).
//
// They drive `reconcileCloseOuts` against a REAL git fixture and a REAL plane wire
// shape, because the two things this feature can get wrong are both invisible to a
// stub: whether the merge commit GitHub made is really an ancestor of the base on
// the remote (so the fixture has a bare remote, a fleet clone and a clone the fake
// `gh` merges in), and what the plane is actually told (so the fake plane records
// the `claim_task` / `update_task` arguments it receives).
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
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/delivery"
)

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
	ghBin  string
	ghDir  string

	plane *fakePlane
	srv   string // roster URL base

	headOID string // the task branch's tip, as the PR JSON advertises it
}

// newCloseOutFixture builds the world. conflicting controls whether the two
// branches really do conflict on two files (a.txt and b.txt) or touch different
// files and merge cleanly.
func newCloseOutFixture(t *testing.T, conflicting bool) *closeOutFixture {
	t.Helper()
	root := t.TempDir()
	// The bare remote's PATH carries the repo identity, because the fleet's
	// checkout is attributed to `acme/widgets` by its origin URL
	// (delivery.MatchesRepoSlug) — a bare name would not match and the close-out
	// would (correctly) refuse to work in it.
	remote := filepath.Join(root, "github.com", "acme", "widgets.git")
	mustGit(t, root, "init", "-q", "--bare", "-b", closeOutTestBase, remote)

	seed := filepath.Join(root, "seed")
	mustGit(t, root, "clone", "-q", remote, seed)
	mustGit(t, seed, "config", "user.email", "test@example.com")
	mustGit(t, seed, "config", "user.name", "close-out test")
	writeFile(t, filepath.Join(seed, "a.txt"), "one\ntwo\nthree\n")
	writeFile(t, filepath.Join(seed, "b.txt"), "x\ny\n")
	writeFile(t, filepath.Join(seed, "keep.txt"), "keep\n")
	mustGit(t, seed, "add", "-A")
	mustGit(t, seed, "commit", "-q", "-m", "base")
	mustGit(t, seed, "push", "-q", "-u", "origin", closeOutTestBase)

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
		mustGit(t, seed, "checkout", "-q", closeOutTestBase)
		writeFile(t, filepath.Join(seed, "a.txt"), "one\nTWO-base\nthree\n")
		writeFile(t, filepath.Join(seed, "b.txt"), "x\nY-base\n")
		mustGit(t, seed, "add", "-A")
		mustGit(t, seed, "commit", "-q", "-m", "base moves on")
		mustGit(t, seed, "push", "-q", "origin", closeOutTestBase)
		mustGit(t, seed, "checkout", "-q", closeOutTestHead)
	}

	dir := filepath.Join(root, closeOutTestSlug, "widgets")
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatalf("mkdir project root: %v", err)
	}
	mustGit(t, root, "clone", "-q", remote, dir)

	f := &closeOutFixture{
		t:      t,
		root:   root,
		dir:    dir,
		seed:   seed,
		remote: remote,
		ghDir:  filepath.Join(root, "fakegh"),
	}
	f.headOID = strings.TrimSpace(mustGit(t, seed, "rev-parse", closeOutTestHead))
	if err := os.MkdirAll(f.ghDir, 0o755); err != nil {
		t.Fatalf("mkdir fake gh: %v", err)
	}
	f.writePR("staging", "MERGEABLE", "CLEAN", `[{"__typename":"CheckRun","name":"ci","status":"COMPLETED","conclusion":"SUCCESS"}]`)
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
  cat "$dir/prlist.json"
  ;;
"pr view")
  sleep "$(cat "$dir/pr-view-delay" 2>/dev/null || echo 0)"
  cat "$dir/prview.json"
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
`, f.ghDir, f.seed, closeOutTestBase, closeOutTestHead, closeOutTestPR)
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
			n, closeOutTestRepo, n, closeOutTestBase, closeOutTestHead, f.headOID))
	}
	body := "[" + strings.Join(entries, ",") + "]"
	if err := os.WriteFile(filepath.Join(f.ghDir, "prlist.json"), []byte(body), 0o600); err != nil {
		f.t.Fatalf("write prlist: %v", err)
	}
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
	f.plane.serveTool("claim_task", `{"task":{"id":"`+closeOutTestTask+`"},"run":{"id":"run-1"}}`)
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
	}.withDefaults()
	d := &Supervisor{o: o, ctx: context.Background()}
	cfg := &config.Config{Projects: []config.Project{{
		Slug:              closeOutTestSlug,
		PrimaryRepo:       closeOutTestRepo,
		IntegrationBranch: closeOutTestBase,
	}}}
	d.instances = []*Instance{{
		name:    "inst-1",
		entry:   &RosterEntry{Name: "inst-1", DesiredState: RosterDesiredRunning, Placement: RosterPlacementLocal},
		cfg:     cfg,
		desired: RosterDesiredRunning,
	}}
	return d
}

// run drives the pass once with the log captured.
func (f *closeOutFixture) run(d *Supervisor) string {
	f.t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	d.reconcileCloseOuts()
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

	var closed map[string]any
	for _, c := range f.callsFor("update_task") {
		if c.args["status"] == "done" {
			closed = c.args
		}
	}
	if closed == nil {
		t.Fatalf("no update_task(status: done); the task was not closed. calls: %v", f.plane.planeCalls())
	}
	if got := closed["runId"]; got != "run-1" {
		t.Errorf("close runId = %v, want the claim's run (run-1)", got)
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
		{"conflicting", "CONFLICTING", "DIRTY", passing, "CONFLICTING"},
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
			if got := f.callsFor("update_task"); len(got) != 0 {
				t.Errorf("update_task was called for a PR that is not ready: %v", got)
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
	second := f.run(d)

	for _, want := range []string{"CONFLICTING", "a.txt", "b.txt", closeOutTestRef} {
		if !strings.Contains(first, want) {
			t.Errorf("the conflict line does not name %q:\n%s", want, first)
		}
	}
	if strings.Contains(second, "CONFLICTING") {
		t.Errorf("an unchanged head/base pair was reported twice:\n%s", second)
	}
	if got := f.callsFor("claim_task"); len(got) != 0 {
		t.Errorf("a conflicting PR was claimed: %v", got)
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
		if !strings.Contains(lines, "not the integration branch") {
			t.Errorf("the stand-down does not name the base mismatch:\n%s", lines)
		}
	})

	t.Run("the project integrates into main", func(t *testing.T) {
		f := newCloseOutFixture(t, false)
		f.servePlane(t, "clanker")
		f.writePR("main", "MERGEABLE", "CLEAN", `[{"__typename":"CheckRun","name":"ci","status":"COMPLETED","conclusion":"SUCCESS"}]`)
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

	if got := f.callsFor("update_task"); len(got) == 0 {
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

	if got := f.callsFor("update_task"); len(got) != 0 {
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
	for _, c := range f.callsFor("update_task") {
		if c.args["release"] == true {
			releases++
			if c.args["runId"] != "run-1" {
				t.Errorf("release runId = %v, want run-1", c.args["runId"])
			}
			// A failed MERGE is what the plane's repair ladder must bound: the
			// checks are still green, so attesting green here would leave a
			// green-but-unmergeable PR re-claimed and re-released forever.
			if c.args["checkGreen"] != false {
				t.Errorf("a merge failure released with checkGreen=%v, want false (the ladder must count it): %v", c.args["checkGreen"], c.args)
			}
		}
		if c.args["status"] == "done" {
			t.Errorf("the task was closed after a failed merge: %v", c.args)
		}
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

	var closed map[string]any
	for _, c := range f.callsFor("update_task") {
		if c.args["release"] == true {
			t.Errorf("a merge that LANDED was released: the PR is no longer OPEN, so the task would be stranded on approved forever: %v", c.args)
		}
		if c.args["status"] == "done" {
			closed = c.args
		}
	}
	if closed == nil {
		t.Fatalf("a merge that landed but could not be verified was not closed. calls: %v", f.plane.planeCalls())
	}
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
		if tool == "update_task" && args["status"] == "done" {
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
	n := 0
	for _, c := range f.callsFor("update_task") {
		if c.args["release"] == true {
			n++
		}
	}
	return n
}
