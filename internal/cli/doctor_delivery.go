package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/delivery"
)

// --- 7b. delivery verifier reach ---------------------------------------------

// checkDelivery reports, per project workdir, whether CLA-253's delivery check
// would find anything to check: git on PATH, at least one non-bare repository
// at or below the workdir, and a remote to judge a claim against. A run whose
// workdir fails any of those still drains the backlog — the verifier fails open
// and says "could not verify" — so this WARNs and never FAILs. The point is
// that the operator sees it before the night, not after it.
//
// It sits beside checkStranded rather than replacing that check's "no git
// repositories found" PASS. Both enumerate the same repositories via
// delivery.Repos, but stranded asks which commits exist only on this machine
// (with no repository there are none), while this asks whether the VERIFIER can
// read one at all — and that is exactly the state stranded passes.
//
// Unlike stranded, the enumeration is not narrowed to repos named after the
// project's slug: the verifier searches whatever the session's workdir holds,
// so a checkout the project does not own by name is still a blind spot to warn
// about.
func checkDelivery(ctx context.Context, cfg *config.Config, e doctorEnv) []check {
	if len(cfg.Projects) == 0 {
		return []check{deliveryCheck(ctx, "delivery", cfg.WorkDir, cfg.ReposFor(""), cfg.PrimaryRepoFor(""), e)}
	}
	out := make([]check, 0, len(cfg.Projects))
	for _, p := range cfg.Projects {
		out = append(out, deliveryCheck(ctx, "delivery["+p.Slug+"]",
			projectWorkDir(cfg, p), cfg.ReposFor(p.Slug), cfg.PrimaryRepoFor(p.Slug), e))
	}
	return out
}

// deliveryCheck is checkDelivery for one project.
func deliveryCheck(ctx context.Context, name, workdir string, repos map[string]string, primary string, e doctorEnv) check {
	c := check{name: name}

	if _, err := e.lookPath("git"); err != nil {
		c.status = warn
		c.detail = "git is not on PATH — the delivery check cannot resolve a repository, so every delivery reports could-not-verify"
		c.remedy = "install git and put it on PATH; until then no delivery claim is checked locally"
		return c
	}

	roots := deliveryRoots(workdir, repos, primary)
	where := strings.Join(roots, ", ")
	if where == "" {
		where = "(no workdir)"
	}

	var found []string
	seen := map[string]bool{}
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		for _, repo := range e.repos(ctx, root) {
			// A declared checkout can be a linked worktree of a repository the
			// workdir scan also found: two paths, one repository. Key on the
			// shared git dir, as strandedRepos does, so it is named once; the
			// first path is kept because it is the one the operator can act on.
			key := repoCommonDir(ctx, e.gitRun, repo)
			if key == "" {
				// Unreadable: no common dir to compare on, so its path is the
				// only identity there is.
				key = resolvePath(repo)
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			found = append(found, repo)
		}
	}
	if len(found) == 0 {
		c.status = warn
		c.detail = "no non-bare git repository at or below " + where +
			" — the delivery check resolves nothing, so every delivery reports could-not-verify"
		c.remedy = "point the project's workdir at the checkout the tasks work in (or a parent holding it), or declare the project's repos; until then deliveries are never verified"
		return c
	}

	reachable := 0
	for _, repo := range found {
		remote, err := delivery.ProbeRemote(ctx, e.gitRun, repo, "origin")
		if err != nil {
			c.info = append(c.info, fmt.Sprintf("%s -> cannot be judged: %v", repo, err))
			continue
		}
		reachable++
		c.info = append(c.info, repo+" -> "+remote)
	}

	switch {
	case reachable == len(found):
		c.status = pass
		c.detail = fmt.Sprintf("the delivery check can resolve %s under %s, each with a remote to judge claims against",
			plural(len(found), "1 repository", fmt.Sprintf("%d repositories", len(found))), where)
	case reachable == 0:
		c.status = warn
		c.detail = fmt.Sprintf("%s found under %s, but none has a remote the delivery check can read — every claim reports could-not-verify",
			plural(len(found), "1 repository", fmt.Sprintf("%d repositories", len(found))), where)
		c.remedy = "add an origin remote to each checkout named above (git remote add origin <url>); the check reads origin, or a repository's only remote"
	default:
		c.status = warn
		c.detail = fmt.Sprintf("%d of %d %s under %s %s no remote the delivery check can read — a claim resolved in one reports could-not-verify",
			len(found)-reachable, len(found), plural(len(found), "repository", "repositories"), where,
			plural(len(found)-reachable, "has", "have"))
		c.remedy = "add an origin remote to the checkout(s) named above (git remote add origin <url>), or accept that claims in them are never verified"
	}
	return c
}

// deliveryRoots lists the directories a project's sessions can be spawned in:
// its workdir, plus every checkout its repos map declares. Sessions start in
// the task's checkout when one is declared (CLA-437), and the verifier is
// rooted there, so a checkout outside the workdir is reachable even though the
// bounded search over the workdir would miss it.
func deliveryRoots(workdir string, repos map[string]string, primary string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(dir string) {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			return
		}
		key := resolvePath(dir)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, dir)
	}
	add(workdir)
	for _, checkout := range config.DeclaredCheckouts(repos, primary, workdir) {
		add(checkout)
	}
	return out
}
