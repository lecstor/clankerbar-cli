package config

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

const embedTestHunk = `@@ -10,3 +10,3 @@
 export function alphaThing(x: number) {
-  return x + 1
+  return x + 2
 }`

func embedTestInput() (items []string, hunks []MapsSessionHunk) {
	return []string{
			"M1. src/a.ts:10-12",
			"M2. src/b.ts:1-2",
			"F1. `alphaThing` defined or changed at src/a.ts:10; used at: nowhere else",
		},
		[]MapsSessionHunk{
			{Path: "src/a.ts", Text: embedTestHunk},
			{Path: "src/b.ts", Text: "@@ -1,2 +1,2 @@\n one\n-two\n+three"},
		}
}

// TestEmbedMapsSessionHunks_Format (CLA-871): each M item is followed by its
// hunk in a diff-fenced block, the fence is closed, and the F item is untouched.
func TestEmbedMapsSessionHunks_Format(t *testing.T) {
	items, hunks := embedTestInput()
	out, cutoff := EmbedMapsSessionHunks(items, hunks, "abc1234", &EmbedHunksOptions{Enabled: true})
	if cutoff != -1 {
		t.Fatalf("cutoff = %d, want -1 (both hunks fit the defaults)", cutoff)
	}
	want := items[0] + "\n\n```diff\n" + embedTestHunk + "\n```"
	if out[0] != want {
		t.Errorf("M1 = %q\nwant %q", out[0], want)
	}
	if !strings.Contains(out[1], "\n\n```diff\n@@ -1,2 +1,2 @@") || !strings.HasSuffix(out[1], "\n```") {
		t.Errorf("M2 is not followed by its fenced hunk:\n%s", out[1])
	}
	if out[2] != items[2] {
		t.Errorf("an F item was modified: %q", out[2])
	}
	if len(out) != len(items) {
		t.Errorf("items = %d, want %d", len(out), len(items))
	}
}

// TestEmbedMapsSessionHunks_DisabledIsByteIdentical (CLA-871): no block, an
// explicit disabled block and an empty one all leave the items exactly as the
// single-pass map made them.
func TestEmbedMapsSessionHunks_DisabledIsByteIdentical(t *testing.T) {
	items, hunks := embedTestInput()
	for _, tc := range []struct {
		name string
		opts *EmbedHunksOptions
	}{
		{name: "nil"},
		{name: "disabled", opts: &EmbedHunksOptions{Enabled: false}},
		{name: "disabled with budgets", opts: &EmbedHunksOptions{MaxBytesPerItem: 10, MaxBytesTotal: 10}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, cutoff := EmbedMapsSessionHunks(items, hunks, "abc1234", tc.opts)
			if cutoff != -1 {
				t.Errorf("cutoff = %d, want -1 when embedding is off", cutoff)
			}
			if !reflect.DeepEqual(out, items) {
				t.Errorf("items changed with embedding off:\ngot  %#v\nwant %#v", out, items)
			}
		})
	}
}

// TestEmbedMapsSessionHunks_PerItemTruncation (CLA-871): a hunk over the
// per-item budget is cut at a line boundary and a marker names the exact git
// command for the rest.
func TestEmbedMapsSessionHunks_PerItemTruncation(t *testing.T) {
	long := "@@ -1,9 +1,9 @@\n+line 1\n+line 2\n+line 3\n+line 4\n+line 5"
	items := []string{"M1. src/a.ts:1-9"}
	hunks := []MapsSessionHunk{{Path: "src/a.ts", Text: long}}
	out, cutoff := EmbedMapsSessionHunks(items, hunks, "abc1234",
		&EmbedHunksOptions{Enabled: true, MaxBytesPerItem: 25, MaxBytesTotal: 100000})
	if cutoff != -1 {
		t.Fatalf("cutoff = %d, want -1 (one item, total budget ample)", cutoff)
	}
	if !strings.Contains(out[0], "(hunk truncated at 25 bytes: run `git diff abc1234..HEAD -- src/a.ts` for the rest)") {
		t.Errorf("the truncation marker does not name the exact git command:\n%s", out[0])
	}
	body := embedBody(t, out[0])
	if len(body) > 25 {
		t.Errorf("embedded hunk is %d bytes, over the 25-byte budget:\n%s", len(body), body)
	}
	if strings.Contains(body, "+line 5") {
		t.Errorf("the budget cut did not drop the hunk's tail:\n%s", body)
	}
	// The cut is on a line boundary: every embedded line is one the hunk had.
	for _, line := range strings.Split(body, "\n") {
		if !strings.Contains(long, line) {
			t.Errorf("truncation left a partial line %q in the block", line)
		}
	}
}

// TestEmbedMapsSessionHunks_TotalCutoff (CLA-871): once the next hunk would
// pass the total budget, that item and every later one keep their plain text and
// the returned cutoff names where embedding stopped.
func TestEmbedMapsSessionHunks_TotalCutoff(t *testing.T) {
	items, hunks := embedTestInput()
	// Exactly the first hunk's bytes: the second cannot fit without passing it.
	budget := len(hunks[0].Text)
	out, cutoff := EmbedMapsSessionHunks(items, hunks, "abc1234",
		&EmbedHunksOptions{Enabled: true, MaxBytesPerItem: DefaultMapsSessionHunkBytesPerItem, MaxBytesTotal: budget})
	if cutoff != 1 {
		t.Fatalf("cutoff = %d, want 1 (the second M item is the first not embedded)", cutoff)
	}
	if !strings.Contains(out[0], "```diff") {
		t.Errorf("the first hunk was not embedded though it fits:\n%s", out[0])
	}
	if out[1] != items[1] {
		t.Errorf("the over-budget item was modified:\n%s", out[1])
	}
	if out[2] != items[2] {
		t.Errorf("an item after the cutoff was modified:\n%s", out[2])
	}
}

// TestEmbedMapsSessionHunks_FenceOutrunsBackticks (CLA-871): a hunk containing a
// backtick run opens a fence longer than the longest run, so it cannot close its
// own block.
func TestEmbedMapsSessionHunks_FenceOutrunsBackticks(t *testing.T) {
	hunk := "@@ -1 +1 @@\n-const before = 1\n+const fence = ``````"
	items := []string{"M1. src/a.ts:1-1"}
	out, _ := EmbedMapsSessionHunks(items, []MapsSessionHunk{{Path: "src/a.ts", Text: hunk}}, "abc1234",
		&EmbedHunksOptions{Enabled: true})
	// The hunk's longest run is 6 backticks, so the fence is 7.
	if !strings.Contains(out[0], "\n\n```````diff\n") {
		t.Errorf("the fence does not outrun the hunk's backticks:\n%s", out[0])
	}
	if !strings.HasSuffix(out[0], "\n```````") {
		t.Errorf("the closing fence is not the opening one:\n%s", out[0])
	}
	if !strings.Contains(out[0], "+const fence = ``````") {
		// The hunk line itself must survive inside the block.
		t.Errorf("the backtick content was altered:\n%s", out[0])
	}
}

// TestEmbedMapsSessionHunks_DoneMarkerEscaped (CLA-871): a hunk line whose
// content is exactly the done marker is embedded escaped, so neither copying the
// line whole nor stripping its diff prefix yields a valid marker.
func TestEmbedMapsSessionHunks_DoneMarkerEscaped(t *testing.T) {
	hunk := "@@ -1,2 +1,2 @@\n context\n+" + DoneMarker
	items := []string{"M1. src/a.ts:1-2"}
	out, _ := EmbedMapsSessionHunks(items, []MapsSessionHunk{{Path: "src/a.ts", Text: hunk}}, "abc1234",
		&EmbedHunksOptions{Enabled: true})
	if !strings.Contains(out[0], "\n+\\"+DoneMarker) {
		t.Errorf("the done-marker line was not escaped:\n%s", out[0])
	}
	for _, line := range strings.Split(embedBody(t, out[0]), "\n") {
		if strings.TrimRight(strings.TrimLeft(line, "+- "), " \t\r") == DoneMarker {
			t.Errorf("an embedded hunk line still reads as the done marker: %q", line)
		}
		if strings.HasPrefix(line, "+"+DoneMarker) {
			t.Errorf("a copied added line would still be the done marker: %q", line)
		}
	}
}

// TestEmbedMapsSessionHunks_EdgeCases covers empty hunks, UTF-8 truncation and
// the missing-base placeholder.
func TestEmbedMapsSessionHunks_EdgeCases(t *testing.T) {
	t.Run("empty hunk text is not embedded", func(t *testing.T) {
		items := []string{"M1. a", "M2. b"}
		out, cutoff := EmbedMapsSessionHunks(items, []MapsSessionHunk{{Path: "a"}, {Path: "b"}}, "",
			&EmbedHunksOptions{Enabled: true})
		if !reflect.DeepEqual(out, items) || cutoff != -1 {
			t.Errorf("an empty hunk was embedded: cutoff %d, items %#v", cutoff, out)
		}
	})
	t.Run("truncation never splits a rune", func(t *testing.T) {
		hunk := "@@ -1 +1 @@\n+世界世界世界世界"
		out, _ := EmbedMapsSessionHunks([]string{"M1. a"}, []MapsSessionHunk{{Path: "a", Text: hunk}}, "",
			&EmbedHunksOptions{Enabled: true, MaxBytesPerItem: 20, MaxBytesTotal: 1000})
		body := embedBody(t, out[0])
		if !utf8.ValidString(body) {
			t.Errorf("truncation split a rune: %q", body)
		}
	})
	t.Run("unknown base keeps the placeholder", func(t *testing.T) {
		marker := MapsSessionHunkTruncatedMarker("", "a.ts", 10)
		if !strings.Contains(marker, "`git diff <mergeBase>..HEAD -- a.ts`") {
			t.Errorf("marker = %q, want the placeholder base", marker)
		}
	})
	t.Run("path with a space is quoted", func(t *testing.T) {
		marker := MapsSessionHunkTruncatedMarker("abc", "dir/a b.ts", 10)
		if !strings.Contains(marker, "`git diff abc..HEAD -- 'dir/a b.ts'`") {
			t.Errorf("marker = %q, want the path single-quoted", marker)
		}
	})
}

// TestMapsSessionHunkBudgetNote (CLA-871): the one-time note names the budget
// the next hunk would pass and the command for the items left plain.
func TestMapsSessionHunkBudgetNote(t *testing.T) {
	note := MapsSessionHunkBudgetNote("abc1234", DefaultMapsSessionHunkBytesTotal)
	for _, want := range []string{"Hunk embedding stopped", "the next hunk would", "61440-byte per-review budget", "from here on", "`git diff abc1234..HEAD -- <path>`"} {
		if !strings.Contains(note, want) {
			t.Errorf("note does not carry %q: %s", want, note)
		}
	}
	if strings.Contains(note, "is spent") {
		t.Errorf("the note claims the whole budget was used: %s", note)
	}
}

// embedBody returns the text between a block's opening diff fence and its
// closing fence.
func embedBody(t *testing.T, block string) string {
	t.Helper()
	open := strings.Index(block, "```diff\n")
	if open < 0 {
		t.Fatalf("no diff fence in %q", block)
	}
	rest := block[open+len("```diff\n"):]
	close := strings.LastIndex(rest, "\n```")
	if close < 0 {
		t.Fatalf("no closing fence in %q", block)
	}
	return rest[:close]
}
