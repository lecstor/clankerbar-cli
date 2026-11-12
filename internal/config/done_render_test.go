package config

import (
	"strings"
	"testing"
)

const renderTip = "0123456789abcdef0123456789abcdef01234567"

func renderAttribution() Attribution {
	return Attribution{Harness: "opencode", Model: "model-x", PrevHarness: "claude", PrevModel: "prev-model"}
}

func TestRenderReviewBlock(t *testing.T) {
	t.Run("every severity, with and without a location", func(t *testing.T) {
		rep := DoneReport{
			Verdict:     "findings",
			ReviewedTip: renderTip,
			Findings: []DoneFinding{
				{
					Severity:     "must",
					EvidenceKind: "gate",
					Evidence:     "go test ./... fails",
					File:         "internal/a.go",
					Line:         12,
					Summary:      "the guard is not covered",
					RequiredFix:  "cover it",
				},
				{
					Severity: "should",
					File:     "internal/b.go",
					Summary:  "the name misleads",
				},
				{Severity: "nit", Summary: "a typo in the doc comment"},
			},
		}
		want := `## Review
- reviewed tip: ` + renderTip + `
- implemented by: harness claude, model prev-model
- reviewed by: harness opencode, model model-x
- verdict: findings

### Findings
- R1 [must] (gate): go test ./... fails internal/a.go:12 - the guard is not covered. Required fix: cover it.
- R2 [should] internal/b.go - the name misleads.
- R3 [nit] - a typo in the doc comment.`
		if got := RenderReviewBlock(renderAttribution(), rep); got != want {
			t.Errorf("RenderReviewBlock() =\n%s\nwant\n%s", got, want)
		}
	})

	t.Run("a pass carries - none, the omitted count and notes", func(t *testing.T) {
		rep := DoneReport{
			Verdict:     "pass",
			ReviewedTip: renderTip,
			Omitted:     2,
			Notes:       "I also read the docs.",
		}
		// An empty harness/model renders the driver's own fallbacks, not a bare
		// word: a brief that says "model " followed by nothing reads as a
		// truncated write.
		want := `## Review
- reviewed tip: ` + renderTip + `
- implemented by: harness unknown harness, model harness default
- reviewed by: harness opencode, model harness default
- verdict: pass

### Findings
- none
(2 further finding(s) were left out of the durable record's 20-finding cap.)

### Notes
I also read the docs.`
		if got := RenderReviewBlock(Attribution{Harness: "opencode"}, rep); got != want {
			t.Errorf("RenderReviewBlock() =\n%s\nwant\n%s", got, want)
		}
	})

	t.Run("a MUST with no evidence", func(t *testing.T) {
		// No `(<kind>):` prefix at all: that is what the driver's digest reads
		// as a downgraded finding.
		rep := DoneReport{
			Verdict:     "findings",
			ReviewedTip: renderTip,
			Findings:    []DoneFinding{{Severity: "must", Summary: "unproven"}},
		}
		want := `## Review
- reviewed tip: ` + renderTip + `
- implemented by: harness claude, model prev-model
- reviewed by: harness opencode, model model-x
- verdict: findings

### Findings
- R1 [must] - unproven.`
		if got := RenderReviewBlock(renderAttribution(), rep); got != want {
			t.Errorf("RenderReviewBlock() =\n%s\nwant\n%s", got, want)
		}
	})
}

func TestRenderFixesSection(t *testing.T) {
	t.Run("every disposition", func(t *testing.T) {
		rep := DoneReport{
			Summary: "Fixed the guard.",
			Notes:   "See the PR.",
			Tests:   "- go test ./...: pass",
			FindingUpdates: []DoneFindingUpdate{
				{FindingID: "1.review-1.1", Status: "fixed", FixCommit: "abcdef1"},
				{FindingID: "1.review-1.2", Status: "refuted", RefutedReason: "the path is unreachable"},
				{FindingID: "1.review-1.3", Status: "filed", FiledTaskRef: "CLA-900"},
				{FindingID: "1.review-1.4", Status: "open"},
			},
		}
		want := `### Fixes
- 1.review-1.1: fixed in abcdef1
- 1.review-1.2: refuted - the path is unreachable
- 1.review-1.3: filed as CLA-900
- 1.review-1.4: not fixed

Fixed the guard.

See the PR.

**Tests**
- go test ./...: pass

fixed by: harness opencode, model model-x`
		att := Attribution{Harness: "opencode", Model: "model-x"}
		if got := RenderFixesSection(att, rep); got != want {
			t.Errorf("RenderFixesSection() =\n%s\nwant\n%s", got, want)
		}
	})

	t.Run("no findings to fix and no tests reported", func(t *testing.T) {
		// The **Tests** section is present even when the session reported
		// nothing: the plane refuses an in_review move without it.
		want := `### Fixes
- no findings to fix

**Tests** — not reported by the session.

fixed by: harness unknown harness, model harness default`
		if got := RenderFixesSection(Attribution{}, DoneReport{}); got != want {
			t.Errorf("RenderFixesSection() =\n%s\nwant\n%s", got, want)
		}
	})
}

func TestRenderDeliverySection(t *testing.T) {
	t.Run("summary and tests", func(t *testing.T) {
		rep := DoneReport{Summary: "Covered the parser.", Tests: "- go build ./...: pass"}
		want := `## Delivery

Covered the parser.

**Tests**
- go build ./...: pass

implemented by: harness opencode, model model-x`
		att := Attribution{Harness: "opencode", Model: "model-x"}
		if got := RenderDeliverySection(att, rep); got != want {
			t.Errorf("RenderDeliverySection() =\n%s\nwant\n%s", got, want)
		}
	})

	t.Run("nothing reported", func(t *testing.T) {
		want := `## Delivery

**Tests** — not reported by the session.

implemented by: harness unknown harness, model harness default`
		if got := RenderDeliverySection(Attribution{}, DoneReport{}); got != want {
			t.Errorf("RenderDeliverySection() =\n%s\nwant\n%s", got, want)
		}
	})
}

func TestAppendOutcome(t *testing.T) {
	const section = "## Review\n- verdict: pass"
	cases := []struct {
		name     string
		existing string
		section  string
		want     string
	}{
		{name: "empty", existing: "", section: section, want: section},
		{name: "whitespace only", existing: " \n\t\n", section: section, want: section},
		{name: "appends below the existing record", existing: "Delivery record.", section: section, want: "Delivery record.\n\n" + section},
		{name: "trailing blanks are not doubled", existing: "Delivery record.\n\n\n", section: section, want: "Delivery record.\n\n" + section},
		{
			name:     "a retry leaves an outcome that already carries the section alone",
			existing: "Delivery record.\n\n" + section,
			section:  section,
			want:     "Delivery record.\n\n" + section,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AppendOutcome(tc.existing, tc.section); got != tc.want {
				t.Errorf("AppendOutcome(%q, ...) = %q, want %q", tc.existing, got, tc.want)
			}
		})
	}

	// Appending twice is the same as appending once: a retried write must not
	// stack the block.
	once := AppendOutcome("Delivery record.", section)
	twice := AppendOutcome(once, section)
	if twice != once {
		t.Errorf("a second append changed the outcome:\n%s\nwant\n%s", twice, once)
	}
	if n := strings.Count(twice, section); n != 1 {
		t.Errorf("the section appears %d times after two appends, want 1", n)
	}
}

func TestHasTestsSectionMatchesThePlaneRule(t *testing.T) {
	cases := map[string]bool{
		"**Tests**\ngo test ./...":                    true,
		"- **Tests:** ran it":                         true,
		"## Tests\nran it":                            true,
		"__Tests__ ran":                               true,
		"> **tests** quoted":                          true,
		"the **Tests** section is in the Fixes block": false,
		"__Testsuite__":                               false,
		"no section at all":                           false,
		RenderTestsSection(""):                        true,
	}
	for outcome, want := range cases {
		if got := HasTestsSection(outcome); got != want {
			t.Errorf("HasTestsSection(%q) = %v, want %v", outcome, got, want)
		}
	}
}
