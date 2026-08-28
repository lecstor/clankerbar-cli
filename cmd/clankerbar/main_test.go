package main

// The command routing is the entry point of CLA-525's doneWhen — bare
// `clankerbar` is the fleet supervisor — so it is pinned here rather than
// left as untested glue. The supervisor's own behaviour lives in
// internal/supervisor; what these tests prove is that the subcommand switch
// sends a bare invocation down the supervise path and keeps the other exit
// codes honest.

import (
	"os"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/teststate"
)

// TestMain isolates the loop state root, the same requirement as every
// package whose tests can reach internal/config (the supervise path loads
// configs and resolves state dirs).
func TestMain(m *testing.M) { os.Exit(teststate.Isolate(m)) }

// Bare `clankerbar` routes to the supervisor: with no subcommand and a config
// dir holding nothing, it must return cleanly (exit 0) — not print usage and
// exit 2 as a bare invocation did before CLA-525. HOME is pointed at a temp
// dir so the test can never see — or spawn children from — the operator's
// real ~/.config/clankerbar.
func TestBareInvocationIsTheSupervisor(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if code := run([]string{"clankerbar"}); code != 0 {
		t.Fatalf("bare `clankerbar` exited %d, want 0 — the supervisor returns cleanly on an empty config dir", code)
	}
}

// Help and version are exit-0 requests, including the supervisor's own help.
func TestHelpAndVersionExitZero(t *testing.T) {
	for _, args := range [][]string{
		{"clankerbar", "--help"},
		{"clankerbar", "help"},
		{"clankerbar", "version"},
		{"clankerbar", "supervise", "--help"},
	} {
		if code := run(args); code != 0 {
			t.Fatalf("%v exited %d, want 0", args, code)
		}
	}
}

// An unknown subcommand keeps its loud exit 2.
func TestUnknownCommandExitsTwo(t *testing.T) {
	if code := run([]string{"clankerbar", "frobnicate"}); code != 2 {
		t.Fatalf("unknown command exited %d, want 2", code)
	}
}