package delivery

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLA-458 regression tests: the verifier's network reads must go out scoped to
// the account that owns a github.com HTTPS remote, because the ambient
// credential helper serves whichever account is ACTIVE - which need not see a
// private repo, and since CLA-457 an Unknown BranchPushed is fail-closed.
//
// The pin below does NOT mock git's opinion away: it uses real temporary
// repositories like the rest of this package, and the one network seam -
// ls-remote against a URL this machine cannot honestly reach in a unit test -
// is served by a recording shim that REFUSES with the production failure
// ("Repository not found", exit 128) unless the invocation carries exactly the
// credential scoping the fix specifies. Before CLA-458 that test fails with
// Unknown; after it, it can only pass by resolving the owner's token through
// `gh auth token --user` and handing git the insteadOf rewrite in its
// environment.

const (
	// credFakeToken is what the fake gh answers with. It is asserted to be the
	// token git actually receives, so the chain "derive owner -> resolve THAT
	// account -> plumb the resolved value" is pinned end to end.
	credFakeToken = "gho_cla458-test-token-not-a-secret"

	// wantKey0/wantVal0 spell the insteadOf rewrite the fix must produce.
	wantKey0 = "url.https://x-access-token:" + credFakeToken + "@github.com/.insteadOf"
	wantVal0 = "https://github.com/"
)

// writeCredShim installs two scripts and returns their paths: a fake `gh` that
// answers `auth token --user` (or fails on demand), and a `git` shim that
// appends each invocation's subcommand and credential-relevant environment to
// $CRED_RECORD before either serving ls-remote itself (refusing unless the
// scoping is present, when $CRED_REQUIRE_TOKEN names a token) or passing
// through to the real git.
func writeCredShim(t *testing.T) (gitShim, ghFake string) {
	t.Helper()
	dir := t.TempDir()

	ghFake = filepath.Join(dir, "gh")
	writeFile(t, ghFake, `#!/bin/sh
if [ "$1" = auth ] && [ "$2" = token ]; then
	if [ "$GH_FAKE_FAIL" = 1 ]; then
		echo "no oauth token found for github.com account $5" >&2
		exit 1
	fi
	printf '%s\n' "$GH_FAKE_TOKEN"
	exit 0
fi
echo "unexpected gh invocation: $*" >&2
exit 42
`)
	if err := os.Chmod(ghFake, 0o755); err != nil {
		t.Fatal(err)
	}

	gitShim = filepath.Join(dir, "git")
	writeFile(t, gitShim, `#!/bin/sh
printf '=== %s\n' "$1" >> "$CRED_RECORD"
printf 'count=%s\n' "${GIT_CONFIG_COUNT-unset}" >> "$CRED_RECORD"
[ -n "${GIT_CONFIG_KEY_0+x}" ] && printf 'key0=%s\n' "$GIT_CONFIG_KEY_0" >> "$CRED_RECORD"
[ -n "${GIT_CONFIG_VALUE_0+x}" ] && printf 'val0=%s\n' "$GIT_CONFIG_VALUE_0" >> "$CRED_RECORD"

case "$1" in
ls-remote)
	last=""
	for a in "$@"; do last="$a"; done
	if [ -n "$CRED_REQUIRE_TOKEN" ]; then
		case "$GIT_CONFIG_KEY_0" in
		*"x-access-token:$CRED_REQUIRE_TOKEN@github.com/"*) ;;
		*)
			echo "remote: Repository not found." >&2
			exit 128
			;;
		esac
	fi
	sha=$(git rev-parse --quiet --verify "$last")
	printf '%s\t%s\n' "$sha" "$last"
	exit 0
;;
fetch)
	exit 0
;;
esac
exec git "$@"
`)
	if err := os.Chmod(gitShim, 0o755); err != nil {
		t.Fatal(err)
	}
	return gitShim, ghFake
}

// credSections splits a CRED_RECORD into per-invocation sections keyed by
// subcommand: each holds the count/key0/val0 lines the shim wrote.
func credSections(t *testing.T, record string) map[string][]string {
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

func credSectionValue(t *testing.T, secs map[string][]string, cmd, prefix string) string {
	t.Helper()
	for _, l := range secs[cmd] {
		if strings.HasPrefix(l, prefix) {
			return strings.TrimPrefix(l, prefix)
		}
	}
	return ""
}

// TestGithubOwner pins the URL parsing the scoping stands on: an https
// github.com URL yields its owner; ssh remotes, other hosts and owner-less
// URLs yield nothing, which callers must read as "run unscoped".
func TestGithubOwner(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"https://github.com/lecstor/ezyapp.git", "lecstor"},
		{"https://github.com/lecstor/ezyapp", "lecstor"},
		{"http://github.com/lecstor/ezyapp.git", "lecstor"},
		{"https://GITHUB.com/lecstor/ezyapp.git", "lecstor"},
		{"https://user@github.com/lecstor/ezyapp.git", "lecstor"},
		{"https://github.com/", ""},
		{"https://github.com", ""},
		{"ssh://git@github.com/lecstor/ezyapp.git", ""},
		{"git@github.com:lecstor/ezyapp.git", ""},
		{"https://gitlab.com/lecstor/ezyapp.git", ""},
		{"https://github.example.com/lecstor/ezyapp.git", ""},
		{"/Users/jason/dev/ezyapp-wt/task", ""},
		{"file:///tmp/repo.git", ""},
		{"https://github.com/%zz/bad.git", ""}, // unparseable
	}
	for _, c := range cases {
		if got := githubOwner(c.raw); got != c.want {
			t.Errorf("githubOwner(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

// THE pin: a remote reachable ONLY as the account that owns it. The shim's
// ls-remote refuses with the production failure unless the invocation carries
// the insteadOf rewrite naming the token our gh answered with - so a Pass here
// is reachable solely through the CLA-458 resolution chain.
func TestPrivateGitHubRemoteIsReadAsTheURLOwner(t *testing.T) {
	env := newEnv(t)
	env.commit("main", "base.txt", "base")
	env.push("main")

	env.checkout("clanker/private-feature")
	env.commit("clanker/private-feature", "work.txt", "work")
	env.push("clanker/private-feature")

	// The driven repo whose origin names no account: the exact shape of
	// https://github.com/lecstor/ezyapp.git from the incident.
	env.git("remote", "set-url", "origin", "https://github.com/lecstor/ezyapp.git")

	record := filepath.Join(t.TempDir(), "cred-record.log")
	t.Setenv("CRED_RECORD", record)
	t.Setenv("CRED_REQUIRE_TOKEN", credFakeToken)
	t.Setenv("GH_FAKE_TOKEN", credFakeToken)
	gitShim, ghFake := writeCredShim(t)

	v := New(env.work, "origin")
	v.gitBin = gitShim
	v.ghBin = ghFake
	rep := v.Verify(context.Background(), Claim{Label: "CLA-458", Branch: "clanker/private-feature"})

	mustStatus(t, rep, BranchPushed, Pass)

	secs := credSections(t, record)
	if got := credSectionValue(t, secs, "ls-remote", "key0="); got != wantKey0 {
		t.Errorf("ls-remote carried key0 %q, want %q", got, wantKey0)
	}
	if got := credSectionValue(t, secs, "ls-remote", "val0="); got != wantVal0 {
		t.Errorf("ls-remote carried val0 %q, want %q", got, wantVal0)
	}

	// Scoping rides ONLY the network commands. Every other invocation - the
	// rev-parses, the remote get-url, merge-base - must have gone out with no
	// credential config at all: a leak there would mean the rewrite is being
	// sprayed over operations that neither need nor expect it.
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

// A local bare remote is the rest of this suite's world: nothing about it is a
// gh account, so nothing may change - no scoping env, and gh never consulted.
func TestLocalRemoteStaysUnscoped(t *testing.T) {
	env := newEnv(t)
	env.commit("main", "base.txt", "base")
	env.push("main")
	env.checkout("clanker/feature")
	env.commit("clanker/feature", "work.txt", "work")
	env.push("clanker/feature")

	record := filepath.Join(t.TempDir(), "cred-record.log")
	t.Setenv("CRED_RECORD", record)
	t.Setenv("GH_FAKE_TOKEN", credFakeToken)
	gitShim, ghFake := writeCredShim(t)

	v := New(env.work, "origin")
	v.gitBin = gitShim
	v.ghBin = ghFake
	rep := v.Verify(context.Background(), Claim{Label: "CLA-253", Branch: "clanker/feature"})

	mustStatus(t, rep, BranchPushed, Pass)

	secs := credSections(t, record)
	for _, l := range secs["ls-remote"] {
		if !strings.HasPrefix(l, "count=unset") {
			t.Errorf("local-path ls-remote should carry no credential config, got %q", l)
		}
	}
}

// An owner gh cannot answer for - an org-owned repo read through a personal
// account - falls back to the unscoped invocation, byte-identical to pre-fix
// behavior: the check still runs, just without credentials it does not have.
func TestOwnerWithoutGhTokenFallsBackUnscoped(t *testing.T) {
	env := newEnv(t)
	env.commit("main", "base.txt", "base")
	env.push("main")
	env.checkout("clanker/feature")
	env.commit("clanker/feature", "work.txt", "work")
	env.push("clanker/feature")
	env.git("remote", "set-url", "origin", "https://github.com/some-org/some-repo.git")

	record := filepath.Join(t.TempDir(), "cred-record.log")
	t.Setenv("CRED_RECORD", record)
	t.Setenv("GH_FAKE_FAIL", "1")
	gitShim, ghFake := writeCredShim(t)

	v := New(env.work, "origin")
	v.gitBin = gitShim
	v.ghBin = ghFake
	rep := v.Verify(context.Background(), Claim{Label: "CLA-458", Branch: "clanker/feature"})

	mustStatus(t, rep, BranchPushed, Pass)

	secs := credSections(t, record)
	for _, l := range secs["ls-remote"] {
		if !strings.HasPrefix(l, "count=unset") {
			t.Errorf("unresolvable owner should leave ls-remote unscoped, got %q", l)
		}
	}
}
