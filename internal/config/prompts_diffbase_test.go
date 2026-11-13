package config

import (
	"regexp"
	"strings"
	"testing"
)

// testDiffBaseSHA and testDiffBaseTip are the values the scan substitutes for
// the merge-base and reviewed-tip placeholders: real 40-character SHAs, so a
// rendered brief reads the way a spawned one does.
const (
	testDiffBaseSHA = "0123456789abcdef0123456789abcdef01234567"
	testDiffBaseTip = "89abcdef0123456789abcdef0123456789abcdef"
)

// builtinBriefs is every shipped brief a phase can run, by name: the map's
// built-ins plus every variant the driver selects by checkpoint or position
// (the no-code and unverified halves, the panel members, the runner variants).
// A new brief added without a line here is the gap this test cannot see, so the
// variant lists themselves are pinned by their own tests elsewhere.
func builtinBriefs() map[string]string {
	briefs := map[string]string{}
	for name, brief := range builtinPhasePrompts {
		briefs[name] = brief
	}
	for name, brief := range map[string]string{
		"no-code review":          noCodeReviewBrief,
		"unverified review":       unverifiedReviewBrief,
		"review-only":             reviewOnlyBrief,
		"unverified review-only":  unverifiedReviewOnlyBrief,
		"panel member":            panelMemberReviewOnlyBrief,
		"unverified panel member": panelMemberUnverifiedReviewOnlyBrief,
		"no-code review-only":     noCodeReviewOnlyBrief,
		"fix":                     fixBrief,
		"fix-verify":              fixVerifyBrief,
		"no-code fix":             noCodeFixBrief,
		"no-code fix-verify":      noCodeFixVerifyBrief,
		"verify":                  verifyBrief,
		"no-code verify":          noCodeVerifyBrief,
		"runner implement":        runnerImplementBrief,
		"runner close-out":        runnerCloseOutBrief,
		"runner review-only":      runnerReviewOnlyBrief,
		"runner fix":              runnerFixBrief,
		"runner fix-verify":       runnerFixVerifyBrief,
		"runner verify":           runnerVerifyBrief,
	} {
		briefs[name] = brief
	}
	return briefs
}

// renderBriefForDiffScan substitutes every placeholder the driver fills, so the
// scan sees the text a session would. The sample PR base is a bare branch name:
// an unfixed brief that diffs from {{prBase}} renders "git diff staging..." and
// fails below.
func renderBriefForDiffScan(brief string) string {
	return strings.NewReplacer(
		PhaseTaskPlaceholder, "11111111-1111-1111-1111-111111111111",
		PhaseRunPlaceholder, "22222222-2222-2222-2222-222222222222.opaque",
		PhaseBranchPlaceholder, "clanker/11111111-a-title",
		PhasePRBasePlaceholder, "staging",
		PhaseBasePlaceholder, testDiffBaseSHA,
		PhaseHarnessPlaceholder, "opencode",
		PhaseModelPlaceholder, "some-model",
		PhasePrevHarnessPlaceholder, "opencode",
		PhasePrevModelPlaceholder, "some-model",
		PhaseNumberPlaceholder, "2",
		PhaseTotalPlaceholder, "3",
		ReviewerRolePlaceholder, "review-1",
		ReviewedTipPlaceholder, testDiffBaseTip,
	).Replace(brief)
}

// diffRefRe captures the revision argument of every `git diff` in a brief.
var diffRefRe = regexp.MustCompile(`git diff\s+([^\s)]+)`)

// diffBaseIsResolved accepts a diff base that cannot be a stale local branch:
// origin/<base> (git computes the merge-base itself), a commit SHA, or the
// documented <reviewedTip> symbol the fix-delta commands carry literally.
func diffBaseIsResolved(ref string) bool {
	if strings.HasPrefix(ref, "origin/") || strings.HasPrefix(ref, "<") {
		return true
	}
	if len(ref) < 7 || len(ref) > 40 {
		return false
	}
	for _, r := range ref {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

// CLA-865: every rendered brief that diffs must start from a resolved commit,
// never a bare local base branch. The local base can be far behind origin — the
// MAK-362 review's produced a first diff full of unrelated files — so a brief
// that says `git diff staging...branch` (or `integration`, or `main`) has the
// reviewer comparing against the wrong tree. The briefs carry {{baseSha}}; this
// test renders every one of them and fails on any `git diff` whose base is
// neither a SHA, origin/<base>, nor the fix-delta's <reviewedTip>.
func TestEveryBuiltinBriefDiffsFromAResolvedCommit(t *testing.T) {
	for name, brief := range builtinBriefs() {
		rendered := renderBriefForDiffScan(brief)
		for _, match := range diffRefRe.FindAllStringSubmatch(rendered, -1) {
			ref := match[1]
			base, _, _ := strings.Cut(ref, "..")
			if diffBaseIsResolved(base) {
				continue
			}
			t.Errorf("the %s brief diffs against the bare base %q, not a merge-base SHA or origin/<base>:\n%s", name, base, rendered)
		}
	}
}

// The review-shaped briefs that read a diff must carry the placeholder this
// test renders as a SHA, so the driver's substitution is what decides the base.
// The verify briefs are exempt by design: their fix-delta is already SHA-based
// (<reviewedTip>..HEAD) and has no base branch to resolve.
func TestReviewBriefsCarryTheResolvedDiffBase(t *testing.T) {
	for name, brief := range map[string]string{
		"review":                  builtinPhasePrompts[ReviewPhaseName],
		"review-only":             reviewOnlyBrief,
		"unverified review-only":  unverifiedReviewOnlyBrief,
		"panel member":            panelMemberReviewOnlyBrief,
		"unverified panel member": panelMemberUnverifiedReviewOnlyBrief,
		"runner review-only":      runnerReviewOnlyBrief,
	} {
		if !strings.Contains(brief, PhaseBasePlaceholder) {
			t.Errorf("the %s brief never carries %s, so its diff names whatever base the model happens to pick:\n%s", name, PhaseBasePlaceholder, brief)
		}
		if strings.Contains(brief, "git diff "+PhasePRBasePlaceholder) {
			t.Errorf("the %s brief diffs from %s, a bare base branch:\n%s", name, PhasePRBasePlaceholder, brief)
		}
	}
}
