package cli

// The per-project layout's preflight (CLA-678): with `project_root` declared,
// doctor FAILS when the layout it asserts does not hold — a configured repo
// resolving outside the project root, a clone whose origin names a different
// repo, or a worktree directory that is missing or outside the root. Without
// the flag there is nothing to check, and the legacy shapes must stay
// untouched.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

func doctorGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// layoutRoot builds a project root holding a clone of repo and its worktrees
// directory, returning the root.
func layoutRoot(t *testing.T, repo string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "acme")
	doctorGit(t, filepath.Join(root, filepath.Base(repo)), "init", "-b", "main")
	doctorGit(t, filepath.Join(root, filepath.Base(repo)), "remote", "add", "origin", "https://github.com/"+repo+".git")
	if err := os.MkdirAll(filepath.Join(root, config.WorktreesDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestCheckProjectRootPassesOnTheLayout(t *testing.T) {
	root := layoutRoot(t, "acme/widgets")
	cfg := &config.Config{ProjectRoot: true, WorkDir: root, PrimaryRepo: "acme/widgets"}
	got := checkProjectRoot("project root", cfg, cfg.ReposFor(""), cfg.PrimaryRepoFor(""), root)
	if len(got) != 1 {
		t.Fatalf("checks = %d, want 1", len(got))
	}
	if got[0].status != pass {
		t.Fatalf("status = %v (%s / %s), want pass", got[0].status, got[0].detail, got[0].remedy)
	}
}

// A repo resolving OUTSIDE the project root is the shape the flag exists to
// catch: the fleet's clone belongs under the root, and a session's scope does
// not reach the tree it actually resolves to.
func TestCheckProjectRootFailsForARepoOutsideTheRoot(t *testing.T) {
	root := layoutRoot(t, "acme/widgets")
	elsewhere := filepath.Join(t.TempDir(), "widgets")
	doctorGit(t, elsewhere, "init", "-b", "main")
	doctorGit(t, elsewhere, "remote", "add", "origin", "https://github.com/acme/widgets.git")

	cfg := &config.Config{
		ProjectRoot: true,
		WorkDir:     root,
		PrimaryRepo: "acme/widgets",
		Repos:       map[string]string{"acme/widgets": elsewhere},
	}
	got := checkProjectRoot("project root", cfg, cfg.ReposFor(""), cfg.PrimaryRepoFor(""), root)
	if len(got) != 1 || got[0].status != fail {
		t.Fatalf("checks = %+v, want one FAIL for a repo outside the project root", got)
	}
	if !strings.Contains(got[0].detail, "OUTSIDE the project root") || !strings.Contains(got[0].detail, elsewhere) {
		t.Errorf("detail %q does not name the outside checkout", got[0].detail)
	}
}

// A clone whose origin names a different repo is the wrong-repo read the layout
// prevents — the same attribution the supervisor's derivation enforces.
func TestCheckProjectRootFailsForAMismatchedOrigin(t *testing.T) {
	root := layoutRoot(t, "acme/widgets")
	doctorGit(t, filepath.Join(root, "widgets"), "remote", "set-url", "origin", "https://github.com/other-owner/widgets.git")

	cfg := &config.Config{ProjectRoot: true, WorkDir: root, PrimaryRepo: "acme/widgets"}
	got := checkProjectRoot("project root", cfg, cfg.ReposFor(""), cfg.PrimaryRepoFor(""), root)
	if len(got) != 1 || got[0].status != fail {
		t.Fatalf("checks = %+v, want one FAIL for a mismatched origin", got)
	}
	if !strings.Contains(got[0].detail, "different repo") {
		t.Errorf("detail %q does not say the clone is a different repo", got[0].detail)
	}
}

// The worktrees directory is where every task worktree is created: missing, the
// first step of a session's brief fails.
func TestCheckProjectRootFailsForAMissingWorktreeDir(t *testing.T) {
	root := layoutRoot(t, "acme/widgets")
	if err := os.RemoveAll(filepath.Join(root, config.WorktreesDirName)); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ProjectRoot: true, WorkDir: root, PrimaryRepo: "acme/widgets"}
	got := checkProjectRoot("project root", cfg, cfg.ReposFor(""), cfg.PrimaryRepoFor(""), root)
	if len(got) != 1 || got[0].status != fail {
		t.Fatalf("checks = %+v, want one FAIL for a missing worktree directory", got)
	}
	if !strings.Contains(got[0].detail, config.WorktreesDirName) {
		t.Errorf("detail %q does not name the missing directory", got[0].detail)
	}
}

// Without the flag there is no layout to assert, so the check reports nothing —
// the legacy workdir shapes must not be told their perfectly valid layout is
// broken.
func TestCheckProjectRootIsSilentWithoutTheFlag(t *testing.T) {
	root := layoutRoot(t, "acme/widgets")
	cfg := &config.Config{WorkDir: root, PrimaryRepo: "acme/widgets"}
	if got := checkProjectRoot("project root", cfg, cfg.ReposFor(""), cfg.PrimaryRepoFor(""), root); got != nil {
		t.Fatalf("checks = %+v, want none without project_root", got)
	}
}
