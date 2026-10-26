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

// opencodeResolveClankerbarURL replicates the way opencode 2.x resolves `mcp`
// across its config layers, applied to an environment this adapter produced.
// The order is opencode's own, read off `opencode debug config` against 2.0.32,
// and it is the whole reason this test exists rather than an assertion that
// OPENCODE_CONFIG holds the right path:
//
//  1. the global config dir (OPENCODE_CONFIG_DIR replaces ~/.config/opencode)
//  2. OPENCODE_CONFIG - the file the driver names
//  3. project-level opencode.json and .opencode/opencode.json, discovered from
//     the INSTANCE DIRECTORY, which is $PWD (see pinPWD)
//  4. OPENCODE_CONFIG_CONTENT
//
// Layer 3 is the one that beats the driver's file; layer 4 is the one the
// driver uses. Both dialects are read (`mcp.<name>` and `mcp.servers.<name>`),
// and later layers overwrite earlier ones key by key, which is why only a block
// named `clankerbar` can displace ours.
func opencodeResolveClankerbarURL(t *testing.T, env []string, skipContent bool) string {
	t.Helper()
	get := func(k string) string { return lastValue(env, k) }
	url := ""
	apply := func(data []byte) {
		var f struct {
			MCP map[string]json.RawMessage `json:"mcp"`
		}
		if json.Unmarshal(data, &f) != nil {
			return
		}
		var entry struct {
			URL string `json:"url"`
		}
		if raw, ok := f.MCP["clankerbar"]; ok && json.Unmarshal(raw, &entry) == nil && entry.URL != "" {
			url = entry.URL
		}
		var servers map[string]struct {
			URL string `json:"url"`
		}
		if raw, ok := f.MCP["servers"]; ok && json.Unmarshal(raw, &servers) == nil {
			if e, ok := servers["clankerbar"]; ok && e.URL != "" {
				url = e.URL
			}
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
	applyFile(filepath.Join(get("OPENCODE_CONFIG_DIR"), "opencode.json")) // 1
	applyFile(get("OPENCODE_CONFIG"))                                     // 2
	applyFile(filepath.Join(get("PWD"), "opencode.json"))                 // 3
	applyFile(filepath.Join(get("PWD"), ".opencode", "opencode.json"))    // 3
	if !skipContent {
		if content := get("OPENCODE_CONFIG_CONTENT"); content != "" { // 4
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

// The content layer carries the named file's servers, moved out of Code Mode
// and into the 2.x dialect, plus the permission policy. OPENCODE_CONFIG still
// names the file itself.
func TestOpencodeConfigContentCarriesTheNamedFile(t *testing.T) {
	path := mcpConfigNaming(t, t.TempDir(), "opencode-mcp.json", "acme")
	env, err := opencode{}.env(Invocation{WorkDir: t.TempDir(), MCPConfigPath: path})
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	var doc struct {
		MCP struct {
			Servers map[string]map[string]any `json:"servers"`
		} `json:"mcp"`
		Permission map[string]any `json:"permission"`
	}
	if err := json.Unmarshal([]byte(lastValue(env, "OPENCODE_CONFIG_CONTENT")), &doc); err != nil {
		t.Fatalf("content is not JSON: %v", err)
	}
	cb := doc.MCP.Servers["clankerbar"]
	if cb["url"] != "https://clankerbar.com/mcp/acme" || cb["type"] != "remote" {
		t.Errorf("clankerbar server = %v, want the named file's url and type", cb)
	}
	if cb["codemode"] != false {
		t.Errorf("codemode = %v, want false", cb["codemode"])
	}
	if _, ok := cb["enabled"]; ok {
		t.Errorf("the 1.x `enabled` key survived into the 2.x dialect: %v", cb)
	}
	if doc.Permission["*"] != "deny" {
		t.Errorf("permission = %v, want the fail-closed policy", doc.Permission)
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
