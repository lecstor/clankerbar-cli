package loop

import (
	"bytes"
	"context"
	"errors"
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
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// Checking, against local git, what a session told the plane it delivered
// (CLA-253). The plane stores a recorded branch and a declared merge and takes the
// clanker at its word; the driver is already local and can simply look.

// --- doubles ---------------------------------------------------------------

type fakeVerifier struct {
	claims []delivery.Claim
	report delivery.Report // returned for every claim
	byRepo string
}

func (f *fakeVerifier) Verify(_ context.Context, c delivery.Claim) delivery.Report {
	f.claims = append(f.claims, c)
	rep := f.report
	rep.Claim = c
	return rep
}

type attestCall struct {
	taskID, runID, commit, integration, pr string
	verified                               bool
}

// attestingReleaser is a Releaser that can also attest — the shape the real
// mcpReleaser has.
type attestingReleaser struct {
	fakeReleaser
	attests []attestCall
	err     error
}

func (a *attestingReleaser) AttestMergeVerified(_ context.Context, taskID, runID string, d plane.Delivery, verified bool) error {
	a.attests = append(a.attests, attestCall{taskID, runID, d.Commit, d.IntegrationBranch, d.PR, verified})
	return a.err
}

// captureLogs redirects the standard logger for one test, so assertions can be
// made on what the operator would actually read at 3am.
func captureLogs(t *testing.T) *syncBuf {
	t.Helper()
	buf := &syncBuf{}
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })
	return buf
}

type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// reported is a scripted Result carrying the delivery claims a session made.
func reported(base harness.Result, reps ...harness.Report) harness.Result {
	base.Reports = reps
	return base
}

func branchReport() harness.Report {
	return harness.Report{TaskID: "t-1", Ref: "CLA-253", RunID: "r-1", Branch: "clanker/x"}
}

func mergeReport() harness.Report {
	return harness.Report{
		TaskID: "t-1", Ref: "CLA-253", RunID: "r-1", Status: "done",
		Commit: "abc1234", IntegrationBranch: "main", PR: "#42",
	}
}

// runWithVerifier drives one drain with a substituted delivery checker.
func runWithVerifier(t *testing.T, h harness.Adapter, rel plane.Releaser, v deliveryVerifier) *config.Config {
	t.Helper()
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.WorkDir = t.TempDir()
	cfg.MaxIterations = 1

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: rel}})
	if v != nil {
		d.newVerifier = func(string) deliveryVerifier { return v }
	}
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	return cfg
}

// --- wiring ----------------------------------------------------------------

func TestVerifyDeliveries_ChecksEveryClaimInTheSessionsWorkdir(t *testing.T) {
	captureLogs(t)
	v := &fakeVerifier{}
	h := &fakeAdapter{steps: []invokeStep{{res: reported(okResult(0, 0), branchReport(), mergeReport())}}}

	runWithVerifier(t, h, &fakeReleaser{}, v)

	if len(v.claims) != 2 {
		t.Fatalf("checked %d claims, want 2: %+v", len(v.claims), v.claims)
	}
	if v.claims[0].Branch != "clanker/x" || v.claims[0].Label != "CLA-253" {
		t.Errorf("branch claim = %+v", v.claims[0])
	}
	if v.claims[1].Commit != "abc1234" || v.claims[1].IntegrationBranch != "main" {
		t.Errorf("delivery claim = %+v", v.claims[1])
	}
}

func TestVerifyDeliveries_UsesTheTargetsWorkdir(t *testing.T) {
	captureLogs(t)
	var gotWorkdir string
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.WorkDir = t.TempDir()
	cfg.MaxIterations = 1
	projectDir := t.TempDir()

	h := &fakeAdapter{steps: []invokeStep{{res: reported(okResult(0, 0), branchReport())}}}
	d := NewMulti(cfg, h, []Target{{Name: "acme", Poller: busyPoller(), WorkDir: projectDir}})
	d.newVerifier = func(workdir string) deliveryVerifier {
		gotWorkdir = workdir
		return &fakeVerifier{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	// A multi-project loop checks each project's claim in ITS OWN tree; checking
	// against the wrong repo is how a green result becomes worse than a red one.
	if gotWorkdir != projectDir {
		t.Errorf("verified in %q, want the target's workdir %q", gotWorkdir, projectDir)
	}
}

func TestVerifyDeliveries_FailureIsLoudAndNamesWhatIsUnpushed(t *testing.T) {
	logs := captureLogs(t)
	v := &fakeVerifier{report: delivery.Report{Checks: []delivery.Check{{
		Kind: delivery.BranchPushed, Status: delivery.Fail,
		Detail: `branch "clanker/x" is 3 commits ahead of origin/clanker/x`,
	}}}}
	h := &fakeAdapter{steps: []invokeStep{{res: reported(okResult(0, 0), branchReport())}}}

	runWithVerifier(t, h, &fakeReleaser{}, v)

	out := logs.String()
	if !strings.Contains(out, "DELIVERY UNVERIFIED") {
		t.Errorf("a failed check must be loud, got:\n%s", out)
	}
	if !strings.Contains(out, "CLA-253") || !strings.Contains(out, "3 commits ahead") {
		t.Errorf("the log must name the task and what is unpushed, got:\n%s", out)
	}
}

// Warn, do not refuse: the driver does not override the session's own report.
// Loud logging is the floor, and the deliberate first cut (decision on CLA-253).
func TestVerifyDeliveries_FailureDoesNotStopTheRunOrRevertTheTask(t *testing.T) {
	captureLogs(t)
	v := &fakeVerifier{report: delivery.Report{Checks: []delivery.Check{{
		Kind: delivery.BranchPushed, Status: delivery.Fail, Detail: "unpushed",
	}}}}
	rel := &fakeReleaser{}
	// Settled: the session already handed the task to review, so the handback must
	// not fire — and a failed check must not resurrect it either.
	settled := harness.Claim{TaskID: "t-1", RunID: "r-1", Settled: true}
	h := &fakeAdapter{steps: []invokeStep{{res: held(reported(okResult(0, 0), branchReport()), settled)}}}

	runWithVerifier(t, h, rel, v)

	if len(rel.calls) != 0 {
		t.Fatalf("a failed delivery check must not move the task: %+v", rel.calls)
	}
}

// Fail open: an unverifiable claim is reported as unverifiable and the run
// carries on. Not knowing is not the same as knowing it is fine — and it is not
// grounds for blocking a legitimate closure either.
func TestVerifyDeliveries_CannotCheckIsNotAFailure(t *testing.T) {
	logs := captureLogs(t)
	v := &fakeVerifier{report: delivery.Report{Checks: []delivery.Check{{
		Kind: delivery.BranchPushed, Status: delivery.Unknown, Detail: "not a git repository",
	}}}}
	rel := &attestingReleaser{}
	h := &fakeAdapter{steps: []invokeStep{{res: reported(okResult(0, 0), mergeReport())}}}

	runWithVerifier(t, h, rel, v)

	out := logs.String()
	if !strings.Contains(out, "could not verify") {
		t.Errorf("an unrunnable check must say so, got:\n%s", out)
	}
	if strings.Contains(out, "DELIVERY UNVERIFIED") {
		t.Errorf("could-not-check must not be reported as a failure, got:\n%s", out)
	}
	if len(rel.attests) != 0 {
		t.Errorf("nothing was checked, so nothing may be attested: %+v", rel.attests)
	}
}

func TestVerifyDeliveries_NothingClaimedIsNotChecked(t *testing.T) {
	captureLogs(t)
	v := &fakeVerifier{}
	h := &fakeAdapter{steps: []invokeStep{{res: okResult(0, 0)}}}

	runWithVerifier(t, h, &fakeReleaser{}, v)

	if len(v.claims) != 0 {
		t.Errorf("a session that claimed no delivery must not be checked: %+v", v.claims)
	}
}

// --- the attestation -------------------------------------------------------

func TestAttestMerge_WritesTheDriversVerdictBothWays(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status delivery.Status
		want   bool
	}{
		{"a delivery that really landed", delivery.Pass, true},
		{"a delivery declared merged that is not", delivery.Fail, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captureLogs(t)
			v := &fakeVerifier{report: delivery.Report{Checks: []delivery.Check{{
				Kind: delivery.CommitMerged, Status: tc.status, Detail: "detail",
			}}}}
			rel := &attestingReleaser{}
			h := &fakeAdapter{steps: []invokeStep{{res: reported(okResult(0, 0), mergeReport())}}}

			runWithVerifier(t, h, rel, v)

			if len(rel.attests) != 1 {
				t.Fatalf("expected exactly one attestation, got %+v", rel.attests)
			}
			got := rel.attests[0]
			if got.verified != tc.want {
				t.Errorf("mergeVerified = %v, want %v", got.verified, tc.want)
			}
			// The attestation has to name what it is about, or a later reader cannot
			// tell what was checked.
			// The PR ref rides along even though the check did not use it: the plane
			// takes `delivery` as an object, and sending a partial one could drop
			// whatever the session put in the fields the driver did not name.
			if got.taskID != "t-1" || got.runID != "r-1" || got.commit != "abc1234" ||
				got.integration != "main" || got.pr != "#42" {
				t.Errorf("attestation = %+v, want it to name the declared delivery in full", got)
			}
		})
	}
}

func TestAttestMerge_BranchOnlyClaimAttestsNothing(t *testing.T) {
	captureLogs(t)
	v := &fakeVerifier{report: delivery.Report{Checks: []delivery.Check{{
		Kind: delivery.BranchPushed, Status: delivery.Pass, Detail: "pushed",
	}}}}
	rel := &attestingReleaser{}
	h := &fakeAdapter{steps: []invokeStep{{res: reported(okResult(0, 0), branchReport())}}}

	runWithVerifier(t, h, rel, v)

	if len(rel.attests) != 0 {
		t.Errorf("mergeVerified is about the MERGE claim only: %+v", rel.attests)
	}
}

// Best-effort throughout: the loud log already carries the finding, so a plane
// that will not take the attestation costs the record, not the run.
func TestAttestMerge_PlaneFailureIsNotFatal(t *testing.T) {
	logs := captureLogs(t)
	v := &fakeVerifier{report: delivery.Report{Checks: []delivery.Check{{
		Kind: delivery.CommitMerged, Status: delivery.Pass,
	}}}}
	rel := &attestingReleaser{err: errors.New("plane unreachable")}
	h := &fakeAdapter{steps: []invokeStep{{res: reported(okResult(0, 0), mergeReport())}}}

	runWithVerifier(t, h, rel, v)

	if !strings.Contains(logs.String(), "could not record the merge check") {
		t.Errorf("a failed attestation must be said out loud, got:\n%s", logs.String())
	}
}

// A Releaser that cannot attest (a not-wired plane) degrades to warn-only.
func TestAttestMerge_NonAttestingReleaserIsNotFatal(t *testing.T) {
	captureLogs(t)
	v := &fakeVerifier{report: delivery.Report{Checks: []delivery.Check{{
		Kind: delivery.CommitMerged, Status: delivery.Pass,
	}}}}
	h := &fakeAdapter{steps: []invokeStep{{res: reported(okResult(0, 0), mergeReport())}}}

	runWithVerifier(t, h, &fakeReleaser{}, v) // Release only, no AttestMergeVerified
}

// An `in_review` hand-off routinely carries the delivery it is proposing, and at
// that moment nothing has merged — nobody has reviewed it yet. Checking it anyway
// would fire the loudest line in the feature on the happy path and stamp
// mergeVerified=false on a task that is fine.
func TestVerifyDeliveries_InReviewIsNotAMergeClaim(t *testing.T) {
	captureLogs(t)
	v := &fakeVerifier{}
	rel := &attestingReleaser{}
	rep := mergeReport()
	rep.Status = "in_review"
	rep.Branch = "clanker/x"
	h := &fakeAdapter{steps: []invokeStep{{res: reported(okResult(0, 0), rep)}}}

	runWithVerifier(t, h, rel, v)

	if len(v.claims) != 1 {
		t.Fatalf("checked %d claims, want 1: %+v", len(v.claims), v.claims)
	}
	if v.claims[0].Branch != "clanker/x" {
		t.Errorf("the branch is still worth checking at in_review: %+v", v.claims[0])
	}
	if v.claims[0].Commit != "" || v.claims[0].IntegrationBranch != "" {
		t.Errorf("an in_review hand-off must not be checked as a merge: %+v", v.claims[0])
	}
	if len(rel.attests) != 0 {
		t.Errorf("nothing merged, nothing to attest: %+v", rel.attests)
	}
}

// --- end to end, against a real repository ---------------------------------

// The whole chain with nothing stubbed but the harness: a session reports a
// branch, and the driver checks it against a REAL git repository and remote. This
// is the CLA-134 shape — the branch exists on the remote and the work does not.
func TestRun_RealGitRepo_ReportsUnpushedWork(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	logs := captureLogs(t)

	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	remote := filepath.Join(root, "remote.git")
	work := filepath.Join(root, "work")
	gitRun(t, root, "init", "--bare", "-b", "main", remote)
	gitRun(t, root, "init", "-b", "main", work)
	gitRun(t, work, "config", "user.email", "driver@example.test")
	gitRun(t, work, "config", "user.name", "Driver Test")
	gitRun(t, work, "config", "commit.gpgsign", "false")
	gitRun(t, work, "remote", "add", "origin", remote)
	writeTestFile(t, filepath.Join(work, "a.txt"), "a")
	gitRun(t, work, "add", ".")
	gitRun(t, work, "commit", "-m", "base")
	gitRun(t, work, "push", "origin", "main")

	gitRun(t, work, "checkout", "-b", "clanker/x")
	writeTestFile(t, filepath.Join(work, "b.txt"), "b")
	gitRun(t, work, "add", ".")
	gitRun(t, work, "commit", "-m", "pushed work")
	gitRun(t, work, "push", "origin", "clanker/x")
	// ... and then the part that never leaves the laptop.
	writeTestFile(t, filepath.Join(work, "c.txt"), "c")
	gitRun(t, work, "add", ".")
	gitRun(t, work, "commit", "-m", "the 900 lines nobody pushed")

	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.WorkDir = work
	cfg.MaxIterations = 1
	h := &fakeAdapter{steps: []invokeStep{{res: reported(okResult(0, 0), branchReport())}}}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := NewMulti(cfg, h, []Target{{Poller: busyPoller()}}).Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	out := logs.String()
	if !strings.Contains(out, "DELIVERY UNVERIFIED") || !strings.Contains(out, "ahead of origin/clanker/x by 1 commit") {
		t.Errorf("the driver should have caught the unpushed commit, got:\n%s", out)
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// Insulated from the developer's own git config: a system-wide
	// `commit.gpgsign = true` would otherwise fail this test on their machine and
	// nowhere else.
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME="+t.TempDir(),
		"GIT_AUTHOR_NAME=Driver Test", "GIT_AUTHOR_EMAIL=driver@example.test",
		"GIT_COMMITTER_NAME=Driver Test", "GIT_COMMITTER_EMAIL=driver@example.test",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
