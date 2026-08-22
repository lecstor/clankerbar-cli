package teststate

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The "MUST install this" rule in the package comment is enforced here, not
// left to whoever adds the next package remembering a doc comment. A package
// whose TEST BINARY can reach internal/config - through the package under
// test, a direct test-file import, or a test-only helper - links a binary
// that can derive and open the real loop state root; without
// TestMain(teststate.Isolate) that binary can neither isolate its tests nor
// be guarded, and a future doctor-style test leaks silently (the CLA-361
// failure shape). This test fails the suite the moment such a binary appears,
// instead of failing the operator's disk later.
//
// The check runs over `go list -json -test ./...` and inspects each
// synthesised `<pkg>.test` entry, because that is the only view whose Deps is
// the closure actually linked into a test binary. Plain per-package listings
// miss two shapes: internal/config itself never appears in its own deps or
// in-package test imports, and a test file's imports are listed directly
// only, so a helper that reaches config transitively would slip past.
func TestEnforcedEverywhereConfigIsImported(t *testing.T) {
	const configPkg = "github.com/lecstor/clankerbar-cli/internal/config"
	const teststatePkg = "github.com/lecstor/clankerbar-cli/internal/teststate"

	cmd := exec.Command("go", "list", "-json", "-test", "./...")
	cmd.Dir = moduleRoot(t) // the test binary's cwd is this package; sweep the whole module
	out, err := cmd.Output()
	if err != nil {
		var lookErr *exec.Error
		if errors.As(err, &lookErr) && errors.Is(lookErr.Err, exec.ErrNotFound) {
			t.Fatalf("go toolchain unavailable, so the isolation rule cannot be enforced: %v", err)
		}
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("go list -json -test ./... failed: %v\n%s", err, ee.Stderr)
		}
		t.Fatalf("go list -json -test ./... failed: %v", err)
	}

	var violations []string
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p struct {
			ImportPath string
			Deps       []string
		}
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("decoding go list output: %v", err)
		}
		if !strings.HasSuffix(p.ImportPath, ".test") {
			continue // not a synthesised test binary; plain entries carry no test closure
		}
		deps := make([]string, 0, len(p.Deps))
		for _, d := range p.Deps {
			// Inside a .test entry, packages built from test variants are
			// named "<import path> [<variant>]" - strip back to the path.
			if i := strings.Index(d, " ["); i >= 0 {
				d = d[:i]
			}
			deps = append(deps, d)
		}
		if contains(deps, configPkg) && !contains(deps, teststatePkg) {
			violations = append(violations, strings.TrimSuffix(p.ImportPath, ".test"))
		}
	}
	if len(violations) > 0 {
		t.Errorf("these packages' tests can reach %s but install no TestMain(teststate.Isolate); add\n\n\tfunc TestMain(m *testing.M) { os.Exit(teststate.Isolate(m)) }\n\nto each (see package internal/teststate). If an in-package TestMain closes as an import cycle, put it in the package's external _test package instead - that is what internal/config does:\n\t%s",
			configPkg, strings.Join(violations, "\n\t"))
	}
}

// moduleRoot walks up from the test binary's cwd to the directory holding
// go.mod, so `go list ./...` sees the whole module however it was invoked.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
