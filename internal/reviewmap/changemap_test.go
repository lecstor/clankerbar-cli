package reviewmap

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestChangeMap(t *testing.T) {
	longCtx := strings.Repeat("x", 90)

	tests := []struct {
		name     string
		diff     string
		maxItems int
		want     []string
	}{
		{
			name: "single added line",
			diff: `diff --git a/foo.ts b/foo.ts
--- a/foo.ts
+++ b/foo.ts
@@ -3 +3,2 @@
 export const a = 1
+export const b = 2
`,
			want: []string{"foo.ts:3-4"},
		},
		{
			name: "multi-line hunk with context",
			diff: `diff --git a/src/app.ts b/src/app.ts
--- a/src/app.ts
+++ b/src/app.ts
@@ -10,5 +10,7 @@ export function handler() {
`,
			want: []string{"src/app.ts:10-16  (in: export function handler() {)"},
		},
		{
			name: "deleted-only hunk",
			diff: `+++ b/src/app.ts
@@ -5,3 +4,0 @@
-old one
-old two
-old three
`,
			want: []string{"src/app.ts (lines removed near line 4)"},
		},
		{
			name: "missing count means one line",
			diff: `+++ b/src/app.ts
@@ -7 +7 @@
-old
+new
`,
			want: []string{"src/app.ts:7-7"},
		},
		{
			name: "ctx truncated to 80 runes",
			diff: "+++ b/app.ts\n@@ -1,2 +1,3 @@ " + longCtx + "\n",
			want: []string{"app.ts:1-3  (in: " + strings.Repeat("x", 80) + ")"},
		},
		{
			name: "dev null file is skipped without stealing the previous file",
			diff: `+++ b/keep.ts
@@ -1 +1,2 @@
 a
+b
+++ /dev/null
@@ -1,3 +0,0 @@
-a
-b
-c
`,
			want: []string{"keep.ts:1-2"},
		},
		{
			name: "above the cap hunks merge per file",
			diff: `+++ b/a.ts
@@ -10,2 +10,3 @@
 x
+y
@@ -40,2 +40,3 @@ ctx
 x
+y
+++ b/b.ts
@@ -1 +1 @@
-x
+y
`,
			maxItems: 2,
			want: []string{
				"a.ts: 10-12, 40-42 (2 changed regions)",
				"b.ts: 1-1 (1 changed regions)",
			},
		},
		{
			name: "merged deleted-only region renders as a point",
			diff: `+++ b/a.ts
@@ -1 +1,2 @@
 x
+y
@@ -5,3 +6,0 @@
-a
-b
-c
`,
			maxItems: 1,
			want:     []string{"a.ts: 1-2, 6-6 (2 changed regions)"},
		},
		{
			name: "unlimited keeps every hunk",
			diff: `+++ b/a.ts
@@ -10,2 +10,3 @@
 x
+y
@@ -40,2 +40,3 @@ ctx
 x
+y
+++ b/b.ts
@@ -1 +1 @@
-x
+y
`,
			maxItems: Unlimited,
			want: []string{
				"a.ts:10-12",
				"a.ts:40-42  (in: ctx)",
				"b.ts:1-1",
			},
		},
		{
			name: "no hunks yields no items",
			diff: "diff --git a/a.ts b/a.ts\n--- a/a.ts\n+++ b/a.ts\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ChangeMap(tc.diff, tc.maxItems)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ChangeMap() = %#v\nwant %#v", got, tc.want)
			}
		})
	}
}

// hunksDiff builds a one-file diff with n hunks, each adding a line at
// start+1.
func hunksDiff(n int) string {
	var b strings.Builder
	b.WriteString("+++ b/a.ts\n")
	for i := range n {
		start := 1 + i*3
		fmt.Fprintf(&b, "@@ -%d,1 +%d,2 @@\n x\n+y\n", start, start)
	}
	return b.String()
}

// TestChangeMapDefaultCap pins the benchmark default at 60 hunks: 60 keep one
// item each, 61 merge per file.
func TestChangeMapDefaultCap(t *testing.T) {
	if got := ChangeMap(hunksDiff(60), 0); len(got) != 60 {
		t.Errorf("ChangeMap(60 hunks, 0) returned %d items, want 60", len(got))
	}

	var regions []string
	for i := range 61 {
		start := 1 + i*3
		regions = append(regions, fmt.Sprintf("%d-%d", start, start+1))
	}
	want := []string{fmt.Sprintf("a.ts: %s (61 changed regions)", strings.Join(regions, ", "))}
	if got := ChangeMap(hunksDiff(61), 0); !reflect.DeepEqual(got, want) {
		t.Errorf("ChangeMap(61 hunks, 0) = %#v\nwant %#v", got, want)
	}

	// An explicit cap above the hunk count keeps every hunk.
	if got := ChangeMap(hunksDiff(61), 61); len(got) != 61 {
		t.Errorf("ChangeMap(61 hunks, 61) returned %d items, want 61", len(got))
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("héllo", 4); got != "héll" {
		t.Errorf("truncate(runes) = %q, want %q", got, "héll")
	}
	if got := truncate("abc", 80); got != "abc" {
		t.Errorf("truncate(short) = %q", got)
	}
}
