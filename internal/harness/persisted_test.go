package harness

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A tool_result too large for Claude Code to inline is replaced by a POINTER to a
// file, and this is the shape that broke the phase seam (CLA-330).
//
// Why the existing suite could not fail on it: every case above builds its own
// tool_result out of `claimOK`, a payload of a couple of hundred bytes, so the
// spill threshold is never anywhere near. The live payload is not a couple of
// hundred bytes - `claim_task` carries the project's standing decisions in full,
// and at 103 of them it was 66KB. A green suite therefore proved nothing about the
// path the driver actually runs, which is the whole argument for having run it
// live at all.
//
// So these tests drive the VERBATIM envelope observed on 2026-08-11, from the
// transcript of the run that failed, rather than a reconstruction of it.

// persistedEnvelope is the tool_result text Claude Code substitutes for a spilled
// result, copied from the failing run's transcript with only the path changed.
// The trailing "..." and the truncated preview are part of it: the preview cuts
// off mid-payload, which is why parsing the preview is not a fix - `run.id` sits
// at the END of a claim payload and is never inside the first 2KB.
func persistedEnvelope(path string) string {
	return "<persisted-output>\n" +
		"Output too large (66.4KB). Full output saved to: " + path + "\n\n" +
		"Preview (first 2KB):\n" +
		"[\n  {\n    \"type\": \"text\",\n    \"text\": \"{\\\"task\\\":{\\\"id\\\":\\\"" + claimUUID +
		"\\\",\\\"number\\\":321,\\\"ref\\\":\\\"" + claimRef + "\\\",\\\"detail\\\":\\\"## Why\n" +
		"...\n" +
		"</persisted-output>"
}

// spillClaimPayload writes the file the harness spilled to, in the layout it uses:
// the original `content` array, verbatim, named after the tool call it belongs to.
func spillClaimPayload(t *testing.T, toolUseID string) string {
	t.Helper()
	body, err := json.Marshal([]map[string]string{{"type": "text", "text": claimOK}})
	if err != nil {
		t.Fatalf("marshal spilled content: %v", err)
	}
	path := filepath.Join(t.TempDir(), toolUseID+".json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write spilled content: %v", err)
	}
	return path
}

// persistedResult is the event the stream carries: content as a BARE STRING, not
// an array of blocks, which is how the envelope arrived in the failing run.
func persistedResult(id, text string) string {
	b, _ := json.Marshal(text)
	return `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"` + id +
		`","content":` + string(b) + `}]}}`
}

func TestClaudeClaimTracking_persistedToolResult(t *testing.T) {
	const id = "toolu_018GK3yeNBtbkE3gyiprL1Dj"
	path := spillClaimPayload(t, id)

	res := claimStream(
		toolUse(id, claimTaskTool, `{"taskId":"`+claimUUID+`"}`),
		persistedResult(id, persistedEnvelope(path)),
	)

	if want := heldClaim(); res.Claim != want {
		t.Fatalf("Claim = %+v, want %+v", res.Claim, want)
	}
	// The conjunct the seam actually gates on (loop.go: `res.Claim.Held()`). Stated
	// separately because this is the assertion whose failure ended the live drain,
	// and a future refactor could keep the fields and lose the property.
	if !res.Claim.Held() {
		t.Error("Held() = false: the phase seam reads exactly this, and false ends the drain")
	}
}

// The guard that keeps a remote string out of the driver's file reads. The path
// is honoured only when it is named after the tool call carrying it; a task body
// quoted back into the stream cannot name an id the harness minted for that call.
func TestClaudeClaimTracking_persistedPathMustBeNamedForItsToolCall(t *testing.T) {
	const id = "toolu_018GK3yeNBtbkE3gyiprL1Dj"
	// A real, readable, perfectly valid claim payload - at a path this tool call
	// has no business reading. Readability is the point: if the guard were absent
	// this file WOULD be parsed, so the test fails for the right reason.
	elsewhere := spillClaimPayload(t, "toolu_someOtherCall")

	for _, tt := range []struct {
		name string
		path string
	}{
		{"a file named for a DIFFERENT tool call", elsewhere},
		{"a relative path", "tool-results/" + id + ".json"},
		{"a directory that merely ends in the id", "/tmp/" + id + ".json/payload"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var console bytes.Buffer
			var res Result
			for _, l := range []string{
				toolUse(id, claimTaskTool, `{"taskId":"`+claimUUID+`"}`),
				persistedResult(id, persistedEnvelope(tt.path)),
			} {
				(claude{}).renderAndParse([]byte(l), &console, &res)
			}
			if res.Claim != (Claim{}) {
				t.Errorf("Claim = %+v, want zero: %s must not be read", res.Claim, tt.path)
			}
			// Refusing to read it must not put us back where CLA-330 started.
			if !strings.Contains(console.String(), "claim NOT tracked") {
				t.Errorf("no diagnostic; console = %q", console.String())
			}
		})
	}
}

// Both silent early exits now say what they dropped. The silence is the defect
// being fixed as much as the parse failure is: `Claim.Held()` is false on a zero
// Claim, so a claim that was never recorded looked exactly like a session that
// never claimed one - which is why localising CLA-330 cost a whole live run.
func TestClaudeClaimTracking_givingUpIsNeverSilent(t *testing.T) {
	for _, tt := range []struct {
		name   string
		result string
		want   string
	}{
		{
			name:   "content that is not JSON at all",
			result: toolResult("u1", "MCP error -32001: request timed out"),
			want:   "is not JSON",
		},
		{
			// What an unrecognised spill envelope degrades to. It is not silently
			// ignored: the operator sees the marker in the log and knows why.
			name:   "a spilled result whose path is not usable",
			result: persistedResult("u1", persistedEnvelope("./relative.json")),
			want:   "is not JSON",
		},
		{
			name:   "a claim that lost the race carries no ids",
			result: toolResult("u1", `{"error":{"code":"already_claimed"}}`),
			want:   "and run id",
		},
		{
			// The half-claim the driver must never act on: a task with no run.
			name:   "a payload carrying a task but no run",
			result: toolResult("u1", `{"task":{"id":"`+claimUUID+`"}}`),
			want:   "and run id",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var console bytes.Buffer
			var res Result
			for _, l := range []string{
				toolUse("u1", claimTaskTool, `{"taskId":"`+claimUUID+`"}`),
				tt.result,
			} {
				(claude{}).renderAndParse([]byte(l), &console, &res)
			}
			if res.Claim != (Claim{}) {
				t.Errorf("Claim = %+v, want zero", res.Claim)
			}
			if got := console.String(); !strings.Contains(got, tt.want) {
				t.Errorf("console = %q, want it to mention %q", got, tt.want)
			}
		})
	}
}

// A spilled result whose file has gone says so by name, rather than leaving the
// driver to conclude the session never claimed anything.
func TestClaudeClaimTracking_persistedFileMissing(t *testing.T) {
	const id = "toolu_018GK3yeNBtbkE3gyiprL1Dj"
	path := filepath.Join(t.TempDir(), id+".json")

	var console bytes.Buffer
	var res Result
	for _, l := range []string{
		toolUse(id, claimTaskTool, `{"taskId":"`+claimUUID+`"}`),
		persistedResult(id, persistedEnvelope(path)),
	} {
		(claude{}).renderAndParse([]byte(l), &console, &res)
	}
	if res.Claim != (Claim{}) {
		t.Errorf("Claim = %+v, want zero", res.Claim)
	}
	if got := console.String(); !strings.Contains(got, path) {
		t.Errorf("console = %q, want it to name the unreadable file", got)
	}
}

// snippet is what keeps a diagnostic from dumping the very payload that was too
// big to inline into a log nobody can scroll.
func TestSnippetIsBounded(t *testing.T) {
	got := snippet(strings.Repeat("detail ", 5000))
	if len(got) > 200 {
		t.Errorf("snippet len = %d, want it bounded", len(got))
	}
	if snippet("") != "(empty)" {
		t.Errorf("snippet(\"\") = %q, want an explicit marker", snippet(""))
	}
	if got := snippet("a\n\tb"); got != "a b" {
		t.Errorf("snippet = %q, want newlines collapsed to keep it one log line", got)
	}
}
