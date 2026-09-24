package harness

// CLA-584: the output-cap stall — its detection, its marker, and the single
// steer resume. The fixture line is MAK-123's final step, taken from the
// recorded iteration log verbatim: reason "length", output 0, reasoning 32000,
// real cost. It is deliberately NOT all-zero usage — the stall is found by the
// output figure alone, and an all-zero fixture would let a wrong detector
// (reusing the CLA-398 test) pass.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// outputCapStallLine is the final step_finish of MAK-123 (2026-09-24): the
// model spent the whole step's output budget reasoning and emitted nothing.
const outputCapStallLine = `{"type":"step_finish","timestamp":1790224963298,"sessionID":"ses_stall","part":{"id":"prt_0d1b916c8001TZLptSg14j5eR1","reason":"length","messageID":"msg_0d1b5eb49001ocbLFtYO6ZW8GZ","sessionID":"ses_stall","type":"step-finish","tokens":{"total":306592,"input":5280,"output":0,"reasoning":32000,"cache":{"write":0,"read":269312}},"cost":0.020799936}}`

// stallClaimEvent is the session getting past its claim before it stalls, so
// the resume's claim seed has something real to observe.
const stallClaimEvent = `{"type":"tool_use","sessionID":"ses_stall","part":{"type":"tool","tool":"clankerbar_claim_task","callID":"c0","state":{"status":"completed","input":{"taskId":"uuid-1"},"output":"{\"task\":{\"id\":\"uuid-1\",\"ref\":\"CLA-584\"},\"run\":{\"id\":\"run-1\"}}"}}}`

// The recorded signature classifies, and lands the two figures the daemon's
// named log line reads: the terminal marker and the step's own reasoning count.
func TestOutputCapStall_Detection(t *testing.T) {
	res := opencodeParsed(stallClaimEvent + "\n" + outputCapStallLine)

	if !(opencode{}).OutputCapNoOutput(res) {
		t.Fatal("the MAK-123 final step (length, output 0) must classify as the output-cap stall")
	}
	if got := res.Raw[TerminalReasonKey]; got != OutputCapReason {
		t.Errorf("terminal_reason = %v, want %q", got, OutputCapReason)
	}
	if got := res.Raw[FinishReasonKey]; got != FinishReasonLength {
		t.Errorf("finish_reason = %v, want %q", got, FinishReasonLength)
	}
	if got := res.Raw[OutputCapReasoningKey]; got != 32000 {
		t.Errorf("reasoning carried = %v, want 32000 — the step's OWN count is what the named log line reports", got)
	}
	// The two signatures must stay apart: the stall reported reasoning AND cost.
	if (opencode{}).ZeroUsageUnknown(res) {
		t.Error("the stall reported reasoning and cost, so it must not read as the all-zero quiet death")
	}
}

// A length stop that DID produce output is not the stall — the model answered,
// it just ran long, and resuming would spend a turn re-doing work that landed.
func TestOutputCapStall_LengthWithOutputIsNotAStall(t *testing.T) {
	res := opencodeParsed(`{"type":"step_finish","sessionID":"ses_stall","part":{"type":"step-finish","reason":"length","tokens":{"total":500,"input":100,"output":42,"reasoning":10,"cache":{"write":0,"read":0}},"cost":0.01}}`)

	if (opencode{}).OutputCapNoOutput(res) {
		t.Error("a length stop that produced output must not classify as the stall")
	}
	if got, ok := res.Raw[TerminalReasonKey]; ok {
		t.Errorf("no terminal_reason for a productive length stop; got %v", got)
	}
}

// The CLA-398 quiet death is a different end and must not be stolen by this one:
// its reason is "unknown", not "length".
func TestOutputCapStall_QuietDeathIsNotAStall(t *testing.T) {
	res := opencodeParsed(quietDeathWithSession)

	if (opencode{}).OutputCapNoOutput(res) {
		t.Error("an unknown/zero-usage death must stay a quiet death, not the output-cap stall")
	}
	if !(opencode{}).ZeroUsageUnknown(res) {
		t.Error("test precondition: the fixture must still be the quiet-death signature")
	}
}

// A stall resumes the SAME session exactly once, carrying the steer, and the
// merged result is the recovered single session.
func TestOutputCapStall_InvokeResumesOnceWithTheSteer(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log")
	console := &bytes.Buffer{}
	opencodeStub(t, `#!/bin/sh
echo "$*" >> "`+logPath+`"
case "$*" in
  *"hit the output token limit"*)
    echo '{"type":"tool_use","sessionID":"ses_stall","part":{"type":"tool","tool":"clankerbar_update_task","callID":"c1","state":{"status":"completed","input":{"taskId":"uuid-1","branch":"clanker/x"},"output":"{\"ok\":true}"}}}'
    echo '{"type":"step_finish","sessionID":"ses_stall","part":{"type":"step-finish","reason":"stop","tokens":{"total":900,"input":800,"output":100,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.001}}'
    ;;
  *)
    echo '`+stallClaimEvent+`'
    echo '`+outputCapStallLine+`'
    ;;
esac
`)

	res, err := (opencode{}).Invoke(context.Background(), Invocation{Prompt: "Work.", Console: console})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("stub log: %v", err)
	}
	runs := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(runs) != 2 {
		t.Fatalf("stub ran %d times, want exactly 2 (the stalled session + ONE steer resume):\n%s", len(runs), data)
	}
	if !strings.Contains(runs[1], "-s ses_stall") {
		t.Errorf("the resume did not target the existing session (`-s ses_stall`): %q", runs[1])
	}
	if !strings.Contains(runs[1], "hit the output token limit") {
		t.Errorf("the resume did not carry the steer: %q", runs[1])
	}

	if (opencode{}).OutputCapNoOutput(res) {
		t.Error("a recovered stall must not keep the stall marker")
	}
	if got := res.Raw[FinishReasonKey]; got != "stop" {
		t.Errorf("finish_reason = %v, want the resume's \"stop\" — a recovered result must not look stalled", got)
	}
	// The continuation's update_task must survive into the merged Claim (the
	// ResumeClaim seed), exactly as CLA-406's arm needs.
	if !res.Claim.HasWIP {
		t.Error("Claim.HasWIP = false; the resumed turn's branch recording was not observed — ResumeClaim seed missing")
	}
	// Spend sums across both runs: the stalled step's total plus the resume's.
	if res.Tokens != 306592+900 {
		t.Errorf("Tokens = %d, want %d summed across the stall and its resume", res.Tokens, 306592+900)
	}
	if !strings.Contains(console.String(), "output-cap stall detected") {
		t.Errorf("console did not announce the stall; got:\n%s", console.String())
	}
}

// A second consecutive stall ends the session: no third run, no fresh session,
// and the merged Result still carries the marker so the daemon can name it.
func TestOutputCapStall_SecondConsecutiveStopEndsWithoutFurtherResume(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log")
	opencodeStub(t, `#!/bin/sh
echo "$*" >> "`+logPath+`"
echo '`+stallClaimEvent+`'
echo '`+outputCapStallLine+`'
`)

	res, err := (opencode{}).Invoke(context.Background(), Invocation{Prompt: "Work.", Console: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("stub log: %v", err)
	}
	runs := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(runs) != 2 {
		t.Fatalf("stub ran %d times, want exactly 2 — one resume, never a loop:\n%s", len(runs), data)
	}
	if !(opencode{}).OutputCapNoOutput(res) {
		t.Error("a second consecutive stall must leave the marker on the merged Result")
	}
	if got := res.Raw[FinishReasonKey]; got != FinishReasonLength {
		t.Errorf("finish_reason = %v, want the second stall's %q", got, FinishReasonLength)
	}
}

// The three ways a stall gets no resume: a productive length stop, a stream
// with no session id to resume into, and a probe. Each ends after the one run.
func TestOutputCapStall_NoResumeCases(t *testing.T) {
	noSession := `{"type":"step_finish","part":{"type":"step-finish","reason":"length","tokens":{"total":500,"input":100,"output":0,"reasoning":900,"cache":{"write":0,"read":0}},"cost":0.01}}`
	productive := `{"type":"step_finish","sessionID":"ses_stall","part":{"type":"step-finish","reason":"length","tokens":{"total":500,"input":100,"output":42,"reasoning":10,"cache":{"write":0,"read":0}},"cost":0.01}}`

	for _, tc := range []struct {
		name  string
		body  string
		probe bool
	}{
		{"length with output", productive, false},
		{"no session id", noSession, false},
		{"probe", outputCapStallLine, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			logPath := filepath.Join(dir, "log")
			opencodeStub(t, `#!/bin/sh
echo "$*" >> "`+logPath+`"
echo '`+tc.body+`'
`)

			if _, err := (opencode{}).Invoke(context.Background(), Invocation{Prompt: "Work.", Probe: tc.probe}); err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			data, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("stub log: %v", err)
			}
			if n := len(strings.Split(strings.TrimSpace(string(data)), "\n")); n != 1 {
				t.Errorf("stub ran %d times, want exactly 1 — no resume is possible here:\n%s", n, data)
			}
		})
	}
}
