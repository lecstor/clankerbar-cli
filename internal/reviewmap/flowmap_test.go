package reviewmap

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

var errTest = errors.New("test error")

func TestChangedDefinitionsDedupesAcrossFiles(t *testing.T) {
	files := []FileDiff{
		addedFile("a.ts", 1, AddedLine{Line: 1, Code: "function alphaThing() {"}),
		addedFile("b.ts", 5, AddedLine{Line: 5, Code: "function alphaThing() {"}),
		addedFile("a.go", 1, AddedLine{Line: 1, Code: "func alphaThing() {"}),
	}
	want := []Definition{{Name: "alphaThing", At: "a.ts:1"}}
	if got := changedDefinitions(files, nil); !reflect.DeepEqual(got, want) {
		t.Errorf("changedDefinitions() = %#v, want %#v", got, want)
	}
}

func TestChangedDefinitionsMoveIsNotARemoval(t *testing.T) {
	src := staticSource{
		head: map[string]string{
			"a.go": "package demo\n",
			"b.go": "package demo\n\ntype Widget struct{}\n",
		},
		base: map[string]string{
			"a.go": "package demo\n\ntype Widget struct{}\n",
			"b.go": "package demo\n",
		},
	}
	removed := FileDiff{Path: "a.go"}
	added := FileDiff{Path: "b.go", Hunks: []Hunk{{Start: 1, Count: 3}}}
	want := []Definition{{Name: "Widget", At: "b.go:3"}}

	// Whichever file the diff lists first, a name that still exists at head
	// is a move, not a removal.
	for _, files := range [][]FileDiff{{removed, added}, {added, removed}} {
		if got := changedDefinitions(files, src); !reflect.DeepEqual(got, want) {
			t.Errorf("changedDefinitions() = %#v\nwant %#v", got, want)
		}
	}
}

func TestRenderFlowItemsCap(t *testing.T) {
	defs := []Definition{
		{Name: "alphaThing", At: "a.ts:1"},
		{Name: "betaThing", At: "a.ts:2"},
		{Name: "gammaThing", At: "a.ts:3"},
	}
	find := func(Definition) (string, error) { return "nowhere else", nil }

	got, err := renderFlowItems(defs, 2, find)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"`alphaThing` defined or changed at a.ts:1; used at: nowhere else",
		"`betaThing` defined or changed at a.ts:2; used at: nowhere else",
		"(1 more changed definitions not listed)",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("renderFlowItems(cap 2) = %#v\nwant %#v", got, want)
	}

	all, err := renderFlowItems(defs, Unlimited, find)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("renderFlowItems(unlimited) returned %d items, want 3", len(all))
	}

	// Zero selects the benchmark default, which is above this list.
	def, err := renderFlowItems(defs, 0, find)
	if err != nil {
		t.Fatal(err)
	}
	if len(def) != 3 {
		t.Errorf("renderFlowItems(0) returned %d items, want 3", len(def))
	}
}

// TestRenderFlowItemsDefaultCap pins the benchmark default at 40 names: 40
// definitions fit exactly, and 41 yields 40 items plus the overflow item.
func TestRenderFlowItemsDefaultCap(t *testing.T) {
	defs := make([]Definition, 41)
	for i := range defs {
		defs[i] = Definition{Name: fmt.Sprintf("def%02d", i), At: fmt.Sprintf("a.ts:%d", i+1)}
	}
	find := func(Definition) (string, error) { return "nowhere else", nil }

	atCap, err := renderFlowItems(defs[:40], 0, find)
	if err != nil {
		t.Fatal(err)
	}
	if len(atCap) != 40 {
		t.Errorf("renderFlowItems(40 defs, 0) returned %d items, want 40", len(atCap))
	}
	if got, want := atCap[39], "`def39` defined or changed at a.ts:40; used at: nowhere else"; got != want {
		t.Errorf("last item at the cap = %q, want %q", got, want)
	}

	overCap, err := renderFlowItems(defs, 0, find)
	if err != nil {
		t.Fatal(err)
	}
	if len(overCap) != 41 {
		t.Fatalf("renderFlowItems(41 defs, 0) returned %d items, want 41", len(overCap))
	}
	if got, want := overCap[40], "(1 more changed definitions not listed)"; got != want {
		t.Errorf("overflow item = %q, want %q", got, want)
	}
	if !IsOverflowItem(overCap[40]) {
		t.Error("IsOverflowItem did not recognise the cap's own note")
	}
	for _, item := range overCap[:40] {
		if IsOverflowItem(item) {
			t.Errorf("IsOverflowItem recognised a definition as the overflow note: %q", item)
		}
	}
}

func TestRenderFlowItemsFinderError(t *testing.T) {
	defs := []Definition{{Name: "alphaThing", At: "a.ts:1"}}
	boom := func(Definition) (string, error) { return "", errTest }
	if _, err := renderFlowItems(defs, 0, boom); err != errTest {
		t.Errorf("renderFlowItems() error = %v, want %v", err, errTest)
	}
}
