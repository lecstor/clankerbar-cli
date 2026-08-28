package harness

// Exec-binary conformance tests for the opencode2 adapter — the mirror of
// opencode_conformance_test.go, against the installed beta binary. Same
// hermetic fake provider and scripts, same XDG isolation, `--standalone` so
// the private server can never attach to a shared opencode2 background service
// (which may be an operator's live TUI).
//
// Opt-in, skipped (free, no spawn) otherwise:
//
//	CLANKERBAR_OPCODE2_CONFORMANCE=1 go test ./internal/harness -run Opencode2Conformance -v
//
// Requires `opencode2` on PATH. Verified against opencode2 v0.0.0-beta-18314
// (2026-08-28). The build is identified from the SESSION RECORD — the
// `version=` line the beta writes into its log under the isolated data dir —
// never from PATH alone (docs/opencode-build.md's rule). Because the stream's
// usable surface is a plain text answer (see opencode2.go), the assertions are
// deliberately modest: the session reached OUR provider, exited per script,
// surfaced the final message, and reported NO usage the beta did not emit —
// plus that the adapter's capabilities stay honest (no claims, no invented
// cost, no fabricated quiet-death marker) whatever the beta does.
//
// NOTE on the operator's ambient config: beta-18314 reads ~/.claude,
// ~/.agents and ~/.config/opencode2 HARDCODED, regardless of XDG_CONFIG_HOME
// (verified via `opencode2 debug config`). The XDG isolation below therefore
// isolates the beta's OWN state but does not hide the machine's ambient
// configs; the fake provider wins the merge for the model this suite asks
// for, which is what the fake-hit assertion checks. The ambient files are
// exactly what the widened doctor audit (spawnsOpencode) reports on.

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	opencode2ConformanceEnv = "CLANKERBAR_OPCODE2_CONFORMANCE"
	opencode2ConformanceOut = 3 * time.Minute
)

// opencode2FakeConfig writes the opencode 2.x-schema config that points the
// beta at the fake provider: `model` is an OBJECT ({providerID, model}),
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

// sessionRecordVersion reads the version of the build that created the session
// from the session record the beta writes under its data dir — the same
// `version=` line docs/opencode-build.md reads for the v1 line
// (~/.local/share/opencode/log/opencode.log records it on the `cli starting`
// line). Never answered from PATH.
var versionRecordRe = regexp.MustCompile(`version=([0-9A-Za-z._-]+)`)

func sessionRecordVersion(t *testing.T, dataDir string) string {
	t.Helper()
	log := filepath.Join(dataDir, "opencode", "log", "opencode.log")
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read session record %s: %v — the beta must write it (docs/opencode2.md)", log, err)
	}
	m := versionRecordRe.FindSubmatch(data)
	if m == nil {
		t.Fatalf("no version= line in session record %s", log)
	}
	return string(m[1])
}

// logOpencode2Build names the build about to be executed: the session record
// version (the load-bearing identification) plus PATH's --version as the
// cross-check that the record and the executed binary agree. A mismatch is a
// hard failure: it would mean the session record does not identify the build.
func logOpencode2Build(t *testing.T, dataDir string) {
	t.Helper()
	record := sessionRecordVersion(t, dataDir)
	if bin, err := exec.LookPath("opencode2"); err == nil {
		t.Logf("execing opencode2 at %s", bin)
	}
	out, err := exec.Command("opencode2", "--version").Output()
	if err != nil {
		t.Fatalf("opencode2 --version: %v", err)
	}
	pathVer := strings.TrimSpace(string(out))
	t.Logf("opencode2 version from session record: %s", record)
	t.Logf("opencode2 version from PATH: %s", pathVer)
	// `--version` prints "opencode2 v0.0.0-beta-18314"; the record carries the
	// bare version. Strip the name prefix for the agreement check.
	if !strings.Contains(pathVer, record) {
		t.Fatalf("session record says %q but PATH says %q — the record does not identify this build", record, pathVer)
	}
}

func TestOpencode2Conformance(t *testing.T) {
	requireOpencode2Conformance(t)

	for _, script := range []string{"stop", "quiet"} {
		t.Run(script, func(t *testing.T) {
			fake := &fakeOpenAI{script: script}
			srv := httptest.NewServer(fake)
			defer srv.Close()
			cfg := opencode2FakeConfig(t, srv.URL)

			// XDG_DATA_HOME is named explicitly (not only via opencodeIsolateEnv)
			// so the session record's log can be read back afterwards.
			dataDir := t.TempDir()
			env := append(opencodeIsolateEnv(t), "XDG_DATA_HOME="+dataDir)

			ctx, cancel := context.WithTimeout(context.Background(), opencode2ConformanceOut)
			defer cancel()
			in := Invocation{
				Prompt:        conformancePrompt,
				Model:         "fake/fake-model", // provider-qualified, like the 1.x requirement
				WorkDir:       t.TempDir(),
				MCPConfigPath: cfg, // → OPENCODE_CONFIG
				Env:           env,
			}
			res, err := (opencode2{}).Invoke(ctx, in)
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			// The build is identified from the SESSION RECORD the run just
			// wrote, never from PATH (docs/opencode-build.md's rule).
			logOpencode2Build(t, dataDir)
			if fake.hit.Load() == 0 {
				t.Fatal("fake provider was never hit — opencode2 resolved a real provider instead of fake/fake-model. NOTE: that run may have spent real tokens")
			}
			// Exit-code expectations are script-specific: the stop script ends
			// 0 cleanly with the answer, while the quiet script is the #43622
			// shape that beta-18314 makes exit 1 (loud failure) — see the
			// switch below, which pins each direction.
			if res.FinalMessage != "OK" {
				t.Errorf("FinalMessage = %q, want %q", res.FinalMessage, "OK")
			}
			// The honest guarantees, per script — pinned to what beta-18314
			// ACTUALLY does (verified 2026-08-28), which differs from both the
			// vanished nightly's doc and the reland draft's assumptions:
			//
			//   stop:  the fake's API stream carries usage (18511 prompt + 2
			//     completion), but beta-18314 does NOT surface it — the plain
			//     text answer emits only a text event, no step_finish. So
			//     UsageReported stays FALSE and Tokens 0; ReportsCost is false
			//     and budget.max_cost_usd is inert (docs/opencode2.md). The
			//     run exits 0 cleanly with the answer.
			//   quiet: the #43622 shape (no finish_reason) makes beta-18314 emit
			//     typed `provider.invalid-output` error events, RETRY internally
			//     a few times, then exit 1 — a LOUD failure, not a silent
			//     exit-0. The adapter reads it as exit 1 with the typed error
			//     text visible to classification, and no usage, no claims, and
			//     NO quiet-death marker (ZeroUsageUnknown stays false: the
			//     build does not exhibit the silent signature).
			switch script {
			case "stop":
				if res.ExitCode != 0 {
					t.Errorf("ExitCode = %d, want 0 (stderr: %.200s)", res.ExitCode, res.Stderr)
				}
				if res.UsageReported {
					t.Error("UsageReported = true — beta-18314 does not surface the provider usage block on a plain text answer")
				}
				if res.Tokens != 0 {
					t.Errorf("Tokens = %d, want 0 — no step_finish follows a plain text answer on beta-18314", res.Tokens)
				}
			case "quiet":
				if res.ExitCode == 0 {
					t.Error("ExitCode = 0 — the #43622 shape must exit 1 on beta-18314 (loud failure, not silent death)")
				}
				if !strings.Contains(opencode2ErrorText(res), "provider.invalid-output") &&
					!strings.Contains(opencode2ErrorText(res), "finish_reason") {
					t.Errorf("typed error event not visible to classification; stderr=%q stdout=%q", res.Stderr, res.Stdout)
				}
				if res.UsageReported {
					t.Error("UsageReported = true — the quiet script's stream carries no usage and none may be invented")
				}
			}
			if res.Claim.Held() {
				t.Error("Claim.Held() = true — impossible: the adapter does not consume tool events to observe a claim")
			}
			if res.CostUSD != 0 {
				t.Errorf("CostUSD = %v, want 0 — the adapter must not invent cost", res.CostUSD)
			}
			if (opencode2{}).ZeroUsageUnknown(res) {
				t.Error("ZeroUsageUnknown = true — must stay false: beta-18314 fails loudly (exit 1 + error event), never the silent exit-0 signature")
			}
		})
	}
}
