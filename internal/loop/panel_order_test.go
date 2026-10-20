package loop

// panel_order_test.go — the panel scheduler's start-order guarantee.
//
// The panel is a backfill queue with a concurrency cap, not a set of waves:
// member k+1 starts when a slot frees, and members start in list order. That
// order is what lets the operator put the slowest reviewer first, so its long
// session overlaps the others instead of extending the round's tail.

import (
	"testing"
	"time"
)

// TestPanelMembersStartInListOrderAndBackfill pins both halves of the contract:
// the first `limit` members start in k order; while the cap is full no further
// member starts; and when member 0 finishes, member 2 starts in the freed slot
// while member 1 is still running. A wave-based scheduler would not start 2
// until both 0 and 1 had finished, and a scheduler that dropped the cap would
// start 2 and 3 without waiting at all.
func TestPanelMembersStartInListOrderAndBackfill(t *testing.T) {
	const (
		n     = 4
		limit = 2
	)
	started := make(chan int, n)
	release := make([]chan struct{}, n)
	for k := range release {
		release[k] = make(chan struct{})
	}
	done := make(chan struct{})

	go func() {
		runPanelMembers(n, limit, func(k int, begun func()) {
			started <- k
			begun()
			<-release[k]
		})
		close(done)
	}()

	expectStart := func(want int) {
		t.Helper()
		select {
		case got := <-started:
			if got != want {
				t.Fatalf("member %d started, want member %d — the panel did not start in list order", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("member %d never started — the scheduler did not backfill a freed slot", want)
		}
	}

	expectStart(0)
	expectStart(1)

	// Both slots are held. Nothing else may start until one of them ends; this
	// is the assertion that makes a dropped concurrency cap fail the test.
	select {
	case got := <-started:
		t.Fatalf("member %d started with both slots still held — the concurrency cap did not hold", got)
	case <-time.After(100 * time.Millisecond):
	}

	// Member 0 ends while member 1 is still in flight. The freed slot must go to
	// member 2, the next in list order, not to a batch-mate and not to member 3.
	release[0] <- struct{}{}
	expectStart(2)

	// Member 1 ends; member 3 starts in the freed slot.
	release[1] <- struct{}{}
	expectStart(3)

	release[2] <- struct{}{}
	release[3] <- struct{}{}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runPanelMembers did not return after every member was released")
	}
}
