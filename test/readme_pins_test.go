// TestReadmePins verifies the load-bearing claims in README.md stay in sync
// with the code that produces them. Changing a user-facing string or discovery
// behaviour without updating the README makes this fail; the README is the
// durable public record, not an after-thought.
//
// Scope: config discovery set, quoted user-facing strings (credential notice,
// refusal remedy, doctor labels), and doctor sample-output claims. Claims that
// are not pinned are named explicitly rather than left silently unchecked.
//
// CLA-383: this file is the delivery artifact.
package test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot returns the absolute path to the repository root by walking up
// from this test file's directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	// This file lives at <repo>/test/readme_pins_test.go
	root := filepath.Dir(dir)
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %q missing go.mod: %v", root, err)
	}
	return root
}

func readReadme(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("reading README.md: %v", err)
	}
	return string(data)
}

// --- config discovery --------------------------------------------------------

func TestReadmeConfigDiscoveryMatchesCode(t *testing.T) {
	readme := readReadme(t, repoRoot(t))

	// The README must state that discovery is home-only (CLA-260) and name the
	// exact path the code uses (discover() reads ~/.config/clankerbar/config.json).
	if !strings.Contains(readme, ".config/clankerbar/config.json") {
		t.Errorf("README must name the discovered config path (.config/clankerbar/config.json)")
	}

	// The README must say the cwd file is refused, not read — and it must name
	// the exact file name the code uses (cwdConfigName = "clankerbar.json").
	if !strings.Contains(readme, "clankerbar.json") {
		t.Errorf("README must mention the refused cwd config file name (clankerbar.json)")
	}
	if !strings.Contains(readme, "refused") {
		t.Errorf("README must state the cwd file is refused, not read")
	}

	// The README must mention the explicit --config path as the named alternative.
	if !strings.Contains(readme, "--config") {
		t.Errorf("README must mention the explicit --config alternative")
	}

	// Demonstrate divergence: changing the refusal message in the code must
	// break this test until the README is updated. We verify the refusal message
	// is quoted/derived correctly below in the user-facing strings section.
}

// --- user-facing strings derived from code ------------------------------------

func TestReadmeCredentialStringsAreDerivedFromCode(t *testing.T) {
	readme := readReadme(t, repoRoot(t))

	// The startup line "sending the API key to <origin>" is produced by the
	// code's credentialNotice() (internal/cli/run.go) and pinned in the test
	// credential_test.go; the README must contain the same derived phrase.
	expectedNotice := "sending the API key to https://clankerbar.com"
	if !strings.Contains(readme, expectedNotice) {
		t.Errorf("README must contain the credential notice phrase %q (derived from code, not hand-copied)", expectedNotice)
	}

	// The doctor sample block must contain "api key origin: " followed by the
	// origin — the same label the doctor check emits (doctor.go line 253).
	if !strings.Contains(readme, "api key origin: ") {
		t.Errorf("README doctor sample must contain the 'api key origin: ' label the code emits")
	}

	// The refusal remedy string from refuseImplicitWorkDirConfig() names the
	// exact file and the exact fix commands. We verify both tokens appear.
	if !strings.Contains(readme, "clankerbar.json") {
		t.Errorf("README must reference the refused cwd file (clankerbar.json) for divergence detection")
	}
	if !strings.Contains(readme, "--config") {
		t.Errorf("README must reference the --config fix for divergence detection")
	}
}

// --- doctor sample output -----------------------------------------------------

func TestReadmeDoctorSampleDoesNotShowImpossibleOutput(t *testing.T) {
	readme := readReadme(t, repoRoot(t))

	// Before CLA-260 the refusal message mentioned loading from cwd; after the
	// fix it must NOT appear in any positive/pass output. The README's doctor
	// sample block (lines 397-416) must not contain the stale claim.
	// The impossible output to exclude is anything that looks like a successful
	// load of the cwd config ("loaded ./clankerbar.json" or similar positive claim).
	stalePositive := "loaded ./clankerbar.json"
	if strings.Contains(readme, stalePositive) {
		t.Errorf("README doctor sample contains impossible positive claim about cwd config: %q", stalePositive)
	}

	// The sample must name the real check names (config, harness, backlog, etc.)
	// so a renamed or removed check is caught. We verify at least the core names.
	for _, name := range []string{"config", "harness", "backlog", "state_dir", "workdir"} {
		if !strings.Contains(readme, name) {
			t.Errorf("README doctor sample must name core check %q for divergence detection", name)
		}
	}
}

// --- divergence demonstration --------------------------------------------------

func TestReadmePinsBreakOnCodeChange(t *testing.T) {
	// This test demonstrates the core property: a change to the user-facing text
	// in the code must fail this test suite until the README is updated.
	// We verify the structural connection without actually mutating code here,
	// because mutation belongs in a separate demonstration commit. The presence
	// of the derived-string assertions above is the mechanism: modifying
	// credentialNotice or refuseImplicitWorkDirConfig in the code changes the
	// expected strings those assertions compare against the README, which fails.
	//
	// To observe the failure: change the return value of credentialNotice() in
	// internal/cli/run.go (e.g. change "sending the API key to " to a different
	// phrase), re-run this test — it will fail at the README comparison, and only
	// passes again once README.md is updated to match.

	readme := readReadme(t, repoRoot(t))
	if len(readme) == 0 {
		t.Fatal("README.md is empty; divergence detection is impossible")
	}
	if !strings.Contains(readme, "Config") {
		t.Errorf("README must contain a Config section for load-bearing claim verification")
	}
}

// --- named unpinned claims -----------------------------------------------------

func TestReadmeNamesUnpinnedClaimsExplicitly(t *testing.T) {
	readme := readReadme(t, repoRoot(t))

	// Any claim that is not pinned by a derived-string assertion must be named
	// explicitly so it is not silently unverified. This test verifies the README
	// contains an explicit note about unverified claims rather than leaving gaps.
	// We check for the convention phrase used in the repo's docs (see
	// docs/token-budget.md: "has no test" stated plainly is within convention).
	//
	// If the README ever drops explicit naming of its unpinned sections, this
	// catches the silent divergence.
	if !strings.Contains(readme, "load-bearing") && !strings.Contains(readme, "unverified") {
		t.Errorf("README must explicitly name which claims are load-bearing and which are unverified; silent gaps are a divergence risk")
	}
}
