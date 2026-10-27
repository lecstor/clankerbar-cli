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

// agentInstructionFiles are the names a harness reads a project's standing
// instructions from, in the session's cwd. AGENTS.md is the cross-tool
// convention and CLAUDE.md the Claude Code one; either is enough to prove the
// operator has oriented the sessions that start here.
var agentInstructionFiles = []string{"AGENTS.md", "CLAUDE.md"}

// checkSessions inspects the directory each project's sessions are spawned in.
// ONE CHECK PER PROJECT, for the same reason the backlog check is per project: a
// multi-project instance can have one queue pointed somewhere that teaches its
// sessions nothing while the others are fine, and an aggregate line would hide
// exactly that.
//
// The failure this exists for is silent and expensive rather than loud. A
// harness reads its instruction file, its skills and its project settings from
// the session's cwd AND UPWARD — never from repos below it. Spawn in a multi-repo
// parent and every session starts with no protocol, no conventions and no
// permissions beyond the ambient ones, then spends its opening minutes
// rediscovering the layout. It still works, which is why nobody notices for
// thirty iterations.
// checkSessions reports on each session the loop will spawn: its workdir, and
// what the harness running there makes of the MCP config it will be handed.
//
// One check per project PER HARNESS (CLA-366), because both axes change the
// answer: the project decides which file (its `/mcp/<slug>`) and the harness
// decides what that file MEANS — a Claude-shaped `.mcp.json` is the session's
// tools for one adapter and a refusal to start for another. Qualified names only
// when a second harness is in play, so a single-harness report is unchanged.
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
			out = append(out, sessionCheck("workdir"+bracket(suffix(h)), cfg.WorkDir, cfg.ResolveMCPConfig(h, "", nil), h))
		}
		return out
	}
	out := make([]check, 0, len(cfg.Projects)*len(harnesses))
	for _, p := range cfg.Projects {
		for _, h := range harnesses {
			out = append(out, sessionCheck("workdir["+p.Slug+suffix(h)+"]", projectWorkDir(cfg, p),
				cfg.ResolveMCPConfig(h, projectMCPConfig(cfg, p), p.MCPConfigPaths), h))
		}
	}
	return out
}

// bracket wraps a non-empty qualifier in the [] the check names use, and leaves
// an empty one alone — so "workdir" stays "workdir" on a single-harness run.
func bracket(s string) string {
	if s == "" {
		return ""
	}
	return "[" + strings.TrimPrefix(s, ":") + "]"
}

// projectWorkDir and projectMCPConfig resolve a project entry EXACTLY as
// loop.Driver.invocation does — the project's own value, falling back to the
// top-level one. Doctor answering a differently-resolved question is the whole
// failure mode it exists to prevent: read p.WorkDir raw and an empty entry
// resolves to the current directory, so a config like
//
//	{"workdir":"~/dev","projects":[{"slug":"acme"}]}
//
// would be diagnosed against wherever the operator happened to run doctor. That
// never FAILs (a cwd always resolves), so it reports green for a directory the
// loop will never use — the dangerous direction.
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

// mcpConfigIsClaudeShaped reports whether the file at path declares Claude's
// `mcpServers` block - the shape config.Validate auto-discovers from a workdir's
// `.mcp.json`.
//
// It answers only on POSITIVE evidence. An empty path, an unreadable file or one
// that will not parse all return false, because each of those is already someone
// else's check (config.Validate fails closed on an unparseable MCP config) and a
// preflight that guesses at a file it could not read is how this arm went wrong in
// the first place.
func mcpConfigIsClaudeShaped(path string) bool {
	if path == "" {
		return false
	}
	// Validate has already expanded a leading ~, so this is the same path the
	// harness will be handed.
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

// mcpConfigNotCheckedNote is the workdir check's stand-in for a verdict on the
// `.mcp.json`, printed on every status it can still reach - so a PASS never reads
// as "the clankerbar wiring is there". The harness supplies the second half,
// because where a session really gets its MCP servers is a fact about the adapter
// and belongs next to the code that proves it.
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
		// The harness cannot start in a directory that is not there, so every
		// iteration for this project would die on spawn.
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
	//
	// The question this used to ask was "does the adapter HAND the path to the
	// session", and that question certifies opencode, which does hand it over and
	// then dies on it. The question it asks now is the one the check actually rests
	// on: does the file MEAN anything to this harness. Only the adapter knows, so
	// only the adapter is asked (harness.Adapter.MCPConfigUse) - see CLA-263.
	adapter, err := harness.Get(harnessName)
	if err != nil {
		// config.Validate rejects an unregistered harness long before doctor runs,
		// so this is all but unreachable in practice. It refuses to guess anyway:
		// assuming a default for an unknown harness is the precise shape of the bug
		// this arm is being fixed for.
		c.status = warn
		c.detail = workdirLabel(dir) + ": " + err.Error()
		c.remedy = "set harness to one of: " + strings.Join(harness.Names(), ", ")
		return c
	}
	use := adapter.MCPConfigUse()

	switch use.Schema {
	case harness.MCPConfigUnused:
		// The file is inert for this harness. Say the exclusion out loud and carry
		// it onto whatever verdict follows, rather than reporting on it either way.
		c.info = append(c.info, mcpConfigNotCheckedNote(use))

	case harness.MCPConfigNative:
		// The path IS handed over, read in the harness's own schema. A
		// Claude-shaped file wires nothing here (and stricter binaries refuse to
		// start on it), so it FAILs rather than WARNs. Since CLA-318 a file named
		// .mcp.json never reaches this arm (config.mcpConfigHandedTo), so only an
		// operator-named file of another name can.
		if mcpConfigIsClaudeShaped(mcpConfigPath) {
			c.status = fail
			c.detail = mcpConfigPath + " is a Claude-shaped .mcp.json (`mcpServers`), which " + harnessName + " cannot read"
			// Naming a config the harness can read is the remedy that wires tools
			// back; "none" (config.MCPConfigNone) is the explicit opt-out.
			c.remedy = "point mcp_config_path at a " + harnessName + " config - " + use.Note
			return c
		}
		// When a path IS configured, the file is the statement - and doctor can
		// now read the opencode schema well enough to verify the statement says
		// clankerbar (CLA-448). The old "not checked" caveat existed because the
		// schema was unreadable; the opencode file is JSON like any other, and a
		// configured file that is silent about clankerbar is exactly the
		// tool-less-session config that burned CLA-351 and CLA-377 to parked.
		// FAIL, not WARN: a green workdir must not certify a session that cannot
		// see the backlog.
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
			// Nothing configured: the harness's own config dir legitimately carries
			// the server, and doctor cannot parse that schema - the caveat stands
			// only for this case (CLA-448).
			c.info = append(c.info, mcpConfigNotCheckedNote(use))
		}

	case harness.MCPConfigClaudeJSON:
		// A session with no .mcp.json reaching it gets no clankerbar tools at all — it
		// starts, burns tokens, and cannot see the backlog. config.Validate has already
		// defaulted this to <workdir>/.mcp.json when that file exists, so an empty value
		// here means none was found and none was configured.
		//
		// This is NOT gated on the multi-repo-parent shape: a plain single-repo workdir
		// with no .mcp.json blinds its sessions just as completely. `parent` only
		// selects the wording, because that is the case an operator is most likely to
		// have reached by accident.
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
		// A schema added to harness but not taught to doctor. Go will not make this
		// a compile error, so it is made LOUD instead of silent - the whole point of
		// CLA-263 is that this arm must never pass a workdir on a premise it has not
		// checked.
		c.status = warn
		c.detail = workdirLabel(dir) + ": doctor cannot judge mcp_config_path for harness " + harnessName
		c.remedy = "teach sessionCheck this harness's MCPConfigUse schema before trusting a green workdir here"
		return c
	}

	if marker := firstMarker(resolved, agentInstructionFiles); marker == "" {
		c.status = warn
		c.detail = workdirLabel(dir) + " has no agent-instructions file (" + strings.Join(agentInstructionFiles, " / ") + ")"
		if parent {
			c.remedy = "add one here naming each repo below and where its protocol lives — a session started in a multi-repo parent loads nothing from the repos under it"
		} else {
			c.remedy = "add one so unattended sessions read the project's conventions instead of inferring them"
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

// isMultiRepoParent reports whether dir is not itself a checkout but holds two or
// more of them — the `~/dev` shape, where a session's cwd is a parent rather than
// a repo. Two is the threshold because a single nested checkout is more likely a
// vendored dependency than a workspace layout.
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
