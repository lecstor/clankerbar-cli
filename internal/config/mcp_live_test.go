package config

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// The doneWhen's live clause: an opencode session spawned in a workdir holding a
// Claude-shaped .mcp.json starts, rather than dying at spawn. Everything up
// through the adapter runs for real - config discovery and the CLA-318 gate, the
// adapter env (OPENCODE_PERMISSION et al), and the installed opencode binary.
// Only the MODEL is bogus, so the run ends right after session creation with a
// provider error instead of doing work or spending money.
//
// Opt-in because it needs the real binary: set CLANKERBAR_LIVE_OPENCODE=1.
// Verified against opencode 1.18.19 (the fleet-pinned version); on 1.18.2 the
// pre-fix behavior was a hard refusal before any session existed.

func TestLiveOpencodeSpawn_InAClaudeShapedWorkdirStarts(t *testing.T) {
	if os.Getenv("CLANKERBAR_LIVE_OPENCODE") == "" {
		t.Skip("live probe; set CLANKERBAR_LIVE_OPENCODE=1 with the opencode binary on PATH")
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skipf("no opencode binary on PATH: %v", err)
	}

	dir := t.TempDir()
	mcp := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(mcp, []byte(`{"mcpServers":{"clankerbar":{"type":"http","url":"https://clankerbar.com/mcp/proj"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	c := &Config{Harness: "opencode", WorkDir: dir, BacklogURL: "https://clankerbar.com", Prompt: "."}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	// Precondition: the gate resolved the Claude-shaped file out of the hand-off.
	handoff := c.ResolveMCPConfig("opencode", "", nil)
	if handoff != "" {
		t.Fatalf("ResolveMCPConfig(opencode) = %q, want \"\" - this probe is pointless unless the gate fired", handoff)
	}

	a, err := harness.Get("opencode")
	if err != nil {
		t.Fatalf("harness.Get(opencode): %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, _ := a.Invoke(ctx, harness.Invocation{
		Prompt:  ".",
		Probe:   true,
		Model:   "bogus/bogus",
		WorkDir: dir,
	})

	if strings.Contains(res.Stdout, "Configuration is invalid") || strings.Contains(res.Stderr, "Configuration is invalid") {
		t.Fatalf("opencode refused to start:\nstdout: %s\nstderr: %s", res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "ses_") {
		t.Fatalf("expected a created session (a \"ses_...\" id on the stream) before the bogus-model error, got:\nstdout: %s\nstderr: %s", res.Stdout, res.Stderr)
	}
}
