// Coupling tests between README.md and the strings this package emits at
// startup and in doctor output (CLA-383). The README is where an operator
// learns where their API key goes, so a quoted string that no longer matches
// the code is guidance rot, not a typo. On the model of internal/release's
// workflow_coupling_test.go: every assertion compares the README against a
// value produced by THIS package at runtime, so rewording the code fails these
// tests until README.md is updated, and vice versa.
//
// Deliberately unpinned (named rather than left silently unchecked):
//   - The doctor SAMPLE block's check rows for harness, backlog, config_dir,
//     workdir, sessions and permissions: their names come from slice-building
//     constructors that need a live environment (lookPath, pollers), and
//     faking one to harvest four more names would not pay for its brittleness.
//     The cheap constructors below (config, state_dir, budget, toolchains)
//     ARE pinned; if another check renames itself the sample goes stale
//     silently until someone extends this test - stated plainly per the repo
//     convention rather than pinned badly.
//   - The `clankerbar: <time>` log prefix framing around the credential notice.
//   - Everything about doctor's WARN/FAIL prose except what is asserted below.
package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

func readReadme(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Skipf("cannot read README.md: %v", err)
	}
	return string(data)
}

// readmeDoctorSample returns the fenced block of README.md that contains the
// doctor's sample output (identified by its first PASS line for the config
// check).
func readmeDoctorSample(t *testing.T, readme string) string {
	t.Helper()
	for _, block := range strings.Split(readme, "```") {
		if strings.Contains(block, "PASS  config") {
			return block
		}
	}
	t.Fatal("README.md has no fenced sample block containing \"PASS  config\" - the doctor sample output is gone or unfindable")
	return ""
}

// The startup line the run loop logs once, naming where CLANKERBAR_API_KEY
// will be sent. Taken from credentialNotice itself so any rewording in code
// breaks this test until the README catches up.
func TestReadmeCredentialNoticeMatchesCode(t *testing.T) {
	readme := readReadme(t)

	cfg := &config.Config{BacklogURL: "https://clankerbar.com"}
	notice := credentialNotice(cfg, "test-key-do-not-send")
	if !strings.Contains(readme, notice) {
		t.Errorf("README must quote the startup credential line %q exactly as credentialNotice emits it", notice)
	}
}

// The doctor sample must show only output the current code can produce. The
// historical rot (CLA-265 review round) was the sample claiming
// `loaded ./clankerbar.json` after cwd discovery was removed - impossible
// output, printed as fact. These assertions derive what IS possible and hold
// the sample to it.
func TestReadmeDoctorSampleMatchesCode(t *testing.T) {
	readme := readReadme(t)
	sample := readmeDoctorSample(t, readme)

	// The api key origin line: label from the const doctor uses, origin from
	// CredentialOrigin itself.
	cfg := &config.Config{BacklogURL: "https://clankerbar.com"}
	wantOriginLine := apiKeyOriginLabel + cfg.CredentialOrigin()
	if !strings.Contains(sample, wantOriginLine) {
		t.Errorf("README doctor sample must contain %q - the label/origin doctor actually prints", wantOriginLine)
	}

	// Every positive `loaded <path>` claim must resolve to the one file
	// Load("") can discover. Derived behaviourally: plant a home config,
	// discover it, measure where it was found.
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgDir := filepath.Join(home, ".config", "clankerbar")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", cfgDir, err)
	}
	cfgPath := filepath.Join(cfgDir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write %s: %v", cfgPath, err)
	}
	loaded, err := config.Load("")
	if err != nil || loaded.Source() == "" {
		t.Fatalf("config.Load(\"\") did not discover the planted home config: %v", err)
	}
	rel, err := filepath.Rel(home, loaded.Source())
	if err != nil {
		t.Fatalf("rel: %v", err)
	}

	loadedRe := regexp.MustCompile(`(?m)^.*\bloaded (\S+)\s*$`)
	matches := loadedRe.FindAllStringSubmatch(sample, -1)
	if len(matches) == 0 {
		t.Error("README doctor sample shows no `loaded <path>` line for the config check")
	}
	for _, m := range matches {
		shownPath := m[1]
		if !strings.HasSuffix(shownPath, "/"+rel) {
			t.Errorf("README doctor sample claims `loaded %s`, but the only file Load(\"\") can discover ends in /%s - impossible output", shownPath, rel)
		}
	}

	// Check names the cheap constructors can supply without a live
	// environment (see the header note for what stays unpinned and why).
	stateCfg := &config.Config{}
	stateCfg.StateDir = filepath.Join(t.TempDir(), "state")
	for _, c := range []check{
		checkConfig(&config.Config{}),
		checkStateDir(stateCfg),
		checkBudget(&config.Config{}),
		checkToolchains(&config.Config{}),
	} {
		if !strings.Contains(sample, c.name) {
			t.Errorf("README doctor sample never names check %q, but doctor emits it", c.name)
		}
	}
}
