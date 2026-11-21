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

// writeCredShim installs two scripts and returns their paths plus the record
// the fake gh writes its argv to: a fake `gh` that answers `auth token --user`
// (or fails on demand), and a `git` shim that appends each invocation's
// subcommand and credential-relevant environment to $CRED_RECORD before either
// serving ls-remote itself (refusing unless the scoping is present, when
// $CRED_REQUIRE_TOKEN names a token) or passing through to the real git.
func writeCredShim(t *testing.T) (gitShim, ghFake, ghRecord string) {
	t.Helper()
	dir := t.TempDir()

	// The fake gh answers the SAME token for every account, so the insteadOf
	// assertions alone cannot tell which account the resolution chain asked
	// for. Its argv is the only evidence, so every call is recorded.
	ghRecord = filepath.Join(dir, "gh-argv.log")

	ghFake = filepath.Join(dir, "gh")
	writeFile(t, ghFake, `#!/bin/sh
printf '%s\n' "$*" >> '`+ghRecord+`'
if [ "$1" = auth ] && [ "$2" = token ]; then
	if [ "$GH_FAKE_FAIL" = 1 ]; then
		echo "no oauth token found for github.com account $4" >&2
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
	return gitShim, ghFake, ghRecord
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

// wantGHProbedOwner asserts the fake gh was asked for owner's token. The shim
// resolves EVERY account to the same token, so the insteadOf assertions cannot
// tell the accounts apart: the recorded argv is what pins WHICH account the
// resolution chain named.
func wantGHProbedOwner(t *testing.T, ghRecord, owner string) {
	t.Helper()
	argv, err := os.ReadFile(ghRecord)
	if err != nil {
		t.Fatalf("gh argv record missing - the fake gh never ran: %v", err)
	}
	if want := "auth token --user " + owner; !strings.Contains(string(argv), want) {
		t.Errorf("gh should have been asked for %s's token, got %q", owner, argv)
	}
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
	gitShim, ghFake, ghRecord := writeCredShim(t)

	v := New(env.work, "origin")
	v.gitBin = gitShim
	v.ghBin = ghFake
	rep := v.Verify(context.Background(), Claim{Label: "CLA-458", Branch: "clanker/private-feature"})

	mustStatus(t, rep, BranchPushed, Pass)

	// The account reached gh: asked for anything else - the active user, the
	// repo name - the shim would still have answered the same token, so only
	// the recorded argv can tell.
	wantGHProbedOwner(t, ghRecord, "lecstor")

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
	gitShim, ghFake, _ := writeCredShim(t)

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
	gitShim, ghFake, ghRecord := writeCredShim(t)

	v := New(env.work, "origin")
	v.gitBin = gitShim
	v.ghBin = ghFake
	rep := v.Verify(context.Background(), Claim{Label: "CLA-458", Branch: "clanker/feature"})

	mustStatus(t, rep, BranchPushed, Pass)

	// gh failed to resolve, but it was asked about the URL's owner - the
	// account the chain derived - not the ambient active one.
	wantGHProbedOwner(t, ghRecord, "some-org")

	secs := credSections(t, record)
	for _, l := range secs["ls-remote"] {
		if !strings.HasPrefix(l, "count=unset") {
			t.Errorf("unresolvable owner should leave ls-remote unscoped, got %q", l)
		}
	}
}

// The probe that ANSWERS "what is this owner's token" must not itself inherit
// the ambient credential. gh reads only its keyring and hosts.yml today, but
// that is an upstream implementation detail: if it ever consulted GH_TOKEN,
// this spawn would answer with the ACTIVE account's token and the scoping
// built on it would silently no-op — invisible to every other test here,
// whose fakes answer from their own variable and ignore the ambient one.
func TestTokenProbeStripsAmbientCredentials(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(t.TempDir(), "probe-record.log")
	t.Setenv("PROBE_RECORD", record)
	t.Setenv("GH_TOKEN", "ambient-decoy-not-the-owners")
	t.Setenv("GITHUB_TOKEN", "ambient-github-decoy")

	gh := filepath.Join(t.TempDir(), "gh")
	writeFile(t, gh, `#!/bin/sh
printf 'GH_TOKEN=%s\n' "${GH_TOKEN-UNSET}" >> "$PROBE_RECORD"
printf 'GITHUB_TOKEN=%s\n' "${GITHUB_TOKEN-UNSET}" >> "$PROBE_RECORD"
printf '%s\n' '`+credFakeToken+`'
`)
	if err := os.Chmod(gh, 0o755); err != nil {
		t.Fatal(err)
	}

	// The probe must still answer, or it is only possible to pass this test by
	// making the spawn fail rather than by stripping anything.
	if got := TokenForOwner(context.Background(), dir, gh, "acme"); got != credFakeToken {
		t.Fatalf("probe must still answer the owner's token, got %q", got)
	}

	body, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("probe record missing - the fake gh never ran: %v", err)
	}
	for _, want := range []string{"GH_TOKEN=UNSET", "GITHUB_TOKEN=UNSET"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("probe child carried the ambient credential: want %s, env was:\n%s", want, body)
		}
	}
}

// TestTokenProbeNamesItsHost pins the probe's argv. `gh auth token` without
// --hostname answers for gh's DEFAULT host, which on a GHES-configured machine
// is a different account's token; the owner is a github.com account by
// construction, so the probe must say so. The exact string also keeps --user at
// $3/$4 - the position every fake gh in this package reads the account from,
// and gh's own "no oauth token ... account X" text echoes.
func TestTokenProbeNamesItsHost(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(t.TempDir(), "probe-argv.log")
	t.Setenv("PROBE_RECORD", record)

	gh := filepath.Join(t.TempDir(), "gh")
	writeFile(t, gh, `#!/bin/sh
printf '%s\n' "$*" >> "$PROBE_RECORD"
printf '%s\n' '`+credFakeToken+`'
`)
	if err := os.Chmod(gh, 0o755); err != nil {
		t.Fatal(err)
	}

	// A failing spawn would pass an argv assertion for the wrong reason.
	if got := TokenForOwner(context.Background(), dir, gh, "acme"); got != credFakeToken {
		t.Fatalf("probe must answer the owner's token, got %q", got)
	}

	body, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("probe record missing - the fake gh never ran: %v", err)
	}
	// The literal host, not githubHost: a test that reads the constant moves with
	// a drift of it and stays green.
	const want = "auth token --user acme --hostname github.com"
	if got := strings.TrimSpace(string(body)); got != want {
		t.Errorf("probe must pin its host, and keep --user at $3/$4:\n want %q\n  got %q", want, got)
	}
}

func TestRedactURLCredentials(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "github x-access-token in git stderr",
			in:   "fatal: unable to access 'https://x-access-token:gho_secret123@github.com/lecstor/repo.git/': Failed to connect",
			want: "fatal: unable to access 'https://redacted@github.com/lecstor/repo.git/': Failed to connect",
		},
		{
			name: "general user:pass and password containing @",
			in:   "error fetching http://alice:p@ss@git.example.com/org/repo.git and https://bob:pw@mirror.local/r",
			want: "error fetching http://redacted@git.example.com/org/repo.git and https://redacted@mirror.local/r",
		},
		{
			name: "preserves scp ssh and @ inside path",
			in:   "git@github-personal:lecstor/repo.git https://github.com/org/email@example.com",
			want: "git@github-personal:lecstor/repo.git https://github.com/org/email@example.com",
		},
	}
	for _, c := range cases {
		if got := redactURLCredentials(c.in); got != c.want {
			t.Errorf("%s:\n want %q\n  got %q", c.name, c.want, got)
		}
	}
}

func TestGitRunOutRedactsURLCredentialsFromStderr(t *testing.T) {
	const secret = "gho_super_secret_token_966"
	dir := t.TempDir()
	fakeGit := filepath.Join(t.TempDir(), "git")
	writeFile(t, fakeGit, `#!/bin/sh
echo "fatal: unable to access 'https://x-access-token:`+secret+`@github.com/lecstor/private.git/': Could not resolve host: github.com" >&2
exit 128
`)
	if err := os.Chmod(fakeGit, 0o755); err != nil {
		t.Fatal(err)
	}

	v := New(dir, "origin")
	v.gitBin = fakeGit
	_, err := v.gitRunOut(context.Background(), dir, ScopeEnv(secret), "fetch", "origin")
	if err == nil {
		t.Fatal("expected error from failing git command")
	}
	msg := err.Error()
	if strings.Contains(msg, secret) || strings.Contains(msg, "x-access-token:") {
		t.Fatalf("gitRunOut leaked credential in error: %q", msg)
	}
	if !strings.Contains(msg, "https://redacted@github.com/lecstor/private.git/") {
		t.Fatalf("gitRunOut should preserve redacted URL for diagnosis, got %q", msg)
	}
}

func TestGitRunOutStripsInheritedGitConfigAndOwnedVars(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "user.name")
	t.Setenv("GIT_CONFIG_VALUE_0", "stale-user")
	t.Setenv("GIT_CONFIG_KEY_1", "user.email")
	t.Setenv("GIT_CONFIG_VALUE_1", "stale@example.com")
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	t.Setenv("GIT_ASKPASS", "/bin/evil")
	t.Setenv("SSH_ASKPASS", "/bin/evil")
	t.Setenv("GIT_SSH_COMMAND", "ssh -oBatchMode=no")
	t.Setenv("GCM_INTERACTIVE", "always")

	dir := t.TempDir()
	fakeGit := filepath.Join(t.TempDir(), "git")
	writeFile(t, fakeGit, "#!/bin/sh\nenv\n")
	if err := os.Chmod(fakeGit, 0o755); err != nil {
		t.Fatal(err)
	}

	v := New(dir, "origin")
	v.gitBin = fakeGit

	out, err := v.gitRunOut(context.Background(), dir, ScopeEnv(credFakeToken), "fetch", "origin")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	counts := map[string][]string{}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		k := envName(line)
		counts[k] = append(counts[k], line)
	}
	for k, want := range map[string]string{
		"GIT_CONFIG_COUNT":    "GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0":    "GIT_CONFIG_KEY_0=" + wantKey0,
		"GIT_CONFIG_VALUE_0":  "GIT_CONFIG_VALUE_0=" + wantVal0,
		"GIT_TERMINAL_PROMPT": "GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS":         "GIT_ASKPASS=echo",
		"SSH_ASKPASS":         "SSH_ASKPASS=echo",
		"GIT_SSH_COMMAND":     "GIT_SSH_COMMAND=ssh -oBatchMode=yes -oConnectTimeout=10",
		"GCM_INTERACTIVE":     "GCM_INTERACTIVE=never",
	} {
		if got := counts[k]; len(got) != 1 || got[0] != want {
			t.Errorf("%s = %v, want single [%q]", k, got, want)
		}
	}
	for _, absent := range []string{"GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1"} {
		if got := counts[absent]; len(got) != 0 {
			t.Errorf("inherited %s should be stripped, got %v", absent, got)
		}
	}

	// Unscoped call must carry no inherited GIT_CONFIG_* at all.
	unscoped, err := v.gitRunOut(context.Background(), dir, nil, "status")
	if err != nil {
		t.Fatalf("unscoped unexpected error: %v", err)
	}
	for _, line := range strings.Split(unscoped, "\n") {
		if isGitConfigEnv(envName(line)) {
			t.Errorf("unscoped gitRunOut leaked inherited git config env: %q", line)
		}
	}
}
