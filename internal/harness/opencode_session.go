package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// opencodeSessionConfig is opencodeSessionConfigWithEnv with no environment: the
// path doctor and the unit tests use, and never a NoBacklogMCP session.
func opencodeSessionConfig(in Invocation) ([]byte, error) {
	return opencodeSessionConfigWithEnv(in, nil)
}

// opencodeSessionConfigWithEnv builds the document pinned as
// OPENCODE_CONFIG_CONTENT: the mcp config file's own keys, every MCP server out
// of Code Mode, and the fail-closed permission policy under `permission`.
//
// env is the environment built so far, BEFORE the content layer is appended, so
// the NoBacklogMCP switch resolves the same ambient layers opencode will load.
//
// The file is parsed as strict JSON with numbers kept as written (UseNumber): it
// feeds fail-closed gates, so a lenient parse would accept what config refused.
// Its own `permission` is replaced, not merged - the file also loads as an
// earlier OPENCODE_CONFIG layer, where its keys would sort ahead of `*`.
func opencodeSessionConfigWithEnv(in Invocation, env []string) ([]byte, error) {
	if in.NoBacklogMCP {
		// A declared OPENCODE_CONFIG_CONTENT replaces this document wholesale
		// (exec takes the last of a duplicate key), and with it the backlog
		// switch. Fail closed rather than hand a standalone review the tools it
		// exists to avoid.
		for _, kv := range in.Env {
			if strings.HasPrefix(kv, "OPENCODE_CONFIG_CONTENT=") {
				return nil, errors.New("a no-backlog session cannot run with a declared OPENCODE_CONFIG_CONTENT: it replaces the adapter's content layer, and with it the switch that keeps the backlog MCP server off")
			}
		}
		// The switch resolves the layers against the environment opencode will
		// SEE, so a caller-declared OPENCODE_CONFIG_DIR or OPENCODE_CONFIG (which
		// exec lets win) cannot name a layer the resolver never read. An
		// INHERITED content layer is dropped: this call is about to write the
		// one that replaces it.
		env = append(withoutEnvKey(env, "OPENCODE_CONFIG_CONTENT"), in.Env...)
	}
	doc := map[string]any{}
	if in.MCPConfigPath != "" {
		data, err := os.ReadFile(in.MCPConfigPath)
		if err != nil {
			return nil, fmt.Errorf("mcp config %s: %w", in.MCPConfigPath, err)
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if err := dec.Decode(&doc); err != nil {
			return nil, fmt.Errorf("mcp config %s: not a JSON object: %w", in.MCPConfigPath, err)
		}
		if doc == nil {
			doc = map[string]any{}
		}
	}
	// CLA-927: a standalone session that must not reach a backlog gets every
	// clankerbar server opencode would load switched off in THIS layer, which
	// opencode merges last. Without it a checkout's checked-in opencode.json
	// hands the review the operator's backlog tools (with a key in env) or fails
	// the preflight with needs_auth (without one).
	if in.NoBacklogMCP {
		decls, err := opencodeBacklogMCPDeclarations(in, env, true)
		if err != nil {
			return nil, err
		}
		disableBacklogMCPServers(doc, decls)
	}
	if raw, ok := doc["mcp"]; ok && raw != nil {
		source := "backlog MCP disable"
		if in.MCPConfigPath != "" {
			source = "mcp config " + in.MCPConfigPath
		}
		if err := opencodeMCPOutOfCodeMode(doc); err != nil {
			return nil, fmt.Errorf("%s: %w", source, err)
		}
	}
	policy := opencodePermissionPolicy(in.Probe, in.WorkDir, in.ExtraDirs)
	doc["permission"] = policy
	// Pin the same policy on every built-in agent.
	agents := map[string]any{}
	for _, name := range opencodePinnedAgents {
		agents[name] = map[string]any{"permission": policy}
	}
	doc["agent"] = agents
	delete(doc, "agents")
	delete(doc, "default_agent")
	// CLA-877: a hung provider stream must ERROR, not stay silent until the phase idle watchdog kills
	// the whole session.
	opencodeProviderTimeouts(in, doc)
	return json.Marshal(doc)
}

// OpencodeSessionConfig is the document the opencode adapter pins as OPENCODE_CONFIG_CONTENT for a
// session with these inputs.
func OpencodeSessionConfig(mcpConfigPath, workdir string, readOnly bool) ([]byte, error) {
	return opencodeSessionConfig(Invocation{MCPConfigPath: mcpConfigPath, WorkDir: workdir, Probe: readOnly})
}

// opencodePinnedAgents are the agents the session config pins the policy on: opencodeAgent, which
// every run selects explicitly, and the other agents 2.0.32 defines. The session's own policy
// denies the subagent tool, so the others are pinned as a second line, not because a session can
// reach them.
var opencodePinnedAgents = []string{opencodeAgent, "plan", "general", "explore"}

// opencodeAgent is the agent every run selects with --agent. Selecting it explicitly means an
// ambient `default_agent` cannot route the session to an agent whose permissions this adapter never
// pinned.
const opencodeAgent = "build"

// opencodeMCPOutOfCodeMode rewrites a config's `mcp` block in place so every server it declares is
// in the 2.x dialect (`mcp.servers.<name>`) with `codemode: false`. Why both halves are needed,
// verified against 2.0.32: - With Code Mode on (the 2.x default) a server's tools are reachable
// only from inside the `execute` tool, which this adapter denies, and whose stream would not show
// the claim anyway.
func opencodeMCPOutOfCodeMode(doc map[string]any) error {
	raw, ok := doc["mcp"]
	if !ok || raw == nil {
		return nil
	}
	mcp, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("`mcp` is not an object")
	}
	servers := map[string]any{}
	if rs, present := mcp["servers"]; present {
		s, ok := rs.(map[string]any)
		if !ok {
			return fmt.Errorf("`mcp.servers` is not an object")
		}
		servers = s
	}
	for name, v := range mcp {
		if name == "servers" {
			continue
		}
		entry, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if _, typed := entry["type"].(string); !typed {
			continue
		}
		if _, dup := servers[name]; dup {
			return fmt.Errorf("MCP server %q is declared both as `mcp.%s` and `mcp.servers.%s`", name, name, name)
		}
		if enabled, isBool := entry["enabled"].(bool); isBool {
			delete(entry, "enabled")
			if !enabled {
				entry["disabled"] = true
			}
		}
		servers[name] = entry
		delete(mcp, name)
	}
	for name, v := range servers {
		entry, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("`mcp.servers.%s` is not an object", name)
		}
		entry["codemode"] = false
	}
	mcp["servers"] = servers
	return nil
}
