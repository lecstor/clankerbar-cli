package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/lecstor/clankerbar-cli/internal/secureurl"
)

// Source is the file the config was loaded from ("" if none).
func (c *Config) Source() string { return c.source }

// BacklogEndpoint returns the full, project-scoped MCP URL the driver's own backlog poll should hit
// — the same `/mcp/<project>` endpoint the harness uses. The slug lives in the harness .mcp.json
// (MCPConfigPath), so we take it from there — but ONLY the slug.
func (c *Config) BacklogEndpoint() string {
	origin := c.CredentialOrigin()
	if origin == "" {
		return ""
	}
	if strings.Contains(c.BacklogURL, "/mcp/") {
		return c.BacklogURL
	}
	raw := mcpURLFromConfig(c.MCPConfigPath)
	if slug := slugFromMCPURL(raw); slug != "" {
		return mcpPath(origin, slug)
	}
	// A pre-CLA-99 file names a bare `/mcp` with no slug to lift out.
	if secureurl.SameOrigin(origin, raw) {
		return raw
	}
	return ""
}

// Slug returns the project slug this config's wiring names — the `<slug>` in a /mcp/<slug>
// endpoint or .mcp.json URL, "" when nothing derivable says.
func (c *Config) Slug() string { return slugFromMCPURL(c.BacklogEndpoint()) }

// ProjectEndpoint returns a project's MCP endpoint: `<origin>/mcp/<slug>`, the slug path-escaped.
// The origin is CredentialOrigin — `backlog_url`, the operator's own config, or the built-in
// default — never a host read off a checkout's .mcp.json.
func (c *Config) ProjectEndpoint(p Project) string {
	origin := c.CredentialOrigin()
	if origin == "" || p.Slug == "" {
		return c.BacklogEndpoint()
	}
	return mcpPath(origin, p.Slug)
}

func mcpPath(origin, slug string) string {
	return origin + "/mcp/" + url.PathEscape(slug)
}

// BacklogSummaryURL returns the URL of the driver's cheap backlog read: the plane's `GET
// .../backlog-summary` surface, which returns {version, counts, claimable, openQuestions,
// loopPaused} in one authenticated call (counts to gate on, plus the console-driven pause the
// driver honours). - `/api/backlog-summary` — the legacy slug-less route, where only a
// project-scoped key can select the project.
func (c *Config) BacklogSummaryURL() string {
	origin := c.CredentialOrigin()
	if origin == "" {
		return ""
	}
	if slug := slugFromMCPURL(c.BacklogEndpoint()); slug != "" {
		return projectSummaryPath(origin, slug)
	}
	return origin + "/api/backlog-summary"
}

// ProjectSummaryURL returns the slug-ful summary URL for one configured project:
// `<origin>/api/projects/<slug>/backlog-summary`, on the same trusted CredentialOrigin as every
// other credentialed call.
func (c *Config) ProjectSummaryURL(p Project) string {
	origin := c.CredentialOrigin()
	if origin == "" {
		return ""
	}
	return projectSummaryPath(origin, p.Slug)
}

func projectSummaryPath(origin, slug string) string {
	return origin + "/api/projects/" + url.PathEscape(slug) + "/backlog-summary"
}

// FleetReportURL returns the URL of the daemon's fleet presence beacon target: the plane's `POST
// .../fleet/report` surface, which takes presence plus any finished iteration records in one
// authenticated call. Unlike the summary route there is NO legacy slug-less form — the report
// endpoint exists only as `/api/projects/<slug>/fleet/report`.
func (c *Config) FleetReportURL() string {
	origin := c.CredentialOrigin()
	if origin == "" {
		return ""
	}
	if slug := slugFromMCPURL(c.BacklogEndpoint()); slug != "" {
		return projectFleetReportPath(origin, slug)
	}
	return ""
}

// ProjectFleetReportURL returns the fleet-report URL for one configured project:
// `<origin>/api/projects/<slug>/fleet/report`, on the same trusted CredentialOrigin as every other
// credentialed call.
func (c *Config) ProjectFleetReportURL(p Project) string {
	origin := c.CredentialOrigin()
	if origin == "" || p.Slug == "" {
		return ""
	}
	return projectFleetReportPath(origin, p.Slug)
}

func projectFleetReportPath(origin, slug string) string {
	return origin + "/api/projects/" + url.PathEscape(slug) + "/fleet/report"
}

// ConversationPendingURL is the planner loop's 2-second wake poll for the single-project config
// (studio-chat.md §4): the project named by the config's MCP wiring. Empty when no slug can be
// derived: the endpoint has no legacy slug-less form, exactly like FleetReportURL, and an
// underivable slug means this config cannot serve chat.
func (c *Config) ConversationPendingURL() string {
	origin := c.CredentialOrigin()
	if origin == "" {
		return ""
	}
	slug := c.Slug()
	if slug == "" {
		return ""
	}
	return projectConversationPendingPath(origin, slug)
}

// ProjectConversationPendingURL is the same poll for one configured project of a multi-project
// instance.
func (c *Config) ProjectConversationPendingURL(p Project) string {
	origin := c.CredentialOrigin()
	if origin == "" || p.Slug == "" {
		return ""
	}
	return projectConversationPendingPath(origin, p.Slug)
}

func projectConversationPendingPath(origin, slug string) string {
	return origin + "/api/projects/" + url.PathEscape(slug) + "/conversations/pending"
}

// ResolveInstanceName composes the Fleet-page instance identity a daemon beacons under: an explicit
// instance_name wins verbatim; otherwise the default is the machine's hostname plus the config
// file's basename minus its .json suffix ("Jasons-MBP/clanker1"), because the config PATH is what
// actually distinguishes co-located daemons. The composed default is truncated to
// MaxInstanceNameLen so it cannot exceed the plane's cap.
func ResolveInstanceName(instanceName, sourcePath, hostname string) string {
	if name := strings.TrimSpace(instanceName); name != "" {
		return name
	}
	base := ""
	if sourcePath != "" {
		base = strings.TrimSuffix(filepath.Base(sourcePath), ".json")
	}
	var name string
	switch {
	case hostname != "" && base != "":
		name = hostname + "/" + base
	case hostname != "":
		name = hostname // no config file in play  -  nothing better than the host
	default:
		name = base // hostname unreadable; the basename still disambiguates
	}
	if len(name) > MaxInstanceNameLen {
		// Byte-truncate, but never split a multi-byte rune: a split rune would marshal as U+FFFD
		// (encoding/json replaces invalid UTF-8), corrupting the very identity this function exists to
		// pin - and two distinct long basenames could even collapse onto one mangled prefix, re-creating
		// the collision CLA-501 exists to fix.
		name = name[:MaxInstanceNameLen]
		for len(name) > 0 && !utf8.ValidString(name) {
			name = name[:len(name)-1]
		}
	}
	return name
}

// ResolvedInstanceName is ResolveInstanceName over this config's instance_name and source path.
func (c *Config) ResolvedInstanceName(hostname string) string {
	return ResolveInstanceName(c.InstanceName, c.source, hostname)
}

// Identity fingerprints the effective config in force — what the fleet presence beacon reports as
// `configIdentity`, so the console can tell two daemons apart when they disagree about how to run
// and can see a RELOAD land. Only exported fields encode; no secret is in the config file by
// contract (the API key lives in CLANKERBAR_API_KEY).
func (c *Config) Identity() string {
	b, err := json.Marshal(c)
	if err != nil {
		// Marshalling a Config cannot fail today (no channels/funcs/cycles); an empty identity reads
		// plane-side as "unknown" rather than lying.
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// slugFromMCPURL extracts the `<slug>` from an `/mcp/<slug>` MCP endpoint URL, or "" when the path
// is bare `/mcp` or not an MCP path at all.
func slugFromMCPURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 2 && parts[0] == "mcp" && parts[1] != "" {
		return parts[1]
	}
	return ""
}

// mcpURLFromConfig reads a harness MCP config and returns the clankerbar MCP server URL, or "" if
// the file is absent or names no such server. Callers use this for the PATH — a project slug —
// never for an origin (see origin.go).
func mcpURLFromConfig(path string) string {
	servers, err := readMCPServers(path)
	if err != nil {
		return ""
	}
	for _, s := range servers {
		if s.name == "clankerbar" {
			return s.url
		}
	}
	for _, s := range servers {
		if s.usesKey {
			return s.url
		}
	}
	return ""
}

// retiredHarnessHint names what replaced a harness that used to be registered, so a config written
// for it fails with the fix in the message rather than a bare "unknown harness".
func retiredHarnessHint(name string) string {
	if name == "opencode2" {
		return ` - "opencode2" was retired: the "opencode" harness now drives opencode 2.x; rename it (and any harnesses.opencode2 block) to "opencode"`
	}
	return ""
}
