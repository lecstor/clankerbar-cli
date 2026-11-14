package harness

import (
	"io"
	"os"
	"strings"
	"testing"
)

// copilotParsed runs a captured stdout stream through exactly the machinery
// Invoke uses: the lineSink that splits it into whole lines, and the incremental
// parser that reads them as they arrive.
func copilotParsed(stdout string) Result {
	return copilotParsedIn(Invocation{}, stdout)
}

func copilotParsedIn(in Invocation, stdout string) Result {
	res := newSessionResult(in)
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
	var res Result
	res = newSessionResult(Invocation{})
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
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
