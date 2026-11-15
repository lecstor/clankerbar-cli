package cli

// CLA-899: doctor reports the effective session_nice, because the dial's effect
// is invisible at runtime - an operator who set it to protect a shared CI runner
// has nothing else on the terminal that says whether it is on.

import (
	"errors"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// priorityEnv is a doctorEnv whose PATH holds nice(1) when niceOnPath is true.
func priorityEnv(niceOnPath bool) doctorEnv {
	return doctorEnv{lookPath: func(file string) (string, error) {
		if file == "nice" && !niceOnPath {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + file, nil
	}}
}

func TestCheckSessionPriorityReportsTheEffectiveValue(t *testing.T) {
	// 0 must not consult PATH at all: a daemon that never set the dial must not
	// gain a failure from a tool it does not use.
	c := checkSessionPriority(&config.Config{}, priorityEnv(false))
	if c.status != pass {
		t.Errorf("the default value is not a fault: status = %s", c.status)
	}
	if !strings.Contains(c.detail, "0 (default)") {
		t.Errorf("detail = %q, want the default named", c.detail)
	}

	c = checkSessionPriority(&config.Config{SessionNice: 10}, priorityEnv(true))
	if !harness.SessionPrioritySupported {
		if c.status != fail || !strings.Contains(c.remedy, "0") {
			t.Fatalf("status = %s, remedy = %q on a platform that cannot honour the dial; want a FAIL naming the 0 remedy", c.status, c.remedy)
		}
		return
	}
	if c.status != pass {
		t.Errorf("a configured priority is not a fault: status = %s", c.status)
	}
	if !strings.Contains(c.detail, "10") {
		t.Errorf("detail = %q, want the effective value", c.detail)
	}
	if !strings.Contains(c.detail, "up to 10 below") {
		t.Errorf("detail = %q, want the offset qualified: nice -n is additive and clamps at 19", c.detail)
	}
	if len(c.info) == 0 || !strings.Contains(strings.Join(c.info, " "), "CI") {
		t.Errorf("info = %v, want the shared-machine reason named", c.info)
	}
}

// The build tag says the platform CAN lower a priority; it does not say the
// binary that does it is installed. Every spawn fails without nice(1), so doctor
// must FAIL rather than promise a priority that never lands.
func TestCheckSessionPriorityFailsWhenNiceIsMissing(t *testing.T) {
	if !harness.SessionPrioritySupported {
		t.Skip("no session priority on this platform; the unsupported refusal is covered below")
	}
	c := checkSessionPriority(&config.Config{SessionNice: 10}, priorityEnv(false))
	if c.status != fail || !strings.Contains(c.detail, "nice(1)") || !strings.Contains(c.remedy, "0") {
		t.Fatalf("status = %s, detail = %q, remedy = %q; want a FAIL naming nice(1) and the 0 remedy", c.status, c.detail, c.remedy)
	}
}

// A negative value is refused by validation; doctor must not paper over it as the
// default it did not read.
func TestCheckSessionPriorityDoesNotReportANegativeAsDefault(t *testing.T) {
	c := checkSessionPriority(&config.Config{SessionNice: -1}, priorityEnv(true))
	if c.status != fail {
		t.Errorf("a negative session_nice is not a pass: status = %s", c.status)
	}
	if strings.Contains(c.detail, "(default)") {
		t.Errorf("detail = %q, want it to name the configured value, not a default", c.detail)
	}
}

// Validation refuses the unsupported case before doctor runs; the check must
// still fail closed rather than report green for a dial the machine cannot honour.
func TestCheckSessionPriorityFailsClosedOnAnUnsupportedPlatform(t *testing.T) {
	c := checkSessionPriority(&config.Config{SessionNice: 10}, priorityEnv(true))
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
