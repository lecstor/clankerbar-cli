package loop

// The allowlist reaches the harness invocation from the config, resolved for the
// target's project and the harness the phase spawns on (CLA-714). Without this
// wire the harness filter would exist and always see nil.

import (
	"reflect"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

func TestInvocationCarriesTheSessionEnvAllowlist(t *testing.T) {
	cfg := &config.Config{
		Harness:      "claude",
		Prompt:       "work",
		BacklogURL:   "https://clankerbar.com",
		EnvAllowlist: []string{"HOME", "PATH", "CLANKERBAR_API_KEY"},
		Projects: []config.Project{{
			Slug:         "acme",
			EnvAllowlist: []string{"HOME", "PATH"},
		}},
	}
	d := &Driver{cfg: cfg}

	in, err := d.invocationOn(Target{Name: "acme", WorkDir: t.TempDir()}, "claude", false)
	if err != nil {
		t.Fatalf("invocationOn: %v", err)
	}
	if want := []string{"HOME", "PATH"}; !reflect.DeepEqual(in.EnvAllowlist, want) {
		t.Fatalf("EnvAllowlist = %v, want the project's list %v", in.EnvAllowlist, want)
	}
}

func TestInvocationWithoutAnAllowlistInheritsEverything(t *testing.T) {
	cfg := &config.Config{Harness: "claude", Prompt: "work", BacklogURL: "https://clankerbar.com"}
	d := &Driver{cfg: cfg}

	in, err := d.invocationOn(Target{}, "claude", false)
	if err != nil {
		t.Fatalf("invocationOn: %v", err)
	}
	if in.EnvAllowlist != nil {
		t.Fatalf("EnvAllowlist = %v, want nil (inherit everything) on a config that declares none", in.EnvAllowlist)
	}
}
