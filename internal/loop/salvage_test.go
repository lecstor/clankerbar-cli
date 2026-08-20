package loop

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/salvage"
)

// Rescuing the work a killed session left uncommitted, and recording where it
// went (CLA-314). The salvage itself is tested against real git in
// internal/salvage; these are about the driver's decisions - when it runs, what
// it records, and what it must never do to the claim.

// --- doubles ---------------------------------------------------------------

type salvageCall struct{ taskID, label string }

type fakeSalvager struct {
	calls []salvageCall
	out   salvage.Outcome
}

func (f *fakeSalvager) Salvage(_ context.Context, taskID, label string) salvage.Outcome {
	f.calls = append(f.calls, salvageCall{taskID, label})
	return f.out
}

type recordCall struct{ taskID, runID, branch string }

// recordingReleaser is a Releaser that can also record a branch - the shape the
// real mcpReleaser has.
type recordingReleaser struct {
	fakeReleaser
	records []recordCall
	err     error
}

func (r *recordingReleaser) RecordBranch(_ context.Context, taskID, runID, branch string) error {
	r.records = append(r.records, recordCall{taskID, runID, branch})
	return r.err
}

func savedOutcome() salvage.Outcome {
	return salvage.Outcome{
		Status: salvage.Saved, Branch: "clanker/t-1-x", Commit: "abc1234",
		Worktree: "/w", Detail: "committed and pushed",
	}
}

// heldClaim is a claim on a task whose id really does spell a branch prefix, so a
// test that swaps in the real salvager is not testing a degenerate id.
func heldClaim() harness.Claim {
	return harness.Claim{TaskID: "cc561415-09ca-4646-bf1b-f9388dc08295", Ref: "CLA-314", RunID: "r-1"}
}

// drainWithSalvager runs one drain with a substituted salvager and returns the
// driver, so callers can assert on what it did.
func drainWithSalvager(t *testing.T, h harness.Adapter, rel *recordingReleaser, s workSalvager) {
	t.Helper()
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.WorkDir = t.TempDir()

	d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: rel}})
	openTestStateDir(t, d)
	d.newSalvager = func(string) workSalvager { return s }
	drainOnce(t, d)
}

// --- when it runs -----------------------------------------------------------

// The decision this pins: salvage runs on EVERY abrupt ending, not only on a
// detected usage limit. A crash and a limit are different to the supervisor and
// identical to the worktree, and the limit is only detected by parsing a stream
// that may not have arrived whole.
func TestSalvage_RunsOnEveryEndingThatLeavesAClaimHeld(t *testing.T) {
	captureLogs(t)
	untrusted := okResult(0, 0)
	untrusted.Untrusted = "a line overran the reader"

	for _, tc := range []struct {
		name string
		res  harness.Result
	}{
		{"clean exit with the work unfinished", okResult(0, 0)},
		{"usage limit", limitResult()},
		{"transient failure", transientResult()},
		{"non-retryable failure", nonRetryableResult()},
		{"a stream that could not be read whole", untrusted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &fakeSalvager{out: savedOutcome()}
			h := &fakeAdapter{steps: []invokeStep{{res: held(tc.res, heldClaim())}}}

			drainWithSalvager(t, h, &recordingReleaser{}, s)

			if len(s.calls) != 1 {
				t.Fatalf("salvaged %d times, want exactly 1: %+v", len(s.calls), s.calls)
			}
			if s.calls[0] != (salvageCall{heldClaim().TaskID, "CLA-314"}) {
				t.Errorf("salvaged %+v, want the held task and its ref", s.calls[0])
			}
		})
	}
}

// A session we SAW finish is not a rescue case. Its worktree may hold anything -
// a scratch file, a stale build - and none of it is unfinished work.
func TestSalvage_SkipsASettledClaim(t *testing.T) {
	captureLogs(t)
	settled := heldClaim()
	settled.Settled = true
	s := &fakeSalvager{out: savedOutcome()}
	h := &fakeAdapter{steps: []invokeStep{{res: held(okResult(0, 0), settled)}}}

	drainWithSalvager(t, h, &recordingReleaser{}, s)

	if len(s.calls) != 0 {
		t.Errorf("salvaged a settled claim: %+v", s.calls)
	}
}

// Nothing observed, nothing to rescue: a session that never claimed leaves no
// task to record a branch against.
func TestSalvage_SkipsASessionThatClaimedNothing(t *testing.T) {
	captureLogs(t)
	s := &fakeSalvager{out: savedOutcome()}
	h := &fakeAdapter{steps: []invokeStep{{res: okResult(0, 0)}}}

	drainWithSalvager(t, h, &recordingReleaser{}, s)

	if len(s.calls) != 0 {
		t.Errorf("salvaged with no claim in hand: %+v", s.calls)
	}
}

// --- what it does with the claim -------------------------------------------

// The composition with CLA-242: a successful salvage puts a branch on the task,
// and a task with a branch must NOT be handed back - releasing it to `ready`
// would discard the takeover flag that tells the next clanker there is work to
// fetch.
func TestSalvage_RecordsTheBranchAndDoesNotHandTheClaimBack(t *testing.T) {
	logs := captureLogs(t)
	rel := &recordingReleaser{}
	h := &fakeAdapter{steps: []invokeStep{{res: held(limitResult(), heldClaim())}}}

	drainWithSalvager(t, h, rel, &fakeSalvager{out: savedOutcome()})

	want := recordCall{heldClaim().TaskID, "r-1", "clanker/t-1-x"}
	if len(rel.records) != 1 || rel.records[0] != want {
		t.Fatalf("recorded %+v, want exactly one %+v", rel.records, want)
	}
	if len(rel.calls) != 0 {
		t.Errorf("handed the claim back after salvaging it: %+v — the takeover hand-off is lost", rel.calls)
	}
	if out := logs.String(); !strings.Contains(out, "clanker/t-1-x") || !strings.Contains(out, "hand-off branch") {
		t.Errorf("the operator's log does not name the hand-off branch:\n%s", out)
	}
}

// CLA-262 forbids handing a claim back on a stream that could not be read whole,
// because a settle we never saw may be in the missing bytes. Recording a branch
// is a different kind of act - it carries no status, so it cannot move a task -
// and the two must coexist: the branch is recorded, the claim is still not
// released.
func TestSalvage_OnAnUntrustedStreamRecordsTheBranchAndStillNeverReleases(t *testing.T) {
	captureLogs(t)
	res := held(okResult(0, 0), heldClaim())
	res.Untrusted = "a line overran the reader"
	rel := &recordingReleaser{}

	drainWithSalvager(t, &fakeAdapter{steps: []invokeStep{{res: res}}}, rel, &fakeSalvager{out: savedOutcome()})

	if len(rel.records) != 1 {
		t.Fatalf("recorded %d branches on the untrusted path, want 1: %+v", len(rel.records), rel.records)
	}
	if len(rel.calls) != 0 {
		t.Errorf("released a claim from an unreadable stream: %+v — CLA-262 says let the lease expire", rel.calls)
	}
}

// Nothing pushed, nothing recorded. An empty hand-off is worse than none: it
// sends the next clanker to fetch a branch that is not there.
func TestSalvage_RecordsNothingWhenNothingWasPushed(t *testing.T) {
	captureLogs(t)
	for _, tc := range []struct {
		name string
		out  salvage.Outcome
	}{
		{"a clean worktree", salvage.Outcome{Status: salvage.Nothing, Detail: "clean"}},
		{"a tree mid-merge", salvage.Outcome{Status: salvage.Refused, Worktree: "/w", Detail: "mid-merge"}},
		{"a push that failed", salvage.Outcome{Status: salvage.Failed, Commit: "abc1234", Detail: "push refused"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rel := &recordingReleaser{}

			drainWithSalvager(t, &fakeAdapter{steps: []invokeStep{{res: held(limitResult(), heldClaim())}}}, rel, &fakeSalvager{out: tc.out})

			if len(rel.records) != 0 {
				t.Errorf("recorded a branch with nothing on the remote: %+v", rel.records)
			}
			// With no branch on the task, the pre-existing handback is still the
			// right move: it costs the plane one reclaim less than an expiring lease.
			if len(rel.calls) != 1 {
				t.Errorf("handed the claim back %d times, want 1: %+v", len(rel.calls), rel.calls)
			}
		})
	}
}

// A record the plane refused leaves the task with no branch on it - so the claim
// is releasable exactly as it was before, and the log has to name the branch that
// is sitting on the remote unrecorded.
func TestSalvage_ARecordThePlaneRefusesLeavesTheClaimReleasable(t *testing.T) {
	logs := captureLogs(t)
	rel := &recordingReleaser{err: errors.New("run_superseded")}

	drainWithSalvager(t, &fakeAdapter{steps: []invokeStep{{res: held(limitResult(), heldClaim())}}}, rel, &fakeSalvager{out: savedOutcome()})

	if len(rel.calls) != 1 {
		t.Errorf("handed the claim back %d times, want 1 — nothing was recorded, so the old path applies: %+v", len(rel.calls), rel.calls)
	}
	if out := logs.String(); !strings.Contains(out, "clanker/t-1-x") || !strings.Contains(out, "IS pushed") {
		t.Errorf("the log must say the work is pushed and where, got:\n%s", out)
	}
}

// A target with no plane writes wired still salvages: the work reaches the
// remote, and the log is the hand-off.
func TestSalvage_WithoutAPlaneRecorderTheWorkIsStillPushed(t *testing.T) {
	logs := captureLogs(t)
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.WorkDir = t.TempDir()
	s := &fakeSalvager{out: savedOutcome()}

	// A plain fakeReleaser is not a plane.Recorder — the shape a not-wired or
	// older plane client has.
	d := NewMulti(cfg, &fakeAdapter{steps: []invokeStep{{res: held(limitResult(), heldClaim())}}},
		[]Target{{Poller: busyPoller(), Releaser: &fakeReleaser{}}})
	openTestStateDir(t, d)
	d.newSalvager = func(string) workSalvager { return s }
	drainOnce(t, d)

	if len(s.calls) != 1 {
		t.Fatalf("salvaged %d times, want 1", len(s.calls))
	}
	if out := logs.String(); !strings.Contains(out, "clanker/t-1-x") {
		t.Errorf("the log must still name the branch, got:\n%s", out)
	}
}

// --- end to end, against a real repository ---------------------------------

// The whole chain with nothing stubbed but the harness: a session claims a task,
// works in its worktree, and is killed by a usage limit with the work
// uncommitted. This is the 112.7M-token shape (CLA-314) - the run that redid a
// task because its work was sitting on disk, invisible to everything.
func TestRun_RealGitRepo_ALimitKillLeavesThePushedBranchOnTheTask(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	logs := captureLogs(t)

	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	remote := filepath.Join(root, "remote.git")
	repo := filepath.Join(root, "repo")
	claim := heldClaim()
	branch := "clanker/cc561415-a-session-killed-mid-task"
	wt := filepath.Join(root, "repo-wt", "cc561415")

	gitRun(t, root, "init", "--bare", "-b", "main", remote)
	gitRun(t, root, "init", "-b", "main", repo)
	gitRun(t, repo, "config", "user.email", "driver@example.test")
	gitRun(t, repo, "config", "user.name", "Driver Test")
	gitRun(t, repo, "config", "commit.gpgsign", "false")
	gitRun(t, repo, "remote", "add", "origin", remote)
	writeTestFile(t, filepath.Join(repo, "a.txt"), "a")
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-m", "base")
	gitRun(t, repo, "push", "origin", "main")
	// The session's worktree, with hours of work in it and not one commit.
	gitRun(t, repo, "worktree", "add", "-b", branch, wt)
	writeTestFile(t, filepath.Join(wt, "the-work.go"), "package work")

	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.WorkDir = root
	cfg.MaxIterations = 1
	rel := &recordingReleaser{}
	h := &fakeAdapter{steps: []invokeStep{{res: held(limitResult(), claim)}}}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: rel}}).Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	// 1. The work is committed.
	head := gitOut(t, wt, "rev-parse", "HEAD")
	if subject := gitOut(t, wt, "log", "-1", "--pretty=format:%s"); !strings.Contains(subject, "WIP salvage") {
		t.Fatalf("the worktree's tip is not a salvage commit: %q", subject)
	}
	// 2. It is on the remote, so ANOTHER machine can take it over. Read from the
	// bare repository itself rather than a tracking ref: what matters is what the
	// host has, not what this clone believes about it.
	if bare := gitOut(t, remote, "rev-parse", "refs/heads/"+branch); bare != head {
		t.Errorf("origin/%s is at %q, want the salvage commit %q", branch, bare, head)
	}
	// 3. The plane knows where it is.
	want := recordCall{claim.TaskID, claim.RunID, branch}
	if len(rel.records) != 1 || rel.records[0] != want {
		t.Fatalf("recorded %+v, want exactly one %+v", rel.records, want)
	}
	// 4. And the claim was not handed back, so the task stays a takeover.
	if len(rel.calls) != 0 {
		t.Errorf("handed the claim back: %+v", rel.calls)
	}
	if out := logs.String(); !strings.Contains(out, "salvaged CLA-314") {
		t.Errorf("the operator's log does not report the salvage:\n%s", out)
	}
}

// gitOut runs git for an assertion and returns its trimmed output.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// The seam's empty-worktree line has to say WHICH clean it is. A clean tree
// reads identically whether the phase committed everything (clean because the
// work landed) or produced nothing at all (clean because nothing was ever
// written) — and the only other discriminator, a `verified <ref>: origin/...`
// line, is an absence, the weakest possible signal (CLA-386).
func TestSalvage_TheEmptyWorktreeLineNamesWhichCase(t *testing.T) {
	clean := salvage.Outcome{Status: salvage.Nothing, Worktree: "/w", Detail: "/w is clean"}

	// A session that committed everything leaves the tree clean WITH a branch on
	// the task — the ordinary "nothing to salvage" case.
	committed := held(okResult(0, 0), harness.Claim{TaskID: heldClaim().TaskID, RunID: "r-1", HasWIP: true})
	committed.Raw[harness.FinishReasonKey] = "stop"

	// A dead phase leaves the tree clean with NO branch — the produced-nothing
	// case, which is a failure the reader has to be able to spot.
	producedNothing := held(deadResult(), heldClaim())

	for _, tc := range []struct {
		name    string
		res     harness.Result
		want    string
		notWant string
	}{
		{"committed everything", committed, "nothing to salvage", "produced nothing"},
		{"produced nothing", producedNothing, "produced nothing", "nothing to salvage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			h := &fakeAdapter{steps: []invokeStep{{res: tc.res}}}
			drainWithSalvager(t, h, &recordingReleaser{}, &fakeSalvager{out: clean})

			out := logs.String()
			if !strings.Contains(out, tc.want) {
				t.Errorf("the log does not say the phase %q:\n%s", tc.want, out)
			}
			if tc.notWant != "" && strings.Contains(out, tc.notWant) {
				t.Errorf("the log says %q in the %s case — the two clean trees must read differently:\n%s", tc.notWant, tc.name, out)
			}
		})
	}
}
