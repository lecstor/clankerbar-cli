package auth_test

import (
	"os"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/teststate"
)

// The auth package reads config.StateHome; the enforcement test in
// internal/teststate requires every package whose tests can reach internal/config
// to isolate the real state home. It also keeps these tests off the operator's
// real credential store.
func TestMain(m *testing.M) { os.Exit(teststate.Isolate(m)) }
