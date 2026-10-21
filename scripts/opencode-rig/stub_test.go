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
		json.RawMessage(`{"name":"edit_task","description":"Revise a task","inputSchema":{"type":"object","properties":{"taskId":{"type":"string"}}}}`),
		json.RawMessage(`{"name":"get_task","description":"Get a task","inputSchema":{"type":"object","properties":{"taskId":{"type":"string"}}}}`),
	}}
}

func TestStubServesInitializeListAndCannedCall(t *testing.T) {
	dir := t.TempDir()
	stdin := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"edit_task","arguments":{"taskId":"CLA-753","runId":null,"title":"","detail":"the new body","category":"","repo":null,"priority":null,"schedule":null,"container":false,"humanOnly":false}}}`,
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

	// The third response is a tools/call: a fully padded edit_task must be
	// answered the way the plane answers it. The `null`s are absence, the text
	// `""`s say nothing, and the two padded `false` flags CLEAR a SET flag.
	call := responses[3].(map[string]any)
	if call["isError"] == true {
		t.Fatalf("padded edit_task was refused: %v", call)
	}
	text := call["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"changed":["detail","container","humanOnly"]`) {
		t.Errorf("receipt = %s, want changed detail, container and humanOnly", text)
	}

	// The call log is the report's source of truth: the keys actually sent must
	// be there, empty values included.
	calls, err := readStubCalls(dir + "/stub-calls.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("logged %d calls, want 1", len(calls))
	}
	if got := strings.Join(calls[0].Keys, ","); got != "category,container,detail,humanOnly,priority,repo,runId,schedule,taskId,title" {
		t.Errorf("argumentKeys = %s", got)
	}
	if calls[0].IsError {
		t.Error("logged call isError = true, want the padded call accepted")
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
	// --max-tool-calls N stops the session AT N calls: the Nth is refused, not
	// answered with an N+1'th.
	first := responses[1].(map[string]any)
	if first["isError"] != true {
		t.Fatalf("the 1st call with --max-tool-calls 1 isError = %v, want true", first["isError"])
	}
	text := first["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "rig_tool_cap") {
		t.Errorf("cap response = %s", text)
	}
	second := responses[2].(map[string]any)
	if second["isError"] != true {
		t.Fatalf("a call past the cap isError = %v, want true", second["isError"])
	}
	if !fileExists(dir + "/stub-capped") {
		t.Error("stub-capped marker missing")
	}
}

// TestStubCapIsOffAtZero pins the "0 = no cap" reading: no call is refused and
// no marker is written.
func TestStubCapIsOffAtZero(t *testing.T) {
	dir := t.TempDir()
	stdin := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_task","arguments":{"taskId":"CLA-1"}}}`)
	var out bytes.Buffer
	if err := serveStub(context.Background(), stdin, &out, stubOptions{
		Scenario: "reasoning-replay", Out: dir, Catalog: testCatalog(t), MaxCalls: 0,
	}); err != nil {
		t.Fatalf("serveStub: %v", err)
	}
	responses := parseStubResponses(t, out.String())
	if responses[1].(map[string]any)["isError"] == true {
		t.Error("a call was refused with no cap set")
	}
	if fileExists(dir + "/stub-capped") {
		t.Error("stub-capped marker written with no cap set")
	}
}

// paddedEditCall is the shape a strict-mode client sends: every property the
// edit_task schema declares, each at its type's empty value. `over` sets the
// members a call really carries.
func paddedEditCall(over map[string]any) map[string]any {
	args := map[string]any{
		"taskId":    "CLA-753",
		"runId":     nil,
		"title":     "",
		"detail":    "",
		"doneWhen":  "",
		"category":  "",
		"repo":      nil,
		"priority":  nil,
		"schedule":  nil,
		"container": false,
		"humanOnly": false,
	}
	for k, v := range over {
		args[k] = v
	}
	return args
}

func changedOf(t *testing.T, res toolResult) []string {
	t.Helper()
	if res.IsError {
		t.Fatalf("call refused: %s", res.Text)
	}
	var payload struct {
		Changed []string `json:"changed"`
	}
	if err := json.Unmarshal([]byte(res.Text), &payload); err != nil {
		t.Fatalf("receipt %q: %v", res.Text, err)
	}
	return payload.Changed
}

// TestEditTaskPaddingClearsTheSetFlags is the ruled residual: `false` is the
// CLEAR spelling on these two flags, so a client that pads every boolean clears
// a flag the task had SET. The receipt names it, and nothing else moves.
func TestEditTaskPaddingClearsTheSetFlags(t *testing.T) {
	st := newEditTaskState()
	got := changedOf(t, editTaskReceipt(st, paddedEditCall(map[string]any{"detail": "the new body"})))
	if want := "detail,container,humanOnly"; strings.Join(got, ",") != want {
		t.Fatalf("changed = %v, want %s", got, want)
	}
	if st.container || st.humanOnly {
		t.Errorf("flags = container %v, humanOnly %v; a padded false must clear both", st.container, st.humanOnly)
	}
}

// TestEditTaskPaddingNullLeavesTheFlags pins the other half: null is the
// leave-it-alone spelling, so the same padded call with null on the flags moves
// only the real field.
func TestEditTaskPaddingNullLeavesTheFlags(t *testing.T) {
	st := newEditTaskState()
	got := changedOf(t, editTaskReceipt(st, paddedEditCall(map[string]any{
		"detail": "the new body", "container": nil, "humanOnly": nil,
	})))
	if want := "detail"; strings.Join(got, ",") != want {
		t.Fatalf("changed = %v, want %s", got, want)
	}
	if !st.container || !st.humanOnly {
		t.Errorf("flags = container %v, humanOnly %v; null must leave both", st.container, st.humanOnly)
	}
}

// TestEditTaskPaddingRepoClearVersusAbsent pins `repo`: "" is the CLEAR and null
// is absence, so the padded spelling leaves routing alone while an explicit ""
// corrects a wrong route.
func TestEditTaskPaddingRepoClearVersusAbsent(t *testing.T) {
	// The flags stay null here so the call isolates the repo spelling: this is
	// about `""` (the CLEAR) versus `null` (absence), where the padded call's
	// `""` on a text field is absence.
	st := newEditTaskState()
	clear := paddedEditCall(map[string]any{"repo": "", "container": nil, "humanOnly": nil})
	if got := changedOf(t, editTaskReceipt(st, clear)); strings.Join(got, ",") != "repo" {
		t.Fatalf("repo \"\" changed = %v, want [repo]", got)
	}

	absent := paddedEditCall(map[string]any{"container": nil, "humanOnly": nil})
	res := editTaskReceipt(newEditTaskState(), absent)
	if !res.IsError || !strings.Contains(res.Text, `"code":"nothing_to_do"`) {
		t.Fatalf("padded call with repo null = %s, want nothing_to_do", res.Text)
	}
}

// TestEditTaskPaddingDropsAFalseOnAnAlreadyFalseFlag covers CLA-745's half: the
// same padded false that CLEARED the flags is a no-op once they are clear, and a
// receipt must not claim it.
func TestEditTaskPaddingDropsAFalseOnAnAlreadyFalseFlag(t *testing.T) {
	st := newEditTaskState()
	_ = changedOf(t, editTaskReceipt(st, paddedEditCall(map[string]any{"detail": "one"})))
	got := changedOf(t, editTaskReceipt(st, paddedEditCall(map[string]any{"priority": "m"})))
	if want := "priority"; strings.Join(got, ",") != want {
		t.Fatalf("changed = %v, want %s", got, want)
	}
}

func TestEditTaskRefusesTwoLongFields(t *testing.T) {
	res := editTaskReceipt(newEditTaskState(), paddedEditCall(map[string]any{"title": "a title", "detail": "a body"}))
	if !res.IsError || !strings.Contains(res.Text, "one_long_field") || !strings.Contains(res.Text, "title and detail") {
		t.Fatalf("two long fields = %s, want one_long_field naming both", res.Text)
	}
}

func TestEditTaskNeedsATaskID(t *testing.T) {
	res := editTaskReceipt(newEditTaskState(), map[string]any{"detail": "a body"})
	if !res.IsError || !strings.Contains(res.Text, "missing_argument") || !strings.Contains(res.Text, "taskId") {
		t.Fatalf("call without taskId = %s, want missing_argument naming taskId", res.Text)
	}
}

func TestEveryScenarioHasAPromptAndResponder(t *testing.T) {
	for name, sc := range scenarios() {
		if sc.PromptFor == nil && strings.TrimSpace(sc.Prompt) == "" {
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

// TestSchemaScenarioPromptFollowsTheSelectedTool is the regression this exists
// for: a proposed tool (or any other catalog tool) must be the one the prompt
// asks for, or the scenario tests the wrong schema.
func TestSchemaScenarioPromptFollowsTheSelectedTool(t *testing.T) {
	schema := scenarios()["schema"]

	got := schema.promptFor("frobnicate")
	if !strings.Contains(got, "clankerbar_frobnicate") {
		t.Errorf("--schema-tool frobnicate prompt = %q, want it to ask for clankerbar_frobnicate", got)
	}
	if strings.Contains(got, "clankerbar_edit_task") {
		t.Errorf("--schema-tool frobnicate prompt still asks for edit_task: %q", got)
	}

	// The default selection names a tool this release's plane still serves: the
	// cutover deletes update_task, so a probe that defaulted to it would exercise
	// a name the plane answers as unknown.
	defaulted := schema.promptFor(defaultSchemaTool)
	if !strings.Contains(defaulted, "clankerbar_"+defaultSchemaTool) || !strings.Contains(defaulted, "send null") {
		t.Errorf("default schema prompt = %q", defaulted)
	}
	if defaultSchemaTool == "update_task" {
		t.Error("the schema probe defaults to update_task, which this release's plane does not serve")
	}

	// An already-prefixed name is not prefixed twice.
	if got := schemaToolName("clankerbar_frobnicate"); got != "clankerbar_frobnicate" {
		t.Errorf("schemaToolName = %q", got)
	}
	if got := schemaToolName(""); got != "clankerbar_"+defaultSchemaTool {
		t.Errorf("schemaToolName(\"\") = %q", got)
	}

	// A scenario that does not name a tool is unaffected by the flag.
	if got := scenarios()["padding"].promptFor("frobnicate"); strings.Contains(got, "frobnicate") {
		t.Errorf("padding prompt drifted with --schema-tool: %q", got)
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
