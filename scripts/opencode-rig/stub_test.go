package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// testCatalog is a small stand-in for the plane's tools/list result. The smoke
// tests use it instead of the live catalog so they need no network and no key.
func testCatalog(t *testing.T) catalog {
	t.Helper()
	return catalog{Tools: []json.RawMessage{
		json.RawMessage(`{"name":"update_task","description":"Update a task","inputSchema":{"type":"object","properties":{"taskId":{"type":"string"}}}}`),
		json.RawMessage(`{"name":"get_task","description":"Get a task","inputSchema":{"type":"object","properties":{"taskId":{"type":"string"}}}}`),
	}}
}

func TestStubServesInitializeListAndCannedCall(t *testing.T) {
	dir := t.TempDir()
	stdin := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"update_task","arguments":{"taskId":"CLA-753","runId":"rig-run-1","review":{"reviewerRole":"review-1","verdict":"pass"},"status":null}}}`,
	}, "\n"))
	var out bytes.Buffer
	if err := serveStub(context.Background(), stdin, &out, stubOptions{
		Scenario: "padding", Out: dir, Catalog: testCatalog(t), MaxCalls: 5,
	}); err != nil {
		t.Fatalf("serveStub: %v", err)
	}

	responses := parseStubResponses(t, out.String())
	if len(responses) != 3 {
		t.Fatalf("got %d responses, want 3 (initialize, tools/list, tools/call): %v", len(responses), responses)
	}
	init := responses[1].(map[string]any)
	if got := init["protocolVersion"]; got != "2025-03-26" {
		t.Errorf("initialize protocolVersion = %v, want the client's version", got)
	}
	if got := init["serverInfo"].(map[string]any)["name"]; got != "clankerbar-opencode-rig-stub" {
		t.Errorf("serverInfo.name = %v", got)
	}
	list := responses[2].(map[string]any)
	if tools, ok := list["tools"].([]any); !ok || len(tools) != 2 {
		t.Errorf("tools/list = %v, want the catalog's 2 tools", list)
	}

	// The third response is a tools/call: a review-shaped update_task with a
	// padded nullable property must be refused like the plane refuses it.
	call := responses[3].(map[string]any)
	if call["isError"] != true {
		t.Fatalf("padded review call isError = %v, want true", call["isError"])
	}
	text := call["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"code":"invalid_review"`) || !strings.Contains(text, "status") {
		t.Errorf("refusal = %s, want invalid_review naming status", text)
	}

	// The call log is the report's source of truth: the keys actually sent must
	// be there, null included.
	calls, err := readStubCalls(dir + "/stub-calls.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("logged %d calls, want 1", len(calls))
	}
	if got := strings.Join(calls[0].Keys, ","); got != "review,runId,status,taskId" {
		t.Errorf("argumentKeys = %s", got)
	}
	if !calls[0].IsError {
		t.Error("logged call isError = false, want true")
	}
	if !fileExists(dir + "/stub-started") {
		t.Error("stub-started marker missing")
	}
}

func TestStubCapsCalls(t *testing.T) {
	dir := t.TempDir()
	stdin := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_task","arguments":{"taskId":"CLA-1"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_task","arguments":{"taskId":"CLA-2"}}}`,
	}, "\n"))
	var out bytes.Buffer
	if err := serveStub(context.Background(), stdin, &out, stubOptions{
		Scenario: "reasoning-replay", Out: dir, Catalog: testCatalog(t), MaxCalls: 1,
	}); err != nil {
		t.Fatalf("serveStub: %v", err)
	}
	responses := parseStubResponses(t, out.String())
	if len(responses) != 2 {
		t.Fatalf("got %d responses", len(responses))
	}
	first := responses[1].(map[string]any)
	if first["isError"] == true {
		t.Error("first call should be within the cap")
	}
	second := responses[2].(map[string]any)
	if second["isError"] != true {
		t.Fatalf("call beyond the cap isError = %v, want true", second["isError"])
	}
	text := second["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "rig_tool_cap") {
		t.Errorf("cap response = %s", text)
	}
	if !fileExists(dir + "/stub-capped") {
		t.Error("stub-capped marker missing")
	}
}

func TestReviewPadding(t *testing.T) {
	got := reviewPadding(map[string]any{
		"taskId": "x", "runId": "r", "review": map[string]any{},
		"status": nil, "priority": nil, "schedule": nil,
	})
	want := []string{"priority", "schedule", "status"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("reviewPadding = %v, want %v", got, want)
	}
	if got := reviewPadding(map[string]any{"taskId": "x", "runId": "r", "review": map[string]any{}}); len(got) != 0 {
		t.Fatalf("clean review call got offenders %v", got)
	}
}

func TestEveryScenarioHasAPromptAndResponder(t *testing.T) {
	for name, sc := range scenarios() {
		if strings.TrimSpace(sc.Prompt) == "" {
			t.Errorf("%s: empty prompt", name)
		}
		if sc.Respond == nil {
			t.Errorf("%s: nil responder", name)
		}
		if sc.Name != name {
			t.Errorf("%s: Name = %q", name, sc.Name)
		}
	}
}

// parseStubResponses decodes newline-delimited JSON-RPC, keyed by request id.
func parseStubResponses(t *testing.T, wire string) map[int]any {
	t.Helper()
	out := map[int]any{}
	for _, line := range strings.Split(strings.TrimSpace(wire), "\n") {
		if line == "" {
			continue
		}
		var msg struct {
			ID     int            `json:"id"`
			Result map[string]any `json:"result"`
			Error  any            `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("bad stub line %q: %v", line, err)
		}
		if msg.Error != nil {
			t.Fatalf("stub replied with error for id %d: %v", msg.ID, msg.Error)
		}
		out[msg.ID] = msg.Result
	}
	return out
}
