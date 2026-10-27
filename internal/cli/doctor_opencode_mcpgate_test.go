package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// CLA-318: doctor's opencode checks report on the file a session is HANDED
// (ResolveMCPConfig), not the raw field, so "none" and a workdir .mcp.json -
// both resolved to nothing - are not read as the session's config.
func TestOpencodeDoctorChecksReadTheGatedPath(t *testing.T) {
	dir := t.TempDir()
	claude := filepath.Join(dir, ".mcp.json")
	body := `{"mcpServers":{"clankerbar":{"type":"http","url":"https://clankerbar.com/mcp/p"}},"permission":{"*":"allow"},"mcp":{"context7":{"enabled":false}}}`
	if err := os.WriteFile(claude, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{config.MCPConfigNone, claude} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			cfg := &config.Config{Harness: "opencode", Prompt: "x", WorkDir: dir, MCPConfigPath: path}
			c := checkOpencodePermissions(cfg, "permissions", "opencode")
			if c.status == fail {
				t.Fatalf("FAIL on a path no session is handed: %s", c.detail)
			}
			for _, line := range append([]string{c.detail}, c.info...) {
				if strings.Contains(line, path) {
					t.Errorf("reported on %s, which the gate never hands to opencode: %q", path, line)
				}
			}
			if got := checkOpencodeMCPDisables(cfg); len(got) != 1 || got[0].status != pass {
				t.Errorf("disables check read the ungated file: %+v", got)
			}
		})
	}
}
