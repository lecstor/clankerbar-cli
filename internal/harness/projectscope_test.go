package harness

// The project-root scope (CLA-678): under the per-project layout a session
// starts in <project root>/<repo name>, and the permission policy must reach
// the WHOLE project — every repo under the project root, and the worktrees
// directory every task worktree lives in — and nothing outside it. The
// boundary is proved by EVALUATING the emitted policy with the replica of
// opencode's own matcher (opencodeEvaluate), not by asserting pattern strings:
// a pattern list can look right and still resolve wrong when it is flattened
// in key order and judged last-match-wins.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpencodePermissionProjectRootScope(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "clankerbar")
	repo := filepath.Join(project, "clankerbar-cli")
	other := filepath.Join(project, "clankerbar")
	worktrees := filepath.Join(project, "worktrees")
	for _, dir := range []string{repo, other, worktrees} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// What the driver builds for a project-rooted config: the project root
	// itself (for the external_directory carve-out) plus every child of it —
	// the sibling repos and the worktrees directory.
	extra := []string{project, worktrees, other}

	type ask struct{ permission, pattern, want string }
	inScope := []ask{
		// The whole project is the working set: the session's own checkout,
		// a sibling repo, and a task worktree under the project root.
		{"read", "internal/harness/opencode.go", "allow"},
		{"read", ".", "allow"},
		{"read", "..", "allow"}, // the project root's own listing
		{"read", "../clankerbar/internal/loop/loop.go", "allow"},
		{"read", "../worktrees/aa0ec080/internal/supervisor/workdir.go", "allow"},
		// Nothing outside the project root: a sibling PROJECT is not in scope,
		// however close it sits on disk.
		{"read", "../other-project/clankerbar/internal/config/config.go", "deny"},
		{"read", "../../etc/hosts", "deny"},
		{"read", "../../../etc/passwd", "deny"},
		{"read", "../.config/clankerbar/opencode.json", "deny"},
		// external_directory is the gate a path-taking tool runs: the project
		// root's subtree is carved out, everything else stays closed —
		// including the MACHINE ROOT just above it and a sibling project beside
		// it.
		{"external_directory", filepath.Join(project, "worktrees") + "/*", "allow"},
		{"external_directory", filepath.Join(other, "internal") + "/*", "allow"},
		{"external_directory", root + "/*", "deny"},
		{"external_directory", filepath.Join(root, "makespdf", "makesPDF") + "/*", "deny"},
		{"external_directory", "/etc/*", "deny"},
		{"external_directory", "/Users/jason/.config/clankerbar/*", "deny"},
	}
	// Edits are only judged on a run allowed to write: a read-only run (probe,
	// connectivity smoke) denies edit outright.
	writes := []ask{
		{"edit", "internal/config/config.go", "allow"},
		{"edit", "../clankerbar/internal/config/config.go", "allow"},
		{"edit", "../worktrees/aa0ec080/scratch.txt", "allow"},
		{"edit", "../../../etc/passwd", "deny"},
		{"edit", "../other-project/clankerbar/x.go", "deny"},
	}

	for _, readOnly := range []bool{false, true} {
		perm := opencodePermission(readOnly, repo, extra)
		checked := append([]ask(nil), inScope...)
		if readOnly {
			checked = append(checked, []ask{
				{"edit", "../clankerbar/internal/config/config.go", "deny"},
				{"edit", "internal/harness/opencode.go", "deny"},
			}...)
		} else {
			checked = append(checked, writes...)
		}
		for _, tc := range checked {
			if got := opencodeEvaluate(t, perm, tc.permission, tc.pattern); got != tc.want {
				t.Errorf("readOnly=%v ask(permission=%s, pattern=%q) = %s, want %s", readOnly, tc.permission, tc.pattern, got, tc.want)
			}
		}
	}
}

// The scope is an ASSERTION about the layout, not an inference from it: without
// it — a session in one of a developer's `~/dev` checkouts, whose parent holds
// ~150 unrelated trees — nothing beside the session's own checkout is granted.
func TestOpencodePermissionWithoutProjectRootStaysInItsCheckout(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "dev")
	repo := filepath.Join(parent, "clankerbar-cli")
	sibling := filepath.Join(parent, "makesPDF")
	for _, dir := range []string{repo, sibling} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	perm := opencodePermission(false, repo, nil)
	if got := opencodeEvaluate(t, perm, "read", "internal/harness/opencode.go"); got != "allow" {
		t.Errorf("the session's own tree must stay readable, got %s", got)
	}
	if got := opencodeEvaluate(t, perm, "read", "../makesPDF/src/main.go"); got != "deny" {
		t.Errorf("a sibling checkout outside a declared project scope = %s, want deny", got)
	}
	if got := opencodeEvaluate(t, perm, "external_directory", sibling+"/*"); got != "deny" {
		t.Errorf("a sibling checkout outside a declared project scope = %s, want deny", got)
	}
}
