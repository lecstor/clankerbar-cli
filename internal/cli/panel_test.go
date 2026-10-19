package cli

import (
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// CLA-639: doctor reports the review panel when a config SETS one, by member and
// model, and reports the single reviewer when it does not. The panel is opt-in
// (decision a0b1373b: no default, no preset, no model names), so an operator's
// preflight has to answer "who reviews this run" in both shapes — the same
// reason the phase harnesses are printed.
func TestDoctor_ReportsTheReviewPanel(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := &config.Config{
		Harness: "opencode",
		WorkDir: t.TempDir(),
		Phases: []config.Phase{
			{Name: "implement"},
			{Name: "review"},
			{Name: "fix"},
		},
		ReviewPanel: []config.ReviewPanelMember{
			{Model: "opencode-go/gpt-6-luna"},
			{Model: "opencode-go/gpt-6-luna"},
			{Model: "opencode-go/gpt-6-luna"},
			{Model: "opencode-go/glm-5.3-flash"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("fixture does not validate: %v", err)
	}

	line := ""
	for _, l := range checkConfig(cfg).info {
		if strings.HasPrefix(l, "review panel: ") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("doctor does not report the review panel; info = %v", checkConfig(cfg).info)
	}
	for _, want := range []string{
		"review-1 -> opencode/opencode-go/gpt-6-luna",
		"review-2 -> opencode/opencode-go/gpt-6-luna",
		"review-3 -> opencode/opencode-go/gpt-6-luna",
		"review-4 -> opencode/opencode-go/glm-5.3-flash",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the panel line does not carry %q: %s", want, line)
		}
	}
}

// A member that INHERITS its harness is reported with the harness it will
// actually run on (CLA-639 N1): the line names where the session runs, not the
// empty field on the member.
func TestDoctor_PanelMemberHarnessIsTheResolvedOne(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := &config.Config{
		Harness: "opencode",
		WorkDir: t.TempDir(),
		Phases: []config.Phase{
			{Name: "implement"},
			{Name: "review"},
			{Name: "fix"},
		},
		ReviewPanel: []config.ReviewPanelMember{{Model: "opencode-go/gpt-6-luna"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("fixture does not validate: %v", err)
	}
	line := ""
	for _, l := range checkConfig(cfg).info {
		if strings.HasPrefix(l, "review panel: ") {
			line = l
		}
	}
	if !strings.Contains(line, "review-1 -> opencode/opencode-go/gpt-6-luna") {
		t.Errorf("an inheriting member is not reported with the harness it runs on: %s", line)
	}
}

// No panel, a built-in review phase: doctor says so, naming the harness/model
// the one reviewer will run on. This is the shape every pre-CLA-639 config has,
// so it is the line an operator sees most.
func TestDoctor_ReportsTheSingleReviewerWhenNoPanelIsSet(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := &config.Config{
		Harness: "claude",
		WorkDir: t.TempDir(),
		Model:   "claude-opus-5",
		Phases: []config.Phase{
			{Name: "implement"},
			{Name: "review"},
			{Name: "fix"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("fixture does not validate: %v", err)
	}
	info := checkConfig(cfg).info
	line := ""
	for _, l := range info {
		if strings.HasPrefix(l, "review: ") {
			line = l
		}
	}
	if line != "review: single reviewer (claude/claude-opus-5)" {
		t.Fatalf("doctor's single-reviewer line = %q, want the resolved harness/model; info = %v", line, info)
	}
	for _, l := range info {
		if strings.HasPrefix(l, "review panel: ") {
			t.Errorf("a config with no panel reported one: %s", l)
		}
	}
}

// A FINAL review phase runs the historical combined brief, not the read-only
// gate doctor's review line is about — and a custom prompt owns its own
// contract — so neither is reported as a reviewer.
func TestDoctor_NoReviewerLineWithoutAReadOnlyGate(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for name, cfg := range map[string]*config.Config{
		"a final built-in review": {
			Harness: "claude",
			WorkDir: t.TempDir(),
			Phases:  []config.Phase{{Name: "implement"}, {Name: "review"}},
		},
		"a custom review prompt": {
			Harness: "claude",
			WorkDir: t.TempDir(),
			Phases:  []config.Phase{{Name: "implement"}, {Name: "review", Prompt: "my own gate"}, {Name: "fix"}},
		},
	} {
		if err := cfg.Validate(); err != nil {
			t.Fatalf("%s: fixture does not validate: %v", name, err)
		}
		for _, l := range checkConfig(cfg).info {
			if strings.HasPrefix(l, "review: ") || strings.HasPrefix(l, "review panel: ") {
				t.Errorf("%s reported a reviewer doctor cannot promise: %s", name, l)
			}
		}
	}
}

// A config with no review phase at all has nothing to report: the line is
// absent, not an empty one.
func TestDoctor_NoReviewLineWithoutAReviewPhase(t *testing.T) {
	single := validCfg(t) // unphased
	for _, l := range checkConfig(single).info {
		if strings.HasPrefix(l, "review: ") || strings.HasPrefix(l, "review panel: ") {
			t.Errorf("an unphased config reported a reviewer: %s", l)
		}
	}
}

// TestDoctor_PanelMemberLabelsAreNotRoles (CLA-639 fix pass 3): a member's
// name is its log label; the reviewer role the durable record is keyed on is
// positional. Doctor prints the role, and the label in parentheses when the two
// differ, so an operator naming a member does not read the name as the record
// key.
func TestDoctor_PanelMemberLabelsAreNotRoles(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := &config.Config{
		Harness: "opencode",
		WorkDir: t.TempDir(),
		Phases: []config.Phase{
			{Name: "implement"},
			{Name: "review"},
			{Name: "fix"},
		},
		ReviewPanel: []config.ReviewPanelMember{
			{Name: "alpha", Model: "opencode-go/gpt-6-luna"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("fixture does not validate: %v", err)
	}
	line := ""
	for _, l := range checkConfig(cfg).info {
		if strings.HasPrefix(l, "review panel: ") {
			line = l
		}
	}
	if !strings.Contains(line, "review-1 (alpha) -> opencode/opencode-go/gpt-6-luna") {
		t.Errorf("a named member is not reported as role (label): %s", line)
	}
}
