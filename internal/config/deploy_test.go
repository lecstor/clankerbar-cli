package config

import "testing"

// --- integration_branch resolution -------------------------------------------

func TestIntegrationBranchForDefaultsToStaging(t *testing.T) {
	c := &Config{}
	if got := c.IntegrationBranchFor(""); got != DefaultIntegrationBranch {
		t.Errorf("empty config: IntegrationBranchFor(%q) = %q, want %q", "", got, DefaultIntegrationBranch)
	}
}

func TestIntegrationBranchForTopLevelWinsOverDefault(t *testing.T) {
	c := &Config{IntegrationBranch: "main"}
	if got := c.IntegrationBranchFor(""); got != "main" {
		t.Errorf("got %q, want main", got)
	}
}

func TestIntegrationBranchForProjectOverridesTopLevel(t *testing.T) {
	c := &Config{
		IntegrationBranch: "main",
		Projects: []Project{
			{Slug: "plane", IntegrationBranch: "staging"},
			{Slug: "cli"}, // no own value: falls through to top-level
		},
	}
	if got := c.IntegrationBranchFor("plane"); got != "staging" {
		t.Errorf("project override ignored: got %q, want staging", got)
	}
	if got := c.IntegrationBranchFor("cli"); got != "main" {
		t.Errorf("project without a value should inherit top-level, got %q", got)
	}
	if got := c.IntegrationBranchFor("unlisted"); got != "main" {
		t.Errorf("unmatched slug should inherit top-level, got %q", got)
	}
}

// The branch is standing project configuration, NOT whatever a session
// declared in its delivery claim - that is a per-session fact about one commit.
func TestIntegrationBranchIsNotDerivedFromAnythingElse(t *testing.T) {
	c := &Config{}
	if got := c.IntegrationBranchFor(""); got != DefaultIntegrationBranch {
		t.Errorf("an empty config must resolve to the default, got %q", got)
	}
}

// --- health_url resolution ---------------------------------------------------

func TestHealthURLForFallsBackToTopLevel(t *testing.T) {
	c := &Config{
		HealthURL: "https://top.example/health",
		Projects: []Project{
			{Slug: "own", HealthURL: "https://own.example/health"},
			{Slug: "inherits"},
		},
	}
	if got := c.HealthURLFor("own"); got != "https://own.example/health" {
		t.Errorf("project value should win, got %q", got)
	}
	if got := c.HealthURLFor("inherits"); got != "https://top.example/health" {
		t.Errorf("project without a value should inherit top-level, got %q", got)
	}
	if got := c.HealthURLFor(""); got != "https://top.example/health" {
		t.Errorf("single-project mode uses the top-level field, got %q", got)
	}
}

func TestHealthURLForEmptyStaysEmpty(t *testing.T) {
	c := &Config{}
	if got := c.HealthURLFor(""); got != "" {
		t.Errorf("no derived default exists: got %q, want empty", got)
	}
}

// --- validation ----------------------------------------------------------------

func TestValidateRefusesUnusableHealthURL(t *testing.T) {
	c := &Config{
		Harness:   "claude",
		Prompt:    "Work the backlog.",
		HealthURL: "not a url at all",
	}
	if err := c.Validate(); err == nil {
		t.Error("a health_url with no scheme and host must be refused")
	}

	c.HealthURL = "/relative/only"
	if err := c.Validate(); err == nil {
		t.Error("a relative health_url must be refused")
	}
}

func TestValidateRefusesBadPerProjectHealthURL(t *testing.T) {
	c := &Config{
		Harness:  "claude",
		Prompt:   "Work the backlog.",
		Projects: []Project{{Slug: "acme", HealthURL: "https://ok first"}},
	}
	if err := c.Validate(); err == nil {
		t.Error("a per-project health_url with a space must be refused")
	}
}

// Plain http to an internal plane is LEGAL here, unlike backlog_url: /health is
// read without credentials, so there is no bearer token for the TLS floor to
// protect. Only fetchability is validated.
func TestValidateAllowsPlainHTTPHealthURL(t *testing.T) {
	c := &Config{
		Harness:   "claude",
		Prompt:    "Work the backlog.",
		HealthURL: "http://plane.internal:8080/health",
	}
	if err := c.Validate(); err != nil {
		t.Errorf("credential-free http health endpoint should validate, got %v", err)
	}
}
