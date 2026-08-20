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
// is asserted rather than assumed. bodies holds EVERY request, in order — a call
// that makes two writes (a park: update_task then ask_question) needs both.
type capture struct {
	method string
	path   string
	auth   string
	accept string
	body   map[string]any // the LAST request's body, for the one-call tests
	bodies []map[string]any
}

func serve(t *testing.T, status int, body string) (*httptest.Server, *capture) {
	t.Helper()
	got := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got.method, got.path = r.Method, r.URL.Path
		got.auth, got.accept = r.Header.Get("Authorization"), r.Header.Get("Accept")
		var parsed map[string]any
		_ = json.Unmarshal(raw, &parsed)
		got.bodies = append(got.bodies, parsed)
		got.body = parsed
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

// A dead-phase park makes two calls in order — the status write first, then the
// OPEN question that raises the park to the operator. Both must carry exactly
// the keys the MCP tool schemas name, or a park fails silently at 3am as "left
// for the next claim" while the task never actually moves (review finding). The
// question must be a non-blocking `clarification`: a `decision` is born answered
// and raises nothing, and a blocking question would un-block to `ready` on any
// answer, handing the task straight back to the fleet the guard just withheld it
// from (CLA-395).
func TestPark_RequestShape(t *testing.T) {
	srv, got := serve(t, http.StatusOK, okBody)

	p, ok := New(srv.URL+"/mcp/demo", "k-1").(ParkAPI)
	if !ok {
		t.Fatal("the wired plane client does not implement ParkAPI, so no task can ever be parked")
	}
	const (
		outcome = "Parked after two consecutive dead phases"
		body    = "**t-1 has defeated two sessions and is now parked.**"
	)
	if err := p.Park(context.Background(), "t-1", "r-1", outcome); err != nil {
		t.Fatalf("Park: %v", err)
	}
	if err := p.AskQuestion(context.Background(), "t-1", body, []string{"a", "b"}, false, "clarification"); err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}

	if len(got.bodies) != 2 {
		t.Fatalf("park made %d calls, want 2 (update_task then ask_question): %+v", len(got.bodies), got.bodies)
	}

	first, _ := got.bodies[0]["params"].(map[string]any)
	if first["name"] != "update_task" {
		t.Errorf("first tool = %v, want update_task", first["name"])
	}
	args, _ := first["arguments"].(map[string]any)
	if args["taskId"] != "t-1" || args["runId"] != "r-1" {
		t.Errorf("first arguments = %+v, want taskId=t-1 runId=r-1", args)
	}
	if args["status"] != "parked" {
		t.Errorf("status = %v, want parked", args["status"])
	}
	if args["outcome"] != outcome {
		t.Errorf("outcome = %v, want the park outcome", args["outcome"])
	}

	second, _ := got.bodies[1]["params"].(map[string]any)
	if second["name"] != "ask_question" {
		t.Errorf("second tool = %v, want ask_question", second["name"])
	}
	qargs, _ := second["arguments"].(map[string]any)
	if qargs["taskId"] != "t-1" {
		t.Errorf("question taskId = %v, want t-1", qargs["taskId"])
	}
	if qargs["body"] != body {
		t.Errorf("question body = %+v, want the park body", qargs["body"])
	}
	opts, _ := qargs["options"].([]any)
	if len(opts) != 2 || opts[0] != "a" || opts[1] != "b" {
		t.Errorf("question options = %+v, want the driver's options", qargs["options"])
	}
	// The shape that makes the park reach the operator: an OPEN question, not a
	// born-answered decision. A blocking question would un-block to `ready` on
	// any answer; a `decision` would read as project-wide standing law.
	if qargs["blocking"] != false {
		t.Errorf("question blocking = %v, want false", qargs["blocking"])
	}
	if qargs["kind"] != "clarification" {
		t.Errorf("question kind = %v, want clarification", qargs["kind"])
	}

	// No decision is recorded anywhere: the park's record is the outcome plus
	// the open question, never a record_decision call.
	for i, b := range got.bodies {
		params, _ := b["params"].(map[string]any)
		if params["name"] == "record_decision" {
			t.Errorf("call %d recorded a decision; the park must raise a question instead", i)
		}
	}
}

// A not-wired plane must not be a ParkAPI that silently does nothing: the driver
// type-asserts, and a no-op park would log "parked" for a task still in the queue.
func TestPark_NotWiredIsNotAParker(t *testing.T) {
	if _, ok := New("", "").(ParkAPI); ok {
		t.Error("the not-wired client claims to park tasks")
	}
}

// A failed question insert is ErrQuestionNotFiled — the sentinel that tells the
// driver the task IS parked and must be reported "parked, and the question is
// missing", never "the park failed, the task is left to the next claim".
func TestAskQuestion_FailureIsQuestionNotFiled(t *testing.T) {
	srv, _ := serve(t, http.StatusInternalServerError, "boom")
	p := New(srv.URL+"/mcp/demo", "k-1").(ParkAPI)

	err := p.AskQuestion(context.Background(), "t-1", "body", nil, false, "clarification")
	if !errors.Is(err, ErrQuestionNotFiled) {
		t.Errorf("err = %v, want ErrQuestionNotFiled", err)
	}
}

// A project-level question (CLA-396) goes to ask_question with NO taskId: a
// fleet trip is not one task's triage, so pinning it to whichever task happened
// to be in flight would make the operator answer about a bystander. It is
// non-blocking — there is no task to block, and the loop's own pause is the
// enforcement the driver clears when this question is answered.
func TestAskProjectQuestion_RequestShape(t *testing.T) {
	srv, got := serve(t, http.StatusOK, okBody)
	p, ok := New(srv.URL+"/mcp/demo", "k-1").(ParkAPI)
	if !ok {
		t.Fatal("the wired plane client does not implement ParkAPI, so no fleet trip can ever raise a question")
	}

	const body = "**5 consecutive dead phases across tasks.**"
	if err := p.AskProjectQuestion(context.Background(), body, []string{"resume", "paused"}, "decision"); err != nil {
		t.Fatalf("AskProjectQuestion: %v", err)
	}

	if len(got.bodies) != 1 {
		t.Fatalf("made %d calls, want 1: %+v", len(got.bodies), got.bodies)
	}
	params, _ := got.bodies[0]["params"].(map[string]any)
	if params["name"] != "ask_question" {
		t.Fatalf("tool = %v, want ask_question", params["name"])
	}
	args, _ := params["arguments"].(map[string]any)
	// The whole point: no taskId key at all, not an empty one the server would
	// have to special-case.
	if _, has := args["taskId"]; has {
		t.Errorf("arguments carry a taskId (%v); a project-level question must not be pinned to a task", args["taskId"])
	}
	if args["body"] != body {
		t.Errorf("body = %+v, want the fleet body", args["body"])
	}
	opts, _ := args["options"].([]any)
	if len(opts) != 2 || opts[0] != "resume" || opts[1] != "paused" {
		t.Errorf("options = %+v, want the driver's options", args["options"])
	}
	if args["blocking"] != false {
		t.Errorf("blocking = %v, want false — there is no task to block", args["blocking"])
	}
	if args["kind"] != "decision" {
		t.Errorf("kind = %v, want decision — a fleet incident is a project-level judgment", args["kind"])
	}
}

// A failed project question insert is ErrQuestionNotFiled too: the caller must
// report "paused, and the question is missing", never "the pause failed".
func TestAskProjectQuestion_FailureIsQuestionNotFiled(t *testing.T) {
	srv, _ := serve(t, http.StatusInternalServerError, "boom")
	p := New(srv.URL+"/mcp/demo", "k-1").(ParkAPI)

	err := p.AskProjectQuestion(context.Background(), "body", nil, "decision")
	if !errors.Is(err, ErrQuestionNotFiled) {
		t.Errorf("err = %v, want ErrQuestionNotFiled", err)
	}
}
