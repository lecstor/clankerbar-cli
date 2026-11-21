package config

import (
	"sort"
	"strings"
)

// ModelForTier resolves one phase's tier to the model alias its session runs on. Everything is
// trimmed on the way through, which is the load-bearing half: a tier or a `"model": " "` that is
// blank-but-not-empty must resolve to the same nothing an absent one does, or a whitespace alias
// reaches the child's model flag and the session dies on an alias no provider has.
func (c *Config) ModelForTier(tier string) (model string, ok bool) {
	return c.ModelForPhase(Phase{Tier: tier})
}

// HarnessFor names the harness one phase runs on: the phase's own, else the run-wide one. Total,
// and the answer for an unphased run is the run-wide harness, which is what every caller wants.
func (c *Config) HarnessFor(ph Phase) string {
	if h := strings.TrimSpace(ph.Harness); h != "" {
		return h
	}
	return c.Harness
}

// SessionFor resolves one harness's invocation fields. The run-wide fields fill the gaps for the
// TOP-LEVEL harness only.
func (c *Config) SessionFor(name string) HarnessConfig {
	hc := c.Harnesses[name]
	if name != c.Harness {
		return hc
	}
	if len(hc.Model) == 0 && c.Model != "" {
		hc.Model = ModelRoutes{c.Model}
	}
	if hc.Models == nil {
		hc.Models = stringMapToRoutes(c.Models)
	}
	if hc.ConfigDir == "" {
		hc.ConfigDir = c.ConfigDir
	}
	if hc.MCPConfigPath == "" {
		hc.MCPConfigPath = c.MCPConfigPath
	}
	if hc.SettingsPath == "" {
		hc.SettingsPath = c.SettingsPath
	}
	return hc
}

// stringMapToRoutes widens the run-wide tier map (plain strings) into a harness block's
// route-valued map.
func stringMapToRoutes(m map[string]string) map[string]ModelRoutes {
	if m == nil {
		return nil
	}
	out := make(map[string]ModelRoutes, len(m))
	for k, v := range m {
		out[k] = ModelRoutes{v}
	}
	return out
}

// ModelForPhase resolves one phase's model: its own pinned alias (Phase.Model, which is how a
// review-panel member names the model it runs on), else its tier through the tier map of the
// harness THAT phase runs on, else that harness's default model. `ok` is false only when a tier was
// NAMED and resolved to nothing, exactly as before: the caller logs it and runs on the default
// rather than stopping an unattended run over a typo.
func (c *Config) ModelForPhase(ph Phase) (model string, ok bool) {
	routes, ok := c.ModelRoutesForPhase(ph)
	if len(routes) == 0 {
		return "", ok
	}
	return routes[0], ok
}

// ModelRoutesForPhase is ModelForPhase with the whole ordered route list: the phase's own pinned
// routes, else its tier's routes through the tier map of the harness that phase runs on, else that
// harness's default routes. The sibling exists because two callers need the whole list: the driver
// builds the adapter's fallback chain from it, and doctor's credential check must see every route
// the run may ask for, not only the first.
func (c *Config) ModelRoutesForPhase(ph Phase) (routes []string, ok bool) {
	hc := c.SessionFor(c.HarnessFor(ph))
	dflt := hc.Model.List()
	// A pinned model is a complete answer and never a typo case: there is no bucket to resolve, so
	// nothing can resolve to nothing.
	if m := ph.Model.List(); len(m) > 0 {
		return m, true
	}
	t := strings.TrimSpace(ph.Tier)
	if t == "" {
		return dflt, true
	}
	if alias := hc.Models[t].List(); len(alias) > 0 {
		return alias, true
	}
	return dflt, false
}

// ModelIDsFor returns every model id the sessions on harnessName will actually run with,
// deduplicated and sorted: each effective phase that resolves to that harness (so the review panel
// and the unphased single phase are both included), with its model resolved through the same chain
// the driver uses - Phase.Model, else Phase.Tier through that harness's tier map, else that
// harness's default. Reading `model` alone would miss the tier and panel paths, which is precisely
// where a config pins a provider the top-level field never mentions.
func (c *Config) ModelIDsFor(harnessName string) []string {
	seen := map[string]bool{}
	var out []string
	for _, ph := range c.EffectivePhases() {
		if c.HarnessFor(ph) != harnessName {
			continue
		}
		routes, _ := c.ModelRoutesForPhase(ph)
		for _, id := range routes {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// ResolveMCPConfig picks the MCP config file one session gets, given the harness it runs on and the
// project scope it runs in. The file carries TWO facts at once — which project (its `/mcp/<slug>`
// URL) and which schema (the harness's) — so the rule has to satisfy both, and it lives here
// because two callers must agree on it: the driver building an invocation and `doctor` reporting on
// what that invocation will be.
func (c *Config) ResolveMCPConfig(harnessName, projectMCP string, projectPerHarness map[string]string) string {
	p := projectPerHarness[harnessName]
	if p == "" && harnessName == c.Harness {
		p = projectMCP
	}
	if p == "" {
		p = c.SessionFor(harnessName).MCPConfigPath
	}
	return mcpConfigHandedTo(harnessName, p)
}

// panelMemberHarnesses is the harness each panel member resolves to when the panel actually
// expands, in member order. A member with no harness of its own inherits the review phase's (see
// reviewPhaseHarness).
func (c *Config) panelMemberHarnesses() []string {
	panel := c.EffectivePanel()
	if len(panel) == 0 || !c.expandsReviewPanel() {
		return nil
	}
	inherited := c.reviewPhaseHarness()
	out := make([]string, 0, len(panel))
	for _, m := range panel {
		h := strings.TrimSpace(m.Harness)
		if h == "" {
			h = inherited
		}
		out = append(out, h)
	}
	return out
}

// PhaseHarnesses lists every harness this config DECLARES — the run-wide one and every phase's
// — deduplicated, with the run-wide one first and the rest in sequence order. Callers are the
// ones that must reason about ALL of them rather than the top-level name alone: `doctor` checking
// binaries, config dirs and permission policy, and Validate checking that each is registered and
// configured.
func (c *Config) PhaseHarnesses() []string {
	out := []string{c.Harness}
	seen := map[string]bool{c.Harness: true}
	for _, ph := range c.Phases {
		h := c.HarnessFor(ph)
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	for _, h := range c.panelMemberHarnesses() {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	// The conflict-only session's own harness, when the block names one that is not already listed.
	if h := c.conflictHarness(); h != "" && !seen[h] {
		seen[h] = true
		out = append(out, h)
	}
	return out
}

// SpawnedHarnesses lists every harness this config will actually START A SESSION on: the run-wide
// one for an unphased run, and otherwise exactly the harnesses its phases resolve to — plus the
// conflict-only session's own harness when the `conflict` block names a different one, because the
// supervisor starts a real session there on a conflicted approved task. PhaseHarnesses seeds
// c.Harness unconditionally, which is right for the questions IT answers - is every declared
// harness registered and configured, is its binary on the PATH - since a declared harness must be
// usable whether or not today's phases reach it.
func (c *Config) SpawnedHarnesses() []string {
	if len(c.Phases) == 0 {
		out := []string{c.Harness}
		if h := c.conflictHarness(); h != "" && h != c.Harness {
			out = append(out, h)
		}
		return out
	}
	var out []string
	seen := map[string]bool{}
	// The configured phases first, exactly as before the panel existed: an absent panel must answer
	// byte-for-byte as it did at 5805722 (including a verify phase that names no harness resolving to
	// the run-wide one, not to the fix phase's).
	for _, ph := range c.Phases {
		h := c.HarnessFor(ph)
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	for _, h := range c.panelMemberHarnesses() {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	// The conflict-only session's own harness, when the block names one that is not already listed:
	// the supervisor really starts a session there on a conflicted approved task, so this list must
	// know about it.
	if h := c.conflictHarness(); h != "" && !seen[h] {
		seen[h] = true
		out = append(out, h)
	}
	return out
}
