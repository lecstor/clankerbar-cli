package config

import (
	"sort"
	"strings"
)

// ModelForTier resolves one phase's tier to the model alias its session runs on.
//
// It is TOTAL — there is no input for which it fails — because the fallback chain
// is the design and not an error path: an unset tier, a tier the operator's map
// does not name, and a tier mapped to a blank string all resolve to the run-wide
// Model, and a blank Model resolves to "" (the harness's own default). That is
// what keeps a config written before tiers existed behaving exactly as it did.
//
// Everything is trimmed on the way through, which is the load-bearing half: a
// tier or a `"model": " "` that is blank-but-not-empty must resolve to the same
// nothing an absent one does, or a whitespace alias reaches the child's model
// flag and the session dies on an alias no provider has.
//
// `ok` is false only when a tier was NAMED and resolved to nothing — the typo
// case. The caller says so in the iteration log rather than refusing: a mistyped
// bucket should cost a log line and a run on the default model, not a stopped
// unattended run at 3am. It is deliberately NOT reported for an unset tier,
// which is the ordinary case and not a mistake.
func (c *Config) ModelForTier(tier string) (model string, ok bool) {
	return c.ModelForPhase(Phase{Tier: tier})
}

// HarnessFor names the harness one phase runs on: the phase's own, else the
// run-wide one. Total, and the answer for an unphased run is the run-wide
// harness, which is what every caller wants.
func (c *Config) HarnessFor(ph Phase) string {
	if h := strings.TrimSpace(ph.Harness); h != "" {
		return h
	}
	return c.Harness
}

// SessionFor resolves one harness's invocation fields.
//
// The run-wide fields fill the gaps for the TOP-LEVEL harness only. For any
// other harness the block stands alone: every field in it is a dialect of that
// harness (see HarnessConfig), so inheriting the top level's would hand a claude
// config dir and a Claude-shaped `.mcp.json` to opencode, which does not ignore
// the difference — it refuses to start. An unconfigured harness therefore
// resolves to empty fields, and each adapter falls back to its own ambient
// defaults, which is a session that may lack tools rather than one that dies on
// somebody else's config. Validate refuses the unconfigured case up front; this
// stays total so nothing downstream has a second error path.
func (c *Config) SessionFor(name string) HarnessConfig {
	hc := c.Harnesses[name]
	if name != c.Harness {
		return hc
	}
	// The run-wide twins are plain strings (they describe the top-level
	// harness); a harness block accepts the route-list form, so the gap-fill
	// widens a string into a one-route list rather than narrowing the block.
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

// stringMapToRoutes widens the run-wide tier map (plain strings) into a harness
// block's route-valued map. Nil stays nil so SessionFor's "the block named no
// map" test keeps distinguishing an absent map from an empty one.
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

// ModelForPhase resolves one phase's model: its own pinned alias (Phase.Model,
// which is how a review-panel member names the model it runs on), else its tier
// through the tier map of the harness THAT phase runs on, else that harness's
// default model.
//
// Same total fallback chain as the single-harness case it replaces — unset tier,
// unknown bucket, and a bucket mapped to blank all resolve to that harness's
// default model, and an empty default means the harness's own — with the
// per-harness lookup being the whole difference. A bucket name is portable and
// the alias in it is not, so "strong" on a claude phase and "strong" on an
// opencode phase are two different aliases under one policy word.
//
// `ok` is false only when a tier was NAMED and resolved to nothing, exactly as
// before: the caller logs it and runs on the default rather than stopping an
// unattended run over a typo. A phase on a harness with no tier map at all
// therefore reports the typo case for any tier it names, which is honest — the
// bucket really does resolve to nothing there.
func (c *Config) ModelForPhase(ph Phase) (model string, ok bool) {
	routes, ok := c.ModelRoutesForPhase(ph)
	if len(routes) == 0 {
		return "", ok
	}
	return routes[0], ok
}

// ModelRoutesForPhase is ModelForPhase with the whole ordered route list: the
// phase's own pinned routes, else its tier's routes through the tier map of the
// harness that phase runs on, else that harness's default routes. The first
// entry is exactly what ModelForPhase returns, so a single-route phase (every
// config written before CLA-782) resolves identically through both.
//
// The sibling exists because two callers need the whole list: the driver builds
// the adapter's fallback chain from it, and doctor's credential check must see
// every route the run may ask for, not only the first. `ok` keeps ModelForPhase's
// meaning: false only when a tier was NAMED and resolved to nothing.
func (c *Config) ModelRoutesForPhase(ph Phase) (routes []string, ok bool) {
	hc := c.SessionFor(c.HarnessFor(ph))
	dflt := hc.Model.List()
	// A pinned model is a complete answer and never a typo case: there is no
	// bucket to resolve, so nothing can resolve to nothing. Reported in the
	// spawn line as-is, which makes the panel's membership legible in the log.
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

// ModelIDsFor returns every model id the sessions on harnessName will actually
// run with, deduplicated and sorted: each effective phase that resolves to that
// harness (so the review panel and the unphased single phase are both included),
// with its model resolved through the same chain the driver uses - Phase.Model,
// else Phase.Tier through that harness's tier map, else that harness's default.
//
// It answers "which models is this config going to ask for", which is a question
// about the RUN rather than about any one field, and there is exactly one caller
// that needs it: `doctor`'s opencode credential check has to know which
// providers those models name before it can say whether a stored credential
// covers them (CLA-687). Reading `model` alone would miss the tier and panel
// paths, which is precisely where a config pins a provider the top-level field
// never mentions.
//
// An EMPTY id is skipped rather than reported: it means the harness's own
// default model, which this config does not name and cannot check. A caller that
// gets nothing back knows only that no session here names a model.
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

// ResolveMCPConfig picks the MCP config file one session gets, given the harness
// it runs on and the project scope it runs in.
//
// The file carries TWO facts at once — which project (its `/mcp/<slug>` URL) and
// which schema (the harness's) — so the rule has to satisfy both, and it lives
// here because two callers must agree on it: the driver building an invocation
// and `doctor` reporting on what that invocation will be. A preflight that
// resolved this differently from the loop would certify a file no session ever
// reads, which is the exact failure MCPConfigUse was added to end.
//
// projectMCP / projectPerHarness are the project's own fields (empty and nil for
// a single-project run). Precedence: the project's file FOR THIS HARNESS, else
// the project's file when this is the top-level harness — it is that harness's
// schema and no other's — else the harness block's own path.
//
// The winner is then narrowed by mcpConfigHandedTo (CLA-318): a file this
// harness cannot be handed (a .mcp.json under opencode, anything under codex,
// or "none") resolves to "" rather than falling through to a lower-precedence
// file. Falling through would hand a project's sessions the harness block's
// file, which in a multi-project run names some other project. The poll's slug
// reads the raw field instead (see BacklogEndpoint).
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

// panelMemberHarnesses is the harness each panel member resolves to when the
// panel actually expands, in member order. A member with no harness of its own
// inherits the review phase's (see reviewPhaseHarness). Empty when no panel is
// set or no phase would expand — an inert panel spawns nothing.
//
// PhaseHarnesses and SpawnedHarnesses use it to add the members to the harnesses
// a config DECLARES and SPAWNS while keeping their own iteration over c.Phases
// (so an absent panel answers exactly as it did before CLA-639).
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

// PhaseHarnesses lists every harness this config DECLARES — the run-wide one and
// every phase's — deduplicated, with the run-wide one first and the rest in
// sequence order.
//
// Callers are the ones that must reason about ALL of them rather than the
// top-level name alone: `doctor` checking binaries, config dirs and permission
// policy, and Validate checking that each is registered and configured. Reporting
// on `harness` alone is how a mixed-harness config earns a green preflight and
// then dies on its second phase.
//
// **Declared, not spawned** — and the difference matters, so pick deliberately.
// A declared harness must be USABLE whether or not today's phases reach it, which
// is why every caller above wants the run-wide one included. For the other
// question — what will this run actually do — use SpawnedHarnesses below, which
// omits a run-wide harness that every phase overrides. Asking this one there
// produces confidently false statements about a harness no session runs on.
func (c *Config) PhaseHarnesses() []string {
	out := []string{c.Harness}
	seen := map[string]bool{c.Harness: true}
	// The configured phases, exactly as before the panel existed. The panel's
	// member harnesses are added on top (CLA-639): they are declared by the
	// config and will be spawned, and a panel whose member harnesses were
	// invisible here would be reported as configured for a harness it never uses
	// while the one it does use went unchecked.
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
	// The conflict-only session's own harness (CLA-759), when the block names one
	// that is not already listed. It is a harness this config can spawn a session
	// on, so both questions below have to see it: doctor checks a DECLARED
	// harness's binary, config dir and permission policy, and the spawn question
	// must know the supervisor will really start one there.
	if h := c.conflictHarness(); h != "" && !seen[h] {
		seen[h] = true
		out = append(out, h)
	}
	return out
}

// SpawnedHarnesses lists every harness this config will actually START A SESSION
// on: the run-wide one for an unphased run, and otherwise exactly the harnesses
// its phases resolve to — plus the conflict-only session's own harness (CLA-759)
// when the `conflict` block names a different one, because the supervisor starts
// a real session there on a conflicted approved task. Deduplicated, in sequence order.
//
// It differs from PhaseHarnesses in one shape, and the difference is the whole
// reason it exists: when EVERY phase names its own harness, the run-wide
// `harness` field is DECLARED but never spawned - `Validate` does not refuse
// that, and the only two spawn sites in the driver (the phase's Invoke and its
// supervised-wait Probe) are both phase-driven. PhaseHarnesses seeds c.Harness
// unconditionally, which is right for the questions IT answers - is every
// declared harness registered and configured, is its binary on the PATH - since
// a declared harness must be usable whether or not today's phases reach it.
//
// It is wrong for any question of the form "what is true of the sessions this run
// will actually run", which is every inert-dial judgement in `doctor`'s budget
// check. Asked of PhaseHarnesses, those get three separate false statements out
// of a config whose phases all override: a live cost ceiling called INERT because
// a never-spawned codex is cost-blind; an unreachable per_harness block for that
// codex counted as a live ceiling; and an inert `max_session_wall_clock` passed
// over in silence because a never-spawned opencode would have honoured it.
//
// So: ask PhaseHarnesses what is CONFIGURED, and SpawnedHarnesses what will RUN.
func (c *Config) SpawnedHarnesses() []string {
	// No phases means the synthesized single phase of EffectivePhases, which runs
	// on the run-wide harness - so there the two are the same answer. A panel
	// beside an unphased run is inert (there is no sequence to multiply), so it
	// adds nothing here either.
	if len(c.Phases) == 0 {
		out := []string{c.Harness}
		if h := c.conflictHarness(); h != "" && h != c.Harness {
			out = append(out, h)
		}
		return out
	}
	var out []string
	seen := map[string]bool{}
	// The configured phases first, exactly as before the panel existed: an
	// absent panel must answer byte-for-byte as it did at 5805722 (including a
	// verify phase that names no harness resolving to the run-wide one, not to
	// the fix phase's). The panel's members are added when one actually expands
	// (CLA-639), because those are sessions this run really starts.
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
	// The conflict-only session's own harness (CLA-759), when the block names one
	// that is not already listed: the supervisor really starts a session there on
	// a conflicted approved task, so this list must know about it.
	if h := c.conflictHarness(); h != "" && !seen[h] {
		seen[h] = true
		out = append(out, h)
	}
	return out
}
