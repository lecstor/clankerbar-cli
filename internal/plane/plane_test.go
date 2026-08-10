package plane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const okBody = `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{}"}]}}`

// capture records what the driver actually put on the wire, so the request shape
// is asserted rather than assumed.
type capture struct {
	method string
	path   string
	auth   string
	accept string
	body   map[string]any
}

func serve(t *testing.T, status int, body string) (*httptest.Server, *capture) {
	t.Helper()
	got := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got.method, got.path = r.Method, r.URL.Path
		got.auth, got.accept = r.Header.Get("Authorization"), r.Header.Get("Accept")
		_ = json.Unmarshal(raw, &got.body)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func TestRelease_RequestShape(t *testing.T) {
	srv, got := serve(t, http.StatusOK, okBody)

	if err := New(srv.URL+"/mcp/demo", "k-1").Release(context.Background(), "t-1", "r-1"); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if got.method != http.MethodPost {
		t.Errorf("method = %s, want POST", got.method)
	}
	if got.path != "/mcp/demo" {
		t.Errorf("path = %s, want /mcp/demo", got.path)
	}
	if got.auth != "Bearer k-1" {
		t.Errorf("Authorization = %q", got.auth)
	}
	// Streamable HTTP may answer as JSON or as SSE; we must accept both or the
	// server is entitled to 406 us.
	if !strings.Contains(got.accept, "application/json") || !strings.Contains(got.accept, "text/event-stream") {
		t.Errorf("Accept = %q, want both json and event-stream", got.accept)
	}
	if got.body["method"] != "tools/call" {
		t.Errorf("method = %v, want tools/call", got.body["method"])
	}

	params, _ := got.body["params"].(map[string]any)
	if params["name"] != "update_task" {
		t.Errorf("tool = %v, want update_task", params["name"])
	}
	args, _ := params["arguments"].(map[string]any)
	if args["taskId"] != "t-1" || args["runId"] != "r-1" {
		t.Errorf("arguments = %+v, want taskId=t-1 runId=r-1", args)
	}
	// `ready` is what returns the task to the claimable queue without charging it
	// a reclaim. `parked` would hide it from next_task and wait for the operator.
	if args["status"] != "ready" {
		t.Errorf("status = %v, want ready", args["status"])
	}
	// The session may have written an outcome; a handback must not clobber it.
	if _, present := args["outcome"]; present {
		t.Errorf("release must not send an outcome, got %+v", args)
	}
}

func TestRelease_ResponseHandling(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantErr    bool
		wantErrHas string
	}{
		{name: "plain JSON success", status: 200, body: okBody},
		{
			name:   "SSE-framed success",
			status: 200,
			body:   "event: message\ndata: " + okBody + "\n\n",
		},
		{
			// MCP reports a refused call as a 200 with isError — missing this would
			// log a successful handback for a task still held.
			name:       "tool-level refusal is an error despite HTTP 200",
			status:     200,
			body:       `{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"run_superseded"}]}}`,
			wantErr:    true,
			wantErrHas: "run_superseded",
		},
		{
			name:       "JSON-RPC error object",
			status:     200,
			body:       `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"unknown tool"}}`,
			wantErr:    true,
			wantErrHas: "unknown tool",
		},
		{
			name:       "HTTP failure",
			status:     500,
			body:       "boom",
			wantErr:    true,
			wantErrHas: "HTTP 500",
		},
		{
			name:       "unparseable body",
			status:     200,
			body:       "not json at all",
			wantErr:    true,
			wantErrHas: "decode response",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := serve(t, tt.status, tt.body)
			err := New(srv.URL+"/mcp/demo", "k-1").Release(context.Background(), "t-1", "r-1")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got nil")
				}
				if !strings.Contains(err.Error(), tt.wantErrHas) {
					t.Errorf("error = %v, want it to mention %q", err, tt.wantErrHas)
				}
				return
			}
			if err != nil {
				t.Fatalf("Release: %v", err)
			}
		})
	}
}

func TestNew_NotWired(t *testing.T) {
	for _, tt := range []struct{ name, url, key string }{
		{"no endpoint", "", "k-1"},
		{"no key", "https://clankerbar.com/mcp/demo", ""},
		{"neither", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := New(tt.url, tt.key).Release(context.Background(), "t-1", "r-1")
			if !errors.Is(err, ErrNotWired) {
				t.Errorf("err = %v, want ErrNotWired", err)
			}
		})
	}
}

// Both ids are required: a half-claim must never reach the plane as a write.
func TestRelease_RequiresBothIDs(t *testing.T) {
	srv, got := serve(t, http.StatusOK, okBody)
	r := New(srv.URL+"/mcp/demo", "k-1")

	for _, tt := range []struct{ name, task, run string }{
		{"no task", "", "r-1"},
		{"no run", "t-1", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := r.Release(context.Background(), tt.task, tt.run); err == nil {
				t.Fatal("expected an error")
			}
			if got.method != "" {
				t.Errorf("must not have hit the network, but saw a %s", got.method)
			}
		})
	}
}

// Recording a work-in-progress branch (CLA-314). The shape is the safety
// property, not a detail: this call has to be legal on a session whose stream
// could not be read whole, and it is legal only because it cannot move the task.
func TestRecordBranch_RequestShape(t *testing.T) {
	srv, got := serve(t, http.StatusOK, okBody)

	r, ok := New(srv.URL+"/mcp/demo", "k-1").(Recorder)
	if !ok {
		t.Fatal("the wired plane client does not implement Recorder, so no branch can ever be recorded")
	}
	if err := r.RecordBranch(context.Background(), "t-1", "r-1", "clanker/x"); err != nil {
		t.Fatalf("RecordBranch: %v", err)
	}

	params, _ := got.body["params"].(map[string]any)
	if params["name"] != "update_task" {
		t.Errorf("tool = %v, want update_task", params["name"])
	}
	args, _ := params["arguments"].(map[string]any)
	if args["taskId"] != "t-1" || args["runId"] != "r-1" || args["branch"] != "clanker/x" {
		t.Errorf("arguments = %+v, want taskId=t-1 runId=r-1 branch=clanker/x", args)
	}
	// The whole distinction from Release. A status is what clears a holder
	// plane-side, so carrying one here would turn a hand-off record into a settle
	// - the one thing CLA-262 forbids on an unreadable stream.
	for _, forbidden := range []string{"status", "outcome", "delivery"} {
		if _, present := args[forbidden]; present {
			t.Errorf("a branch record carried %q (%+v) - it must revise the record and nothing else", forbidden, args)
		}
	}
}

// An empty branch is refused before it reaches the wire: recording one would tell
// the next clanker to fetch a branch that does not exist.
func TestRecordBranch_RefusesAnEmptyBranch(t *testing.T) {
	srv, got := serve(t, http.StatusOK, okBody)
	r := New(srv.URL+"/mcp/demo", "k-1").(Recorder)

	for _, tc := range []struct{ taskID, runID, branch string }{
		{"t-1", "r-1", ""},
		{"", "r-1", "clanker/x"},
		{"t-1", "", "clanker/x"},
	} {
		if err := r.RecordBranch(context.Background(), tc.taskID, tc.runID, tc.branch); err == nil {
			t.Errorf("RecordBranch(%q, %q, %q) was accepted", tc.taskID, tc.runID, tc.branch)
		}
	}
	if got.method != "" {
		t.Errorf("a refused record still reached the plane (%s %s)", got.method, got.path)
	}
}

// A not-wired plane must not be a Recorder that silently does nothing: the driver
// type-asserts, and a no-op recorder would log "recorded" for a branch nothing
// knows about.
func TestRecordBranch_NotWiredIsNotARecorder(t *testing.T) {
	if _, ok := New("", "").(Recorder); ok {
		t.Error("the not-wired client claims to record branches")
	}
}
