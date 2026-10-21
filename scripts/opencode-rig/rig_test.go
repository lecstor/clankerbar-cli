package main

import (
	"encoding/json"
	"io"
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

// TestDispatchAcceptsTheDocumentedFlagOnlyInvocation is the regression for the
// rig's own Quick start: `run.sh --model ...` (no `run` word) must run, and a
// --dry-run must not poison the directory it prepared.
func TestDispatchAcceptsTheDocumentedFlagOnlyInvocation(t *testing.T) {
	out := filepath.Join(t.TempDir(), "run")
	args := []string{
		"--model", "commandcode/gpt-6-luna",
		"--scenario", "schema",
		"--schema-tool", "frobnicate",
		"--schemas", filepath.Join("testdata", "schema-proposed.json"),
		"--upstream", "https://api.commandcode.invalid/provider/v1",
		"--out", out,
		"--dry-run",
	}
	printed := captureStdout(t, func() int { return dispatch(args) })
	if !strings.Contains(printed, "clankerbar_frobnicate") {
		t.Errorf("the dry run's printed session does not ask for the selected tool:\n%s", printed)
	}

	// A dry run writes no run.json, so the very directory it prepared is usable
	// by the real run (the reuse guard refuses a directory that holds one).
	if fileExists(filepath.Join(out, "run.json")) {
		t.Fatal("--dry-run wrote run.json; the directory is now refused for the real run")
	}
	if code := dispatch(args); code != 0 {
		t.Fatalf("second dispatch into the same --out = %d, want 0", code)
	}

	// The rig pins the adapter's read-only policy: `shell` unscoped would let the
	// model read the repo the sandbox rules were meant to hide.
	content, err := os.ReadFile(filepath.Join(out, "opencode-config-content.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(content, &doc); err != nil {
		t.Fatal(err)
	}
	perm := doc["permission"].(map[string]any)
	if perm["shell"] != "deny" {
		t.Errorf("permission.shell = %v, want deny (the read-only policy)", perm["shell"])
	}
	if perm["edit"] != "deny" {
		t.Errorf("permission.edit = %v, want deny", perm["edit"])
	}

	// Both config artifacts can carry a literal provider key: owner-only.
	for _, name := range []string{"opencode-config.json", "opencode-config-content.json"} {
		info, err := os.Stat(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", name, got)
		}
	}
}

// TestThePaddingScenarioRefusesAPreCutoverCatalog pins the loud failure: the
// residual lives on edit_task, so a catalog that predates the tool cutover
// would leave the model with every tool except the one the scenario studies,
// and the report would describe a run that proved nothing.
func TestThePaddingScenarioRefusesAPreCutoverCatalog(t *testing.T) {
	dir := t.TempDir()
	cat := filepath.Join(dir, "pre-cutover.json")
	if err := os.WriteFile(cat, []byte(`{"tools":[{"name":"old_task_tool","inputSchema":{"type":"object"}}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	code := dispatch([]string{
		"--model", "commandcode/gpt-6-luna",
		"--scenario", "padding",
		"--schemas", cat,
		"--upstream", "https://api.commandcode.invalid/provider/v1",
		"--out", filepath.Join(dir, "run"),
		"--dry-run",
	})
	if code != 1 {
		t.Fatalf("dispatch = %d, want 1 (a catalog with no edit_task exercises nothing)", code)
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what it
// printed.
func captureStdout(t *testing.T, fn func() int) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	code := fn()
	os.Stdout = old
	w.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("dispatch = %d, want 0", code)
	}
	return string(data)
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
