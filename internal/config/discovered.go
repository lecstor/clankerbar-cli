package config

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// This file holds CLA-266's rule: a DISCOVERED MCP config may not silently decide what the next
// unattended session executes.

// checkDiscoveredMCPConfig refuses a workdir-discovered MCP config that decides what the next
// unattended session executes: any local-command server entry not named in `allow` (rule 1), or any
// opencode-schema policy key (rule 2, which no allowlist relaxes).
func (c *Config) checkDiscoveredMCPConfig(path, label string, allow []string) error {
	if path == "" {
		return nil
	}
	f, err := readMCPFile(path)
	if err != nil {
		return fmt.Errorf("%s (%s): unreadable, so what it would hand the next session cannot be checked: %w", label, path, err)
	}

	// Rule 2 first: a file overriding the adapter's pinned policy is not the document it was assumed
	// to be, whatever its server entries say, and no allowlist speaks to this — the allowlist names
	// servers, never policy.
	for _, k := range []struct {
		key  string
		raw  json.RawMessage
		what string
	}{
		// The mechanism behind this one, measured in 1.18.19.
		{"permission", f.Permission, "permission rules, which merge into the policy this driver exports and reorder it"},
		{"plugin", f.Plugin, "code loaded and run at startup"},
		{"agent", f.Agent, "agent definitions, including their modes and permissions"},
		{"agents", f.Agents, "agent definitions (2.x dialect), including their permissions"},
		{"default_agent", f.DefaultAgent, "the agent a session starts on"},
	} {
		if !rawSet(k.raw) {
			continue
		}
		return fmt.Errorf(
			"%s (%s): this discovered file sets %q, which under OPENCODE_CONFIG is the session's entire "+
				"config and would override %s. A <workdir>/.mcp.json found by default does not get to do "+
				"that; name the file explicitly (%s) if you mean it",
			label, path, k.key, k.what, label,
		)
	}

	// Rule 1: local-process entries, in either block, minus the names the operator allowlisted.
	allowed := make(map[string]bool, len(allow))
	for _, name := range allow {
		if name = strings.TrimSpace(name); name != "" {
			allowed[name] = true
		}
	}
	var processes []string
	for _, block := range []map[string]mcpEntry{f.MCPServers, f.MCP} {
		for name, s := range block {
			if s.startsProcess() && !allowed[name] {
				processes = append(processes, "\""+name+"\"")
			}
		}
	}
	sort.Strings(processes)
	if len(processes) > 0 {
		return fmt.Errorf(
			"%s (%s): this discovered file declares local-process MCP server(s) %s — a command entry starts "+
				"a process at session init, before any permission rule applies, running as you with the "+
				"session's whole environment (CLANKERBAR_API_KEY included), and this file was found by "+
				"default rather than named by you. List the entry under allow_local_mcp_servers if you mean "+
				"it, or name the file explicitly (%s) to adopt it whole",
			label, path, strings.Join(processes, ", "), label,
		)
	}
	return nil
}
