package mcpbridge

import (
	"os"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/teststate"
)

// internal/mcpbridge reaches internal/config (ResolveOrigin reads the operator's
// config), so its test binary must isolate the loop state home and the real
// credential store, exactly as teststate's enforcement test requires.
func TestMain(m *testing.M) { os.Exit(teststate.Isolate(m)) }
