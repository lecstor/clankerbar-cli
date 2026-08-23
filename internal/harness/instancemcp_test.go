package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLA-441, the second half: which project's MCP server a spawned session
// actually reaches.
//
// opencode resolves its config in five layers, and only two of them are the
// driver's. The env this adapter hands over has to name /mcp/<this project> once
// every one of them has been applied - which is not the same claim as
// "OPENCODE_CONFIG points at the right file", and the difference is a fortnight
// of ezyapp-labelled sessions holding CLA-* leases.

// mcpConfigNaming writes an opencode-schema config whose clankerbar server names
// /mcp/<slug>, and returns its path.
func mcpConfigNaming(t *testing.T, dir, name, slug string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	body := `{"mcp":{"clankerbar":{"type":"remote","url":"https://clankerbar.com/mcp/` + slug + `","enabled":true}}}`
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// opencodeResolveClankerbarURL replicates the way opencode resolves `mcp` across
// its config layers, applied to an environment this adapter produced. The merge
// order is opencode's own (1.18.x), and it is the whole reason this test exists
// rather than an assertion that OPENCODE_CONFIG holds the right path:
//
//  1. the global config in OPENCODE_CONFIG_DIR
//  2. OPENCODE_CONFIG - the file the driver names
//  3. project-level opencode.json, discovered from the INSTANCE DIRECTORY, which
//     is $PWD (see pinPWD - that is what makes this the same defect)
//  4. the OPENCODE_CONFIG_DIR directory, merged again
//  5. OPENCODE_CONFIG_CONTENT
//
// Layers 3 and 4 are the two that beat the driver's file; layer 5 is the one the
// driver now uses. Three further layers exist below it in loadInstanceState (the
// console active-org config, a managed config dir, managed preferences); they are
// not modelled because nothing a repo or this driver writes can reach them. Later layers overwrite earlier ones KEY BY KEY, which is why
// only a block named `clankerbar` can displace ours.
func opencodeResolveClankerbarURL(t *testing.T, env []string, skipContent bool) string {
	t.Helper()
	get := func(k string) string { return lastValue(env, k) }
	url := ""
	apply := func(data []byte) {
		var f struct {
			MCP map[string]struct {
				URL string `json:"url"`
			} `json:"mcp"`
		}
		if json.Unmarshal(data, &f) != nil {
			return
		}
		if entry, ok := f.MCP["clankerbar"]; ok {
			url = entry.URL
		}
	}
	applyFile := func(path string) {
		if path == "" {
			return
		}
		if data, err := os.ReadFile(path); err == nil {
			apply(data)
		}
	}
	globalDir := get("OPENCODE_CONFIG_DIR")
	applyFile(filepath.Join(globalDir, "opencode.json"))  // 1
	applyFile(get("OPENCODE_CONFIG"))                     // 2
	applyFile(filepath.Join(get("PWD"), "opencode.json")) // 3
	applyFile(filepath.Join(globalDir, "opencode.json"))  // 4
	if !skipContent {
		if content := get("OPENCODE_CONFIG_CONTENT"); content != "" { // 5
			apply([]byte(content))
		}
	}
	return url
}

// A drain session for project B reaches /mcp/B with BOTH hostile layers in
// place: a global config naming /mcp/A (the shape that redirected the sessions
// started in ~/dev) and a checked-in opencode.json in B's own workdir naming
// /mcp/A (the shape that redirected the sessions started in a repo).
func TestSpawnedSessionResolvesItsOwnProjectMCP(t *testing.T) {
	globalDir := t.TempDir()
	mcpConfigNaming(t, globalDir, "opencode.json", "project-a")

	workdirB := t.TempDir()
	mcpConfigNaming(t, workdirB, "opencode.json", "project-a")
	fileB := mcpConfigNaming(t, t.TempDir(), "opencode-mcp-b.json", "project-b")

	env, err := opencode{}.env(Invocation{WorkDir: workdirB, ConfigDir: globalDir, MCPConfigPath: fileB})
	if err != nil {
		t.Fatalf("env: %v", err)
	}

	// The instance directory is B's workdir, not the daemon's - layer 3 above
	// reads from wherever this points.
	if got := lastValue(env, "PWD"); got != workdirB {
		t.Fatalf("instance directory = %q, want %q", got, workdirB)
	}
	if got := opencodeResolveClankerbarURL(t, env, false); !strings.HasSuffix(got, "/mcp/project-b") {
		t.Errorf("resolved clankerbar server = %q, want /mcp/project-b - a drain session must work the project it polls", got)
	}

	// And the same resolution WITHOUT the content layer, which is what shipped
	// before this change: it lands on project A. Without this line the test
	// above could pass for a config nothing was contending.
	if got := opencodeResolveClankerbarURL(t, env, true); !strings.HasSuffix(got, "/mcp/project-a") {
		t.Errorf("with OPENCODE_CONFIG_CONTENT dropped the resolution = %q, want /mcp/project-a - if the hostile layers do not win here, this test is not testing anything", got)
	}
}

// The content layer carries the named file BYTE FOR BYTE. It is the same file
// OPENCODE_CONFIG names, re-sent at a layer nothing merges after, so nothing
// about the project's config is re-authored on the way through.
func TestOpencodeConfigContentIsTheNamedFile(t *testing.T) {
	path := mcpConfigNaming(t, t.TempDir(), "opencode-mcp.json", "acme")
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	env, err := opencode{}.env(Invocation{WorkDir: t.TempDir(), MCPConfigPath: path})
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	if got := lastValue(env, "OPENCODE_CONFIG_CONTENT"); got != string(want) {
		t.Errorf("OPENCODE_CONFIG_CONTENT = %q, want the named file's exact bytes %q", got, want)
	}
	if got := lastValue(env, "OPENCODE_CONFIG"); got != path {
		t.Errorf("OPENCODE_CONFIG = %q, want %q - the path is still named, the content merely wins later", got, path)
	}
}

// An MCP config that cannot be read FAILS THE SPAWN. The alternative is a
// session that starts, looks healthy, and claims from whichever project the
// ambient config names - the exact failure this change exists to end, so it must
// not be the fallback for a missing file.
func TestOpencodeRefusesToSpawnWithoutItsMCPConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.json")
	if _, err := (opencode{}).env(Invocation{WorkDir: t.TempDir(), MCPConfigPath: missing}); err == nil {
		t.Fatal("env returned no error for an unreadable mcp config - a session spawned without its project pin drains someone else's backlog")
	}
}
