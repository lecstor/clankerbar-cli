package cli

import (
	"context"
	"encoding/json"
	"fmt"
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

// sqliteOpencodeCredentials counts the rows of opencode.db's credential table
// per integration. It selects the integration id and a count only: no column
// holding a secret is ever read. The database is opened read-only.
func sqliteOpencodeCredentials(ctx context.Context, dbPath string) (map[string]int, error) {
	sq, err := exec.LookPath("sqlite3")
	if err != nil {
		return nil, fmt.Errorf("sqlite3 not on PATH")
	}
	ctx, cancel := context.WithTimeout(ctx, binVersionTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, sq, "-readonly", "-separator", "\t", dbPath,
		"select coalesce(integration_id, ''), count(*) from credential group by 1").Output()
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

// checkOpencodeAuth reports on 2.x's credential store when the run spawns
// opencode. A missing store or an empty one is a WARN, not a FAIL: a provider
// key can also come from the provider config (`{env:...}`), which this cannot
// see.
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
		c.detail = db + " holds no credential"
		c.remedy = "run `opencode auth login`, or give the provider its key in the opencode config"
		return []check{c}
	}
	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	sort.Strings(names)
	c.status = pass
	c.detail = fmt.Sprintf("%s holds credentials for: %s (values not read)", db, strings.Join(names, ", "))
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
				c.status = warn
				c.detail = sess.MCPConfigPath + " declares its own `permission` block; it is also loaded as OPENCODE_CONFIG, where opencode merges it key by key under the pinned policy and its keys sort ahead of the `*` catch-all"
				c.remedy = "remove `permission` from the mcp config; the adapter supplies the policy"
				return c
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
