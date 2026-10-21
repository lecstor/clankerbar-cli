package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// paddedEditCalls drives the padding scenario's own stub over the three calls
// its prompt asks for, fully padded as a strict-mode client sends them, so the
// report tests read the receipts the scenario really produces: call 1 clears
// both flags, and calls 2 and 3 carry the same padded `false` as no-ops.
func paddedEditCalls(t *testing.T) []stubCall {
	t.Helper()
	respond := scenarios()["padding"].Respond
	sent := []string{
		`{"taskId":"CLA-753","detail":"the new body","repo":null,"priority":null,"schedule":null,"container":false,"humanOnly":false}`,
		`{"taskId":"CLA-753","detail":"","repo":"","priority":null,"schedule":null,"container":false,"humanOnly":false}`,
		`{"taskId":"CLA-753","detail":"","repo":null,"priority":null,"schedule":null,"container":false,"humanOnly":false}`,
	}
	calls := make([]stubCall, 0, len(sent))
	for i, raw := range sent {
		var args map[string]any
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			t.Fatal(err)
		}
		res := respond("edit_task", args)
		calls = append(calls, stubCall{
			Seq:       i + 1,
			Name:      "edit_task",
			Arguments: json.RawMessage(raw),
			Keys:      sortedKeys(args),
			IsError:   res.IsError,
			Result:    res.Text,
		})
	}
	return calls
}

func reportFixture(t *testing.T) (runRecord, []rigEvent, []stubCall, []capturedRequest) {
	t.Helper()
	cost := 0.002
	rec := runRecord{
		RunID: "padding-test", Scenario: "padding", Model: "commandcode/deepseek/deepseek-v4.1-flash",
		Provider: "commandcode", Upstream: "https://api.commandcode.ai/provider/v1",
		ProxyURL: "http://127.0.0.1:1", SchemaSource: "https://clankerbar.com/mcp/clankerbar",
		Tools: []string{"edit_task", "get_task"}, Workdir: "/runs/x/sandbox",
		Deadline: "5m0s", MaxToolCalls: 6, ExitCode: 0, DurationMS: 1234,
		StubStarted: true, IsolationDirs: []string{"XDG_DATA_HOME=/runs/x/xdg/data"},
		Prompt: "Call clankerbar_edit_task three times.",
	}
	calls := paddedEditCalls(t)
	events := []rigEvent{{Part: &rigPart{
		Type: "step-finish", Reason: "tool-calls", Cost: &cost,
		Tokens: &rigTokens{Input: 13836, Output: 96, Reasoning: 28, Cache: &rigCache{Read: 2048}},
	}}}
	requests := []capturedRequest{
		{
			Seq: 1, Method: "POST", Path: "/chat/completions", AuthOmit: true,
			Body: json.RawMessage(`{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"system","content":"s"},{"role":"user","content":"Call clankerbar_edit_task three times."}],"tools":[{"type":"function","function":{"name":"clankerbar_edit_task"}}],"stream":true,"reasoning_effort":"max"}`),
		},
		{
			Seq: 2, Method: "POST", Path: "/chat/completions", AuthOmit: true,
			Body: json.RawMessage(`{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"system","content":"s"},{"role":"user","content":"Call clankerbar_edit_task three times."},{"role":"assistant","content":null,"tool_calls":[{"id":"call_0"}],"reasoning_content":"thinking about the residual"},{"role":"tool","content":"{\"ok\":true}","tool_call_id":"call_0"}],"tools":[{"type":"function","function":{"name":"clankerbar_edit_task"}}],"stream":true}`),
		},
	}
	return rec, events, calls, requests
}

func TestRenderReportShowsKeysReasoningAndReplayedFields(t *testing.T) {
	rec, events, calls, requests := reportFixture(t)
	report := renderReport(rec, events, calls, requests)

	for _, want := range []string{
		"# opencode rig: padding",
		"`commandcode/deepseek/deepseek-v4.1-flash`",
		"| 1 | `edit_task` |",
		"`container=false`", // the CLEAR spelling, shown as sent
		"`repo=\"\"`",       // repo's clear
		"`repo=null`",       // repo's absence
		"`priority=null`",   // a nullable enum padded null
		"ok; changed: detail, container, humanOnly",
		"ok; changed: repo",
		"error `nothing_to_do`",
		"| 1 | tool-calls | 13836 | 96 | 28 | 2048 | 0.002000 |",
		"`reasoning_content`=string(", // the replay field
		"reasoning replay: 1 of 1",
		"reasoning tokens reported on the event stream: 28",
		"`edit_task` padding by call",
		"`container=false` (CLEARS a set flag)",
		"`humanOnly=false` (CLEARS a set flag)",
		"`container=false` (no-op (already false))",
		"`humanOnly=false` (no-op (already false))",
		"`repo=\"\"` (CLEARS to primary)",
		"`repo=null` (absent)",
		"`opencode-config-content.json`",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report is missing %q\n---\n%s", want, report)
		}
	}
	// The auth header is never part of the report's evidence.
	if strings.Contains(report, "authorization header recorded: true") {
		t.Error("report claims the Authorization header was recorded")
	}
}

// TestEditTaskResidualSeparatesAClearFromANoOp is the MUST from review round 1:
// after call 1 has cleared both flags, calls 2 and 3 send the same padded
// `false` and the stub drops it as a no-op (CLA-745, decision c06cdfc7). The
// observation must follow the RECEIPT, not the argument, so it can never claim
// a clear that did not happen.
func TestEditTaskResidualSeparatesAClearFromANoOp(t *testing.T) {
	lines := editTaskResidual(paddedEditCalls(t))
	if len(lines) != 3 {
		t.Fatalf("lines = %q, want one per edit_task call", lines)
	}
	for _, want := range []string{"`container=false` (CLEARS a set flag)", "`humanOnly=false` (CLEARS a set flag)"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("call 1 = %q, want %q (both flags start SET)", lines[0], want)
		}
	}
	for _, line := range lines[1:] {
		if strings.Contains(line, "CLEARS a set flag") {
			t.Errorf("later call claims a clear the receipt does not show: %q", line)
		}
		if strings.Count(line, "no-op (already false)") != 2 {
			t.Errorf("later call = %q, want both padded falses reported as no-ops", line)
		}
	}
}

// TestFlagClearLabelUsesTheReceiptNotTheArgument pins every branch: a `false`
// is only a clear when the call's own receipt lists the field as changed.
func TestFlagClearLabelUsesTheReceiptNotTheArgument(t *testing.T) {
	cases := []struct {
		name string
		call stubCall
		want string
	}{
		{"the receipt lists the field", stubCall{Result: `{"changed":["container"]}`}, "CLEARS a set flag"},
		{"a receipt without the field", stubCall{Result: `{"changed":["repo"]}`}, "no-op (already false)"},
		{"refused nothing_to_do", stubCall{IsError: true, Result: `{"error":{"code":"nothing_to_do"}}`}, "no-op (already false)"},
		{"refused for another reason", stubCall{IsError: true, Result: `{"error":{"code":"one_long_field"}}`}, "not applied (call refused `one_long_field`)"},
		{"no receipt to read", stubCall{Result: `{"ok":true}`}, "the CLEAR spelling (the receipt does not say what changed)"},
	}
	for _, tc := range cases {
		if got := flagClearLabel("container", editTaskReceiptFactsOf(tc.call)); got != tc.want {
			t.Errorf("%s: label = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestRenderReportExplainsAZeroRequestRun(t *testing.T) {
	rec, _, _, _ := reportFixture(t)
	rec.StubStarted = false
	report := renderReport(rec, nil, nil, nil)
	for _, want := range []string{
		"No tools/call reached the stub.",
		"The stub never started",
		"No request reached the proxy.",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("empty-run report is missing %q\n%s", want, report)
		}
	}
}

func TestRenderKeysMarksTheEmptyValues(t *testing.T) {
	got := renderKeys(json.RawMessage(`{"b":null,"a":"x","c":0,"d":false,"e":"","f":true}`))
	want := "`a`, `b=null`, `c`, `d=false`, `e=\"\"`, `f=true`"
	if got != want {
		t.Fatalf("renderKeys = %s, want %s", got, want)
	}
}

func TestMessageMetaOfKeepsUnknownFields(t *testing.T) {
	m := messageMetaOf(3, json.RawMessage(`{"role":"assistant","content":null,"tool_calls":[{}],"reasoning_content":"abc","signature":"xyz"}`))
	if m.Role != "assistant" || !m.ContentNil || m.ToolCalls != 1 {
		t.Fatalf("meta = %+v", m)
	}
	names := []string{}
	for _, e := range m.Extras {
		names = append(names, e.Name)
		if e.Kind == "string" && e.Len != 3 {
			t.Errorf("%s: string length = %d, want 3 (the content, not the JSON encoding)", e.Name, e.Len)
		}
	}
	if strings.Join(names, ",") != "reasoning_content,signature" {
		t.Fatalf("extras = %v", names)
	}
	// An empty reasoning_content is replayed-but-empty: it must read as 0, not
	// as the two bytes of its JSON quotes.
	empty := messageMetaOf(0, json.RawMessage(`{"role":"assistant","content":null,"reasoning_content":""}`))
	if len(empty.Extras) != 1 || empty.Extras[0].Len != 0 {
		t.Fatalf("empty reasoning_content = %+v, want string(0)", empty.Extras)
	}
}

func TestReplayStatsOnlyCountsLaterRequests(t *testing.T) {
	_, _, _, requests := reportFixture(t)
	replayed, assistants := replayStats(requests)
	if replayed != 1 || assistants != 1 {
		t.Fatalf("replayStats = %d/%d, want 1/1", replayed, assistants)
	}
}

func TestWriteReportWritesTheSameText(t *testing.T) {
	rec, events, calls, requests := reportFixture(t)
	path := filepath.Join(t.TempDir(), "report.md")
	if err := writeReport(path, rec, events, calls, requests); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != renderReport(rec, events, calls, requests) {
		t.Error("writeReport and renderReport disagree")
	}
}

func TestReadEventsAndCallsTolerateATruncatedLastLine(t *testing.T) {
	dir := t.TempDir()
	events := filepath.Join(dir, "events.jsonl")
	os.WriteFile(events, []byte(`{"type":"step_finish","part":{"type":"step-finish"}}
{"type":"text","part":{"type":"te`), 0o644)
	got, err := readEvents(events)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1 complete line", len(got))
	}
	calls := filepath.Join(dir, "calls.jsonl")
	os.WriteFile(calls, []byte(`{"seq":1,"name":"x","argumentKeys":["a"],"arguments":{}}`+"\n"+`{"seq":`), 0o644)
	gotCalls, err := readStubCalls(calls)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotCalls) != 1 || gotCalls[0].Name != "x" {
		t.Fatalf("calls = %+v", gotCalls)
	}
}
