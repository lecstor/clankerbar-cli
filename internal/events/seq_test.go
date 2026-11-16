package events

import (
	"fmt"
	"sync"
	"testing"
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
