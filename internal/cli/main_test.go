package cli

import (
	"os"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/teststate"
)

// TestMain isolates the loop state dir for every test in this binary (CLA-361):
// doctorRun and friends really create the state dir a config resolves to, and
// without this a fixture workdir leaks a 001-<hash> directory into the
// operator's ~/.local/state/clankerbar/loop. The binary also fails if anything
// was created under the real root during the run.
func TestMain(m *testing.M) { os.Exit(teststate.Isolate(m)) }
