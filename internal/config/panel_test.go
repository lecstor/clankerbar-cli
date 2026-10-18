package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The single-review opt-out (CLA-639) as a fixture helper.
//
// A review PANEL expands each built-in review phase into one review phase per
// member, and an unset `review_panel` resolves to the trial panel (the operator
// chose it for every task: decision c8e7509a). The other fixtures pin the
// sequence BEFORE that expansion — one review, one record, one seam — so they
// say so out loud here rather than leaning on the field's zero value (nil means
// TRIAL, not "none"). Go-built configs may spell the opt-out directly; a FILE
// that writes `"review_panel": []` is refused instead (reviewPanelSet).
func noPanel(c *Config) *Config {
	c.ReviewPanel = []ReviewPanelMember{}
	return c
}

// writePanelConfig writes a config file through Load's own path so the JSON
// round trip — including how "absent" differs from "empty" — is the real one.
func writePanelConfig(t *testing.T, body string) *Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load(%s): %v", p, err)
	}
	return cfg
}

// TestReviewPanel_TrialDefault: a config that never mentions a panel runs the
// operator's trial panel on every task (decision c8e7509a) — three runs of
// gpt-6-luna and one of glm-5.3-flash, each an independent review phase.
func TestReviewPanel_TrialDefault(t *testing.T) {
	cfg := writePanelConfig(t, `{
		"harness": "opencode",
		"phases": [{"name": "implement"}, {"name": "review"}, {"name": "fix"}]
	}`)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a config that never mentions a panel must validate: %v", err)
	}
	panel := cfg.EffectivePanel()
	if len(panel) != 4 {
		t.Fatalf("the trial panel has %d members, want 4", len(panel))
	}
	var luna, glm int
	for _, m := range panel {
		if m.Harness != "opencode" {
			t.Errorf("member %q runs on harness %q, want opencode", PanelMemberName(panel, 0), m.Harness)
		}
		switch m.Model {
		case "opencode-go/gpt-6-luna":
			luna++
		case "opencode-go/glm-5.3-flash":
			glm++
		}
	}
	if luna != 3 || glm != 1 {
		t.Errorf("trial panel models = %d x gpt-6-luna + %d x glm-5.3-flash, want 3 + 1", luna, glm)
	}
	// And the effective sequence is what the driver iterates: implement + four
	// reviewers + fix, each reviewer under its own role.
	got := cfg.EffectivePhases()
	if len(got) != 6 {
		t.Fatalf("effective phases = %d, want 6 (implement, four reviewers, fix)", len(got))
	}
	wantRoles := []string{"review-1", "review-2", "review-3", "review-4"}
	for i, role := range wantRoles {
		if got[i+1].Name != role || !got[i+1].IsPanelMember() {
			t.Errorf("phase %d = %q (panel=%v), want panel member %q", i+1, got[i+1].Name, got[i+1].IsPanelMember(), role)
		}
		if got[i+1].Prompt == "" {
			t.Errorf("%s resolved to no prompt — the member inherits the review phase's brief", role)
		}
	}
	if got[5].Name != FixPhaseName || got[5].IsPanelMember() {
		t.Errorf("the fix phase was swallowed by the expansion: %q (panel=%v)", got[5].Name, got[5].IsPanelMember())
	}
}

// TestReviewPanel_ExplicitMembers expands exactly the list the operator wrote,
// allowing the same model more than once: each entry is an independent review.
func TestReviewPanel_ExplicitMembers(t *testing.T) {
	cfg := writePanelConfig(t, `{
		"harness": "claude",
		"phases": [{"name": "implement"}, {"name": "review"}, {"name": "fix"}],
		"review_panel": [
			{"name": "alpha", "model": "opus"},
			{"model": "opus"},
			{"tier": "cheap"}
		]
	}`)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	got := cfg.EffectivePhases()
	if len(got) != 5 {
		t.Fatalf("effective phases = %d, want 5", len(got))
	}
	roles := []string{got[1].Name, got[2].Name, got[3].Name}
	if roles[0] != "alpha" || roles[1] != "review-2" || roles[2] != "review-3" {
		t.Errorf("reviewer roles = %v, want [alpha review-2 review-3] — a named member keeps its name, an unnamed one takes its position", roles)
	}
	// The same model twice is deliberate: two independent reviewers, not one
	// deduplicated reviewer.
	if got[1].Model != "opus" || got[2].Model != "opus" {
		t.Errorf("models = %q/%q, want opus/opus", got[1].Model, got[2].Model)
	}
	if got[3].Tier != "cheap" || got[3].Model != "" {
		t.Errorf("the tier member resolved to model %q tier %q, want the tier with no pinned model", got[3].Model, got[3].Tier)
	}
	for _, ph := range got[1:4] {
		if ph.Harness != "claude" {
			t.Errorf("member %q runs on %q, want the review phase's claude harness", ph.Name, ph.Harness)
		}
	}
}

// TestReviewPanel_ValidationRefusals pins the two refusals the task names: an
// empty panel, and a member whose harness is not configured.
func TestReviewPanel_ValidationRefusals(t *testing.T) {
	base := func(panel string) string {
		return `{
			"harness": "claude",
			"phases": [{"name": "implement"}, {"name": "review"}, {"name": "fix"}],
			"review_panel": ` + panel + `
		}`
	}
	t.Run("an empty panel is refused", func(t *testing.T) {
		cfg := writePanelConfig(t, base(`[]`))
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "review_panel is empty") {
			t.Fatalf("Validate() = %v, want a review_panel-is-empty refusal", err)
		}
	})
	t.Run("a member on an unknown harness is refused", func(t *testing.T) {
		cfg := writePanelConfig(t, base(`[{"harness": "not-a-harness", "model": "m"}]`))
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "unknown harness") {
			t.Fatalf("Validate() = %v, want an unknown-harness refusal", err)
		}
	})
	t.Run("a member on an unconfigured harness is refused", func(t *testing.T) {
		cfg := writePanelConfig(t, base(`[{"harness": "opencode", "model": "m"}]`))
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "harnesses.opencode") {
			t.Fatalf("Validate() = %v, want a missing-harnesses-block refusal", err)
		}
	})
	t.Run("a panelled custom review prompt is refused", func(t *testing.T) {
		cfg := writePanelConfig(t, `{
			"harness": "claude",
			"phases": [{"name": "implement"}, {"name": "review", "prompt": "my own gate"}, {"name": "fix"}],
			"review_panel": [{"model": "opus"}]
		}`)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "its own prompt") {
			t.Fatalf("Validate() = %v, want the custom-prompt refusal", err)
		}
	})
	t.Run("a configured member harness is accepted", func(t *testing.T) {
		cfg := writePanelConfig(t, `{
			"harness": "claude",
			"harnesses": {"opencode": {"config_dir": "/tmp/oc", "mcp_config_path": "/tmp/oc.json"}},
			"phases": [{"name": "implement"}, {"name": "review"}, {"name": "fix"}],
			"review_panel": [{"harness": "opencode", "model": "opencode-go/gpt-6-luna"}]
		}`)
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate() = %v, want a configured opencode member accepted", err)
		}
	})
}

// TestReviewPanel_NoPanelWithoutAReviewPhase: a config with no phased review has
// nothing to expand, so an absent panel stays inert and does not multiply
// anything. This is the unphased-run guarantee the task keeps.
func TestReviewPanel_NoPanelWithoutAReviewPhase(t *testing.T) {
	cfg := &Config{Harness: "claude", Prompt: "Work the backlog."}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("an unphased config must validate: %v", err)
	}
	if got := cfg.EffectivePhases(); len(got) != 1 {
		t.Fatalf("an unphased run resolved to %d phases, want 1", len(got))
	}
}

// TestReviewPanel_ModelPinnedDirectly: a member's model is an alias, not a tier
// bucket, and it wins over the review phase's tier without being looked up in
// the tier map.
func TestReviewPanel_ModelPinnedDirectly(t *testing.T) {
	cfg := &Config{
		Harness: "opencode",
		Phases:  []Phase{{Name: ImplementPhaseName}, {Name: ReviewPhaseName}, {Name: FixPhaseName}},
		ReviewPanel: []ReviewPanelMember{
			{Model: "opencode-go/gpt-6-luna"},
		},
	}
	member := cfg.EffectivePhases()[1]
	if got, ok := cfg.ModelForPhase(member); got != "opencode-go/gpt-6-luna" || !ok {
		t.Errorf("ModelForPhase(member) = (%q, %v), want the pinned alias", got, ok)
	}
}
