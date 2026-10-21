package loop

// panel_order_test.go — the panel scheduler's start-order guarantee.
//
// The panel is a backfill queue with a concurrency cap, not a set of waves:
// member k+1 starts when a slot frees, and members start in list order. That
// order is what lets the operator put the slowest reviewer first, so its long
// session overlaps the others instead of extending the round's tail.

import (
	"sync"
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
		}, nil)
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

// TestPanelMembersReleaseBeforeTheSuccessorRuns observes that member k's release
// is recorded before member k+1's run function begins, and would catch a
// consistently wrong order. Whether the successor's run function begins before
// the release is recorded is schedule-dependent, not pinned: the successor's
// start after the close is a scheduling race — in the real panel it also has its
// own git worktree add to get through — so a lucky run can still see the release
// first. A swap in the release's order would silently turn the panel's
// release-point assertion, which counts releases at a launch, into a race.
// TestReleaseFiresBeforeTheSuccessorIsUnblocked is the deterministic pin for that
// ordering; do not read this test as covering it (CLA-757 review rounds
// 1.review-1.2 and 2.review-5.1).
func TestPanelMembersReleaseBeforeTheSuccessorRuns(t *testing.T) {
	const n = 4
	var mu sync.Mutex
	var released []int
	// seen[k] is how many releases member k's run function observed when it
	// began. Member k is spawned only after member k-1 released it, so it must
	// see exactly k releases.
	seen := make([]int, n)

	runPanelMembers(n, n, func(k int, begun func()) {
		mu.Lock()
		seen[k] = len(released)
		mu.Unlock()
		begun()
	}, func(k int) {
		mu.Lock()
		released = append(released, k)
		mu.Unlock()
	})

	mu.Lock()
	defer mu.Unlock()
	if len(released) != n {
		t.Fatalf("release probe saw %v, want one release for each of the %d members", released, n)
	}
	for k := 0; k < n; k++ {
		if seen[k] != k {
			t.Errorf("member %d's run began with %d releases observed, want %d — a successor was unblocked before its predecessor's release was observed",
				k, seen[k], k)
		}
	}
}

// TestReleaseFiresBeforeTheSuccessorIsUnblocked is the deterministic half of
// TestPanelMembersReleaseBeforeTheSuccessorRuns. At the scheduler level the
// successor's start after the close is a scheduling race, so a swapped order
// could slip through on a lucky run; the release's own order is one function, so
// it is checked here without a schedule.
func TestReleaseFiresBeforeTheSuccessorIsUnblocked(t *testing.T) {
	var released []int
	var unblockedWith []int
	releaseMember(2,
		func(k int) { released = append(released, k) },
		func() { unblockedWith = append(unblockedWith, released...) })
	if len(unblockedWith) != 1 || unblockedWith[0] != 2 {
		t.Fatalf("the successor was unblocked with releases %v observed, want [2] — the release must be observed before the successor is unblocked", unblockedWith)
	}
}

// TestPanelMembersReleaseAMemberThatNeverStarts pins the safety-net defer: a
// member whose session never reaches its launch boundary (a setup failure, a
// budget refusal) never calls begun itself, and the defer is the only path that
// releases its successor. Without it the scheduler waits forever, and the
// release probe must still see exactly one release for that member so a panel
// fixture's count cannot be satisfied by an accidental double release
// (CLA-757 review 1.review-1.3).
func TestPanelMembersReleaseAMemberThatNeverStarts(t *testing.T) {
	const n = 3
	var mu sync.Mutex
	releases := make([]int, n)
	done := make(chan struct{})

	go func() {
		runPanelMembers(n, 1, func(k int, begun func()) {
			// Member 1 never launches a session.
			if k == 1 {
				return
			}
			begun()
		}, func(k int) {
			mu.Lock()
			releases[k]++
			mu.Unlock()
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runPanelMembers did not return — a member that never starts was not released by the safety-net defer")
	}
	mu.Lock()
	defer mu.Unlock()
	for k := 0; k < n; k++ {
		if releases[k] != 1 {
			t.Errorf("member %d released %d times, want exactly 1 — the release probe must observe every member once, whether it started or not",
				k, releases[k])
		}
	}
}
