package loop

import (
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// The runner renders a done report into the legacy `## Review` block, and the
// driver parses that block back out of the task's outcome to build the fix
// phase's list. The two must agree on the shape, or the fixer is held to a
// record nobody wrote.
func TestRenderedDoneReportSurvivesTheDriverParsers(t *testing.T) {
	rep := config.DoneReport{
		Verdict:     "findings",
		ReviewedTip: "0123456789abcdef0123456789abcdef01234567",
		Findings: []config.DoneFinding{
			{
				Severity:     "must",
				EvidenceKind: "untested",
				Evidence:     "internal/config/done_report.go:1 has no test",
				File:         "internal/config/done_report.go",
				Line:         1,
				Summary:      "the cap is unpinned",
				RequiredFix:  "pin the cap",
			},
			{
				Severity:    "should",
				File:        "internal/config/done_render.go",
				Line:        20,
				Summary:     "the note reads oddly",
				RequiredFix: "reword it",
			},
			{Severity: "nit", Summary: "a typo"},
		},
		Notes: "Reviewed the whole diff.",
	}
	existing := "## Delivery\n\nPushed the parser.\n\n**Tests**\n- go test ./internal/config/...: pass"
	att := config.Attribution{Harness: "opencode", Model: "model-x", PrevHarness: "claude", PrevModel: "prev-model"}
	block := config.RenderReviewBlock(att, rep)
	outcome := config.AppendOutcome(existing, block)

	if !reviewRecordLine(outcome) {
		t.Fatalf("the rendered block is not a review block to the driver:\n%s", outcome)
	}
	if got := lastReviewBlock(outcome); got != block {
		t.Errorf("lastReviewBlock() =\n%s\nwant the rendered block\n%s", got, block)
	}

	digest := reviewFindingsDigest(outcome)
	if digest == "" {
		t.Fatal("reviewFindingsDigest() found no findings in the rendered block")
	}
	for _, want := range []string{
		"- R1 [MUST]",
		"- R2 [SHOULD]",
		"- R3 [NIT]",
		"the cap is unpinned",
		"the note reads oddly",
		"a typo",
	} {
		if !strings.Contains(digest, want) {
			t.Errorf("the digest does not carry %q:\n%s", want, digest)
		}
	}
	// Every MUST the renderer wrote carries its kind and evidence, which is the
	// shape the driver's structural check reads: nothing here may be downgraded.
	if strings.Contains(digest, "downgraded") {
		t.Errorf("the digest downgraded a rendered finding:\n%s", digest)
	}
}
