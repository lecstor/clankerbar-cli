package harness

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// copilotParsed runs a captured stdout stream through exactly the machinery
// Invoke uses: the lineSink that splits it into whole lines, and the incremental
// parser that reads them as they arrive.
func copilotParsed(stdout string) Result {
	return copilotParsedIn(Invocation{}, stdout)
}

func copilotParsedIn(in Invocation, stdout string) Result {
	res := newSessionResult(in)
	// The real Invoke retains the tail it parsed; mirror that so the
	// classification tests can run against a Result shaped like production's.
	res.Stdout = stdout
	var p copilotParse
	sink := newLineSink(func(line []byte) { p.line(line, &res, io.Discard) })
	_, _ = io.WriteString(sink, stdout)
	sink.Flush()
	p.finish(&res)
	return res
}

// The captured stream is a REAL `copilot --output-format json` run of Copilot CLI
// 1.0.112 against clankerbar's /mcp (testdata/copilot/mcp-smoke.jsonl): one
// get_backlog_summary tool call and its answer, with the bulky per-request cache
// diagnostics stripped. It is the fixture that pins every figure this adapter
// reports to what the CLI actually emitted.
func copilotSmokeStream(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("testdata/copilot/mcp-smoke.jsonl")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(data)
}

func TestCopilotParse_capturedSmoke(t *testing.T) {
	res := copilotParsed(copilotSmokeStream(t))

	wantMsg := "clankerbar\nbacklog=169, ready=13, in_progress=2, in_review=0, approved=1, blocked=0, parked=2, archived=47, done=655"
	if res.FinalMessage != wantMsg {
		t.Errorf("FinalMessage = %q, want the final_answer message %q", res.FinalMessage, wantMsg)
	}
	// tokenDetails is the disjoint report: fresh input 15811 + cache read 29824
	// + output 65. (modelMetrics.usage.inputTokens is the INCLUSIVE 45635, so a
	// field-for-field copy of that would double-count the cache.)
	if want := 15811 + 29824 + 65; res.Tokens != want {
		t.Errorf("Tokens = %d, want %d", res.Tokens, want)
	}
	wantUsage := TokenUsage{Input: 15811, Output: 65, CacheRead: 29824}
	if res.Usage != wantUsage {
		t.Errorf("Usage = %+v, want %+v", res.Usage, wantUsage)
	}
	if !res.UsageReported {
		t.Error("UsageReported = false, want true: the stream carried a usage checkpoint")
	}
	if res.CostReported {
		t.Error("CostReported = true: copilot reports AI credits, never dollars")
	}
	credits, ok := res.Raw["ai_credits"].(float64)
	if !ok || credits < 0.3836 || credits > 0.3837 {
		t.Errorf("Raw[ai_credits] = %v, want 0.383668", res.Raw["ai_credits"])
	}
	if got := res.Raw["premium_requests"]; got != 1 {
		t.Errorf("Raw[premium_requests] = %v, want 1", got)
	}
	if got := res.ClankerbarCalls; got != 1 {
		t.Errorf("ClankerbarCalls = %d, want 1 for the completed get_backlog_summary", got)
	}
	mcp, _ := res.Raw["mcp_servers"].(map[string]string)
	if mcp["clankerbar"] != "connected" {
		t.Errorf("Raw[mcp_servers][clankerbar] = %q, want connected (%v)", mcp["clankerbar"], mcp)
	}
}

// Two usage checkpoints: the CLI's counters are cumulative, so the LAST one is
// the session total. Summing would double-count every call the earlier
// checkpoint already included.
func TestCopilotParse_usageCheckpointsAreCumulative(t *testing.T) {
	stream := `{"type":"session.usage_checkpoint","data":{"totalNanoAiu":100,"totalPremiumRequests":1,"accountingSnapshot":{"totalNanoAiu":100,"tokenDetails":{"input":{"tokenCount":100},"output":{"tokenCount":10},"cache_read":{"tokenCount":0},"cache_write":{"tokenCount":0}}}}}`
	stream += "\n" + `{"type":"assistant.message","data":{"content":"working"}}`
	stream += "\n" + `{"type":"session.usage_checkpoint","data":{"totalNanoAiu":300,"totalPremiumRequests":3,"accountingSnapshot":{"totalNanoAiu":300,"tokenDetails":{"input":{"tokenCount":300},"output":{"tokenCount":30},"cache_read":{"tokenCount":50},"cache_write":{"tokenCount":5}}}}}`
	res := copilotParsed(stream)
	if want := 300 + 30 + 50 + 5; res.Tokens != want {
		t.Errorf("Tokens = %d, want %d (the last cumulative checkpoint, not the sum)", res.Tokens, want)
	}
	if got := res.Raw["premium_requests"]; got != 3 {
		t.Errorf("premium_requests = %v, want the last checkpoint's 3", got)
	}
}

// An older/partial snapshot may carry only modelMetrics, whose inputTokens is
// INCLUSIVE of the cache buckets; they have to be subtracted or the four buckets
// double count.
func TestCopilotParse_modelMetricsFallback(t *testing.T) {
	stream := `{"type":"session.usage_checkpoint","data":{"accountingSnapshot":{"modelMetrics":{"m1":{"usage":{"inputTokens":1000,"outputTokens":100,"cacheReadTokens":200,"cacheWriteTokens":50,"reasoningTokens":10}}}}}}`
	res := copilotParsed(stream)
	want := TokenUsage{Input: 750, Output: 100, CacheRead: 200, CacheWrite: 50}
	if res.Usage != want {
		t.Errorf("Usage = %+v, want %+v", res.Usage, want)
	}
	// Reasoning is a diagnostic here, not a fifth bucket: the CLI folds it into
	// outputTokens, so adding it would overstate the session's billed total.
	if got := res.Raw["reasoning_tokens"]; got != 10 {
		t.Errorf("reasoning_tokens = %v, want 10", got)
	}
	if want := 750 + 100 + 200 + 50; res.Tokens != want {
		t.Errorf("Tokens = %d, want %d", res.Tokens, want)
	}
}

// A zero tokenDetails field is not evidence of a hole: the CLI reported, the
// report happened to be zero. UsageReported must stay true so the driver bounds
// only attempts that reported NOTHING (CLA-288).
func TestCopilotParse_zeroUsageIsStillAReport(t *testing.T) {
	res := copilotParsed(`{"type":"session.usage_checkpoint","data":{"accountingSnapshot":{"tokenDetails":{"input":{"tokenCount":0},"output":{"tokenCount":0},"cache_read":{"tokenCount":0},"cache_write":{"tokenCount":0}}}}}`)
	if !res.UsageReported {
		t.Error("UsageReported = false for a checkpoint that carried an all-zero report")
	}
	if res.Tokens != 0 {
		t.Errorf("Tokens = %d, want 0", res.Tokens)
	}
}

// A zero tokenDetails with real modelMetrics: the fallback has to run, or the
// budget would see a session that reported nothing.
func TestCopilotParse_zeroTokenDetailsFallsBackToModelMetrics(t *testing.T) {
	res := copilotParsed(`{"type":"session.usage_checkpoint","data":{"totalNanoAiu":5000000000,"accountingSnapshot":{"tokenDetails":{"input":{"tokenCount":0},"output":{"tokenCount":0},"cache_read":{"tokenCount":0},"cache_write":{"tokenCount":0}},"modelMetrics":{"m1":{"usage":{"inputTokens":80,"outputTokens":20,"cacheReadTokens":30}}}}}}`)
	if want := (80 - 30) + 20 + 30; res.Tokens != want {
		t.Errorf("Tokens = %d, want %d from the modelMetrics fallback", res.Tokens, want)
	}
	if got := res.Raw["ai_credits"]; got != float64(5) {
		t.Errorf("ai_credits = %v, want 5", got)
	}
}

func TestCopilotParse_empty(t *testing.T) {
	res := copilotParsed("")
	if res.FinalMessage != "" || res.Tokens != 0 || res.UsageReported {
		t.Errorf("empty stream: got msg=%q tokens=%d usageReported=%v, want empty/0/false",
			res.FinalMessage, res.Tokens, res.UsageReported)
	}
}

// The stream arrives in whatever chunks the pipe hands over. An event cut in
// half must still parse as the whole one does, or a usage checkpoint would be a
// silent budget under-count.
func TestCopilotParse_streamSplitMidEvent(t *testing.T) {
	stream := copilotSmokeStream(t)
	res := newSessionResult(Invocation{})
	var p copilotParse
	sink := newLineSink(func(line []byte) { p.line(line, &res, io.Discard) })
	for i := 0; i < len(stream); i += 7 {
		end := min(i+7, len(stream))
		if _, err := io.WriteString(sink, stream[i:end]); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	sink.Flush()
	p.finish(&res)
	if want := 15811 + 29824 + 65; res.Tokens != want {
		t.Errorf("Tokens = %d, want %d — a 7-byte-chunked stream must parse as the whole one does", res.Tokens, want)
	}
	if res.FinalMessage == "" {
		t.Error("FinalMessage is empty after a chunked stream")
	}
}

// A claim_task request and its accepted result, in copilot's event shapes. The
// claim's ids live in the tool RESULT, and the result's content is the MCP text
// as a JSON string — exactly what the shared observer reads for claude and
// opencode.
func TestCopilotParse_observesAClaim(t *testing.T) {
	const taskID, runID = "11111111-2222-3333-4444-555555555555", "run-copilot-1"
	claimContent := `{"task":{"id":"` + taskID + `","ref":"CLA-118"},"run":{"id":"` + runID + `"}}`
	stream := `{"type":"assistant.message","data":{"content":"","toolRequests":[{"toolCallId":"c1","name":"clankerbar-claim_task","arguments":{"taskId":"` + taskID + `"},"mcpServerName":"clankerbar","mcpToolName":"claim_task"}]}}`
	stream += "\n" + `{"type":"tool.execution_start","data":{"toolCallId":"c1","arguments":{"taskId":"` + taskID + `"},"mcpServerName":"clankerbar","mcpToolName":"claim_task","success":true}}`
	stream += "\n" + `{"type":"tool.execution_complete","data":{"toolCallId":"c1","success":true,"result":{"content":` + jsonString(t, claimContent) + `}}}`
	stream += "\n" + `{"type":"assistant.message","data":{"content":"claimed","phase":"final_answer"}}`
	res := copilotParsed(stream)

	if !res.Claim.Held() {
		t.Fatalf("Claim = %+v, want held after an accepted claim_task", res.Claim)
	}
	if res.Claim.TaskID != taskID || res.Claim.RunID != runID || res.Claim.Ref != "CLA-118" {
		t.Errorf("Claim = %+v, want task %s run %s ref CLA-118", res.Claim, taskID, runID)
	}
	if res.ClankerbarCalls != 1 {
		t.Errorf("ClankerbarCalls = %d, want 1", res.ClankerbarCalls)
	}
	if res.FinalMessage != "claimed" {
		t.Errorf("FinalMessage = %q", res.FinalMessage)
	}
}

// A REFUSED claim must not be tracked: the plane's refusal carries no ids, and a
// half-claim is the one thing the driver must never be handed.
func TestCopilotParse_refusedClaimIsNotTracked(t *testing.T) {
	stream := `{"type":"assistant.message","data":{"content":"","toolRequests":[{"toolCallId":"c1","name":"clankerbar-claim_task","arguments":{"taskId":"t-1"},"mcpServerName":"clankerbar","mcpToolName":"claim_task"}]}}`
	stream += "\n" + `{"type":"tool.execution_complete","data":{"toolCallId":"c1","success":false,"error":"task already claimed","result":{"content":"task already claimed"}}}`
	res := copilotParsed(stream)
	if res.Claim.Held() {
		t.Errorf("Claim = %+v, want no claim after a refused claim_task", res.Claim)
	}
	if res.ClankerbarCalls != 0 {
		t.Errorf("ClankerbarCalls = %d: a refused call proves nothing about reachability", res.ClankerbarCalls)
	}
}

// submit_for_review settles the tracked claim, which is what tells the driver
// not to hand the task back.
func TestCopilotParse_settlingCallSettlesTheClaim(t *testing.T) {
	const taskID, runID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "run-2"
	claim := `{"task":{"id":"` + taskID + `","ref":"CLA-1"},"run":{"id":"` + runID + `"}}`
	stream := `{"type":"assistant.message","data":{"content":"","toolRequests":[{"toolCallId":"c1","name":"clankerbar-claim_task","arguments":{"taskId":"` + taskID + `"},"mcpServerName":"clankerbar","mcpToolName":"claim_task"}]}}`
	stream += "\n" + `{"type":"tool.execution_complete","data":{"toolCallId":"c1","success":true,"result":{"content":` + jsonString(t, claim) + `}}}`
	stream += "\n" + `{"type":"assistant.message","data":{"content":"","toolRequests":[{"toolCallId":"c2","name":"clankerbar-submit_for_review","arguments":{"taskId":"` + taskID + `","branch":"clanker/x"},"mcpServerName":"clankerbar","mcpToolName":"submit_for_review"}]}}`
	stream += "\n" + `{"type":"tool.execution_complete","data":{"toolCallId":"c2","success":true,"result":{"content":"{\"status\":\"in_review\"}"}}}`
	res := copilotParsed(stream)
	if !res.Claim.Settled {
		t.Errorf("Claim.Settled = false, want true after an accepted submit_for_review (%+v)", res.Claim)
	}
	if len(res.Reports) == 0 {
		t.Error("no Report recorded for the branch the review call carried")
	}
}

func jsonString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal %q: %v", s, err)
	}
	return string(b)
}

// flagPositions returns the argv positions of flag, so a test can assert a flag is
// absent without matching a prompt that merely contains its text.
func flagPositions(args []string, flag string) []int {
	var at []int
	for i, a := range args {
		if a == flag {
			at = append(at, i)
		}
	}
	return at
}

func TestCopilotArgs(t *testing.T) {
	in := Invocation{
		Prompt:        "Work the backlog.",
		Model:         "  gpt-5.6-sol  ",
		MCPConfigPath: "/work/.mcp.json",
		ExtraDirs:     []string{"/repos/one", "/repos/two"},
	}
	args := copilotArgs(in)
	joined := strings.Join(args, " ")

	for _, want := range []string{"--output-format json", "--no-ask-user", "--additional-mcp-config @/work/.mcp.json"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q missing %q", joined, want)
		}
	}
	if i := indexOf(args, "--model"); i < 0 || args[i+1] != "gpt-5.6-sol" {
		t.Errorf("args %q: --model must carry the trimmed alias", joined)
	}
	// The fail-closed posture: the four grants, each as its own flag/value pair.
	for _, rule := range []string{"read", "write", "shell", "clankerbar"} {
		found := false
		for _, i := range flagPositions(args, "--allow-tool") {
			if i+1 < len(args) && args[i+1] == rule {
				found = true
			}
		}
		if !found {
			t.Errorf("args %q: missing --allow-tool %s", joined, rule)
		}
	}
	for _, dir := range in.ExtraDirs {
		found := false
		for _, i := range flagPositions(args, "--add-dir") {
			if i+1 < len(args) && args[i+1] == dir {
				found = true
			}
		}
		if !found {
			t.Errorf("args %q: missing --add-dir %s", joined, dir)
		}
	}
	if len(args) < 2 || args[len(args)-2] != "-p" || args[len(args)-1] != in.Prompt {
		t.Errorf("args %q: the prompt must be the VALUE of a trailing -p", joined)
	}
	for _, never := range []string{"--allow-all", "--allow-all-tools", "--allow-all-paths", "--allow-all-urls", "--yolo"} {
		if len(flagPositions(args, never)) > 0 {
			t.Errorf("args %q carries %s, which would defeat the fail-closed posture", joined, never)
		}
	}
}

// A blank model emits no flag at all; the shared ratchet covers the raw-field
// read, and this pins the flag's absence for THIS adapter's builder.
func TestCopilotArgs_blankModelEmitsNoFlag(t *testing.T) {
	for _, blank := range []string{"", " ", "\t", "  \n "} {
		args := copilotArgs(Invocation{Prompt: "work", Model: blank})
		if len(flagPositions(args, "--model")) > 0 {
			t.Errorf("args %q for model %q: --model must be absent", strings.Join(args, " "), blank)
		}
	}
}

// A probe is read-only by construction: no write/shell grant, explicit denies,
// and its only allow is the cheap clankerbar read - never --allow-all.
func TestCopilotProbeIsReadOnly(t *testing.T) {
	args := copilotArgs(Invocation{Probe: true, Prompt: "Work the backlog.", MCPConfigPath: "/work/.mcp.json"})
	joined := strings.Join(args, " ")
	for _, denied := range []string{"write", "shell"} {
		found := false
		for _, i := range flagPositions(args, "--deny-tool") {
			if i+1 < len(args) && args[i+1] == denied {
				found = true
			}
		}
		if !found {
			t.Errorf("probe args %q: missing --deny-tool %s", joined, denied)
		}
	}
	for _, i := range flagPositions(args, "--allow-tool") {
		if i+1 < len(args) && args[i+1] != "clankerbar(get_backlog_summary)" {
			t.Errorf("probe args %q: only the read-only clankerbar tool may be allowed, got %q", joined, args[i+1])
		}
	}
	if !strings.Contains(joined, "get_backlog_summary") {
		t.Errorf("probe args %q: the prompt must ask for the read-only clankerbar call that proves the MCP wiring", joined)
	}
	if !strings.HasSuffix(joined, "-p "+copilotPrompt(Invocation{Probe: true, MCPConfigPath: "/work/.mcp.json"})) {
		t.Errorf("probe args %q do not end with the probe prompt", joined)
	}
}

// Without an MCP config a probe has no server to reach, so it stays the
// cheapest possible request.
func TestCopilotProbeWithoutMCPIsJustAPing(t *testing.T) {
	args := copilotArgs(Invocation{Probe: true, Prompt: "Work the backlog."})
	if !strings.HasSuffix(strings.Join(args, " "), "-p .") {
		t.Errorf("args %q: want the bare '.' probe prompt", strings.Join(args, " "))
	}
	for _, i := range flagPositions(args, "--allow-tool") {
		t.Errorf("args %q: a probe without an MCP config granted %q", strings.Join(args, " "), args[i+1])
	}
}

func TestCopilotDetectLimit(t *testing.T) {
	cases := []struct {
		name   string
		res    Result
		want   bool
		stop   bool
		reason string
	}{
		{
			name: "documented plan exhaustion",
			res:  Result{ExitCode: 1, Stdout: `{"type":"session.error","data":{"errorType":"query","message":"Plan: 100% used, resets 2026-12-01"}}`},
			want: true, stop: true, reason: "budget_exhausted",
		},
		{
			name: "service quota message",
			res:  Result{Stderr: "Error: quota exceeded - upgrade or enable additional usage"},
			want: true, stop: true, reason: "budget_exhausted",
		},
		{
			name: "exceeded the included allowance",
			res:  Result{Stderr: "You have exceeded your included AI credits for this month."},
			want: true, stop: true, reason: "budget_exhausted",
		},
		{
			name: "insufficient credits",
			res:  Result{Stdout: `{"type":"session.error","data":{"message":"Insufficient AI credits"}}`},
			want: true, stop: true, reason: "budget_exhausted",
		},
		{
			name: "out of credits",
			res:  Result{Stderr: "You are out of AI credits for this month."},
			want: true, stop: true, reason: "budget_exhausted",
		},
		{
			name: "documented weekly rate limit",
			res:  Result{ExitCode: 1, Stdout: `{"type":"session.error","data":{"message":"You've reached your weekly rate limit."}}`},
			want: true, stop: true, reason: "usage_limit",
		},
		{
			name: "documented session rate limit",
			res:  Result{Stderr: "You've hit your session rate limit."},
			want: true, stop: true, reason: "usage_limit",
		},
		{
			name: "documented per-model rate limit",
			res:  Result{Stderr: "You've hit the rate limit for this model."},
			want: true, stop: true, reason: "usage_limit",
		},
		{
			name: "the 75% notice is a warning, not a limit",
			res:  Result{Stdout: `{"type":"session.warning","data":{"message":"You've used over 75% of your weekly usage limit. Your limit resets soon."}}`},
		},
		{
			name: "a task body quoting the plan exhaustion is narration",
			res:  Result{Stdout: `{"type":"assistant.message","data":{"content":"The task says: Plan: 100% used, resets 2026-12-01"}}`},
		},
		{
			name: "a tool result quoting it is tool output",
			res:  Result{Stdout: `{"type":"tool.execution_complete","data":{"toolCallId":"c1","success":true,"result":{"content":"Out of AI credits, quota exceeded"}}}`},
		},
		{
			name: "a FAILED tool call quoting it is still tool output",
			res:  Result{Stdout: `{"type":"tool.execution_complete","data":{"toolCallId":"c1","success":false,"error":"grep: Plan: 100% used","result":{"content":"grep: Plan: 100% used"}}}`},
		},
		{
			name: "a 429 is transient, not a limit",
			res:  Result{ExitCode: 1, Stdout: `{"type":"session.error","data":{"message":"Execution failed: 429 Too Many Requests"}}`},
		},
		{
			name: "an unavailable model is neither",
			res:  Result{ExitCode: 1, Stdout: `{"type":"session.error","data":{"errorType":"query","message":"Execution failed: Model \"nope\" is not available."}}`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lim := (copilot{}).DetectLimit(tc.res)
			if lim.Limited != tc.want {
				t.Fatalf("DetectLimit.Limited = %v, want %v", lim.Limited, tc.want)
			}
			if !tc.want {
				return
			}
			if lim.Stop != tc.stop {
				t.Errorf("Limit.Stop = %v, want %v (metered billing: no wait)", lim.Stop, tc.stop)
			}
			if lim.Reason != tc.reason {
				t.Errorf("Limit.Reason = %q, want %q", lim.Reason, tc.reason)
			}
		})
	}
}

func TestCopilotIsTransient(t *testing.T) {
	cases := []struct {
		name string
		res  Result
		want bool
	}{
		{"429 error event", Result{ExitCode: 1, Stdout: `{"type":"session.error","data":{"message":"Execution failed: 429 Too Many Requests"}}`}, true},
		{"service unavailable", Result{Stderr: "Error: 503 Service Unavailable"}, true},
		{"connection reset", Result{Stderr: "Error: read ECONNRESET"}, true},
		{"socket hang up", Result{Stderr: "Error: socket hang up"}, true},
		{"unavailable model is a real failure", Result{ExitCode: 1, Stdout: `{"type":"session.error","data":{"errorType":"query","message":"Execution failed: Model \"nope\" is not available."}}`}, false},
		{"clean run", Result{Stdout: `{"type":"assistant.message","data":{"content":"done"}}`}, false},
		{"a task body mentioning a 500 is narration", Result{Stdout: `{"type":"assistant.message","data":{"content":"the server returned 500 earlier"}}`}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (copilot{}).IsTransient(tc.res); got != tc.want {
				t.Errorf("IsTransient = %v, want %v", got, tc.want)
			}
		})
	}
}

// Diagnostic must read the same scope the classifiers do: the CLI's own text,
// never the agent's narration or a tool's output.
func TestCopilotDiagnosticExcludesNarrationAndToolOutput(t *testing.T) {
	res := Result{
		Stderr: "Warning: Skipped workspace MCP servers (.mcp.json) because the folder is not trusted.",
		Stdout: strings.Join([]string{
			`{"type":"assistant.message","data":{"content":"backlog body quoting Plan: 100% used, resets 2030"}}`,
			`{"type":"tool.execution_complete","data":{"toolCallId":"c1","success":true,"result":{"content":"quota exceeded"}}}`,
			`{"type":"session.error","data":{"errorType":"query","message":"Execution failed: connection reset by peer"}}`,
		}, "\n"),
	}
	diag := (copilot{}).Diagnostic(res)
	if !strings.Contains(diag, "connection reset by peer") {
		t.Errorf("Diagnostic %q does not carry the harness's own error", diag)
	}
	if strings.Contains(diag, "Plan: 100% used") || strings.Contains(diag, "quota exceeded") {
		t.Errorf("Diagnostic %q copied agent/tool text into the classification scope", diag)
	}
	if lim := (copilot{}).DetectLimit(res); lim.Limited {
		t.Errorf("DetectLimit fired on text outside the diagnostic scope: %+v", lim)
	}
}

// The captured invalid-model stream: a real error event and exit 1, which is a
// real failure - neither a limit nor a blip.
func TestCopilotCapturedModelError(t *testing.T) {
	data, err := os.ReadFile("testdata/copilot/model-unavailable.jsonl")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	res := copilotParsed(string(data))
	if lim := (copilot{}).DetectLimit(res); lim.Limited {
		t.Errorf("DetectLimit = %+v, want no limit", lim)
	}
	if (copilot{}).IsTransient(res) {
		t.Error("IsTransient = true for a model that does not exist")
	}
	if !strings.Contains((copilot{}).Diagnostic(res), "not available") {
		t.Errorf("Diagnostic %q does not name the real failure", (copilot{}).Diagnostic(res))
	}
}

func TestCopilotEnvPinsCopilotHome(t *testing.T) {
	env := (copilot{}).env(Invocation{WorkDir: t.TempDir(), ConfigDir: "/config/copilot"})
	found := false
	for _, e := range env {
		if e == "COPILOT_HOME=/config/copilot" {
			found = true
		}
	}
	if !found {
		t.Errorf("env %v does not carry COPILOT_HOME from ConfigDir", env)
	}
}

// An ambient COPILOT_ALLOW_ALL=true would approve every tool AND trust the
// folder - skills, plugins, MCP servers and hooks included - so it must not
// survive into a session, and neither must the probabilistic assisted judge. A
// config-declared value does not reopen them either.
func TestCopilotEnvClosesTheAmbientApprovalShortcuts(t *testing.T) {
	t.Setenv("COPILOT_ALLOW_ALL", "true")
	t.Setenv("COPILOT_ASSISTED_APPROVAL", "true")
	env := (copilot{}).env(Invocation{
		WorkDir: t.TempDir(),
		Env:     []string{"COPILOT_ALLOW_ALL=true", "COPILOT_ASSISTED_APPROVAL=true"},
	})
	for _, key := range []string{"COPILOT_ALLOW_ALL", "COPILOT_ASSISTED_APPROVAL"} {
		values := []string{}
		for _, e := range env {
			if k, v, ok := strings.Cut(e, "="); ok && k == key {
				values = append(values, v)
			}
		}
		if len(values) != 1 {
			t.Errorf("%s appears %d times (%v), want exactly one: a duplicate leaves the inherited value in force", key, len(values), values)
			continue
		}
		if values[0] != "false" {
			t.Errorf("%s = %q, want false", key, values[0])
		}
	}
}

// copilotStub installs an executable named `copilot` first on PATH.
func copilotStub(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "copilot"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The real Invoke path, end to end through a stub binary: the argv the adapter
// builds reaches the child, and the captured stream comes back parsed.
func TestCopilotInvokeParsesTheStreamAndPassesThePrompt(t *testing.T) {
	dir := t.TempDir()
	stream := filepath.Join(dir, "stream.jsonl")
	if err := os.WriteFile(stream, []byte(copilotSmokeStream(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	argv := filepath.Join(dir, "argv.txt")
	t.Setenv("COPILOT_STUB_STREAM", stream)
	t.Setenv("COPILOT_STUB_ARGV", argv)
	copilotStub(t, "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$COPILOT_STUB_ARGV\"\ncat \"$COPILOT_STUB_STREAM\"\n")

	res, err := (copilot{}).Invoke(context.Background(), Invocation{
		Prompt:        "Work the backlog.",
		WorkDir:       dir,
		Console:       io.Discard,
		MCPConfigPath: "/work/.mcp.json",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if want := 15811 + 29824 + 65; res.Tokens != want {
		t.Errorf("Tokens = %d, want %d", res.Tokens, want)
	}
	if !strings.Contains(res.FinalMessage, "backlog=169") {
		t.Errorf("FinalMessage = %q", res.FinalMessage)
	}
	got, err := os.ReadFile(argv)
	if err != nil {
		t.Fatalf("stub wrote no argv: %v", err)
	}
	for _, want := range []string{"-p\nWork the backlog.", "--additional-mcp-config\n@/work/.mcp.json", "--output-format\njson"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("argv %q does not contain %q", got, want)
		}
	}
}

// A non-zero exit is still a parsed Result, and its documented exhaustion text
// classifies as a Stop the driver can act on.
func TestCopilotInvokeReturnsALimitFromANonZeroExit(t *testing.T) {
	copilotStub(t, "#!/bin/sh\n"+
		"echo '{\"type\":\"session.error\",\"data\":{\"errorType\":\"query\",\"message\":\"Plan: 100% used, resets 2026-12-01\"}}'\n"+
		"exit 1\n")

	res, err := (copilot{}).Invoke(context.Background(), Invocation{Prompt: "work", Console: io.Discard})
	if err != nil {
		t.Fatalf("Invoke returned an error for an ordinary non-zero exit: %v", err)
	}
	if res.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", res.ExitCode)
	}
	lim := (copilot{}).DetectLimit(res)
	if !lim.Limited || !lim.Stop {
		t.Errorf("DetectLimit = %+v, want Limited and Stop", lim)
	}
}

// The cap has to END the session, not hang on it.
func TestCopilotInvokeEndsASessionThatOutlivesItsWallClockCap(t *testing.T) {
	copilotStub(t, "#!/bin/sh\nexec sleep 30\n")
	start := time.Now()
	res, err := (copilot{}).Invoke(context.Background(), Invocation{
		Prompt:              "work",
		Console:             io.Discard,
		MaxSessionWallClock: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Invoke returned an error for our own kill: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Invoke took %s; the wall-clock cap did not end the session", elapsed)
	}
	if !(copilot{}).WallClockCapped(res) {
		t.Errorf("WallClockCapped = false, want the cap marker (%+v, raw %v)", res, res.Raw)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 for a killed session", res.ExitCode)
	}
}

// A caller's cancellation is not the phase reaching its backstop.
func TestCopilotCallerCancellationIsNotAWallClockCap(t *testing.T) {
	copilotStub(t, "#!/bin/sh\nexec sleep 30\n")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	res, err := (copilot{}).Invoke(ctx, Invocation{
		Prompt:  "work",
		Console: io.Discard,
		// No MaxSessionWallClock: the only deadline is the caller's.
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if (copilot{}).WallClockCapped(res) {
		t.Error("a caller-cancelled session was reported as wall-clock capped")
	}
}

// The live smoke: ONE real headless Copilot session, read-only, with the
// adapter's fail-closed probe posture, reaching clankerbar over the real
// /mcp. Opt-in because it is a paid call, and it needs the operator's auth
// (the machine's stored Copilot credential) plus a real .mcp.json:
//
//	CLANKERBAR_COPILOT_LIVE=1 CLANKERBAR_COPILOT_MCP=$PWD/.mcp.json \
//	  go test ./internal/harness -run TestCopilotLiveSmoke -v
//
// What it pins that no fixture can: the custom-MCP config really loads from
// --additional-mcp-config, the `${CLANKERBAR_API_KEY}` header expands, a
// clankerbar tool call is observed through toolRequests/execution_complete, and
// the probe writes nothing.
func TestCopilotLiveSmoke(t *testing.T) {
	if os.Getenv("CLANKERBAR_COPILOT_LIVE") == "" {
		t.Skip("set CLANKERBAR_COPILOT_LIVE=1 and CLANKERBAR_COPILOT_MCP=<.mcp.json> to run the paid live smoke")
	}
	mcp := os.Getenv("CLANKERBAR_COPILOT_MCP")
	if mcp == "" {
		t.Skip("CLANKERBAR_COPILOT_MCP must name the .mcp.json to hand the session")
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	res, err := (copilot{}).Invoke(ctx, Invocation{
		Probe:         true,
		WorkDir:       dir,
		MCPConfigPath: mcp,
		Console:       io.Discard,
	})
	if err != nil {
		t.Fatalf("live probe: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("live probe exited %d: stdout=%s stderr=%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if res.ClankerbarCalls < 1 {
		t.Errorf("ClankerbarCalls = %d, want at least 1: the read-only get_backlog_summary never succeeded (stderr %q)", res.ClankerbarCalls, res.Stderr)
	}
	mcpStatus, _ := res.Raw["mcp_servers"].(map[string]string)
	if mcpStatus["clankerbar"] != "connected" {
		t.Errorf("Raw[mcp_servers][clankerbar] = %q, want connected (%v)", mcpStatus["clankerbar"], mcpStatus)
	}
	if lim := (copilot{}).DetectLimit(res); lim.Limited {
		t.Errorf("DetectLimit = %+v on a healthy session", lim)
	}
	if !res.UsageReported || res.Tokens == 0 {
		t.Errorf("usage not reported: reported=%v tokens=%d", res.UsageReported, res.Tokens)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the read-only probe wrote %d entries into the workdir: %v", len(entries), entries)
	}
	t.Logf("live probe: tokens=%d credits=%.3f final=%q", res.Tokens, res.Raw["ai_credits"], snippetText(res.FinalMessage))
}

func snippetText(s string) string {
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}
