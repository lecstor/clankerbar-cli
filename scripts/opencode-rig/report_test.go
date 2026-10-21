package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func reportFixture() (runRecord, []rigEvent, []stubCall, []capturedRequest) {
	cost := 0.002
	rec := runRecord{
		RunID: "padding-test", Scenario: "padding", Model: "commandcode/deepseek/deepseek-v4.1-flash",
		Provider: "commandcode", Upstream: "https://api.commandcode.ai/provider/v1",
		ProxyURL: "http://127.0.0.1:1", SchemaSource: "https://clankerbar.com/mcp/clankerbar",
		Tools: []string{"update_task", "get_task"}, Workdir: "/runs/x/sandbox",
		Deadline: "5m0s", MaxToolCalls: 6, ExitCode: 0, DurationMS: 1234,
		StubStarted: true, IsolationDirs: []string{"XDG_DATA_HOME=/runs/x/xdg/data"},
		Prompt: "Call clankerbar_update_task.",
	}
	calls := []stubCall{{
		Seq: 1, Name: "update_task",
		Arguments: json.RawMessage(`{"taskId":"CLA-753","runId":"rig-run-1","review":{"reviewerRole":"review-1","verdict":"pass"},"status":null,"priority":null}`),
		Keys:      []string{"priority", "review", "runId", "status", "taskId"},
		IsError:   true,
		Result:    `{"error":{"code":"invalid_review","message":"a review call carries only taskId, runId and review."}}`,
	}}
	events := []rigEvent{{Part: &rigPart{
		Type: "step-finish", Reason: "tool-calls", Cost: &cost,
		Tokens: &rigTokens{Input: 13836, Output: 96, Reasoning: 28, Cache: &rigCache{Read: 2048}},
	}}}
	requests := []capturedRequest{
		{
			Seq: 1, Method: "POST", Path: "/chat/completions", AuthOmit: true,
			Body: json.RawMessage(`{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"system","content":"s"},{"role":"user","content":"Call clankerbar_update_task."}],"tools":[{"type":"function","function":{"name":"clankerbar_update_task"}}],"stream":true,"reasoning_effort":"max"}`),
		},
		{
			Seq: 2, Method: "POST", Path: "/chat/completions", AuthOmit: true,
			Body: json.RawMessage(`{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"system","content":"s"},{"role":"user","content":"Call clankerbar_update_task."},{"role":"assistant","content":null,"tool_calls":[{"id":"call_0"}],"reasoning_content":"thinking about the schema"},{"role":"tool","content":"{\"ok\":true}","tool_call_id":"call_0"}],"tools":[{"type":"function","function":{"name":"clankerbar_update_task"}}],"stream":true}`),
		},
	}
	return rec, events, calls, requests
}

func TestRenderReportShowsKeysReasoningAndReplayedFields(t *testing.T) {
	rec, events, calls, requests := reportFixture()
	report := renderReport(rec, events, calls, requests)

	for _, want := range []string{
		"# opencode rig: padding",
		"`commandcode/deepseek/deepseek-v4.1-flash`",
		"| 1 | `update_task` |",
		"`priority=null`", // the padding signal
		"`status=null`",
		"error `invalid_review`",
		"| 1 | tool-calls | 13836 | 96 | 28 | 2048 | 0.002000 |",
		"`reasoning_content`=string(", // the replay field
		"reasoning replay: 1 of 1",
		"reasoning tokens reported on the event stream: 28",
		"review padding:",
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

func TestRenderReportExplainsAZeroRequestRun(t *testing.T) {
	rec, _, _, _ := reportFixture()
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

func TestRenderKeysMarksNulls(t *testing.T) {
	got := renderKeys(json.RawMessage(`{"b":null,"a":"x","c":0,"d":false}`))
	want := "`a`, `b=null`, `c`, `d`"
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
	}
	if strings.Join(names, ",") != "reasoning_content,signature" {
		t.Fatalf("extras = %v", names)
	}
}

func TestReplayStatsOnlyCountsLaterRequests(t *testing.T) {
	_, _, _, requests := reportFixture()
	replayed, assistants := replayStats(requests)
	if replayed != 1 || assistants != 1 {
		t.Fatalf("replayStats = %d/%d, want 1/1", replayed, assistants)
	}
}

func TestWriteReportWritesTheSameText(t *testing.T) {
	rec, events, calls, requests := reportFixture()
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
