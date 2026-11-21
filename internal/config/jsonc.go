package config

import (
	"encoding/json"

	"github.com/lecstor/clankerbar-cli/internal/jsonc"
)

// JSONC - JSON with comments and trailing commas - is what opencode's own config files are written
// in, and `opencode.jsonc` is the conventional name for the global one.

// parseJSONCInto parses a JSONC document into the mcpFile model.
func parseJSONCInto(data []byte) (mcpFile, error) {
	var f mcpFile
	err := json.Unmarshal(jsonc.Strip(data), &f)
	return f, err
}
