package config

import "testing"

func TestEscalationEmpty(t *testing.T) {
	e := Escalation{}
	tier, rule := e.Evaluate([]string{"src/lib/auth.go"}, "security")
	if tier != "" || rule != "" {
		t.Errorf("empty escalation must return nothing, got tier=%q rule=%q", tier, rule)
	}
}

func TestEscalationPathMatch(t *testing.T) {
	e := Escalation{
		PathRules: map[string]string{"drizzle/**": "strong", "docs/architecture.md": "strong"},
	}
	tests := []struct {
		path     string
		wantTier string
		wantRule string
	}{
		{"drizzle/meta/0012_foo.sql", "strong", "path: matched drizzle/**"},
		{"packages/web/db/migration.ts", "", ""},
		{"docs/architecture.md", "strong", "path: matched docs/architecture.md"},
		{"src/lib/auth.ts", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			tier, rule := e.PathDiff([]string{tc.path})
			if tier != tc.wantTier || rule != tc.wantRule {
				t.Errorf("Evaluate([%q]) = (%q, %q), want (%q, %q)", tc.path, tier, rule, tc.wantTier, tc.wantRule)
			}
		})
	}
}

func TestEscalationCategoryMatch(t *testing.T) {
	e := Escalation{
		CategoryRules: map[string]string{"security": "strong", "migration": "strong"},
	}
	tier, rule := e.Evaluate([]string{}, "security")
	if tier != "strong" || rule != "category: matched security" {
		t.Errorf("category match failed: got (%q, %q), want (strong, category: matched security)", tier, rule)
	}
	tier, rule = e.Evaluate([]string{}, "feature")
	if tier != "" || rule != "" {
		t.Errorf("unknown category must return nothing, got (%q, %q)", tier, rule)
	}
}

func TestEscalationOnlyRaises(t *testing.T) {
	// Even if the current phase is configured as "standard", escalation
	// can only move it to a stronger tier ("strong" here). There is no
	// mechanism to lower from a configured tier.
	e := Escalation{
		PathRules: map[string]string{"**": "strong"},
	}
	tier, _ := e.PathDiff([]string{"anything.ts"})
	if tier != "strong" {
		t.Errorf("escalation should only raise to strong, got %q", tier)
	}
}
