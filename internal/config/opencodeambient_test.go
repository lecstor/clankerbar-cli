package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLA-441: opencode merges config files the driver never named - a global one
// under its config dir, and a project-level `opencode.json` discovered from the
// session's instance directory - and both merge AFTER the file OPENCODE_CONFIG
// names. A `clankerbar` block in one of those pointed 14 CLA-* leases at sessions
// spawned for a different project. Spawned sessions are pinned past it now
// (OPENCODE_CONFIG_CONTENT); these files are still a live trap for interactive
// ones, so validate/doctor say so.

func writeAmbientFile(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func clankerbarBlock(slug string) string {
	return `{"mcp":{"clankerbar":{"type":"remote","url":"https://clankerbar.com/mcp/` + slug + `"}}}`
}

// opencodeRun is a minimal single-project opencode config draining `slug` out of
// `workdir`, with `configDir` as opencode's global config directory.
func opencodeRun(t *testing.T, slug, workdir, configDir string) *Config {
	t.Helper()
	// opencode reads ~/.opencode as a global config directory whatever
	// OPENCODE_CONFIG_DIR says, so HOME is isolated: without this these tests
	// would pass or fail on what happens to be in the developer's home.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	mcp := writeAmbientFile(t, filepath.Join(t.TempDir(), "opencode-mcp.json"), clankerbarBlock(slug))
	return &Config{
		Harness:       "opencode",
		Prompt:        "Work the next backlog item.",
		WorkDir:       workdir,
		MCPConfigPath: mcp,
		Harnesses:     map[string]HarnessConfig{"opencode": {ConfigDir: configDir}},
	}
}

func onlyConflict(t *testing.T, got []OpencodeAmbientConflict) OpencodeAmbientConflict {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("conflicts = %v, want exactly one", got)
	}
	return got[0]
}

// The global file: loaded by every opencode session on the machine, whatever
// directory it runs in. This is the layer that redirected the sessions started
// in ~/dev.
func TestGlobalOpencodeConfigNamingAnotherProjectIsFlagged(t *testing.T) {
	configDir := t.TempDir()
	writeAmbientFile(t, filepath.Join(configDir, "opencode.jsonc"), `{
  // the operator's own interactive setup
  "mcp": {
    "clankerbar": { "type": "remote", "url": "https://clankerbar.com/mcp/clankerbar" },
  }
}`)
	c := opencodeRun(t, "ezyapp", t.TempDir(), configDir)

	got := onlyConflict(t, c.OpencodeAmbientConflicts())
	if got.Scope != "global" || got.Got != "clankerbar" || got.Want != "ezyapp" {
		t.Errorf("conflict = %+v, want the global file named clankerbar against a run draining ezyapp", got)
	}
	if !strings.Contains(got.String(), "opencode.jsonc") {
		t.Errorf("the message must name the file: %q", got.String())
	}
}

// The project-level file, in a repo the run declares. CLA-437 spawns each
// session in the TASK'S checkout, so a committed opencode.json in any declared
// repo is discovered from there - the second live mechanism, and the one tidying
// the global file would never have found.
func TestDeclaredRepoOpencodeConfigNamingAnotherProjectIsFlagged(t *testing.T) {
	workdir := t.TempDir()
	repo := filepath.Join(workdir, "clankerbar-cli")
	writeAmbientFile(t, filepath.Join(repo, "opencode.json"), clankerbarBlock("clankerbar"))

	c := opencodeRun(t, "ezyapp", workdir, "")
	c.Repos = map[string]string{"lecstor/clankerbar-cli": repo}

	got := onlyConflict(t, c.OpencodeAmbientConflicts())
	if got.Scope != "session directory" || got.Got != "clankerbar" || got.Want != "ezyapp" {
		t.Errorf("conflict = %+v, want the declared repo's own file flagged", got)
	}
}

// Everything that must NOT be flagged. Each of these was a real shape on the
// operator's machine while this was being diagnosed, and a check that cried wolf
// on them would be turned off within a day.
func TestOpencodeAmbientConflictsStayQuiet(t *testing.T) {
	t.Run("agreeing slug", func(t *testing.T) {
		configDir := t.TempDir()
		writeAmbientFile(t, filepath.Join(configDir, "opencode.json"), clankerbarBlock("ezyapp"))
		if got := opencodeRun(t, "ezyapp", t.TempDir(), configDir).OpencodeAmbientConflicts(); len(got) != 0 {
			t.Errorf("conflicts = %v, want none: the file names the project the run works", got)
		}
	})
	t.Run("disabled block", func(t *testing.T) {
		configDir := t.TempDir()
		writeAmbientFile(t, filepath.Join(configDir, "opencode.json"),
			`{"mcp":{"clankerbar":{"type":"remote","url":"https://clankerbar.com/mcp/clankerbar","enabled":false}}}`)
		if got := opencodeRun(t, "ezyapp", t.TempDir(), configDir).OpencodeAmbientConflicts(); len(got) != 0 {
			t.Errorf("conflicts = %v, want none: opencode does not start a disabled server, so it redirects nothing", got)
		}
	})
	t.Run("differently named block", func(t *testing.T) {
		configDir := t.TempDir()
		writeAmbientFile(t, filepath.Join(configDir, "opencode.json"),
			`{"mcp":{"clankerbar-interactive":{"type":"remote","url":"https://clankerbar.com/mcp/clankerbar","headers":{"Authorization":"Bearer {env:CLANKERBAR_API_KEY}"}}}}`)
		if got := opencodeRun(t, "ezyapp", t.TempDir(), configDir).OpencodeAmbientConflicts(); len(got) != 0 {
			t.Errorf("conflicts = %v, want none: opencode merges KEY BY KEY, so a block under another name displaces nothing - even one holding the API key", got)
		}
	})
	t.Run("no opencode session", func(t *testing.T) {
		configDir := t.TempDir()
		writeAmbientFile(t, filepath.Join(configDir, "opencode.json"), clankerbarBlock("clankerbar"))
		c := opencodeRun(t, "ezyapp", t.TempDir(), configDir)
		c.Harness = "claude"
		if got := c.OpencodeAmbientConflicts(); len(got) != 0 {
			t.Errorf("conflicts = %v, want none: a run that spawns no opencode session never merges these files", got)
		}
	})
	t.Run("no such file", func(t *testing.T) {
		if got := opencodeRun(t, "ezyapp", t.TempDir(), t.TempDir()).OpencodeAmbientConflicts(); len(got) != 0 {
			t.Errorf("conflicts = %v, want none", got)
		}
	})
}

// A multi-project run: the global file is judged against EVERY slug drained (it
// is loaded by all of them, so it is only wrong when it agrees with none), while
// each project's own workdir is judged against that project alone.
func TestGlobalFileIsJudgedAgainstEveryProjectDrained(t *testing.T) {
	configDir := t.TempDir()
	writeAmbientFile(t, filepath.Join(configDir, "opencode.json"), clankerbarBlock("clankerbar"))
	c := opencodeRun(t, "ezyapp", t.TempDir(), configDir)
	c.Projects = []Project{{Slug: "ezyapp", WorkDir: t.TempDir()}, {Slug: "clankerbar", WorkDir: t.TempDir()}}

	if got := c.OpencodeAmbientConflicts(); len(got) != 0 {
		t.Errorf("conflicts = %v, want none: the global file names one of the projects this run drains", got)
	}

	writeAmbientFile(t, filepath.Join(configDir, "opencode.json"), clankerbarBlock("someone-else"))
	got := onlyConflict(t, c.OpencodeAmbientConflicts())
	if got.Want != "clankerbar, ezyapp" {
		t.Errorf("want = %q, want both drained slugs named", got.Want)
	}
}

// JSONC is what opencode's own config is written in, and the global file is
// conventionally `.jsonc`. A strict parse fails on it, which would have reported
// "nothing to see here" for exactly the file that caused this.
func TestJSONCParsesWhereStrictJSONWouldNot(t *testing.T) {
	body := `{
  // a line comment holding a decoy: https://clankerbar.com/mcp/decoy
  /* and a block comment
     spanning lines */
  "mcp": {
    "clankerbar": {
      "type": "remote",
      "url": "https://clankerbar.com/mcp/real", // trailing line comment
    },
  },
}`
	path := writeAmbientFile(t, filepath.Join(t.TempDir(), "opencode.jsonc"), body)
	if got := opencodeConfigClankerbarSlug(path); got != "real" {
		t.Errorf("slug = %q, want \"real\" - comments, trailing commas, and a // inside a URL must all survive the strip", got)
	}
}

// The strip is string-aware, which is the whole difficulty: every clankerbar URL
// in one of these files contains "//".
func TestStripJSONCLeavesStringsAlone(t *testing.T) {
	in := `{"url":"https://clankerbar.com/mcp/x","q":"a /* b */ c"} // tail`
	out := string(stripJSONC([]byte(in)))
	if !strings.Contains(out, `https://clankerbar.com/mcp/x`) {
		t.Errorf("stripJSONC ate a URL: %q", out)
	}
	if !strings.Contains(out, `a /* b */ c`) {
		t.Errorf("stripJSONC ate a quoted comment-looking string: %q", out)
	}
	if strings.Contains(out, "// tail") {
		t.Errorf("stripJSONC left a real comment behind: %q", out)
	}
}

// The parser gap that made the check necessary is closed WITHOUT loosening the
// security gates: readMCPServers still discloses an entry the file claims is
// disabled, because those callers decide whether a credential may be sent and
// whether a local process may start, and "the file says it is off" is not a fact
// a gate should take on trust.
func TestDisabledEntriesAreStillDisclosedToTheSecurityGates(t *testing.T) {
	path := writeAmbientFile(t, filepath.Join(t.TempDir(), ".mcp.json"),
		`{"mcpServers":{"clankerbar":{"type":"http","url":"https://clankerbar.com/mcp/x","enabled":false}}}`)
	servers, err := readMCPServers(path)
	if err != nil {
		t.Fatalf("readMCPServers: %v", err)
	}
	if len(servers) != 1 || servers[0].name != "clankerbar" {
		t.Fatalf("servers = %+v, want the disabled entry still listed", servers)
	}
}

// ~/.opencode is opencode's OTHER global directory, read whatever config_dir
// says - a live 1.18.19 run logs `loading path=/Users/jason/.opencode/opencode.json`
// alongside the config_dir files. A block parked there reaches every session, so
// it is checked too.
func TestHomeOpencodeDirIsCheckedRegardlessOfConfigDir(t *testing.T) {
	c := opencodeRun(t, "ezyapp", t.TempDir(), t.TempDir())
	home := os.Getenv("HOME")
	writeAmbientFile(t, filepath.Join(home, ".opencode", "opencode.json"), clankerbarBlock("clankerbar"))

	got := onlyConflict(t, c.OpencodeAmbientConflicts())
	if got.Scope != "global" || got.Got != "clankerbar" {
		t.Errorf("conflict = %+v, want the ~/.opencode file flagged as a global one", got)
	}
}

// `config.json` is a global NAME ONLY. opencode reads it in its config
// directory; a project-level config.json is an ordinary file thousands of repos
// carry for something else, and reading those as opencode config would be a
// check that fires on strangers' data.
func TestConfigJSONIsAGlobalNameOnly(t *testing.T) {
	workdir := t.TempDir()
	writeAmbientFile(t, filepath.Join(workdir, "config.json"), clankerbarBlock("clankerbar"))
	if got := opencodeRun(t, "ezyapp", workdir, t.TempDir()).OpencodeAmbientConflicts(); len(got) != 0 {
		t.Errorf("conflicts = %v, want none: a workdir config.json is not an opencode config", got)
	}

	configDir := t.TempDir()
	writeAmbientFile(t, filepath.Join(configDir, "config.json"), clankerbarBlock("clankerbar"))
	if got := opencodeRun(t, "ezyapp", t.TempDir(), configDir).OpencodeAmbientConflicts(); len(got) != 1 {
		t.Errorf("conflicts = %v, want one: opencode DOES load config.json in its config dir", got)
	}
}

// $XDG_CONFIG_HOME/opencode (default ~/.config/opencode) is opencode's real
// global config directory, and OPENCODE_CONFIG_DIR does NOT move it - that
// variable only appends a directory to a later merge layer. So the scan cannot
// be "whatever config_dir names": a run with config_dir unset, or pointed
// elsewhere, still loads this file into every session (CLA-441 review).
func TestGlobalScanCoversXDGConfigHomeWithNoConfigDir(t *testing.T) {
	c := opencodeRun(t, "ezyapp", t.TempDir(), "") // no config_dir at all
	xdg := os.Getenv("XDG_CONFIG_HOME")
	writeAmbientFile(t, filepath.Join(xdg, "opencode", "opencode.jsonc"), clankerbarBlock("clankerbar"))

	got := onlyConflict(t, c.OpencodeAmbientConflicts())
	if got.Scope != "global" || got.Got != "clankerbar" {
		t.Errorf("conflict = %+v, want the XDG global file flagged with no config_dir set", got)
	}
}

// ...and its default location when XDG_CONFIG_HOME is unset.
func TestGlobalScanCoversDotConfigWhenXDGIsUnset(t *testing.T) {
	c := opencodeRun(t, "ezyapp", t.TempDir(), "")
	t.Setenv("XDG_CONFIG_HOME", "")
	writeAmbientFile(t, filepath.Join(os.Getenv("HOME"), ".config", "opencode", "opencode.json"), clankerbarBlock("clankerbar"))

	got := onlyConflict(t, c.OpencodeAmbientConflicts())
	if got.Scope != "global" {
		t.Errorf("conflict = %+v, want ~/.config/opencode checked when XDG_CONFIG_HOME is unset", got)
	}
}

// A discovered opencode config is merged into every session that loads it, and
// checkDiscoveredMCPConfig's refusal never sees one - it gates only the file
// mcp_config_path resolved to. So the keys that decide what a session IS get
// reported through the same channel.
//
// `permission` is on that list, against the plausible reading that it cannot
// bite. OPENCODE_PERMISSION is applied after every config layer, but the merge
// is per key and the flatten is in INSERTION order, so a file declaring
// `permission.read` moves `read` ahead of our sorted-first `*` catch-all and the
// catch-all matches LAST - a total read denial, which is the CLA-441 wall
// itself (CLA-441 second review).
func TestAmbientConfigDeclaringSessionShapingKeysIsReported(t *testing.T) {
	configDir := t.TempDir()
	writeAmbientFile(t, filepath.Join(configDir, "opencode.json"),
		`{"permission":{"read":{"*.env":"ask"}},"plugin":["./evil.js"],"agent":{"build":{"mode":"all"}},`+
			`"mcp":{"clankerbar":{"url":"https://clankerbar.com/mcp/ezyapp"},"local":{"command":["node","serve.js"]}}}`)
	c := opencodeRun(t, "ezyapp", t.TempDir(), configDir)

	got := onlyConflict(t, c.OpencodeAmbientConflicts()) // the slug agrees, so only the override finding
	if len(got.Overrides) != 4 {
		t.Fatalf("overrides = %v, want permission, plugin, agent and the local-process server", got.Overrides)
	}
	for _, want := range []string{"permission", "plugin", "agent", "local"} {
		if !strings.Contains(got.String(), want) {
			t.Errorf("message must name %q: %q", want, got.String())
		}
	}
}

// The noise cases. Each of these was on the operator's own machine while this
// was being written, and a check that fires on them is one they learn to skim.
func TestSessionShapingReportIgnoresPowerlessDeclarations(t *testing.T) {
	t.Run("agent that only picks a model", func(t *testing.T) {
		configDir := t.TempDir()
		// The operator's real global config: the whole agent block chooses a
		// cheap model for generating session titles.
		writeAmbientFile(t, filepath.Join(configDir, "opencode.jsonc"),
			`{"agent":{"title":{"model":"opencode-go/deepseek-v4-flash"}},"mcp":{"clankerbar":{"url":"https://clankerbar.com/mcp/ezyapp"}}}`)
		if got := opencodeRun(t, "ezyapp", t.TempDir(), configDir).OpencodeAmbientConflicts(); len(got) != 0 {
			t.Errorf("conflicts = %v, want none: a model choice is cost and style, not authority", got)
		}
	})
	t.Run("disabled local-process server", func(t *testing.T) {
		configDir := t.TempDir()
		writeAmbientFile(t, filepath.Join(configDir, "opencode.json"),
			`{"mcp":{"local":{"command":["node","serve.js"],"enabled":false},"clankerbar":{"url":"https://clankerbar.com/mcp/ezyapp"}}}`)
		if got := opencodeRun(t, "ezyapp", t.TempDir(), configDir).OpencodeAmbientConflicts(); len(got) != 0 {
			t.Errorf("conflicts = %v, want none: opencode does not start a disabled server", got)
		}
	})
	t.Run("an agent block this cannot parse is reported anyway", func(t *testing.T) {
		configDir := t.TempDir()
		writeAmbientFile(t, filepath.Join(configDir, "opencode.json"),
			`{"agent":["build"],"mcp":{"clankerbar":{"url":"https://clankerbar.com/mcp/ezyapp"}}}`)
		got := onlyConflict(t, opencodeRun(t, "ezyapp", t.TempDir(), configDir).OpencodeAmbientConflicts())
		if len(got.Overrides) != 1 {
			t.Errorf("overrides = %v, want the unparseable block reported - the shape nobody modelled is the one nobody has looked at", got.Overrides)
		}
	})
}

// CLA-541: an opencode2 run gets the same ambient-config audit as an opencode
// one (spawnsOpencode covers both lines), it scans the v2 global config dir
// (~/.config/opencode2 — the file an operator actually edits for the beta),
// and the v2 `mcp.servers` dialect parses (verified against beta-18314,
// docs/opencode2.md). The run's own slug is derived through the same dialect.
func opencode2Run(t *testing.T, slug, workdir string) *Config {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	mcp := writeAmbientFile(t, filepath.Join(t.TempDir(), "opencode2-mcp.json"),
		`{"mcp":{"servers":{"clankerbar":{"type":"http","url":"https://clankerbar.com/mcp/`+slug+`"}}}}`)
	return &Config{
		Harness:       "opencode2",
		Prompt:        "Work the next backlog item.",
		WorkDir:       workdir,
		MCPConfigPath: mcp,
	}
}

func TestOpencode2RunScansItsGlobalDirAndDialect(t *testing.T) {
	home := t.TempDir()
	cfg := opencode2Run(t, "proj", t.TempDir())
	t.Setenv("HOME", home)
	// The v2 global config dir names a DIFFERENT project in the beta's own
	// mcp.servers dialect.
	writeAmbientFile(t, filepath.Join(home, ".config/opencode2/opencode.json"),
		`{"mcp":{"servers":{"clankerbar":{"type":"http","url":"https://clankerbar.com/mcp/other"}}}}`)
	got := cfg.OpencodeAmbientConflicts()
	c := onlyConflict(t, got)
	if c.Got != "other" || c.Want != "proj" {
		t.Errorf("conflict = %+v, want other vs proj", c)
	}
	if c.Scope != "global" {
		t.Errorf("scope = %q, want global", c.Scope)
	}
}

func TestOpencode2GlobalDirIsNotScannedForV1Runs(t *testing.T) {
	// The v2 config dir is scanned for opencode2 runs only — a v1 run's audit
	// must not send the operator to edit ~/.config/opencode2, a file no v1
	// session loads.
	cfg := opencodeRun(t, "proj", t.TempDir(), "")
	writeAmbientFile(t, filepath.Join(t.TempDir(), ".config/opencode2/opencode.json"),
		`{"mcp":{"servers":{"clankerbar":{"type":"http","url":"https://clankerbar.com/mcp/other"}}}}`)
	if got := cfg.OpencodeAmbientConflicts(); len(got) != 0 {
		t.Errorf("v1 run flagged a v2-only config: %+v", got)
	}
}

func TestOpencode2RunScansDeclaredConfigDir(t *testing.T) {
	cfg := opencode2Run(t, "proj", t.TempDir())
	dir := t.TempDir()
	cfg.Harnesses = map[string]HarnessConfig{"opencode2": {ConfigDir: dir}}
	writeAmbientFile(t, filepath.Join(dir, "opencode.json"),
		`{"mcp":{"servers":{"clankerbar":{"type":"http","url":"https://clankerbar.com/mcp/other"}}}}`)
	got := cfg.OpencodeAmbientConflicts()
	if c := onlyConflict(t, got); c.Got != "other" {
		t.Errorf("conflict = %+v, want the declared config_dir's redirect surfaced", c)
	}
}
