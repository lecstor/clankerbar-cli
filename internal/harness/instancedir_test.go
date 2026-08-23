package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLA-441, the third half: the SHAPE of the permission patterns.
//
// opencode makes its read/edit asks relative to the git worktree it resolves at
// the instance directory. The policy only ever emitted the root-relative form -
// correct for an instance directory outside any repo, and correct for nothing
// else. Once CLA-437 started each session in the task's checkout and the PWD pin
// made the instance directory actually be that checkout, every ask came back
// repo-relative ("AGENTS.md"), matched no allow, and fell to the `*` catch-all
// deny. Widening the grants' SCOPE could not fix it, which is why four tasks
// stalled against it: the shape was wrong, not the reach.
//
// These evaluate the emitted policy the way opencode does (opencodeEvaluate,
// the replica the README's "Verifying the policy against a live session" section
// sits beside), for both instance-dir shapes.

// checkoutAt makes dir look like what opencode resolves as a git worktree root.
// kind "dir" is an ordinary clone; kind "file" is a LINKED WORKTREE, whose .git
// is a file - the shape every per-task worktree in this fleet has, and the one a
// naive os.Stat-for-a-directory test would miss.
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

// rootRelOf is the ask form a "/"-worktree session uses: the absolute path minus
// its leading separator.
func rootRelOf(path string) string {
	return strings.TrimPrefix(filepath.ToSlash(path), "/")
}

// A session whose instance directory IS a checkout asks with repo-relative
// paths, and those asks have to resolve to allow inside the tree and deny
// outside it. Both checkout shapes, because a per-task worktree's .git is a file.
func TestPolicyMatchesTheAsksOfAnInCheckoutSession(t *testing.T) {
	for _, kind := range []string{"dir", "file"} {
		t.Run(kind, func(t *testing.T) {
			parent := t.TempDir()
			repo := checkoutAt(t, filepath.Join(parent, "repo"), kind)
			perm := opencodePermission(false, repo, nil)

			for _, ask := range []string{"AGENTS.md", "internal/harness/opencode.go", "docs/releases.md"} {
				if got := opencodeEvaluate(t, perm, "read", ask); got != "allow" {
					t.Errorf("read %q = %s, want allow - this is the form opencode asks with inside a checkout (CLA-441)", ask, got)
				}
				if got := opencodeEvaluate(t, perm, "edit", ask); got != "allow" {
					t.Errorf("edit %q = %s, want allow", ask, got)
				}
			}
			// Escapes still fail closed.
			for _, ask := range []string{"../other-repo/secrets.txt", "../../etc/hosts"} {
				if got := opencodeEvaluate(t, perm, "read", ask); got != "deny" {
					t.Errorf("read %q = %s, want deny - an undeclared escape must stay closed", ask, got)
				}
				if got := opencodeEvaluate(t, perm, "edit", ask); got != "deny" {
					t.Errorf("edit %q = %s, want deny", ask, got)
				}
			}
			// The root-relative layer survives beside it: one policy is correct
			// for whichever shape the session turns out to ask with, rather than
			// correct for whichever shape was in fashion.
			abs := rootRelOf(repo) + "/AGENTS.md"
			if got := opencodeEvaluate(t, perm, "read", abs); got != "allow" {
				t.Errorf("read %q = %s, want allow - the non-repo ask form must not be dropped when the in-checkout one is added", abs, got)
			}
			// And nothing about the rest of the posture moved.
			p := parsePolicy(t, perm)
			if p["*"] != "deny" {
				t.Errorf("* = %q, want deny", p["*"])
			}
			assertNetworkDenied(t, p)
			if got := opencodeEvaluate(t, perm, "read", "mcp:clankerbar:https://clankerbar.com/skills/clankerbar.md"); got != "allow" {
				t.Errorf("MCP resource read = %s, want allow (CLA-382/CLA-418 must survive this change)", got)
			}
		})
	}
}

// A session started in a SUBDIRECTORY of a checkout asks with paths relative to
// the worktree ROOT, which is above its own workdir. The subtree it was pointed
// at is allowed; the repo above it is not, so the workdir boundary is still a
// boundary rather than "the whole repo, because we are in one".
func TestPolicyScopesASubdirectoryOfACheckout(t *testing.T) {
	repo := checkoutAt(t, filepath.Join(t.TempDir(), "repo"), "dir")
	sub := filepath.Join(repo, "services", "api")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	perm := opencodePermission(false, sub, nil)

	for _, ask := range []string{"services/api/main.go", "services/api"} {
		if got := opencodeEvaluate(t, perm, "read", ask); got != "allow" {
			t.Errorf("read %q = %s, want allow - the workdir's own subtree, asked relative to the worktree root", ask, got)
		}
	}
	for _, ask := range []string{"AGENTS.md", "services/web/main.go", "../elsewhere"} {
		if got := opencodeEvaluate(t, perm, "read", ask); got != "deny" {
			t.Errorf("read %q = %s, want deny - outside the workdir, even though it is inside the same repo", ask, got)
		}
		if got := opencodeEvaluate(t, perm, "edit", ask); got != "deny" {
			t.Errorf("edit %q = %s, want deny", ask, got)
		}
	}
}

// The non-repo shape is unchanged, and that matters as much as the new one: the
// documented multi-repo-parent workdir (~/dev) asks with root-relative paths, and
// a plain relative ask there is not a thing the policy should have started
// allowing.
func TestPolicyKeepsTheNonRepoShape(t *testing.T) {
	// A real directory carrying no .git. That NONE of its ancestors carries one
	// either is an assumption about TMPDIR, not something this test can
	// establish - opencodeWorktreeRoot walks to "/" and there is no
	// GIT_CEILING_DIRECTORIES hook here. True on macOS and on CI; false for
	// anyone whose TMPDIR sits inside a checkout, where this test would report
	// the in-checkout shape and be right to (CLA-441 review).
	dir := t.TempDir()
	perm := opencodePermission(false, dir, nil)

	if got := opencodeEvaluate(t, perm, "read", rootRelOf(dir)+"/clankerbar-cli/AGENTS.md"); got != "allow" {
		t.Errorf("root-relative read = %s, want allow", got)
	}
	if got := opencodeEvaluate(t, perm, "read", "AGENTS.md"); got != "deny" {
		t.Errorf("bare relative read = %s, want deny - no worktree here, so opencode never asks in that form and allowing it would widen the policy for nothing", got)
	}
}

// The worktree opencode resolves is found by WALKING UP, because git does: a
// session in <repo>/internal is inside <repo>'s worktree and asks with
// "internal/..." patterns. Reading that directory as "not a repo" is what emits
// the wrong pattern layer.
func TestWorktreeRootWalksUp(t *testing.T) {
	repo := checkoutAt(t, filepath.Join(t.TempDir(), "repo"), "dir")
	deep := filepath.Join(repo, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got, ok := opencodeWorktreeRoot(deep)
	if !ok {
		t.Fatalf("opencodeWorktreeRoot(%s) found no worktree, want %s", deep, repo)
	}
	if resolved, err := filepath.EvalSymlinks(got); err != nil || resolved != mustEval(t, repo) {
		t.Errorf("worktree root = %q, want %q", got, repo)
	}
	if _, ok := opencodeWorktreeRoot(t.TempDir()); ok {
		t.Error("a directory with no .git anywhere above it must resolve to no worktree - that is opencode's worktree-\"/\" case")
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", p, err)
	}
	return r
}

// The asks in this table are TRANSCRIBED FROM A LIVE SESSION, not invented:
// ~/.local/share/opencode/log/opencode.log records one `evaluated
// permission=... pattern=... action.action=...` line per ask (the README's
// "Verifying the policy against a live session"), and on 2026-08-22/23 it held
// 291 read/edit asks in this shape, every one of them resolving
// `action.permission=* action.action=deny action.pattern=*` - the catch-all -
// against sessions carrying the root-relative policy. For example:
//
//	13:14:37.652Z run=e629637e permission=read pattern=docs/token-budget.md            action.action=deny action.pattern=*
//	16:26:06.222Z run=79136af6 permission=read pattern=apps/web/src/server/mcp/server.ts action.action=deny action.pattern=*
//	09:35:34.841Z run=9127d59d permission=read pattern=AGENTS.md                        action.action=deny action.pattern=*
//	08:44:49.128Z run=10c1674e permission=read pattern=internal/harness/harness.go      action.action=deny action.pattern=*
//
// The same log holds 2759 asks in the OTHER shape - pattern=Users/jason/dev/...
// matching action.pattern=Users/jason/dev/** with action.action=allow - from
// sessions whose instance directory was the multi-repo parent. Both shapes are
// real, live, and one policy has to serve them; that is what this asserts, with
// the failing halves' own patterns.
func TestTheAsksALiveSessionWasDeniedNowResolveAllow(t *testing.T) {
	repo := checkoutAt(t, filepath.Join(t.TempDir(), "repo"), "dir")
	perm := opencodePermission(false, repo, nil)
	for _, ask := range []string{
		"docs/token-budget.md",
		"apps/web/src/server/mcp/server.ts",
		"AGENTS.md",
		"internal/harness/harness.go",
	} {
		if got := opencodeEvaluate(t, perm, "read", ask); got != "allow" {
			t.Errorf("read %q = %s, want allow - this exact ask was logged falling to the catch-all deny in a live session", ask, got)
		}
	}

	// ...and the root-relative shape the same log shows being allowed keeps
	// being allowed, from the workdir that produced it.
	parent := opencodePermission(false, "/Users/jason/dev", nil)
	if got := opencodeEvaluate(t, parent, "read", "Users/jason/dev/clankerbar-worktrees/ea1f319a/apps/web/src/server/mcp/server.ts"); got != "allow" {
		t.Errorf("root-relative read = %s, want allow - transcribed from run=2065bd6d, which matched action.pattern=Users/jason/dev/**", got)
	}
}

// The extra-dir ask form is computed against the WORKTREE ROOT, not the
// workdir, and those differ exactly when the session starts below the top of
// its checkout. Without this case the two never met: the two-repo test spawns at
// a worktree root, and the subdirectory test declares no extra dirs - so
// reverting scopeExtraDirs to take the workdir left the suite green (CLA-441
// review).
func TestExtraDirFormIsRelativeToTheWorktreeNotTheWorkdir(t *testing.T) {
	parent := t.TempDir()
	repo := checkoutAt(t, filepath.Join(parent, "repo"), "dir")
	sub := filepath.Join(repo, "services", "api")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sibling := checkoutAt(t, filepath.Join(parent, "repo-b"), "dir")

	read := rules(parsePolicy(t, opencodePermission(false, sub, []string{sibling})), "read")
	if !has(read, "../repo-b/**:allow") {
		t.Errorf("read = %v; the sibling's ask form is path.relative(worktree, dir) = ../repo-b, not workdir-relative ../../../repo-b", read)
	}
	if has(read, "../../../repo-b/**:allow") {
		t.Errorf("read = %v; a workdir-relative form is a pattern no session ever asks with", read)
	}
}

// An extra dir that CONTAINS the worktree has no bounded ask form: filepath.Rel
// gives a pure dot-dot chain, and opencode's `*` spans "/", so "../**" means
// everything outside the tree rather than that directory. Two live layouts
// produce it - a declared parent of a submodule, and a linked worktree created
// under its own repo - and before the CLA-441 review both handed read/edit an
// escape: the first OVERWROTE the "../**" deny with an allow, the second sorted
// after it and beat it.
func TestAnExtraDirAboveTheWorktreeCannotOpenAnEscape(t *testing.T) {
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
			workdir := checkoutAt(t, layout.workdir(base), "file")
			extra := checkoutAt(t, layout.extra(base), "dir")
			perm := opencodePermission(false, workdir, []string{extra})

			for _, ask := range []string{"../../etc/passwd", "../secret", "../../../Users/someone/.ssh/id_rsa"} {
				if got := opencodeEvaluate(t, perm, "read", ask); got != "deny" {
					t.Errorf("read %q = %s, want deny - an ancestor tree's dot-dot chain must not be emitted as a glob", ask, got)
				}
				if got := opencodeEvaluate(t, perm, "edit", ask); got != "deny" {
					t.Errorf("edit %q = %s, want deny", ask, got)
				}
			}
			// The declared tree is still reachable, through the two forms that
			// CAN bound it - so this is a narrowing of the ask shapes, not a
			// withdrawal of the grant.
			p := parsePolicy(t, perm)
			if !hasRelFormAllow(rules(p, "read"), extra) {
				t.Errorf("read = %v; the declared tree's root-relative allow must survive", p["read"])
			}
			if !hasAbsAllow(rules(p, "external_directory"), extra) {
				t.Errorf("external_directory = %v; the declared tree's absolute allow must survive", p["external_directory"])
			}
		})
	}
}

// The subdirectory shape must not depend on a directory NAME sorting after a
// deny: ten byte values that can legally start a path component sort below "*"
// (0x2a), so a "**" deny beaten by the prefixed allow locks such a workdir out
// of its own tree. Nothing is denied there now - the policy's `*` catch-all
// already covers everything no rule allows (CLA-441 review).
func TestASubdirectoryWhoseNameSortsLowIsNotLockedOut(t *testing.T) {
	repo := checkoutAt(t, filepath.Join(t.TempDir(), "repo"), "dir")
	sub := filepath.Join(repo, "!important")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	perm := opencodePermission(false, sub, nil)

	if got := opencodeEvaluate(t, perm, "read", "!important/main.go"); got != "allow" {
		t.Errorf("read = %s, want allow - a session locked out of its own workdir is the exact failure this task exists to end", got)
	}
	if got := opencodeEvaluate(t, perm, "read", "AGENTS.md"); got != "deny" {
		t.Errorf("read = %s, want deny - the repo above the workdir is still out of scope", got)
	}
}
