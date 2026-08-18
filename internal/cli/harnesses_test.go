package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// doctor's per-harness fan-out (CLA-366). A mixed sequence spawns binaries the
// run-wide `harness` never names, so a preflight that reports only that one
// certifies half the run and stays green about the other half.

// mixedDoctorCfg is implement-on-opencode / review-on-claude, validated.
func mixedDoctorCfg(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := &config.Config{
		Harness: "claude",
		WorkDir: t.TempDir(),
		Phases: []config.Phase{
			{Name: "implement", Harness: "opencode"},
			{Name: "review"},
		},
		Harnesses: map[string]config.HarnessConfig{
			"opencode": {ConfigDir: t.TempDir()},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("fixture config does not validate: %v", err)
	}
	return cfg
}

// The back-compat half matters as much as the fan-out: a single-harness report
// must read exactly as it always did, which is why the FIRST entry keeps the
// bare label and only the rest are qualified.
func TestDoctorFanOut_LabelsAndSingleHarnessBackCompat(t *testing.T) {
	single := validCfg(t)
	for _, tc := range []struct {
		what string
		got  []string
		want string
	}{
		{"harness", names(checkHarnesses(context.Background(), single, okEnv())), "harness"},
		{"config_dir", names(checkConfigDirs(single)), "config_dir"},
		{"permissions", names(checkPermissionsAll(single)), "permissions"},
		{"workdir", names(checkSessions(single)), "workdir"},
	} {
		if len(tc.got) != 1 || tc.got[0] != tc.want {
			t.Errorf("single-harness %s checks = %v, want exactly [%s] — an unqualified label is the pre-existing output", tc.what, tc.got, tc.want)
		}
	}

	mixed := mixedDoctorCfg(t)
	if got := names(checkHarnesses(context.Background(), mixed, okEnv())); len(got) != 2 || got[0] != "harness" || got[1] != "harness[opencode]" {
		t.Errorf("mixed harness checks = %v, want [harness harness[opencode]] — the second binary has to be looked for too", got)
	}
	if got := names(checkConfigDirs(mixed)); len(got) != 2 || got[1] != "config_dir[opencode]" {
		t.Errorf("mixed config_dir checks = %v, want a qualified second entry", got)
	}
	if got := names(checkPermissionsAll(mixed)); len(got) != 2 || got[1] != "permissions[opencode]" {
		t.Errorf("mixed permissions checks = %v, want a qualified second entry", got)
	}
	if got := names(checkSessions(mixed)); len(got) != 2 || got[0] != "workdir[claude]" || got[1] != "workdir[opencode]" {
		t.Errorf("mixed session checks = %v, want one per harness — the same file means different things to each", got)
	}
}

// The binary a phase needs is one doctor must actually look for: a missing
// opencode on PATH is a run that dies at phase 1, and reporting only claude
// would have called that config green.
func TestDoctorFanOut_AMissingPhaseBinaryFails(t *testing.T) {
	e := okEnv()
	e.lookPath = func(f string) (string, error) {
		if f == "opencode" {
			return "", errors.New("not found")
		}
		return "/usr/local/bin/" + f, nil
	}
	got := checkHarnesses(context.Background(), mixedDoctorCfg(t), e)
	if len(got) != 2 || got[1].status != fail {
		t.Fatalf("checks = %+v, want the opencode entry to FAIL", got)
	}
	if got[0].status != pass {
		t.Errorf("the claude entry should still pass: %+v", got[0])
	}
}

// The reversed pairing is the one that used to go dark: with claude as a PHASE
// rather than the run's harness, the allowlist audit and the settings-policy
// check both keyed off `harness` and reported nothing about the session that
// actually runs the verification verbs.
func TestDoctorFanOut_ClaudeAsAPhaseStillGetsItsAllowlistAudited(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := &config.Config{
		Harness:   "opencode",
		WorkDir:   t.TempDir(),
		ConfigDir: t.TempDir(),
		Phases: []config.Phase{
			{Name: "implement"},
			{Name: "review", Harness: "claude"},
		},
		Harnesses: map[string]config.HarnessConfig{
			"claude": {ConfigDir: t.TempDir()},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("fixture config does not validate: %v", err)
	}

	if c := checkToolchains(cfg); strings.Contains(c.detail, "no allowlist to audit") {
		t.Errorf("toolchains check went dark for a claude REVIEW phase: %q", c.detail)
	}
}

// A cost ceiling is only as good as the harnesses that can feed it. In a mixed
// run, one blind harness is enough to make max_cost_usd a line that reports a
// ceiling it cannot hold.
func TestCheckBudget_CostIsInertWhenAnyPhaseHarnessCannotReportIt(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := &config.Config{
		Harness: "claude", // reports cost
		WorkDir: t.TempDir(),
		Phases: []config.Phase{
			{Name: "implement", Harness: "codex"}, // does not
			{Name: "review"},
		},
		Harnesses: map[string]config.HarnessConfig{"codex": {ConfigDir: t.TempDir()}},
		Budget:    config.Budget{MaxCostUSD: 10},
	}
	// Validate refuses codex in a phase sequence, so drive the check directly —
	// the arithmetic under test is checkBudget's, not Validate's.
	if got := costBlindHarnesses(cfg); len(got) != 1 || got[0] != "codex" {
		t.Fatalf("costBlindHarnesses = %v, want [codex]", got)
	}
	c := checkBudget(cfg)
	if !strings.Contains(c.detail, "INERT") {
		t.Errorf("budget detail %q should mark max_cost_usd inert: a share of this run's spend never reaches it", c.detail)
	}
}
