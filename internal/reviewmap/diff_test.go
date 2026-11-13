package reviewmap

import (
	"reflect"
	"testing"
)

func TestParseDiffLineNumbers(t *testing.T) {
	diff := `diff --git a/a.ts b/a.ts
index 1111111..2222222 100644
--- a/a.ts
+++ b/a.ts
@@ -1,2 +1,3 @@
 one
+two
-three
+four
`
	files := ParseDiff(diff)
	want := []FileDiff{{
		Path: "a.ts",
		Hunks: []Hunk{{
			Start: 1,
			Count: 3,
			Added: []AddedLine{
				{Line: 2, Code: "two"},
				{Line: 3, Code: "four"},
			},
		}},
	}}
	if !reflect.DeepEqual(files, want) {
		t.Errorf("ParseDiff() = %#v\nwant %#v", files, want)
	}
}

func TestParseDiffNoNewlineMarkerDoesNotAdvance(t *testing.T) {
	diff := `+++ b/a.ts
@@ -1,1 +1,1 @@
-old
\ No newline at end of file
+new
\ No newline at end of file
`
	files := ParseDiff(diff)
	want := []AddedLine{{Line: 1, Code: "new"}}
	if len(files) != 1 || len(files[0].Hunks) != 1 {
		t.Fatalf("ParseDiff() shape = %#v", files)
	}
	if got := files[0].Hunks[0].Added; !reflect.DeepEqual(got, want) {
		t.Errorf("Added = %#v, want %#v", got, want)
	}
}

func TestParseDiffMultipleFiles(t *testing.T) {
	diff := `diff --git a/a.go b/a.go
--- a/a.go
+++ b/a.go
@@ -1 +1,2 @@ first ctx
 package a
+var X = 1
diff --git a/b.go b/b.go
--- a/b.go
+++ b/b.go
@@ -5,0 +6,2 @@
+var Y = 2
+var Z = 3
`
	files := ParseDiff(diff)
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2: %#v", len(files), files)
	}
	if files[0].Path != "a.go" || files[1].Path != "b.go" {
		t.Errorf("paths = %q, %q", files[0].Path, files[1].Path)
	}
	if got := files[0].Hunks[0].Context; got != "first ctx" {
		t.Errorf("context = %q", got)
	}
	if got := files[1].Hunks[0].Added; !reflect.DeepEqual(got, []AddedLine{
		{Line: 6, Code: "var Y = 2"},
		{Line: 7, Code: "var Z = 3"},
	}) {
		t.Errorf("second file added = %#v", got)
	}
}
