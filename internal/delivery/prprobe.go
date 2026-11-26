package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// PRProbe names the pull request a successful doctor probe read.
type PRProbe struct {
	Slug   string
	Number int
}

// ErrNoPRProbeTarget reports that the doctor probe found no pull request it
// could read — no github.com repository under the workdir, or none of them has
// a pull request. It is NOT a gate failure: there was simply nothing to read,
// and the probe says so rather than pretending it exercised the parse path.
var ErrNoPRProbeTarget = errors.New("no pull request to probe")

// probeMaxRepos bounds how many github.com repositories the doctor probe tries
// before giving up. A fleet workdir holds a dozen; the first one with a PR
// proves the parse path, and a repo with none costs one `gh pr list` to skip.
const probeMaxRepos = 4

// prProbeTimeout bounds the whole probe. doctor is documented as a cron gate,
// so a wedged gh or an unreachable API must not hold the preflight open.
const prProbeTimeout = 20 * time.Second

// ProbeGHJSON runs a REAL `gh pr view --json` against a pull request found in a
// repository under workdir and reports whether the answer parsed. It exists so
// `clankerbar doctor` can catch the CLA-776 class of fault before a run: gh on
// PATH is necessary but not sufficient, because a gh that decorates its stdout
// (the shipped GH_FORCE_TTY=0 did exactly that) cannot be parsed by the gate
// and every delivery silently went unjudged for two months.
//
// The pull request is discovered rather than configured: the first github.com
// repository with a PR (any state) is read through the gate's own command and
// parser. Repositories with no PR are skipped, and an SSH host alias is
// resolved exactly as the gate resolves it. The error the caller classifies is
// the parse one (IsGHParseError); every other failure — gh not authenticated,
// no network — is an unavailable prerequisite, not a broken gate.
//
// ProbeGHJSON is the single-workdir form. doctor uses ProbeGHJSONDirs, because
// the delivery gate judges each project's effective workdir and its declared
// checkouts, which need not live under the top-level cfg.WorkDir.
func ProbeGHJSON(ctx context.Context, ghBin, workdir string) (PRProbe, error) {
	return ProbeGHJSONDirs(ctx, ghBin, []string{workdir})
}

// ProbeGHJSONDirs is ProbeGHJSON over EVERY workdir the delivery gate judges:
// each configured project's effective workdir plus the checkouts its repos map
// declares (doctor assembles that list). It reads the first pull request found
// in any of them, so a multi-project config whose projects carry their own
// workdirs exercises the gate's parse path instead of reporting it untested.
//
// Repositories are deduplicated by git common directory, so a project whose
// workdir and declared checkout name the same repository — or one workdir
// nested inside another — is tried once.
func ProbeGHJSONDirs(ctx context.Context, ghBin string, workdirs []string) (PRProbe, error) {
	ctx, cancel := context.WithTimeout(ctx, prProbeTimeout)
	defer cancel()

	v := New("", "")
	if ghBin != "" {
		v.ghBin = ghBin
	}
	lookupHost := v.sshAliasLookup(ctx)

	// Collect the candidate repositories across every workdir FIRST, deduped by
	// the shared git common dir (the same identity candidateRepos uses within
	// one workdir).
	var repos []string
	seenRepo := map[string]bool{}
	for _, workdir := range workdirs {
		if strings.TrimSpace(workdir) == "" {
			continue
		}
		wv := New(workdir, "")
		for _, repo := range wv.candidateRepos(ctx) {
			common, root, ok := wv.repoAt(ctx, repo)
			key := common
			if !ok || key == "" {
				key = root
			}
			if key == "" {
				key = repo
			}
			if seenRepo[key] {
				continue
			}
			seenRepo[key] = true
			repos = append(repos, repo)
		}
	}

	tried := 0
	var lastNoTarget error
	for _, repo := range repos {
		if ctx.Err() != nil {
			return PRProbe{}, ctx.Err()
		}
		url, err := v.remoteURL(ctx, repo)
		if err != nil {
			continue
		}
		slug, ok := githubSlugResolving(url, lookupHost)
		if !ok {
			continue
		}
		// probeMaxRepos bounds the repositories actually READ; comparing before
		// incrementing is what makes the constant bound what it says.
		if tried >= probeMaxRepos {
			break
		}
		tried++
		// Scope by the RESOLVED slug, not the raw remote URL: an scp-like SSH
		// remote (or a host alias) is not parsed by githubOwner, and the
		// close-out scopes by this same slug (ghAPIScope). One PR must not be
		// read under two identities.
		scope := v.ghAPIScope(ctx, repo, slug)
		num, err := v.probePRNumber(ctx, repo, slug, scope)
		if err != nil {
			if errors.Is(err, ErrNoPRProbeTarget) {
				lastNoTarget = fmt.Errorf("%w: %s has no pull request to read", ErrNoPRProbeTarget, slug)
				continue
			}
			return PRProbe{}, err
		}
		// The read itself is the gate's own: same --json selection, same
		// parser, so the probe cannot pass while the gate's parse is broken.
		if _, _, err := v.readPR(ctx, repo, slug, strconv.Itoa(num), scope); err != nil {
			return PRProbe{}, err
		}
		return PRProbe{Slug: slug, Number: num}, nil
	}
	if lastNoTarget != nil {
		return PRProbe{}, lastNoTarget
	}
	where := strings.Join(workdirs, ", ")
	if strings.TrimSpace(where) == "" {
		where = "an empty workdir"
	}
	return PRProbe{}, fmt.Errorf("%w: no github.com repository at or below %s", ErrNoPRProbeTarget, where)
}

// probePRNumber asks gh for one pull request number in slug, any state: a
// merged or closed PR proves the `--json` output just as well as an open one,
// and an open one is not guaranteed to exist.
func (v *Verifier) probePRNumber(ctx context.Context, dir, slug string, scope []string) (int, error) {
	out, err := v.runGHScoped(ctx, dir, scope, "pr", "list",
		"--repo", slug, "--state", "all", "--limit", "1", "--json", "number")
	if err != nil {
		return 0, err
	}
	var rows []struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return 0, &ghParseError{
			detail: fmt.Sprintf("could not parse gh pr list output: %v", err),
			err:    err,
		}
	}
	if len(rows) == 0 || rows[0].Number <= 0 {
		return 0, ErrNoPRProbeTarget
	}
	return rows[0].Number, nil
}
