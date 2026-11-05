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

// BacklogEndpoint returns the full, project-scoped MCP URL the driver's own
// backlog poll should hit — the same `/mcp/<project>` endpoint the harness uses.
//
// BacklogURL defaults to a bare base (`https://clankerbar.com`), which the plane
// rejects without the project slug in the path. The slug lives in the harness
// .mcp.json (MCPConfigPath), so we take it from there — but ONLY the slug. The
// origin always comes from CredentialOrigin, because this URL carries the API key
// (CLA-257): a file inside the workdir may say which project, never which host.
// An explicit backlog_url that already names a `/mcp/<project>` path wins outright.
//
// The raw field is read, not the harness-gated resolution (ResolveMCPConfig):
// since CLA-318 the file stays every run's slug source even when the running
// harness would not be handed it.
//
// Returns "" when no usable project-scoped endpoint can be resolved (a bare base
// and no slug). That is deliberate: New("") yields a not-wired poller, so the loop
// falls into blind drain — which still makes progress — rather than retrying
// forever against a slug-less base the plane can only reject.
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
	// A pre-CLA-99 file names a bare `/mcp` with no slug to lift out. Take its
	// path verbatim, but only once its origin is proven to BE the trusted one —
	// which Validate has already refused the file for otherwise. Dropping to ""
	// here instead would silently disable the driver's one write (the CLA-242
	// release-on-interrupt) for those setups: a security fix quietly deleting an
	// unrelated feature.
	if secureurl.SameOrigin(origin, raw) {
		return raw
	}
	return ""
}

// Slug returns the project slug this config's wiring names — the `<slug>` in a
// /mcp/<slug> endpoint or .mcp.json URL, "" when nothing derivable says. It is
// how single-project mode answers "which project is this", the same derivation
// the poll and the sessions already agree on.
func (c *Config) Slug() string { return slugFromMCPURL(c.BacklogEndpoint()) }

// ProjectEndpoint returns a project's MCP endpoint: `<origin>/mcp/<slug>`, the
// slug path-escaped. The origin is CredentialOrigin — `backlog_url`, the
// operator's own config, or the built-in default — never a host read off a
// checkout's .mcp.json (CLA-257).
//
// The slug need not be a project this config declares: propose-config --slug and
// upload --project may both name one it does not, so the write lands where the
// caller said rather than necessarily where this config's sessions are pointed.
// The plane membership-gates /mcp/<slug>, so an unreachable slug is refused
// there, never mapped onto a project here.
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

// BacklogSummaryURL returns the URL of the driver's cheap backlog read: the
// plane's `GET .../backlog-summary` surface, which returns {version, counts,
// claimable, openQuestions, loopPaused} in one authenticated call (counts to gate
// on, plus the console-driven pause the driver honours).
//
// Two forms exist (CLA-141), and which one we return decides which key kinds work:
//
//   - `/api/projects/<slug>/backlog-summary` — project named in the PATH, so the
//     operator's ACCOUNT key works (membership-gated, exactly like /mcp/<slug>);
//     a project key works too, for its own slug. Returned whenever a slug can be
//     derived from the resolved MCP endpoint (an .mcp.json naming /mcp/<slug>).
//   - `/api/backlog-summary` — the legacy slug-less route, where only a
//     project-scoped key can select the project. The fallback when no slug is
//     derivable, so pre-CLA-141 setups keep working unchanged.
//
// The origin is CredentialOrigin — `backlog_url`, the operator's own config, or
// the default base — and nothing else. Before CLA-257 it fell back to the origin
// named by the workdir's .mcp.json whenever backlog_url was left at its default,
// which is the normal setup, so a committed file in a cloned repo could redirect
// this credentialed GET to any host over plain http. A self-hosted plane is still
// reachable; it just has to be named in `backlog_url`. Only the SLUG still comes
// from .mcp.json, and it only chooses a path on an origin we already trust.
//
// Returns "" when no origin can be resolved (New("") then yields a not-wired,
// blind poller).
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

// ProjectSummaryURL returns the slug-ful summary URL for one configured project
// (CLA-142): `<origin>/api/projects/<slug>/backlog-summary`, on the same trusted
// CredentialOrigin as every other credentialed call.
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

// FleetReportURL returns the URL of the daemon's fleet presence beacon target
// (CLA-466): the plane's `POST .../fleet/report` surface, which takes presence
// plus any finished iteration records in one authenticated call.
//
// Unlike the summary route there is NO legacy slug-less form — the report
// endpoint exists only as `/api/projects/<slug>/fleet/report` (CLA-465). So a
// setup with no derivable slug has no fleet URL at all: reporting is off for
// that run (doctor says so), and the loop behaves exactly as it did before
// fleets existed. Same origin rule as every credentialed call: CredentialOrigin,
// never a path read off the workdir's .mcp.json.
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
// `<origin>/api/projects/<slug>/fleet/report`, on the same trusted
// CredentialOrigin as every other credentialed call.
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

// ResolveInstanceName composes the Fleet-page instance identity a daemon
// beacons under (CLA-466): an explicit instance_name wins verbatim; otherwise
// the default is the machine's hostname plus the config file's basename minus
// its .json suffix ("Jasons-MBP/clanker1"), because the config PATH is what
// actually distinguishes co-located daemons (CLA-501). The earlier
// bare-hostname fallback keyed every same-host daemon to one presence row,
// each beacon overwriting the last.
//
// The config-identity hash was considered and rejected as the discriminator:
// it fingerprints file CONTENT, and the live co-located configs hash identically.
//
// The composed default is truncated to MaxInstanceNameLen so it cannot exceed
// the plane's cap. Two paths identical within that prefix would still collide;
// that is pathological, and visible to doctor's sibling scan, which resolves
// identities through this same function.
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
		// Byte-truncate, but never split a multi-byte rune: a split rune would
		// marshal as U+FFFD (encoding/json replaces invalid UTF-8), corrupting
		// the very identity this function exists to pin - and two distinct long
		// basenames could even collapse onto one mangled prefix, re-creating the
		// collision CLA-501 exists to fix. Back off to a rune boundary instead.
		name = name[:MaxInstanceNameLen]
		for len(name) > 0 && !utf8.ValidString(name) {
			name = name[:len(name)-1]
		}
	}
	return name
}

// ResolvedInstanceName is ResolveInstanceName over this config's instance_name
// and source path.
func (c *Config) ResolvedInstanceName(hostname string) string {
	return ResolveInstanceName(c.InstanceName, c.source, hostname)
}

// Identity fingerprints the effective config in force — what the fleet presence
// beacon reports as `configIdentity` (CLA-466), so the console can tell two
// daemons apart when they disagree about how to run and can see a RELOAD land.
//
// It is a SHA-256 over the JSON encoding of the config AS VALIDATED (flag
// overrides applied), recomputed on demand rather than cached: a RELOAD swaps
// d.cfg mid-run, and the next beacon must carry the new identity. Only exported
// fields encode; no secret is in the config file by contract (the API key lives
// in CLANKERBAR_API_KEY). Map fields encode with sorted keys, so the fingerprint
// is stable across processes.
func (c *Config) Identity() string {
	b, err := json.Marshal(c)
	if err != nil {
		// Marshalling a Config cannot fail today (no channels/funcs/cycles); an
		// empty identity reads plane-side as "unknown" rather than lying.
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// slugFromMCPURL extracts the `<slug>` from an `/mcp/<slug>` MCP endpoint URL, or
// "" when the path is bare `/mcp` (a pre-CLA-99 project-key endpoint) or not an
// MCP path at all.
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

// mcpURLFromConfig reads a harness MCP config and returns the clankerbar MCP
// server URL, or "" if the file is absent or names no such server.
//
// Callers use this for the PATH — a project slug — never for an origin (see
// origin.go). The old "else the first http server's URL" fallback is gone with
// it: it made an unrelated MCP entry (a docs server, a browser driver) speak for
// clankerbar, and picked which one by map iteration order. What is left is the
// entry named `clankerbar`, else whichever entry is handed CLANKERBAR_API_KEY —
// the two ways a file can actually mean "this is the clankerbar server".
//
// A read/parse failure yields "", which is safe HERE and only here: Validate has
// already refused such a file outright (checkMCPConfigOrigins fails closed), so
// this is never reached with one.
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

// retiredHarnessHint names what replaced a harness that used to be registered,
// so a config written for it fails with the fix in the message rather than a
// bare "unknown harness". `opencode2` drove the 2.x preview; since CLA-675 the
// `opencode` harness drives 2.x itself and the preview adapter is gone. It is
// not kept as an alias: one name per adapter keeps the binary doctor looks up,
// the harnesses block, budgets and logs all agreeing.
func retiredHarnessHint(name string) string {
	if name == "opencode2" {
		return ` - "opencode2" was retired: the "opencode" harness now drives opencode 2.x; rename it (and any harnesses.opencode2 block) to "opencode"`
	}
	return ""
}
