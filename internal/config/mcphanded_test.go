package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLA-318: the auto-discovered <workdir>/.mcp.json is Claude Code's schema, and
// Validate used to fill mcp_config_path with it whatever harness would run. The
// gate (mcpConfigHandedTo, applied by ResolveMCPConfig at hand-off) decides per
// harness what a SESSION may be handed, while the raw field stays every run's
// slug source for the poll (BacklogEndpoint). These tests pin both halves: which
// file reaches which harness, and that the explicit "none" opt-out survives
// Validate without being silently re-discovered.

func TestResolveMCPConfig_TheWorkdirFileOnlyReachesHarnessesThatReadIt(t *testing.T) {
	t.Run("opencode is not handed a Claude-shaped .mcp.json", func(t *testing.T) {
		// The run-wide path is exactly what discovery fills, inherited into the
		// run-wide harness's session config - the shape an operator's opencode
		// workdir resolves to with no mcp_config_path set anywhere.
		c := &Config{Harness: "opencode", MCPConfigPath: "/w/.mcp.json"}
		if got := c.ResolveMCPConfig("opencode", "", nil); got != "" {
			t.Errorf("ResolveMCPConfig(opencode) = %q, want \"\" - opencode reads `mcp`, not `mcpServers`, and older binaries than the pinned one refuse to start on it", got)
		}
	})
	t.Run("codex is handed nothing at all", func(t *testing.T) {
		c := &Config{Harness: "codex", MCPConfigPath: "/w/.mcp.json"}
		if got := c.ResolveMCPConfig("codex", "", nil); got != "" {
			t.Errorf("ResolveMCPConfig(codex) = %q, want \"\" - codex has no per-run MCP flag, so there is nothing it could do with any file", got)
		}
		c2 := &Config{Harness: "codex", MCPConfigPath: "/w/oc.json"}
		if got := c2.ResolveMCPConfig("codex", "", nil); got != "" {
			t.Errorf("ResolveMCPConfig(codex) = %q, want \"\" even for a well-formed file - MCPConfigUnused is about the schema question never arising", got)
		}
	})
	t.Run("claude keeps the workdir file unchanged", func(t *testing.T) {
		// Asserted as exact equality on purpose. This is the mutation tripwire for
		// the only harness actually in use: if the gate ever swallows claude's
		// file too, every spawned session loses its clankerbar tools while every
		// other test here still passes. Verified by mutation: dropping
		// MCPConfigClaudeJSON in mcpConfigHandedTo fails this subtest.
		c := &Config{Harness: "claude", MCPConfigPath: "/w/.mcp.json"}
		if got := c.ResolveMCPConfig("claude", "", nil); got != "/w/.mcp.json" {
			t.Errorf("ResolveMCPConfig(claude) = %q, want \"/w/.mcp.json\" verbatim - claude's headless mode has no auto-discovery, so this path IS the wiring", got)
		}
	})
	t.Run("an opencode-schema file under opencode passes verbatim", func(t *testing.T) {
		c := &Config{Harness: "opencode", MCPConfigPath: "/w/oc-mcp.json"}
		if got := c.ResolveMCPConfig("opencode", "", nil); got != "/w/oc-mcp.json" {
			t.Errorf("ResolveMCPConfig(opencode) = %q, want the operator's own file passed through - only the .mcp.json name is gated, content is doctor's business", got)
		}
	})
	t.Run("an unregistered harness is not guessed at", func(t *testing.T) {
		c := &Config{Harnesses: map[string]HarnessConfig{"futurecli": {MCPConfigPath: "/x/f.json"}}}
		if got := c.ResolveMCPConfig("futurecli", "", nil); got != "/x/f.json" {
			t.Errorf("ResolveMCPConfig(futurecli) = %q, want it unchanged - Validate refuses unknown harnesses before this runs, so passing through beats inventing policy", got)
		}
	})
}

func TestResolveMCPConfig_AnOperatorNamedClaudeFileUnderOpencodeIsDroppedToo(t *testing.T) {
	// After Validate there is no provenance left on a path: an operator-set
	// harnesses.opencode.mcp_config_path = <workdir>/.mcp.json and a discovered
	// one are the same string. The gate drops both rather than re-handing a file
	// whose schema cannot wire anything - spawn stays independent of how tolerant
	// the installed binary is (1.18.2 refused to start; 1.18.19 starts but wires
	// no servers from `mcpServers`). The contradiction itself is still reported:
	// doctor FAILs a Claude-SHAPED file under opencode whenever one reaches it,
	// which after this gate means only files the OPERATOR named.
	c := mixedCfg()
	c.Harnesses["opencode"] = HarnessConfig{ConfigDir: "/oc/config", MCPConfigPath: "/w/.mcp.json"}
	if got := c.ResolveMCPConfig("opencode", "", nil); got != "" {
		t.Errorf("ResolveMCPConfig(opencode) = %q, want \"\" - an explicitly named .mcp.json is dropped by the same rule as a discovered one", got)
	}
}

func TestResolveMCPConfig_NoneMeansNone(t *testing.T) {
	c := &Config{Harness: "claude", MCPConfigPath: MCPConfigNone}
	for _, h := range []string{"claude", "opencode", "codex"} {
		if got := c.ResolveMCPConfig(h, "", nil); got != "" {
			t.Errorf("ResolveMCPConfig(%s) = %q with mcp_config_path \"none\", want \"\" for every harness", h, got)
		}
	}
	// A per-harness "none" beats a real run-wide file for that harness alone -
	// the one way to keep claude wired while opting opencode out.
	c2 := mixedCfg()
	c2.MCPConfigPath = "/w/.mcp.json"
	c2.Harnesses["opencode"] = HarnessConfig{ConfigDir: "/oc/config", MCPConfigPath: MCPConfigNone}
	if got := c2.ResolveMCPConfig("opencode", "", nil); got != "" {
		t.Errorf("ResolveMCPConfig(opencode) = %q, want \"\" - the per-harness none outranks the run-wide file", got)
	}
	if got := c2.ResolveMCPConfig("claude", "", nil); got != "/w/.mcp.json" {
		t.Errorf("ResolveMCPConfig(claude) = %q, want the run-wide file untouched by opencode's none", got)
	}
}

const cla318ClaudeShaped = `{"mcpServers":{"clankerbar":{"type":"http","url":"https://clankerbar.com/mcp/proj"}}}`

func TestValidate_MCPConfigNoneOptsOutWithoutRediscovery(t *testing.T) {
	dir := t.TempDir()
	mcp := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(mcp, []byte(cla318ClaudeShaped), 0o600); err != nil {
		t.Fatal(err)
	}

	c := &Config{
		Harness:       "opencode",
		WorkDir:       dir,
		MCPConfigPath: MCPConfigNone,
		BacklogURL:    "https://clankerbar.com/mcp/proj",
		Prompt:        "work",
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil - \"none\" is a statement, not an omission to be second-guessed", err)
	}
	if c.MCPConfigPath != MCPConfigNone {
		t.Fatalf("after Validate, mcp_config_path = %q, want \"none\" kept verbatim - re-discovering %s here is the exact bug CLA-318 fixes", c.MCPConfigPath, mcp)
	}
	// With the file opted out, the poll keeps its scope only through an explicit
	// /mcp/<slug> backlog_url - asserted so the documented consequence is pinned,
	// not folklore.
	if got := c.BacklogEndpoint(); got != "https://clankerbar.com/mcp/proj" {
		t.Errorf("BacklogEndpoint() = %q, want the explicit project-scoped backlog_url", got)
	}

	// Same config with a bare base: the slug died with the opt-out, so the
	// endpoint is empty (blind drain) rather than guessed.
	c2 := &Config{Harness: "opencode", WorkDir: dir, MCPConfigPath: MCPConfigNone, BacklogURL: "https://clankerbar.com", Prompt: "work"}
	if err := c2.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	if got := c2.BacklogEndpoint(); got != "" {
		t.Errorf("BacklogEndpoint() = %q, want \"\" - with .mcp.json opted out there is no slug to lift, and inventing one is worse than a visible empty", got)
	}
}

func TestValidate_MCPConfigNonePerHarnessAndPerProject(t *testing.T) {
	dir := t.TempDir()
	mcp := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(mcp, []byte(cla318ClaudeShaped), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("per-harness none survives beside a live top-level file", func(t *testing.T) {
		c := mixedCfg()
		c.WorkDir = dir
		c.MCPConfigPath = mcp
		c.Harnesses["opencode"] = HarnessConfig{ConfigDir: "/oc/config", MCPConfigPath: MCPConfigNone}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() = %v, want nil", err)
		}
		if got := c.Harnesses["opencode"].MCPConfigPath; got != MCPConfigNone {
			t.Fatalf("harnesses.opencode.mcp_config_path = %q after Validate, want \"none\" kept", got)
		}
	})

	t.Run("per-project none survives and resolves to nothing", func(t *testing.T) {
		c := &Config{
			Harness:       "claude",
			WorkDir:       dir,
			MCPConfigPath: mcp,
			BacklogURL:    "https://clankerbar.com/mcp/proj",
			Prompt:        "work",
			Projects:      []Project{{Slug: "proj", WorkDir: dir, MCPConfigPath: MCPConfigNone}},
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() = %v, want nil", err)
		}
		if got := c.Projects[0].MCPConfigPath; got != MCPConfigNone {
			t.Fatalf("projects[0].mcp_config_path = %q after Validate, want \"none\" kept", got)
		}
		// What the driver hands this project's sessions, resolved exactly as
		// loop.go builds the invocation.
		if got := c.ResolveMCPConfig("claude", c.Projects[0].MCPConfigPath, nil); got != "" {
			t.Errorf("ResolveMCPConfig(claude, project none) = %q, want \"\"", got)
		}
	})
}

func TestValidate_AWorkdirDotMcpJsonUnderOpencodeStillFillsForThePollButResolvesEmpty(t *testing.T) {
	// The two-consumer split the whole fix rests on: Validate KEEPS filling the
	// field (the poll reads the slug off it), the hand-off gates it out of the
	// session. Losing either half breaks something real - no slug blind-drains
	// the poll; handing the file over is the bug.
	dir := t.TempDir()
	mcp := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(mcp, []byte(cla318ClaudeShaped), 0o600); err != nil {
		t.Fatal(err)
	}

	c := &Config{Harness: "opencode", WorkDir: dir, BacklogURL: "https://clankerbar.com", Prompt: "work"}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	if c.MCPConfigPath != mcp {
		t.Fatalf("Validate filled mcp_config_path = %q, want %q - the poll still derives its slug from the file", c.MCPConfigPath, mcp)
	}
	if got := c.BacklogEndpoint(); got != "https://clankerbar.com/mcp/proj" {
		t.Errorf("BacklogEndpoint() = %q, want the file-derived project-scoped path - the poll is untouched by the gate", got)
	}
	if got := c.ResolveMCPConfig("opencode", "", nil); got != "" {
		t.Errorf("ResolveMCPConfig(opencode) = %q, want \"\" - the same file never reaches the session", got)
	}
	if !strings.HasSuffix(mcp, ".mcp.json") {
		t.Fatal("test setup drifted: the fixture file stopped being .mcp.json-shaped")
	}
}
