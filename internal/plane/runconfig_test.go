package plane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// rcResult builds an MCP tools/call body whose single text content is payload.
func rcResult(payload string) string {
	encoded, _ := json.Marshal(payload)
	var b strings.Builder
	b.WriteString(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":`)
	b.Write(encoded)
	b.WriteString(`}]}}`)
	return b.String()
}

// A project that never stored a config answers version 0 with the empty
// default document; the client collapses both facts into ErrNoConfig so the
// caller's fallback branch is one comparison.
func TestRunConfig_VersionZeroMeansNothingStored(t *testing.T) {
	srv, _ := serve(t, http.StatusOK, rcResult(`{"config":{},"version":0,"pendingProposal":null}`))
	defer srv.Close()
	if _, err := NewRunConfigAPI(srv.URL+"/mcp/demo", "k").RunConfig(context.Background()); !errors.Is(err, ErrNoConfig) {
		t.Fatalf("err = %v, want ErrNoConfig", err)
	}
}

func TestRunConfig_ParsesVersionDocumentAndPending(t *testing.T) {
	srv, got := serve(t, http.StatusOK, rcResult(
		`{"config":{"harness":"opencode","budget":{"max_tokens":10}},"version":4,"pendingProposal":{"id":"p-1"}}`))
	defer srv.Close()
	st, err := NewRunConfigAPI(srv.URL+"/mcp/demo", "k").RunConfig(context.Background())
	if err != nil {
		t.Fatalf("RunConfig: %v", err)
	}
	if st.Version != 4 || st.Pending != true {
		t.Errorf("version=%d pending=%t, want 4/true", st.Version, st.Pending)
	}
	var doc map[string]any
	if err := json.Unmarshal(st.Config, &doc); err != nil || doc["harness"] != "opencode" {
		t.Errorf("document = %s (%v), want the stored harness inside", st.Config, err)
	}
	params, _ := got.body["params"].(map[string]any)
	if params["name"] != "get_project_run_config" {
		t.Errorf("tool = %v, want get_project_run_config", params["name"])
	}
}

func TestProposeRunConfig_SendsTheFullDocumentAndNotes(t *testing.T) {
	srv, got := serve(t, http.StatusOK, okBody)
	defer srv.Close()
	doc := map[string]any{"harness": "codex", "models": map[string]any{"strong": "x"}}
	if err := NewRunConfigAPI(srv.URL+"/mcp/demo", "k").ProposeRunConfig(context.Background(), doc, "why"); err != nil {
		t.Fatalf("ProposeRunConfig: %v", err)
	}
	params, _ := got.body["params"].(map[string]any)
	if params["name"] != "propose_project_run_config" {
		t.Fatalf("tool = %v, want propose_project_run_config", params["name"])
	}
	args, _ := params["arguments"].(map[string]any)
	if args["notes"] != "why" {
		t.Errorf("notes = %v, want the derivation carried for the ratify card", args["notes"])
	}
	cfg, _ := args["config"].(map[string]any)
	if cfg == nil || cfg["harness"] != "codex" {
		t.Errorf("config = %+v, want the full proposed document", args["config"])
	}
}

func TestProposeRunConfig_RefusesAnEmptyDocument(t *testing.T) {
	err := NewRunConfigAPI("https://plane.example/mcp/demo", "k").ProposeRunConfig(context.Background(), map[string]any{}, "")
	if err == nil {
		t.Fatal("an empty proposal went out; it should have been refused locally")
	}
}

func TestNewRunConfigAPI_NotWiredRefusesAndStaysNarrow(t *testing.T) {
	rc := NewRunConfigAPI("", "")
	if _, err := rc.RunConfig(context.Background()); !errors.Is(err, ErrNotWired) {
		t.Errorf("RunConfig err = %v, want ErrNotWired", err)
	}
	if err := rc.ProposeRunConfig(context.Background(), map[string]any{"harness": "x"}, ""); !errors.Is(err, ErrNotWired) {
		t.Errorf("ProposeRunConfig err = %v, want ErrNotWired", err)
	}
	// The not-wired value is its OWN type, implementing only what RunConfigAPI
	// names: asserting it into a capability nobody asked for must fail.
	if _, ok := rc.(Releaser); ok {
		t.Error("the run-config not-wired client claims Releaser capabilities")
	}
}
