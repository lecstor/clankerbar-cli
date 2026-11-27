package cli

// `clankerbar mcp` is a stdio server, so its tests care about three things the
// bridge tests cannot see from inside internal/mcpbridge: the flag surface, that
// --host wiring reaches the right URL, and that stdout carries only JSON-RPC
// even when the plane fails.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

func parseMcp(args []string) (mcpFlags, string, error) {
	var f mcpFlags
	fs := newMcpFlagSet(&f)
	var out bytes.Buffer
	fs.SetOutput(&out)
	err := parseFlags(fs, args)
	return f, out.String(), err
}

func TestMcpHelpListsItsFlags(t *testing.T) {
	_, out, err := parseMcp([]string{"--help"})
	if !errors.Is(err, pflag.ErrHelp) {
		t.Fatalf("err = %v, want pflag.ErrHelp", err)
	}
	if !strings.Contains(out, "Usage: clankerbar mcp [flags]") {
		t.Errorf("usage lost its heading:\n%s", out)
	}
	for _, flag := range []string{"--host", "--project", "--multi"} {
		if !strings.Contains(out, flag) {
			t.Errorf("usage does not list %s:\n%s", flag, out)
		}
	}
}

func TestMcpRejectsPositionalArguments(t *testing.T) {
	_, out, err := parseMcp([]string{"stray"})
	if err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Fatalf("err = %v, want an unexpected-argument refusal", err)
	}
	if !strings.Contains(out, "Usage: clankerbar mcp") {
		t.Errorf("rejection printed no usage:\n%s", out)
	}
}

func TestMcpRejectsConflictingOrEmptyFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--multi", "--project", "clankerbar"},
		{"--project", "   "},
	} {
		err := mcpRun(context.Background(), args, strings.NewReader(""), io.Discard, io.Discard)
		if err == nil {
			t.Errorf("mcpRun(%v) succeeded", args)
		}
	}
}

func TestMcpBridgesToHost(t *testing.T) {
	t.Setenv("CLANKERBAR_API_KEY", "test-key")
	var path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer srv.Close()

	var out, errOut strings.Builder
	if err := mcpRun(context.Background(),
		[]string{"--host", srv.URL, "--multi"},
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`+"\n"),
		&out, &errOut); err != nil {
		t.Fatalf("mcpRun: %v", err)
	}
	if path != "/mcp" {
		t.Errorf("server path = %q, want /mcp for --multi", path)
	}
	if auth != "Bearer test-key" {
		t.Errorf("Authorization = %q", auth)
	}
	if errOut.Len() != 0 {
		t.Errorf("unexpected stderr:\n%s", errOut.String())
	}
	line := strings.TrimSpace(out.String())
	if !json.Valid([]byte(line)) {
		t.Fatalf("stdout is not one JSON-RPC line: %q", out.String())
	}
}

// The `.clankerbar.json` walk reaches the CLI: a plain `clankerbar mcp` started
// in a mapped checkout targets that project's endpoint.
func TestMcpDiscoversProjectFromClankerbarJSON(t *testing.T) {
	t.Setenv("CLANKERBAR_API_KEY", "test-key")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".clankerbar.json"), []byte(`{"project":"clankerbar-cli"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer srv.Close()

	var out, errOut strings.Builder
	if err := mcpRun(context.Background(),
		[]string{"--host", srv.URL},
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`+"\n"),
		&out, &errOut); err != nil {
		t.Fatalf("mcpRun: %v", err)
	}
	if path != "/mcp/clankerbar-cli" {
		t.Errorf("server path = %q, want the .clankerbar.json project", path)
	}
	if errOut.Len() != 0 {
		t.Errorf("unexpected stderr:\n%s", errOut.String())
	}
}

// A plane failure is still a JSON-RPC error on stdout; a human-facing
// diagnostic must never land in the protocol pipe.
func TestMcpStdoutCarriesOnlyJSONRPCOnFailure(t *testing.T) {
	t.Setenv("CLANKERBAR_API_KEY", "test-key")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "upstream down")
	}))
	defer srv.Close()

	var out, errOut strings.Builder
	if err := mcpRun(context.Background(),
		[]string{"--host", srv.URL, "--multi"},
		strings.NewReader(`{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}`+"\n"),
		&out, &errOut); err != nil {
		t.Fatalf("mcpRun: %v", err)
	}
	line := strings.TrimSpace(out.String())
	if !json.Valid([]byte(line)) {
		t.Fatalf("stdout is not one JSON-RPC line: %q", out.String())
	}
	var resp struct {
		ID    json.RawMessage `json:"id"`
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatal(err)
	}
	if string(resp.ID) != "3" || resp.Error == nil {
		t.Errorf("response = %s, want an error preserving id 3", line)
	}
	if errOut.Len() != 0 {
		t.Errorf("the failure was not a silent protocol answer; stderr:\n%s", errOut.String())
	}
}

// A malformed mapping warns on stderr once and falls back to the bare surface;
// stdout stays protocol-only.
func TestMcpMalformedProjectFileWarnsOnStderr(t *testing.T) {
	t.Setenv("CLANKERBAR_API_KEY", "test-key")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".clankerbar.json"), []byte(`{"project":`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer srv.Close()

	var out, errOut strings.Builder
	if err := mcpRun(context.Background(),
		[]string{"--host", srv.URL},
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`+"\n"),
		&out, &errOut); err != nil {
		t.Fatalf("mcpRun: %v", err)
	}
	if path != "/mcp" {
		t.Errorf("server path = %q, want the bare surface", path)
	}
	if !strings.Contains(errOut.String(), ".clankerbar.json") {
		t.Errorf("stderr does not warn about the malformed mapping:\n%s", errOut.String())
	}
	if !json.Valid([]byte(strings.TrimSpace(out.String()))) {
		t.Errorf("stdout is not JSON-RPC: %q", out.String())
	}
}
