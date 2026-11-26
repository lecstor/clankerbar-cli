package delivery

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/secureurl"
)

// run executes one git command in dir and returns its trimmed stdout.
//
// The environment is deliberately hostile to interaction: an unattended run has
// no terminal, and a git that decides to ask for a password would hang the driver
// rather than answering the question.
func (v *Verifier) run(ctx context.Context, dir string, args ...string) (string, error) {
	return v.gitRun(ctx, dir, nil, args...)
}

// runScoped is run for the NETWORK commands: it adds the credential scoping
// the resolved remote implies (remoteCredEnv), so ls-remote and fetch read a
// private github.com remote as an account that can see it. Local operations
// must stay on run: scoping costs a `gh auth token` spawn and local commands
// never consult credentials.
func (v *Verifier) runScoped(ctx context.Context, dir string, args ...string) (string, error) {
	return v.gitRun(ctx, dir, v.remoteCredEnv(ctx, dir), args...)
}

// gitRun is run's engine, with extraEnv appended to the child's environment.
// The WaitDelay discipline is not decoration: `git ls-remote` and `git fetch`
// spawn helpers (ssh, git-remote-https, a credential helper) that inherit the
// pipes behind these buffers, and os/exec's Wait blocks until every inheritor
// closes its write end — so a killed git can still leave the call sitting
// there. Measured at 8s against a 500ms deadline, returning a nil error, which
// would have read as a successful check. WaitDelay is what actually cuts it
// off, and the truncated read is reported as the failure it is.
func (v *Verifier) gitRun(ctx context.Context, dir string, extraEnv []string, args ...string) (string, error) {
	out, err := v.gitRunOut(ctx, dir, extraEnv, args...)
	if err != nil {
		return "", err
	}
	return out, nil
}

// gitRunOut is gitRun that also returns the child's stdout when it FAILED, for the
// commands whose exit status is part of their answer: `merge-tree` exits 1 when the
// merge conflicts, and the conflicting paths are on the stdout that status came with.
// Every other caller wants gitRun, whose contract is "no output with an error".
func (v *Verifier) gitRunOut(ctx context.Context, dir string, extraEnv []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, v.gitBin, args...)
	cmd.Dir = dir
	cmd.Env = GitEnv(os.Environ(), extraEnv)
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return stdout.String(), fmt.Errorf("git %s: %w: %s", args[0], err, secureurl.RedactUserinfo(firstLine(msg)))
		}
		return stdout.String(), fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// --- credential resolution for remote reads (CLA-458) ------------------------
//
// A driven repository's origin may be an HTTPS URL that names no account
// (`https://github.com/lecstor/ezyapp.git`). git then asks the ambient
// credential helper without naming one either, and `gh auth git-credential`
// serves whichever gh account is ACTIVE - which need not see the private repo,
// so ls-remote answers "Repository not found", every check degrades to Unknown,
// and since CLA-457 that Unknown is fail-closed: evidence that cannot run is
// not evidence. The sibling repos whose origins DO embed their owner never hit
// this, which is why the breakage looked like one repo being haunted.
//
// The fix rides the one GitHub credential mechanism this package already
// depends on - `gh`, the same binary prCheck drives - rather than inventing a
// second store: derive the owner from the remote URL, resolve THAT account's
// token with `gh auth token --user`, and let git read the remote AS the owner
// through a per-command insteadOf rewrite. Scoping by owner is load-bearing:
// probed on gh 2.96.0, the credential helper refuses to serve a NON-active
// account even when asked by name (an empty answer for username=lecstor while
// username-less requests serve the active user), so credential.<url>.username
// cannot carry this.
//
// The rewrite travels in the child's ENVIRONMENT (GIT_CONFIG_COUNT and
// friends), never its argv: `-c url.<token>@github.com/...` would print the
// token to ps(1) for every process on the box, while secrets in a child's env
// are already this driver's hygiene model - CLANKERBAR_API_KEY rides the same
// way. Nothing is persisted and no repository config is touched; the scope is
// exactly this one git invocation.
//
// Every fallback below lands on "run unscoped" - byte-identical to pre-CLA-458
// behavior - so fail-open stays fail-open: non-github hosts, ssh remotes,
// owners with no gh token, and a failing or absent gh all degrade to today.

const (
	// insteadOfBase is the URL prefix git rewrites remote URLs INTO when the
	// credential scoping applies: the token rides as the userinfo of a
	// x-access-token URL, the convention GitHub Actions checkout established
	// and GitHub's HTTPS endpoint accepts for OAuth tokens.
	insteadOfBase = "url.https://x-access-token:%s@github.com/.insteadOf"

	// insteadOfPrefix is WHAT gets rewritten: any https://github.com/ URL this
	// invocation resolves - including through a remote name - reads as the
	// token'd base above.
	insteadOfPrefix = "https://github.com/"

	// githubHost is the host every owner this package scopes belongs to:
	// githubOwner derives an owner from github.com URLs only. The token probe
	// names it explicitly because `gh auth token` otherwise answers for gh's
	// DEFAULT host - on a machine whose gh default is a GHES instance that is
	// a different account's token, silently scoping the wrong credential.
	githubHost = "github.com"
)

// githubOwner extracts the account that owns a github.com HTTPS remote URL:
// the first path segment of https://github.com/<owner>/<repo>. Empty for
// anything else, because empty means "no scoping known": ssh remotes
// authenticate with keys, other hosts are not gh accounts, GitHub Enterprise
// would need its own gh hostname, and an owner-less URL has nothing to derive.
func githubOwner(remoteURL string) string {
	u, err := url.Parse(remoteURL)
	if err != nil {
		return ""
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return ""
	}
	if !strings.EqualFold(u.Hostname(), "github.com") {
		return ""
	}
	owner := strings.TrimPrefix(u.Path, "/")
	if i := strings.IndexByte(owner, '/'); i >= 0 {
		owner = owner[:i]
	}
	return owner
}

// --- exported scoping helpers (CLA-459) ---------------------------------------
//
// Doctor's deploy_lag check (internal/cli/deploy_lag.go) reads remotes with
// ls-remote and fetch too - it measures deploy lag against the REMOTE tip of
// the integration branch - so it has the identical account-mismatch gap
// CLA-458 closed here. These wrappers exist so it can apply exactly this
// resolution rather than grow a second copy of it; the Verifier's own methods
// below delegate to the same functions.

// GithubOwner reports the account that owns a github.com HTTPS remote URL, or
// "" when no scoping is derivable. See githubOwner.
func GithubOwner(remoteURL string) string {
	return githubOwner(remoteURL)
}

// ScopeEnv returns the environment additions that scope ONE git invocation to
// token: every https://github.com/ URL that invocation resolves is rewritten,
// through GIT_CONFIG_* insteadOf config, into a token-bearing x-access-token
// URL. The token travels in the child's environment, never its argv - see the
// package comment on this section for why ps(1) rules argv out. The result is
// meant as extra env on a single exec; nothing is persisted.
func ScopeEnv(token string) []string {
	return []string{
		"GIT_CONFIG_COUNT=1",
		fmt.Sprintf("GIT_CONFIG_KEY_0="+insteadOfBase, token),
		"GIT_CONFIG_VALUE_0=" + insteadOfPrefix,
	}
}

// TokenForOwner resolves owner's GitHub token with `gh auth token --user`,
// spawning ghBin in dir under runGH's interaction-hostile discipline. The spawn
// drops the ambient GH_TOKEN/GITHUB_TOKEN and names github.com explicitly: the
// answer must come from gh's own credential store for the account named, never
// from the active account's environment, and never from whatever host gh treats
// as its default - a github.com owner is a github.com account by construction
// (see githubOwner), and a GHES default host would answer for a different
// account. Any failure - gh missing, no such account, a wedged call - is an
// empty token, which callers treat as "run unscoped". A token is also refused
// if it cannot ride a single config value intact (embedded whitespace would
// corrupt the insteadOf key into something silently wrong).
func TokenForOwner(ctx context.Context, dir, ghBin, owner string) string {
	// --user stays at $3/$4: every fake gh in this package, and the account
	// name in its failure message, read the account from that position.
	out, err := runGHProbe(ctx, ghBin, dir, "auth", "token", "--user", owner, "--hostname", githubHost)
	if err != nil {
		return ""
	}
	token := strings.TrimSpace(out)
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return ""
	}
	return token
}

// ScopeRemoteEnv is the whole CLA-458 resolution for one already-read remote
// URL: the env additions that let git read THAT remote as the github account
// owning it, or nil when no scoping applies - non-github host, or an owner
// whose token gh cannot serve. nil means "run unscoped", byte-identical to
// pre-CLA-458 behavior; every fallback stays fail-open.
func ScopeRemoteEnv(ctx context.Context, dir, ghBin, remoteURL string) []string {
	owner := githubOwner(remoteURL)
	if owner == "" {
		return nil
	}
	token := TokenForOwner(ctx, dir, ghBin, owner)
	if token == "" {
		return nil
	}
	return ScopeEnv(token)
}

// remoteCredEnv returns the environment additions that scope repo's resolved
// remote to the account that owns it, or nil when no scoping applies. It costs
// two local probes per network command - one `git remote get-url`, one `gh
// auth token` spawn - and no cache, deliberately: Verify handles a single
// claim, so the worst case is a handful of spawns at session end, while a
// cache would outlive the gh auth state it was keyed on for every run after.
func (v *Verifier) remoteCredEnv(ctx context.Context, repo string) []string {
	raw, err := v.remoteURL(ctx, repo)
	if err != nil || raw == "" {
		return nil
	}
	return ScopeRemoteEnv(ctx, repo, v.ghBin, raw)
}

// ghTokenFor resolves owner's GitHub token through `gh auth token --user` -
// the same gh binary, and therefore the same credential mechanism, the PR
// check already runs on. See TokenForOwner.
func (v *Verifier) ghTokenFor(ctx context.Context, dir, owner string) string {
	return TokenForOwner(ctx, dir, v.ghBin, owner)
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
