package config

import (
	"encoding/json"

	"github.com/lecstor/clankerbar-cli/internal/jsonc"
)

// JSONC - JSON with comments and trailing commas - is what opencode's own config
// files are written in, and `opencode.jsonc` is the conventional name for the
// global one. Go's encoding/json refuses both extensions, so a strict parse of a
// perfectly ordinary opencode config fails, and the ambient-config check
// (opencodeambient.go) would have reported "nothing to see here" for exactly the
// file that caused CLA-441.
//
// This tolerance is confined to files opencode DISCOVERS on its own. The files
// the driver NAMES - mcp_config_path and its per-harness and per-project
// variants - are still parsed strictly by readMCPFile, and deliberately: those
// feed security gates that fail closed, and widening what they accept would
// widen what an untrusted workdir file can say without being refused. Two
// parsers is the price of not moving that line.
//
// The stripper itself lives in internal/jsonc because the harness adapter's MCP
// preflight reads the same discovered files (CLA-703) and cannot import this
// package - config imports harness, not the other way round.

// parseJSONCInto parses a JSONC document into the mcpFile model.
func parseJSONCInto(data []byte) (mcpFile, error) {
	var f mcpFile
	err := json.Unmarshal(jsonc.Strip(data), &f)
	return f, err
}
