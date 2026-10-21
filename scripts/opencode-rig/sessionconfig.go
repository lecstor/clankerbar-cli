package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// generatedContent returns the document the opencode adapter pins as
// OPENCODE_CONFIG_CONTENT for these inputs. It calls the CLI's own exported
// code, so the rig cannot drift from the fleet's permission policy, Code Mode
// rewrite or agent pins.
func generatedContent(mcpConfigPath, workdir string, readOnly bool) ([]byte, error) {
	return harness.OpencodeSessionConfig(mcpConfigPath, workdir, readOnly)
}

// sessionConfigCmd is the tiny entry point the rig (and a curious operator) can
// use to see exactly what a session would be handed:
//
//	go run ./scripts/opencode-rig session-config --mcp-config X --workdir Y
func sessionConfigCmd(args []string) int {
	fs := flagSet("session-config")
	mcpConfig := fs.String("mcp-config", "", "source opencode config (may be empty)")
	workdir := fs.String("workdir", "", "session workdir (permission scope)")
	readOnly := fs.Bool("read-only", false, "generate the read-only (probe) policy")
	if err := parseFlags(fs, args); err != nil {
		return 2
	}
	content, err := generatedContent(*mcpConfig, *workdir, *readOnly)
	if err != nil {
		fmt.Fprintf(os.Stderr, "opencode-rig: %v\n", err)
		return 1
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, content, "", "  "); err != nil {
		os.Stdout.Write(content)
		fmt.Fprintln(os.Stdout)
		return 0
	}
	pretty.WriteByte('\n')
	os.Stdout.Write(pretty.Bytes())
	return 0
}

// sourceConfigOpts is what buildSourceConfig needs to assemble the file the rig
// passes as OPENCODE_CONFIG (and from which the content layer is generated).
type sourceConfigOpts struct {
	// BasePath is the fleet's own config, if any. Its provider block (and any
	// other servers it declares) is kept; its clankerbar server is replaced.
	BasePath string
	// ProviderID is the provider parsed from --model ("commandcode").
	ProviderID string
	// ProxyBaseURL is the in-process proxy's base URL.
	ProxyBaseURL string
	// Self is the rig binary, re-exec'd by opencode as the stdio MCP server.
	Self string
	// StubArgs are the arguments that turn Self into the stub.
	StubArgs []string
}

// buildSourceConfig reads the base config (if any), points the provider's
// baseURL at the local proxy, and replaces the clankerbar MCP server with the
// rig's stdio stub. Everything else the base declares - reasoning effort, other
// providers, disabled servers - is left exactly as the fleet has it.
func buildSourceConfig(opt sourceConfigOpts) ([]byte, string, error) {
	doc := map[string]any{}
	if opt.BasePath != "" {
		data, err := os.ReadFile(opt.BasePath)
		if err != nil {
			return nil, "", err
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		// Keep numbers byte-faithful, as the adapter does for this same file.
		dec.UseNumber()
		if err := dec.Decode(&doc); err != nil {
			return nil, "", fmt.Errorf("%s: not a JSON object: %w", opt.BasePath, err)
		}
		if doc == nil {
			doc = map[string]any{}
		}
	}

	// Put the clankerbar server where the 2.x dialect expects it, and remove
	// either dialect's existing entry: the rig's session must talk to the stub,
	// never to the plane.
	mcp, _ := doc["mcp"].(map[string]any)
	if mcp == nil {
		mcp = map[string]any{}
	}
	servers, _ := mcp["servers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	delete(servers, "clankerbar")
	delete(mcp, "clankerbar")
	command := append([]string{opt.Self}, opt.StubArgs...)
	servers["clankerbar"] = map[string]any{
		"type":    "local",
		"command": command,
		"enabled": true,
	}
	mcp["servers"] = servers
	doc["mcp"] = mcp

	// Redirect only the provider under test. A provider the base config does
	// not declare can still be injected: opencode merges the layers, and an
	// options-only block adds the baseURL without disturbing the rest of the
	// provider definition. If that is not true for a provider, --upstream and a
	// full provider block in --mcp-config are the documented fallback.
	provider := map[string]any{}
	if raw, ok := doc["provider"].(map[string]any); ok {
		provider = raw
	}
	pid := opt.ProviderID
	if pid == "" {
		return nil, "", fmt.Errorf("no provider id")
	}
	entry, _ := provider[pid].(map[string]any)
	if entry == nil {
		entry = map[string]any{}
	}
	options, _ := entry["options"].(map[string]any)
	if options == nil {
		options = map[string]any{}
	}
	existing, _ := options["baseURL"].(string)
	options["baseURL"] = opt.ProxyBaseURL
	entry["options"] = options
	provider[pid] = entry
	doc["provider"] = provider

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, existing, err
	}
	return append(data, '\n'), existing, nil
}

// writeSourceConfig writes the generated source config beside the run's other
// artifacts and returns its path.
func writeSourceConfig(dir string, data []byte) (string, error) {
	path := filepath.Join(dir, "opencode-config.json")
	if err := writeCredentialFile(path, data); err != nil {
		return "", err
	}
	return path, nil
}

// writeCredentialFile writes a generated config artifact that may carry a
// provider key. os.WriteFile applies its mode only when it CREATES the file, so
// an artifact left behind by an earlier run (0644, or any other mode) is
// chmodded too: replacing it must not keep the old permissions.
func writeCredentialFile(path string, data []byte) error {
	const mode os.FileMode = 0o600
	if err := os.WriteFile(path, data, mode); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

// providerFromModel splits a `provider/model` id on the FIRST slash; model ids
// themselves contain slashes (commandcode/deepseek/deepseek-v4.1-flash).
func providerFromModel(model string) (provider, rest string, err error) {
	i := strings.Index(model, "/")
	if i <= 0 || i == len(model)-1 {
		return "", "", fmt.Errorf("--model must be provider/model (got %q)", model)
	}
	return model[:i], model[i+1:], nil
}
