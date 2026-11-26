package delivery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

// githubSlug extracts owner/name from a git remote URL, or reports that the
// remote is not a github.com one. The known prefix forms are matched
// explicitly rather than searching for "github.com" anywhere in the string —
// a host like "mygithub.example.com" contains the needle and must not match.
//
// This is the DIRECT grammar only. A remote whose host is an SSH config alias
// (git@github-personal:lecstor/clankerbar-cli.git) is not a github.com remote
// to this function; githubSlugResolving resolves the alias through ssh, which
// is what the PR check uses. Kept pure so the grammar is pinned without a
// process spawn.
func githubSlug(remoteURL string) (string, bool) {
	return githubSlugResolving(remoteURL, nil)
}

// githubSlugResolving is githubSlug plus SSH host alias resolution. When the
// URL is an ssh form whose host is not github.com, resolve is asked for the
// hostname that alias selects (production: `ssh -G`, the effective config);
// the remote is accepted when that comes back as github.com. resolve == nil
// means "direct forms only".
//
// The alias case is not exotic: a machine with
//
//	Host github-personal
//	  HostName github.com
//
// in ~/.ssh/config has `git@github-personal:lecstor/clankerbar-cli.git` as its
// origin, and every remote read reported it as "not a github.com repository",
// so the PR check was skipped rather than run (CLA-728 PR 230, 2026-10-21).
func githubSlugResolving(remoteURL string, resolve func(host string) (string, bool)) (string, bool) {
	host, path, sshish, ok := splitRemoteHostPath(remoteURL)
	if !ok {
		return "", false
	}
	if !isGitHubHost(host) {
		if !sshish || resolve == nil {
			return "", false
		}
		real, ok := resolve(host)
		if !ok || !isGitHubHost(real) {
			return "", false
		}
	}
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	return parts[0] + "/" + parts[1], true
}

// splitRemoteHostPath splits a git remote URL into the host and the owner/name
// path below it, for the forms git actually writes: scheme URLs (https, http,
// ssh, git+ssh) and scp-like syntax ([user@]host:path). sshish reports an ssh
// form — the only kind a Host alias can apply to. ok is false for anything
// else, including a local filesystem path, which is not a remote host at all.
func splitRemoteHostPath(remoteURL string) (host, path string, sshish, ok bool) {
	u := strings.TrimSpace(remoteURL)
	u = strings.TrimSuffix(u, ".git")
	if u == "" {
		return "", "", false, false
	}
	if i := strings.Index(u, "://"); i > 0 {
		parsed, err := url.Parse(u)
		if err != nil {
			return "", "", false, false
		}
		switch strings.ToLower(parsed.Scheme) {
		case "http", "https":
		case "ssh", "git+ssh":
			sshish = true
		default:
			return "", "", false, false
		}
		path = strings.TrimPrefix(parsed.Path, "/")
		host = parsed.Hostname()
		return host, path, sshish, host != "" && path != ""
	}
	// scp-like: [user@]host:path. A colon after a slash is a path
	// separator (a local path, or a Windows drive), not a host.
	if i := strings.LastIndex(u, ":"); i > 0 && !strings.Contains(u[:i], "/") {
		host = u[:i]
		if j := strings.LastIndex(host, "@"); j >= 0 {
			host = host[j+1:]
		}
		path = strings.TrimPrefix(u[i+1:], "/")
		return host, path, true, host != "" && path != ""
	}
	return "", "", false, false
}

// isGitHubHost is an EXACT host comparison, because "mygithub.example.com"
// contains the needle and must not match.
func isGitHubHost(host string) bool {
	return strings.EqualFold(strings.TrimSpace(host), "github.com")
}

// sshAliasTimeout bounds one `ssh -G` lookup. It reads configuration and never
// opens a connection, so it is fast; the bound exists so a pathological config
// (a Match exec block that blocks) cannot hold a delivery check open.
const sshAliasTimeout = 5 * time.Second

// sshAliasHost resolves an SSH host alias to the HostName its configuration
// selects, using `ssh -G <alias>` — the effective configuration ssh itself
// would use, which is exactly the question. An unknown alias resolves to
// itself, which is why the caller still compares the ANSWER with github.com
// rather than trusting that resolution happened.
//
// The host is passed AFTER `--`: it comes from a git remote URL, which can
// spell a host beginning with '-' (the scp-like split only requires no '/'
// before the colon), and without the marker ssh would read that string as an
// OPTION rather than a destination.
func sshAliasHost(ctx context.Context, host string) (string, bool) {
	cctx, cancel := context.WithTimeout(ctx, sshAliasTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "ssh", "-G", "--", host)
	cmd.WaitDelay = ghWaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", false
	}
	for _, line := range strings.Split(stdout.String(), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.EqualFold(f[0], "hostname") {
			return f[1], true
		}
	}
	return "", false
}

// sshAliasLookup returns a memoized resolver for one check. A workdir of many
// repositories routinely shares one alias, and `ssh -G` costs a process spawn:
// without the cache, narrowing to the right repository would pay it once per
// candidate. Failures are remembered too — an absent ssh or an unreadable
// config does not change between candidates.
func (v *Verifier) sshAliasLookup(ctx context.Context) func(host string) (string, bool) {
	resolved := map[string]string{}
	failed := map[string]bool{}
	return func(host string) (string, bool) {
		if h, ok := resolved[host]; ok {
			return h, true
		}
		if failed[host] {
			return "", false
		}
		h, ok := sshAliasHost(ctx, host)
		if !ok {
			failed[host] = true
			return "", false
		}
		resolved[host] = h
		return h, true
	}
}

// ghNotFoundError marks a `gh` failure that means THE PR IS NOT THERE, rather
// than that the tool could not run. A delivery naming a PR that does not
// exist is a false claim to refuse, not an outage to ride through.
type ghNotFoundError struct{ detail string }

func (e *ghNotFoundError) Error() string { return e.detail }

// ghParseError marks gh output that is not the JSON this gate asked for. It is
// deliberately NOT an absence of evidence: gh ran and answered, and the answer
// is unintelligible — a defect in the gate's own contract (CLA-776: an
// inherited GH_FORCE_TTY made gh colour its JSON, and the parse error was
// logged as "could not verify … carrying on" on every delivery for two
// months). Callers refuse the delivery on it, loudly, rather than carrying on.
type ghParseError struct {
	detail string
	err    error
}

func (e *ghParseError) Error() string { return e.detail }
func (e *ghParseError) Unwrap() error { return e.err }

// IsGHParseError reports whether err is gh output this package could not parse.
// The doctor preflight classifies its probe's failures with it: a parse failure
// means the delivery gate is broken NOW and is a FAIL, while auth and network
// failures of the same probe are only a WARN.
func IsGHParseError(err error) bool {
	var pe *ghParseError
	return errors.As(err, &pe)
}

// runGH executes one gh command in dir and returns its trimmed stdout, with
// the same interaction-hostile environment and cut-off discipline as the git
// runner: an unattended run has no terminal to answer prompts with, and a
// wedged helper must not hold the check past its WaitDelay.
func (v *Verifier) runGH(ctx context.Context, dir string, args ...string) (string, error) {
	return v.runGHScoped(ctx, dir, nil, args...)
}

// runGHScoped is runGH plus environment additions for THIS invocation — the
// shape credential scoping has to take, since a token must reach this child
// and no other. When additions ride along, the ambient GH_TOKEN and
// GITHUB_TOKEN are dropped from the child's environment first rather than
// appended over: an env array may carry duplicates and which entry a child
// resolves is unspecified, so shadowing is not a guarantee a credential is
// allowed to lean on. Empty additions leave the environment untouched,
// byte-for-byte the pre-scoping behavior.
func (v *Verifier) runGHScoped(ctx context.Context, dir string, extraEnv []string, args ...string) (string, error) {
	return runGHBin(ctx, v.ghBin, dir, extraEnv, false, args...)
}

// runGHProbe is the credential probe's spawn: runGHBin with the parent's
// GH_TOKEN/GITHUB_TOKEN dropped. It is a named wrapper rather than a bare bool
// at the call because this is the one spawn where the flag decides which
// credential the child can see.
func runGHProbe(ctx context.Context, ghBin, dir string, args ...string) (string, error) {
	return runGHBin(ctx, ghBin, dir, nil, true, args...)
}

// runGHBin is runGH with the binary spelled out, so the per-owner token
// resolution in delivery.go (TokenForOwner, CLA-458) drives the same spawn
// discipline without a Verifier in hand - CLA-459's doctor consumer passes
// "gh" here. extraEnv carries per-invocation environment additions (see
// runGHScoped); pass nil for the plain spawn. dropAmbientTokens removes the
// parent's GH_TOKEN/GITHUB_TOKEN even when no additions ride the invocation:
// the credential probe must answer for the account it names, never the one the
// ambient environment is authenticated as - callers use runGHProbe for that.
func runGHBin(ctx context.Context, ghBin, dir string, extraEnv []string, dropAmbientTokens bool, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, ghBin, args...)
	cmd.Dir = dir
	cmd.Env = ghEnv(os.Environ(), extraEnv, dropAmbientTokens)
	cmd.WaitDelay = ghWaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		low := strings.ToLower(msg)
		if strings.Contains(low, "could not resolve to a pullrequest") ||
			strings.Contains(low, "not found") ||
			strings.Contains(low, "no pull requests") {
			return "", &ghNotFoundError{detail: fmt.Sprintf("gh %s: %s", args[0], firstLine(msg))}
		}
		if msg != "" {
			return "", fmt.Errorf("gh %s: %w: %s", args[0], err, firstLine(msg))
		}
		return "", fmt.Errorf("gh %s: %w", args[0], err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// ghEnv builds the environment for one gh invocation: the parent environment
// with every variable this function OWNS removed (so an ambient value cannot
// win by accident of position — duplicate entries have no portable
// precedence), the per-invocation additions the caller passes (held to the same
// ownership rule), and LAST the non-interactive and no-colour settings this
// driver requires — so neither an inherited value nor an addition can
// reintroduce a colour switch the output parser depends on.
//
// The ambient GH_TOKEN/GITHUB_TOKEN go when a scoped token rides this
// invocation or when dropAmbientTokens asks for it explicitly - the credential
// probe, whose whole job is to answer for a NAMED account. With neither, the
// inherited environment is kept, byte-for-byte the pre-scoping behavior.
//
// # Why GH_FORCE_TTY is gone, not zeroed (CLA-776)
//
// gh treats ANY value of GH_FORCE_TTY as "force terminal-style output", and
// reads a number as a column width. The CLA-310 gate shipped `GH_FORCE_TTY=0`,
// which therefore turned colour ON: `gh pr view --json` printed
// ANSI-coloured, pretty-printed JSON, encoding/json refused it at the first
// `\x1b`, and every delivery's PR check degraded to a "could not verify …
// carrying on" — unnoticed for two months because the unit tests drive a fake
// gh that never colours anything.
//
// So the variable is REMOVED (from the inherited environment too: an ambient
// value would do the same damage) and the colour-forcing family is stripped
// alongside it. NO_COLOR and CLICOLOR=0 are set as the positive statement of
// the same requirement, so a gh version that ignores one honours the other.
func ghEnv(parent, extraEnv []string, dropAmbientTokens bool) []string {
	// Colour and interaction settings this child OWNS. Setting ours by
	// appending over an inherited value is not enough: duplicate entries have
	// unspecified precedence, so the parent's must be gone.
	owned := func(name string) bool {
		switch name {
		case "GH_FORCE_TTY", "CLICOLOR_FORCE", "FORCE_COLOR", "NO_COLOR", "CLICOLOR",
			"GH_NO_UPDATE_NOTIFIER", "GIT_TERMINAL_PROMPT":
			return true
		}
		return false
	}
	env := make([]string, 0, len(parent)+len(extraEnv)+4)
	for _, e := range parent {
		name := envName(e)
		if owned(name) {
			continue
		}
		// An ambient token only goes when a scoped one rides this invocation or
		// the caller asked for it to be dropped; otherwise the previous
		// behaviour is kept exactly.
		if (len(extraEnv) > 0 || dropAmbientTokens) && isGhTokenVar(name) {
			continue
		}
		env = append(env, e)
	}
	// Additions ride the same rule: a variable this function OWNS cannot re-enter
	// through the caller, so no extraEnv can turn colour back on behind the
	// parser's back.
	for _, e := range extraEnv {
		if owned(envName(e)) {
			continue
		}
		env = append(env, e)
	}
	return append(env,
		"GH_NO_UPDATE_NOTIFIER=1",
		"NO_COLOR=1",
		"CLICOLOR=0",
		"GIT_TERMINAL_PROMPT=0",
	)
}

// envName is the NAME part of a NAME=value environment entry.
func envName(e string) string {
	if i := strings.IndexByte(e, '='); i > 0 {
		return e[:i]
	}
	return e
}

// isGhTokenVar reports whether an environment NAME is one of the variables gh
// reads a token from ahead of its keyring. Both spellings must go when a
// scoped token rides the invocation, or a stale ambient value could win by
// accident of position.
func isGhTokenVar(name string) bool {
	switch name {
	case "GH_TOKEN", "GITHUB_TOKEN":
		return true
	}
	return false
}
