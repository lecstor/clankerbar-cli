package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

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

// TestReviewPanel_AbsentIsTheSingleReviewer (CLA-639, decision a0b1373b): a
// config that never mentions a panel is the shape that existed before this
// feature — ONE review phase, with no expansion, no panel members and no roles
// beyond `review-1`. The panel is opt-in; there is no default and no preset.
func TestReviewPanel_AbsentIsTheSingleReviewer(t *testing.T) {
	cfg := writePanelConfig(t, `{
		"harness": "opencode",
		"phases": [{"name": "implement"}, {"name": "review"}, {"name": "fix"}]
	}`)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a config that never mentions a panel must validate: %v", err)
	}
	if panel := cfg.EffectivePanel(); len(panel) != 0 {
		t.Fatalf("an absent review_panel resolved to %d members, want none — the CLI ships no panel of its own", len(panel))
	}
	got := cfg.EffectivePhases()
	if len(got) != 3 {
		t.Fatalf("effective phases = %d, want 3 (implement, review, fix) — the review must not be multiplied", len(got))
	}
	if got[1].Name != ReviewPhaseName || got[1].IsPanelMember() {
		t.Errorf("phase 1 = %q (panel=%v), want the operator's own %q phase", got[1].Name, got[1].IsPanelMember(), ReviewPhaseName)
	}
	if got[1].Prompt != reviewOnlyBrief {
		t.Errorf("the single review did not resolve to the historical review-only brief")
	}
	if got[1].Name != "review" {
		t.Errorf("the single review phase = %q, want the configured name %q — the historical reviewer role derives from it", got[1].Name, ReviewPhaseName)
	}
	if got[2].Prompt != fixBrief {
		t.Errorf("the fix after the single review did not resolve to the legacy fix brief — the legacy fix path is what an absent panel must keep")
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

// TestReviewPanel_FourMembersExpand expands the operator's own four-member
// shape: the same model three times plus another once (decision c8e7509a's
// fleet, spelled out — an EXAMPLE, not a default), each an independent review
// phase on the same configured review.
func TestReviewPanel_FourMembersExpand(t *testing.T) {
	cfg := writePanelConfig(t, `{
		"harness": "opencode",
		"phases": [{"name": "implement"}, {"name": "review"}, {"name": "fix"}],
		"review_panel": [
			{"harness": "opencode", "model": "opencode-go/gpt-6-luna"},
			{"harness": "opencode", "model": "opencode-go/gpt-6-luna"},
			{"harness": "opencode", "model": "opencode-go/gpt-6-luna"},
			{"harness": "opencode", "model": "opencode-go/glm-5.3-flash"}
		]
	}`)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if panel := cfg.EffectivePanel(); len(panel) != 4 {
		t.Fatalf("the configured panel has %d members, want 4", len(panel))
	}
	got := cfg.EffectivePhases()
	if len(got) != 6 {
		t.Fatalf("effective phases = %d, want 6 (implement, four reviewers, fix)", len(got))
	}
	var luna, glm int
	for i := 1; i <= 4; i++ {
		ph := got[i]
		if !ph.IsPanelMember() || ph.Name != "review-"+strconv.Itoa(i) {
			t.Errorf("phase %d = %q (panel=%v), want panel member review-%d", i, ph.Name, ph.IsPanelMember(), i)
		}
		if ph.Prompt != reviewOnlyBrief {
			t.Errorf("%s did not inherit the review phase's resolved brief (the panel-member variant is swapped in at spawn, by invocationFor)", ph.Name)
		}
		switch ph.Model {
		case "opencode-go/gpt-6-luna":
			luna++
		case "opencode-go/glm-5.3-flash":
			glm++
		}
	}
	if luna != 3 || glm != 1 {
		t.Errorf("panel members = %d x gpt-6-luna + %d x glm-5.3-flash, want 3 + 1", luna, glm)
	}
	if got[5].Name != FixPhaseName || got[5].IsPanelMember() {
		t.Errorf("the fix phase was swallowed by the expansion: %q (panel=%v)", got[5].Name, got[5].IsPanelMember())
	}
}

// TestReviewPanel_ValidationRefusals pins the refusals the operator's decision
// names: an empty panel, a member whose harness (own or inherited) is not
// configured, and a panel beside a review phase it cannot multiply.
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
		if !strings.Contains(err.Error(), "remove the key for a single reviewer") {
			t.Errorf("the empty-panel refusal does not say how to ask for a single reviewer: %v", err)
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
	t.Run("a member INHERITING an unconfigured harness is refused", func(t *testing.T) {
		// The member names no harness, so it reviews on the review phase's —
		// which the member inherits in turn. Validation runs on the EFFECTIVE
		// panel, so the inherited harness is held to the same rule as a harness a
		// member names itself: a harness other than the run-wide one needs a
		// block of its own, whether the phase names it or inherits it.
		cfg := writePanelConfig(t, `{
			"harness": "claude",
			"phases": [{"name": "implement"}, {"name": "review", "harness": "opencode"}, {"name": "fix"}],
			"review_panel": [{"model": "opencode-go/gpt-6-luna"}]
		}`)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "harnesses.opencode") {
			t.Fatalf("Validate() = %v, want the inherited harness held to the missing-block rule", err)
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
	t.Run("a panelled custom review prompt is refused", func(t *testing.T) {
		cfg := writePanelConfig(t, `{
			"harness": "claude",
			"phases": [{"name": "implement"}, {"name": "review", "prompt": "my own gate"}, {"name": "fix"}],
			"review_panel": [{"model": "opus"}]
		}`)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "built-in") {
			t.Fatalf("Validate() = %v, want the panel-needs-a-built-in-review refusal", err)
		}
	})
	t.Run("a panelled FINAL review is refused", func(t *testing.T) {
		// The review is final, so it runs the historical review-and-fix brief and
		// never expands; a panel beside it is a no-op the operator could only
		// discover from the log.
		cfg := writePanelConfig(t, `{
			"harness": "claude",
			"phases": [{"name": "implement"}, {"name": "review"}],
			"review_panel": [{"model": "opus"}]
		}`)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "built-in, non-final") {
			t.Fatalf("Validate() = %v, want the final-review refusal", err)
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

// TestReviewPanel_UnphasedConfigWithAPanelStaysInert: an unphased run has no
// phase sequence at all, so there is nothing for a panel to multiply; the key
// is inert rather than refused (the marshalled round trip of a config that
// never mentioned one must survive).
func TestReviewPanel_UnphasedConfigWithAPanelStaysInert(t *testing.T) {
	cfg := &Config{
		Harness:     "claude",
		Prompt:      "Work the backlog.",
		ReviewPanel: []ReviewPanelMember{{Model: "opus"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("an unphased config with an inert panel must validate: %v", err)
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
