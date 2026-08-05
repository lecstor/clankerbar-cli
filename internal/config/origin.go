package config

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strings"
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

// credentialEnvVar is the environment variable holding the account-scoped key. An
// `.mcp.json` server entry that references it is, by construction, a place the key
// will be sent — which is what makes it this file's business.
const credentialEnvVar = "CLANKERBAR_API_KEY"

// credentialOrigin returns the scheme://host of raw if it is a place a bearer
// token may be sent, and an error naming the reason if it is not.
//
// The floor is TLS: `https` anywhere, or `http` only to loopback (where there is
// no network to eavesdrop on, and where a local plane under development lives).
// Anything else would put an account-scoped credential on the wire in cleartext.
func credentialOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%q is not a URL: %w", raw, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("%q has no scheme and host", raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !isLoopbackHost(u.Hostname()) {
			return "", fmt.Errorf("%q is plain http to a non-loopback host — the API key would go over the wire in cleartext; use https", raw)
		}
	default:
		return "", fmt.Errorf("%q has scheme %q; only https (or http to loopback) may carry the API key", raw, u.Scheme)
	}
	return strings.ToLower(u.Scheme) + "://" + u.Host, nil
}

// isLoopbackHost reports whether host is the local machine — 127.0.0.0/8, ::1, or
// the "localhost" name. Nothing else is resolved: a DNS lookup here would let an
// attacker-controlled name masquerade as loopback for exactly as long as the
// lookup says so, and the point of this check is that it cannot be talked out of.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// CredentialOrigin is the ONE origin this config may send CLANKERBAR_API_KEY to.
// It comes from `backlog_url` — the operator's own config file, or the built-in
// default — and never from a file inside the workdir. Validate has already checked
// it, so a bad one cannot reach here; "" means no origin is configured at all.
func (c *Config) CredentialOrigin() string {
	origin, err := credentialOrigin(c.BacklogURL)
	if err != nil {
		return ""
	}
	return origin
}

// mcpServer is one `mcpServers` entry of a Claude-shaped `.mcp.json`, reduced to
// what deciding "may the key go there" needs.
type mcpServer struct {
	name    string
	url     string
	usesKey bool // some header or env value references CLANKERBAR_API_KEY
}

// readMCPServers parses a Claude-shaped `.mcp.json` and returns its http(s)
// servers, sorted by name so a refusal names the same one on every run.
// Best-effort, exactly as before: an absent or unparseable file yields nothing.
//
// Best-effort is safe here BECAUSE nothing this returns is trusted with an
// origin. A file we fail to read contributes no slug and no server to check; it
// cannot contribute a destination, because destinations no longer come from here.
func readMCPServers(path string) []mcpServer {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(expandHome(path))
	if err != nil {
		return nil
	}
	var f struct {
		MCPServers map[string]struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return nil
	}
	out := make([]mcpServer, 0, len(f.MCPServers))
	for name, s := range f.MCPServers {
		if s.URL == "" {
			continue // a stdio server: no URL, so no origin to police
		}
		out = append(out, mcpServer{name: name, url: s.URL, usesKey: referencesKey(s.Headers) || referencesKey(s.Env)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
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

// checkMCPConfigOrigins refuses an `.mcp.json` that would send the API key
// somewhere this config does not trust.
//
// The driver's own requests are already safe — it builds every credentialed URL
// from CredentialOrigin — but the same file is handed to the harness
// (`--mcp-config`), whose session carries CLANKERBAR_API_KEY in its environment
// and will happily substitute it into whatever `Authorization` header the file
// declares. Policing only our own client would fix the smaller half of the leak.
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
	trusted := c.CredentialOrigin()
	for _, s := range readMCPServers(path) {
		if s.name != "clankerbar" && !s.usesKey {
			continue
		}
		origin, err := credentialOrigin(s.url)
		if err != nil {
			return fmt.Errorf("%s (%s): server %q: %w", label, path, s.name, err)
		}
		if trusted != "" && !strings.EqualFold(origin, trusted) {
			return fmt.Errorf(
				"%s (%s): server %q points at %s, but this config only sends %s to %s — set backlog_url to that origin if you mean it, or fix the file (a checkout's .mcp.json is not trusted to redirect an account-scoped key)",
				label, path, s.name, origin, credentialEnvVar, trusted,
			)
		}
	}
	return nil
}
