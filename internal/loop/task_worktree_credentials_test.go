package loop

// CLA-970: prepareTaskWorktree's fetch must reach a private HTTPS github.com
// origin as the account that owns it, not as whichever gh account is ACTIVE.
// `gh auth git-credential` serves the active account, so on a multi-account
// machine the fetch - and therefore every fact the prep is built on - fails
// with "Repository not found".
//
// The tests drive the PRODUCTION taskWorktreeGit through a recording `git`
// stand-in that REFUSES the fetch unless it carries exactly the scoping CLA-458
// specifies, and a fake `gh` that answers the owner's token. A green prep is
// reachable only through the chain: read origin's URL, derive the owner,
// resolve THAT account's token via `gh auth token --user`, hand git the
// insteadOf rewrite in this one invocation's environment.

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
	// taskWorktreeCredRemoteURL is the private-looking origin the tests give the
	// fixture: HTTPS github.com naming an owner that is not the machine's active
	// gh account.
	taskWorktreeCredRemoteURL = "https://github.com/lecstor/private-repo.git"

	// taskWorktreeCredOwner is the account the URL names, and the one gh must be
	// asked about.
	taskWorktreeCredOwner = "lecstor"

	// taskWorktreeCredToken is what the fake gh answers with; the git stand-in
	// asserts git received exactly this token.
	taskWorktreeCredToken = "gho_cla970-worktree-token-not-a-secret"

	// taskWorktreeCredWantKey0/taskWorktreeCredWantVal0 spell the insteadOf
	// rewrite the scoping must produce.
	taskWorktreeCredWantKey0 = "url.https://x-access-token:" + taskWorktreeCredToken + "@github.com/.insteadOf"
	taskWorktreeCredWantVal0 = "https://github.com/"
)

// worktreeCredCall is one git invocation the stand-in recorded.
type worktreeCredCall struct {
	cmd    string
	fields map[string]string
}

// taskWorktreeCredShims are the fakes and the files they record to.
type taskWorktreeCredShims struct {
	binDir string
	gitLog string
	ghLog  string
}

// installTaskWorktreeCredShims writes a recording git stand-in and a fake gh into
// one bin dir, which the caller must put first on PATH: taskWorktreeGit execs
// "git" off PATH. The real git is resolved first and baked in by absolute path,
// so pass-throughs cannot re-enter the stand-in.
func installTaskWorktreeCredShims(t *testing.T) taskWorktreeCredShims {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("resolve real git before installing the stand-in: %v", err)
	}
	dir := t.TempDir()
	s := taskWorktreeCredShims{
		binDir: dir,
		gitLog: filepath.Join(t.TempDir(), "git.log"),
		ghLog:  filepath.Join(t.TempDir(), "gh.log"),
	}
	gitBody := strings.NewReplacer("__REAL_GIT__", realGit, "__GIT_LOG__", s.gitLog).Replace(`#!/bin/sh
# A stand-in git for the CLA-970 worktree tests: records the credentials each
# invocation received, refuses the fetch unless it is scoped, and passes
# everything else through to the real git. taskWorktreeGit always prefixes
# -C <dir>, so the subcommand is $3.
cmd="$3"
printf '=== %s\n' "$cmd" >> "__GIT_LOG__"
printf 'args=%s\n' "$*" >> "__GIT_LOG__"
printf 'count=%s\n' "${GIT_CONFIG_COUNT-unset}" >> "__GIT_LOG__"
[ -n "${GIT_CONFIG_KEY_0+x}" ] && printf 'key0=%s\n' "$GIT_CONFIG_KEY_0" >> "__GIT_LOG__"
[ -n "${GIT_CONFIG_VALUE_0+x}" ] && printf 'val0=%s\n' "$GIT_CONFIG_VALUE_0" >> "__GIT_LOG__"
printf 'keys=%s\n' "$(env | grep -c '^GIT_CONFIG_KEY_')" >> "__GIT_LOG__"

if [ "$cmd" = fetch ]; then
	case "$GIT_CONFIG_KEY_0" in
	*"x-access-token:$TASKWT_CRED_TOKEN@github.com/"*) ;;
	*)
		echo "remote: Repository not found." >&2
		exit 128
		;;
	esac
	if [ -n "${TASKWT_CRED_FAIL_FETCH:-}" ]; then
		echo "fatal: unable to access 'https://x-access-token:$TASKWT_CRED_TOKEN@github.com/lecstor/private-repo.git/': The requested URL returned error: 502" >&2
		exit 128
	fi
	# The remote is unreachable offline; the clone already carries the refs the
	# prep reads, so a scoped fetch succeeds without touching the network.
	exit 0
fi

exec "__REAL_GIT__" "$@"
`)
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(gitBody), 0o755); err != nil {
		t.Fatal(err)
	}
	ghBody := strings.NewReplacer("__GH_LOG__", s.ghLog).Replace(`#!/bin/sh
# A fake gh: records every invocation, answers the named account's token, and
# refuses anything else - so a scoping implementation that mutated gh's auth
# state (login/switch) fails loudly instead of passing.
printf '%s\n' "$*" >> "__GH_LOG__"
if [ "$1" = auth ] && [ "$2" = token ] && [ "$3" = --user ]; then
	printf '%s\n' "$TASKWT_CRED_TOKEN"
	exit 0
fi
echo "unexpected gh invocation: $*" >&2
exit 42
`)
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(ghBody), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TASKWT_CRED_TOKEN", taskWorktreeCredToken)
	return s
}

// useTaskWorktreeCredShims puts the fakes first on PATH.
func useTaskWorktreeCredShims(t *testing.T, s taskWorktreeCredShims) {
	t.Helper()
	t.Setenv("PATH", s.binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// worktreeCredCalls reads the stand-in's log into one entry per invocation.
func worktreeCredCalls(t *testing.T, log string) []worktreeCredCall {
	t.Helper()
	body, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("credential record missing - the stand-in never ran: %v", err)
	}
	var calls []worktreeCredCall
	var cur *worktreeCredCall
	for _, line := range strings.Split(string(body), "\n") {
		if line == "" {
			continue
		}
		if cmd, ok := strings.CutPrefix(line, "=== "); ok {
			calls = append(calls, worktreeCredCall{cmd: cmd, fields: map[string]string{}})
			cur = &calls[len(calls)-1]
			continue
		}
		if name, value, ok := strings.Cut(line, "="); ok && cur != nil {
			cur.fields[name] = value
		}
	}
	return calls
}

func worktreeCredCallsNamed(calls []worktreeCredCall, cmd string) []worktreeCredCall {
	var out []worktreeCredCall
	for _, c := range calls {
		if c.cmd == cmd {
			out = append(out, c)
		}
	}
	return out
}

// assertWorktreeCredScoped pins exactly the environment CLA-458's scoping
// produces, including that an inherited GIT_CONFIG_* set did not survive.
func assertWorktreeCredScoped(t *testing.T, c worktreeCredCall) {
	t.Helper()
	if got := c.fields["count"]; got != "1" {
		t.Errorf("%s carried GIT_CONFIG_COUNT %q, want 1", c.cmd, got)
	}
	if got := c.fields["key0"]; got != taskWorktreeCredWantKey0 {
		t.Errorf("%s carried key0 %q, want %q", c.cmd, got, taskWorktreeCredWantKey0)
	}
	if got := c.fields["val0"]; got != taskWorktreeCredWantVal0 {
		t.Errorf("%s carried val0 %q, want %q", c.cmd, got, taskWorktreeCredWantVal0)
	}
	if got := c.fields["keys"]; got != "1" {
		t.Errorf("%s saw %s GIT_CONFIG_KEY_* entries, want exactly the scoped one", c.cmd, got)
	}
}

// TestTaskWorktreeFetchScopesToTheRemoteOwner is THE pin: the stand-in refuses
// the fetch unless it carries the insteadOf rewrite naming the token our gh
// answered with, so a prepared worktree is reachable solely through the
// resolution chain.
func TestTaskWorktreeFetchScopesToTheRemoteOwner(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	gitRun(t, f.checkout, "remote", "set-url", "origin", taskWorktreeCredRemoteURL)
	shims := installTaskWorktreeCredShims(t)
	useTaskWorktreeCredShims(t, shims)
	// An inherited git config channel must not shadow the scoped rewrite.
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "url.https://shadow.invalid/.insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", "https://shadow.invalid/")
	t.Setenv("GIT_CONFIG_KEY_1", "credential.helper")
	t.Setenv("GIT_CONFIG_VALUE_1", "!false")
	t.Setenv("GIT_CONFIG_PARAMETERS", "'credential.helper=!false'")

	prep, err := f.prepare(t, f.request())
	if err != nil {
		t.Fatalf("prepare over a private HTTPS github origin failed: %v - the fetch was not scoped to the remote owner", err)
	}
	if want := filepath.Join(f.checkout+"-wt", taskWorktreeID[:8]); prep.Dir != want {
		t.Errorf("Dir = %q, want %q", prep.Dir, want)
	}
	// Through the production runner, not gitOut: gitOut would hit the stand-in
	// directly, with the test's own (deliberately polluted) environment.
	if got, err := taskWorktreeGit(context.Background(), prep.Dir, "rev-parse", "--abbrev-ref", "HEAD"); err != nil || got != taskWorktreeBranch {
		t.Errorf("worktree is on %q (%v), want %q", got, err, taskWorktreeBranch)
	}

	calls := worktreeCredCalls(t, shims.gitLog)
	fetches := worktreeCredCallsNamed(calls, "fetch")
	if len(fetches) != 1 {
		t.Fatalf("%d fetch invocation(s) recorded, want exactly 1", len(fetches))
	}
	if args := fetches[0].fields["args"]; !strings.Contains(args, "fetch origin --prune") {
		t.Errorf("fetch invocation = %q, want `fetch origin --prune`", args)
	}
	assertWorktreeCredScoped(t, fetches[0])

	// Scoping rides the network command only.
	for _, c := range calls {
		if c.cmd == "fetch" {
			continue
		}
		if c.fields["count"] != "unset" || c.fields["keys"] != "0" {
			t.Errorf("local %s invocation carried credential scoping: %v", c.cmd, c.fields)
		}
	}

	// gh was asked for the URL owner's token and nothing else: the fake refuses
	// any other subcommand, so a global auth mutation cannot pass silently.
	body, err := os.ReadFile(shims.ghLog)
	if err != nil {
		t.Fatalf("gh was never consulted: %v", err)
	}
	want := "auth token --user " + taskWorktreeCredOwner + " --hostname github.com"
	if lines := strings.Split(strings.TrimSpace(string(body)), "\n"); len(lines) != 1 || lines[0] != want {
		t.Errorf("gh invocations = %q, want exactly one %q", lines, want)
	}
}

// A failed scoped fetch can quote the token-bearing URL on stderr, and the prep
// error is logged and reported to the plane. The credential must not survive it,
// while the redacted URL stays for diagnosis.
func TestTaskWorktreeFetchRedactsTheScopedToken(t *testing.T) {
	f := newTaskWorktreeRepo(t)
	gitRun(t, f.checkout, "remote", "set-url", "origin", taskWorktreeCredRemoteURL)
	shims := installTaskWorktreeCredShims(t)
	useTaskWorktreeCredShims(t, shims)
	t.Setenv("TASKWT_CRED_FAIL_FETCH", "1")

	_, err := f.prepare(t, f.request())
	if err == nil {
		t.Fatal("a failing fetch must fail the prep")
	}
	if !errors.Is(err, errWorktreeFetch) {
		t.Errorf("err = %v, want it wrapped in errWorktreeFetch so the caller retries instead of counting a refusal", err)
	}
	msg := err.Error()
	if strings.Contains(msg, taskWorktreeCredToken) || strings.Contains(msg, "x-access-token:") {
		t.Fatalf("the prep error leaked the scoped token: %q", msg)
	}
	if !strings.Contains(msg, "https://redacted@github.com/lecstor/private-repo.git/") {
		t.Errorf("the error should keep the redacted URL for diagnosis, got %q", msg)
	}
}
