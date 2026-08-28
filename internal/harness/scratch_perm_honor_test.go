package harness

// TEMP diagnostic - never commit. Does beta-18314 HONOR the OPENCODE_PERMISSION
// env var? The fake emits a tool_use (bash) part; if the env policy is
// enforced with "*": "deny", the tool is refused and the session reports it
// rather than executing. toolExec records whether bash actually ran.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type toolCallFake struct {
	hit     atomic.Int64
	toolRun atomic.Bool
	tool    string
}

func (f *toolCallFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/models":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[{"id":"fake-model","object":"model","owned_by":"fake"}]}`)
	case "/v1/chat/completions":
		f.hit.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)
		chunk := func(delta map[string]any, finish any) {
			ch := map[string]any{
				"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": "fake-model",
				"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
			}
			b, _ := json.Marshal(ch)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
		// First turn: emit a tool call for a bash tool.
		chunk(map[string]any{"role": "assistant", "content": ""}, nil)
		chunk(map[string]any{"tool_calls": []map[string]any{{
			"index": 0,
			"id":    "call_1",
			"type":  "function",
			"function": map[string]any{
				"name":      f.tool,
				"arguments": `{"path":"/tmp/opencode/clankerbar-perm-probe-write.txt","content":"probe"}`,
			},
		}}}, "tool_calls")
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	case "/v1/tools/bash": // some builds POST tool exec here? unknown; record anyway
		f.toolRun.Store(true)
		w.WriteHeader(200)
	default:
		http.NotFound(w, r)
	}
}

func TestScratchPermEnvHonor(t *testing.T) {
	os.Remove("/tmp/opencode/clankerbar-perm-probe-ran")
	// Tool-registry probe FIRST: what tools does beta-18314 register at boot in
	// a hermetic env (no MCP, no plugins)? A fake that emits a tool call for
	// each candidate name; whatever does NOT error "Unknown tool" is registered.
	for _, tool := range []string{"bash", "read", "write", "edit", "glob", "grep", "list", "webfetch", "fetch"} {
		t.Run("tool-"+tool, func(t *testing.T) {
			fake := &toolCallFake{tool: tool}
			srv := httptest.NewServer(fake)
			defer srv.Close()
			cfgPath := t.TempDir() + "/opencode.json"
			if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf(`{
  "$schema": "https://opencode.ai/config.json",
  "model": { "providerID": "fake", "model": "fake-model" },
  "provider": {
    "fake": {
      "npm": "@ai-sdk/openai-compatible",
      "options": { "baseURL": %q, "apiKey": "test-key" },
      "models": { "fake-model": { "name": "Fake Model" } }
    }
  }
}`, srv.URL+"/v1")), 0o600); err != nil {
				t.Fatal(err)
			}
			env := os.Environ()
			env = append(env, opencodeIsolateEnv(t)...)
			env = append(env, "OPENCODE_CONFIG="+cfgPath)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "opencode2", "run", "--standalone", "--format", "json", "--model", "fake/fake-model", "--", "call the "+tool+" tool")
			cmd.Dir = t.TempDir()
			cmd.Env = env
			out, _ := cmd.CombinedOutput()
			known := !strings.Contains(string(out), "Unknown tool")
			fmt.Printf("TOOL %-10s known=%v\n", tool, known)
		})
	}
	// CONFIG-BLOCK probe: is a `permission` block in the config file honored?
	// The WIP claims scoped allows work there ("only in a permission-only
	// overlay document"). With `"permission": {"write": "allow"}` and NO --auto,
	// the write executes iff the config block is read.
	for _, permCfg := range []string{`{"write": "allow"}`, `{"write": "deny"}`} {
		t.Run("cfg-"+strings.ReplaceAll(permCfg, " ", "_"), func(t *testing.T) {
			fake := &toolCallFake{tool: "write"}
			srv := httptest.NewServer(fake)
			defer srv.Close()
			cfg := fmt.Sprintf(`{
  "$schema": "https://opencode.ai/config.json",
  "model": { "providerID": "fake", "model": "fake-model" },
  "permission": %s,
  "provider": {
    "fake": {
      "npm": "@ai-sdk/openai-compatible",
      "options": { "baseURL": %q, "apiKey": "test-key" },
      "models": { "fake-model": { "name": "Fake Model" } }
    }
  }
}`, permCfg, srv.URL+"/v1")
			cfgPath := t.TempDir() + "/opencode.json"
			if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			env := os.Environ()
			env = append(env, opencodeIsolateEnv(t)...)
			env = append(env, "OPENCODE_CONFIG="+cfgPath)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "opencode2", "run", "--standalone", "--format", "json", "--model", "fake/fake-model", "--", "write a file")
			cmd.Dir = t.TempDir()
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			_ = out
			ran := false
			if _, statErr := os.Stat("/tmp/opencode/clankerbar-perm-probe-write.txt"); statErr == nil {
				ran = true
			}
			fmt.Printf("PERMCFG %-28s err=%v exit=%d fileWritten=%v\n", "cfg-"+permCfg, err, cmd.ProcessState.ExitCode(), ran)
		})
	}
	// Decisive probe: `--auto` auto-approves what is not explicitly denied. If
	// OPENCODE_PERMISSION is honored, `{"write":"allow","*":"deny"}` + --auto
	// must EXECUTE the write while `{"*":"deny"}` + --auto must refuse it. If
	// both execute (or both refuse), the env var is not a knob this build reads.
	for _, tc := range []struct {
		name string
		perm string
		auto bool
	}{
		{"auto-writeallow", `{"write": "allow", "*": "deny"}`, true},
		{"auto-denyall", `{"*": "deny"}`, true},
		{"noauto-writeallow", `{"write": "allow", "*": "deny"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &toolCallFake{tool: "write"}
			srv := httptest.NewServer(fake)
			defer srv.Close()
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
}`, srv.URL+"/v1")
			cfgPath := t.TempDir() + "/opencode.json"
			if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			env := os.Environ()
			env = append(env, opencodeIsolateEnv(t)...)
			env = append(env, "OPENCODE_CONFIG="+cfgPath)
			env = append(env, "OPENCODE_PERMISSION="+tc.perm)
			os.Remove("/tmp/opencode/clankerbar-perm-probe-write.txt")
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			args := []string{"run", "--standalone", "--format", "json"}
			if tc.auto {
				args = append(args, "--auto")
			}
			args = append(args, "--model", "fake/fake-model", "--", "write a file")
			cmd := exec.CommandContext(ctx, "opencode2", args...)
			cmd.Dir = t.TempDir()
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			ran := false
			if _, statErr := os.Stat("/tmp/opencode/clankerbar-perm-probe-write.txt"); statErr == nil {
				ran = true
			}
			fmt.Printf("PERM %-24s err=%v exit=%d fileWritten=%v\n", tc.name, err, cmd.ProcessState.ExitCode(), ran)
			for _, line := range strings.Split(string(out), "\n") {
				if strings.Contains(line, `"tool_use"`) {
					if len(line) > 500 {
						line = line[:500] + "…"
					}
					fmt.Printf("  %s\n", line)
				}
			}
		})
	}
}
