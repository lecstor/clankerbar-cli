package config

import (
	"path/filepath"

	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// MCPConfigNone is the mcp_config_path value that means "no MCP config": set it
// at any level (run-wide, per-harness, per-project) to opt out explicitly. It
// exists because an empty value cannot opt out - empty re-runs the harness-blind
// <workdir>/.mcp.json discovery, whose Claude schema opencode cannot read
// (CLA-318). "none" survives Validate untouched, skips the origin
// checks (there is no file to check), and resolves to nothing at invocation time
// for every harness. A single-project run that sets it loses the slug its poll
// would otherwise derive from .mcp.json, so name the project in backlog_url
// (/mcp/<slug>) to keep the poll project-scoped.
const MCPConfigNone = "none"

// mcpConfigHandedTo narrows a resolved MCP config path to what one harness may
// actually be handed (CLA-318).
//
// The auto-discovered file is always named .mcp.json - Claude Code's project MCP
// config, whose schema opencode cannot read and whose wiring codex never
// sees. That one discovery feeds two consumers with conflicting needs: the
// driver's poll reads the slug out of the file whatever harness runs, while a
// SESSION should receive only a file in ITS schema. So Validate keeps filling
// the field for everyone, and this gate decides at hand-off whether the file
// reaches the session:
//
//   - codex (MCPConfigUnused): nothing is ever handed - the adapter drops the
//     path anyway, so handing it nothing costs nothing.
//   - opencode (MCPConfigNative): a file named .mcp.json is Claude-shaped by
//     convention and is refused here rather than passed as OPENCODE_CONFIG;
//     any other path passes verbatim, because the operator named that file
//     themselves and doctor still reports on its shape.
//   - claude (MCPConfigClaudeJSON): everything passes, exactly as before.
//
// An unknown harness returns the path unchanged: Validate refuses those long
// before this runs, and refusing to guess beats inventing policy here.
func mcpConfigHandedTo(harnessName, path string) string {
	switch {
	case path == "", path == MCPConfigNone:
		return ""
	}
	adapter, err := harness.Get(harnessName)
	if err != nil {
		return path
	}
	switch adapter.MCPConfigUse().Schema {
	case harness.MCPConfigUnused:
		return ""
	case harness.MCPConfigNative:
		if filepath.Base(path) == ".mcp.json" {
			return ""
		}
	}
	return path
}

// mcpUnderWorkDir normalizes an MCP config path the way every other path field
// is normalized (expandHome, then resolve relative against the workdir), except
// that the "none" opt-out passes through untouched: it is a word the resolution
// layer interprets, not a path (see MCPConfigNone).
func mcpUnderWorkDir(p, workdir string) string {
	if p == MCPConfigNone {
		return p
	}
	return underWorkDir(expandHome(p), workdir)
}
