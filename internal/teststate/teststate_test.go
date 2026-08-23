package teststate

import (
	"os"
	"reflect"
	"testing"
)

// The exemplar package follows its own rule (CLA-361): this binary installs
// the same isolation and guard every config-reaching test binary must carry.
func TestMain(m *testing.M) { os.Exit(Isolate(m)) }

// The guard's arithmetic is the whole detection: entries in the after snapshot
// that the before snapshot lacks are pollution, whatever else changed. Pinned
// here so a refactor of Isolate cannot quietly weaken it.
func TestAddedNamesReturnsOnlyNewEntries(t *testing.T) {
	before := []string{"001-aaa", "001-bbb"}
	after := []string{"001-aaa", "001-ccc", "001-bbb", "001-ddd"}
	got := addedNames(before, after)
	want := []string{"001-ccc", "001-ddd"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("addedNames = %v, want %v", got, want)
	}
}

func TestAddedNamesWithNoNewEntriesIsEmpty(t *testing.T) {
	before := []string{"001-aaa"}
	after := []string{"001-aaa"}
	if got := addedNames(before, after); len(got) != 0 {
		t.Errorf("addedNames = %v, want empty", got)
	}
}

func TestAddedNamesWithEmptyBeforeReturnsEverything(t *testing.T) {
	// A missing real root reads as an empty snapshot: the first run on a fresh
	// machine must still flag every entry the run created there.
	got := addedNames(nil, []string{"001-aaa", "001-bbb"})
	if len(got) != 2 {
		t.Errorf("addedNames = %v, want both entries", got)
	}
}
