package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// --- 6. session workdirs -----------------------------------------------------

// agentInstructionFiles are the names a harness reads a project's standing instructions from, in
// the session's cwd. AGENTS.md is the cross-tool convention and CLAUDE.md the Claude Code one;
// either is enough to prove the operator has oriented the sessions that start here.
var agentInstructionFiles = []string{"AGENTS.md", "CLAUDE.md"}

// checkSessions inspects the directory each project's sessions are spawned in. A harness reads its
// instruction file, its skills and its project settings from the session's cwd AND UPWARD — never
// from repos below it.
func checkSessions(cfg *config.Config) []check {
	harnesses := cfg.PhaseHarnesses()
	suffix := func(h string) string {
		if len(harnesses) == 1 {
			return ""
		}
		return ":" + h
	}
	if len(cfg.Projects) == 0 {
		out := make([]check, 0, len(harnesses))
		for _, h := range harnesses {
			c := sessionCheck("workdir"+bracket(suffix(h)), cfg.WorkDir, cfg.ResolveMCPConfig(h, "", nil), h)
			out = append(out, noteWithheldMCPConfig(c, cfg.WithheldMCPConfig(h, nil), h))
		}
		return out
	}
	out := make([]check, 0, len(cfg.Projects)*len(harnesses))
	for _, p := range cfg.Projects {
		for _, h := range harnesses {
			c := sessionCheck("workdir["+p.Slug+suffix(h)+"]", projectWorkDir(cfg, p),
				cfg.ResolveMCPConfig(h, projectMCPConfig(cfg, p), p.MCPConfigPaths), h)
			out = append(out, noteWithheldMCPConfig(c, cfg.WithheldMCPConfig(h, &p), h))
		}
	}
	return out
}

// noteWithheldMCPConfig raises a PASS to WARN when the operator named an MCP config that the
// hand-off gate withholds from harnessName: the session starts without it and the config reads as
// if it were in force. An existing WARN gains the finding; a FAIL stands and carries it as info.
func noteWithheldMCPConfig(c check, path, harnessName string) check {
	if path == "" {
		return c
	}
	detail := "mcp_config_path " + path + " is not being handed to " + harnessName + " sessions"
	remedy := "codex ignores mcp_config_path (its servers come from [mcp_servers] in config.toml under CODEX_HOME)"
	if harnessName != "codex" {
		remedy = "a file named .mcp.json is Claude-shaped and is withheld from " + harnessName + "; name a config this harness reads instead, per project in projects[].mcp_config_paths." + harnessName + " or per harness in harnesses." + harnessName + ".mcp_config_path"
	}
	switch c.status {
	case pass:
		c.status = warn
		c.detail = detail
		c.remedy = remedy
		return c
	case warn:
		c.detail += "; " + detail
		c.remedy += "; " + remedy
		return c
	}
	c.info = append(c.info, detail+" - "+remedy)
	return c
}

// bracket wraps a non-empty qualifier in the [] the check names use, and leaves an empty one alone
// — so "workdir" stays "workdir" on a single-harness run.
func bracket(s string) string {
	if s == "" {
		return ""
	}
	return "[" + strings.TrimPrefix(s, ":") + "]"
}

// projectWorkDir and projectMCPConfig resolve a project entry EXACTLY as loop.Driver.invocation
// does — the project's own value, falling back to the top-level one.
func projectWorkDir(cfg *config.Config, p config.Project) string {
	if p.WorkDir != "" {
		return p.WorkDir
	}
	return cfg.WorkDir
}

func projectMCPConfig(cfg *config.Config, p config.Project) string {
	if p.MCPConfigPath != "" {
		return p.MCPConfigPath
	}
	return cfg.MCPConfigPath
}

// mcpConfigIsClaudeShaped reports whether the file at path declares Claude's `mcpServers` block -
// the shape config.Validate auto-discovers from a workdir's `.mcp.json`. An empty path, an
// unreadable file or one that will not parse all return false, because each of those is already
// someone else's check (config.Validate fails closed on an unparseable MCP config) and a preflight
// that guesses at a file it could not read is how this arm went wrong in the first place.
func mcpConfigIsClaudeShaped(path string) bool {
	if path == "" {
		return false
	}
	// Validate has already expanded a leading ~, so this is the same path the harness will be handed.
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return false
	}
	_, ok := top["mcpServers"]
	return ok
}

// mcpConfigNotCheckedNote is the workdir check's stand-in for a verdict on the `.mcp.json`, printed
// on every status it can still reach - so a PASS never reads as "the clankerbar wiring is there".
// The harness supplies the second half, because where a session really gets its MCP servers is a
// fact about the adapter and belongs next to the code that proves it.
func mcpConfigNotCheckedNote(use harness.MCPConfigUse) string {
	return ".mcp.json not checked: " + use.Note
}

func sessionCheck(name, dir, mcpConfigPath, harnessName string) check {
	c := check{name: name}

	resolved := dir
	if resolved == "" {
		resolved = "."
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		// The harness cannot start in a directory that is not there, so every iteration for this project
		// would die on spawn.
		c.status = fail
		c.detail = workdirLabel(dir) + " does not resolve: " + err.Error()
		c.remedy = "create it, or point this project's workdir at a real checkout"
		return c
	}
	if !fi.IsDir() {
		c.status = fail
		c.detail = workdirLabel(dir) + " is not a directory"
		c.remedy = "point the workdir at a directory, not a file"
		return c
	}

	parent := isMultiRepoParent(resolved)

	// The `.mcp.json` arm, and what the configured harness makes of that file.
	adapter, err := harness.Get(harnessName)
	if err != nil {
		// config.Validate rejects an unregistered harness long before doctor runs, so this is all but
		// unreachable in practice.
		c.status = warn
		c.detail = workdirLabel(dir) + ": " + err.Error()
		c.remedy = "set harness to one of: " + strings.Join(harness.Names(), ", ")
		return c
	}
	use := adapter.MCPConfigUse()

	switch use.Schema {
	case harness.MCPConfigUnused:
		// The file is inert for this harness. Say the exclusion out loud and carry it onto whatever
		// verdict follows, rather than reporting on it either way.
		c.info = append(c.info, mcpConfigNotCheckedNote(use))

	case harness.MCPConfigNative:
		// The path IS handed over, read in the harness's own schema. A Claude-shaped file wires nothing
		// here (and stricter binaries refuse to start on it), so it FAILs rather than WARNs.
		if mcpConfigIsClaudeShaped(mcpConfigPath) {
			c.status = fail
			c.detail = mcpConfigPath + " is a Claude-shaped .mcp.json (`mcpServers`), which " + harnessName + " cannot read"
			// Naming a config the harness can read is the remedy that wires tools back; "none"
			// (config.MCPConfigNone) is the explicit opt-out.
			c.remedy = "point mcp_config_path at a " + harnessName + " config - " + use.Note
			return c
		}
		// When a path IS configured, the file is the statement - and doctor can now read the opencode
		// schema well enough to verify the statement says clankerbar.
		if mcpConfigPath != "" {
			if url, ok := config.MCPClankerbar(mcpConfigPath); ok {
				server := url
				if server == "" {
					server = "(local command)"
				}
				c.info = append(c.info, "clankerbar MCP server: "+server+" ("+mcpConfigPath+")")
			} else {
				c.status = fail
				c.detail = mcpConfigPath + " does not declare a usable clankerbar MCP server"
				c.remedy = "name an entry \"clankerbar\" (or one referencing CLANKERBAR_API_KEY) in that file, not \"enabled\": false - sessions spawned with it would have no clankerbar tools"
				return c
			}
		} else {
			// Nothing configured: the harness's own config dir legitimately carries the server, and doctor
			// cannot parse that schema - the caveat stands only for this case.
			c.info = append(c.info, mcpConfigNotCheckedNote(use))
		}

	case harness.MCPConfigClaudeJSON:
		// A session with no .mcp.json reaching it gets no clankerbar tools at all — it starts, burns
		// tokens, and cannot see the backlog.
		if mcpConfigPath == "" {
			c.status = warn
			if parent {
				c.detail = workdirLabel(dir) + " looks like a multi-repo parent and has no .mcp.json"
			} else {
				c.detail = workdirLabel(dir) + " has no .mcp.json"
			}
			c.remedy = "add an .mcp.json there (or set mcp_config_path) — sessions spawned here would have no clankerbar tools; if mcp_config_path is \"none\", that opt-out is why"
			return c
		}

	default:
		// A schema added to harness but not taught to doctor. Go will not make this a compile error, so
		// it is made LOUD instead of silent - the whole point of CLA-263 is that this arm must never pass
		// a workdir on a premise it has not checked.
		c.status = warn
		c.detail = workdirLabel(dir) + ": doctor cannot judge mcp_config_path for harness " + harnessName
		c.remedy = "teach sessionCheck this harness's MCPConfigUse schema before trusting a green workdir here"
		return c
	}

	if marker := firstMarker(resolved, agentInstructionFiles); marker == "" {
		c.status = warn
		c.detail = workdirLabel(dir) + " has no agent-instructions file (" + strings.Join(agentInstructionFiles, " / ") + ")"
		if parent {
			c.remedy = "add one here naming each repo below and where its protocol lives — a session started in a multi-repo parent loads nothing from the repos under it — " + setupSkillSection(1, "agent-instructions file")
		} else {
			c.remedy = "add one so unattended sessions read the project's conventions instead of inferring them — " + setupSkillSection(1, "agent-instructions file")
		}
		return c
	}

	c.status = pass
	c.detail = resolved + " (" + firstMarker(resolved, agentInstructionFiles) + ")"
	if parent {
		c.detail += ", multi-repo parent"
	}
	return c
}

// isMultiRepoParent reports whether dir is not itself a checkout but holds two or more of them —
// the `~/dev` shape, where a session's cwd is a parent rather than a repo. Two is the threshold
// because a single nested checkout is more likely a vendored dependency than a workspace layout.
func isMultiRepoParent(dir string) bool {
	if dir == "" {
		dir = "."
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	repos := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, entry.Name(), ".git")); err == nil {
			repos++
			if repos >= 2 {
				return true
			}
		}
	}
	return false
}

func workdirLabel(dir string) string {
	if dir == "" {
		return "the current directory"
	}
	return dir
}
