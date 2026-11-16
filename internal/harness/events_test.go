package harness

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

	"github.com/lecstor/clankerbar-cli/internal/events"
)

// recSink records the normalizer's calls, so the mapping table is asserted
// without an HTTP hop.
type recSink struct {
	calls []string
	todos [][]events.TodoItem
}

func (s *recSink) ToolCall(tool, callID, target string) {
	s.calls = append(s.calls, fmt.Sprintf("tool_call|%s|%s|%s", tool, callID, target))
}
func (s *recSink) ClankerbarMcpCall(tool, callID, target string) {
	s.calls = append(s.calls, fmt.Sprintf("clankerbar_mcp_call|%s|%s|%s", tool, callID, target))
}
func (s *recSink) TodoChange(todos []events.TodoItem) {
	s.todos = append(s.todos, todos)
	s.calls = append(s.calls, "todo_change")
}
func (s *recSink) SetRunID(string) {}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func TestClaudeActivityMapping(t *testing.T) {
	cases := []struct {
		name string
		tool string
		in   string
		want string
	}{
		{"bash command", "Bash", `{"command":"pnpm test"}`, "tool_call|Bash|c1|pnpm test"},
		{"read file path", "Read", `{"file_path":"internal/x.go"}`, "tool_call|Read|c1|internal/x.go"},
		{"edit file path", "Edit", `{"file_path":"a.go","old_string":"x","new_string":"y"}`, "tool_call|Edit|c1|a.go"},
		{"glob path", "Glob", `{"pattern":"*.go","path":"internal"}`, "tool_call|Glob|c1|internal"},
		{"webfetch url", "WebFetch", `{"url":"https://example.com/x","prompt":"read"}`, "tool_call|WebFetch|c1|https://example.com/x"},
		{"unknown tool takes common key", "SomeTool", `{"path":"docs/x.md"}`, "tool_call|SomeTool|c1|docs/x.md"},
		{"unknown tool with no target", "SomeTool", `{"nested":{"x":1}}`, "tool_call|SomeTool|c1|"},
		{"clankerbar claim", "mcp__clankerbar__claim_task", `{"taskId":"uuid-1"}`, "clankerbar_mcp_call|claim_task|c1|uuid-1"},
		{"clankerbar question", "mcp__clankerbar__escalate_question", `{"questionId":"q-1"}`, "clankerbar_mcp_call|escalate_question|c1|q-1"},
		{"clankerbar call with no identifying arg", "mcp__clankerbar__heartbeat", `{"runId":"r-1"}`, "clankerbar_mcp_call|heartbeat|c1|"},
		{"clankerbar call with no input", "mcp__clankerbar__list_tasks", `{}`, "clankerbar_mcp_call|list_tasks|c1|"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &recSink{}
			NoteClaudeToolEvent(s, tc.tool, "c1", raw(tc.in))
			if len(s.calls) != 1 || s.calls[0] != tc.want {
				t.Fatalf("calls = %v, want [%q]", s.calls, tc.want)
			}
		})
	}
}

func TestClaudeTodoMapping(t *testing.T) {
	s := &recSink{}
	NoteClaudeToolEvent(s, "TodoWrite", "c1", raw(`{"todos":[
		{"content":"write schema","status":"completed","activeForm":"writing schema"},
		{"content":"write volume","status":"in_progress","activeForm":"writing volume"}
	]}`))
	if len(s.todos) != 1 {
		t.Fatalf("todo events = %d, want 1", len(s.todos))
	}
	got := s.todos[0]
	if len(got) != 2 || got[0].Content != "write schema" || got[0].Status != "completed" ||
		got[1].Content != "write volume" || got[1].Status != "in_progress" {
		t.Fatalf("todos = %+v", got)
	}
	// The per-harness decoration must not survive into the wire shape.
	if strings.Contains(s.calls[0], "activeForm") {
		t.Fatalf("decorations leaked: %v", s.calls)
	}
}

func TestOpencodeActivityMapping(t *testing.T) {
	cases := []struct {
		name string
		tool string
		in   string
		want string
	}{
		{"shell command", "shell", `{"command":"pnpm test","description":"run"}`, "tool_call|shell|c1|pnpm test"},
		{"read filePath", "read", `{"filePath":"a.go"}`, "tool_call|read|c1|a.go"},
		{"write path", "write", `{"path":"b.go","content":"x"}`, "tool_call|write|c1|b.go"},
		{"grep", "grep", `{"pattern":"func main","path":"cmd"}`, "tool_call|grep|c1|cmd"},
		{"unknown with nothing", "weird", `{"flag":true}`, "tool_call|weird|c1|"},
		{"clankerbar progress", "clankerbar_record_progress", `{"taskId":"uuid-1","branch":"clanker/x"}`, "clankerbar_mcp_call|record_progress|c1|uuid-1"},
		{"clankerbar heartbeat", "clankerbar_heartbeat", `{"runId":"r-1"}`, "clankerbar_mcp_call|heartbeat|c1|"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &recSink{}
			noteOpencodeToolEvent(s, tc.tool, "c1", raw(tc.in))
			if len(s.calls) != 1 || s.calls[0] != tc.want {
				t.Fatalf("calls = %v, want [%q]", s.calls, tc.want)
			}
		})
	}
}

func TestOpencodeTodoMapping(t *testing.T) {
	s := &recSink{}
	noteOpencodeToolEvent(s, "todowrite", "c1", raw(`{"todos":[{"content":"a","status":"pending"}]}`))
	noteOpencodeToolEvent(s, "TodoWrite", "c2", raw(`{"todos":[{"content":"b","status":"completed"}]}`))
	if len(s.todos) != 2 {
		t.Fatalf("todo events = %d, want 2 (both spellings map)", len(s.todos))
	}
	// A non-todo tool is not mistaken for one.
	s = &recSink{}
	noteOpencodeToolEvent(s, "read", "c3", raw(`{"filePath":"x"}`))
	if len(s.todos) != 0 {
		t.Fatalf("a read produced a todo event")
	}
}

// capturedBatch is one ingest request the fake plane received.
type capturedBatch struct {
	path    string
	harness string
	events  []map[string]any
}

type eventPlane struct {
	mu      sync.Mutex
	batches []capturedBatch
	notify  chan struct{}
}

func newEventPlane(t *testing.T) (*eventPlane, *httptest.Server) {
	t.Helper()
	p := &eventPlane{notify: make(chan struct{}, 32)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var b struct {
			Harness string           `json:"harness"`
			Events  []map[string]any `json:"events"`
		}
		if err := json.Unmarshal(body, &b); err != nil {
			t.Errorf("ingest body is not JSON: %v (%s)", err, body)
		}
		p.mu.Lock()
		p.batches = append(p.batches, capturedBatch{path: r.URL.Path, harness: b.Harness, events: b.Events})
		p.mu.Unlock()
		p.notify <- struct{}{}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"accepted":1}`)
	}))
	return p, srv
}

func (p *eventPlane) waitForEvents(t *testing.T, want int) []capturedBatch {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		n := 0
		for _, b := range p.batches {
			n += len(b.events)
		}
		p.mu.Unlock()
		if n >= want {
			p.mu.Lock()
			out := append([]capturedBatch(nil), p.batches...)
			p.mu.Unlock()
			return out
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d channel events", want)
	return nil
}

func eventSinkFactory(t *testing.T, srv *httptest.Server, harnessName string) func() *events.Emitter {
	t.Helper()
	seq := events.NewSeqCounter(events.NewMemorySeqStore(), nil)
	return func() *events.Emitter {
		return events.New(events.Config{
			Origin:      srv.URL,
			APIKey:      "clk_test_key",
			Harness:     harnessName,
			Seq:         seq,
			Client:      srv.Client(),
			FlushEvery:  10 * time.Millisecond,
			SendTimeout: 500 * time.Millisecond,
			CloseGrace:  time.Second,
		})
	}
}

// TestClaudeParserEmitsActivityEvents drives the real claude stream parser with
// a claim, a todo snapshot and a shell call, and asserts the three activity
// events reach the plane under the claim's run id.
func TestClaudeParserEmitsActivityEvents(t *testing.T) {
	plane, srv := newEventPlane(t)
	defer srv.Close()
	in := Invocation{NewEventSink: eventSinkFactory(t, srv, "claude")}
	res := newSessionResult(in)
	defer res.closeEvents()

	claimUse := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"call-1","name":"mcp__clankerbar__claim_task","input":{"taskId":"uuid-1"}}],"usage":{}}}`
	claimResult := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","content":"{\"task\":{\"id\":\"uuid-1\",\"ref\":\"CLA-1\"},\"run\":{\"id\":\"run-1.secret\"}}"}]}}`
	todoUse := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"call-2","name":"TodoWrite","input":{"todos":[{"content":"schema","status":"completed"},{"content":"emitter","status":"in_progress"}]}}],"usage":{}}}`
	bashUse := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"call-3","name":"Bash","input":{"command":"pnpm test"}}],"usage":{}}}`

	for _, line := range []string{claimUse, claimResult, todoUse, bashUse} {
		(claude{}).renderAndParse([]byte(line), io.Discard, &res)
	}
	batches := plane.waitForEvents(t, 3)
	// The 10ms test flush interval can split the session across batches; the
	// contract is the events and their order, not that a live session flushed in
	// one tick (production's cadence is 2s).
	var events []map[string]any
	for _, b := range batches {
		if b.harness != "claude" {
			t.Errorf("harness = %q", b.harness)
		}
		if b.path != "/api/runs/run-1.secret/events" {
			t.Errorf("path = %q", b.path)
		}
		events = append(events, b.events...)
	}
	var kinds []string
	for _, ev := range events {
		kinds = append(kinds, fmt.Sprint(ev["type"]))
	}
	want := []string{"clankerbar_mcp_call", "todo_change", "tool_call"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("event types = %v, want %v", kinds, want)
	}
	if events[0]["tool"] != "claim_task" || events[0]["target"] != "uuid-1" {
		t.Errorf("claim event = %v", events[0])
	}
	if events[2]["tool"] != "Bash" || events[2]["target"] != "pnpm test" {
		t.Errorf("tool call event = %v", events[2])
	}
	for i, ev := range events {
		if int(ev["seq"].(float64)) != i+1 {
			t.Errorf("seq[%d] = %v", i, ev["seq"])
		}
	}
}

// TestOpencodeParserEmitsActivityEvents does the same through the opencode
// stream's terminal tool parts, where the call and its result arrive together.
func TestOpencodeParserEmitsActivityEvents(t *testing.T) {
	plane, srv := newEventPlane(t)
	defer srv.Close()
	in := Invocation{NewEventSink: eventSinkFactory(t, srv, "opencode")}
	p := newOpencodeParse(in, io.Discard)
	defer p.closeEvents()

	lines := []string{
		`{"type":"tool_use","sessionID":"ses_1","part":{"partID":"prt_1","type":"tool","id":"call_1","tool":"clankerbar_claim_task","state":{"status":"completed","input":{"taskId":"uuid-1"},"output":"{\"task\":{\"id\":\"uuid-1\",\"ref\":\"CLA-1\"},\"run\":{\"id\":\"run-1.secret\"}}"}}}`,
		`{"type":"tool_use","sessionID":"ses_1","part":{"partID":"prt_2","type":"tool","id":"call_2","tool":"todowrite","state":{"status":"completed","input":{"todos":[{"content":"emitter","status":"in_progress"}]},"output":"ok"}}}`,
		`{"type":"tool_use","sessionID":"ses_1","part":{"partID":"prt_3","type":"tool","id":"call_3","tool":"shell","state":{"status":"completed","input":{"command":"go test ./..."},"output":"ok"}}}`,
	}
	for _, line := range lines {
		p.line([]byte(line))
	}
	batches := plane.waitForEvents(t, 3)
	var events []map[string]any
	for _, b := range batches {
		if b.harness != "opencode" {
			t.Errorf("harness = %q", b.harness)
		}
		events = append(events, b.events...)
	}
	got := fmt.Sprint(events[0]["type"], ",", events[1]["type"], ",", events[2]["type"])
	if got != "clankerbar_mcp_call,todo_change,tool_call" {
		t.Fatalf("event types = %s", got)
	}
	if events[2]["tool"] != "shell" || events[2]["target"] != "go test ./..." {
		t.Errorf("shell event = %v", events[2])
	}
}
