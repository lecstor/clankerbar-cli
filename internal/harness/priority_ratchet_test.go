package harness

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The ratchet for CLA-899's session_nice dial. A NEW adapter (or a new spawn
// inside an existing one: a second server, a resumed run) that calls
// exec.CommandContext directly would start that process at the daemon's own
// priority whatever the config says, and nothing else would fail. Derived from
// the directory rather than a hand-kept list, for the same reason as the
// Invocation.ModelArg ratchet in model_test.go.
func TestEverySessionSpawnGoesThroughSessionCommand(t *testing.T) {
	// priority_unix.go / priority_other.go ARE the wrapper: their own
	// exec.CommandContext is the one legitimate call, and the paths differ per
	// build tag.
	exempt := map[string]bool{
		"priority_unix.go":  true,
		"priority_other.go": true,
	}
	raw := regexp.MustCompile(`exec\.CommandContext\(`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	swept := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || exempt[name] {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		swept++
		if hit := raw.Find(src); hit != nil {
			t.Errorf("%s spawns a process with %s instead of sessionCommand — session_nice would not reach it", name, hit)
		}
	}
	// Guard the guard: a sweep that read nothing would pass silently forever.
	if swept < 3 {
		t.Fatalf("the sweep read %d files, which is too few to be reading the package at all", swept)
	}
}

// The second half of the ratchet: every call must carry the invocation's value.
// A spawn that goes through sessionCommand with a literal 0 would satisfy the
// sweep above and still ignore the operator's dial. sessionCommand's context
// argument is named differently per adapter (sctx, lctx, ctx, ectx), so the
// pattern matches any single argument before the priority one.
func TestEverySessionCommandCallCarriesTheInvocationsNice(t *testing.T) {
	call := regexp.MustCompile(`sessionCommand\((\w+), ([^,]+),`)
	// niceValue is startOpencodeServer's parameter. Its ONE caller is asserted
	// below to pass in.Nice, so the value still comes from the invocation.
	allowed := map[string]bool{"in.Nice": true, "niceValue": true}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	calls := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") ||
			name == "priority_unix.go" || name == "priority_other.go" {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, m := range call.FindAllStringSubmatch(string(src), -1) {
			calls++
			if !allowed[m[2]] {
				t.Errorf("%s: sessionCommand(ctx, %s, ...) — the priority argument must be in.Nice, or session_nice is dropped for that spawn", name, m[2])
			}
		}
	}
	// The opencode server's parameter has to be fed from the invocation.
	stream, err := os.ReadFile("opencode_stream.go")
	if err != nil {
		t.Fatalf("read opencode_stream.go: %v", err)
	}
	if !strings.Contains(string(stream), "startOpencodeServer(sctx, in.Nice, env, workDir)") {
		t.Error("opencode_stream.go no longer passes in.Nice to startOpencodeServer — the session's tool calls run in that server")
	}
	// Four adapters spawn a session (claude, codex, copilot, opencode), plus
	// claude's probe, opencode's private server and its post-run export.
	if calls < 7 {
		t.Fatalf("found %d sessionCommand call(s), want at least 7 — the sweep is not reading the adapters", calls)
	}
}

// Config's validation refuses a positive session_nice where the platform has no
// nice(1), using this constant. Pinned so a future platform file cannot flip it
// silently.
func TestSessionPrioritySupportedMatchesTheSpawn(t *testing.T) {
	if !SessionPrioritySupported {
		t.Skip("this platform has no session priority; the refusal is config's to test there")
	}
	cmd, err := sessionCommand(context.Background(), 1, "true")
	if err != nil {
		t.Fatalf("sessionCommand with a supported platform refused: %v", err)
	}
	if filepath.Base(cmd.Path) != "nice" {
		t.Errorf("a lowered session spawns %q, want the nice(1) wrapper", cmd.Path)
	}
}
