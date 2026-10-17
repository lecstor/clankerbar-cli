package plane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// The task reads are the driver's only view of a task's repo (CLA-437), so both
// the wire shape they send and the envelopes they accept are pinned here.

// mcpTextBody builds a successful tools/call response whose single text content
// carries inner, escaped the way the wire actually escapes it.
func mcpTextBody(t *testing.T, inner string) string {
	t.Helper()
	text, err := json.Marshal(inner)
	if err != nil {
		t.Fatalf("marshal inner: %v", err)
	}
	return `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":` + string(text) + `}]}}`
}

// mustClient hands the concrete client back: the task-read methods live on
// *mcpReleaser, not on the Releaser write interface New returns.
func mustClient(t *testing.T, srv *httptest.Server) *mcpReleaser {
	t.Helper()
	url := "http://127.0.0.1:1/mcp/x"
	if srv != nil {
		url = srv.URL + "/mcp/demo"
	}
	return New(url, "k-1").(*mcpReleaser)
}

func TestPeekNextTask(t *testing.T) {
	ctx := context.Background()
	t.Run("next with id and repo", func(t *testing.T) {
		srv, got := serve(t, http.StatusOK, mcpTextBody(t, `{"next":{"id":"83ea12ef-1","ref":"CLA-437","repo":"lecstor/clankerbar-cli"},"readyCount":2}`))
		next, err := mustClient(t, srv).PeekNextTask(ctx)
		if err != nil {
			t.Fatalf("PeekNextTask: %v", err)
		}
		if next.TaskID != "83ea12ef-1" || next.Repo != "lecstor/clankerbar-cli" {
			t.Errorf("next = %+v, want the queued task's id and repo", next)
		}
		if got.body["method"] != "tools/call" {
			t.Errorf("method = %v, want tools/call", got.body["method"])
		}
		params := got.body["params"].(map[string]any)
		if params["name"] != "next_task" {
			t.Errorf("tool = %v, want next_task", params["name"])
		}
		// CLA-448: the plane's next_task schema is `inputSchema: {}` and refuses a
		// null `arguments`; the driver must send an empty OBJECT (a session's MCP
		// SDK does the same). This failed live on every daemon as "Invalid input /
		// expected: record / params.arguments", which sent the peek to the
		// primary-repo fallback and silently defeated CLA-437's repo routing.
		args, ok := params["arguments"].(map[string]any)
		if !ok || len(args) != 0 {
			t.Errorf("arguments = %#v, want an empty object, not nil or null", params["arguments"])
		}
	})
	t.Run("empty queue", func(t *testing.T) {
		srv, _ := serve(t, http.StatusOK, mcpTextBody(t, `{"next":null}`))
		next, err := mustClient(t, srv).PeekNextTask(ctx)
		if err != nil {
			t.Fatalf("PeekNextTask: %v", err)
		}
		if next.TaskID != "" || next.Repo != "" {
			t.Errorf("next = %+v, want zero value for an empty queue", next)
		}
	})
	t.Run("repo null decodes to empty", func(t *testing.T) {
		srv, _ := serve(t, http.StatusOK, mcpTextBody(t, `{"next":{"id":"t-9","repo":null}}`))
		next, err := mustClient(t, srv).PeekNextTask(ctx)
		if err != nil {
			t.Fatalf("PeekNextTask: %v", err)
		}
		if next.TaskID != "t-9" || next.Repo != "" {
			t.Errorf("next = %+v, want id kept and repo empty", next)
		}
	})
	t.Run("sse frame accepted like the writes", func(t *testing.T) {
		srv, _ := serve(t, http.StatusOK, "data: "+mcpTextBody(t, `{"next":{"id":"t-7","repo":"o/r"}}`)+"\n\n")
		next, err := mustClient(t, srv).PeekNextTask(ctx)
		if err != nil {
			t.Fatalf("PeekNextTask: %v", err)
		}
		if next.Repo != "o/r" {
			t.Errorf("repo = %q, want o/r from an SSE-framed response", next.Repo)
		}
	})
	t.Run("tool refusal is an error", func(t *testing.T) {
		srv, _ := serve(t, http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"refused"}]}}`)
		_, err := mustClient(t, srv).PeekNextTask(ctx)
		if err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("err = %v, want the tool-level refusal surfaced", err)
		}
	})
}

func TestTaskRepo(t *testing.T) {
	ctx := context.Background()
	t.Run("wrapped envelope", func(t *testing.T) {
		srv, got := serve(t, http.StatusOK, mcpTextBody(t, `{"task":{"id":"t-1","repo":"lecstor/clankerbar"}}`))
		repo, err := mustClient(t, srv).TaskRepo(ctx, "t-1")
		if err != nil {
			t.Fatalf("TaskRepo: %v", err)
		}
		if repo != "lecstor/clankerbar" {
			t.Errorf("repo = %q", repo)
		}
		params := got.body["params"].(map[string]any)
		if params["name"] != "get_task" {
			t.Errorf("tool = %v, want get_task", params["name"])
		}
		args := params["arguments"].(map[string]any)
		if args["taskId"] != "t-1" {
			t.Errorf("taskId = %v, want t-1", args["taskId"])
		}
	})
	t.Run("bare envelope", func(t *testing.T) {
		srv, _ := serve(t, http.StatusOK, mcpTextBody(t, `{"id":"t-1","repo":"o/other"}`))
		repo, err := mustClient(t, srv).TaskRepo(ctx, "t-1")
		if err != nil {
			t.Fatalf("TaskRepo: %v", err)
		}
		if repo != "o/other" {
			t.Errorf("repo = %q, want the bare object read when there is no wrapper", repo)
		}
	})
	t.Run("empty taskId refused locally", func(t *testing.T) {
		if _, err := mustClient(t, nil).TaskRepo(ctx, ""); err == nil {
			t.Error("TaskRepo with an empty taskId must be refused before any request")
		}
	})
	t.Run("not wired", func(t *testing.T) {
		nw := notWired{}
		if _, err := nw.TaskRepo(ctx, "t-1"); !errors.Is(err, ErrNotWired) {
			t.Errorf("err = %v, want ErrNotWired", err)
		}
		if _, err := nw.PeekNextTask(ctx); !errors.Is(err, ErrNotWired) {
			t.Errorf("err = %v, want ErrNotWired", err)
		}
	})
	t.Run("http failure carries the status", func(t *testing.T) {
		srv, _ := serve(t, http.StatusServiceUnavailable, "nope")
		_, err := mustClient(t, srv).TaskRepo(ctx, "t-1")
		if err == nil || !strings.Contains(err.Error(), "503") {
			t.Errorf("err = %v, want the HTTP status named", err)
		}
	})
}

// The CLA-451 checkpoint peek reads a task's holder state off get_task: who
// holds it (claimedByRun), whether work is handed over (branch), and in which
// envelope. The fields are null on an unheld task and must decode to empty.
func TestTaskState(t *testing.T) {
	ctx := context.Background()
	t.Run("wrapped envelope", func(t *testing.T) {
		srv, got := serve(t, http.StatusOK, mcpTextBody(t, `{"task":{"id":"t-1","ref":"CLA-451","status":"in_progress","claimedByRun":"r-9","branch":"clanker/x","outcome":"## Review\nverdict: pass"}}`))
		st, err := mustClient(t, srv).TaskState(ctx, "t-1")
		if err != nil {
			t.Fatalf("TaskState: %v", err)
		}
		want := TaskState{Status: "in_progress", Ref: "CLA-451", ClaimedByRun: "r-9", Branch: "clanker/x", Outcome: "## Review\nverdict: pass"}
		if !reflect.DeepEqual(st, want) {
			t.Errorf("state = %+v, want %+v", st, want)
		}
		params := got.body["params"].(map[string]any)
		if params["name"] != "get_task" {
			t.Errorf("tool = %v, want get_task", params["name"])
		}
		args := params["arguments"].(map[string]any)
		if args["taskId"] != "t-1" {
			t.Errorf("taskId = %v, want t-1", args["taskId"])
		}
	})
	t.Run("bare envelope", func(t *testing.T) {
		srv, _ := serve(t, http.StatusOK, mcpTextBody(t, `{"status":"ready","claimedByRun":null,"branch":null}`))
		st, err := mustClient(t, srv).TaskState(ctx, "t-2")
		if err != nil {
			t.Fatalf("TaskState: %v", err)
		}
		want := TaskState{Status: "ready"}
		if !reflect.DeepEqual(st, want) {
			t.Errorf("state = %+v, want %+v — nulls decode to empty, like repo above", st, want)
		}
	})
	// The delivery record rides the same response (CLA-497): noCode is the
	// plane's declared "this task shipped no code" flag, and it must decode off
	// BOTH envelopes — and stay false when the record or the flag is absent or
	// null, exactly like every other field here.
	t.Run("delivery noCode decodes", func(t *testing.T) {
		srv, _ := serve(t, http.StatusOK, mcpTextBody(t, `{"task":{"id":"t-3","status":"done","delivery":{"noCode":true,"commit":null,"pr":null}}}`))
		st, err := mustClient(t, srv).TaskState(ctx, "t-3")
		if err != nil {
			t.Fatalf("TaskState: %v", err)
		}
		want := TaskState{Status: "done", DeliveryNoCode: true}
		if !reflect.DeepEqual(st, want) {
			t.Errorf("state = %+v, want %+v", st, want)
		}
	})
	t.Run("absent and null delivery decode false", func(t *testing.T) {
		srv, _ := serve(t, http.StatusOK, mcpTextBody(t, `{"task":{"id":"t-4","status":"in_progress","claimedByRun":"r-1","delivery":null}}`))
		st, err := mustClient(t, srv).TaskState(ctx, "t-4")
		if err != nil {
			t.Fatalf("TaskState: %v", err)
		}
		want := TaskState{Status: "in_progress", ClaimedByRun: "r-1"}
		if !reflect.DeepEqual(st, want) {
			t.Errorf("state = %+v, want %+v — a null delivery is no delivery", st, want)
		}
	})
	t.Run("empty taskId refused locally", func(t *testing.T) {
		if _, err := mustClient(t, nil).TaskState(ctx, ""); err == nil {
			t.Error("TaskState with an empty taskId must be refused before any request")
		}
	})
	t.Run("not wired", func(t *testing.T) {
		nw := notWired{}
		if _, err := nw.TaskState(ctx, "t-1"); !errors.Is(err, ErrNotWired) {
			t.Errorf("err = %v, want ErrNotWired", err)
		}
	})
}

// The compact `reviews` block (CLA-636) is what the CLA-637 seam gate reads:
// a sealed record still serves its metadata — round, role, run, reviewed tip,
// counts — while its findings are withheld, a released one serves its findings,
// and `counted`/`openMustIds` ride beside the records. The decode is
// descriptive, so an unexpected shape in one field must not fail the read.
func TestTaskState_ReviewsDecode(t *testing.T) {
	ctx := context.Background()
	t.Run("sealed metadata and released findings", func(t *testing.T) {
		srv, _ := serve(t, http.StatusOK, mcpTextBody(t, `{"task":{"id":"t-1","status":"in_progress","claimedByRun":"r-1","reviews":{"records":[
			{"id":"rec-1","round":1,"cycle":1,"reviewerRole":"review-1","runId":"r-1","harness":"claude","model":"opus-test","branch":"clanker/x","reviewedTip":"sha1","scope":{"kind":"full-diff","base":"staging","range":"clanker/x"},"verdict":"findings","state":"sealed","counts":{"must":1,"should":2,"nit":3},"findingsTotal":6,"at":"2026-10-17T10:00:00.000Z"},
			{"id":"rec-2","round":2,"cycle":1,"reviewerRole":"review-2","runId":"r-1","harness":"opencode","model":"kimi-k3","branch":"clanker/x","reviewedTip":"sha2","scope":{"kind":"full-diff","base":"staging","range":"clanker/x"},"verdict":"pass","state":"open","counts":{"must":0,"should":0,"nit":0},"findingsTotal":1,"at":"2026-10-17T11:00:00.000Z","findings":[
				{"id":"2.review-2.1","severity":"should","evidenceKind":null,"evidence":null,"file":"a.go","line":7,"kind":"style","summary":"s","requiredFix":"f","raisedByModel":"kimi-k3","status":"open","fixCommit":null,"refutedReason":null,"filedTaskRef":null}]}
		],"total":2,"counted":2,"openMustIds":["2.review-2.2"]}}}`))
		st, err := mustClient(t, srv).TaskState(ctx, "t-1")
		if err != nil {
			t.Fatalf("TaskState: %v", err)
		}
		if len(st.Reviews) != 2 {
			t.Fatalf("decoded %d records, want 2", len(st.Reviews))
		}
		sealed := st.Reviews[0]
		if sealed.Round != 1 || sealed.Cycle != 1 || sealed.ReviewerRole != "review-1" ||
			sealed.RunID != "r-1" || sealed.ReviewedTip != "sha1" || sealed.State != "sealed" ||
			sealed.Harness != "claude" || sealed.Model != "opus-test" || sealed.Verdict != "findings" {
			t.Errorf("sealed record metadata = %+v", sealed)
		}
		if sealed.Counts.Must != 1 || sealed.Counts.Should != 2 || sealed.Counts.Nit != 3 || sealed.FindingsTotal != 6 {
			t.Errorf("sealed record counts = %+v total=%d, want must=1 should=2 nit=3 total=6", sealed.Counts, sealed.FindingsTotal)
		}
		if len(sealed.Findings) != 0 {
			t.Errorf("a sealed record served %d findings; the plane withholds them", len(sealed.Findings))
		}
		if sealed.Scope.Kind != "full-diff" || sealed.Scope.Base != "staging" || sealed.Scope.Range != "clanker/x" {
			t.Errorf("sealed record scope = %+v", sealed.Scope)
		}
		released := st.Reviews[1]
		if released.State != "open" || released.Round != 2 || released.ReviewerRole != "review-2" {
			t.Errorf("released record metadata = %+v", released)
		}
		if len(released.Findings) != 1 {
			t.Fatalf("released record served %d findings, want 1", len(released.Findings))
		}
		f := released.Findings[0]
		if f.ID != "2.review-2.1" || f.Severity != "should" || f.Line != 7 || f.RaisedByModel != "kimi-k3" || f.Status != "open" {
			t.Errorf("released finding = %+v", f)
		}
		if st.ReviewCounted != 2 {
			t.Errorf("ReviewCounted = %d, want 2", st.ReviewCounted)
		}
		if len(st.ReviewOpenMustIDs) != 1 || st.ReviewOpenMustIDs[0] != "2.review-2.2" {
			t.Errorf("ReviewOpenMustIDs = %v, want [2.review-2.2]", st.ReviewOpenMustIDs)
		}
	})
	t.Run("an absent block decodes empty", func(t *testing.T) {
		srv, _ := serve(t, http.StatusOK, mcpTextBody(t, `{"task":{"id":"t-2","status":"ready"}}`))
		st, err := mustClient(t, srv).TaskState(ctx, "t-2")
		if err != nil {
			t.Fatalf("TaskState: %v", err)
		}
		if len(st.Reviews) != 0 || st.ReviewCounted != 0 || len(st.ReviewOpenMustIDs) != 0 {
			t.Errorf("a task with no reviews decoded %+v, want empty", st)
		}
	})
	t.Run("an unexpected leaf shape does not fail the read", func(t *testing.T) {
		srv, _ := serve(t, http.StatusOK, mcpTextBody(t, `{"task":{"id":"t-3","status":"in_progress","reviews":{"records":[{"round":"one","reviewerRole":7,"counts":{"must":"x"},"findingsTotal":null,"findings":[{}]}],"counted":"x","openMustIds":"y"}}}`))
		st, err := mustClient(t, srv).TaskState(ctx, "t-3")
		if err != nil {
			t.Fatalf("TaskState must not fail on leaf shapes it does not understand: %v", err)
		}
		if len(st.Reviews) != 1 {
			t.Fatalf("decoded %d records, want the one record with its fields narrowed to zero values", len(st.Reviews))
		}
		if r := st.Reviews[0]; r.Round != 0 || r.ReviewerRole != "" || r.Counts.Must != 0 || r.FindingsTotal != 0 || len(r.Findings) != 1 {
			t.Errorf("non-string/number leaves did not narrow to zero values: %+v", r)
		}
		if st.ReviewCounted != 0 || len(st.ReviewOpenMustIDs) != 0 {
			t.Errorf("non-number counted / non-array openMustIds decoded as %d / %v", st.ReviewCounted, st.ReviewOpenMustIDs)
		}
	})
}
