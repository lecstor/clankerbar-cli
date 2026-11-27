package mcpbridge

// The bridge tests run the real Run against an httptest.Server: the point is
// the wire behaviour (headers, JSON vs SSE, status mapping, concurrency, large
// payloads), not a mock of the transport.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/auth"
)

func runBridge(t *testing.T, input, target, origin string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut strings.Builder
	err = Run(context.Background(), Config{
		Target: target,
		Origin: origin,
		In:     strings.NewReader(input),
		Out:    &out,
		Err:    &errOut,
	})
	return out.String(), errOut.String(), err
}

// outputLines splits stdout into lines and fails on anything that is not a
// JSON object/array: the pipe belongs to the MCP client.
func outputLines(t *testing.T, out string) []json.RawMessage {
	t.Helper()
	if out == "" {
		return nil
	}
	var lines []json.RawMessage
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if line == "" {
			t.Fatalf("stdout carries an empty line:\n%q", out)
		}
		if !json.Valid([]byte(line)) {
			t.Fatalf("stdout line is not JSON: %q", line)
		}
		if strings.ContainsAny(line, "\r\n") {
			t.Fatalf("stdout line carries a line break: %q", line)
		}
		lines = append(lines, json.RawMessage(line))
	}
	return lines
}

func lineByID(t *testing.T, lines []json.RawMessage, wantID string) json.RawMessage {
	t.Helper()
	for _, line := range lines {
		var probe struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			t.Fatalf("decode response: %v\n%s", err, line)
		}
		if string(probe.ID) == wantID {
			return line
		}
	}
	t.Fatalf("no response with id %s in %v", wantID, lines)
	return nil
}

func errorOf(t *testing.T, line json.RawMessage) (code int, message string) {
	t.Helper()
	var resp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("decode error response: %v\n%s", err, line)
	}
	if resp.Error == nil {
		t.Fatalf("response carries no error: %s", line)
	}
	return resp.Error.Code, resp.Error.Message
}

const initRequest = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`

// A JSON body is relayed as one compact line, the credential and protocol
// headers are exactly what the plane expects, and a notification's body never
// reaches stdout.
func TestBridgeRelaysJSONAndSendsHeaders(t *testing.T) {
	t.Setenv(auth.EnvAPIKey, "test-key")

	type captured struct {
		method, auth, accept, contentType, version string
	}
	var mu sync.Mutex
	var seen []captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(body, &probe); err != nil {
			t.Errorf("server received non-JSON: %v", err)
		}
		mu.Lock()
		seen = append(seen, captured{
			method:      probe.Method,
			auth:        r.Header.Get("Authorization"),
			accept:      r.Header.Get("Accept"),
			contentType: r.Header.Get("Content-Type"),
			version:     r.Header.Get("MCP-Protocol-Version"),
		})
		mu.Unlock()
		if probe.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// A pretty-printed body proves the bridge compacts it to one line.
		payload, _ := json.MarshalIndent(map[string]any{
			"jsonrpc": "2.0",
			"id":      probe.ID,
			"result":  map[string]any{"method": probe.Method},
		}, "", "  ")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	input := strings.Join([]string{
		initRequest,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
	}, "\n") + "\n"
	out, errOut, err := runBridge(t, input, srv.URL+"/mcp", srv.URL)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if errOut != "" {
		t.Errorf("unexpected diagnostics on stderr:\n%s", errOut)
	}
	lines := outputLines(t, out)
	if len(lines) != 2 {
		t.Fatalf("got %d stdout lines, want 2 (initialize + tools/list):\n%s", len(lines), out)
	}
	lineByID(t, lines, "1")
	lineByID(t, lines, "2")

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 {
		t.Fatalf("server saw %d requests, want 3", len(seen))
	}
	for _, got := range seen {
		if got.auth != "Bearer test-key" {
			t.Errorf("%s Authorization = %q", got.method, got.auth)
		}
		if got.accept != "application/json, text/event-stream" {
			t.Errorf("%s Accept = %q", got.method, got.accept)
		}
		if got.contentType != "application/json" {
			t.Errorf("%s Content-Type = %q", got.method, got.contentType)
		}
		if got.method == "initialize" && got.version != "" {
			t.Errorf("initialize carried MCP-Protocol-Version %q; the version belongs in the body", got.version)
		}
		if got.method != "initialize" && got.version != "2025-03-26" {
			t.Errorf("%s MCP-Protocol-Version = %q, want the negotiated version", got.method, got.version)
		}
	}
}

// A notification answer is a 202 with no body; either way nothing may appear on
// stdout.
func TestBridgeNotificationWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		handler func(http.ResponseWriter)
	}{
		{name: "202", status: http.StatusAccepted, handler: func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusAccepted)
		}},
		{name: "200 empty", status: http.StatusOK, handler: func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				tc.handler(w)
			}))
			defer srv.Close()
			out, errOut, err := runBridge(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n", srv.URL+"/mcp", srv.URL)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if out != "" {
				t.Errorf("notification produced stdout:\n%q", out)
			}
			if errOut != "" {
				t.Errorf("unexpected stderr:\n%s", errOut)
			}
		})
	}
}

// text/event-stream carries the same payloads, one per event, including events
// whose JSON spans several data: lines.
func TestBridgeRelaysEventStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = io.WriteString(w, "event: message\r\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"a\":1}}\r\n\r\n")
		_, _ = io.WriteString(w, ": keepalive\r\n\r\n")
		_, _ = io.WriteString(w, "data: {\"jsonrpc\":\"2.0\",\"id\":1,\n")
		_, _ = io.WriteString(w, "data: \"result\":null}\n\n")
	}))
	defer srv.Close()

	out, errOut, err := runBridge(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`+"\n", srv.URL+"/mcp", srv.URL)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if errOut != "" {
		t.Errorf("unexpected stderr:\n%s", errOut)
	}
	lines := outputLines(t, out)
	if len(lines) != 2 {
		t.Fatalf("got %d event lines, want 2:\n%s", len(lines), out)
	}
	lineByID(t, lines, "1")
	if !strings.Contains(string(lines[1]), `"result":null`) {
		t.Errorf("second event = %s", lines[1])
	}
}

// A line well past 64 KiB in both directions must survive the stdin scanner and
// the stdout relay.
func TestBridgeHandlesLargePayloads(t *testing.T) {
	t.Setenv(auth.EnvAPIKey, "test-key")
	const blobLen = 200 << 10
	blob := strings.Repeat("x", blobLen)
	var requestBytes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestBytes = len(body)
		var probe struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(body, &probe)
		w.Header().Set("Content-Type", "application/json")
		payload, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      probe.ID,
			"result":  map[string]any{"content": []any{map[string]any{"type": "text", "text": blob}}},
		})
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	blobJSON, _ := json.Marshal(blob)
	input := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"blob":%s}}}`, blobJSON) + "\n"
	out, errOut, err := runBridge(t, input, srv.URL+"/mcp", srv.URL)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if errOut != "" {
		t.Errorf("unexpected stderr:\n%s", errOut)
	}
	if requestBytes < blobLen {
		t.Errorf("server received %d bytes, want at least the %d-byte argument", requestBytes, blobLen)
	}
	lines := outputLines(t, out)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	if len(lines[0]) <= 64<<10 {
		t.Errorf("relayed response is %d bytes; the large payload was lost", len(lines[0]))
	}
	if !strings.Contains(string(lines[0]), blob[:64]) {
		t.Error("relayed response does not carry the payload")
	}
}

// Concurrent requests get concurrent answers, each on its own whole line.
func TestBridgeConcurrentResponsesDoNotInterleave(t *testing.T) {
	t.Setenv(auth.EnvAPIKey, "test-key")
	const n = 8
	var inFlight, maxInFlight int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &probe)
		cur := atomic.AddInt32(&inFlight, 1)
		for {
			max := atomic.LoadInt32(&maxInFlight)
			if cur <= max || atomic.CompareAndSwapInt32(&maxInFlight, max, cur) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		w.Header().Set("Content-Type", "application/json")
		payload, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      probe.ID,
			"result":  map[string]any{"method": probe.Method},
		})
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	var input strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&input, `{"jsonrpc":"2.0","id":%d,"method":"tools/list","params":{}}`+"\n", i)
	}
	out, errOut, err := runBridge(t, input.String(), srv.URL+"/mcp", srv.URL)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if errOut != "" {
		t.Errorf("unexpected stderr:\n%s", errOut)
	}
	lines := outputLines(t, out)
	if len(lines) != n {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), n, out)
	}
	for i := 1; i <= n; i++ {
		lineByID(t, lines, fmt.Sprint(i))
	}
	if got := atomic.LoadInt32(&maxInFlight); got < 2 {
		t.Errorf("max concurrent plane requests = %d; the bridge did not pipeline", got)
	}
}

// Keyless discovery works with no credential; anything else answers with a
// well-formed JSON-RPC error naming `clankerbar login` instead of hanging.
func TestBridgeMissingAuth(t *testing.T) {
	t.Setenv(auth.EnvAPIKey, "")
	var mu sync.Mutex
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &probe)
		mu.Lock()
		methods = append(methods, probe.Method)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		payload, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      probe.ID,
			"result":  map[string]any{"method": probe.Method},
		})
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	input := strings.Join([]string{
		initRequest,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"next_task","arguments":{}}}`,
	}, "\n") + "\n"
	out, errOut, err := runBridge(t, input, srv.URL+"/mcp", srv.URL)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	lines := outputLines(t, out)
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3:\n%s", len(lines), out)
	}
	lineByID(t, lines, "1")
	lineByID(t, lines, "2")
	code, message := errorOf(t, lineByID(t, lines, "3"))
	if code != codeUnauthenticated || !strings.Contains(message, "clankerbar login") {
		t.Errorf("missing-auth error = %d %q", code, message)
	}
	if !strings.Contains(errOut, "clankerbar login") {
		t.Errorf("stderr does not carry the login hint:\n%s", errOut)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(methods) != 2 {
		t.Errorf("server saw %v; the unauthenticated tools/call must not be forwarded", methods)
	}
}

// HTTP failures become JSON-RPC errors that preserve the request id.
func TestBridgeHTTPFailureBecomesJSONRPCError(t *testing.T) {
	t.Setenv(auth.EnvAPIKey, "test-key")
	cases := []struct {
		name     string
		status   int
		wantCode int
		wantText string
		wantBody bool
	}{
		{name: "401", status: http.StatusUnauthorized, wantCode: codeUnauthenticated, wantText: "clankerbar login"},
		{name: "403", status: http.StatusForbidden, wantCode: codeUnauthenticated, wantText: "clankerbar login"},
		{name: "500", status: http.StatusInternalServerError, wantCode: codeInternal, wantText: "500", wantBody: true},
		{name: "503", status: http.StatusServiceUnavailable, wantCode: codeInternal, wantText: "503", wantBody: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, "boom")
			}))
			defer srv.Close()
			out, _, err := runBridge(t, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{}}`+"\n", srv.URL+"/mcp", srv.URL)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			lines := outputLines(t, out)
			if len(lines) != 1 {
				t.Fatalf("got %d lines, want 1:\n%s", len(lines), out)
			}
			code, message := errorOf(t, lineByID(t, lines, "9"))
			if code != tc.wantCode || !strings.Contains(message, tc.wantText) {
				t.Errorf("error = %d %q, want code %d mentioning %q", code, message, tc.wantCode, tc.wantText)
			}
			if tc.wantBody && !strings.Contains(message, "boom") {
				t.Errorf("error does not quote the server body: %q", message)
			}
		})
	}
}

// A dead plane is an answer too: the client gets an error rather than a hang.
func TestBridgeTransportFailureBecomesJSONRPCError(t *testing.T) {
	t.Setenv(auth.EnvAPIKey, "test-key")
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	target := srv.URL + "/mcp"
	srv.Close() // nothing is listening now

	out, _, err := runBridge(t, `{"jsonrpc":"2.0","id":5,"method":"tools/list","params":{}}`+"\n", target, strings.TrimSuffix(target, "/mcp"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	lines := outputLines(t, out)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1:\n%s", len(lines), out)
	}
	code, message := errorOf(t, lineByID(t, lines, "5"))
	if code != codeInternal || !strings.Contains(message, "call ") {
		t.Errorf("transport error = %d %q", code, message)
	}
}

// A cross-origin redirect must never be followed: the second server sees no
// request at all, and the client gets a JSON-RPC error.
func TestBridgeRefusesCrossOriginRedirect(t *testing.T) {
	t.Setenv(auth.EnvAPIKey, "test-key")
	var leaked int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&leaked, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer other.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/mcp", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	out, _, err := runBridge(t, `{"jsonrpc":"2.0","id":4,"method":"tools/list","params":{}}`+"\n", redirector.URL+"/mcp", redirector.URL)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	lines := outputLines(t, out)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1:\n%s", len(lines), out)
	}
	code, message := errorOf(t, lineByID(t, lines, "4"))
	if code != codeInternal || !strings.Contains(message, "redirect") {
		t.Errorf("redirect error = %d %q", code, message)
	}
	if got := atomic.LoadInt32(&leaked); got != 0 {
		t.Errorf("the redirect target received %d request(s); the credential followed a cross-origin hop", got)
	}
}

// Invalid input gets the standard parse-error response, not silence.
func TestBridgeInvalidJSONGetsParseError(t *testing.T) {
	out, _, err := runBridge(t, "not json\n", "http://127.0.0.1:1/mcp", "http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	lines := outputLines(t, out)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1:\n%s", len(lines), out)
	}
	var resp struct {
		ID    json.RawMessage `json:"id"`
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(lines[0], &resp); err != nil {
		t.Fatal(err)
	}
	if string(resp.ID) != "null" || resp.Error.Code != codeParse {
		t.Errorf("parse error response = %s", lines[0])
	}
}

// EOF drains in-flight work before Run returns; the response written to stdout
// must be complete, not lost to process exit.
func TestBridgeDrainsInFlightBeforeEOFReturns(t *testing.T) {
	t.Setenv(auth.EnvAPIKey, "test-key")
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(body, &probe)
		once.Do(func() { close(started) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"ok":true}}`, probe.ID)
	}))
	defer srv.Close()

	var out strings.Builder
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), Config{
			Target: srv.URL + "/mcp",
			Origin: srv.URL,
			In:     strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}` + "\n"),
			Out:    &out,
			Err:    io.Discard,
		})
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the request never reached the server")
	}
	select {
	case err := <-done:
		t.Fatalf("Run returned while a request was still in flight: %v", err)
	default:
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after draining")
	}
	if !strings.Contains(out.String(), `"result":{"ok":true}`) {
		t.Errorf("the drained response never reached stdout: %q", out.String())
	}
}

// A cancelled context (SIGINT/SIGTERM in main) is a clean exit, not an error.
func TestBridgeCancellationReturnsCleanly(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{
			Target: "http://127.0.0.1:1/mcp",
			Origin: "http://127.0.0.1:1",
			In:     pr,
			Out:    io.Discard,
			Err:    io.Discard,
		})
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v after cancellation, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

// The stored login is the point: with no CLANKERBAR_API_KEY in the environment,
// the token comes from internal/auth instead.
func TestBridgeResolvesStoredCredential(t *testing.T) {
	t.Setenv(auth.EnvAPIKey, "")
	store := filepath.Join(t.TempDir(), "auth.json")
	t.Setenv(auth.EnvAuthFile, store)

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer srv.Close()
	if err := auth.Save(srv.URL, auth.Credential{Token: "clk_stored", Scope: auth.ScopeUser}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	out, errOut, err := runBridge(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`+"\n", srv.URL+"/mcp", srv.URL)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if errOut != "" {
		t.Errorf("unexpected stderr:\n%s", errOut)
	}
	outputLines(t, out)
	if gotAuth != "Bearer clk_stored" {
		t.Errorf("Authorization = %q, want the stored credential", gotAuth)
	}
	if strings.Contains(out, "clk_stored") {
		t.Error("the token was echoed to stdout")
	}
}
