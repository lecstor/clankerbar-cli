// Coupling tests between README.md and what this package emits (the CLA-383
// convention: internal/config/readme_pins_test.go, internal/cli/).
//
// codex.go's ExtraDirs comment has pointed at "the README alongside
// repos/primary_repo" since CLA-437; until CLA-443 no such section existed and
// nothing failed, because nothing reads the README. Every assertion here
// compares the README against a string this package's behaviour depends on, so
// editing either side away from the other fails these tests until they are
// reconciled. The README's surrounding prose (rationale paragraphs, examples,
// links) is deliberately not pinned - see the header of
// internal/config/readme_pins_test.go for why.
package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readmeForPins(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Skipf("cannot read README.md: %v", err)
	}
	return string(data)
}

// The multi-repo section must keep documenting every mechanism a declared repo
// feeds, per harness: the loud resolution failure, claude's argv flag,
// opencode's policy gate, and codex's operator-owned sandbox roots. A rename
// here without the README, or a README rewrite that drops one of them, breaks
// this test until the two sides agree again.
func TestReadmeDocumentsRepoResolutionAndHarnessGrants(t *testing.T) {
	r := readmeForPins(t)
	for _, want := range []string{
		"`repos`",
		"`primary_repo`",
		"repo_not_found",
		"--add-dir",
		"external_directory",
		"writable_roots",
	} {
		if !strings.Contains(r, want) {
			t.Errorf("README no longer documents %q, which this package's behaviour depends on - update one side to match the other", want)
		}
	}
}
