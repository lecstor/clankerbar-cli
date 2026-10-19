package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// The opencode 2.x doctor checks (CLA-675): the binary is 2.x, a credential is
// stored, and the posture the adapter pins on every session is fail-closed with
// every MCP server out of Code Mode.

// opencodeShellGapNote is the known, accepted gap doctor states beside the
// permission posture. It is information, never a failure: the operator accepted
// it on 2026-10-18 (CLA-675).
const opencodeShellGapNote = "known, accepted gap: opencode 2.x does not path-gate the shell tool - only its `workdir` argument hits external_directory, so a shell command can touch paths outside the workdir (accepted 2026-10-18: unattended sessions can already run arbitrary commands inside the workdir)"

var opencodeVersionRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// checkOpencodeVersion fails a 1.x binary. The adapter speaks only the 2.x
// dialect: a 1.x `opencode run` rejects --standalone, ignores the content
// layer's codemode keys, and reads its policy from the env var 2.x dropped.
func checkOpencodeVersion(c *check, ver string) {
	m := opencodeVersionRe.FindStringSubmatch(ver)
	if m == nil {
		c.status = warn
		c.remedy = "could not read a version from `opencode --version`; the adapter needs opencode 2.0 or newer"
		return
	}
	if major, _ := strconv.Atoi(m[1]); major < 2 {
		c.status = fail
		c.detail += " - opencode 1.x is no longer supported"
		c.remedy = "install opencode 2.x (the official installer puts it at ~/.opencode/bin/opencode) and put it first on PATH"
	}
}

// opencodeDataDir is where 2.x keeps opencode.db: $XDG_DATA_HOME/opencode, or
// ~/.local/share/opencode.
func opencodeDataDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "opencode")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "opencode")
}

// sqliteOpencodeCredentials counts the ACTIVE rows of opencode.db's credential
// table per integration. It selects the integration id, the `active` flag and a
// count only: no column holding a secret is ever read. The database is opened
// read-only. A row whose `active` is 0 is not counted; NULL counts as active.
// The table records no expiry, so a counted credential may still be expired or
// revoked at the provider - presence is not validity, and the check says so.
func sqliteOpencodeCredentials(ctx context.Context, dbPath string) (map[string]int, error) {
	sq, err := exec.LookPath("sqlite3")
	if err != nil {
		return nil, fmt.Errorf("sqlite3 not on PATH")
	}
	ctx, cancel := context.WithTimeout(ctx, binVersionTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, sq, "-readonly", "-separator", "\t", dbPath,
		"select coalesce(integration_id, ''), count(*) from credential where coalesce(active, 1) != 0 group by 1").Output()
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		name, n, _ := strings.Cut(line, "\t")
		v, _ := strconv.Atoi(n)
		if name == "" {
			name = "(unnamed)"
		}
		counts[name] += v
	}
	return counts, nil
}

// modelProvider is the provider a model id names: opencode ids are
// `<provider>/<model>` ("opencode-go/deepseek-v4-flash"). ok is false for an id
// with no provider half - a bare alias names nothing this can look up in the
// credential store, and guessing a provider from it would invent a finding.
func modelProvider(id string) (provider string, ok bool) {
	p, _, found := strings.Cut(id, "/")
	p = strings.TrimSpace(p)
	if !found || p == "" {
		return "", false
	}
	return p, true
}

// checkOpencodeAuth reports on 2.x's credential store when the run spawns
// opencode. A missing store or an empty one is a WARN, not a FAIL: a provider
// key can also come from the provider config (`{env:...}`), which this cannot
// see.
//
// A NON-EMPTY store is not by itself a green (CLA-687). Credentials are stored
// per provider (the `integration_id` column) and a session only works on the
// provider its model names, so a store holding `google` says nothing about a
// config whose models run on `opencode-go`: every session dies with
// `provider.auth` (401) on its first call, and the check used to report PASS
// because it counted rows rather than matching them. The providers are therefore
// taken from the models the run will ACTUALLY use - Config.ModelIDsFor, which
// resolves the phase/tier/panel chain - and compared with the ids the store
// holds. A named provider with no stored credential is a WARN rather than a
// FAIL, for the same reason an empty store is: the key may be declared in the
// opencode config instead, which this cannot read.
func checkOpencodeAuth(ctx context.Context, cfg *config.Config, e doctorEnv) []check {
	if !slices.Contains(cfg.SpawnedHarnesses(), "opencode") {
		return nil
	}
	c := check{name: "opencode_auth"}
	db := filepath.Join(opencodeDataDir(), "opencode.db")
	if _, err := os.Stat(db); err != nil {
		c.status = warn
		c.detail = db + " does not exist - no stored opencode credential"
		c.remedy = "run `opencode auth login` (2.x stores credentials in opencode.db, not auth.json), or give the provider its key in the opencode config"
		return []check{c}
	}
	if e.opencodeCredentials == nil {
		c.status = warn
		c.detail = db + " exists; credentials not inspected"
		return []check{c}
	}
	counts, err := e.opencodeCredentials(ctx, db)
	if err != nil {
		c.status = warn
		c.detail = db + " exists but its credential table could not be read: " + err.Error()
		c.remedy = "check `opencode auth list` by hand"
		return []check{c}
	}
	if len(counts) == 0 {
		c.status = warn
		c.detail = db + " holds no active credential"
		c.remedy = "run `opencode auth login`, or give the provider its key in the opencode config"
		return []check{c}
	}
	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	sort.Strings(names)
	// Which providers the configured models name, and which models put them
	// there (so the detail can point at the model, not just the provider).
	needed := map[string][]string{}
	var unattributed []string
	for _, id := range cfg.ModelIDsFor("opencode") {
		p, ok := modelProvider(id)
		if !ok {
			unattributed = append(unattributed, id)
			continue
		}
		needed[p] = append(needed[p], id)
	}
	var missing []string
	for _, p := range slices.Sorted(maps.Keys(needed)) {
		if counts[p] == 0 {
			missing = append(missing, p)
		}
	}
	if len(unattributed) > 0 {
		// Said, not warned: an id with no provider half cannot be matched against
		// the store either way, and inventing a provider for it would report a
		// gap nobody can act on.
		c.info = append(c.info, "model ids naming no provider, so not checked against the store: "+strings.Join(unattributed, ", "))
	}
	if len(missing) > 0 {
		gaps := make([]string, 0, len(missing))
		for _, p := range missing {
			gaps = append(gaps, fmt.Sprintf("%s (used by %s)", p, strings.Join(needed[p], ", ")))
		}
		c.status = warn
		c.detail = fmt.Sprintf("%s holds active credentials for: %s - but the configured models run on %s, which the store holds no credential for; a session on such a model ends at its first call with provider.auth", db, strings.Join(names, ", "), strings.Join(gaps, "; "))
		c.remedy = "run `opencode auth login` for " + strings.Join(missing, ", ") + ", or give that provider its key in the opencode config"
		return []check{c}
	}
	c.status = pass
	c.detail = fmt.Sprintf("%s holds active credentials for: %s (values not read; presence is not validity - the store records no expiry, so a revoked or expired key still counts)", db, strings.Join(names, ", "))
	if providers := slices.Sorted(maps.Keys(needed)); len(providers) > 0 {
		c.detail += "; the configured models run on " + strings.Join(providers, ", ")
	}
	return []check{c}
}

// checkOpencodePermissions reports the posture the adapter pins via
// OPENCODE_CONFIG_CONTENT, built by the adapter's own code. It does not ask
// opencode (`opencode debug config` talks to the operator's background service,
// which doctor must not start or attach to); the content layer is merged last,
// so what it carries is what wins for every key it sets.
func checkOpencodePermissions(cfg *config.Config, label, harnessName string) check {
	c := check{name: label}
	sess := cfg.SessionFor(harnessName)
	doc, err := harness.OpencodeSessionConfig(sess.MCPConfigPath, cfg.WorkDir, false)
	if err != nil {
		c.status = fail
		c.detail = "the adapter cannot build its session config, so it refuses to spawn: " + err.Error()
		c.remedy = "fix mcp_config_path: it must be a strict-JSON opencode config"
		return c
	}
	var parsed struct {
		Permission map[string]any `json:"permission"`
		MCP        struct {
			Servers map[string]map[string]any `json:"servers"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(doc, &parsed); err != nil {
		c.status = fail
		c.detail = "the session config does not parse: " + err.Error()
		return c
	}
	var problems []string
	for _, tool := range []string{"*", "webfetch", "websearch", "execute"} {
		if parsed.Permission[tool] != "deny" {
			problems = append(problems, tool+" is not denied")
		}
	}
	inCode := []string{}
	for name, s := range parsed.MCP.Servers {
		if s["codemode"] != false {
			inCode = append(inCode, name)
		}
	}
	sort.Strings(inCode)
	if len(inCode) > 0 {
		problems = append(problems, "MCP servers left in Code Mode: "+strings.Join(inCode, ", "))
	}
	if len(problems) > 0 {
		c.status = fail
		c.detail = "the pinned policy is not fail-closed: " + strings.Join(problems, "; ")
		return c
	}
	c.info = append(c.info, opencodeShellGapNote)
	if sess.MCPConfigPath != "" {
		var named map[string]json.RawMessage
		if data, err := os.ReadFile(sess.MCPConfigPath); err == nil && json.Unmarshal(data, &named) == nil {
			if raw, ok := named["permission"]; ok && string(raw) != "null" {
				// Verified on 2.0.32 (CLA-675 review): a top-level permission
				// block in the OPENCODE_CONFIG layer neither loosened nor locked
				// out the pinned policy. Said, not warned: it does nothing.
				c.info = append(c.info, sess.MCPConfigPath+" declares a top-level `permission` block; on opencode 2.x the pinned policy wins over it, so it has no effect - remove it to avoid confusion")
			}
		}
	}
	if where, v, ok := opencodeEnvOverride(cfg, harnessName, "OPENCODE_CONFIG_CONTENT"); ok {
		c.status = warn
		c.detail = "env replaces the adapter's OPENCODE_CONFIG_CONTENT (declared at " + where + "), and with it the fail-closed permission block"
		c.remedy = "drop it - opencode 2.x then runs with whatever policy that value carries, or none: " + truncate(v, 60)
		return c
	}
	if where, _, ok := opencodeEnvOverride(cfg, harnessName, "OPENCODE_PERMISSION"); ok {
		c.status = warn
		c.detail = "OPENCODE_PERMISSION is declared at " + where + ", and opencode 2.x ignores it"
		c.remedy = "drop it; the policy lives in the `permission` block the adapter pins via OPENCODE_CONFIG_CONTENT"
		return c
	}
	c.status = pass
	c.detail = fmt.Sprintf("fail-closed `permission` block pinned via OPENCODE_CONFIG_CONTENT (execute, webfetch, websearch denied); %d MCP %s out of Code Mode",
		len(parsed.MCP.Servers), plural(len(parsed.MCP.Servers), "server", "servers"))
	return c
}

// checkOpencodeMCPDisables flags MCP entries in the opencode mcp config files
// that carry no `type`: partial entries such as `"context7": {"enabled":
// false}`, meant to switch off a server the global config declares. Verified
// on 2.0.32 (CLA-675 review), a partial entry in a later layer does NOT stop
// that server, in either dialect (`servers.X: {"disabled": true}` included);
// only a full entry with its `type` and `command`/`url` and the off switch
// does. The adapter cannot repair it either: it moves only typed entries out of
// the 1.x dialect, because a typeless one is not a server it can describe.
func checkOpencodeMCPDisables(cfg *config.Config) []check {
	if !slices.Contains(cfg.SpawnedHarnesses(), "opencode") {
		return nil
	}
	paths := []string{}
	if p := cfg.SessionFor("opencode").MCPConfigPath; p != "" {
		paths = append(paths, p)
	}
	for _, pr := range cfg.Projects {
		if p := pr.MCPConfigPaths["opencode"]; p != "" && !slices.Contains(paths, p) {
			paths = append(paths, p)
		}
	}
	c := check{name: "opencode_mcp_disables", status: pass, detail: "no partial MCP entries in the opencode mcp config"}
	var found []string
	for _, p := range paths {
		for _, name := range opencodePartialMCPEntries(p) {
			found = append(found, fmt.Sprintf("%s: %s", p, name))
		}
	}
	if len(found) == 0 {
		return []check{c}
	}
	c.status = warn
	c.detail = "MCP entries with no `type` do not switch anything off on opencode 2.x: " + strings.Join(found, "; ")
	c.remedy = "repeat the server's full entry from the global opencode config with the switch added, e.g. " +
		`"context7": {"type": "remote", "url": "<its url>", "enabled": false}` +
		" (or under mcp.servers with \"disabled\": true), or remove the entry and the server from the global config"
	return []check{c}
}

// opencodePartialMCPEntries names the entries under `mcp` (1.x dialect) and
// `mcp.servers` (2.x) of one file that carry no `type`. An unreadable file
// yields nothing: other checks report that.
func opencodePartialMCPEntries(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var f struct {
		MCP map[string]json.RawMessage `json:"mcp"`
	}
	if json.Unmarshal(data, &f) != nil {
		return nil
	}
	var out []string
	typeless := func(prefix string, raw json.RawMessage) {
		var e map[string]json.RawMessage
		if json.Unmarshal(raw, &e) != nil {
			return
		}
		if _, ok := e["type"]; !ok {
			out = append(out, prefix)
		}
	}
	for name, raw := range f.MCP {
		if name == "servers" {
			var servers map[string]json.RawMessage
			if json.Unmarshal(raw, &servers) == nil {
				for sn, sraw := range servers {
					typeless("mcp.servers."+sn, sraw)
				}
			}
			continue
		}
		typeless("mcp."+name, raw)
	}
	sort.Strings(out)
	return out
}
