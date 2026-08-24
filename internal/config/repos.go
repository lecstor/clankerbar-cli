package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrRepoNotFound is the sentinel for a repo identity that resolves to no local
// checkout. Its text is the iteration-failure code the loop surfaces verbatim:
// a session that would otherwise start in the multi-repo parent — the exact
// wrong-repo-reads and cwd-scoped-grant shape this resolution exists to kill
// (docs/proposals/agent-rule-scoping.md, test cases 1 and 2) — is refused
// instead, loudly, with the identity that could not be resolved.
var ErrRepoNotFound = errors.New("repo_not_found")

// ResolveCheckout maps one repo identity to the local checkout a session for it
// should start in.
//
// The identity is the task's `repo` field (`owner/name`), or — when the caller
// passes "" (no repo known) — the project's PRIMARY repo, which is what keeps a
// session out of the workdir even when the plane names no repo yet. Resolution
// order, first match wins:
//
//  1. an explicit `repos` entry keyed by the FULL identity ("owner/name");
//  2. an explicit `repos` entry keyed by its bare name ("name") — the common
//     spelling, since local checkouts are named by their last path segment;
//  3. the workdir itself, when its own basename IS the bare name — the
//     single-repo layout, where the operator points `workdir` at the checkout;
//  4. `<workdir>/<bare name>`, when that directory exists — the multi-repo
//     parent convention, no declaration needed;
//  5. nothing: ErrRepoNotFound. Never "start in the workdir anyway" — a silent
//     fallback to the parent is the bug being fixed, so once anything is
//     configured (or any step above could have matched), failure is loud.
//
// An empty identity is resolved through the same steps against the primary:
// `primary_repo` when set, else the sole `repos` key when exactly one is
// declared, else — only when NOTHING is configured at all — ("", nil), meaning
// "this config has no opinion"; the caller then keeps its legacy workdir
// behaviour, which is what keeps every existing single-repo config working
// unchanged. A project that declares repos but neither a resolvable primary nor
// a sole entry gets ErrRepoNotFound: ambiguity is the operator's to resolve,
// not the driver's to guess.
//
// Configured paths may be relative (resolved against workdir, like every other
// path in this config) or start with ~. Every result is verified to exist as a
// directory before it is returned — a declared-but-absent path fails here, at
// the resolution step, rather than as a cryptic exec error three frames deeper.
func ResolveCheckout(repos map[string]string, primary, workdir, repo string) (string, error) {
	if workdir == "" {
		workdir = "."
	}
	featureOn := len(repos) > 0 || strings.TrimSpace(primary) != ""
	identity := strings.TrimSpace(repo)
	if identity == "" {
		if !featureOn {
			return "", nil // legacy: nothing declared, no opinion, keep the workdir
		}
		primary = strings.TrimSpace(primary)
		if primary == "" {
			if len(repos) != 1 {
				return "", fmt.Errorf("%w: the task names no repo and the project declares %d repos with no primary_repo to fall back to", ErrRepoNotFound, len(repos))
			}
			for sole := range repos {
				primary = sole
			}
		}
		identity = primary
	}
	dir, ok := lookupCheckout(repos, workdir, identity)
	if !ok {
		return "", fmt.Errorf("%w: %q resolves to no local checkout under %s (declare it in repos, or check out it there)", ErrRepoNotFound, identity, displayDir(workdir))
	}
	return dir, nil
}

// DeclaredCheckouts resolves EVERY repo a project declares — each `repos` entry
// plus the primary — to the checkouts a session's permission policy must cover,
// whatever directory the session starts in (agent-rule-scoping piece 2). The
// spawn cwd is NOT included by construction; callers drop it if it collides.
//
// Entries that resolve to nothing are skipped here rather than failed: this is
// the reach-widening list, where omitting an unknown entry degrades to today's
// scope, while failing would stop a run over half a declaration. doctor reports
// each entry's resolution separately, which is where a broken path is seen.
//
// The output is deduplicated (two identities may share a checkout) and sorted
// (stable policy bytes across otherwise-identical configs).
func DeclaredCheckouts(repos map[string]string, primary, workdir string) []string {
	identities := make([]string, 0, len(repos)+1)
	for k := range repos {
		identities = append(identities, k)
	}
	if p := strings.TrimSpace(primary); p != "" {
		identities = append(identities, p)
	}
	sort.Strings(identities)
	var out []string
	seen := map[string]bool{}
	for _, id := range identities {
		if id == "" {
			continue
		}
		dir, ok := lookupCheckout(repos, workdir, id)
		if !ok || seen[dir] {
			continue
		}
		seen[dir] = true
		out = append(out, dir)
	}
	return out
}

// lookupCheckout runs the four matching steps of ResolveCheckout's documented
// order against one identity. ok=false means no step matched; a configured path
// that does not exist also reads as no match (the caller's error says why).
func lookupCheckout(repos map[string]string, workdir, identity string) (string, bool) {
	name := basenameOf(identity)
	if name == "" {
		return "", false
	}
	// 1. full "owner/name" key, 2. bare-name key.
	for _, key := range []string{identity, name} {
		if p, ok := repos[key]; ok && strings.TrimSpace(p) != "" {
			abs, err := absAgainst(expandHome(strings.TrimSpace(p)), workdir)
			if err != nil || !isCheckoutDir(abs) {
				return "", false
			}
			return abs, true
		}
	}
	absWd, err := filepath.Abs(workdir)
	if err != nil {
		return "", false
	}
	absWd = filepath.Clean(absWd)
	// 3. the workdir itself is the checkout (single-repo layout).
	if basenameOf(filepath.ToSlash(absWd)) == name && isCheckoutDir(absWd) {
		return absWd, true
	}
	// 4. the multi-repo-parent convention.
	candidate := filepath.Join(absWd, name)
	if isCheckoutDir(candidate) {
		return candidate, true
	}
	return "", false
}

// basenameOf strips the owner segment of a repo identity: "owner/name" ->
// "name". A bare name passes through unchanged.
func basenameOf(repo string) string {
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		return repo[i+1:]
	}
	return repo
}

// absAgainst resolves a possibly-relative path against base, like underWorkDir
// does for the rest of this config.
func absAgainst(p, base string) (string, error) {
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	return filepath.Clean(p), nil
}

// isCheckoutDir reports whether path exists and is a directory. It deliberately
// does not require a .git entry: a checkout's worktrees carry .git as a FILE,
// and refusing those would refuse exactly the trees sessions work in.
func isCheckoutDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// displayDir renders a workdir in error text: "." is the driver's cwd, which
// reads as nothing to an operator scanning a log.
func displayDir(dir string) string {
	if dir == "." || dir == "" {
		return "the workdir"
	}
	return dir
}

// validateRepos is the structural half of repo-map validation, run by Validate:
// keys and values must be non-blank. It deliberately does NOT stat the
// configured paths - a checkout that appears later (or sits on another machine
// sharing the config) must not refuse every run; resolution fails the iteration
// only when a session actually needs it, and doctor reports each entry's
// reachability before that. A primary naming no declared key is likewise left
// alone: the convention steps of the resolution can still legitimately match it.
func validateRepos(repos map[string]string, label string) error {
	for k, v := range repos {
		if strings.TrimSpace(k) == "" {
			return fmt.Errorf("%s: repos has an entry with an empty name", label)
		}
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%s: repos.%s has an empty path", label, k)
		}
	}
	return nil
}
