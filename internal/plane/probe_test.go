package plane

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestProbeUsesListProjects pins the common case: the bare /mcp account surface
// answers list_projects, so that is the liveness check and the only call made.
func TestProbeUsesListProjects(t *testing.T) {
	srv, got := serve(t, http.StatusOK, okBody)
	tool, err := Probe(context.Background(), srv.URL+"/mcp", "k-1")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if tool != "list_projects" {
		t.Errorf("tool = %q, want list_projects", tool)
	}
	if got.auth != "Bearer k-1" {
		t.Errorf("Authorization = %q", got.auth)
	}
	if len(got.bodies) != 1 {
		t.Errorf("calls = %d, want exactly one", len(got.bodies))
	}
}

// TestProbeFallsBackOnAProjectEndpoint: a project-scoped endpoint does not
// register list_projects, so the probe must recognise the unknown-tool answer
// and ask the project surface for something it does serve.
func TestProbeFallsBackOnAProjectEndpoint(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.Unmarshal(raw, &req)
		mu.Lock()
		calls = append(calls, req.Params.Name)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if req.Params.Name == "list_projects" {
			_, _ = io.WriteString(w, fmt.Sprintf(unknownToolBody, "list_projects"))
			return
		}
		_, _ = io.WriteString(w, okBody)
	}))
	t.Cleanup(srv.Close)

	tool, err := Probe(context.Background(), srv.URL+"/mcp/acme", "k-1")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if tool != "get_backlog_summary" {
		t.Errorf("tool = %q, want get_backlog_summary", tool)
	}
	if len(calls) != 2 || calls[0] != "list_projects" || calls[1] != "get_backlog_summary" {
		t.Errorf("calls = %v, want list_projects then get_backlog_summary", calls)
	}
}

// A refusal of the CALL is the answer: falling through to another tool would
// report a healthy credential for an endpoint that said no.
func TestProbeDoesNotFallThroughOnAToolRefusal(t *testing.T) {
	const refusal = `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"list_projects refused: not available on this endpoint"}],"isError":true}}`
	srv, got := serve(t, http.StatusOK, refusal)
	_, err := Probe(context.Background(), srv.URL+"/mcp/acme", "k-1")
	if err == nil {
		t.Fatal("Probe accepted a refused call")
	}
	if len(got.bodies) != 1 {
		t.Errorf("calls = %d, want exactly one (no fallback)", len(got.bodies))
	}
}

func TestProbeReportsARejectedCredential(t *testing.T) {
	srv, _ := serve(t, http.StatusUnauthorized, `{"error":"unauthorized"}`)
	_, err := Probe(context.Background(), srv.URL+"/mcp", "bad-key")
	if err == nil {
		t.Fatal("Probe accepted a 401")
	}
	if !Unauthorized(err) {
		t.Errorf("Unauthorized(%v) = false, want true", err)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("the error does not name the status: %v", err)
	}
}

func TestProbeNotWired(t *testing.T) {
	if _, err := Probe(context.Background(), "", "k"); err != ErrNotWired {
		t.Errorf("Probe with no endpoint = %v, want ErrNotWired", err)
	}
	if _, err := Probe(context.Background(), "https://clankerbar.com/mcp", ""); err != ErrNotWired {
		t.Errorf("Probe with no key = %v, want ErrNotWired", err)
	}
}

func TestUnauthorizedOnlyFor401And403(t *testing.T) {
	for status, want := range map[int]bool{
		http.StatusUnauthorized:        true,
		http.StatusForbidden:           true,
		http.StatusNotFound:            false,
		http.StatusInternalServerError: false,
	} {
		srv, _ := serve(t, status, "{}")
		_, err := Probe(context.Background(), srv.URL+"/mcp", "k")
		if got := Unauthorized(err); got != want {
			t.Errorf("Unauthorized(HTTP %d) = %v, want %v", status, got, want)
		}
	}
}
