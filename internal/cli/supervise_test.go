package cli

// Supervise's flag discipline, its wiring refusal and its empty-fleet no-op.
// The supervisor's own behaviour (spawn-per-entry, restart, fleet-wide drain,
// reconcile) is tested in internal/supervisor; what is pinned here is the CLI
// wrapper: it follows the shared flag rules, refuses to run without the
// account key, and returns cleanly over a roster with nothing local on it —
// all hermetic, with the roster served by a local fake plane and HOME
// isolated so the operator's real config and state are never touched.

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// The account key is the whole credential: a supervisor without it has no
// desired state to reconcile against, and running children on none is exactly
// the un-steered fleet this phase exists to end. The refusal is a wiring
// error, said loudly, not a degraded mode.
func TestSuperviseRefusesWithoutAccountKey(t *testing.T) {
	t.Setenv("CLANKERBAR_API_KEY", "")
	t.Setenv("HOME", t.TempDir())
	err := Supervise(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "CLANKERBAR_API_KEY") {
		t.Fatalf("Supervise without the account key returned %v, want a refusal naming the key", err)
	}
}

// A roster with nothing local on it (an empty one) is a clean no-op, not an
// error and not an idle hang. The whole run is hermetic: HOME is isolated so
// the operator's real config is never read, the roster is served by a local
// fake plane the test's config points at, and the fake key never leaves the
// test.
func TestSuperviseEmptyRosterReturnsCleanly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"entries": []}`))
	}))
	defer srv.Close()

	t.Setenv("CLANKERBAR_API_KEY", "test-key")
	t.Setenv("HOME", t.TempDir())
	writeSuperviseConfig(t, srv.URL)

	if err := Supervise(context.Background(), nil); err != nil {
		t.Fatalf("Supervise over an empty roster returned %v, want nil", err)
	}
}

// The machine-stated workdir root (phase 2a) arrives via CLANKERBAR_WORKDIR_ROOT:
// Supervise reads it and says so before the empty-fleet no-op returns. With the
// variable unset, the derivation stays off and nothing is said about it.
func TestSuperviseReadsWorkdirRootFromEnv(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"entries": []}`))
	}))
	defer srv.Close()

	var buf lockedLog
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	t.Setenv("CLANKERBAR_API_KEY", "test-key")
	t.Setenv("HOME", t.TempDir())
	writeSuperviseConfig(t, srv.URL)

	if err := Supervise(context.Background(), nil); err != nil {
		t.Fatalf("Supervise without the root env returned %v, want nil", err)
	}
	if strings.Contains(buf.String(), WorkdirRootEnv) {
		t.Fatalf("Supervise spoke about %s while it was unset:\n%s", WorkdirRootEnv, buf.String())
	}

	buf.Reset()
	t.Setenv(WorkdirRootEnv, "/machine/dev-root")
	if err := Supervise(context.Background(), nil); err != nil {
		t.Fatalf("Supervise with %s set returned %v, want nil", WorkdirRootEnv, err)
	}
	if !strings.Contains(buf.String(), "/machine/dev-root") {
		t.Fatalf("Supervise did not report deriving from %s:\n%s", "/machine/dev-root", buf.String())
	}
}

// writeSuperviseConfig drops the supervisor's local config into the isolated
// HOME, pointing the roster at the fake plane (backlog_url is where the
// roster endpoint is derived from — the account key never lands in a file).
func writeSuperviseConfig(t *testing.T, backlogURL string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), ".config", "clankerbar")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]string{"backlog_url": backlogURL})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// lockedLog is a concurrency-safe log sink for the tests above.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLog) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *lockedLog) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}
