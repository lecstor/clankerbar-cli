package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// --- fixtures ----------------------------------------------------------------

// The SHAs the deploy-lag fixtures talk about. They are arbitrary hex strings;
// only their relationships matter (who is an ancestor of whom, who exists in
// which repository), and those relationships live in the fixture knobs below.
const (
	lagDeployed = "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111" // what /health reports
	lagTip      = "ffff9999ffff9999ffff9999ffff9999ffff9999" // remote tip of staging
)

// lagRepoDir is the fake working copy every fixture reports as THE repository.
const lagRepoDir = "/fake/workdir/clankerbar"

// lagFixture is a scripted world for deployLagCheck: a /health answer, one
// repository whose object database is objects, a remote whose staging branch
// points at lagTip, and the ancestry/count answers the comparison needs.
type lagFixture struct {
	healthCommit string
	healthErr    error

	objects     map[string]bool // shas present in the repository
	fetchBrings []string        // shas a successful fetch adds to objects
	hasBranch   bool            // does origin carry refs/heads/staging at all
	ancestor    bool            // merge-base --is-ancestor deployed tip
	behindCount int             // rev-list --count deployed..tip
	aheadCount  int             // rev-list --count tip..deployed
	oldestAge   time.Duration   // age of the oldest undeployed commit

	fetchErr error    // what `git fetch` fails with (nil = succeeds)
	fetchLog []string // every fetch, as "repo remote branch"
	logCalls []string // every git subcommand invoked, args joined
}

func newLagFixture() *lagFixture {
	return &lagFixture{
		healthCommit: lagDeployed,
		objects:      map[string]bool{lagDeployed: true, lagTip: true},
		hasBranch:    true,
		ancestor:     true,
	}
}

func (f *lagFixture) run(_ context.Context, dir string, args ...string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("no args")
	}
	f.logCalls = append(f.logCalls, strings.Join(args, " "))
	switch args[0] {
	case "cat-file":
		sha := strings.TrimSuffix(args[2], "^{commit}")
		if f.objects[sha] {
			return "", nil
		}
		return "", fmt.Errorf("git cat-file: Not a valid object name %s", args[2])
	case "remote":
		return "origin\n", nil
	case "ls-remote":
		if !f.hasBranch {
			return "", nil
		}
		return lagTip + "\trefs/heads/staging\n", nil
	case "fetch":
		f.fetchLog = append(f.fetchLog, dir+" "+args[len(args)-2]+" "+args[len(args)-1])
		if f.fetchErr != nil {
			return "", f.fetchErr
		}
		for _, sha := range f.fetchBrings {
			f.objects[sha] = true
		}
		return "", nil
	case "merge-base":
		if f.ancestor {
			return "", nil
		}
		return "", errors.New("not an ancestor")
	case "rev-list":
		rng := args[2]
		switch {
		case strings.HasSuffix(rng, ".."+lagTip):
			return strconv.Itoa(f.behindCount) + "\n", nil
		case strings.HasPrefix(rng, lagTip+".."):
			return strconv.Itoa(f.aheadCount) + "\n", nil
		default:
			return "", fmt.Errorf("unexpected range %q", rng)
		}
	case "log":
		ts := time.Now().Add(-f.oldestAge).Unix()
		return strconv.FormatInt(ts, 10) + "\n", nil
	default:
		return "", fmt.Errorf("unexpected git command %q", args[0])
	}
}

// lagEnv wires a fixture into a doctorEnv, overriding only the three seams the
// check touches.
func lagEnv(f *lagFixture) doctorEnv {
	e := okEnv()
	e.fetchHealth = func(_ context.Context, _ string) (deployHealth, error) {
		return deployHealth{Version: deployVersion{Commit: f.healthCommit}}, f.healthErr
	}
	e.repos = func(context.Context, string) []string { return []string{lagRepoDir} }
	e.gitRun = f.run
	return e
}

// runLagCheck drives one check against a fixture with sane defaults: a healthy
// /health answer naming lagDeployed, and a remote staging tip the deployment
// sits behind by n commits aged age.
func runLagCheck(t *testing.T, f *lagFixture) check {
	t.Helper()
	cfg := validCfg(t)
	return deployLagCheck(context.Background(), "deploy_lag",
		"https://plane.example/health", cfg.IntegrationBranchFor(""), cfg.WorkDir, lagEnv(f))
}

// --- the gap itself ----------------------------------------------------------

func TestDeployLagWarnsWhenBehindAndStale(t *testing.T) {
	f := newLagFixture()
	f.behindCount = 21
	f.oldestAge = 13 * time.Hour

	c := runLagCheck(t, f)
	if c.status != warn {
		t.Fatalf("21 commits overnight: got %v, want WARN (%s)", c.status, c.detail)
	}
	if !strings.Contains(c.detail, "21 commits") {
		t.Errorf("the size of the gap must be named, got %q", c.detail)
	}
	if !strings.Contains(c.detail, "13 hours") {
		t.Errorf("the age of the gap must be named, got %q", c.detail)
	}
	if c.remedy == "" {
		t.Error("a WARN must carry a remedy")
	}
}

// One commit for ten minutes is normal and healthy; a check that warns on it
// gets ignored by the time the overnight gap arrives (CLA-174's lesson). Age is
// the discriminator, but the count is still reported on the quiet line.
func TestDeployLagBehindButFreshPasses(t *testing.T) {
	f := newLagFixture()
	f.behindCount = 1
	f.oldestAge = 10 * time.Minute

	c := runLagCheck(t, f)
	if c.status != pass {
		t.Fatalf("one fresh commit behind: got %v, want PASS (%s)", c.status, c.detail)
	}
	if !strings.Contains(c.detail, "1 commit") {
		t.Errorf("the count is always reported, even on a PASS, got %q", c.detail)
	}
	if !strings.Contains(c.detail, "10 min") {
		t.Errorf("the age is reported too, got %q", c.detail)
	}
}

// Exactly level: nothing to report but the match itself.
func TestDeployLagLevelPasses(t *testing.T) {
	f := newLagFixture()
	f.behindCount = 0

	c := runLagCheck(t, f)
	if c.status != pass {
		t.Fatalf("level deployment: got %v, want PASS (%s)", c.status, c.detail)
	}
	if !strings.Contains(c.detail, "matches") {
		t.Errorf("detail should say the deployment matches, got %q", c.detail)
	}
}

// AHEAD of the shared line is the case that will bite (task CLA-322): a local
// branch not yet merged, or a hotfix shipped directly, is the NORMAL state of a
// working clanker and must never read as a stale deploy.
func TestDeployLagAheadIsNotAStaleDeploy(t *testing.T) {
	f := newLagFixture()
	f.ancestor = false
	f.behindCount = 0
	f.aheadCount = 3

	c := runLagCheck(t, f)
	if c.status != pass {
		t.Fatalf("deployed ahead of staging: got %v, want PASS (%s)", c.status, c.detail)
	}
	for _, want := range []string{"ahead", "not a stale deploy"} {
		if !strings.Contains(c.detail, want) {
			t.Errorf("detail %q should say it is %q", c.detail, want)
		}
	}
}

func TestDeployLagDivergedWarns(t *testing.T) {
	f := newLagFixture()
	f.ancestor = false
	f.behindCount = 5
	f.aheadCount = 2

	c := runLagCheck(t, f)
	if c.status != warn {
		t.Fatalf("diverged histories: got %v, want WARN (%s)", c.status, c.detail)
	}
	if !strings.Contains(c.detail, "diverged") {
		t.Errorf("detail should say diverged, got %q", c.detail)
	}
}

// An unreadable age must not silence the check into a PASS: not knowing the
// gap's age is not knowing the deployment is current.
func TestDeployLagUnreadableAgeStillWarns(t *testing.T) {
	f := newLagFixture()
	f.behindCount = 4
	f.oldestAge = 0
	// Break ONLY the log call, keeping every other answer healthy.
	base := f.run
	e := lagEnv(f)
	e.gitRun = func(ctx context.Context, dir string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "log" {
			return "", errors.New("git log: blown away")
		}
		return base(ctx, dir, args...)
	}
	cfg := validCfg(t)
	c := deployLagCheck(context.Background(), "deploy_lag",
		"https://plane.example/health", cfg.IntegrationBranchFor(""), cfg.WorkDir, e)

	if c.status != warn {
		t.Fatalf("unreadable age over a real gap: got %v, want WARN (%s)", c.status, c.detail)
	}
	if !strings.Contains(c.detail, "could not be read") {
		t.Errorf("detail should say why the age is missing, got %q", c.detail)
	}
}

// --- version.commit values that are not a lag --------------------------------

// CLA-313 deliberately ships "unknown" for a build that could not name a
// commit. It degrades honestly here: its OWN warning, never a commit-count lag.
func TestDeployLagUnknownCommitValueWarnsByName(t *testing.T) {
	f := newLagFixture()
	f.healthCommit = "unknown"

	c := runLagCheck(t, f)
	if c.status != warn {
		t.Fatalf("unknown stamp: got %v, want WARN (%s)", c.status, c.detail)
	}
	if !strings.Contains(c.detail, `"unknown"`) {
		t.Errorf("the value itself must be named, got %q", c.detail)
	}
	if strings.Contains(strings.ToLower(c.detail), "behind") {
		t.Errorf("a non-SHA value must never be reported as a lag, got %q", c.detail)
	}
}

func TestDeployLagDirtyCommitValueWarnsByName(t *testing.T) {
	f := newLagFixture()
	f.healthCommit = "abc1234d-dirty"

	c := runLagCheck(t, f)
	if c.status != warn {
		t.Fatalf("dirty stamp: got %v, want WARN (%s)", c.status, c.detail)
	}
	if !strings.Contains(c.detail, "abc1234d-dirty") {
		t.Errorf("the value itself must be named, got %q", c.detail)
	}
	if !strings.Contains(strings.ToLower(c.detail), "dirty") {
		t.Errorf("detail should explain the -dirty suffix, got %q", c.detail)
	}
}

func TestDeployLagUnrecognisedValueWarns(t *testing.T) {
	f := newLagFixture()
	f.healthCommit = "release-2026-08-22"

	c := runLagCheck(t, f)
	if c.status != warn {
		t.Fatalf("non-commit stamp: got %v, want WARN (%s)", c.status, c.detail)
	}
	if !strings.Contains(c.detail, "release-2026-08-22") {
		t.Errorf("the value itself must be named, got %q", c.detail)
	}
}

func TestDeployLagMissingStampWarns(t *testing.T) {
	f := newLagFixture()
	f.healthCommit = ""

	c := runLagCheck(t, f)
	if c.status != warn {
		t.Fatalf("missing stamp: got %v, want WARN (%s)", c.status, c.detail)
	}
	if !strings.Contains(c.detail, "version.commit") {
		t.Errorf("detail should name the field, got %q", c.detail)
	}
}

// --- everything around the comparison ----------------------------------------

func TestDeployLagUnreachableHealthEndpointWarns(t *testing.T) {
	f := newLagFixture()
	f.healthErr = errors.New("dial tcp: connection refused")

	c := runLagCheck(t, f)
	if c.status != warn {
		t.Fatalf("unreachable endpoint: got %v, want WARN (%s)", c.status, c.detail)
	}
	if !strings.Contains(c.detail, "could not read") {
		t.Errorf("detail should say the endpoint could not be read, got %q", c.detail)
	}
	if !strings.Contains(c.remedy, "health_url") {
		t.Errorf("remedy should point at health_url, got %q", c.remedy)
	}
}

// Unconfigured is quiet by design: an operator who never sets health_url gets
// one explanatory PASS, not a permanent warning about a feature they opted out
// of.
func TestDeployLagNotConfiguredPassesQuietly(t *testing.T) {
	cfg := validCfg(t)
	c := deployLagCheck(context.Background(), "deploy_lag",
		cfg.HealthURLFor(""), cfg.IntegrationBranchFor(""), cfg.WorkDir, okEnv())
	if c.status != pass {
		t.Fatalf("unconfigured check: got %v, want PASS (%s)", c.status, c.detail)
	}
	if !strings.Contains(c.detail, "not configured") || !strings.Contains(c.detail, "health_url") {
		t.Errorf("detail should say what would enable it, got %q", c.detail)
	}
	if c.remedy != "" {
		t.Errorf("a PASS carries no remedy, got %q", c.remedy)
	}
}

// A clone that has simply not fetched since the build went out is the common
// case the honest-warn trap hides: fetching the INTEGRATION BRANCH brings the
// deployed SHA along, and the check proceeds to a real verdict.
func TestDeployLagFetchesTheIntegrationBranchWhenTheCommitIsMissing(t *testing.T) {
	f := newLagFixture()
	delete(f.objects, lagDeployed) // the clone predates the deploy
	f.fetchBrings = []string{lagDeployed, lagTip}
	f.behindCount = 9
	f.oldestAge = 3 * time.Hour

	c := runLagCheck(t, f)
	if c.status != warn {
		t.Fatalf("stale clone recovered by fetch: got %v, want WARN (%s)", c.status, c.detail)
	}
	if len(f.fetchLog) == 0 {
		t.Fatal("expected at least one fetch of the integration branch")
	}
	for _, entry := range f.fetchLog {
		if !strings.HasSuffix(entry, " staging") {
			t.Errorf("fetches must target the integration branch, got %q", entry)
		}
	}
	if !strings.Contains(c.detail, "9 commits") {
		t.Errorf("after recovery the real gap should be reported, got %q", c.detail)
	}
}

func TestDeployLagHonestWarnWhenNoCloneHasTheCommit(t *testing.T) {
	f := newLagFixture()
	delete(f.objects, lagDeployed) // never arrives, even after fetches

	c := runLagCheck(t, f)
	if c.status != warn {
		t.Fatalf("commit nowhere locally: got %v, want WARN (%s)", c.status, c.detail)
	}
	if !strings.Contains(c.detail, shortSHA(lagDeployed)) {
		t.Errorf("detail should name the deployed commit, got %q", c.detail)
	}
	if !strings.Contains(c.detail, "shallow") {
		t.Errorf("detail should hint at the likely cause, got %q", c.detail)
	}
}

// A multi-repo parent can hold dozens of clones and none owes doctor a network
// round trip: the hunt is bounded even when every fetch succeeds.
func TestDeployLagFetchHuntIsBounded(t *testing.T) {
	f := newLagFixture()
	delete(f.objects, lagDeployed)
	repos := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		repos = append(repos, fmt.Sprintf("/fake/parent/repo-%d", i))
	}
	e := lagEnv(f)
	e.repos = func(context.Context, string) []string { return repos }

	cfg := validCfg(t)
	c := deployLagCheck(context.Background(), "deploy_lag",
		"https://plane.example/health", "staging", cfg.WorkDir, e)

	if c.status != warn {
		t.Fatalf("bounded hunt: got %v, want WARN (%s)", c.status, c.detail)
	}
	if len(f.fetchLog) != maxDeployLagFetches {
		t.Errorf("fetched %d repositories, want the bound of %d", len(f.fetchLog), maxDeployLagFetches)
	}
}

// The bound caps ATTEMPTS, not successes: a remote that refuses every fetch
// must not convert the hunt into one round trip per candidate clone - that is
// precisely the failure case the bound exists for.
func TestDeployLagFetchHuntBoundCountsFailedAttempts(t *testing.T) {
	f := newLagFixture()
	delete(f.objects, lagDeployed)
	f.fetchErr = errors.New("connection refused")
	repos := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		repos = append(repos, fmt.Sprintf("/fake/parent/repo-%d", i))
	}
	e := lagEnv(f)
	e.repos = func(context.Context, string) []string { return repos }

	cfg := validCfg(t)
	c := deployLagCheck(context.Background(), "deploy_lag",
		"https://plane.example/health", "staging", cfg.WorkDir, e)

	if c.status != warn {
		t.Fatalf("hunt against a dead remote: got %v, want WARN (%s)", c.status, c.detail)
	}
	if len(f.fetchLog) != maxDeployLagFetches {
		t.Errorf("attempted %d fetches against a dead remote, want the bound of %d", len(f.fetchLog), maxDeployLagFetches)
	}
	if !strings.Contains(c.detail, "trying to fetch") {
		t.Errorf("detail should say the fetches were attempted, got %q", c.detail)
	}
}

// deployGitRun carries its own deadline (doctor's context has none): a wedged
// git binary must end in an error within the bound, not hang the preflight.
func TestDeployGitRunBoundsEachCommand(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nsleep 30\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	old := deployGitTimeout
	deployGitTimeout = 100 * time.Millisecond
	t.Cleanup(func() { deployGitTimeout = old })

	start := time.Now()
	_, err := deployGitRun(context.Background(), ".", "ls-remote", "origin")
	if err == nil {
		t.Fatal("a wedged git must fail once its deadline passes, not hang")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("git exec returned only after %s; the per-command deadline did not fire", elapsed)
	}
}

func TestDeployLagRemoteHasNoSuchBranchWarns(t *testing.T) {
	f := newLagFixture()
	f.hasBranch = false

	c := runLagCheck(t, f)
	if c.status != warn {
		t.Fatalf("missing integration branch: got %v, want WARN (%s)", c.status, c.detail)
	}
	if !strings.Contains(c.detail, "staging") {
		t.Errorf("detail should name the branch asked about, got %q", c.detail)
	}
	if !strings.Contains(c.remedy, "integration_branch") {
		t.Errorf("remedy should point at integration_branch, got %q", c.remedy)
	}
}

// --- wiring ------------------------------------------------------------------

// Per-project mode: one check per project, each resolving ITS health_url and
// integration branch the same way the rest of doctor resolves project config
// (project value wins, top-level falls back).
func TestDeployLagsCheckedPerProject(t *testing.T) {
	cfg := validCfg(t)
	cfg.Projects = []config.Project{
		{Slug: "acme"}, // falls back to top-level
		{Slug: "other", HealthURL: "https://acme.test/health", IntegrationBranch: "main"},
	}
	cfg.HealthURL = "https://top.example/health"

	var fetched []string
	e := lagEnv(newLagFixture())
	e.fetchHealth = func(_ context.Context, u string) (deployHealth, error) {
		fetched = append(fetched, u)
		return deployHealth{Version: deployVersion{Commit: "unknown"}}, nil
	}

	checks := checkDeployLags(context.Background(), cfg, e)
	if len(checks) != 2 {
		t.Fatalf("got %d checks (%v), want one per project", len(checks), names(checks))
	}
	first := find(t, checks, "deploy_lag[acme]")
	if first.status != warn || !strings.Contains(first.detail, `"unknown"`) {
		t.Errorf("acme check should have hit its endpoint, got %v (%s)", first.status, first.detail)
	}
	second := find(t, checks, "deploy_lag[other]")
	if second.status != warn || !strings.Contains(second.detail, `"unknown"`) {
		t.Errorf("other check should have hit its endpoint, got %v (%s)", second.status, second.detail)
	}
	if len(fetched) != 2 || fetched[0] != "https://top.example/health" || fetched[1] != "https://acme.test/health" {
		t.Errorf("endpoints resolved wrongly: %v", fetched)
	}

	// The second project overrides the integration branch too, visible in the
	// unconfigured-style info the check carries before it can compare.
	if !strings.Contains(second.info[0], "main") {
		t.Errorf("per-project integration_branch should surface, got %v", second.info)
	}
}

// No project configured a URL: one aggregate quiet line rather than one per
// project repeating it.
func TestDeployLagsNoneConfiguredIsOneQuietLine(t *testing.T) {
	cfg := validCfg(t)
	cfg.Projects = []config.Project{{Slug: "acme"}, {Slug: "other"}}

	checks := checkDeployLags(context.Background(), cfg, okEnv())
	if len(checks) != 1 {
		t.Fatalf("got %d checks (%v), want one aggregate line", len(checks), names(checks))
	}
	if checks[0].status != pass || !strings.Contains(checks[0].detail, "not configured") {
		t.Errorf("want a quiet PASS explaining the field, got %v (%s)", checks[0].status, checks[0].detail)
	}
}

// Mixed configuration: when at least one project is monitored, a project that
// names no endpoint still gets its own quiet line. A missing line is
// indistinguishable from a forgotten one - the operator must be able to see
// that project B is simply not monitored.
func TestDeployLagsUnconfiguredProjectStillGetsItsLine(t *testing.T) {
	cfg := validCfg(t)
	cfg.Projects = []config.Project{
		{Slug: "acme", HealthURL: "https://acme.test/health"}, // monitored
		{Slug: "dark"}, // no URL anywhere (top level unset too): unmonitored, but visible
	}

	e := lagEnv(newLagFixture())
	var fetched []string
	e.fetchHealth = func(_ context.Context, u string) (deployHealth, error) {
		fetched = append(fetched, u)
		return deployHealth{Version: deployVersion{Commit: lagDeployed}}, nil
	}
	checks := checkDeployLags(context.Background(), cfg, e)
	if len(checks) != 2 {
		t.Fatalf("got %d checks (%v), want one per project including the unconfigured one", len(checks), names(checks))
	}
	watched := find(t, checks, "deploy_lag[acme]")
	if watched.status != pass || !strings.Contains(watched.detail, "matches") {
		t.Errorf("acme should have run a full comparison against its endpoint, got %v (%s)", watched.status, watched.detail)
	}
	dark := find(t, checks, "deploy_lag[dark]")
	if dark.status != pass || !strings.Contains(dark.detail, "not configured") || !strings.Contains(dark.detail, "health_url") {
		t.Errorf("the unconfigured project should get its own quiet PASS, got %v (%s)", dark.status, dark.detail)
	}
	if len(fetched) != 1 || fetched[0] != "https://acme.test/health" {
		t.Errorf("only the monitored project's endpoint should be read, got %v", fetched)
	}
}

// The full preflight must keep reporting the check (and stay green) on a config
// that never mentions it.
func TestDoctorChecksIncludeDeployLagWhenUnconfigured(t *testing.T) {
	checks := doctorChecks(context.Background(), validCfg(t), okEnv())
	c := find(t, checks, "deploy_lag")
	if c.status != pass || !strings.Contains(c.detail, "not configured") {
		t.Errorf("unconfigured deploy_lag: got %v (%s), want a quiet PASS", c.status, c.detail)
	}
}
