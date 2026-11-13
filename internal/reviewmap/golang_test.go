package reviewmap

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// staticSource serves file content from memory, so Go.Names can be tested
// without a repository.
type staticSource struct {
	head map[string]string
	base map[string]string
}

func (s staticSource) Head(path string) ([]byte, error) { return staticRead(s.head, path) }

func (s staticSource) Base(path string) ([]byte, error) { return staticRead(s.base, path) }

func staticRead(files map[string]string, path string) ([]byte, error) {
	content, ok := files[path]
	if !ok {
		return nil, errTest
	}
	return []byte(content), nil
}

func TestGoNames(t *testing.T) {
	tests := []struct {
		name    string
		head    string
		base    string
		hunks   []Hunk
		deleted bool
		want    []Definition
	}{
		{
			name:  "new type",
			head:  "package demo\n\ntype Widget struct {\n\tName string\n}\n",
			base:  "package demo\n",
			hunks: []Hunk{{Start: 1, Count: 5}},
			want:  []Definition{{Name: "Widget", At: "a.go:3"}},
		},
		{
			name:  "body-only method change keeps the receiver",
			head:  "package demo\n\nfunc (d *driver) pickTarget(name string) error {\n\treturn nil\n}\n",
			base:  "package demo\n",
			hunks: []Hunk{{Start: 3, Count: 3}},
			want: []Definition{{
				Name: "driver.pickTarget", Grep: "pickTarget", At: "a.go:3",
			}},
		},
		{
			name:  "generic receiver",
			head:  "package demo\n\nfunc (l *List[T]) Push(v T) {\n}\n",
			base:  "package demo\n",
			hunks: []Hunk{{Start: 1, Count: 4}},
			want:  []Definition{{Name: "List.Push", Grep: "Push", At: "a.go:3"}},
		},
		{
			name:  "changed signature",
			head:  "package demo\n\nfunc LoadConfig(path string, strict bool) error {\n\treturn nil\n}\n",
			base:  "package demo\n",
			hunks: []Hunk{{Start: 1, Count: 5}},
			want:  []Definition{{Name: "LoadConfig", At: "a.go:3"}},
		},
		{
			name: "hunk without added lines still marks the declaration it covers",
			head: "package demo\n\nfunc (d *driver) pickTarget() {\n\ta := 1\n\tb := 2\n}\n",
			base: "package demo\n",
			// A deletion with context but no added line: the head range is
			// the only way this hunk can mark the function.
			hunks: []Hunk{{Start: 5, Count: 2}},
			want: []Definition{{
				Name: "driver.pickTarget", Grep: "pickTarget", At: "a.go:3",
			}},
		},
		{
			name: "exported const and var, unexported dropped",
			head: `package demo

const ExportedConst = 1
var ExportedVar = 2
var unexported = 3
`,
			base:  "package demo\n",
			hunks: []Hunk{{Start: 1, Count: 5}},
			want: []Definition{
				{Name: "ExportedConst", At: "a.go:3"},
				{Name: "ExportedVar", At: "a.go:4"},
			},
		},
		{
			name: "entry points and test functions are noise",
			head: `package demo

func init() {}
func main() {}
func TestThing() {}
func BenchmarkThing() {}
func FuzzThing() {}
func ab() {}
`,
			base:  "package demo\n",
			hunks: []Hunk{{Start: 1, Count: 8}},
		},
		{
			name: "String and Error methods are noise",
			head: `package demo

func (d *driver) String() string { return "" }
func (d *driver) Error() string { return "" }
`,
			base:  "package demo\n",
			hunks: []Hunk{{Start: 1, Count: 4}},
		},
		{
			name:  "hunk outside the span changes nothing",
			head:  "package demo\n\ntype Widget struct{}\n",
			base:  "package demo\n",
			hunks: []Hunk{{Start: 10, Count: 2}},
		},
		{
			name:  "removed exported func and type",
			head:  "package demo\n",
			base:  "package demo\n\nfunc OldThing() {}\nfunc oldThing() {}\ntype OldWidget struct{}\nconst RemovedConst = 1\n",
			hunks: []Hunk{{Start: 1, Count: 1}},
			want: []Definition{
				{Name: "OldThing", At: "a.go", Removed: true},
				{Name: "OldWidget", At: "a.go", Removed: true},
			},
		},
		{
			name:  "removed exported method",
			head:  "package demo\n",
			base:  "package demo\n\nfunc (d *driver) Close() {}\n",
			hunks: []Hunk{{Start: 1, Count: 1}},
			want: []Definition{{
				Name: "driver.Close", Grep: "Close", At: "a.go", Removed: true,
			}},
		},
		{
			name:    "deleted file reports its exported declarations",
			deleted: true,
			base:    "package demo\n\ntype Legacy struct{}\n\nfunc Unused() {}\n",
			want: []Definition{
				{Name: "Legacy", At: "a.go", Removed: true},
				{Name: "Unused", At: "a.go", Removed: true},
			},
		},
		{
			name:  "parse failure falls back to the hunk header",
			head:  "package demo\n\nfunc Broken( {\n\ta := 1\n\tb := 2\n\tc := 3\n\treturn 1\n}\n",
			base:  "package demo\n",
			hunks: []Hunk{{Start: 6, Count: 3, Context: "func Broken( {"}},
			want: []Definition{
				{Name: "Broken", At: "a.go:6 (body changed)"},
			},
		},
		{
			name:  "parse failure without a header names nothing",
			head:  "package demo\n\nfunc Broken( {\n",
			base:  "package demo\n",
			hunks: []Hunk{{Start: 3, Count: 1}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := FileDiff{Path: "a.go", Deleted: tc.deleted, Hunks: tc.hunks}
			src := staticSource{
				head: map[string]string{"a.go": tc.head},
				base: map[string]string{"a.go": tc.base},
			}
			got := Go{}.Names(f, src)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Go.Names() = %#v\nwant %#v", got, tc.want)
			}
		})
	}
}

func TestGoHeaderReceiver(t *testing.T) {
	tests := []struct {
		context string
		name    string
		grep    string
	}{
		{"func Load(path string) error {", "Load", ""},
		{"func (d *driver) pickTarget(name string) error {", "driver.pickTarget", "pickTarget"},
		{"func (l *List[T]) Push(v T) {", "List.Push", "Push"},
		{"func (d driver) Close() {", "driver.Close", "Close"},
		{"type driver struct {", "", ""},
	}
	for _, tc := range tests {
		f := FileDiff{Path: "a.go", Hunks: []Hunk{{
			Start: 4, Count: 2, Context: tc.context,
		}}}
		got := Go{}.Names(f, nil)
		switch {
		case tc.name == "" && len(got) != 0:
			t.Errorf("Names(%q) = %#v, want none", tc.context, got)
		case tc.name != "" && (len(got) != 1 || got[0].Name != tc.name || got[0].Grep != tc.grep):
			t.Errorf("Names(%q) = %#v, want %q (grep %q)", tc.context, got, tc.name, tc.grep)
		}
	}
}

// diffHead diffs base..HEAD in r with the Go-aware helper.
func diffHead(t *testing.T, r *testRepo, base string) string {
	t.Helper()
	diff, err := Diff(Range{Repo: r.dir, Base: base, Head: "HEAD"})
	if err != nil {
		t.Fatalf("Diff() error: %v", err)
	}
	return diff
}

func flowMapHead(t *testing.T, r *testRepo, base string) []string {
	t.Helper()
	items, err := FlowMap(diffHead(t, r, base), Range{Repo: r.dir, Base: base, Head: "HEAD"}, 0)
	if err != nil {
		t.Fatalf("FlowMap() error: %v", err)
	}
	return items
}

// TestGoFlowMapMethodChange covers a body-only change to a method: the head
// parse finds the declaration span, the display keeps the receiver, and the
// test use sorts after the non-test use.
func TestGoFlowMapMethodChange(t *testing.T) {
	r := newTestRepo(t)
	r.write("driver.go", "package demo\n\ntype driver struct{}\n")
	r.write("method.go", "package demo\n\nfunc (d *driver) pickTarget(name string) error {\n\treturn nil\n}\n")
	r.write("caller.go", "package demo\n\nfunc useDriver(d *driver) error {\n\treturn d.pickTarget(\"a\")\n}\n")
	r.write("z_use.go", "package demo\n\nfunc useAgain(d *driver) error {\n\treturn d.pickTarget(\"z\")\n}\n")
	// aaa_test.go sorts before caller.go, so only test-last ordering puts its
	// use after z_use.go.
	r.write("aaa_test.go", "package demo\n\nfunc helperPick() {\n\tvar d *driver\n\t_ = d.pickTarget(\"t\")\n}\n")
	r.write("caller_test.go", `package demo

import "testing"

func TestPickTarget(t *testing.T) {
	var d *driver
	_ = d.pickTarget("b")
}
`)
	r.commit("base")
	base := r.git("rev-parse", "HEAD")

	r.write("method.go", `package demo

func (d *driver) pickTarget(name string) error {
	if name == "" {
		return nil
	}
	return nil
}
`)
	r.commit("head")

	want := []string{
		"`driver.pickTarget` defined or changed at method.go:3; " +
			"used at: caller.go:4, z_use.go:4, aaa_test.go:5, caller_test.go:7",
	}
	if got := flowMapHead(t, r, base); !reflect.DeepEqual(got, want) {
		t.Errorf("FlowMap() = %#v\nwant %#v", got, want)
	}
}

func TestGoFlowMapNewType(t *testing.T) {
	r := newTestRepo(t)
	r.write("keep.go", "package demo\n\nvar placeholder = 1\n")
	r.write("use.go", "package demo\n\nvar active = 1\n")
	r.commit("base")
	base := r.git("rev-parse", "HEAD")

	r.write("widget.go", "package demo\n\ntype Widget struct {\n\tName string\n}\n")
	r.write("use.go", "package demo\n\nvar active = 1\nvar activeWidget Widget\n")
	r.commit("head")

	want := []string{
		"`Widget` defined or changed at widget.go:3; used at: use.go:4",
	}
	if got := flowMapHead(t, r, base); !reflect.DeepEqual(got, want) {
		t.Errorf("FlowMap() = %#v\nwant %#v", got, want)
	}
}

func TestGoFlowMapChangedSignature(t *testing.T) {
	r := newTestRepo(t)
	r.write("load.go", "package demo\n\nfunc LoadConfig(path string) error {\n\treturn nil\n}\n")
	r.write("uses.go", "package demo\n\nfunc run() error {\n\treturn LoadConfig(\"a\")\n}\n")
	r.commit("base")
	base := r.git("rev-parse", "HEAD")

	r.write("load.go", "package demo\n\nfunc LoadConfig(path string, strict bool) error {\n\treturn nil\n}\n")
	r.commit("head")

	want := []string{
		"`LoadConfig` defined or changed at load.go:3; used at: uses.go:4",
	}
	if got := flowMapHead(t, r, base); !reflect.DeepEqual(got, want) {
		t.Errorf("FlowMap() = %#v\nwant %#v", got, want)
	}
}

// TestGoFlowMapRemovedFunc pins the contract break this map exists for: an
// exported function the diff removes is listed only while a use survives.
func TestGoFlowMapRemovedFunc(t *testing.T) {
	r := newTestRepo(t)
	r.write("old.go", "package demo\n\nfunc OldThing() error {\n\treturn nil\n}\n\nfunc GoneThing() {}\n")
	r.write("uses.go", "package demo\n\nfunc UseOld() error {\n\treturn OldThing()\n}\n")
	r.commit("base")
	base := r.git("rev-parse", "HEAD")

	r.write("old.go", "package demo\n")
	r.commit("head")

	want := []string{
		"`OldThing` removed from old.go; still referenced at: uses.go:4",
	}
	if got := flowMapHead(t, r, base); !reflect.DeepEqual(got, want) {
		t.Errorf("FlowMap() = %#v\nwant %#v", got, want)
	}
}

func TestGoFlowMapDeletedFile(t *testing.T) {
	r := newTestRepo(t)
	r.write("legacy.go", "package demo\n\ntype Legacy struct {\n\tValue int\n}\n\nfunc Unused() {}\n")
	r.write("uses.go", "package demo\n\nvar legacyValue Legacy\n")
	r.commit("base")
	base := r.git("rev-parse", "HEAD")

	if err := os.Remove(filepath.Join(r.dir, "legacy.go")); err != nil {
		t.Fatal(err)
	}
	r.commit("head")

	want := []string{
		"`Legacy` removed from legacy.go; still referenced at: uses.go:3",
	}
	if got := flowMapHead(t, r, base); !reflect.DeepEqual(got, want) {
		t.Errorf("FlowMap() = %#v\nwant %#v", got, want)
	}
}

func TestGoFlowMapExportedConst(t *testing.T) {
	r := newTestRepo(t)
	r.write("limits.go", "package demo\n\nconst placeholder = 1\n")
	r.write("use.go", "package demo\n\nvar current = 1\n")
	r.commit("base")
	base := r.git("rev-parse", "HEAD")

	r.write("limits.go", "package demo\n\nconst MaxRetries = 3\n")
	r.write("use.go", "package demo\n\nvar current = MaxRetries\n")
	r.commit("head")

	want := []string{
		"`MaxRetries` defined or changed at limits.go:3; used at: use.go:3",
	}
	if got := flowMapHead(t, r, base); !reflect.DeepEqual(got, want) {
		t.Errorf("FlowMap() = %#v\nwant %#v", got, want)
	}
}

// TestGoFlowMapParseFallback changes a file that does not parse: the map still
// reports the enclosing function from the hunk header and does not error.
func TestGoFlowMapParseFallback(t *testing.T) {
	r := newTestRepo(t)
	r.write("broken.go", "package demo\n\nfunc Broken( {\n\ta := 1\n\tb := 2\n\tc := 3\n\td := 4\n\te := 5\n\treturn 1\n}\n")
	r.write("uses.go", "package demo\n\nfunc useBroken() {\n\tBroken()\n}\n")
	r.commit("base")
	base := r.git("rev-parse", "HEAD")

	r.write("broken.go", "package demo\n\nfunc Broken( {\n\ta := 1\n\tb := 2\n\tc := 3\n\td := 4\n\te := 5\n\treturn 2\n}\n")
	r.commit("head")

	want := []string{
		"`Broken` defined or changed at broken.go:6 (body changed); " +
			"used at: broken.go:3, uses.go:4",
	}
	if got := flowMapHead(t, r, base); !reflect.DeepEqual(got, want) {
		t.Errorf("FlowMap() = %#v\nwant %#v", got, want)
	}
}

// TestDiffGoHunkHeaders pins the reusable diff helper: Go hunks carry the
// enclosing function through a temporary attributes file, and the repository
// is left untouched.
func TestDiffGoHunkHeaders(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.go", `package demo

type driver struct{}

func (d *driver) pickTarget(name string) error {
	return nil
	return nil
	return nil
	return nil
	return nil
	return nil
	return nil
}
`)
	r.commit("base")
	base := r.git("rev-parse", "HEAD")

	r.write("a.go", `package demo

type driver struct{}

func (d *driver) pickTarget(name string) error {
	return nil
	return nil
	return nil
	return nil
	return nil
	return ErrNope
	return nil
}
`)
	r.commit("head")

	diff := diffHead(t, r, base)
	var header string
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "@@ ") {
			header = line
			break
		}
	}
	if !strings.Contains(header, "func (d *driver) pickTarget(name string) error {") {
		t.Errorf("hunk header = %q, want the enclosing Go func", header)
	}
	if change := ChangeMap(diff, 0); len(change) == 0 ||
		!strings.Contains(change[0], "(in: func (d *driver) pickTarget(") {
		t.Errorf("ChangeMap() = %#v, want the Go func context on the item", change)
	}

	if status := r.git("status", "--porcelain"); status != "" {
		t.Errorf("Diff() changed the repository: %q", status)
	}
	if _, err := os.Stat(filepath.Join(r.dir, ".gitattributes")); !os.IsNotExist(err) {
		t.Errorf(".gitattributes exists after Diff(): %v", err)
	}
}

// TestDiffBadRevision pins the error path: an unknown revision fails with
// git's own message rather than an empty diff.
func TestDiffBadRevision(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.go", "package demo\n")
	r.commit("base")

	if _, err := Diff(Range{Repo: r.dir, Base: "no-such-revision", Head: "HEAD"}); err == nil {
		t.Fatal("Diff() error = nil, want a bad-revision failure")
	}
}

// TestGoAttributesFile pins the attributes content the helper passes with -c.
func TestGoAttributesFile(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.go", "package demo\n")
	r.commit("base")

	name, cleanup, err := goAttributesFile()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	content, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != goAttributes {
		t.Errorf("attributes file = %q, want %q", content, goAttributes)
	}

	got := r.git("-c", "core.attributesFile="+name, "check-attr", "diff", "--", "a.go")
	if want := "a.go: diff: golang"; got != want {
		t.Errorf("check-attr = %q, want %q", got, want)
	}
}

// TestDiffRepoAttributesWin pins precedence: a repository's own .gitattributes
// still names the driver where it sets one.
func TestDiffRepoAttributesWin(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.go", "package demo\n")
	r.write(".gitattributes", "*.go diff=custom\n")
	r.commit("base")

	name, cleanup, err := goAttributesFile()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	got := r.git("-c", "core.attributesFile="+name, "check-attr", "diff", "--", "a.go")
	if want := "a.go: diff: custom"; got != want {
		t.Errorf("check-attr = %q, want %q (repo attributes must win)", got, want)
	}
}
