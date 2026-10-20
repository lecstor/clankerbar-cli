package config

// The inherited-env allowlist (CLA-714): a config that says nothing keeps full
// inheritance, and one that declares a list replaces it wholly at the most
// specific level that speaks. Validation is per level and names where it found
// the problem, exactly as the env maps' validation does (CLA-462).

import (
	"strings"
	"testing"
)

func TestEnvAllowlistDefaultIsFullInheritance(t *testing.T) {
	c := defaults()
	if got := c.SessionEnvAllowlist("claude", ""); got != nil {
		t.Fatalf("SessionEnvAllowlist = %v, want nil (inherit everything) on a config that declares nothing", got)
	}
	if c.EnvAllowlistDeclared() {
		t.Fatal("EnvAllowlistDeclared = true with no level declaring one")
	}
}

// The composition rule is REPLACEMENT, not a union: the most specific level
// that declares a list is the whole answer. A union would make it impossible to
// keep a broadly-allowed variable out of one project's sessions.
func TestSessionEnvAllowlistMostSpecificLevelWins(t *testing.T) {
	top := []string{"HOME", "PATH"}
	perHarness := []string{"HOME", "PATH", "OPENCODE_CONFIG"}
	perProject := []string{"HOME", "PATH", "GH_TOKEN"}
	perPair := []string{"HOME", "PATH", "GH_TOKEN", "CLANKERBAR_API_KEY"}

	t.Run("top level alone", func(t *testing.T) {
		c := &Config{EnvAllowlist: top}
		if got := c.SessionEnvAllowlist("claude", ""); strings.Join(got, ",") != strings.Join(top, ",") {
			t.Fatalf("got %v, want the top-level list %v", got, top)
		}
	})

	t.Run("harness replaces the top level", func(t *testing.T) {
		c := &Config{EnvAllowlist: top, Harnesses: map[string]HarnessConfig{
			"opencode": {EnvAllowlist: perHarness},
		}}
		if got := c.SessionEnvAllowlist("opencode", ""); strings.Join(got, ",") != strings.Join(perHarness, ",") {
			t.Fatalf("got %v, want the harness list %v", got, perHarness)
		}
		// A harness that declares none keeps the top-level answer.
		if got := c.SessionEnvAllowlist("claude", ""); strings.Join(got, ",") != strings.Join(top, ",") {
			t.Fatalf("claude got %v, want the top-level list %v", got, top)
		}
	})

	t.Run("project replaces the harness and top level", func(t *testing.T) {
		c := &Config{
			EnvAllowlist: top,
			Harnesses:    map[string]HarnessConfig{"opencode": {EnvAllowlist: perHarness}},
			Projects:     []Project{{Slug: "acme", EnvAllowlist: perProject}},
		}
		if got := c.SessionEnvAllowlist("opencode", "acme"); strings.Join(got, ",") != strings.Join(perProject, ",") {
			t.Fatalf("got %v, want the project list %v", got, perProject)
		}
		// A different project keeps the harness answer.
		if got := c.SessionEnvAllowlist("opencode", "other"); strings.Join(got, ",") != strings.Join(perHarness, ",") {
			t.Fatalf("other project got %v, want the harness list %v", got, perHarness)
		}
	})

	t.Run("project per harness replaces everything", func(t *testing.T) {
		c := &Config{
			EnvAllowlist: top,
			Harnesses:    map[string]HarnessConfig{"opencode": {EnvAllowlist: perHarness}},
			Projects: []Project{{
				Slug:                   "acme",
				EnvAllowlist:           perProject,
				EnvAllowlistPerHarness: map[string][]string{"opencode": perPair},
			}},
		}
		if got := c.SessionEnvAllowlist("opencode", "acme"); strings.Join(got, ",") != strings.Join(perPair, ",") {
			t.Fatalf("got %v, want the project-per-harness list %v", got, perPair)
		}
		// Another harness on the same project keeps the project list.
		if got := c.SessionEnvAllowlist("claude", "acme"); strings.Join(got, ",") != strings.Join(perProject, ",") {
			t.Fatalf("claude on acme got %v, want the project list %v", got, perProject)
		}
	})
}

// Validation walks all four levels and labels the one that is wrong, the same
// shape the env-map validation uses.
func TestValidateEnvAllowlistWalksEveryLevel(t *testing.T) {
	mk := func(mutate func(c *Config)) *Config {
		c := defaults()
		c.WorkDir = t.TempDir()
		mutate(c)
		return c
	}
	cases := []struct {
		name    string
		build   func(*Config)
		wantErr string
	}{
		{"top level", func(c *Config) { c.EnvAllowlist = []string{"BAD NAME"} }, `env_allowlist[0] = "BAD NAME"`},
		{"harness block", func(c *Config) {
			c.Harnesses = map[string]HarnessConfig{"opencode": {EnvAllowlist: []string{"A=B"}}}
		}, `harnesses.opencode.env_allowlist[0]`},
		{"project block", func(c *Config) {
			c.Projects = []Project{{Slug: "p", WorkDir: t.TempDir(), EnvAllowlist: []string{""}}}
		}, `projects[0].env_allowlist[0]`},
		{"project per harness", func(c *Config) {
			c.Projects = []Project{{Slug: "p", WorkDir: t.TempDir(),
				EnvAllowlistPerHarness: map[string][]string{"claude": {"HOME", "HOME"}}}}
		}, `projects[0].env_allowlist_per_harness.claude names HOME twice`},
		{"explicitly empty", func(c *Config) {
			c.EnvAllowlist = []string{}
		}, `env_allowlist is empty`},
		{"surrounding whitespace", func(c *Config) {
			c.EnvAllowlist = []string{" HOME"}
		}, `has surrounding whitespace`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := mk(tc.build).Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// A valid list passes, at every level, and a harness block that declares ONLY
// an allowlist is not the empty block Validate refuses.
func TestValidateEnvAllowlistAcceptsAValidList(t *testing.T) {
	c := defaults()
	c.WorkDir = t.TempDir()
	c.EnvAllowlist = []string{"HOME", "PATH", "CLANKERBAR_API_KEY"}
	c.Harnesses = map[string]HarnessConfig{"opencode": {EnvAllowlist: []string{"HOME", "PATH"}}}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if (HarnessConfig{EnvAllowlist: []string{"HOME"}}).Empty() {
		t.Fatal("a harness block declaring only an allowlist reads as empty")
	}
}

// doctor prints the DECLARED names, never values, and the env block's names are
// kept distinct from the inherited allowlist.
func TestSessionEnvNamesListsDeclaredNamesOnly(t *testing.T) {
	c := &Config{
		Env: EnvMap{"GH_TOKEN": {Literal: "secret-value"}},
		Harnesses: map[string]HarnessConfig{
			"opencode": {Env: EnvMap{"OPENCODE_TOKEN": {Literal: "another-secret"}}},
		},
		Projects: []Project{{Slug: "acme", Env: EnvMap{"PROJECT_TOKEN": {Literal: "x"}}}},
	}
	got := c.SessionEnvNames("opencode", "acme")
	want := []string{"GH_TOKEN", "OPENCODE_TOKEN", "PROJECT_TOKEN"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("SessionEnvNames = %v, want %v", got, want)
	}
	for _, name := range got {
		if strings.Contains(name, "secret") {
			t.Fatalf("SessionEnvNames leaked a value: %q", name)
		}
	}
}
