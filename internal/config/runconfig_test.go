package config

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// overlayBase is a local config with a value in every family the stored
// document may replace, so an overlay that silently keeps a local value shows
// up as the LOCAL string, never as a zero.
func overlayBase() *Config {
	c := defaults()
	c.Harness = "claude"
	c.Model = "local-model"
	c.Models = map[string]string{"strong": "local-strong"}
	c.Harnesses = map[string]HarnessConfig{
		"claude": {Model: "local-model", Models: map[string]string{"strong": "local-strong"}, ConfigDir: "/local/claude"},
	}
	c.MaxTurns = 111
	c.MaxSessionWallClock = Duration(111 * time.Second)
	c.Budget = Budget{MaxTokens: 1000, MaxCostUSD: 1, MaxWallClock: Duration(1000 * time.Second), MaxSessionTokens: 500,
		PerHarness: map[string]HarnessBudget{"claude": {MaxTokens: 900}}}
	c.Escalation = Escalation{PathRules: map[string]string{"local/**": "standard"}}
	return c
}

func TestApplyRunConfig_EveryConsumedFamilyOverlaysAndAbsentKeepsLocal(t *testing.T) {
	doc := &RunConfigDoc{
		SchemaVersion:       1,
		Harness:             "opencode",
		Model:               "plane-model",
		Models:              map[string]string{"strong": "plane-strong"},
		Harnesses:           map[string]RunConfigHarnessBlock{"opencode": {Model: "oc-default", Models: map[string]string{"cheap": "oc-cheap"}}},
		MaxTurns:            42,
		MaxSessionWallClock: Duration(42 * time.Second),
		Budget: &RunConfigBudget{
			MaxTokens:        77_000_000,
			MaxCostUSD:       9.5,
			MaxWallClock:     Duration(2 * time.Hour),
			MaxSessionTokens: 3_000_000,
			PerHarness:       map[string]HarnessBudget{"opencode": {MaxTokens: 5_000_000}},
		},
		Escalation: &RunConfigEscalation{CategoryRules: map[string]string{"bug": "strong"}},
	}

	base := overlayBase()
	if err := base.ApplyRunConfig(doc); err != nil {
		t.Fatalf("ApplyRunConfig: %v", err)
	}

	if base.Harness != "opencode" || base.Model != "plane-model" {
		t.Errorf("harness/model = %q/%q, want the stored values", base.Harness, base.Model)
	}
	if !reflect.DeepEqual(base.Models, doc.Models) {
		t.Errorf("models = %v, want %v (the ratified map replaces, not merges)", base.Models, doc.Models)
	}
	// The per-harness POLICY half overlays; the machine twins stay local.
	if got := base.SessionFor("opencode"); got.Model != "oc-default" || got.ConfigDir != "" {
		t.Errorf("opencode block model=%q configDir=%q, want the stored model and NO inherited local dir", got.Model, got.ConfigDir)
	}
	if got := base.SessionFor("claude"); got.Model != "local-model" || got.ConfigDir != "/local/claude" {
		t.Errorf("unnamed claude block drifted: %+v", got)
	}
	if base.MaxTurns != 42 || base.MaxSessionWallClock != Duration(42*time.Second) {
		t.Errorf("backstops = turns %d clock %v, want the stored ones", base.MaxTurns, base.MaxSessionWallClock)
	}
	if base.Budget.MaxTokens != 77_000_000 || base.Budget.MaxCostUSD != 9.5 || base.Budget.MaxSessionTokens != 3_000_000 {
		t.Errorf("budget dials did not overlay: %+v", base.Budget)
	}
	if base.Budget.MaxWallClock != Duration(2*time.Hour) {
		t.Errorf("budget wall clock = %v, want 2h", base.Budget.MaxWallClock)
	}
	if hb := base.Budget.PerHarness["opencode"]; hb.MaxTokens != 5_000_000 {
		t.Errorf("per_harness[opencode] = %+v, want the stored block", hb)
	}
	if tier, rule := base.Escalation.Evaluate(nil, "bug"); tier != "strong" || rule == "" {
		t.Errorf("stored category rule did not evaluate: tier=%q rule=%q", tier, rule)
	}
}

func TestApplyRunConfig_NilBaseHarnessesAllocatesOnOverlay(t *testing.T) {
	// The common single-harness config has no `harnesses:` key, so defaults()
	// leaves the map nil; every overlay test base happens to pre-populate it.
	// A stored document carrying a harnesses block over the nil-map shape used
	// to panic with "assignment to entry in nil map". The precondition is
	// asserted, not assumed: if defaults() ever starts pre-populating the map
	// this test must fail loudly rather than silently stop exercising the
	// nil path while staying green - the exact drift this bug was born from.
	base := defaults()
	if base.Harnesses != nil {
		t.Fatalf("precondition: defaults() must leave Harnesses nil for the nil-base shape, got %d entries", len(base.Harnesses))
	}
	if err := base.ApplyRunConfig(&RunConfigDoc{
		SchemaVersion: 1,
		Harnesses:     map[string]RunConfigHarnessBlock{"opencode": {Model: "oc-default", Models: map[string]string{"cheap": "oc-cheap"}}},
	}); err != nil {
		t.Fatalf("ApplyRunConfig: %v", err)
	}
	if got := base.Harnesses["opencode"]; got.Model != "oc-default" {
		t.Errorf("opencode block model = %q, want the stored one (overlay must allocate the nil map)", got.Model)
	}
	// The policy half lands; the machine-local half stays absent, as everywhere else.
	if got := base.SessionFor("opencode"); got.Model != "oc-default" || got.ConfigDir != "" {
		t.Errorf("opencode session model=%q configDir=%q, want the stored model and no machine-local wiring", got.Model, got.ConfigDir)
	}
}

// Only the past and present are consumable: a document carrying a
// $schema_version newer than RunConfigSchemaVersion reports SchemaNewer, so
// the consume points can refuse it before Empty or the overlay ever see it.
// Pinned against the constant, not a literal, so a future version bump moves
// the boundary without rewriting the test — while the existing overlay tests
// (all SchemaVersion: 1) pin that today's version still applies.
func TestRunConfigDoc_SchemaNewerRefusesOnlyTheFuture(t *testing.T) {
	for _, doc := range []*RunConfigDoc{nil, {}, {SchemaVersion: RunConfigSchemaVersion}} {
		if doc.SchemaNewer() {
			t.Errorf("SchemaNewer() = true for %+v, want false (this build's own version is consumable)", doc)
		}
	}
	if doc := (&RunConfigDoc{SchemaVersion: RunConfigSchemaVersion + 1}); !doc.SchemaNewer() {
		t.Errorf("SchemaNewer() = false for %+v, want true (a newer schema may have changed the known keys' meaning)", doc)
	}
}

// The overlay itself is the backstop behind the consume points' loud refusal:
// a newer-schema document applied through a direct ApplyRunConfig call must
// still be a no-op, so a future caller that forgets the SchemaNewer check
// keeps the previous config instead of running redefined keys.
func TestApplyRunConfig_NewerSchemaIsANoOp(t *testing.T) {
	base := overlayBase()
	before := base.Clone()
	base.ApplyRunConfig(&RunConfigDoc{
		SchemaVersion: RunConfigSchemaVersion + 1,
		Harness:       "opencode",
		Model:         "plane-x",
		Models:        map[string]string{"strong": "plane-strong"},
		MaxTurns:      42,
		Budget:        &RunConfigBudget{MaxTokens: 77_000_000},
		Escalation:    &RunConfigEscalation{CategoryRules: map[string]string{"bug": "strong"}},
	})
	if !reflect.DeepEqual(base, before) {
		t.Errorf("a newer-schema document overlaid: harness=%q model=%q turns=%d (want the previous config kept)",
			base.Harness, base.Model, base.MaxTurns)
	}
}

func TestApplyRunConfig_EmptyDocumentIsANoOp(t *testing.T) {
	base := overlayBase()
	before := base.Clone()
	for _, doc := range []*RunConfigDoc{nil, {}, {SchemaVersion: 1}} {
		if err := base.ApplyRunConfig(doc); err != nil {
			t.Fatalf("empty ApplyRunConfig: %v", err)
		}
	}
	if !reflect.DeepEqual(base.Models, before.Models) || base.Harness != before.Harness || base.MaxTurns != before.MaxTurns {
		t.Errorf("an empty document changed something: harness=%q turns=%d", base.Harness, base.MaxTurns)
	}
}

func TestClone_AnOverlayNeverWritesThroughToTheBase(t *testing.T) {
	base := overlayBase()
	cp := base.Clone()
	// The harness swap here carries its own model so the CLA-475 swap guard
	// lets it through: the point is Clone isolation, not the swap refusal.
	if err := cp.ApplyRunConfig(&RunConfigDoc{
		SchemaVersion: 1,
		Harness:       "codex",
		Model:         "cp-model",
		Models:        map[string]string{"only": "cp"},
		Budget:        &RunConfigBudget{PerHarness: map[string]HarnessBudget{"codex": {MaxTokens: 1}}},
		Escalation:    &RunConfigEscalation{CategoryRules: map[string]string{"x": "y"}},
	}); err != nil {
		t.Fatalf("ApplyRunConfig: %v", err)
	}
	if base.Harness != "claude" {
		t.Errorf("base harness became %q through the clone", base.Harness)
	}
	if _, ok := base.Models["only"]; ok {
		t.Error("base models map was mutated through the clone")
	}
	if _, ok := base.Budget.PerHarness["codex"]; ok {
		t.Error("base per_harness map was mutated through the clone")
	}
	if base.Escalation.CategoryRules != nil {
		t.Error("base escalation map was mutated through the clone")
	}
}

// CLA-475: a stored harness swap must not hand the new harness the old one's
// machine-local wiring. SessionFor attaches the run-wide model/models plus
// config_dir/mcp_config_path/settings_path to whichever harness is top-level,
// and ResolveMCPConfig serves a project's own mcp_config_path to the top-level
// harness, so re-pointing `harness` over a wired base silently re-homes that
// dialect. The overlay refuses loudly (leaving the base untouched) unless the
// new harness already declares its own value for every wired field — a
// merely non-empty block still inherits through its gaps, so the check is per
// field, not per block.
func TestApplyRunConfig_HarnessSwapRefusesInheritedWiring(t *testing.T) {
	wiredBase := func() *Config {
		c := defaults()
		c.Harness = "claude"
		c.Model = "claude-alias"
		c.Models = map[string]string{"strong": "claude-strong"}
		c.ConfigDir = "/local/claude-dir"
		c.MCPConfigPath = "/local/claude-mcp.json"
		c.SettingsPath = "/local/claude-settings.json"
		return c
	}

	// The reported shape: a typical single-harness file (run-wide wiring, no
	// harnesses block) plus a document that only re-points the harness. It
	// must be refused, and the base must be untouched for the caller's loud
	// path to keep.
	base := wiredBase()
	before := base.Clone()
	err := base.ApplyRunConfig(&RunConfigDoc{SchemaVersion: 1, Harness: "opencode"})
	if err == nil {
		t.Fatal("a harness swap over run-wide wiring was applied; want a refusal")
	}
	for _, want := range []string{"opencode", "claude", "config_dir", "mcp_config_path", "settings_path", "model"} {
		if got := err.Error(); !containsFold(got, want) {
			t.Errorf("refusal %q does not name %q", got, want)
		}
	}
	if base.Harness != before.Harness || base.Model != before.Model ||
		base.ConfigDir != before.ConfigDir || base.MCPConfigPath != before.MCPConfigPath ||
		base.SettingsPath != before.SettingsPath {
		t.Errorf("a refused overlay mutated the base: harness=%q model=%q dir=%q", base.Harness, base.Model, base.ConfigDir)
	}
	// The new harness must not have inherited through SessionFor even though
	// the overlay was refused: it still resolves ambient, never claude's.
	if got := base.SessionFor("opencode"); got.ConfigDir != "" || got.MCPConfigPath != "" || got.SettingsPath != "" || got.Model != "" {
		t.Errorf("refused swap still re-homes wiring via SessionFor: %+v", got)
	}

	// A partial block is still a refusal: a model alone does not cover the
	// config dir gap the new top-level would inherit through.
	partial := wiredBase()
	partial.Harnesses = map[string]HarnessConfig{"opencode": {Model: "oc-model"}}
	if err := partial.ApplyRunConfig(&RunConfigDoc{SchemaVersion: 1, Harness: "opencode"}); err == nil {
		t.Error("a swap whose new block covers only the model was applied; want a refusal naming the machine gaps")
	} else if got := err.Error(); !containsFold(got, "config_dir") {
		t.Errorf("partial-block refusal %q does not name the uncovered config_dir gap", got)
	}

	// A complete local block lets the same swap through: every wired field
	// has its own value, so nothing is inherited.
	complete := wiredBase()
	complete.Harnesses = map[string]HarnessConfig{"opencode": {
		Model: "oc-model", Models: map[string]string{"strong": "oc-strong"},
		ConfigDir: "/local/opencode-dir", MCPConfigPath: "/local/opencode-mcp.json", SettingsPath: "/local/opencode-settings.json",
	}}
	if err := complete.ApplyRunConfig(&RunConfigDoc{SchemaVersion: 1, Harness: "opencode"}); err != nil {
		t.Fatalf("a fully-wired new block was refused: %v", err)
	}
	if complete.Harness != "opencode" {
		t.Errorf("harness = %q, want opencode", complete.Harness)
	}
	if got := complete.SessionFor("opencode"); got.ConfigDir != "/local/opencode-dir" || got.Model != "oc-model" {
		t.Errorf("wired swap resolved %+v, want the new block's own wiring", got)
	}
	if got := complete.SessionFor("claude"); got.ConfigDir != "" || got.Model != "" {
		t.Errorf("the old harness kept run-wide wiring after the swap: %+v (run-wide must not follow the swap)", got)
	}

	// An unwired base swaps freely: nothing run-wide to inherit, so the new
	// top-level resolves ambient rather than somebody else's dialect.
	bare := defaults()
	bare.Harness = "claude"
	if err := bare.ApplyRunConfig(&RunConfigDoc{SchemaVersion: 1, Harness: "opencode"}); err != nil {
		t.Fatalf("an unwired swap was refused: %v", err)
	}
	if bare.Harness != "opencode" {
		t.Errorf("harness = %q, want opencode", bare.Harness)
	}

	// A document-provided model is the new harness's own, not an inheritance:
	// run-wide wiring otherwise empty, the swap lands on the stored alias.
	aliased := defaults()
	aliased.Harness = "claude"
	aliased.Model = "claude-alias"
	if err := aliased.ApplyRunConfig(&RunConfigDoc{SchemaVersion: 1, Harness: "opencode", Model: "plane-model"}); err != nil {
		t.Fatalf("a swap carrying its own model was refused: %v", err)
	}
	if aliased.Model != "plane-model" {
		t.Errorf("model = %q, want the stored plane-model", aliased.Model)
	}

	// Project wiring follows the same rule: a project's own mcp_config_path
	// is that harness's schema, so swapping the top-level without a
	// per-harness entry for the new harness is refused.
	proj := defaults()
	proj.Harness = "claude"
	proj.Projects = []Project{{Slug: "demo", MCPConfigPath: "/proj/claude-mcp.json"}}
	if err := proj.ApplyRunConfig(&RunConfigDoc{SchemaVersion: 1, Harness: "opencode"}); err == nil {
		t.Error("a swap over a project mcp_config_path was applied; want a refusal")
	}
	projOK := defaults()
	projOK.Harness = "claude"
	projOK.Projects = []Project{{Slug: "demo", MCPConfigPath: "/proj/claude-mcp.json",
		MCPConfigPaths: map[string]string{"opencode": "/proj/opencode-mcp.json"}}}
	if err := projOK.ApplyRunConfig(&RunConfigDoc{SchemaVersion: 1, Harness: "opencode"}); err != nil {
		t.Fatalf("a swap with a per-harness project file was refused: %v", err)
	}

	// An empty top-level is not a free pass: run-wide wiring with no named
	// harness would still be handed to the new top-level on a swap, so it is
	// refused like any other inheritance.
	emptyTop := defaults()
	emptyTop.Harness = ""
	emptyTop.ConfigDir = "/local/dir"
	if err := emptyTop.ApplyRunConfig(&RunConfigDoc{SchemaVersion: 1, Harness: "opencode"}); err == nil {
		t.Error("a swap over run-wide wiring with an empty top-level was applied; want a refusal")
	} else if got := err.Error(); !containsFold(got, "config_dir") {
		t.Errorf("empty-top refusal %q does not name the inherited config_dir", got)
	}
}

func containsFold(hay, needle string) bool {
	return len(hay) >= len(needle) && (func() bool {
		h, n := hay, needle
		// ASCII case-fold is enough for the field names this asserts on.
		for i := 0; i+len(n) <= len(h); i++ {
			match := true
			for j := 0; j < len(n); j++ {
				a, b := h[i+j], n[j]
				if a >= 'A' && a <= 'Z' {
					a += 'a' - 'A'
				}
				if b >= 'A' && b <= 'Z' {
					b += 'a' - 'A'
				}
				if a != b {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
		return false
	})()
}

// The plane stores integer seconds; the round trip through its document shape
// must land back on the same Duration the CLI resolves from.
func TestRunConfigDocument_RoundTripsThroughTheStoredShape(t *testing.T) {
	c := overlayBase()
	c.MaxSessionWallClock = Duration(90 * time.Second)
	c.Budget.MaxWallClock = Duration(3600 * time.Second)

	data, err := json.Marshal(c.RunConfigDocument())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc RunConfigDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal into the mirror type: %v (%s)", err, data)
	}
	if doc.MaxSessionWallClock != Duration(90*time.Second) {
		t.Errorf("max_session_wall_clock = %v, want 90s", doc.MaxSessionWallClock)
	}
	if doc.Budget == nil || doc.Budget.MaxWallClock != Duration(3600*time.Second) {
		t.Errorf("budget.max_wall_clock did not survive: %+v", doc.Budget)
	}
	if doc.Harness != "claude" || len(doc.Models) == 0 || doc.Escalation == nil {
		t.Errorf("movable families missing from the rendered document: %s", data)
	}
	// Nothing machine-local may ride along.
	for _, banned := range []string{"workdir", "mcp_config_path", "env", "state_dir", "settings_path", "config_dir"} {
		if _, ok := c.RunConfigDocument()[banned]; ok {
			t.Errorf("rendered document carries machine-local key %q", banned)
		}
	}
}
