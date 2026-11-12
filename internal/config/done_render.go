package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Attribution is who wrote what: the session that produced this report, and the
// one whose work it is about. The runner resolves it because a session cannot
// reliably name its own model and cannot see its predecessor's.
type Attribution struct {
	Harness, Model, PrevHarness, PrevModel string
}

// RenderReviewBlock renders the LEGACY `## Review` block a runner-claimed review
// session's report becomes. The driver parses this back - the fix phase reads
// findings by id and the seam gates on the heading - so the shape is an
// interface, not cosmetics: the severity bracket and the `(<kind>): <evidence>`
// prefix after it are load-bearing (see reviewFindingsDigest).
func RenderReviewBlock(a Attribution, r DoneReport) string {
	var b strings.Builder
	b.WriteString(ReviewRecordHeading + "\n")
	b.WriteString("- reviewed tip: " + r.ReviewedTip + "\n")
	b.WriteString("- implemented by: harness " + harnessOr(a.PrevHarness) + ", model " + modelOr(a.PrevModel) + "\n")
	b.WriteString("- reviewed by: harness " + harnessOr(a.Harness) + ", model " + modelOr(a.Model) + "\n")
	b.WriteString("- verdict: " + r.Verdict + "\n")
	b.WriteString("\n### Findings\n")
	if len(r.Findings) == 0 {
		b.WriteString("- none\n")
	}
	for i, f := range r.Findings {
		b.WriteString(renderFindingLine(i+1, f) + "\n")
	}
	if r.Omitted > 0 {
		fmt.Fprintf(&b, "(%d further finding(s) were left out of the durable record's 20-finding cap.)\n", r.Omitted)
	}
	if notes := strings.TrimSpace(r.Notes); notes != "" {
		b.WriteString("\n### Notes\n" + notes + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderFindingLine renders one finding in the shape the driver parses back: id,
// bracketed severity, then a MUST's evidence prefix, location, summary and fix.
func renderFindingLine(n int, f DoneFinding) string {
	line := fmt.Sprintf("- R%d [%s]", n, f.Severity)
	if f.Severity == "must" {
		if f.EvidenceKind != "" {
			line += " (" + f.EvidenceKind + "):"
			if f.Evidence != "" {
				line += " " + f.Evidence
			}
		} else if f.Evidence != "" {
			line += " " + f.Evidence
		}
	}
	if loc := findingLoc(f); loc != "" {
		line += " " + loc
	}
	if summary := strings.TrimSpace(f.Summary); summary != "" {
		line += " - " + summary + "."
	}
	if fix := strings.TrimSpace(f.RequiredFix); fix != "" {
		line += " Required fix: " + fix + "."
	}
	return line
}

// findingLoc is `file:line`, or `file` when the session gave no line, or "" when
// it named no file - the caller drops the whole location clause then.
func findingLoc(f DoneFinding) string {
	if f.File == "" {
		return ""
	}
	if f.Line > 0 {
		return f.File + ":" + strconv.Itoa(f.Line)
	}
	return f.File
}

// RenderFixesSection renders the fix phase's outcome section: the disposition of
// every finding, then the fix session's own evidence. The **Tests** section is
// always present because the plane refuses an in_review move whose outcome lacks
// one.
func RenderFixesSection(a Attribution, r DoneReport) string {
	var b strings.Builder
	b.WriteString("### Fixes\n")
	if len(r.FindingUpdates) == 0 {
		b.WriteString("- no findings to fix")
	} else {
		lines := make([]string, 0, len(r.FindingUpdates))
		for _, u := range r.FindingUpdates {
			lines = append(lines, fixUpdateLine(u))
		}
		b.WriteString(strings.Join(lines, "\n"))
	}

	parts := []string{b.String()}
	if summary := strings.TrimSpace(r.Summary); summary != "" {
		parts = append(parts, summary)
	}
	if notes := strings.TrimSpace(r.Notes); notes != "" {
		parts = append(parts, notes)
	}
	parts = append(parts, RenderTestsSection(r.Tests),
		"fixed by: harness "+harnessOr(a.Harness)+", model "+modelOr(a.Model))
	return strings.Join(parts, "\n\n")
}

// fixUpdateLine renders one findingUpdate. An unrecognised status is printed as
// given rather than guessed at; ValidateFindingUpdates refuses it before a
// record is written.
func fixUpdateLine(u DoneFindingUpdate) string {
	switch u.Status {
	case "fixed":
		return "- " + u.FindingID + ": fixed in " + u.FixCommit
	case "refuted":
		return "- " + u.FindingID + ": refuted - " + u.RefutedReason
	case "filed":
		return "- " + u.FindingID + ": filed as " + u.FiledTaskRef
	case "open":
		return "- " + u.FindingID + ": not fixed"
	default:
		return "- " + u.FindingID + ": " + u.Status
	}
}

// RenderDeliverySection renders the delivery record an implement session's
// report becomes: what it did, what it verified, and who did it.
func RenderDeliverySection(a Attribution, r DoneReport) string {
	parts := []string{"## Delivery"}
	if summary := strings.TrimSpace(r.Summary); summary != "" {
		parts = append(parts, summary)
	}
	parts = append(parts, RenderTestsSection(r.Tests),
		"implemented by: harness "+harnessOr(a.Harness)+", model "+modelOr(a.Model))
	return strings.Join(parts, "\n\n")
}

// TestsHeading is the heading the rendered sections write.
const TestsHeading = "**Tests**"

// testsSection mirrors the plane's in_review guard (TESTS_SECTION in the plane's
// repositories/tasks/delivery.ts): the header must open a line, and `__Tests__`
// and `## Tests` count too. RE2 has no lookahead, so the plane's
// `tests(?![a-z0-9])` is spelled as an optional non-alphanumeric tail.
var testsSection = regexp.MustCompile(`(?im)^[ \t]*(?:[-*+]|\d+[.)]|>|\|)?[ \t]*(?:\*\*[ \t]*tests\b[^*\n]*\*\*|__[ \t]*tests(?:[^a-z0-9_\n][^_\n]*)?__|#{1,6}[ \t]+tests\b)`)

// HasTestsSection reports whether the plane would accept outcome's **Tests**
// section, so the runner repairs exactly the outcomes it would refuse.
func HasTestsSection(outcome string) bool { return testsSection.MatchString(outcome) }

// RenderTestsSection is the **Tests** block every outcome section carries. The
// absence is stated rather than omitted: the plane requires the section, and a
// missing one is the whole call refused.
func RenderTestsSection(tests string) string {
	if strings.TrimSpace(tests) == "" {
		return TestsHeading + " — not reported by the session."
	}
	return TestsHeading + "\n" + strings.TrimSpace(tests)
}

// AppendOutcome appends section to an outcome as it stands. The plane's
// statusless outcome is a full replacement, so a fresh section written over it
// would lose the records already there. A section already present verbatim is
// left alone: a retried write must not stack the same block twice.
func AppendOutcome(existing, section string) string {
	if strings.TrimSpace(existing) == "" {
		return section
	}
	if strings.Contains(existing, section) {
		return existing
	}
	return strings.TrimRight(existing, "\n ") + "\n\n" + section
}

// harnessOr and modelOr render an attribution value for a record line. An empty
// model is normal (the harness default) and must not render as the bare word
// "model" followed by nothing, which reads like a truncated write.
func harnessOr(v string) string { return doneAttributionOr(v, "unknown harness") }

func modelOr(v string) string { return doneAttributionOr(v, "harness default") }

func doneAttributionOr(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
