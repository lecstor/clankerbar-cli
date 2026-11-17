package cli

// The hook command's bar is mostly about what it does NOT do: the no-op paths
// must send nothing and every runtime failure must still exit 0, because Claude
// Code waits on this process after every tool call.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type hookCapture struct {
	mu     sync.Mutex
	paths  []string
	bodies [][]byte
	status int
	reply  string
}

func newHookCapture() *hookCapture {
	return &hookCapture{status: http.StatusOK, reply: `{"accepted":1}`}
}

func (c *hookCapture) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.paths = append(c.paths, r.URL.Path)
		c.bodies = append(c.bodies, body)
		status, reply := c.status, c.reply
		c.mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (c *hookCapture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.paths)
}

func (c *hookCapture) only(t *testing.T) (string, []byte) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) != 1 {
		t.Fatalf("got %d request(s), want 1", len(c.bodies))
	}
	return c.paths[0], c.bodies[0]
}

// hookHome points HOME at a temp config naming srv as the plane, and clears the
// machine-local opt-out so an ambient value cannot decide a test.
func hookHome(t *testing.T, srv *httptest.Server) {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "clankerbar")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := []byte(`{"backlog_url":` + jsonString(srv.URL) + `}`)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CLANKERBAR_EVENTS", "")
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func bindSession(t *testing.T, runID, apiKey string) {
	t.Helper()
	t.Setenv("CLANKERBAR_RUN_ID", runID)
	t.Setenv("CLANKERBAR_API_KEY", apiKey)
}

const hookPayload = `{"hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"pnpm test"},"tool_use_id":"toolu_1"}`

func TestHookEmitSendsOneEvent(t *testing.T) {
	c := newHookCapture()
	srv := c.server(t)
	hookHome(t, srv)
	bindSession(t, "run-1", "clk_test_key_12345678")

	if err := hookEmit(nil, strings.NewReader(hookPayload)); err != nil {
		t.Fatalf("hookEmit: %v", err)
	}
	path, body := c.only(t)
	if path != "/api/runs/run-1/events" {
		t.Errorf("path = %q", path)
	}
	if !strings.Contains(string(body), `"tool_call"`) || !strings.Contains(string(body), `"pnpm test"`) {
		t.Errorf("body = %s, want the tool_call for the payload", body)
	}
}

func TestHookEmitNoOpsWithoutTheRunID(t *testing.T) {
	c := newHookCapture()
	srv := c.server(t)
	hookHome(t, srv)
	t.Setenv("CLANKERBAR_RUN_ID", "")
	t.Setenv("CLANKERBAR_API_KEY", "clk_test_key_12345678")

	if err := hookEmit(nil, strings.NewReader(hookPayload)); err != nil {
		t.Fatalf("hookEmit: %v", err)
	}
	if n := c.count(); n != 0 {
		t.Fatalf("sent %d request(s) with no run id, want 0", n)
	}
}

func TestHookEmitNoOpsWithoutTheAPIKey(t *testing.T) {
	c := newHookCapture()
	srv := c.server(t)
	hookHome(t, srv)
	t.Setenv("CLANKERBAR_RUN_ID", "run-1")
	t.Setenv("CLANKERBAR_API_KEY", "")

	if err := hookEmit(nil, strings.NewReader(hookPayload)); err != nil {
		t.Fatalf("hookEmit: %v", err)
	}
	if n := c.count(); n != 0 {
		t.Fatalf("sent %d request(s) with no API key, want 0", n)
	}
}

func TestHookEmitNoOpsWithTheNoEventsFlag(t *testing.T) {
	c := newHookCapture()
	srv := c.server(t)
	hookHome(t, srv)
	bindSession(t, "run-1", "clk_test_key_12345678")

	if err := hookEmit([]string{"--no-events"}, strings.NewReader(hookPayload)); err != nil {
		t.Fatalf("hookEmit: %v", err)
	}
	if n := c.count(); n != 0 {
		t.Fatalf("sent %d request(s) with --no-events, want 0", n)
	}
}

func TestHookEmitNoOpsWithTheEnvOptOut(t *testing.T) {
	for _, value := range []string{"off", "0", "FALSE"} {
		t.Run(value, func(t *testing.T) {
			c := newHookCapture()
			srv := c.server(t)
			hookHome(t, srv)
			bindSession(t, "run-1", "clk_test_key_12345678")
			t.Setenv("CLANKERBAR_EVENTS", value)

			if err := hookEmit(nil, strings.NewReader(hookPayload)); err != nil {
				t.Fatalf("hookEmit: %v", err)
			}
			if n := c.count(); n != 0 {
				t.Fatalf("sent %d request(s) with CLANKERBAR_EVENTS=%s, want 0", n, value)
			}
		})
	}
}

func TestHookEmitExitsCleanlyWhenTheDialIsOff(t *testing.T) {
	c := newHookCapture()
	c.status = http.StatusForbidden
	c.reply = `{"error":{"code":"events_disabled","message":"The project's events.activity dial is off."}}`
	srv := c.server(t)
	hookHome(t, srv)
	bindSession(t, "run-1", "clk_test_key_12345678")

	if err := hookEmit(nil, strings.NewReader(hookPayload)); err != nil {
		t.Fatalf("a dial refusal must not fail the hook: %v", err)
	}
	if n := c.count(); n != 1 {
		t.Fatalf("got %d request(s), want the one attempt the plane refuses", n)
	}
}

func TestHookEmitExitsCleanlyWhenTheSendFails(t *testing.T) {
	c := newHookCapture()
	c.status = http.StatusInternalServerError
	c.reply = `{"error":{"code":"unavailable"}}`
	srv := c.server(t)
	hookHome(t, srv)
	bindSession(t, "run-1", "clk_test_key_12345678")

	if err := hookEmit(nil, strings.NewReader(hookPayload)); err != nil {
		t.Fatalf("a failed send must not fail the hook: %v", err)
	}
}

func TestHookEmitNoOpsOnANonPostToolUsePayload(t *testing.T) {
	c := newHookCapture()
	srv := c.server(t)
	hookHome(t, srv)
	bindSession(t, "run-1", "clk_test_key_12345678")

	payload := `{"hook_event_name":"Stop","tool_name":"","tool_input":{},"tool_use_id":""}`
	if err := hookEmit(nil, strings.NewReader(payload)); err != nil {
		t.Fatalf("hookEmit: %v", err)
	}
	if n := c.count(); n != 0 {
		t.Fatalf("sent %d request(s) for a Stop payload, want 0", n)
	}
}

func TestHookEmitNoOpsWhenTheConfigCannotBeRead(t *testing.T) {
	c := newHookCapture()
	srv := c.server(t)
	hookHome(t, srv)
	bindSession(t, "run-1", "clk_test_key_12345678")

	// An explicit --config that does not exist is an error, not the "discovered
	// default absent" default: the hook still must send nothing and exit 0.
	missing := filepath.Join(t.TempDir(), "absent-config.json")
	if err := hookEmit([]string{"--config", missing}, strings.NewReader(hookPayload)); err != nil {
		t.Fatalf("an unreadable config must not fail the hook: %v", err)
	}
	if n := c.count(); n != 0 {
		t.Fatalf("sent %d request(s) with an unreadable config, want 0", n)
	}
}

func TestHookEmitNoOpsWhenTheConfigNamesNoOrigin(t *testing.T) {
	c := newHookCapture()
	srv := c.server(t)
	hookHome(t, srv)
	bindSession(t, "run-1", "clk_test_key_12345678")

	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"backlog_url":"not a url"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := hookEmit([]string{"--config", cfgPath}, strings.NewReader(hookPayload)); err != nil {
		t.Fatalf("a config with no trusted origin must not fail the hook: %v", err)
	}
	if n := c.count(); n != 0 {
		t.Fatalf("sent %d request(s) with no trusted origin, want 0", n)
	}
}

func TestHookRoutesEmitAndHelp(t *testing.T) {
	c := newHookCapture()
	srv := c.server(t)
	hookHome(t, srv)
	bindSession(t, "run-1", "clk_test_key_12345678")

	if err := Hook(context.Background(), []string{"--help"}); err != nil {
		t.Fatalf("hook --help: %v", err)
	}
	if err := Hook(context.Background(), []string{"frobnicate"}); err == nil {
		t.Fatal("an unknown hook subcommand must be an error")
	}
	if err := Hook(context.Background(), []string{"emit", "--no-events"}); err != nil {
		t.Fatalf("hook emit --no-events: %v", err)
	}
	if n := c.count(); n != 0 {
		t.Fatalf("the help/unknown paths sent %d request(s)", n)
	}
}
