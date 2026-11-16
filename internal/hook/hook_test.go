package hook

// The hook's contract is narrow, so the tests are the contract: the three
// activity mappings, redaction before send, the no-op payloads, sequence
// continuity across processes, and the send bound.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type recorded struct {
	path string
	auth string
	body []byte
}

type capture struct {
	mu       sync.Mutex
	requests []recorded
	status   int
	reply    string
	block    chan struct{}
}

func newCapture() *capture { return &capture{status: http.StatusOK, reply: `{"accepted":1}`} }

func (c *capture) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.requests = append(c.requests, recorded{
			path: r.URL.Path,
			auth: r.Header.Get("Authorization"),
			body: body,
		})
		block := c.block
		c.mu.Unlock()
		if block != nil {
			<-block
		}
		w.WriteHeader(c.status)
		_, _ = io.WriteString(w, c.reply)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (c *capture) all() []recorded {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]recorded(nil), c.requests...)
}

func (c *capture) one(t *testing.T) recorded {
	t.Helper()
	got := c.all()
	if len(got) != 1 {
		t.Fatalf("got %d ingest request(s), want 1", len(got))
	}
	return got[0]
}

type wireBatch struct {
	Harness string           `json:"harness"`
	Events  []map[string]any `json:"events"`
}

func decode(t *testing.T, r recorded) wireBatch {
	t.Helper()
	var b wireBatch
	if err := json.Unmarshal(r.body, &b); err != nil {
		t.Fatalf("decode batch: %v (%s)", err, r.body)
	}
	if len(b.Events) != 1 {
		t.Fatalf("got %d event(s), want 1: %s", len(b.Events), r.body)
	}
	return b
}

func testConfig(t *testing.T, srv *httptest.Server) Config {
	t.Helper()
	return Config{
		RunID:   "11111111-2222-3333-4444-555555555555",
		APIKey:  "clk_test_key_12345678",
		Origin:  srv.URL,
		SeqPath: filepath.Join(t.TempDir(), "run-event-seq.json"),
		Logf:    func(string, ...any) {},
	}
}

func emit(t *testing.T, cfg Config, payload string) bool {
	t.Helper()
	return Emit(cfg, strings.NewReader(payload))
}

func payload(tool, input, id string) string {
	return `{"session_id":"s1","hook_event_name":"PostToolUse","tool_name":"` + tool +
		`","tool_input":` + input + `,"tool_use_id":"` + id + `"}`
}

func TestEmitMapsAPostToolUseToToolCall(t *testing.T) {
	c := newCapture()
	srv := c.server(t)
	cfg := testConfig(t, srv)

	if !emit(t, cfg, payload("Bash", `{"command":"pnpm test"}`, "toolu_1")) {
		t.Fatal("Emit reported no event")
	}

	got := c.one(t)
	if got.path != "/api/runs/"+cfg.RunID+"/events" {
		t.Errorf("path = %q, want the run's ingest endpoint", got.path)
	}
	if got.auth != "Bearer "+cfg.APIKey {
		t.Errorf("authorization = %q, want the session's key", got.auth)
	}
	b := decode(t, got)
	if b.Harness != "claude" {
		t.Errorf("harness = %q, want claude", b.Harness)
	}
	e := b.Events[0]
	if e["type"] != "tool_call" || e["tool"] != "Bash" || e["callId"] != "toolu_1" || e["target"] != "pnpm test" {
		t.Errorf("event = %v, want a tool_call with the command as target", e)
	}
	if e["seq"] != float64(1) {
		t.Errorf("seq = %v, want 1 (first event of the run)", e["seq"])
	}
}

func TestEmitMapsTodoWriteToTodoChange(t *testing.T) {
	c := newCapture()
	srv := c.server(t)
	cfg := testConfig(t, srv)

	emit(t, cfg, payload("TodoWrite",
		`{"todos":[{"content":"write the test","status":"completed"},{"content":"wire the hook","status":"in_progress"}]}`,
		"toolu_todo"))

	e := decode(t, c.one(t)).Events[0]
	if e["type"] != "todo_change" {
		t.Fatalf("type = %v, want todo_change", e["type"])
	}
	todos, ok := e["todos"].([]any)
	if !ok || len(todos) != 2 {
		t.Fatalf("todos = %v, want the two-item snapshot", e["todos"])
	}
	first := todos[0].(map[string]any)
	if first["content"] != "write the test" || first["status"] != "completed" {
		t.Errorf("first todo = %v", first)
	}
}

func TestEmitMapsAClankerbarMcpCallToTheBareToolName(t *testing.T) {
	c := newCapture()
	srv := c.server(t)
	cfg := testConfig(t, srv)

	emit(t, cfg, payload("mcp__clankerbar__claim_task", `{"taskId":"CB-605"}`, "toolu_mcp"))

	e := decode(t, c.one(t)).Events[0]
	if e["type"] != "clankerbar_mcp_call" || e["tool"] != "claim_task" || e["target"] != "CB-605" {
		t.Errorf("event = %v, want a clankerbar_mcp_call with the bare name and task id", e)
	}
}

func TestEmitRedactsKnownSecretsBeforeSending(t *testing.T) {
	c := newCapture()
	srv := c.server(t)
	cfg := testConfig(t, srv)

	secret := "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ012345"
	emit(t, cfg, payload("Bash", `{"command":"curl -H 'Authorization: Bearer `+secret+`' https://example.test"}`, "toolu_redact"))

	got := c.one(t)
	if strings.Contains(string(got.body), secret) || strings.Contains(string(got.body), "ghp_") {
		t.Fatalf("the token survived into the request: %s", got.body)
	}
	if !strings.Contains(string(got.body), "[redacted]") {
		t.Fatalf("no redaction marker in the request: %s", got.body)
	}
	e := decode(t, got).Events[0]
	if e["redacted"] != true {
		t.Errorf("redacted marker = %v, want true", e["redacted"])
	}
}

func TestEmitRedactsTodoContent(t *testing.T) {
	c := newCapture()
	srv := c.server(t)
	cfg := testConfig(t, srv)

	emit(t, cfg, payload("TodoWrite",
		`{"todos":[{"content":"use clk_ABCDEFGH12345678 to call the plane","status":"pending"}]}`,
		"toolu_todo_redact"))

	got := c.one(t)
	if strings.Contains(string(got.body), "clk_ABCDEFGH12345678") {
		t.Fatalf("the key survived into the request: %s", got.body)
	}
	if !strings.Contains(string(got.body), "[redacted]") {
		t.Fatalf("no redaction marker in the request: %s", got.body)
	}
}

func TestEmitContinuesTheSequenceAcrossInvocations(t *testing.T) {
	c := newCapture()
	srv := c.server(t)
	cfg := testConfig(t, srv)

	emit(t, cfg, payload("Bash", `{"command":"one"}`, "toolu_1"))
	emit(t, cfg, payload("Bash", `{"command":"two"}`, "toolu_2"))

	got := c.all()
	if len(got) != 2 {
		t.Fatalf("got %d requests, want 2", len(got))
	}
	for i, want := range []float64{1, 2} {
		if seq := decode(t, got[i]).Events[0]["seq"]; seq != want {
			t.Errorf("request %d seq = %v, want %v", i, seq, want)
		}
	}
}

func TestEmitSerializesConcurrentHooksIntoAscendingSeqs(t *testing.T) {
	c := newCapture()
	srv := c.server(t)
	cfg := testConfig(t, srv)

	const n = 5
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			emit(t, cfg, payload("Bash", `{"command":"parallel"}`, "toolu_"+string(rune('a'+i))))
		}(i)
	}
	wg.Wait()

	got := c.all()
	if len(got) != n {
		t.Fatalf("got %d requests, want %d", len(got), n)
	}
	// The lock is held across the send, so arrival order must match sequence
	// order; a lower seq arriving after a higher one would be dropped by the
	// plane as a duplicate.
	for i, r := range got {
		if seq := decode(t, r).Events[0]["seq"]; seq != float64(i+1) {
			t.Errorf("arrival %d carried seq %v, want %d", i, seq, i+1)
		}
	}
}

func TestEmitNoOpsOnANonPostToolUsePayload(t *testing.T) {
	c := newCapture()
	srv := c.server(t)
	cfg := testConfig(t, srv)

	if emit(t, cfg, `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"x"},"tool_use_id":"t"}`) {
		t.Error("Emit reported an event for PreToolUse")
	}
	if got := c.all(); len(got) != 0 {
		t.Fatalf("sent %d request(s) for a PreToolUse payload", len(got))
	}
}

func TestEmitNoOpsOnAPayloadWithNoTool(t *testing.T) {
	c := newCapture()
	srv := c.server(t)
	cfg := testConfig(t, srv)

	if emit(t, cfg, `{"hook_event_name":"PostToolUse"}`) {
		t.Error("Emit reported an event for a payload with no tool")
	}
	if got := c.all(); len(got) != 0 {
		t.Fatalf("sent %d request(s) for a payload with no tool", len(got))
	}
}

func TestEmitNoOpsOnMalformedJSON(t *testing.T) {
	c := newCapture()
	srv := c.server(t)
	cfg := testConfig(t, srv)

	if emit(t, cfg, `{"hook_event_name":`) {
		t.Error("Emit reported an event for malformed JSON")
	}
	if got := c.all(); len(got) != 0 {
		t.Fatalf("sent %d request(s) for malformed JSON", len(got))
	}
}

// The lock wait is a stated bound: a hook that cannot take the lock drops its
// event rather than queueing behind a slow send, and it must not touch the wire
// before the drop. flock is per open file description, so this same process
// contends with itself.
func TestEmitDropsTheEventWhenTheLockIsHeld(t *testing.T) {
	c := newCapture()
	srv := c.server(t)
	cfg := testConfig(t, srv)
	cfg.LockWait = 50 * time.Millisecond

	unlock, ok := acquireLock(cfg.SeqPath+".lock", time.Second)
	if !ok {
		t.Fatal("could not take the sequence lock for the test")
	}
	defer unlock()

	start := time.Now()
	if emit(t, cfg, payload("Bash", `{"command":"x"}`, "t")) {
		t.Error("Emit reported an event while another hook held the lock")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Emit waited %s for the lock, want it bounded by the 50ms LockWait", elapsed)
	}
	if got := c.all(); len(got) != 0 {
		t.Fatalf("sent %d request(s) while the lock was held, want 0", len(got))
	}
}

// A session with nowhere to keep the per-run counter cannot number an event,
// so it must not send an unnumbered one.
func TestEmitNoOpsWithoutASequenceFile(t *testing.T) {
	c := newCapture()
	srv := c.server(t)
	cfg := testConfig(t, srv)
	cfg.SeqPath = ""

	if emit(t, cfg, payload("Bash", `{"command":"x"}`, "t")) {
		t.Error("Emit reported an event with no sequence file")
	}
	if got := c.all(); len(got) != 0 {
		t.Fatalf("sent %d request(s) with no sequence file, want 0", len(got))
	}
}

func TestEmitBoundsASlowSend(t *testing.T) {
	c := newCapture()
	c.block = make(chan struct{})
	srv := c.server(t)
	cfg := testConfig(t, srv)
	cfg.SendTimeout = 100 * time.Millisecond
	cfg.CloseGrace = 400 * time.Millisecond

	start := time.Now()
	emit(t, cfg, payload("Bash", `{"command":"slow"}`, "toolu_slow"))
	elapsed := time.Since(start)
	close(c.block) // release the handler before the server is closed

	if elapsed > time.Second {
		t.Fatalf("a slow send blocked the hook for %s, want it bounded by the 100ms timeout", elapsed)
	}
}

func TestEmitTreatsTheDialRefusalAsANoop(t *testing.T) {
	c := newCapture()
	c.status = http.StatusForbidden
	c.reply = `{"error":{"code":"events_disabled","message":"The project's events.activity dial is off."}}`
	srv := c.server(t)
	cfg := testConfig(t, srv)
	var logged []string
	cfg.Logf = func(format string, args ...any) { logged = append(logged, format) }

	// The plane is the dial's authority: the hook makes its one attempt, the
	// plane stores nothing and refuses, and the hook still exits cleanly.
	if !emit(t, cfg, payload("Bash", `{"command":"x"}`, "t")) {
		t.Error("Emit did not attempt the send whose answer is the dial")
	}
	if len(c.all()) != 1 {
		t.Fatalf("got %d requests, want the one the plane refuses", len(c.all()))
	}
	if len(logged) == 0 {
		t.Error("the events_disabled refusal was not logged")
	}
}

func TestEmitEscapesTheCompoundRunCredential(t *testing.T) {
	c := newCapture()
	srv := c.server(t)
	cfg := testConfig(t, srv)
	cfg.RunID = "11111111-2222-3333-4444-555555555555.secret-part_1"

	emit(t, cfg, payload("Bash", `{"command":"x"}`, "t"))

	if got := c.one(t).path; got != "/api/runs/"+cfg.RunID+"/events" {
		t.Errorf("path = %q, want the compound credential in the address", got)
	}
}
