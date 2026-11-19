package harness

// CLA-927: a STANDALONE session (clankerbar review run) must not reach a backlog.
//
// review run names no MCP config, so opencode loads whatever its own layers
// declare. A reviewed checkout that checks in `mcp.clankerbar` therefore used to
// hand the review the operator's backlog tools when CLANKERBAR_API_KEY was in the
// environment, and to fail the preflight with needs_auth when it was not.
// NoBacklogMCP switches those servers off in the content layer and skips the wait;
// these tests pin both halves.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// noBacklogStatusServer answers /api/mcp with a fixed status and lets the fake
// `run` start unconditionally, so the test can see whether the adapter polled at
// all — without the fake's usual "refuse until connected" gate, which would fail
// a session that does not wait (and a no-backlog session must not wait).
func noBacklogStatusServer(t *testing.T, status string) (*httptest.Server, *atomic.Int32, *atomic.Bool) {
	t.Helper()
	var polls atomic.Int32
	var started atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/api/mcp", func(w http.ResponseWriter, r *http.Request) {
		polls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"location":{"directory":%q},"data":[{"name":"clankerbar","status":{"status":%q}}]}`,
			r.URL.Query().Get("location[directory]"), status)
	})
	mux.HandleFunc("/run-started", func(w http.ResponseWriter, r *http.Request) {
		started.Store(true)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &polls, &started
}

// ambientClankerbarProject writes a checkout-level opencode.json naming the
// backlog server with a credential header, the makesPDF shape.
func ambientClankerbarProject(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	writeAt(t, filepath.Join(work, "opencode.json"), `{
  "mcp": {
    "servers": {
      "clankerbar": {
        "type": "remote",
        "url": "https://clankerbar.com/mcp/makespdf",
        "headers": {"Authorization": "Bearer {env:CLANKERBAR_API_KEY}"}
      }
    }
  }
}`)
	return work
}

// The content layer carries a FULL off switch for every clankerbar server the
// ambient layers declare — the project file's and the global dir's — including
// one renamed but handed the credential. A partial `{"disabled": true}` would not
// switch a server off on 2.0.32 (see the conformance suite's mcpDisable), so the
// entry must keep its transport. Unrelated servers are left alone, and no
// credential moves into the child's environment with the entry.
func TestOpencodeNoBacklogMCPDisablesEveryAmbientClankerbarServer(t *testing.T) {
	for _, key := range []string{"", "sk-live"} {
		t.Run("CLANKERBAR_API_KEY="+key, func(t *testing.T) {
			t.Setenv("CLANKERBAR_API_KEY", key)

			globalDir := t.TempDir()
			writeAt(t, filepath.Join(globalDir, "opencode.json"),
				`{"mcp":{"cb":{"type":"remote","url":"https://clankerbar.com/mcp/other","headers":{"Authorization":"Bearer {env:CLANKERBAR_API_KEY}"}},"other":{"type":"remote","url":"https://docs.example/mcp"}}}`)
			work := ambientClankerbarProject(t)

			env, err := opencode{}.env(Invocation{WorkDir: work, ConfigDir: globalDir, NoBacklogMCP: true})
			if err != nil {
				t.Fatalf("env: %v", err)
			}
			var doc struct {
				MCP struct {
					Servers map[string]map[string]any `json:"servers"`
				} `json:"mcp"`
			}
			content := lastValue(env, "OPENCODE_CONFIG_CONTENT")
			if err := json.Unmarshal([]byte(content), &doc); err != nil {
				t.Fatalf("content is not JSON: %v\n%s", err, content)
			}
			for _, name := range []string{"clankerbar", "cb"} {
				entry := doc.MCP.Servers[name]
				if entry["disabled"] != true {
					t.Errorf("%s was not switched off: %v", name, entry)
				}
				if entry["type"] != "remote" || entry["url"] == nil {
					t.Errorf("%s did not keep its transport, so the switch is only a partial entry: %v", name, entry)
				}
				if _, ok := entry["enabled"]; ok {
					t.Errorf("%s carries the 1.x `enabled` key beside `disabled`: %v", name, entry)
				}
				if _, ok := entry["headers"]; ok {
					t.Errorf("%s carried its credential headers into the child's environment: %v", name, entry)
				}
			}
			if got := doc.MCP.Servers["clankerbar"]["url"]; got != "https://clankerbar.com/mcp/makespdf" {
				t.Errorf("the project server's url = %v, want the declared one", got)
			}
			if _, ok := doc.MCP.Servers["other"]; ok {
				t.Errorf("an unrelated MCP server was switched off: %v", doc.MCP.Servers["other"])
			}
			if doc.MCP.Servers["clankerbar"]["codemode"] != false {
				t.Errorf("the disabled entry was left in Code Mode: %v", doc.MCP.Servers["clankerbar"])
			}
		})
	}
}

// The switch normalises every declaration into the 2.x container, drops the
// same-named direct entry it replaces, and keeps a 1.x server literally NAMED
// "servers" (which shares the container's key) out of the way.
func TestDisableBacklogMCPServersShapes(t *testing.T) {
	decls := map[string]opencodeMCPDeclaration{
		"clankerbar": {entry: map[string]any{"type": "remote", "url": "https://cb.example/mcp", "headers": map[string]any{"Authorization": "Bearer x"}}},
	}
	cases := []struct {
		name        string
		doc         map[string]any
		decls       map[string]opencodeMCPDeclaration
		wantServers map[string]map[string]any
	}{
		{
			name:  "a 1.x direct declaration",
			doc:   map[string]any{"mcp": map[string]any{"clankerbar": map[string]any{"type": "remote", "url": "https://cb.example/mcp"}}},
			decls: decls,
			wantServers: map[string]map[string]any{
				"clankerbar": {"type": "remote", "url": "https://cb.example/mcp", "disabled": true},
			},
		},
		{
			name:  "an empty document",
			doc:   map[string]any{},
			decls: decls,
			wantServers: map[string]map[string]any{
				"clankerbar": {"type": "remote", "url": "https://cb.example/mcp", "disabled": true},
			},
		},
		{
			name: "a 1.x server literally named servers is kept",
			doc:  map[string]any{"mcp": map[string]any{"servers": map[string]any{"type": "local", "command": []any{"keep"}}}},
			decls: func() map[string]opencodeMCPDeclaration {
				d := map[string]opencodeMCPDeclaration{}
				for name, dcl := range decls {
					d[name] = dcl
				}
				d["servers"] = opencodeMCPDeclaration{entry: map[string]any{"type": "remote", "url": "https://old.example/mcp"}}
				return d
			}(),
			wantServers: map[string]map[string]any{
				"clankerbar": {"type": "remote", "url": "https://cb.example/mcp", "disabled": true},
				"servers":    {"type": "remote", "url": "https://old.example/mcp", "disabled": true},
			},
		},
		{
			name:        "a transport-less declaration is left alone",
			doc:         map[string]any{},
			decls:       map[string]opencodeMCPDeclaration{"clankerbar": {entry: map[string]any{"enabled": false}}},
			wantServers: nil,
		},
		{
			// Left for opencodeMCPOutOfCodeMode to refuse, so the spawn fails
			// closed rather than running with the switch unwritten.
			name:        "a non-object mcp block is not repaired",
			doc:         map[string]any{"mcp": "nope"},
			decls:       decls,
			wantServers: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disableBacklogMCPServers(tc.doc, tc.decls)
			mcp, _ := tc.doc["mcp"].(map[string]any)
			got, _ := mcp["servers"].(map[string]any)
			if len(got) != len(tc.wantServers) {
				t.Fatalf("servers = %v, want %v", got, tc.wantServers)
			}
			for name, want := range tc.wantServers {
				entry, ok := got[name].(map[string]any)
				if !ok {
					t.Fatalf("%s missing from %v", name, got)
				}
				if entry["type"] != want["type"] || entry["disabled"] != want["disabled"] {
					t.Errorf("%s = %v, want %v", name, entry, want)
				}
				if want["url"] != nil && entry["url"] != want["url"] {
					t.Errorf("%s url = %v, want %v", name, entry["url"], want["url"])
				}
				if _, ok := entry["headers"]; ok {
					t.Errorf("%s kept its credential headers: %v", name, entry)
				}
			}
		})
	}
}

// The preflight is skipped: even a status endpoint that never connects cannot
// delay the first turn, because the adapter starts no private server at all.
// This is the incident's needs_auth failure, from the other side.
func TestOpencodeNoBacklogMCPDoesNotStartOrWaitForAServer(t *testing.T) {
	shortMCPTimeouts(t, 5*time.Second, 5*time.Millisecond)

	srv, polls, started := noBacklogStatusServer(t, "needs_auth") // the incident's status
	writeFakeOpencode(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := (opencode{}).Invoke(ctx, Invocation{
		Prompt:       "Review the change.",
		WorkDir:      ambientClankerbarProject(t),
		NoBacklogMCP: true,
		// No MCPConfigPath: exactly what review run hands over.
		Env: []string{"FAKE_OPENCODE_URL=" + srv.URL},
	})
	if err != nil {
		t.Fatalf("Invoke: %v (a no-backlog session must not wait for the server)", err)
	}
	if !started.Load() {
		t.Fatal("the run command never started")
	}
	if got := polls.Load(); got != 0 {
		t.Errorf("the adapter polled the MCP status %d times; a no-backlog session has nothing to wait for", got)
	}
	if res.FinalMessage != "OK" {
		t.Errorf("FinalMessage = %q, want OK", res.FinalMessage)
	}
}

// The switch resolves the layers against the environment opencode will see, not
// only the adapter's own: a caller-declared OPENCODE_CONFIG_DIR (which exec lets
// win) must not name a config dir the switch never read.
func TestOpencodeNoBacklogMCPResolvesCallerDeclaredLayers(t *testing.T) {
	dir := t.TempDir()
	writeAt(t, filepath.Join(dir, "opencode.json"),
		`{"mcp":{"clankerbar":{"type":"remote","url":"https://cb.example/mcp"}}}`)
	env, err := opencode{}.env(Invocation{
		WorkDir:      t.TempDir(),
		NoBacklogMCP: true,
		Env:          []string{"OPENCODE_CONFIG_DIR=" + dir},
	})
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	if !strings.Contains(lastValue(env, "OPENCODE_CONFIG_CONTENT"), `"disabled":true`) {
		t.Errorf("the caller-declared config dir's clankerbar server was not switched off: %s",
			lastValue(env, "OPENCODE_CONFIG_CONTENT"))
	}
}

// A declared OPENCODE_CONFIG_CONTENT replaces the adapter's content layer, and
// with it the off switch. That must fail closed rather than silently hand the
// review the backlog tools.
func TestOpencodeNoBacklogMCPRefusesAnOverridingContentLayer(t *testing.T) {
	_, err := opencode{}.env(Invocation{
		WorkDir:      ambientClankerbarProject(t),
		NoBacklogMCP: true,
		Env:          []string{`OPENCODE_CONFIG_CONTENT={"mcp":{}}`},
	})
	if err == nil {
		t.Fatal("env accepted a session whose declared content layer replaces the backlog disable")
	}
	if !strings.Contains(err.Error(), "OPENCODE_CONFIG_CONTENT") {
		t.Errorf("the error does not name the overriding variable: %v", err)
	}
}

// The unreadable-layer gate still applies: a config file that cannot be read
// leaves the effective set unknown, so the switch cannot be trusted to have
// covered every server.
func TestOpencodeNoBacklogMCPFailsClosedOnAnUnreadableLayer(t *testing.T) {
	work := t.TempDir()
	writeAt(t, filepath.Join(work, "opencode.json"), `{"mcp": {not json`)
	if _, err := (opencode{}).env(Invocation{WorkDir: work, NoBacklogMCP: true}); err == nil {
		t.Fatal("env accepted a no-backlog session whose config layers could not be read")
	}
}
