package supervisor

// The workdir derivation's fail-closed branches (CLA-547, phase 2a of
// docs/proposals/daemon-supervisor.md; the per-project root and the clone from
// CLA-678): the derived path is <root>/<project slug>/<repo name> with the
// project's worktrees under <root>/<project slug>/worktrees, a MISSING repo
// directory is cloned into place, and a directory that exists but is not a
// checkout of the expected repo is a refusal naming the path tried. The tests
// build real git checkouts and real bare repositories so both branches are
// verified against actual remotes and a real clone, not mocks.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// gitRun runs git in dir, failing the test on any error.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// makeCheckout creates a real git checkout at dir whose origin remote names
// repo ("owner/name").
func makeCheckout(t *testing.T, dir, repo string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "init", "-b", "main")
	gitRun(t, dir, "remote", "add", "origin", "https://github.com/"+repo+".git")
}

// makeBareRemote creates a bare repository at <base>/<owner>/<name>.git and
// returns base, so a test can point cloneRemoteBase at it and exercise a real
// clone without touching the network.
func makeBareRemote(t *testing.T, repo string) string {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, repo+".git")
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, base, "init", "--bare", dir)
	return base
}

// pointClonesAt makes every clone in this test read from base instead of the
// network. Not parallel-safe: cloneRemoteBase is process-wide, like the rest of
// this package's overridable externals.
func pointClonesAt(t *testing.T, base string) {
	t.Helper()
	prev := cloneRemoteBase
	cloneRemoteBase = "file://" + base + "/"
	t.Cleanup(func() { cloneRemoteBase = prev })
}

// The happy path: <root>/<project>/<repo name> resolves to the project's own
// clone when it exists and is a checkout of the expected repo, in both the
// https and ssh remote spellings, and for a bare-name identity.
func TestDeriveWorkdirResolvesProjectRootAndRepo(t *testing.T) {
	root := t.TempDir()
	pr := filepath.Join(root, "clankerbar")
	makeCheckout(t, filepath.Join(pr, "clankerbar"), "lecstor/clankerbar")

	got, err := DeriveWorkdir(context.Background(), root, "clankerbar", "lecstor/clankerbar")
	if err != nil {
		t.Fatalf("DeriveWorkdir: %v", err)
	}
	if want := filepath.Join(pr, "clankerbar"); got != want {
		t.Fatalf("DeriveWorkdir = %q, want %q", got, want)
	}

	// The ssh spelling of the same remote must match too.
	gitRun(t, got, "remote", "set-url", "origin", "git@github.com:lecstor/clankerbar.git")
	if _, err := DeriveWorkdir(context.Background(), root, "clankerbar", "lecstor/clankerbar"); err != nil {
		t.Fatalf("DeriveWorkdir with an ssh origin: %v", err)
	}

	// A trailing slash on the identity is tolerated.
	if _, err := DeriveWorkdir(context.Background(), root, "clankerbar", "lecstor/clankerbar/"); err != nil {
		t.Fatalf("DeriveWorkdir with a trailing slash: %v", err)
	}

	// A bare-name identity matches whatever owner hosts the checkout — the
	// same owner-agnostic match the config's own resolution applies.
	if _, err := DeriveWorkdir(context.Background(), root, "clankerbar", "clankerbar"); err != nil {
		t.Fatalf("DeriveWorkdir with a bare-name identity: %v", err)
	}

	// Two projects sharing one root never see each other's repos: the same repo
	// name under a different slug is a different directory.
	makeCheckout(t, filepath.Join(root, "other-project", "clankerbar"), "lecstor/clankerbar")
	other, err := DeriveWorkdir(context.Background(), root, "other-project", "lecstor/clankerbar")
	if err != nil {
		t.Fatalf("DeriveWorkdir for a second project: %v", err)
	}
	if want := filepath.Join(root, "other-project", "clankerbar"); other != want {
		t.Fatalf("DeriveWorkdir = %q, want %q (a project's repos are its own)", other, want)
	}
	if other == got {
		t.Fatal("two projects resolved to one directory — a project's repos are its own")
	}
}

// A MISSING repo directory is materialized by cloning it (CLA-678) — the fleet
// works in its own clones, so nobody has to check anything out for it first.
func TestDeriveWorkdirClonesAMissingRepo(t *testing.T) {
	base := makeBareRemote(t, "acme/widgets")
	pointClonesAt(t, base)
	root := t.TempDir()

	got, err := DeriveWorkdir(context.Background(), root, "acme", "acme/widgets")
	if err != nil {
		t.Fatalf("DeriveWorkdir of a missing repo: %v", err)
	}
	if want := filepath.Join(root, "acme", "widgets"); got != want {
		t.Fatalf("DeriveWorkdir = %q, want %q", got, want)
	}
	remote, err := checkoutOriginRemote(got)
	if err != nil {
		t.Fatalf("the clone is not an attributable checkout: %v", err)
	}
	if !strings.Contains(remote, "acme/widgets") {
		t.Fatalf("the clone's origin is %q, want it to name acme/widgets", remote)
	}

	// A second derivation is a no-op: the clone exists, so nothing is re-cloned
	// (which would destroy whatever the sessions have done in it).
	marker := filepath.Join(got, "session-work")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := DeriveWorkdir(context.Background(), root, "acme", "acme/widgets"); err != nil {
		t.Fatalf("second DeriveWorkdir: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the existing clone was replaced by a second derivation: %v", err)
	}
}

// A clone that cannot run leaves NO directory behind: a partial clone sitting
// at the derived path would be indistinguishable from the developer's own tree,
// which the next reconcile must refuse.
func TestDeriveWorkdirFailedCloneLeavesNothing(t *testing.T) {
	pointClonesAt(t, t.TempDir()) // a base with no such repo
	root := t.TempDir()

	_, err := DeriveWorkdir(context.Background(), root, "acme", "acme/ghost")
	if err == nil {
		t.Fatal("DeriveWorkdir cloned a repo that does not exist")
	}
	if !errors.Is(err, ErrWorkdirRefused) {
		t.Fatalf("err = %v, want the ErrWorkdirRefused sentinel", err)
	}
	if !strings.Contains(err.Error(), filepath.Join(root, "acme", "ghost")) {
		t.Errorf("err %q does not name the path tried", err)
	}
	entries, readErr := os.ReadDir(filepath.Join(root, "acme"))
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	for _, e := range entries {
		t.Errorf("a failed clone left %s behind", e.Name())
	}
}

// Fail-closed: a derived directory that EXISTS but is not a checkout of the
// expected repo refuses — and is never replaced. A plain directory, a checkout
// of a different repo, a repo with no attributable remote: all refuse, naming
// the path tried.
func TestDeriveWorkdirRefusesNonMatchingCheckout(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	// A plain directory with no .git at all.
	plain := filepath.Join(root, "acme", "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := DeriveWorkdir(ctx, root, "acme", "acme/plain"); err == nil {
		t.Fatal("DeriveWorkdir accepted a plain directory as a checkout")
	}

	// A real checkout of a DIFFERENT repo: the derived path exists, but it is
	// not the repo the project names. Same repo name, different owner — the
	// derived path is identical, the attribution is not. It must be refused and
	// left exactly as it was.
	makeCheckout(t, filepath.Join(root, "acme", "widgets"), "acme/widgets")
	_, err := DeriveWorkdir(ctx, root, "acme", "other-owner/widgets")
	if err == nil {
		t.Fatal("DeriveWorkdir accepted a checkout of the wrong repo")
	}
	if !strings.Contains(err.Error(), filepath.Join(root, "acme", "widgets")) {
		t.Errorf("err %q does not name the path tried %q", err, filepath.Join(root, "acme", "widgets"))
	}
	if !strings.Contains(err.Error(), "other-owner/widgets") {
		t.Errorf("err %q does not name the expected repo", err)
	}
	if !strings.Contains(err.Error(), "acme/widgets") {
		t.Errorf("err %q does not name what the origin remote actually is", err)
	}

	// The boundary pair this codebase exists to tell apart: `clankerbar` must
	// never match `clankerbar-cli` (the project's own repo pair, CLA-351). The
	// derived path for lecstor/clankerbar IS <root>/acme/clankerbar, so the
	// tree that sits there has to be materialized as the other repo of the pair.
	makeCheckout(t, filepath.Join(root, "acme", "clankerbar"), "lecstor/clankerbar-cli")
	if _, err := DeriveWorkdir(ctx, root, "acme", "lecstor/clankerbar"); err == nil {
		t.Fatal("DeriveWorkdir matched lecstor/clankerbar against a clankerbar-cli checkout — the boundary rule is missing")
	}
	if _, err := os.Stat(filepath.Join(root, "acme", "clankerbar", ".git")); err != nil {
		t.Fatalf("the refused checkout was removed: %v", err)
	}

	// A git repository with NO remotes cannot be attributed to any repo.
	noRemote := filepath.Join(root, "acme", "noremote")
	if err := os.MkdirAll(noRemote, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, noRemote, "init", "-b", "main")
	if _, err := DeriveWorkdir(ctx, root, "acme", "acme/noremote"); err == nil {
		t.Fatal("DeriveWorkdir accepted a checkout with no remotes")
	}

	// Several remotes and no origin cannot be attributed either — refuse
	// rather than guess.
	twoRemotes := filepath.Join(root, "acme", "tworemotes")
	if err := os.MkdirAll(twoRemotes, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, twoRemotes, "init", "-b", "main")
	gitRun(t, twoRemotes, "remote", "add", "upstream", "https://github.com/acme/tworemotes.git")
	gitRun(t, twoRemotes, "remote", "add", "fork", "https://github.com/other/tworemotes.git")
	if _, err := DeriveWorkdir(ctx, root, "acme", "acme/tworemotes"); err == nil {
		t.Fatal("DeriveWorkdir accepted a checkout with two remotes and no origin")
	}

	// The one attribution fallback: a checkout whose ONLY remote is not named
	// origin is still attributable — the single remote IS the repo.
	onlyUpstream := filepath.Join(root, "acme", "onlyupstream")
	if err := os.MkdirAll(onlyUpstream, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, onlyUpstream, "init", "-b", "main")
	gitRun(t, onlyUpstream, "remote", "add", "upstream", "https://github.com/acme/onlyupstream.git")
	if _, err := DeriveWorkdir(ctx, root, "acme", "acme/onlyupstream"); err != nil {
		t.Fatalf("DeriveWorkdir refused a checkout whose only remote names the repo: %v", err)
	}

	// A FILE where the clone belongs is refused, not replaced: it is somebody's
	// file, not a failed clone of ours.
	filePath := filepath.Join(root, "acme", "file-repo")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := DeriveWorkdir(ctx, root, "acme", "acme/file-repo"); err == nil {
		t.Fatal("DeriveWorkdir accepted a file where the repo belongs")
	}
	if _, statErr := os.Stat(filePath); statErr != nil {
		t.Fatalf("DeriveWorkdir removed a file it did not create: %v", statErr)
	}
}

// Missing inputs, and a slug that is not one path segment, are refusals: there
// is either nothing to derive from, or a path that would escape the root.
func TestDeriveWorkdirRefusesMissingInputsAndBadSlugs(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	if _, err := DeriveWorkdir(ctx, "", "acme", "acme/widgets"); err == nil {
		t.Fatal("DeriveWorkdir with no root must refuse")
	}
	if _, err := DeriveWorkdir(ctx, root, "acme", ""); err == nil {
		t.Fatal("DeriveWorkdir with no repo must refuse")
	}
	for _, slug := range []string{"..", ".", "a/b", ""} {
		if _, err := DeriveWorkdir(ctx, root, slug, "acme/widgets"); err == nil {
			t.Errorf("DeriveWorkdir accepted project slug %q", slug)
		}
	}
	// A repo identity with no owner cannot be cloned from, so it cannot be
	// derived: there is no clone URL to make.
	if _, err := DeriveWorkdir(ctx, root, "acme", "widgets"); err == nil {
		t.Fatal("DeriveWorkdir cloned from a bare-name identity — there is no owner to clone from")
	}
}

// The supervisor's own working directory is never part of the derivation:
// a failed derivation is an error, and a successful one is the derived path —
// there is no branch that returns the cwd.
func TestDeriveWorkdirNeverFallsBackToTheSupervisorCwd(t *testing.T) {
	// The supervisor sits in cwd; the root is a DIFFERENT directory.
	cwd := t.TempDir()
	t.Chdir(cwd)
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()

	// The root's checkout is the derived path, never the cwd.
	makeCheckout(t, filepath.Join(root, "acme", "widgets"), "acme/widgets")
	got, err := DeriveWorkdir(context.Background(), root, "acme", "acme/widgets")
	if err != nil {
		t.Fatalf("DeriveWorkdir: %v", err)
	}
	if got == cwd || strings.HasPrefix(got, cwd+string(os.PathSeparator)) {
		t.Fatalf("DeriveWorkdir returned %q — the supervisor's cwd %q must never be a result", got, cwd)
	}

	// A ~ root expands like every other path this config accepts.
	makeCheckout(t, filepath.Join(t.TempDir(), "tilde", "acme", "tilde"), "acme/tilde")
	if _, err := DeriveWorkdir(context.Background(), "~", "acme", "acme/tilde"); err == nil {
		t.Fatal("DeriveWorkdir('~', 'acme') resolved — ~ must expand to the home dir, not match a temp checkout")
	}
}

// The worktree directory is the fleet's own, so unlike a repo clone it IS
// created — and only ever inside the project root.
func TestDeriveWorktreesDir(t *testing.T) {
	root := t.TempDir()
	dir, err := DeriveWorktreesDir(root, "acme")
	if err != nil {
		t.Fatalf("DeriveWorktreesDir: %v", err)
	}
	if want := filepath.Join(root, "acme", WorktreesDirName); dir != want {
		t.Fatalf("DeriveWorktreesDir = %q, want %q", dir, want)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("the worktree directory was not created: %v", err)
	}

	// It is inside the project root, which is what makes the project root a
	// valid permission scope for every worktree a session makes here.
	if _, err := DeriveWorktreesDir(root, ".."); err == nil {
		t.Fatal("DeriveWorktreesDir accepted a slug that escapes the root")
	}
}

// The per-instance seam derives one PROJECT ROOT per project, materializes that
// project's repos and its worktree directory under it, and refuses the whole
// instance when any project's derivation fails.
func TestDeriveInstanceWorkdirsPerProject(t *testing.T) {
	ctx := context.Background()
	base := makeBareRemote(t, "acme/widgets")
	gitRun(t, base, "init", "--bare", filepath.Join(base, "acme", "gadgets.git"))
	pointClonesAt(t, base)

	root := t.TempDir()
	t.Run("single project takes its slug from the MCP endpoint", func(t *testing.T) {
		cfg := &config.Config{
			BacklogURL:  "https://clankerbar.com/mcp/acme",
			PrimaryRepo: "acme/widgets",
		}
		got, err := deriveInstanceWorkdirs(ctx, cfg, root, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got["acme"] != filepath.Join(root, "acme") {
			t.Fatalf("deriveInstanceWorkdirs = %v, want {\"acme\": %q}", got, filepath.Join(root, "acme"))
		}
		if _, err := os.Stat(filepath.Join(root, "acme", "widgets", ".git")); err != nil {
			t.Fatalf("the project's repo was not materialized: %v", err)
		}
		if fi, err := os.Stat(filepath.Join(root, "acme", WorktreesDirName)); err != nil || !fi.IsDir() {
			t.Fatalf("the project's worktree directory was not created: %v", err)
		}
	})

	t.Run("a sole repos key is primary implicitly", func(t *testing.T) {
		cfg := &config.Config{
			BacklogURL: "https://clankerbar.com/mcp/acme",
			Repos:      map[string]string{"acme/widgets": "widgets"},
		}
		got, err := deriveInstanceWorkdirs(ctx, cfg, root, "")
		if err != nil {
			t.Fatal(err)
		}
		if got["acme"] != filepath.Join(root, "acme") {
			t.Fatalf("deriveInstanceWorkdirs = %v, want the sole repos key's project root", got)
		}
	})

	t.Run("multi-project derives per project and clones every declared repo", func(t *testing.T) {
		cfg := &config.Config{Projects: []config.Project{
			{Slug: "one", PrimaryRepo: "acme/widgets", Repos: map[string]string{"acme/gadgets": "gadgets"}},
			{Slug: "two", PrimaryRepo: "acme/gadgets"},
		}}
		got, err := deriveInstanceWorkdirs(ctx, cfg, root, "")
		if err != nil {
			t.Fatal(err)
		}
		if got["one"] != filepath.Join(root, "one") || got["two"] != filepath.Join(root, "two") {
			t.Fatalf("deriveInstanceWorkdirs = %v, want one and two as project roots", got)
		}
		// Both of project one's repos, and not project two's, sit under its root.
		for _, repo := range []string{"widgets", "gadgets"} {
			if _, err := os.Stat(filepath.Join(root, "one", repo, ".git")); err != nil {
				t.Errorf("project one is missing its %s clone: %v", repo, err)
			}
		}
	})

	t.Run("one bad project refuses the instance", func(t *testing.T) {
		cfg := &config.Config{Projects: []config.Project{
			{Slug: "one", PrimaryRepo: "acme/widgets"},
			{Slug: "two", PrimaryRepo: "acme/ghost"},
		}}
		_, err := deriveInstanceWorkdirs(ctx, cfg, root, "")
		if err == nil {
			t.Fatal("deriveInstanceWorkdirs succeeded with a project whose repo cannot be cloned")
		}
		if !strings.Contains(err.Error(), "project two") || !strings.Contains(err.Error(), filepath.Join(root, "two", "ghost")) {
			t.Fatalf("err %q does not name the failing project and the path tried", err)
		}
	})

	t.Run("a project naming no repo is a refusal, not a skip", func(t *testing.T) {
		cfg := &config.Config{BacklogURL: "https://clankerbar.com/mcp/acme", PrimaryRepo: ""}
		_, err := deriveInstanceWorkdirs(ctx, cfg, root, "")
		if err == nil {
			t.Fatal("deriveInstanceWorkdirs succeeded with no repo declared")
		}
		if !strings.Contains(err.Error(), "primary_repo") {
			t.Fatalf("err %q does not tell the operator what to declare", err)
		}
	})

	t.Run("a config naming no project slug is a refusal", func(t *testing.T) {
		cfg := &config.Config{PrimaryRepo: "acme/widgets"}
		if _, err := deriveInstanceWorkdirs(ctx, cfg, root, ""); err == nil {
			t.Fatal("deriveInstanceWorkdirs succeeded with no project slug to root the layout on")
		}
	})
}

// A SYMLINK at the derived repo path is refused, in both of the shapes the
// project-root layout makes dangerous (CLA-678 review MUST 1.1 / 2.1):
//
//   - a link to a matching checkout OUTSIDE the project root would pass the
//     origin check and be handed to sessions as the fleet's clone, so the
//     session's project-root scope would reach outside the project and the
//     fleet would read and edit a developer's own tree;
//   - a link to anything else is the same escape with a wrong origin.
//
// Neither is adopted, and neither target is touched.
func TestDeriveWorkdirRefusesASymlinkedRepo(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	pr := filepath.Join(root, "acme")
	if err := os.MkdirAll(pr, 0o755); err != nil {
		t.Fatal(err)
	}

	// A developer's checkout outside the project root, with the EXPECTED origin:
	// this is the shape that used to verify and be returned.
	outside := filepath.Join(t.TempDir(), "widgets")
	makeCheckout(t, outside, "acme/widgets")
	if err := os.Symlink(outside, filepath.Join(pr, "widgets")); err != nil {
		t.Fatal(err)
	}
	_, err := DeriveWorkdir(ctx, root, "acme", "acme/widgets")
	if err == nil {
		t.Fatal("DeriveWorkdir accepted a symlink to a matching checkout outside the project root")
	}
	if !errors.Is(err, ErrWorkdirRefused) {
		t.Fatalf("err = %v, want the ErrWorkdirRefused sentinel", err)
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("err %q does not say the path is a symlink", err)
	}
	// The developer's checkout is untouched.
	if _, statErr := os.Stat(filepath.Join(outside, ".git")); statErr != nil {
		t.Fatalf("the outside checkout was touched: %v", statErr)
	}

	// A dangling symlink is refused too, and not replaced by a clone.
	if err := os.Remove(filepath.Join(pr, "widgets")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "nowhere"), filepath.Join(pr, "widgets")); err != nil {
		t.Fatal(err)
	}
	if _, err := DeriveWorkdir(ctx, root, "acme", "acme/widgets"); err == nil {
		t.Fatal("DeriveWorkdir accepted a dangling symlink")
	}

	// A symlink at the PROJECT ROOT relocates the whole project — every clone
	// and the worktree directory — so it is refused before anything is derived.
	realRoot := filepath.Join(t.TempDir(), "real")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	linkedRoot := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := DeriveWorkdir(ctx, filepath.Dir(linkedRoot), "linked", "acme/widgets"); err == nil {
		t.Fatal("DeriveWorkdir accepted a symlinked project root")
	}
}

// A SYMLINK at the project's worktrees directory is refused: it is the fleet's
// own area, and a link there would put every worktree a session creates (and
// the scope granted for it) outside the project root.
func TestDeriveWorktreesDirRefusesASymlink(t *testing.T) {
	root := t.TempDir()
	pr := filepath.Join(root, "acme")
	if err := os.MkdirAll(pr, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(pr, WorktreesDirName)); err != nil {
		t.Fatal(err)
	}
	_, err := DeriveWorktreesDir(root, "acme")
	if err == nil {
		t.Fatal("DeriveWorktreesDir accepted a symlinked worktree directory")
	}
	if !errors.Is(err, ErrWorkdirRefused) {
		t.Fatalf("err = %v, want the ErrWorkdirRefused sentinel", err)
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("err %q does not say the directory is a symlink", err)
	}
	// Nothing was created through the link.
	entries, readErr := os.ReadDir(outside)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("the outside directory gained %d entries through the link", len(entries))
	}

	// The project root itself being a symlink refuses the worktree derivation
	// as well.
	linkedRoot := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(pr, linkedRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := DeriveWorktreesDir(filepath.Dir(linkedRoot), "linked"); err == nil {
		t.Fatal("DeriveWorktreesDir accepted a symlinked project root")
	}
}

// A bare-name repos key that aliases the full primary identity is materialized
// under the FULL identity: the bare name has no owner, so it cannot be cloned,
// and attempting it first refused an instance whose primary already supplied
// the clone URL (CLA-678 review MUST 3.2).
func TestDeriveInstanceWorkdirsBareKeyAliasesThePrimary(t *testing.T) {
	base := makeBareRemote(t, "acme/widgets")
	pointClonesAt(t, base)
	root := t.TempDir()

	cfg := &config.Config{
		BacklogURL:  "https://clankerbar.com/mcp/acme",
		Repos:       map[string]string{"widgets": "/some/local/path"},
		PrimaryRepo: "acme/widgets",
	}
	got, err := deriveInstanceWorkdirs(context.Background(), cfg, root, "")
	if err != nil {
		t.Fatalf("deriveInstanceWorkdirs refused a bare key aliasing the primary: %v", err)
	}
	if got["acme"] != filepath.Join(root, "acme") {
		t.Fatalf("deriveInstanceWorkdirs = %v, want the project root", got)
	}
	// The clone was made from the FULL identity, under the shared checkout name.
	remote, err := checkoutOriginRemote(filepath.Join(root, "acme", "widgets"))
	if err != nil {
		t.Fatalf("the alias's clone is not attributable: %v", err)
	}
	if !strings.Contains(remote, "acme/widgets") {
		t.Fatalf("the clone's origin is %q, want acme/widgets", remote)
	}
}

// Two FULL identities with the same checkout name cannot share one directory
// under the project root: the conflict is reported BEFORE anything is cloned,
// naming both identities, rather than surfacing as a wrong-origin refusal after
// the first clone exists (CLA-678 review SHOULD 2.2).
func TestDeriveInstanceWorkdirsRefusesDuplicateCheckoutNames(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{
		BacklogURL:  "https://clankerbar.com/mcp/acme",
		Repos:       map[string]string{"other/api": "/some/local/path"},
		PrimaryRepo: "acme/api",
	}
	_, err := deriveInstanceWorkdirs(context.Background(), cfg, root, "")
	if err == nil {
		t.Fatal("deriveInstanceWorkdirs accepted two repos deriving the same checkout name")
	}
	if !errors.Is(err, ErrWorkdirRefused) {
		t.Fatalf("err = %v, want the ErrWorkdirRefused sentinel", err)
	}
	for _, want := range []string{"acme/api", "other/api", "api"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q does not name %q", err, want)
		}
	}
	// Nothing was cloned for either identity.
	if entries, readErr := os.ReadDir(filepath.Join(root, "acme")); readErr == nil && len(entries) > 0 {
		t.Fatalf("the refused instance still materialized %d entries", len(entries))
	}
}
