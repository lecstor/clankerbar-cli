// Package teststate keeps `go test` out of the operator's real loop state dir.
//
// CLA-361: test fixtures whose workdir is a t.TempDir() leaf derive a state
// dir (config.ResolveStateDir) from it, and anything that opens that dir -
// doctor's state_dir check, the loop preflight - really creates
// <real-state-home>/clankerbar/loop/001-<hash> on the operator's machine.
// Six hundred and eighty of those accumulated before anyone looked. The
// per-test fix does not hold: any current or future test that reaches the
// derivation without setting an override leaks again, silently.
//
// So isolation is binary-wide and structural. Isolate is called from
// TestMain; it points XDG_STATE_HOME at a fresh temporary directory for every
// test in the binary (stateHome honours it, spawned subprocesses inherit it,
// and a per-test t.Setenv still overrides it where a test wants its own), and
// afterwards compares the REAL root against a snapshot taken before the run,
// failing the binary if the run added anything there. A leak therefore cannot
// return quietly: it turns into a red suite naming the directories created.
//
// Every package whose tests can reach config.ResolveStateDir or
// config.StateRoot MUST install this:
//
//	func TestMain(m *testing.M) { os.Exit(teststate.Isolate(m)) }
//
// A package without it can neither isolate its tests nor be guarded by this
// guard; add it before adding a test that derives a state dir. Today that is
// internal/config, internal/cli and internal/loop - the only importers of
// internal/config, which owns the derivation.
package teststate

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// Isolate runs m as an isolated test binary and returns its exit code, forced
// non-zero if the post-run guard finds new entries under the real loop state
// root. Call it from TestMain and os.Exit the result:
//
//	func TestMain(m *testing.M) { os.Exit(teststate.Isolate(m)) }
func Isolate(m *testing.M) int {
	// The real root must be resolved BEFORE the environment is overridden:
	// config.StateRoot reads the live process environment, and after Setenv it
	// would answer with the isolated directory instead.
	real, err := config.StateRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "teststate: cannot resolve the real loop state root:", err)
		return 1
	}
	before := readDirNames(real)

	iso, err := os.MkdirTemp("", "clankerbar-test-state-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "teststate: cannot create an isolated state home:", err)
		return 1
	}
	defer os.RemoveAll(iso)
	if err := os.Setenv("XDG_STATE_HOME", iso); err != nil {
		fmt.Fprintln(os.Stderr, "teststate: cannot point XDG_STATE_HOME at the isolated home:", err)
		return 1
	}

	code := m.Run()

	if added := addedNames(before, readDirNames(real)); len(added) > 0 {
		fmt.Fprintf(os.Stderr, "teststate: GUARD: %d entries were created under the REAL loop state root %s during this test run:\n", len(added), real)
		for _, name := range added {
			fmt.Fprintf(os.Stderr, "teststate: GUARD:   %s\n", filepath.Join(real, name))
		}
		fmt.Fprintln(os.Stderr, "teststate: GUARD: a test is deriving or writing state dirs without isolation - see package internal/teststate")
		if code == 0 {
			code = 1
		}
	}
	return code
}

// readDirNames lists one level of dir; a missing directory reads as empty.
func readDirNames(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// addedNames returns the names present in after but not in before, in sorted
// order.
func addedNames(before, after []string) []string {
	was := make(map[string]bool, len(before))
	for _, n := range before {
		was[n] = true
	}
	var added []string
	for _, n := range after {
		if !was[n] {
			added = append(added, n)
		}
	}
	return added
}
