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
			grepOut: "a.ts\x003\x00function alphaThing() {\n",
			defAt:   "a.ts:3",
			want:    "nowhere else",
		},
		{
			name:    "body-changed definition location is dropped",
			grepOut: "a.ts\x0010\x00function alphaThing() {\n",
			defAt:   "a.ts:10 (body changed)",
			want:    "nowhere else",
		},
		{
			name:    "text after the line number may contain colons",
			grepOut: "a.ts\x004\x00alphaThing(\"x:y\")\n",
			defAt:   "a.ts:3",
			want:    "a.ts:4",
		},
		{
			name:    "path containing a colon keeps its line",
			grepOut: "src/a:b.ts\x003\x00alphaThing()\n",
			defAt:   "src/z.ts:9",
			want:    "src/a:b.ts:3",
		},
		{
			name:    "definition line in a colon path is dropped",
			grepOut: "src/a:b.ts\x003\x00alphaThing()\n",
			defAt:   "src/a:b.ts:3",
			want:    "nowhere else",
		},
		{
			name:    "test uses come last",
			grepOut: "src/a.ts\x001\x00alphaThing()\npkg/foo.test.ts\x002\x00alphaThing()\ntest/b.ts\x003\x00alphaThing()\n",
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
		lines[i] = fmt.Sprintf("src/a.ts\x00%d\x00alphaThing()", i+1)
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

// testRepo is a throwaway git repository for the integration tests.
type testRepo struct {
	t   *testing.T
	dir string
}

func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	r := &testRepo{t: t, dir: t.TempDir()}
	r.git("init", "-q")
	r.git("config", "user.email", "reviewmap@example.com")
	r.git("config", "user.name", "reviewmap test")
	r.git("config", "commit.gpgsign", "false")
	return r
}

func (r *testRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *testRepo) write(name, content string) {
	r.t.Helper()
	path := filepath.Join(r.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *testRepo) commit(message string) {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "-q", "-m", message)
}

// TestFlowMapIntegration builds a real repository, diffs two commits and lets
// FlowMap run the real git grep in the checked-out head.
func TestFlowMapIntegration(t *testing.T) {
	r := newTestRepo(t)

	r.write("src/defs.ts", "export const placeholder = 1\n")
	r.write("src/beta-uses.ts", strings.Repeat("betaThing()\n", 10))
	r.write("src/uses-a.ts", strings.Repeat("alphaThing()\n", 3))
	r.write("src/other.spec.ts", "alphaThing()\n")
	r.write("test/uses.test.ts", strings.Repeat("alphaThing()\n", 2))
	// These must never show up as uses: declarations, minified bundles and
	// files outside the searched globs.
	r.write("src/globals.d.ts", "declare function alphaThing(): void\n")
	r.write("src/vendor.min.js", "alphaThing()\n")
	r.write("docs/notes.md", "alphaThing is mentioned here\n")

	r.commit("base")
	base := r.git("rev-parse", "HEAD")

	r.write("src/defs.ts", `export const placeholder = 1

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
	r.commit("head")

	diff := r.git("diff", base, "HEAD")

	want := []string{
		"`alphaThing` defined or changed at src/defs.ts:3; used at: " +
			"src/uses-a.ts:1, src/uses-a.ts:2, src/uses-a.ts:3, " +
			"src/other.spec.ts:1, test/uses.test.ts:1, test/uses.test.ts:2",
		"`betaThing` defined or changed at src/defs.ts:7; used at: " +
			"src/beta-uses.ts:1, src/beta-uses.ts:2, src/beta-uses.ts:3, src/beta-uses.ts:4, " +
			"src/beta-uses.ts:5, src/beta-uses.ts:6, src/beta-uses.ts:7, src/beta-uses.ts:8 (+2 more)",
		"`gammaThing` defined or changed at src/defs.ts:11; used at: nowhere else",
	}

	got, err := FlowMap(diff, r.dir, 0)
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

// TestFlowMapExcludedOnlyMatch pins the exit-1 classification: when the only
// occurrence of a definition is in a file git grep excludes, that is "nowhere
// else", not an error.
func TestFlowMapExcludedOnlyMatch(t *testing.T) {
	r := newTestRepo(t)
	r.write("src/keep.ts", "export const placeholder = 1\n")
	r.commit("base")
	base := r.git("rev-parse", "HEAD")

	// TypeScript.Match accepts a .d.ts path, but useGlobs excludes it, so
	// git grep exits 1 with no output for the only occurrence.
	r.write("src/types.d.ts", "export interface OnlyHere {\n  value: string\n}\n")
	r.commit("head")

	diff := r.git("diff", base, "HEAD")
	got, err := FlowMap(diff, r.dir, 0)
	if err != nil {
		t.Fatalf("FlowMap() error: %v", err)
	}
	want := []string{"`OnlyHere` defined or changed at src/types.d.ts:1; used at: nowhere else"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FlowMap() = %#v, want %#v", got, want)
	}
}

func TestFlowMapOutsideRepository(t *testing.T) {
	diff := "+++ b/a.ts\n@@ -1 +1,2 @@\n x\n+function alphaThing() {\n"
	if _, err := FlowMap(diff, t.TempDir(), 0); err == nil {
		t.Fatal("FlowMap() error = nil, want a git grep failure outside a repository")
	}
}
