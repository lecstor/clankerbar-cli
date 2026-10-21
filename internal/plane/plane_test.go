package plane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

const okBody = `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{}"}]}}`

// unknownToolBody is the plane's real answer to a tool it does not register,
// captured from the live server (protocol 39) and parameterised on the tool
// name: a 200 carrying result.isError and this text.
const unknownToolBody = `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"MCP error -32602: Tool %s not found"}],"isError":true}}`

// capture records what the driver actually put on the wire, so the request shape
// is asserted rather than assumed. bodies holds EVERY request, in order — a call
// that makes two writes (a park: settle_task then ask_question) needs both.
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
	if params["name"] != "release_task" {
		t.Errorf("tool = %v, want release_task", params["name"])
	}
	args, _ := params["arguments"].(map[string]any)
	if args["taskId"] != "t-1" || args["runId"] != "r-1" {
		t.Errorf("arguments = %+v, want taskId=t-1 runId=r-1", args)
	}
	// The default handback is the call itself: neither the expiry nor the driver
	// marker is set, and a handback must not carry a status or an outcome. The
	// session may have written an outcome; releasing must not clobber it.
	for _, forbidden := range []string{"expire", "driver", "checkGreen", "status", "outcome", "delivery"} {
		if _, present := args[forbidden]; present {
			t.Errorf("release must not carry %q: %+v", forbidden, args)
		}
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
	if params["name"] != "record_progress" {
		t.Errorf("tool = %v, want record_progress", params["name"])
	}
	args, _ := params["arguments"].(map[string]any)
	if args["taskId"] != "t-1" || args["runId"] != "r-1" || args["branch"] != "clanker/x" {
		t.Errorf("arguments = %+v, want taskId=t-1 runId=r-1 branch=clanker/x", args)
	}
	// The whole distinction from Release. record_progress has no status field at
	// all, but it can carry an outcome and a delivery, so those must stay absent:
	// this call is a hand-off record and nothing else.
	for _, forbidden := range []string{"outcome", "delivery"} {
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

func TestCloseReviewRound_RequestShape(t *testing.T) {
	srv, got := serve(t, http.StatusOK, okBody)
	if err := New(srv.URL+"/mcp/demo", "k-1").(ReviewRoundCloser).
		CloseReviewRound(context.Background(), "t-1", "r-1", 2); err != nil {
		t.Fatalf("CloseReviewRound: %v", err)
	}
	params, _ := got.body["params"].(map[string]any)
	if params["name"] != "record_review" {
		t.Errorf("tool = %v, want record_review", params["name"])
	}
	args, _ := params["arguments"].(map[string]any)
	if args["taskId"] != "t-1" || args["runId"] != "r-1" {
		t.Errorf("arguments = %+v, want taskId=t-1 runId=r-1", args)
	}
	if args["closeRound"] != float64(2) {
		t.Errorf("closeRound = %v, want 2", args["closeRound"])
	}
	// A close is not a revision, a status move or an outcome write.
	if len(args) != 3 {
		t.Errorf("arguments = %+v, want only taskId, runId and closeRound", args)
	}
}

// A close is a write like any other: an empty id or a non-positive round is a
// caller bug, refused before anything reaches the plane (the same guard the
// other writes carry).
func TestCloseReviewRound_RefusesEmptyIdentity(t *testing.T) {
	rel := New("https://plane.test/mcp/demo", "k-1").(ReviewRoundCloser)
	for _, tc := range []struct {
		name          string
		taskID, runID string
		round         int
	}{
		{"no task", "", "r-1", 1},
		{"no run", "t-1", "", 1},
		{"no round", "t-1", "r-1", 0},
	} {
		if err := rel.CloseReviewRound(context.Background(), tc.taskID, tc.runID, tc.round); err == nil {
			t.Errorf("%s: CloseReviewRound succeeded, want a refusal", tc.name)
		}
	}
}

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
		t.Fatalf("park made %d calls, want 2 (settle_task then ask_question): %+v", len(got.bodies), got.bodies)
	}

	first, _ := got.bodies[0]["params"].(map[string]any)
	if first["name"] != "settle_task" {
		t.Errorf("first tool = %v, want settle_task", first["name"])
	}
	args, _ := first["arguments"].(map[string]any)
	if args["taskId"] != "t-1" || args["runId"] != "r-1" {
		t.Errorf("first arguments = %+v, want taskId=t-1 runId=r-1", args)
	}
	if args["disposition"] != "parked" {
		t.Errorf("disposition = %v, want parked", args["disposition"])
	}
	if args["outcome"] != outcome {
		t.Errorf("outcome = %v, want the park's reason", args["outcome"])
	}
	if _, present := args["status"]; present {
		t.Errorf("settle_task has no status field, but the call carried one: %+v", args)
	}

	second, _ := got.bodies[1]["params"].(map[string]any)
	if second["name"] != "ask_question" {
		t.Errorf("second tool = %v, want ask_question", second["name"])
	}
	qargs, _ := second["arguments"].(map[string]any)
	if qargs["blocking"] != false {
		t.Errorf("blocking = %v, want false - a parked task has nothing left to block", qargs["blocking"])
	}
	if qargs["kind"] != "clarification" {
		t.Errorf("kind = %v, want clarification - a decision is born answered", qargs["kind"])
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

// Heartbeat (CLA-358) renews a live claim's lease. The request shape is pinned
// for the same reason Release's is: the loop beats on this every ten minutes
// for the life of every session, and a shape drift would read as "renewal
// failing" on every run at once.

func TestHeartbeat_RequestShape(t *testing.T) {
	srv, got := serve(t, http.StatusOK, okBody)

	if err := New(srv.URL+"/mcp/demo", "k-1").(Heartbeat).Heartbeat(context.Background(), "r-9"); err != nil {
		t.Fatalf("Heartbeat: %v", err)
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
	if got.body["method"] != "tools/call" {
		t.Errorf("method = %v, want tools/call", got.body["method"])
	}
	params, _ := got.body["params"].(map[string]any)
	if params["name"] != "heartbeat" {
		t.Errorf("tool = %v, want heartbeat", params["name"])
	}
	args, _ := params["arguments"].(map[string]any)
	if args["runId"] != "r-9" {
		t.Errorf("arguments = %+v, want runId=r-9", args)
	}
}

func TestHeartbeat_RequiresARunID(t *testing.T) {
	srv, got := serve(t, http.StatusOK, okBody)

	err := New(srv.URL+"/mcp/demo", "k-1").(Heartbeat).Heartbeat(context.Background(), "")
	if err == nil {
		t.Fatal("Heartbeat with an empty runId returned nil, want an error")
	}
	if len(got.bodies) != 0 {
		t.Fatalf("%d request(s) left the driver, want none - an empty runId must fail before the wire", len(got.bodies))
	}
}

func TestHeartbeat_NotWiredDegradesCleanly(t *testing.T) {
	p := New("", "")
	hb, ok := p.(Heartbeat)
	if !ok {
		t.Fatal("a not-wired plane does not satisfy Heartbeat; the renewer would skip it silently instead of degrading with ErrNotWired")
	}
	if err := hb.Heartbeat(context.Background(), "r-1"); !errors.Is(err, ErrNotWired) {
		t.Errorf("err = %v, want ErrNotWired", err)
	}
}

// --- CLA-708: ending a dead session's lease ---------------------------------

func TestExpireLease_RequestShape(t *testing.T) {
	srv, got := serve(t, http.StatusOK, okBody)

	if err := New(srv.URL+"/mcp/demo", "k-1").ExpireLease(context.Background(), "t-1", "r-1"); err != nil {
		t.Fatalf("ExpireLease: %v", err)
	}
	params, _ := got.body["params"].(map[string]any)
	if params["name"] != "release_task" {
		t.Errorf("tool = %v, want release_task", params["name"])
	}
	args, _ := params["arguments"].(map[string]any)
	if args["taskId"] != "t-1" || args["runId"] != "r-1" {
		t.Errorf("arguments = %+v, want taskId=t-1 runId=r-1", args)
	}
	if args["expire"] != true {
		t.Errorf("expire = %v, want true", args["expire"])
	}
	if args["driver"] != true {
		t.Errorf("driver = %v, want true - the plane records this as a driver's call for a dead session", args["driver"])
	}
	// `checkGreen` is the other attestation and belongs to a close-out release;
	// the expiry and a green-check claim are two different assertions.
	for _, forbidden := range []string{"checkGreen", "status", "outcome", "delivery"} {
		if _, present := args[forbidden]; present {
			t.Errorf("expire must not carry %q: %+v", forbidden, args)
		}
	}
}

func TestReleaseDriver_RequestShape(t *testing.T) {
	srv, got := serve(t, http.StatusOK, okBody)

	if err := New(srv.URL+"/mcp/demo", "k-1").ReleaseDriver(context.Background(), "t-1", "r-1"); err != nil {
		t.Fatalf("ReleaseDriver: %v", err)
	}
	params, _ := got.body["params"].(map[string]any)
	if params["name"] != "release_task" {
		t.Errorf("tool = %v, want release_task", params["name"])
	}
	args, _ := params["arguments"].(map[string]any)
	if args["driver"] != true {
		t.Errorf("arguments = %+v, want driver=true", args)
	}
	if _, present := args["expire"]; present {
		t.Errorf("ReleaseDriver must not send expire: %+v", args)
	}
}

// CLA-748: the merge attestation is a RECORD write. It goes through
// record_progress with the delivery echoed whole, and it must NOT call
// complete_task - attesting a delivery is not closing the task, and a CLI that
// closed as a side effect would end the run the session is still working in.
func TestAttestMergeVerified_WritesTheDeliveryWithoutClosing(t *testing.T) {
	srv, got := serve(t, http.StatusOK, okBody)
	d := Delivery{Commit: "abc123", IntegrationBranch: "staging", PR: "#12"}

	if err := New(srv.URL+"/mcp/demo", "k-1").(Attester).
		AttestMergeVerified(context.Background(), "t-1", "r-1", d, true); err != nil {
		t.Fatalf("AttestMergeVerified: %v", err)
	}
	params, _ := got.body["params"].(map[string]any)
	if params["name"] != "record_progress" {
		t.Errorf("tool = %v, want record_progress (not complete_task: an attestation must not close the task)", params["name"])
	}
	args, _ := params["arguments"].(map[string]any)
	if args["taskId"] != "t-1" || args["runId"] != "r-1" {
		t.Errorf("arguments = %+v, want taskId=t-1 runId=r-1", args)
	}
	delivery, _ := args["delivery"].(map[string]any)
	for field, want := range map[string]any{
		"commit":            "abc123",
		"integrationBranch": "staging",
		"mergeVerified":     true,
		"pr":                "#12",
	} {
		if delivery[field] != want {
			t.Errorf("delivery.%s = %v, want %v (delivery = %+v)", field, delivery[field], want, delivery)
		}
	}
	// No outcome and no status: this revises the record the session declared.
	for _, forbidden := range []string{"outcome", "status"} {
		if _, present := args[forbidden]; present {
			t.Errorf("the attestation carried %q: %+v", forbidden, args)
		}
	}
}

// CLA-748: the plane and the CLI are released as one coordinated cutover, so an
// unknown tool is a hard error, never a cue to retry an older spelling. Every
// moved write must report the version it needs, and no call may fall back to
// update_task.
func TestUnknownTool_NamesTheRequiredPlaneVersionAndDoesNotRetry(t *testing.T) {
	calls := []struct {
		name string
		tool string
		call func(Releaser) error
	}{
		{"Release", "release_task", func(r Releaser) error { return r.Release(context.Background(), "t-1", "r-1") }},
		{"ReleaseDriver", "release_task", func(r Releaser) error { return r.ReleaseDriver(context.Background(), "t-1", "r-1") }},
		{"ExpireLease", "release_task", func(r Releaser) error { return r.ExpireLease(context.Background(), "t-1", "r-1") }},
		{"RecordBranch", "record_progress", func(r Releaser) error {
			return r.(Recorder).RecordBranch(context.Background(), "t-1", "r-1", "clanker/x")
		}},
		{"CloseReviewRound", "record_review", func(r Releaser) error {
			return r.(ReviewRoundCloser).CloseReviewRound(context.Background(), "t-1", "r-1", 1)
		}},
		{"Park", "settle_task", func(r Releaser) error {
			return r.(ParkAPI).Park(context.Background(), "t-1", "r-1", "why")
		}},
		{"AttestMergeVerified", "record_progress", func(r Releaser) error {
			return r.(Attester).AttestMergeVerified(context.Background(), "t-1", "r-1",
				Delivery{Commit: "abc", IntegrationBranch: "staging"}, true)
		}},
	}
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			srv, got := serve(t, http.StatusOK, fmt.Sprintf(unknownToolBody, tc.tool))
			err := tc.call(New(srv.URL+"/mcp/demo", "k-1"))
			if !errors.Is(err, ErrPlaneTooOld) {
				t.Fatalf("err = %v, want ErrPlaneTooOld", err)
			}
			if !strings.Contains(err.Error(), strconv.Itoa(RequiredPlaneProtocol)) {
				t.Errorf("the error does not name the plane protocol this release needs (%d): %v", RequiredPlaneProtocol, err)
			}
			// One call and no retry: the fallback that used to make a second call
			// with the old spelling is gone with update_task itself.
			if len(got.bodies) != 1 {
				t.Fatalf("made %d calls, want exactly 1 (no retry): %+v", len(got.bodies), got.bodies)
			}
			params, _ := got.bodies[0]["params"].(map[string]any)
			if params["name"] == "update_task" {
				t.Errorf("retried the deleted tool: %+v", params)
			}
		})
	}
}

// A refusal of the CALL is not an unknown tool, even when it mentions a task by a
// name that looks like one: only a message naming the tool AND calling it not
// found is classified.
func TestToolRefusal_IsNotAnUnknownTool(t *testing.T) {
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"not_holding"}]}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"run_superseded"}]}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"record_progress refused: not_found: No task t-1 in this project."}]}}`,
	} {
		srv, _ := serve(t, http.StatusOK, body)
		err := New(srv.URL+"/mcp/demo", "k-1").ExpireLease(context.Background(), "t-1", "r-1")
		if err == nil || errors.Is(err, ErrPlaneTooOld) {
			t.Errorf("refusal %s classified as an unknown tool: %v", body, err)
		}
	}
}

func TestExpireLease_NotWired(t *testing.T) {
	if err := New("", "").ExpireLease(context.Background(), "t-1", "r-1"); !errors.Is(err, ErrNotWired) {
		t.Errorf("err = %v, want ErrNotWired", err)
	}
}

func TestExpireLease_RequiresBothIDs(t *testing.T) {
	srv, got := serve(t, http.StatusOK, okBody)
	r := New(srv.URL+"/mcp/demo", "k-1")
	for _, tt := range []struct{ name, task, run string }{
		{"no task", "", "r-1"},
		{"no run", "t-1", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := r.ExpireLease(context.Background(), tt.task, tt.run); err == nil {
				t.Fatal("expected an error")
			}
			if got.method != "" {
				t.Errorf("must not have hit the network, but saw a %s", got.method)
			}
		})
	}
}
