package events

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestSeqCounterPersistsAcrossCounters(t *testing.T) {
	store := NewMemorySeqStore()
	first := NewSeqCounter(store, nil)
	if got := first.Next("run-1.secret"); got != 1 {
		t.Fatalf("first Next = %d", got)
	}
	if got := first.Next("run-1.secret"); got != 2 {
		t.Fatalf("second Next = %d", got)
	}
	first.Commit("run-1.secret", 2)

	second := NewSeqCounter(store, nil)
	if got := second.Next("run-1.other-secret"); got != 3 {
		t.Fatalf("resumed Next = %d, want 3 (the counter follows the run, not the credential secret)", got)
	}
}

func TestSeqCounterKeysPerRun(t *testing.T) {
	c := NewSeqCounter(NewMemorySeqStore(), nil)
	if got := c.Next("run-a"); got != 1 {
		t.Fatalf("run-a first = %d", got)
	}
	if got := c.Next("run-b"); got != 1 {
		t.Fatalf("run-b first = %d, want its own counter", got)
	}
	if got := c.Next("run-a"); got != 2 {
		t.Fatalf("run-a second = %d", got)
	}
}

func TestSeqCounterConcurrentNextIsUnique(t *testing.T) {
	c := NewSeqCounter(NewMemorySeqStore(), nil)
	const n = 500
	var mu sync.Mutex
	seen := map[uint64]bool{}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seq := c.Next("run-1")
			mu.Lock()
			if seen[seq] {
				t.Errorf("sequence %d handed out twice", seq)
			}
			seen[seq] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(seen) != n {
		t.Fatalf("unique sequences = %d, want %d", len(seen), n)
	}
	for i := uint64(1); i <= n; i++ {
		if !seen[i] {
			t.Fatalf("sequence %d was never handed out", i)
		}
	}
}

// failingStore fails every read, the case where saving would overwrite an
// unreadable file with an empty map.
type failingStore struct{ saved bool }

func (f *failingStore) Load() (map[string]RunSeq, error) { return nil, fmt.Errorf("boom") }
func (f *failingStore) Save(map[string]RunSeq) error     { f.saved = true; return nil }

func TestSeqCounterDoesNotSaveAfterFailedLoad(t *testing.T) {
	store := &failingStore{}
	c := NewSeqCounter(store, nil)
	if got := c.Next("run-1"); got != 1 {
		t.Fatalf("Next = %d, want 1 despite the failed load", got)
	}
	c.Commit("run-1", 1)
	if store.saved {
		t.Fatal("Save ran after the initial Load failed, overwriting an unreadable file")
	}
}

func TestSeqCounterPersistsAnAllocationBeforeTheSend(t *testing.T) {
	store := NewMemorySeqStore()
	first := NewSeqCounter(store, nil)
	if got := first.Next("run-1"); got != 1 {
		t.Fatalf("Next = %d", got)
	}
	first.Persist("run-1") // what the emitter does BEFORE the POST
	// The process dies here, with no Commit. The next process must resume ABOVE
	// the batch that may have landed, not reuse numbers the plane would drop.
	second := NewSeqCounter(store, nil)
	if got := second.Next("run-1"); got != 2 {
		t.Fatalf("resumed Next = %d, want 2 (the allocation was durable before the send)", got)
	}
}

func TestSeqCounterSendLockSerializesOneRunOnly(t *testing.T) {
	c := NewSeqCounter(NewMemorySeqStore(), nil)
	unlockA := c.SendLock("run-a")
	acquired := make(chan struct{})
	go func() {
		unlock := c.SendLock("run-a")
		close(acquired)
		unlock()
	}()
	select {
	case <-acquired:
		t.Fatal("a second sender acquired the same run's send lock")
	case <-time.After(50 * time.Millisecond):
	}
	unlockA()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("the send lock was never released")
	}
	// A different run has its own slot.
	unlockB := c.SendLock("run-b")
	unlockB()
}

func TestSeqCounterKeepsMarksOfLiveRuns(t *testing.T) {
	store := NewMemorySeqStore()
	c := NewSeqCounter(store, nil)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	// More runs than the file cap, all touched just now: evicting any of them
	// could restart a live run at 1, whose events the plane would drop.
	total := maxPersistedRuns + 10
	for i := 0; i < total; i++ {
		c.Commit(fmt.Sprintf("run-%d", i), 1)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != total {
		t.Fatalf("persisted marks = %d, want all %d fresh ones", len(loaded), total)
	}
	// Once they have aged out, the cap applies again.
	now = now.Add(2 * minPersistedAge)
	c.Commit("run-fresh", 1)
	loaded, err = store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != maxPersistedRuns {
		t.Fatalf("persisted marks = %d, want the %d cap once the old ones age out", len(loaded), maxPersistedRuns)
	}
	if _, ok := loaded["run-fresh"]; !ok {
		t.Fatal("the fresh mark was evicted")
	}
}
