package plane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
