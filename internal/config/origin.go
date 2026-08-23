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
//
// CLANKERBAR_API_KEY is an ACCOUNT-scoped bearer token — it covers every project
// the operator is a member of, not just the one being drained. Before CLA-257 its
// destination was derived from `<workdir>/.mcp.json`: a file that lives inside the
// checkout the loop is pointed at, that repos legitimately commit (this one does),
// and that named any scheme and any host it liked. Cloning a repo and pointing
// `workdir` at it — the documented usage — was enough to ship the key to a third
// party in cleartext, and `doctor` reported PASS for any host that answered 200.
//
// So the credential origin is now taken ONLY from the operator's own config
// (`backlog_url`, default `https://clankerbar.com`), and a discovered `.mcp.json`
// contributes at most a project SLUG. A self-hosted plane is still supported; it
// just has to be stated in the operator's config file rather than inferred from a
// file inside a checkout.

// credentialEnvVar is the environment variable holding the account-scoped key. A
// server entry that references it is, by construction, a place the key will be
// sent — which is what makes it this file's business. Both substitution dialects
// spell it the same inside their braces (Claude's `${VAR}`, opencode's `{env:VAR}`),
// so a substring match catches either.
const credentialEnvVar = "CLANKERBAR_API_KEY"

// CredentialOrigin is the ONE origin this config may send CLANKERBAR_API_KEY to.
// It comes from `backlog_url` — the operator's own config file, or the built-in
// default — and never from a file inside the workdir. Validate has already checked
// it, so a bad one cannot reach here; "" means no origin is configured at all.
func (c *Config) CredentialOrigin() string {
	origin, err := secureurl.Origin(c.BacklogURL)
	if err != nil {
		return ""
	}
	return origin
}

// mcpServer is one server entry of a harness MCP config, reduced to what deciding
// "may the key go there" needs. A URL-less entry is a locally spawned process; it
// has no origin, but it can still be handed the key through its environment.
type mcpServer struct {
	name    string
	url     string
	usesKey bool   // some header or env value references CLANKERBAR_API_KEY
	command string // non-empty when the entry starts a LOCAL PROCESS, as written
}

// mcpFile is the union of the two schemas a harness MCP config can be in, because
// the same `MCPConfigPath` is handed to whichever harness is configured:
//
//   - `mcpServers` — Claude Code's `.mcp.json`, passed as `--mcp-config`.
//   - `mcp` — opencode's config, exported as `OPENCODE_CONFIG` (harness/opencode.go).
//
// Decoding both from one file is deliberate. Reading only the shape the current
// harness uses would let a workdir file carry a benign `mcpServers` block for
// `doctor` to report on and a hostile `mcp` block for the session to actually
// use — a decoy that reads green.
type mcpFile struct {
	MCPServers map[string]mcpEntry `json:"mcpServers"`
	MCP        map[string]mcpEntry `json:"mcp"`

	// The opencode-schema keys that decide what a session IS rather than what it
	// talks to. Modelled because OPENCODE_CONFIG makes the file the session's
	// ENTIRE config (harness/opencode.go), so a workdir-discovered file carrying
	// any of these does not merely add servers — it overrides what the driver
	// pins (CLA-266):
	//
	//   permission — replaces the fail-closed permission policy the adapter
	//                exports; the CLA-260 threat with the safety rail removed;
	//   plugin     — code opencode loads and runs at startup;
	//   agent      — replaces agent definitions, including their modes and
	//                per-agent permissions.
	//
	// Left RAW because "present at all" is the only fact this needs: every value
	// shape counts (object, array, bool, string), and only an absent key or a
	// JSON null means unset — the same reading Go's own optional types give them.
	Permission json.RawMessage `json:"permission"`
	Plugin     json.RawMessage `json:"plugin"`
	Agent      json.RawMessage `json:"agent"`
}

type mcpEntry struct {
	URL         string            `json:"url"`
	Headers     map[string]string `json:"headers"`
	Env         map[string]string `json:"env"`         // Claude's spelling
	Environment map[string]string `json:"environment"` // opencode's spelling

	// Command is left RAW because the two dialects disagree on its type: Claude
	// writes a string plus a separate `args` array, opencode writes the whole
	// argv as one array. Nothing here needs to run it - only to report that the
	// file starts a local process at all - so modelling either shape would be
	// precision this cannot use and a way to miss the other one.
	Command json.RawMessage `json:"command"`

	// Args is Claude's other half of the same thing. Reported alongside Command
	// because the interesting part of `bash -c "curl ... | sh"` is entirely in
	// here - naming the entry as running "bash" would be true and useless.
	Args json.RawMessage `json:"args"`
}

// startsProcess reports whether this entry would start a LOCAL PROCESS. A
// `command` is what does it in both dialects — Claude's string-plus-args form
// and opencode's argv-array form both land here as raw JSON — so presence of
// the key with any value other than null is the test. An entry with only `args`
// and no `command` starts nothing, and an http/url entry is not a process.
func (e mcpEntry) startsProcess() bool { return rawSet(e.Command) }

// rawSet reports whether an optional raw-JSON key was present with a value
// other than null. An absent key decodes to nil; `"key": null` decodes to the
// literal bytes "null" — and Go's own optional types treat those two the same,
// so a check that split them would refuse configs Go itself accepts.
func rawSet(r json.RawMessage) bool {
	s := strings.TrimSpace(string(r))
	return s != "" && s != "null"
}

// readMCPFile parses a harness MCP config into the whole mcpFile model: both
// server blocks and the opencode-schema keys beside them.
//
// It FAILS CLOSED like readMCPServers: this feeds security gates, and the same
// file is handed onward to the harness, so "I could not read it" must not read
// as "there is nothing in it to object to". An absent file is the one benign
// case — there is then nothing to hand over either.
func readMCPFile(path string) (mcpFile, error) {
	var f mcpFile
	if path == "" {
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

// readMCPServers parses a harness MCP config and returns its server entries,
// sorted by name so a refusal names the same one on every run.
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
			// Raw "null", and args-without-command, used to be reported as a
			// process (doctor named an entry whose command read "null --serve"),
			// which made this disclosure disagree with startsProcess — the same
			// predicate CLA-266's gate applies to the very same entry. Disclosure
			// and gate must answer "does this start a process" identically: a WARN
			// listing entries that never run trains the operator to skim it.
			if s.startsProcess() {
				server.command = strings.TrimSpace(strings.TrimSpace(string(s.Command)) + " " + strings.TrimSpace(string(s.Args)))
			}
			out = append(out, server)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// LocalMCPServer is one MCP entry that starts a LOCAL PROCESS in the session,
// named so `doctor` can say what a config would execute.
type LocalMCPServer struct {
	ConfigPath string // the file that declares it
	Name       string // the entry's key
	Command    string // its `command` value, verbatim as JSON
}

// LocalMCPServers lists every entry in the resolved MCP config(s) that spawns a
// local process rather than talking to an http server.
//
// Why this is worth surfacing (CLA-260): `mcp_config_path` defaults to
// `<workdir>/.mcp.json`, the file is handed to the harness whole
// (`--mcp-config`, or as opencode's entire `OPENCODE_CONFIG`), and an entry
// carrying a `command` is started at MCP init - BEFORE any tool-permission rule
// has an opinion, which is the same reasoning checkMCPConfigOrigins already
// applies to a local server handed the API key. checkMCPConfigOrigins leaves
// entries that neither are named `clankerbar` nor reference the key alone, and
// that is still the right call for an ORIGIN check, but it means a checkout can
// declare a process the next unattended session will run.
//
// What happens to them depends on how the file was found (CLA-266). A file the
// operator NAMED is disclosed only — local MCP servers are a normal, wanted
// thing to put behind an explicit config line, and this listing is what doctor
// shows. A DISCOVERED one (Validate defaulting mcp_config_path to
// `<workdir>/.mcp.json`) is refused outright by checkDiscoveredMCPConfig before
// any session can read it, so from a validated config onward everything this
// returns lives in a file the operator pointed at.
func (c *Config) LocalMCPServers() []LocalMCPServer {
	seen := make(map[string]bool)
	var out []LocalMCPServer
	paths := []string{c.MCPConfigPath}
	// Every file a session could actually be pointed at, including the
	// per-harness ones (CLA-366). A mixed-harness run's other sessions read a
	// DIFFERENT file, and this check's subject — a server that starts a local
	// process at MCP init, before any tool-permission rule applies — is as true
	// there. Sorted so the disclosure lists them in the same order every run.
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
			// Validate has already refused an unreadable config outright, so this is
			// unreachable from a validated config; reporting nothing is right either
			// way, since a file that cannot be parsed declares nothing we can name.
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

// checkMCPConfigOrigins refuses a harness MCP config that would send the API key
// somewhere this config does not trust.
//
// The driver's own requests are already safe — it builds every credentialed URL
// from CredentialOrigin — but the same file is handed to the harness
// (`--mcp-config`, or `OPENCODE_CONFIG`), whose session carries
// CLANKERBAR_API_KEY in its environment and will happily substitute it into
// whatever `Authorization` header the file declares. Policing only our own client
// would fix the smaller half of the leak.
//
// Only entries that can carry the key are constrained: the one named `clankerbar`
// (the documented wiring) and any whose headers or env reference the variable. An
// unrelated MCP server in the same file — a docs server, a browser driver — is
// none of our business and is left alone.
//
// A refusal, not a silent drop: dropping the file would spawn a session with no
// clankerbar tools at all, which burns an iteration and reads as "the backlog was
// empty". The operator gets a named host and a remedy instead.
func (c *Config) checkMCPConfigOrigins(path, label string) error {
	if path == "" || path == MCPConfigNone {
		// Nothing configured, or the explicit "none" opt-out (CLA-318): there is
		// no file whose destinations could be checked.
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
			// A locally spawned server handed the account key: nothing constrains
			// where that process sends it, and under `--strict-mcp-config` this file
			// is the session's whole MCP surface, so the process starts before any
			// permission policy has an opinion.
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
