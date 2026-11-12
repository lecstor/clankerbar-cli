package harness

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// claudeStub writes an executable `claude` onto PATH: every test here needs the
// same shape, and the differences are all in the script.
func claudeStub(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A claude session that emits nothing is killed, with the cause on the Result.
func TestClaudeIdleKillsASilentSession(t *testing.T) {
	claudeStub(t, "#!/bin/sh\nexec sleep 30\n")

	start := time.Now()
	res, err := (claude{}).Invoke(context.Background(), Invocation{
		Prompt:         "work",
		MaxSessionIdle: 200 * time.Millisecond,
		Console:        io.Discard,
	})
	if err != nil {
		t.Fatalf("Invoke returned an error for our own kill: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("Invoke took %s - the watchdog did not end the session", elapsed)
	}
	if !strings.HasPrefix(res.IdleKilled, "no output for ") {
		t.Errorf("IdleKilled = %q, want the no-output cause", res.IdleKilled)
	}
	if res.ExitCode == 0 {
		t.Error("ExitCode = 0 - the child was not killed")
	}
}

// claude's tool_use opens the call and its later tool_result closes it: a call
// that never returns pauses the idle clock, and the tool cap ends the session.
func TestClaudeIdlePausesForAToolCallAndTheToolCapKills(t *testing.T) {
	claudeStub(t, `#!/bin/sh
echo '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}]}}'
exec sleep 30
`)

	start := time.Now()
	res, err := (claude{}).Invoke(context.Background(), Invocation{
		Prompt:              "work",
		MaxSessionIdle:      3 * time.Second, // wide: the stub must reach its tool line before this under -race load
		MaxToolCallDuration: 4 * time.Second,
		Console:             io.Discard,
	})
	if err != nil {
		t.Fatalf("Invoke returned an error for our own kill: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("Invoke took %s - the tool cap did not end the session", elapsed)
	}
	if res.IdleKilled != "tool call toolu_1 open for 4s" {
		t.Errorf("IdleKilled = %q, want the tool-cap cause naming the open call", res.IdleKilled)
	}
	if res.ExitCode == 0 {
		t.Error("ExitCode = 0 - the child was not killed")
	}
}

// A normal claude session is parsed exactly as before.
func TestClaudeIdleLeavesAWorkingSessionAlone(t *testing.T) {
	claudeStub(t, `#!/bin/sh
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"done"}],"usage":{"input_tokens":1,"output_tokens":1}}}'
echo '{"type":"result","result":"done","usage":{"input_tokens":1,"output_tokens":1}}'
`)

	res, err := (claude{}).Invoke(context.Background(), Invocation{
		Prompt:         "work",
		MaxSessionIdle: 2 * time.Second,
		Console:        io.Discard,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.IdleKilled != "" {
		t.Errorf("IdleKilled = %q on a working session, want none", res.IdleKilled)
	}
	if res.ExitCode != 0 || res.FinalMessage != "done" {
		t.Errorf("ExitCode = %d, FinalMessage = %q, want 0 / %q", res.ExitCode, res.FinalMessage, "done")
	}
}
