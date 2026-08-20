//go:build unix

package harness

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// The capture seam, end to end: a child that dies by a signal (not an exit, not
// the adapter's own kill) has to leave the signal on the Result, because the
// exit code alone reads -1 for every signal and tells no one whether the runner
// killed it, the OS did (OOM), or the child crashed (CLA-386).
//
// The stub is a real executable on PATH that emits one event and then SIGKILLs
// itself — the silent-death shape the overnight drain saw, with the process gone
// before it could write anything else.
func TestInvokeCapturesTheSignalOfASilentlyKilledChild(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "opencode")
	script := `#!/bin/sh
echo '{"type":"step_start","part":{"type":"step-start"}}'
kill -9 $$
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("writing stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:  "work",
		Console: io.Discard,
	})
	if err != nil {
		t.Fatalf("Invoke returned an error for a signalled child: %v — a signal death must read as a Result, not a launch failure", err)
	}
	if res.ExitSignal == 0 {
		t.Fatal("ExitSignal = 0 — the child was killed by a signal and the Result does not say which; the post-mortem loses the whole point")
	}
	if got := SignalName(res.ExitSignal); got != "SIGKILL" {
		t.Errorf("ExitSignal = %d (%s), want SIGKILL", res.ExitSignal, got)
	}
	if res.ExitCode == 0 {
		t.Error("ExitCode = 0 — the child did not die on a signal; the stub is inert")
	}
	if res.Untrusted != "" {
		t.Errorf("Untrusted = %q — a clean EOF after the child died is a readable stream, not a truncated one", res.Untrusted)
	}
}
