package loop

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// The drain-level half of the run-lost ending: when the plane refuses a lease
// renewal because this run no longer holds the task, the session is ended at once
// and the driver neither salvages its work, nor hands the claim back, nor spawns
// the next phase.

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
		in.OnClaim(lostRunClaim())
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
	// Nor is anything salvaged: the worktree belongs to a task this run no longer
	// holds, so committing or pushing from it is exactly what the plane forbade.
	if len(sv.calls) != 0 {
		t.Fatalf("salvaged %v, want nothing salvaged", sv.calls)
	}
}
