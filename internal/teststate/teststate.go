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
// afterwards compares the REAL root - and its parent clankerbar dir - against
// snapshots taken before the run, failing the binary if the run added anything
// there. A leak therefore cannot return quietly: it turns into a red suite
// naming the directories created. TestEnforcedEverywhereConfigIsImported pins
// the other half: every package whose tests can reach this derivation MUST
// install Isolate, and that is checked mechanically rather than by comment.
//
// Every package whose tests can reach config.ResolveStateDir or
// config.StateRoot MUST install this:
//
//	func TestMain(m *testing.M) { os.Exit(teststate.Isolate(m)) }
//
// Today that is internal/config, internal/cli and internal/loop; the
// enforcement test fails the suite if a new package reaches internal/config
// without installing it.
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
// root or beside it. Call it from TestMain and os.Exit the result:
//
//	func TestMain(m *testing.M) { os.Exit(teststate.Isolate(m)) }
func Isolate(m *testing.M) int {
	// The real root must be resolved BEFORE the environment is overridden:
	// config.StateRoot reads the live process environment, and after Setenv it
	// would answer with the isolated directory instead. The resolve error gets
	// its own variable: every error below shares one name otherwise, and the
	// degraded-mode warning at the bottom would read whatever assignment last
	// touched it (nil, after a successful MkdirTemp).
	realRoot, rootErr := config.StateRoot()
	guard := rootErr == nil
	if !guard {
		// Nowhere real to guard against (HOME unset in a hermetic sandbox,
		// for example). Degrade to isolation alone rather than failing every
		// guarded package before a single test has run - isolation without
		// the guard still keeps tests off the operator's machine.
		realRoot = ""
	}

	var beforeRoot, beforeParent []string
	if guard {
		beforeRoot = readDirNames(realRoot)
		beforeParent = readDirNames(filepath.Dir(realRoot))
	}

	iso, mkErr := os.MkdirTemp("", "clankerbar-test-state-")
	if mkErr != nil {
		fmt.Fprintln(os.Stderr, "teststate: cannot create an isolated state home:", mkErr)
		return 1
	}
	defer os.RemoveAll(iso)
	if err := os.Setenv("XDG_STATE_HOME", iso); err != nil {
		fmt.Fprintln(os.Stderr, "teststate: cannot point XDG_STATE_HOME at the isolated home:", err)
		return 1
	}

	code := m.Run()

	if !guard {
		fmt.Fprintln(os.Stderr, "teststate: WARNING: pollution guard disabled for this run: the real loop state root could not be resolved ("+
			rootErr.Error()+"); XDG_STATE_HOME isolation was still active")
		return code
	}
	if added := addedNames(beforeRoot, readDirNames(realRoot)); len(added) > 0 {
		reportAdded(realRoot, added, true)
		if code == 0 {
			code = 1
		}
	}
	// One level up as well: entries created next to loop/ under clankerbar/
	// are the same leak wearing a different parent.
	if added := addedNames(beforeParent, readDirNames(filepath.Dir(realRoot))); len(added) > 0 {
		reportAdded(filepath.Dir(realRoot), added, false)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// reportAdded prints the guard failure for one watched directory, including
// the way out of the known false positive: a live loop starting mid-run. Only
// a loop start under loop/ mints a statedir, so the sibling directory gets a
// plainer explanation.
func reportAdded(dir string, added []string, isLoopDir bool) {
	fmt.Fprintf(os.Stderr, "teststate: GUARD: %d entries were created under %s during this test run:\n", len(added), dir)
	for _, name := range added {
		fmt.Fprintf(os.Stderr, "teststate: GUARD:   %s\n", filepath.Join(dir, name))
	}
	fmt.Fprintln(os.Stderr, "teststate: GUARD: a test is deriving or writing state dirs without isolation - see package internal/teststate")
	if isLoopDir {
		fmt.Fprintln(os.Stderr, "teststate: GUARD: not test pollution? A clankerbar loop that STARTED while this suite ran creates exactly such an entry; it joins the next run's baseline, so re-run before investigating.")
	} else {
		fmt.Fprintln(os.Stderr, "teststate: GUARD: not test pollution? An external clankerbar process may have created this while the suite ran; it joins the next run's baseline, so re-run before investigating.")
	}
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
