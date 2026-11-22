package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// CLA-459 regression tests: doctor's deploy_lag check reads remotes with
// ls-remote and fetch, so it has the account-mismatch gap CLA-458 closed for
// delivery's verifier - an HTTPS github origin that names no account is read
// as whichever gh account is ACTIVE, which on this machine cannot see the
// private lecstor repos, and the check degraded to "could not read refs"
// exactly where measuring real deploy lag matters.
//
// The pin does NOT mock git's opinion away: it drives the PRODUCTION runner
// (newDeployGitRun, the memoized runner doctorEnv installs by default) end to
// end over a real temporary repository, with a recording `git` shim and a fake
// `gh` on PATH.
// The shim REFUSES ls-remote with the production failure ("Repository not
// found", exit 128) unless the invocation carries exactly the credential
// scoping CLA-458 specifies, so a green check is reachable only through the
// resolution chain: derive the owner from the remote URL, resolve THAT
// account's token via `gh auth token --user`, hand git the insteadOf rewrite
// in its environment. Local commands must stay unscoped; the record proves it.

const (
	// deployCredFakeToken is what the fake gh answers with. It is asserted to
	// be the token git actually receives, so the chain "derive owner ->
	// resolve THAT account -> plumb the resolved value" is pinned end to end.
	deployCredFakeToken = "gho_cla459-test-token-not-a-secret"

	// deployCredWantKey0/deployCredWantVal0 spell the insteadOf rewrite the
	// scoping must produce, keyed to that token.
	deployCredWantKey0 = "url.https://x-access-token:" + deployCredFakeToken + "@github.com/.insteadOf"
	deployCredWantVal0 = "https://github.com/"
)

// writeDeployCredShims installs the two fakes this suite drives deployGitRun
// through, and returns their directory plus the two recording paths. The real
// git binary is resolved BEFORE the caller narrows PATH, and baked into the
// shim by absolute path: deployGitRun execs "git" off PATH, so the shim itself
// must be first on PATH, and its pass-throughs must not re-enter themselves.
func writeDeployCredShims(t *testing.T) (binDir, record, ghRecord string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("resolve real git before narrowing PATH: %v", err)
	}
	binDir = t.TempDir()
	record = filepath.Join(t.TempDir(), "deploy-cred-record.log")
	ghRecord = filepath.Join(t.TempDir(), "deploy-cred-gh.log")

	gitShim := filepath.Join(binDir, "git")
	body := strings.NewReplacer("__REAL_GIT__", realGit, "__RECORD__", record).Replace(`#!/bin/sh
printf '=== %s\n' "$1" >> "$DEPLOY_CRED_RECORD"
printf 'count=%s\n' "${GIT_CONFIG_COUNT-unset}" >> "$DEPLOY_CRED_RECORD"
[ -n "${GIT_CONFIG_KEY_0+x}" ] && printf 'key0=%s\n' "$GIT_CONFIG_KEY_0" >> "$DEPLOY_CRED_RECORD"
[ -n "${GIT_CONFIG_VALUE_0+x}" ] && printf 'val0=%s\n' "$GIT_CONFIG_VALUE_0" >> "$DEPLOY_CRED_RECORD"

case "$1" in
ls-remote)
	if [ -n "$DEPLOY_CRED_REQUIRE_TOKEN" ]; then
		case "$GIT_CONFIG_KEY_0" in
		*"x-access-token:$DEPLOY_CRED_REQUIRE_TOKEN@github.com/"*) ;;
		*)
			echo "remote: Repository not found." >&2
			exit 128
			;;
		esac
	fi
	if [ -n "$DEPLOY_CRED_FAKE_TIP_SHA" ]; then
		printf '%s\trefs/heads/staging\n' "$DEPLOY_CRED_FAKE_TIP_SHA"
		exit 0
	fi
	last=""
	for a in "$@"; do last="$a"; done
	sha=$("__REAL_GIT__" rev-parse --quiet --verify "$last")
	[ -n "$sha" ] && printf '%s\t%s\n' "$sha" "$last"
	exit 0
;;
fetch)
	exit 0
;;
esac

exec "__REAL_GIT__" "$@"
`)
	if err := os.WriteFile(gitShim, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	ghFake := filepath.Join(binDir, "gh")
	ghBody := strings.NewReplacer("__GH_RECORD__", ghRecord).Replace(`#!/bin/sh
printf '%s\n' "$*" >> "__GH_RECORD__"
if [ -n "$DEPLOY_GH_FAKE_SLEEP" ]; then
	exec sleep "$DEPLOY_GH_FAKE_SLEEP"
fi
if [ "$1" = auth ] && [ "$2" = token ]; then
	if [ "$DEPLOY_GH_FAKE_FAIL" = 1 ]; then
		echo "no oauth token found for github.com account $4" >&2
		exit 1
	fi
	if [ -n "$DEPLOY_GH_FAKE_TOKEN_BY_OWNER" ]; then
		printf 'gho_%s\n' "$4"
		exit 0
	fi
	printf '%s\n' "$DEPLOY_CRED_FAKE_TOKEN"
	exit 0
fi
echo "unexpected gh invocation: $*" >&2
exit 42
`)
	if err := os.WriteFile(ghFake, []byte(ghBody), 0o755); err != nil {
		t.Fatal(err)
	}
	return binDir, record, ghRecord
}

// deployCredRepo builds the world the check reads: a real repository holding
// the deployed commit on main and (usually) the integration tip one commit
// ahead on staging, with origin set to originURL. Everything runs with the
// ambient PATH, so call it before writeDeployCredShims narrows it.
func deployCredRepo(t *testing.T, originURL string, withStaging bool) (dir, deployed string) {
	t.Helper()
	dir = t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "doctor-cred-test@example.com")
	run("config", "user.name", "deploy lag cred test")
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "base")
	deployed = run("rev-parse", "HEAD")
	if withStaging {
		run("checkout", "-q", "-b", "staging")
		if err := os.WriteFile(filepath.Join(dir, "tip.txt"), []byte("tip\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		run("add", ".")
		run("commit", "-q", "-m", "tip")
	}
	run("remote", "add", "origin", originURL)
	return dir, deployed
}

// deployCredEnvFor wires a prepared repo into a doctorEnv whose ONLY
// substitution is the world (health answer, repo discovery): gitRun stays the
// production runner (the memoized one defaultDoctorEnv installs), because the
// thing under test is exactly the env it assembles around the real exec.
func deployCredEnvFor(repoDir, deployed string) doctorEnv {
	e := okEnv()
	e.fetchHealth = func(context.Context, string) (deployHealth, error) {
		return deployHealth{Version: deployVersion{Commit: deployed}}, nil
	}
	e.repos = func(context.Context, string) []string { return []string{repoDir} }
	e.gitRun = newDeployGitRun()
	return e
}

// deployCredSections splits a DEPLOY_CRED_RECORD into per-invocation sections
// keyed by subcommand; each holds the count/key0/val0 lines the shim wrote.
func deployCredSections(t *testing.T, record string) map[string][]string {
	t.Helper()
	body, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("credential record missing - the shim never ran: %v", err)
	}
	out := map[string][]string{}
	cur := ""
	for _, line := range strings.Split(string(body), "\n") {
		switch {
		case strings.HasPrefix(line, "=== "):
			cur = strings.TrimPrefix(line, "=== ")
			out[cur] = nil
		case cur != "" && line != "":
			out[cur] = append(out[cur], line)
		}
	}
	return out
}

func deployCredSectionValue(t *testing.T, secs map[string][]string, cmd, prefix string) string {
	t.Helper()
	for _, l := range secs[cmd] {
		if strings.HasPrefix(l, prefix) {
			return strings.TrimPrefix(l, prefix)
		}
	}
	return ""
}

// deployCredInvocation is one recorded git invocation in the order the shim
// wrote it. deployCredSections collapses repeated subcommands; this keeps them
// apart, for tests that must see each invocation's own scope.
type deployCredInvocation struct {
	cmd  string
	key0 string
}

func deployCredInvocations(t *testing.T, record string) []deployCredInvocation {
	t.Helper()
	body, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("credential record missing - the shim never ran: %v", err)
	}
	var out []deployCredInvocation
	cur := deployCredInvocation{}
	for _, line := range strings.Split(string(body), "\n") {
		switch {
		case strings.HasPrefix(line, "=== "):
			if cur.cmd != "" {
				out = append(out, cur)
			}
			cur = deployCredInvocation{cmd: strings.TrimPrefix(line, "=== ")}
		case strings.HasPrefix(line, "key0="):
			cur.key0 = strings.TrimPrefix(line, "key0=")
		}
	}
	if cur.cmd != "" {
		out = append(out, cur)
	}
	return out
}

// THE pin: a github.com remote readable only as the account that owns it. The
// shim refuses ls-remote unless the invocation carries the insteadOf rewrite
// naming the token our gh answered with, so a PASS here is reachable solely
// through CLA-458's resolution chain applied to deploy_lag.
func TestDeployLagScopesLsRemoteToTheURLOwner(t *testing.T) {
	repoDir, deployed := deployCredRepo(t, "https://github.com/lecstor/ezyapp.git", true)
	binDir, record, ghRecord := writeDeployCredShims(t)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DEPLOY_CRED_RECORD", record)
	t.Setenv("DEPLOY_CRED_GH_RECORD", ghRecord)
	t.Setenv("DEPLOY_CRED_REQUIRE_TOKEN", deployCredFakeToken)
	t.Setenv("DEPLOY_CRED_FAKE_TOKEN", deployCredFakeToken)

	c := deployLagCheck(context.Background(), "deploy_lag",
		"https://plane.example/health", "staging", repoDir, deployCredEnvFor(repoDir, deployed))

	if c.status != pass {
		t.Fatalf("scoped ls-remote over a complete world: got %v, want PASS (%s)", c.status, c.detail)
	}
	if !strings.Contains(c.detail, "1 commit") {
		t.Errorf("the real gap should be measured once refs are readable, got %q", c.detail)
	}

	secs := deployCredSections(t, record)
	if got := deployCredSectionValue(t, secs, "ls-remote", "key0="); got != deployCredWantKey0 {
		t.Errorf("ls-remote carried key0 %q, want %q", got, deployCredWantKey0)
	}
	if got := deployCredSectionValue(t, secs, "ls-remote", "val0="); got != deployCredWantVal0 {
		t.Errorf("ls-remote carried val0 %q, want %q", got, deployCredWantVal0)
	}

	// The owner reached gh: the token came from the URL's account, not the
	// ambient active one.
	ghCalls, err := os.ReadFile(ghRecord)
	if err != nil || !strings.Contains(string(ghCalls), "auth token --user lecstor") {
		t.Errorf("gh should have been asked for lecstor's token, got %q (%v)", string(ghCalls), err)
	}

	// Scoping rides ONLY the network commands. Every other invocation - the
	// remote probes, cat-file, merge-base, rev-list, log - must have gone out
	// with no credential config at all.
	for cmd, lines := range secs {
		if cmd == "ls-remote" || cmd == "fetch" {
			continue
		}
		for _, l := range lines {
			if strings.HasPrefix(l, "count=1") || strings.HasPrefix(l, "key0=") || strings.HasPrefix(l, "val0=") {
				t.Errorf("%s invocation carried credential scoping (%s): local commands must stay unscoped", cmd, l)
			}
		}
	}
}

// Fetch is the other network command, taken when the remote tip is missing
// locally. The shim advertises a tip it never sends (fetch exits 0, the object
// stays absent), so the check lands on its honest WARN - but the RECORD is the
// assertion: both network commands went out scoped while the hunt failed.
func TestDeployLagScopesFetchTheSameWay(t *testing.T) {
	repoDir, deployed := deployCredRepo(t, "https://github.com/lecstor/ezyapp.git", false)
	binDir, record, ghRecord := writeDeployCredShims(t)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DEPLOY_CRED_RECORD", record)
	t.Setenv("DEPLOY_CRED_GH_RECORD", ghRecord)
	t.Setenv("DEPLOY_CRED_REQUIRE_TOKEN", deployCredFakeToken)
	t.Setenv("DEPLOY_CRED_FAKE_TOKEN", deployCredFakeToken)
	t.Setenv("DEPLOY_CRED_FAKE_TIP_SHA", lagTip)

	c := deployLagCheck(context.Background(), "deploy_lag",
		"https://plane.example/health", "staging", repoDir, deployCredEnvFor(repoDir, deployed))

	if c.status != warn {
		t.Fatalf("tip advertised but unfetchable: got %v, want WARN (%s)", c.status, c.detail)
	}
	if !strings.Contains(c.detail, "could not be fetched") || !strings.Contains(c.detail, shortSHA(lagTip)) {
		t.Errorf("detail should name the tip it could not fetch, got %q", c.detail)
	}

	secs := deployCredSections(t, record)
	for _, cmd := range []string{"ls-remote", "fetch"} {
		if got := deployCredSectionValue(t, secs, cmd, "key0="); got != deployCredWantKey0 {
			t.Errorf("%s carried key0 %q, want %q", cmd, got, deployCredWantKey0)
		}
		if got := deployCredSectionValue(t, secs, cmd, "val0="); got != deployCredWantVal0 {
			t.Errorf("%s carried val0 %q, want %q", cmd, got, deployCredWantVal0)
		}
	}
}

// A non-github host has no gh owner to derive: nothing may be scoped, and gh
// must not be consulted at all - the fallback costs nothing where it cannot apply.
func TestDeployLagNonGitHubHostStaysUnscoped(t *testing.T) {
	repoDir, deployed := deployCredRepo(t, "https://gitlab.com/lecstor/ezyapp.git", true)
	binDir, record, ghRecord := writeDeployCredShims(t)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DEPLOY_CRED_RECORD", record)
	t.Setenv("DEPLOY_CRED_GH_RECORD", ghRecord)
	t.Setenv("DEPLOY_CRED_FAKE_TOKEN", deployCredFakeToken)

	c := deployLagCheck(context.Background(), "deploy_lag",
		"https://plane.example/health", "staging", repoDir, deployCredEnvFor(repoDir, deployed))

	if c.status != pass {
		t.Fatalf("non-github host: got %v, want PASS (%s)", c.status, c.detail)
	}
	secs := deployCredSections(t, record)
	for _, l := range secs["ls-remote"] {
		if !strings.HasPrefix(l, "count=unset") {
			t.Errorf("non-github ls-remote should carry no credential config, got %q", l)
		}
	}
	if _, err := os.Stat(ghRecord); err == nil {
		body, _ := os.ReadFile(ghRecord)
		t.Errorf("gh should never be consulted for a non-github remote, got %q", string(body))
	}
}

// An owner gh cannot answer for - an org-owned repo read through an account
// that is not a member - falls back to the unscoped invocation, byte-identical
// to pre-CLA-459 behavior: the check still runs, just without credentials it
// does not have.
func TestDeployLagOwnerWithoutGhTokenFallsBackUnscoped(t *testing.T) {
	repoDir, deployed := deployCredRepo(t, "https://github.com/some-org/some-repo.git", true)
	binDir, record, ghRecord := writeDeployCredShims(t)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DEPLOY_CRED_RECORD", record)
	t.Setenv("DEPLOY_CRED_GH_RECORD", ghRecord)
	t.Setenv("DEPLOY_GH_FAKE_FAIL", "1")

	c := deployLagCheck(context.Background(), "deploy_lag",
		"https://plane.example/health", "staging", repoDir, deployCredEnvFor(repoDir, deployed))

	if c.status != pass {
		t.Fatalf("unresolvable owner: got %v, want PASS (%s)", c.status, c.detail)
	}
	secs := deployCredSections(t, record)
	for _, l := range secs["ls-remote"] {
		if !strings.HasPrefix(l, "count=unset") {
			t.Errorf("unresolvable owner should leave ls-remote unscoped, got %q", l)
		}
	}
	ghCalls, err := os.ReadFile(ghRecord)
	if err != nil || !strings.Contains(string(ghCalls), "--user some-org") {
		t.Errorf("gh should have been asked for some-org's token before falling back, got %q (%v)", string(ghCalls), err)
	}
}

// A wedged gh must not hold the network command past its own deadline. Doctor's
// context is signal-only - it carries no deadline - so the credential
// resolution has to run under deployGitTimeout itself: a gh that hangs is then
// reaped at the deadline and the command ends as a bounded fail-open error
// instead of stalling the preflight for as long as gh is wedged.
func TestDeployLagBoundsCredentialResolutionByTheCommandDeadline(t *testing.T) {
	old := deployGitTimeout
	deployGitTimeout = 2 * time.Second
	t.Cleanup(func() { deployGitTimeout = old })

	repoDir, _ := deployCredRepo(t, "https://github.com/lecstor/ezyapp.git", true)
	binDir, record, ghRecord := writeDeployCredShims(t)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DEPLOY_CRED_RECORD", record)
	t.Setenv("DEPLOY_CRED_GH_RECORD", ghRecord)
	t.Setenv("DEPLOY_GH_FAKE_SLEEP", "10") // far past the 2s deadline

	start := time.Now()
	_, err := deployGitRun(context.Background(), repoDir, "ls-remote", "--heads", "--", "origin", "refs/heads/staging")
	elapsed := time.Since(start)

	if err == nil {
		t.Errorf("the expired deadline should have failed the command, got nil")
	}
	if elapsed > 5*time.Second {
		t.Errorf("credential resolution outlived the command deadline by %s - gh is not bound by deployGitTimeout", elapsed)
	}
	if body, err := os.ReadFile(ghRecord); err != nil || !strings.Contains(string(body), "auth token --user lecstor") {
		t.Errorf("gh should have started its token lookup before the deadline reaped it, got %q (%v)", string(body), err)
	}
}

// One check pass can issue up to eight network commands, but only two of them
// share a directory: the check's own ls-remote and fetch resolve against the
// same clone, while locateDeployRepo's six candidate fetches are in six
// distinct clones. This fixture runs the two same-clone commands; the gh record
// is the evidence they shared one resolution. The cross-clone case - the worst
// path, where no candidate holds the deployed commit - is pinned separately in
// TestDeployLagCredentialMemoSpansClonesOfOneOwner.
func TestDeployLagCredentialsResolveOncePerPass(t *testing.T) {
	repoDir, deployed := deployCredRepo(t, "https://github.com/lecstor/ezyapp.git", false)
	binDir, record, ghRecord := writeDeployCredShims(t)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DEPLOY_CRED_RECORD", record)
	t.Setenv("DEPLOY_CRED_GH_RECORD", ghRecord)
	t.Setenv("DEPLOY_CRED_REQUIRE_TOKEN", deployCredFakeToken)
	t.Setenv("DEPLOY_CRED_FAKE_TOKEN", deployCredFakeToken)
	t.Setenv("DEPLOY_CRED_FAKE_TIP_SHA", lagTip)

	c := deployLagCheck(context.Background(), "deploy_lag",
		"https://plane.example/health", "staging", repoDir, deployCredEnvFor(repoDir, deployed))
	if c.status != warn {
		t.Fatalf("tip advertised but unfetchable: got %v, want WARN (%s)", c.status, c.detail)
	}

	// Both commands went out scoped - the WARN is for the object, not for
	// credentials - and both carried the SAME resolution.
	secs := deployCredSections(t, record)
	for _, cmd := range []string{"ls-remote", "fetch"} {
		if got := deployCredSectionValue(t, secs, cmd, "key0="); got != deployCredWantKey0 {
			t.Errorf("%s carried key0 %q, want %q", cmd, got, deployCredWantKey0)
		}
	}

	body, err := os.ReadFile(ghRecord)
	if err != nil {
		t.Fatalf("credential record missing - gh never ran: %v", err)
	}
	calls := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(calls) != 1 {
		t.Fatalf("one pass ran %d credential resolutions, want 1: %q", len(calls), body)
	}
}

// locateDeployRepo fetches into up to maxDeployLagFetches DISTINCT clones, each
// its own repository directory, so a memo keyed by directory would still spawn
// gh once per candidate - the worst path doctor sits in when no clone can
// settle the deployed commit. The memo is keyed by the resolved OWNER instead:
// two clones of one repository share a single gh resolution, and both network
// commands still carry its scope.
func TestDeployLagCredentialMemoSpansClonesOfOneOwner(t *testing.T) {
	first, _ := deployCredRepo(t, "https://github.com/lecstor/ezyapp.git", false)
	second, _ := deployCredRepo(t, "https://github.com/lecstor/ezyapp.git", false)
	binDir, record, ghRecord := writeDeployCredShims(t)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DEPLOY_CRED_RECORD", record)
	t.Setenv("DEPLOY_CRED_GH_RECORD", ghRecord)
	t.Setenv("DEPLOY_CRED_FAKE_TOKEN", deployCredFakeToken)

	run := newDeployGitRun()
	if _, err := run(context.Background(), first, "fetch", "--quiet", "--", "origin", "staging"); err != nil {
		t.Fatalf("fetch in first clone: %v", err)
	}
	if _, err := run(context.Background(), second, "ls-remote", "--heads", "--", "origin", "refs/heads/staging"); err != nil {
		t.Fatalf("ls-remote in second clone: %v", err)
	}

	secs := deployCredSections(t, record)
	for _, cmd := range []string{"fetch", "ls-remote"} {
		if got := deployCredSectionValue(t, secs, cmd, "key0="); got != deployCredWantKey0 {
			t.Errorf("%s carried key0 %q, want %q", cmd, got, deployCredWantKey0)
		}
	}

	body, err := os.ReadFile(ghRecord)
	if err != nil {
		t.Fatalf("credential record missing - gh never ran: %v", err)
	}
	if calls := strings.Split(strings.TrimSpace(string(body)), "\n"); len(calls) != 1 {
		t.Fatalf("two clones of one owner ran %d credential resolutions, want 1: %q", len(calls), body)
	}
}

// A network command that names no repository must run UNSCPED rather than
// borrow the repository's default remote. A bare `git fetch` reads the branch's
// tracked upstream, and a named remote written without `--` cannot be told
// apart from an option value, so scoping to the default remote's owner would
// hand the wrong account's token to whatever the command actually reads. No
// caller names none today; pin the fail-open contract so a future one cannot
// silently reintroduce it.
func TestDeployLagUnnamedNetworkTargetStaysUnscoped(t *testing.T) {
	repoDir, _ := deployCredRepo(t, "https://github.com/lecstor/ezyapp.git", false)
	binDir, record, ghRecord := writeDeployCredShims(t)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DEPLOY_CRED_RECORD", record)
	t.Setenv("DEPLOY_CRED_GH_RECORD", ghRecord)
	t.Setenv("DEPLOY_CRED_FAKE_TOKEN", deployCredFakeToken)

	run := newDeployGitRun()
	if _, err := run(context.Background(), repoDir, "fetch", "--quiet"); err != nil {
		t.Fatalf("bare fetch: %v", err)
	}
	if _, err := run(context.Background(), repoDir, "ls-remote", "--heads", "upstream", "refs/heads/staging"); err != nil {
		t.Fatalf("ls-remote at a named remote without `--`: %v", err)
	}

	secs := deployCredSections(t, record)
	for _, cmd := range []string{"fetch", "ls-remote"} {
		for _, l := range secs[cmd] {
			if !strings.HasPrefix(l, "count=unset") {
				t.Errorf("%s named no repository and must stay unscoped, got %q", cmd, l)
			}
		}
	}
	if body, err := os.ReadFile(ghRecord); err == nil {
		t.Errorf("gh must not be consulted for an invocation naming no remote, got %q", string(body))
	}
}

// An scp-like ssh target is a URL, not a remote NAME: it must not spend a
// `git remote get-url git@github.com:...` lookup that can only fail. It also
// needs no token - ssh carries its own auth - so the invocation stays unscoped.
func TestDeployLagScpLikeTargetSkipsRemoteLookup(t *testing.T) {
	repoDir, _ := deployCredRepo(t, "https://github.com/lecstor/ezyapp.git", false)
	binDir, record, ghRecord := writeDeployCredShims(t)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DEPLOY_CRED_RECORD", record)
	t.Setenv("DEPLOY_CRED_GH_RECORD", ghRecord)
	t.Setenv("DEPLOY_CRED_FAKE_TOKEN", deployCredFakeToken)

	run := newDeployGitRun()
	if _, err := run(context.Background(), repoDir, "ls-remote", "--heads", "--",
		"git@github.com:lecstor/ezyapp.git", "refs/heads/staging"); err != nil {
		t.Fatalf("ls-remote at an scp-like URL: %v", err)
	}

	for _, inv := range deployCredInvocations(t, record) {
		if inv.cmd == "remote" {
			t.Errorf("an scp-like URL must not be resolved as a remote name, saw a %q invocation", inv.cmd)
		}
		if inv.cmd == "ls-remote" && inv.key0 != "" {
			t.Errorf("an ssh target needs no token, got key0 %q", inv.key0)
		}
	}
	if body, err := os.ReadFile(ghRecord); err == nil {
		t.Errorf("gh must not be consulted for an ssh target, got %q", string(body))
	}
}

// The scope must follow the remote the INVOCATION names, never the
// repository's default remote. The insteadOf rewrite matches every
// https://github.com/ URL a command touches, so a scope keyed to origin while
// the command reads upstream would hand upstream the wrong account's token.
// Each invocation's own recorded key0 proves where its scope came from, and the
// gh record proves the owner-keyed memo shares org-b's token with the bare URL
// instead of resolving it again.
func TestDeployLagScopesToTheRemoteTheInvocationTargets(t *testing.T) {
	repoDir, _ := deployCredRepo(t, "https://github.com/org-a/repo.git", true)
	addRemote := exec.Command("git", "remote", "add", "upstream", "https://github.com/org-b/repo.git")
	addRemote.Dir = repoDir
	if out, err := addRemote.CombinedOutput(); err != nil {
		t.Fatalf("add upstream remote: %v: %s", err, out)
	}

	binDir, record, ghRecord := writeDeployCredShims(t)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DEPLOY_CRED_RECORD", record)
	t.Setenv("DEPLOY_CRED_GH_RECORD", ghRecord)
	t.Setenv("DEPLOY_CRED_FAKE_TOKEN", deployCredFakeToken)
	t.Setenv("DEPLOY_GH_FAKE_TOKEN_BY_OWNER", "1")
	t.Setenv("DEPLOY_CRED_FAKE_TIP_SHA", lagTip)

	run := newDeployGitRun()
	wantTokens := []string{"gho_org-a", "gho_org-b", "gho_org-b"}
	for _, target := range []string{
		"origin",
		"upstream",
		"https://github.com/org-b/repo.git", // a bare URL names its own repository
	} {
		if _, err := run(context.Background(), repoDir, "ls-remote", "--heads", "--", target, "refs/heads/staging"); err != nil {
			t.Fatalf("ls-remote at %s: %v", target, err)
		}
	}

	// One gh resolution per OWNER: org-a, then org-b, which the bare URL reuses.
	body, err := os.ReadFile(ghRecord)
	if err != nil {
		t.Fatalf("credential record missing - gh never ran: %v", err)
	}
	calls := strings.Split(strings.TrimSpace(string(body)), "\n")
	wantOwners := []string{"org-a", "org-b"}
	if len(calls) != len(wantOwners) {
		t.Fatalf("got %d resolutions (%q), want one per owner %v", len(calls), body, wantOwners)
	}
	for i, owner := range wantOwners {
		if !strings.Contains(calls[i], "--user "+owner) {
			t.Errorf("resolution %d should ask gh for %q, got %q", i, owner, calls[i])
		}
	}

	// Every invocation, in order, carried the token of the remote IT names -
	// including the bare URL that reused org-b's memo entry.
	var got []string
	for _, inv := range deployCredInvocations(t, record) {
		if inv.cmd == "ls-remote" {
			got = append(got, inv.key0)
		}
	}
	if len(got) != len(wantTokens) {
		t.Fatalf("recorded %d network invocations, want %d: %q", len(got), len(wantTokens), got)
	}
	for i, token := range wantTokens {
		if !strings.Contains(got[i], "x-access-token:"+token+"@") {
			t.Errorf("invocation %d carried key0 %q, want token %q", i, got[i], token)
		}
	}
}

// Network invocations name their repository after `--`; a bare URL is
// unambiguous without it. Anything else cannot be told apart from git's own
// option values, so it returns "" and the invocation runs unscoped - never
// under the default remote's owner.
func TestDeployNetworkRemote(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"named remote after terminator", []string{"ls-remote", "--heads", "--", "upstream", "refs/heads/staging"}, "upstream"},
		{"url after terminator", []string{"fetch", "--quiet", "--", "https://github.com/org/repo.git", "main"}, "https://github.com/org/repo.git"},
		{"scp-like url after terminator", []string{"fetch", "--quiet", "--", "git@github.com:org/repo.git", "main"}, "git@github.com:org/repo.git"},
		{"bare url without terminator", []string{"ls-remote", "https://github.com/org/repo.git"}, "https://github.com/org/repo.git"},
		{"named remote without terminator stays unnamed", []string{"ls-remote", "--heads", "upstream", "refs/heads/staging"}, ""},
		{"repository not named", []string{"fetch", "--quiet"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := deployNetworkRemote(tc.args); got != tc.want {
				t.Errorf("deployNetworkRemote(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// The credential scope must tell a URL git reads as written from a remote NAME
// it must resolve. An scp-like ssh target is a URL: treating it as a name would
// spend a `git remote get-url git@github.com:...` that can only fail before the
// (correct) unscoped answer.
func TestDeployTargetIsURL(t *testing.T) {
	tests := []struct {
		target string
		want   bool
	}{
		{"https://github.com/org/repo.git", true},
		{"ssh://git@github.com/org/repo.git", true},
		{"git@github.com:org/repo.git", true},
		{"origin", false},
		{"upstream", false},
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.target, func(t *testing.T) {
			if got := deployTargetIsURL(tc.target); got != tc.want {
				t.Errorf("deployTargetIsURL(%q) = %v, want %v", tc.target, got, tc.want)
			}
		})
	}
}
