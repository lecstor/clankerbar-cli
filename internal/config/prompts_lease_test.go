package config

import (
	"strings"
	"testing"
)

// CLA-917: every shipped session-claimed phase brief runs under the loop
// driver's leaseRenewer (internal/loop/heartbeat.go), which renews the run's
// lease from Go every third of the 30-minute lease while the session runs. The
// served clankerbar protocol's "heartbeat every ~10 min" is written for a
// session that holds its own claim, so a brief that leaves it in force buys a
// redundant second heartbeat per session (measured on MAK-331, CLA-848 and the
// MAK-331 takeover). The literals below are the brief's own copy, not the
// const: an edit to the const that no one decided the wording of fails here.
func TestSessionClaimedBriefsCarryTheDriverLeaseRenewal(t *testing.T) {
	briefs := map[string]string{
		"implement":                      builtinPhasePrompts[ImplementPhaseName],
		"review (final)":                 builtinPhasePrompts[ReviewPhaseName],
		"review (no-code)":               noCodeReviewBrief,
		"review (unverified)":            unverifiedReviewBrief,
		"review-only":                    reviewOnlyBrief,
		"review-only (panel member)":     panelMemberReviewOnlyBrief,
		"review-only (panel unverified)": panelMemberUnverifiedReviewOnlyBrief,
		"review-only (unverified)":       unverifiedReviewOnlyBrief,
		"review-only (no-code)":          noCodeReviewOnlyBrief,
		"fix":                            fixBrief,
		"fix (verify follows)":           fixVerifyBrief,
		"fix (no-code)":                  noCodeFixBrief,
		"fix (no-code, verify follows)":  noCodeFixVerifyBrief,
		"verify":                         verifyBrief,
		"verify (no-code)":               noCodeVerifyBrief,
	}

	const renewal = "The driver renews the run's lease from Go every third of the lease while this session " +
		"runs, so do NOT call heartbeat for renewal."
	for name, brief := range briefs {
		if !strings.Contains(brief, renewal) {
			t.Errorf("the %s brief does not carry the driver-renewal sentence:\n%s", name, brief)
		}
	}
}

// The renewal sentence must not swallow the ONE heartbeat a resumed brief still
// needs: the heartbeat(runId) that resumes the run (or settles it, on a no-code
// checkout the task's own session already settled). Pin the pair, in order, so
// a later edit cannot keep the prohibition and drop the way back in.
func TestResumedSessionClaimedBriefsKeepTheResumeHeartbeat(t *testing.T) {
	briefs := map[string]string{
		"review (final)":                 builtinPhasePrompts[ReviewPhaseName],
		"review (no-code)":               noCodeReviewBrief,
		"review (unverified)":            unverifiedReviewBrief,
		"review-only":                    reviewOnlyBrief,
		"review-only (panel member)":     panelMemberReviewOnlyBrief,
		"review-only (panel unverified)": panelMemberUnverifiedReviewOnlyBrief,
		"review-only (unverified)":       unverifiedReviewOnlyBrief,
		"review-only (no-code)":          noCodeReviewOnlyBrief,
		"fix":                            fixBrief,
		"fix (verify follows)":           fixVerifyBrief,
		"fix (no-code)":                  noCodeFixBrief,
		"fix (no-code, verify follows)":  noCodeFixVerifyBrief,
		"verify":                         verifyBrief,
		"verify (no-code)":               noCodeVerifyBrief,
	}

	const (
		resume  = `heartbeat("` + PhaseRunPlaceholder + `")`
		check   = "That first heartbeat(runId) is the resume/settlement check and stays."
		renewal = "The driver renews the run's lease from Go every third of the lease while this session " +
			"runs, so do NOT call heartbeat for renewal."
	)
	for name, brief := range briefs {
		hb := strings.Index(brief, resume)
		if hb == -1 {
			t.Errorf("the %s brief no longer names the resume heartbeat %s:\n%s", name, resume, brief)
			continue
		}
		if !strings.Contains(brief, check) {
			t.Errorf("the %s brief does not keep the resume/settlement heartbeat:\n%s", name, brief)
		}
		if renew := strings.Index(brief, renewal); renew == -1 || renew < hb {
			t.Errorf("the %s brief's renewal sentence (at %d) does not follow its resume heartbeat (at %d):\n%s",
				name, renew, hb, brief)
		}
	}
}

// A handoff respawn's prompt is the preamble plus the predecessor's own text,
// so it never sees the phase brief that carries the renewal sentence: the
// preamble has to carry it too, or a handoff successor follows the served
// 10-minute cadence for the rest of the session.
func TestHandoffPreambleCarriesTheDriverLeaseRenewal(t *testing.T) {
	for _, want := range []string{
		`heartbeat("` + PhaseRunPlaceholder + `")`,
		"The driver renews the run's lease from Go every third of the lease while this session runs, so do NOT call heartbeat for renewal.",
		"That first heartbeat(runId) is the resume/settlement check and stays.",
	} {
		if !strings.Contains(HandoffPreamble, want) {
			t.Errorf("HandoffPreamble does not say %q:\n%s", want, HandoffPreamble)
		}
	}
}

// The supervisor's conflict-only session is the deliberate exception: it is
// launched by internal/supervisor/defaultConflictRun, outside the loop driver,
// so no leaseRenewer beats for it and the session really does hold its own
// claim. It keeps the 10-minute cadence, and must NOT get the driver sentence.
func TestConflictBriefKeepsItsOwnHeartbeatCadence(t *testing.T) {
	brief := ConflictBrief(ConflictBriefInput{
		TaskID:   "task-1",
		TaskRef:  "CLA-1",
		RunID:    "run-1",
		Branch:   "clanker/branch",
		Base:     "staging",
		PR:       "#12",
		Repo:     "owner/repo",
		WorkDir:  "/work",
		Worktree: "/work-wt",
	})
	if strings.Contains(brief, "renews the run's lease from Go") {
		t.Errorf("the conflict brief carries the driver-renewal sentence, but no driver-side renewer runs for that session:\n%s", brief)
	}
	if !strings.Contains(brief, "again every ~10 minutes") {
		t.Errorf("the conflict brief lost its own heartbeat cadence, and nothing else renews that claim:\n%s", brief)
	}
}
