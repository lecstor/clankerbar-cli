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
	base.ApplyRunConfig(doc)

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
	base.ApplyRunConfig(&RunConfigDoc{
		SchemaVersion: 1,
		Harnesses:     map[string]RunConfigHarnessBlock{"opencode": {Model: "oc-default", Models: map[string]string{"cheap": "oc-cheap"}}},
	})
	if got := base.Harnesses["opencode"]; got.Model != "oc-default" {
		t.Errorf("opencode block model = %q, want the stored one (overlay must allocate the nil map)", got.Model)
	}
	// The policy half lands; the machine-local half stays absent, as everywhere else.
	if got := base.SessionFor("opencode"); got.Model != "oc-default" || got.ConfigDir != "" {
		t.Errorf("opencode session model=%q configDir=%q, want the stored model and no machine-local wiring", got.Model, got.ConfigDir)
	}
}

func TestApplyRunConfig_EmptyDocumentIsANoOp(t *testing.T) {
	base := overlayBase()
	before := base.Clone()
	for _, doc := range []*RunConfigDoc{nil, {}, {SchemaVersion: 1}} {
		base.ApplyRunConfig(doc)
	}
	if !reflect.DeepEqual(base.Models, before.Models) || base.Harness != before.Harness || base.MaxTurns != before.MaxTurns {
		t.Errorf("an empty document changed something: harness=%q turns=%d", base.Harness, base.MaxTurns)
	}
}

func TestClone_AnOverlayNeverWritesThroughToTheBase(t *testing.T) {
	base := overlayBase()
	cp := base.Clone()
	cp.ApplyRunConfig(&RunConfigDoc{
		SchemaVersion: 1,
		Harness:       "codex",
		Models:        map[string]string{"only": "cp"},
		Budget:        &RunConfigBudget{PerHarness: map[string]HarnessBudget{"codex": {MaxTokens: 1}}},
		Escalation:    &RunConfigEscalation{CategoryRules: map[string]string{"x": "y"}},
	})
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
