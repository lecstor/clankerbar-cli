package loop

import (
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

func inboxTestDriver(wired bool) *Driver {
	cfg := &config.Config{
		Harness:    "claude",
		Prompt:     "work",
		BacklogURL: "https://clankerbar.com",
	}
	d := &Driver{cfg: cfg}
	if wired {
		d.SetInboxChannel("https://clankerbar.com", "clk_test")
	}
	return d
}

// Delivery is capability-gated and probe-free: claude only in v1, never a probe, and never
// when the plane wiring is absent.
func TestInvocationCarriesInboxOnlyForACapableWiredHarness(t *testing.T) {
	cases := []struct {
		name    string
		driver  *Driver
		harness string
		probe   bool
		want    bool
	}{
		{"claude is wired", inboxTestDriver(true), "claude", false, true},
		{"no wiring", inboxTestDriver(false), "claude", false, false},
		{"probe", inboxTestDriver(true), "claude", true, false},
		{"opencode has no delivery in v1", inboxTestDriver(true), "opencode", false, false},
		{"codex has no delivery in v1", inboxTestDriver(true), "codex", false, false},
		{"copilot has no delivery in v1", inboxTestDriver(true), "copilot", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in, err := tc.driver.invocationOn(Target{}, tc.harness, tc.probe)
			if err != nil {
				t.Fatalf("invocationOn: %v", err)
			}
			if got := in.Inbox != nil; got != tc.want {
				t.Fatalf("Inbox set = %v, want %v", got, tc.want)
			}
			if in.Inbox != nil {
				if in.Inbox.Origin != "https://clankerbar.com" || in.Inbox.APIKey != "clk_test" {
					t.Errorf("Inbox = %+v", in.Inbox)
				}
			}
		})
	}
}
