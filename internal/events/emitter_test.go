package events

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// capture is a test plane: it records every ingest request.
type capture struct {
	mu      sync.Mutex
	bodies  [][]byte
	headers []http.Header
	paths   []string
	notify  chan struct{}
}

func newCapture() *capture { return &capture{notify: make(chan struct{}, 64)} }

func (c *capture) handler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, b)
		c.headers = append(c.headers, r.Header.Clone())
		c.paths = append(c.paths, r.URL.Path)
		c.mu.Unlock()
		c.notify <- struct{}{}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func (c *capture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

func (c *capture) snapshot() ([][]byte, []http.Header, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.bodies...), append([]http.Header(nil), c.headers...), append([]string(nil), c.paths...)
}

// waitFor polls until cond is true or the bound expires. It is the test-side
// wait for a background sender, never something production does.
func (c *capture) waitFor(t *testing.T, want int) [][]byte {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c.count() >= want {
			bodies, _, _ := c.snapshot()
			return bodies
		}
		time.Sleep(2 * time.Millisecond)
	}
	bodies, _, _ := c.snapshot()
	t.Fatalf("timed out waiting for %d ingest request(s); got %d", want, len(bodies))
	return nil
}

func decodeBatch(t *testing.T, body []byte) batch {
	t.Helper()
	var b batch
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatalf("decode batch: %v (%s)", err, body)
	}
	return b
}

func eventSeqs(t *testing.T, b batch) []uint64 {
	t.Helper()
	out := make([]uint64, 0, len(b.Events))
	for _, raw := range b.Events {
		var env struct {
			Seq uint64 `json:"seq"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("decode event: %v (%s)", err, raw)
		}
		out = append(out, env.Seq)
	}
	return out
}

func newTestEmitter(srv *httptest.Server, seq *SeqCounter, logf func(string, ...any)) *Emitter {
	return newTestEmitterEvery(srv, seq, logf, 10*time.Millisecond)
}

func newTestEmitterEvery(srv *httptest.Server, seq *SeqCounter, logf func(string, ...any), every time.Duration) *Emitter {
	if seq == nil {
		seq = NewSeqCounter(NewMemorySeqStore(), nil)
	}
	return New(Config{
		Origin:      srv.URL,
		APIKey:      "clk_test_key",
		Harness:     "claude",
		Seq:         seq,
		Client:      srv.Client(),
		Logf:        logf,
		FlushEvery:  every,
		SendTimeout: 300 * time.Millisecond,
		CloseGrace:  500 * time.Millisecond,
	})
}

func TestEmitterBatchesFiftyEvents(t *testing.T) {
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusOK, `{"accepted":50}`))
	defer srv.Close()
	// A long ticker leaves the count trigger as the only flush path, so the
	// assertion is about the 50-event cap and not a race with the clock.
	e := newTestEmitterEvery(srv, nil, nil, time.Hour)
	e.SetRunID("run-1")

	for i := 0; i < MaxBatchEvents; i++ {
		e.ToolCall("Bash", fmt.Sprintf("call-%d", i), "echo hi")
	}
	bodies := c.waitFor(t, 1)
	if len(bodies) != 1 {
		t.Fatalf("requests = %d, want the first batch to carry all 50 events", len(bodies))
	}
	b := decodeBatch(t, bodies[0])
	if b.Harness != "claude" {
		t.Errorf("harness = %q", b.Harness)
	}
	if len(b.Events) != MaxBatchEvents {
		t.Fatalf("events = %d, want %d", len(b.Events), MaxBatchEvents)
	}
	seqs := eventSeqs(t, b)
	for i, seq := range seqs {
		if seq != uint64(i+1) {
			t.Fatalf("seq[%d] = %d, want %d (strictly ascending from 1)", i, seq, i+1)
		}
	}
	e.Close()
}

func TestEmitterFlushesOnTicker(t *testing.T) {
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusOK, `{"accepted":1}`))
	defer srv.Close()
	e := newTestEmitter(srv, nil, nil)
	e.SetRunID("run-1")
	e.ToolCall("Read", "call-1", "a.go")
	// One event is nowhere near the count trigger; only the ticker sends it.
	bodies := c.waitFor(t, 1)
	b := decodeBatch(t, bodies[0])
	if len(b.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(b.Events))
	}
	e.Close()
}

func TestEmitterPostsToRunEndpointWithBearer(t *testing.T) {
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusOK, `{"accepted":1}`))
	defer srv.Close()
	e := newTestEmitter(srv, nil, nil)
	e.SetRunID("uuid-1.secret-part")
	e.ToolCall("Bash", "call-1", "ls")
	c.waitFor(t, 1)
	_, headers, paths := c.snapshot()
	if paths[0] != "/api/runs/uuid-1.secret-part/events" {
		t.Errorf("path = %q", paths[0])
	}
	if got := headers[0].Get("Authorization"); got != "Bearer clk_test_key" {
		t.Errorf("Authorization = %q", got)
	}
	if got := headers[0].Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	e.Close()
}

func TestEmitterRedactsTargetBeforeSend(t *testing.T) {
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusOK, `{"accepted":1}`))
	defer srv.Close()
	e := newTestEmitter(srv, nil, nil)
	e.SetRunID("run-1")
	e.ToolCall("Bash", "call-1", "export KEY=clk_"+strings.Repeat("a", 64))
	bodies := c.waitFor(t, 1)
	body := string(bodies[0])
	if strings.Contains(body, strings.Repeat("a", 64)) {
		t.Fatalf("the key reached the wire: %s", body)
	}
	if !strings.Contains(body, `"redacted":true`) || !strings.Contains(body, "[redacted]") {
		t.Fatalf("event lacks the redaction marker: %s", body)
	}
	e.Close()
}

func TestEmitterTruncatesTargetTo512Bytes(t *testing.T) {
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusOK, `{"accepted":1}`))
	defer srv.Close()
	e := newTestEmitter(srv, nil, nil)
	e.SetRunID("run-1")
	e.ToolCall("Bash", "call-1", strings.Repeat("x", 2000))
	bodies := c.waitFor(t, 1)
	var b batch
	if err := json.Unmarshal(bodies[0], &b); err != nil {
		t.Fatal(err)
	}
	var ev ToolCallEvent
	if err := json.Unmarshal(b.Events[0], &ev); err != nil {
		t.Fatal(err)
	}
	if len(ev.Target) != MaxTargetBytes {
		t.Errorf("target length = %d, want %d", len(ev.Target), MaxTargetBytes)
	}
	if !ev.Truncated {
		t.Error("truncated marker missing")
	}
	e.Close()
}

func TestEmitterTodoCaps(t *testing.T) {
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusOK, `{"accepted":1}`))
	defer srv.Close()
	e := newTestEmitter(srv, nil, nil)
	e.SetRunID("run-1")
	todos := make([]TodoItem, 0, 60)
	for i := 0; i < 60; i++ {
		todos = append(todos, TodoItem{Content: strings.Repeat("t", 400), Status: "pending"})
	}
	e.TodoChange(todos)
	bodies := c.waitFor(t, 1)
	var b batch
	if err := json.Unmarshal(bodies[0], &b); err != nil {
		t.Fatal(err)
	}
	var ev TodoChangeEvent
	if err := json.Unmarshal(b.Events[0], &ev); err != nil {
		t.Fatal(err)
	}
	if len(ev.Todos) != MaxTodos {
		t.Errorf("todos = %d, want %d", len(ev.Todos), MaxTodos)
	}
	if len(ev.Todos[0].Content) != MaxTodoContentBytes {
		t.Errorf("content length = %d, want %d", len(ev.Todos[0].Content), MaxTodoContentBytes)
	}
	if !ev.Truncated {
		t.Error("truncated marker missing")
	}
	e.Close()
}

func TestEmitterBatchBodyStaysUnder256KB(t *testing.T) {
	// 40 todo events at the per-item caps are far more than one body; buildBatch
	// must split them, never produce an over-cap body.
	seq := NewSeqCounter(NewMemorySeqStore(), nil)
	var pending []event
	todos := make([]TodoItem, 0, 50)
	for j := 0; j < 50; j++ {
		todos = append(todos, TodoItem{Content: strings.Repeat("x", 256), Status: "completed"})
	}
	for i := 0; i < 40; i++ {
		pending = append(pending, event{kind: TypeTodoChange, at: "2026-01-01T00:00:00.000Z", todos: todos})
	}
	seen := 0
	for len(pending) > 0 {
		body, n, _, consumed, _ := buildBatch(pending, "run-1", "opencode", seq)
		if consumed == 0 {
			t.Fatalf("buildBatch made no progress on %d pending events", len(pending))
		}
		if n > 0 && len(body) > MaxBatchBytes {
			t.Fatalf("batch body = %d bytes, over the %d cap", len(body), MaxBatchBytes)
		}
		seen += n
		pending = pending[consumed:]
	}
	if seen != 40 {
		t.Errorf("batched %d events, want 40", seen)
	}
}

func TestEmitterAddNeverBlocksOnSlowSend(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer func() {
		close(release)
		srv.Close()
	}()
	e := newTestEmitter(srv, nil, nil)
	e.SetRunID("run-1")

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2000; i++ {
			e.ToolCall("Bash", fmt.Sprintf("call-%d", i), "echo hi")
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("adding events blocked behind a slow send")
	}
	// Close is bounded by CloseGrace even though the sender is stuck in Do.
	start := time.Now()
	e.Close()
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Close blocked for %s", elapsed)
	}
}

func TestEmitterFailureDoesNotFailOrBlock(t *testing.T) {
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusInternalServerError, `{"error":"boom"}`))
	defer srv.Close()
	var logs []string
	var logMu sync.Mutex
	e := newTestEmitter(srv, nil, func(format string, args ...any) {
		logMu.Lock()
		logs = append(logs, fmt.Sprintf(format, args...))
		logMu.Unlock()
	})
	e.SetRunID("run-1")
	e.ToolCall("Bash", "call-1", "false")
	c.waitFor(t, 1)
	e.ToolCall("Bash", "call-2", "false")
	c.waitFor(t, 2)
	e.Close()

	logMu.Lock()
	defer logMu.Unlock()
	if len(logs) == 0 || !strings.Contains(strings.Join(logs, "\n"), "batch send failed") {
		t.Fatalf("failure was not reported: %v", logs)
	}
}

func TestEmitterStopsOnEventsDisabled(t *testing.T) {
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusForbidden, `{"error":{"code":"events_disabled"}}`))
	defer srv.Close()
	var logs []string
	var logMu sync.Mutex
	e := newTestEmitter(srv, nil, func(format string, args ...any) {
		logMu.Lock()
		logs = append(logs, fmt.Sprintf(format, args...))
		logMu.Unlock()
	})
	e.SetRunID("run-1")
	e.ToolCall("Bash", "call-1", "ls")
	c.waitFor(t, 1)
	e.ToolCall("Bash", "call-2", "ls")
	time.Sleep(80 * time.Millisecond)
	e.Close()
	if got := c.count(); got != 1 {
		t.Fatalf("requests = %d, want 1 (the dial-off refusal stops sending)", got)
	}
	logMu.Lock()
	defer logMu.Unlock()
	if !strings.Contains(strings.Join(logs, "\n"), "refused this run's channel") {
		t.Fatalf("the stop was not said once: %v", logs)
	}
}

func TestEmitterNeverClaimedDropsAndSaysSo(t *testing.T) {
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusOK, `{"accepted":1}`))
	defer srv.Close()
	var logs []string
	var logMu sync.Mutex
	e := newTestEmitter(srv, nil, func(format string, args ...any) {
		logMu.Lock()
		logs = append(logs, fmt.Sprintf(format, args...))
		logMu.Unlock()
	})
	e.ToolCall("Bash", "call-1", "ls")
	e.Close()
	if got := c.count(); got != 0 {
		t.Fatalf("requests = %d, want none without a run id", got)
	}
	logMu.Lock()
	defer logMu.Unlock()
	if !strings.Contains(strings.Join(logs, "\n"), "never claimed") {
		t.Fatalf("the drop was not reported: %v", logs)
	}
}

func TestEmittersShareRunSequence(t *testing.T) {
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusOK, `{"accepted":1}`))
	defer srv.Close()
	seq := NewSeqCounter(NewMemorySeqStore(), nil)
	a := newTestEmitter(srv, seq, nil)
	b := newTestEmitter(srv, seq, nil)
	a.SetRunID("run-1")
	b.SetRunID("run-1")
	a.ToolCall("Bash", "a-1", "a")
	b.ToolCall("Bash", "b-1", "b")
	bodies := c.waitFor(t, 2)
	seen := map[uint64]bool{}
	for _, body := range bodies {
		for _, s := range eventSeqs(t, decodeBatch(t, body)) {
			if seen[s] {
				t.Fatalf("sequence %d was used twice across sessions on one run", s)
			}
			seen[s] = true
		}
	}
	if !seen[1] || !seen[2] {
		t.Fatalf("seqs = %v, want 1 and 2", seen)
	}
	a.Close()
	b.Close()
}

func TestEmitterDropsOverCapIdentifiers(t *testing.T) {
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusOK, `{"accepted":1}`))
	defer srv.Close()
	e := newTestEmitter(srv, nil, nil)
	e.SetRunID("run-1")
	e.ToolCall("Bash", strings.Repeat("c", MaxCallIDBytes+1), "ls")
	e.ToolCall("Bash", "ok", "ls")
	c.waitFor(t, 1)
	bodies, _, _ := c.snapshot()
	b := decodeBatch(t, bodies[0])
	if len(b.Events) != 1 {
		t.Fatalf("events = %d, want only the well-formed call", len(b.Events))
	}
	e.Close()
}
