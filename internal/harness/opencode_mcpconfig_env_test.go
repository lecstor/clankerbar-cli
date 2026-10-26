package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLA-318: the gate lives in config (ResolveMCPConfig), but its point is what
// the ADAPTER exports, so pin the wire: a resolved path travels as
// OPENCODE_CONFIG, an empty one exports none, and an ambient value never leaks.

func opencodeConfigVars(t *testing.T, in Invocation) []string {
	t.Helper()
	env, err := (opencode{}).env(in)
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	var out []string
	for _, kv := range env {
		if strings.HasPrefix(kv, "OPENCODE_CONFIG=") {
			out = append(out, kv)
		}
	}
	return out
}

func TestOpencodeEnv_HandsTheConfigFileOverOnlyWhenThereIsOne(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oc-mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcp":{"clankerbar":{"type":"remote","url":"https://clankerbar.com/mcp/p"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := opencodeConfigVars(t, Invocation{WorkDir: dir, MCPConfigPath: path})
	if len(got) != 1 || got[0] != "OPENCODE_CONFIG="+path {
		t.Errorf("named opencode config: got %q, want exactly OPENCODE_CONFIG=%s", got, path)
	}
	if got := opencodeConfigVars(t, Invocation{WorkDir: dir}); len(got) != 0 {
		t.Errorf("empty MCPConfigPath exported %q; the gate resolving \"\" must mean no variable", got)
	}
}

func TestOpencodeEnv_DropsAnAmbientOpencodeConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPENCODE_CONFIG", filepath.Join(dir, ".mcp.json"))
	if got := opencodeConfigVars(t, Invocation{WorkDir: dir}); len(got) != 0 {
		t.Errorf("ambient OPENCODE_CONFIG reached the session as %q; only the driver's resolution may set it", got)
	}
	// An explicit caller value is a deliberate choice and still applies.
	want := "OPENCODE_CONFIG=" + filepath.Join(dir, "explicit.json")
	got := opencodeConfigVars(t, Invocation{WorkDir: dir, Env: []string{want}})
	if len(got) != 1 || got[0] != want {
		t.Errorf("explicit in.Env OPENCODE_CONFIG: got %q, want [%s]", got, want)
	}
}
