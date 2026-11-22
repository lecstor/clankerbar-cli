package salvage

// CLA-970: salvage's two network commands (ls-remote and push) must reach a
// private HTTPS github.com remote as the account that owns it, not as whichever
// gh account is ACTIVE on the machine. `gh auth git-credential` serves the
// active account, so on a multi-account host the push this package exists to
// make answers "Repository not found" and the rescue is defeated (CLA-314).
//
// The tests do not mock git's opinion away: a recording `git` stand-in REFUSES
// the network subcommands unless they carry exactly the credential scoping
// CLA-458 specifies, and a fake `gh` answers the owner's token. A green salvage
// is reachable only through the production chain: read the remote URL, derive
// the owner, resolve THAT account's token via `gh auth token --user`, hand git
// the insteadOf rewrite in its environment. Local commands must stay unscoped,
// and the record proves it.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	// salvageCredRemoteURL is the private-looking origin the tests give the
	// fixture once it is built: HTTPS github.com naming an owner that is not the
	// test machine's active gh account.
	salvageCredRemoteURL = "https://github.com/lecstor/private-repo.git"

	// salvageCredOwner is the account the URL names, and the one gh must be
	// asked about.
	salvageCredOwner = "lecstor"

	// salvageCredToken is what the fake gh answers with. The git stand-in
	// asserts git received exactly this token in GIT_CONFIG_KEY_0, so the chain
	// "derive owner -> resolve THAT account -> plumb the resolved value" is
	// pinned end to end.
	salvageCredToken = "gho_cla970-test-token-not-a-secret"

	// salvageCredWantKey0/salvageCredWantVal0 spell the insteadOf rewrite the
	// scoping must produce, keyed to that token.
	salvageCredWantKey0 = "url.https://x-access-token:" + salvageCredToken + "@github.com/.insteadOf"
	salvageCredWantVal0 = "https://github.com/"
)

// credCall is one git invocation the stand-in recorded: the subcommand it
// classified and the environment fields the assertions read.
type credCall struct {
	cmd    string
	fields map[string]string
}

// salvageCredShims are the two fakes and the files they record to.
type salvageCredShims struct {
	git    string
	gh     string
	gitLog string
	ghLog  string
}

// installSalvageCredShims writes the git stand-in and the fake gh for one test,
// and exports the values they read. The real git is resolved BEFORE the stand-in
// is installed and baked into it by absolute path, so its pass-throughs cannot
// re-enter themselves.
func installSalvageCredShims(t *testing.T) salvageCredShims {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("resolve real git before installing the stand-in: %v", err)
	}
	dir := t.TempDir()
	s := salvageCredShims{
		git:    filepath.Join(dir, "git-standin.sh"),
		gh:     filepath.Join(dir, "gh-fake.sh"),
		gitLog: filepath.Join(dir, "git.log"),
		ghLog:  filepath.Join(dir, "gh.log"),
	}
	gitBody := strings.NewReplacer("__REAL_GIT__", realGit, "__GIT_LOG__", s.gitLog).Replace(`#!/bin/sh
# A stand-in git for the CLA-970 credential tests: records the credentials each
# invocation received, refuses the network subcommands unless scoped, and passes
# everything else through to the real git.
cmd="$1"
case " $* " in
*" push "*) cmd=push ;;
*" ls-remote "*) cmd=ls-remote ;;
esac
printf '=== %s\n' "$cmd" >> "__GIT_LOG__"
printf 'args=%s\n' "$*" >> "__GIT_LOG__"
printf 'count=%s\n' "${GIT_CONFIG_COUNT-unset}" >> "__GIT_LOG__"
[ -n "${GIT_CONFIG_KEY_0+x}" ] && printf 'key0=%s\n' "$GIT_CONFIG_KEY_0" >> "__GIT_LOG__"
[ -n "${GIT_CONFIG_VALUE_0+x}" ] && printf 'val0=%s\n' "$GIT_CONFIG_VALUE_0" >> "__GIT_LOG__"
printf 'keys=%s\n' "$(env | grep -c '^GIT_CONFIG_KEY_')" >> "__GIT_LOG__"

case "$cmd" in
push|ls-remote)
	case "$GIT_CONFIG_KEY_0" in
	*"x-access-token:$SALVAGE_CRED_TOKEN@github.com/"*) ;;
	*)
		echo "remote: Repository not found." >&2
		exit 128
		;;
	esac
	;;
esac

if [ -n "${SALVAGE_CRED_FAIL_PUSH:-}" ] && [ "$cmd" = push ]; then
	echo "fatal: unable to access 'https://x-access-token:$SALVAGE_CRED_TOKEN@github.com/lecstor/private-repo.git/': The requested URL returned error: 502" >&2
	echo "error: RPC failed; HTTP 400 curl 22 The requested URL returned error: 400" >&2
	exit 1
fi

# The stand-in plays the private host: a scoped network command is replayed
# against the local bare repository, so scoping is the only way it can succeed.
# $1=subcommand $2/$3 its flags, $4=<remote> $5=<ref or refspec>.
case "$cmd" in
ls-remote) exec "__REAL_GIT__" -C "$PWD" ls-remote --heads -- "$SALVAGE_CRED_LOCAL_REMOTE" "$5" ;;
push) exec "__REAL_GIT__" -C "$PWD" push --no-verify -- "$SALVAGE_CRED_LOCAL_REMOTE" "$5" ;;
esac

exec "__REAL_GIT__" "$@"
`)
	if err := os.WriteFile(s.git, []byte(gitBody), 0o755); err != nil {
		t.Fatal(err)
	}
	ghBody := strings.NewReplacer("__GH_LOG__", s.ghLog).Replace(`#!/bin/sh
# A fake gh: records every invocation, answers the named account's token, and
# refuses anything else - so a scoping implementation that mutated gh's auth
# state (login/switch) would fail loudly instead of passing.
printf '%s\n' "$*" >> "__GH_LOG__"
if [ "${SALVAGE_CRED_GH_FAIL:-}" = 1 ]; then
	echo "no oauth token found for github.com account $4" >&2
	exit 1
fi
if [ "$1" = auth ] && [ "$2" = token ] && [ "$3" = --user ]; then
	printf '%s\n' "$SALVAGE_CRED_TOKEN"
	exit 0
fi
echo "unexpected gh invocation: $*" >&2
exit 42
`)
	if err := os.WriteFile(s.gh, []byte(ghBody), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SALVAGE_CRED_TOKEN", salvageCredToken)
	return s
}

// useSalvageCredShims points one salvager at the fakes and exports the values
// the git stand-in reads.
func useSalvageCredShims(t *testing.T, e *env, s salvageCredShims) *Salvager {
	t.Helper()
	t.Setenv("SALVAGE_CRED_RECORD", s.gitLog)
	t.Setenv("SALVAGE_CRED_LOCAL_REMOTE", e.remote)
	salvager := New(e.root, "")
	salvager.gitBin = s.git
	salvager.ghBin = s.gh
	return salvager
}

// credCalls reads the stand-in's log into one entry per invocation.
func credCalls(t *testing.T, log string) []credCall {
	t.Helper()
	body, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("credential record missing - the stand-in never ran: %v", err)
	}
	var calls []credCall
	var cur *credCall
	for _, line := range strings.Split(string(body), "\n") {
		if line == "" {
			continue
		}
		if cmd, ok := strings.CutPrefix(line, "=== "); ok {
			calls = append(calls, credCall{cmd: cmd, fields: map[string]string{}})
			cur = &calls[len(calls)-1]
			continue
		}
		if name, value, ok := strings.Cut(line, "="); ok && cur != nil {
			cur.fields[name] = value
		}
	}
	return calls
}

func credCallsNamed(calls []credCall, cmd string) []credCall {
	var out []credCall
	for _, c := range calls {
		if c.cmd == cmd {
			out = append(out, c)
		}
	}
	return out
}

// assertScoped pins exactly the environment CLA-458's scoping must produce: one
// GIT_CONFIG entry (an inherited GIT_CONFIG_KEY_* set is stripped, so the count
// is not 2), keyed to the owner's token.
func assertScoped(t *testing.T, c credCall) {
	t.Helper()
	if got := c.fields["count"]; got != "1" {
		t.Errorf("%s carried GIT_CONFIG_COUNT %q, want 1", c.cmd, got)
	}
	if got := c.fields["key0"]; got != salvageCredWantKey0 {
		t.Errorf("%s carried key0 %q, want %q", c.cmd, got, salvageCredWantKey0)
	}
	if got := c.fields["val0"]; got != salvageCredWantVal0 {
		t.Errorf("%s carried val0 %q, want %q", c.cmd, got, salvageCredWantVal0)
	}
	if got := c.fields["keys"]; got != "1" {
		t.Errorf("%s saw %s GIT_CONFIG_KEY_* entries in its environment, want exactly the scoped one", c.cmd, got)
	}
}

// assertOnlyNetworkCommandsWereScoped is the other half of the fix: scoping
// rides the network invocations only. Every local command - remote reads,
// status, add, commit, rev-parse - must have gone out with no credential config
// at all.
func assertOnlyNetworkCommandsWereScoped(t *testing.T, calls []credCall) {
	t.Helper()
	for _, c := range calls {
		if c.cmd == "push" || c.cmd == "ls-remote" {
			continue
		}
		if c.fields["count"] != "unset" || c.fields["keys"] != "0" {
			t.Errorf("local %s invocation carried credential scoping: %v", c.cmd, c.fields)
		}
	}
}

// assertGHReadOnlyTokenProbe pins that gh was asked for the URL owner's token
// and nothing else: a scoping implementation that mutated global gh auth would
// have to call another subcommand, which the fake refuses.
func assertGHReadOnlyTokenProbe(t *testing.T, log string) {
	t.Helper()
	body, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("gh was never consulted: %v", err)
	}
	want := "auth token --user " + salvageCredOwner + " --hostname github.com"
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 1 || lines[0] != want {
		t.Errorf("gh invocations = %q, want exactly one %q", lines, want)
	}
}

// THE push pin: a private github.com origin usable only as the account that owns
// it. The stand-in refuses the push unless the invocation carries the insteadOf
// rewrite naming the token our gh answered with, so a saved outcome is reachable
// solely through the resolution chain.
func TestSalvageScopesThePushToTheRemoteOwner(t *testing.T) {
	e := newEnv(t)
	// The fixture's origin is a local path; the production failure only happens
	// over HTTPS. The stand-in plays the private host, so the scoped push is the
	// only one that can land.
	git(t, e.repo, "remote", "set-url", "origin", salvageCredRemoteURL)
	shims := installSalvageCredShims(t)
	salvager := useSalvageCredShims(t, e, shims)
	// An inherited git config channel must not shadow the scoped rewrite: git
	// must receive the scoped count/key/value and nothing else.
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "url.https://shadow.invalid/.insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", "https://shadow.invalid/")
	t.Setenv("GIT_CONFIG_KEY_1", "credential.helper")
	t.Setenv("GIT_CONFIG_VALUE_1", "!false")
	t.Setenv("GIT_CONFIG_PARAMETERS", "'credential.helper=!false'")
	write(t, filepath.Join(e.wt, "work.txt"), "hours of it")

	out := salvager.Salvage(context.Background(), taskID, "CLA-970")

	if !out.Saved() {
		t.Fatalf("status = %q (%s), want saved - the scoped push is what makes the rescue land", out.Status, out.Detail)
	}
	if got := git(t, e.remote, "rev-parse", "refs/heads/"+e.branch); got != out.Commit {
		t.Errorf("the bare origin holds %q on %s, want the salvage commit %q", got, e.branch, out.Commit)
	}

	calls := credCalls(t, shims.gitLog)
	pushes := credCallsNamed(calls, "push")
	if len(pushes) != 1 {
		t.Fatalf("%d push invocation(s) recorded, want exactly 1", len(pushes))
	}
	assertScoped(t, pushes[0])
	assertOnlyNetworkCommandsWereScoped(t, calls)
	assertGHReadOnlyTokenProbe(t, shims.ghLog)
}

// delivered's ls-remote is the other network command: the untracked-only rule
// (CLA-695) asks the remote whether the recorded branch is at the worktree's tip.
// Unscoped, that read answers "Repository not found", delivery is judged false,
// and the untracked file is committed and pushed - the exact tip nobody chose.
func TestSalvageScopesTheDeliveredCheckToTheRemoteOwner(t *testing.T) {
	e := newEnv(t)
	// The session delivered: committed, pushed, and recorded its branch.
	write(t, filepath.Join(e.wt, "the-work.go"), "package work")
	git(t, e.wt, "add", ".")
	git(t, e.wt, "commit", "-m", "the work")
	git(t, e.wt, "push", "origin", "HEAD:refs/heads/"+e.branch)
	delivered := git(t, e.wt, "rev-parse", "HEAD")
	git(t, e.repo, "remote", "set-url", "origin", salvageCredRemoteURL)
	shims := installSalvageCredShims(t)
	salvager := useSalvageCredShims(t, e, shims)
	write(t, filepath.Join(e.wt, "worker-configuration.d.ts"), "generated")

	out := salvager.Salvage(context.Background(), taskID, "CLA-970", e.branch)

	if out.Status != Untracked {
		t.Fatalf("status = %q (%s), want %q - the scoped ls-remote is what proves delivery", out.Status, out.Detail, Untracked)
	}
	if got := git(t, e.wt, "rev-parse", "HEAD"); got != delivered {
		t.Errorf("HEAD moved to %s, want the delivered tip %s untouched", got, delivered)
	}

	calls := credCalls(t, shims.gitLog)
	reads := credCallsNamed(calls, "ls-remote")
	if len(reads) != 1 {
		t.Fatalf("%d ls-remote invocation(s) recorded, want exactly 1", len(reads))
	}
	assertScoped(t, reads[0])
	assertOnlyNetworkCommandsWereScoped(t, calls)
	assertGHReadOnlyTokenProbe(t, shims.ghLog)
}

// A scoped command's token rides the URL git echoes when it fails, and that
// stderr becomes the outcome detail a driver logs. The retry's detail is the one
// path that carries a full stderr through, so it is driven end to end here; the
// unit pin below covers the plain runners' error shape.
func TestSalvageRedactsTheScopedTokenFromAFailedPush(t *testing.T) {
	e := newEnv(t)
	git(t, e.repo, "remote", "set-url", "origin", salvageCredRemoteURL)
	shims := installSalvageCredShims(t)
	salvager := useSalvageCredShims(t, e, shims)
	t.Setenv("SALVAGE_CRED_FAIL_PUSH", "1")
	write(t, filepath.Join(e.wt, "work.txt"), "hours of it")

	out := salvager.Salvage(context.Background(), taskID, "CLA-970")

	if out.Status != Failed {
		t.Fatalf("status = %q (%s), want failed", out.Status, out.Detail)
	}
	if strings.Contains(out.Detail, salvageCredToken) || strings.Contains(out.Detail, "x-access-token:") {
		t.Fatalf("the outcome leaked the scoped token: %s", out.Detail)
	}
	if !strings.Contains(out.Detail, "https://redacted@github.com/lecstor/private-repo.git/") {
		t.Errorf("the detail should keep the redacted URL for diagnosis, got: %s", out.Detail)
	}
	// The retry reaches the same remote, so it is scoped the same way.
	if pushes := credCallsNamed(credCalls(t, shims.gitLog), "push"); len(pushes) != 2 {
		t.Errorf("%d push invocation(s) recorded, want 2 (the original and the HTTP/1.1 retry)", len(pushes))
	}
}

// The error shapes every runner produces redact the URL userinfo. runError is
// run's shape and errorLine is the detail's, and both can quote a failing
// scoped command's stderr, so a credential must not survive either.
func TestSalvageGitErrorsRedactURLCredentials(t *testing.T) {
	const secret = "gho_super_secret_token_970"
	credentialed := "fatal: unable to access 'https://x-access-token:" + secret + "@github.com/lecstor/private-repo.git/': The requested URL returned error: 502"

	err := runError("push", errors.New("exit status 128"), credentialed)
	if msg := err.Error(); strings.Contains(msg, secret) || strings.Contains(msg, "x-access-token:") {
		t.Errorf("runError leaked the credential: %q", msg)
	} else if !strings.Contains(msg, "https://redacted@github.com/lecstor/private-repo.git/") {
		t.Errorf("runError should keep the redacted URL for diagnosis, got %q", msg)
	}

	if got := errorLine(credentialed); strings.Contains(got, secret) || strings.Contains(got, "x-access-token:") {
		t.Errorf("errorLine leaked the credential: %q", got)
	} else if !strings.Contains(got, "https://redacted@github.com/lecstor/private-repo.git/") {
		t.Errorf("errorLine should keep the redacted URL for diagnosis, got %q", got)
	}
}
