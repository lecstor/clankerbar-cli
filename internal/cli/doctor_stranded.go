package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// --- 7. stranded commits ----------------------------------------------------

// checkStranded reports commits that exist only on this machine: reachable from
// a local branch or a worktree HEAD (including detached ones), and from no
// remote-tracking ref.
//
// The failure this exists for is silent and irreversible, not loud and
// expensive: on 2026-08-28 three commits were found that existed in exactly one
// place on earth — local `main` in one checkout, on no remote branch and no
// other local branch. They had been committed on a permanent branch the
// workflow forbids (so they were unpushable) and in a worktree under
// /private/tmp, which macOS sweeps; when the tree went, the only surviving
// pointer was a local ref nobody looks at. The causes vary — a swept temp dir,
// an abandoned worktree, a stale checkout, an iteration that died mid-flight —
// and they all have one signature: commits reachable from a local ref and from
// no remote ref.
//
// This check is deliberately inert: it reports and recommends, and never
// deletes, moves, pushes or rebases. A clean worktree is not evidence of
// abandonment, and a live holder may be mid-task, so acting on someone else's
// tree is dangerous — a report that surfaces the work and does nothing is safe
// precisely because it does nothing.
func checkStranded(ctx context.Context, cfg *config.Config, e doctorEnv) check {
	c := check{name: "stranded", status: pass}

	repos := strandedRepos(ctx, cfg, e)
	if len(repos) == 0 {
		c.detail = "no git repositories found under " + strings.Join(configuredWorkDirs(cfg), ", ")
		return c
	}

	// One check for every repo, with the findings carrying the repo path when
	// there is more than one: the report must summarise, not dump — the CLI repo
	// alone has 27 worktrees, and a per-tip line for every clean one is exactly
	// the noise an operator learns to skim past.
	multi := len(repos) > 1
	var lines []string
	withFindings, unchecked := 0, 0
	for _, repo := range repos {
		report, found := strandedFindings(ctx, e.gitRun, repo)
		// A repo is counted as unchecked when none of its lines is a REAL
		// finding — read failure, or every per-tip comparison failed: its only
		// lines are errors, and the summary sentence must not read it as work
		// that exists only on this machine. A repo with findings counts as one
		// that holds stranded commits even when a line beside them is an error.
		if len(report) > 0 && !found {
			unchecked++
		}
		if found {
			withFindings++
		}
		for _, f := range report {
			// Error lines name the repo even when it is the only one in scope:
			// the remedy points at "the repo named above", and a bare "could
			// not enumerate" line names nothing. Finding lines stay bare — the
			// ref or worktree path is the handle a human rescues by — and take
			// the repo prefix only when more than one repo is in scope.
			if multi || f.err {
				f.text = repo + ": " + f.text
			}
			lines = append(lines, f.text)
		}
	}

	if len(lines) == 0 {
		c.detail = fmt.Sprintf("%d %s checked — no commit exists only on this machine",
			len(repos), plural(len(repos), "repo", "repos"))
		return c
	}
	c.status = warn
	switch {
	case unchecked > 0 && withFindings == 0:
		c.detail = fmt.Sprintf("%d of %d %s could not be fully checked",
			unchecked, len(repos), plural(len(repos), "repo", "repos"))
		c.remedy = "run the failing git command by hand in the repo named above, or accept the gap"
	case unchecked > 0:
		c.detail = fmt.Sprintf("%d of %d %s could not be fully checked, and %d hold commits that exist only on this machine",
			unchecked, len(repos), plural(len(repos), "repo", "repos"), withFindings)
		c.remedy = "run the failing git command by hand in the repo named above, and move any live work to a branch and push it"
	default:
		c.detail = fmt.Sprintf("%d of %d %s hold commits that exist only on this machine",
			withFindings, len(repos), plural(len(repos), "repo", "repos"))
		c.remedy = "these commits are on no remote, so only this machine has them — nothing was changed; move any live work to a branch and push it, or delete the ref if it is debris"
	}
	c.info = lines
	return c
}

// strandedRepos lists the repositories the stranded check reports on: every
// checkout the config resolves to for each project — the declared repos
// (CLA-437), and the checkouts under the project's workdir.
//
// The workdir scan is scoped to the project's own repos when a slug is
// configured: a multi-repo parent holds checkouts that belong to other
// projects, and warning about every one of them is how a WARN line becomes
// something an operator skims. The project's repos are taken to be the ones
// named after its slug — the same deliberate under-reporting direction as
// detectToolchains: a missed report is a one-off, a wall of irrelevant
// warnings buries the one that mattered.
func strandedRepos(ctx context.Context, cfg *config.Config, e doctorEnv) []string {
	type target struct {
		dir     string
		slug    string
		repos   map[string]string
		primary string
	}
	var targets []target
	if len(cfg.Projects) == 0 {
		targets = append(targets, target{dir: cfg.WorkDir, repos: cfg.ReposFor(""), primary: cfg.PrimaryRepoFor("")})
	} else {
		for _, p := range cfg.Projects {
			targets = append(targets, target{
				dir:     projectWorkDir(cfg, p),
				slug:    p.Slug,
				repos:   cfg.ReposFor(p.Slug),
				primary: cfg.PrimaryRepoFor(p.Slug),
			})
		}
	}

	// delivery.Repos returns working-tree dirs deduplicated by repository, so
	// linked worktrees never double-report. The declared checkouts can collide
	// with the scan the other way round — a declared path that is a WORKTREE of
	// a repo the scan also found: two paths, one repository. Dedupe the
	// combined list by the shared git directory, the same identity
	// candidateRepos uses, so a repository is checked once no matter how many
	// of its trees the config names.
	seen := map[string]bool{}
	var out []string
	add := func(dir string) {
		if dir == "" {
			return
		}
		key := repoCommonDir(ctx, e.gitRun, dir)
		if key == "" {
			// The repo could not be read at all: keep it on its path — the
			// check's error line will say why — rather than silently dropping
			// it under a key we cannot know.
			key = resolvePath(dir)
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, dir)
		}
	}
	for _, t := range targets {
		dir := t.dir
		if dir == "" {
			dir = "."
		}
		// Declared repos first: an explicit `repos` path is the project's repo
		// by declaration, even when the checkout lives outside the workdir.
		for _, checkout := range config.DeclaredCheckouts(t.repos, t.primary, dir) {
			add(checkout)
		}
		for _, repo := range e.repos(ctx, dir) {
			if t.slug != "" && !repoHasSlug(repo, dir, t.slug) {
				continue
			}
			add(repo)
		}
	}
	return out
}

// repoHasSlug reports whether repo is one of the project's own checkouts: the
// workdir itself (whatever its name — the single-repo case, where the workdir
// IS the checkout), or a checkout whose directory name starts with the slug.
func repoHasSlug(repo, workdir, slug string) bool {
	if resolvePath(repo) == resolvePath(workdir) {
		return true
	}
	return strings.HasPrefix(filepath.Base(repo), slug)
}

// repoCommonDir is the shared git directory of a checkout — the identity a
// repository keeps across all its working trees. The main checkout reports
// ".git" relative to itself, a linked worktree reports the common dir as an
// absolute path; both are normalised to the same resolved path, so two trees
// of one repo compare equal. Empty when the repo cannot be read.
func repoCommonDir(ctx context.Context, gitRun func(ctx context.Context, dir string, args ...string) (string, error), dir string) string {
	out, err := gitRun(ctx, dir, "rev-parse", "--git-common-dir")
	if err != nil || out == "" {
		return ""
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(dir, out)
	}
	return resolvePath(out)
}

// strandedLine is one report line for a repo: a finding naming a tip that
// reaches stranded commits, or an error line naming a git read that failed.
// err lines are prefixed with the repo path by the caller even in single-repo
// mode, so the repo the remedy points at ("run the failing git command by hand
// in the repo named above") is actually named; finding lines stay bare, the
// ref or worktree path being the handle a human rescues by.
type strandedLine struct {
	text string
	err  bool
}

// strandedFindings returns the lines to report for one repository — a finding
// per tip that reaches a commit no remote-tracking ref reaches, and an error
// line per git read that failed — and whether any line is a REAL finding
// (commits established to exist only on this machine).
//
// A finding names the ref (a branch) or the worktree path (a detached HEAD —
// the only handle a detached commit has), how many commits are stranded, and
// the newest one's subject and date: the three facts a human needs to tell live
// work from debris at a glance.
func strandedFindings(ctx context.Context, gitRun func(ctx context.Context, dir string, args ...string) (string, error), repo string) ([]strandedLine, bool) {
	branches, err := gitRun(ctx, repo, "for-each-ref", "--format=%(refname:short) %(objectname)", "refs/heads")
	if err != nil {
		return []strandedLine{{text: "could not enumerate the local branches: " + err.Error(), err: true}}, false
	}
	listing, err := gitRun(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return []strandedLine{{text: "could not enumerate the worktrees: " + err.Error(), err: true}}, false
	}

	// Tips, deduplicated by sha with the branch name winning: a detached HEAD
	// that happens to sit on a branch tip is the same commits, and the branch is
	// the stable handle a human rescues by.
	names := map[string]string{}
	var shas []string
	add := func(sha, name string) {
		if sha == "" {
			return
		}
		if _, ok := names[sha]; ok {
			return
		}
		names[sha] = name
		shas = append(shas, sha)
	}
	for _, line := range strings.Split(branches, "\n") {
		if name, sha, ok := strings.Cut(line, " "); ok {
			add(sha, name)
		}
	}
	for _, wt := range parseStrandedWorktrees(listing) {
		add(wt.head, "detached HEAD at "+wt.path)
	}
	if len(shas) == 0 {
		return nil, false // an unborn repository has no commits to strand
	}

	remotes, err := gitRun(ctx, repo, "for-each-ref", "refs/remotes")
	if err != nil {
		return []strandedLine{{text: "could not read the remote-tracking refs: " + err.Error(), err: true}}, false
	}
	if remotes == "" {
		// No remote-tracking refs at all: every local commit qualifies, and
		// per-tip lines would dump the whole history. One finding names the
		// state instead — the whole repository is on no remote.
		all, err := gitRun(ctx, repo, strandedRevListArgs(shas)...)
		if err != nil {
			return []strandedLine{{text: "could not enumerate the local commits: " + err.Error(), err: true}}, false
		}
		if all == "" {
			return nil, false
		}
		n := strings.Count(all, "\n") + 1
		subject, date, err := newestCommit(ctx, gitRun, repo, firstLine(all))
		if err != nil {
			return []strandedLine{{text: fmt.Sprintf("no remote-tracking refs — %s exist only on this machine, but the newest could not be read: %s",
				plural(n, "1 commit", fmt.Sprintf("%d commits", n)), err.Error()), err: true}}, true
		}
		return []strandedLine{{text: fmt.Sprintf("no remote-tracking refs — %s exist only on this machine, newest %q (%s)",
			plural(n, "1 commit", fmt.Sprintf("%d commits", n)), truncate(subject, 60), date)}}, true
	}

	var lines []strandedLine
	found := false
	for _, sha := range shas {
		stranded, err := gitRun(ctx, repo, "rev-list", sha, "--not", "--remotes")
		if err != nil {
			lines = append(lines, strandedLine{text: names[sha] + ": could not compare against the remotes: " + err.Error(), err: true})
			continue
		}
		if stranded == "" {
			continue
		}
		// The rev-list output above IS the finding — commits established to be
		// on no remote — even when the newest one's subject cannot be read.
		found = true
		// rev-list prints newest first, one sha per line; the runner trimmed the
		// trailing newline, so a non-empty list has count = newlines + 1.
		n := strings.Count(stranded, "\n") + 1
		subject, date, err := newestCommit(ctx, gitRun, repo, firstLine(stranded))
		if err != nil {
			lines = append(lines, strandedLine{text: fmt.Sprintf("%s: %s stranded, but the newest could not be read: %s",
				names[sha], plural(n, "1 commit", fmt.Sprintf("%d commits", n)), err.Error()), err: true})
			continue
		}
		lines = append(lines, strandedLine{text: fmt.Sprintf("%s: %s, newest %q (%s)",
			names[sha], plural(n, "1 commit", fmt.Sprintf("%d commits", n)), truncate(subject, 60), date)})
	}
	return lines, found
}

// strandedRevListArgs builds `rev-list <tips...> --not --remotes`: every commit
// reachable from a tip and from no remote-tracking ref. Built as one slice so
// the two call sites cannot drift apart on the arg order.
func strandedRevListArgs(shas []string) []string {
	out := make([]string, 0, len(shas)+3)
	out = append(out, "rev-list")
	out = append(out, shas...)
	out = append(out, "--not", "--remotes")
	return out
}

// newestCommit renders a commit's subject and short date — the two facts the
// stranded report promises per finding.
func newestCommit(ctx context.Context, gitRun func(ctx context.Context, dir string, args ...string) (string, error), repo, sha string) (subject, date string, err error) {
	out, err := gitRun(ctx, repo, "show", "-s", "--format=%s%n%ad", "--date=short", sha)
	if err != nil {
		return "", "", err
	}
	subject, date, _ = strings.Cut(out, "\n")
	return subject, date, nil
}

// strandedWorktree is one entry of `git worktree list --porcelain`, kept only
// for the entries that matter to the stranded check: detached heads.
type strandedWorktree struct {
	path     string
	head     string
	detached bool
}

// parseStrandedWorktrees reads `git worktree list --porcelain`: blank-line-
// separated blocks, each led by `worktree <path>` with `HEAD <sha>` and, when a
// branch is checked out, `branch refs/heads/<name>`. A `detached` block has no
// branch line and is exactly the tip no branch names. Bare blocks (no working
// tree) are dropped: their HEAD is a branch, so the branch enumeration already
// covers it.
func parseStrandedWorktrees(listing string) []strandedWorktree {
	var out []strandedWorktree
	var cur strandedWorktree
	flush := func() {
		if cur.detached && cur.path != "" && cur.head != "" {
			out = append(out, cur)
		}
		cur = strandedWorktree{}
	}
	for _, line := range strings.Split(listing, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.TrimSpace(line) == "":
			flush()
		case strings.HasPrefix(line, "worktree "):
			flush()
			cur.path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "HEAD "):
			cur.head = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "detached"):
			cur.detached = true
		}
	}
	flush()
	return out
}
