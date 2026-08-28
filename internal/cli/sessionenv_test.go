package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// --- CLA-462: doctor verifies the declared session env before a run leans on it

// Every declared fromCommand is executed by the preflight; a failure is a FAIL
// naming the variable and where it was declared, because every spawn that
// overlay reaches will refuse until it is fixed. Outputs are never printed.
func TestCheckSessionEnvRunsDeclaredCommands(t *testing.T) {
	cfg := validCfg(t)
	cfg.Env = config.EnvMap{
		"GOOD":     config.CommandEnv("echo ok"),
		"GH_TOKEN": config.CommandEnv("gh auth token"),
	}
	ran := 0
	e := defaultDoctorEnv()
	e.runEnvCmd = func(ctx context.Context, command string) (string, error) {
		ran++
		if strings.Contains(command, "gh") {
			return "", errors.New("account not logged in")
		}
		return "ok", nil
	}

	checks := checkSessionEnv(context.Background(), cfg, e)
	if ran != 2 {
		t.Fatalf("ran %d commands, want one per declaration (2)", ran)
	}
	var sawPass, sawFail bool
	for _, c := range checks {
		if c.status == fail && strings.Contains(c.name, "GH_TOKEN") {
			sawFail = true
			for _, want := range []string{"gh auth token", "account not logged in"} {
				if !strings.Contains(c.detail, want) {
					t.Errorf("FAIL row should mention %q: %q", want, c.detail)
				}
			}
			if !strings.Contains(c.remedy, "GH_TOKEN") {
				t.Errorf("remedy should name the variable: %q", c.remedy)
			}
		}
		if c.status == pass && strings.Contains(c.name, "GOOD") {
			sawPass = true
		}
		if strings.Contains(c.detail+c.remedy+c.name, "ok\nvalue-of-good") {
			t.Errorf("check leaked command output: %+v", c)
		}
	}
	if !sawFail || !sawPass {
		t.Fatalf("want one PASS and one FAIL row, got %+v", checks)
	}
}

// An "@path" literal is re-checked under its owner-only rule: resolution moved
// to spawn time (CLA-462), so the preflight is where a chmod-rotted secret file
// is still caught cheaply.
func TestCheckSessionEnvVerifiesPathFiles(t *testing.T) {
	cfg := validCfg(t)
	cfg.Env = config.EnvMap{"SECRET": {Literal: "@/home/me/.secrets/tok"}}
	e := defaultDoctorEnv()
	e.envPath = func(path string) error {
		if path != "/home/me/.secrets/tok" {
			t.Errorf("checked %q", path)
		}
		return errors.New("insecure file mode")
	}

	checks := checkSessionEnv(context.Background(), cfg, e)
	if len(checks) != 1 || checks[0].status != fail {
		t.Fatalf("want a single FAIL row for the rotted file, got %+v", checks)
	}
	if !strings.Contains(checks[0].detail, "/home/me/.secrets/tok") {
		t.Errorf("FAIL should name the file: %q", checks[0].detail)
	}
}

// A scope whose repos push with no GH_TOKEN source declared anywhere is the
// exact shape of the 2026-08-24 incident; the warning names the fix. Declaring
// the token at ANY applicable level - here a harness block - silences it.
func TestCheckTokenSourcesWarnsWithoutDeclaration(t *testing.T) {
	cfg := validCfg(t)
	cfg.Repos = map[string]string{"clankerbar-cli": "/tmp/somewhere"}

	warned := false
	for _, c := range checkTokenSources(cfg) {
		if c.status == warn {
			warned = true
			if !strings.Contains(c.remedy, "GH_TOKEN") || !strings.Contains(c.remedy, "fromCommand") {
				t.Errorf("remedy should name the fix: %q", c.remedy)
			}
		}
	}
	if !warned {
		t.Fatal("repos without any GH_TOKEN source did not warn")
	}

	cfg.Harnesses = map[string]config.HarnessConfig{
		"claude": {Env: config.EnvMap{"GH_TOKEN": config.CommandEnv("gh auth token")}},
	}
	for _, c := range checkTokenSources(cfg) {
		if c.status != pass {
			t.Errorf("a harness-level declaration counts as a source, got %s (%s)", c.status, c.detail)
		}
	}
}

// No repos declared means nothing is known to push and there is nothing to say.
func TestCheckTokenSourcesSilentWithoutRepos(t *testing.T) {
	cfg := validCfg(t)
	checks := checkTokenSources(cfg)
	if len(checks) != 1 || checks[0].status != pass {
		t.Fatalf("want a quiet pass, got %+v", checks)
	}
}

// Multi-project: each project's own declaration state is judged separately.
func TestCheckTokenSourcesJudgesPerProject(t *testing.T) {
	cfg := validCfg(t)
	cfg.Projects = []config.Project{
		{Slug: "has-token", WorkDir: t.TempDir(),
			Env: config.EnvMap{"GH_TOKEN": config.CommandEnv("gh auth token")}},
		{Slug: "no-token", WorkDir: t.TempDir()},
	}
	cfg.Repos = map[string]string{"shared": "/tmp/x"}

	var statuses []string
	for _, c := range checkTokenSources(cfg) {
		statuses = append(statuses, c.name+"="+c.status.String())
	}
	got := strings.Join(statuses, ", ")
	want := "gh token source[has-token]=PASS, gh token source[no-token]=WARN"
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

// When OPENCODE_PERMISSION is declared at two levels, the session carries the
// MORE specific one (SessionEnv's per-key overlay, adapters append it last), so
// that is the declaration doctor must name — pointing the operator at the layer
// whose value loses the overlay sends them to edit the wrong line.
func TestOpencodePermissionOverrideNamesTheWinningLayer(t *testing.T) {
	cfg := validCfg(t)
	cfg.Env = config.EnvMap{"OPENCODE_PERMISSION": config.CommandEnv("echo top")}
	cfg.Harnesses = map[string]config.HarnessConfig{
		"opencode": {Env: config.EnvMap{"OPENCODE_PERMISSION": {Literal: `{"bash":"allow"}`}}},
	}

	where, v, ok := opencodePermissionOverride(cfg, "opencode")
	if !ok {
		t.Fatal("want a found override")
	}
	if where != "harnesses.opencode.env" {
		t.Fatalf("named %q, want the harness block (the more specific layer)", where)
	}
	if v != `{"bash":"allow"}` {
		t.Fatalf("quoted %q, want the WINNING declaration's value", v)
	}

	// The pair block beats the project block beats everything above.
	cfg.Projects = []config.Project{{Slug: "p", WorkDir: t.TempDir(),
		Env: config.EnvMap{"OPENCODE_PERMISSION": {Literal: "proj"}},
		EnvPerHarness: map[string]config.EnvMap{
			"opencode": {"OPENCODE_PERMISSION": config.CommandEnv("gh auth token -u x")},
		}}}
	where, _, ok = opencodePermissionOverride(cfg, "opencode")
	if !ok || where != "projects[0].env_per_harness.opencode" {
		t.Fatalf("named %q (ok=%v), want the project-per-harness block", where, ok)
	}
}
