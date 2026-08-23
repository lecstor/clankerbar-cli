package config

import (
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
// opencode discovers config on its own, in merge order: the global file under
// its config dir, then OPENCODE_CONFIG (the file this driver names), then every
// project-level `opencode.json` it finds from the INSTANCE DIRECTORY, then the
// OPENCODE_CONFIG_DIR directory again, then OPENCODE_CONFIG_CONTENT. Three of
// those five layers are not ours, and two of them merge AFTER the file we point
// at - which is how sessions spawned for one project spent a fortnight claiming,
// working and handing back another project's tasks: 14 CLA-* leases held by
// sessions labelled [ezyapp], and no ezyapp task ever worked (CLA-441).
//
// harness/opencode.go now pins the driver's own block through
// OPENCODE_CONFIG_CONTENT, which opencode merges LAST, so none of these files
// can redirect a spawned session any more. This check is what stops the fix from
// being invisible: a file that still names the wrong project is a live trap for
// every interactive session and for any future path that loses the content pin,
// and it is one line of output to say so.
//
// It WARNS rather than refusing, deliberately - see OpencodeAmbientConflicts.

// The file names opencode discovers by itself. Transcribed from what a live
// 1.18.19 run logs it is `loading` at startup, not from the docs:
//
//	loading path=/Users/jason/.config/opencode/config.json
//	loading path=/Users/jason/.config/opencode/opencode.json
//	loading path=/Users/jason/.config/opencode/opencode.jsonc
//	loading path=/Users/jason/.opencode/opencode.json
//	loading path=/Users/jason/.opencode/opencode.jsonc
//
// Two spellings everywhere, because the global file is conventionally `.jsonc`
// (it has comments) and a project-level one conventionally is not. `config.json`
// is a GLOBAL name only: opencode reads it in its config directory, and a
// project-level `config.json` is an ordinary file thousands of repos carry for
// something else entirely.
var (
	opencodeGlobalConfigNames  = []string{"config.json", "opencode.json", "opencode.jsonc"}
	opencodeProjectConfigNames = []string{"opencode.json", "opencode.jsonc"}
)

// opencodeHomeConfigDir is opencode's OTHER global directory. It is read whatever
// OPENCODE_CONFIG_DIR says - the live log above shows both being loaded in one
// run - so a `clankerbar` block parked here is discovered by every session even
// on a config whose `config_dir` points elsewhere.
const opencodeHomeConfigDir = "~/.opencode"

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
	// Got is the slug the file's clankerbar server actually names.
	Got string
}

func (c OpencodeAmbientConflict) String() string {
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
			got := opencodeConfigClankerbarSlug(path)
			if got == "" || containsString(scope.wantSlugs, got) {
				continue
			}
			out = append(out, OpencodeAmbientConflict{
				Path:  path,
				Scope: scope.label,
				Want:  strings.Join(scope.wantSlugs, ", "),
				Got:   got,
			})
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
		globals := []string{opencodeHomeConfigDir}
		if dir := c.SessionFor("opencode").ConfigDir; dir != "" {
			globals = append(globals, dir)
		}
		seen := map[string]bool{}
		for _, dir := range globals {
			dir = expandHome(dir)
			if seen[dir] {
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
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	f, err := parseJSONCInto(data)
	if err != nil {
		return ""
	}
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
