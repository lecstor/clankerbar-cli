package supervisor

// The workdir derivation's fail-closed branches (CLA-547, phase 2a of
// docs/proposals/daemon-supervisor.md): the derived path is <root>/<repo name>,
// and a missing directory or one that is not a checkout of the expected repo
// is a refusal naming the path tried. The tests build real git checkouts so
// the "is a checkout" branch is verified against actual remotes, not mocks.

import (
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

// The happy path: `~/dev` + `lecstor/clankerbar` resolves to `~/dev/clankerbar`
// when that directory is a checkout of the expected repo, in both the https
// and ssh remote spellings, and for a bare-name identity.
func TestDeriveWorkdirResolvesRootAndRepo(t *testing.T) {
	root := t.TempDir()
	makeCheckout(t, filepath.Join(root, "clankerbar"), "lecstor/clankerbar")

	got, err := DeriveWorkdir(root, "lecstor/clankerbar")
	if err != nil {
		t.Fatalf("DeriveWorkdir: %v", err)
	}
	if want := filepath.Join(root, "clankerbar"); got != want {
		t.Fatalf("DeriveWorkdir = %q, want %q", got, want)
	}

	// The ssh spelling of the same remote must match too.
	gitRun(t, got, "remote", "set-url", "origin", "git@github.com:lecstor/clankerbar.git")
	if _, err := DeriveWorkdir(root, "lecstor/clankerbar"); err != nil {
		t.Fatalf("DeriveWorkdir with an ssh origin: %v", err)
	}

	// A trailing slash on the identity is tolerated.
	if _, err := DeriveWorkdir(root, "lecstor/clankerbar/"); err != nil {
		t.Fatalf("DeriveWorkdir with a trailing slash: %v", err)
	}

	// A bare-name identity matches whatever owner hosts the checkout — the
	// same owner-agnostic match the config's own resolution applies.
	if _, err := DeriveWorkdir(root, "clankerbar"); err != nil {
		t.Fatalf("DeriveWorkdir with a bare-name identity: %v", err)
	}

	// A relative root resolves against the cwd; pointing it at an empty dir
	// must refuse (nothing named clankerbar sits there).
	t.Chdir(t.TempDir())
	if _, err := DeriveWorkdir(".", "clankerbar"); err == nil {
		t.Fatal("DeriveWorkdir('.', 'clankerbar') resolved — nothing named clankerbar sits under the cwd")
	}

	// A ~ root expands like every other path this config accepts.
	makeCheckout(t, filepath.Join(t.TempDir(), "tilde"), "acme/tilde")
	if _, err := DeriveWorkdir("~", "acme/tilde"); err == nil {
		t.Fatal("DeriveWorkdir('~', 'acme/tilde') resolved — ~ must expand to the home dir, not match a temp checkout")
	}
}

// Fail-closed branch 1: the derived directory must exist. A missing directory
// is refused with the path that was tried, and never created.
func TestDeriveWorkdirRefusesMissingDirectory(t *testing.T) {
	root := t.TempDir()
	wantPath := filepath.Join(root, "ghost")
	_, err := DeriveWorkdir(root, "acme/ghost")
	if err == nil {
		t.Fatal("DeriveWorkdir resolved a missing checkout — fail-closed says refuse")
	}
	if !errors.Is(err, ErrWorkdirRefused) {
		t.Fatalf("err = %v, want the ErrWorkdirRefused sentinel", err)
	}
	if !strings.Contains(err.Error(), wantPath) {
		t.Errorf("err %q does not name the path tried %q", err, wantPath)
	}
	if _, statErr := os.Stat(wantPath); !os.IsNotExist(statErr) {
		t.Errorf("derivation created %s — the derived directory must never be created", wantPath)
	}

	// A path that exists but is a FILE is just as un-runnable.
	filePath := filepath.Join(root, "file-repo")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := DeriveWorkdir(root, "acme/file-repo"); err == nil {
		t.Fatal("DeriveWorkdir accepted a derived path that is a file")
	}

	// Missing inputs are refusals too: there is nothing to derive from.
	if _, err := DeriveWorkdir("", "acme/ghost"); err == nil {
		t.Fatal("DeriveWorkdir with no root must refuse")
	}
	if _, err := DeriveWorkdir(root, ""); err == nil {
		t.Fatal("DeriveWorkdir with no repo must refuse")
	}
}

// Fail-closed branch 2: the derived directory must be a checkout of the
// EXPECTED repo. A plain directory, a checkout of a different repo, a repo
// with no attributable remote — all refuse, naming the path tried.
func TestDeriveWorkdirRefusesNonMatchingCheckout(t *testing.T) {
	root := t.TempDir()

	// A plain directory with no .git at all.
	plain := filepath.Join(root, "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := DeriveWorkdir(root, "acme/plain"); err == nil {
		t.Fatal("DeriveWorkdir accepted a plain directory as a checkout")
	}

	// A real checkout of a DIFFERENT repo: the derived path exists, but it is
	// not the repo the project names. Same repo name, different owner — the
	// derived path is identical, the attribution is not.
	makeCheckout(t, filepath.Join(root, "widgets"), "acme/widgets")
	_, err := DeriveWorkdir(root, "other-owner/widgets")
	if err == nil {
		t.Fatal("DeriveWorkdir accepted a checkout of the wrong repo")
	}
	if !strings.Contains(err.Error(), filepath.Join(root, "widgets")) {
		t.Errorf("err %q does not name the path tried %q", err, filepath.Join(root, "widgets"))
	}
	if !strings.Contains(err.Error(), "other-owner/widgets") {
		t.Errorf("err %q does not name the expected repo", err)
	}
	if !strings.Contains(err.Error(), "acme/widgets") {
		t.Errorf("err %q does not name what the origin remote actually is", err)
	}

	// The boundary pair this codebase exists to tell apart: `clankerbar` must
	// never match `clankerbar-cli` (the project's own repo pair, CLA-351).
	makeCheckout(t, filepath.Join(root, "clankerbar-cli"), "lecstor/clankerbar-cli")
	if _, err := DeriveWorkdir(root, "lecstor/clankerbar"); err == nil {
		t.Fatal("DeriveWorkdir matched lecstor/clankerbar against a clankerbar-cli checkout — the boundary rule is missing")
	}

	// A git repository with NO remotes cannot be attributed to any repo.
	noRemote := filepath.Join(root, "noremote")
	if err := os.MkdirAll(noRemote, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, noRemote, "init", "-b", "main")
	if _, err := DeriveWorkdir(root, "acme/noremote"); err == nil {
		t.Fatal("DeriveWorkdir accepted a checkout with no remotes")
	}

	// Several remotes and no origin cannot be attributed either — refuse
	// rather than guess.
	twoRemotes := filepath.Join(root, "tworemotes")
	if err := os.MkdirAll(twoRemotes, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, twoRemotes, "init", "-b", "main")
	gitRun(t, twoRemotes, "remote", "add", "upstream", "https://github.com/acme/tworemotes.git")
	gitRun(t, twoRemotes, "remote", "add", "fork", "https://github.com/other/tworemotes.git")
	if _, err := DeriveWorkdir(root, "acme/tworemotes"); err == nil {
		t.Fatal("DeriveWorkdir accepted a checkout with two remotes and no origin")
	}

	// The one attribution fallback: a checkout whose ONLY remote is not named
	// origin is still attributable — the single remote IS the repo.
	onlyUpstream := filepath.Join(root, "onlyupstream")
	if err := os.MkdirAll(onlyUpstream, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, onlyUpstream, "init", "-b", "main")
	gitRun(t, onlyUpstream, "remote", "add", "upstream", "https://github.com/acme/onlyupstream.git")
	if _, err := DeriveWorkdir(root, "acme/onlyupstream"); err != nil {
		t.Fatalf("DeriveWorkdir refused a checkout whose only remote names the repo: %v", err)
	}
}

// The supervisor's own working directory is never part of the derivation:
// a failed derivation is an error, and a successful one is the derived path —
// there is no branch that returns the cwd.
func TestDeriveWorkdirNeverFallsBackToTheSupervisorCwd(t *testing.T) {
	// The supervisor sits in cwd; the root is a DIFFERENT directory.
	cwd := t.TempDir()
	t.Chdir(cwd)
	root := t.TempDir()

	// The root's checkout is the derived path, never the cwd.
	makeCheckout(t, filepath.Join(root, "clankerbar"), "lecstor/clankerbar")
	got, err := DeriveWorkdir(root, "lecstor/clankerbar")
	if err != nil {
		t.Fatalf("DeriveWorkdir: %v", err)
	}
	if got == cwd || strings.HasPrefix(got, cwd+string(os.PathSeparator)) {
		t.Fatalf("DeriveWorkdir returned %q — the supervisor's cwd %q must never be a result", got, cwd)
	}

	// A missing checkout under the root refuses even though the cwd is a
	// perfectly good directory to have fallen back to.
	if _, err := DeriveWorkdir(root, "acme/ghost"); err == nil {
		t.Fatal("DeriveWorkdir fell back to a runnable directory on a missing derivation — fail-closed says refuse")
	}
}

// The per-instance seam derives one workdir per project from that project's
// repo, with the config's documented primary/sole-repo resolution, and refuses
// the whole instance when any project's derivation fails.
func TestDeriveInstanceWorkdirsPerProject(t *testing.T) {
	root := t.TempDir()
	makeCheckout(t, filepath.Join(root, "widgets"), "acme/widgets")
	makeCheckout(t, filepath.Join(root, "gadgets"), "acme/gadgets")

	t.Run("single project uses the primary repo", func(t *testing.T) {
		cfg := &config.Config{PrimaryRepo: "acme/widgets"}
		got, err := deriveInstanceWorkdirs(cfg, root)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[""] != filepath.Join(root, "widgets") {
			t.Fatalf("deriveInstanceWorkdirs = %v, want {\"\": %q}", got, filepath.Join(root, "widgets"))
		}
	})

	t.Run("a sole repos key is primary implicitly", func(t *testing.T) {
		cfg := &config.Config{Repos: map[string]string{"acme/widgets": "/unused"}}
		got, err := deriveInstanceWorkdirs(cfg, root)
		if err != nil {
			t.Fatal(err)
		}
		if got[""] != filepath.Join(root, "widgets") {
			t.Fatalf("deriveInstanceWorkdirs = %v, want the sole repos key derived", got)
		}
	})

	t.Run("multi-project derives per project", func(t *testing.T) {
		cfg := &config.Config{Projects: []config.Project{
			{Slug: "one", PrimaryRepo: "acme/widgets"},
			{Slug: "two", PrimaryRepo: "acme/gadgets"},
		}}
		got, err := deriveInstanceWorkdirs(cfg, root)
		if err != nil {
			t.Fatal(err)
		}
		if got["one"] != filepath.Join(root, "widgets") || got["two"] != filepath.Join(root, "gadgets") {
			t.Fatalf("deriveInstanceWorkdirs = %v, want one -> widgets, two -> gadgets", got)
		}
	})

	t.Run("one bad project refuses the instance", func(t *testing.T) {
		cfg := &config.Config{Projects: []config.Project{
			{Slug: "one", PrimaryRepo: "acme/widgets"},
			{Slug: "two", PrimaryRepo: "acme/ghost"},
		}}
		_, err := deriveInstanceWorkdirs(cfg, root)
		if err == nil {
			t.Fatal("deriveInstanceWorkdirs succeeded with a project whose checkout is missing")
		}
		if !strings.Contains(err.Error(), "project two") || !strings.Contains(err.Error(), filepath.Join(root, "ghost")) {
			t.Fatalf("err %q does not name the failing project and the path tried", err)
		}
	})

	t.Run("a project naming no repo is a refusal, not a skip", func(t *testing.T) {
		cfg := &config.Config{PrimaryRepo: ""}
		_, err := deriveInstanceWorkdirs(cfg, root)
		if err == nil {
			t.Fatal("deriveInstanceWorkdirs succeeded with no repo declared")
		}
		if !strings.Contains(err.Error(), "primary_repo") {
			t.Fatalf("err %q does not tell the operator what to declare", err)
		}
	})
}
