package loop

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// The drain-level half of the run-lost ending: when the plane refuses a lease
// renewal because this run no longer holds the task, the session is ended at once
// and the driver neither hands the claim back nor spawns the next phase. It does
// commit and push the worktree to the task branch, so the next claim finds a
// clean tree.

// lostRunClaim is the task the doomed session claims, so the renewer has
// something to beat.
func lostRunClaim() harness.Claim {
	return harness.Claim{TaskID: "t-1", Ref: "CLA-1", RunID: "r-1"}
}

// blockingAdapter holds its session open until the context is cancelled, which
// only the renewer can do in this fixture. It records the cancellation cause and
// how many sessions were spawned, so a second phase is visible.
type blockingAdapter struct {
	*fakeAdapter
	// claim is what the session claims; zero means lostRunClaim().
	claim   harness.Claim
	once    sync.Once
	started chan struct{}
	mu      sync.Mutex
	invokes int
	cause   error
}

func (a *blockingAdapter) Invoke(ctx context.Context, in harness.Invocation) (harness.Result, error) {
	a.mu.Lock()
	a.invokes++
	a.mu.Unlock()
	if in.OnClaim != nil {
		c := a.claim
		if c.TaskID == "" {
			c = lostRunClaim()
		}
		in.OnClaim(c)
	}
	a.once.Do(func() { close(a.started) })
	<-ctx.Done()
	a.mu.Lock()
	a.cause = context.Cause(ctx)
	a.mu.Unlock()
	return harness.Result{}, ctx.Err()
}

func (a *blockingAdapter) spawned() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.invokes
}

func (a *blockingAdapter) cancelledBy() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cause
}

func TestDrainPhases_RunLostEndsTheSessionAndTheDrain(t *testing.T) {
	hb := &recordingHeartbeat{err: fmt.Errorf("%w: run_superseded", plane.ErrRunLost), failuresLeft: 999}
	rel := &fakeReleaser{}
	h := &blockingAdapter{fakeAdapter: &fakeAdapter{}, started: make(chan struct{})}
	d := phaseDriverWithReleaser(t, h, twoPhases(), renewingReleaser{fakeReleaser: rel, hb: hb})
	// The production cadence is a third of a 30-minute lease; the test needs the
	// refusal to land while the session is still running.
	d.leaseRenewalInterval = time.Millisecond
	sv := &fakeSalvager{}
	d.newSalvager = func(string) workSalvager { return sv }

	// The drain blocks inside the first phase's session, so it runs on its own
	// goroutine and the test watches both the spawn and the ending.
	type drainOut struct {
		tokens int
		stop   bool
		err    error
	}
	done := make(chan drainOut, 1)
	go func() {
		tokens, _, stop, err := drainPhasesOnce(t, d)
		done <- drainOut{tokens, stop, err}
	}()

	select {
	case <-h.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the first phase never spawned")
	}

	var out drainOut
	select {
	case out = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the drain never returned after the run was refused")
	}
	if out.err != nil || out.stop {
		t.Fatalf("drainPhases: err=%v stop=%v, want a drain that ends without failing the run", out.err, out.stop)
	}
	if out.tokens != 0 {
		t.Errorf("counted %d token(s) for a session that reported none", out.tokens)
	}
	// The session ended because its context was cancelled, and by the plane's
	// verdict - not by anything else.
	if cause := h.cancelledBy(); !errors.Is(cause, plane.ErrRunLost) {
		t.Fatalf("session cancellation cause = %v, want the plane's ErrRunLost refusal", cause)
	}
	if n := h.spawned(); n != 1 {
		t.Fatalf("%d session(s) spawned, want 1: the phase after a lost run must not start", n)
	}
	// Nothing is owed a handback: the claim is not this run's to give back, and
	// posting it `ready` (or expiring it) would act on someone else's task.
	if len(rel.calls) != 0 || len(rel.expireCalls) != 0 {
		t.Fatalf("handback calls = release %v, expire %v; want none", rel.calls, rel.expireCalls)
	}
	// The worktree IS salvaged, under the claim the session was seen holding (the
	// adapter's Result carries none): the branch is still the task's.
	if len(sv.calls) != 1 || sv.calls[0].taskID != "t-1" {
		t.Fatalf("salvaged %v, want one salvage of t-1", sv.calls)
	}
}

// 2026-11-28 incident (MAK-531): a fix phase was ended by a lost run with its
// edits uncommitted, and the next two claims refused the dirty worktree and
// parked the task. The run-lost ending must leave the work committed and pushed
// on the task branch and the tree clean, without recording anything on the plane.
func TestRun_RealGitRepo_RunLostSalvagesTheDirtyWorktree(t *testing.T) {
	root, remote, wt, branch := newDriverGitEnv(t)
	writeTestFile(t, filepath.Join(wt, "a.txt"), "edited by the fix phase")
	writeTestFile(t, filepath.Join(wt, "new.go"), "package fix")

	hb := &recordingHeartbeat{err: fmt.Errorf("%w: run_superseded", plane.ErrRunLost), failuresLeft: 999}
	rel := &recordingReleaser{}
	claim := harness.Claim{TaskID: "cc561415-0000-0000-0000-000000000000", Ref: "MAK-531", RunID: "r-1"}
	h := &blockingAdapter{fakeAdapter: &fakeAdapter{}, started: make(chan struct{}), claim: claim}
	d := phaseDriverWithReleaser(t, h, twoPhases(), recordingRenewer{recordingReleaser: rel, hb: hb})
	d.cfg.WorkDir = root
	d.leaseRenewalInterval = time.Millisecond
	logs := captureLogs(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, _, _, stop, err := d.drainPhases(ctx, 1, 0, d.targets[0], spend{start: time.Now()}); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if left := gitOut(t, wt, "status", "--porcelain"); left != "" {
		t.Fatalf("worktree still dirty after a run-lost ending:\n%s", left)
	}
	head := gitOut(t, wt, "rev-parse", "HEAD")
	if pushed := gitOut(t, remote, "rev-parse", "refs/heads/"+branch); pushed != head {
		t.Fatalf("origin/%s is at %q, want the salvage commit %q", branch, pushed, head)
	}
	if files := gitOut(t, wt, "show", "--pretty=format:", "--name-only", "HEAD"); !strings.Contains(files, "a.txt") || !strings.Contains(files, "new.go") {
		t.Errorf("the salvage commit does not carry the session's edits:\n%s", files)
	}
	// The claim is not this run's: nothing is recorded or handed back.
	if len(rel.records) != 0 || len(rel.calls) != 0 || len(rel.expireCalls) != 0 {
		t.Errorf("plane writes after a lost run: records %v, release %v, expire %v; want none", rel.records, rel.calls, rel.expireCalls)
	}
	if out := logs.String(); !strings.Contains(out, "pushed its uncommitted work") {
		t.Errorf("the driver did not log what it pushed:\n%s", out)
	}
}

// recordingRenewer is a recordingReleaser that can also renew leases.
type recordingRenewer struct {
	*recordingReleaser
	hb *recordingHeartbeat
}

func (r recordingRenewer) Heartbeat(ctx context.Context, runID string) error {
	return r.hb.Heartbeat(ctx, runID)
}
