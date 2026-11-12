package loop

// done_check_test.go — the adapter-side check of a runner-claimed session's done
// report. It must apply the same PURE validation the matching apply* function
// applies, so a session steered by the check and the record the runner then
// writes cannot disagree.

import (
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

func TestReportCheck_NoReportOwed(t *testing.T) {
	r := doneRun(t, reportNone, nil, resumed())
	if check := r.reportCheck(); check != nil {
		t.Fatal("a phase that owes no report was given a check; the adapter would resume a session over nothing")
	}
}

func TestReportCheck_Implement(t *testing.T) {
	r := doneRun(t, reportImplement, nil, nil)
	check := r.reportCheck()
	if check == nil {
		t.Fatal("a runner-briefed implement phase owes a report, got no check")
	}
	if err := check("I pushed it and stopped."); err == nil || !strings.Contains(err.Error(), config.DoneMarker) {
		t.Fatalf("missing report: err = %v, want it to name the %s marker", err, config.DoneMarker)
	}
	if err := check(doneReportFor("not json at all")); err == nil || !strings.Contains(err.Error(), "could not be parsed") {
		t.Fatalf("unparseable report: err = %v, want the parse failure named", err)
	}
	if err := check(doneReportFor(`{"summary":"wired it","tests":"go test ./..."}`)); err != nil {
		t.Fatalf("a valid implement report was refused: %v", err)
	}
}

// A handoff defers the report, and the adapter's resume would overwrite the final
// message that carries it; a verify round's handoff is dropped, so it still owes one.
func TestReportCheck_HandoffOwesNoReportExceptOnVerify(t *testing.T) {
	handoff := "Out of context.\n\n" + config.HandoffMarker + "\nContinue the implementation from the last commit."
	if err := doneRun(t, reportImplement, nil, nil).reportCheck()(handoff); err != nil {
		t.Fatalf("an implement session that handed off was refused (a resume would clobber the handoff): %v", err)
	}
	if err := doneRun(t, reportVerify, nil, resumed()).reportCheck()(handoff); err == nil {
		t.Fatal("a verify round's handoff passed the check, but verify rounds never chain and still owe the report")
	}
	empty := "Done.\n\n" + config.HandoffMarker + "\n"
	if err := doneRun(t, reportImplement, nil, nil).reportCheck()(empty); err == nil {
		t.Fatal("a handoff block with no successor prompt is refused by the driver, so the report is still owed")
	}
}

func TestReportCheck_Review(t *testing.T) {
	r := doneRun(t, reportReview, nil, resumed())
	check := r.reportCheck()
	if check == nil {
		t.Fatal("a runner-briefed review phase owes a report, got no check")
	}
	const otherTip = "1111222233334444555566667777888899990000"
	want := config.DoneReport{Verdict: "pass", ReviewedTip: otherTip}.ValidateReview(doneTip)
	if want == nil {
		t.Fatal("the fixture names the wrong tip but ValidateReview accepted it")
	}
	err := check(doneReportFor(`{"verdict":"pass","reviewedTip":"` + otherTip + `"}`))
	if err == nil || err.Error() != want.Error() {
		t.Fatalf("wrong tip: err = %v, want ValidateReview's own %q", err, want)
	}
	if err := check(doneReportFor(`{"verdict":"pass","reviewedTip":"` + doneTip + `"}`)); err != nil {
		t.Fatalf("a valid review report was refused: %v", err)
	}
}

func TestReportCheck_Fix(t *testing.T) {
	const batch = `[{"findingId":"1.review-1.1","status":"fixed","fixCommit":"abc1234"},` +
		`{"findingId":"1.review-1.1","status":"fixed","fixCommit":"abc1234"}]`
	for _, kind := range []reportKind{reportFixVerify, reportFixLast} {
		r := doneRun(t, kind, nil, resumed())
		check := r.reportCheck()
		if check == nil {
			t.Fatalf("kind %v: a runner-briefed fix phase owes a report, got no check", kind)
		}
		if err := check(doneReportFor(`{"summary":"fixed it","findingUpdates":` + batch + `}`)); err == nil ||
			!strings.Contains(err.Error(), "duplicate findingId") {
			t.Fatalf("kind %v: duplicate batch: err = %v, want the duplicate refused", kind, err)
		}
		valid := `{"summary":"fixed it","findingUpdates":[{"findingId":"1.review-1.1","status":"fixed","fixCommit":"abc1234"}]}`
		if err := check(doneReportFor(valid)); err != nil {
			t.Fatalf("kind %v: a valid fix report was refused: %v", kind, err)
		}
		// A park replaces every other record, so the batch is never used.
		if err := check(doneReportFor(`{"park":"superseded by abc1234","findingUpdates":` + batch + `}`)); err != nil {
			t.Fatalf("kind %v: a parked fix was refused over a batch the runner would not read: %v", kind, err)
		}
	}
}

func TestReportCheck_Verify(t *testing.T) {
	r := doneRun(t, reportVerify, nil, resumed())
	check := r.reportCheck()
	if check == nil {
		t.Fatal("a runner-briefed verify phase owes a report, got no check")
	}
	const otherTip = "1111222233334444555566667777888899990000"
	want := config.DoneReport{Verdict: "pass", ReviewedTip: otherTip}.ValidateReview(doneTip)
	if want == nil {
		t.Fatal("the fixture names the wrong tip but ValidateReview accepted it")
	}
	err := check(doneReportFor(`{"verdict":"pass","reviewedTip":"` + otherTip + `"}`))
	if err == nil || err.Error() != want.Error() {
		t.Fatalf("wrong tip: err = %v, want ValidateReview's own %q", err, want)
	}
	// The round's finding answers ride the same record check.
	badBatch := `{"verdict":"pass","reviewedTip":"` + doneTip + `","findingUpdates":[` +
		`{"findingId":"1.review-1.1","status":"fixed","fixCommit":"abc1234"},` +
		`{"findingId":"1.review-1.1","status":"open"}]}`
	if err := check(doneReportFor(badBatch)); err == nil || !strings.Contains(err.Error(), "duplicate findingId") {
		t.Fatalf("duplicate batch: err = %v, want the duplicate refused", err)
	}
	if err := check(doneReportFor(`{"verdict":"pass","reviewedTip":"` + doneTip + `"}`)); err != nil {
		t.Fatalf("a valid verification report was refused: %v", err)
	}
}

// buildInvocation is where the report contract reaches the adapter: a phase that
// owes a report carries a check, and one that does not carries none.
func TestBuildInvocation_SetsTheReportCheck(t *testing.T) {
	d, _ := phaseDriver(t, &fakeAdapter{}, twoPhases())
	phases := twoPhases()
	for _, tc := range []struct {
		name string
		kind reportKind
		want bool
	}{
		{name: "a runner-briefed review owes one", kind: reportReview, want: true},
		{name: "a phase that owes no report", kind: reportNone, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &phaseRun{
				d: d, t: d.targets[0], ph: phases[1], phases: phases, phaseIdx: 1,
				kind: tc.kind, prBase: "staging",
			}
			at := &phaseAttempt{}
			if v := r.buildInvocation(at); v.next != attemptFall || v.err != nil {
				t.Fatalf("buildInvocation: next=%v err=%v", v.next, v.err)
			}
			if got := at.inv.CheckReport != nil; got != tc.want {
				t.Fatalf("CheckReport set = %v, want %v", got, tc.want)
			}
		})
	}
}
