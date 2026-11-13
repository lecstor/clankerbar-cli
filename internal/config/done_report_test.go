package config

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Parser

// parseCase is one ParseDone case: the message it scans, whether a marker is
// found at all, and the summary or the log-worthy error expected back.
type parseCase struct {
	name        string
	final       string
	wantFound   bool
	wantSummary string
	errHas      string
}

func checkParseCases(t *testing.T, cases []parseCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep, found, err := ParseDone(tc.final)
			if found != tc.wantFound {
				t.Fatalf("found = %v, want %v (final %q)", found, tc.wantFound, tc.final)
			}
			if tc.errHas != "" {
				if err == nil {
					t.Fatalf("ParseDone accepted %q; want an error naming %q", tc.final, tc.errHas)
				}
				if !strings.Contains(err.Error(), tc.errHas) {
					t.Errorf("error %q does not name %q", err, tc.errHas)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDone(%q) = %v", tc.final, err)
			}
			if rep.Summary != tc.wantSummary {
				t.Errorf("summary = %q, want %q", rep.Summary, tc.wantSummary)
			}
		})
	}
}

func TestParseDoneFindsTheMarker(t *testing.T) {
	checkParseCases(t, []parseCase{
		{
			name:        "no marker",
			final:       "All done, the task is on its branch.",
			wantFound:   false,
			wantSummary: "",
			errHas:      "",
		},
		{
			name:        "marker line followed by the object",
			final:       "Pushed.\n" + DoneMarker + "\n" + `{"summary":"Wired the parser"}`,
			wantFound:   true,
			wantSummary: "Wired the parser",
			errHas:      "",
		},
		{
			name:        "trailing whitespace on the marker line is ignored",
			final:       DoneMarker + "  \r\n" + `{"summary":"ok"}`,
			wantFound:   true,
			wantSummary: "ok",
			errHas:      "",
		},
		{
			// An indented marker is a quotation inside a list item, not an
			// emission.
			name:        "an indented marker line does not match",
			final:       "  " + DoneMarker + "\n" + `{"summary":"x"}`,
			wantFound:   false,
			wantSummary: "",
			errHas:      "",
		},
		{
			// The other natural way a session quotes the marker while explaining
			// the mechanism: unindented, so indentation alone would not catch it.
			name:        "a marker line inside a fence does not match",
			final:       "The shape is:\n```\n" + DoneMarker + "\n" + `{"summary":"x"}` + "\n```\nI did not emit one.",
			wantFound:   false,
			wantSummary: "",
			errHas:      "",
		},
		{
			// The block ends the message, so a quoted marker above the real one
			// must not win.
			name:        "the last marker wins",
			final:       DoneMarker + "\n" + `{"summary":"stale"}` + "\n" + DoneMarker + "\n" + `{"summary":"real"}`,
			wantFound:   true,
			wantSummary: "real",
			errHas:      "",
		},
		{
			name:        "a json fence is stripped",
			final:       DoneMarker + "\n```json\n" + `{"summary":"fenced"}` + "\n```",
			wantFound:   true,
			wantSummary: "fenced",
			errHas:      "",
		},
		{
			name:        "a courtesy line after a fenced report is ignored",
			final:       DoneMarker + "\n```json\n" + `{"summary":"fenced"}` + "\n```\nThat is everything.",
			wantFound:   true,
			wantSummary: "fenced",
			errHas:      "",
		},
		{
			name:        "a bare fence is stripped",
			final:       DoneMarker + "\n```\n" + `{"summary":"bare"}` + "\n```\n",
			wantFound:   true,
			wantSummary: "bare",
			errHas:      "",
		},
		{
			name:        "an inline mention is not a marker line",
			final:       "end your final message with " + DoneMarker + " and then one JSON object",
			wantFound:   false,
			wantSummary: "",
			errHas:      "",
		},
		{
			// The handoff marker's own text must not be read as this marker.
			name:        "the handoff marker is not the done marker",
			final:       HandoffMarker + "\nResume: implement the parser.",
			wantFound:   false,
			wantSummary: "",
			errHas:      "",
		},
		{
			name:        "unknown fields are ignored",
			final:       DoneMarker + "\n" + `{"summary":"known","somethingNew":{"a":1}}`,
			wantFound:   true,
			wantSummary: "known",
			errHas:      "",
		},
		{
			name:        "a fenced CRLF body",
			final:       DoneMarker + "\r\n```json\r\n" + `{"summary":"s"}` + "\r\n```\r\n",
			wantFound:   true,
			wantSummary: "s",
			errHas:      "",
		},
	})
}

func TestParseDoneRefusesABadBody(t *testing.T) {
	checkParseCases(t, []parseCase{
		{
			name:        "trailing text after the object",
			final:       DoneMarker + "\n" + `{"summary":"x"}` + " and then a note",
			wantFound:   true,
			wantSummary: "",
			errHas:      "text follows",
		},
		{
			name:        "a second JSON value",
			final:       DoneMarker + "\n" + `{"summary":"a"}{"summary":"b"}`,
			wantFound:   true,
			wantSummary: "",
			errHas:      "second JSON value",
		},
		{
			name:        "an empty body",
			final:       "Pushed.\n" + DoneMarker,
			wantFound:   true,
			wantSummary: "",
			errHas:      "carries no done report",
		},
		{
			name:        "an empty fence",
			final:       DoneMarker + "\n```json\n```",
			wantFound:   true,
			wantSummary: "",
			errHas:      "carries no done report",
		},
		{
			name:        "a body that is not JSON",
			final:       DoneMarker + "\nnot json at all",
			wantFound:   true,
			wantSummary: "",
			errHas:      "does not decode",
		},
		{
			name:        "a JSON array",
			final:       DoneMarker + "\n[1,2,3]",
			wantFound:   true,
			wantSummary: "",
			errHas:      "does not decode",
		},
		{
			// A fence tagged with another language is not a wrapper around this
			// object; it is left for the decoder to refuse.
			name:        "a non-JSON fence is not stripped",
			final:       DoneMarker + "\n```text\n" + `{"summary":"x"}` + "\n```",
			wantFound:   true,
			wantSummary: "",
			errHas:      "does not decode",
		},
	})
}

func TestParseDoneDecodesTheWholeReport(t *testing.T) {
	final := DoneMarker + "\n" +
		`{"summary":"s","tests":"- ok","risk":{"touches":["secrets"]},"park":"why",` +
		`"verdict":"pass","reviewedTip":"` + validTip() + `","omitted":2,"notes":"n"}`
	rep, found, err := ParseDone(final)
	if !found || err != nil {
		t.Fatalf("found = %v, err = %v", found, err)
	}
	if rep.Summary != "s" || rep.Tests != "- ok" || rep.Park != "why" {
		t.Errorf("summary/tests/park = %q/%q/%q", rep.Summary, rep.Tests, rep.Park)
	}
	if rep.Verdict != "pass" || rep.ReviewedTip != validTip() || rep.Omitted != 2 || rep.Notes != "n" {
		t.Errorf("verdict/tip/omitted/notes = %q/%q/%d/%q", rep.Verdict, rep.ReviewedTip, rep.Omitted, rep.Notes)
	}
	if !rep.RiskDeclared() || len(rep.Risk.Touches) != 1 || rep.Risk.Touches[0] != "secrets" {
		t.Errorf("risk = %+v, want one secrets touch", rep.Risk)
	}
}

// A session reporting "nothing left out" reaches for [] or null as readily as 0.
// A list of anything is not a count, so it is refused rather than guessed at.
func TestParseDoneOmittedShapes(t *testing.T) {
	body := func(omitted string) string {
		return DoneMarker + "\n" +
			`{"verdict":"pass","reviewedTip":"` + validTip() + `","findings":[],"omitted":` + omitted +
			`,"findingUpdates":[],"notes":"","tests":"- go build ./...: pass"}`
	}

	for _, tc := range []struct {
		name    string
		omitted string
		want    DoneCount
	}{
		{name: "zero", omitted: "0", want: 0},
		{name: "a count", omitted: "3", want: 3},
		{name: "an empty array", omitted: "[]", want: 0},
		{name: "an empty array with whitespace", omitted: "[ ]", want: 0},
		{name: "null", omitted: "null", want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep, found, err := ParseDone(body(tc.omitted))
			if !found || err != nil {
				t.Fatalf("found = %v, err = %v", found, err)
			}
			if rep.Omitted != tc.want {
				t.Errorf("omitted = %d, want %d", rep.Omitted, tc.want)
			}
		})
	}

	for _, tc := range []struct {
		name    string
		omitted string
		errHas  string
	}{
		{name: "a list of one", omitted: `["a"]`, errHas: "not a list"},
		{name: "a negative count", omitted: "-1", errHas: "must not be negative"},
		{name: "a quoted count", omitted: `"2"`, errHas: "does not decode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, found, err := ParseDone(body(tc.omitted))
			if !found {
				t.Fatal("found = false, want true: the marker line is present")
			}
			if err == nil {
				t.Fatalf("ParseDone accepted omitted %s; want an error", tc.omitted)
			}
			if !strings.Contains(err.Error(), tc.errHas) {
				t.Errorf("error %q does not name %q", err, tc.errHas)
			}
		})
	}
}

func TestParseDoneRefusesAnOversizeBody(t *testing.T) {
	const wrapper = `{"notes":""}`
	atCap := `{"notes":"` + strings.Repeat("x", DoneReportMax-len(wrapper)) + `"}`
	if len(atCap) != DoneReportMax {
		t.Fatalf("test body is %d bytes, not the %d-byte cap", len(atCap), DoneReportMax)
	}

	rep, found, err := ParseDone(DoneMarker + "\n" + atCap)
	if !found || err != nil {
		t.Fatalf("found = %v, err = %v; a body exactly at the cap must be accepted", found, err)
	}
	if want := DoneReportMax - len(wrapper); len(rep.Notes) != want {
		t.Errorf("notes = %d bytes, want %d", len(rep.Notes), want)
	}

	over := atCap[:len(atCap)-2] + `x"}`
	if _, found, err := ParseDone(DoneMarker + "\n" + over); !found || err == nil {
		t.Fatalf("found = %v, err = %v; a body over the cap must be refused", found, err)
	} else if !strings.Contains(err.Error(), "over the") {
		t.Errorf("error %q does not name the cap", err)
	}
}

// ---------------------------------------------------------------------------
// Validators

func validTip() string { return "0123456789abcdef0123456789abcdef01234567" }

func TestDoneReportValidateReview(t *testing.T) {
	must := DoneFinding{Severity: "must", Summary: "the guard is untested", EvidenceKind: "untested", Evidence: "internal/x.go:10"}
	should := DoneFinding{Severity: "should", Summary: "the name misleads"}

	pass := DoneReport{Verdict: "pass", ReviewedTip: validTip()}
	passFindings := DoneReport{Verdict: "pass", ReviewedTip: validTip(), Findings: []DoneFinding{must, should}}
	good := DoneReport{Verdict: "findings", ReviewedTip: validTip(), Findings: []DoneFinding{must, should}, FindingUpdates: []DoneFindingUpdate{{FindingID: "1.review-1.1", Status: "open"}}}
	noFindings := DoneReport{Verdict: "findings", ReviewedTip: validTip()}
	noVerdict := DoneReport{ReviewedTip: validTip()}
	badVerdict := DoneReport{Verdict: "approve", ReviewedTip: validTip()}
	upperTip := DoneReport{Verdict: "pass", ReviewedTip: "0123456789ABCDEF0123456789abcdef01234567"}
	shortTip := DoneReport{Verdict: "pass", ReviewedTip: "012345"}
	longTip := DoneReport{Verdict: "pass", ReviewedTip: strings.Repeat("a", 65)}
	nonHexTip := DoneReport{Verdict: "pass", ReviewedTip: "0123456789abcdeg"}
	unknownSeverity := DoneReport{Verdict: "findings", ReviewedTip: validTip(), Findings: []DoneFinding{{Severity: "blocker", Summary: "x"}}}
	emptySummary := DoneReport{Verdict: "findings", ReviewedTip: validTip(), Findings: []DoneFinding{{Severity: "should", Summary: "  "}}}
	badUpdates := DoneReport{Verdict: "pass", ReviewedTip: validTip(), FindingUpdates: []DoneFindingUpdate{{FindingID: "1.review-1.1", Status: "fixed"}}}

	cases := []struct {
		name   string
		rep    DoneReport
		tip    string
		errHas string
	}{
		{name: "pass with no findings", rep: pass, tip: "", errHas: ""},
		{name: "findings with findings", rep: good, tip: "", errHas: ""},
		{name: "findings agreeing with the verified tip", rep: good, tip: validTip(), errHas: ""},
		{name: "the report's tip abbreviates the verified one", rep: good, tip: validTip() + "abc", errHas: ""},
		{name: "the verified tip is an abbreviation", rep: good, tip: validTip()[:12], errHas: ""},
		{name: "pass carrying findings", rep: passFindings, tip: "", errHas: "carries 2 finding(s)"},
		{name: "findings with none", rep: noFindings, tip: "", errHas: "carries no findings"},
		{name: "a missing verdict", rep: noVerdict, tip: "", errHas: "verdict"},
		{name: "an unknown verdict", rep: badVerdict, tip: "", errHas: "verdict"},
		{name: "an uppercase tip", rep: upperTip, tip: "", errHas: "reviewedTip"},
		{name: "a tip too short to identify a commit", rep: shortTip, tip: "", errHas: "reviewedTip"},
		{name: "a tip longer than a full commit id", rep: longTip, tip: "", errHas: "reviewedTip"},
		{name: "a non-hex tip", rep: nonHexTip, tip: "", errHas: "reviewedTip"},
		{name: "a tip that does not match the verified one", rep: good, tip: strings.Repeat("f", 40), errHas: "does not match"},
		{name: "a verified tip too short to abbreviate", rep: good, tip: "01234", errHas: "does not match"},
		{name: "a finding with an unknown severity", rep: unknownSeverity, tip: "", errHas: "findings[0]"},
		{name: "a finding with an empty summary", rep: emptySummary, tip: "", errHas: "findings[0]"},
		{name: "a bad findingUpdate is reported through", rep: badUpdates, tip: "", errHas: "fixCommit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.rep.ValidateReview(tc.tip)
			if tc.errHas == "" {
				if err != nil {
					t.Fatalf("ValidateReview() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateReview() accepted the report; want an error naming %q", tc.errHas)
			}
			if !strings.Contains(err.Error(), tc.errHas) {
				t.Errorf("error %q does not name %q", err, tc.errHas)
			}
		})
	}
}

func TestDoneReportValidateReviewCapsFindings(t *testing.T) {
	rep := DoneReport{Verdict: "findings", ReviewedTip: validTip()}
	for i := 0; i < 20; i++ {
		rep.Findings = append(rep.Findings, DoneFinding{Severity: "nit", Summary: "small"})
	}
	if err := rep.ValidateReview(""); err != nil {
		t.Fatalf("ValidateReview() refused 20 findings: %v", err)
	}
	rep.Findings = append(rep.Findings, DoneFinding{Severity: "nit", Summary: "one too many"})
	err := rep.ValidateReview("")
	if err == nil {
		t.Fatal("ValidateReview() accepted 21 findings")
	}
	if !strings.Contains(err.Error(), "20-finding cap") {
		t.Errorf("error %q does not name the cap", err)
	}
}

func TestValidateFindingUpdates(t *testing.T) {
	dup := []DoneFindingUpdate{
		{FindingID: "1.review-1.1", Status: "open"},
		{FindingID: "1.review-1.1", Status: "fixed", FixCommit: "abcdef1"},
	}
	cases := []struct {
		name   string
		upds   []DoneFindingUpdate
		errHas string
	}{
		{name: "none", upds: nil, errHas: ""},
		{name: "fixed", upds: []DoneFindingUpdate{{FindingID: "1.review-1.1", Status: "fixed", FixCommit: "abcdef1"}}, errHas: ""},
		{name: "refuted", upds: []DoneFindingUpdate{{FindingID: "1.review-1.1", Status: "refuted", RefutedReason: "the path is unreachable"}}, errHas: ""},
		{name: "filed", upds: []DoneFindingUpdate{{FindingID: "1.review-1.1", Status: "filed", FiledTaskRef: "CLA-900"}}, errHas: ""},
		{name: "open", upds: []DoneFindingUpdate{{FindingID: "1.review-1.1", Status: "open"}}, errHas: ""},
		{name: "fixed without a commit", upds: []DoneFindingUpdate{{FindingID: "1.review-1.1", Status: "fixed"}}, errHas: "fixCommit"},
		{name: "fixed with an uppercase commit", upds: []DoneFindingUpdate{{FindingID: "1.review-1.1", Status: "fixed", FixCommit: "ABCDEF1"}}, errHas: "fixCommit"},
		{name: "refuted without evidence", upds: []DoneFindingUpdate{{FindingID: "1.review-1.1", Status: "refuted"}}, errHas: "refutedReason"},
		{name: "filed without a ref", upds: []DoneFindingUpdate{{FindingID: "1.review-1.1", Status: "filed"}}, errHas: "filedTaskRef"},
		{name: "a missing findingId", upds: []DoneFindingUpdate{{FindingID: " ", Status: "open"}}, errHas: "findingUpdates[0]"},
		{name: "an unknown status", upds: []DoneFindingUpdate{{FindingID: "1.review-1.1", Status: "wontfix"}}, errHas: "findingUpdates[0]"},
		{name: "a duplicate findingId", upds: dup, errHas: "findingUpdates[1]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateFindingUpdates(tc.upds)
			if tc.errHas == "" {
				if err != nil {
					t.Fatalf("ValidateFindingUpdates() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateFindingUpdates() accepted the batch; want an error naming %q", tc.errHas)
			}
			if !strings.Contains(err.Error(), tc.errHas) {
				t.Errorf("error %q does not name %q", err, tc.errHas)
			}
		})
	}
}

func TestDoneReportRiskDeclared(t *testing.T) {
	// An empty touches list is a declaration; an absent risk object is not.
	for _, tc := range []struct {
		name string
		rep  DoneReport
		want bool
	}{
		{"declared with no touches", DoneReport{Risk: &DoneRisk{Touches: []string{}}}, true},
		{"declared with touches", DoneReport{Risk: &DoneRisk{Touches: []string{"secrets"}}}, true},
		{"undeclared", DoneReport{}, false},
	} {
		if got := tc.rep.RiskDeclared(); got != tc.want {
			t.Errorf("%s: RiskDeclared = %v, want %v", tc.name, got, tc.want)
		}
	}
}
