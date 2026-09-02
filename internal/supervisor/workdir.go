package supervisor

// Workdir derivation (phase 2a of docs/proposals/daemon-supervisor.md in the
// clankerbar repo): each supervised child's workdir comes from one
// machine-stated root plus the repo the instance's project names —
// `~/dev` + `lecstor/clankerbar` resolves to `~/dev/clankerbar`. The
// irreducible local input stays one env var (the API key) and one path (the
// root); everything else the derivation needs names a repo, never a path.
//
// Derivation FAILS CLOSED, both conditions learned from CLA-441:
//
//   - The derived directory must EXIST. It is never created, and a missing
//     directory refuses the daemon with the path that was tried.
//   - The derived directory must be a CHECKOUT of the repo the project names —
//     its origin remote must name that repo. A directory that exists but is
//     something else (a plain dir, a checkout of a different repo, a repo with
//     no attributable remote) refuses the daemon with the path that was tried.
//
// There is no fallback branch at all. In particular the supervisor's own
// working directory is never used — that fallback is exactly how sessions
// ended up in the daemon's start directory with another project's MCP config
// and another project's grants (CLA-441), so a derivation that cannot verify
// the directory it derived is an ERROR, never a path to run in.
//
// A derivation that succeeds is consumed by the materialized config (phase 2b)
// and, once the roster exists (phase 3b), by the plane's `project.primary_repo`
// in place of the local declaration this phase reads.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/delivery"
)

// ErrWorkdirRefused is the sentinel for a derived workdir that failed the
// fail-closed conditions. The wrapped text names the path that was tried and
// why it was refused.
var ErrWorkdirRefused = errors.New("workdir_refused")

// DeriveWorkdir maps one repo identity to the workdir a supervised child for
// it runs in, from the machine-stated root.
//
// The derived path is <root>/<repo name> — the last path segment of the
// identity, so `lecstor/clankerbar` derives to <root>/clankerbar. The root may
// be relative (resolved against the supervisor's cwd, like every other path
// this config resolves) or start with ~.
//
// Fail-closed, in order: the derived directory must exist (never created), and
// it must be a checkout of the expected repo — its attributable origin remote
// must name the identity (see MatchesRepoSlug for the comparison and its
// boundaries). Either failure returns an error naming the path tried; there is
// no fallback, and never one to the supervisor's own working directory.
func DeriveWorkdir(root, repo string) (string, error) {
	root = expandHome(strings.TrimSpace(root))
	repo = strings.TrimSuffix(strings.TrimSpace(repo), "/")
	if root == "" {
		return "", fmt.Errorf("%w: no machine workdir root to derive from", ErrWorkdirRefused)
	}
	if repo == "" {
		return "", fmt.Errorf("%w: no repo to derive from - the instance's project names none", ErrWorkdirRefused)
	}
	dir, err := filepath.Abs(filepath.Join(root, basenameOfRepo(repo)))
	if err != nil {
		return "", fmt.Errorf("%w: cannot resolve %s (derived from root %s and repo %s): %v", ErrWorkdirRefused, filepath.Join(root, basenameOfRepo(repo)), root, repo, err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		// Never create it: a directory the supervisor made for a child that has
		// not started yet would be adopted by that child, and a created-then-
		// verified path would turn "the operator did not check it out" into "the
		// machine made a checkout-shaped hole" — the wrong-workdir failure from
		// the other side.
		return "", fmt.Errorf("%w: %s does not exist (derived from root %s and repo %s) - refusing, never created", ErrWorkdirRefused, dir, root, repo)
	}
	remote, err := checkoutOriginRemote(dir)
	if err != nil {
		return "", fmt.Errorf("%w: %s is not a checkout of %s: %v", ErrWorkdirRefused, dir, repo, err)
	}
	if !delivery.MatchesRepoSlug(remote, repo) {
		return "", fmt.Errorf("%w: %s is a checkout of a different repo - its origin remote names %q, expected %s", ErrWorkdirRefused, dir, remote, repo)
	}
	return dir, nil
}

// checkoutOriginRemote returns the URL of the remote a checkout can be
// attributed to: "origin" when the checkout has one, else its only remote.
// A checkout with several remotes and no origin cannot be attributed to one
// repo, and fail-closed says refuse rather than guess; so does a git
// repository with no remotes at all (it is a checkout of SOMETHING, but not
// of anything the derivation can verify).
func checkoutOriginRemote(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "remote").Output()
	if err != nil {
		return "", fmt.Errorf("not a git checkout (git remote: %v)", err)
	}
	names := strings.Fields(string(out))
	if len(names) == 0 {
		return "", errors.New("a git repository with no remotes, so which repo it is cannot be verified")
	}
	remote := ""
	for _, n := range names {
		if n == "origin" {
			remote = n
			break
		}
	}
	if remote == "" {
		if len(names) != 1 {
			return "", fmt.Errorf("%d remotes and no origin, so which repo it is cannot be attributed", len(names))
		}
		remote = names[0]
	}
	urlBytes, err := exec.Command("git", "-C", dir, "remote", "get-url", remote).Output()
	if err != nil {
		return "", fmt.Errorf("cannot read its remote %s (git remote get-url: %v)", remote, err)
	}
	url := strings.TrimSpace(string(urlBytes))
	if url == "" {
		return "", fmt.Errorf("its remote %s has an empty URL", remote)
	}
	return url, nil
}

// deriveInstanceWorkdirs derives the workdir for every project one instance
// drives, from the machine-stated root. The phase-2 model is one project per
// instance (the roster entry names a project), so a config driving several
// projects derives one workdir per project from that project's repo; a
// derivation failure for ANY of them refuses the whole instance — the daemon
// runs everywhere its sessions run, or nowhere. The map is keyed by project
// slug; the single-project config derives under the empty slug.
func deriveInstanceWorkdirs(cfg *config.Config, root string) (map[string]string, error) {
	slugs := make([]string, 0, len(cfg.Projects)+1)
	if len(cfg.Projects) == 0 {
		slugs = append(slugs, "")
	} else {
		for _, p := range cfg.Projects {
			slugs = append(slugs, p.Slug)
		}
	}
	out := make(map[string]string, len(slugs))
	for _, slug := range slugs {
		label := slug
		if slug == "" {
			label = "the instance"
		}
		repo, err := instancePrimaryRepo(cfg, slug)
		if err != nil {
			return nil, fmt.Errorf("project %s: %w", label, err)
		}
		dir, err := DeriveWorkdir(root, repo)
		if err != nil {
			return nil, fmt.Errorf("project %s: %w", label, err)
		}
		out[slug] = dir
	}
	return out, nil
}

// instancePrimaryRepo is the repo identity one project's workdir derives from:
// the project's primary repo (per-project overriding the top-level), with the
// config's own documented fallback — with exactly one repo declared and no
// primary named, that one is primary implicitly. A project that names no repo
// is a REFUSAL, not a skip: a daemon whose workdir cannot be derived must not
// run on an underived one, which is the exact fallback shape CLA-441 forbids.
func instancePrimaryRepo(cfg *config.Config, slug string) (string, error) {
	if primary := strings.TrimSpace(cfg.PrimaryRepoFor(slug)); primary != "" {
		return primary, nil
	}
	repos := cfg.ReposFor(slug)
	if len(repos) == 1 {
		for sole := range repos {
			return strings.TrimSpace(sole), nil
		}
	}
	if len(repos) > 1 {
		return "", fmt.Errorf("%w: names %d repos and no primary_repo to derive from", ErrWorkdirRefused, len(repos))
	}
	return "", fmt.Errorf("%w: names no repo (set primary_repo) to derive its workdir from", ErrWorkdirRefused)
}

// basenameOfRepo strips the owner segment of a repo identity: "owner/name" ->
// "name". A bare name passes through unchanged.
func basenameOfRepo(repo string) string {
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		return repo[i+1:]
	}
	return repo
}

// expandHome expands a leading ~ like every other path this config accepts.
func expandHome(p string) string {
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
