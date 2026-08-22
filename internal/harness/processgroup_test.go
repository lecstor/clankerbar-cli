package harness

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A stub that backgrounds a child which outlives its parent and writes a
// file after a delay. The kill mechanism must terminate both the parent
// and the descendant, so the file is never created.
func TestProcessGroupKillTerminatesAGrandchild(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "claude")
	marker := filepath.Join(dir, "grandchild_alive")
	script := `#!/bin/sh
# Emit a stream event that crosses the token ceiling immediately.
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"work"}],"usage":{"input_tokens":60000000,"output_tokens":0}}}'
# Background a grandchild that outlives the parent session.
(sleep 60; touch ` + marker + `) &
# Parent continues so the adapter kills it mid-stream.
exec sleep 60
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("writing stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	start := time.Now()
	res, err := (claude{}).Invoke(context.Background(), Invocation{
		Prompt:           "work",
		MaxSessionTokens: 10, // any positive ceiling kills quickly
		Console:          io.Discard,
	})
	elapsed := time.Since(start)

	// The adapter must return promptly, not hang on the grandchild's
	// inherited stdout pipe.
	if elapsed > 10*time.Second {
		t.Fatalf("Invoke blocked for %s: the kill did not reach the grandchild holding the stream", elapsed)
	}
	if err != nil {
		t.Fatalf("Invoke returned an error for the ceiling kill: %v — must be orderly", err)
	}
	if !(claude{}).TokenCeilingHit(res) {
		t.Fatal("the Result does not carry the ceiling marker")
	}
	if res.ExitCode == 0 {
		t.Error("ExitCode = 0 — the child was not killed; the group kill did not fire")
	}

	// The grandchild must be dead: the marker file must never appear.
	if _, exists := os.Stat(marker); !os.IsNotExist(exists) {
		t.Errorf("grandchild marker file %s exists — the descendant survived the group kill", marker)
	}
}

// The same shape for the opencode adapter: a wall-clock cap kills the
// process group, not just the direct child, so descendants die.
func TestOpencodeProcessGroupKillTerminatesAGrandchild(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "grandchild_alive")
	opencodeStub(t, `#!/bin/sh
# Emit a step event that triggers the cap quickly.
echo '{"type":"step_start","sessionID":"s1","part":{"type":"step-start"}}'
echo '{"type":"text","sessionID":"s1","part":{"type":"text","text":"working"}}'
echo '{"type":"step_finish","sessionID":"s1","part":{"type":"step-finish","reason":"unknown","tokens":{"input":0,"output":0,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0}}'
# Background a grandchild that outlives the parent.
(sleep 60; touch `+marker+`) &
# Parent continues so the adapter kills it mid-session.
exec sleep 60
`)

	start := time.Now()
	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:              "work",
		MaxSessionWallClock: 500 * time.Millisecond,
		Console:             io.Discard,
	})
	elapsed := time.Since(start)

	if elapsed > 20*time.Second {
		t.Fatalf("Invoke blocked for %s: the group kill did not reach the grandchild", elapsed)
	}
	if err != nil {
		t.Fatalf("Invoke returned an error for the cap: %v", err)
	}
	if !(opencode{}).WallClockCapped(res) {
		t.Error("the session was not reported as wall-clock capped")
	}

	if _, exists := os.Stat(marker); !os.IsNotExist(exists) {
		t.Errorf("grandchild marker file %s exists — the descendant survived the group kill", marker)
	}
}
