package delivery

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// resolveRepo finds the repository whose refs carry branch. See the package doc:
// linked worktrees share a ref database, so any working tree of the right
// repository will do, and the search only has to identify the repository.
//
// candidateRepos collects one working tree per distinct repository at or below the
// workdir, deduplicated by the shared git common directory so a repo and its
// linked worktrees count once. Bounded by maxDepth and maxEntries.
func (v *Verifier) candidateRepos(ctx context.Context) []string {
	if v.workdir == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string

	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if ctx.Err() != nil {
			return
		}
		common, root, inRepo := v.repoAt(ctx, dir)
		// `git rev-parse` answers for the nearest ENCLOSING repository, which may be
		// far above dir. Below the workdir that must not count: a `git init`-ed home
		// directory would otherwise match ~/dev itself, and the walk would stop there
		// having found no project at all. At the workdir itself an enclosing repo IS
		// the answer (the single-project case, where the workdir is the checkout or a
		// directory inside it) — but if it merely encloses, keep descending too.
		atRoot := inRepo && sameDir(root, dir)
		if inRepo && (atRoot || depth == 0) {
			if !seen[common] {
				seen[common] = true
				out = append(out, dir)
			}
		}
		if atRoot {
			// Do not descend: everything inside shares these refs, and a nested
			// repository is not part of the worktree convention this serves.
			return
		}
		if depth >= maxDepth {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		probed := 0
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			if probed++; probed > maxEntries {
				return
			}
			walk(filepath.Join(dir, e.Name()), depth+1)
		}
	}
	walk(v.workdir, 0)
	return out
}

// repoAt reports the shared git directory and the working-tree root for dir, and
// whether dir is inside a non-bare repository at all. The common dir (not the
// per-worktree one) is what makes a worktree and its parent repository compare
// equal; the root is what tells "this IS a checkout" from "something above it is".
//
// A bare repository is rejected: sessions never work in one, it has no working tree
// a clanker could have committed in, and — as a mirror or a local remote sitting
// beside the checkouts — it would answer "yes, that branch exists" while having no
// remote of its own to check anything against. It reports not-a-repo rather than
// stopping the walk, so a bare repo's internals are still skipped by the depth
// bound rather than probed directory by directory.
func (v *Verifier) repoAt(ctx context.Context, dir string) (common, root string, ok bool) {
	out, err := v.run(ctx, dir, "rev-parse", "--git-common-dir")
	if err != nil || out == "" {
		return "", "", false
	}
	top, err := v.run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil || top == "" {
		return "", "", false // bare, or otherwise not a working tree
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(dir, out)
	}
	return realPath(out), realPath(top), true
}

// realPath normalises a path for comparison: symlinks resolved where possible
// (macOS temp dirs are /var -> /private/var), lexically cleaned otherwise.
func realPath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

func sameDir(a, b string) bool { return realPath(a) == realPath(b) }

// resolveRepoByURL filters repos to those whose remote URL matches the claim's repo slug.
func (v *Verifier) resolveRepoByURL(ctx context.Context, repos []string, repoSlug string) []string {
	if repoSlug == "" {
		return repos
	}
	var matched []string
	for _, r := range repos {
		url, err := v.remoteURL(ctx, r)
		if err != nil || url == "" {
			continue
		}
		if MatchesRepoSlug(url, repoSlug) {
			matched = append(matched, r)
		}
	}
	if len(matched) == 0 {
		// If no working copy matches, fall back to all repos so the refusal is honest.
		return repos
	}
	return matched
}

// MatchesRepoSlug reports whether a remote URL names the repo slug (owner/name),
// tolerating https and ssh forms. It is the one comparison the codebase uses
// for "is this checkout the repo named X": the delivery verifier uses it to
// attribute a branch to a repo (CLA-351), and the supervisor's workdir
// derivation uses it to refuse a child whose checkout is a different repo
// (CLA-547).
func MatchesRepoSlug(url, slug string) bool {
	if slug == "" {
		return true
	}
	// Tolerate both ssh (git@host:owner/name) and https (https://host/owner/name) forms.
	// The slug is 'owner/name'. We look for the exact sequence after stripping protocol.
	clean := url
	// Strip the two trimmable suffixes in BOTH orders until stable: "name.git/",
	// "name/.git", "/name.git" and trailing "/" all resolve to "…/name".
	for {
		before := clean
		clean = strings.TrimSuffix(clean, ".git")
		clean = strings.TrimSuffix(clean, "/")
		if clean == before {
			break
		}
	}
	clean = strings.TrimPrefix(clean, "https://")
	clean = strings.TrimPrefix(clean, "http://")
	clean = strings.TrimPrefix(clean, "git@")
	// After stripping protocol and ".git", both forms carry the slug as a path
	// segment: ssh 'github.com:lecstor/clankerbar-cli' and https
	// 'github.com/lecstor/clankerbar-cli'. The check requires a BOUNDARY on both
	// sides of the slug — preceded by start, '/' or ':', followed by '/' or end —
	// so `lecstor/clankerbar` never matches `lecstor/clankerbar-cli` (the
	// project's own repo pair: the whole point of CLA-351 is to tell them apart).
	for i := 0; i+len(slug) <= len(clean); i++ {
		if !strings.HasPrefix(clean[i:], slug) {
			continue
		}
		before, after := byte('/'), byte('/')
		if i > 0 {
			before = clean[i-1]
		}
		if j := i + len(slug); j < len(clean) {
			after = clean[j]
		}
		if (before == '/' || before == ':') && (after == '/' || after == 0) {
			return true
		}
	}
	return false
}

// resolveRemote picks the remote to judge against: ProbeRemote's choice, keeping
// the configured name when none can be read. A repo with several remotes and no
// `origin` then fails its remote read loudly, rather than silently checking
// against whichever remote happened to sort first.
func (v *Verifier) resolveRemote(ctx context.Context, repo string) string {
	remote, err := ProbeRemote(ctx, v.run, repo, v.remote)
	if err != nil {
		return v.remote
	}
	return remote
}

// remoteURL reads the URL of the repository's resolved remote, for callers
// that need to know WHERE the remote points (the PR check derives the GitHub
// owner/name from it). Empty with a nil error is not possible here: a remote
// always has a URL or cannot be read.
func (v *Verifier) remoteURL(ctx context.Context, repo string) (string, error) {
	return v.run(ctx, repo, "remote", "get-url", v.resolveRemote(ctx, repo))
}

// lsRemote reads the remote's tip for a branch WITHOUT mutating anything. Empty
// SHA with a nil error means the remote genuinely has no such branch — which is a
// Fail, not an Unknown.
//
// This is a NETWORK command: it goes out scoped to the account that owns the
// remote (runScoped), or a private repo answers "Repository not found" and the
// check degrades to Unknown for want of credentials rather than evidence.
func (v *Verifier) lsRemote(ctx context.Context, repo, remote, branch string) (string, error) {
	out, err := v.runScoped(ctx, repo, "ls-remote", "--heads", "--", remote, "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == "refs/heads/"+branch {
			return fields[0], nil
		}
	}
	return "", nil
}

func (v *Verifier) haveObject(ctx context.Context, repo, sha string) bool {
	_, err := v.run(ctx, repo, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

func (v *Verifier) isAncestor(ctx context.Context, repo, a, b string) bool {
	_, err := v.run(ctx, repo, "merge-base", "--is-ancestor", a, b)
	return err == nil
}
