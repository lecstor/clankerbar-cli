package loop

import (
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// CLA-318: the driver must build an opencode session's MCP path through the
// gate (config.ResolveMCPConfig), so a Claude-shaped .mcp.json never reaches it,
// while the claude phase still gets its file.
func TestInvocation_OpencodeIsNotHandedADotMcpJson(t *testing.T) {
	impl := &fakeAdapter{steps: []invokeStep{{res: checkpointed(10, 0.10)}}}
	review := &fakeAdapter{steps: []invokeStep{{res: okResult(5, 0.05)}}}
	d := mixedDriver(t, impl, review)
	d.cfg.Harnesses["opencode"] = config.HarnessConfig{
		ConfigDir: "/oc/config", MCPConfigPath: "/oc/.mcp.json",
		Model: config.ModelRoutes{"opencode-go/deepseek-v4-flash"},
	}

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if got := impl.invocations[0].MCPConfigPath; got != "" {
		t.Errorf("opencode session handed %q, want \"\" - a .mcp.json is Claude-shaped", got)
	}
	if got := review.invocations[0].MCPConfigPath; got != "/repo/.mcp.json" {
		t.Errorf("claude session handed %q, want /repo/.mcp.json", got)
	}
}
