package cli

// Supervise's flag discipline and its empty-fleet no-op. The supervisor's own
// behaviour (spawn-per-file, restart, fleet-wide drain) is tested in
// internal/supervisor; what is pinned here is the CLI wrapper: it follows the
// shared flag rules and resolves the config dir without ever touching the
// operator's real one (HOME is isolated in the tests that reach it).

import (
	"context"
	"strings"
	"testing"
)

// supervise --help is a request that succeeded, not an error.
func TestSuperviseHelpIsClean(t *testing.T) {
	if err := Supervise(context.Background(), []string{"--help"}); err != nil {
		t.Fatalf("supervise --help returned %v, want nil", err)
	}
}

// supervise takes no positionals, like every other subcommand.
func TestSuperviseRejectsPositionalArgs(t *testing.T) {
	err := Supervise(context.Background(), []string{"bogus"})
	if err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Fatalf("supervise bogus returned %v, want an unexpected-argument error", err)
	}
}

func TestSuperviseRejectsUnknownFlag(t *testing.T) {
	if err := Supervise(context.Background(), []string{"--nope"}); err == nil {
		t.Fatal("supervise --nope returned nil, want an unknown-flag error")
	}
}

// Bare Supervise with nothing to supervise is a clean no-op: pins ConfigDir's
// home-relative construction (the same path `run` auto-discovers) and the
// empty-fleet return. HOME is isolated so the operator's real config dir is
// never enumerated — and nothing is spawned either way, since the dir is
// empty.
func TestSuperviseEmptyConfigDirReturnsCleanly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := Supervise(context.Background(), nil); err != nil {
		t.Fatalf("Supervise on an empty config dir returned %v, want nil", err)
	}
}