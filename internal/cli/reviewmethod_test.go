package cli

import (
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// reviewLine returns doctor's reviewer line (single reviewer or panel).
func reviewLine(t *testing.T, cfg *config.Config) string {
	t.Helper()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("fixture does not validate: %v", err)
	}
	for _, l := range checkConfig(cfg).info {
		if strings.HasPrefix(l, "review: ") || strings.HasPrefix(l, "review panel: ") {
			return l
		}
	}
	t.Fatalf("doctor reported no reviewer line; info = %v", checkConfig(cfg).info)
	return ""
}

// TestDoctor_ReportsTheMapsSessionMethod (CLA-869): a single reviewer on the
// maps-session method reports the method and the model each stage will run on,
// with an unset stage resolved to the phase's own model.
func TestDoctor_ReportsTheMapsSessionMethod(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := &config.Config{
		Harness: "opencode",
		WorkDir: t.TempDir(),
		Phases: []config.Phase{
			{Name: "implement"},
			{
				Name:         "review",
				Model:        config.ModelRoutes{"opencode-go/base"},
				ReviewMethod: config.ReviewMethodMapsSession,
				MapsSession:  &config.MapsSessionOptions{NotesModel: "opencode-go/strong"},
			},
			{Name: "fix"},
		},
	}
	line := reviewLine(t, cfg)
	for _, want := range []string{
		"review: single reviewer (opencode/opencode-go/base)",
		"maps-session: notes=opencode-go/strong",
		"findings=opencode-go/base",
		"chunk_size=25",
		"flow_max=40",
		"embed_hunks=off",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("doctor's reviewer line does not carry %q: %s", want, line)
		}
	}

	// A phase naming no model at all with only notes_model set: the findings turn
	// inherits the notes stage in the live session, so doctor must print that —
	// not "harness default", which the record would contradict (round 1
	// 1.review-1.1).
	edge := &config.Config{
		Harness: "opencode",
		WorkDir: t.TempDir(),
		Phases: []config.Phase{
			{Name: "implement"},
			{
				Name:         "review",
				ReviewMethod: config.ReviewMethodMapsSession,
				MapsSession:  &config.MapsSessionOptions{NotesModel: "opencode-go/strong"},
			},
			{Name: "fix"},
		},
	}
	line = reviewLine(t, edge)
	for _, want := range []string{
		"review: single reviewer (opencode/harness default)",
		"notes=opencode-go/strong",
		"findings=opencode-go/strong",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("doctor's notes-only edge line does not carry %q: %s", want, line)
		}
	}
	if strings.Contains(line, "findings=harness default") {
		t.Errorf("doctor printed findings=harness default where the findings turn runs on the notes model: %s", line)
	}
}

// TestDoctor_ReportsEachPanelMembersMethod (CLA-869): the panel line marks the
// maps-session seats and leaves a single-pass seat unmarked, so a mixed panel is
// readable before the run.
func TestDoctor_ReportsEachPanelMembersMethod(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := &config.Config{
		Harness: "opencode",
		WorkDir: t.TempDir(),
		Phases: []config.Phase{
			{Name: "implement"},
			{Name: "review", ReviewMethod: config.ReviewMethodMapsSession},
			{Name: "fix"},
		},
		ReviewPanel: []config.ReviewPanelMember{
			{Model: config.ModelRoutes{"opencode-go/maps"}},
			{Model: config.ModelRoutes{"opencode-go/plain"}, ReviewMethod: config.ReviewMethodSinglePass},
		},
	}
	line := reviewLine(t, cfg)
	if !strings.Contains(line, "review-1 -> opencode/opencode-go/maps (maps-session:") {
		t.Errorf("the maps-session seat is not marked with its method: %s", line)
	}
	if strings.Contains(line, "review-2 -> opencode/opencode-go/plain (maps-session:") {
		t.Errorf("the single-pass seat was marked maps-session: %s", line)
	}
	if !strings.Contains(line, "review-2 -> opencode/opencode-go/plain") {
		t.Errorf("the single-pass seat is missing from the panel line: %s", line)
	}
}

// TestDoctor_ReportsEmbeddedHunkBudgets (CLA-871): a review that will carry its
// hunks in the prompt prints that, with the resolved budgets, so the operator
// can see the prompt it is about to pay for.
func TestDoctor_ReportsEmbeddedHunkBudgets(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := &config.Config{
		Harness: "opencode",
		WorkDir: t.TempDir(),
		Phases: []config.Phase{
			{Name: "implement"},
			{
				Name:         "review",
				ReviewMethod: config.ReviewMethodMapsSession,
				MapsSession: &config.MapsSessionOptions{
					EmbedHunks: &config.EmbedHunksOptions{Enabled: true, MaxBytesPerItem: 2048},
				},
			},
			{Name: "fix"},
		},
	}
	line := reviewLine(t, cfg)
	for _, want := range []string{
		"embed_hunks=on(2048/61440)",
		"notes=harness default",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("doctor's reviewer line does not carry %q: %s", want, line)
		}
	}
}
