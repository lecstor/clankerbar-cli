package loop

// CLA-899: the dial has to reach the harness invocation from the config. Without
// this wire the spawn path would honour a priority nothing ever set.

import (
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

func TestInvocationCarriesTheSessionNice(t *testing.T) {
	cfg := &config.Config{
		Harness:     "claude",
		Prompt:      "work",
		BacklogURL:  "https://clankerbar.com",
		SessionNice: 10,
	}
	d := &Driver{cfg: cfg}

	in, err := d.invocationOn(Target{Name: "acme", WorkDir: t.TempDir()}, "claude", false)
	if err != nil {
		t.Fatalf("invocationOn: %v", err)
	}
	if in.Nice != 10 {
		t.Fatalf("Nice = %d, want the config's 10", in.Nice)
	}
}

func TestInvocationSessionNiceDefaultsToZero(t *testing.T) {
	cfg := &config.Config{Harness: "claude", Prompt: "work", BacklogURL: "https://clankerbar.com"}
	d := &Driver{cfg: cfg}

	in, err := d.invocationOn(Target{}, "claude", false)
	if err != nil {
		t.Fatalf("invocationOn: %v", err)
	}
	if in.Nice != 0 {
		t.Fatalf("Nice = %d, want 0 (unchanged) on a config that sets none", in.Nice)
	}
}
