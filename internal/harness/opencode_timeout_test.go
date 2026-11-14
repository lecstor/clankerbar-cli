package harness

// CLA-877: the provider stream timeout the adapter pins into the materialised
// opencode config.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// opencodeProviderChunkTimeout reaches the session config as
// `providers.<id>.settings.chunkTimeout`, in the 2.x native dialect, for every
// provider the invocation's routes name.
func TestOpencodeSessionConfigCarriesTheProviderChunkTimeout(t *testing.T) {
	in := Invocation{
		Model: "opencode-go/deepseek-v4.1-flash",
		Routes: []string{
			"opencode-go/deepseek-v4.1-flash",
			"opencode/deepseek-v4.1-flash-free",
		},
	}
	data, err := opencodeSessionConfig(in)
	if err != nil {
		t.Fatalf("opencodeSessionConfig: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	providers, _ := doc["providers"].(map[string]any)
	if providers == nil {
		t.Fatal("the session config carries no `providers` block")
	}
	want := float64(opencodeProviderChunkTimeout.Milliseconds())
	for _, id := range []string{"opencode-go", "opencode"} {
		entry, _ := providers[id].(map[string]any)
		if entry == nil {
			t.Errorf("providers.%s missing", id)
			continue
		}
		settings, _ := entry["settings"].(map[string]any)
		if got := settings["chunkTimeout"]; got != want {
			t.Errorf("providers.%s.settings.chunkTimeout = %v, want %v", id, got, want)
		}
	}
}

// The pin MERGES into an entry an MCP config already carries: the operator's
// name/settings (an apiKey lives under settings in the 2.x dialect) survive.
func TestOpencodeProviderTimeoutMergesIntoAnExistingProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	doc := `{
	  "providers": {
	    "opencode-go": {"name": "Go", "settings": {"timeout": 1200000, "chunkTimeout": 900000}},
	    "other": {"settings": {"chunkTimeout": 12345}}
	  }
	}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := opencodeSessionConfig(Invocation{MCPConfigPath: path, Model: "opencode-go/deepseek-v4.1-flash"})
	if err != nil {
		t.Fatalf("opencodeSessionConfig: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	providers := got["providers"].(map[string]any)
	entry := providers["opencode-go"].(map[string]any)
	if entry["name"] != "Go" {
		t.Errorf("providers.opencode-go.name = %v, want the file's name preserved", entry["name"])
	}
	settings := entry["settings"].(map[string]any)
	if settings["timeout"] != float64(1200000) {
		t.Errorf("providers.opencode-go.settings.timeout = %v, want the file's value preserved", settings["timeout"])
	}
	if settings["chunkTimeout"] != float64(opencodeProviderChunkTimeout.Milliseconds()) {
		t.Errorf("providers.opencode-go.settings.chunkTimeout = %v, want the adapter's bound", settings["chunkTimeout"])
	}
	// A provider this invocation does not name is untouched.
	other := providers["other"].(map[string]any)["settings"].(map[string]any)
	if other["chunkTimeout"] != float64(12345) {
		t.Errorf("providers.other.settings.chunkTimeout = %v, want the file's value", other["chunkTimeout"])
	}
}

// No model route names no provider: the pin is skipped rather than guessing.
func TestOpencodeProviderTimeoutSkipsAnUnnamedProvider(t *testing.T) {
	data, err := opencodeSessionConfig(Invocation{})
	if err != nil {
		t.Fatalf("opencodeSessionConfig: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := doc["providers"]; ok {
		t.Error("the session config invented a providers block with no model route to name")
	}
}

// The production bound must stay inside the phase idle watchdog's 15 minutes,
// which is the whole point: opencode errors before the runner kills the phase.
func TestOpencodeProviderChunkTimeoutIsUnderTheIdleWatchdog(t *testing.T) {
	if opencodeProviderChunkTimeout <= 0 || opencodeProviderChunkTimeout >= 15*time.Minute {
		t.Fatalf("opencodeProviderChunkTimeout = %s, want a bound under the 15m idle watchdog", opencodeProviderChunkTimeout)
	}
}
