package harness

// Exec-binary conformance tests for the opencode adapter.
//
// These are the "how we call opencode" tests: they run the REAL `opencode`
// binary that execs on PATH through the REAL adapter (`opencode{}.Invoke`),
// pointed at a hermetic fake OpenAI-compatible provider that scripts exactly
// what "the model" returns — including the shape from
// https://github.com/anomalyco/opencode/issues/43622 that makes opencode
// process a response and hand the CLI a GARBLED/BROKEN result: a final step
// that reads `reason: "unknown"` with all-zero usage, and a silent exit 0.
//
// Everything else in this package parses SAVED opencode output; these tests
// are the only ones that exec the binary, so they are opt-in:
//
//	CLANKERBAR_OPCODE_CONFORMANCE=1 go test ./internal/harness -run OpencodeConformance -v
//
// and skipped (free, no network, no spawn) otherwise.
//
// Live mode, for one genuine paid turn against the provider configured on this
// machine (the "is opencode adoption-ready" gate):
//
//	CLANKERBAR_OPCODE_CONFORMANCE_LIVE=1 \
//	OPENCODE_LIVE_MODEL=opencode-go/deepseek-v4-flash \
//	go test ./internal/harness -run OpencodeConformance_live -v
//
// Two findings that shaped this test, both verified against opencode 1.18.18:
//
//   - The model id MUST be provider-qualified ("fake/fake-model", never
//     "fake-model"): a bare id does not resolve and opencode silently runs its
//     own default provider (opencode-go) instead — which is how the very first
//     draft of this test "passed" against a real backend. The provider's
//     `npm: @ai-sdk/openai-compatible` is present in opencode's node_modules.
//     Asserting the fake provider was actually HIT (never just reached) is the
//     guard that makes "hit the fake, not opencode-go" a test property.
//   - opencode run is client/server: a server already running holds the config,
//     so `OPENCODE_CONFIG` alone is not enough to re-point a warm server. This
//     test isolates the server's state via XDG_CONFIG_HOME/XDG_DATA_HOME/
//     XDG_CACHE_HOME so each run spawns a fresh server that reads OUR config.
//     The production fleet does not set these; it relies on config_dir pointing
//     at the real config dir (see docs/harness-conformance.md).
//
// `config_dir` is set on the Invocation for fleet parity but appears inert on
// 1.18.18 (OPENCODE_CONFIG_DIR is not a knob opencode honors); the XDG isolate
// is what actually pins the config. Both facts are documented in
// docs/harness-conformance.md.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	conformanceEnv    = "CLANKERBAR_OPCODE_CONFORMANCE"
	conformanceLive   = "CLANKERBAR_OPCODE_CONFORMANCE_LIVE"
	conformanceModel  = "OPENCODE_LIVE_MODEL" // e.g. opencode-go/deepseek-v4-flash
	conformancePrompt = "Reply with exactly: OK"
	// conformanceTimeout bounds each exec'd session so a version that stalls
	// talking to the fake provider fails fast with a targeted message instead of
	// blocking until go test's default timeout. A plain context deadline — never
	// Invocation.MaxSessionWallClock, which would add the wall_clock_capped
	// terminal marker and muddy the quiet-death assertions.
	conformanceTimeout = 3 * time.Minute
)

// fakeOpenAI is a minimal OpenAI-compatible streaming chat server. Each
// instance scripts one response shape; opencode's session sees it as a real
// provider. `hit` records that a /v1/chat/completions request arrived, which is
// the proof the session ran against THIS provider and not opencode's built-in.
type fakeOpenAI struct {
	script string // "stop" (clean, with usage) or "quiet" (#43622 shape)
	hit    atomic.Int64
}

// chat/completions streams chunks framed exactly as the AI SDK's
// openai-compatible provider parses them. The ONLY variable between the two
// scripts is whether any chunk carries a non-null finish_reason:
//
//   - stop:  a terminal chunk with finish_reason "stop" plus a usage block →
//            opencode reports reason "stop" and the usage.
//   - quiet: NO chunk ever carries a non-null finish_reason, no usage →
//            opencode's final step reads reason "unknown", all-zero usage, and
//            the process exits 0 with no error — the silent-death signature
//            (anomalyco/opencode#43622, our CLA-398/401/406 family).
func (f *fakeOpenAI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/models":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[{"id":"fake-model","object":"model","owned_by":"fake"}]}`)
	case "/v1/chat/completions":
		f.hit.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)
		if flusher == nil {
			// http.NotFoundWriter in httptest always flushes, but fail
			// predictably if this fake is ever reused against a writer that does
			// not implement Flusher rather than panicking mid-stream.
			http.Error(w, "response writer does not support flushing", http.StatusInternalServerError)
			return
		}
		chunk := func(delta map[string]any, finish any, usage any) {
			c := make(map[string]any, len(delta)+1)
			for k, v := range delta {
				c[k] = v
			}
			c["finish_reason"] = finish
			ch := map[string]any{
				"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": "fake-model",
				"choices": []map[string]any{{"index": 0, "delta": c, "finish_reason": finish}},
			}
			if usage != nil {
				ch["usage"] = usage
			}
			b, _ := json.Marshal(ch)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
		switch f.script {
		case "quiet":
			chunk(map[string]any{"role": "assistant", "content": ""}, nil, nil)
			chunk(map[string]any{"content": "OK"}, nil, nil)
			fmt.Fprint(w, "data: [DONE]\n\n")
			flusher.Flush()
		default: // stop
			chunk(map[string]any{"role": "assistant", "content": ""}, nil, nil)
			chunk(map[string]any{"content": "OK"}, nil, nil)
			chunk(map[string]any{}, "stop", map[string]any{"prompt_tokens": 18511, "completion_tokens": 2, "total_tokens": 18513})
			fmt.Fprint(w, "data: [DONE]\n\n")
			flusher.Flush()
		}
	default:
		http.NotFound(w, r)
	}
}

// opencodeFakeConfig writes the opencode-schema config that points opencode at
// the fake provider. It is handed to the adapter exactly as the fleet hands
// `harnesses.opencode.mcp_config_path` — via OPENCODE_CONFIG — so a file opencode
// cannot parse (a Claude-shaped one) would die here, which is the CLA-263
// surface. The fleet has no reason to pass --model (opencode owns its model in
// its config), so the model lives in this file and must be provider-qualified.
func opencodeFakeConfig(t *testing.T, providerURL string) string {
	t.Helper()
	cfg := fmt.Sprintf(`{
  "$schema": "https://opencode.ai/config.json",
  "model": "fake/fake-model",
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
		t.Fatalf("write opencode config: %v", err)
	}
	return dir + "/opencode.json"
}

// opencodeIsolateEnv redirects opencode's entire state (config, data, cache)
// into fresh temp dirs so the client/server it spawns is guaranteed to read OUR
// config and never a long-lived server holding the fleet's. This is the knob
// that makes the test hermetic; it is the only env the test adds on top of what
// the adapter itself sets (OPENCODE_PERMISSION / OPENCODE_CONFIG_DIR /
// OPENCODE_CONFIG).
func opencodeIsolateEnv(t *testing.T) []string {
	t.Helper()
	keys := []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME"}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+t.TempDir())
	}
	return out
}

// invokeOpencode drives the REAL adapter: it execs `opencode run --format json`
// exactly the way production does, on the caller's PATH, with the adapter's own
// env (permission policy, config dir, OPENCODE_CONFIG) plus the isolate env. A
// session that outlives conformanceTimeout is killed and the test fails rather
// than hanging.
func invokeOpencode(t *testing.T, cfgPath, workdir string, extraEnv []string) (Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), conformanceTimeout)
	defer cancel()
	in := Invocation{
		Prompt:        conformancePrompt,
		WorkDir:       workdir,
		MCPConfigPath: cfgPath, // → OPENCODE_CONFIG
		ConfigDir:     t.TempDir(),
		Env:           extraEnv,
	}
	return (opencode{}).Invoke(ctx, in)
}

// requireOpencodeConformance skips unless the operator has explicitly opted in
// AND the binary is on PATH. CI's `go test -race ./...` therefore stays green,
// fast and free; the exec test is a local gate, not a gate on every push.
func requireOpencodeConformance(t *testing.T) {
	t.Helper()
	if os.Getenv(conformanceEnv) == "" {
		t.Skipf("set %s=1 to exec the real opencode binary (see docs/harness-conformance.md)", conformanceEnv)
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skipf("no opencode binary on PATH: %v", err)
	}
}

// logOpencodeBuild names the binary that is about to be executed — the fleet
// runs Homebrew stable 1.18.x, and the docs warn that the abandoned opencode2
// nightly can silently take over if its dir lands on PATH (docs/opencode-build.md).
// Not a hard failure: a newer stable should not break the suite. It lets the
// log say which build this result came from. The nightly's version is
// `0.0.0-next-…`, so a warn keys on that prefix (a stable pre-release like
// 1.19.0-rc is not flagged — the check is log-only either way).
var opencodeVersionishRe = regexp.MustCompile(`^\d+\.\d+\.\d+`)

func logOpencodeBuild(t *testing.T) {
	t.Helper()
	if bin, err := exec.LookPath("opencode"); err == nil {
		t.Logf("execing opencode at %s", bin)
	}
	out, err := exec.Command("opencode", "--version").Output()
	if err != nil {
		t.Logf("opencode --version: %v", err)
		return
	}
	ver := strings.TrimSpace(string(out))
	t.Logf("opencode version: %s", ver)
	if strings.HasPrefix(ver, "0.0.0") || !opencodeVersionishRe.MatchString(ver) {
		t.Logf("WARNING: this does not look like the fleet's stable 1.18.x line — see docs/opencode-build.md")
	}
}

// TestOpencodeConformance_control pins the healthy shape: a provider that
// streams a finished answer (reason stop, with usage) makes the adapter report
// a clean, spend-accounted, NOT-silent session.
func TestOpencodeConformance_control(t *testing.T) {
	requireOpencodeConformance(t)
	logOpencodeBuild(t)

	fake := &fakeOpenAI{script: "stop"}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	cfg := opencodeFakeConfig(t, srv.URL)

	res, err := invokeOpencode(t, cfg, t.TempDir(), opencodeIsolateEnv(t))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if fake.hit.Load() == 0 {
		t.Fatal("fake provider was never hit — opencode used a different (real) provider instead of fake/fake-model. NOTE: if the model id failed to resolve, that real run may have spent a real paid token before this failure")
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if !res.UsageReported {
		t.Errorf("UsageReported = false, want true (the stop script carries a usage block)")
	}
	if res.Tokens == 0 {
		t.Errorf("Tokens = 0, want > 0 (usage was sent)")
	}
	if got := res.Raw[FinishReasonKey]; got != "stop" {
		t.Errorf("finish_reason = %v, want %q", got, "stop")
	}
	if res.FinalMessage != "OK" {
		t.Errorf("FinalMessage = %q, want %q", res.FinalMessage, "OK")
	}
	if (opencode{}).ZeroUsageUnknown(res) {
		t.Error("ZeroUsageUnknown = true on a healthy stop session")
	}
}

// TestOpencodeConformance_quietDeath pins the CLI's handling of the #43622
// shape: opencode processes a model response whose stream never carries a
// finish reason, and hands the CLI a silent exit-0, reason "unknown",
// all-zero-usage end. The CLI's job is to NAME that death rather than read it
// as a clean completion.
//
// The assertion is SIGNATURE-CONSISTENT, deliberately, so a fix upstream does
// not produce a false red:
//
//   - If the session that just ran carried the signature (opencode still has
//     the bug), the CLI MUST flag it — that is the regression this test
//     catches (the adapter stops naming a death opencode still produces).
//   - If the session did NOT carry the signature (upstream fixed it), the CLI
//     must NOT flag it, and the test turns GREEN with a loud log that the
//     scenario changed and needs re-pinning — never a red for working code.
//   - If upstream lands the "fix" we told them not to ship (the unbounded
//     retry loop), the session never finishes and conformanceTimeout trips:
//     a targeted red.
func TestOpencodeConformance_quietDeath(t *testing.T) {
	requireOpencodeConformance(t)
	logOpencodeBuild(t)

	fake := &fakeOpenAI{script: "quiet"}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	cfg := opencodeFakeConfig(t, srv.URL)

	res, err := invokeOpencode(t, cfg, t.TempDir(), opencodeIsolateEnv(t))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if fake.hit.Load() == 0 {
		t.Fatal("fake provider was never hit — opencode used a different (real) provider instead of fake/fake-model. NOTE: if the model id failed to resolve, that real run may have spent a real paid token before this failure")
	}

	if observedQuietDeath(res) {
		// The broken-world branch: this build still emits the #43622 death.
		// Note UsageReported is deliberately not asserted — opencode emits a
		// (all-zero) tokens block even on the quiet shape, and the adapter
		// correctly counts "the event existed" as reported (CLA-288); the
		// discriminator is the all-zero SUM plus the unknown reason.
		if (opencode{}).ZeroUsageUnknown(res) {
			t.Logf("reproduced the #43622 silent-death signature (exit 0, reason unknown, all-zero usage) and the CLI names it (ZeroUsageUnknown true)")
		} else {
			t.Error("session carried the #43622 silent-death signature (exit 0, reason unknown, all-zero usage) but ZeroUsageUnknown is false — the CLI would read this death as a clean completion")
		}
		return
	}

	// The post-fix branch. Green, because the CLI is not misclassifying
	// anything — but it is a changed world the fixture was built for, so say so
	// out loud rather than quietly passing.
	if (opencode{}).ZeroUsageUnknown(res) {
		t.Error("session did NOT carry the #43622 signature but ZeroUsageUnknown is true — a false positive")
	}
	t.Logf("WARNING: the #43622 silent-death signature (exit 0, reason unknown, all-zero usage) no longer reproduced with this opencode build — upstream likely fixed it. re-pin docs/harness-conformance.md and reconsider CLA-406/ZeroUsageUnknown; this PASS is no longer coverage of the old bug")
}

// observedQuietDeath reports whether THIS session, as produced by whatever
// `opencode` binary just ran, carried the #43622 signature: exit 0 with a final
// step whose reason is "unknown" and whose usage summed to zero. It is a
// property of the WORLD (what the binary handed over), not of the CLI — the
// CLI's obligation, asserted above, is to flag this signature whenever it sees
// it and only when it sees it.
func observedQuietDeath(res Result) bool {
	if res.ExitCode != 0 {
		return false
	}
	if reason, _ := res.Raw[FinishReasonKey].(string); reason != FinishReasonUnknown {
		return false
	}
	return res.Tokens == 0 && res.CostUSD == 0
}

// TestOpencodeConformance_live is the paid, non-hermetic gate: ONE real turn
// against whatever provider this machine has configured (it must be named via
// OPENCODE_LIVE_MODEL), to confirm the adapter reaches a real provider and its
// stream parses end to end. Opt-in via CLANKERBAR_OPCODE_CONFORMANCE_LIVE=1.
// Run it before flipping the fleet back to opencode; see docs/harness-conformance.md.
func TestOpencodeConformance_live(t *testing.T) {
	if os.Getenv(conformanceLive) == "" {
		t.Skipf("set %s=1 (and %s) for the live one-turn gate", conformanceLive, conformanceModel)
	}
	model := os.Getenv(conformanceModel)
	if model == "" {
		t.Fatalf("%s must name a model opencode can resolve, e.g. opencode-go/deepseek-v4-flash", conformanceModel)
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skipf("no opencode binary on PATH: %v", err)
	}
	logOpencodeBuild(t)

	in := Invocation{
		Prompt:  conformancePrompt,
		Model:   model,
		WorkDir: t.TempDir(),
		// No XDG isolate: the live gate uses the operator's real auth/config.
	}
	ctx, cancel := context.WithTimeout(context.Background(), conformanceTimeout)
	defer cancel()
	res, err := (opencode{}).Invoke(ctx, in)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0 (stderr: %s)", res.ExitCode, res.Stderr)
	}
	reason, _ := res.Raw[FinishReasonKey].(string)
	if reason == "" {
		t.Errorf("finish_reason is empty — a live provider should end with a reason (exit was clean otherwise)")
	} else {
		t.Logf("finish_reason = %q (a trivial \"Reply with exactly: OK\" turn should be \"stop\", but the gate is NOT being a quiet death, not the exact reason)", reason)
	}
	if !res.UsageReported {
		t.Errorf("UsageReported = false — expected the real provider to report usage")
	}
	if (opencode{}).ZeroUsageUnknown(res) {
		t.Error("ZeroUsageUnknown = true on a live provider run")
	}
	t.Logf("live turn: tokens=%d cost=$%.4f final=%q", res.Tokens, res.CostUSD, res.FinalMessage)
}
