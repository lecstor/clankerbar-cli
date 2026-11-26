package loop

// pr_handover_test.go — CLA-1035's runner-owned PR: the opener, the linkage it
// records, and the worktree reclaim that only runs after the hand-over. The
// runner briefs' own assertions live in internal/config/prompts_runner_test.go.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/delivery"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// fakePROpener stands in for delivery.EnsurePR: it records the spec the runner
// assembled and answers with a fixed number.
type fakePROpener struct {
	number int
	err    error
	specs  []delivery.PRSpec
}

func (f *fakePROpener) EnsurePR(_ context.Context, spec delivery.PRSpec) (int, error) {
	f.specs = append(f.specs, spec)
	if f.err != nil {
		return 0, f.err
	}
	if f.number == 0 {
		return 42, nil
	}
	return f.number, nil
}

func TestPRTitleCarriesTheRefAndFallsBack(t *testing.T) {
	r := &phaseRun{opts: phaseRunOpts{runner: &runnerBriefing{task: plane.TaskBrief{Ref: "CLA-1035", Title: "Runner-owned PR creation"}}}}
	if got := r.prTitle(plane.TaskState{}, "clanker/x"); got != "CLA-1035: Runner-owned PR creation" {
		t.Errorf("prTitle = %q, want the ref and title", got)
	}
	// The plane's record is authoritative for the ref when the briefing has none.
	r.opts.runner = &runnerBriefing{task: plane.TaskBrief{Title: "Runner-owned PR creation"}}
	if got := r.prTitle(plane.TaskState{Ref: "CLA-7"}, "clanker/x"); got != "CLA-7: Runner-owned PR creation" {
		t.Errorf("prTitle = %q, want the task record's ref", got)
	}
	r.opts.runner.task.Title = ""
	if got := r.prTitle(plane.TaskState{Ref: "CLA-7"}, "clanker/x"); got != "CLA-7" {
		t.Errorf("prTitle = %q, want the ref alone when there is no title", got)
	}
	r.opts.runner = nil
	if got := r.prTitle(plane.TaskState{}, "clanker/x"); got != "clanker/x" {
		t.Errorf("prTitle = %q, want the branch when there is nothing else", got)
	}
}

func TestPRBodyIsDeterministicAndCarriesTheRecord(t *testing.T) {
	r := &phaseRun{}
	rep := config.DoneReport{
		Summary: "added the runner-owned PR step",
		Tests:   "go test -race ./internal/loop",
		Verdict: "pass",
	}
	got := r.prBody(plane.TaskState{Ref: "CLA-1035"}, rep, "clanker/x")
	want := "Task: CLA-1035\n" +
		"Branch: `clanker/x`\n" +
		"\n## Summary\n\nadded the runner-owned PR step\n" +
		"\n## Tests\n\ngo test -race ./internal/loop\n" +
		"\n## Review\n\npass\n"
	if got != want {
		t.Errorf("prBody =\n%q\nwant\n%q", got, want)
	}
	// The same inputs render the same body: a retry must not churn the PR.
	if again := r.prBody(plane.TaskState{Ref: "CLA-1035"}, rep, "clanker/x"); again != got {
		t.Error("prBody is not deterministic")
	}
}

// A fix report that omitted its own tests still gives the PR the gates the
// stored outcome records: the NEWEST round's, without the renderer's
// attribution line.
func TestPRBodyFallsBackToTheStoredOutcomeTests(t *testing.T) {
	r := &phaseRun{}
	attr := config.Attribution{Harness: "opencode", Model: "deepseek"}
	// An implement round, then the fix round: the fix is the newest Tests block.
	outcome := config.AppendOutcome(
		"## Delivery\n\nimplemented it\n\n"+config.RenderTestsSection("go build ./..."),
		config.RenderFixesSection(attr, config.DoneReport{Summary: "fixed it", Tests: "go vet ./..."}))
	got := r.prBody(plane.TaskState{Outcome: outcome}, config.DoneReport{Summary: "fixed it"}, "clanker/x")
	if !strings.Contains(got, "## Tests\n\ngo vet ./...\n") {
		t.Errorf("body does not carry the newest stored Tests section:\n%s", got)
	}
	if strings.Contains(got, "go build ./...") {
		t.Errorf("body carries an older round's tests:\n%s", got)
	}
	if strings.Contains(got, "fixed by:") {
		t.Errorf("body leaks the renderer's attribution into Tests:\n%s", got)
	}
}

// The fix report schema has no verdict field, so the body falls back to the
// newest released record's verdict there.
func TestPRBodyFallsBackToTheReleasedRecordVerdict(t *testing.T) {
	r := &phaseRun{}
	st := plane.TaskState{Ref: "CLA-1035", Reviews: []plane.ReviewRecordState{
		{Round: 1, State: "open", Verdict: "findings"},
		{Round: 2, State: "sealed", Verdict: "pass"}, // sealed: not released yet
	}}
	got := r.prBody(st, config.DoneReport{Summary: "fixed it"}, "clanker/x")
	if !strings.Contains(got, "## Review\n\nfindings\n") {
		t.Errorf("body = %q, want the newest released verdict", got)
	}
}

func TestOutcomeTestsReadsTheHeadingForms(t *testing.T) {
	cases := map[string]string{
		"**Tests**\ngo test ./...":                      "go test ./...",
		"## Tests\n\ngo vet ./...":                      "go vet ./...",
		"### Fixes\n**Tests**\nran them":                "ran them",
		"no tests here":                                 "",
		"**Tests**\nold\n\n### Fixes\n\n**Tests**\nnew": "new",
		// The renderer's attribution line follows every Tests block and ends it.
		"**Tests**\ngates\n\nfixed by: harness opencode, model deepseek": "gates",
	}
	for outcome, want := range cases {
		if got := outcomeTests(outcome); got != want {
			t.Errorf("outcomeTests(%q) = %q, want %q", outcome, got, want)
		}
	}
}

func TestHandOverVerifiedRound_OpensThePRThenLinksItThenSubmits(t *testing.T) {
	rel := &reportReleaser{st: plane.TaskState{Status: "in_progress", Outcome: "reviewed"}}
	r := doneRun(t, reportVerify, rel, resumed())
	r.opts.runner = &runnerBriefing{task: plane.TaskBrief{Ref: "CLA-1", Title: "the task", Repo: "acme/widgets", Branch: "task/t-1"}}
	opener := &fakePROpener{number: 7}
	r.d.newPROpener = func(string) prEnsurer { return opener }
	r.end.doneReport = &config.DoneReport{Tests: "go test ./...", Verdict: "pass"}
	at := doneAttempt("")
	r.end.claim = &at.res

	r.handOverVerifiedRound(context.Background(), at, plane.TaskState{Status: "in_progress", Outcome: "reviewed"})

	if want := []string{callDeliveryPR, callSubmit}; !equalCalls(rel.calls, want) {
		t.Fatalf("writes = %v, want %v — the PR is linked before the hand-over", rel.calls, want)
	}
	if len(opener.specs) != 1 {
		t.Fatalf("opened %d PRs, want one", len(opener.specs))
	}
	spec := opener.specs[0]
	if spec.Head != "task/t-1" || spec.Base != "staging" || spec.Slug != "acme/widgets" {
		t.Errorf("spec = %+v, want the task's branch on the PR base in the task's repo", spec)
	}
	if spec.Title != "CLA-1: the task" {
		t.Errorf("title = %q, want the ref and task title", spec.Title)
	}
	if !strings.Contains(spec.Body, "Task: CLA-1") || !strings.Contains(spec.Body, "go test ./...") {
		t.Errorf("body = %q, want the ref and the verifier's tests", spec.Body)
	}
	if len(rel.prs) != 1 || rel.prs[0] != "#7" {
		t.Errorf("recorded prs = %v, want #7", rel.prs)
	}
	if r.end.verifyStatus != "in_review" || !at.res.Claim.Settled {
		t.Errorf("verifyStatus/settled = %q/%v, want in_review/true", r.end.verifyStatus, at.res.Claim.Settled)
	}
}

// A PR that cannot be opened fails the hand-over closed: no delivery.pr, no
// submit, no reclaim, and the claim stays live for the ordinary handback.
func TestHandOverVerifiedRound_PRCreateFailureFailsClosed(t *testing.T) {
	rel := &reportReleaser{st: plane.TaskState{Status: "in_progress"}}
	r := doneRun(t, reportVerify, rel, resumed())
	r.d.newPROpener = func(string) prEnsurer { return &fakePROpener{err: errors.New("gh pr create: HTTP 502")} }
	r.opts.taskDir = t.TempDir()
	r.end.doneReport = &config.DoneReport{Tests: "go test ./..."}
	at := doneAttempt("")
	r.end.claim = &at.res

	r.handOverVerifiedRound(context.Background(), at, plane.TaskState{Status: "in_progress"})

	if len(rel.calls) != 0 {
		t.Fatalf("writes = %v, want none after the PR failed", rel.calls)
	}
	if at.res.Claim.Settled {
		t.Error("the claim was settled without a PR link")
	}
	if r.end.verifyStatus == "in_review" {
		t.Error("the task moved to in_review without a PR link")
	}
	if !strings.Contains(r.end.reportProblem, "pull request") {
		t.Errorf("reportProblem = %q, want the PR failure named", r.end.reportProblem)
	}
}

// A received PR whose link cannot be recorded must not be submitted either: an
// in_review task with delivery.pr null is the state this closes.
func TestHandOverVerifiedRound_PRLinkFailureFailsClosed(t *testing.T) {
	rel := &reportReleaser{
		st:   plane.TaskState{Status: "in_progress"},
		errs: map[string]error{callDeliveryPR: errors.New("the plane is not answering")},
	}
	r := doneRun(t, reportVerify, rel, resumed())
	r.end.doneReport = &config.DoneReport{Tests: "go test ./..."}
	at := doneAttempt("")
	r.end.claim = &at.res

	r.handOverVerifiedRound(context.Background(), at, plane.TaskState{Status: "in_progress"})

	if want := []string{callDeliveryPR}; !equalCalls(rel.calls, want) {
		t.Fatalf("writes = %v, want only the failed %v", rel.calls, want)
	}
	if at.res.Claim.Settled {
		t.Error("the claim was settled although delivery.pr never landed")
	}
}

func TestApplyFixLastReport_PRCreateFailureFailsClosed(t *testing.T) {
	rel := &reportReleaser{st: plane.TaskState{Outcome: "reviewed"}}
	r := doneRun(t, reportFixLast, rel, resumed())
	r.d.newVerifier = func(string, bool) deliveryVerifier { return passVerifier() }
	r.d.newPROpener = func(string) prEnsurer { return &fakePROpener{err: errors.New("gh pr create: HTTP 502")} }
	r.opts.runner = &runnerBriefing{task: plane.TaskBrief{Branch: "task/t-1"}}
	at := doneAttempt(doneReportFor(`{"summary":"fixed it","findingUpdates":[{"findingId":"1.review-1.1","status":"fixed","fixCommit":"abc1234"}]}`))
	r.end.claim = &at.res

	r.applyDoneReport(context.Background(), at)

	// The findings and the branch are still recorded; the hand-over is what fails.
	if want := []string{callFindings, callBranch}; !equalCalls(rel.calls, want) {
		t.Fatalf("writes = %v, want %v with no submit", rel.calls, want)
	}
	if len(rel.submits) != 0 {
		t.Errorf("submissions = %+v, want none", rel.submits)
	}
	if at.res.Claim.Settled {
		t.Error("the claim was settled without a PR link")
	}
	if !strings.Contains(r.end.reportProblem, "pull request") {
		t.Errorf("reportProblem = %q, want the PR failure named", r.end.reportProblem)
	}
}

// A reclaim failure after a successful hand-over is a warning: the task is
// already in review with its PR open and linked.
func TestHandOverVerifiedRound_ReclaimFailureDoesNotFailTheHandOver(t *testing.T) {
	rel := &reportReleaser{st: plane.TaskState{Status: "in_progress"}}
	r := doneRun(t, reportVerify, rel, resumed())
	r.opts.taskDir = filepath.Join(t.TempDir(), "not-a-worktree")
	r.end.doneReport = &config.DoneReport{Tests: "go test ./..."}
	at := doneAttempt("")
	r.end.claim = &at.res

	r.handOverVerifiedRound(context.Background(), at, plane.TaskState{Status: "in_progress"})

	if len(rel.submits) != 1 {
		t.Fatalf("submissions = %+v, want the hand-over to stand", rel.submits)
	}
	if r.end.verifyStatus != "in_review" || !at.res.Claim.Settled {
		t.Error("a reclaim failure undid the hand-over")
	}
	if r.end.reportProblem != "" {
		t.Errorf("reportProblem = %q, want none: a reclaim failure is a warning", r.end.reportProblem)
	}
}

// preparedTaskWorktree is a real prepared task worktree whose branch is pushed
// as a hand-over requires, with the fixture's main checkout to inspect.
func preparedTaskWorktree(t *testing.T) (*taskWorktreeRepo, string) {
	t.Helper()
	f := newTaskWorktreeRepo(t)
	f.commitOn(t, taskWorktreeBranch, "origin/staging", "work.txt", "work", "work")
	req := f.request()
	req.Resume, req.NoMerge = true, true
	prep, err := f.prepare(t, req)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	gitRun(t, prep.Dir, "push", "-u", "origin", taskWorktreeBranch)
	return f, prep.Dir
}

// The reclaim is the LAST step: only a successful submit may remove the tree.
// A failed submit, and a failed PR step, each leave it for the takeover.
func TestHandOverVerifiedRound_ReclaimsOnlyAfterTheSubmit(t *testing.T) {
	handOver := func(t *testing.T, dir string, rel *reportReleaser, opener *fakePROpener) *phaseRun {
		t.Helper()
		r := doneRun(t, reportVerify, rel, nil)
		if opener != nil {
			r.d.newPROpener = func(string) prEnsurer { return opener }
		}
		r.opts.taskDir = dir
		r.opts.runner = &runnerBriefing{task: plane.TaskBrief{Branch: taskWorktreeBranch}}
		r.end.doneReport = &config.DoneReport{Tests: "go test ./..."}
		at := doneAttempt("")
		r.end.claim = &at.res
		r.handOverVerifiedRound(context.Background(), at, plane.TaskState{Status: "in_progress"})
		return r
	}

	t.Run("a failed submit leaves the tree", func(t *testing.T) {
		f, dir := preparedTaskWorktree(t)
		rel := &reportReleaser{
			st:   plane.TaskState{Status: "in_progress"},
			errs: map[string]error{callSubmit: errors.New("the plane is not answering")},
		}
		handOver(t, dir, rel, nil)

		if _, err := os.Stat(dir); err != nil {
			t.Errorf("the tree was reclaimed although the submit failed: %v", err)
		}
		if !taskRefExists(t, f.checkout, "refs/heads/"+taskWorktreeBranch) {
			t.Error("the branch was deleted although the submit failed")
		}
	})

	t.Run("a successful submit reclaims the tree", func(t *testing.T) {
		f, dir := preparedTaskWorktree(t)
		handOver(t, dir, &reportReleaser{st: plane.TaskState{Status: "in_progress"}}, nil)

		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("the tree still exists after a successful hand-over (err=%v)", err)
		}
		if taskRefExists(t, f.checkout, "refs/heads/"+taskWorktreeBranch) {
			t.Error("the local branch survived the successful hand-over")
		}
	})

	t.Run("a failed PR leaves the tree", func(t *testing.T) {
		_, dir := preparedTaskWorktree(t)
		handOver(t, dir, &reportReleaser{st: plane.TaskState{Status: "in_progress"}},
			&fakePROpener{err: errors.New("gh pr create: HTTP 502")})

		if _, err := os.Stat(dir); err != nil {
			t.Errorf("the tree was reclaimed although the PR step failed: %v", err)
		}
	})
}

// The runner reclaims the tree it prepared, from the main checkout: the
// worktree is removed and the local branch deleted, without --force or -D.
func TestReclaimTaskWorktree_RemovesTheTreeAndThePushedBranch(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	// The branch exists on origin, as it does by the time a hand-over runs.
	f.commitOn(t, taskWorktreeBranch, "origin/staging", "work.txt", "work", "work")
	req := f.request()
	req.Resume, req.NoMerge = true, true
	prep, err := f.prepare(t, req)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// The session's own commit, pushed: the state the reclaim is written for.
	writeTestFile(t, filepath.Join(prep.Dir, "more.txt"), "more")
	gitRun(t, prep.Dir, "add", "-A")
	gitRun(t, prep.Dir, "commit", "-m", "more")
	gitRun(t, prep.Dir, "push", "-u", "origin", taskWorktreeBranch)

	if err := reclaimTaskWorktree(context.Background(), prep.Dir, taskWorktreeBranch); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if _, err := os.Stat(prep.Dir); !os.IsNotExist(err) {
		t.Errorf("the worktree directory still exists after reclaim (err=%v)", err)
	}
	for _, path := range taskWorktreeListedPaths(t, f.checkout) {
		if taskWorktreeSamePath(path, prep.Dir) {
			t.Errorf("%s is still registered as a worktree", prep.Dir)
		}
	}
	if taskRefExists(t, f.checkout, "refs/heads/"+taskWorktreeBranch) {
		t.Errorf("the local branch %s survived the reclaim", taskWorktreeBranch)
	}
	// The pushed branch is the work: it must still be on origin.
	if !taskRefExists(t, f.origin, "refs/heads/"+taskWorktreeBranch) {
		t.Errorf("reclaim deleted the branch from origin")
	}
}

// The no-`-D` half of the reclaim is load-bearing: a branch standing ahead of
// its upstream holds the only copy of an unpushed commit, so `git branch -d`
// must refuse and the branch (and that commit) must survive in the checkout.
func TestReclaimTaskWorktree_KeepsABranchAheadOfItsUpstream(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	f.commitOn(t, taskWorktreeBranch, "origin/staging", "work.txt", "work", "work")
	req := f.request()
	req.Resume, req.NoMerge = true, true
	prep, err := f.prepare(t, req)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	gitRun(t, prep.Dir, "push", "-u", "origin", taskWorktreeBranch)
	// One more commit, deliberately NOT pushed: this is the session's last
	// local work, and the reclaim must not be able to delete it.
	writeTestFile(t, filepath.Join(prep.Dir, "unpushed.txt"), "unpushed")
	gitRun(t, prep.Dir, "add", "-A")
	gitRun(t, prep.Dir, "commit", "-m", "unpushed")
	unpushedTip := gitOut(t, prep.Dir, "rev-parse", "HEAD")

	err = reclaimTaskWorktree(context.Background(), prep.Dir, taskWorktreeBranch)
	if err == nil || !strings.Contains(err.Error(), "deleting the local branch") {
		t.Fatalf("reclaim(branch ahead of upstream) = %v, want a refusal naming the branch deletion", err)
	}
	if !taskRefExists(t, f.checkout, "refs/heads/"+taskWorktreeBranch) {
		t.Fatal("the local branch was deleted with its unpushed commit")
	}
	if got := gitOut(t, f.checkout, "rev-parse", "refs/heads/"+taskWorktreeBranch); got != unpushedTip {
		t.Errorf("the surviving branch is at %s, want the unpushed tip %s", got, unpushedTip)
	}
	// The pushed state on origin is unchanged; only the local commit was at risk.
	if got := gitOut(t, f.origin, "rev-parse", "refs/heads/"+taskWorktreeBranch); got == unpushedTip {
		t.Error("the unpushed commit reached origin; the fixture no longer tests the refusal")
	}
}

func TestReclaimTaskWorktree_NeverTouchesTheMainCheckout(t *testing.T) {
	f := newTaskWorktreeRepo(t)

	err := reclaimTaskWorktree(context.Background(), f.checkout, "staging")
	if err == nil || !strings.Contains(err.Error(), "main checkout") {
		t.Fatalf("reclaim(main checkout) = %v, want a refusal naming the main checkout", err)
	}
	if !taskRefExists(t, f.checkout, "refs/heads/staging") {
		t.Error("the refusal still deleted the main checkout's branch")
	}
}

func TestReclaimTaskWorktree_RefusesAnotherBranchsTree(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	prep, err := f.prepare(t, f.request())
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}

	if err := reclaimTaskWorktree(context.Background(), prep.Dir, "clanker/somebody-else"); err == nil {
		t.Fatal("reclaim removed a worktree that is not on the task's branch")
	}
	if _, err := os.Stat(prep.Dir); err != nil {
		t.Errorf("the refused worktree was removed anyway: %v", err)
	}
}

// No --force: untracked files are the session's, and the reclaim leaves the
// tree for the operator rather than deleting them.
func TestReclaimTaskWorktree_LeavesUntrackedFilesAlone(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	f.commitOn(t, taskWorktreeBranch, "origin/staging", "work.txt", "work", "work")
	req := f.request()
	req.Resume, req.NoMerge = true, true
	prep, err := f.prepare(t, req)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	gitRun(t, prep.Dir, "push", "-u", "origin", taskWorktreeBranch)
	writeTestFile(t, filepath.Join(prep.Dir, "scratch.txt"), "the session's scratch")

	if err := reclaimTaskWorktree(context.Background(), prep.Dir, taskWorktreeBranch); err == nil {
		t.Fatal("reclaim force-removed a dirty worktree")
	}
	if _, err := os.Stat(filepath.Join(prep.Dir, "scratch.txt")); err != nil {
		t.Errorf("the untracked file went with the refused reclaim: %v", err)
	}
	if !taskRefExists(t, f.checkout, "refs/heads/"+taskWorktreeBranch) {
		t.Error("the refused reclaim deleted the local branch anyway")
	}
}

// A tree outside the task-worktree roots is somebody else's: a registered
// worktree is not enough.
func TestReclaimTaskWorktree_RefusesATreeOutsideTheTaskRoots(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	outside := filepath.Join(t.TempDir(), "elsewhere")
	gitRun(t, f.checkout, "worktree", "add", "-b", "clanker/elsewhere", outside, "origin/staging")

	err := reclaimTaskWorktree(context.Background(), outside, "clanker/elsewhere")
	if err == nil || !strings.Contains(err.Error(), "task-worktree root") {
		t.Fatalf("reclaim(outside tree) = %v, want a refusal naming the task-worktree root", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("the outside tree was removed anyway: %v", err)
	}
}
