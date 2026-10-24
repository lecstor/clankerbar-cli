package config

import (
	"bytes"
	"encoding/json"
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
	if got[1].Model.First() != "opus" || got[2].Model.First() != "opus" {
		t.Errorf("models = %q/%q, want opus/opus", got[1].Model, got[2].Model)
	}
	if got[3].Tier != "cheap" || !got[3].Model.Empty() {
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
		switch ph.Model.First() {
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
		ReviewPanel: []ReviewPanelMember{{Model: ModelRoutes{"opus"}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("an unphased config with an inert panel must validate: %v", err)
	}
	if got := cfg.EffectivePhases(); len(got) != 1 {
		t.Fatalf("an unphased run resolved to %d phases, want 1", len(got))
	}
}

// TestReviewPanel_ConcurrencyCap: the fan-out cap is 0/absent = the whole
// panel, 1 = the historical sequential path, and any larger value is clamped to
// the panel's own size. A single reviewer never reports more than 1, and a
// negative cap is refused rather than read as "unbounded".
func TestReviewPanel_ConcurrencyCap(t *testing.T) {
	cfg := &Config{
		Harness: "claude",
		Phases:  []Phase{{Name: ImplementPhaseName}, {Name: ReviewPhaseName}, {Name: FixPhaseName}},
		ReviewPanel: []ReviewPanelMember{
			{}, {}, {},
		},
	}
	if got := cfg.EffectivePanelConcurrency(3); got != 3 {
		t.Errorf("an absent cap resolved to %d, want the whole panel (3)", got)
	}
	cfg.ReviewPanelConcurrency = 1
	if got := cfg.EffectivePanelConcurrency(3); got != 1 {
		t.Errorf("cap 1 resolved to %d, want the sequential path", got)
	}
	cfg.ReviewPanelConcurrency = 2
	if got := cfg.EffectivePanelConcurrency(3); got != 2 {
		t.Errorf("cap 2 over a panel of 3 resolved to %d, want 2", got)
	}
	cfg.ReviewPanelConcurrency = 9
	if got := cfg.EffectivePanelConcurrency(3); got != 3 {
		t.Errorf("a cap larger than the panel resolved to %d, want the panel (3)", got)
	}
	if got := cfg.EffectivePanelConcurrency(1); got != 1 {
		t.Errorf("a single reviewer resolved to %d, want 1", got)
	}
	cfg.ReviewPanelConcurrency = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("a negative review_panel_concurrency validated")
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
			{Model: ModelRoutes{"opencode-go/gpt-6-luna"}},
		},
	}
	member := cfg.EffectivePhases()[1]
	if got, ok := cfg.ModelForPhase(member); got != "opencode-go/gpt-6-luna" || !ok {
		t.Errorf("ModelForPhase(member) = (%q, %v), want the pinned alias", got, ok)
	}
}

// TestReviewPanel_NullIsAbsent (CLA-639 fix pass 3): a supervised daemon's child
// config is a MARSHALLED config, and before this fix the marshalled form of a
// config with no panel wrote `"review_panel": null`. Load treated the key's
// mere presence as "the operator set a panel", so the child refused to start
// with "review_panel is empty" on the standard [implement, review, fix] shape.
// Only a JSON ARRAY counts as set now; `null` reads exactly like an absent key.
func TestReviewPanel_NullIsAbsent(t *testing.T) {
	cfg := writePanelConfig(t, `{
		"harness": "claude",
		"phases": [{"name": "implement"}, {"name": "review"}, {"name": "fix"}],
		"review_panel": null
	}`)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("an explicit null panel must read as absent and validate: %v", err)
	}
	if got := cfg.EffectivePanel(); len(got) != 0 {
		t.Fatalf("null panel resolved to %d members, want none", len(got))
	}
	if got := cfg.EffectivePhases(); len(got) != 3 {
		t.Fatalf("effective phases = %d, want 3 — null must not expand anything", len(got))
	}
}

// TestReviewPanel_MarshalledConfigRoundTrips is the supervised-daemon shape
// (CLA-639 fix pass 3): materializeConfig marshals the validated config into the
// child's own file, and the child Loads and Validates it again. A config with no
// panel must round-trip cleanly (no `review_panel` key at all), and a config
// with a panel must keep its members byte-for-byte through the same trip.
func TestReviewPanel_MarshalledConfigRoundTrips(t *testing.T) {
	writeMarshalled := func(t *testing.T, data []byte) *Config {
		t.Helper()
		p := filepath.Join(t.TempDir(), "materialized.json")
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		cfg, err := Load(p)
		if err != nil {
			t.Fatalf("Load(materialized): %v", err)
		}
		return cfg
	}

	t.Run("no panel", func(t *testing.T) {
		cfg := writePanelConfig(t, `{
			"harness": "claude",
			"phases": [{"name": "implement"}, {"name": "review"}, {"name": "fix"}]
		}`)
		if err := cfg.Validate(); err != nil {
			t.Fatalf("source config does not validate: %v", err)
		}
		data, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if bytes.Contains(data, []byte(`"review_panel"`)) {
			t.Errorf("the marshalled config of a panel-less run names review_panel: %s", data)
		}
		loaded := writeMarshalled(t, data)
		if err := loaded.Validate(); err != nil {
			t.Fatalf("a materialized config with no panel must validate: %v", err)
		}
		if got := loaded.EffectivePhases(); len(got) != 3 {
			t.Fatalf("materialized effective phases = %d, want 3", len(got))
		}
	})

	t.Run("a panel keeps its members", func(t *testing.T) {
		cfg := writePanelConfig(t, `{
			"harness": "claude",
			"harnesses": {"opencode": {"model": "opencode-go/gpt-6-luna"}},
			"phases": [{"name": "implement"}, {"name": "review"}, {"name": "fix"}],
			"review_panel": [
				{"name": "alpha", "harness": "opencode"},
				{"name": "beta", "harness": "opencode", "model": "opencode-go/glm-5.3-flash"}
			]
		}`)
		if err := cfg.Validate(); err != nil {
			t.Fatalf("source config does not validate: %v", err)
		}
		data, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		loaded := writeMarshalled(t, data)
		if err := loaded.Validate(); err != nil {
			t.Fatalf("a materialized panel config must validate: %v", err)
		}
		panel := loaded.EffectivePanel()
		if len(panel) != 2 {
			t.Fatalf("materialized panel = %d members, want 2", len(panel))
		}
		if panel[0].Name != "alpha" || panel[0].Harness != "opencode" {
			t.Errorf("member 0 = %+v, want alpha on opencode", panel[0])
		}
		if panel[1].Name != "beta" || panel[1].Model.First() != "opencode-go/glm-5.3-flash" {
			t.Errorf("member 1 = %+v, want beta with its pinned model", panel[1])
		}
		got := loaded.EffectivePhases()
		if len(got) != 4 || got[1].Name != "alpha" || got[2].Name != "beta" {
			t.Fatalf("materialized effective phases = %v, want implement, alpha, beta, fix", phaseNames(got))
		}
	})
}

// TestReviewPanel_FinalReviewNeverExpands (CLA-639 fix pass 3): the position
// check lives in the expansion itself, so EffectivePhases can never multiply a
// FINAL review even when a caller skips Validate. Validate still refuses a
// sequence with more than one built-in review phase beside a panel: a panel
// expands exactly one, and which one it picked would be trivia.
func TestReviewPanel_FinalReviewNeverExpands(t *testing.T) {
	cfg := writePanelConfig(t, `{
		"harness": "claude",
		"phases": [{"name": "implement"}, {"name": "review"}, {"name": "fix", "prompt": "custom fix"}, {"name": "review"}],
		"review_panel": [{"model": "a"}, {"model": "b"}]
	}`)
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "ONE built-in") {
		t.Fatalf("Validate() = %v, want a refusal naming the single expandable built-in review", err)
	}
	// Called WITHOUT Validate: the expansion itself must not multiply the final
	// review even though the first one still expands.
	got := cfg.EffectivePhases()
	if len(got) != 5 {
		t.Fatalf("effective phases = %v, want 5 (implement, 2 members, fix, the final review)", phaseNames(got))
	}
	last := got[len(got)-1]
	if last.IsPanelMember() {
		t.Fatalf("the FINAL review expanded into a panel member: %+v", last)
	}
	if last.Name != ReviewPhaseName || last.Prompt != builtinPhasePrompts[ReviewPhaseName] {
		t.Errorf("the final review = %q with prompt %.60q, want the historical combined brief", last.Name, last.Prompt)
	}
	members := 0
	for i, ph := range got {
		if ph.IsPanelMember() {
			members++
			if i == len(got)-1 {
				t.Errorf("a panel member sits at the last position")
			}
		}
	}
	if members != 2 {
		t.Errorf("panel members = %d, want 2 (only the non-final review expands)", members)
	}
}

// TestReviewPanel_MemberNamesAreLogLabels (CLA-639 fix pass 3): names are
// validated like phase names (they become iteration-log filenames) and are
// unique; the reviewer ROLE stays positional, so naming a member never changes
// which durable record it writes.
func TestReviewPanel_MemberNamesAreLogLabels(t *testing.T) {
	base := func(panel string) string {
		return `{
			"harness": "claude",
			"phases": [{"name": "implement"}, {"name": "review"}, {"name": "fix"}],
			"review_panel": ` + panel + `
		}`
	}
	t.Run("a bad name is refused", func(t *testing.T) {
		cfg := writePanelConfig(t, base(`[{"name": "Alpha"}]`))
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "lowercase letters, digits and hyphens") {
			t.Fatalf("Validate() = %v, want the phase-name format refusal", err)
		}
	})
	t.Run("a duplicate name is refused", func(t *testing.T) {
		cfg := writePanelConfig(t, base(`[{"name": "alpha"}, {"name": "alpha"}]`))
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "duplicate member name") {
			t.Fatalf("Validate() = %v, want the duplicate-name refusal", err)
		}
	})
	t.Run("a review-N name from another position is refused", func(t *testing.T) {
		// This also covers the derived/explicit collision the labels map used to
		// catch last: `review-2` written on member 0 is refused here with a
		// message that says which family owns it, before the map ever sees it.
		cfg := writePanelConfig(t, base(`[{"name": "review-2"}, {}]`))
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "reserved review-N family") {
			t.Fatalf("Validate() = %v, want member 0's review-2 refused", err)
		}
	})
	t.Run("a member named after a built-in phase is refused", func(t *testing.T) {
		cfg := writePanelConfig(t, base(`[{"name": "review"}]`))
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "built-in phase name") {
			t.Fatalf("Validate() = %v, want the built-in-phase name refused", err)
		}
	})
	t.Run("a member named after a configured phase is refused", func(t *testing.T) {
		cfg := writePanelConfig(t, `{
			"harness": "claude",
			"phases": [
				{"name": "implement"},
				{"name": "audit", "prompt": "my own gate"},
				{"name": "review"},
				{"name": "fix"}
			],
			"review_panel": [{"name": "audit"}]
		}`)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "already the name of phases[1]") {
			t.Fatalf("Validate() = %v, want a configured phase name refused", err)
		}
	})
	t.Run("a review-N name is refused on any non-owning member", func(t *testing.T) {
		cfg := writePanelConfig(t, base(`[{"name": "review-2"}, {"name": "alpha"}]`))
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "reserved review-N family") {
			t.Fatalf("Validate() = %v, want member 0's review-2 refused", err)
		}
	})
	t.Run("a review-N name beyond the panel names no member but is refused", func(t *testing.T) {
		// A single member named review-5: there is no member 5 to own it, so the
		// message must speak about the family and this member's own label.
		cfg := writePanelConfig(t, base(`[{"name": "review-5"}]`))
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "reserved review-N family") {
			t.Fatalf("Validate() = %v, want review-5 refused as a family name", err)
		}
		if !strings.Contains(err.Error(), `"review-1"`) {
			t.Errorf("the refusal does not name this member's derived label: %v", err)
		}
	})
	t.Run("a member may name its own derived label", func(t *testing.T) {
		cfg := writePanelConfig(t, base(`[{"name": "review-1"}, {}]`))
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate() = %v, want member 0's own review-1 accepted", err)
		}
		got := cfg.EffectivePhases()
		if got[1].Name != "review-1" || got[2].Name != "review-2" {
			t.Fatalf("member labels = %q, %q, want review-1, review-2", got[1].Name, got[2].Name)
		}
	})
	t.Run("a derived label collision says it is derived", func(t *testing.T) {
		cfg := writePanelConfig(t, `{
			"harness": "claude",
			"phases": [
				{"name": "implement"},
				{"name": "review-1", "prompt": "my own gate"},
				{"name": "review"},
				{"name": "fix"}
			],
			"review_panel": [{}, {}]
		}`)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "derived label") {
			t.Fatalf("Validate() = %v, want the derived-label collision named as such", err)
		}
		if !strings.Contains(err.Error(), "review_panel[0]'s derived label") {
			t.Errorf("the derived-label refusal mixes 0- and 1-based numbering: %v", err)
		}
		if !strings.Contains(err.Error(), "set `name`") {
			t.Errorf("the derived-label refusal does not tell the operator to set `name`: %v", err)
		}
	})
	t.Run("alpha and beta are accepted and keep their labels", func(t *testing.T) {
		cfg := writePanelConfig(t, base(`[{"name": "alpha"}, {"name": "beta"}]`))
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate() = %v, want named members accepted", err)
		}
		got := cfg.EffectivePhases()
		if got[1].Name != "alpha" || got[2].Name != "beta" {
			t.Fatalf("member labels = %q, %q, want alpha, beta", got[1].Name, got[2].Name)
		}
	})
}

// TestReviewPanel_CodexMemberIsRefused: a member runs one review phase of the
// sequence, so it is held to the same claim-tracking rule a phase is. Codex
// does not observe the session's task claim, so a codex member is refused at
// validation rather than discovered as an ended sequence at 3am.
func TestReviewPanel_CodexMemberIsRefused(t *testing.T) {
	cfg := writePanelConfig(t, `{
		"harness": "claude",
		"harnesses": {"codex": {"model": "gpt-5.1"}},
		"phases": [{"name": "implement"}, {"name": "review"}, {"name": "fix"}],
		"review_panel": [{"harness": "codex"}]
	}`)
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "does not observe the session's task claim") {
		t.Fatalf("Validate() = %v, want the claim-tracking refusal for a codex member", err)
	}
}

// TestReviewPanel_HarnessesMatchTheLegacyAnswer (CLA-639 fix pass 3): with no
// panel, SpawnedHarnesses and PhaseHarnesses answer exactly as they did before
// this feature — the raw `phases`, so a verify phase that names no harness
// resolves to the run-wide one, not to the fix phase's. With a panel, the
// members' resolved harnesses are added, because those are sessions the run
// really starts.
func TestReviewPanel_HarnessesMatchTheLegacyAnswer(t *testing.T) {
	t.Run("no panel: the pre-CLA-639 answer", func(t *testing.T) {
		cfg := writePanelConfig(t, `{
			"harness": "claude",
			"harnesses": {"opencode": {"model": "opencode-go/gpt-6-luna"}},
			"phases": [
				{"name": "implement", "harness": "opencode"},
				{"name": "review", "harness": "opencode"},
				{"name": "fix", "harness": "opencode"},
				{"name": "verify"}
			]
		}`)
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate() = %v", err)
		}
		if got := strings.Join(cfg.SpawnedHarnesses(), ","); got != "opencode,claude" {
			t.Errorf("SpawnedHarnesses() = %q, want the pre-panel answer opencode,claude (the verify phase is on the run-wide harness)", got)
		}
		if got := strings.Join(cfg.PhaseHarnesses(), ","); got != "claude,opencode" {
			t.Errorf("PhaseHarnesses() = %q, want claude,opencode (run-wide first)", got)
		}
	})
	t.Run("a panel adds its members", func(t *testing.T) {
		cfg := writePanelConfig(t, `{
			"harness": "opencode",
			"harnesses": {"claude": {"model": "claude-opus-5"}},
			"phases": [{"name": "implement"}, {"name": "review"}, {"name": "fix"}],
			"review_panel": [{"harness": "claude"}]
		}`)
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate() = %v", err)
		}
		if got := strings.Join(cfg.SpawnedHarnesses(), ","); got != "opencode,claude" {
			t.Errorf("SpawnedHarnesses() = %q, want opencode,claude — the member runs on claude", got)
		}
		if got := strings.Join(cfg.PhaseHarnesses(), ","); got != "opencode,claude" {
			t.Errorf("PhaseHarnesses() = %q, want opencode,claude", got)
		}
	})
}

// TestReviewPanel_PhaseModelIsAConfigKey (CLA-598, superseding the CLA-639
// rule): Phase.Model used to be internal plumbing for member expansion, never a
// `phases[].model` key. Operator decision 3eddb7a5 made it user-settable, on
// both the local file and the stored document, so a `model` on a phase now
// loads, resolves, and marshals back out — alongside a MEMBER's model, which
// was always user-facing JSON.
func TestReviewPanel_PhaseModelIsAConfigKey(t *testing.T) {
	cfg := writePanelConfig(t, `{
		"harness": "claude",
		"phases": [{"name": "implement", "model": "opencode-go/gpt-6-luna"}, {"name": "review"}, {"name": "fix"}],
		"review_panel": [{"model": "opencode-go/gpt-6-luna"}]
	}`)
	if got := cfg.Phases[0].Model.First(); got != "opencode-go/gpt-6-luna" {
		t.Fatalf("phases[0].model loaded as %q, want the configured alias", got)
	}
	if got, ok := cfg.ModelForPhase(cfg.Phases[0]); got != "opencode-go/gpt-6-luna" || !ok {
		t.Errorf("ModelForPhase(phases[0]) = %q/%v, want the configured alias", got, ok)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc struct {
		Phases      []map[string]json.RawMessage `json:"phases"`
		ReviewPanel []map[string]json.RawMessage `json:"review_panel"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc.Phases) != 3 || len(doc.Phases[0]["model"]) == 0 {
		t.Errorf("the phase's model did not survive marshalling: %s", data)
	}
	// An unset model stays absent (`omitempty`), so a config that never named
	// one keeps the marshalled form — and therefore Identity() — it always had.
	if _, ok := doc.Phases[1]["model"]; ok {
		t.Errorf("phases[1] marshalled an empty `model` key, moving every identity: %s", data)
	}
	if len(doc.ReviewPanel) != 1 || len(doc.ReviewPanel[0]["model"]) == 0 {
		t.Errorf("the member's model did not survive marshalling: %s", data)
	}
}

// TestReviewPanel_AbsentKeepsTheConfigIdentity (CLA-639 fix pass 3, still held
// by CLA-598): with no panel and no phase model, Identity() must hash exactly
// what it hashed before either key existed. The golden below was computed at
// 5805722 with this exact literal config; the phase `model` still stays out of
// the marshalled form when unset, because it is tagged `omitempty` — which is
// what keeps an operator who names no models off a config-identity change.
func TestReviewPanel_AbsentKeepsTheConfigIdentity(t *testing.T) {
	cfg := &Config{Harness: "claude", Phases: []Phase{
		{Name: "implement"}, {Name: "review"}, {Name: "fix"},
	}}
	const prePanelIdentity = "e83c686b2bfc9c8c60e23d1946116ca7ca5c71da23c7fa50be60a16452cdd7f8"
	if got := cfg.Identity(); got != prePanelIdentity {
		t.Errorf("Identity() = %s, want the pre-panel identity %s", got, prePanelIdentity)
	}
}

func phaseNames(phases []Phase) []string {
	out := make([]string, len(phases))
	for i, ph := range phases {
		out[i] = ph.Label(i)
	}
	return out
}

// TestReviewPanel_MemberTierBeatsTheReviewPhasesModel: a member that names a
// TIER must run on that tier, not on the model inherited from the configured
// review phase. ModelForPhase resolves Model before Tier, so the tier branch of
// panelMember has to clear the inherited model — otherwise the member's explicit
// choice is silently inert (CLA-598 fix round 2, 2.review-1.2/3.1/2.1/4.1/5.1).
func TestReviewPanel_MemberTierBeatsTheReviewPhasesModel(t *testing.T) {
	cfg := writePanelConfig(t, `{
		"harness": "opencode",
		"models": {"strong": "opencode-go/glm-5.3-flash"},
		"phases": [
			{"name": "implement"},
			{"name": "review", "model": "opencode-go/gpt-6-luna"},
			{"name": "fix"}
		],
		"review_panel": [
			{"tier": "strong"},
			{"model": "opencode-go/gpt-6-luna"}
		]
	}`)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	got := cfg.EffectivePhases()
	if len(got) != 4 {
		t.Fatalf("effective phases = %d, want 4 (implement, two reviewers, fix)", len(got))
	}
	tiered, modeld := got[1], got[2]
	if tiered.Tier != "strong" || !tiered.Model.Empty() {
		t.Errorf("the tier member expanded to model=%q tier=%q, want tier strong with NO inherited model", tiered.Model, tiered.Tier)
	}
	if model, ok := cfg.ModelForPhase(tiered); !ok || model != "opencode-go/glm-5.3-flash" {
		t.Errorf("the tier member resolves to %q/%v, want opencode-go/glm-5.3-flash — the tier it named", model, ok)
	}
	if modeld.Model.First() != "opencode-go/gpt-6-luna" || modeld.Tier != "" {
		t.Errorf("the model member expanded to model=%q tier=%q, want its own alias and no tier", modeld.Model, modeld.Tier)
	}
}

// TestReviewPanel_OptionalParsesDefaultsFalseAndExpands (CLA-784): `optional`
// is a per-member key, absent by default, and expansion carries it onto the
// phase the driver runs — that phase flag is what tells a trial seat from a
// required one without re-reading the panel by position at end time.
func TestReviewPanel_OptionalParsesDefaultsFalseAndExpands(t *testing.T) {
	cfg := writePanelConfig(t, `{
		"harness": "opencode",
		"phases": [{"name": "implement"}, {"name": "review"}, {"name": "fix"}],
		"review_panel": [{"model": "a"}, {"model": "b", "optional": true}]
	}`)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want a panel with a required and an optional member accepted", err)
	}
	if cfg.ReviewPanel[0].Optional {
		t.Error("a member with no `optional` key parsed as optional, want required (the default)")
	}
	if !cfg.ReviewPanel[1].Optional {
		t.Error("`optional: true` did not parse")
	}
	phases := cfg.EffectivePhases()
	if len(phases) != 4 {
		t.Fatalf("effective phases = %d, want 4 (implement, two reviewers, fix)", len(phases))
	}
	if phases[1].IsOptionalPanelMember() {
		t.Error("the required member's expanded phase is marked optional")
	}
	if !phases[2].IsOptionalPanelMember() {
		t.Error("the optional member's expanded phase is not marked optional — the driver cannot tell a trial seat from a required one")
	}
	for _, idx := range []int{0, 3} {
		if phases[idx].IsOptionalPanelMember() {
			t.Errorf("phase %d is not a panel member but reports optional", idx)
		}
	}
}

// TestReviewPanel_OptionalKeepsTheConfigIdentity (CLA-784): `omitempty` is
// load-bearing exactly as it is for the phase model — Config.Identity() hashes
// the marshalled form, so a config that never writes `optional` must hash what
// it hashed before the key existed. A member that sets it moves the identity,
// correctly: a trial seat is a deliberate change to the panel.
func TestReviewPanel_OptionalKeepsTheConfigIdentity(t *testing.T) {
	const body = `{
		"harness": "opencode",
		"phases": [{"name": "implement"}, {"name": "review"}, {"name": "fix"}],
		"review_panel": [%s]
	}`
	plain := writePanelConfig(t, strings.Replace(body, "%s", `{"model": "a"}`, 1))
	explicitFalse := writePanelConfig(t, strings.Replace(body, "%s", `{"model": "a", "optional": false}`, 1))
	withOptional := writePanelConfig(t, strings.Replace(body, "%s", `{"model": "a"}, {"model": "b", "optional": true}`, 1))

	// Computed from this exact literal at 5fc5ee1, BEFORE the field existed. A
	// config that never writes `optional` must hash exactly what it hashed then;
	// otherwise every existing panel's identity moves on upgrade.
	const preOptionalPanelIdentity = "5714a6038b8f7c8f71591da2e3f88f326fc3ddce8cbb9797b31f0e8fe128087f"
	if got := plain.Identity(); got != preOptionalPanelIdentity {
		t.Errorf("Identity() = %s, want the pre-optional identity %s", got, preOptionalPanelIdentity)
	}
	if got, want := plain.Identity(), explicitFalse.Identity(); got != want {
		t.Errorf("an explicit `optional: false` changed the config identity: %s != %s — omitempty must keep the default out of the marshalled form", got, want)
	}
	if plain.Identity() == withOptional.Identity() {
		t.Error("setting `optional: true` did not change the config identity — a trial seat is part of the panel")
	}
	for name, cfg := range map[string]*Config{"plain": plain, "explicit false": explicitFalse} {
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		if strings.Contains(string(data), `"optional"`) {
			t.Errorf("%s: the marshal carries an `optional` key with no true value, moving every existing panel's identity: %s", name, data)
		}
	}
	data, err := json.Marshal(withOptional)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"optional":true`) {
		t.Errorf("the configured trial seat did not marshal: %s", data)
	}
}

// TestReviewPanel_ValidateRefusesAnAllOptionalPanel (CLA-784): an optional seat
// is a trial reviewer whose failure never blocks the fix. With no required
// member there is no review a round could be said to have produced, so every
// task would run the members and spawn no fix — refused at the door.
func TestReviewPanel_ValidateRefusesAnAllOptionalPanel(t *testing.T) {
	cfg := writePanelConfig(t, `{
		"harness": "opencode",
		"phases": [{"name": "implement"}, {"name": "review"}, {"name": "fix"}],
		"review_panel": [{"optional": true}, {"optional": true}]
	}`)
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "ALL are optional") {
		t.Fatalf("Validate() = %v, want an all-optional refusal", err)
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("the refusal does not name the remedy (leave a member required): %v", err)
	}

	// One required member is enough — the shape every real panel has.
	ok := writePanelConfig(t, `{
		"harness": "opencode",
		"phases": [{"name": "implement"}, {"name": "review"}, {"name": "fix"}],
		"review_panel": [{"model": "a"}, {"optional": true}]
	}`)
	if err := ok.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want a panel with one required member accepted", err)
	}
}
