package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testBaseConfig = `{
  "$comment": "a base config shaped like the fleet's opencode-mcp.json",
  "mcp": {
    "clankerbar": {"type": "remote", "url": "https://clankerbar.com/mcp/clankerbar", "enabled": true},
    "servers": {"other": {"type": "local", "command": ["other-mcp"]}}
  },
  "provider": {
    "commandcode": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Command Code",
      "options": {"baseURL": "https://api.commandcode.ai/provider/v1", "apiKey": "{env:COMMANDCODE_API_KEY}"},
      "models": {"deepseek/deepseek-v4.1-flash": {"options": {"reasoningEffort": "max"}}}
    }
  }
}`

func TestBuildSourceConfigRedirectsProviderAndReplacesClankerbar(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.json")
	if err := os.WriteFile(base, []byte(testBaseConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	data, replaced, err := buildSourceConfig(sourceConfigOpts{
		BasePath:     base,
		ProviderID:   "commandcode",
		ProxyBaseURL: "http://127.0.0.1:12345",
		Self:         "/tmp/rig",
		StubArgs:     []string{"stub", "--scenario", "padding"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if replaced != "https://api.commandcode.ai/provider/v1" {
		t.Errorf("replaced upstream = %q", replaced)
	}
	var doc struct {
		MCP map[string]any `json:"mcp"`
	}
	var full map[string]any
	if err := json.Unmarshal(data, &full); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(data, &doc)
	servers := doc.MCP["servers"].(map[string]any)
	clankerbar := servers["clankerbar"].(map[string]any)
	if clankerbar["type"] != "local" {
		t.Errorf("clankerbar type = %v, want local (the stub)", clankerbar["type"])
	}
	cmd := clankerbar["command"].([]any)
	if cmd[0] != "/tmp/rig" || cmd[1] != "stub" || cmd[3] != "padding" {
		t.Errorf("stub command = %v", cmd)
	}
	if _, still1x := doc.MCP["clankerbar"]; still1x {
		t.Error("the 1.x clankerbar entry survived; the session could still reach the plane")
	}
	if _, ok := servers["other"]; !ok {
		t.Error("an unrelated server was dropped")
	}
	provider := full["provider"].(map[string]any)["commandcode"].(map[string]any)
	options := provider["options"].(map[string]any)
	if options["baseURL"] != "http://127.0.0.1:12345" {
		t.Errorf("baseURL = %v", options["baseURL"])
	}
	if options["apiKey"] != "{env:COMMANDCODE_API_KEY}" {
		t.Errorf("apiKey was disturbed: %v", options["apiKey"])
	}
	models := provider["models"].(map[string]any)["deepseek/deepseek-v4.1-flash"].(map[string]any)
	if models["options"].(map[string]any)["reasoningEffort"] != "max" {
		t.Errorf("reasoningEffort was disturbed: %v", models)
	}
}

func TestGeneratedContentIsTheAdaptersAndRebuildsTheMCP(t *testing.T) {
	dir := t.TempDir()
	workdir := filepath.Join(dir, "sandbox")
	if err := os.Mkdir(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(dir, "base.json")
	if err := os.WriteFile(base, []byte(testBaseConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	data, _, err := buildSourceConfig(sourceConfigOpts{
		BasePath: base, ProviderID: "commandcode", ProxyBaseURL: "http://127.0.0.1:1", Self: "/tmp/rig",
	})
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "source.json")
	if err := os.WriteFile(source, data, 0o644); err != nil {
		t.Fatal(err)
	}
	content, err := generatedContent(source, workdir, false)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(content, &doc); err != nil {
		t.Fatal(err)
	}
	servers := doc["mcp"].(map[string]any)["servers"].(map[string]any)
	clankerbar := servers["clankerbar"].(map[string]any)
	if clankerbar["codemode"] != false {
		t.Errorf("clankerbar codemode = %v, want false (Code Mode hides the tools)", clankerbar["codemode"])
	}
	perm := doc["permission"].(map[string]any)
	if perm["*"] != "deny" {
		t.Errorf("permission[*] = %v, want deny", perm["*"])
	}
	if perm["*_*"] != "allow" {
		t.Errorf("permission[*_*] = %v, want allow (MCP tools must survive the catch-all)", perm["*_*"])
	}
	agent := doc["agent"].(map[string]any)["build"].(map[string]any)
	if agent["permission"] == nil {
		t.Error("the build agent was not pinned")
	}
	for _, gone := range []string{"agents", "default_agent"} {
		if _, ok := doc[gone]; ok {
			t.Errorf("%s survived the content layer", gone)
		}
	}
	// The provider redirect rides through the content layer too, because the
	// content layer is what opencode merges last.
	provider := doc["provider"].(map[string]any)["commandcode"].(map[string]any)
	if provider["options"].(map[string]any)["baseURL"] != "http://127.0.0.1:1" {
		t.Errorf("content-layer baseURL = %v", provider["options"].(map[string]any)["baseURL"])
	}
}

func TestProviderFromModel(t *testing.T) {
	p, rest, err := providerFromModel("commandcode/deepseek/deepseek-v4.1-flash")
	if err != nil || p != "commandcode" || rest != "deepseek/deepseek-v4.1-flash" {
		t.Fatalf("got %q %q %v", p, rest, err)
	}
	for _, bad := range []string{"", "nomodel", "/x", "x/"} {
		if _, _, err := providerFromModel(bad); err == nil {
			t.Errorf("providerFromModel(%q) did not error", bad)
		}
	}
}

func TestProviderBaseURLLooksInTheConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(testBaseConfig), 0o644)
	got, err := providerBaseURL(path, "commandcode")
	if err != nil || got != "https://api.commandcode.ai/provider/v1" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := providerBaseURL(path, "openrouter"); err == nil {
		t.Error("an undeclared provider with no baseURL did not error")
	}
}

func TestMergeEnvReplacesAnExistingKeyInsteadOfDuplicatingIt(t *testing.T) {
	base := []string{"A=1", "B=2", "A=3"}
	got := mergeEnv(base, []string{"A=9", "C=4"})
	want := []string{"A=9", "B=2", "C=4"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("mergeEnv = %v, want %v", got, want)
	}
}

func TestSessionEnvPinsTheFleetLayersAndIsolation(t *testing.T) {
	o := runOptions{Out: "/runs/x", Workdir: "/runs/x/sandbox", ConfigDir: "~/.config/opencode"}
	env := sessionEnv(o, []byte(`{"mcp":{}}`), []string{"XDG_DATA_HOME=/runs/x/xdg/data"}, []string{"K=V"})
	vals := map[string]string{}
	for _, kv := range env {
		i := strings.Index(kv, "=")
		vals[kv[:i]] = kv[i+1:]
	}
	if vals["PWD"] != "/runs/x/sandbox" {
		t.Errorf("PWD = %q", vals["PWD"])
	}
	if vals["OPENCODE_CONFIG"] != "/runs/x/opencode-config.json" {
		t.Errorf("OPENCODE_CONFIG = %q", vals["OPENCODE_CONFIG"])
	}
	if vals["OPENCODE_CONFIG_CONTENT"] != `{"mcp":{}}` {
		t.Errorf("OPENCODE_CONFIG_CONTENT = %q", vals["OPENCODE_CONFIG_CONTENT"])
	}
	if vals["XDG_DATA_HOME"] != "/runs/x/xdg/data" {
		t.Errorf("XDG_DATA_HOME = %q", vals["XDG_DATA_HOME"])
	}
	if vals["K"] != "V" {
		t.Errorf("extra env lost")
	}
	if vals["OPENCODE_PASSWORD"] != "" {
		t.Error("an inherited OPENCODE_PASSWORD was not cleared for the server")
	}
}
