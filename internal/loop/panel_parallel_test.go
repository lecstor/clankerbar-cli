package loop

// panel_parallel_test.go — CLA-697's fan-out, driven end to end over a REAL git
// repository so the worktrees are real worktrees.
//
// The tests assert the properties the task's bar names:
//   - concurrent success: every member is in flight at the same time, each in a
//     distinct DETACHED worktree at the reviewed tip, and the fix runs once on the
//     merged, released round;
//   - one member failing: the healthy members' records still reach the fix, and
//     the failed member is named INCOMPLETE;
//   - cap = 1: today's sequential behaviour, including no member worktrees.
//
// A barrier in the fake adapter is what proves concurrency rather than assuming
// it from timing: a review call blocks until all N reviews have STARTED, so a
// sequential run cannot pass the first one.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/delivery"
	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// panelParallelRepo is a real repository with one branch carrying one commit —
// the reviewed tip. A panel member's worktree is cut from it, so the worktree is
// a genuine `git worktree add --detach <tip>` and the tests can read its HEAD.
func panelParallelRepo(t *testing.T) (repo, branch, tip string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	root := t.TempDir()
	repo = filepath.Join(root, "repo")
	gitRun(t, root, "init", "-b", "main", repo)
	gitRun(t, repo, "config", "user.email", "driver@example.test")
	gitRun(t, repo, "config", "user.name", "Driver Test")
	gitRun(t, repo, "config", "commit.gpgsign", "false")
	writeTestFile(t, filepath.Join(repo, "a.txt"), "base")
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-m", "base")
	branch = "clanker/panel-parallel"
	gitRun(t, repo, "checkout", "-b", branch)
	writeTestFile(t, filepath.Join(repo, "a.txt"), "the reviewed work")
	gitRun(t, repo, "commit", "-am", "the reviewed work")
	tip = gitOut(t, repo, "rev-parse", "HEAD")
	return repo, branch, tip
}

// panelVerifier is a concurrency-safe delivery verifier: every member's
// checkpointEvidence asks it about the same branch at once, and the shipped
// fakeVerifier appends to a slice.
type panelVerifier struct{ tip string }

func (v *panelVerifier) Verify(context.Context, delivery.Claim) delivery.Report {
	return delivery.Report{Checks: []delivery.Check{{
		Kind: delivery.BranchPushed, Status: delivery.Pass, Tip: v.tip,
		Detail: "panel fixture: branch present",
	}}}
}

// panelParallelAdapter answers implement, review (concurrently) and fix. It is
// safe for concurrent review calls: the driver really does call it from N
// goroutines.
type panelParallelAdapter struct {
	*fakeAdapter
	rel     *reviewReleaser
	branch  string
	tip     string
	members int
	// expectConcurrent is how many reviews must be in flight before any may
	// finish: the effective cap. 1 is the sequential path, where there is no
	// barrier to wait on.
	expectConcurrent int
	// wantRecords is how many durable records the fix should see.
	wantRecords int
	// failLabel is the member that records nothing, when set.
	failLabel string
	// failToolLoop, when set, is the cause the failing member's adapter
	// reports for killing it on a repeated failing tool call (CLA-739),
	// still holding the claim it resumed.
	failToolLoop string

	mu          sync.Mutex
	calls       int
	reviewCalls int
	fixCalls    int
	fixPrompt   string
	inFlight    int
	maxInFlight int
	started     int
	allStarted  chan struct{}
	// per member label: the directory it ran in and that directory's HEAD.
	workdirs map[string]string
	heads    map[string]string
	// order is the reviewer role of every adapter invocation, in the order the
	// adapters were actually entered: the launch order the panel promises.
	order []string
	errs  []string
}

func newPanelParallelAdapter(rel *reviewReleaser, branch, tip string, members int) *panelParallelAdapter {
	return &panelParallelAdapter{
		fakeAdapter:      &fakeAdapter{},
		rel:              rel,
		branch:           branch,
		tip:              tip,
		members:          members,
		expectConcurrent: members,
		wantRecords:      members,
		allStarted:       make(chan struct{}),
		workdirs:         map[string]string{},
		heads:            map[string]string{},
	}
}

// reviewRoleFromPrompt reads the role the driver substituted into the brief: it
// is the one identifier both paths carry, sequential (where the session runs in
// the shared checkout) and parallel (where the worktree's name also spells the
// member's log LABEL, not its role).
func reviewRoleFromPrompt(prompt string) string {
	const marker = `reviewerRole: "`
	i := strings.Index(prompt, marker)
	if i < 0 {
		return ""
	}
	rest := prompt[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func (a *panelParallelAdapter) Invoke(ctx context.Context, in harness.Invocation) (harness.Result, error) {
	a.mu.Lock()
	a.calls++
	call := a.calls
	a.mu.Unlock()

	switch {
	case strings.Contains(in.Prompt, "REVIEW-ONLY gate"):
		return a.review(in)
	case strings.Contains(in.Prompt, "the FIX phase"):
		a.mu.Lock()
		a.fixCalls++
		a.fixPrompt = in.Prompt
		// The join must have closed the panel's round BEFORE the fix spawns.
		closed := len(a.rel.closes)
		records := len(a.rel.records)
		a.mu.Unlock()
		if closed != 1 {
			a.recordErr("the fix phase spawned with %d closes recorded, want exactly 1", closed)
		}
		if records != a.wantRecords {
			a.recordErr("the fix phase spawned with %d durable records, want %d", records, a.wantRecords)
		}
		a.rel.mu.Lock()
		a.rel.status = "in_review"
		a.rel.outcome += "\n" + config.ReviewRecordHeading + "\n- verdict: findings\n"
		a.rel.mu.Unlock()
		return held(okResult(5, 0.05), a.claim()), nil
	}
	// The implement phase: claim the task and deliver the branch at the tip the
	// fixture put on origin.
	_ = call
	return held(okResult(10, 0.10), a.claim()), nil
}

func (a *panelParallelAdapter) claim() harness.Claim {
	return harness.Claim{TaskID: "t-1", RunID: "r-1", Branch: a.branch, HasWIP: true}
}

func (a *panelParallelAdapter) review(in harness.Invocation) (harness.Result, error) {
	label := reviewRoleFromPrompt(in.Prompt)
	if label == "" {
		a.recordErr("a review brief carried no reviewerRole: %s", in.Prompt)
		return held(okResult(0, 0), a.claim()), nil
	}
	head, err := panelGitOutput(in.WorkDir, "rev-parse", "HEAD")
	if err != nil {
		a.recordErr("member %s: git rev-parse HEAD in %s: %v", label, in.WorkDir, err)
	}
	// A probe artifact, exactly the thing a member must not leave in the shared
	// tree. The driver discards this worktree, so it must never reach the branch.
	if err := os.WriteFile(filepath.Join(in.WorkDir, "member-probe-"+label+".txt"), []byte("artifact"), 0o600); err != nil {
		a.recordErr("member %s: could not write its probe artifact: %v", label, err)
	}
	// A TRACKED edit too: a review-only member must never have this salvaged and
	// committed on its behalf, which would move the tip past the reviewed one.
	if err := os.WriteFile(filepath.Join(in.WorkDir, "a.txt"), []byte("a member edit"), 0o600); err != nil {
		a.recordErr("member %s: could not edit the tracked file: %v", label, err)
	}

	a.mu.Lock()
	a.reviewCalls++
	a.order = append(a.order, label)
	a.workdirs[label] = in.WorkDir
	a.heads[label] = head
	a.inFlight++
	if a.inFlight > a.maxInFlight {
		a.maxInFlight = a.inFlight
	}
	a.started++
	if a.started == a.expectConcurrent {
		close(a.allStarted)
	}
	a.mu.Unlock()

	if label == a.failLabel {
		// The barrier still counts it as started — the panel's other members must
		// not be serialised by the failure — but it writes NO durable record.
		a.waitBarrier(label)
		a.mu.Lock()
		a.inFlight--
		a.mu.Unlock()
		if a.failToolLoop != "" {
			res := okResult(1, 0.01)
			res.ExitCode = -1
			res.ToolLoop = a.failToolLoop
			res.Claim = in.ResumeClaim
			return res, nil
		}
		return okResult(1, 0.01), nil
	}

	// Prove the members overlap: wait until every member the cap allows has
	// started before any one of them finishes. A sequential driver blocks here on
	// member 1 for the whole timeout — which is the point.
	a.waitBarrier(label)

	err = a.rel.writeReviewRecord(plane.ReviewRecordState{
		Round: 1, ReviewerRole: label, RunID: "r-1", ReviewedTip: a.tip, State: "sealed",
		Findings: []plane.ReviewFindingState{
			mustFinding("1."+label+".1", "open", "a defect only "+label+" saw"),
		},
	})
	if err != nil {
		a.recordErr("member %s could not append its record: %v", label, err)
	}

	a.mu.Lock()
	a.inFlight--
	a.mu.Unlock()
	return held(okResult(5, 0.50), a.claim()), nil
}

// waitBarrier blocks until expectConcurrent reviews have started. With a cap of
// 1 there is nothing to prove and nothing to wait for.
func (a *panelParallelAdapter) waitBarrier(label string) {
	if a.expectConcurrent <= 1 {
		return
	}
	select {
	case <-a.allStarted:
	case <-time.After(10 * time.Second):
		a.mu.Lock()
		started := a.started
		a.mu.Unlock()
		a.recordErr("member %s waited for %d concurrent members but only %d started — the panel ran sequentially", label, a.expectConcurrent, started)
	}
}

func (a *panelParallelAdapter) recordErr(format string, args ...any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.errs = append(a.errs, fmt.Sprintf(format, args...))
}

func panelGitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// panelParallelDriver wires a panel over a real checkout. concurrency 0 means
// the config's default (the whole panel). The fakeFleet is on the target so the
// iteration RECORD is observable: free text in its Outcome is what the plane
// refused in CLA-738, and the parallel path is the shipped default.
func panelParallelDriver(t *testing.T, h harness.Adapter, rel *reviewReleaser, members []config.ReviewPanelMember, workdir, tip string, concurrency int) (*Driver, *fakeFleet) {
	t.Helper()
	cfg := fastCfg()
	cfg.Prompt = ""
	cfg.Phases = panelPhases()
	cfg.ReviewPanel = members
	cfg.ReviewPanelConcurrency = concurrency
	cfg.WorkDir = workdir
	cfg.MaxIterations = 1
	ff := &fakeFleet{}
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: rel, Fleet: ff}})
	d.newAdapter = func(string) (harness.Adapter, error) { return h, nil }
	openTestStateDir(t, d)
	d.newVerifier = func(string, bool) deliveryVerifier { return &panelVerifier{tip: tip} }
	d.probePRBase = func(context.Context, string, string) (bool, string, error) { return true, "", nil }
	return d, ff
}

// memberNames is the panel's configured order, for the launch-order assertion.
func memberNames(members []config.ReviewPanelMember) []string {
	out := make([]string, len(members))
	for i, m := range members {
		out[i] = m.Name
	}
	return out
}

// requireOneFleetRecord is CLA-738's invariant on the PARALLEL path — the
// shipped default, and the shape the CLA-598/CLA-717/CLA-727 incident ran. The
// panel must post exactly one iteration record and its Outcome must be one of
// the four words the plane knows: free text there made marshalReport refuse the
// WHOLE report, dropping the record and the presence refresh riding it.
func requireOneFleetRecord(t *testing.T, ff *fakeFleet) fleet.Iteration {
	t.Helper()
	recs := ff.records()
	if len(recs) != 1 {
		t.Fatalf("iteration records = %d, want 1 — the panel's report was dropped or duplicated", len(recs))
	}
	requireFleetOutcome(t, recs[0].Outcome, "")
	return recs[0]
}

func TestDrainPhases_ParallelPanelRunsMembersAtOnceInDetachedWorktrees(t *testing.T) {
	repo, branch, tip := panelParallelRepo(t)
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented\n"}
	members := panelMembers()
	a := newPanelParallelAdapter(rel, branch, tip, len(members))
	d, ff := panelParallelDriver(t, a, rel, members, repo, tip, 0)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	a.mu.Lock()
	for _, e := range a.errs {
		t.Error(e)
	}
	maxInFlight := a.maxInFlight
	workdirs := make(map[string]string, len(a.workdirs))
	for k, v := range a.workdirs {
		workdirs[k] = v
	}
	heads := make(map[string]string, len(a.heads))
	for k, v := range a.heads {
		heads[k] = v
	}
	order := append([]string(nil), a.order...)
	a.mu.Unlock()
	if maxInFlight != len(members) {
		t.Errorf("max reviews in flight = %d, want %d — the members did not run concurrently", maxInFlight, len(members))
	}
	if len(workdirs) != len(members) {
		t.Fatalf("reviews ran in %d distinct worktrees, want %d: %v", len(workdirs), len(members), workdirs)
	}
	// The adapters were ENTERED in the config's list order, not merely created
	// in it (CLA-738 review 3.2): the next member is released from this one's
	// launch boundary, not from its checkout being ready.
	for k, m := range members {
		if k >= len(order) || order[k] != m.Name {
			t.Fatalf("adapter invocation order = %v, want the panel's list order %v", order, memberNames(members))
		}
	}
	// And the iteration record the plane receives keeps the four-word enum.
	requireOneFleetRecord(t, ff)
	seen := map[string]bool{}
	for label, dir := range workdirs {
		if dir == repo {
			t.Errorf("member %s ran in the shared checkout", label)
		}
		if seen[dir] {
			t.Errorf("two members shared the worktree %s", dir)
		}
		seen[dir] = true
		if heads[label] != tip {
			t.Errorf("member %s reviewed HEAD %s, want the reviewed tip %s", label, heads[label], tip)
		}
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("member %s's worktree %s still exists after the panel (err=%v)", label, dir, err)
		}
		if _, err := os.Stat(filepath.Join(repo, "member-probe-"+label+".txt")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("member %s's probe artifact leaked into the shared checkout", label)
		}
	}
	// The reviewed tip did not move: a review-only member's tree is never
	// salvaged, so nothing can have been committed on its behalf.
	if got := gitOut(t, repo, "rev-parse", branch); got != tip {
		t.Errorf("branch tip moved from %s to %s — a member worktree was salvaged", tip, got)
	}
	if got := gitOut(t, repo, "show", branch+":a.txt"); got != "the reviewed work" {
		t.Errorf("the reviewed tree reads %q, want the member's tracked edit discarded", got)
	}
	if len(rel.closes) != 1 || rel.closes[0] != 1 {
		t.Errorf("closed rounds = %v, want exactly [1] after the join", rel.closes)
	}
	if a.fixCalls != 1 {
		t.Errorf("fix sessions = %d, want 1", a.fixCalls)
	}
	if len(rel.records) != len(members) {
		t.Fatalf("durable records = %d, want %d", len(rel.records), len(members))
	}
	for _, r := range rel.records {
		if r.State != "open" {
			t.Errorf("record %s is %q after the drain, want the round released", r.ReviewerRole, r.State)
		}
	}
}

func TestDrainPhases_ParallelPanelOneMemberFailingStillRunsTheFix(t *testing.T) {
	repo, branch, tip := panelParallelRepo(t)
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented\n"}
	members := panelMembers()
	a := newPanelParallelAdapter(rel, branch, tip, len(members))
	a.failLabel = "review-2"
	a.wantRecords = len(members) - 1
	d, ff := panelParallelDriver(t, a, rel, members, repo, tip, 0)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	a.mu.Lock()
	for _, e := range a.errs {
		t.Error(e)
	}
	maxInFlight := a.maxInFlight
	fixCalls := a.fixCalls
	fix := a.fixPrompt
	a.mu.Unlock()
	if maxInFlight != len(members) {
		t.Errorf("max reviews in flight = %d, want %d — the failure must not serialise the panel", maxInFlight, len(members))
	}
	if len(rel.records) != len(members)-1 {
		t.Fatalf("durable records = %d, want %d (the failing member wrote none): %+v", len(rel.records), len(members)-1, rel.records)
	}
	if len(rel.closes) != 1 || rel.closes[0] != 1 {
		t.Errorf("closed rounds = %v, want exactly [1] — the healthy members' round must still be released", rel.closes)
	}
	if fixCalls != 1 {
		t.Fatalf("fix sessions = %d, want 1 — a failed member must not strand the panel's findings", fixCalls)
	}
	if !strings.Contains(fix, "review-2") || !strings.Contains(fix, "INCOMPLETE") {
		t.Errorf("the fix brief does not name the failed member as INCOMPLETE:\n%s", fix)
	}
	if !strings.Contains(fix, "1.review-1.1") || !strings.Contains(fix, "1.review-3.1") {
		t.Errorf("the fix brief does not carry the healthy members' findings:\n%s", fix)
	}
	// CLA-738: the INCOMPLETE panel is a log fact, never free text in the
	// iteration outcome — a prose outcome makes the plane refuse the whole report.
	requireOneFleetRecord(t, ff)
}

func TestDrainPhases_ParallelPanelCapOneIsTheSequentialPath(t *testing.T) {
	repo, branch, tip := panelParallelRepo(t)
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented\n"}
	members := panelMembers()
	a := newPanelParallelAdapter(rel, branch, tip, len(members))
	a.expectConcurrent = 1
	d, ff := panelParallelDriver(t, a, rel, members, repo, tip, 1)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	a.mu.Lock()
	for _, e := range a.errs {
		t.Error(e)
	}
	maxInFlight := a.maxInFlight
	workdirs := make(map[string]string, len(a.workdirs))
	for k, v := range a.workdirs {
		workdirs[k] = v
	}
	fixCalls := a.fixCalls
	a.mu.Unlock()
	if maxInFlight != 1 {
		t.Errorf("max reviews in flight = %d with cap 1, want 1", maxInFlight)
	}
	for label, dir := range workdirs {
		if dir != repo {
			t.Errorf("member %s ran in %s with cap 1, want the shared checkout %s", label, dir, repo)
		}
	}
	if len(rel.records) != len(members) {
		t.Fatalf("durable records = %d, want %d", len(rel.records), len(members))
	}
	if len(rel.closes) != 1 || rel.closes[0] != 1 {
		t.Errorf("closed rounds = %v, want exactly [1]", rel.closes)
	}
	if fixCalls != 1 {
		t.Errorf("fix sessions = %d, want 1", fixCalls)
	}
	requireOneFleetRecord(t, ff)
}

// --- CLA-697, review round 1: the fix delta --------------------------------

// TestPanelSpendLedgerAggregatesPeers pins the ledger's arithmetic: a member's
// published total is what its SIBLINGS see, never itself (its own accumulator
// already carries that), and a republish REPLACES the total rather than adding
// to it — the member publishes its running accumulator, not a delta.
func TestPanelSpendLedgerAggregatesPeers(t *testing.T) {
	ledger := newPanelSpend()
	one := &panelSpendHandle{ledger: ledger, self: ledger.join()}
	two := &panelSpendHandle{ledger: ledger, self: ledger.join()}

	if tokens, cost := one.peers(); tokens != 0 || cost != 0 {
		t.Fatalf("a ledger with nothing published reports (%d, %v), want zero", tokens, cost)
	}
	one.publish(120, 0.60)
	if tokens, cost := two.peers(); tokens != 120 || cost != 0.60 {
		t.Errorf("the sibling's published spend reads (%d, %v), want (120, 0.6)", tokens, cost)
	}
	if tokens, cost := one.peers(); tokens != 0 || cost != 0 {
		t.Errorf("a member must not count its own spend as a peer's: (%d, %v)", tokens, cost)
	}
	one.publish(200, 0.90)
	two.publish(50, 0.25)
	if tokens, cost := one.peers(); tokens != 50 || cost != 0.25 {
		t.Errorf("the other member's republished total reads (%d, %v), want (50, 0.25)", tokens, cost)
	}
}

// TestPanelSpendLedgerIsConcurrencySafe hammers the ledger from many goroutines:
// the members really do publish from separate goroutines at once (CLA-697), and
// the ledger's whole job is to make that safe. Run under -race it is the check
// that matters; without it, it still pins that nothing panics or deadlocks.
func TestPanelSpendLedgerIsConcurrencySafe(t *testing.T) {
	ledger := newPanelSpend()
	var wg sync.WaitGroup
	for k := 0; k < 8; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			h := &panelSpendHandle{ledger: ledger, self: ledger.join()}
			for i := 1; i <= 50; i++ {
				h.publish(k*i, float64(k)*0.1)
				h.peers()
			}
		}(k)
	}
	wg.Wait()
}

// TestDrainPhases_ParallelPanelStopsAMemberWhenTheAggregateReachesTheCeiling is
// review finding 1.review-1.1's fix, end to end. With cap 2 over three members,
// the third member waits on the concurrency slot until a sibling has ENDED; by
// then that sibling's $0.50 is published, and the run-wide check reads
// $0.10 (implement) + $0.50 (the sibling) ≥ the $0.50 ceiling, so the member
// refuses to spawn. Before the fix its check measured only the prior run totals
// plus its own (still zero) local spend and it spawned, taking the panel's spend
// past the ceiling the aggregate had already reached.
func TestDrainPhases_ParallelPanelStopsAMemberWhenTheAggregateReachesTheCeiling(t *testing.T) {
	repo, branch, tip := panelParallelRepo(t)
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented\n"}
	members := panelMembers()
	a := newPanelParallelAdapter(rel, branch, tip, len(members))
	// Only the concurrent pair runs to the barrier; the third member is parked on
	// the cap until one of them ends, which is what staggers the starts.
	a.expectConcurrent = 2
	d, ff := panelParallelDriver(t, a, rel, members, repo, tip, 2)
	// The implement attempt spends $0.10; each review costs $0.50.
	d.cfg.Budget = config.Budget{MaxCostUSD: 0.50}

	_, _, stop, err := drainPhasesOnce(t, d)
	if err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	if !stop {
		t.Error("a member tripping the run-wide ceiling must stop the run")
	}
	a.mu.Lock()
	reviews := a.reviewCalls
	fixCalls := a.fixCalls
	errs := append([]string(nil), a.errs...)
	a.mu.Unlock()
	for _, e := range errs {
		t.Error(e)
	}
	if reviews != 2 {
		t.Errorf("review sessions = %d, want 2 — a member spawned against a ceiling the panel's aggregate had already reached", reviews)
	}
	if fixCalls != 0 {
		t.Errorf("fix sessions = %d, want 0 — the run stopped on the ceiling instead", fixCalls)
	}
	requireOneFleetRecord(t, ff)
}

// TestDrainPhases_ParallelPanelBaselineFailureLeavesTheSharedClaimToTheJoin is
// review finding 1.review-3.1's fix: one member's pre-spawn baseline-read
// failure must not expire the ONE claim its siblings are still recording under.
// The claim here carries pushed work, so the handback is an expireLease — and
// the property is that it happens EXACTLY ONCE (the panel's join), not once per
// member. Before the fix each of the three members called releaseHeldClaim on
// its way out, ending the shared lease three times over.
func TestDrainPhases_ParallelPanelBaselineFailureLeavesTheSharedClaimToTheJoin(t *testing.T) {
	repo, branch, tip := panelParallelRepo(t)
	rel := &reviewReleaser{
		status:  "in_progress",
		holder:  "r-1",
		outcome: "implemented\n",
		readErr: func() error { return errors.New("plane down") },
	}
	members := panelMembers()
	a := newPanelParallelAdapter(rel, branch, tip, len(members))
	d, ff := panelParallelDriver(t, a, rel, members, repo, tip, 0)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	a.mu.Lock()
	reviews := a.reviewCalls
	errs := append([]string(nil), a.errs...)
	a.mu.Unlock()
	for _, e := range errs {
		t.Error(e)
	}
	requireOneFleetRecord(t, ff)
	if reviews != 0 {
		t.Fatalf("review sessions = %d, want 0 — every member's baseline read failed", reviews)
	}
	rel.mu.Lock()
	expires := append([]releaseCall(nil), rel.expireCalls...)
	releases := append([]releaseCall(nil), rel.calls...)
	rel.mu.Unlock()
	if len(releases) != 0 {
		t.Errorf("the claim carries pushed work and must never be Release()d: %v", releases)
	}
	if len(expires) != 1 {
		t.Errorf("expireLease calls = %d, want exactly 1 (the join's single handback, not one per member): %v", len(expires), expires)
	}
}
