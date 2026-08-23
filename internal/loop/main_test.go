package loop

import (
	"os"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/teststate"
)

// TestMain isolates the loop state dir for every test in this binary (CLA-361):
// the loop preflight opens the state dir a config resolves to, and without this
// a fixture workdir would leak a state dir into the operator's real
// ~/.local/state/clankerbar/loop. The binary also fails if anything was created
// under the real root during the run.
func TestMain(m *testing.M) { os.Exit(teststate.Isolate(m)) }
