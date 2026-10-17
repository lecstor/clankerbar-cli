package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// This file answers one question: which opencode config files will a session
// merge that the DRIVER never named, and does one of them point the `clankerbar`
// MCP server at a different project than the sessions loading it are meant to
// work?
//
// opencode 2.x discovers config on its own, in merge order (read off `opencode
// debug config` against 2.0.32): the global config dir, then OPENCODE_CONFIG
// (the file this driver names), then the project-level `opencode.json` and
// `.opencode/opencode.json` it finds from the INSTANCE DIRECTORY, then
// OPENCODE_CONFIG_CONTENT. The global dir and the project files are not ours,
// and the project files merge AFTER the file we point at - which is how, on
// 1.x, sessions spawned for one project spent a fortnight claiming, working and
// handing back another project's tasks: 14 CLA-* leases held by sessions
// labelled [ezyapp], and no ezyapp task ever worked (CLA-441).
//
// harness/opencode.go pins the driver's own config through
// OPENCODE_CONFIG_CONTENT, which opencode merges after every layer this driver
// or a checkout can write, so none of these files can redirect a spawned
// session any more. This check is what stops the fix from being invisible: a
// file that still names the wrong project is a live trap for every interactive
// session and for any future path that loses the content pin, and it is one
// line of output to say so.
//
// It WARNS rather than refusing, deliberately - see OpencodeAmbientConflicts.

// The file names opencode discovers by itself. `opencode.json` is the name
// 2.0.32 was observed loading in both the global dir and a project; the `.jsonc`
// spelling and the global `config.json` are carried over from 1.x, where a live
// run logged loading all three, and are scanned because a stale file there is
// cheap to report and an unread one costs nothing. `config.json` is a GLOBAL
// name only: a project-level `config.json` is an ordinary file thousands of
// repos carry for something else entirely.
var (
	opencodeGlobalConfigNames  = []string{"config.json", "opencode.json", "opencode.jsonc"}
	opencodeProjectConfigNames = []string{"opencode.json", "opencode.jsonc", filepath.Join(".opencode", "opencode.json"), filepath.Join(".opencode", "opencode.jsonc")}
)

// opencodeGlobalConfigDirs is the directory opencode 2.x reads global config
// from: `$XDG_CONFIG_HOME/opencode`, or `~/.config/opencode` when that is unset
// - unless OPENCODE_CONFIG_DIR is set, which REPLACES it (verified against
// 2.0.32). The adapter exports `config_dir` as OPENCODE_CONFIG_DIR, so a run
// with a config_dir reads that directory and not the default one. 2.x does NOT
// read ~/.opencode, ~/.claude, ~/.agents or ~/.config/opencode2 as config
// (verified; 1.x read ~/.opencode).
func opencodeGlobalConfigDirs(configDir string) []string {
	if configDir != "" {
		return []string{expandHome(configDir)}
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = expandHome("~/.config")
	}
	return []string{filepath.Join(base, "opencode")}
}

// OpencodeAmbientConflict is one discovered opencode config whose `clankerbar`
// MCP server names a project that the sessions loading it do not work.
type OpencodeAmbientConflict struct {
	// Path is the file on disk.
	Path string
	// Scope says which sessions load it: "global" (every opencode session on
	// this machine) or "session directory" (sessions whose instance directory
	// is that tree).
	Scope string
	// Want is the slug those sessions poll and are meant to work - more than
	// one, comma-joined, for the global file of a multi-project run.
	Want string
	// Got is the slug the file's clankerbar server actually names. Empty on an
	// Overrides finding, which is about the file's other keys rather than its
	// backlog.
	Got string
	// Overrides names what this file carries that decides what a session IS
	// rather than what it talks to - see sessionShapingKeys. Empty on a slug
	// finding; the two kinds are reported separately, because their remedies
	// have nothing in common.
	Overrides []string
}

func (c OpencodeAmbientConflict) String() string {
	if len(c.Overrides) > 0 {
		return fmt.Sprintf("%s (%s) declares %s, which every session loading it inherits", c.Path, c.Scope, strings.Join(c.Overrides, " and "))
	}
	return fmt.Sprintf("%s (%s) names /mcp/%s, but the sessions that load it work /mcp/%s", c.Path, c.Scope, c.Got, c.Want)
}

// OpencodeAmbientConflicts lists every discovered opencode config whose
// clankerbar server names the wrong project. Empty when the run spawns no
// opencode session, when no such file exists, or when every one of them agrees
// with the project it would serve.
//
// A WARNING, not a refusal, which is a departure from how this package treats
// every other slug disagreement (those are hard errors: see the projects[i] slug
// check and the harnesses.<name> one). Three reasons, and the first is the one
// that decides it:
//
//   - The file can no longer do the damage. OPENCODE_CONFIG_CONTENT is merged
//     after every layer listed above, so a spawned session's `mcp.clankerbar` is
//     the driver's whatever these files say. Refusing to start over a disarmed
//     trap trades a silent wrong-project drain for a loud no-drain-at-all.
//   - The file is frequently not the operator's to change. A project-level
//     `opencode.json` is a CHECKED-IN file of somebody's repo - this CLI's own
//     repo carries one - so a run that declares two repos would be refused
//     startup over a file that is correct for its own repo and merely foreign to
//     this run.
//   - It still needs saying, because it is not harmless: every INTERACTIVE
//     opencode session in that tree gets the wrong backlog, which is exactly how
//     this was found.
//
// Nothing here reads a file the run would not otherwise hand to a session, and a
// file it cannot parse is passed over rather than guessed at: this is a
// diagnostic, and a diagnostic that fails closed would be a startup outage on a
// malformed file that opencode itself tolerates.
func (c *Config) OpencodeAmbientConflicts() []OpencodeAmbientConflict {
	if !c.spawnsOpencode() {
		return nil
	}
	var out []OpencodeAmbientConflict
	for _, scope := range c.opencodeAmbientScopes() {
		for _, name := range scope.names {
			path := filepath.Join(scope.dir, name)
			f, ok := readOpencodeConfig(path)
			if !ok {
				continue
			}
			if got := clankerbarSlugIn(f); got != "" && !containsString(scope.wantSlugs, got) {
				out = append(out, OpencodeAmbientConflict{
					Path:  path,
					Scope: scope.label,
					Want:  strings.Join(scope.wantSlugs, ", "),
					Got:   got,
				})
			}
			if keys := sessionShapingKeys(f); len(keys) > 0 {
				out = append(out, OpencodeAmbientConflict{Path: path, Scope: scope.label, Overrides: keys})
			}
		}
	}
	return out
}

// ambientScope is one directory opencode will discover config in, and the slugs
// the sessions that discover it are meant to work.
type ambientScope struct {
	dir       string
	label     string
	names     []string
	wantSlugs []string
}

// opencodeAmbientScopes lists where to look. The global config dir is checked
// against EVERY slug the run drains - it is loaded by all of them, so it is only
// wrong if it agrees with none. Each session directory is checked against the
// one project whose sessions start there.
//
// The session directories are the project's workdir AND every checkout it
// declares, because CLA-437 spawns each session in the task's own repo: a
// checked-in `opencode.json` in any declared repo is discovered from there, which
// is the second of the two live mechanisms in CLA-441 and the one no amount of
// tidying the global file would have found.
func (c *Config) opencodeAmbientScopes() []ambientScope {
	var scopes []ambientScope
	if all := c.drainedSlugs(); len(all) > 0 {
		globals := opencodeGlobalConfigDirs(c.SessionFor("opencode").ConfigDir)
		seen := map[string]bool{}
		for _, dir := range globals {
			if dir == "" || seen[dir] {
				continue
			}
			seen[dir] = true
			scopes = append(scopes, ambientScope{dir: dir, label: "global", names: opencodeGlobalConfigNames, wantSlugs: all})
		}
	}
	add := func(workdir, slug string, repos map[string]string, primary string) {
		if slug == "" {
			return
		}
		seen := map[string]bool{}
		for _, dir := range append([]string{workdir}, DeclaredCheckouts(repos, primary, workdir)...) {
			if dir == "" || seen[dir] {
				continue
			}
			seen[dir] = true
			scopes = append(scopes, ambientScope{dir: dir, label: "session directory", names: opencodeProjectConfigNames, wantSlugs: []string{slug}})
		}
	}
	if len(c.Projects) == 0 {
		add(c.WorkDir, slugFromMCPURL(mcpURLFromConfig(c.MCPConfigPath)), c.Repos, c.PrimaryRepo)
		return scopes
	}
	for _, p := range c.Projects {
		workdir := p.WorkDir
		if workdir == "" {
			workdir = c.WorkDir
		}
		add(workdir, p.Slug, p.Repos, p.PrimaryRepo)
	}
	return scopes
}

// drainedSlugs is every project slug this run works, sorted. A single-project
// run's slug comes from its MCP config the same way BacklogEndpoint derives it.
func (c *Config) drainedSlugs() []string {
	var out []string
	if len(c.Projects) == 0 {
		if s := slugFromMCPURL(mcpURLFromConfig(c.MCPConfigPath)); s != "" {
			out = append(out, s)
		}
		return out
	}
	for _, p := range c.Projects {
		if p.Slug != "" {
			out = append(out, p.Slug)
		}
	}
	sort.Strings(out)
	return out
}

// spawnsOpencode reports whether any session this run spawns runs on opencode.
// SPAWNED, not merely declared: an opencode block nothing ever runs makes these
// files irrelevant, and a warning about an irrelevant file is how an operator
// learns to skim the ones that matter.
func (c *Config) spawnsOpencode() bool {
	for _, h := range c.SpawnedHarnesses() {
		if h == "opencode" {
			return true
		}
	}
	return false
}

// opencodeConfigClankerbarSlug reads one discovered opencode config and returns
// the project slug its `clankerbar` MCP server names, or "" when the file is
// absent, unreadable, unparseable, carries no such server, has it DISABLED, or
// gives it a URL that is not an /mcp/<slug> endpoint.
//
// Only the server literally NAMED `clankerbar` counts. That is narrower than
// mcpURLFromConfig, which also accepts "whichever entry is handed the API key",
// and the narrowness is the point: opencode merges these files KEY BY KEY, so
// only an entry sharing the driver's own key can displace it. A differently
// named block pointed at another project - `clankerbar-interactive`, the shape
// the operator's own global config now uses - displaces nothing and must not be
// reported as if it did.
//
// `"enabled": false` is respected for the same reason: opencode does not start a
// disabled server, so it cannot redirect anything. This is deliberately NOT done
// in readMCPServers, whose callers are security gates - there, an entry the file
// merely CLAIMS is disabled is still an entry the operator should see disclosed,
// and a later merge layer can flip the flag back.
func opencodeConfigClankerbarSlug(path string) string {
	f, ok := readOpencodeConfig(path)
	if !ok {
		return ""
	}
	return clankerbarSlugIn(f)
}

// readOpencodeConfig reads and JSONC-parses one discovered opencode config. ok
// is false for a file that is absent, unreadable or unparseable - a diagnostic
// that failed closed would be a startup outage on a malformed file opencode
// itself tolerates.
func readOpencodeConfig(path string) (mcpFile, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return mcpFile{}, false
	}
	f, err := parseJSONCInto(data)
	if err != nil {
		return mcpFile{}, false
	}
	return f, true
}

// sessionShapingKeys names what a discovered opencode config carries that
// decides what a session IS rather than what it talks to.
// checkDiscoveredMCPConfig REFUSES these, but only on the file
// `mcp_config_path` resolved to - a file opencode discovers by itself never
// reaches that gate, and it is merged into every session all the same.
//
// `permission` is on the list because the merge is recursive and PER KEY: on
// 2.x the adapter's policy rides in OPENCODE_CONFIG_CONTENT, the last layer,
// but a key an ambient file declared keeps ITS position in the merged object
// and the evaluator is last-match-wins over that insertion order. Read out of
// 1.18.19 (`o={...r,...a}` then a recursive descent, flattened with
// Object.entries); 2.x accepts the same block shape and its ordering was not
// re-derived from source, so the report stands on the 1.x reading. Let an
// ambient file declare `permission.read` and `read` sorts AHEAD of the policy's
// `*` deny, which then becomes the final match for every read - the CLA-441
// wall arrived at from the other direction (CLA-441 second review).
//
// `plugin` is code opencode loads and runs at session start. An `mcp` entry
// with a `command` is the same threat wearing a different key, and
// startsProcess is the predicate the CLA-266 gate already applies to it, so it
// is reported here rather than left as the conspicuous omission from a list
// about what a session runs.
//
// `agent` is reported only for an entry carrying an AUTHORITY key - see
// authorityAgents. The unfiltered version fired on the operator's own global
// config, whose whole agent block picks a cheap model for generating titles,
// and a WARN like that is how an operator learns to skim the ones that matter.
func sessionShapingKeys(f mcpFile) []string {
	var out []string
	if rawSet(f.Permission) {
		out = append(out, "`permission` (rules merged into the session's own policy, which reorders it and can deny everything)")
	}
	if rawSet(f.Plugin) {
		out = append(out, "`plugin` (code run at session start)")
	}
	if names := authorityAgents(f.Agent); len(names) > 0 {
		out = append(out, "`agent` with tool or permission authority ("+strings.Join(names, ", ")+")")
	}
	if names := localProcessServers(f); len(names) > 0 {
		out = append(out, "an `mcp` server that starts a local process ("+strings.Join(names, ", ")+")")
	}
	return out
}

// authorityAgents names the agents in a discovered config's `agent` block that
// carry a key deciding what that agent MAY DO - its permissions, its tool set,
// its mode, whether it is disabled, or its prompt. An agent entry that only
// picks a model or a temperature changes cost and style, not authority, and is
// not worth an operator's attention on every doctor run.
//
// An `agent` block this cannot parse counts as authority-carrying: the report is
// the conservative side, and a block shaped in a way this does not model is
// precisely the one nobody has looked at.
func authorityAgents(raw json.RawMessage) []string {
	if !rawSet(raw) {
		return nil
	}
	var block map[string]map[string]json.RawMessage
	if err := json.Unmarshal(raw, &block); err != nil {
		return []string{"unparseable agent block"}
	}
	var out []string
	for _, name := range sortedRawKeys(block) {
		for _, key := range []string{"permission", "tools", "mode", "disable", "prompt"} {
			if rawSet(block[name][key]) {
				out = append(out, name)
				break
			}
		}
	}
	return out
}

// localProcessServers names the MCP entries of a discovered config that would
// start a local process. Disabled entries are skipped for the same reason the
// slug check skips them: opencode does not start them. This is a DIAGNOSTIC, not
// the CLA-266 gate, which keeps looking at an entry the file claims is off.
func localProcessServers(f mcpFile) []string {
	var out []string
	for _, block := range []map[string]mcpEntry{f.MCP, f.MCPServers} {
		for _, name := range sortedEntryKeys(block) {
			if e := block[name]; e.startsProcess() && !e.disabled() {
				out = append(out, name)
			}
		}
	}
	return out
}

func sortedRawKeys(m map[string]map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedEntryKeys(m map[string]mcpEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// clankerbarSlugIn returns the project slug the file's `clankerbar` MCP server
// names, or "" when it carries no such server, has it DISABLED, or gives it a
// URL that is not an /mcp/<slug> endpoint.
func clankerbarSlugIn(f mcpFile) string {
	for _, block := range []map[string]mcpEntry{f.MCP, f.MCPServers} {
		entry, ok := block["clankerbar"]
		if !ok || entry.disabled() {
			continue
		}
		if s := slugFromMCPURL(entry.URL); s != "" {
			return s
		}
	}
	return ""
}

func containsString(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
