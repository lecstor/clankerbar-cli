package config

import "strings"

// PRBaseProbe answers, for one integration branch, whether origin has
// refs/heads/<integration> and what the remote's symbolic HEAD points at. The
// caller closes over the repository and context; nil means no probe could run.
type PRBaseProbe func(integration string) (onOrigin bool, defaultBranch string, err error)

// PRBaseSource records which rule resolved a repo's PR merge target, so the
// phase driver and the deterministic close-out can log the same decision.
type PRBaseSource int

const (
	// PRBaseOverride is the operator's per-repo pr_base entry.
	PRBaseOverride PRBaseSource = iota
	// PRBaseIntegrationOnOrigin is the project's integration branch, present on origin.
	PRBaseIntegrationOnOrigin
	// PRBaseOriginDefault is origin's default branch, used when the integration
	// branch does not exist there.
	PRBaseOriginDefault
	// PRBaseIntegrationFallback is the integration branch, kept when no probe
	// could answer.
	PRBaseIntegrationFallback
)

// PRBaseResolution is ResolvePRBase's outcome.
type PRBaseResolution struct {
	Base        string
	Source      PRBaseSource
	Integration string // the project's integration branch, before any probe
	Default     string // origin's default branch, when the probe read it
	Err         error  // why the probe could not answer; Source is then the fallback
}

// ResolvePRBase resolves the branch one repo's PRs target: the operator's
// per-repo pr_base override, else the project's integration branch when origin
// has it, else origin's default branch, else the integration branch.
//
// It never fails. A probe that cannot run degrades to the integration branch
// rather than blocking the work that needs a merge target, and every caller gets
// the same answer for the same repo: a brief that names one branch and a
// close-out that judges the PR against another is the disagreement this prevents.
func ResolvePRBase(c *Config, slug, repo string, probe PRBaseProbe) PRBaseResolution {
	integration := strings.TrimSpace(c.IntegrationBranchFor(slug))
	if base := strings.TrimSpace(c.PRBaseFor(slug, repo)); base != "" {
		return PRBaseResolution{Base: base, Source: PRBaseOverride, Integration: integration}
	}
	if probe == nil {
		return PRBaseResolution{Base: integration, Source: PRBaseIntegrationFallback, Integration: integration}
	}
	onOrigin, def, err := probe(integration)
	switch {
	case err != nil:
		return PRBaseResolution{Base: integration, Source: PRBaseIntegrationFallback, Integration: integration, Err: err}
	case onOrigin:
		return PRBaseResolution{Base: integration, Source: PRBaseIntegrationOnOrigin, Integration: integration, Default: def}
	case def != "":
		return PRBaseResolution{Base: def, Source: PRBaseOriginDefault, Integration: integration, Default: def}
	default:
		return PRBaseResolution{Base: integration, Source: PRBaseIntegrationFallback, Integration: integration}
	}
}
