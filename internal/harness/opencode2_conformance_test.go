package harness

// Exec-binary conformance tests for the opencode2 adapter — the mirror of
// opencode_conformance_test.go, against the NIGHTLY binary. Same hermetic fake
// provider and scripts, same XDG isolation, `--standalone` so the private
// server can never attach to a shared opencode2 background service (which may
// be an operator's live TUI).
//
// Opt-in, skipped (free, no spawn) otherwise:
//
//	CLANKERBAR_OPCODE2_CONFORMANCE=1 go test ./internal/harness -run Opencode2Conformance -v
//
// Requires `opencode2` on PATH. Verified against opencode2 v0.0.0-dev-17653.
// Because the stream is text-only (see opencode2.go), the assertions are
// deliberately modest: the session reached OUR provider, exited cleanly, and
// surfaced the final message — plus that the adapters's capabilities stay
// honest (no claims, no usage, no quiet-death marker) whatever the nightly does.

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	opencode2ConformanceEnv = "CLANKERBAR_OPCODE2_CONFORMANCE"
	opencode2ConformanceOut = 3 * time.Minute
)

// opencode2FakeConfig writes the opencode 2.x-schema config that points the
// nightly at the fake provider: `model` is an OBJECT ({providerID, model}),
// servers live under `mcp.servers` — the dialect difference documented in
// opencode2.go. Handed to the adapter exactly as the fleet hands its harness a
// config file, via OPENCODE_CONFIG.
func opencode2FakeConfig(t *testing.T, providerURL string) string {
	t.Helper()
	cfg := fmt.Sprintf(`{
  "$schema": "https://opencode.ai/config.json",
  "model": { "providerID": "fake", "model": "fake-model" },
  "provider": {
    "fake": {
      "npm": "@ai-sdk/openai-compatible",
      "options": { "baseURL": %q, "apiKey": "test-key" },
      "models": { "fake-model": { "name": "Fake Model" } }
    }
  }
}`, providerURL+"/v1")
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/opencode.json", []byte(cfg), 0o600); err != nil {
		t.Fatalf("write opencode2 config: %v", err)
	}
	return dir + "/opencode.json"
}

func requireOpencode2Conformance(t *testing.T) {
	t.Helper()
	if os.Getenv(opencode2ConformanceEnv) == "" {
		t.Skipf("set %s=1 to exec the real opencode2 binary (see docs/opencode2.md)", opencode2ConformanceEnv)
	}
	if _, err := exec.LookPath("opencode2"); err != nil {
		t.Skipf("no opencode2 binary on PATH: %v", err)
	}
}

func logOpencode2Build(t *testing.T) {
	t.Helper()
	out, err := exec.Command("opencode2", "--version").Output()
	if err != nil {
		t.Logf("opencode2 --version: %v", err)
		return
	}
	t.Logf("opencode2 version: %s", strings.TrimSpace(string(out)))
}

func TestOpencode2Conformance(t *testing.T) {
	requireOpencode2Conformance(t)
	logOpencode2Build(t)

	for _, script := range []string{"stop", "quiet"} {
		t.Run(script, func(t *testing.T) {
			fake := &fakeOpenAI{script: script}
			srv := httptest.NewServer(fake)
			defer srv.Close()
			cfg := opencode2FakeConfig(t, srv.URL)

			ctx, cancel := context.WithTimeout(context.Background(), opencode2ConformanceOut)
			defer cancel()
			in := Invocation{
				Prompt:        conformancePrompt,
				Model:         "fake/fake-model", // provider-qualified, like the 1.x requirement
				WorkDir:       t.TempDir(),
				MCPConfigPath: cfg, // → OPENCODE_CONFIG
				Env:           opencodeIsolateEnv(t),
			}
			res, err := (opencode2{}).Invoke(ctx, in)
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if fake.hit.Load() == 0 {
				t.Fatal("fake provider was never hit — opencode2 resolved a real provider instead of fake/fake-model. NOTE: that run may have spent real tokens")
			}
			if res.ExitCode != 0 {
				t.Errorf("ExitCode = %d, want 0 (stderr: %.200s)", res.ExitCode, res.Stderr)
			}
			if res.FinalMessage != "OK" {
				t.Errorf("FinalMessage = %q, want %q", res.FinalMessage, "OK")
			}
			if res.UsageReported {
				t.Error("UsageReported = true, want false — the opencode2 stream never reports usage")
			}
			// The honest guarantees: no claims observed, no quiet-death marker
			// fabricated, no cost invented — whatever the nightly stream looked like.
			if res.Claim.Held() {
				t.Error("Claim.Held() = true — impossible: the stream carries no tool events to observe a claim from")
			}
			if (opencode2{}).ZeroUsageUnknown(res) {
				t.Error("ZeroUsageUnknown = true — must stay false: the marker requires a step_finish event this surface does not emit")
			}
			if res.CostUSD != 0 {
				t.Errorf("CostUSD = %v, want 0 — this adapter must not invent cost", res.CostUSD)
			}
		})
	}
}
