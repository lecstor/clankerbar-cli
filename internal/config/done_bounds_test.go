package config

import (
	"strings"
	"testing"
)

// A bound check that counted bytes or runes would disagree with the plane, which
// measures JavaScript string.length.
func TestUTF16Len(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		want     int
	}{
		{"empty", "", 0},
		{"ascii", "abc", 3},
		{"two-byte UTF-8 char", "é", 1},
		{"non-BMP char", "😀", 2},
		{"mixed", "a😀é", 4},
	} {
		if got := utf16Len(tc.in); got != tc.want {
			t.Errorf("utf16Len(%s) = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestValidateReviewBounds(t *testing.T) {
	// The finding is otherwise valid, so only a bound can make the report bad.
	report := func(f DoneFinding) DoneReport {
		return DoneReport{Verdict: "findings", ReviewedTip: validTip(), Findings: []DoneFinding{f}}
	}
	cases := []struct {
		name   string
		find   DoneFinding
		errHas string
	}{
		{
			name: "every field at its bound",
			find: DoneFinding{
				Severity:     "must",
				Summary:      strings.Repeat("s", boundFindingSummary),
				Evidence:     strings.Repeat("e", boundFindingEvidence),
				EvidenceKind: strings.Repeat("k", boundFindingEvidenceKind),
				File:         strings.Repeat("f", boundFindingFile),
				Kind:         strings.Repeat("d", boundFindingKind),
				RequiredFix:  strings.Repeat("r", boundFindingRequiredFix),
			},
		},
		{
			name:   "summary one over",
			find:   DoneFinding{Severity: "must", Summary: strings.Repeat("s", boundFindingSummary+1)},
			errHas: "findings[0].summary is 1001 chars; the bound is 1000",
		},
		{
			name:   "evidence one over",
			find:   DoneFinding{Severity: "must", Summary: "small", Evidence: strings.Repeat("e", boundFindingEvidence+1)},
			errHas: "findings[0].evidence is 2001 chars; the bound is 2000",
		},
		{
			name:   "evidenceKind one over",
			find:   DoneFinding{Severity: "must", Summary: "small", EvidenceKind: strings.Repeat("k", boundFindingEvidenceKind+1)},
			errHas: "findings[0].evidenceKind is 41 chars; the bound is 40",
		},
		{
			name:   "file one over",
			find:   DoneFinding{Severity: "must", Summary: "small", File: strings.Repeat("f", boundFindingFile+1)},
			errHas: "findings[0].file is 301 chars; the bound is 300",
		},
		{
			name:   "kind one over",
			find:   DoneFinding{Severity: "must", Summary: "small", Kind: strings.Repeat("d", boundFindingKind+1)},
			errHas: "findings[0].kind is 81 chars; the bound is 80",
		},
		{
			name:   "requiredFix one over",
			find:   DoneFinding{Severity: "must", Summary: "small", RequiredFix: strings.Repeat("r", boundFindingRequiredFix+1)},
			errHas: "findings[0].requiredFix is 1001 chars; the bound is 1000",
		},
		{
			name: "the first over-bound field is the one reported",
			find: DoneFinding{
				Severity: "must",
				Summary:  strings.Repeat("s", boundFindingSummary+1),
				Evidence: strings.Repeat("e", boundFindingEvidence+1),
			},
			errHas: "findings[0].summary is 1001 chars; the bound is 1000",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := report(tc.find).ValidateReview("")
			if tc.errHas == "" {
				if err != nil {
					t.Fatalf("ValidateReview() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateReview() accepted the report; want %q", tc.errHas)
			}
			if err.Error() != tc.errHas {
				t.Errorf("error = %q, want %q", err, tc.errHas)
			}
		})
	}
}

// The bound is UTF-16 code units, not runes or bytes: the plane counts a
// non-BMP rune as two.
func TestValidateReviewBoundsCountUTF16Units(t *testing.T) {
	report := func(summary string) DoneReport {
		return DoneReport{Verdict: "findings", ReviewedTip: validTip(),
			Findings: []DoneFinding{{Severity: "must", Summary: summary}}}
	}
	// 600 emoji are 600 runes but 1200 UTF-16 units.
	err := report(strings.Repeat("😀", 600)).ValidateReview("")
	if err == nil {
		t.Fatal("ValidateReview() accepted 600 emoji; the plane counts them as 1200 chars")
	}
	if want := "findings[0].summary is 1200 chars; the bound is 1000"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
	// 1000 é is 2000 UTF-8 bytes but 1000 UTF-16 units, inside the bound.
	if err := report(strings.Repeat("é", 1000)).ValidateReview(""); err != nil {
		t.Errorf("ValidateReview() refused a 1000-unit summary: %v", err)
	}
}

func TestValidateFindingUpdatesBounds(t *testing.T) {
	cases := []struct {
		name   string
		upd    DoneFindingUpdate
		errHas string
	}{
		{
			name: "refutedReason at the bound",
			upd: DoneFindingUpdate{FindingID: "1.review-1.1", Status: "refuted",
				RefutedReason: strings.Repeat("r", boundFindingRefutedReason)},
		},
		{
			name: "refutedReason one over",
			upd: DoneFindingUpdate{FindingID: "1.review-1.1", Status: "refuted",
				RefutedReason: strings.Repeat("r", boundFindingRefutedReason+1)},
			errHas: "findingUpdates[0].refutedReason is 1001 chars; the bound is 1000",
		},
		{
			// The plane's bound is 100; the runner's own commit-id rule is
			// stricter (64), so only the over-bound message is pinned here.
			name: "fixCommit one over",
			upd: DoneFindingUpdate{FindingID: "1.review-1.1", Status: "fixed",
				FixCommit: strings.Repeat("a", boundFindingFixCommit+1)},
			errHas: "findingUpdates[0].fixCommit is 101 chars; the bound is 100",
		},
		{
			name: "filedTaskRef one over",
			upd: DoneFindingUpdate{FindingID: "1.review-1.1", Status: "filed",
				FiledTaskRef: strings.Repeat("t", boundFindingFiledTaskRef+1)},
			errHas: "findingUpdates[0].filedTaskRef is 51 chars; the bound is 50",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateFindingUpdates([]DoneFindingUpdate{tc.upd})
			if tc.errHas == "" {
				if err != nil {
					t.Fatalf("ValidateFindingUpdates() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateFindingUpdates() accepted the update; want %q", tc.errHas)
			}
			if err.Error() != tc.errHas {
				t.Errorf("error = %q, want %q", err, tc.errHas)
			}
		})
	}
}
