package delivery

// prcreate.go — the PR the RUNNER opens for a runner-claimed hand-over (CLA-1035;
// see docs/runner-owned-mechanics.md, plan B7).
//
// A runner-claimed session never opens a PR: the runner knows the branch is on
// origin before it hands the task over, so it can open the PR itself and link it
// on the task. That closes the gap a session's own terminal step leaves - a
// session that dies between `gh pr create` and its done report leaves a PR
// nobody recorded, and the task burning a reclaim slot with delivery.pr null.
//
// The read half is OpenPRs from prclose.go, so a retry, a takeover or a session
// that opened a PR anyway reuses that PR instead of creating a duplicate.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// PRSpec is one pull request EnsurePR creates when the branch has none. It is
// the deterministic description the runner already holds: the repository and
// branch the work sits in, the base it targets, and the title/body the runner
// assembles from the task's record.
type PRSpec struct {
	// Repo is a working directory of the repository. gh runs in it, and the
	// GitHub slug is read from its origin remote when Slug is empty.
	Repo string
	// Slug is the GitHub repository ("owner/name"). Empty resolves it from
	// Repo's origin remote, which is the repository the branch was pushed to.
	Slug string
	// Base is the branch the PR targets (the repo's configured PR base); Head is
	// the pushed task branch.
	Base, Head string
	// Title and Body are passed verbatim to `gh pr create`.
	Title, Body string
}

// prCreateAttempts is the one create plus the single retry the settled design
// allows before the hand-over fails closed.
const prCreateAttempts = 2

// EnsurePR returns the number of the OPEN pull request for spec.Head targeting
// spec.Base, creating one when there is none.
//
// It is idempotent across retries and takeovers: an existing open PR for the
// branch is reused, and after a create that failed in a way that may still have
// landed (a lost response, unparseable output) it re-reads the open PRs before
// retrying, so the retry cannot open a duplicate. A second failure is returned
// to the caller, which fails the hand-over closed.
func (v *Verifier) EnsurePR(ctx context.Context, spec PRSpec) (int, error) {
	spec = normalisePRSpec(spec)
	if err := spec.validate(); err != nil {
		return 0, err
	}
	if _, err := exec.LookPath(v.ghBin); err != nil {
		return 0, fmt.Errorf("ensure PR: %s is not on PATH", v.ghBin)
	}
	slug, err := v.resolvePRSlug(ctx, spec)
	if err != nil {
		return 0, err
	}
	// The open read runs FIRST and its failure is fatal: without it a create
	// cannot know whether the branch already has a PR, and opening a second one
	// is the duplicate this function exists to prevent.
	if num, ok, err := v.openPRFor(ctx, spec.Repo, slug, spec.Head, spec.Base); err != nil {
		return 0, fmt.Errorf("ensure PR for %s: reading open pull requests: %w", spec.Head, err)
	} else if ok {
		return num, nil
	}
	var lastErr error
	for attempt := 0; attempt < prCreateAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return 0, fmt.Errorf("ensure PR for %s: %w", spec.Head, ctx.Err())
			case <-time.After(v.prCreateBackoff):
			}
			// The failed create may have landed with its answer lost: adopt what
			// is now open rather than opening a second PR.
			if num, ok, rerr := v.openPRFor(ctx, spec.Repo, slug, spec.Head, spec.Base); rerr == nil && ok {
				return num, nil
			}
		}
		num, cerr := v.createPR(ctx, spec, slug)
		if cerr == nil {
			return num, nil
		}
		lastErr = cerr
	}
	return 0, fmt.Errorf("ensure PR for %s on %s failed after %d attempts: %w",
		spec.Head, spec.Repo, prCreateAttempts, lastErr)
}

// CreatePR opens one pull request, without the reuse-then-retry discipline
// EnsurePR applies. A caller that might be retried wants EnsurePR.
func (v *Verifier) CreatePR(ctx context.Context, spec PRSpec) (int, error) {
	spec = normalisePRSpec(spec)
	if err := spec.validate(); err != nil {
		return 0, err
	}
	if _, err := exec.LookPath(v.ghBin); err != nil {
		return 0, fmt.Errorf("create PR: %s is not on PATH", v.ghBin)
	}
	slug, err := v.resolvePRSlug(ctx, spec)
	if err != nil {
		return 0, err
	}
	return v.createPR(ctx, spec, slug)
}

// normalisePRSpec trims the members gh would otherwise receive with stray
// whitespace, which matters for the refs (an untrimmed base is a different branch
// name to git) and cosmetically for the text.
func normalisePRSpec(spec PRSpec) PRSpec {
	spec.Repo = strings.TrimSpace(spec.Repo)
	spec.Slug = strings.TrimSpace(spec.Slug)
	spec.Base = strings.TrimSpace(spec.Base)
	spec.Head = strings.TrimSpace(spec.Head)
	spec.Title = strings.TrimSpace(spec.Title)
	spec.Body = strings.TrimSpace(spec.Body)
	return spec
}

// validate refuses a spec gh cannot satisfy, before it costs a spawn.
func (s PRSpec) validate() error {
	switch {
	case s.Repo == "":
		return errors.New("ensure PR: no working directory to run gh in")
	case s.Base == "":
		return errors.New("ensure PR: no base branch to target")
	case s.Head == "":
		return errors.New("ensure PR: no head branch to open a PR from")
	case s.Title == "":
		return errors.New("ensure PR: no title")
	}
	return nil
}

// resolvePRSlug returns the GitHub repository the PR belongs to: the caller's
// explicit slug, or the one derived from the working directory's origin remote.
// The remote is what the branch was actually pushed to, so it wins over any
// configured identity that could name a different repository.
func (v *Verifier) resolvePRSlug(ctx context.Context, spec PRSpec) (string, error) {
	if spec.Slug != "" {
		return spec.Slug, nil
	}
	url, err := v.remoteURL(ctx, spec.Repo)
	if err != nil {
		return "", fmt.Errorf("ensure PR: read the remote of %s: %w", spec.Repo, err)
	}
	slug, ok := githubSlugResolving(url, v.sshAliasLookup(ctx))
	if !ok {
		return "", fmt.Errorf("ensure PR: the remote of %s (%s) is not a github.com repository", spec.Repo, url)
	}
	return slug, nil
}

// openPRFor reports the open PR for head targeting base, and whether there is
// one. A PR for the branch against another base is deliberately not reused: the
// runner's PR must target the configured PR base.
func (v *Verifier) openPRFor(ctx context.Context, repo, slug, head, base string) (int, bool, error) {
	refs, err := v.OpenPRs(ctx, repo, slug, head)
	if err != nil {
		return 0, false, err
	}
	for _, pr := range refs {
		if pr.Base == base {
			return pr.Number, true, nil
		}
	}
	return 0, false, nil
}

// createPR runs `gh pr create` under the credentials of the account that owns
// the slug, and parses the new PR's number from the URL gh prints.
func (v *Verifier) createPR(ctx context.Context, spec PRSpec, slug string) (int, error) {
	out, err := v.runGHScoped(ctx, spec.Repo, v.ghAPIScope(ctx, spec.Repo, slug),
		"pr", "create", "--repo", slug, "--base", spec.Base, "--head", spec.Head,
		"--title", spec.Title, "--body", spec.Body)
	if err != nil {
		return 0, err
	}
	num, ok := parseCreatedPR(out)
	if !ok {
		return 0, &ghParseError{
			detail: fmt.Sprintf("gh pr create printed no pull request URL: %q", firstLine(out)),
			err:    errors.New("no pull request number in gh pr create output"),
		}
	}
	return num, nil
}

// createdPRURL matches the number in the URL gh prints for a new PR
// ("https://github.com/owner/name/pull/123").
var createdPRURL = regexp.MustCompile(`/pull/(\d+)`)

// parseCreatedPR reads the LAST pull request number in gh's output: the URL is
// printed after any informational lines, and an earlier number would only be
// noise in the same stream.
func parseCreatedPR(out string) (int, bool) {
	matches := createdPRURL.FindAllStringSubmatch(out, -1)
	if len(matches) == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(matches[len(matches)-1][1])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
