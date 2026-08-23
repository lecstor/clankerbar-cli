package config_test

import (
	"os"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/teststate"
)

// TestMain isolates the loop state dir for every test in this binary (CLA-361):
// state-dir derivation lands in a fresh temp dir, and the binary fails if the
// run created anything under the real root (~/.local/state/clankerbar/loop).
//
// This lives in package config_test because internal/teststate imports
// internal/config for its guard, and an in-package test file would close that
// as an import cycle. One compiled test binary covers both this external test
// package and the in-package one (statedir_test.go et al), so a single
// TestMain guards them all: a guard TEST would only see pollution made between
// its own start and cleanup, while the derivation is reachable from any test
// in the binary.
func TestMain(m *testing.M) { os.Exit(teststate.Isolate(m)) }
