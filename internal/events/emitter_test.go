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
	"unicode/utf8"
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

// newTranscriptEmitter is newTestEmitter with the transcript dial on.
func newTranscriptEmitter(srv *httptest.Server, seq *SeqCounter, logf func(string, ...any)) *Emitter {
	e := newTestEmitter(srv, seq, logf)
	e.cfg.Transcript = true
	return e
}

func TestFlushDefaultsMatchTheDesignNote(t *testing.T) {
	// Pinned because the numbers are the contract, not tuning: 2 s / 50 events /
	// 256 KB, whichever is reached first.
	if FlushInterval != 2*time.Second {
		t.Errorf("FlushInterval = %s, want 2s", FlushInterval)
	}
	if MaxBatchEvents != 50 {
		t.Errorf("MaxBatchEvents = %d, want 50", MaxBatchEvents)
	}
	if MaxBatchBytes != 256<<10 {
		t.Errorf("MaxBatchBytes = %d, want 256 KiB", MaxBatchBytes)
	}
	var cfg Config
	cfg.defaults()
	if cfg.FlushEvery != FlushInterval {
		t.Errorf("default FlushEvery = %s, want %s", cfg.FlushEvery, FlushInterval)
	}
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

func TestEmitterDropsInvalidTodoStatuses(t *testing.T) {
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusOK, `{"accepted":1}`))
	defer srv.Close()
	e := newTestEmitter(srv, nil, nil)
	e.SetRunID("run-1")

	// The plane refuses a status it does not know, so the item is dropped and
	// the snapshot is marked truncated: it is no longer the harness's answer.
	e.TodoChange([]TodoItem{
		{Content: "keep", Status: "pending"},
		{Content: "drop", Status: "cancelled"},
		{Content: "keep2", Status: "completed"},
	})
	bodies := c.waitFor(t, 1)
	var b batch
	if err := json.Unmarshal(bodies[0], &b); err != nil {
		t.Fatal(err)
	}
	var ev TodoChangeEvent
	if err := json.Unmarshal(b.Events[0], &ev); err != nil {
		t.Fatal(err)
	}
	if len(ev.Todos) != 2 || ev.Todos[0].Content != "keep" || ev.Todos[1].Content != "keep2" {
		t.Fatalf("todos = %+v, want only the valid statuses", ev.Todos)
	}
	if !ev.Truncated {
		t.Error("truncated marker missing on a dropped item")
	}

	// An all-invalid list sends NOTHING: an empty list would claim the harness
	// cleared its todos, which is a different fact from a list we could not use.
	e.TodoChange([]TodoItem{{Content: "drop", Status: "cancelled"}})
	time.Sleep(50 * time.Millisecond)
	if got := c.count(); got != 1 {
		t.Fatalf("requests = %d, want 1 (an all-invalid list sends no event)", got)
	}
	e.Close()
}

func TestEmitterSendsAnEmptyTodoSnapshot(t *testing.T) {
	// A cleared list is a real snapshot: the status line must be able to show
	// "no todos" rather than keeping the last non-empty list forever.
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusOK, `{"accepted":1}`))
	defer srv.Close()
	e := newTestEmitter(srv, nil, nil)
	e.SetRunID("run-1")
	e.TodoChange(nil)
	bodies := c.waitFor(t, 1)
	var b batch
	if err := json.Unmarshal(bodies[0], &b); err != nil {
		t.Fatal(err)
	}
	var ev TodoChangeEvent
	if err := json.Unmarshal(b.Events[0], &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Todos == nil || len(ev.Todos) != 0 {
		t.Fatalf("todos = %v, want a present empty list", ev.Todos)
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

func TestEmitterBatchCountsSeparatorBytes(t *testing.T) {
	// Fill the body to the BYTE, separators included: rounding to whole events
	// must still leave the encoded request at or under MaxBatchBytes. The comma
	// between adjacent events is easy to forget, and the plane refuses an
	// over-cap body with 413 and the batch is dropped.
	const harness = "opencode"
	remaining := MaxBatchBytes - (len(`{"harness":"","events":[]}`) + len(harness))
	const n = MaxBatchEvents - 1 // leave one pending event to prove the boundary
	pending := make([]event, 0, n+1)
	probe := NewSeqCounter(NewMemorySeqStore(), nil)
	sum := 0
	for i := 0; i < n; i++ {
		ev := event{kind: TypeTodoChange, at: "2026-01-01T00:00:00.000Z", todos: []TodoItem{{Status: "pending"}}}
		raw, err := ev.marshal(probe.Next("size"))
		if err != nil {
			t.Fatal(err)
		}
		sum += len(raw)
		pending = append(pending, ev)
	}
	target := remaining - (n - 1) // n-1 commas between n events
	if sum > target {
		t.Fatalf("event overhead %d already exceeds the %d-byte body", sum, target)
	}
	pending[0].todos[0].Content = strings.Repeat("x", target-sum)
	pending = append(pending, event{kind: TypeToolCall, at: "2026-01-01T00:00:00.000Z", tool: "Bash", callID: "call-over"})

	body, got, _, consumed, dropped := buildBatch(pending, "run-1", harness, NewSeqCounter(NewMemorySeqStore(), nil))
	if dropped != 0 {
		t.Fatalf("dropped = %d, want none", dropped)
	}
	if got != n || consumed != n {
		t.Fatalf("batch events = %d, consumed = %d, want %d — the extra event needs one more separator than is left", got, consumed, n)
	}
	if len(body) != MaxBatchBytes {
		t.Fatalf("body = %d bytes, want exactly the %d-byte cap", len(body), MaxBatchBytes)
	}
}

func TestEmitterBatchNeverExceedsCapAtTheBoundary(t *testing.T) {
	// 49 events whose raw bytes alone fit `remaining` but whose commas do not:
	// the uncounted separator is what let the old buildBatch emit a body over
	// the cap (413 batch_too_large, and the batch is dropped).
	const harness = "opencode"
	remaining := MaxBatchBytes - (len(`{"harness":"","events":[]}`) + len(harness))
	const n = MaxBatchEvents - 1
	per := remaining / n // 49 raw events of this size still fit by raw bytes
	pending := make([]event, 0, n)
	for i := 0; i < n; i++ {
		ev := event{kind: TypeTodoChange, at: "2026-01-01T00:00:00.000Z", todos: []TodoItem{{Status: "pending"}}}
		base, err := ev.marshal(uint64(i + 1))
		if err != nil {
			t.Fatal(err)
		}
		ev.todos[0].Content = strings.Repeat("x", per-len(base))
		pending = append(pending, ev)
	}
	sum := 0
	for i := range pending {
		raw, err := pending[i].marshal(uint64(i + 1))
		if err != nil {
			t.Fatal(err)
		}
		sum += len(raw)
	}
	if sum != per*n || sum > remaining {
		t.Fatalf("test sizing is wrong: raw total %d, per-event %d, remaining %d", sum, per, remaining)
	}

	body, got, _, consumed, dropped := buildBatch(pending, "run-1", harness, NewSeqCounter(NewMemorySeqStore(), nil))
	if dropped != 0 {
		t.Fatalf("dropped = %d, want none", dropped)
	}
	if got == n {
		t.Fatalf("all %d events were accepted: the %d separators were not counted against the body cap", n, n-1)
	}
	if len(body) > MaxBatchBytes {
		t.Fatalf("body = %d bytes, over the %d cap", len(body), MaxBatchBytes)
	}
	if consumed != got {
		t.Fatalf("consumed = %d, accepted = %d — the boundary must stop adding, not drop", consumed, got)
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

func TestEmitterStopsOnRunClosed(t *testing.T) {
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusConflict, `{"error":{"code":"run_closed"}}`))
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
		t.Fatalf("requests = %d, want 1 (a closed run stops sending)", got)
	}
	e.mu.Lock()
	pending, disabled := len(e.pending), e.disabled
	e.mu.Unlock()
	if pending != 0 || !disabled {
		t.Fatalf("pending = %d, disabled = %v: the stop must clear the buffer", pending, disabled)
	}
	logMu.Lock()
	defer logMu.Unlock()
	if n := strings.Count(strings.Join(logs, "\n"), "refused this run's channel"); n != 1 {
		t.Fatalf("the stop was said %d time(s), want once: %v", n, logs)
	}
}

func TestEmitterRetriesRateLimit(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"code":"rate_limited"}}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"accepted":1}`)
	}))
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

	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := attempts
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("attempts = %d, want a retry after the 429", n)
		}
		time.Sleep(2 * time.Millisecond)
	}
	e.Close()

	mu.Lock()
	n := attempts
	mu.Unlock()
	if n != 2 {
		t.Fatalf("attempts = %d, want 2 (one 429, one retry)", n)
	}
	logMu.Lock()
	defer logMu.Unlock()
	if strings.Contains(strings.Join(logs, "\n"), "batch send failed") {
		t.Fatalf("a retried rate limit was reported as a failure: %v", logs)
	}
}

func TestEmitterDropsAnEventOverTheWholeBodyCap(t *testing.T) {
	// One event that cannot fit the body on its own is dropped, not sent to be
	// refused with 413: the next event still goes.
	seq := NewSeqCounter(NewMemorySeqStore(), nil)
	huge := event{kind: TypeTodoChange, at: "2026-01-01T00:00:00.000Z",
		todos: []TodoItem{{Content: strings.Repeat("x", MaxBatchBytes), Status: "pending"}}}
	small := event{kind: TypeToolCall, at: "2026-01-01T00:00:00.000Z", tool: "Bash", callID: "call-1"}
	body, n, _, consumed, dropped := buildBatch([]event{huge, small}, "run-1", "claude", seq)
	if dropped != 1 || consumed != 2 || n != 1 || len(body) > MaxBatchBytes {
		t.Fatalf("n = %d, consumed = %d, dropped = %d, body = %d", n, consumed, dropped, len(body))
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

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// highWaterPlane implements the plane's per-run dedupe rule (run-feed-core.ts:
// `seq <= highWaterSeq` is a duplicate, one high-water mark per run).
type highWaterPlane struct {
	mu        sync.Mutex
	highWater uint64
	accepted  []uint64
	dupes     int
}

func (p *highWaterPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var b batch
	_ = json.Unmarshal(body, &b)
	p.mu.Lock()
	for _, raw := range b.Events {
		var env struct {
			Seq uint64 `json:"seq"`
		}
		_ = json.Unmarshal(raw, &env)
		if env.Seq <= p.highWater {
			p.dupes++
			continue
		}
		p.highWater = env.Seq
		p.accepted = append(p.accepted, env.Seq)
	}
	p.mu.Unlock()
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"accepted":1}`)
}

func TestEmittersSerializePerRunSends(t *testing.T) {
	// Two sessions of one run post from independent goroutines. A allocates
	// seq 1 and its transport is held open; if B were free to send while A is
	// in flight it would land seq 2 first, and the plane's high-water rule
	// would drop A's batch (the default review panel runs members in parallel).
	plane := &highWaterPlane{}
	srv := httptest.NewServer(plane)
	defer srv.Close()

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var gateMu sync.Mutex
	gated := false
	base := srv.Client().Transport
	gatedClient := &http.Client{
		Timeout: 2 * time.Second,
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			gateMu.Lock()
			first := !gated
			gated = true
			gateMu.Unlock()
			if first {
				entered <- struct{}{}
				<-release
			}
			return base.RoundTrip(req)
		}),
	}

	seq := NewSeqCounter(NewMemorySeqStore(), nil)
	mk := func(client *http.Client) *Emitter {
		return New(Config{
			Origin: srv.URL, APIKey: "clk_test_key", Harness: "claude", Seq: seq,
			Client: client, FlushEvery: time.Hour,
			SendTimeout: 2 * time.Second, CloseGrace: 500 * time.Millisecond,
		})
	}
	a, b := mk(gatedClient), mk(srv.Client())
	a.SetRunID("run-1")
	b.SetRunID("run-1")
	a.ToolCall("Bash", "a-1", "a")
	b.ToolCall("Bash", "b-1", "b")

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); a.flush(1, 2*time.Second) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("emitter A never reached the transport")
	}
	wg.Add(1)
	go func() { defer wg.Done(); b.flush(1, 2*time.Second) }()
	// Long enough for an unserialized B to reach the plane while A is held.
	time.Sleep(150 * time.Millisecond)
	close(release)
	waitGroup(t, &wg, 3*time.Second)
	a.Close()
	b.Close()

	plane.mu.Lock()
	defer plane.mu.Unlock()
	if plane.dupes != 0 {
		t.Fatalf("plane dropped %d event(s) as duplicates; accepted = %v", plane.dupes, plane.accepted)
	}
	if len(plane.accepted) != 2 || plane.accepted[0] != 1 || plane.accepted[1] != 2 {
		t.Fatalf("accepted = %v, want [1 2] in allocation order", plane.accepted)
	}
}

// waitGroup waits for wg with a bound, so a broken send cannot hang the suite.
func waitGroup(t *testing.T, wg *sync.WaitGroup, d time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatal("send goroutines did not finish")
	}
}

// decodeTranscript decodes the one event of a single-event batch.
func decodeTranscript[T any](t *testing.T, bodies [][]byte, i int) T {
	t.Helper()
	b := decodeBatch(t, bodies[i])
	if len(b.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(b.Events))
	}
	var ev T
	if err := json.Unmarshal(b.Events[0], &ev); err != nil {
		t.Fatalf("decode event: %v (%s)", err, b.Events[0])
	}
	return ev
}

func TestEmitterToolResultCapsAt64KB(t *testing.T) {
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusOK, `{"accepted":1}`))
	defer srv.Close()
	e := newTranscriptEmitter(srv, nil, nil)
	e.SetRunID("run-1")

	output := strings.Repeat("x", MaxTranscriptBytes+4096)
	e.ToolResult("call-1", true, output, false)
	bodies := c.waitFor(t, 1)
	ev := decodeTranscript[ToolResultEvent](t, bodies, 0)
	if ev.Type != TypeToolResult || ev.CallID != "call-1" || !ev.OK {
		t.Fatalf("event = %+v", ev)
	}
	if ev.Output == nil || len(*ev.Output) != MaxTranscriptBytes {
		t.Fatalf("output length = %v, want %d", ev.Output, MaxTranscriptBytes)
	}
	if ev.OutputBytes != len(output) {
		t.Errorf("outputBytes = %d, want the ORIGINAL %d", ev.OutputBytes, len(output))
	}
	if !ev.Truncated {
		t.Error("truncated marker missing")
	}
	e.Close()
}

func TestEmitterTranscriptCapsDoNotSplitUTF8(t *testing.T) {
	// The cap is BYTES; a cut through a multi-byte rune must walk back, or the
	// JSON encoder rewrites it as U+FFFD and grows the string past the cap.
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusOK, `{"accepted":1}`))
	defer srv.Close()
	e := newTranscriptEmitter(srv, nil, nil)
	e.SetRunID("run-1")

	output := "a" + strings.Repeat("é", MaxTranscriptBytes/2+10)
	e.ToolResult("call-1", true, output, false)
	e.AssistantText(output)
	bodies := c.waitFor(t, 1)
	for _, raw := range decodeBatch(t, bodies[0]).Events {
		var ev struct {
			Type   string  `json:"type"`
			Output *string `json:"output"`
			Text   string  `json:"text"`
		}
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatal(err)
		}
		got := ev.Text
		if ev.Output != nil {
			got = *ev.Output
		}
		if len(got) > MaxTranscriptBytes {
			t.Errorf("%s is %d bytes, over the cap", ev.Type, len(got))
		}
		if !utf8.ValidString(got) {
			t.Errorf("%s is not valid UTF-8 at the cap boundary", ev.Type)
		}
	}
	e.Close()
}

func TestEmitterAssistantTextCapsAt64KB(t *testing.T) {
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusOK, `{"accepted":1}`))
	defer srv.Close()
	e := newTranscriptEmitter(srv, nil, nil)
	e.SetRunID("run-1")

	text := strings.Repeat("y", MaxTranscriptBytes+1)
	e.AssistantText(text)
	bodies := c.waitFor(t, 1)
	ev := decodeTranscript[AssistantTextEvent](t, bodies, 0)
	if ev.Type != TypeAssistantText || len(ev.Text) != MaxTranscriptBytes {
		t.Fatalf("event = %+v", ev)
	}
	if ev.TextBytes != len(text) {
		t.Errorf("textBytes = %d, want the ORIGINAL %d", ev.TextBytes, len(text))
	}
	if !ev.Truncated {
		t.Error("truncated marker missing")
	}
	e.Close()
}

func TestEmitterTranscriptRedactsBeforeTruncation(t *testing.T) {
	// A secret beyond the cap must not survive being "cut": redaction runs over
	// the full string first, and the redacted marker is what the cap then sees.
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusOK, `{"accepted":1}`))
	defer srv.Close()
	e := newTranscriptEmitter(srv, nil, nil)
	e.SetRunID("run-1")

	secret := "ghp_" + strings.Repeat("a", 36)
	output := strings.Repeat("o", MaxTranscriptBytes-20) + " " + secret
	text := strings.Repeat("t", MaxTranscriptBytes-20) + " sk-ant-" + strings.Repeat("b", 30)
	e.ToolResult("call-1", true, output, false)
	e.AssistantText(text)
	bodies := c.waitFor(t, 1)
	for _, body := range bodies {
		if strings.Contains(string(body), "ghp_") || strings.Contains(string(body), "sk-ant-") {
			t.Fatalf("a key reached the wire: %s", body)
		}
		if !strings.Contains(string(body), `"redacted":true`) {
			t.Fatalf("redacted marker missing: %s", body)
		}
	}
	e.Close()
}

func TestEmitterToolResultWithholdsEnvReadOutput(t *testing.T) {
	// `.env*`: the output is dropped entirely and the event says redacted. The
	// byte count still measures what was withheld, so a reader can see that
	// something was there.
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusOK, `{"accepted":1}`))
	defer srv.Close()
	e := newTranscriptEmitter(srv, nil, nil)
	e.SetRunID("run-1")

	output := "DATABASE_URL=postgres://user:pw@host/db\n"
	e.ToolResult("call-env", true, output, true)
	bodies := c.waitFor(t, 1)
	ev := decodeTranscript[ToolResultEvent](t, bodies, 0)
	if ev.Output != nil {
		t.Fatalf("output survived the .env drop: %q", *ev.Output)
	}
	if !ev.Redacted {
		t.Error("redacted marker missing on a withheld output")
	}
	if ev.OutputBytes != len(output) {
		t.Errorf("outputBytes = %d, want the withheld length %d", ev.OutputBytes, len(output))
	}
	if strings.Contains(string(bodies[0]), "DATABASE_URL") {
		t.Fatal("the .env output reached the wire")
	}
	e.Close()
}

func TestEmitterTranscriptOffSendsNoTranscriptEvents(t *testing.T) {
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusOK, `{"accepted":1}`))
	defer srv.Close()
	e := newTestEmitter(srv, nil, nil) // Transcript defaults to false
	e.SetRunID("run-1")

	if e.TranscriptEnabled() {
		t.Fatal("transcript read as enabled with the dial off")
	}
	e.ToolResult("call-1", true, "output", false)
	e.AssistantText("hello")
	e.ToolCall("Bash", "call-2", "ls")
	bodies := c.waitFor(t, 1)
	b := decodeBatch(t, bodies[0])
	if len(b.Events) != 1 {
		t.Fatalf("events = %d, want only the activity call", len(b.Events))
	}
	var ev struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(b.Events[0], &ev)
	if ev.Type != TypeToolCall {
		t.Fatalf("type = %q, want %q", ev.Type, TypeToolCall)
	}
	e.Close()
}

func TestEmitterStopsTranscriptWhenThePlaneReportsDrops(t *testing.T) {
	// The ACK's dropped.transcript is how a run that crossed the 50 MB cap (or
	// whose transcript dial was switched off) learns to stop. The plane has
	// already appended its own run_cap marker; the CLI stops paying to send.
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusOK, `{"accepted":1,"dropped":{"transcript":1}}`))
	defer srv.Close()
	var logs []string
	var logMu sync.Mutex
	e := newTranscriptEmitter(srv, nil, func(format string, args ...any) {
		logMu.Lock()
		logs = append(logs, fmt.Sprintf(format, args...))
		logMu.Unlock()
	})
	e.cfg.FlushEvery = time.Hour // the test drives flush explicitly
	e.SetRunID("run-1")

	e.ToolResult("call-1", true, "big output", false)
	e.flush(1, time.Second)
	if e.TranscriptEnabled() {
		t.Fatal("the drop report did not stop the transcript tier")
	}

	// Activity still flows; further transcript events are refused at enqueue.
	e.ToolResult("call-2", true, "more output", false)
	e.ToolCall("Bash", "call-3", "ls")
	e.flush(1, time.Second)
	bodies, _, _ := c.snapshot()
	last := decodeBatch(t, bodies[len(bodies)-1])
	if len(last.Events) != 1 {
		t.Fatalf("last batch carries %d events, want only the activity call", len(last.Events))
	}
	var ev struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(last.Events[0], &ev)
	if ev.Type != TypeToolCall {
		t.Fatalf("type = %q, want %q", ev.Type, TypeToolCall)
	}
	e.Close()

	logMu.Lock()
	defer logMu.Unlock()
	joined := strings.Join(logs, "\n")
	if n := strings.Count(joined, "stopped this run's transcript tier"); n != 1 {
		t.Fatalf("the stop was said %d time(s), want once: %v", n, logs)
	}
}

func TestEmitterTranscriptAddAndCloseNeverBlock(t *testing.T) {
	// The transcript tier is the same sender: a slow plane must not make a
	// 64 KB tool result block the parser, and Close stays bounded.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer func() {
		close(release)
		srv.Close()
	}()
	e := newTranscriptEmitter(srv, nil, nil)
	e.SetRunID("run-1")

	output := strings.Repeat("z", MaxTranscriptBytes)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			e.ToolResult(fmt.Sprintf("call-%d", i), true, output, false)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("adding transcript events blocked behind a slow send")
	}
	start := time.Now()
	e.Close()
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Close blocked for %s", elapsed)
	}
}
