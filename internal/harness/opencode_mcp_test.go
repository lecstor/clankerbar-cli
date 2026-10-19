package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// writeFakeOpencode puts a stand-in `opencode` on PATH. The stand-in answers
// `serve` by announcing the test server's address and password, and `run` by
// proving it was only called after the MCP status flipped to connected.
func writeFakeOpencode(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
serve)
  echo "server listening on $FAKE_OPENCODE_URL"
  echo "server password test-password"
  sleep 60
  ;;
run)
  curl -fsS "$FAKE_OPENCODE_URL/run-started" >/dev/null || exit 7
  printf '%s\n' '{"type":"step_start","sessionID":"ses_test","part":{"type":"step-start"}}'
  printf '%s\n' '{"type":"text","sessionID":"ses_test","part":{"type":"text","text":"OK"}}'
  printf '%s\n' '{"type":"step_finish","sessionID":"ses_test","part":{"type":"step-finish","reason":"stop","tokens":{"total":10,"input":8,"output":2},"cost":0.001}}'
  ;;
session)
  # The post-run usage export is best-effort; make it fail so the stream figures
  # stand and the test does not depend on a real opencode data dir.
  exit 1
  ;;
*)
  exit 0
  ;;
esac
`
	path := filepath.Join(dir, "opencode")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// mcpTestServer serves /api/mcp and /run-started. The first connectAfter polls
// report pending, then connected; /run-started records whether it was reached
// before that flip.
func mcpTestServer(t *testing.T, connectAfter int32) (*httptest.Server, *atomic.Int32, *atomic.Bool, *atomic.Bool) {
	t.Helper()
	var polls atomic.Int32
	var connected atomic.Bool
	var startedBeforeConnected atomic.Bool
	var started atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/api/mcp", func(w http.ResponseWriter, r *http.Request) {
		n := polls.Add(1)
		if connectAfter > 0 && n >= connectAfter {
			connected.Store(true)
		}
		status := "pending"
		if connected.Load() {
			status = "connected"
		}
		w.Header().Set("Content-Type", "application/json")
		dir := r.URL.Query().Get("location[directory]")
		fmt.Fprintf(w, `{"location":{"directory":%q},"data":[{"name":"clankerbar","status":{"status":%q}}]}`, dir, status)
	})
	mux.HandleFunc("/run-started", func(w http.ResponseWriter, r *http.Request) {
		started.Store(true)
		if !connected.Load() {
			startedBeforeConnected.Store(true)
			http.Error(w, "not connected", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &polls, &started, &startedBeforeConnected
}

func writeMCPConfig(t *testing.T) string {
	t.Helper()
	return writeMCPConfigDoc(t, `{
  "mcp": {
    "servers": {
      "clankerbar": {
        "type": "remote",
        "url": "https://clankerbar.example/mcp",
        "headers": {"Authorization": "Bearer {env:CLANKERBAR_API_KEY}"}
      }
    }
  }
}`)
}

func writeMCPConfigDoc(t *testing.T, doc string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.json")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeAt writes body at path, creating the parent directory.
func writeAt(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// shortMCPTimeouts shrinks the preflight bounds for one test and restores them.
func shortMCPTimeouts(t *testing.T, timeout, poll time.Duration) {
	t.Helper()
	oldTimeout, oldPoll := opencodeMCPStartupTimeout, opencodeMCPPollInterval
	opencodeMCPStartupTimeout, opencodeMCPPollInterval = timeout, poll
	t.Cleanup(func() { opencodeMCPStartupTimeout, opencodeMCPPollInterval = oldTimeout, oldPoll })
}

// The first model turn must not be submitted until the clankerbar server has
// reported connected. The fake `run` refuses to start unless the test server
// has flipped, so a regression that spawns the run early fails on exit 7.
func TestOpencodeWaitsForMCPBeforeTheFirstTurn(t *testing.T) {
	oldTimeout, oldPoll := opencodeMCPStartupTimeout, opencodeMCPPollInterval
	opencodeMCPStartupTimeout, opencodeMCPPollInterval = 5*time.Second, 5*time.Millisecond
	t.Cleanup(func() { opencodeMCPStartupTimeout, opencodeMCPPollInterval = oldTimeout, oldPoll })

	srv, _, started, startedBeforeConnected := mcpTestServer(t, 3)
	writeFakeOpencode(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := (opencode{}).Invoke(ctx, Invocation{
		Prompt:        "Review the change.",
		WorkDir:       t.TempDir(),
		MCPConfigPath: writeMCPConfig(t),
		Env:           []string{"FAKE_OPENCODE_URL=" + srv.URL},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !started.Load() {
		t.Fatal("the run command never started")
	}
	if startedBeforeConnected.Load() {
		t.Fatal("the prompt was submitted before the clankerbar MCP server reported connected")
	}
	if res.FinalMessage != "OK" {
		t.Errorf("FinalMessage = %q, want OK", res.FinalMessage)
	}
}

// A server that never connects must fail the session with ErrMCPNotConnected —
// a named infrastructure error the driver can retry — and must never run the
// model turn. The error is not a silent zero Result.
func TestOpencodeMCPWaitTimeoutIsAnInfrastructureError(t *testing.T) {
	oldTimeout, oldPoll := opencodeMCPStartupTimeout, opencodeMCPPollInterval
	// Long enough to start the fake server and read its announcement; the point
	// is that the MCP status never flips, not that the server is slow to start.
	opencodeMCPStartupTimeout, opencodeMCPPollInterval = 1*time.Second, 10*time.Millisecond
	t.Cleanup(func() { opencodeMCPStartupTimeout, opencodeMCPPollInterval = oldTimeout, oldPoll })

	srv, _, started, _ := mcpTestServer(t, 0) // never flips
	writeFakeOpencode(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := (opencode{}).Invoke(ctx, Invocation{
		Prompt:        "Review the change.",
		WorkDir:       t.TempDir(),
		MCPConfigPath: writeMCPConfig(t),
		Env:           []string{"FAKE_OPENCODE_URL=" + srv.URL},
	})
	if !errors.Is(err, ErrMCPNotConnected) {
		t.Fatalf("Invoke error = %v, want ErrMCPNotConnected", err)
	}
	if !strings.Contains(err.Error(), "clankerbar") || !strings.Contains(err.Error(), "did not connect") {
		t.Errorf("the infrastructure error does not name the server and the failure: %v", err)
	}
	if started.Load() {
		t.Fatal("the model turn ran even though the MCP server never connected")
	}
}

// mcpStatusServer answers /api/mcp with one fixed status and records whether
// the run ever started. omitLocation drops `location` from the response, the
// shape finding 2.1 says must not release the first turn.
func mcpStatusServer(t *testing.T, status string, omitLocation bool) (*httptest.Server, *atomic.Bool) {
	t.Helper()
	var started atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/api/mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if omitLocation {
			fmt.Fprintf(w, `{"data":[{"name":"clankerbar","status":{"status":%q}}]}`, status)
			return
		}
		fmt.Fprintf(w, `{"location":{"directory":%q},"data":[{"name":"clankerbar","status":{"status":%q}}]}`,
			r.URL.Query().Get("location[directory]"), status)
	})
	mux.HandleFunc("/run-started", func(w http.ResponseWriter, r *http.Request) {
		started.Store(true)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &started
}

// CLA-703 review, finding 1.1: with no named MCP config, OPENCODE_CONFIG_DIR is
// the fallback layer that loads the clankerbar server. The preflight must
// resolve it and hold the first turn until it connects, not leave --standalone
// with no wait at all.
func TestOpencodeWaitsForMCPFromTheConfigDir(t *testing.T) {
	shortMCPTimeouts(t, 5*time.Second, 5*time.Millisecond)

	srv, _, started, startedBeforeConnected := mcpTestServer(t, 3)
	writeFakeOpencode(t)
	cfgDir := t.TempDir()
	writeAt(t, filepath.Join(cfgDir, "opencode.json"), `{"mcp":{"servers":{"clankerbar":{"type":"remote","url":"https://cb.example/mcp"}}}}`)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := (opencode{}).Invoke(ctx, Invocation{
		Prompt:  "Review the change.",
		WorkDir: t.TempDir(),
		// No MCPConfigPath: the server comes from the config dir only.
		ConfigDir: cfgDir,
		Env:       []string{"FAKE_OPENCODE_URL=" + srv.URL},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !started.Load() {
		t.Fatal("the run command never started")
	}
	if startedBeforeConnected.Load() {
		t.Fatal("the prompt was submitted before the config-dir clankerbar server reported connected")
	}
	if res.FinalMessage != "OK" {
		t.Errorf("FinalMessage = %q, want OK", res.FinalMessage)
	}
}

// CLA-703 review, finding 3.1: with no named MCP config, a PROJECT-level
// opencode.json is a layer opencode merges, so its clankerbar server is waited
// for too. The suite is XDG-isolated by TestMain, so this WorkDir is the only
// ambient layer in play.
func TestOpencodeWaitsForMCPFromAmbientProjectConfig(t *testing.T) {
	shortMCPTimeouts(t, 5*time.Second, 5*time.Millisecond)

	srv, _, started, startedBeforeConnected := mcpTestServer(t, 3)
	writeFakeOpencode(t)
	work := t.TempDir()
	// JSONC, the shape these files are actually written in: this suite would be
	// lying if it only ever handed the resolver strict JSON.
	writeAt(t, filepath.Join(work, "opencode.jsonc"), `{
  // the project's backlog server
  "mcp": {"clankerbar": {"type": "remote", "url": "https://cb.example/mcp"}},
}`)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := (opencode{}).Invoke(ctx, Invocation{
		Prompt:  "Review the change.",
		WorkDir: work,
		Env:     []string{"FAKE_OPENCODE_URL=" + srv.URL},
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !started.Load() {
		t.Fatal("the run command never started")
	}
	if startedBeforeConnected.Load() {
		t.Fatal("the prompt was submitted before the ambient project-level clankerbar server reported connected")
	}
}

// CLA-703 review, finding 3.1: a layer that exists but cannot be parsed leaves
// the effective config unknown. That FAILS CLOSED — no session is spawned — and
// the error names the failure and the infrastructure class.
func TestOpencodeUnreadableAmbientConfigFailsClosed(t *testing.T) {
	shortMCPTimeouts(t, 200*time.Millisecond, 10*time.Millisecond)

	srv, started := mcpStatusServer(t, "connected", false)
	writeFakeOpencode(t)
	work := t.TempDir()
	writeAt(t, filepath.Join(work, "opencode.json"), `{"mcp": {not json`)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := (opencode{}).Invoke(ctx, Invocation{
		Prompt:  "Review the change.",
		WorkDir: work,
		Env:     []string{"FAKE_OPENCODE_URL=" + srv.URL},
	})
	if !errors.Is(err, ErrMCPNotConnected) {
		t.Fatalf("Invoke error = %v, want ErrMCPNotConnected", err)
	}
	if !strings.Contains(err.Error(), "cannot establish") {
		t.Errorf("the error does not say the effective config could not be established: %v", err)
	}
	if started.Load() {
		t.Fatal("the model turn ran despite an unestablishable effective config")
	}
}

// CLA-703 review, finding 2.1: a status that does not identify its location is
// not evidence about THIS session's location, so it must not release the first
// turn — even when it says "connected".
func TestOpencodeMCPStatusMustIdentifyItsLocation(t *testing.T) {
	shortMCPTimeouts(t, 200*time.Millisecond, 10*time.Millisecond)

	srv, started := mcpStatusServer(t, "connected", true)
	writeFakeOpencode(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := (opencode{}).Invoke(ctx, Invocation{
		Prompt:        "Review the change.",
		WorkDir:       t.TempDir(),
		MCPConfigPath: writeMCPConfig(t),
		Env:           []string{"FAKE_OPENCODE_URL=" + srv.URL},
	})
	if !errors.Is(err, ErrMCPNotConnected) {
		t.Fatalf("Invoke error = %v, want ErrMCPNotConnected", err)
	}
	if !strings.Contains(err.Error(), "did not identify its location") {
		t.Errorf("the error does not name the missing location: %v", err)
	}
	if started.Load() {
		t.Fatal("the model turn ran on a location-less status")
	}
}

// CLA-703 review, finding 4.3: a config-disabled clankerbar entry is still
// waited for, so the RUNTIME `disabled` status fails the preflight fast instead
// of letting a full paid session start with no backlog tools.
func TestOpencodeDisabledClankerbarFailsThePreflight(t *testing.T) {
	shortMCPTimeouts(t, 5*time.Second, 10*time.Millisecond)

	srv, started := mcpStatusServer(t, "disabled", false)
	writeFakeOpencode(t)
	path := writeMCPConfigDoc(t, `{"mcp":{"servers":{"clankerbar":{"type":"remote","url":"https://cb.example/mcp","disabled":true}}}}`)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	startedAt := time.Now()
	_, err := (opencode{}).Invoke(ctx, Invocation{
		Prompt:        "Review the change.",
		WorkDir:       t.TempDir(),
		MCPConfigPath: path,
		Env:           []string{"FAKE_OPENCODE_URL=" + srv.URL},
	})
	if !errors.Is(err, ErrMCPNotConnected) {
		t.Fatalf("Invoke error = %v, want ErrMCPNotConnected", err)
	}
	if !strings.Contains(err.Error(), "disabled") {
		t.Errorf("the error does not name the disabled status: %v", err)
	}
	if started.Load() {
		t.Fatal("a paid session started with a disabled clankerbar server")
	}
	if elapsed := time.Since(startedAt); elapsed >= opencodeMCPStartupTimeout {
		t.Errorf("the disabled status took %s to fail, want an immediate answer", elapsed)
	}
}

// CLA-703 review, finding 4.1 (adapter half): a run-wide cancellation inside
// the preflight window is returned AS the error, not as a clean zero Result, so
// the driver can classify it as a shutdown rather than a no_mcp miss.
func TestOpencodeMCPPreflightCancellationIsAnError(t *testing.T) {
	shortMCPTimeouts(t, 5*time.Second, 5*time.Millisecond)

	srv, _, started, _ := mcpTestServer(t, 0) // never connects
	writeFakeOpencode(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := (opencode{}).Invoke(ctx, Invocation{
		Prompt:        "Review the change.",
		WorkDir:       t.TempDir(),
		MCPConfigPath: writeMCPConfig(t),
		Env:           []string{"FAKE_OPENCODE_URL=" + srv.URL},
	})
	if err == nil {
		t.Fatal("a cancelled preflight returned a clean result")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Invoke error = %v, want context.Canceled", err)
	}
	if started.Load() {
		t.Fatal("the model turn ran after the run was cancelled")
	}
}

// The server keys its MCP state by the canonical path, so the status query has
// to use the same one. On macOS this is the /var -> /private/var symlink; a raw
// path polled a location no session used and the first turn still raced.
func TestOpencodeWorkDirResolvesSymlinks(t *testing.T) {
	dir := t.TempDir()
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got := opencodeWorkDir(link); got != want {
		t.Errorf("opencodeWorkDir(%q) = %q, want the resolved %q", link, got, want)
	}
	if got := opencodeWorkDir(""); got != "" {
		t.Errorf("opencodeWorkDir(\"\") = %q, want empty", got)
	}
}

// The run must attach to the adapter's server, never spawn a second private one:
// --standalone is removed and --server is inserted after the subcommand.
func TestWithOpencodeServer(t *testing.T) {
	args := []string{"run", "--standalone", "--format", "json", "--agent", "build", "--", "hi"}
	got := strings.Join(withOpencodeServer(args, "http://127.0.0.1:1234"), " ")
	want := "run --server http://127.0.0.1:1234 --format json --agent build -- hi"
	if got != want {
		t.Errorf("withOpencodeServer = %q, want %q", got, want)
	}
}

// The wait targets the clankerbar server(s) the config names, and not some
// unrelated MCP server in the same file or an ambient layer.
func TestOpencodeMCPServers(t *testing.T) {
	write := func(t *testing.T, doc string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "opencode.json")
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cases := []struct {
		name string
		path string
		want []string
	}{
		{
			name: "the documented name, 2.x dialect",
			path: write(t, `{"mcp":{"servers":{"clankerbar":{"type":"remote","url":"https://cb.example/mcp"},"other":{"type":"local","command":["x"]}}}}`),
			want: []string{"clankerbar"},
		},
		{
			name: "the documented name, 1.x dialect",
			path: write(t, `{"mcp":{"clankerbar":{"type":"remote","url":"https://cb.example/mcp"},"other":{"type":"local","command":["x"]}}}`),
			want: []string{"clankerbar"},
		},
		{
			name: "a renamed server handed the credential is the backlog's",
			path: write(t, `{"mcp":{"servers":{"cb":{"type":"remote","url":"https://cb.example/mcp","headers":{"Authorization":"Bearer {env:CLANKERBAR_API_KEY}"}},"other":{"type":"remote","url":"https://other.example/mcp"}}}}`),
			want: []string{"cb"},
		},
		{
			// CLA-703 review, finding 4.3: disabled entries are waited for so the
			// runtime status can fail the preflight fast instead of after a paid
			// session with no backlog tools.
			name: "a disabled clankerbar entry is still waited for",
			path: write(t, `{"mcp":{"servers":{"clankerbar":{"type":"remote","url":"https://cb.example/mcp","disabled":true}}}}`),
			want: []string{"clankerbar"},
		},
		{
			// CLA-703 review, finding 1.2: a 1.x server literally NAMED "servers"
			// is told apart from the 2.x container by its VALUE, exactly as the
			// config parser does.
			name: "a 1.x server literally named servers",
			path: write(t, `{"mcp":{"servers":{"type":"remote","url":"https://clankerbar.example/mcp","headers":{"Authorization":"Bearer {env:CLANKERBAR_API_KEY}"}}}}`),
			want: []string{"servers"},
		},
		{
			name: "a 2.x container and 1.x entries in one file",
			path: write(t, `{"mcp":{"servers":{"clankerbar":{"type":"remote","url":"https://cb.example/mcp"}},"other":{"type":"local","command":["x"]}}}`),
			want: []string{"clankerbar"},
		},
		{
			name: "no declared config means nothing to wait for",
			path: "",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := opencodeMCPServers(tc.path)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("opencodeMCPServers(%s) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// A successful clankerbar call is what proves the MCP server was usable; a
// refused call and a non-clankerbar tool do not.
func TestOpencodeParseCountsSuccessfulClankerbarCalls(t *testing.T) {
	const stream = `{"type":"tool_use","sessionID":"ses_1","part":{"partID":"prt_1","type":"tool","id":"call_0","tool":"clankerbar_get_task","state":{"status":"completed","input":{"taskId":"T-9"},"output":"{}"}}}
{"type":"tool_use","sessionID":"ses_1","part":{"partID":"prt_2","type":"tool","id":"call_1","tool":"clankerbar_claim_task","state":{"status":"error","input":{"taskId":"T-9"},"error":"No tool named \"clankerbar_claim_task\" is currently available."}}}
{"type":"tool_use","sessionID":"ses_1","part":{"partID":"prt_3","type":"tool","id":"call_2","tool":"read","state":{"status":"completed","input":{"filePath":"a.go"},"output":"ok"}}}
{"type":"tool_use","sessionID":"ses_1","part":{"partID":"prt_1","type":"tool","id":"call_0","tool":"clankerbar_get_task","state":{"status":"completed","input":{"taskId":"T-9"},"output":"{}"}}}`
	res := opencodeParsed(stream)
	if res.ClankerbarCalls != 1 {
		t.Errorf("ClankerbarCalls = %d, want 1 (one success; the refused call and the replay do not count)", res.ClankerbarCalls)
	}
}

// The claude stream reports a tool call and its result as separate events; the
// shared observer must count the successful clankerbar result there too.
func TestClaudeClankerbarCallCounter(t *testing.T) {
	var res Result
	noteToolUse(claimTaskTool, "call-1", []byte(`{"taskId":"T-9"}`), &res)
	noteToolResult(&res, "call-1", false, []byte(`{"task":{}}`), io.Discard)
	noteToolUse(claimTaskTool, "call-2", []byte(`{"taskId":"T-9"}`), &res)
	noteToolResult(&res, "call-2", true, []byte(`refused`), io.Discard)
	if res.ClankerbarCalls != 1 {
		t.Errorf("ClankerbarCalls = %d, want 1", res.ClankerbarCalls)
	}
}
