package cli

// CLA-899: doctor reports the effective session_nice, because the dial's effect
// is invisible at runtime - an operator who set it to protect a shared CI runner
// has nothing else on the terminal that says whether it is on.

import (
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

func TestCheckSessionPriorityReportsTheEffectiveValue(t *testing.T) {
	c := checkSessionPriority(&config.Config{})
	if c.status != pass {
		t.Errorf("the default value is not a fault: status = %s", c.status)
	}
	if !strings.Contains(c.detail, "0 (default)") {
		t.Errorf("detail = %q, want the default named", c.detail)
	}

	c = checkSessionPriority(&config.Config{SessionNice: 10})
	if c.status != pass {
		t.Errorf("a configured priority is not a fault: status = %s", c.status)
	}
	if !strings.Contains(c.detail, "10") {
		t.Errorf("detail = %q, want the effective value", c.detail)
	}
	if len(c.info) == 0 || !strings.Contains(strings.Join(c.info, " "), "CI") {
		t.Errorf("info = %v, want the shared-machine reason named", c.info)
	}
}

// Validation refuses this before doctor runs; the check must still fail closed
// rather than report green for a dial the machine cannot honour.
func TestCheckSessionPriorityFailsClosedOnAnUnsupportedPlatform(t *testing.T) {
	c := checkSessionPriority(&config.Config{SessionNice: 10})
	if harness.SessionPrioritySupported {
		if c.status != pass {
			t.Fatalf("status = %s on a supported platform, want PASS", c.status)
		}
		return
	}
	if c.status != fail || !strings.Contains(c.remedy, "0") {
		t.Fatalf("status = %s, remedy = %q; want a FAIL naming the 0 remedy", c.status, c.remedy)
	}
}
