package config

import (
	"strings"
	"testing"
)

// RunnerDoneReportFor is what a handoff successor of a runner-claimed phase is
// told, so each runner brief must map to exactly the report ITS phase gives and
// every other brief must map to nothing.
func TestRunnerDoneReportFor_NamesTheReportOfTheRunnerBrief(t *testing.T) {
	cases := []struct {
		name  string
		brief string
		// want is a field only that phase's report list carries.
		want string
	}{
		{name: "implement", brief: RunnerImplementBrief(), want: "park"},
		{name: "review-only", brief: RunnerReviewOnlyBrief(), want: "verdict"},
		{name: "fix (last)", brief: RunnerFixBrief(), want: "risk"},
		{name: "fix-verify", brief: RunnerFixVerifyBrief(), want: "findingUpdates"},
		{name: "verify", brief: RunnerVerifyBrief(), want: "verdict"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RunnerDoneReportFor(tc.brief)
			if got == "" {
				t.Fatalf("RunnerDoneReportFor(%s) = \"\"; a successful handoff of that phase would not know the report format", tc.name)
			}
			if !strings.Contains(got, DoneMarker) {
				t.Errorf("the note for %s does not name the marker %q: %s", tc.name, DoneMarker, got)
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("the note for %s does not name the field %q its phase reports: %s", tc.name, tc.want, got)
			}
		})
	}
}

func TestRunnerDoneReportFor_IsEmptyForEveryOtherBrief(t *testing.T) {
	// The close-out is a runner brief too, but it reports nothing: its session
	// only closes the task. A note demanding a report there would ask for answers
	// the phase has no business giving.
	briefs := []struct{ name, brief string }{
		{"close-out", RunnerCloseOutBrief()},
		{"implement", BuiltinImplementBrief()},
		{"review-only", ReviewOnlyBrief()},
		{"fix", FixBrief()},
		{"fix-verify", FixVerifyBrief()},
		{"verify", VerifyBrief()},
		{"empty", ""},
		{"custom", "review it yourself and say what you found"},
		{"custom mentioning a report", "end with a done report of your own shape"},
	}
	for _, tc := range briefs {
		if got := RunnerDoneReportFor(tc.brief); got != "" {
			t.Errorf("RunnerDoneReportFor(%s) = %q, want \"\"", tc.name, got)
		}
	}
}

// The fix phases differ by exactly one field: only the LAST fix hands the task
// over, so only it must be asked for a risk declaration.
func TestRunnerDoneReportFor_FixLastAsksForRiskAndFixVerifyDoesNot(t *testing.T) {
	last := RunnerDoneReportFor(RunnerFixBrief())
	verify := RunnerDoneReportFor(RunnerFixVerifyBrief())
	if !strings.Contains(last, "risk") {
		t.Errorf("the last fix's report note does not ask for the risk declaration the hand-over needs: %s", last)
	}
	if strings.Contains(verify, "risk") {
		t.Errorf("the fix-verify note asks for a risk declaration its phase never uses: %s", verify)
	}
	if !strings.Contains(verify, "findingUpdates") {
		t.Errorf("the fix-verify note does not name findingUpdates: %s", verify)
	}
}
