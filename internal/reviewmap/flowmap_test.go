package reviewmap

import (
	"errors"
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
	if got := changedDefinitions(files); !reflect.DeepEqual(got, want) {
		t.Errorf("changedDefinitions() = %#v, want %#v", got, want)
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

func TestRenderFlowItemsFinderError(t *testing.T) {
	defs := []Definition{{Name: "alphaThing", At: "a.ts:1"}}
	boom := func(Definition) (string, error) { return "", errTest }
	if _, err := renderFlowItems(defs, 0, boom); err != errTest {
		t.Errorf("renderFlowItems() error = %v, want %v", err, errTest)
	}
}
