package config

// CLA-870: the standalone review brief. It must be the review gate's
// substance without a task: the reading discipline and the MUST bar survive,
// the done-when input does not, and the done report is the one the command
// parses.

import (
	"strings"
	"testing"
)

func TestReviewRunBriefCarriesTheChangeAndNoTask(t *testing.T) {
	const base = "0123456789abcdef0123456789abcdef01234567"
	const head = "89abcdef0123456789abcdef0123456789abcdef"
	brief := ReviewRunBrief(ReviewRunBriefOptions{
		Base:        base,
		Head:        head,
		Title:       "Fix the parser",
		Description: "It drops the last token.",
	})

	for _, want := range []string{
		base,
		head,
		"git diff " + base + ".." + head,
		"Fix the parser",
		"It drops the last token.",
		"REVIEW ONLY",
		"NO claimed task",
		"NO done-when bar",
		DoneMarker,
		"reviewedTip",
		"verdict",
		"findings",
		"omitted",
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("the brief does not carry %q:\n%s", want, brief)
		}
	}
	if strings.Contains(brief, "{{") {
		t.Errorf("the brief carries a placeholders the command cannot substitute:\n%s", brief)
	}
	if strings.Contains(brief, "doneWhen") {
		t.Errorf("the brief still names the task doneWhen bar it told the session does not exist:\n%s", brief)
	}
	// The gate scope is the phase's, minus the sentences that name a phase
	// sequence this command does not have.
	for _, unwanted := range []string{"implement phase", "fix and verify phases", "panel shares this one worktree", "next member's"} {
		if strings.Contains(brief, unwanted) {
			t.Errorf("the brief carries phase-only gate-scope wording %q:\n%s", unwanted, brief)
		}
	}
	for _, want := range []string{"THE GATES ARE NOT YOURS TO RE-RUN", "TARGETED probe", "No earlier phase has vouched"} {
		if !strings.Contains(brief, want) {
			t.Errorf("the brief does not carry the gate scope's %q:\n%s", want, brief)
		}
	}
	if !strings.Contains(brief, "A MUST here cannot cite a task's done-when bar") {
		t.Errorf("the brief does not restate MUST for a taskless review:\n%s", brief)
	}
}

func TestReviewRunBriefTitleAndDescriptionAreOptional(t *testing.T) {
	brief := ReviewRunBrief(ReviewRunBriefOptions{Base: "aaaaaaa", Head: "bbbbbbb"})
	for _, unwanted := range []string{"### Title", "### Description"} {
		if strings.Contains(brief, unwanted) {
			t.Errorf("a brief with no title or description carries %q:\n%s", unwanted, brief)
		}
	}
	for _, want := range []string{"- Base: `aaaaaaa`", "- Head: `bbbbbbb`", DoneMarker} {
		if !strings.Contains(brief, want) {
			t.Errorf("the brief does not carry %q:\n%s", want, brief)
		}
	}
}

// The map method's findings turn ends by pointing at the review brief's report,
// so the brief must actually define that report in the same words ParseDone and
// ValidateReview read.
func TestReviewRunBriefReportIsThePhaseReportShape(t *testing.T) {
	brief := ReviewRunBrief(ReviewRunBriefOptions{Base: "aaaaaaa", Head: "bbbbbbb"})
	if !strings.Contains(brief, runnerReviewReportFields) {
		t.Errorf("the brief does not carry the review report's field list:\n%s", brief)
	}
	if !strings.Contains(reviewRunMUSTBarGrades, "untested - a new or changed guard") {
		t.Errorf("the standalone bar dropped the untested kind:\n%s", reviewRunMUSTBarGrades)
	}
}
