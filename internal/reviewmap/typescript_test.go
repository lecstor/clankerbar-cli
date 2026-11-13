package reviewmap

import (
	"reflect"
	"testing"
)

func addedFile(path string, hunkStart int, lines ...AddedLine) FileDiff {
	return FileDiff{Path: path, Hunks: []Hunk{{Start: hunkStart, Count: len(lines), Added: lines}}}
}

func tsNames(f FileDiff) []Definition { return TypeScript{}.Names(f) }

func namesOf(defs []Definition) []string {
	names := make([]string, len(defs))
	for i, d := range defs {
		names[i] = d.Name
	}
	return names
}

func TestTypeScriptPatterns(t *testing.T) {
	tests := []struct {
		name string
		code string
		want []string
	}{
		{"function declaration", "export function alphaThing() {", []string{"alphaThing"}},
		{"generator function", "function* betaGen(", []string{"betaGen"}},
		{"arrow with parenthesised params", "const alphaThing = (a: number) => a", []string{"alphaThing"}},
		{"arrow typed both sides", "let betaThing: Handler = async (a: string): Promise<void> =>", []string{"betaThing"}},
		{"arrow bare parameter", "var gammaThing = x => x", []string{"gammaThing"}},
		{"function expression", "const alphaThing = async function () {", []string{"alphaThing"}},
		{"interface", "export interface AlphaThing {", []string{"AlphaThing"}},
		{"type alias", "type BetaThing = string", []string{"BetaThing"}},
		{"class", "export class GammaThing {", []string{"GammaThing"}},
		{"enum", "enum DeltaThing {", []string{"DeltaThing"}},
		{"method with modifiers", "  public alphaThing(a: string): void {", []string{"alphaThing"}},
		{"generic method", "  betaThing<T>(a: T): void {", []string{"betaThing"}},
		{"plain assignment is not a definition", "const alphaThing = 1", nil},
		{"plain call is not a definition", "alphaThing()", nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := FileDiff{Path: "src/a.ts", Hunks: []Hunk{{
				Start: 4,
				Count: 1,
				Added: []AddedLine{{Line: 4, Code: tc.code}},
			}}}
			got := namesOf(tsNames(f))
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Names(%q) = %v, want %v", tc.code, got, tc.want)
			}
		})
	}
}

func TestTypeScriptSkipSet(t *testing.T) {
	// Every name the benchmark's skip set lists (CLA-864), so a dropped or
	// misspelled entry fails.
	skipNames := []string{
		"constructor", "render", "default", "props", "state", "index", "main",
		"init", "setup", "test", "it", "describe", "expect", "then", "catch",
		"map", "filter", "reduce", "forEach", "push", "get", "set", "has",
		"toString", "value", "data", "error", "result", "options", "config",
		"handler", "callback", "fn", "cb", "if", "for", "while", "switch",
		"return", "function",
	}
	for _, name := range skipNames {
		code := "function " + name + "() {"
		f := addedFile("src/a.ts", 1, AddedLine{Line: 1, Code: code})
		if got := tsNames(f); len(got) != 0 {
			t.Errorf("Names(%q) = %v, want none (skip set)", code, namesOf(got))
		}
	}

	// Control: a name outside the set is kept.
	f := addedFile("src/a.ts", 1, AddedLine{Line: 1, Code: "function keptThing() {"})
	if got := namesOf(tsNames(f)); !reflect.DeepEqual(got, []string{"keptThing"}) {
		t.Errorf("Names(control) = %v, want [keptThing]", got)
	}
}

func TestTypeScriptMinLength(t *testing.T) {
	for _, code := range []string{"function ab() {", "const xy = () => 1", "class Ab {"} {
		f := addedFile("src/a.ts", 1, AddedLine{Line: 1, Code: code})
		if got := tsNames(f); len(got) != 0 {
			t.Errorf("Names(%q) = %v, want none (min length)", code, namesOf(got))
		}
	}
	f := addedFile("src/a.ts", 1, AddedLine{Line: 1, Code: "function abc() {"})
	if got := namesOf(tsNames(f)); !reflect.DeepEqual(got, []string{"abc"}) {
		t.Errorf("Names(three-char name) = %v, want [abc]", got)
	}
}

func TestTypeScriptFirstOccurrenceWins(t *testing.T) {
	f := FileDiff{Path: "src/a.ts", Hunks: []Hunk{{
		Start: 2,
		Count: 8,
		Added: []AddedLine{
			{Line: 2, Code: "function alphaThing() {"},
			{Line: 9, Code: "function alphaThing() {"},
		},
	}}}
	want := []Definition{{Name: "alphaThing", At: "src/a.ts:2"}}
	if got := tsNames(f); !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %#v, want %#v", got, want)
	}
}

func TestTypeScriptHeaderContext(t *testing.T) {
	f := FileDiff{Path: "src/a.ts", Hunks: []Hunk{{
		Start:   10,
		Count:   3,
		Context: "export function alphaThing() {",
		Added:   []AddedLine{{Line: 11, Code: "  return 2"}},
	}}}
	want := []Definition{{Name: "alphaThing", At: "src/a.ts:10 (body changed)"}}
	if got := tsNames(f); !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %#v, want %#v", got, want)
	}
}

func TestTypeScriptAddedLineBeatsHeaderContext(t *testing.T) {
	f := FileDiff{Path: "src/a.ts", Hunks: []Hunk{{
		Start:   10,
		Count:   1,
		Context: "function alphaThing() {",
		Added:   []AddedLine{{Line: 10, Code: "function alphaThing() {"}},
	}}}
	want := []Definition{{Name: "alphaThing", At: "src/a.ts:10"}}
	if got := tsNames(f); !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %#v, want %#v", got, want)
	}
}

func TestExtractorSelection(t *testing.T) {
	ts := TypeScript{}
	for _, path := range []string{"a.ts", "a.tsx", "a.js", "a.jsx", "a.mjs", "a.cjs", "a.mts", "a.cts.ts"} {
		if !ts.Match(path) {
			t.Errorf("TypeScript.Match(%q) = false, want true", path)
		}
	}
	for _, path := range []string{"a.go", "a.json", "a.css", "a.tsx.md"} {
		if ts.Match(path) {
			t.Errorf("TypeScript.Match(%q) = true, want false", path)
		}
	}

	if _, ok := extractorFor("a.ts").(TypeScript); !ok {
		t.Errorf("extractorFor(a.ts) = %T, want TypeScript", extractorFor("a.ts"))
	}
	if _, ok := extractorFor("a.go").(noopExtractor); !ok {
		t.Errorf("extractorFor(a.go) = %T, want noopExtractor", extractorFor("a.go"))
	}
	if got := extractorFor("a.go").Names(FileDiff{Path: "a.go"}); len(got) != 0 {
		t.Errorf("noop extractor returned %#v", got)
	}
}
