package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// doctor's planner preflight (studio-chat.md §2, D23): report every project with
// no planner, refuse the instance only when none has one, and FAIL rather than
// substitute when the assigned harness cannot be served here.

type rcFake struct {
	state *plane.RunConfigState
	err   error
}

func (f rcFake) RunConfig(context.Context) (*plane.RunConfigState, error) { return f.state, f.err }
func (f rcFake) ProposeRunConfig(context.Context, map[string]any, string) error {
	return errors.New("not used")
}

func plannerConfig(t *testing.T, projects ...string) *config.Config {
	t.Helper()
	cfgDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfgDir, ".credentials.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &config.Config{
		Harness:    "claude",
		Role:       config.RolePlanner,
		Prompt:     "Work the next backlog item.",
		BacklogURL: "https://clankerbar.com/mcp/demo",
		WorkDir:    t.TempDir(),
		ConfigDir:  cfgDir,
	}
	// A Claude-shaped file resolves for claude; its existence is not read here.
	c.MCPConfigPath = c.WorkDir + "/.mcp.json"
	for _, slug := range projects {
		p := config.Project{Slug: slug}
		p.MCPConfigPath = c.MCPConfigPath
		c.Projects = append(c.Projects, p)
	}
	return c
}

func plannerDoc(t *testing.T, role map[string]any) *plane.RunConfigState {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"roles": map[string]any{"planner": role}})
	if err != nil {
		t.Fatal(err)
	}
	return &plane.RunConfigState{Version: 1, Config: raw}
}

func plannerEnv(states ...*plane.RunConfigState) doctorEnv {
	i := 0
	return doctorEnv{
		apiKey:   "k",
		lookPath: func(string) (string, error) { return "/usr/local/bin/claude", nil },
		binVersion: func(context.Context, string) (string, error) {
			return "claude 2.1.322", nil
		},
		newRCfgAPI: func(string, string) plane.RunConfigAPI {
			st := &plane.RunConfigState{}
			if i < len(states) {
				st = states[i]
			}
			i++
			return rcFake{state: st}
		},
	}
}

// findCheck returns the LAST check with the name: the aggregate `planner` line
// is appended after the per-project ones, so the aggregate's verdict is what a
// caller asking about the instance wants.
func findCheck(checks []check, name string) *check {
	for i := len(checks) - 1; i >= 0; i-- {
		if checks[i].name == name {
			return &checks[i]
		}
	}
	return nil
}

func TestDoctorWorkerRoleIsReported(t *testing.T) {
	c := &config.Config{Role: config.RoleWorker, Harness: "claude"}
	checks := checkPlanner(context.Background(), c, plannerEnv())
	if len(checks) != 1 || checks[0].status != pass || !strings.Contains(checks[0].detail, "worker") {
		t.Fatalf("checks = %+v, want one worker PASS", checks)
	}
}

func TestDoctorPlannerNoDerivableProjectFails(t *testing.T) {
	c := &config.Config{Role: config.RolePlanner, Harness: "claude", WorkDir: t.TempDir()}
	checks := checkPlanner(context.Background(), c, plannerEnv())
	c0 := findCheck(checks, "planner")
	if c0 == nil || c0.status != fail {
		t.Fatalf("checks = %+v, want a planner FAIL with no pollable project", checks)
	}
}

func TestDoctorPlannerWithNoConfiguredRoleFails(t *testing.T) {
	c := plannerConfig(t) // no roles.planner anywhere
	env := plannerEnv(&plane.RunConfigState{Version: 1, Config: json.RawMessage(`{}`)})
	checks := checkPlanner(context.Background(), c, env)
	if c0 := findCheck(checks, "planner"); c0 == nil || c0.status != fail ||
		!strings.Contains(c0.detail, "no project's resolved run config carries roles.planner") {
		t.Fatalf("checks = %+v, want the aggregate FAIL", checks)
	}
}

func TestDoctorPlannerUsableRolePasses(t *testing.T) {
	c := plannerConfig(t)
	env := plannerEnv(plannerDoc(t, map[string]any{"harness": "claude"}))
	checks := checkPlanner(context.Background(), c, env)
	c0 := findCheck(checks, "planner[demo]")
	if c0 == nil || c0.status != pass {
		t.Fatalf("checks = %+v, want a PASS for a runnable claude planner", checks)
	}
	if !strings.Contains(c0.detail, "harness") || !strings.Contains(c0.detail, "model (harness default)") {
		t.Errorf("detail = %q, want the harness and the default model", c0.detail)
	}
}

func TestDoctorPlannerUnconfiguredRoleHarnessFails(t *testing.T) {
	c := plannerConfig(t)
	env := plannerEnv(plannerDoc(t, map[string]any{"harness": "opencode"}))
	checks := checkPlanner(context.Background(), c, env)
	if c0 := findCheck(checks, "planner[demo]"); c0 == nil || c0.status != fail {
		t.Fatalf("checks = %+v, want a FAIL for a non-top-level harness with no local block", checks)
	}
	if !strings.Contains(findCheck(checks, "planner[demo]").detail, "harnesses.opencode") {
		t.Errorf("detail does not name the missing block: %q", findCheck(checks, "planner[demo]").detail)
	}
}

func TestDoctorPlannerUnknownHarnessFails(t *testing.T) {
	c := plannerConfig(t)
	env := plannerEnv(plannerDoc(t, map[string]any{"harness": "chatgpt"}))
	checks := checkPlanner(context.Background(), c, env)
	if c0 := findCheck(checks, "planner[demo]"); c0 == nil || c0.status != fail ||
		!strings.Contains(c0.detail, "cannot be served") {
		t.Fatalf("checks = %+v, want a FAIL naming the unresolvable role", checks)
	}
}

func TestDoctorPlannerUnresolvableTierFails(t *testing.T) {
	c := plannerConfig(t)
	env := plannerEnv(plannerDoc(t, map[string]any{"harness": "claude", "tier": "missing"}))
	checks := checkPlanner(context.Background(), c, env)
	if c0 := findCheck(checks, "planner[demo]"); c0 == nil || c0.status != fail ||
		!strings.Contains(c0.detail, "resolves to no model") {
		t.Fatalf("checks = %+v, want the no-fallback tier FAIL", checks)
	}
}

func TestDoctorPlannerModelIsUnverified(t *testing.T) {
	c := plannerConfig(t)
	env := plannerEnv(plannerDoc(t, map[string]any{"harness": "claude", "model": "opus"}))
	checks := checkPlanner(context.Background(), c, env)
	c0 := findCheck(checks, "planner[demo]")
	if c0 == nil || c0.status != warn || !strings.Contains(c0.detail, "model_unverified") {
		t.Fatalf("checks = %+v, want a model_unverified WARN", checks)
	}
}

func TestDoctorPlannerMissingBinaryFails(t *testing.T) {
	c := plannerConfig(t)
	env := plannerEnv(plannerDoc(t, map[string]any{"harness": "claude"}))
	env.lookPath = func(string) (string, error) { return "", errors.New("not found") }
	checks := checkPlanner(context.Background(), c, env)
	if c0 := findCheck(checks, "planner[demo]"); c0 == nil || c0.status != fail ||
		!strings.Contains(c0.detail, "not on PATH") {
		t.Fatalf("checks = %+v, want a FAIL for a missing binary", checks)
	}
}

func TestDoctorPlannerUnpinnedCredentialsWarn(t *testing.T) {
	c := plannerConfig(t)
	c.ConfigDir = ""
	env := plannerEnv(plannerDoc(t, map[string]any{"harness": "claude"}))
	checks := checkPlanner(context.Background(), c, env)
	if c0 := findCheck(checks, "planner[demo]"); c0 == nil || c0.status != warn ||
		!strings.Contains(c0.detail, "no config_dir pinned") {
		t.Fatalf("checks = %+v, want a credential WARN", checks)
	}
}

func TestDoctorPlannerMissingConfigDirFails(t *testing.T) {
	c := plannerConfig(t)
	c.ConfigDir = filepath.Join(t.TempDir(), "gone")
	env := plannerEnv(plannerDoc(t, map[string]any{"harness": "claude"}))
	checks := checkPlanner(context.Background(), c, env)
	if c0 := findCheck(checks, "planner[demo]"); c0 == nil || c0.status != fail ||
		!strings.Contains(c0.detail, "does not resolve") {
		t.Fatalf("checks = %+v, want a FAIL for a config dir that does not exist", checks)
	}
}

func TestDoctorPlannerMissingMCPConfigFails(t *testing.T) {
	c := plannerConfig(t)
	c.MCPConfigPath = ""
	env := plannerEnv(plannerDoc(t, map[string]any{"harness": "claude"}))
	checks := checkPlanner(context.Background(), c, env)
	if c0 := findCheck(checks, "planner[demo]"); c0 == nil || c0.status != fail ||
		!strings.Contains(c0.detail, "reply_conversation") {
		t.Fatalf("checks = %+v, want a FAIL when no MCP config reaches the session", checks)
	}
}

func TestDoctorPlannerOneProjectWithoutRoleIsNotAggregateFailure(t *testing.T) {
	c := plannerConfig(t, "with-chat", "without-chat")
	with := plannerDoc(t, map[string]any{"harness": "claude"})
	without := &plane.RunConfigState{Version: 1, Config: json.RawMessage(`{}`)}
	env := plannerEnv(with, without)
	checks := checkPlanner(context.Background(), c, env)
	if c0 := findCheck(checks, "planner[with-chat]"); c0 == nil || c0.status != pass {
		t.Fatalf("checks = %+v, want the configured project to PASS", checks)
	}
	if c0 := findCheck(checks, "planner[without-chat]"); c0 == nil || c0.status != pass ||
		!strings.Contains(c0.detail, "chat off") {
		t.Fatalf("checks = %+v, want the opted-out project reported as chat off", checks)
	}
	if c0 := findCheck(checks, "planner[demo]"); c0 != nil && c0.status == fail {
		t.Fatalf("aggregate FAIL with one usable planner: %+v", *c0)
	}
}

func TestDoctorPlannerUnreadableConfigWarnsAndDoesNotFail(t *testing.T) {
	c := plannerConfig(t)
	env := plannerEnv()
	env.newRCfgAPI = func(string, string) plane.RunConfigAPI {
		return rcFake{err: errors.New("plane down")}
	}
	checks := checkPlanner(context.Background(), c, env)
	if c0 := findCheck(checks, "planner[demo]"); c0 == nil || c0.status != warn {
		t.Fatalf("checks = %+v, want a WARN when the plane cannot be read", checks)
	}
	// Nothing could be verified, so the instance-level line must not pass in
	// silence: it says so as a WARN, never a refusal.
	if c0 := findCheck(checks, "planner"); c0 == nil || c0.status != warn {
		t.Fatalf("checks = %+v, want an aggregate WARN, never a FAIL, over unreadable configs", checks)
	}
}

// One project resolved with no planner, another unreadable: the unreadable one
// may carry the planner, so a transient plane error must not refuse the
// instance. Only a fully-resolved, fully-unconfigured set FAILs.
func TestDoctorPlannerMixedUnreadableDoesNotFailTheAggregate(t *testing.T) {
	c := plannerConfig(t, "no-chat", "unreadable")
	env := plannerEnv()
	calls := 0
	env.newRCfgAPI = func(string, string) plane.RunConfigAPI {
		calls++
		if calls == 2 {
			return rcFake{err: errors.New("plane down")}
		}
		return rcFake{state: &plane.RunConfigState{Version: 1, Config: json.RawMessage(`{}`)}}
	}
	checks := checkPlanner(context.Background(), c, env)
	if c0 := findCheck(checks, "planner[no-chat]"); c0 == nil || c0.status != pass {
		t.Fatalf("checks = %+v, want the resolved no-planner project reported", checks)
	}
	if c0 := findCheck(checks, "planner[unreadable]"); c0 == nil || c0.status != warn {
		t.Fatalf("checks = %+v, want the unreadable project at WARN", checks)
	}
	if c0 := findCheck(checks, "planner"); c0 == nil || c0.status != warn {
		t.Fatalf("checks = %+v, want an aggregate WARN, not the no-planner FAIL", checks)
	}
}
