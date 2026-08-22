package harness

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// CLA-299: a run whose Wait failed with something other than an exit status must
// still return whatever the session announced, fully parsed, alongside the error
// — a session that spent and died in Wait reports its spend, its final message
// and any claim on the stream, instead of raw Stdout/Stderr and zeroes. These
// tests pin that ordering per adapter, plus the negative case the plan called
// out: a Start-shaped failure reaches the parse with EMPTY output, which must
// yield an honest zero Result rather than a panic.

// failAfterWriter succeeds on its first n Writes and then fails — the stand-in
// for a console tee dying mid-session (a full disk under the iteration log).
type failAfterWriter struct {
	n     int
	calls int
}

func (f *failAfterWriter) Write(p []byte) (int, error) {
	f.calls++
	if f.calls > f.n {
		return 0, errors.New("console write failed (simulated ENOSPC)")
	}
	return len(p), nil
}

// stripPath points PATH at an empty directory, so every exec.Command lookup for
// a real binary fails at Start — the launch-failure shape, where nothing was
// ever emitted and a zero Result is the right answer.
func stripPath(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

// --- codex -----------------------------------------------------------------
//
// The console tee is caller-supplied and sits LAST in the capture's writer
// chain, so a failing console surfaces as os/exec's copy error: a non-exit Run
// failure whose stdout had already reached the tail AND the parser for every
// chunk written before the failure. That makes this the one non-exit shape
// reachable end to end under this adapter (it sets no WaitDelay, so the
// grandchild-holds-the-pipe shape blocks in Run rather than erroring).

func TestCodexInvokeReturnsAParsedResultAlongsideANonExitRunError(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "codex")
	script := `#!/bin/sh
echo '{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":10,"output_tokens":50,"reasoning_output_tokens":5}}'
echo '{"type":"agent_message","item":{"type":"message","text":"done"}}'
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("writing stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	res, err := (codex{}).Invoke(context.Background(), Invocation{
		Prompt: "work",
		// Fail on the FIRST console write: the copier reads whole chunks (both
		// lines typically arrive in one read, hence one Write call), and the
		// capture's tail+parser sit BEFORE the console in the chain, so the
		// failing chunk has already been parsed when the error surfaces.
		Console: &failAfterWriter{n: 0},
	})
	if err == nil {
		t.Fatal("Invoke returned no error for a failing console; the fixture did not produce a run failure")
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		t.Fatalf("run error was an *exec.ExitError (%v); the fixture must exercise the NON-exit branch", err)
	}
	if res.Tokens != 155 {
		t.Errorf("Tokens = %d, want 155 — a session that announced its spend before the failure must reach the budget accumulator", res.Tokens)
	}
	if res.FinalMessage != "done" {
		t.Errorf("FinalMessage = %q, want %q — the final message survives the failure alongside the spend", res.FinalMessage, "done")
	}
	if !res.UsageReported {
		t.Error("UsageReported = false — the stream DID report; silence here would feed the zero-spend bound")
	}
}

func TestCodexInvokeOnALaunchFailureParsesToAZeroResult(t *testing.T) {
	stripPath(t)

	res, err := (codex{}).Invoke(context.Background(), Invocation{Prompt: "work"})
	if err == nil {
		t.Fatal("Invoke succeeded without the binary on PATH; the negative case did not fire")
	}
	if res.Tokens != 0 || res.CostUSD != 0 || res.FinalMessage != "" || res.UsageReported {
		t.Errorf("launch failure produced %+v — with nothing emitted, parsing must yield an honest zero", res)
	}
	if res.Claim.Held() {
		t.Error("a launch failure must never carry a claim; releaseHeldClaim would post ready over a live lease")
	}
}

// --- opencode ---------------------------------------------------------------
//
// Any deadline context sets cmd.WaitDelay, so a session that exits CLEANLY but
// leaves a grandchild holding its output pipes past the delay comes back from
// Run as exec.ErrWaitDelay — a non-exit failure carrying a session that ran,
// emitted everything, and exited successfully. This is the ran-and-died-in-Wait
// shape of CLA-299, driven here through the real exec machinery (~5s: the hard
// coded delay).

func TestOpencodeInvokeReturnsAParsedResultAlongsideAWaitDelayDeath(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "opencode")
	// The claim line is the recorded 1.18.x terminal tool-part shape (see
	// opencode_resume_test.go); the grandchild inherits stdout, so the pipe stays
	// open past WaitDelay after the parent exits 0.
	script := `#!/bin/sh
echo '{"type":"tool_use","sessionID":"ses_dead","part":{"type":"tool","tool":"clankerbar_claim_task","callID":"c0","state":{"status":"completed","input":{"taskId":"uuid-1"},"output":"{\"task\":{\"id\":\"uuid-1\",\"ref\":\"EZY-196\"},\"run\":{\"id\":\"run-1\"}}"}}}'
echo '{"type":"text","sessionID":"ses_dead","part":{"type":"text","text":"done"}}'
echo '{"type":"step_finish","sessionID":"ses_dead","part":{"type":"step-finish","reason":"stop","tokens":{"total":1200,"input":200,"output":1000,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.5}}'
sleep 8 &
exit 0
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("writing stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:              "work",
		MaxSessionWallClock: 30 * time.Second, // a cap far away — it exists only to set WaitDelay
		Console:             io.Discard,
	})
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("Invoke error = %v, want exec.ErrWaitDelay — the stub must die in Wait after emitting, not any other way", err)
	}
	if (opencode{}).WallClockCapped(res) {
		t.Error("the session was marked wall-clock capped; the child exited well inside its cap — this is a death in Wait, not our kill")
	}
	if res.Tokens != 1200 || res.CostUSD != 0.5 {
		t.Errorf("Tokens = %d, CostUSD = %v; want 1200/0.5 — the session spent, exited cleanly and died in Wait, and the budget must see all of it", res.Tokens, res.CostUSD)
	}
	if res.FinalMessage != "done" {
		t.Errorf("FinalMessage = %q, want %q", res.FinalMessage, "done")
	}
	if !res.Claim.Held() || res.Claim.TaskID != "uuid-1" || res.Claim.RunID != "run-1" {
		t.Errorf("Claim = %+v — the claim observed on the stream must survive into the Result beside the error, or releaseHeldClaim drops a live lease", res.Claim)
	}
	if !res.UsageReported {
		t.Error("UsageReported = false — the step_finish reported; silence here would feed the zero-spend bound")
	}
	if res.Untrusted != "" {
		t.Errorf("Untrusted = %q — the stream reached its end and was read whole; nothing here is truncated", res.Untrusted)
	}
}

func TestOpencodeInvokeOnALaunchFailureParsesToAZeroResult(t *testing.T) {
	stripPath(t)

	res, err := (opencode{}).Invoke(context.Background(), Invocation{Prompt: "work"})
	if err == nil {
		t.Fatal("Invoke succeeded without the binary on PATH; the negative case did not fire")
	}
	if res.Tokens != 0 || res.CostUSD != 0 || res.FinalMessage != "" || res.UsageReported {
		t.Errorf("launch failure produced %+v — with nothing emitted, parsing must yield an honest zero", res)
	}
	if res.Claim.Held() {
		t.Error("a launch failure must never carry a claim; releaseHeldClaim would post ready over a live lease")
	}
	if res.Raw[FinishReasonKey] != nil {
		t.Errorf("Raw carried a finish reason (%v) for a session that never ran", res.Raw[FinishReasonKey])
	}
}

// --- claude probe -----------------------------------------------------------
//
// The probe's streams are memory-backed tails, it tees to no console, and it
// sets no WaitDelay: no stub behaviour reachable on darwin/linux produces a
// non-exit Run error after output (signals and context kills arrive as
// *exec.ExitError; Start failures emit nothing). So the positive case drives
// the probeResult seam directly with a synthetic run error — the seam IS the
// ordering under test — while the negative case proves an empty stream parses
// to an honest zero without panicking.

func TestClaudeProbeParsesBeforeClassifyingTheRunError(t *testing.T) {
	stdout, stderr := newTail(), newTail()
	object := `{"is_error":false,"result":"the answer","terminal_reason":"","total_cost_usd":0.25,` +
		`"usage":{"input_tokens":100,"output_tokens":20,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`
	stdout.Write([]byte(object))
	runErr := errors.New("exec: WaitDelay expired before I/O complete")

	res, err := (claude{}).probeResult(stdout, stderr, runErr)
	if !errors.Is(err, runErr) {
		t.Fatalf("probeResult error = %v, want the run error passed through", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0 — a non-exit failure sets no exit code", res.ExitCode)
	}
	if res.Tokens != 120 || res.CostUSD != 0.25 {
		t.Errorf("Tokens = %d, CostUSD = %v; want 120/0.25 — the figures were sitting in stdout when the wait failed, and Probe charges on every path", res.Tokens, res.CostUSD)
	}
	if res.FinalMessage != "the answer" {
		t.Errorf("FinalMessage = %q, want %q", res.FinalMessage, "the answer")
	}
	if !res.UsageReported {
		t.Error("UsageReported = false — the object carried accounting; silence here would feed the zero-spend bound")
	}
	if res.Untrusted != "" {
		t.Errorf("Untrusted = %q — nothing was dropped; the whole object arrived", res.Untrusted)
	}
}

func TestClaudeProbeEmptyStreamOnALaunchFailureParsesToAZeroResult(t *testing.T) {
	stdout, stderr := newTail(), newTail()

	res, err := (claude{}).probeResult(stdout, stderr,
		&exec.Error{Name: "claude", Err: exec.ErrNotFound})
	if err == nil {
		t.Fatal("probeResult swallowed a launch failure")
	}
	var ee *exec.Error
	if !errors.As(err, &ee) {
		t.Errorf("error = %v (%T), want the *exec.Error passed through", err, err)
	}
	if res.Tokens != 0 || res.CostUSD != 0 || res.FinalMessage != "" || res.UsageReported {
		t.Errorf("empty stream parsed to %+v — with nothing emitted, parsing must yield an honest zero, not a panic", res)
	}
}

// The refactor that introduced probeResult moved the happy path onto it too;
// drive Probe end to end once through a fake binary so the extraction itself is
// pinned against the real exec machinery, not only against crafted tails.
func TestClaudeProbeHappyPathThroughTheSharedSeam(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "claude")
	script := `#!/bin/sh
echo '{"is_error":false,"result":"alive","total_cost_usd":0.02,"usage":{"input_tokens":10,"output_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}'
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("writing stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, err := (claude{}).Probe(context.Background(), Invocation{Prompt: ".", WorkDir: dir})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if out.Tokens != 12 || out.CostUSD != 0.02 {
		t.Errorf("Probe reported Tokens=%d CostUSD=%v, want 12/0.02", out.Tokens, out.CostUSD)
	}
	if out.Limit.Limited {
		t.Error("a clean probe verdict came back Limited")
	}
}
