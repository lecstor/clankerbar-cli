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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// secondStallLine is a SECOND consecutive stall with its OWN reasoning count,
// deliberately different from the first's 32000: the merged Result must carry
// THIS step's figure, not the sum of both stalls' thinking (the numeric Raw
// keys are summed by mergeResume; the stall's count is recomputed instead).
const secondStallLine = `{"type":"step_finish","timestamp":1790224970000,"sessionID":"ses_stall","part":{"id":"prt_0d1b916c8002TZLptSg14j5eR2","reason":"length","messageID":"msg_0d1b5eb49002ocbLFtYO6ZW8GZ","sessionID":"ses_stall","type":"step-finish","tokens":{"total":290112,"input":5290,"output":0,"reasoning":28000,"cache":{"write":0,"read":261000}},"cost":0.019}}`

// A second consecutive stall ends the session: no third run, no fresh session,
// and the merged Result still carries the marker — with the SECOND step's own
// reasoning count — so the daemon can name it.
func TestOutputCapStall_SecondConsecutiveStopEndsWithoutFurtherResume(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log")
	opencodeStub(t, `#!/bin/sh
echo "$*" >> "`+logPath+`"
case "$*" in
  *"hit the output token limit"*)
    echo '`+stallClaimEvent+`'
    echo '`+secondStallLine+`'
    ;;
  *)
    echo '`+stallClaimEvent+`'
    echo '`+outputCapStallLine+`'
    ;;
esac
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
	if got := res.Raw[OutputCapReasoningKey]; got != 28000 {
		t.Errorf("reasoning carried = %v, want the SECOND stall's own 28000 — the driver's named line must not report the two steps' counts summed (%d+%d=%d)",
			got, 32000, 28000, 32000+28000)
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

// The stall pair — reason and output — must describe ONE step_finish, and a
// reason-less event is not a step either figure may be read from: deadscan
// tracks its own lastReason/lastOut that way, and the adapter must agree with
// the retrospective scan on the same log or the two report different sessions.
// Both directions of the rule are pinned here and mirrored in deadscan_test.go.
func TestOutputCapStall_ReasonlessFinalStepNeverResumes(t *testing.T) {
	// A productive length step, then a reason-less step reporting zero output:
	// the previous step's "length" must NOT be paired with this event's zero.
	reasonless := `{"type":"step_finish","sessionID":"ses_stall","part":{"type":"step-finish","tokens":{"total":700,"input":100,"output":0,"reasoning":5,"cache":{"write":0,"read":0}},"cost":0.01}}`
	res := opencodeParsed(`{"type":"step_finish","sessionID":"ses_stall","part":{"type":"step-finish","reason":"length","tokens":{"total":500,"input":100,"output":3000,"reasoning":10,"cache":{"write":0,"read":0}},"cost":0.01}}` + "\n" + reasonless)

	if (opencode{}).OutputCapNoOutput(res) {
		t.Error("a reason-less final step must not lend its zero output to the previous step's \"length\"")
	}
	if got, ok := res.Raw[TerminalReasonKey]; ok {
		t.Errorf("no terminal_reason is warranted; got %v", got)
	}

	// The mirror: the last REASONED step is the stall, and a reason-less step
	// after it changes nothing — the pair still names one step.
	mirror := opencodeParsed(`{"type":"step_finish","sessionID":"ses_stall","part":{"type":"step-finish","reason":"length","tokens":{"total":500,"input":100,"output":0,"reasoning":900,"cache":{"write":0,"read":0}},"cost":0.01}}` + "\n" +
		`{"type":"step_finish","sessionID":"ses_stall","part":{"type":"step-finish","tokens":{"total":1200,"input":100,"output":500,"reasoning":5,"cache":{"write":0,"read":0}},"cost":0.01}}`)
	if !(opencode{}).OutputCapNoOutput(mirror) {
		t.Error("a reason-less step after a length/zero step must not un-stall the step that hit the cap")
	}
	if got := mirror.Raw[OutputCapReasoningKey]; got != 900 {
		t.Errorf("reasoning = %v, want 900 — the STALLED step's own count, not the reason-less step's", got)
	}
}

// failWriter is a console whose writes always fail: os/exec's copy of the
// session's stdout then fails after the bytes have already reached the line
// sink, which is the one way to reach runSession's error arm WITH events
// observed. It also records what was written, so a test can see which arm the
// resume took.
type failWriter struct{ notes bytes.Buffer }

func (w *failWriter) Write(p []byte) (int, error) {
	w.notes.Write(p)
	return 0, errors.New("console gone")
}

// An errored resume still folds the turn's claim observation: the resumed turn
// may have recorded a branch before the failure, and a driver acting on the
// stale pre-resume claim would hand the task back over work it just pushed.
func TestOutputCapStall_ErroredResumeKeepsTheClaimObservation(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log")
	opencodeStub(t, `#!/bin/sh
echo "$*" >> "`+logPath+`"
echo '{"type":"tool_use","sessionID":"ses_stall","part":{"type":"tool","tool":"clankerbar_update_task","callID":"c1","state":{"status":"completed","input":{"taskId":"uuid-1","branch":"clanker/x"},"output":"{\"ok\":true}"}}}'
`)

	rec := &failWriter{}
	res := opencodeParsed(stallClaimEvent + "\n" + outputCapStallLine)
	(opencode{}).resumeOutputCapStall(context.Background(), Invocation{Prompt: "Work.", Console: rec}, &res, time.Time{})

	// Precondition: the resume must really have taken the ERROR arm, or this
	// test would pass through mergeResume without exercising the fold at all.
	if !strings.Contains(rec.notes.String(), "stall resume errored") {
		t.Fatalf("the resume did not error as the fixture requires; console got:\n%s", rec.notes.String())
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("stub log: %v", err)
	}
	if n := len(strings.Split(strings.TrimSpace(string(data)), "\n")); n != 1 {
		t.Fatalf("stub ran %d times, want the single resume attempt:\n%s", n, data)
	}
	if !res.Claim.HasWIP {
		t.Error("the errored resume's update_task observation was dropped — the driver would release the task over work the resume pushed")
	}
	if !(opencode{}).OutputCapNoOutput(res) {
		t.Error("the stall marker must survive an errored resume — the stall still stands")
	}
}

// A cancelled run does not spend on a resume that would be killed on arrival,
// and does not report an error for it: the phase is being torn down, not
// stalling. Nothing is spawned and nothing is said.
func TestOutputCapStall_CancelledRunDoesNotResume(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log")
	opencodeStub(t, `#!/bin/sh
echo "$*" >> "`+logPath+`"
echo '`+outputCapStallLine+`'
`)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	console := &bytes.Buffer{}
	res := opencodeParsed(stallClaimEvent + "\n" + outputCapStallLine)
	(opencode{}).resumeOutputCapStall(ctx, Invocation{Prompt: "Work.", Console: console}, &res, time.Time{})

	if data, err := os.ReadFile(logPath); err == nil && strings.TrimSpace(string(data)) != "" {
		t.Errorf("a cancelled run must not spawn a resume; the stub ran:\n%s", data)
	}
	if out := console.String(); out != "" {
		t.Errorf("a cancelled run must say nothing — no announcement, no errored resume; console got:\n%s", out)
	}
}

// A session that stalled before it ever claimed is resumed too: unlike CLA-406's
// probe, this resume has no coherence to verify and asks nothing of the session,
// and the stalled step emitted nothing, so the transcript it re-sends is tiny.
// The divergence from the probe path's "no claim, no resume" is deliberate, and
// this pins it so a later reader does not "fix" it by accident.
func TestOutputCapStall_ResumesBeforeAClaimToo(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log")
	opencodeStub(t, `#!/bin/sh
echo "$*" >> "`+logPath+`"
case "$*" in
  *"hit the output token limit"*)
    echo '{"type":"step_finish","sessionID":"ses_stall","part":{"type":"step-finish","reason":"stop","tokens":{"total":900,"input":800,"output":100,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.001}}'
    ;;
  *)
    echo '`+outputCapStallLine+`'
    ;;
esac
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
		t.Fatalf("stub ran %d times, want a claim-less stall resumed once:\n%s", len(runs), data)
	}
	if !strings.Contains(runs[1], "hit the output token limit") {
		t.Errorf("the resume did not carry the steer: %q", runs[1])
	}
	if (opencode{}).OutputCapNoOutput(res) {
		t.Error("a recovered stall must not keep the marker")
	}
}
