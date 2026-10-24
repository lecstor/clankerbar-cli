package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/delivery"
)

// --- 1b. repos ---------------------------------------------------------------

// checkRepos reports, per project (or once for a single-project run), every repo
// the config declares and whether it resolves to a local checkout (CLA-437).
//
// This is the preflight for two silent failures at once. A declared repo whose
// checkout is missing fails every iteration that names it with repo_not_found —
// better seen here than in an overnight log. And a project that declares NO
// repos keeps the legacy workdir behaviour, which is correct but easy to mistake
// for "sessions already start in the task's repo"; saying which mode is live is
// the difference.
func checkRepos(cfg *config.Config) []check {
	report := func(label string, repos map[string]string, primary, workdir string) check {
		c := check{name: label}
		if len(repos) == 0 && strings.TrimSpace(primary) == "" {
			c.status = pass
			c.detail = "none declared - sessions start in the workdir (" + orNone(workdir) + "); declare repos to start them in the task's checkout"
			return c
		}
		c.status = pass
		c.detail = "every declared repo resolves to a checkout"
		idents := make([]string, 0, len(repos)+1)
		for k := range repos {
			idents = append(idents, k)
		}
		sort.Strings(idents)
		if p := strings.TrimSpace(primary); p != "" && !slices.Contains(idents, p) {
			idents = append(idents, p)
		}
		bad := 0
		for _, id := range idents {
			mark := ""
			if strings.TrimSpace(primary) != "" && id == strings.TrimSpace(primary) {
				mark = " (primary)"
			}
			dir, err := config.ResolveCheckout(repos, primary, workdir, id)
			if err != nil {
				bad++
				c.info = append(c.info, id+mark+" -> NOT FOUND")
				continue
			}
			c.info = append(c.info, id+mark+" -> "+dir)
		}
		if bad > 0 {
			c.status = warn
			c.detail = fmt.Sprintf("%d of %d declared repos resolve to no local checkout - any task naming one fails its iteration", bad, len(idents))
			c.remedy = "check the repo out under the workdir, or fix its repos path"
		}
		return c
	}

	if len(cfg.Projects) == 0 {
		return append([]check{report("repos", cfg.ReposFor(""), cfg.PrimaryRepoFor(""), cfg.WorkDir)},
			checkProjectRoot("project root", cfg, cfg.ReposFor(""), cfg.PrimaryRepoFor(""), cfg.WorkDir)...)
	}
	out := make([]check, 0, 2*len(cfg.Projects))
	for _, p := range cfg.Projects {
		workdir := projectWorkDir(cfg, p)
		out = append(out, report("repos["+p.Slug+"]", cfg.ReposFor(p.Slug), cfg.PrimaryRepoFor(p.Slug), workdir))
		out = append(out, checkProjectRoot("project root["+p.Slug+"]", cfg, cfg.ReposFor(p.Slug), cfg.PrimaryRepoFor(p.Slug), workdir)...)
	}
	return out
}

// checkProjectRoot reports on the per-project layout (CLA-678): with
// `project_root` declared, every workdir in the config is a fleet project root
// — <root>/<project slug>, holding that project's clones under
// <project root>/<repo name> and its worktrees under
// <project root>/worktrees — and this check FAILS when the layout does not
// hold, because every one of those facts is load-bearing at run time:
//
//   - a configured repo resolving OUTSIDE the project root is a session
//     reading a tree its permission scope does not cover, or one the fleet
//     will try to clone over;
//   - a clone whose origin names a different repo than the identity it sits
//     under is the wrong-repo read the layout exists to prevent (the same
//     attribution rule the supervisor's derivation enforces, checked here
//     before a daemon is refused at spawn time);
//   - a missing or outside-the-root worktrees directory is where a task
//     worktree fails to be created, which costs an iteration its whole session.
//
// A config that does not declare `project_root` gets no check: there the
// workdir is whatever the operator pointed it at, and its repos are wherever
// the `repos` map says, by design.
func checkProjectRoot(label string, cfg *config.Config, repos map[string]string, primary, workdir string) []check {
	if !cfg.ProjectRoot {
		return nil
	}
	c := check{name: label}
	if strings.TrimSpace(workdir) == "" {
		c.status = fail
		c.detail = "project_root is declared but no workdir names the project root"
		c.remedy = "set workdir to <root>/<project slug>, or drop project_root"
		return []check{c}
	}
	rootAbs, err := filepath.Abs(workdir)
	if err != nil {
		c.status = fail
		c.detail = fmt.Sprintf("workdir %s cannot be resolved: %v", workdir, err)
		return []check{c}
	}
	rootAbs = filepath.Clean(rootAbs)

	var problems []string
	// The project root and everything the layout puts under it are checked on
	// CANONICAL paths: a lexical comparison says <root>/worktrees is inside
	// <root> even when that name is a symlink to a directory on another disk.
	// The supervisor refuses a symlink at any of these paths (see
	// supervisor.DeriveWorkdir), so doctor must fail for the same layout rather
	// than pass a config the derivation will refuse at reconcile time.
	if fi, lerr := os.Lstat(rootAbs); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		problems = append(problems, "the project root "+rootAbs+" is a symlink to "+resolvePath(rootAbs)+" - it must be a real directory, never a link to a tree outside it")
	}
	if fi, statErr := os.Stat(rootAbs); statErr != nil || !fi.IsDir() {
		problems = append(problems, "the project root "+rootAbs+" does not exist")
	}
	canonRoot := resolvePath(rootAbs)
	// The worktree directory is the fleet's own, so it must be the fixed
	// <project root>/worktrees - anything else means the layout is not what
	// the sessions' scope (and their briefs) assume.
	wt := filepath.Join(rootAbs, config.WorktreesDirName)
	if fi, lerr := os.Lstat(wt); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		problems = append(problems, "the worktree directory "+wt+" is a symlink to "+resolvePath(wt)+" - it must be a real directory inside the project root")
	} else if fi, statErr := os.Stat(wt); statErr != nil || !fi.IsDir() {
		problems = append(problems, "the worktree directory "+wt+" is missing (the fleet creates it at reconcile; `clankerbar supervise` makes it on its first derivation)")
	} else if !insideDir(canonRoot, resolvePath(wt)) {
		problems = append(problems, "the worktree directory "+wt+" is outside the project root")
	}

	idents := make([]string, 0, len(repos)+1)
	for k := range repos {
		idents = append(idents, k)
	}
	sort.Strings(idents)
	if p := strings.TrimSpace(primary); p != "" && !slices.Contains(idents, p) {
		idents = append(idents, p)
	}
	// Two FULL identities that end in the same name derive the SAME directory
	// (<project root>/<name>), so only one of them can ever be the clone there
	// and the other reads as a wrong-origin checkout. Name the conflict here,
	// where the operator can fix the config, rather than letting the layout
	// report the symptom. A bare key beside a full identity of the same name is
	// NOT a conflict: it is that repo spelled short, and the derivation
	// materializes the full identity (the only spelling with a clone URL).
	byBase := make(map[string]string, len(idents))
	for _, id := range idents {
		base := filepath.Base(id)
		prev, ok := byBase[base]
		if ok && prev != id && strings.Contains(prev, "/") && strings.Contains(id, "/") {
			problems = append(problems, fmt.Sprintf("%s and %s both derive the directory %s - two repos cannot share one clone path; drop one from the project", prev, id, filepath.Join(rootAbs, base)))
			continue
		}
		if !ok {
			byBase[base] = id
		}
	}
	for _, id := range idents {
		dir, err := config.ResolveCheckout(repos, primary, rootAbs, id)
		if err != nil {
			// The repos check above already reports an unresolvable identity;
			// this one is about the layout, so it stays quiet here.
			continue
		}
		if fi, lerr := os.Lstat(dir); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
			problems = append(problems, fmt.Sprintf("%s (%s) is a symlink to %s - the fleet's clone is a real directory inside the project root, never a link to a checkout outside it", id, dir, resolvePath(dir)))
			continue
		}
		if !insideDir(canonRoot, resolvePath(dir)) {
			problems = append(problems, fmt.Sprintf("%s resolves to %s, which is OUTSIDE the project root - a session's scope covers the project root, so it could not read it, and the fleet's own clone belongs at %s", id, dir, filepath.Join(rootAbs, filepath.Base(dir))))
			continue
		}
		remote, remoteErr := gitRemoteURL(dir)
		if remoteErr != nil {
			problems = append(problems, fmt.Sprintf("%s (%s) has no attributable origin remote: %v", id, dir, remoteErr))
			continue
		}
		if !delivery.MatchesRepoSlug(remote, id) {
			problems = append(problems, fmt.Sprintf("%s is a checkout of a different repo - its origin names %q", id, remote))
		}
	}
	if len(problems) > 0 {
		c.status = fail
		c.detail = strings.Join(problems, "; ")
		c.remedy = "fix the layout, or drop project_root to run the legacy shape (one checkout per workdir)"
		return []check{c}
	}
	c.status = pass
	c.detail = fmt.Sprintf("%s holds every configured repo and its worktree directory", rootAbs)
	return []check{c}
}

// insideDir reports whether path is dir itself or sits below it, comparing
// CLEANED absolute paths so a symlink-free path and its ".."-normalized
// spelling never read as different trees.
func insideDir(dir, path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(abs))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// gitRemoteURL reads a checkout's attributable origin remote ("origin" when it
// has one, else its only remote), the same rule the supervisor's derivation
// applies - a checkout with several remotes and no origin cannot be attributed
// to one repo.
func gitRemoteURL(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "remote").Output()
	if err != nil {
		return "", fmt.Errorf("not a git checkout")
	}
	names := strings.Fields(string(out))
	if len(names) == 0 {
		return "", fmt.Errorf("a git repository with no remotes")
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
			return "", fmt.Errorf("%d remotes and no origin", len(names))
		}
		remote = names[0]
	}
	urlBytes, err := exec.Command("git", "-C", dir, "remote", "get-url", remote).Output()
	if err != nil {
		return "", fmt.Errorf("cannot read remote %s", remote)
	}
	url := strings.TrimSpace(string(urlBytes))
	if url == "" {
		return "", fmt.Errorf("remote %s has an empty URL", remote)
	}
	return url, nil
}
