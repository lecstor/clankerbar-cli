package planner

import (
	"os"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/teststate"
)

// TestMain isolates the loop state dir for every test in this binary: the
// planner resolves and opens the same state directory a drain does, and the
// guard fails the binary if a test wrote under the real root.
func TestMain(m *testing.M) { os.Exit(teststate.Isolate(m)) }
