package cli

// CLA-466: doctor's fleet activity-reporting check. The bar it pins: wired +
// reachable reads PASS, every gap (no key, no derivable endpoint, rejected key,
// an older plane without the route, unreachable) reads WARN with a remedy, and
// NOTHING here FAILs - reporting is telemetry, so its absence must not fail the
// cron gate (`doctor && run`) over a condition that costs no loop behaviour.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/fleet"
)

// fleetCfg is a valid config whose BacklogURL names an /mcp/<slug> path, so a
// slug-ful fleet report URL can be derived from it.
func fleetCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg := validCfg(t)
	cfg.BacklogURL = "https://plane.example/mcp/acme"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("fixture config does not validate: %v", err)
	}
	return cfg
}

func TestDoctorFleet_WiredAndReachablePasses(t *testing.T) {
	e := okEnv()
	checks := doctorChecks(context.Background(), fleetCfg(t), e)
	c := find(t, checks, "fleet")
	if c.status != pass {
		t.Fatalf("fleet check = %s (%s), want PASS", c.status, c.detail)
	}
	if !strings.Contains(c.detail, "reachable") {
		t.Errorf("PASS detail %q does not say what was verified", c.detail)
	}
}

func TestDoctorFleet_UnwiredCasesWarn(t *testing.T) {
	t.Run("no API key", func(t *testing.T) {
		e := okEnv()
		e.apiKey = ""
		c := find(t, doctorChecks(context.Background(), fleetCfg(t), e), "fleet")
		if c.status != warn || !strings.Contains(c.detail, "reporting is off") {
			t.Errorf("fleet = %s (%s), want WARN naming reporting off", c.status, c.detail)
		}
	})
	t.Run("no derivable endpoint", func(t *testing.T) {
		e := okEnv()
		// validCfg names no /mcp/<slug>, so no slug-ful report URL exists.
		c := find(t, doctorChecks(context.Background(), validCfg(t), e), "fleet")
		if c.status != warn || !strings.Contains(c.detail, "reporting is off") {
			t.Errorf("fleet = %s (%s), want WARN naming reporting off", c.status, c.detail)
		}
	})
}

func TestDoctorFleet_BadAnswersWarnNeverFail(t *testing.T) {
	cases := []struct {
		name     string
		probeErr error
		wantIn   string
	}{
		{
			name:     "rejected key",
			probeErr: &fleet.ProbeError{Status: 401, Body: "unauthorized"},
			wantIn:   "key was rejected",
		},
		{
			name:     "plane predates fleets",
			probeErr: &fleet.ProbeError{Status: 404, Body: "not found"},
			wantIn:   "no fleet-report route",
		},
		{
			name:     "unexpected status",
			probeErr: &fleet.ProbeError{Status: 500, Body: "boom"},
			wantIn:   "answered unexpectedly",
		},
		{
			name:     "unreachable",
			probeErr: errors.New("dial tcp: connection refused"),
			wantIn:   "unreachable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := okEnv()
			e.fleetProbe = func(context.Context, string, string) error { return tc.probeErr }
			c := find(t, doctorChecks(context.Background(), fleetCfg(t), e), "fleet")
			if c.status != warn {
				t.Errorf("fleet = %s (%s), want WARN — a reporting gap must never fail the cron gate", c.status, c.detail)
			}
			if !strings.Contains(c.detail, tc.wantIn) {
				t.Errorf("detail %q does not name the finding (%s)", c.detail, tc.wantIn)
			}
			if c.remedy == "" {
				t.Errorf("WARN without a remedy is noise the operator skims past")
			}
		})
	}
}

func TestDoctorFleet_PerProjectChecks(t *testing.T) {
	cfg := fleetCfg(t)
	cfg.Projects = []config.Project{{Slug: "acme"}, {Slug: "other"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("fixture config does not validate: %v", err)
	}
	e := okEnv()
	checks := doctorChecks(context.Background(), cfg, e)
	for _, slug := range []string{"acme", "other"} {
		c := find(t, checks, "fleet["+slug+"]")
		if c.status != pass {
			t.Errorf("fleet[%s] = %s (%s), want PASS", slug, c.status, c.detail)
		}
	}
}
