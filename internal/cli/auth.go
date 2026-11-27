package cli

// auth.go — `clankerbar login`, `logout`, `whoami`, and the shared credential
// resolution every other command now uses.
//
// Before this, every credentialed command read CLANKERBAR_API_KEY from its own
// process environment. That is fine for a shell but not for a GUI/TUI harness
// that never inherits one, so the operator had to paste the key into each
// harness's configuration. `login` writes it once to the owner-only store in
// internal/auth, and the commands fall back to it — env still wins, so an
// explicit export remains the override.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/lecstor/clankerbar-cli/internal/auth"
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/plane"
	"github.com/lecstor/clankerbar-cli/internal/secureurl"
)

type loginFlags struct {
	host         string
	projectScope bool
}

func newLoginFlagSet(f *loginFlags) *pflag.FlagSet {
	fs := newFlagSet("login")
	fs.StringVar(&f.host, "host", "", "clankerbar origin to log in to (default: this config's backlog_url, else "+auth.DefaultOrigin+")")
	fs.BoolVar(&f.projectScope, "project-scope", false, "request a key bound to one project instead of the account-wide user key")
	return fs
}

type logoutFlags struct {
	host string
	all  bool
}

func newLogoutFlagSet(f *logoutFlags) *pflag.FlagSet {
	fs := newFlagSet("logout")
	fs.StringVar(&f.host, "host", "", "origin to forget (default: this config's backlog_url, else "+auth.DefaultOrigin+")")
	fs.BoolVar(&f.all, "all", false, "forget every stored origin")
	return fs
}

type whoamiFlags struct {
	host string
}

func newWhoamiFlagSet(f *whoamiFlags) *pflag.FlagSet {
	fs := newFlagSet("whoami")
	fs.StringVar(&f.host, "host", "", "origin to report on (default: this config's backlog_url, else "+auth.DefaultOrigin+")")
	return fs
}

// Login is `clankerbar login`: run the browser device flow and save the key.
func Login(ctx context.Context, args []string) error {
	return loginRun(ctx, args, os.Stdout, os.Stderr)
}

func loginRun(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return loginRunWith(ctx, args, stdout, stderr, nil)
}

// loginRunWith is loginRun with the poll wait injectable, so the tests exercise
// the whole flow without spending minutes asleep.
func loginRunWith(ctx context.Context, args []string, stdout, stderr io.Writer, sleep func(context.Context, time.Duration) error) error {
	var f loginFlags
	fs := newLoginFlagSet(&f)
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil
		}
		return err
	}
	origin, err := resolveAuthHost(f.host)
	if err != nil {
		return err
	}
	scope := auth.ScopeUser
	if f.projectScope {
		scope = auth.ScopeProject
	}
	grant, err := auth.RequestDeviceCode(ctx, origin, scope)
	if err != nil {
		return err
	}
	printDeviceInstructions(stderr, grant)
	token, err := auth.PollDeviceToken(ctx, origin, grant, auth.PollOptions{Sleep: sleep})
	if err != nil {
		return err
	}
	cred := auth.Credential{
		Token:       token.AccessToken,
		Scope:       token.Scope,
		ProjectSlug: token.ProjectSlug,
		SavedAt:     time.Now().UTC(),
	}
	if cred.Scope == "" {
		// A plane older than CLA-1037 does not echo `scope`; the grant asked for
		// this one, so recording it keeps whoami from reporting "unknown".
		cred.Scope = scope
	}
	if err := auth.Save(origin, cred); err != nil {
		return fmt.Errorf("save the credential: %w", err)
	}
	path, _, err := auth.Path()
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "Logged in to %s (%s).\n", origin, describeScope(cred.Scope, cred.ProjectSlug))
	fmt.Fprintf(stderr, "Credential saved to %s (owner-only).\n", path)
	return nil
}

// printDeviceInstructions puts the two things the operator needs on stderr: the
// URL to open and the code to confirm. stdout stays empty so `login` can be run
// where stdout is a pipe without polluting it.
func printDeviceInstructions(w io.Writer, grant auth.DeviceAuthorization) {
	uri := strings.TrimSpace(grant.VerificationURIComplete)
	if uri == "" {
		uri = strings.TrimSpace(grant.VerificationURI)
	}
	if uri != "" {
		fmt.Fprintf(w, "To approve this machine, open:\n  %s\n", uri)
	}
	fmt.Fprintf(w, "and confirm the code: %s\n\nWaiting for approval...\n", grant.UserCode)
}

// Logout is `clankerbar logout`: forget one origin (or all of them). Idempotent
// when already logged out.
func Logout(ctx context.Context, args []string) error {
	return logoutRun(ctx, args, os.Stdout, os.Stderr)
}

func logoutRun(_ context.Context, args []string, stdout, stderr io.Writer) error {
	var f logoutFlags
	fs := newLogoutFlagSet(&f)
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.all && strings.TrimSpace(f.host) != "" {
		return errors.New("--all and --host are mutually exclusive: --all forgets every origin, --host forgets one")
	}
	if f.all {
		n, err := auth.RemoveAll()
		if err != nil {
			return err
		}
		if n == 0 {
			fmt.Fprintln(stderr, "Not logged in to any origin - nothing to forget.")
			return nil
		}
		fmt.Fprintf(stderr, "Logged out of %d stored %s.\n", n, plural(n, "origin", "origins"))
		return nil
	}
	origin, err := resolveAuthHost(f.host)
	if err != nil {
		return err
	}
	removed, err := auth.Remove(origin)
	if err != nil {
		return err
	}
	if !removed {
		fmt.Fprintf(stderr, "Not logged in to %s - nothing to forget.\n", origin)
		return nil
	}
	fmt.Fprintf(stderr, "Logged out of %s.\n", origin)
	return nil
}

// Whoami is `clankerbar whoami`: report where the credential comes from and
// prove the plane still accepts it. Exits non-zero when unauthenticated or when
// the key is rejected.
func Whoami(ctx context.Context, args []string) error {
	return whoamiRun(ctx, args, os.Stdout, os.Stderr)
}

func whoamiRun(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	var f whoamiFlags
	fs := newWhoamiFlagSet(&f)
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil
		}
		return err
	}
	origin, err := resolveAuthHost(f.host)
	if err != nil {
		return err
	}
	res, err := auth.Resolve(origin)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "origin: %s\n", origin)
	if res.Token == "" {
		fmt.Fprintln(stdout, "source: none")
		return fmt.Errorf("not authenticated for %s: set %s, or run `clankerbar login`", origin, auth.EnvAPIKey)
	}
	switch res.Source {
	case auth.SourceEnv:
		fmt.Fprintf(stdout, "source: %s\n", auth.EnvAPIKey)
	case auth.SourceFile:
		fmt.Fprintf(stdout, "source: credential file %s\n", res.Path)
	}
	if res.Scope != "" {
		fmt.Fprintf(stdout, "scope: %s\n", describeScope(res.Scope, res.ProjectSlug))
	} else {
		fmt.Fprintln(stdout, "scope: unknown")
	}

	endpoint := origin + "/mcp"
	if res.Scope == auth.ScopeProject && res.ProjectSlug != "" {
		endpoint = origin + "/mcp/" + url.PathEscape(res.ProjectSlug)
	}
	tool, probeErr := plane.Probe(ctx, endpoint, res.Token)
	if probeErr != nil {
		if plane.Unauthorized(probeErr) {
			return fmt.Errorf("the credential for %s was rejected by the plane (401/403) - it may be revoked: run `clankerbar login` again: %w", origin, probeErr)
		}
		return fmt.Errorf("could not verify the credential at %s: %w", endpoint, probeErr)
	}
	fmt.Fprintf(stdout, "status: OK (%s succeeded)\n", tool)
	return nil
}

// resolveAuthHost applies the login/logout/whoami --host default: the config's
// own credential origin, else the built-in plane. The result is always a
// credential-safe canonical origin, never the raw flag.
func resolveAuthHost(flag string) (string, error) {
	if host := strings.TrimSpace(flag); host != "" {
		origin, err := secureurl.Origin(host)
		if err != nil {
			return "", fmt.Errorf("--host: %w", err)
		}
		return origin, nil
	}
	cfg, err := config.Load("")
	if err != nil {
		return "", fmt.Errorf("load config: %w", err)
	}
	raw := strings.TrimSpace(cfg.BacklogURL)
	if raw == "" {
		return auth.DefaultOrigin, nil
	}
	origin, err := secureurl.Origin(raw)
	if err != nil {
		return "", fmt.Errorf("the config's backlog_url cannot carry a credential (%w) - pass --host <origin>", err)
	}
	return origin, nil
}

// credentialFor resolves the credential for cfg's trusted origin. It is the one
// place the env-then-store precedence is applied to a config, so the commands
// cannot disagree about it.
func credentialFor(cfg *config.Config) (auth.Resolution, error) {
	origin := cfg.CredentialOrigin()
	if origin == "" {
		return auth.Resolution{}, errors.New("no credential-safe origin is configured - set backlog_url in the config")
	}
	return auth.Resolve(origin)
}

// requireCredential is credentialFor plus the refusal a command that cannot run
// unauthenticated needs.
func requireCredential(cfg *config.Config) (auth.Resolution, error) {
	res, err := credentialFor(cfg)
	if err != nil {
		return res, err
	}
	if res.Token == "" {
		return res, fmt.Errorf("not authenticated for %s: set %s, or run `clankerbar login`", cfg.CredentialOrigin(), auth.EnvAPIKey)
	}
	return res, nil
}

// exportStoredCredential puts a file-resolved key into this process's
// environment. Run and supervise spawn sessions/children that inherit
// CLANKERBAR_API_KEY and hand it to the plane's MCP server; without this the
// daemon would authenticate its own calls from the store while every session it
// spawns stayed keyless.
func exportStoredCredential(res auth.Resolution) error {
	if res.Source != auth.SourceFile || res.Token == "" {
		return nil
	}
	return os.Setenv(auth.EnvAPIKey, res.Token)
}

// describeScope renders a stored scope for a human line.
func describeScope(scope, projectSlug string) string {
	switch scope {
	case auth.ScopeUser:
		return "user (all projects)"
	case auth.ScopeProject:
		if projectSlug != "" {
			return "project " + projectSlug
		}
		return "project"
	case "":
		return "unknown"
	default:
		return scope
	}
}
