package main

// The command routing is the entry point of CLA-525's doneWhen — bare
// `clankerbar` is the fleet supervisor — so it is pinned here rather than
// left as untested glue. The supervisor's own behaviour lives in
// internal/supervisor; what these tests prove is that the subcommand switch
// sends a bare invocation down the supervise path and keeps the other exit
// codes honest.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/teststate"
)

// TestMain isolates the loop state root, the same requirement as every
// package whose tests can reach internal/config (the supervise path loads
// configs and resolves state dirs).
func TestMain(m *testing.M) { os.Exit(teststate.Isolate(m)) }

// Bare `clankerbar` routes to the supervisor: with no subcommand it must
// return cleanly (exit 0) — not print usage and exit 2 as a bare invocation
// did before CLA-525. Since phase 3b the supervisor reconciles against the
// account-scoped roster, so the machine layer is stubbed: HOME points at a
// temp dir (the operator's real ~/.config/clankerbar is unreachable), a
// planted config names a LOCAL fake plane (so the roster URL never touches
// the real one), and a fake account key replaces whatever the ambient
// environment carries — the test must never depend on, or spend, the
// operator's real key. An empty roster is the supervisor's documented clean
// return: nothing to supervise.
func TestBareInvocationIsTheSupervisor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"entries":[]}`)
	}))
	defer srv.Close()

	home := t.TempDir()
	cfgDir := filepath.Join(home, ".config", "clankerbar")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The roster URL derives from backlog_url, so naming the fake plane is
	// what keeps the invocation off the real one.
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"),
		[]byte(fmt.Sprintf(`{"backlog_url":%q}`, srv.URL)), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", home)
	t.Setenv("CLANKERBAR_API_KEY", "test-key") // the irreducible account credential
	if code := run([]string{"clankerbar"}); code != 0 {
		t.Fatalf("bare `clankerbar` exited %d, want 0 — the supervisor returns cleanly when there is nothing to supervise", code)
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
