package supervisor

// Workdir derivation (phase 2a of docs/proposals/daemon-supervisor.md in the
// clankerbar repo; per-project roots per CLA-678): each supervised child's
// workdir comes from one machine-stated root plus the project the instance
// belongs to and the repo its tasks route to. The layout is
//
//	<root>/<project slug>/                the project root - one project's world
//	<root>/<project slug>/<repo name>     the fleet's clone of one repo
//	<root>/<project slug>/worktrees/      every worktree the fleet makes here
//
// The irreducible local input stays one env var (the API key) and one path (the
// root); everything else the derivation needs names a repo, never a path. The
// project level is what makes cross-repo work inside one project possible at
// all: an opencode session's permission scope is the PROJECT ROOT (CLA-678), so
// `**` allow / `../**` deny means exactly "this project's repos and worktrees"
// rather than "one checkout and whatever happens to sit beside it".
//
// Derivation FAILS CLOSED, the conditions learned from CLA-441 and kept:
//
//   - The derived repo directory is CLONED when it does not exist (CLA-678:
//     the fleet owns these clones, so a missing one is materialized from the
//     plane's repo identity). The clone is the ONLY creation; a directory that
//     cannot be cloned refuses.
//   - A derived directory that EXISTS but is not a checkout of the repo the
//     project names is a REFUSAL with the path that was tried - a plain dir, a
//     checkout of a different repo, a repo with no attributable remote. The
//     developer's own unrelated tree is never adopted, and never touched.
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
	"context"
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

// WorktreesDirName is the directory every worktree the fleet creates for a
// project lives under: <root>/<project slug>/worktrees (see config, which owns
// the name because the driver's permission scope and the briefs that name it
// have to agree on one spelling).
const WorktreesDirName = config.WorktreesDirName

// ProjectRoot maps the machine-stated root plus a project slug to the directory
// that holds everything the fleet owns for that project:
// `<root>/<project slug>`. The root may be relative (resolved against the
// supervisor's cwd, like every other path this config resolves) or start with
// ~.
//
// Nothing is created here: this is the pure path derivation the other two
// derivation steps build on, and a caller that only wants to name the project's
// world (doctor, a log line) must not create one as a side effect.
func ProjectRoot(root, project string) (string, error) {
	root = expandHome(strings.TrimSpace(root))
	if root == "" {
		return "", fmt.Errorf("%w: no machine workdir root to derive from", ErrWorkdirRefused)
	}
	slug, err := projectSegment(project)
	if err != nil {
		return "", err
	}
	dir, err := filepath.Abs(filepath.Join(root, slug))
	if err != nil {
		return "", fmt.Errorf("%w: cannot resolve %s (derived from root %s and project %s): %v", ErrWorkdirRefused, filepath.Join(root, slug), root, slug, err)
	}
	return dir, nil
}

// DeriveWorktreesDir is the directory every worktree the fleet creates for one
// project lives under: `<root>/<project slug>/worktrees` (see WorktreesDirName).
//
// Unlike a repo clone, this directory IS created: it is the fleet's own area,
// not the operator's, and a session told to make a worktree in a directory that
// does not exist yet would fail on the first step of its brief. It is created
// inside the project root, and only there.
func DeriveWorktreesDir(root, project string) (string, error) {
	pr, err := ProjectRoot(root, project)
	if err != nil {
		return "", err
	}
	// The project root and the worktree directory are the fleet's own area.
	// A SYMLINK at either is not: it is a link to a tree somewhere else, and
	// following it would put the sessions' scope (and every worktree a task
	// creates) outside the project root the layout promises. The worktrees
	// name is Lstat-ed BEFORE MkdirAll, because MkdirAll accepts an existing
	// symlinked directory without complaint and would create through it.
	if err := checkProjectRootPath(pr); err != nil {
		return "", fmt.Errorf("%w: %v", ErrWorkdirRefused, err)
	}
	dir := filepath.Join(pr, WorktreesDirName)
	if fi, err := os.Lstat(dir); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%w: the project's worktree directory %s is a symlink - it must be a real directory inside the project root, never a link to a tree outside it", ErrWorkdirRefused, dir)
		}
		if !fi.IsDir() {
			return "", fmt.Errorf("%w: the project's worktree directory %s exists and is not a directory", ErrWorkdirRefused, dir)
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("%w: cannot stat the project's worktree directory %s: %v", ErrWorkdirRefused, dir, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("%w: cannot create the project's worktree directory %s: %v", ErrWorkdirRefused, dir, err)
	}
	if err := requireInsideProjectRoot(pr, dir); err != nil {
		return "", fmt.Errorf("%w: %v", ErrWorkdirRefused, err)
	}
	return dir, nil
}

// DeriveWorkdir maps one repo identity to the workdir a supervised child for it
// runs in, from the machine-stated root and the project's slug:
//
//	<root>/<project slug>/<repo name>
//
// The last path segment of the identity names the directory, so
// `lecstor/clankerbar` derives to <root>/<slug>/clankerbar.
//
// A MISSING directory is materialized by CLONING the repo into it (CLA-678) —
// the fleet works in its own clones, so the operator's layout is neither
// required nor read. The clone reads the plane's repo identity and the run's
// git credential (see cloneRepo); a clone that cannot run refuses with the
// path tried.
//
// Fail-closed after the clone, in order: the derived directory must EXIST and
// be a checkout of the expected repo — its attributable origin remote must name
// the identity (see delivery.MatchesRepoSlug for the comparison and its
// boundaries). A directory that exists but is a checkout of a different repo,
// or no checkout at all, is a REFUSAL naming the path: the developer's own
// tree is never adopted, mutated, or re-cloned over.
func DeriveWorkdir(ctx context.Context, root, project, repo string) (string, error) {
	pr, err := ProjectRoot(root, project)
	if err != nil {
		return "", err
	}
	repo = strings.TrimSuffix(strings.TrimSpace(repo), "/")
	if repo == "" {
		return "", fmt.Errorf("%w: no repo to derive from - the instance's project names none", ErrWorkdirRefused)
	}
	dir := filepath.Join(pr, basenameOfRepo(repo))
	if err := checkProjectRootPath(pr); err != nil {
		return "", fmt.Errorf("%w: %v", ErrWorkdirRefused, err)
	}
	switch fi, statErr := os.Lstat(dir); {
	case statErr == nil:
		// The path exists. It is either the fleet's own clone (verified below)
		// or somebody else's tree, and a tree that exists is NEVER replaced:
		// cloning over it would destroy work, and adopting it would read the
		// wrong repo. So: verify, or refuse.
		//
		// Lstat, not Stat: a SYMLINK here is refused outright. It is the one
		// shape that passes every other check while pointing somewhere else —
		// a link to a matching checkout OUTSIDE the project root would verify
		// its origin and then be handed to sessions as this project's clone,
		// so the fleet would read and edit a developer's own tree and the
		// session's project-root scope would reach outside the project.
		if fi.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%w: %s is a symlink - the fleet's clone is a real directory inside the project root, never a link to a checkout outside it", ErrWorkdirRefused, dir)
		}
		if !fi.IsDir() {
			return "", fmt.Errorf("%w: %s exists and is not a directory - refusing to replace it", ErrWorkdirRefused, dir)
		}
	case os.IsNotExist(statErr):
		if err := cloneRepo(ctx, dir, repo); err != nil {
			return "", fmt.Errorf("%w: %s does not exist and could not be cloned from %s: %v", ErrWorkdirRefused, dir, repo, err)
		}
	default:
		return "", fmt.Errorf("%w: cannot stat %s (derived from root %s and repo %s): %v", ErrWorkdirRefused, dir, root, repo, statErr)
	}
	// Belt and braces after materialization: the root must STILL be a real
	// directory (it may have been swapped for a link while the clone ran), and
	// the derived path must canonically remain inside it. A link anywhere above
	// the final component is refused rather than granted.
	if err := checkProjectRootPath(pr); err != nil {
		return "", fmt.Errorf("%w: %v", ErrWorkdirRefused, err)
	}
	if err := requireInsideProjectRoot(pr, dir); err != nil {
		return "", fmt.Errorf("%w: %v", ErrWorkdirRefused, err)
	}
	remote, err := checkoutOriginRemote(dir)
	if err != nil {
		// A directory that exists but is not a checkout at all is the
		// developer's own tree sitting where the clone belongs. Refusing is
		// the whole point: neither adopting nor deleting it is ours to do.
		return "", fmt.Errorf("%w: %s is not a checkout of %s: %v", ErrWorkdirRefused, dir, repo, err)
	}
	if !delivery.MatchesRepoSlug(remote, repo) {
		return "", fmt.Errorf("%w: %s is a checkout of a different repo - its origin remote names %q, expected %s", ErrWorkdirRefused, dir, remote, repo)
	}
	return dir, nil
}

// checkProjectRootPath refuses a project root that is itself a symlink. The
// root is the one directory the layout derives everything else from, so a link
// there would relocate every clone and every worktree the project has — and the
// permission scope granted from it — to wherever the link points.
func checkProjectRootPath(pr string) error {
	fi, err := os.Lstat(pr)
	switch {
	case err == nil:
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("the project root %s is a symlink - it must be a real directory, never a link to a tree outside the machine root", pr)
		}
		if !fi.IsDir() {
			return fmt.Errorf("the project root %s exists and is not a directory", pr)
		}
		return nil
	case os.IsNotExist(err):
		return nil // DeriveWorkdir/cloneRepo create it; the caller checks again after
	default:
		return fmt.Errorf("cannot stat the project root %s: %v", pr, err)
	}
}

// requireInsideProjectRoot checks that path really is inside pr, comparing
// CANONICAL paths: a lexical filepath.Rel says yes to <root>/<slug>/worktrees
// even when that name is a symlink to somewhere else entirely. Both sides are
// resolved with EvalSymlinks, so a link anywhere along either path is seen for
// where it actually points.
func requireInsideProjectRoot(pr, path string) error {
	canonRoot, err := filepath.EvalSymlinks(pr)
	if err != nil {
		return fmt.Errorf("cannot resolve the project root %s: %v", pr, err)
	}
	canonPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("cannot resolve %s: %v", path, err)
	}
	if !isWithinDir(canonRoot, canonPath) {
		return fmt.Errorf("%s resolves to %s, which is outside the project root %s - a symlink there would put the session's scope outside the project", path, canonPath, canonRoot)
	}
	return nil
}

// isWithinDir reports whether path is dir itself or sits below it, on CLEANED
// absolute paths.
func isWithinDir(dir, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// projectSegment validates and normalizes the project slug that becomes one
// path segment of the derived layout. A slug is a plane slug (lowercase, no
// separators); anything that would escape the root or name no project at all is
// refused rather than silently cleaned into a different directory.
func projectSegment(project string) (string, error) {
	slug := strings.TrimSpace(project)
	switch slug {
	case "":
		return "", fmt.Errorf("%w: the instance names no project slug to derive a project root from", ErrWorkdirRefused)
	case ".", "..":
		return "", fmt.Errorf("%w: %q is not a project slug", ErrWorkdirRefused, slug)
	}
	if strings.ContainsAny(slug, `/\`) || strings.ContainsRune(slug, 0) {
		return "", fmt.Errorf("%w: %q is not a single path segment", ErrWorkdirRefused, slug)
	}
	return slug, nil
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

// deriveInstanceWorkdirs derives the PROJECT ROOT for every project one
// instance drives, from the machine-stated root, materializing each project's
// repos and its worktree directory under it. The phase-2 model is one project
// per instance (the roster entry names a project), so a config driving several
// projects derives one project root per project; a derivation failure for ANY
// of them refuses the whole instance — the daemon runs everywhere its sessions
// run, or nowhere. The map is keyed by project slug; the single-project config
// derives under the slug its MCP endpoint names.
//
// This is the one place the fleet reaches the network outside a session: a repo
// the project declares but has not cloned is cloned here, once, at reconcile.
// Everything already present is only verified (and never touched).
func deriveInstanceWorkdirs(ctx context.Context, cfg *config.Config, root string) (map[string]string, error) {
	slugs := make([]string, 0, len(cfg.Projects)+1)
	if len(cfg.Projects) == 0 {
		slugs = append(slugs, cfg.Slug())
	} else {
		for _, p := range cfg.Projects {
			slugs = append(slugs, p.Slug)
		}
	}
	out := make(map[string]string, len(slugs))
	for _, slug := range slugs {
		label := slug
		if label == "" {
			label = "the instance"
		}
		pr, err := ProjectRoot(root, slug)
		if err != nil {
			return nil, fmt.Errorf("project %s: %w", label, err)
		}
		repos, err := projectRepos(cfg, slug)
		if err != nil {
			return nil, fmt.Errorf("project %s: %w", label, err)
		}
		if len(repos) == 0 {
			// A daemon whose world cannot be derived must not run in an
			// underived one, which is the exact fallback shape CLA-441 forbids:
			// sessions would start in the project root with no repo at all.
			return nil, fmt.Errorf("project %s: %w: names no repo (set primary_repo) to materialize", label, ErrWorkdirRefused)
		}
		for _, repo := range repos {
			if _, err := DeriveWorkdir(ctx, root, slug, repo); err != nil {
				return nil, fmt.Errorf("project %s: %w", label, err)
			}
		}
		if _, err := DeriveWorktreesDir(root, slug); err != nil {
			return nil, fmt.Errorf("project %s: %w", label, err)
		}
		out[slug] = pr
	}
	return out, nil
}

// projectRepos lists the repo identities one project's fleet clones and scopes
// to: every identity the project's `repos` map names (its keys — the value is
// an optional local path, not an identity), plus the project's primary repo.
// Sorted and deduplicated so the derivation's log lines and failures are stable
// across runs.
//
// Deduplication is by the CHECKOUT NAME the layout derives — the identity's
// last path segment — not by the identity string: <project root>/<name> is the
// only directory a repo gets, so two spellings of one name are one checkout.
// A bare `widgets` key beside the full `acme/widgets` primary is exactly that
// alias: the full identity is the one a clone URL can be derived from, so it
// materializes the checkout and the uncloneable bare name is never attempted
// first (which would refuse the whole instance over a repo the primary already
// supplies). Two FULL identities that share a name (acme/api and other/api)
// cannot both be materialized at one path, and that conflict is returned here,
// before anything is cloned, naming both identities and the directory — the
// project configuration is what needs correcting.
//
// A project that names NO repo at all is a REFUSAL, not a skip: a daemon whose
// world cannot be derived must not run in an underived one, which is the exact
// fallback shape CLA-441 forbids.
func projectRepos(cfg *config.Config, slug string) ([]string, error) {
	repos := cfg.ReposFor(slug)
	primary := strings.TrimSpace(cfg.PrimaryRepoFor(slug))

	out := make([]string, 0, len(repos)+1)
	byName := make(map[string]string, len(repos)+1) // derived checkout name -> identity
	add := func(id string) error {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil
		}
		name := basenameOfRepo(id)
		prev, ok := byName[name]
		if !ok {
			byName[name] = id
			out = append(out, id)
			return nil
		}
		if prev == id {
			return nil
		}
		full := func(s string) bool { return strings.Contains(s, "/") }
		switch {
		case full(prev) && !full(id):
			// A bare spelling of an identity already materialized: the full
			// spelling is the clone's identity, so this adds nothing.
			return nil
		case !full(prev) && full(id):
			// The bare spelling came first; upgrade it IN PLACE to the full
			// identity, so the clone is attempted under the name that can
			// actually produce a clone URL.
			for i := range out {
				if out[i] == prev {
					out[i] = id
					break
				}
			}
			byName[name] = id
			return nil
		default:
			return fmt.Errorf("%w: repos %s and %s both derive the checkout %s - two repos cannot share one name under the project root; rename one or drop it from the project", ErrWorkdirRefused, prev, id, name)
		}
	}
	for _, id := range sortedKeys(repos) {
		if err := add(id); err != nil {
			return nil, err
		}
	}
	if err := add(primary); err != nil {
		return nil, err
	}
	return out, nil
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
