package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitForMCPAcceptsConnectedAndSendsBasicAuth(t *testing.T) {
	dir := t.TempDir()
	resolved, err := resolveWorkdir(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/mcp" {
			http.NotFound(w, r)
			return
		}
		if u, p, ok := r.BasicAuth(); !ok || u != "opencode" || p != "pw" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		if got := r.URL.Query().Get("location[directory]"); got != resolved {
			t.Errorf("location = %q, want the resolved dir %q", got, resolved)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"location": map[string]any{"directory": resolved},
			"data": []map[string]any{
				{"name": "github", "status": map[string]any{"status": "pending"}},
				{"name": "clankerbar", "status": map[string]any{"status": "connected"}},
			},
		})
	}))
	defer srv.Close()

	s := &rigServer{url: srv.URL, password: "pw"}
	if err := s.waitForMCP(context.Background(), "clankerbar", dir, 2*time.Second); err != nil {
		t.Fatalf("waitForMCP: %v", err)
	}
}

func TestWaitForMCPRetriesPendingThenSucceeds(t *testing.T) {
	dir := t.TempDir()
	resolved, _ := resolveWorkdir(dir)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		state := "pending"
		if n >= 2 {
			state = "connected"
		}
		json.NewEncoder(w).Encode(map[string]any{
			"location": map[string]any{"directory": resolved},
			"data":     []map[string]any{{"name": "clankerbar", "status": map[string]any{"status": state}}},
		})
	}))
	defer srv.Close()
	s := &rigServer{url: srv.URL, password: "pw"}
	if err := s.waitForMCP(context.Background(), "clankerbar", dir, 2*time.Second); err != nil {
		t.Fatalf("waitForMCP: %v", err)
	}
	if got := calls.Load(); got < 2 {
		t.Fatalf("polled %d times, want at least 2", got)
	}
}

func TestWaitForMCPTreatsFailedAsAVerdict(t *testing.T) {
	dir := t.TempDir()
	resolved, _ := resolveWorkdir(dir)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"location": map[string]any{"directory": resolved},
			"data":     []map[string]any{{"name": "clankerbar", "status": map[string]any{"status": "failed"}}},
		})
	}))
	defer srv.Close()
	s := &rigServer{url: srv.URL, password: "pw"}
	err := s.waitForMCP(context.Background(), "clankerbar", dir, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("err = %v, want a failed-status verdict", err)
	}
}

func TestWaitForMCPRefusesAStatusForAnotherLocation(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"location": map[string]any{"directory": "/somewhere/else"},
			"data":     []map[string]any{{"name": "clankerbar", "status": map[string]any{"status": "connected"}}},
		})
	}))
	defer srv.Close()
	s := &rigServer{url: srv.URL, password: "pw"}
	err := s.waitForMCP(context.Background(), "clankerbar", dir, 250*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "location") {
		t.Fatalf("err = %v, want a location mismatch", err)
	}
}

// TestStartRigServerCapturesAnnouncementsWithoutLoggingThePassword pins the two
// announcer lines the preflight depends on, and the secret handling: the
// password line is never written to the run's log.
func TestStartRigServerCapturesAnnouncementsWithoutLoggingThePassword(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake opencode binary is a shell script")
	}
	out := t.TempDir()
	bin := fakeBin(t, `
echo "some ordinary server chatter"
echo "server listening on http://127.0.0.1:43210"
echo "server password sup3r-secret"
sleep 30
`)
	s, err := startRigServer(bin, os.Environ(), out, out)
	if err != nil {
		t.Fatalf("startRigServer: %v", err)
	}
	defer s.stop()
	if s.url != "http://127.0.0.1:43210" {
		t.Errorf("url = %q", s.url)
	}
	if s.password != "sup3r-secret" {
		t.Errorf("password = %q", s.password)
	}
	stdout, err := os.ReadFile(filepath.Join(out, "opencode-server-stdout.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stdout), "ordinary server chatter") {
		t.Errorf("the server's ordinary stdout was not logged: %q", stdout)
	}
	if strings.Contains(string(stdout), "sup3r-secret") {
		t.Error("the server password was written to the log")
	}
}

func TestResolveWorkdirResolvesSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	got, err := resolveWorkdir(link)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(dir)
	if got != want {
		t.Fatalf("resolveWorkdir(%q) = %q, want %q", link, got, want)
	}
}

// fakeBin writes an executable shell script and returns its path.
func fakeBin(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-opencode")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
