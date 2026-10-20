package cli

// doctor's half of the session env allowlist (CLA-714): it must report which
// shape is in force, name the variables a session will receive (never values),
// and FAIL when the declared list would starve a session.

import (
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// clearWorkdirRoot makes the "derivation is on" signal deterministic: a test
// binary started by the fleet supervisor already carries the variable.
func clearWorkdirRoot(t *testing.T) {
	t.Helper()
	t.Setenv(WorkdirRootEnv, "")
}

func statusOf(checks []check, name string) (check, bool) {
	for _, c := range checks {
		if c.name == name {
			return c, true
		}
	}
	return check{}, false
}

func TestDoctorSessionEnvAllowlistDefaultIsFullInheritance(t *testing.T) {
	clearWorkdirRoot(t)
	cfg := &config.Config{Harness: "claude", Prompt: "work", BacklogURL: "https://clankerbar.com"}

	got := checkSessionEnvAllowlist(cfg)
	if len(got) != 1 {
		t.Fatalf("checks = %v, want one for the single harness/single project scope", got)
	}
	c := got[0]
	if c.status != pass {
		t.Errorf("status = %v, want pass: full inheritance is the documented default", c.status)
	}
	if !strings.Contains(c.detail, "whole environment") {
		t.Errorf("detail = %q, want it to say sessions inherit the daemon's whole environment", c.detail)
	}
}

func TestDoctorSessionEnvAllowlistNamesWhatSessionsReceive(t *testing.T) {
	clearWorkdirRoot(t)
	cfg := &config.Config{
		Harness:      "claude",
		Prompt:       "work",
		BacklogURL:   "https://clankerbar.com",
		EnvAllowlist: []string{"PATH", "HOME", "CLANKERBAR_API_KEY"},
		Env:          config.EnvMap{"GH_TOKEN": {Literal: "super-secret-value"}},
	}

	got := checkSessionEnvAllowlist(cfg)
	if len(got) != 1 {
		t.Fatalf("checks = %v, want one", got)
	}
	c := got[0]
	if c.status != pass {
		t.Fatalf("status = %v (%s), want pass for a list carrying the minimum", c.status, c.detail)
	}
	for _, want := range []string{"HOME", "PATH", "CLANKERBAR_API_KEY"} {
		if !strings.Contains(c.detail, want) {
			t.Errorf("detail = %q, want it to name %s", c.detail, want)
		}
	}
	if !strings.Contains(c.detail, "GH_TOKEN") {
		t.Errorf("detail = %q, want the env block's names too - they are what the session also receives", c.detail)
	}
	if strings.Contains(c.detail, "super-secret-value") || strings.Contains(c.remedy, "super-secret-value") {
		t.Fatalf("doctor printed a VALUE: %q / %q", c.detail, c.remedy)
	}
}

func TestDoctorSessionEnvAllowlistFailsOnAMissingMinimum(t *testing.T) {
	clearWorkdirRoot(t)
	cfg := &config.Config{
		Harness:      "claude",
		Prompt:       "work",
		BacklogURL:   "https://clankerbar.com",
		EnvAllowlist: []string{"HOME", "PATH"},
	}

	got := checkSessionEnvAllowlist(cfg)
	c, ok := statusOf(got, "session env allowlist")
	if !ok {
		t.Fatalf("checks = %v, want a session env allowlist check", got)
	}
	if c.status != fail {
		t.Fatalf("status = %v, want fail when CLANKERBAR_API_KEY is dropped", c.status)
	}
	if !strings.Contains(c.detail, "CLANKERBAR_API_KEY") || !strings.Contains(c.remedy, "CLANKERBAR_API_KEY") {
		t.Errorf("detail/remedy = %q / %q, want both to name the missing variable", c.detail, c.remedy)
	}
	// A dropped HOME/PATH is named too, not just the first miss.
	cfg.EnvAllowlist = []string{"CLANKERBAR_API_KEY"}
	c, _ = statusOf(checkSessionEnvAllowlist(cfg), "session env allowlist")
	for _, want := range []string{"HOME", "PATH"} {
		if !strings.Contains(c.detail, want) {
			t.Errorf("detail = %q, want it to name missing %s", c.detail, want)
		}
	}
}

// The fourth required variable joins the minimum only while the workdir
// derivation is on.
func TestDoctorSessionEnvAllowlistRequiresTheWorkdirRootWhenDeriving(t *testing.T) {
	clearWorkdirRoot(t)
	minimal := []string{"HOME", "PATH", "CLANKERBAR_API_KEY"}
	cfg := &config.Config{Harness: "claude", Prompt: "work", BacklogURL: "https://clankerbar.com", EnvAllowlist: minimal}

	if c, _ := statusOf(checkSessionEnvAllowlist(cfg), "session env allowlist"); c.status != pass {
		t.Fatalf("status = %v on a non-deriving config, want pass", c.status)
	}

	cfg.ProjectRoot = true
	c, _ := statusOf(checkSessionEnvAllowlist(cfg), "session env allowlist")
	if c.status != fail || !strings.Contains(c.detail, WorkdirRootEnv) {
		t.Fatalf("status/detail = %v/%q, want fail naming %s once project_root says the derivation is on", c.status, c.detail, WorkdirRootEnv)
	}

	// The supervisor's own environment is the other signal: doctor run against a
	// BASE config (project_root unset) by the supervisor still derives.
	cfg.ProjectRoot = false
	t.Setenv(WorkdirRootEnv, "/machine/root")
	c, _ = statusOf(checkSessionEnvAllowlist(cfg), "session env allowlist")
	if c.status != fail || !strings.Contains(c.detail, WorkdirRootEnv) {
		t.Fatalf("status/detail = %v/%q, want fail while %s is in the environment", c.status, c.detail, WorkdirRootEnv)
	}
}

// A per-project list is reported and checked per project, not folded into one.
func TestDoctorSessionEnvAllowlistPerProject(t *testing.T) {
	clearWorkdirRoot(t)
	cfg := &config.Config{
		Harness:      "claude",
		Prompt:       "work",
		BacklogURL:   "https://clankerbar.com",
		EnvAllowlist: []string{"HOME", "PATH", "CLANKERBAR_API_KEY"},
		Projects: []config.Project{
			{Slug: "good"},
			{Slug: "narrow", EnvAllowlist: []string{"HOME"}},
		},
	}
	got := checkSessionEnvAllowlist(cfg)
	if len(got) != 2 {
		t.Fatalf("checks = %v, want one per project scope", got)
	}
	good, ok := statusOf(got, "session env allowlist[good]")
	if !ok {
		t.Fatalf("checks = %v, want session env allowlist[good]", got)
	}
	if good.status != pass {
		t.Errorf("good: status = %v (%s), want pass", good.status, good.detail)
	}
	narrow, ok := statusOf(got, "session env allowlist[narrow]")
	if !ok {
		t.Fatalf("checks = %v, want session env allowlist[narrow]", got)
	}
	if narrow.status != fail {
		t.Errorf("narrow: status = %v, want fail - its list replaces the top-level one", narrow.status)
	}
}

// A run-wide `harness` every phase overrides is DECLARED but never spawned, so
// its allowlist gets no check at all — the same doctrine runsHarness and
// costBlindHarnesses follow in the budget check. Reporting (and FAILing) its
// scope would send the operator to fix a harness no session ever runs on, and a
// false FAIL blocks a run that works.
func TestDoctorSessionEnvAllowlistSkipsANeverSpawnedRunWideHarness(t *testing.T) {
	clearWorkdirRoot(t)
	cfg := &config.Config{
		Harness:    "claude",
		Prompt:     "work",
		BacklogURL: "https://clankerbar.com",
		Phases: []config.Phase{
			{Name: "implement", Harness: "opencode"},
			{Name: "verify", Harness: "opencode"},
		},
		EnvAllowlist: []string{"HOME", "PATH", "CLANKERBAR_API_KEY"},
		Harnesses: map[string]config.HarnessConfig{
			"claude":   {EnvAllowlist: []string{"HOME"}},
			"opencode": {EnvAllowlist: []string{"HOME", "PATH", "CLANKERBAR_API_KEY"}},
		},
	}

	got := checkSessionEnvAllowlist(cfg)
	if len(got) != 1 {
		t.Fatalf("checks = %v, want exactly one for the only spawned harness (opencode)", got)
	}
	c := got[0]
	if c.name != "session env allowlist" {
		t.Errorf("name = %q, want it unqualified - only one harness spawns, so no harness qualifier is needed", c.name)
	}
	if c.status != pass {
		t.Errorf("status = %v (%s), want pass: claude's deficient list is never used", c.status, c.detail)
	}
	for _, wrong := range []string{"[claude]", "claude:"} {
		if strings.Contains(c.name, wrong) {
			t.Errorf("name = %q, want no check for the never-spawned run-wide harness", c.name)
		}
	}
}

// The multi-harness naming still qualifies by harness for the ones that DO
// spawn, so the skip above cannot be had by simply dropping the qualifier.
func TestDoctorSessionEnvAllowlistQualifiesWhenTwoHarnessesSpawn(t *testing.T) {
	clearWorkdirRoot(t)
	cfg := &config.Config{
		Harness:      "claude",
		Prompt:       "work",
		BacklogURL:   "https://clankerbar.com",
		EnvAllowlist: []string{"HOME", "PATH", "CLANKERBAR_API_KEY"},
		Phases: []config.Phase{
			{Name: "implement", Harness: "claude"},
			{Name: "verify", Harness: "opencode"},
		},
	}

	got := checkSessionEnvAllowlist(cfg)
	if len(got) != 2 {
		t.Fatalf("checks = %v, want one per spawned harness", got)
	}
	for _, want := range []string{"session env allowlist[claude]", "session env allowlist[opencode]"} {
		if _, ok := statusOf(got, want); !ok {
			t.Errorf("checks = %v, want %q", got, want)
		}
	}
}

// The config summary says which shape is in force, beside the declared names.
func TestDoctorConfigReportsTheInheritanceShape(t *testing.T) {
	clearWorkdirRoot(t)
	base := &config.Config{Harness: "claude", Prompt: "work", BacklogURL: "https://clankerbar.com"}

	c := checkConfig(base)
	joined := strings.Join(c.info, "\n")
	if !strings.Contains(joined, "whole environment") {
		t.Errorf("info = %q, want it to say sessions inherit the whole environment when no allowlist is declared", joined)
	}

	base.EnvAllowlist = []string{"HOME", "PATH", "CLANKERBAR_API_KEY"}
	c = checkConfig(base)
	joined = strings.Join(c.info, "\n")
	if !strings.Contains(joined, "env_allowlist above") || !strings.Contains(joined, "CLANKERBAR_API_KEY") {
		t.Errorf("info = %q, want the allowlist shape AND the variable names", joined)
	}
}

// The env overlay is appended AFTER the inheritEnv filter and wins (every
// adapter's env() puts in.Env last), so a required variable the env block
// declares reaches the session even when the allowlist omits it. FAILing that
// shape sent the operator to fix a run that works (CLA-736 review 1.review-2.1).
func TestDoctorSessionEnvAllowlistCountsTheEnvBlockAsPresent(t *testing.T) {
	clearWorkdirRoot(t)
	cfg := &config.Config{
		Harness:      "opencode",
		Prompt:       "work",
		BacklogURL:   "https://clankerbar.com",
		EnvAllowlist: []string{"HOME", "PATH"},
		Env:          config.EnvMap{"CLANKERBAR_API_KEY": {Literal: "sk-not-a-real-key"}},
	}

	c, ok := statusOf(checkSessionEnvAllowlist(cfg), "session env allowlist")
	if !ok {
		t.Fatal("no session env allowlist check")
	}
	if c.status != pass {
		t.Fatalf("status = %v (%s), want pass: the env block injects the key and wins over the allowlist", c.status, c.detail)
	}
	if strings.Contains(c.detail, "missing") {
		t.Errorf("detail = %q, want no missing-variable claim for a variable the env block declares", c.detail)
	}

	// Genuinely nowhere is still a FAIL: the union is what changed, not the check.
	cfg.Env = nil
	c, _ = statusOf(checkSessionEnvAllowlist(cfg), "session env allowlist")
	if c.status != fail {
		t.Fatalf("status = %v (%s), want fail when CLANKERBAR_API_KEY is in neither the allowlist nor the env block", c.status, c.detail)
	}
	if !strings.Contains(c.detail, "CLANKERBAR_API_KEY") || !strings.Contains(c.remedy, "CLANKERBAR_API_KEY") {
		t.Errorf("detail/remedy = %q / %q, want both to name the genuinely missing variable", c.detail, c.remedy)
	}
}

// SSH_AUTH_SOCK is ADVICE, never a verdict: an ssh clone_url_base works from an
// on-disk key, so only an agent-held key needs the socket (CLA-736 review
// 1.review-2.2).
func TestDoctorSessionEnvAllowlistNotesSSHAuthSock(t *testing.T) {
	clearWorkdirRoot(t)
	cfg := &config.Config{
		Harness:      "opencode",
		Prompt:       "work",
		BacklogURL:   "https://clankerbar.com",
		EnvAllowlist: []string{"HOME", "PATH", "CLANKERBAR_API_KEY"},
	}

	c, ok := statusOf(checkSessionEnvAllowlist(cfg), "session env allowlist")
	if !ok {
		t.Fatal("no session env allowlist check")
	}
	if c.status != pass {
		t.Fatalf("status = %v (%s), want pass - the socket is advice, not a requirement", c.status, c.detail)
	}
	if !strings.Contains(strings.Join(c.info, "\n"), "SSH_AUTH_SOCK") {
		t.Errorf("info = %q, want a note that an agent-held ssh key needs SSH_AUTH_SOCK", c.info)
	}

	// Once the name reaches the session - through the allowlist OR the env block
	// - the advice is noise and must go.
	cfg.EnvAllowlist = append(cfg.EnvAllowlist, "SSH_AUTH_SOCK")
	c, _ = statusOf(checkSessionEnvAllowlist(cfg), "session env allowlist")
	if strings.Contains(strings.Join(c.info, "\n"), "SSH_AUTH_SOCK") {
		t.Errorf("info = %q, want no note when the allowlist carries SSH_AUTH_SOCK", c.info)
	}

	cfg.EnvAllowlist = []string{"HOME", "PATH", "CLANKERBAR_API_KEY"}
	cfg.Env = config.EnvMap{"SSH_AUTH_SOCK": {Literal: "/tmp/agent.sock"}}
	c, _ = statusOf(checkSessionEnvAllowlist(cfg), "session env allowlist")
	if strings.Contains(strings.Join(c.info, "\n"), "SSH_AUTH_SOCK") {
		t.Errorf("info = %q, want no note when the env block declares SSH_AUTH_SOCK", c.info)
	}
}
