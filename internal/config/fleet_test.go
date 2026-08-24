package config

// CLA-466 config surface: instance_name validation, the fleet-report URL
// derivation (slug-ful only - the route has no legacy form), and the config
// identity fingerprint the presence beacon carries.

import (
	"strings"
	"testing"
)

func TestValidateInstanceNameCap(t *testing.T) {
	cfg := func(name string) *Config {
		return &Config{Harness: "claude", Prompt: "Work.", InstanceName: name}
	}
	if err := cfg(strings.Repeat("x", MaxInstanceNameLen)).Validate(); err != nil {
		t.Errorf("a %d-char name must validate: %v", MaxInstanceNameLen, err)
	}
	err := cfg(strings.Repeat("x", MaxInstanceNameLen+1)).Validate()
	if err == nil || !strings.Contains(err.Error(), "instance_name") {
		t.Errorf("an over-long name must be refused with a readable error, got %v", err)
	}
}

func TestFleetReportURLDerivation(t *testing.T) {
	t.Run("explicit mcp path wins", func(t *testing.T) {
		cfg := &Config{Harness: "claude", Prompt: "Work.", BacklogURL: "https://plane.example/mcp/acme"}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("validate: %v", err)
		}
		want := "https://plane.example/api/projects/acme/fleet/report"
		if got := cfg.FleetReportURL(); got != want {
			t.Errorf("FleetReportURL = %q, want %q", got, want)
		}
		p := Project{Slug: "other"}
		if got := cfg.ProjectFleetReportURL(p); got != "https://plane.example/api/projects/other/fleet/report" {
			t.Errorf("ProjectFleetReportURL = %q", got)
		}
	})
	t.Run("no slug means no fleet URL at all", func(t *testing.T) {
		cfg := &Config{Harness: "claude", Prompt: "Work."} // default base, no /mcp/<slug>
		if err := cfg.Validate(); err != nil {
			t.Fatalf("validate: %v", err)
		}
		if got := cfg.FleetReportURL(); got != "" {
			t.Errorf("FleetReportURL = %q, want \"\" — the report route exists only slug-ful; reporting must be off rather than pointed nowhere", got)
		}
		if got := cfg.ProjectFleetReportURL(Project{}); got != "" {
			t.Errorf("ProjectFleetReportURL(slugless) = %q, want \"\"", got)
		}
	})
}

func TestIdentityIsDeterministicAndSensitiveToEdits(t *testing.T) {
	a := &Config{Harness: "claude", Prompt: "Work.", MaxIterations: 3, Models: map[string]string{"strong": "opus"}}
	b := &Config{Harness: "claude", Prompt: "Work.", MaxIterations: 3, Models: map[string]string{"strong": "opus"}}
	if a.Identity() == "" {
		t.Fatal("Identity is empty")
	}
	if a.Identity() != b.Identity() {
		t.Error("identical configs must fingerprint identically (map keys sort in JSON encoding)")
	}
	c := &Config{Harness: "claude", Prompt: "Work.", MaxIterations: 4, Models: map[string]string{"strong": "opus"}}
	if c.Identity() == a.Identity() {
		t.Error("a config edit must change the identity — that is what makes a RELOAD visible on the Fleet page")
	}
	if len(a.Identity()) != 64 {
		t.Errorf("identity = %d chars, want a full sha256 hex", len(a.Identity()))
	}
}
