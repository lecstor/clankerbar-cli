package reviewmap

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestFormatUses(t *testing.T) {
	tests := []struct {
		name    string
		grepOut string
		defAt   string
		want    string
	}{
		{
			name:  "no output",
			defAt: "a.ts:1",
			want:  "nowhere else",
		},
		{
			name:    "definition line is dropped",
			grepOut: "a.ts:3:function alphaThing() {\n",
			defAt:   "a.ts:3",
			want:    "nowhere else",
		},
		{
			name:    "body-changed definition location is dropped",
			grepOut: "a.ts:10:function alphaThing() {\n",
			defAt:   "a.ts:10 (body changed)",
			want:    "nowhere else",
		},
		{
			name:    "text after the line number may contain colons",
			grepOut: "a.ts:4:alphaThing(\"x:y\")\n",
			defAt:   "a.ts:3",
			want:    "a.ts:4",
		},
		{
			name:    "test uses come last",
			grepOut: "src/a.ts:1:alphaThing()\npkg/foo.test.ts:2:alphaThing()\ntest/b.ts:3:alphaThing()\n",
			defAt:   "src/z.ts:9",
			want:    "src/a.ts:1, pkg/foo.test.ts:2, test/b.ts:3",
		},
		{
			name:    "eight uses then a count",
			grepOut: strings.Join(useLines(10), "\n") + "\n",
			defAt:   "src/z.ts:9",
			want:    strings.Join(useLocations(8), ", ") + " (+2 more)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatUses(tc.grepOut, tc.defAt); got != tc.want {
				t.Errorf("formatUses() = %q, want %q", got, tc.want)
			}
		})
	}
}

func useLines(n int) []string {
	lines := make([]string, n)
	for i := range n {
		lines[i] = fmt.Sprintf("src/a.ts:%d:alphaThing()", i+1)
	}
	return lines
}

func useLocations(n int) []string {
	locs := make([]string, n)
	for i := range n {
		locs[i] = fmt.Sprintf("src/a.ts:%d", i+1)
	}
	return locs
}

func TestIsTestPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"src/tests/a.ts", true},
		{"tests/a.ts", true},
		{"src/test/a.ts", true},
		{"src/__tests__/a.ts", true},
		{"src/e2e/a.ts", true},
		{"src/spec/a.ts", true},
		{"src/a.test.ts", true},
		{"src/a.spec.tsx", true},
		{"src/a.ts", false},
		{"src/contest/a.ts", false},
		{"src/spectrum/a.ts", false},
	}
	for _, tc := range tests {
		if got := isTestPath(tc.path); got != tc.want {
			t.Errorf("isTestPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestFlowMapIntegration builds a real repository, diffs two commits and lets
// FlowMap run the real git grep in the checked-out head.
func TestFlowMapIntegration(t *testing.T) {
	dir := t.TempDir()

	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, content string) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	git("init", "-q")
	git("config", "user.email", "reviewmap@example.com")
	git("config", "user.name", "reviewmap test")
	git("config", "commit.gpgsign", "false")

	write("src/defs.ts", "export const placeholder = 1\n")
	write("src/beta-uses.ts", strings.Repeat("betaThing()\n", 10))
	write("src/uses-a.ts", strings.Repeat("alphaThing()\n", 3))
	write("src/other.spec.ts", "alphaThing()\n")
	write("test/uses.test.ts", strings.Repeat("alphaThing()\n", 2))
	// These must never show up as uses: declarations, minified bundles and
	// files outside the searched globs.
	write("src/globals.d.ts", "declare function alphaThing(): void\n")
	write("src/vendor.min.js", "alphaThing()\n")
	write("docs/notes.md", "alphaThing is mentioned here\n")

	git("add", "-A")
	git("commit", "-q", "-m", "base")
	base := git("rev-parse", "HEAD")

	write("src/defs.ts", `export const placeholder = 1

export function alphaThing() {
  return 1
}

export function betaThing() {
  return 2
}

export function gammaThing() {
  return 3
}
`)
	git("add", "-A")
	git("commit", "-q", "-m", "head")

	diff := git("diff", base, "HEAD")

	want := []string{
		"`alphaThing` defined or changed at src/defs.ts:3; used at: " +
			"src/uses-a.ts:1, src/uses-a.ts:2, src/uses-a.ts:3, " +
			"src/other.spec.ts:1, test/uses.test.ts:1, test/uses.test.ts:2",
		"`betaThing` defined or changed at src/defs.ts:7; used at: " +
			"src/beta-uses.ts:1, src/beta-uses.ts:2, src/beta-uses.ts:3, src/beta-uses.ts:4, " +
			"src/beta-uses.ts:5, src/beta-uses.ts:6, src/beta-uses.ts:7, src/beta-uses.ts:8 (+2 more)",
		"`gammaThing` defined or changed at src/defs.ts:11; used at: nowhere else",
	}

	got, err := FlowMap(diff, dir, 0)
	if err != nil {
		t.Fatalf("FlowMap() error: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FlowMap() =\n%#v\nwant\n%#v", got, want)
	}

	changeWant := []string{"src/defs.ts:1-13"}
	if changeGot := ChangeMap(diff, 0); !reflect.DeepEqual(changeGot, changeWant) {
		t.Errorf("ChangeMap(real diff) = %#v, want %#v", changeGot, changeWant)
	}
}

func TestFlowMapOutsideRepository(t *testing.T) {
	diff := "+++ b/a.ts\n@@ -1 +1,2 @@\n x\n+function alphaThing() {\n"
	if _, err := FlowMap(diff, t.TempDir(), 0); err == nil {
		t.Fatal("FlowMap() error = nil, want a git grep failure outside a repository")
	}
}
