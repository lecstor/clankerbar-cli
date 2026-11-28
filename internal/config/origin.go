package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/secureurl"
)

// This file holds one rule: WHERE the operator's API key is allowed to go.

// credentialEnvVar is the environment variable holding the account-scoped key. A server entry that
// references it is, by construction, a place the key will be sent — which is what makes it this
// file's business.
const credentialEnvVar = "CLANKERBAR_API_KEY"

// CredentialOrigin is the ONE origin this config may send CLANKERBAR_API_KEY to. Validate has
// already checked it, so a bad one cannot reach here; "" means no origin is configured at all.
func (c *Config) CredentialOrigin() string {
	origin, err := secureurl.Origin(c.BacklogURL)
	if err != nil {
		return ""
	}
	return origin
}

// ResolveOrigin is the --host / config backlog_url / built-in plane ladder shared by the auth
// commands and the MCP bridge, so the credential-origin precedence exists once. load reads the
// config file the caller owns: Load("") refuses an implicit workdir config, while LoadHome is for
// a command (the bridge) launched inside a checkout. The result is canonical and credential-safe:
// https anywhere, or http to loopback only.
func ResolveOrigin(host string, load func() (*Config, error)) (string, error) {
	if h := strings.TrimSpace(host); h != "" {
		origin, err := secureurl.Origin(h)
		if err != nil {
			return "", fmt.Errorf("--host: %w", err)
		}
		return origin, nil
	}
	cfg, err := load()
	if err != nil {
		return "", fmt.Errorf("load config: %w (pass --host <origin> to skip it)", err)
	}
	raw := strings.TrimSpace(cfg.BacklogURL)
	if raw == "" {
		return defaultBacklogURL, nil
	}
	origin, err := secureurl.Origin(raw)
	if err != nil {
		return "", fmt.Errorf("the config's backlog_url cannot carry a credential (%w) - pass --host <origin>", err)
	}
	return origin, nil
}

// mcpServer is one server entry of a harness MCP config, reduced to what deciding "may the key go
// there" needs. A URL-less entry is a locally spawned process; it has no origin, but it can still
// be handed the key through its environment.
type mcpServer struct {
	name    string
	url     string
	usesKey bool   // some header or env value references CLANKERBAR_API_KEY
	command string // non-empty when the entry starts a LOCAL PROCESS, as written
}

// mcpFile is the union of the two schemas a harness MCP config can be in, because the same
// `MCPConfigPath` field serves every harness (ResolveMCPConfig decides which file a session is
// actually handed): - `mcpServers` — Claude Code's `.mcp.json`, passed as `--mcp-config`. Reading
// only the shape the current harness uses would let a workdir file carry a benign `mcpServers`
// block for `doctor` to report on and a hostile `mcp` block for the session to actually use — a
// decoy that reads green.
type mcpFile struct {
	MCPServers map[string]mcpEntry `json:"mcpServers"`
	MCP        map[string]mcpEntry `json:"mcp"`

	// The opencode-schema keys that decide what a session IS rather than what it talks to. Modelled
	// because OPENCODE_CONFIG makes the file the session's ENTIRE config (harness/opencode.go), so a
	// workdir-discovered file carrying any of these does not merely add servers — it overrides what
	// the driver pins: permission — replaces the fail-closed permission policy the adapter exports;
	// the CLA-260 threat with the safety rail removed; plugin — code opencode loads and runs at
	// startup; agent — replaces agent definitions, including their modes and per-agent permissions.
	Permission json.RawMessage `json:"permission"`
	Plugin     json.RawMessage `json:"plugin"`
	Agent      json.RawMessage `json:"agent"`
	// Agents and DefaultAgent are the 2.x spellings: `agents.<name>` (with a `permissions` list) and
	// the agent a session starts on.
	Agents       json.RawMessage `json:"agents"`
	DefaultAgent json.RawMessage `json:"default_agent"`
}

// UnmarshalJSON decodes the union mcpFile shape and additionally flattens the opencode 2.x
// `mcp.servers` dialect into MCP, so every consumer (the credential-origin gates, the local-process
// disclosure, the ambient-config audit, the slug derivation) sees an opencode 2.x config's servers
// exactly as a 1.x config's. The two dialects cannot collide on a real file: opencode 1.x writes
// `mcp` as a map of server-name -> entry, opencode 2.x writes it as an object with a `servers`
// sub-map.
func (f *mcpFile) UnmarshalJSON(data []byte) error {
	var raw struct {
		MCPServers map[string]mcpEntry `json:"mcpServers"`
		MCP        json.RawMessage     `json:"mcp"`
		Permission json.RawMessage     `json:"permission"`
		Plugin     json.RawMessage     `json:"plugin"`
		Agent      json.RawMessage     `json:"agent"`
		Agents     json.RawMessage     `json:"agents"`
		Default    json.RawMessage     `json:"default_agent"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	f.MCPServers = raw.MCPServers
	f.Permission, f.Plugin, f.Agent = raw.Permission, raw.Plugin, raw.Agent
	f.Agents, f.DefaultAgent = raw.Agents, raw.Default
	if len(raw.MCP) == 0 || string(raw.MCP) == "null" {
		return nil
	}
	var v1 map[string]mcpEntry
	if err := json.Unmarshal(raw.MCP, &v1); err != nil {
		return err
	}
	if e, ok := v1["servers"]; ok && len(v1) == 1 && entryIsZero(e) {
		var v2 struct {
			Servers map[string]mcpEntry `json:"servers"`
		}
		if err := json.Unmarshal(raw.MCP, &v2); err == nil {
			f.MCP = v2.Servers
			return nil
		}
	}
	f.MCP = v1
	return nil
}

// entryIsZero reports whether an mcpEntry decoded as all-zero — no URL, no header/env maps, no
// command/args, no enabled flag.
func entryIsZero(e mcpEntry) bool {
	return e.URL == "" && e.Headers == nil && e.Env == nil && e.Environment == nil &&
		e.Command == nil && e.Args == nil && e.Enabled == nil && e.Disabled == nil
}

type mcpEntry struct {
	URL         string            `json:"url"`
	Headers     map[string]string `json:"headers"`
	Env         map[string]string `json:"env"`         // Claude's spelling
	Environment map[string]string `json:"environment"` // opencode's spelling

	// Command is left RAW because the two dialects disagree on its type: Claude writes a string plus a
	// separate `args` array, opencode writes the whole argv as one array. Nothing here needs to run it
	// - only to report that the file starts a local process at all - so modelling either shape would
	// be precision this cannot use and a way to miss the other one.
	Command json.RawMessage `json:"command"`

	// Args is Claude's other half of the same thing. Reported alongside Command because the
	// interesting part of `bash -c "curl ...
	Args json.RawMessage `json:"args"`

	// Enabled is opencode's per-server switch. A POINTER because absent means enabled and `false`
	// means disabled, and a plain bool cannot tell those apart - which would read every entry of every
	// Claude-shaped file, where the key does not exist at all, as switched off.
	Enabled *bool `json:"enabled"`

	// Disabled is the 2.x spelling of the same switch, inverted: `"disabled": true` turns a server
	// off. Read only where Enabled is, for the same reason.
	Disabled *bool `json:"disabled"`
}

// disabled reports whether this entry is switched off by an explicit 1.x `"enabled": false` or 2.x
// `"disabled": true`.
func (e mcpEntry) disabled() bool {
	return (e.Enabled != nil && !*e.Enabled) || (e.Disabled != nil && *e.Disabled)
}

// startsProcess reports whether this entry would start a LOCAL PROCESS. An entry with only `args`
// and no `command` starts nothing, and an http/url entry is not a process.
func (e mcpEntry) startsProcess() bool { return rawSet(e.Command) }

// rawSet reports whether an optional raw-JSON key was present with a value other than null. An
// absent key decodes to nil; `"key": null` decodes to the literal bytes "null" — and Go's own
// optional types treat those two the same, so a check that split them would refuse configs Go
// itself accepts.
func rawSet(r json.RawMessage) bool {
	s := strings.TrimSpace(string(r))
	return s != "" && s != "null"
}

// readMCPFile parses a harness MCP config into the whole mcpFile model: both server blocks and the
// opencode-schema keys beside them.
func readMCPFile(path string) (mcpFile, error) {
	var f mcpFile
	// "none" is the opt-out word, never a file: a `none` in the driver's cwd must not be read as the
	// run's MCP config (MCPConfigNone).
	if path == "" || path == MCPConfigNone {
		return f, nil
	}
	data, err := os.ReadFile(expandHome(path))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return f, nil
		}
		return f, fmt.Errorf("%s: %w", path, err)
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return f, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// readMCPServers parses a harness MCP config and returns its server entries, sorted by name so a
// refusal names the same one on every run.
func readMCPServers(path string) ([]mcpServer, error) {
	f, err := readMCPFile(path)
	if err != nil {
		return nil, err
	}
	out := make([]mcpServer, 0, len(f.MCPServers)+len(f.MCP))
	for _, block := range []map[string]mcpEntry{f.MCPServers, f.MCP} {
		for name, s := range block {
			server := mcpServer{
				name:    name,
				url:     s.URL,
				usesKey: referencesKey(s.Headers) || referencesKey(s.Env) || referencesKey(s.Environment),
			}
			// Only an entry that would actually START something carries a command.
			if s.startsProcess() {
				server.command = strings.TrimSpace(strings.TrimSpace(string(s.Command)) + " " + strings.TrimSpace(string(s.Args)))
			}
			out = append(out, server)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// LocalMCPServer is one MCP entry that starts a LOCAL PROCESS in the session, named so `doctor` can
// say what a config would execute.
type LocalMCPServer struct {
	ConfigPath string // the file that declares it
	Name       string // the entry's key
	Command    string // its `command` value, verbatim as JSON
}

// LocalMCPServers lists every entry in the resolved MCP config(s) that spawns a local process
// rather than talking to an http server. A file the operator NAMED is disclosed only — local MCP
// servers are a normal, wanted thing to put behind an explicit config line, and this listing is
// what doctor shows.
func (c *Config) LocalMCPServers() []LocalMCPServer {
	seen := make(map[string]bool)
	var out []LocalMCPServer
	paths := []string{c.MCPConfigPath}
	// Every file a session could actually be pointed at, including the per-harness ones.
	for _, name := range sortedKeys(c.Harnesses) {
		paths = append(paths, c.Harnesses[name].MCPConfigPath)
	}
	for _, p := range c.Projects {
		paths = append(paths, p.MCPConfigPath)
		for _, name := range sortedKeys(p.MCPConfigPaths) {
			paths = append(paths, p.MCPConfigPaths[name])
		}
	}
	for _, path := range paths {
		if path == "" || path == MCPConfigNone || seen[path] {
			continue
		}
		seen[path] = true
		servers, err := readMCPServers(path)
		if err != nil {
			// Validate has already refused an unreadable config outright, so this is unreachable from a
			// validated config; reporting nothing is right either way, since a file that cannot be parsed
			// declares nothing we can name.
			continue
		}
		for _, s := range servers {
			if s.command == "" {
				continue
			}
			out = append(out, LocalMCPServer{ConfigPath: path, Name: s.name, Command: s.command})
		}
	}
	return out
}

func referencesKey(m map[string]string) bool {
	for _, v := range m {
		if strings.Contains(v, credentialEnvVar) {
			return true
		}
	}
	return false
}

// MCPClankerbar reports the usable clankerbar server the file at path declares: an entry named
// `clankerbar`, or one referencing `CLANKERBAR_API_KEY`, that is not explicitly `"enabled": false`.
func MCPClankerbar(path string) (url string, ok bool) {
	f, err := readMCPFile(path)
	if err != nil {
		return "", false
	}
	for _, block := range []map[string]mcpEntry{f.MCPServers, f.MCP} {
		for name, s := range block {
			if s.disabled() {
				continue
			}
			if name == "clankerbar" || referencesKey(s.Headers) || referencesKey(s.Env) || referencesKey(s.Environment) {
				return s.URL, true
			}
		}
	}
	return "", false
}

// checkMCPConfigOrigins refuses a harness MCP config that would send the API key somewhere this
// config does not trust. Policing only our own client would fix the smaller half of the leak.
func checkMCPConfigNamesClankerbar(path, label string) error {
	if path == "" || path == MCPConfigNone {
		return nil
	}
	fi, err := os.Stat(expandHome(path))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("%s: %w", label, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s: %s is not a regular file", label, path)
	}
	if _, ok := MCPClankerbar(path); !ok {
		return fmt.Errorf("%s: %s does not declare a usable clankerbar server — sessions would start with no clankerbar tools "+
			"(add an entry named \"clankerbar\", or one referencing %s, that is not \"enabled\": false)",
			label, path, credentialEnvVar)
	}
	return nil
}

func (c *Config) checkMCPConfigOrigins(path, label string) error {
	if path == "" || path == MCPConfigNone {
		// Nothing configured, or the explicit "none" opt-out: there is no file whose destinations could
		// be checked.
		return nil
	}
	servers, err := readMCPServers(path)
	if err != nil {
		return fmt.Errorf("%s: unreadable, so what it points the API key at cannot be checked: %w", label, err)
	}
	if len(servers) == 0 {
		return nil
	}
	trusted := c.CredentialOrigin()
	if trusted == "" {
		return fmt.Errorf("%s (%s): no trusted origin is configured, so nothing can be checked against it — set backlog_url", label, path)
	}
	for _, s := range servers {
		if s.name != "clankerbar" && !s.usesKey {
			continue
		}
		if s.url == "" {
			// A locally spawned server handed the account key: nothing constrains where that process sends
			// it, and under `--strict-mcp-config` this file is the session's whole MCP surface, so the
			// process starts before any permission policy has an opinion.
			return fmt.Errorf(
				"%s (%s): server %q is a local command handed %s — a spawned process may send it anywhere; give the key only to an http server on %s",
				label, path, s.name, credentialEnvVar, trusted,
			)
		}
		origin, err := secureurl.Origin(s.url)
		if err != nil {
			return fmt.Errorf("%s (%s): server %q: %w", label, path, s.name, err)
		}
		if !strings.EqualFold(origin, trusted) {
			return fmt.Errorf(
				"%s (%s): server %q points at %s, but this config only sends %s to %s — set backlog_url to that origin if you mean it, or fix the file (a checkout's .mcp.json is not trusted to redirect an account-scoped key)",
				label, path, s.name, origin, credentialEnvVar, trusted,
			)
		}
	}
	return nil
}
