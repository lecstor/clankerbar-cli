package harness

import (
	"encoding/json"
	"strings"
	"testing"
)

// Text that arrives FROM THE BACKLOG must never be readable as a usage limit or
// as a transient failure (CLA-258).
//
// A session's stdout is the whole event stream, and the events carry the verbatim
// MCP responses to `claim_task` / `next_task` — so the task the session is working
// on is quoted, in full, inside the same bytes the limit scan reads. A bug report
// whose body says `hit your` or `api error: 500` (this task's own body says both)
// used to make the driver report a cap that never happened: it slept, re-spawned
// the same paid session, re-claimed the same task, and did it again, with every
// budget ceiling sitting inert inside the drain.
//
// So the classifiers read only what the HARNESS said — stderr, its own non-event
// output, typed error events, `terminal_reason` — never the agent's narration.

// poison is a task body of exactly the shape that used to trip every scanner: the
// three strings the bar names, plus a verbatim reset line for good measure.
const poison = "You've hit your session limit · resets 9:40pm (Europe/Madrid) — " +
	"a task body containing `hit your`, `usage limit` or `weekly limit` fakes a cap, " +
	"and `api error: 500` fakes a transient failure. Too many requests, out of credits. " +
	// CLA-268 widened the transient arm to catch the CLI's documented
	// mid-response notices, and its own task body quotes the wording verbatim —
	// so the widening arrived with a fresh way to fake a blip from the backlog.
	// It is only safe because the scope excludes narration, which is precisely
	// what this fixture exists to keep true.
	"API Error: Connection closed mid-response. The response above may be incomplete."

// claudePoisonedStream is a realistic `--output-format stream-json` session that
// claimed the poisoned task, quoted it back, and then finished cleanly.
func claudePoisonedStream(t *testing.T) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"task": map[string]any{"ref": "CLA-258", "detail": poison}})
	if err != nil {
		t.Fatalf("marshal task: %v", err)
	}
	// The MCP response reaches the stream as a STRING inside the tool_result block.
	toolResult, err := json.Marshal(string(body))
	if err != nil {
		t.Fatalf("marshal tool_result: %v", err)
	}
	narration, err := json.Marshal("Reading CLA-258: " + poison)
	if err != nil {
		t.Fatalf("marshal narration: %v", err)
	}
	return strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__clankerbar__claim_task","id":"tu1","input":{}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":` + string(toolResult) + `}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":` + string(narration) + `}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":` + string(narration) +
			`,"total_cost_usd":0.42,"usage":{"input_tokens":10,"output_tokens":5}}`,
	}, "\n") + "\n"
}

func TestClaudeDoesNotReadTheBacklogAsALimit(t *testing.T) {
	res := Result{
		ExitCode: 0,
		Stdout:   claudePoisonedStream(t),
		Raw:      map[string]any{"terminal_reason": ""},
	}

	if lim := (claude{}).DetectLimit(res); lim.Limited {
		t.Errorf("a task body quoted back in the stream was read as a usage limit (reason %q, reset %v)", lim.Reason, lim.ResetAt)
	}
	if (claude{}).IsTransient(res) {
		t.Error("a task body quoted back in the stream was read as a transient failure")
	}
	assertDiagnosticIsClean(t, "claude", (claude{}).Diagnostic(res))
}

// The mid-response arm (CLA-268) against a task body carrying ONLY that wording,
// with every other trigger removed — the shared `poison` above also contains
// `api error: 500`, which trips the first arm and would mask what this is about.
//
// What this pins is the SCOPE, not the anchoring: the wording reaches the stream
// through a tool_result, an assistant turn, and a clean `result` event, and
// claudeText excludes all three, so the regex is never handed the text at all.
// That is the first line of defence and it holds whatever the arm looks like.
// (The anchoring is pinned separately, by the `prose mentions mid-response
// without the prefix` case in TestClaudeIsTransient, which feeds the string
// through a channel the scope DOES read. Verified: making the arm unanchored
// reddens that case and leaves this one green.)
//
// It matters because CLA-268's own body quotes the notice verbatim, so the very
// task that widened the classifier is a live specimen of the text that must not
// fake a blip from the backlog — and, as the Diagnostic check below covers, must
// not be printed at the operator either.
func TestClaudeDoesNotReadAQuotedMidResponseNoticeAsTransient(t *testing.T) {
	const onlyMidResponse = "CLA-268: the classifier misses `API Error: Connection closed mid-response. " +
		"The response above may be incomplete.` and a miss stops the daemon."

	body, err := json.Marshal(map[string]any{"task": map[string]any{"ref": "CLA-268", "detail": onlyMidResponse}})
	if err != nil {
		t.Fatalf("marshal task: %v", err)
	}
	toolResult, err := json.Marshal(string(body))
	if err != nil {
		t.Fatalf("marshal tool_result: %v", err)
	}
	narration, err := json.Marshal("Reading CLA-268: " + onlyMidResponse)
	if err != nil {
		t.Fatalf("marshal narration: %v", err)
	}
	res := Result{ExitCode: 0, Stdout: strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__clankerbar__claim_task","id":"tu1","input":{}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":` + string(toolResult) + `}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":` + string(narration) + `}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":` + string(narration) + `}`,
	}, "\n") + "\n"}

	if (claude{}).IsTransient(res) {
		t.Error("a task body quoting the mid-response notice was read as a transient failure — the new arm is reachable from the backlog")
	}
	assertDiagnosticIsClean(t, "claude", (claude{}).Diagnostic(res))
}

// The scoping must not cost the real signals. Each of these is the HARNESS
// speaking, in one of the three places it speaks.
func TestClaudeStillSeesTheHarnessOwnFailures(t *testing.T) {
	limitCases := []struct {
		name string
		res  Result
	}{
		{"terminal_reason", Result{Raw: map[string]any{"terminal_reason": "usage_limit"}}},
		{"stderr", Result{Stderr: "You've hit your session limit · resets 9:40pm (Europe/Madrid)"}},
		{"plain stdout, no stream at all", Result{Stdout: "You've hit your weekly limit · resets Sunday 12:00am"}},
		{"an ERROR result event", Result{Stdout: `{"type":"result","subtype":"error_during_execution","is_error":true,` +
			`"result":"You've hit your session limit · resets 9:40pm"}`}},
	}
	for _, tc := range limitCases {
		t.Run("limit/"+tc.name, func(t *testing.T) {
			if !(claude{}).DetectLimit(tc.res).Limited {
				t.Error("a real usage limit went unrecognised")
			}
		})
	}

	transientCases := []struct {
		name string
		res  Result
	}{
		{"stderr", Result{Stderr: "API Error: 500 Internal Server Error"}},
		{"plain stdout", Result{Stdout: "API Error: 529 overloaded_error"}},
		{"an ERROR result event", Result{Stdout: `{"type":"result","subtype":"error_during_execution","is_error":true,` +
			`"result":"API Error: 503 Service Unavailable"}`}},
	}
	for _, tc := range transientCases {
		t.Run("transient/"+tc.name, func(t *testing.T) {
			if !(claude{}).IsTransient(tc.res) {
				t.Error("a real transient failure went unrecognised")
			}
		})
	}
}

// A reset time is read only from what the CLI itself wrote — never from free
// text, even the free text of a session it says FAILED.
//
// This is not just about how long the loop naps. waitPastBudget abandons the run
// outright when a reset lands past the wall-clock ceiling, so a reset lifted out of
// a `result` string would let a task body containing "resets Sunday 12:00am" end
// somebody's overnight run.
func TestClaudeResetTimeIsNotTakenFromFreeText(t *testing.T) {
	quoted, err := json.Marshal("The task body says: " + poison)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// A self-consistent stream: the CLI says the session hit the cap, and the
	// result text is where the quoted task body ended up.
	stream := `{"type":"result","subtype":"error_during_execution","is_error":true,` +
		`"terminal_reason":"usage_limit","result":` + string(quoted) + "}\n"
	res := Result{Stdout: stream, Raw: map[string]any{"terminal_reason": "usage_limit"}}

	lim := (claude{}).DetectLimit(res)
	if !lim.Limited {
		t.Fatal("terminal_reason: usage_limit is the CLI's own word and must still be believed")
	}
	if !lim.ResetAt.IsZero() {
		t.Errorf("the reset time was lifted out of free text: %v", lim.ResetAt)
	}

	// The same limit, stated where the CLI actually states it, keeps its reset.
	onStderr := Result{Stderr: "You've hit your session limit · resets 9:40pm (Europe/Madrid)"}
	if (claude{}).DetectLimit(onStderr).ResetAt.IsZero() {
		t.Error("a reset the CLI stated on stderr must still be parsed")
	}
}

// The probe path is `--output-format json` — one object, with no per-line
// contract. A pretty-printed object must not be walked line by line, or every
// line after the opening brace reads as CLI text and the agent's own `result`
// lands back in the scan. A probe that misreads its answer keeps the supervised
// wait from ever ending.
func TestClaudeProbeShapeIsParsedWhole(t *testing.T) {
	body, err := json.MarshalIndent(map[string]any{
		"type": "result", "subtype": "success", "is_error": false,
		"result": "The task body says: " + poison,
	}, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	res := Result{Stdout: string(body) + "\n"}

	if lim := (claude{}).DetectLimit(res); lim.Limited {
		t.Error("a pretty-printed probe result was read as a usage limit")
	}
	if (claude{}).IsTransient(res) {
		t.Error("a pretty-printed probe result was read as a transient failure")
	}
}

// A typed error event that is not the terminal `result` must still be read — the
// parity codex and opencode already have. Dropping it would turn a retryable blip
// the CLI announced this way into "non-retryable, stopping".
func TestClaudeReadsTypedErrorEvents(t *testing.T) {
	res := Result{Stdout: `{"type":"error","error":{"message":"API Error: 503 Service Unavailable"}}` + "\n"}
	if !(claude{}).IsTransient(res) {
		t.Error("a typed error event went unread")
	}
}

// codexPoisonedStream is a realistic `codex exec --json` session that quoted the
// poisoned task back and finished cleanly.
func codexPoisonedStream(t *testing.T) string {
	t.Helper()
	quoted, err := json.Marshal("Reading CLA-258: " + poison)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return strings.Join([]string{
		`{"type":"item.completed","item":{"type":"function_call_output","text":` + string(quoted) + `}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":` + string(quoted) + `}}`,
		`{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":5}}`,
	}, "\n") + "\n"
}

func TestCodexDoesNotReadTheBacklogAsALimit(t *testing.T) {
	res := Result{ExitCode: 0, Stdout: codexPoisonedStream(t)}

	if (codex{}).DetectLimit(res).Limited {
		t.Error("a task body quoted back in the stream was read as a usage limit")
	}
	if (codex{}).IsTransient(res) {
		t.Error("a task body quoted back in the stream was read as a transient failure")
	}
	assertDiagnosticIsClean(t, "codex", (codex{}).Diagnostic(res))
}

func TestCodexStillSeesTheHarnessOwnFailures(t *testing.T) {
	if !(codex{}).DetectLimit(Result{Stderr: "You've hit your usage limit, try again in 4 hours"}).Limited {
		t.Error("a real usage limit on stderr went unrecognised")
	}
	if !(codex{}).IsTransient(Result{Stdout: `{"type":"turn.failed","error":{"statusCode":429}}`}) {
		t.Error("a real transient failure in a typed error event went unrecognised")
	}
}

// opencode already scoped both scans (opencodeErrorText); this pins it against
// the same payload, so the property is stated once for all three adapters.
func TestOpencodeDoesNotReadTheBacklogAsALimit(t *testing.T) {
	quoted, err := json.Marshal("Reading CLA-258: " + poison)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	res := Result{ExitCode: 0, Stdout: strings.Join([]string{
		`{"type":"text","part":{"type":"text","text":` + string(quoted) + `}}`,
		`{"type":"step_finish","part":{"type":"step-finish","reason":"stop","tokens":{"total":10},"cost":0.01}}`,
	}, "\n") + "\n"}

	if (opencode{}).DetectLimit(res).Limited {
		t.Error("a task body quoted back in the stream was read as a budget stop")
	}
	if (opencode{}).IsTransient(res) {
		t.Error("a task body quoted back in the stream was read as a transient failure")
	}
	assertDiagnosticIsClean(t, "opencode", (opencode{}).Diagnostic(res))
}

// assertDiagnosticIsClean holds the half of CLA-268 that the classifiers alone
// do not: Diagnostic's text is RENDERED to the operator's terminal when a run
// stops, so a scope wider than IsTransient's does not merely misclassify — it
// prints a quoted task body at them. Every adapter must pass it.
//
// This is the guard that a `return res.Stdout + res.Stderr` implementation
// fails. Nothing else in the suite would: such a change compiles, vets clean,
// and leaves every classifier verdict unchanged.
func assertDiagnosticIsClean(t *testing.T, name, diag string) {
	t.Helper()
	for _, leaked := range []string{"hit your", "api error: 500", "mid-response"} {
		if strings.Contains(strings.ToLower(diag), leaked) {
			t.Errorf("%s.Diagnostic exposed a quoted task body (found %q): %q", name, leaked, diag)
		}
	}
}
