package config

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
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

	rendered, err := c.RunConfigDocument()
	if err != nil {
		t.Fatalf("RunConfigDocument: %v", err)
	}
	data, err := json.Marshal(rendered)
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
		if _, ok := rendered[banned]; ok {
			t.Errorf("rendered document carries machine-local key %q", banned)
		}
	}
}

// --- the pipeline section (CLA-598) -----------------------------------------

// pipelineBase is a local config with a DIFFERENT pipeline from the stored one:
// a two-and-a-bit sequence, one local reviewer, a different concurrency and a
// tier everywhere. Every assertion below can tell a stored value from a local
// one, and a wrongly-kept local value shows up as the LOCAL string rather than
// as a zero.
func pipelineBase() *Config {
	c := defaults()
	c.Harness = "opencode"
	c.Prompt = ""
	c.Models = map[string]string{"local": "local-model"}
	c.Phases = []Phase{
		{Name: "implement", Tier: "local"},
		{Name: "review", Tier: "local"},
		{Name: "fix", Tier: "local"},
	}
	c.ReviewPanel = []ReviewPanelMember{{Name: "local", Model: "local/one"}}
	// The cap is the MACHINE's dial (decision e1365721). A distinct local value
	// is what every overlay assertion uses to prove the stored document never
	// moved it.
	c.ReviewPanelConcurrency = 3
	return c
}

// operatorPipeline is the document the task's doneWhen describes: implement /
// review / fix / verify on opencode, implement pinned to a model directly, a
// five-member panel. It deliberately carries NO review_panel_concurrency: that
// is the machine's dial and propose-config does not export it (decision
// e1365721).
func operatorPipeline() *RunConfigDoc {
	return &RunConfigDoc{
		SchemaVersion: 1,
		Phases: []RunConfigPhase{
			{Name: "implement", Model: "opencode-go/gpt-6-luna", MaxWallClock: Duration(30 * time.Minute)},
			{Name: "review"},
			{Name: "fix"},
			{Name: "verify"},
		},
		ReviewPanel: []RunConfigPanelMember{
			{Model: "opencode-go/gpt-6-luna"},
			{Model: "opencode-go/gpt-6-luna"},
			{Model: "opencode-go/gpt-6-luna"},
			{Model: "opencode-go/glm-5.3-flash"},
			{Name: "tiebreak", Model: "opencode-go/gpt-6-luna"},
		},
	}
}

func TestApplyRunConfig_PipelineSectionReplacesTheLocalPipeline(t *testing.T) {
	base := pipelineBase()
	if err := base.ApplyRunConfig(operatorPipeline()); err != nil {
		t.Fatalf("ApplyRunConfig: %v", err)
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("the overlaid pipeline must validate: %v", err)
	}
	if len(base.Phases) != 4 {
		t.Fatalf("phases = %d, want the stored 4", len(base.Phases))
	}
	if base.Phases[0].Tier != "" {
		t.Errorf("phases[0].tier = %q, want it cleared by the stored phase (the document omitted it)", base.Phases[0].Tier)
	}
	if got, ok := base.ModelForPhase(base.Phases[0]); got != "opencode-go/gpt-6-luna" || !ok {
		t.Errorf("ModelForPhase(implement) = %q/%v, want the stored direct model", got, ok)
	}
	if got, ok := base.ModelForPhase(base.EffectivePhases()[0]); got != "opencode-go/gpt-6-luna" || !ok {
		t.Errorf("the effective implement phase resolves to %q/%v, want the stored direct model", got, ok)
	}
	if len(base.ReviewPanel) != 5 || base.ReviewPanel[4].Name != "tiebreak" {
		t.Errorf("review panel = %+v, want the stored 5 members", base.ReviewPanel)
	}
	if base.ReviewPanelConcurrency != 3 {
		t.Errorf("review_panel_concurrency = %d, want the LOCAL 3 kept: the stored document does not own the machine's cap (decision e1365721)", base.ReviewPanelConcurrency)
	}
	if base.Prompt != "" {
		t.Errorf("prompt = %q, want the local one cleared by the stored phases", base.Prompt)
	}
	if got := len(base.EffectivePhases()); got != 8 {
		t.Errorf("effective phases = %d, want 8 (implement, five reviewers, fix, verify)", got)
	}
}

// The pipeline is ONE section (decision 6bf1fa15): carrying any of prompt,
// phases or review_panel replaces all three, in both directions, so prompt and
// phases cannot both survive an overlay. `review_panel_concurrency` is outside
// the section (decision e1365721) and survives whichever way it goes.
func TestApplyRunConfig_PipelineSectionClearsTheKeysItOmits(t *testing.T) {
	t.Run("stored phases clear a local panel and prompt", func(t *testing.T) {
		base := defaults()
		base.Harness = "opencode"
		base.Prompt = "local prompt"
		base.ReviewPanel = []ReviewPanelMember{{Model: "local/one"}}
		base.ReviewPanelConcurrency = 2

		if err := base.ApplyRunConfig(&RunConfigDoc{
			SchemaVersion: 1,
			Phases: []RunConfigPhase{
				{Name: "implement"}, {Name: "review"}, {Name: "fix"},
			},
		}); err != nil {
			t.Fatalf("ApplyRunConfig: %v", err)
		}
		if base.Prompt != "" {
			t.Errorf("prompt = %q, want it cleared: the stored section omitted it", base.Prompt)
		}
		if base.ReviewPanel != nil {
			t.Errorf("review panel = %+v, want it cleared: the stored section omitted it", base.ReviewPanel)
		}
		if base.ReviewPanelConcurrency != 2 {
			t.Errorf("review_panel_concurrency = %d, want the local 2 kept: the cap is machine-owned, not part of the replaced section", base.ReviewPanelConcurrency)
		}
	})
	t.Run("a stored prompt clears a local sequence and panel", func(t *testing.T) {
		base := pipelineBase()
		if err := base.ApplyRunConfig(&RunConfigDoc{SchemaVersion: 1, Prompt: "stored prompt"}); err != nil {
			t.Fatalf("ApplyRunConfig: %v", err)
		}
		if base.Prompt != "stored prompt" {
			t.Errorf("prompt = %q, want the stored one", base.Prompt)
		}
		if base.Phases != nil {
			t.Errorf("phases = %+v, want them cleared: the stored section omitted them", base.Phases)
		}
		if base.ReviewPanel != nil {
			t.Errorf("panel = %+v, want it cleared: the stored section omitted it", base.ReviewPanel)
		}
		if base.ReviewPanelConcurrency != 3 {
			t.Errorf("review_panel_concurrency = %d, want the local 3 kept with the panel gone", base.ReviewPanelConcurrency)
		}
	})
}

// A document that says nothing about the pipeline leaves the local one alone —
// the pre-CLA-598 posture, still the rule for every other family.
func TestApplyRunConfig_DocumentWithoutPipelineKeepsTheLocalPipeline(t *testing.T) {
	base := pipelineBase()
	if err := base.ApplyRunConfig(&RunConfigDoc{SchemaVersion: 1, MaxTurns: 9}); err != nil {
		t.Fatalf("ApplyRunConfig: %v", err)
	}
	if len(base.Phases) != 3 || base.Phases[0].Tier != "local" {
		t.Errorf("phases = %+v, want the local sequence untouched", base.Phases)
	}
	if len(base.ReviewPanel) != 1 || base.ReviewPanel[0].Model != "local/one" || base.ReviewPanelConcurrency != 3 {
		t.Errorf("panel = %+v/%d, want the local panel untouched", base.ReviewPanel, base.ReviewPanelConcurrency)
	}
	if base.MaxTurns != 9 {
		t.Errorf("max_turns = %d, want the stored 9 (the non-pipeline family still overlays)", base.MaxTurns)
	}
}

// review_panel_concurrency is machine-owned (decision e1365721). A stored value
// must never override the local file's, and the key alone is not a pipeline
// statement: it cannot drag in a stored panel or clear a local one.
func TestApplyRunConfig_StoredPanelConcurrencyIsIgnored(t *testing.T) {
	t.Run("a stored cap never overrides the local one", func(t *testing.T) {
		base := pipelineBase()
		doc := operatorPipeline()
		doc.ReviewPanelConcurrency = 5
		if err := base.ApplyRunConfig(doc); err != nil {
			t.Fatalf("ApplyRunConfig: %v", err)
		}
		if base.ReviewPanelConcurrency != 3 {
			t.Errorf("review_panel_concurrency = %d, want the local 3, not the stored 5", base.ReviewPanelConcurrency)
		}
		if len(base.ReviewPanel) != 5 {
			t.Errorf("review panel = %d members, want the stored 5: the rest of the section still applies", len(base.ReviewPanel))
		}
	})
	t.Run("the key alone is not a pipeline statement", func(t *testing.T) {
		base := pipelineBase()
		if err := base.ApplyRunConfig(&RunConfigDoc{SchemaVersion: 1, ReviewPanelConcurrency: 5}); err != nil {
			t.Fatalf("ApplyRunConfig: %v", err)
		}
		if len(base.Phases) != 3 || base.Phases[0].Tier != "local" {
			t.Errorf("phases = %+v, want the local sequence untouched", base.Phases)
		}
		if len(base.ReviewPanel) != 1 || base.ReviewPanel[0].Model != "local/one" {
			t.Errorf("panel = %+v, want the local panel untouched", base.ReviewPanel)
		}
		if base.ReviewPanelConcurrency != 3 {
			t.Errorf("review_panel_concurrency = %d, want the local 3", base.ReviewPanelConcurrency)
		}
	})
}

// An overlay on a CLONE must never reach the base config's pipeline: the driver
// shares one base across targets, so a phase or panel written in place would
// leak one project's policy into the next.
func TestClone_PipelineSlicesAreIndependent(t *testing.T) {
	base := pipelineBase()
	cp := base.Clone()
	cp.Phases[0].Model = "clone-only"
	cp.Phases[0].Tier = "clone-only"
	cp.ReviewPanel[0].Model = "clone-only"
	cp.ReviewPanelConcurrency = 7

	if base.Phases[0].Model != "" || base.Phases[0].Tier != "local" {
		t.Errorf("base phases mutated through the clone: %+v", base.Phases[0])
	}
	if base.ReviewPanel[0].Model != "local/one" || base.ReviewPanelConcurrency != 3 {
		t.Errorf("base panel mutated through the clone: %+v/%d", base.ReviewPanel[0], base.ReviewPanelConcurrency)
	}
	// An overlay REPLACES the slices, so the clone must not end up aliasing the
	// document either.
	doc := operatorPipeline()
	if err := cp.ApplyRunConfig(doc); err != nil {
		t.Fatalf("ApplyRunConfig: %v", err)
	}
	doc.Phases[0].Name = "mutated-after-apply"
	if cp.Phases[0].Name == "mutated-after-apply" {
		t.Error("the clone's phases alias the document slice")
	}
	if len(base.Phases) != 3 {
		t.Errorf("the overlay on a clone replaced the base's phases: %d", len(base.Phases))
	}
}

// propose-config lifts the live pipeline onto the plane, so the rendered
// document must carry the section in the plane's vocabulary — integer seconds,
// `model` beside `tier`, empty fields omitted — while `review_panel_concurrency`
// stays behind: it is the machine's, not the run's (decision e1365721).
func TestRunConfigDocument_RoundTripsThePipelineAndPhaseModel(t *testing.T) {
	c := pipelineBase()
	c.Phases[0].Model = "opencode-go/gpt-6-luna"
	c.Phases[0].Tier = ""
	c.Phases[0].MaxWallClock = Duration(45 * time.Minute)
	c.Phases[1].Tier = "strong"

	rendered, err := c.RunConfigDocument()
	if err != nil {
		t.Fatalf("RunConfigDocument: %v", err)
	}
	data, err := json.Marshal(rendered)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v (%s)", err, data)
	}
	if _, ok := raw["review_panel_concurrency"]; ok {
		t.Errorf("the rendered document carries review_panel_concurrency, which is machine-owned and must not travel (%s)", data)
	}
	var doc RunConfigDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal into the mirror type: %v (%s)", err, data)
	}
	if len(doc.Phases) != 3 {
		t.Fatalf("phases = %d, want the local 3 (%s)", len(doc.Phases), data)
	}
	if doc.Phases[0].Model != "opencode-go/gpt-6-luna" {
		t.Errorf("phases[0].model = %q, want the configured alias (%s)", doc.Phases[0].Model, data)
	}
	if doc.Phases[0].Tier != "" {
		t.Errorf("phases[0].tier = %q, want it omitted when unset (%s)", doc.Phases[0].Tier, data)
	}
	if doc.Phases[0].MaxWallClock != Duration(45*time.Minute) {
		t.Errorf("phases[0].max_wall_clock = %v, want 45m (%s)", doc.Phases[0].MaxWallClock, data)
	}
	if doc.Phases[1].Model != "" || doc.Phases[1].Tier != "strong" {
		t.Errorf("phases[1] = model %q tier %q, want the tier kept and no model (%s)", doc.Phases[1].Model, doc.Phases[1].Tier, data)
	}
	// Integer seconds, not a duration string: the stored shape is the plane's.
	var rawPhases struct {
		Phases []map[string]json.RawMessage `json:"phases"`
	}
	if err := json.Unmarshal(data, &rawPhases); err != nil {
		t.Fatalf("unmarshal raw phases: %v", err)
	}
	if got := string(rawPhases.Phases[0]["max_wall_clock"]); got != "2700" {
		t.Errorf("max_wall_clock on the wire = %s, want the integer 2700", got)
	}
	if len(doc.ReviewPanel) != 1 || doc.ReviewPanel[0].Model != "local/one" || doc.ReviewPanelConcurrency != 0 {
		t.Errorf("panel did not round-trip: %+v/%d (%s)", doc.ReviewPanel, doc.ReviewPanelConcurrency, data)
	}
}

// A configured phase names its model ONE way (operator decision 3eddb7a5), and
// the refusal names the phase. A panel MEMBER may still set both — expansion
// clears the tier, so the alias wins by construction.
func TestValidate_PhaseSetsModelOrTierNotBoth(t *testing.T) {
	t.Run("from the file", func(t *testing.T) {
		cfg := writePanelConfig(t, `{
			"harness": "opencode",
			"phases": [{"name": "implement", "model": "opencode-go/gpt-6-luna", "tier": "strong"}, {"name": "review"}, {"name": "fix"}]
		}`)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "both `model`") || !strings.Contains(err.Error(), `"implement"`) {
			t.Fatalf("Validate() = %v, want a both-set refusal naming the phase", err)
		}
	})
	t.Run("from the document", func(t *testing.T) {
		base := pipelineBase()
		doc := &RunConfigDoc{SchemaVersion: 1, Phases: []RunConfigPhase{
			{Name: "implement", Model: "opencode-go/gpt-6-luna", Tier: "strong"},
			{Name: "review"}, {Name: "fix"},
		}}
		if err := base.ApplyRunConfig(doc); err != nil {
			t.Fatalf("ApplyRunConfig: %v", err)
		}
		err := base.Validate()
		if err == nil || !strings.Contains(err.Error(), "both `model`") {
			t.Fatalf("Validate() = %v, want a both-set refusal", err)
		}
	})
	t.Run("a panel member may set both", func(t *testing.T) {
		cfg := writePanelConfig(t, `{
			"harness": "opencode",
			"models": {"strong": "opencode-go/glm-5.3-flash"},
			"phases": [{"name": "implement"}, {"name": "review"}, {"name": "fix"}],
			"review_panel": [{"model": "opencode-go/gpt-6-luna", "tier": "strong"}]
		}`)
		if err := cfg.Validate(); err != nil {
			t.Fatalf("a member setting both model and tier must validate: %v", err)
		}
		member := cfg.EffectivePhases()[1]
		if member.Model != "opencode-go/gpt-6-luna" || member.Tier != "" {
			t.Errorf("expanded member = model %q tier %q, want the alias with the tier cleared", member.Model, member.Tier)
		}
	})
}

// A stored step naming a harness this machine has no block for is refused by
// Validate with a StepHarnessError naming the step and the harness, which is
// what the loop logs and what doctor turns into a failed check.
func TestValidate_StoredStepOnAnUnrunnableHarnessIsATypedRefusal(t *testing.T) {
	base := pipelineBase()
	if err := base.ApplyRunConfig(&RunConfigDoc{SchemaVersion: 1, Phases: []RunConfigPhase{
		{Name: "implement"},
		{Name: "review", Harness: "codex"},
		{Name: "fix"},
	}}); err != nil {
		t.Fatalf("ApplyRunConfig: %v", err)
	}
	err := base.Validate()
	var she *StepHarnessError
	if !errors.As(err, &she) {
		t.Fatalf("Validate() = %v (%T), want a *StepHarnessError", err, err)
	}
	if she.Harness != "codex" || she.Step != `phases[1] ("review")` {
		t.Errorf("StepHarnessError = %+v, want step %q on codex", she, `phases[1] ("review")`)
	}
	if !strings.Contains(she.Error(), "codex") || !strings.Contains(she.Error(), "review") {
		t.Errorf("the refusal does not name the step and harness: %v", she)
	}
}

// The pipeline section is judged by KEY presence (decision 6bf1fa15): a stored
// `{"prompt": ""}` is a pipeline statement and must clear the local phases and
// panel, not read as an absent section (CLA-598 fix round 2, 2.review-3.2).
func TestApplyRunConfig_AnExplicitlyEmptyPromptIsAPipelineKey(t *testing.T) {
	base := pipelineBase()
	base.Prompt = "local prompt"

	var doc RunConfigDoc
	if err := json.Unmarshal([]byte(`{"$schema_version": 1, "prompt": ""}`), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !doc.pipelineSet() {
		t.Fatal("an explicitly present empty `prompt` did not mark the pipeline section")
	}
	if doc.Empty() {
		t.Fatal("a document carrying `prompt` read as empty, so it would not overlay at all")
	}
	if err := base.ApplyRunConfig(&doc); err != nil {
		t.Fatalf("ApplyRunConfig: %v", err)
	}
	if base.Prompt != "" {
		t.Errorf("prompt = %q, want the stored empty string", base.Prompt)
	}
	if base.Phases != nil {
		t.Errorf("phases = %+v, want them cleared: the stored section omitted them", base.Phases)
	}
	if base.ReviewPanel != nil {
		t.Errorf("panel = %+v, want it cleared: the stored section omitted it", base.ReviewPanel)
	}
	if base.ReviewPanelConcurrency != 3 {
		t.Errorf("review_panel_concurrency = %d, want the local 3 kept — it is machine-owned", base.ReviewPanelConcurrency)
	}

	// CONTRAST: a document without the key still leaves the local pipeline alone.
	absent, err := json.Marshal(map[string]any{"$schema_version": 1, "max_turns": 9})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var noPrompt RunConfigDoc
	if err := json.Unmarshal(absent, &noPrompt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if noPrompt.pipelineSet() {
		t.Error("a document with no pipeline key marked the section set")
	}
}

// The plane is deliberately stricter than the CLI about a panel with no
// sequence to expand (decision 94497792): it stores a panel only beside the
// phase it multiplies. propose-config must refuse that shape rather than offer
// a document the operator could never ratify (CLA-598 fix round 2,
// 2.review-3.3/1.3/5.2).
func TestRunConfigDocument_RefusesAPanelThePlaneCannotRatify(t *testing.T) {
	c := defaults()
	c.Harness = "opencode"
	c.Prompt = "custom operator prompt"
	c.ReviewPanel = []ReviewPanelMember{{Name: "a", Model: "opencode-go/gpt-6-luna"}}
	if err := c.Validate(); err != nil {
		t.Fatalf("the CLI itself tolerates this shape (the panel is inert locally): %v", err)
	}

	doc, err := c.RunConfigDocument()
	if err == nil {
		t.Fatalf("RunConfigDocument exported %v, want a refusal: the plane refuses a panel with no expandable review phase", doc)
	}
	if !strings.Contains(err.Error(), "review_panel") || !strings.Contains(err.Error(), "review") {
		t.Errorf("the refusal does not name the panel and the missing review phase: %v", err)
	}

	// A config whose sequence CAN expand the panel still exports.
	ok := pipelineBase()
	ok.Prompt = ""
	if err := ok.Validate(); err != nil {
		t.Fatalf("the expandable fixture must validate: %v", err)
	}
	rendered, err := ok.RunConfigDocument()
	if err != nil {
		t.Fatalf("RunConfigDocument refused an expandable panel: %v", err)
	}
	if _, has := rendered["review_panel"]; !has {
		t.Errorf("the expandable panel was omitted from the document: %v", rendered)
	}
}
