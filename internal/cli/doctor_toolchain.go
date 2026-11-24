package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// --- 10. toolchain grants -----------------------------------------------------

// toolchainMarkers maps a marker file to the command a session must be allowed to execute to verify
// that repo. Only markers that name their tool UNAMBIGUOUSLY are listed: a bare package.json could
// be npm, pnpm or yarn, so the lockfile is the marker and a lockless repo is left alone rather than
// guessed at.
var toolchainMarkers = []struct{ marker, tool string }{
	{"go.mod", "go"},
	{"Cargo.toml", "cargo"},
	{"pnpm-lock.yaml", "pnpm"},
	{"yarn.lock", "yarn"},
	{"package-lock.json", "npm"},
}

// checkToolchains cross-references the build tools the backlog's repos need against what the
// permission policy actually grants. A headless claude session FAILS CLOSED: anything the policy
// does not name is refused outright, with no prompt reaching the operator.
func runsHarness(cfg *config.Config, name string) bool {
	for _, h := range cfg.SpawnedHarnesses() {
		if h == name {
			return true
		}
	}
	return false
}

// costBlindHarnesses names every harness this run spawns that cannot report what a session cost, in
// the order SpawnedHarnesses lists them. SPAWNED, not declared: a never-spawned cost-blind harness
// would make the run's live `max_cost_usd` report as INERT and send the operator to fix a harness
// no session ever runs on.
func costBlindHarnesses(cfg *config.Config) []string {
	var out []string
	for _, h := range cfg.SpawnedHarnesses() {
		if !harnessReportsCost(h) {
			out = append(out, h)
		}
	}
	return out
}

func checkToolchains(cfg *config.Config) check {
	c := check{name: "toolchains", status: pass}
	// The allowlist is Claude's, but claude need not be the RUN's harness: a mixed sequence can
	// implement elsewhere and review on claude, and it is the claude session that runs the
	// verification verbs this check is about.
	if !runsHarness(cfg, "claude") {
		// SpawnedHarnesses, to agree with the gate directly above it. Listing PhaseHarnesses here named
		// the DECLARED harness too, so a config whose phases all override it printed "no allowlist to
		// audit for claude, opencode" - a sentence naming claude among the harnesses it has just said
		// have no claude allowlist.
		c.detail = "no allowlist to audit for " + strings.Join(cfg.SpawnedHarnesses(), ", ")
		return c
	}
	settingsPath := cfg.SessionFor("claude").SettingsPath

	needed := detectToolchains(cfg)
	if len(needed) == 0 {
		c.detail = "no unambiguous build toolchains detected in the session workdirs"
		return c
	}
	tools := make([]string, 0, len(needed))
	for _, t := range needed {
		tools = append(tools, t.tool)
	}

	if settingsPath == "" {
		// checkPermissions already warns about running on the ambient allowlist; here the useful thing is
		// to name what would have to be in it.
		c.detail = "needs " + strings.Join(tools, ", ") + " — grants come from the ambient allowlist, which doctor does not audit"
		return c
	}

	allow, denyAll, denyVerbs := grantedCommands(cfg)
	var missing, blocked, narrowed []string
	for _, t := range needed {
		switch {
		case denyAll[t.tool]:
			// A bare-head deny wins over every allow, so this one is decided and worth naming separately —
			// an operator looking at an allow entry would call it granted.
			blocked = append(blocked, t.tool+" ("+t.where+")")
		case !allow[t.tool]:
			missing = append(missing, t.tool+" ("+t.where+")")
		case len(denyVerbs[t.tool]) > 0:
			// Granted, but with holes. Not a warning on its own — this is what a careful policy looks like
			// — so it is reported alongside the PASS.
			narrowed = append(narrowed, t.tool+" (except "+strings.Join(denyVerbs[t.tool], ", ")+")")
		}
	}

	switch {
	case len(blocked) > 0:
		c.status = warn
		c.detail = "denied by policy: " + strings.Join(blocked, ", ")
		c.remedy = "remove the deny rule in " + settingsPath + ", or accept that tasks in those repos cannot be verified — " + setupSkillSection(2, "pre-allow every toolchain")
	case len(missing) > 0:
		c.status = warn
		c.detail = "no grant for: " + strings.Join(missing, ", ")
		c.remedy = "allow the verbs each one needs in " + settingsPath +
			" (e.g. Bash(go build:*), Bash(go vet:*), Bash(go test:*)) — a headless session fails closed, so an ungranted tool is refused with no prompt and its task ships unverified — " + setupSkillSection(2, "pre-allow every toolchain")
	default:
		c.detail = "granted: " + strings.Join(tools, ", ")
		if len(narrowed) > 0 {
			c.detail += "; narrowed by deny rules: " + strings.Join(narrowed, ", ")
		}
	}
	return c
}

// neededToolchain is a tool the repos require, with one example of where the requirement was found
// — an operator fixing a grant wants to know which repo asked for it.
type neededToolchain struct {
	tool  string
	where string
}

// detectToolchains looks in each session workdir and, when that workdir is a multi-repo parent, in
// the checkouts under it that belong to the project. A session rooted in a multi-repo parent COULD
// be sent into any repo below it, but only the project's own repos are plausibly in its backlog —
// and warning about every unrelated checkout on the machine is how a WARN line becomes something an
// operator skims.
func detectToolchains(cfg *config.Config) []neededToolchain {
	type target struct{ dir, slug string }
	var targets []target
	if len(cfg.Projects) == 0 {
		targets = append(targets, target{dir: cfg.WorkDir})
	} else {
		for _, p := range cfg.Projects {
			targets = append(targets, target{dir: projectWorkDir(cfg, p), slug: p.Slug})
		}
	}

	found := map[string]string{}
	for _, t := range targets {
		dir := t.dir
		if dir == "" {
			dir = "."
		}
		for _, scan := range append([]string{dir}, projectRepos(dir, t.slug)...) {
			for _, m := range toolchainMarkers {
				if _, ok := found[m.tool]; ok {
					continue
				}
				if _, err := os.Stat(filepath.Join(scan, m.marker)); err == nil {
					found[m.tool] = scan
				}
			}
		}
	}

	// Iterate the marker table, not the map, so the output order is stable.
	out := make([]neededToolchain, 0, len(found))
	for _, m := range toolchainMarkers {
		if where, ok := found[m.tool]; ok {
			out = append(out, neededToolchain{tool: m.tool, where: where})
		}
	}
	return out
}

// projectRepos returns the immediate subdirectories of dir that are checkouts belonging to slug. An
// empty slug matches nothing: without a project name there is no way to tell the project's repos
// from everything else on the disk.
func projectRepos(dir, slug string) []string {
	if slug == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), slug) {
			continue
		}
		child := filepath.Join(dir, entry.Name())
		if _, err := os.Stat(filepath.Join(child, ".git")); err == nil {
			out = append(out, child)
		}
	}
	return out
}

// policySettingsPaths lists every settings file Claude will MERGE for these sessions, in no
// particular order — auditing only the drain policy would report a tool as ungranted when another
// layer already allows it.
func policySettingsPaths(cfg *config.Config) []string {
	// The claude SESSION's fields, not the run's: in a mixed sequence claude may be a phase rather
	// than the top-level harness, and then the policy that gates it is the one in its own block.
	hc := cfg.SessionFor("claude")
	paths := []string{hc.SettingsPath}
	if hc.ConfigDir != "" {
		paths = append(paths,
			filepath.Join(hc.ConfigDir, "settings.json"),
			filepath.Join(hc.ConfigDir, "settings.local.json"),
		)
	}
	// config owns this list: it is the confinement boundary, and statedir.Open is handed the same one.
	for _, dir := range cfg.SessionWorkDirs() {
		paths = append(paths,
			filepath.Join(dir, ".claude", "settings.json"),
			filepath.Join(dir, ".claude", "settings.local.json"),
		)
	}
	return paths
}

// grantedCommands returns, per Bash command head, whether the policy grants it and whether it
// denies it OUTRIGHT. Deny wins over allow in Claude's policy, but only for what the deny rule
// actually covers: a narrow `Bash(go run:*)` deny alongside a `Bash(go test:*)` allow is a normal,
// careful setup, and reporting that as "go is denied, tasks in those repos cannot be verified"
// would be flatly wrong.
func grantedCommands(cfg *config.Config) (allow, denyAll map[string]bool, denyVerbs map[string][]string) {
	allow, denyAll, denyVerbs = map[string]bool{}, map[string]bool{}, map[string][]string{}
	for _, path := range policySettingsPaths(cfg) {
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue // absent or unreadable is not a grant; other checks report it
		}
		var doc struct {
			Permissions struct {
				Allow []string `json:"allow"`
				Deny  []string `json:"deny"`
			} `json:"permissions"`
		}
		if json.Unmarshal(data, &doc) != nil {
			continue // checkPermissions reports unparseable JSON as a FAIL
		}
		for _, rule := range doc.Permissions.Allow {
			if head, _ := bashHead(rule); head != "" {
				allow[head] = true
			}
		}
		for _, rule := range doc.Permissions.Deny {
			head, verb := bashHead(rule)
			switch {
			case head == "":
			case verb == "":
				denyAll[head] = true
			default:
				denyVerbs[head] = append(denyVerbs[head], head+" "+verb)
			}
		}
	}
	return allow, denyAll, denyVerbs
}

// bashHead splits a Bash permission rule into its command word and the verb that qualifies it, if
// any: `Bash(go build:*)` yields ("go", "build"), while `Bash(go:*)` and `Bash(go)` yield ("go",
// "").
func bashHead(rule string) (head, verb string) {
	inner, ok := strings.CutPrefix(strings.TrimSpace(rule), "Bash(")
	if !ok {
		return "", ""
	}
	inner = strings.TrimSuffix(inner, ")")
	if before, _, found := strings.Cut(inner, ":"); found {
		inner = before
	}
	fields := strings.Fields(inner)
	switch len(fields) {
	case 0:
		return "", ""
	case 1:
		return fields[0], ""
	default:
		return fields[0], fields[1]
	}
}

func firstField(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}
