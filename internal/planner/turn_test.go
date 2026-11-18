package planner

import (
	"context"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

func (f *fakeFleet) countKind(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.sent {
		if r.State.Kind == kind {
			n++
		}
	}
	return n
}

// 1.review-1.4: the wall clock must bound the WHOLE turn from the claim, not
// only the harness invocation. The plane derives the claim's lease from the same
// max_wall_clock, so a slow pre-turn read could otherwise consume it and the
// reply would be refused as claim_lost.
func TestTurnDeadlineStartsAtTheClaimAndUsesTheLease(t *testing.T) {
	f := newFixture(t, map[string]any{"harness": "claude", "max_wall_clock": 900})
	f.poller.pending = pending("conv-1")
	c := claim("conv-1", 2, "hi")
	c.LeaseExpiresIn = 120
	f.convs.claim = c
	f.adapter.results = []harness.Result{{Tokens: 1}}

	f.lap(t)

	if len(f.adapter.deadlines) != 1 {
		t.Fatalf("deadlines = %v, want one", f.adapter.deadlines)
	}
	// 120 s lease less the 30 s reply margin, not the role's 900 s wall clock.
	if d := f.adapter.deadlines[0]; d > 90*time.Second || d < 80*time.Second {
		t.Errorf("adapter deadline = %v, want the claim's lease less the reply margin (~90s)", d)
	}
}

// 1.review-1.2: a turn can occupy the whole wall-clock bound, which is the
// plane's stale window, so presence is refreshed while the session runs.
type presenceAdapter struct {
	harness.Adapter
	fleet *fakeFleet
}

func (a *presenceAdapter) Capabilities() harness.Capabilities {
	return harness.Capabilities{TracksClaims: true, ResumesSessions: true}
}

func (a *presenceAdapter) Invoke(ctx context.Context, in harness.Invocation) (harness.Result, error) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a.fleet.countKind(fleet.StateIteration) >= 3 {
			break
		}
		select {
		case <-ctx.Done():
			return harness.Result{}, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	return harness.Result{SessionRef: in.SessionRef, Tokens: 1}, nil
}

func TestPresenceRefreshesWhileATurnRuns(t *testing.T) {
	f := newFixture(t, map[string]any{"harness": "claude"})
	f.planner.presence = 5 * time.Millisecond
	f.poller.pending = pending("conv-1")
	f.convs.claim = claim("conv-1", 2, "hi")
	f.planner.adapterFor = func(string) (harness.Adapter, error) {
		return &presenceAdapter{fleet: f.fleet}, nil
	}

	f.lap(t)

	// One at turn start, then at least one mid-turn refresh while the session
	// held; one would leave the row stale on a long turn.
	if n := f.fleet.countKind(fleet.StateIteration); n < 3 {
		t.Fatalf("iteration beacons = %d, want the turn's start plus mid-turn refreshes", n)
	}
}

// 1.review-1.14: TaskRef is the console's task-ref slot, and a conversation UUID
// is not a task ref. The conversation id stays in the local turn log.
func TestIterationBeaconDoesNotFakeATaskRef(t *testing.T) {
	f := newFixture(t, map[string]any{"harness": "claude"})
	f.poller.pending = pending("conv-1")
	f.convs.claim = claim("conv-1", 2, "hi")
	f.adapter.results = []harness.Result{{Tokens: 1}}

	f.lap(t)

	f.fleet.mu.Lock()
	defer f.fleet.mu.Unlock()
	for _, r := range f.fleet.sent {
		if r.State.Kind != fleet.StateIteration {
			continue
		}
		if r.State.TaskRef != "" {
			t.Errorf("iteration beacon TaskRef = %q, want empty (a conversation id is not a task ref)", r.State.TaskRef)
		}
		if r.State.Phase != "planner" {
			t.Errorf("iteration beacon Phase = %q, want planner", r.State.Phase)
		}
		return
	}
	t.Fatal("no iteration beacon")
}
