package teststate

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The "MUST install this" rule in the package comment is enforced here, not
// left to whoever adds the next package remembering a doc comment. A package
// whose tests can reach internal/config - directly or through anything it
// imports - compiles those tests into a binary that can derive and open the
// real loop state root; without TestMain(teststate.Isolate) that binary can
// neither isolate its tests nor be guarded, and a future doctor-style test
// leaks silently (the CLA-361 failure shape). This test fails the suite the
// moment such a package appears, instead of failing the operator's disk later.
func TestEnforcedEverywhereConfigIsImported(t *testing.T) {
	const configPkg = "github.com/lecstor/clankerbar-cli/internal/config"
	const teststatePkg = "github.com/lecstor/clankerbar-cli/internal/teststate"

	cmd := exec.Command("go", "list", "-json", "./...")
	cmd.Dir = moduleRoot(t) // the test binary's cwd is this package; sweep the whole module
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("go list -json ./... failed: %v\n%s", err, ee.Stderr)
		}
		t.Fatalf("go list -json ./... failed: %v", err)
	}

	var violations []string
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p struct {
			ImportPath   string
			Deps         []string
			TestGoFiles  []string
			XTestGoFiles []string
			TestImports  []string
			XTestImports []string
		}
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("decoding go list output: %v", err)
		}
		if p.ImportPath == teststatePkg {
			continue // the mechanism itself; its own tests need no import of it
		}
		if len(p.TestGoFiles) == 0 && len(p.XTestGoFiles) == 0 {
			continue // no test binary, nothing to isolate or guard
		}
		reaches := contains(p.Deps, configPkg) ||
			contains(p.TestImports, configPkg) ||
			contains(p.XTestImports, configPkg)
		if !reaches {
			continue
		}
		installs := contains(p.TestImports, teststatePkg) ||
			contains(p.XTestImports, teststatePkg)
		if !installs {
			violations = append(violations, p.ImportPath)
		}
	}
	if len(violations) > 0 {
		t.Errorf("these packages' tests can reach %s but install no TestMain(teststate.Isolate); add\n\n\tfunc TestMain(m *testing.M) { os.Exit(teststate.Isolate(m)) }\n\nto each (see package internal/teststate):\n\t%s",
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
