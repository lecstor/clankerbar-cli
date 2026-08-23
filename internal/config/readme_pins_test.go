// Coupling tests between README.md and the config-discovery/refusal code
// (CLA-383). A security-relevant claim here once rotted unnoticed: the README
// listed ./clankerbar.json as a discovered location long after Load("") stopped
// discovering it. Nothing failed, because nothing read the README.
//
// These tests are the connection, on the model of internal/release's
// workflow_coupling_test.go: every assertion compares the README against values
// or emitted strings taken from THIS package at runtime, so rewording the code
// fails them until README.md is updated - and editing the README away from what
// the code does fails them just the same.
//
// Deliberately unpinned (named rather than left silently unchecked):
//   - The README's prose AROUND these claims (rationale paragraphs, links,
//     formatting) - pinning prose style would make ordinary editing fail.
//   - The "Unparseable MCP config" remedy row is pinned by its stable row name
//     only; its remedy prose shares no wording with the emitted error, and
//     forcing a match would be brittle (see TestReadmeRefusalRemediesMatchCode).
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/secureurl"
)

// readReadme returns the repo README, which lives two directories above this
// package.
func readReadme(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		// Skip rather than fail: this package must stay testable if it is ever
		// vendored somewhere without the repo root beside it.
		t.Skipf("cannot read README.md: %v", err)
	}
	return string(data)
}

// discoveredHomeRelPath exercises real discovery - not homeConfigRelPath - by
// pointing HOME at a temp dir, planting a config where discover() looks, and
// measuring where Load("") actually found it. Deriving from behaviour means a
// change to the discovery mechanism itself moves this value and fails the
// README comparisons below until the docs catch up.
func discoveredHomeRelPath(t *testing.T) string {
	t.Helper()
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
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") with a planted home config: %v", err)
	}
	if cfg.Source() == "" {
		t.Fatal("Load(\"\") did not record the discovered file as its source")
	}
	rel, err := filepath.Rel(home, cfg.Source())
	if err != nil {
		t.Fatalf("rel(%s, %s): %v", home, cfg.Source(), err)
	}
	return rel
}

// The README documents one discovery set: exactly one file found on its own
// (the home config), anything else named with --config, and an unnamed cwd
// clankerbar.json refused outright. This test holds all three claims to what
// Load("")/discover() actually do.
func TestReadmeConfigDiscoveryMatchesLoad(t *testing.T) {
	readme := readReadme(t)

	// Home-only discovery, named with the path the code really resolves.
	rel := discoveredHomeRelPath(t)
	if !strings.Contains(readme, "~/"+rel) {
		t.Errorf("README must name the discovered config path ~/%s - it no longer matches what Load(\"\") discovers", rel)
	}

	// An explicit --config must appear as the way to name any other file.
	if !strings.Contains(readme, "--config") && !strings.Contains(readme, "-c ./") {
		t.Errorf("README must document --config as the way to name a non-discovered config file")
	}

	// The cwd file is refused, not silently adopted. Both halves matter:
	// the refusal itself (behaviour) and the README saying so (docs), each
	// named via the constants the code uses.
	workdir := t.TempDir()
	t.Chdir(workdir)
	cwdCfg := filepath.Join(workdir, cwdConfigName)
	if err := os.WriteFile(cwdCfg, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write %s: %v", cwdCfg, err)
	}
	if _, err := Load(""); err == nil {
		t.Fatalf("Load(\"\") loaded %s: implicit workdir configs are supposed to be refused (CLA-260)", cwdConfigName)
	} else if !strings.Contains(err.Error(), "--config") {
		t.Errorf("the cwd-config refusal should carry its own --config remedy, got: %v", err)
	}

	if !strings.Contains(readme, cwdConfigName) {
		t.Errorf("README must name the refused cwd config file (%s)", cwdConfigName)
	}
	if !strings.Contains(readme, "refused") {
		t.Errorf("README must state that an unnamed cwd config is refused, not read")
	}
}

// The four refusals of the README's credential-origin table each have a remedy;
// three of those remedies share contiguous wording with the error the code
// actually emits, so both sides can be held to it. Emitted errors are captured
// by triggering the real refusals, never copied.
func TestReadmeRefusalRemediesMatchCode(t *testing.T) {
	readme := readReadme(t)

	for _, row := range []string{
		"Foreign origin",
		"Cleartext non-loopback",
		"Local server handed the key",
		"Unparseable MCP config",
	} {
		if !strings.Contains(readme, row) {
			t.Errorf("README refusal table is missing the %q row", row)
		}
	}

	c := defaults()
	c.BacklogURL = "https://clankerbar.com"
	dir := t.TempDir()

	// Cleartext non-loopback: secureurl refuses plain http to a public host.
	_, err := secureurl.Origin("http://example.com/backlog")
	if err == nil {
		t.Fatal("secureurl.Origin accepted a cleartext non-loopback URL")
	}
	// Shared wording: "cleartext" names the failure on both sides.
	assertCoupled(t, readme, "Cleartext non-loopback", err.Error(), []string{"cleartext"})

	// Foreign origin: a keyed server pointing somewhere other than backlog_url.
	foreign := filepath.Join(dir, "foreign.json")
	writeFile(t, foreign, `{"mcpServers":{"clankerbar":{"type":"http","url":"https://evil.example/mcp"}}}`)
	err = c.checkMCPConfigOrigins(foreign, "mcp_config_path")
	if err == nil {
		t.Fatal("checkMCPConfigOrigins accepted a keyed server on a foreign origin")
	}
	assertCoupled(t, readme, "Foreign origin", err.Error(),
		[]string{"not trusted to redirect", "if you mean it, or fix the file"})

	// Local server handed the key: a command entry that carries the key.
	local := filepath.Join(dir, "local.json")
	writeFile(t, local, `{"mcpServers":{"clankerbar":{"command":"npx","args":["-y","clankerbar-mcp"]}}}`)
	err = c.checkMCPConfigOrigins(local, "mcp_config_path")
	if err == nil {
		t.Fatal("checkMCPConfigOrigins accepted a local command handed the API key")
	}
	assertCoupled(t, readme, "Local server handed the key", err.Error(),
		[]string{"a spawned process may send it anywhere"})

	// Unparseable MCP config: coupled by row name only (see the header note).
	unparseable := filepath.Join(dir, "garbage.json")
	writeFile(t, unparseable, "{not json")
	if err := c.checkMCPConfigOrigins(unparseable, "mcp_config_path"); err == nil {
		t.Fatal("checkMCPConfigOrigins accepted an unparseable MCP config")
	}
}

// assertCoupled checks that each fragment appears in BOTH the README's table row
// region and the emitted error, so either side drifting from the shared wording
// fails. frag context names the refusal in the failure message.
func assertCoupled(t *testing.T, readme, context, emitted string, fragments []string) {
	t.Helper()
	for _, frag := range fragments {
		if !strings.Contains(emitted, frag) {
			t.Errorf("%s: the emitted error no longer contains %q - reword it back, or update the README's remedy to match:\n  %s", context, frag, emitted)
		}
		if !strings.Contains(readme, frag) {
			t.Errorf("%s: README no longer contains %q, which the code's emitted error carries - update one side to match the other", context, frag)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
