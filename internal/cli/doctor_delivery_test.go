package cli

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

// --- delivery -----------------------------------------------------------------

// The delivery check's first prerequisite is the binary every read runs
// through: no git on PATH and every claim reports could-not-verify.
func TestDeliveryGitMissingWarns(t *testing.T) {
	e := okEnv()
	e.lookPath = func(file string) (string, error) {
		if file == "git" {
			return "", errors.New("not found")
		}
		return "/usr/local/bin/" + file, nil
	}
	e.repos = func(context.Context, string) []string {
		t.Error("the repository search must not run without git")
		return nil
	}

	c := deliveryCheck(context.Background(), "delivery", t.TempDir(), nil, "", e)

	if c.status != warn {
		t.Fatalf("status = %v (%s), want WARN", c.status, c.detail)
	}
	for _, want := range []string{"git is not on PATH", "could-not-verify"} {
		if !strings.Contains(c.detail, want) {
			t.Errorf("detail %q is missing %q", c.detail, want)
		}
	}
	if c.remedy == "" {
		t.Error("a WARN must carry a remedy")
	}
}

// A workdir that is not a repository is the silent case this check exists for:
// the verifier would answer "could not verify" every time, and the run still
// drains.
func TestDeliveryWorkdirThatIsNotARepositoryWarns(t *testing.T) {
	workdir := t.TempDir()

	c := deliveryCheck(context.Background(), "delivery", workdir, nil, "", okEnv())

	if c.status != warn {
		t.Fatalf("status = %v (%s), want WARN", c.status, c.detail)
	}
	for _, want := range []string{"no non-bare git repository", workdir, "could-not-verify"} {
		if !strings.Contains(c.detail, want) {
			t.Errorf("detail %q is missing %q", c.detail, want)
		}
	}
	if c.remedy == "" {
		t.Error("a WARN must carry a remedy")
	}
}

// A workdir the verifier cannot read must reach stdout as a WARN and must not
// fail the command: `doctor && run` still drains a backlog from it.
func TestDoctorRunWarnsOnAnUnverifiableWorkdir(t *testing.T) {
	dir := t.TempDir() // not a repository
	cfgPath := filepath.Join(t.TempDir(), "clankerbar.json")
	if err := os.WriteFile(cfgPath, []byte(`{"harness":"claude","workdir":"`+dir+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if err := doctorRun(context.Background(), &out, cfgPath, config.Overrides{}, realGitEnv(t)); err != nil {
		t.Fatalf("a WARN must not fail the command: %v\n%s", err, out.String())
	}
	rendered := out.String()
	if !strings.Contains(rendered, "WARN  delivery") {
		t.Errorf("the delivery check never reached stdout:\n%s", rendered)
	}
	for _, want := range []string{"no non-bare git repository", dir, "-> "} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the rendered delivery line is missing %q:\n%s", want, rendered)
		}
	}
}

// A resolvable repository with a remote is the PASS case, and it must name the
// repository and the remote the verifier will read.
func TestDeliveryRepositoryWithOriginPasses(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	e := okEnv()
	e.repos = func(context.Context, string) []string { return []string{repo} }
	e.gitRun = func(_ context.Context, dir string, args ...string) (string, error) {
		if dir == repo && len(args) > 0 && args[0] == "remote" {
			return "origin\n", nil
		}
		return "", errors.New("unexpected git: " + strings.Join(args, " ") + " in " + dir)
	}

	c := deliveryCheck(context.Background(), "delivery", t.TempDir(), nil, "", e)

	if c.status != pass {
		t.Fatalf("status = %v (%s), want PASS", c.status, c.detail)
	}
	if !strings.Contains(c.detail, "1 repository") {
		t.Errorf("detail should count the repository found, got %q", c.detail)
	}
	if len(c.info) != 1 || !strings.Contains(c.info[0], repo+" -> origin") {
		t.Errorf("info should name the repo and its remote, got %v", c.info)
	}
}

// A repository with no remote cannot be judged against anything: WARN, naming
// the repository and the remedy.
func TestDeliveryRepositoryWithoutRemoteWarns(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	e := okEnv()
	e.repos = func(context.Context, string) []string { return []string{repo} }
	e.gitRun = func(context.Context, string, ...string) (string, error) { return "", nil }

	c := deliveryCheck(context.Background(), "delivery", t.TempDir(), nil, "", e)

	if c.status != warn {
		t.Fatalf("status = %v (%s), want WARN", c.status, c.detail)
	}
	if !strings.Contains(c.detail, "none has a remote") {
		t.Errorf("detail should say no remote is readable, got %q", c.detail)
	}
	if len(c.info) != 1 || !strings.Contains(c.info[0], repo) || !strings.Contains(c.info[0], "no remote") {
		t.Errorf("info should name the repo and the missing remote, got %v", c.info)
	}
	if !strings.Contains(c.remedy, "origin") {
		t.Errorf("remedy should name the remote to add, got %q", c.remedy)
	}
}

// Several remotes and no origin: the verifier keeps reading "origin" and fails,
// so this must WARN even though a remote exists.
func TestDeliveryOnlyNonOriginRemotesWarns(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	e := okEnv()
	e.repos = func(context.Context, string) []string { return []string{repo} }
	e.gitRun = func(context.Context, string, ...string) (string, error) {
		return "upstream\nfork\n", nil
	}

	c := deliveryCheck(context.Background(), "delivery", t.TempDir(), nil, "", e)

	if c.status != warn {
		t.Fatalf("status = %v (%s), want WARN", c.status, c.detail)
	}
	if len(c.info) != 1 || !strings.Contains(c.info[0], "no origin") {
		t.Errorf("info should say there is no origin, got %v", c.info)
	}
}

// A repo the verifier can read beside one it cannot: WARN, naming which is
// which, because a claim resolved in the unreadable one is never judged.
func TestDeliveryMixedRepositoriesWarn(t *testing.T) {
	good := filepath.Join(t.TempDir(), "good")
	bad := filepath.Join(t.TempDir(), "bad")
	e := okEnv()
	e.repos = func(context.Context, string) []string { return []string{good, bad} }
	e.gitRun = func(_ context.Context, dir string, _ ...string) (string, error) {
		if dir == good {
			return "origin\n", nil
		}
		return "", nil
	}

	c := deliveryCheck(context.Background(), "delivery", t.TempDir(), nil, "", e)

	if c.status != warn {
		t.Fatalf("status = %v (%s), want WARN", c.status, c.detail)
	}
	if !strings.Contains(c.detail, "1 of 2 repositories") {
		t.Errorf("detail should count the unreadable one against the total, got %q", c.detail)
	}
	joined := strings.Join(c.info, "\n")
	if !strings.Contains(joined, good+" -> origin") || !strings.Contains(joined, bad+" -> no remote") {
		t.Errorf("info should classify both repos, got %v", c.info)
	}
	if c.remedy == "" {
		t.Error("a WARN must carry a remedy")
	}
}

// The search is per project workdir: each project gets its own line, scoped to
// its own repositories, and a project inheriting the top-level workdir resolves
// it the way the loop does.
func TestDeliveryIsPerProject(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	repoA, repoB := filepath.Join(dirA, "repo-a"), filepath.Join(dirB, "repo-b")
	cfg := validCfgIn(t, dirB)
	cfg.Projects = []config.Project{{Slug: "alpha", WorkDir: dirA}, {Slug: "beta"}}

	e := okEnv()
	e.repos = func(_ context.Context, workdir string) []string {
		switch workdir {
		case dirA:
			return []string{repoA}
		case dirB:
			return []string{repoB}
		}
		return nil
	}
	e.gitRun = func(context.Context, string, ...string) (string, error) { return "origin\n", nil }

	checks := checkDelivery(context.Background(), cfg, e)
	if len(checks) != 2 {
		t.Fatalf("got %d checks (%v), want one per project", len(checks), names(checks))
	}
	alpha := find(t, checks, "delivery[alpha]")
	if alpha.status != pass || len(alpha.info) != 1 || !strings.Contains(alpha.info[0], repoA) {
		t.Errorf("alpha should report only its own repo, got %v (%v)", alpha.status, alpha.info)
	}
	beta := find(t, checks, "delivery[beta]")
	if beta.status != pass || len(beta.info) != 1 || !strings.Contains(beta.info[0], repoB) {
		t.Errorf("beta should inherit the top-level workdir and report its repo, got %v (%v)", beta.status, beta.info)
	}
}

// A declared checkout outside the workdir is where sessions actually start, so
// the check must find it through the repos map rather than warn that the
// (empty) workdir holds nothing.
func TestDeliveryReachesDeclaredCheckoutOutsideTheWorkdir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	f := strandedEnvOnBranch(t, "clanker/x")
	workdir := t.TempDir() // holds no repository of its own

	cfg := validCfgIn(t, workdir)
	cfg.Repos = map[string]string{"owner/repo": f.repo}

	c := deliveryCheck(context.Background(), "delivery", workdir, cfg.Repos, cfg.PrimaryRepo, realGitEnv(t))

	if c.status != pass {
		t.Fatalf("status = %v (%s), want PASS via the declared checkout", c.status, c.detail)
	}
	if len(c.info) != 1 || !strings.Contains(c.info[0], f.repo+" -> origin") {
		t.Errorf("the declared checkout should be the one named, got %v", c.info)
	}
}

// Real git: a checkout under the workdir passes and is named; a bare repository
// is not a verifiable candidate, so a workdir holding only one warns.
func TestDeliveryRealRepositories(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	f := strandedEnvOnBranch(t, "clanker/x")

	c := deliveryCheck(context.Background(), "delivery", f.root, nil, "", realGitEnv(t))
	if c.status != pass {
		t.Fatalf("checkout under the workdir: status = %v (%s), want PASS", c.status, c.detail)
	}
	if len(c.info) != 1 || !strings.Contains(c.info[0], f.repo+" -> origin") {
		t.Errorf("the checkout should be named once with its remote, got %v", c.info)
	}

	bare := t.TempDir()
	gitT(t, filepath.Dir(bare), "init", "--bare", filepath.Join(bare, "remote.git"))
	c = deliveryCheck(context.Background(), "delivery", bare, nil, "", realGitEnv(t))
	if c.status != warn {
		t.Fatalf("bare-only workdir: status = %v (%s), want WARN", c.status, c.detail)
	}
	if !strings.Contains(c.detail, "no non-bare git repository") {
		t.Errorf("a bare repository must not count as verifiable, got %q", c.detail)
	}
}
