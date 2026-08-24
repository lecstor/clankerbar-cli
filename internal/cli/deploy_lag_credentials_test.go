package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// CLA-459 regression tests: doctor's deploy_lag check reads remotes with
// ls-remote and fetch, so it has the account-mismatch gap CLA-458 closed for
// delivery's verifier - an HTTPS github origin that names no account is read
// as whichever gh account is ACTIVE, which on this machine cannot see the
// private lecstor repos, and the check degraded to "could not read refs"
// exactly where measuring real deploy lag matters.
//
// The pin does NOT mock git's opinion away: it drives the PRODUCTION runner
// (deployGitRun, as doctorEnv.gitRun defaults to) end to end over a real
// temporary repository, with a recording `git` shim and a fake `gh` on PATH.
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
if [ "$1" = auth ] && [ "$2" = token ]; then
	if [ "$DEPLOY_GH_FAKE_FAIL" = 1 ]; then
		echo "no oauth token found for github.com account $4" >&2
		exit 1
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
// production deployGitRun, because the thing under test is exactly the env it
// assembles around the real exec.
func deployCredEnvFor(repoDir, deployed string) doctorEnv {
	e := okEnv()
	e.fetchHealth = func(context.Context, string) (deployHealth, error) {
		return deployHealth{Version: deployVersion{Commit: deployed}}, nil
	}
	e.repos = func(context.Context, string) []string { return []string{repoDir} }
	e.gitRun = deployGitRun
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
