package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The role branch of `run`: a planner instance must refuse the two local
// preconditions it cannot work around — no key, and no project to poll — before
// it opens a state dir or touches the plane.

func TestRunPlannerRefusesWithoutAKey(t *testing.T) {
	cfg := plannerConfig(t)
	if err := runPlanner(context.Background(), cfg, ""); err == nil || !strings.Contains(err.Error(), "CLANKERBAR_API_KEY") {
		t.Fatalf("runPlanner = %v, want a refusal naming the missing key", err)
	}
}

func TestRunPlannerRefusesWithoutAProject(t *testing.T) {
	cfg := plannerConfig(t)
	cfg.BacklogURL = "https://clankerbar.com"
	cfg.MCPConfigPath = ""
	if err := runPlanner(context.Background(), cfg, "k"); err == nil || !strings.Contains(err.Error(), "no project slug") {
		t.Fatalf("runPlanner = %v, want a refusal naming the missing project", err)
	}
}

// `run` must take the planner branch for role planner rather than building a
// drain driver; the missing key is the cheapest observable proof it did.
func TestRunDispatchesThePlannerRole(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "planner.json")
	body := `{
		"role": "planner",
		"harness": "claude",
		"backlog_url": "https://clankerbar.com/mcp/demo",
		"workdir": "` + dir + `",
		"prompt": "Work the next backlog item."
	}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLANKERBAR_API_KEY", "")
	err := Run(context.Background(), []string{"-c", cfgPath})
	if err == nil || !strings.Contains(err.Error(), "planner needs CLANKERBAR_API_KEY") {
		t.Fatalf("Run = %v, want the planner branch's key refusal", err)
	}
}
