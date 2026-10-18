package cli

import (
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// CLA-639: doctor reports the review panel, by member and model. The trial panel
// runs on every task by default, so an operator who never wrote `review_panel`
// still has to be able to see where four review sessions will spend before an
// overnight run — the same reason the phase harnesses are printed.
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

	// A config with no built-in review phase has no panel to report: the line is
	// absent, not an empty one.
	single := validCfg(t) // unphased
	for _, l := range checkConfig(single).info {
		if strings.HasPrefix(l, "review panel: ") {
			t.Errorf("an unphased config reported a review panel: %s", l)
		}
	}
}
