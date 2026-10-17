package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The SHAPE of the permission patterns (CLA-441, re-fitted for 2.x in CLA-675).
//
// opencode 2.x makes its read/edit asks relative to the INSTANCE DIRECTORY,
// whether or not that directory is in a git checkout and wherever in the
// checkout it sits (verified against 2.0.32, see opencode_conformance_test.go).
// 1.x asked relative to the git worktree, or to "/" outside one, and the policy
// carried a layer for each; a policy still emitting those shapes denied a 2.x
// session's writes to its own workdir.
//
// These evaluate the emitted policy the way opencode does (opencodeEvaluate,
// the replica the README's "Verifying the policy against a live session" section
// sits beside).

// checkoutAt makes dir look like a git worktree root. kind "dir" is an ordinary
// clone; kind "file" is a LINKED WORKTREE, whose .git is a file. On 2.x neither
// changes the ask shape, which is what the tests using it pin.
func checkoutAt(t *testing.T, dir, kind string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	git := filepath.Join(dir, ".git")
	var err error
	if kind == "file" {
		err = os.WriteFile(git, []byte("gitdir: /elsewhere/.git/worktrees/x\n"), 0o644)
	} else {
		err = os.MkdirAll(git, 0o755)
	}
	if err != nil {
		t.Fatalf("making %s a %s checkout: %v", dir, kind, err)
	}
	return dir
}

// rootRelOf is the ask form a 1.x "/"-worktree session used: the absolute path
// minus its leading separator.
func rootRelOf(path string) string {
	return strings.TrimPrefix(filepath.ToSlash(path), "/")
}

// Every instance-directory shape asks the same way: plain relative paths are the
// working set, "../" escapes are denied.
func TestPolicyMatchesInstanceRelativeAsks(t *testing.T) {
	for name, mk := range map[string]func(t *testing.T) string{
		"plain directory": func(t *testing.T) string { return t.TempDir() },
		"checkout":        func(t *testing.T) string { return checkoutAt(t, filepath.Join(t.TempDir(), "repo"), "dir") },
		"linked worktree": func(t *testing.T) string { return checkoutAt(t, filepath.Join(t.TempDir(), "wt"), "file") },
		"checkout subdirectory": func(t *testing.T) string {
			repo := checkoutAt(t, filepath.Join(t.TempDir(), "repo"), "dir")
			sub := filepath.Join(repo, "services", "api")
			if err := os.MkdirAll(sub, 0o755); err != nil {
				t.Fatal(err)
			}
			return sub
		},
	} {
		t.Run(name, func(t *testing.T) {
			perm := opencodePermission(false, mk(t), nil)
			for _, ask := range []string{"AGENTS.md", "internal/harness/opencode.go", "w1.txt", "."} {
				if got := opencodeEvaluate(t, perm, "read", ask); got != "allow" {
					t.Errorf("read %q = %s, want allow", ask, got)
				}
				if got := opencodeEvaluate(t, perm, "edit", ask); got != "allow" {
					t.Errorf("edit %q = %s, want allow", ask, got)
				}
			}
			for _, ask := range []string{"../other-repo/secrets.txt", "../../etc/hosts"} {
				if got := opencodeEvaluate(t, perm, "read", ask); got != "deny" {
					t.Errorf("read %q = %s, want deny - an undeclared escape must stay closed", ask, got)
				}
				if got := opencodeEvaluate(t, perm, "edit", ask); got != "deny" {
					t.Errorf("edit %q = %s, want deny", ask, got)
				}
			}
			p := parsePolicy(t, perm)
			if p["*"] != "deny" {
				t.Errorf("* = %q, want deny", p["*"])
			}
			assertNetworkDenied(t, p)
		})
	}
}

// The 1.x root-relative form is gone: 2.x never asks in it, so an allow for it
// would only ever match a relative path that happens to spell it.
func TestPolicyNoLongerEmitsTheRootRelativeLayer(t *testing.T) {
	dir := t.TempDir()
	read := rules(parsePolicy(t, opencodePermission(false, dir, nil)), "read")
	for _, r := range read {
		if strings.HasPrefix(r, rootRelOf(dir)) {
			t.Errorf("read carries the 1.x root-relative rule %q", r)
		}
	}
}

// The extra-dir ask form is relative to the instance directory, so a session in
// a checkout's subdirectory reaches a sibling repo as ../../../repo-b.
func TestExtraDirFormIsRelativeToTheInstanceDirectory(t *testing.T) {
	parent := t.TempDir()
	repo := checkoutAt(t, filepath.Join(parent, "repo"), "dir")
	sub := filepath.Join(repo, "services", "api")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sibling := checkoutAt(t, filepath.Join(parent, "repo-b"), "dir")

	perm := opencodePermission(false, sub, []string{sibling})
	if got := opencodeEvaluate(t, perm, "edit", "../../../repo-b/main.go"); got != "allow" {
		t.Errorf("edit of the declared sibling = %s, want allow", got)
	}
	if got := opencodeEvaluate(t, perm, "edit", "../../../repo-c/main.go"); got != "deny" {
		t.Errorf("edit of an undeclared sibling = %s, want deny", got)
	}
	if !hasAbsAllow(rules(parsePolicy(t, perm), "external_directory"), sibling) {
		t.Error("the declared sibling has no external_directory allow")
	}
}

// An extra dir that CONTAINS the instance directory has no bounded ask form:
// filepath.Rel gives a pure dot-dot chain, and opencode's `*` spans "/", so
// "../**" means everything outside the tree rather than that directory. Two
// live layouts produce it - a declared parent of a submodule, and a linked
// worktree created under its own repo - and before the CLA-441 review both
// handed read/edit an escape.
func TestAnExtraDirAboveTheInstanceDirectoryCannotOpenAnEscape(t *testing.T) {
	for name, layout := range map[string]struct{ workdir, extra func(base string) string }{
		"declared parent of a submodule": {
			workdir: func(b string) string { return filepath.Join(b, "super", "sub") },
			extra:   func(b string) string { return filepath.Join(b, "super") },
		},
		"worktree created under its own repo": {
			workdir: func(b string) string { return filepath.Join(b, "repo", "worktrees", "task1") },
			extra:   func(b string) string { return filepath.Join(b, "repo") },
		},
	} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			extra := checkoutAt(t, layout.extra(base), "dir")
			workdir := checkoutAt(t, layout.workdir(base), "file")
			perm := opencodePermission(false, workdir, []string{extra})

			for _, ask := range []string{"../../etc/passwd", "../secret", "../../../Users/someone/.ssh/id_rsa"} {
				if got := opencodeEvaluate(t, perm, "read", ask); got != "deny" {
					t.Errorf("read %q = %s, want deny - an ancestor tree's dot-dot chain must not be emitted as a glob", ask, got)
				}
				if got := opencodeEvaluate(t, perm, "edit", ask); got != "deny" {
					t.Errorf("edit %q = %s, want deny", ask, got)
				}
			}
			// The external_directory gate stays open for the declared tree.
			if !hasAbsAllow(rules(parsePolicy(t, perm), "external_directory"), extra) {
				t.Errorf("external_directory has no absolute allow for %s", extra)
			}
		})
	}
}

// A workdir whose name sorts below "*" (0x2a) is not locked out: no rule in the
// policy depends on a directory name sorting after a deny.
func TestAWorkdirWhoseNameSortsLowIsNotLockedOut(t *testing.T) {
	sub := filepath.Join(t.TempDir(), "!important")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	perm := opencodePermission(false, sub, nil)
	if got := opencodeEvaluate(t, perm, "read", "main.go"); got != "allow" {
		t.Errorf("read = %s, want allow", got)
	}
	if got := opencodeEvaluate(t, perm, "read", "../AGENTS.md"); got != "deny" {
		t.Errorf("read = %s, want deny", got)
	}
}
