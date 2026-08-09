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
	"and `api error: 500` fakes a transient failure. Too many requests, out of credits."

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

// A reset time is only trusted from the same scoped text. Reading it out of the
// narration would let backlog text choose how long the loop sleeps.
func TestClaudeResetTimeIsNotTakenFromNarration(t *testing.T) {
	res := Result{
		Stdout: claudePoisonedStream(t),
		Raw:    map[string]any{"terminal_reason": "usage_limit"},
	}
	lim := (claude{}).DetectLimit(res)
	if !lim.Limited {
		t.Fatal("terminal_reason: usage_limit is the harness's own word and must still be believed")
	}
	if !lim.ResetAt.IsZero() {
		t.Errorf("the reset time came from the agent's narration: %v", lim.ResetAt)
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
}
