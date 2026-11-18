package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The planner role plumbing (studio-chat.md §1/§2): the local `role` key, the
// stored `roles.planner` block, and ResolvePlanner's strict no-fallback chain.

func TestValidateRoleAcceptsWorkerAndPlannerOnly(t *testing.T) {
	for _, role := range []string{"", "worker", "planner", " planner "} {
		c := defaults()
		c.Role = role
		if err := c.Validate(); err != nil {
			t.Errorf("Validate with role %q = %v, want nil", role, err)
		}
	}
	c := defaults()
	c.Role = "triage"
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "unknown role") {
		t.Fatalf("Validate with role triage = %v, want an unknown-role refusal", err)
	}
}

func TestEffectiveRoleDefaultsToWorker(t *testing.T) {
	c := defaults()
	if got := c.EffectiveRole(); got != RoleWorker {
		t.Errorf("EffectiveRole() = %q, want %q", got, RoleWorker)
	}
	c.Role = RolePlanner
	if got := c.EffectiveRole(); got != RolePlanner {
		t.Errorf("EffectiveRole() = %q, want %q", got, RolePlanner)
	}
}

func TestResolvePlannerWithoutRole(t *testing.T) {
	c := defaults()
	if c.PlannerEnabled() {
		t.Fatal("PlannerEnabled() = true with no stored role")
	}
	if _, err := c.ResolvePlanner(); err != ErrNoPlanner {
		t.Fatalf("ResolvePlanner() error = %v, want ErrNoPlanner", err)
	}
}

func TestResolvePlannerRefusesBadAssignments(t *testing.T) {
	cases := []struct {
		name string
		role RoleAssignment
		want string
	}{
		{"no harness", RoleAssignment{}, "names no harness"},
		{"unknown harness", RoleAssignment{Harness: "chatgpt"}, "does not know"},
		{"model and tier", RoleAssignment{Harness: "claude", Model: "opus", Tier: "strong"}, "both `model`"},
		{"tier resolves to nothing", RoleAssignment{Harness: "claude", Tier: "mystery"}, "resolves to no model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := defaults()
			c.Models = map[string]string{"strong": "opus"}
			role := tc.role
			c.Planner = &role
			_, err := c.ResolvePlanner()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ResolvePlanner() = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestResolvePlannerResolvesTiersOnTheRolesOwnHarness(t *testing.T) {
	c := defaults() // top-level harness: claude
	c.Harnesses = map[string]HarnessConfig{
		"opencode": {Models: map[string]ModelRoutes{"fast": {"free-model"}}},
	}
	c.Models = map[string]string{"fast": "claude-haiku"} // the worker's map must not be consulted
	c.Planner = &RoleAssignment{Harness: "opencode", Tier: "fast"}
	sess, err := c.ResolvePlanner()
	if err != nil {
		t.Fatalf("ResolvePlanner: %v", err)
	}
	if sess.Harness != "opencode" || sess.Model != "free-model" {
		t.Errorf("resolved %s/%s, want opencode/free-model", sess.Harness, sess.Model)
	}
}

func TestResolvePlannerDoesNotFallBackToTheWorkerModel(t *testing.T) {
	c := defaults()
	c.Model = "opus" // the worker's local model
	c.Planner = &RoleAssignment{Harness: "opencode"}
	sess, err := c.ResolvePlanner()
	if err != nil {
		t.Fatalf("ResolvePlanner: %v", err)
	}
	if sess.Model != "" {
		t.Errorf("Model = %q, want empty (the harness's own default, never the worker's alias)", sess.Model)
	}
}

func TestResolvePlannerBoundsFallbacks(t *testing.T) {
	c := defaults()
	wall := Duration(45 * time.Minute)
	c.MaxTurns = 77
	c.Budget.MaxWallClock = wall
	c.Budget.MaxSessionTokens = 1234
	c.Planner = &RoleAssignment{Harness: "claude"}
	sess, err := c.ResolvePlanner()
	if err != nil {
		t.Fatalf("ResolvePlanner: %v", err)
	}
	if sess.MaxTurns != 77 {
		t.Errorf("MaxTurns = %d, want the run-wide 77", sess.MaxTurns)
	}
	if sess.MaxWallClock != wall.Duration() {
		t.Errorf("MaxWallClock = %v, want the budget's %v", sess.MaxWallClock, wall.Duration())
	}
	if sess.MaxSessionTokens != 1234 {
		t.Errorf("MaxSessionTokens = %d, want the budget's 1234", sess.MaxSessionTokens)
	}

	// The role's own values win, and an unset run falls to the shipped defaults.
	c.Planner = &RoleAssignment{Harness: "claude", MaxTurns: 12, MaxWallClock: Duration(90 * time.Second), MaxSessionTokens: 400000}
	sess, err = c.ResolvePlanner()
	if err != nil {
		t.Fatalf("ResolvePlanner: %v", err)
	}
	if sess.MaxTurns != 12 || sess.MaxWallClock != 90*time.Second || sess.MaxSessionTokens != 400000 {
		t.Errorf("resolved bounds = %d / %v / %d, want the role's 12 / 90s / 400000",
			sess.MaxTurns, sess.MaxWallClock, sess.MaxSessionTokens)
	}

	empty := defaults()
	empty.Planner = &RoleAssignment{Harness: "claude"}
	sess, err = empty.ResolvePlanner()
	if err != nil {
		t.Fatalf("ResolvePlanner: %v", err)
	}
	if sess.MaxTurns != DefaultMaxTurns {
		t.Errorf("MaxTurns = %d, want DefaultMaxTurns %d", sess.MaxTurns, DefaultMaxTurns)
	}
	if sess.MaxWallClock != DefaultPlannerWallClock {
		t.Errorf("MaxWallClock = %v, want %v (the plane derives its lease from it)", sess.MaxWallClock, DefaultPlannerWallClock)
	}
	if sess.MaxSessionTokens != 0 {
		t.Errorf("MaxSessionTokens = %d, want 0 (uncapped)", sess.MaxSessionTokens)
	}
	if sess.Prompt != BuiltinPlannerPrompt {
		t.Error("Prompt is not the built-in planner brief")
	}
}

func TestResolvePlannerPromptOverride(t *testing.T) {
	c := defaults()
	c.Planner = &RoleAssignment{Harness: "claude", Prompt: "  You are Acme's planner.  "}
	sess, err := c.ResolvePlanner()
	if err != nil {
		t.Fatalf("ResolvePlanner: %v", err)
	}
	if sess.Prompt != "You are Acme's planner." {
		t.Errorf("Prompt = %q, want the stored brief trimmed", sess.Prompt)
	}
}

func TestApplyRunConfigCarriesRoles(t *testing.T) {
	var doc RunConfigDoc
	body := `{"roles":{"planner":{"harness":"opencode","model":"free-model","max_wall_clock":600,"max_session_tokens":500000,"max_turns":9}}}`
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.Empty() {
		t.Fatal("a document carrying roles decoded as Empty")
	}
	c := defaults()
	if err := c.ApplyRunConfig(&doc); err != nil {
		t.Fatalf("ApplyRunConfig: %v", err)
	}
	if !c.PlannerEnabled() {
		t.Fatal("PlannerEnabled() = false after a document carrying roles.planner")
	}
	if c.Planner.Harness != "opencode" || c.Planner.Model != "free-model" ||
		c.Planner.MaxWallClock != Duration(600*time.Second) || c.Planner.MaxSessionTokens != 500000 || c.Planner.MaxTurns != 9 {
		t.Errorf("applied role = %+v, want the document's block", *c.Planner)
	}

	// `roles: {}` is the explicit opt-out: chat off.
	var emptyRoles RunConfigDoc
	if err := json.Unmarshal([]byte(`{"roles":{}}`), &emptyRoles); err != nil {
		t.Fatalf("decode empty roles: %v", err)
	}
	if emptyRoles.Roles == nil {
		t.Fatal("roles: {} decoded as an absent key; the section's opt-out is not representable")
	}
	if emptyRoles.Empty() {
		t.Fatal("roles: {} must count as a statement, not as an empty document")
	}
	c2 := defaults()
	c2.Planner = &RoleAssignment{Harness: "claude"}
	if err := c2.ApplyRunConfig(&emptyRoles); err != nil {
		t.Fatalf("ApplyRunConfig: %v", err)
	}
	if c2.PlannerEnabled() {
		t.Error("roles: {} left a planner enabled; it is the explicit opt-out")
	}

	// An absent key resets a config a previous overlay had turned on.
	c3 := defaults()
	c3.Planner = &RoleAssignment{Harness: "claude"}
	var none RunConfigDoc
	if err := none.UnmarshalJSON([]byte(`{"model":"opus"}`)); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := c3.ApplyRunConfig(&none); err != nil {
		t.Fatalf("ApplyRunConfig: %v", err)
	}
	if c3.PlannerEnabled() {
		t.Error("a document with no roles key left a stale planner in force")
	}
}

func TestCloneCopiesThePlannerRole(t *testing.T) {
	c := defaults()
	c.Planner = &RoleAssignment{Harness: "claude", Model: "opus"}
	cp := c.Clone()
	cp.Planner.Model = "haiku"
	if c.Planner.Model != "opus" {
		t.Errorf("base planner mutated through the clone: %q", c.Planner.Model)
	}
}

func TestPlannerRoleIsNotDecodedFromTheLocalFile(t *testing.T) {
	b, err := json.Marshal(&Config{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "planner") {
		t.Errorf("Config marshals an effective planner role into the local-file shape: %s", b)
	}
}
