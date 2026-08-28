package cli

// CLA-466: doctor's fleet activity-reporting check. The bar it pins: wired +
// reachable reads PASS, every gap (no key, no derivable endpoint, rejected key,
// an older plane without the route, unreachable) reads WARN with a remedy, and
// NOTHING here FAILs - reporting is telemetry, so its absence must not fail the
// cron gate (`doctor && run`) over a condition that costs no loop behaviour.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

// --- CLA-501: the instance-identity sibling scan -------------------------------

// writeDaemonConfigFile writes one owner-only config body into dir/base.
func writeDaemonConfigFile(t *testing.T, dir, base, body string) string {
	t.Helper()
	p := filepath.Join(dir, base)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

const identityCfgBody = `{"harness":"claude","prompt":"Work.","backlog_url":"https://plane.example/mcp/acme"}`

// loadDaemonConfig writes a plain daemon config (no instance_name) and loads it,
// so cfg.Source() names a real file whose siblings can be scanned.
func loadDaemonConfig(t *testing.T, dir, base string) *config.Config {
	t.Helper()
	p := writeDaemonConfigFile(t, dir, base, identityCfgBody)
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("config.Load(%s): %v", p, err)
	}
	return cfg
}

// The check is wired into the preflight: it must appear by name alongside the
// other fleet-adjacent checks.
func TestDoctorInstanceIdentityIsWired(t *testing.T) {
	checks := doctorChecks(context.Background(), fleetCfg(t), okEnv())
	find(t, checks, "instance identity")
}

// The bug this check exists for is invisible from inside one daemon: each
// co-located daemon believes its own name is fine. Two siblings declaring the
// SAME explicit instance_name would beacon under one Fleet row again.
func TestDoctorInstanceIdentityCollisionBetweenSiblingsWarns(t *testing.T) {
	dir := t.TempDir()
	writeDaemonConfigFile(t, dir, "clanker1.json",
		`{"harness":"claude","prompt":"Work.","instance_name":"box-a"}`)
	self := writeDaemonConfigFile(t, dir, "clanker2.json",
		`{"harness":"claude","prompt":"Work.","instance_name":"box-a"}`)
	cfg, err := config.Load(self)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	c := checkInstanceIdentity(cfg)
	if c.status != warn {
		t.Fatalf("two sibling configs with the same instance_name = %s (%s), want WARN", c.status, c.detail)
	}
	for _, want := range []string{"clanker1.json", "clanker2.json"} {
		if !strings.Contains(c.detail, want) {
			t.Errorf("WARN detail %q does not name %q", c.detail, want)
		}
	}
}

// The fix's own shape must NOT read as a collision: distinct basenames with no
// instance_name resolve distinctly now - warning on them would nag every
// well-configured multi-daemon host.
func TestDoctorInstanceIdentityDistinctBasenamesPass(t *testing.T) {
	dir := t.TempDir()
	writeDaemonConfigFile(t, dir, "clanker1.json", identityCfgBody)
	cfg := loadDaemonConfig(t, dir, "clanker2.json")
	c := checkInstanceIdentity(cfg)
	if c.status != pass {
		t.Fatalf("two unnamed sibling configs = %s (%s), want PASS (distinct basenames are distinct identities)", c.status, c.detail)
	}
	host, _ := os.Hostname()
	if !strings.Contains(strings.Join(c.info, "\n"), host+"/clanker2") {
		t.Errorf("info %v does not show the resolved identity", c.info)
	}
}

// A file the loader could not parse cannot start a daemon, so it can never
// beacon anything to collide with: the scan skips it rather than crashing or
// mis-reporting it as a peer.
func TestDoctorInstanceIdentitySkipsUnparseableSiblings(t *testing.T) {
	dir := t.TempDir()
	writeDaemonConfigFile(t, dir, "notes.json", `{not json`)
	writeDaemonConfigFile(t, dir, "clanker1.json", identityCfgBody)
	cfg := loadDaemonConfig(t, dir, "clanker2.json")
	c := checkInstanceIdentity(cfg)
	if c.status != pass {
		t.Fatalf("unparseable sibling = %s (%s), want PASS with the bad file skipped", c.status, c.detail)
	}
}

// Flags-only runs have no config directory to look sideways at.
func TestDoctorInstanceIdentityNoConfigFilePasses(t *testing.T) {
	c := checkInstanceIdentity(&config.Config{Harness: "claude", Prompt: "Work."})
	if c.status != pass || !strings.Contains(c.detail, "no config file") {
		t.Errorf("flags-only run = %s (%s), want PASS explaining nothing was scanned", c.status, c.detail)
	}
}
