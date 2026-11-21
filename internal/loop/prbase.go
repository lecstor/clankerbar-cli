package loop

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/delivery"
)

// prBaseProbeTimeout bounds the one network round-trip prBaseFor runs.
var prBaseProbeTimeout = 30 * time.Second

// prBaseFor resolves the branch this task's repo PRs target, substituted into the phase briefs as
// {{prBase}}. The rule itself is config.ResolvePRBase, shared with the deterministic close-out so
// both judge a repo's merge target the same.
func (d *Driver) prBaseFor(ctx context.Context, t Target, dir, repo string) string {
	var probe config.PRBaseProbe
	if d.probePRBase != nil && dir != "" {
		probe = func(integration string) (bool, string, error) {
			return d.probePRBase(ctx, dir, integration)
		}
	}
	res := config.ResolvePRBase(d.targetCfg(t), t.Name, repo, probe)
	switch {
	case res.Source == config.PRBaseOverride:
		log.Printf("%spr_base override: %s PRs target %q", labelOf(t), displayRepo(dir, repo), res.Base)
	case res.Source == config.PRBaseOriginDefault:
		log.Printf("%s%s has no %q on origin — its PRs target origin's default branch %q",
			labelOf(t), displayRepo(dir, repo), res.Integration, res.Base)
	case res.Err != nil:
		log.Printf("%scould not read origin to resolve the PR base of %s (%v) — the brief names the integration branch %q",
			labelOf(t), displayRepo(dir, repo), res.Err, res.Integration)
	}
	return res.Base
}

// displayRepo names the subject of a prBaseFor log line: the task's repo identity when the plane
// supplied one, else the checkout the probe ran in.
func displayRepo(dir, repo string) string {
	if repo != "" {
		return repo
	}
	return dir
}

// ProbeOriginPRBase asks the origin remote of dir two questions in ONE network round-trip: whether
// refs/heads/<integration> exists there, and what its symbolic HEAD points at — the answers
// config.ResolvePRBase's resolution order needs. Any scoping failure degrades to running unscoped,
// the same fail-open shape delivery keeps; the probe has no correctness cliff on top of it because
// its own failure mode is prBaseFor's fallback.
func ProbeOriginPRBase(ctx context.Context, dir, integration string) (bool, string, error) {
	ctx, cancel := context.WithTimeout(ctx, prBaseProbeTimeout)
	defer cancel()
	remoteURL, _ := originRemoteURL(ctx, dir)
	extraEnv := delivery.ScopeRemoteEnv(ctx, dir, "gh", remoteURL)
	return runOriginLsRemote(ctx, dir, integration, extraEnv)
}

// originRemoteURL reads origin's URL in dir for the credential-scoping lookup. A local read: it
// never consults credentials, and an error (no origin, not a repository) leaves the scoping empty,
// which means "run unscoped".
func originRemoteURL(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// runOriginLsRemote is ProbeOriginPRBase's one git exec, split out so the probe reads as its two
// answers. WaitDelay reaps the helpers ls-remote spawns — they inherit the pipes behind these
// buffers, and a killed git can otherwise leave the call sitting past its deadline.
func runOriginLsRemote(ctx context.Context, dir, integration string, extraEnv []string) (bool, string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "ls-remote", "--symref", "origin", "HEAD", "refs/heads/"+integration)
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=echo",
		"SSH_ASKPASS=echo",
		"GIT_SSH_COMMAND=ssh -oBatchMode=yes -oConnectTimeout=10",
		"GCM_INTERACTIVE=never",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return false, "", fmt.Errorf("git ls-remote: %w: %s", err, firstLineOf(msg))
		}
		return false, "", fmt.Errorf("git ls-remote: %w", err)
	}
	onOrigin, defaultBranch := parseOriginRefs(stdout.String(), integration)
	return onOrigin, defaultBranch, nil
}

// parseOriginRefs reads runOriginLsRemote's output: the "ref: refs/heads/X HEAD" symref line when
// the remote's HEAD is symbolic, and any line naming refs/heads/<integration>. Exact-field
// matching, never substring: "refs/heads/staging" must not be read as a match for an integration
// branch named "ging".
func parseOriginRefs(out, integration string) (onOrigin bool, defaultBranch string) {
	want := "refs/heads/" + integration
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) == 3 && fields[0] == "ref:" && fields[2] == "HEAD":
			if target, ok := strings.CutPrefix(fields[1], "refs/heads/"); ok {
				defaultBranch = target
			}
		case len(fields) == 2 && fields[1] == want:
			onOrigin = true
		}
	}
	return onOrigin, defaultBranch
}

// firstLineOf trims a git stderr message to its first line, so a two-line remote error does not run
// into the log's next sentence.
func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
