package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// Every harness the registry actually offers must pass config validation. The
// accepted set is derived from harness.Known (not a hand-kept switch), so a newly
// registered adapter can never be rejected here again — the bug this guards: the
// opencode adapter was registered but the old validation switch still rejected it,
// making `clankerbar run --harness opencode` fail before harness.Get was consulted.
func TestValidateHarnessFromRegistry(t *testing.T) {
	names := harness.Names()
	if len(names) == 0 {
		t.Fatal("registry is empty; expected at least the built-in adapters")
	}
	sawOpencode := false
	for _, name := range names {
		if name == "opencode" {
			sawOpencode = true
		}
		c := defaults()
		c.Harness = name
		if err := c.Validate(); err != nil {
			t.Errorf("Validate() rejected registered harness %q: %v", name, err)
		}
	}
	if !sawOpencode {
		t.Error("opencode must be registered and accepted (the CLA-117 adapter has to be reachable)")
	}

	// An unregistered harness is still rejected, and the message lists the
	// registered names so the error is actionable.
	c := defaults()
	c.Harness = "nope-not-a-harness"
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate() accepted an unregistered harness")
	}
	if !strings.Contains(err.Error(), "opencode") {
		t.Errorf("rejection message %q should list the registered harness names", err.Error())
	}
}

func TestResolveEnv(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "token")
	if err := os.WriteFile(secret, []byte("  sk-ant-oat01-abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("nil map yields nil", func(t *testing.T) {
		got, err := resolveEnv(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Fatalf("want nil, got %v", got)
		}
	})

	t.Run("literals and @file, sorted and trimmed", func(t *testing.T) {
		got, err := resolveEnv(map[string]string{
			"ZED":                     "last",
			"CLAUDE_CODE_OAUTH_TOKEN": "@" + secret,
			"ALPHA":                   "first",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{
			"ALPHA=first",
			"CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-abc",
			"ZED=last",
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("want %v, got %v", want, got)
		}
	})

	t.Run("missing @file is an error naming the key", func(t *testing.T) {
		_, err := resolveEnv(map[string]string{"TOK": "@" + filepath.Join(dir, "nope")})
		if err == nil {
			t.Fatal("want error for missing file, got nil")
		}
	})
}

func TestValidatePopulatesEnvSlice(t *testing.T) {
	c := defaults()
	c.Env = map[string]string{"FOO": "bar"}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := c.EnvSlice(); !reflect.DeepEqual(got, []string{"FOO=bar"}) {
		t.Fatalf("EnvSlice = %v", got)
	}
}

func TestBacklogEndpoint(t *testing.T) {
	dir := t.TempDir()
	mcp := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(mcp, []byte(`{"mcpServers":{"clankerbar":{"type":"http","url":"https://clankerbar.com/mcp/proj"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("derives project-scoped URL from .mcp.json when backlog_url is a bare base", func(t *testing.T) {
		c := &Config{BacklogURL: "https://clankerbar.com", MCPConfigPath: mcp}
		if got := c.BacklogEndpoint(); got != "https://clankerbar.com/mcp/proj" {
			t.Errorf("BacklogEndpoint() = %q, want the .mcp.json URL", got)
		}
	})

	t.Run("explicit backlog_url with an /mcp path wins over .mcp.json", func(t *testing.T) {
		c := &Config{BacklogURL: "https://example.com/mcp/other", MCPConfigPath: mcp}
		if got := c.BacklogEndpoint(); got != "https://example.com/mcp/other" {
			t.Errorf("BacklogEndpoint() = %q, want the explicit backlog_url", got)
		}
	})

	t.Run("returns empty when only a bare base and no .mcp.json url are available", func(t *testing.T) {
		// A slug-less base is not a usable endpoint; "" makes New() not-wired so the
		// loop blind-drains instead of retrying an endpoint the plane always rejects.
		c := &Config{BacklogURL: "https://clankerbar.com", MCPConfigPath: ""}
		if got := c.BacklogEndpoint(); got != "" {
			t.Errorf("BacklogEndpoint() = %q, want \"\"", got)
		}
	})
}

// BacklogSummaryURL points at the project-scoped /api/backlog-summary route (counts
// + console pause). It needs only the plane ORIGIN — no project slug in the path —
// so it resolves in more cases than BacklogEndpoint, including a bare base.
func TestBacklogSummaryURL(t *testing.T) {
	dir := t.TempDir()
	mcp := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(mcp, []byte(`{"mcpServers":{"clankerbar":{"type":"http","url":"https://self.example.com/mcp/proj"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("takes the origin of the resolved MCP endpoint (honours .mcp.json / self-host)", func(t *testing.T) {
		c := &Config{BacklogURL: "https://clankerbar.com", MCPConfigPath: mcp}
		if got := c.BacklogSummaryURL(); got != "https://self.example.com/api/backlog-summary" {
			t.Errorf("BacklogSummaryURL() = %q, want the .mcp.json origin", got)
		}
	})

	t.Run("falls back to BacklogURL's own origin when no MCP endpoint resolves", func(t *testing.T) {
		// The key improvement over BacklogEndpoint: a bare base with no .mcp.json is
		// still usable here (the route needs no project slug), so pause/count-gating
		// work rather than dropping to blind mode.
		c := &Config{BacklogURL: "https://clankerbar.com", MCPConfigPath: ""}
		if got := c.BacklogSummaryURL(); got != "https://clankerbar.com/api/backlog-summary" {
			t.Errorf("BacklogSummaryURL() = %q, want the bare-base origin", got)
		}
	})

	t.Run("strips any /mcp path on an explicit backlog_url down to the origin", func(t *testing.T) {
		c := &Config{BacklogURL: "https://example.com/mcp/other", MCPConfigPath: ""}
		if got := c.BacklogSummaryURL(); got != "https://example.com/api/backlog-summary" {
			t.Errorf("BacklogSummaryURL() = %q, want the origin only", got)
		}
	})

	t.Run("an explicit backlog_url wins over a different .mcp.json origin", func(t *testing.T) {
		// Explicit config overrides (README): the operator pointed backlog_url at a
		// self-hosted plane, so the summary poll must hit THAT origin even though
		// .mcp.json names https://self.example.com. Regression for finding #4.
		c := &Config{BacklogURL: "https://plane.internal/mcp/proj", MCPConfigPath: mcp}
		if got := c.BacklogSummaryURL(); got != "https://plane.internal/api/backlog-summary" {
			t.Errorf("BacklogSummaryURL() = %q, want the explicit backlog_url's origin to win", got)
		}
	})

	t.Run("the default base does NOT override a resolved .mcp.json origin", func(t *testing.T) {
		// The flip side: leaving backlog_url at the default must still defer to the
		// .mcp.json origin, so a self-hosted plane wired only through .mcp.json works.
		c := &Config{BacklogURL: defaultBacklogURL, MCPConfigPath: mcp}
		if got := c.BacklogSummaryURL(); got != "https://self.example.com/api/backlog-summary" {
			t.Errorf("BacklogSummaryURL() = %q, want the .mcp.json origin", got)
		}
	})

	t.Run("empty when no origin can be resolved", func(t *testing.T) {
		c := &Config{BacklogURL: "", MCPConfigPath: ""}
		if got := c.BacklogSummaryURL(); got != "" {
			t.Errorf("BacklogSummaryURL() = %q, want \"\"", got)
		}
	})
}
