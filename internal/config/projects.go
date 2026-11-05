package config

import (
	"fmt"
	"net/url"
	"strings"
)

// ReposFor resolves the CLA-437 repo -> checkout map for one project: a
// matching projects[] entry's own NON-EMPTY map replaces the top-level one, and
// everything else — an unmatched slug (how a single-project run reaches here
// with no projects at all), or an entry that declared none — falls back to the
// top-level map. Total, so no caller has a second error path.
func (c *Config) ReposFor(slug string) map[string]string {
	for _, p := range c.Projects {
		if p.Slug == slug && len(p.Repos) > 0 {
			return p.Repos
		}
	}
	return c.Repos
}

// PrimaryRepoFor resolves the CLA-437 no-repo fallback for one project: a
// matching projects[] entry's own non-empty value wins, and everything else
// falls back to the top-level field. Total, so no caller has a second error
// path.
func (c *Config) PrimaryRepoFor(slug string) string {
	for _, p := range c.Projects {
		if p.Slug == slug && strings.TrimSpace(p.PrimaryRepo) != "" {
			return p.PrimaryRepo
		}
	}
	return c.PrimaryRepo
}

// AllowUncheckedPRFor resolves the CLA-310 empty-rollup opt-out for one
// project: a matching projects[] entry's own value wins, and everything else
// — including an unmatched slug, which is how a single-project run reaches
// here with an empty one — falls back to the top-level field. Total, so no
// caller has a second error path.
func (c *Config) AllowUncheckedPRFor(slug string) bool {
	for _, p := range c.Projects {
		if p.Slug == slug {
			return p.AllowUncheckedPR
		}
	}
	return c.AllowUncheckedPR
}

// DefaultIntegrationBranch is the branch whose tip doctor's deploy_lag check
// (CLA-322) judges the deployed build against when neither the project nor the
// top-level config names one. Both of this project's repos integrate through
// `staging`, so that is the convention the default encodes; an operator whose
// project integrates elsewhere sets integration_branch explicitly rather than
// getting an answer about a branch they do not have.
const DefaultIntegrationBranch = "staging"

// HealthURLFor resolves the deploy_lag check's /health endpoint for one
// project: a matching projects[] entry's value wins when it is set, and
// everything else falls back to the top-level field. Empty means not
// monitored - there is deliberately no derived default, because a guessed
// environment would answer about the wrong deployment.
func (c *Config) HealthURLFor(slug string) string {
	for _, p := range c.Projects {
		if p.Slug == slug {
			return firstNonEmpty(p.HealthURL, c.HealthURL)
		}
	}
	return c.HealthURL
}

// IntegrationBranchFor resolves the branch the deployed build is judged
// against for one project: a matching projects[] entry's value wins when it is
// set, then the top-level field, then DefaultIntegrationBranch. Total, so the
// check never has to carry its own fallback.
func (c *Config) IntegrationBranchFor(slug string) string {
	for _, p := range c.Projects {
		if p.Slug == slug {
			return firstNonEmpty(p.IntegrationBranch, c.IntegrationBranch, DefaultIntegrationBranch)
		}
	}
	return firstNonEmpty(c.IntegrationBranch, DefaultIntegrationBranch)
}

// firstNonEmpty returns the first argument that is not "", or "" if none is.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// PRBaseFor resolves the operator's per-repo PR-base override for one project
// and one task repo: a matching projects[] entry's own NON-EMPTY map replaces
// the top-level one (the same replace-not-merge shape ReposFor uses), and the
// map is then consulted by the FULL identity ("owner/name") first, then by the
// bare name — the common spelling, and the same fallback ResolveCheckout
// applies to checkouts (CLA-608).
//
// Empty means "nothing overridden": the caller resolves the branch against
// the origin remote instead. It does NOT mean "no branch" — a validated map
// entry always names one.
func (c *Config) PRBaseFor(slug, repo string) string {
	if strings.TrimSpace(repo) == "" {
		return ""
	}
	m := c.PRBase
	for _, p := range c.Projects {
		if p.Slug == slug && len(p.PRBase) > 0 {
			m = p.PRBase
			break
		}
	}
	if v, ok := m[repo]; ok {
		return v
	}
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		if v, ok := m[repo[i+1:]]; ok {
			return v
		}
	}
	return ""
}

// validateHealthURL holds a health_url to "an absolute http(s) URL with a
// host". It is deliberately weaker than backlog_url's TLS floor: /health is a
// public endpoint read without credentials, so there is no bearer token to
// keep off the wire, and a plain-http internal plane is a legitimate thing to
// point at. What it refuses is a value that could never be fetched at all - a
// relative reference, or a scheme no HTTP client speaks - which would
// otherwise surface as an opaque "unsupported protocol scheme" error from
// doctor instead of as the misfiled config line it is.
func validateHealthURL(raw, label string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: %q is not a URL: %w", label, raw, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("%s: %q is not an absolute URL with a scheme and host", label, raw)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("%s: %q must be an http or https URL; scheme %q can never be fetched", label, raw, u.Scheme)
	}
	return nil
}

// validateIntegrationBranch holds integration_branch to a bare ref-name shape.
// The value is passed to `git fetch` and `git ls-remote` by doctor's deploy_lag
// check (CLA-322); a value carrying refspec syntax (":") would make the fetch
// argument a src:dst spec and MUTATE LOCAL REFS during a preflight, a leading
// "-" reads as option syntax, and whitespace or anything outside a branch
// name's alphabet is rejected by git downstream anyway. Empty is legal: it
// means the default branch.
func validateIntegrationBranch(raw, label string) error {
	if raw == "" {
		return nil
	}
	if strings.HasPrefix(raw, "-") || strings.ContainsAny(raw, ": \t\r\n") {
		return fmt.Errorf("%s: %q must be a bare branch name (no leading \"-\", no \":\", no whitespace)", label, raw)
	}
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '/', r == '.', r == '_', r == '-':
		default:
			return fmt.Errorf("%s: %q must be a plain branch name ([A-Za-z0-9/._-])", label, raw)
		}
	}
	return nil
}

// validatePRBase holds every pr_base entry to a bare branch name, one level
// deeper than validateIntegrationBranch's own field: each VALUE reaches the
// phase briefs and (through them) `git`/`gh pr create` arguments, so it carries
// the same refspec-injection hazards a bad integration_branch does, plus the
// extra one of being substituted into prose. An empty value is refused rather
// than passed through: validateIntegrationBranch reads empty as "the default
// branch", but a map entry saying nothing is a half-written override whose
// silence would read as "resolved per repo" when the operator plainly meant to
// name a branch — omit the entry instead. Keys are held to non-blank, sorted so
// the reported entry does not change between runs on unchanged input.
func validatePRBase(m map[string]string, label string) error {
	for _, repo := range sortedKeys(m) {
		if strings.TrimSpace(repo) == "" {
			return fmt.Errorf("%s: a repo key is empty", label)
		}
		branch := m[repo]
		if strings.TrimSpace(branch) == "" {
			return fmt.Errorf("%s[%q]: names no branch (omit the entry to let the repo's PR base resolve against origin)", label, repo)
		}
		if err := validateIntegrationBranch(branch, fmt.Sprintf("%s[%q]", label, repo)); err != nil {
			return err
		}
	}
	return nil
}

// AllowLocalMCPServersFor resolves the CLA-266 discovered-file allowlist for
// one project: a matching projects[] entry's own NON-EMPTY list replaces the
// top-level one, and everything else — an unmatched slug (how a single-project
// run reaches here with no projects at all), or an entry that set none — falls
// back to the top-level list. There is deliberately no way to say "none here"
// while the top level allows names: an empty allowlist is indistinguishable
// from not having configured one, and pretending otherwise would let a project
// line look tighter than it runs. Total, so no caller has a second error path.
func (c *Config) AllowLocalMCPServersFor(slug string) []string {
	for _, p := range c.Projects {
		if p.Slug == slug && len(p.AllowLocalMCPServers) > 0 {
			return p.AllowLocalMCPServers
		}
	}
	return c.AllowLocalMCPServers
}
