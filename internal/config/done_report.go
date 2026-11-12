package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// DoneReportMax caps the done report body. The runner writes the task's records
// from it, so an over-size body is refused rather than truncated: a truncated
// report is a confident write of half a review.
const DoneReportMax = 32 << 10

// DoneReport is the one JSON object a runner-claimed session emits after
// DoneMarker. The runner turns it into the task's records itself (record_progress
// outcome, record_review findings and findingUpdates, submit_for_review), so the
// fields here are an interface with the served briefs, not a convenience.
type DoneReport struct {
	Summary        string              `json:"summary,omitempty"`
	Tests          string              `json:"tests,omitempty"`
	Risk           *DoneRisk           `json:"risk,omitempty"`
	Park           string              `json:"park,omitempty"`
	Verdict        string              `json:"verdict,omitempty"`
	ReviewedTip    string              `json:"reviewedTip,omitempty"`
	Findings       []DoneFinding       `json:"findings,omitempty"`
	Omitted        int                 `json:"omitted,omitempty"`
	FindingUpdates []DoneFindingUpdate `json:"findingUpdates,omitempty"`
	Notes          string              `json:"notes,omitempty"`
}

// DoneRisk is the session's risk declaration for submit_for_review. A nil Risk
// is UNDECLARED and buys no per-batch auto-approval, which is why RiskDeclared
// exists: an empty touches list is a declaration, a missing risk is not.
type DoneRisk struct {
	Touches []string `json:"touches"`
}

// DoneFinding is one review finding. The plane caps a durable record at 20
// findings; the session keeps every MUST, the most important of the rest, and
// reports the remainder as Omitted.
type DoneFinding struct {
	Severity     string `json:"severity"`
	Summary      string `json:"summary"`
	EvidenceKind string `json:"evidenceKind,omitempty"`
	Evidence     string `json:"evidence,omitempty"`
	File         string `json:"file,omitempty"`
	Line         int    `json:"line,omitempty"`
	Kind         string `json:"kind,omitempty"`
	RequiredFix  string `json:"requiredFix,omitempty"`
}

// DoneFindingUpdate is one entry of the durable record's findingUpdates batch:
// the fix or verify phase's disposition of a finding, by plane-assigned id.
type DoneFindingUpdate struct {
	FindingID     string `json:"findingId"`
	Status        string `json:"status"`
	FixCommit     string `json:"fixCommit,omitempty"`
	RefutedReason string `json:"refutedReason,omitempty"`
	FiledTaskRef  string `json:"filedTaskRef,omitempty"`
}

// ParseDone finds and decodes the done report in a session's final message.
//
// The scan mirrors loop.parseHandoff deliberately (the two markers are read out
// of the same message shape): the LAST marker line wins because the block is
// defined as ending the message, the marker must be UNINDENTED, and it must not
// sit inside a ``` fence - both are the natural shapes of a session quoting the
// mechanism rather than emitting a report.
//
// found is separate from err so a marker with an unreadable body is logged as
// what it is (a report the runner could not use) rather than as "no marker".
// Unknown fields are ignored: the briefs and the runner may drift by a field
// without a session's whole report becoming unreadable.
func ParseDone(final string) (rep DoneReport, found bool, err error) {
	body, ok := doneBody(final)
	if !ok {
		return DoneReport{}, false, nil
	}
	body = stripDoneFence(stripTrailingModelArtifacts(body))
	if body == "" {
		return DoneReport{}, true, errors.New("the done marker line carries no done report")
	}
	if len(body) > DoneReportMax {
		return DoneReport{}, true, fmt.Errorf("the done report is %d bytes, over the %d-byte cap", len(body), DoneReportMax)
	}

	dec := json.NewDecoder(strings.NewReader(body))
	if err = dec.Decode(&rep); err != nil {
		return DoneReport{}, true, fmt.Errorf("the done report does not decode as one JSON object: %v", err)
	}
	// Anything but end-of-input after the object means the block is not exactly
	// the one value the marker promises.
	var trailing json.RawMessage
	if err = dec.Decode(&trailing); err == nil {
		return DoneReport{}, true, errors.New("the done report carries a second JSON value after the report object")
	} else if !errors.Is(err, io.EOF) {
		return DoneReport{}, true, fmt.Errorf("text follows the done report's JSON object: %v", err)
	}
	return rep, true, nil
}

// doneBody returns the trimmed text after the last marker line.
func doneBody(final string) (string, bool) {
	at := -1
	inFence := false
	lines := strings.Split(final, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if !inFence && strings.TrimRight(line, " \t\r") == DoneMarker {
			at = i
		}
	}
	if at < 0 {
		return "", false
	}
	return strings.TrimSpace(strings.Join(lines[at+1:], "\n")), true
}

// stripDoneFence unwraps a body the session fenced, which is a common shape for
// showing JSON. A fence with any other language tag is not a wrapper around
// this object and is left for the decoder to reject.
func stripDoneFence(body string) string {
	lines := strings.Split(body, "\n")
	if !strings.HasPrefix(strings.TrimSpace(lines[0]), "```") {
		return body
	}
	lang := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[0]), "```"))
	if lang != "" && !strings.EqualFold(lang, "json") {
		return body
	}
	// The object ends at the FIRST closing fence: a courtesy line after it is
	// prose about the report, not part of it.
	rest := lines[1:]
	for i, l := range rest {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			rest = rest[:i]
			break
		}
	}
	return strings.TrimSpace(strings.Join(rest, "\n"))
}

// ValidateReview holds a review-shaped done report to the record the runner
// writes: the verdict and its findings agree, the reviewed tip is a commit, no
// finding string is over the plane's bound, and a verify report's findingUpdates
// are shippable. tip is the checkpoint tip the runner verified, "" when it had
// none to verify against.
//
// The errors name the offending field or finding index because they go to the
// log, where "invalid report" is worth nothing to whoever reads it.
func (r DoneReport) ValidateReview(tip string) error {
	switch r.Verdict {
	case "pass":
		if n := len(r.Findings); n > 0 {
			return fmt.Errorf("verdict is \"pass\" but the report carries %d finding(s)", n)
		}
	case "findings":
		if len(r.Findings) == 0 {
			return errors.New("verdict is \"findings\" but the report carries no findings")
		}
	default:
		return fmt.Errorf("verdict %q is neither \"pass\" nor \"findings\"", r.Verdict)
	}
	if !validLowerHex(r.ReviewedTip, 7, 64) {
		return fmt.Errorf("reviewedTip %q is not 7-64 lowercase hex characters", r.ReviewedTip)
	}
	if tip != "" && !tipsAgree(tip, r.ReviewedTip) {
		return fmt.Errorf("reviewedTip %q does not match the verified tip %q", r.ReviewedTip, tip)
	}
	if n := len(r.Findings); n > 20 {
		return fmt.Errorf("the report carries %d findings, over the 20-finding cap of the durable record", n)
	}
	for i, f := range r.Findings {
		switch f.Severity {
		case "must", "should", "nit":
		default:
			return fmt.Errorf("findings[%d]: severity %q is not must, should or nit", i, f.Severity)
		}
		if strings.TrimSpace(f.Summary) == "" {
			return fmt.Errorf("findings[%d] (%s): summary is empty", i, f.Severity)
		}
		// The plane refuses an over-bound record whole, so the same refusal
		// here is the difference between a re-report and a lost review.
		if err := checkBounds(fmt.Sprintf("findings[%d]", i), []boundedField{
			{"summary", f.Summary, boundFindingSummary},
			{"evidence", f.Evidence, boundFindingEvidence},
			{"evidenceKind", f.EvidenceKind, boundFindingEvidenceKind},
			{"file", f.File, boundFindingFile},
			{"kind", f.Kind, boundFindingKind},
			{"requiredFix", f.RequiredFix, boundFindingRequiredFix},
		}); err != nil {
			return err
		}
	}
	return ValidateFindingUpdates(r.FindingUpdates)
}

// ValidateFindingUpdates holds the durable record's findingUpdates batch to the
// shape the plane accepts, bounds included. Each status requires the field that
// carries its evidence, and a batch is all-or-nothing, so a duplicate id is
// refused here rather than discovered by the plane's refusal.
func ValidateFindingUpdates(us []DoneFindingUpdate) error {
	seen := make(map[string]bool, len(us))
	for i, u := range us {
		if strings.TrimSpace(u.FindingID) == "" {
			return fmt.Errorf("findingUpdates[%d]: findingId is empty", i)
		}
		if seen[u.FindingID] {
			return fmt.Errorf("findingUpdates[%d]: duplicate findingId %q", i, u.FindingID)
		}
		seen[u.FindingID] = true
		// Bounds first, so an over-limit field is named as the plane names it
		// rather than as a shape violation.
		if err := checkBounds(fmt.Sprintf("findingUpdates[%d]", i), []boundedField{
			{"refutedReason", u.RefutedReason, boundFindingRefutedReason},
			{"fixCommit", u.FixCommit, boundFindingFixCommit},
			{"filedTaskRef", u.FiledTaskRef, boundFindingFiledTaskRef},
		}); err != nil {
			return err
		}
		switch u.Status {
		case "fixed":
			if !validLowerHex(u.FixCommit, 7, 64) {
				return fmt.Errorf("findingUpdates[%d] (%s): fixCommit %q is not 7-64 lowercase hex characters", i, u.FindingID, u.FixCommit)
			}
		case "refuted":
			if strings.TrimSpace(u.RefutedReason) == "" {
				return fmt.Errorf("findingUpdates[%d] (%s): refutedReason is empty", i, u.FindingID)
			}
		case "filed":
			if strings.TrimSpace(u.FiledTaskRef) == "" {
				return fmt.Errorf("findingUpdates[%d] (%s): filedTaskRef is empty", i, u.FindingID)
			}
		case "open":
		default:
			return fmt.Errorf("findingUpdates[%d] (%s): status %q is not fixed, refuted, filed or open", i, u.FindingID, u.Status)
		}
	}
	return nil
}

// RiskDeclared reports whether the report declares risk at all: touches: [] is a
// declaration, an absent risk object is UNDECLARED.
func (r DoneReport) RiskDeclared() bool { return r.Risk != nil }

// validLowerHex reports whether s is lowercase hex of a plausible commit-id
// length. Abbreviated ids are accepted because a session reads them from git.
func validLowerHex(s string, min, max int) bool {
	if len(s) < min || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// tipsAgree reports whether two commits name the same revision: either literally
// equal, or one an abbreviation of the other long enough to identify it.
func tipsAgree(a, b string) bool {
	if a == b {
		return true
	}
	if len(a) >= 7 && strings.HasPrefix(b, a) {
		return true
	}
	return len(b) >= 7 && strings.HasPrefix(a, b)
}
