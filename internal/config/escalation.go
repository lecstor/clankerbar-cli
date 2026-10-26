// Escalation holds config-owned review-tier escalation rules (CLA-379).
// Two mechanical triggers, both operator-owned and both evaluated by the CLI
// at review-spawn time — the plane holds the rules; the CLI only evaluates the
// diff and reports which rule fired (operator decision 2026-08-23).
package config

import (
	"path/filepath"
	"strings"
)

// EscalationRule is one path-glob or category mapping.
type EscalationRule struct {
	// Match is either a glob pattern against changed paths (path trigger) or
	// a category value (task-declared risk trigger).
	Match string `json:"match"`

	// Tier is the tier bucket to escalate TO ("strong", etc.).
	Tier string `json:"tier"`
}

// Escalation is the per-project escalation block in CLI config.
// Both triggers are mechanical and operator-owned; no content-based
// classification, and escalation only ever raises (standard -> strong),
// never lowers. If both triggers fire, the result is the same single
// escalation (the tier is raised once).
type Escalation struct {
	// PathRules maps file-path globs to tier bucket names. Any changed path
	// that matches any glob escalates the review phase to that tier.
	PathRules map[string]string `json:"path_rules"`

	// CategoryRules maps task category values to tier bucket names. Any
	// claimed task whose category matches escalates its review phase.
	CategoryRules map[string]string `json:"category_rules"`
}

// Evaluate returns the escalated tier (if any) and which rule fired.
// It takes the changed file paths from the branch diff and the task's
// category. Only raises; never lowers; empty rules change nothing.
// matchGlob matches a file path against a glob that may contain "**" for
// recursive directory matching (any number of levels).
func matchGlob(glob, path string) bool {
	// If no **, use standard filepath.Match.
	if !strings.Contains(glob, "**") {
		m, _ := filepath.Match(glob, path)
		if m {
			return true
		}
		m2, _ := filepath.Match(glob, filepath.Base(path))
		return m2
	}
	// For patterns with **, split and match recursively.
	parts := strings.Split(glob, "/")
	pathParts := strings.Split(path, "/")
	return matchParts(parts, pathParts)
}

func matchParts(patParts, pathParts []string) bool {
	if len(patParts) == 0 {
		return len(pathParts) == 0
	}
	if patParts[0] == "**" {
		// ** matches zero or more path segments.
		for i := 0; i <= len(pathParts); i++ {
			if matchParts(patParts[1:], pathParts[i:]) {
				return true
			}
		}
		return false
	}
	if len(pathParts) == 0 {
		return false
	}
	m, _ := filepath.Match(patParts[0], pathParts[0])
	if !m && patParts[0] != pathParts[0] {
		return false
	}
	return matchParts(patParts[1:], pathParts[1:])
}

func (e Escalation) Evaluate(changedPaths []string, category string) (tier string, rule string) {
	if e.PathRules != nil {
		for glob, t := range e.PathRules {
			if t == "" {
				continue
			}
			for _, p := range changedPaths {
				if matchGlob(glob, p) {
					return t, "path: matched " + glob
				}
			}
		}
	}
	if e.CategoryRules != nil && category != "" {
		if t, ok := e.CategoryRules[category]; ok && t != "" {
			return t, "category: matched " + category
		}
	}
	return "", ""
}

// PathDiff evaluates escalation rules against changed file paths only.
func (e Escalation) PathDiff(changedPaths []string) (tier string, rule string) {
	return e.Evaluate(changedPaths, "")
}
