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

// CLA-501: the resolved instance identity. The bare-hostname fallback keyed
// every co-located daemon to ONE presence row  -  each beacon overwriting the
// last  -  so the default is now hostname plus config basename. The
// content-hash Identity above was rejected as the discriminator on purpose:
// co-located daemon configs routinely hash identically.

func TestResolveInstanceName(t *testing.T) {
	t.Run("two configs with different basenames resolve to two distinct identities", func(t *testing.T) {
		a := ResolveInstanceName("", "/home/op/fleet/clanker1.json", "Jasons-MBP")
		b := ResolveInstanceName("", "/home/op/fleet/clanker2.json", "Jasons-MBP")
		if a == b {
			t.Fatalf("co-located daemons with distinct config files must not share an identity; both %q", a)
		}
		if a != "Jasons-MBP/clanker1" {
			t.Errorf("a = %q, want %q", a, "Jasons-MBP/clanker1")
		}
	})
	t.Run("identical content under different names still resolves distinctly", func(t *testing.T) {
		// The live four-daemon fleet this fix targets serves IDENTICAL configs
		// from clanker1.json..clanker4.json; only the path tells them apart.
		a := ResolveInstanceName("", "/home/op/fleet/clanker1.json", "h")
		b := ResolveInstanceName("", "/home/op/fleet/clanker4.json", "h")
		if a == b {
			t.Fatal("same content, different basename: identities must differ")
		}
	})
	t.Run("explicit instance_name wins over the default", func(t *testing.T) {
		got := ResolveInstanceName("box-a", "/home/op/fleet/clanker1.json", "Jasons-MBP")
		if got != "box-a" {
			t.Errorf("got %q, want the explicit name verbatim", got)
		}
	})
	t.Run("a whitespace-only name counts as unset", func(t *testing.T) {
		got := ResolveInstanceName("   ", "/home/op/fleet/clanker1.json", "Jasons-MBP")
		if got != "Jasons-MBP/clanker1" {
			t.Errorf("got %q, want the default composition", got)
		}
	})
	t.Run("no config file falls back to the hostname alone", func(t *testing.T) {
		got := ResolveInstanceName("", "", "Jasons-MBP")
		if got != "Jasons-MBP" {
			t.Errorf("got %q, want the bare hostname (nothing better exists)", got)
		}
	})
	t.Run("an unreadable hostname still leaves the basename", func(t *testing.T) {
		got := ResolveInstanceName("", "/home/op/fleet/clanker1.json", "")
		if got != "clanker1" {
			t.Errorf("got %q, want the basename alone (minus the .json suffix)", got)
		}
	})
	t.Run("the composed default is capped at MaxInstanceNameLen", func(t *testing.T) {
		long := strings.Repeat("x", MaxInstanceNameLen) // basename longer than the cap by itself
		got := ResolveInstanceName("", "/fleet/"+long+".json", "Jasons-MBP")
		if len(got) > MaxInstanceNameLen {
			t.Errorf("resolved default is %d chars; the plane refuses anything over %d", len(got), MaxInstanceNameLen)
		}
	})
	t.Run("the method resolves through the config's own source path", func(t *testing.T) {
		cfg := &Config{Harness: "claude", Prompt: "Work."}
		cfg.source = "/home/op/fleet/clanker2.json"
		if got := cfg.ResolvedInstanceName("Jasons-MBP"); got != "Jasons-MBP/clanker2" {
			t.Errorf("ResolvedInstanceName = %q", got)
		}
	})
}
