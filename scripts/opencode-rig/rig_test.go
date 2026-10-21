package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestOpencodeArgsAttachesToThePreflightServer(t *testing.T) {
	o := runOptions{Model: "commandcode/deepseek/deepseek-v4.1-flash", Prompt: "hello"}
	standalone := opencodeArgs(o)
	joined := strings.Join(standalone, " ")
	for _, want := range []string{"run", "--standalone", "--format json", "--agent build", "--model commandcode/deepseek/deepseek-v4.1-flash", "-- hello"} {
		if !strings.Contains(joined, want) {
			t.Errorf("standalone argv %q is missing %q", joined, want)
		}
	}

	o.ServerURL = "http://127.0.0.1:9999"
	attached := strings.Join(opencodeArgs(o), " ")
	if strings.Contains(attached, "--standalone") {
		t.Error("an attached run still asks for its own private server (the MCP race)")
	}
	if !strings.Contains(attached, "--server http://127.0.0.1:9999") {
		t.Errorf("argv %q does not attach to the preflighted server", attached)
	}
}

func TestRunSessionReadsStdinFromDevNull(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake opencode binary is a shell script")
	}
	out, work := runDirs(t)
	bin := fakeBin(t, `
if IFS= read -r line; then
  printf 'stdin=%s\n' "$line" > "$FAKE_OUT/stdin"
else
  printf 'stdin=eof\n' > "$FAKE_OUT/stdin"
fi
`)
	o := runOptions{
		OpencodeBin: bin, Out: out, Workdir: work, Prompt: "x", Deadline: 5 * time.Second,
		ExtraEnv: []string{"FAKE_OUT=" + out},
	}
	if code, err := runSession(o, nil, nil, o.ExtraEnv); err != nil || code != 0 {
		t.Fatalf("runSession = %d, %v", code, err)
	}
	got, err := os.ReadFile(filepath.Join(out, "stdin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "stdin=eof\n" {
		t.Fatalf("stdin = %q, want eof (opencode run hangs on an open non-TTY stdin)", got)
	}
}

func TestRunSessionDeadlineKillsTheSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake opencode binary is a shell script")
	}
	out, work := runDirs(t)
	bin := fakeBin(t, "sleep 30")
	o := runOptions{OpencodeBin: bin, Out: out, Workdir: work, Prompt: "x", Deadline: 300 * time.Millisecond}
	start := time.Now()
	code, err := runSession(o, nil, nil, nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if code != 124 {
		t.Errorf("exit = %d, want 124 (timed out)", code)
	}
	if elapsed > 10*time.Second {
		t.Errorf("the deadline took %s; the process group was not killed", elapsed)
	}
	if !fileExists(filepath.Join(out, "timed-out")) {
		t.Error("timed-out marker missing")
	}
}

func TestRunSessionStopsAtTheStubCallCap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake opencode binary is a shell script")
	}
	out, work := runDirs(t)
	bin := fakeBin(t, "sleep 30")
	o := runOptions{OpencodeBin: bin, Out: out, Workdir: work, Prompt: "x", Deadline: 10 * time.Second, MaxToolCalls: 1}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = runSession(o, nil, nil, nil)
	}()
	// The stub writes this marker when a call beyond the cap arrives; the rig
	// polls it and stops the session.
	if err := os.WriteFile(filepath.Join(out, "stub-capped"), []byte("2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runSession did not stop after the cap marker appeared")
	}
}

func TestRunSessionKeepsTheEventStreamAndStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake opencode binary is a shell script")
	}
	out, work := runDirs(t)
	bin := fakeBin(t, `
echo '{"type":"text","part":{"type":"text","text":"hi"}}'
echo 'stderr note' >&2
`)
	o := runOptions{OpencodeBin: bin, Out: out, Workdir: work, Prompt: "x", Deadline: 5 * time.Second}
	if code, err := runSession(o, nil, nil, nil); err != nil || code != 0 {
		t.Fatalf("runSession = %d, %v", code, err)
	}
	events, err := readEvents(filepath.Join(out, "opencode-events.jsonl"))
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %v, %v", events, err)
	}
	stderr, err := os.ReadFile(filepath.Join(out, "opencode-stderr.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stderr), "stderr note") {
		t.Errorf("stderr = %q", stderr)
	}
}

func runDirs(t *testing.T) (out, work string) {
	t.Helper()
	out = t.TempDir()
	work = filepath.Join(out, "sandbox")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	return out, work
}
