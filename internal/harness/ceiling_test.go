package harness

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The kill seam, end to end: consume marks the ceiling and cancels the derived
// context, exec.CommandContext SIGKILLs the child, Wait returns an ExitError,
// and the marker survives Invoke. Every earlier test fakes the kill function,
// so the wiring between the cancellation and the reaped process was the one
// unpinned link (CLA-343 review, finding 3).
//
// The stub is a real executable on PATH: it emits the same stream shape claude
// 2.1.229 does — assistant events carrying per-turn message.usage — then sits
// in a sleep so the kill has something to interrupt.
func TestClaudeInvokeKillsTheProcessWhenTheCeilingCrosses(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "claude")
	script := `#!/bin/sh
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"one"}],"usage":{"input_tokens":60000000,"output_tokens":0}}}'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"two"}],"usage":{"input_tokens":60000000,"output_tokens":0}}}'
exec sleep 30
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("writing stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	res, err := (claude{}).Invoke(context.Background(), Invocation{
		Prompt:           "work",
		MaxSessionTokens: 100_000_000, // the second event's 120M crosses it
		Console:          io.Discard,
	})
	if err != nil {
		t.Fatalf("Invoke returned an error for our own ceiling kill: %v — the kill must read as an orderly end, not a launch failure", err)
	}
	if !(claude{}).TokenCeilingHit(res) {
		t.Fatal("the Result does not carry the ceiling marker through Invoke")
	}
	if res.ExitCode == 0 {
		t.Error("ExitCode = 0 — the child was not killed; the mid-stream kill is inert")
	}
	if res.Tokens != 120_000_000 {
		t.Errorf("Tokens = %d, want the 120M summed before the kill — the spend must reach the budget", res.Tokens)
	}
	if res.Untrusted != "" {
		t.Errorf("Untrusted = %q: our own kill closes the pipe cleanly; a killed-by-ceiling session is not an unreadable stream", res.Untrusted)
	}
}

// The same seam, driven the other way: a session whose stream ends BEFORE the
// ceiling is crossed completes normally — exit 0, no marker — so the ceiling
// cannot fire on a session that already ended.
func TestClaudeInvokeLetsACleanSessionFinishUnderTheCeiling(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "claude")
	script := `#!/bin/sh
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"small"}],"usage":{"input_tokens":1000,"output_tokens":100}}}'
echo '{"type":"result","subtype":"success","result":"done","total_cost_usd":0.01,"usage":{"input_tokens":1100,"output_tokens":100}}'
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("writing stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	res, err := (claude{}).Invoke(context.Background(), Invocation{
		Prompt:           "work",
		MaxSessionTokens: 100_000_000,
		Console:          io.Discard,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if (claude{}).TokenCeilingHit(res) {
		t.Error("a clean session under the ceiling was marked as killed")
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if res.Tokens != 1200 {
		t.Errorf("Tokens = %d, want the result event's cumulative 1200", res.Tokens)
	}
	if !strings.Contains(res.FinalMessage, "done") {
		t.Errorf("FinalMessage = %q, want the result text", res.FinalMessage)
	}
}
