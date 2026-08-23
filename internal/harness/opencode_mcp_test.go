package harness

import (
	"strings"
	"testing"
)

// CLA-318: the gate lives in config (ResolveMCPConfig), but its whole point is
// what the ADAPTER exports - so pin the wire itself. A Claude-shaped file must
// never travel as OPENCODE_CONFIG, and an opencode-schema one must.

func TestOpencodeEnv_HandsTheConfigFileOverOnlyWhenThereIsOne(t *testing.T) {
	env := (opencode{}).env(Invocation{WorkDir: "/w", MCPConfigPath: "/w/oc-mcp.json"})
	found := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "OPENCODE_CONFIG=") {
			found = true
			if kv != "OPENCODE_CONFIG=/w/oc-mcp.json" {
				t.Errorf("got %q, want the path verbatim", kv)
			}
		}
	}
	if !found {
		t.Error("an opencode-schema MCP config path was set but never exported as OPENCODE_CONFIG - the wiring silently vanished")
	}

	env = (opencode{}).env(Invocation{WorkDir: "/w"})
	for _, kv := range env {
		if strings.HasPrefix(kv, "OPENCODE_CONFIG=") {
			t.Errorf("got %q with an empty MCPConfigPath - the gate resolving \"\" must mean NO variable, not a stale or relative one", kv)
		}
	}
}
