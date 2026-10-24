package cli

import (
	"fmt"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// --- 8. mcp servers ----------------------------------------------------------

// checkMCPServers names the MCP entries that start a LOCAL PROCESS in every
// spawned session.
//
// `mcp_config_path` defaults to `<workdir>/.mcp.json` - a file inside a checkout,
// which the sessions themselves can write - and the harness starts every server
// it declares at MCP init, before any allow/deny rule is consulted. CLA-257
// constrains where that file may send the API key; nothing constrains what it may
// RUN, and nothing said so out loud. This says so.
//
// A WARN, never a FAIL: a local MCP server in a repo's .mcp.json is a normal,
// wanted thing, and failing the preflight over one would block runs that work.
// What an operator needs is to have seen the list once.
func checkMCPServers(cfg *config.Config) check {
	c := check{name: "mcp_servers"}
	local := cfg.LocalMCPServers()
	if len(local) == 0 && len(cfg.AllowLocalMCPServers) == 0 && !anyProjectAllowlists(cfg) {
		c.status = pass
		c.detail = "no MCP server starts a local process"
		return c
	}
	c.status = warn
	if len(local) == 0 {
		c.detail = "an allow_local_mcp_servers list admits named entries from discovered <workdir>/.mcp.json files"
	} else {
		c.detail = plural(len(local), "1 MCP server starts a local process", fmt.Sprintf("%d MCP servers start local processes", len(local))) + " in every session"
	}
	for _, s := range local {
		c.info = append(c.info, s.Name+": "+truncate(s.Command, 80)+"  ("+s.ConfigPath+")")
	}
	// The CLA-266 opt-out gets the same visibility as its CLA-310 sibling
	// (allow_unchecked_pr): a loose state nobody can see before it fires is not
	// an operator's choice, it is a surprise. A discovered file refuses every
	// command entry EXCEPT these names, so the list IS part of what runs.
	if names := cfg.AllowLocalMCPServers; len(names) > 0 {
		c.info = append(c.info, "allow_local_mcp_servers: "+strings.Join(names, ", ")+"  (admitted from any discovered <workdir>/.mcp.json)")
	}
	for _, p := range cfg.Projects {
		if names := p.AllowLocalMCPServers; len(names) > 0 {
			c.info = append(c.info, "projects["+p.Slug+"].allow_local_mcp_servers: "+strings.Join(names, ", "))
		}
	}
	c.remedy = "confirm you meant each of these - they run at session start, before any permission rule applies, and only allowlisted or explicitly-named configs are accepted now"
	return c
}

// checkOpencodeAmbientConfigs reports the opencode config files the driver never
// names but opencode merges anyway, whose `clankerbar` MCP server points at
// another project (CLA-441). The detection - and the reason it WARNS rather than
// failing the run - lives in config.OpencodeAmbientConflicts; this renders it.
//
// The PASS line says nothing about the mechanism: a check that explains a hazard
// nobody has is a line the operator learns to skip past.
func checkOpencodeAmbientConfigs(cfg *config.Config) check {
	c := check{name: "opencode_ambient_config", status: pass}
	conflicts := cfg.OpencodeAmbientConflicts()
	if len(conflicts) == 0 {
		c.detail = "no discovered opencode config redirects the clankerbar MCP server"
		return c
	}
	c.status = warn
	// The two kinds are counted and remedied SEPARATELY. They share a scan and
	// nothing else: one is a file naming the wrong backlog, mitigated for
	// spawned sessions by the content pin; the other is a file that shapes
	// what the session may do, which the pin does nothing about. A single
	// sentence covering both said the mitigation out loud on findings it does
	// not apply to (CLA-441 second review).
	var slugs, shaping int
	for _, cf := range conflicts {
		c.info = append(c.info, cf.String())
		if len(cf.Overrides) > 0 {
			shaping++
		} else {
			slugs++
		}
	}
	var details, remedies []string
	if slugs > 0 {
		details = append(details, plural(slugs,
			"1 opencode config names a different project's backlog",
			fmt.Sprintf("%d opencode configs name a different project's backlog", slugs)))
		remedies = append(remedies, "the wrong-backlog ones are mitigated for SPAWNED sessions by OPENCODE_CONFIG_CONTENT, but every INTERACTIVE "+
			"opencode session in that tree still gets the wrong one: rename the block (the operator's own global one is now "+
			"`clankerbar-interactive`), disable it, or point its url at the right slug")
	}
	if shaping > 0 {
		details = append(details, plural(shaping,
			"1 opencode config shapes what every session it loads into may do",
			fmt.Sprintf("%d opencode configs shape what every session they load into may do", shaping)))
		remedies = append(remedies, "the session-shaping ones are NOT mitigated by anything here - `mcp_config_path` files are refused for these keys, "+
			"a file opencode discovers is not: remove the key, or accept that every session started in that tree runs with it")
	}
	c.detail = strings.Join(details, "; ")
	c.remedy = strings.Join(remedies, "; ")
	return c
}

// anyProjectAllowlists reports whether any project sets its own
// allow_local_mcp_servers, so the pass line above does not hide a configured
// list just because no current entry starts a process.
func anyProjectAllowlists(cfg *config.Config) bool {
	for _, p := range cfg.Projects {
		if len(p.AllowLocalMCPServers) > 0 {
			return true
		}
	}
	return false
}
