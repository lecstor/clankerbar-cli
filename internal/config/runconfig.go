// Run-config consumption: the CLI side of the plane's stored execution-config document.
package config

import (
	"encoding/json"
	"fmt"
	"strings"
)

// RunConfigSchemaVersion is the newest `$schema_version` this build understands. A stored document
// carrying a HIGHER version is refused at both consume points (the loop's applyRunConfig and
// doctor's checkRunConfigs), loudly and keeping the previous config — never partially applied.
const RunConfigSchemaVersion = 1

// RunConfigHarnessBlock is the POLICY half of a per-harness block as the plane stores it. The
// machine-fit twins (`config_dir`, `mcp_config_path`, `settings_path`) are paths and wiring the
// plane never carries, so an overlay can only fill these two fields of an existing local block —
// it cannot hand a session somebody else's dialect.
type RunConfigHarnessBlock struct {
	Model  string            `json:"model"`
	Models map[string]string `json:"models"`
}

// RunConfigBudget mirrors Budget as the plane stores it: integer seconds for wall clock (Duration
// accepts a bare number), zero meaning unset exactly as in the local file.
type RunConfigBudget struct {
	MaxTokens        int                      `json:"max_tokens"`
	MaxCostUSD       float64                  `json:"max_cost_usd"`
	MaxWallClock     Duration                 `json:"max_wall_clock"`
	MaxSessionTokens int                      `json:"max_session_tokens"`
	PerHarness       map[string]HarnessBudget `json:"per_harness"`
}

// RunConfigEscalation mirrors Escalation. The rules stay mechanical globs and category names; the
// CLI evaluates them at review-spawn time exactly as when they came from the local file.
type RunConfigEscalation struct {
	PathRules     map[string]string `json:"path_rules"`
	CategoryRules map[string]string `json:"category_rules"`
}

// RunConfigPhase is the plane's stored phase shape (RunConfigPhase in the plane's shared schema),
// mapped onto Phase by RunConfigDoc.phases.
type RunConfigPhase struct {
	Name         string   `json:"name"`
	Prompt       string   `json:"prompt"`
	MaxTurns     int      `json:"max_turns"`
	MaxWallClock Duration `json:"max_wall_clock"`
	Tier         string   `json:"tier"`
	Model        string   `json:"model"`
	Harness      string   `json:"harness"`
	// ReviewMethod and MapsSession are this build's review-method knobs. They are decoded so a plane
	// whose stored schema learns them needs no second change here; RunConfigDocument refuses to EXPORT
	// them until that happens (the v1 schema refuses unknown phase keys), so today they can only
	// arrive from a hand-written document the plane accepted.
	ReviewMethod string              `json:"review_method"`
	MapsSession  *MapsSessionOptions `json:"maps_session"`
}

// RunConfigPanelMember is one stored review-panel member, mapped onto ReviewPanelMember. A member
// may set both `model` and `tier` (the alias wins on expansion), exactly as in the local file —
// the one-model rule applies to a configured PHASE, not to a member.
type RunConfigPanelMember struct {
	Name         string              `json:"name"`
	Harness      string              `json:"harness"`
	Model        string              `json:"model"`
	Tier         string              `json:"tier"`
	Optional     bool                `json:"optional,omitempty"`
	ReviewMethod string              `json:"review_method"`
	MapsSession  *MapsSessionOptions `json:"maps_session"`
}

// RunConfigEvents mirrors the plane's `events` block. The consumer is resolveRunConfigEvents on the
// plane; this side must fail closed on its own, and a stored `transcript: true` without activity is
// only ever read as off.
type RunConfigEvents struct {
	Activity   bool `json:"activity"`
	Transcript bool `json:"transcript"`
}

// RunConfigRole is one stored role block (`roles.planner`), mapped onto RoleAssignment. There is no
// `name`: a role is keyed by its own name, and a role's `prompt` is a custom brief, never a
// built-in selector.
type RunConfigRole struct {
	Harness          string   `json:"harness"`
	Model            string   `json:"model"`
	Tier             string   `json:"tier"`
	Prompt           string   `json:"prompt"`
	MaxTurns         int      `json:"max_turns"`
	MaxWallClock     Duration `json:"max_wall_clock"`
	MaxSessionTokens int      `json:"max_session_tokens"`
}

// RunConfigDoc mirrors the plane's stored execution-config document — only the keys this build
// consumes. A newer `$schema_version` is NOT covered by that rule — it means the known keys
// themselves may have changed meaning — so the consume points refuse it outright; see
// RunConfigSchemaVersion and SchemaNewer.
type RunConfigDoc struct {
	SchemaVersion          int                              `json:"$schema_version"`
	Harness                string                           `json:"harness"`
	Model                  string                           `json:"model"`
	Models                 map[string]string                `json:"models"`
	Harnesses              map[string]RunConfigHarnessBlock `json:"harnesses"`
	Prompt                 string                           `json:"prompt"`
	promptSet              bool                             // the `prompt` KEY was present (see UnmarshalJSON)
	Phases                 []RunConfigPhase                 `json:"phases"`
	ReviewPanel            []RunConfigPanelMember           `json:"review_panel"`
	ReviewPanelConcurrency int                              `json:"review_panel_concurrency"`
	MaxTurns               int                              `json:"max_turns"`
	MaxSessionWallClock    Duration                         `json:"max_session_wall_clock"`
	Budget                 *RunConfigBudget                 `json:"budget"`
	Escalation             *RunConfigEscalation             `json:"escalation"`
	// Events is the agent event channel's per-project opt-in. A nil pointer is an ABSENT key (both
	// dials off, the local rules); a present block — even one with both false — is the project
	// stating where the dials sit.
	Events *RunConfigEvents `json:"events"`

	// Roles is the sixth whole-section (studio-chat.md §1): `roles.planner` is the conversation
	// role's harness, model, brief and bounds. Only `planner` is consumed in v1.
	Roles map[string]RunConfigRole `json:"roles"`
}

// UnmarshalJSON decodes the document and records whether the `prompt` KEY was present, because the
// pipeline section is judged on key presence (decision 6bf1fa15): a document carrying `"prompt":
// ""` is a stored statement that clears the local pipeline, exactly like an empty non-nil `phases`
// array, and a string field cannot tell that from an absent key.
func (d *RunConfigDoc) UnmarshalJSON(data []byte) error {
	type docAlias RunConfigDoc // no methods: decoding into it cannot recurse here
	var a docAlias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	// A map probe, not a *string: `"prompt": null` still carries the key, and the section rule is
	// about the key, not the value.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	*d = RunConfigDoc(a)
	_, d.promptSet = probe["prompt"]
	return nil
}

// SchemaNewer reports whether the document carries a `$schema_version` newer than this build
// understands (see RunConfigSchemaVersion). The consume points check this BEFORE Empty: a
// newer-schema document that happens to set nothing else must still be refused loudly as a version
// mismatch, not waved through as "nothing consumable".
func (d *RunConfigDoc) SchemaNewer() bool {
	if d == nil {
		return false
	}
	return d.SchemaVersion > RunConfigSchemaVersion
}

// pipelineSet reports whether the stored document carries ANY key of the pipeline section (prompt,
// phases, review_panel), which is what makes the overlay replace all three: the section is one
// unit, exactly as the plane stores it (decision 6bf1fa15). `review_panel_concurrency` is NOT a
// section key (decision e1365721): a document carrying only that is not a pipeline statement at
// all, so the local pipeline stays untouched.
func (d *RunConfigDoc) pipelineSet() bool {
	if d == nil {
		return false
	}
	return d.promptSet || d.Prompt != "" || d.Phases != nil || d.ReviewPanel != nil
}

// Empty reports whether the document consumes to nothing: every field a stored document may set is
// absent. The plane's default document is exactly this — `{}` with a schema version — and it
// means "the local file rules".
func (d *RunConfigDoc) Empty() bool {
	if d == nil {
		return true
	}
	return d.Harness == "" && d.Model == "" && d.Models == nil && d.Harnesses == nil &&
		!d.pipelineSet() &&
		d.MaxTurns == 0 && d.MaxSessionWallClock == 0 &&
		d.Budget == nil && d.Escalation == nil && d.Events == nil && d.Roles == nil
}

// Clone returns a copy safe to overlay a run-config document onto: every map ApplyRunConfig can
// replace or mutate is copied one level deep, so the base config the driver shares across targets
// is never written through. Fields the overlay never touches (projects, repos, env, wiring) share
// backing storage — treat them read-only on the clone, which is all the drain path does.
func (c *Config) Clone() *Config {
	cp := *c
	cp.Models = cloneStrMap(c.Models)
	if c.Harnesses != nil {
		cp.Harnesses = make(map[string]HarnessConfig, len(c.Harnesses))
		for name, hc := range c.Harnesses {
			hc.Models = cloneRoutesMap(hc.Models)
			cp.Harnesses[name] = hc
		}
	}
	if c.Budget.PerHarness != nil {
		cp.Budget.PerHarness = make(map[string]HarnessBudget, len(c.Budget.PerHarness))
		for k, v := range c.Budget.PerHarness {
			cp.Budget.PerHarness[k] = v
		}
	}
	if c.Escalation.PathRules != nil {
		cp.Escalation.PathRules = cloneStrMap(c.Escalation.PathRules)
	}
	if c.Escalation.CategoryRules != nil {
		cp.Escalation.CategoryRules = cloneStrMap(c.Escalation.CategoryRules)
	}
	// Element values are written by Validate's per-project discovery, so the slice gets its own
	// backing array even though the overlay itself never touches projects.
	cp.Projects = append([]Project(nil), c.Projects...)
	// The pipeline is replaced wholesale by an overlay, so both slices get their own backing arrays:
	// appending to (or writing into) the clone's phases or panel must never reach the base config the
	// driver shares across targets, and an overlay on a clone must never alias the base.
	cp.Phases = append([]Phase(nil), c.Phases...)
	for i := range cp.Phases {
		cp.Phases[i].MapsSession = cloneMapsSession(cp.Phases[i].MapsSession)
	}
	cp.ReviewPanel = append([]ReviewPanelMember(nil), c.ReviewPanel...)
	for i := range cp.ReviewPanel {
		cp.ReviewPanel[i].MapsSession = cloneMapsSession(cp.ReviewPanel[i].MapsSession)
	}
	// The planner role is replaced wholesale by an overlay (a pointer), so the clone gets its own
	// block: a later apply on the clone must never rewrite the base config the driver shares across
	// targets.
	if c.Planner != nil {
		p := *c.Planner
		cp.Planner = &p
	}
	return &cp
}

// cloneMapsSession copies a maps-session options block so a clone's phases never alias the base
// config's. The block is read-only today, but the pipeline replacement and the panel expansion both
// hand pointers around, and a future write through one must not reach the config every target
// shares.
func cloneMapsSession(o *MapsSessionOptions) *MapsSessionOptions {
	if o == nil {
		return nil
	}
	cp := *o
	if cp.EmbedHunks != nil {
		e := *cp.EmbedHunks
		cp.EmbedHunks = &e
	}
	return &cp
}

func cloneStrMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ApplyRunConfig overlays a stored document onto c IN PLACE: a field the document sets wins, a
// field it omits keeps the local value — except the `events` and `roles` sections, which belong
// to the stored document alone (an absent block is both events dials off, fail closed, and no
// planner, fail closed: no local file may turn either on). Callers re-run Validate afterwards: an
// overlay CAN produce a combination the local file alone could not (a stored harness name nobody
// registered), and a refused combination must be loud, not silently half-applied.
func (c *Config) ApplyRunConfig(doc *RunConfigDoc) error {
	if doc.SchemaNewer() {
		return nil
	}
	if doc == nil {
		// No document at all is not a stored `{}`: there is nothing to overlay and nothing to fail closed
		// on, so leave the config untouched.
		return nil
	}
	// The event channel's dials are reset FIRST, before the empty document returns: the stored,
	// effective document is the authority, so an absent block means both dials off (fail closed) —
	// including the plane's default `{}` document, so a caller reusing a live config cannot keep the
	// channel on a project that says nothing.
	c.Events = RunConfigEvents{}
	if doc.Events != nil {
		c.Events = *doc.Events
	}
	if c.Events.Transcript && !c.Events.Activity {
		// The plane refuses this combination at save time; a document that still carries it (a
		// hand-edited store, a future reader) fails closed.
		c.Events.Transcript = false
	}
	// The `roles` section is the stored document's alone too (studio-chat.md §1/§2): a nil map means
	// the project inherited no planner and chat is OFF, which is the fail-closed answer for a project
	// that never ratified one.
	if doc.Empty() {
		c.Planner = plannerRoleFromDoc(doc)
		return nil
	}
	if err := c.CheckHarnessSwap(doc); err != nil {
		return err
	}
	c.Planner = plannerRoleFromDoc(doc)
	// The PIPELINE is one section (decision 6bf1fa15): a document carrying any of
	// prompt/phases/review_panel replaces all three, and a key it omits is cleared to its zero value
	// rather than kept from the local file.
	if doc.pipelineSet() {
		c.Prompt = doc.Prompt
		if doc.Phases == nil {
			c.Phases = nil
		} else {
			phases := make([]Phase, len(doc.Phases))
			for i, p := range doc.Phases {
				phases[i] = Phase{
					Name:         p.Name,
					Prompt:       p.Prompt,
					MaxTurns:     p.MaxTurns,
					MaxWallClock: p.MaxWallClock,
					Tier:         p.Tier,
					Model:        planeRoutes(p.Model),
					Harness:      p.Harness,
					ReviewMethod: p.ReviewMethod,
					MapsSession:  cloneMapsSession(p.MapsSession),
				}
			}
			c.Phases = phases
		}
		if doc.ReviewPanel == nil {
			c.ReviewPanel = nil
		} else {
			panel := make([]ReviewPanelMember, len(doc.ReviewPanel))
			for i, m := range doc.ReviewPanel {
				panel[i] = ReviewPanelMember{Name: m.Name, Harness: m.Harness, Model: planeRoutes(m.Model), Tier: m.Tier, Optional: m.Optional,
					ReviewMethod: m.ReviewMethod, MapsSession: cloneMapsSession(m.MapsSession)}
			}
			c.ReviewPanel = panel
		}
		// reviewPanelSet records whether the panel KEY is in force, which is now the document's answer
		// rather than the file's: a section replace that omits the key means "no panel", while an
		// explicit empty array is still the half-edited shape Validate refuses.
		c.reviewPanelSet = doc.ReviewPanel != nil
	}
	// c.ReviewPanelConcurrency is NOT updated here: how many panel seats run at once is the machine's
	// dial (decision e1365721), so the local value is kept even when the document carries one.
	if h := strings.TrimSpace(doc.Harness); h != "" {
		c.Harness = h
	}
	if m := strings.TrimSpace(doc.Model); m != "" {
		c.Model = m
	}
	if doc.Models != nil {
		c.Models = doc.Models
	}
	// The loop below writes into c.Harnesses, which the common single-harness config (no `harnesses:`
	// key) leaves nil: defaults() never creates the map and Clone only copies a non-nil source.
	if len(doc.Harnesses) > 0 && c.Harnesses == nil {
		c.Harnesses = make(map[string]HarnessConfig, len(doc.Harnesses))
	}
	for name, block := range doc.Harnesses {
		hc := c.Harnesses[name]
		if m := strings.TrimSpace(block.Model); m != "" {
			hc.Model = ModelRoutes{m}
		}
		if block.Models != nil {
			hc.Models = stringMapToRoutes(block.Models)
		}
		c.Harnesses[name] = hc
	}
	if doc.MaxTurns > 0 {
		c.MaxTurns = doc.MaxTurns
	}
	if doc.MaxSessionWallClock > 0 {
		c.MaxSessionWallClock = doc.MaxSessionWallClock
	}
	if b := doc.Budget; b != nil {
		if b.MaxTokens > 0 {
			c.Budget.MaxTokens = b.MaxTokens
		}
		if b.MaxCostUSD > 0 {
			c.Budget.MaxCostUSD = b.MaxCostUSD
		}
		if b.MaxWallClock > 0 {
			c.Budget.MaxWallClock = b.MaxWallClock
		}
		if b.MaxSessionTokens > 0 {
			c.Budget.MaxSessionTokens = b.MaxSessionTokens
		}
		if b.PerHarness != nil {
			c.Budget.PerHarness = b.PerHarness
		}
	}
	if e := doc.Escalation; e != nil {
		if e.PathRules != nil {
			c.Escalation.PathRules = e.PathRules
		}
		if e.CategoryRules != nil {
			c.Escalation.CategoryRules = e.CategoryRules
		}
	}
	// The event channel's dials were applied at the top, before the empty document returned: they are
	// the stored document's alone.
	return nil
}

// CheckHarnessSwap refuses a document that re-points the top-level harness when the swap would hand
// the new harness the old one's machine-local wiring — the same guard ApplyRunConfig enforces,
// exported so the supervisor's per-instance harness override (which re-points the top-level over
// the same machine layer through a different door) rides exactly it. An empty top-level is not a
// free pass: run-wide wiring with no named harness would still be handed to the new top-level on a
// swap, so it is refused like any other inheritance.
func (c *Config) CheckHarnessSwap(doc *RunConfigDoc) error {
	if doc == nil {
		return nil
	}
	newH := strings.TrimSpace(doc.Harness)
	oldH := strings.TrimSpace(c.Harness)
	if newH == "" || newH == oldH {
		return nil
	}
	return c.refuseHarnessSwap(doc, newH)
}

// refuseHarnessSwap reports whether re-pointing the top-level harness from the base's to newH would
// hand newH the old harness's dialect. SessionFor fills Model, Models, ConfigDir, MCPConfigPath and
// SettingsPath from the run-wide fields for the top-level harness only; ResolveMCPConfig
// additionally serves a project's own mcp_config_path to the top-level harness.
func (c *Config) refuseHarnessSwap(doc *RunConfigDoc, newH string) error {
	oldH := strings.TrimSpace(c.Harness)
	baseBlock := c.Harnesses[newH]
	docBlock := doc.Harnesses[newH]
	docModel := strings.TrimSpace(doc.Model)

	var inherited []string
	// Policy pair: the document CAN re-wire these, so a document-provided value (run-wide or
	// per-harness) is the new harness's own, not an inheritance.
	if strings.TrimSpace(c.Model) != "" && docModel == "" &&
		strings.TrimSpace(docBlock.Model) == "" && baseBlock.Model.Empty() {
		inherited = append(inherited, "model")
	}
	if len(c.Models) > 0 && doc.Models == nil &&
		docBlock.Models == nil && len(baseBlock.Models) == 0 {
		inherited = append(inherited, "models")
	}
	// Machine twins: the document carries none, so only a local block value keeps the new harness off
	// the run-wide (old) one.
	if strings.TrimSpace(c.ConfigDir) != "" && strings.TrimSpace(baseBlock.ConfigDir) == "" {
		inherited = append(inherited, "config_dir")
	}
	if strings.TrimSpace(c.MCPConfigPath) != "" && strings.TrimSpace(baseBlock.MCPConfigPath) == "" {
		inherited = append(inherited, "mcp_config_path")
	}
	if strings.TrimSpace(c.SettingsPath) != "" && strings.TrimSpace(baseBlock.SettingsPath) == "" {
		inherited = append(inherited, "settings_path")
	}
	for _, p := range c.Projects {
		if strings.TrimSpace(p.MCPConfigPath) == "" {
			continue
		}
		if strings.TrimSpace(p.MCPConfigPaths[newH]) == "" {
			inherited = append(inherited, fmt.Sprintf("projects[%s].mcp_config_path", p.Slug))
		}
	}
	if len(inherited) == 0 {
		return nil
	}
	return fmt.Errorf("harness %q cannot replace %q: the new harness would inherit %s from the current top-level wiring "+
		"(run-wide/project machine fields follow SessionFor/ResolveMCPConfig) — declare %s under `harnesses.%s` (or per-project `mcp_config_paths[%s]`) locally first",
		newH, oldH, strings.Join(inherited, ", "), strings.Join(inherited, ", "), newH, newH)
}

// RunConfigDocument renders the config's MOVABLE half as the plane-side proposal document
// `propose-config` imports: the pipeline (prompt, phases, review_panel, and each phase's model),
// the consumed families and the two backstops, in the plane's own snake_case vocabulary with wall
// clock as integer seconds. Nothing machine-local (workdir, wiring, env, state dirs) is included
// — importing those would ask the plane to custody what it must not, and the origin rule depends
// on the trusted statement staying where the operator wrote it until ratified.
func (c *Config) RunConfigDocument() (map[string]any, error) {
	doc := map[string]any{}
	if c.Harness != "" {
		doc["harness"] = c.Harness
	}
	if c.Model != "" {
		doc["model"] = c.Model
	}
	if len(c.Models) > 0 {
		doc["models"] = cloneStrMap(c.Models)
	}
	if len(c.Harnesses) > 0 {
		blocks := map[string]any{}
		for name, hc := range c.Harnesses {
			block := map[string]any{}
			// The plane's schema carries plain strings, so a route list travels as its FIRST route: the run
			// config is the operator's ratified policy half, and a fallback chain is a local-file shape
			// until the plane grows one.
			if first := hc.Model.First(); first != "" {
				block["model"] = first
			}
			if len(hc.Models) > 0 {
				block["models"] = routesToPlainMap(hc.Models)
			}
			if len(block) > 0 {
				blocks[name] = block
			}
		}
		if len(blocks) > 0 {
			doc["harnesses"] = blocks
		}
	}
	// The pipeline section, in the plane's own vocabulary.
	if len(c.ReviewPanel) > 0 && !c.expandsReviewPanel() {
		return nil, fmt.Errorf("cannot propose this config: `review_panel` needs a built-in, non-final %q phase "+
			"to expand, and this config's sequence has none, so the plane would refuse the document (a panel is "+
			"stored policy there, never a key that can do nothing). Give the config a built-in %q phase with a "+
			"phase after it (the fix), or remove `review_panel`", ReviewPhaseName, ReviewPhaseName)
	}
	// CLA-784 (review 1.review-1.3): the stored-run-config schema is the PLANE's, and it does not yet
	// know `review_panel[].optional` — its validator refuses the key as unknown, so a document
	// carrying a trial seat is one the operator could never ratify.
	if len(c.ReviewPanel) > 0 {
		for i, m := range c.ReviewPanel {
			if m.Optional {
				return nil, fmt.Errorf("cannot propose this config: `review_panel[%d].optional` is a TRIAL seat, "+
					"and the plane's stored-run-config schema does not accept that key yet (it refuses unknown "+
					"panel keys), so the document would be refused at ratification — and emitting it without the "+
					"flag would silently promote the trial seat to a required reviewer on every machine applying "+
					"the pipeline. Keep the trial seat local until the plane's schema and this build's "+
					"`$schema_version` (currently v%d) move in step, or remove `optional` from the member",
					i, RunConfigSchemaVersion)
			}
		}
	}
	// CLA-869: the v1 stored-run-config schema refuses unknown PHASE keys too, so `review_method` and
	// `maps_session` are keys the plane would reject at ratification.
	for i, ph := range c.Phases {
		if ph.ReviewMethod != "" || ph.MapsSession != nil {
			return nil, fmt.Errorf("cannot propose this config: `phases[%d].review_method` / `.maps_session` are "+
				"review-method keys the plane's stored-run-config schema does not accept yet (it refuses unknown "+
				"phase keys), so the document would be refused at ratification — and emitting it without them "+
				"would run every machine's review single-pass while this config asks for the maps-session "+
				"method. Keep the method local until the plane's schema and this build's `$schema_version` "+
				"(currently v%d) move in step, or leave `review_method` at the single-pass default",
				i, RunConfigSchemaVersion)
		}
	}
	for i, m := range c.ReviewPanel {
		if m.ReviewMethod != "" || m.MapsSession != nil {
			return nil, fmt.Errorf("cannot propose this config: `review_panel[%d].review_method` / `.maps_session` "+
				"are review-method keys the plane's stored-run-config schema does not accept yet (it refuses "+
				"unknown panel keys), so the document would be refused at ratification — and emitting the member "+
				"without them would run its review single-pass while this config asks for maps-session. Keep the "+
				"method local until the plane's schema and this build's `$schema_version` (currently v%d) move in "+
				"step, or leave the member's method at the review phase's own",
				i, RunConfigSchemaVersion)
		}
	}
	pipeline := map[string]any{}
	if c.Prompt != "" && c.Prompt != defaultPrompt {
		pipeline["prompt"] = c.Prompt
	}
	if len(c.Phases) > 0 {
		phases := make([]map[string]any, len(c.Phases))
		for i, ph := range c.Phases {
			entry := map[string]any{}
			if ph.Name != "" {
				entry["name"] = ph.Name
			}
			if ph.Prompt != "" {
				entry["prompt"] = ph.Prompt
			}
			if ph.MaxTurns > 0 {
				entry["max_turns"] = ph.MaxTurns
			}
			if ph.MaxWallClock > 0 {
				entry["max_wall_clock"] = int(ph.MaxWallClock.Duration().Seconds())
			}
			if ph.Tier != "" {
				entry["tier"] = ph.Tier
			}
			if first := ph.Model.First(); first != "" {
				entry["model"] = first
			}
			if ph.Harness != "" {
				entry["harness"] = ph.Harness
			}
			phases[i] = entry
		}
		pipeline["phases"] = phases
	}
	if len(c.ReviewPanel) > 0 {
		panel := make([]map[string]any, len(c.ReviewPanel))
		for i, m := range c.ReviewPanel {
			entry := map[string]any{}
			if m.Name != "" {
				entry["name"] = m.Name
			}
			if m.Harness != "" {
				entry["harness"] = m.Harness
			}
			if first := m.Model.First(); first != "" {
				entry["model"] = first
			}
			if m.Tier != "" {
				entry["tier"] = m.Tier
			}
			if m.Optional {
				// Unreachable while the plane-side-schema gate above is in force: the field is kept so that the
				// day the plane's stored schema learns the key — and this build's RunConfigSchemaVersion
				// moves in step — the trial seat travels without a second edit here.
				entry["optional"] = true
			}
			panel[i] = entry
		}
		pipeline["review_panel"] = panel
	}
	for k, v := range pipeline {
		doc[k] = v
	}
	if c.MaxTurns > 0 {
		doc["max_turns"] = c.MaxTurns
	}
	if c.MaxSessionWallClock > 0 {
		doc["max_session_wall_clock"] = int(c.MaxSessionWallClock.Duration().Seconds())
	}
	if c.Budget.CountsSpend() || c.Budget.MaxSessionTokens > 0 || c.Budget.MaxWallClock > 0 || len(c.Budget.PerHarness) > 0 {
		b := map[string]any{}
		if c.Budget.MaxTokens > 0 {
			b["max_tokens"] = c.Budget.MaxTokens
		}
		if c.Budget.MaxCostUSD > 0 {
			b["max_cost_usd"] = c.Budget.MaxCostUSD
		}
		if c.Budget.MaxWallClock > 0 {
			b["max_wall_clock"] = int(c.Budget.MaxWallClock.Duration().Seconds())
		}
		if c.Budget.MaxSessionTokens > 0 {
			b["max_session_tokens"] = c.Budget.MaxSessionTokens
		}
		if len(c.Budget.PerHarness) > 0 {
			per := map[string]any{}
			for name, hb := range c.Budget.PerHarness {
				entry := map[string]any{}
				if hb.MaxTokens > 0 {
					entry["max_tokens"] = hb.MaxTokens
				}
				if hb.MaxCostUSD > 0 {
					entry["max_cost_usd"] = hb.MaxCostUSD
				}
				if len(entry) > 0 {
					per[name] = entry
				}
			}
			if len(per) > 0 {
				b["per_harness"] = per
			}
		}
		if len(b) > 0 {
			doc["budget"] = b
		}
	}
	if c.Escalation.PathRules != nil || c.Escalation.CategoryRules != nil {
		e := map[string]any{}
		if c.Escalation.PathRules != nil {
			e["path_rules"] = cloneStrMap(c.Escalation.PathRules)
		}
		if c.Escalation.CategoryRules != nil {
			e["category_rules"] = cloneStrMap(c.Escalation.CategoryRules)
		}
		doc["escalation"] = e
	}
	return doc, nil
}

// planeRoutes widens the plane's plain-string model field into this package's route type.
func planeRoutes(model string) ModelRoutes {
	if strings.TrimSpace(model) == "" {
		return nil
	}
	return ModelRoutes{model}
}

// routesToPlainMap renders a route-valued tier map back into the plane's plain-string schema, first
// route only — the same narrowing RunConfigDocument applies to a single model field.
func routesToPlainMap(m map[string]ModelRoutes) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for bucket, routes := range m {
		first := routes.First()
		if first == "" {
			continue
		}
		out[bucket] = first
	}
	return out
}
