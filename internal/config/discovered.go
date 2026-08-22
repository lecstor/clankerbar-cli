package config

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// This file holds CLA-266's rule: a DISCOVERED MCP config may not silently
// decide what the next unattended session executes.
//
// `mcp_config_path` defaults to `<workdir>/.mcp.json` (discoverMCPConfig). That
// default points into a checkout — which the sessions themselves can write,
// since they run with edit permission in it, and which may have been cloned
// from anywhere — and the file is then handed to the harness WHOLE:
//
//   - under claude, as `--mcp-config --strict-mcp-config`, making it the
//     session's entire MCP surface;
//   - under opencode, as `OPENCODE_CONFIG`, making it the session's ENTIRE
//     configuration.
//
// Three refusals follow for a file nobody named:
//
//  1. An MCP entry carrying a `command` starts a process at MCP init — before
//     any tool-permission rule has an opinion. That is the reasoning origin.go
//     already states for a local server handed the API key; it applies whether
//     or not the process gets the key, because it inherits the session's whole
//     environment (CLANKERBAR_API_KEY among it) and runs as the operator.
//
//  2. Under opencode the same file may set top-level keys that decide what a
//     session IS rather than what it talks to: `permission` would override the
//     fail-closed policy the adapter pins (the CLA-260 threat with the safety
//     rail removed), `plugin` loads startup code, `agent` replaces agent and
//     per-agent permission definitions. A checkout silently deciding any of
//     those has no honest reading.
//
//  3. An UNREADABLE discovered file is refused outright, for the same reason
//     every other reader of this file fails closed: it is handed onward either
//     way, so "cannot be checked" cannot mean "accepted".
//
// The escape hatch is naming things, in two grades:
//
//   - `allow_local_mcp_servers` lists the ENTRY NAMES an operator means to run
//     from a discovered file — the CLA-310 shape: the safe state is the default,
//     the loose state is a visible, operator-owned config line doctor reports.
//     It admits those names and NOTHING ELSE, and never reaches rule 2.
//   - naming the FILE itself (`mcp_config_path`, per harness or per project)
//     adopts it wholesale — the operator's vetting statement, the same line
//     this package drew when clankerbar.json stopped being discovered from the
//     working directory (refuseImplicitWorkDirConfig).
//
// Both checks read BOTH schema blocks (`mcpServers` and `mcp`) regardless of
// which harness will run, for the reason mcpFile records: a file that shows one
// block to a gate and another to the harness is exactly the decoy CLA-257's own
// review caught.

// checkDiscoveredMCPConfig refuses a workdir-discovered MCP config that decides
// what the next unattended session executes: any local-command server entry not
// named in `allow` (rule 1), or any opencode-schema policy key (rule 2, which
// no allowlist relaxes).
//
// label names the config field in errors ("mcp_config_path",
// "projects[0].mcp_config_path"), matching checkMCPConfigOrigins, so the
// operator is told which line to add next to the place they see every other
// MCP-config refusal. allow is the entry-name allowlist resolved for the scope
// this path serves (AllowLocalMCPServersFor).
//
// It is called only where Validate has just FILLED the path by discovery, so
// the provenance is known at the call site and nothing has to be remembered
// between there and here.
func (c *Config) checkDiscoveredMCPConfig(path, label string, allow []string) error {
	if path == "" {
		return nil
	}
	f, err := readMCPFile(path)
	if err != nil {
		return fmt.Errorf("%s (%s): unreadable, so what it would hand the next session cannot be checked: %w", label, path, err)
	}

	// Rule 2 first: a file overriding the adapter's pinned policy is not the
	// document it was assumed to be, whatever its server entries say, and no
	// allowlist speaks to this — the allowlist names servers, never policy.
	for _, k := range []struct {
		key  string
		raw  json.RawMessage
		what string
	}{
		{"permission", f.Permission, "the fail-closed permission policy this driver's adapter exports"},
		{"plugin", f.Plugin, "code loaded and run at startup"},
		{"agent", f.Agent, "agent definitions, including their modes and permissions"},
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

	// Rule 1: local-process entries, in either block, minus the names the
	// operator allowlisted. Sorted so a file declaring several names the same
	// one on every run — an error message that changes between runs reads as a
	// flaky check, not a full one.
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
