// Run-config consumption (CLA-410): the CLI side of the plane's stored
// execution-config document (CLA-408, phase 1). The plane stores a per-project
// document whose keys are this file's own snake_case config names; at start of
// run and on every backlog-summary `runConfigVersion` bump, the loop overlays
// that document over its local file — per project — so each project's sessions
// run under its own ratified policy. Until a project has one (version 0, or a
// document carrying nothing), the local file rules byte for byte.
//
// What is consumed is exactly what CLA-410's bar names plus the two numeric
// backstops stored in this config's native units: harness, model, tier buckets
// (`models`, and the per-harness blocks' model/models half), budget,
// escalation rules, max_turns, max_session_wall_clock. Deliberately NOT
// consumed yet: prompt and phases (order and briefs stay CLI-side until the
// state-machine reshape moves sequencing onto the plane), transitions (the
// workflow document is data for that same reshape), backlog_url and
// allow_local_mcp_servers (part of the local bootstrap core), notes
// (console-facing reasoning).
package config

import "strings"

// RunConfigHarnessBlock is the POLICY half of a per-harness block as the plane
// stores it. The machine-fit twins (`config_dir`, `mcp_config_path`,
// `settings_path`) are paths and wiring the plane never carries, so an overlay
// can only fill these two fields of an existing local block — it cannot hand a
// session somebody else's dialect.
type RunConfigHarnessBlock struct {
	Model  string            `json:"model"`
	Models map[string]string `json:"models"`
}

// RunConfigBudget mirrors Budget as the plane stores it: integer seconds for
// wall clock (Duration accepts a bare number), zero meaning unset exactly as
// in the local file.
type RunConfigBudget struct {
	MaxTokens        int                      `json:"max_tokens"`
	MaxCostUSD       float64                  `json:"max_cost_usd"`
	MaxWallClock     Duration                 `json:"max_wall_clock"`
	MaxSessionTokens int                      `json:"max_session_tokens"`
	PerHarness       map[string]HarnessBudget `json:"per_harness"`
}

// RunConfigEscalation mirrors Escalation. The rules stay mechanical globs and
// category names; the CLI evaluates them at review-spawn time exactly as when
// they came from the local file.
type RunConfigEscalation struct {
	PathRules     map[string]string `json:"path_rules"`
	CategoryRules map[string]string `json:"category_rules"`
}

// RunConfigDoc mirrors the plane's stored execution-config document — only the
// keys this build consumes. JSON decoding ignores everything else, so a newer
// plane's added keys degrade to "unconsumed" rather than unmarshal errors;
// Empty reports that case so the caller can say "nothing applied" honestly.
type RunConfigDoc struct {
	SchemaVersion       int                              `json:"$schema_version"`
	Harness             string                           `json:"harness"`
	Model               string                           `json:"model"`
	Models              map[string]string                `json:"models"`
	Harnesses           map[string]RunConfigHarnessBlock `json:"harnesses"`
	MaxTurns            int                              `json:"max_turns"`
	MaxSessionWallClock Duration                         `json:"max_session_wall_clock"`
	Budget              *RunConfigBudget                 `json:"budget"`
	Escalation          *RunConfigEscalation             `json:"escalation"`
}

// Empty reports whether the document consumes to nothing: every field a stored
// document may set is absent. The plane's default document is exactly this —
// `{}` with a schema version — and it means "the local file rules".
func (d *RunConfigDoc) Empty() bool {
	if d == nil {
		return true
	}
	return d.Harness == "" && d.Model == "" && d.Models == nil && d.Harnesses == nil &&
		d.MaxTurns == 0 && d.MaxSessionWallClock == 0 &&
		d.Budget == nil && d.Escalation == nil
}

// Clone returns a copy safe to overlay a run-config document onto: every map
// ApplyRunConfig can replace or mutate is copied one level deep, so the base
// config the driver shares across targets is never written through. Fields the
// overlay never touches (projects, repos, env, wiring) share backing storage —
// treat them read-only on the clone, which is all the drain path does.
func (c *Config) Clone() *Config {
	cp := *c
	cp.Models = cloneStrMap(c.Models)
	if c.Harnesses != nil {
		cp.Harnesses = make(map[string]HarnessConfig, len(c.Harnesses))
		for name, hc := range c.Harnesses {
			hc.Models = cloneStrMap(hc.Models)
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
	// Element values are written by Validate's per-project discovery, so the
	// slice gets its own backing array even though the overlay itself never
	// touches projects.
	cp.Projects = append([]Project(nil), c.Projects...)
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

// ApplyRunConfig overlays a stored document onto c IN PLACE: a field the
// document sets wins, a field it omits keeps the local value. Maps replace
// wholesale when present — the document is the operator's ratified word for
// that map, and merging would resurrect entries the ratification dropped.
// Callers re-run Validate afterwards: an overlay CAN produce a combination the
// local file alone could not (a stored harness name nobody registered), and a
// refused combination must be loud, not silently half-applied.
func (c *Config) ApplyRunConfig(doc *RunConfigDoc) {
	if doc.Empty() {
		return
	}
	if h := strings.TrimSpace(doc.Harness); h != "" {
		c.Harness = h
	}
	if m := strings.TrimSpace(doc.Model); m != "" {
		c.Model = m
	}
	if doc.Models != nil {
		c.Models = doc.Models
	}
	for name, block := range doc.Harnesses {
		hc := c.Harnesses[name]
		if m := strings.TrimSpace(block.Model); m != "" {
			hc.Model = m
		}
		if block.Models != nil {
			hc.Models = block.Models
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
}

// RunConfigDocument renders the config's MOVABLE half as the plane-side
// proposal document `propose-config` imports: the consumed families plus the
// two backstops, in the plane's own snake_case vocabulary with wall clock as
// integer seconds. Nothing machine-local (workdir, wiring, env, state dirs)
// is included — importing those would ask the plane to custody what it must
// not, and the origin rule depends on the trusted statement staying where the
// operator wrote it until ratified.
func (c *Config) RunConfigDocument() map[string]any {
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
			if hc.Model != "" {
				block["model"] = hc.Model
			}
			if len(hc.Models) > 0 {
				m := cloneStrMap(hc.Models)
				block["models"] = m
			}
			if len(block) > 0 {
				blocks[name] = block
			}
		}
		if len(blocks) > 0 {
			doc["harnesses"] = blocks
		}
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
	return doc
}
