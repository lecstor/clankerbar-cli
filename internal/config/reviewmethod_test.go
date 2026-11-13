package config

import (
	"strings"
	"testing"
)

// mapsSessionConfig loads a config body through the normal file loader, the way
// an operator's config reaches Validate.
func mapsSessionConfig(t *testing.T, body string) *Config {
	t.Helper()
	return writePanelConfig(t, body)
}

// TestReviewMethod_ParsesBothSpellings (CLA-869): `review_method:
// "maps-session"` and its options parse onto the phase, and an omitted method
// stays the shipped single-pass review rather than some non-empty default.
func TestReviewMethod_ParsesBothSpellings(t *testing.T) {
	cfg := mapsSessionConfig(t, `{
		"harness": "opencode",
		"phases": [
			{"name": "implement"},
			{"name": "review", "review_method": "maps-session",
			 "maps_session": {"chunk_size": 10, "notes_model": "opencode-go/strong", "findings_model": "opencode-go/cheap", "flow_max": 7}},
			{"name": "fix"}
		]
	}`)
	phases := cfg.EffectivePhases()
	review := phases[1]
	if !review.UsesMapsSession() {
		t.Fatalf("review_method did not survive load: %#v", review.ReviewMethod)
	}
	got := review.MapsSessionDefaults()
	if got.ChunkSize != 10 || got.FlowMax != 7 {
		t.Errorf("maps_session = chunk_size %d / flow_max %d, want 10 / 7", got.ChunkSize, got.FlowMax)
	}
	if got.NotesModel != "opencode-go/strong" || got.FindingsModel != "opencode-go/cheap" {
		t.Errorf("stage models = %q / %q, want the configured pair", got.NotesModel, got.FindingsModel)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want the opencode maps-session review accepted", err)
	}

	// A mixed-harness sequence: the review phase names opencode under a claude
	// run, with its own block. The capability question is about THIS PHASE's
	// harness, not the run's.
	mixed := mapsSessionConfig(t, `{
		"harness": "claude",
		"harnesses": {"opencode": {"config_dir": "/tmp/oc", "mcp_config_path": "/tmp/oc.json"}},
		"phases": [
			{"name": "implement"},
			{"name": "review", "harness": "opencode", "review_method": "maps-session"},
			{"name": "fix"}
		]
	}`)
	if err := mixed.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want a maps-session review phase on opencode under a claude run accepted", err)
	}

	plain := mapsSessionConfig(t, `{
		"harness": "opencode",
		"phases": [{"name": "implement"}, {"name": "review"}, {"name": "fix"}]
	}`)
	if plain.EffectivePhases()[1].UsesMapsSession() {
		t.Error("a review phase that names no method resolved to maps-session — the shipped single-pass review must stay the default")
	}
	if err := plain.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want the untouched shape accepted", err)
	}
}

// TestMapsSessionOptions_Defaults (CLA-869): the zero-value profile is the
// benchmarked one (25 items a turn, 40 flow items), and an empty stage model
// inherits the phase's model rather than inventing one.
func TestMapsSessionOptions_Defaults(t *testing.T) {
	got := (Phase{}).MapsSessionDefaults()
	if got.ChunkSize != 25 {
		t.Errorf("ChunkSize = %d, want the benchmark's 25", got.ChunkSize)
	}
	if DefaultMapsSessionChunkSize != 25 || DefaultMapsSessionFlowMax != 40 {
		t.Errorf("defaults = %d / %d, want the benchmarked 25 / 40", DefaultMapsSessionChunkSize, DefaultMapsSessionFlowMax)
	}
	if got.FlowMax != 40 {
		t.Errorf("FlowMax = %d, want the benchmark's 40", got.FlowMax)
	}
	if got.NotesModel != "" || got.FindingsModel != "" {
		t.Errorf("stage models = %q / %q, want empty (inherit the phase's model)", got.NotesModel, got.FindingsModel)
	}

	// A partial block fills only the knobs it leaves zero, and whitespace is not
	// a model name.
	partial := Phase{MapsSession: &MapsSessionOptions{ChunkSize: 5, NotesModel: "  "}}
	got = partial.MapsSessionDefaults()
	if got.ChunkSize != 5 || got.FlowMax != DefaultMapsSessionFlowMax {
		t.Errorf("partial = chunk_size %d / flow_max %d, want 5 / the default", got.ChunkSize, got.FlowMax)
	}
	if got.NotesModel != "" {
		t.Errorf("NotesModel = %q, want whitespace trimmed to empty", got.NotesModel)
	}
}

// TestReviewMethod_ValidateRefusesNonSequencingHarnesses (CLA-869): a harness
// that cannot continue one session across turns is refused up front, and the
// message names the harness rather than letting the run discover it mid-phase.
func TestReviewMethod_ValidateRefusesNonSequencingHarnesses(t *testing.T) {
	for _, harnessName := range []string{"claude", "codex"} {
		cfg := mapsSessionConfig(t, `{
			"harness": "`+harnessName+`",
			"phases": [{"name": "implement"}, {"name": "review", "review_method": "maps-session"}, {"name": "fix"}]
		}`)
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("%s: Validate() accepted maps-session on a harness with no turn sequences", harnessName)
		}
		if !strings.Contains(err.Error(), `"`+harnessName+`"`) {
			t.Errorf("%s: the refusal does not name the harness: %v", harnessName, err)
		}
		if !strings.Contains(err.Error(), "single-pass") {
			t.Errorf("%s: the refusal does not say what to do instead: %v", harnessName, err)
		}
	}
}

// TestReviewMethod_ValidateRefusals pins the inert and nonsensical shapes.
func TestReviewMethod_ValidateRefusals(t *testing.T) {
	t.Run("unknown method", func(t *testing.T) {
		cfg := mapsSessionConfig(t, `{
			"harness": "opencode",
			"phases": [{"name": "implement"}, {"name": "review", "review_method": "maps"}, {"name": "fix"}]
		}`)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), `unknown review_method "maps"`) {
			t.Fatalf("Validate() = %v, want an unknown-method refusal", err)
		}
	})
	t.Run("options without the method are inert", func(t *testing.T) {
		cfg := mapsSessionConfig(t, `{
			"harness": "opencode",
			"phases": [{"name": "implement"}, {"name": "review", "maps_session": {"chunk_size": 5}}, {"name": "fix"}]
		}`)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "would be inert") {
			t.Fatalf("Validate() = %v, want an inert-options refusal", err)
		}
	})
	t.Run("the method on a non-review phase", func(t *testing.T) {
		cfg := mapsSessionConfig(t, `{
			"harness": "opencode",
			"phases": [{"name": "implement", "review_method": "maps-session"}, {"name": "review"}, {"name": "fix"}]
		}`)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "needs a review phase") {
			t.Fatalf("Validate() = %v, want a needs-a-review-phase refusal", err)
		}
	})
	t.Run("negative chunk size", func(t *testing.T) {
		cfg := mapsSessionConfig(t, `{
			"harness": "opencode",
			"phases": [{"name": "implement"}, {"name": "review", "review_method": "maps-session", "maps_session": {"chunk_size": -1}}, {"name": "fix"}]
		}`)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "chunk_size must be positive") {
			t.Fatalf("Validate() = %v, want a negative-chunk-size refusal", err)
		}
	})
	t.Run("the method on a verify phase", func(t *testing.T) {
		cfg := mapsSessionConfig(t, `{
			"harness": "opencode",
			"phases": [{"name": "implement"}, {"name": "review"}, {"name": "fix"}, {"name": "verify", "review_method": "maps-session"}]
		}`)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "needs a review phase") {
			t.Fatalf("Validate() = %v, want a verify phase refused the review method", err)
		}
	})
	t.Run("negative flow cap", func(t *testing.T) {
		cfg := mapsSessionConfig(t, `{
			"harness": "opencode",
			"phases": [{"name": "implement"}, {"name": "review", "review_method": "maps-session", "maps_session": {"flow_max": -2}}, {"name": "fix"}]
		}`)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "flow_max must be positive") {
			t.Fatalf("Validate() = %v, want a negative-flow_max refusal", err)
		}
	})
	t.Run("explicit single-pass is the default, not a refusal", func(t *testing.T) {
		cfg := mapsSessionConfig(t, `{
			"harness": "claude",
			"phases": [{"name": "implement"}, {"name": "review", "review_method": "single-pass"}, {"name": "fix"}]
		}`)
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate() = %v, want an explicit single-pass review accepted on any harness", err)
		}
	})
}

// TestReviewMethod_PanelMemberInheritance (CLA-869): a member inherits the
// review phase's method and options, and its own statement wins — so a panel can
// mix a maps-session seat with a single-pass one.
func TestReviewMethod_PanelMemberInheritance(t *testing.T) {
	cfg := mapsSessionConfig(t, `{
		"harness": "opencode",
		"phases": [
			{"name": "implement"},
			{"name": "review", "review_method": "maps-session", "maps_session": {"chunk_size": 10}},
			{"name": "fix"}
		],
		"review_panel": [
			{"model": "opencode-go/one"},
			{"model": "opencode-go/two", "review_method": "single-pass"},
			{"model": "opencode-go/three", "maps_session": {"chunk_size": 4}}
		]
	}`)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want the mixed panel accepted", err)
	}
	var members []Phase
	for _, ph := range cfg.EffectivePhases() {
		if ph.IsPanelMember() {
			members = append(members, ph)
		}
	}
	if len(members) != 3 {
		t.Fatalf("effective members = %d, want 3", len(members))
	}
	if !members[0].UsesMapsSession() || members[0].MapsSessionDefaults().ChunkSize != 10 {
		t.Errorf("member 1 = method %q chunk %d, want the review phase's maps-session / 10", members[0].ReviewMethod, members[0].MapsSessionDefaults().ChunkSize)
	}
	if members[1].UsesMapsSession() {
		t.Errorf("member 2 kept maps-session, want its own single-pass override to win")
	}
	if !members[2].UsesMapsSession() || members[2].MapsSessionDefaults().ChunkSize != 4 {
		t.Errorf("member 3 = method %q chunk %d, want the inherited method with its own chunk size 4", members[2].ReviewMethod, members[2].MapsSessionDefaults().ChunkSize)
	}
}

// TestReviewMethod_PanelMemberRefusedOnANonSequencingHarness: a member's own
// harness decides whether it can run the sequence, so a panel with one opencode
// seat and one claude maps-session seat is refused naming claude.
func TestReviewMethod_PanelMemberRefusedOnANonSequencingHarness(t *testing.T) {
	cfg := mapsSessionConfig(t, `{
		"harness": "opencode",
		"harnesses": {"claude": {"config_dir": "/tmp/claude", "model": "opus"}},
		"phases": [{"name": "implement"}, {"name": "review", "review_method": "maps-session"}, {"name": "fix"}],
		"review_panel": [
			{"model": "opencode-go/one"},
			{"harness": "claude", "model": "opus"}
		]
	}`)
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() accepted a maps-session panel member on claude")
	}
	if !strings.Contains(err.Error(), `"claude"`) {
		t.Errorf("the refusal does not name the member's harness: %v", err)
	}
}

// TestReviewMethod_AbsentKeepsTheConfigIdentity (CLA-869): the knobs stay out of
// Config.Identity when unset — the same rule every optional phase field follows —
// and a config that adopts the method correctly moves the identity.
func TestReviewMethod_AbsentKeepsTheConfigIdentity(t *testing.T) {
	cfg := &Config{Harness: "claude", Phases: []Phase{
		{Name: "implement"}, {Name: "review"}, {Name: "fix"},
	}}
	// The same golden TestReviewPanel_AbsentKeepsTheConfigIdentity pins: the
	// pre-panel, pre-maps-session identity of this literal config. An untouched
	// config must hash exactly as it did, or every daemon's fleet presence row
	// moves for a feature nobody turned on.
	const preMapsIdentity = "e83c686b2bfc9c8c60e23d1946116ca7ca5c71da23c7fa50be60a16452cdd7f8"
	if got := cfg.Identity(); got != preMapsIdentity {
		t.Errorf("Identity() = %s, want the untouched %s", got, preMapsIdentity)
	}
	cfg.Phases[1].ReviewMethod = ReviewMethodMapsSession
	if cfg.Identity() == preMapsIdentity {
		t.Error("adopting maps-session must move the identity — it is a config edit")
	}
}

// TestReviewMethod_StoredDocumentCarriesTheKnobs (CLA-869): the plane's stored
// document shape decodes and applies the method and its options onto the phase
// and the panel member, so a plane whose schema learns the keys needs no
// second change here.
func TestReviewMethod_StoredDocumentCarriesTheKnobs(t *testing.T) {
	base := &Config{Harness: "opencode", Prompt: "local"}
	doc := &RunConfigDoc{SchemaVersion: 1,
		Phases: []RunConfigPhase{
			{Name: "implement"},
			{Name: "review", ReviewMethod: ReviewMethodMapsSession,
				MapsSession: &MapsSessionOptions{ChunkSize: 7, FindingsModel: "opencode-go/cheap"}},
			{Name: "fix"},
		},
		ReviewPanel: []RunConfigPanelMember{
			{Model: "opencode-go/one"},
			{Model: "opencode-go/two", ReviewMethod: ReviewMethodSinglePass},
		},
	}
	if err := base.ApplyRunConfig(doc); err != nil {
		t.Fatalf("ApplyRunConfig: %v", err)
	}
	if !base.Phases[1].UsesMapsSession() {
		t.Fatalf("the stored review phase lost its method: %#v", base.Phases[1])
	}
	if got := base.Phases[1].MapsSessionDefaults(); got.ChunkSize != 7 || got.FindingsModel != "opencode-go/cheap" {
		t.Errorf("stored options = %+v, want chunk_size 7 / findings opencode-go/cheap", got)
	}
	if got := base.ReviewPanel[1].ReviewMethod; got != ReviewMethodSinglePass {
		t.Errorf("stored member method = %q, want single-pass", got)
	}
	// A clone must not alias the stored block.
	clone := base.Clone()
	clone.Phases[1].MapsSession.ChunkSize = 99
	if base.Phases[1].MapsSession.ChunkSize != 7 {
		t.Error("a clone shares the base config's maps_session block")
	}
}

// TestReviewMethod_ProposeRefusesUntilThePlaneKnowsIt (CLA-869): propose-config
// must not emit a document the plane's v1 schema would refuse, and must not
// silently drop the method either.
func TestReviewMethod_ProposeRefusesUntilThePlaneKnowsIt(t *testing.T) {
	cfg := mapsSessionConfig(t, `{
		"harness": "opencode",
		"phases": [{"name": "implement"}, {"name": "review", "review_method": "maps-session"}, {"name": "fix"}]
	}`)
	_, err := cfg.RunConfigDocument()
	if err == nil {
		t.Fatal("RunConfigDocument exported a maps-session phase the plane's v1 schema refuses")
	}
	if !strings.Contains(err.Error(), "review_method") || !strings.Contains(err.Error(), "schema") {
		t.Errorf("the refusal does not name the key and the reason: %v", err)
	}

	// A panel member's own method is refused the same way.
	panel := mapsSessionConfig(t, `{
		"harness": "opencode",
		"phases": [{"name": "implement"}, {"name": "review"}, {"name": "fix"}],
		"review_panel": [{"model": "opencode-go/one", "review_method": "maps-session"}]
	}`)
	if _, err := panel.RunConfigDocument(); err == nil || !strings.Contains(err.Error(), "review_panel[0]") {
		t.Errorf("RunConfigDocument() = %v, want a refusal naming the member", err)
	}
}
