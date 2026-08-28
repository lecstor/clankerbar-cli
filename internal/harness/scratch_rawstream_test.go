package harness

// TEMP diagnostic - never commit. Captures the RAW --format json stream the
// installed beta-18314 actually emits for the two fake scripts, so the
// adapter's parser can be corrected against reality rather than assumption.

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScratchRawStream(t *testing.T) {
	// Config discovery check: which dirs does beta-18314 actually read, with
	// XDG_CONFIG_HOME pointed elsewhere?
	t.Run("debug-config", func(t *testing.T) {
		env := os.Environ()
		env = append(env, opencodeIsolateEnv(t)...)
		env = append(env, "OPENCODE_CONFIG_DIR=/tmp/opencode/nonexistent-oc2cfg")
		out, err := exec.Command("opencode2", "debug", "config").CombinedOutput()
		fmt.Printf("=== debug config: err=%v\n%s\n", err, string(out))
	})
	// OPENCODE_CONFIG_DIR honored as an ADDITION? Point it at a real dir with a
	// marker config file; if the marker's provider resolves, files in it load.
	t.Run("config-dir-addition", func(t *testing.T) {
		cfgDir := t.TempDir()
		if err := os.WriteFile(cfgDir+"/opencode.json", []byte(`{"model":{"providerID":"marker","model":"marker-model"},"provider":{"marker":{"npm":"@ai-sdk/openai-compatible","options":{"baseURL":"http://127.0.0.1:1/v1","apiKey":"k"},"models":{"marker-model":{"name":"Marker"}}}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		env := os.Environ()
		env = append(env, opencodeIsolateEnv(t)...)
		env = append(env, "OPENCODE_CONFIG_DIR="+cfgDir)
		out, err := exec.Command("opencode2", "debug", "config").CombinedOutput()
		fmt.Printf("=== config-dir addition: err=%v\n%s\n", err, string(out))
	})
	// Does OPENCODE_CONFIG_DIR steer PLUGIN loading? The herdr wrapper's whole
	// purpose is "OPENCODE_CONFIG_DIR keeps opencode2 pointed at our dedicated
	// config + plugins dir so the herdr v2 plugin loads there". Put a marker
	// plugin under <dir>/plugins and see whether the session log shows it
	// loading when OPENCODE_CONFIG_DIR=<dir>.
	t.Run("config-dir-plugins", func(t *testing.T) {
		cfgDir := t.TempDir()
		pluginDir := cfgDir + "/plugins"
		if err := os.MkdirAll(pluginDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pluginDir+"/marker-plugin.js", []byte(`export const ClankerbarMarkerPlugin = { id: "clankerbar-marker", setup() { console.log("CLANKERBAR-MARKER-PLUGIN-LOADED"); } }; export default ClankerbarMarkerPlugin;`), 0o600); err != nil {
			t.Fatal(err)
		}
		dataDir := t.TempDir()
		env := os.Environ()
		env = append(env, opencodeIsolateEnv(t)...)
		env = append(env, "XDG_DATA_HOME="+dataDir)
		env = append(env, "OPENCODE_CONFIG_DIR="+cfgDir)
		// A DEAD provider on purpose: the plugin-load log lines are written at
		// server boot, before the model is ever reached, so the run fails fast
		// (connection refused) and spends nothing while still proving whether
		// the plugin dir was scanned.
		deadCfg := cfgDir + "/opencode.json"
		_ = os.WriteFile(deadCfg, []byte(`{"model":{"providerID":"dead","model":"dead-model"},"provider":{"dead":{"npm":"@ai-sdk/openai-compatible","options":{"baseURL":"http://127.0.0.1:1/v1","apiKey":"k"},"models":{"dead-model":{"name":"Dead"}}}}}`), 0o600)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "opencode2", "run", "--standalone", "--format", "json", "--model", "dead/dead-model", "--", "hi")
		cmd.Dir = t.TempDir()
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		fmt.Printf("=== plugins probe: err=%v\n%s\n", err, string(out))
		logf := filepath.Join(dataDir, "opencode", "log", "opencode.log")
		if data, err := os.ReadFile(logf); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.Contains(line, "marker") || strings.Contains(line, "plugin") {
					fmt.Printf("LOG: %s\n", line)
				}
			}
		}
	})
	for _, script := range []string{"stop", "quiet"} {
		t.Run(script, func(t *testing.T) {
			fake := &fakeOpenAI{script: script}
			srv := httptest.NewServer(fake)
			defer srv.Close()
			cfgPath := opencode2FakeConfig(t, srv.URL)

			dir := t.TempDir()
			dataDir := t.TempDir()
			env := os.Environ()
			env = append(env, opencodeIsolateEnv(t)...)
			env = append(env, "XDG_DATA_HOME="+dataDir)
			env = append(env, "OPENCODE_CONFIG="+cfgPath)

			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "opencode2", "run", "--standalone", "--format", "json", "--model", "fake/fake-model", "--", conformancePrompt)
			cmd.Dir = dir
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			fmt.Printf("=== %s: err=%v exit=%d stderr=%s\n", script, err, cmd.ProcessState.ExitCode(), cmd.ProcessState.String())
			lines := strings.Split(string(out), "\n")
			for i, l := range lines {
				if strings.TrimSpace(l) == "" {
					continue
				}
				if len(l) > 600 {
					l = l[:600] + "…<TRUNC>"
				}
				fmt.Printf("[%d] %s\n", i, l)
			}
			// also show the session record
			rec := filepath.Join(dataDir, "opencode", "log", "opencode.log")
			if data, err := os.ReadFile(rec); err == nil {
				s := string(data)
				if len(s) > 2000 {
					s = s[:2000]
				}
				fmt.Printf("--- session record (head) ---\n%s\n", s)
			} else {
				fmt.Printf("--- no session record at %s: %v\n", rec, err)
			}
		})
	}
}