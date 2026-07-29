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

// BacklogSummaryURL points at the plane's backlog-summary surface (counts + console
// pause). When a project slug is derivable from the resolved MCP endpoint it returns
// the slug-ful `/api/projects/<slug>/backlog-summary` form (CLA-141), which the
// operator's ACCOUNT key can poll; only when no slug resolves does it fall back to
// the legacy slug-less route, which needs a project-scoped key. Origin precedence is
// unchanged, and it still resolves in more cases than BacklogEndpoint (a bare base
// with no .mcp.json is usable — via the legacy form).
func TestBacklogSummaryURL(t *testing.T) {
	dir := t.TempDir()
	mcp := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(mcp, []byte(`{"mcpServers":{"clankerbar":{"type":"http","url":"https://self.example.com/mcp/proj"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("takes the origin of the resolved MCP endpoint (honours .mcp.json / self-host)", func(t *testing.T) {
		// The .mcp.json names /mcp/proj, so the slug rides into the path too (CLA-141):
		// the account key can poll this URL, no project-scoped key needed.
		c := &Config{BacklogURL: "https://clankerbar.com", MCPConfigPath: mcp}
		if got := c.BacklogSummaryURL(); got != "https://self.example.com/api/projects/proj/backlog-summary" {
			t.Errorf("BacklogSummaryURL() = %q, want the .mcp.json origin + slug-ful path", got)
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

	t.Run("an explicit /mcp/<slug> backlog_url yields that origin AND that slug", func(t *testing.T) {
		// Pre-CLA-141 this stripped down to the origin (the legacy route ignored the
		// slug); now the slug the operator explicitly named selects the slug-ful form.
		c := &Config{BacklogURL: "https://example.com/mcp/other", MCPConfigPath: ""}
		if got := c.BacklogSummaryURL(); got != "https://example.com/api/projects/other/backlog-summary" {
			t.Errorf("BacklogSummaryURL() = %q, want the origin + slug-ful path", got)
		}
	})

	t.Run("an explicit backlog_url wins over a different .mcp.json origin", func(t *testing.T) {
		// Explicit config overrides (README): the operator pointed backlog_url at a
		// self-hosted plane, so the summary poll must hit THAT origin even though
		// .mcp.json names https://self.example.com. Regression for finding #4.
		c := &Config{BacklogURL: "https://plane.internal/mcp/proj", MCPConfigPath: mcp}
		if got := c.BacklogSummaryURL(); got != "https://plane.internal/api/projects/proj/backlog-summary" {
			t.Errorf("BacklogSummaryURL() = %q, want the explicit backlog_url's origin to win", got)
		}
	})

	t.Run("the default base does NOT override a resolved .mcp.json origin", func(t *testing.T) {
		// The flip side: leaving backlog_url at the default must still defer to the
		// .mcp.json origin, so a self-hosted plane wired only through .mcp.json works.
		c := &Config{BacklogURL: defaultBacklogURL, MCPConfigPath: mcp}
		if got := c.BacklogSummaryURL(); got != "https://self.example.com/api/projects/proj/backlog-summary" {
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

// ---------------------------------------------------------------------------
// Multi-project config (CLA-142): the `projects` list and its URL derivation.

func TestProjectsValidation(t *testing.T) {
	base := func() *Config {
		return &Config{Harness: "claude", Prompt: "Work the backlog."}
	}

	t.Run("no projects list — single-project mode validates exactly as before", func(t *testing.T) {
		if err := base().Validate(); err != nil {
			t.Fatalf("Validate() = %v, want nil", err)
		}
	})

	t.Run("a valid projects list normalizes paths and passes", func(t *testing.T) {
		c := base()
		c.Projects = []Project{
			{Slug: "clankerbar", WorkDir: "~/dev"},
			{Slug: "ezyapp", WorkDir: "/repos/ezyapp"},
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() = %v, want nil", err)
		}
		if strings.HasPrefix(c.Projects[0].WorkDir, "~") {
			t.Errorf("workdir %q not home-expanded", c.Projects[0].WorkDir)
		}
	})

	t.Run("a project without a slug is rejected", func(t *testing.T) {
		c := base()
		c.Projects = []Project{{WorkDir: "/repos/x"}}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "slug is required") {
			t.Errorf("Validate() = %v, want a slug-required error", err)
		}
	})

	t.Run("duplicate slugs are rejected", func(t *testing.T) {
		c := base()
		c.Projects = []Project{{Slug: "same"}, {Slug: "same"}}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate slug") {
			t.Errorf("Validate() = %v, want a duplicate-slug error", err)
		}
	})

	t.Run("each project's mcp config defaults to its own workdir's .mcp.json", func(t *testing.T) {
		dir := t.TempDir()
		mcp := filepath.Join(dir, ".mcp.json")
		if err := os.WriteFile(mcp, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		c := base()
		c.Projects = []Project{{Slug: "proj", WorkDir: dir}, {Slug: "bare", WorkDir: t.TempDir()}}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		if got := c.Projects[0].MCPConfigPath; got != mcp {
			t.Errorf("MCPConfigPath = %q, want the workdir's own .mcp.json %q", got, mcp)
		}
		// No .mcp.json in the workdir → left empty (the loop falls back to the
		// top-level mcp_config_path at invocation time).
		if got := c.Projects[1].MCPConfigPath; got != "" {
			t.Errorf("MCPConfigPath = %q, want \"\" when the workdir has no .mcp.json", got)
		}
	})

	t.Run("the top-level mcp_config_path also defaults from the workdir", func(t *testing.T) {
		// Claude's -p mode does not auto-discover .mcp.json; without this default a
		// bare run from a workdir carrying one would spawn sessions with no
		// clankerbar tools and the poller could derive no slug.
		dir := t.TempDir()
		mcp := filepath.Join(dir, ".mcp.json")
		if err := os.WriteFile(mcp, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		c := base()
		c.WorkDir = dir
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		if got := c.MCPConfigPath; got != mcp {
			t.Errorf("MCPConfigPath = %q, want the workdir's .mcp.json %q", got, mcp)
		}
	})
}

func TestProjectSummaryURL(t *testing.T) {
	t.Run("defaults to the public plane origin with the project's slug in the path", func(t *testing.T) {
		c := &Config{BacklogURL: defaultBacklogURL}
		p := Project{Slug: "ezyapp"}
		if got := c.ProjectSummaryURL(p); got != "https://clankerbar.com/api/projects/ezyapp/backlog-summary" {
			t.Errorf("ProjectSummaryURL() = %q", got)
		}
	})

	t.Run("uses the project's own .mcp.json origin (self-hosted plane per project)", func(t *testing.T) {
		dir := t.TempDir()
		mcp := filepath.Join(dir, ".mcp.json")
		if err := os.WriteFile(mcp, []byte(`{"mcpServers":{"clankerbar":{"type":"http","url":"https://plane.internal/mcp/proj"}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		c := &Config{BacklogURL: defaultBacklogURL}
		p := Project{Slug: "proj", MCPConfigPath: mcp}
		if got := c.ProjectSummaryURL(p); got != "https://plane.internal/api/projects/proj/backlog-summary" {
			t.Errorf("ProjectSummaryURL() = %q, want the project's .mcp.json origin", got)
		}
	})

	t.Run("an explicit backlog_url origin wins for every project", func(t *testing.T) {
		c := &Config{BacklogURL: "https://plane.internal"}
		p := Project{Slug: "proj"}
		if got := c.ProjectSummaryURL(p); got != "https://plane.internal/api/projects/proj/backlog-summary" {
			t.Errorf("ProjectSummaryURL() = %q, want the explicit origin", got)
		}
	})

	t.Run("path-escapes a slug so a hostile config cannot smuggle path segments", func(t *testing.T) {
		c := &Config{BacklogURL: defaultBacklogURL}
		p := Project{Slug: "a/b"}
		if got := c.ProjectSummaryURL(p); got != "https://clankerbar.com/api/projects/a%2Fb/backlog-summary" {
			t.Errorf("ProjectSummaryURL() = %q, want the slug path-escaped", got)
		}
	})
}
