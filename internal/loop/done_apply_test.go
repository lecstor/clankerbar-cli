package loop

// done_apply_test.go — the runner writes a session's records from its done
// report. The tests here pin what each report kind writes and in what ORDER
// (the plane refuses the run's outcome once its review record is sealed, and
// answers findings only after they are recorded), what a missing or invalid
// report writes (nothing), and the verify round's hand-over.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// The write order a test asserts against. Reads are counted, never listed: only
// the writes have an order the plane depends on.
const (
	callOutcome  = "RecordOutcome"
	callAppend   = "AppendReview"
	callFindings = "UpdateFindings"
	callSubmit   = "SubmitForReview"
	callBranch   = "RecordBranch"
	callPark     = "Park"
	callClose    = "CloseReviewRound"
)

// reportReleaser is the runner's plane client as a done report uses it: the
// task read the records are appended to, and every write in the order it
// arrived. The writes model what the real plane does to the record they write,
// so a later read sees them the way a retried attempt would.
type reportReleaser struct {
	startReleaser
	st    plane.TaskState
	stErr error
	calls []string
	// errs fails one write by its call name, for the write-failure paths.
	errs map[string]error

	outcomes []string
	appends  []plane.ReviewAppend
	updates  [][]plane.FindingUpdate
	submits  []plane.Submission
	branches []string
	parks    []string
	rounds   int
}

func (r *reportReleaser) TaskState(context.Context, string) (plane.TaskState, error) {
	if r.stErr != nil {
		return plane.TaskState{}, r.stErr
	}
	return r.st, nil
}

func (r *reportReleaser) write(name string) error {
	r.calls = append(r.calls, name)
	return r.errs[name]
}

func (r *reportReleaser) RecordOutcome(_ context.Context, _, _, outcome string) error {
	if err := r.write(callOutcome); err != nil {
		return err
	}
	// The statusless outcome replaces the whole field, as on the plane.
	r.st.Outcome = outcome
	r.outcomes = append(r.outcomes, outcome)
	return nil
}

func (r *reportReleaser) AppendReview(_ context.Context, _, runID string, rec plane.ReviewAppend) (int, error) {
	if err := r.write(callAppend); err != nil {
		return 0, err
	}
	r.appends = append(r.appends, rec)
	r.rounds++
	// The record lands SEALED, with its findings withheld, exactly as the plane
	// stores it while its round is open.
	r.st.Reviews = append(r.st.Reviews, plane.ReviewRecordState{
		Round:        r.rounds,
		ReviewerRole: rec.Role,
		RunID:        runID,
		ReviewedTip:  rec.ReviewedTip,
		Branch:       rec.Branch,
		State:        "sealed",
	})
	return r.rounds, nil
}

func (r *reportReleaser) UpdateFindings(_ context.Context, _, _ string, updates []plane.FindingUpdate) error {
	if err := r.write(callFindings); err != nil {
		return err
	}
	r.updates = append(r.updates, updates)
	return nil
}

func (r *reportReleaser) SubmitForReview(_ context.Context, _, _ string, sub plane.Submission) error {
	if err := r.write(callSubmit); err != nil {
		return err
	}
	r.submits = append(r.submits, sub)
	r.st.Status = "in_review"
	return nil
}

func (r *reportReleaser) RecordBranch(_ context.Context, _, _, branch string) error {
	if err := r.write(callBranch); err != nil {
		return err
	}
	r.branches = append(r.branches, branch)
	r.st.Branch = branch
	return nil
}

func (r *reportReleaser) Park(_ context.Context, _, _, outcome string) error {
	if err := r.write(callPark); err != nil {
		return err
	}
	r.parks = append(r.parks, outcome)
	r.st.Status = "parked"
	return nil
}

func (r *reportReleaser) CloseReviewRound(_ context.Context, _, _ string, round int) error {
	if err := r.write(callClose); err != nil {
		return err
	}
	for i := range r.st.Reviews {
		if r.st.Reviews[i].Round == round && r.st.Reviews[i].State == "sealed" {
			r.st.Reviews[i].State = "open"
		}
	}
	return nil
}

var _ plane.ReportWriter = (*reportReleaser)(nil)

// doneTip is the reviewed tip every review report in these tests names.
const doneTip = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

// doneReportFor renders a final message carrying rep after the marker, the shape
// every runner brief prescribes.
func doneReportFor(rep string) string {
	return "Done.\n" + config.DoneMarker + "\n" + rep
}

// doneRun is one phase pass ready to apply a report: the kind is set directly,
// so a test exercises the writes rather than the brief that decided them.
func doneRun(t *testing.T, kind reportKind, rel plane.Releaser, prev *harness.Result) *phaseRun {
	t.Helper()
	return &phaseRun{
		d:        &Driver{cfg: &config.Config{}},
		t:        Target{Name: "t", Releaser: rel},
		phases:   []config.Phase{{Name: config.ImplementPhaseName}, {Name: config.ReviewPhaseName}},
		ph:       config.Phase{Name: config.ReviewPhaseName, Prompt: config.ReviewOnlyBrief()},
		phaseIdx: 1,
		kind:     kind,
		prev:     prev,
		prBase:   "staging",
	}
}

// doneAttempt is a clean exit still holding the task, with final as its report
// and the attribution invocationFor resolved for the phase.
func doneAttempt(final string) *phaseAttempt {
	at := &phaseAttempt{inv: harness.Invocation{WorkDir: "/repo"}, orderlyExit: true}
	at.res = harness.Result{
		ExitCode:     0,
		FinalMessage: final,
		Claim:        harness.Claim{TaskID: "t-1", Ref: "CLA-1", RunID: "r-1"},
	}
	at.attr = config.Attribution{Harness: "opencode", Model: "deepseek", PrevHarness: "opencode", PrevModel: "cheap"}
	at.role = "review-1"
	return at
}

// resumed is a predecessor claim with a verified tip: the checkpoint a later
// phase resumes.
func resumed() *harness.Result {
	return &harness.Result{
		Claim:         harness.Claim{TaskID: "t-1", Ref: "CLA-1", RunID: "r-0", HasWIP: true, Branch: "task/t-1"},
		CheckpointTip: doneTip,
	}
}

func TestApplyDoneReport_ImplementRecordsTheBranchThenTheDelivery(t *testing.T) {
	rel := &reportReleaser{}
	r := doneRun(t, reportImplement, rel, nil)
	r.opts.runner = &runnerBriefing{task: plane.TaskBrief{Branch: "clanker/t-1"}}
	r.d.newVerifier = func(string, bool) deliveryVerifier { return passVerifier() }
	at := doneAttempt(doneReportFor(`{"summary":"wired it","tests":"go test ./..."}`))
	r.end.claim = &at.res

	r.applyDoneReport(context.Background(), at)

	if want := []string{callBranch, callOutcome}; !equalCalls(rel.calls, want) {
		t.Fatalf("writes = %v, want %v — the branch must be recorded before the outcome it belongs to", rel.calls, want)
	}
	if len(rel.branches) != 1 || rel.branches[0] != "clanker/t-1" {
		t.Errorf("recorded branches = %v, want the runner's own name for the fresh claim", rel.branches)
	}
	if !at.res.Claim.HasWIP || at.res.Claim.Branch != "clanker/t-1" {
		t.Errorf("claim = %+v, want the recorded branch folded onto it", at.res.Claim)
	}
	if len(rel.outcomes) != 1 || !strings.Contains(rel.outcomes[0], "## Delivery") {
		t.Fatalf("outcomes = %v, want one delivery section", rel.outcomes)
	}
	if !strings.Contains(rel.outcomes[0], "**Tests**") {
		t.Errorf("the delivery outcome carries no Tests section: %s", rel.outcomes[0])
	}
	if !strings.Contains(rel.outcomes[0], "harness opencode, model deepseek") {
		t.Errorf("the delivery is not attributed to the session: %s", rel.outcomes[0])
	}
}

func TestApplyDoneReport_ImplementParkWritesTheParkOnly(t *testing.T) {
	rel := &reportReleaser{st: plane.TaskState{Outcome: "an earlier record"}}
	r := doneRun(t, reportImplement, rel, nil)
	r.d.newVerifier = func(string, bool) deliveryVerifier { return passVerifier() }
	at := doneAttempt(doneReportFor(`{"park":"superseded by 1234abc"}`))
	r.end.claim = &at.res

	r.applyDoneReport(context.Background(), at)

	if want := []string{callPark}; !equalCalls(rel.calls, want) {
		t.Fatalf("writes = %v, want only %v — a park replaces the delivery record", rel.calls, want)
	}
	if len(rel.parks) != 1 {
		t.Fatalf("parks = %v, want one", rel.parks)
	}
	if !strings.Contains(rel.parks[0], "an earlier record") || !strings.Contains(rel.parks[0], "superseded by 1234abc") {
		t.Errorf("the park outcome must carry the record it found AND the reason: %s", rel.parks[0])
	}
	if !at.res.Claim.Settled {
		t.Error("a successful park left the claim held — the driver would hand the task back over its own park")
	}
}

func TestApplyDoneReport_ReviewWritesTheOutcomeThenTheRecord(t *testing.T) {
	rel := &reportReleaser{st: plane.TaskState{Outcome: "implemented"}}
	r := doneRun(t, reportReview, rel, resumed())
	at := doneAttempt(doneReportFor(`{"verdict":"findings","reviewedTip":"` + doneTip + `","findings":[{"severity":"must","summary":"a leak","evidenceKind":"test","evidence":"go test fails","file":"a.go","line":7}]}`))
	r.end.claim = &at.res

	r.applyDoneReport(context.Background(), at)

	if want := []string{callOutcome, callAppend}; !equalCalls(rel.calls, want) {
		t.Fatalf("writes = %v, want %v — the plane refuses the outcome once the record is sealed", rel.calls, want)
	}
	if len(rel.appends) != 1 {
		t.Fatalf("appended %d records, want one", len(rel.appends))
	}
	rec := rel.appends[0]
	if rec.Role != "review-1" || rec.Verdict != "findings" || rec.ReviewedTip != doneTip {
		t.Errorf("record = %+v, want this run's review-1 record at the reviewed tip", rec)
	}
	if rec.ScopeKind != "full-diff" || rec.ScopeBase != "staging" || rec.ScopeRange != "staging.."+doneTip {
		t.Errorf("scope = %s %s %s, want the full diff against the PR base", rec.ScopeKind, rec.ScopeBase, rec.ScopeRange)
	}
	if rec.Harness != "opencode" || rec.Model != "deepseek" {
		t.Errorf("record attribution = %s/%s, want the session that reviewed", rec.Harness, rec.Model)
	}
	if len(rec.Findings) != 1 || rec.Findings[0].RaisedByModel != "deepseek" {
		t.Errorf("findings = %+v, want one finding attributed to the reviewing model", rec.Findings)
	}
	if !strings.Contains(rel.outcomes[0], config.ReviewRecordHeading) {
		t.Errorf("the outcome carries no review block: %s", rel.outcomes[0])
	}
}

func TestApplyDoneReport_ReviewSkipsARecordThatAlreadyMatches(t *testing.T) {
	rel := &reportReleaser{st: plane.TaskState{
		Outcome: "implemented\n\n" + config.ReviewRecordHeading + "\n- reviewed tip: " + doneTip,
		Reviews: []plane.ReviewRecordState{{Round: 1, ReviewerRole: "review-1", RunID: "r-1", ReviewedTip: doneTip, State: "open"}},
	}}
	r := doneRun(t, reportReview, rel, resumed())
	at := doneAttempt(doneReportFor(`{"verdict":"pass","reviewedTip":"` + doneTip + `"}`))
	r.end.claim = &at.res

	r.applyDoneReport(context.Background(), at)

	if len(rel.calls) != 0 {
		t.Fatalf("writes = %v, want none — this run's record is already on the task", rel.calls)
	}
	if r.end.reportProblem != "" {
		t.Errorf("reportProblem = %q, want none: a record that is already there is not a failure", r.end.reportProblem)
	}
}

func TestApplyDoneReport_ReviewTreatsADuplicateReviewerAsWritten(t *testing.T) {
	rel := &reportReleaser{errs: map[string]error{callAppend: fmt.Errorf("%w: review-1 already recorded", plane.ErrDuplicateReviewer)}}
	r := doneRun(t, reportReview, rel, resumed())
	at := doneAttempt(doneReportFor(`{"verdict":"pass","reviewedTip":"` + doneTip + `"}`))
	r.end.claim = &at.res

	r.applyDoneReport(context.Background(), at)

	if want := []string{callOutcome, callAppend}; !equalCalls(rel.calls, want) {
		t.Fatalf("writes = %v, want %v", rel.calls, want)
	}
	if r.end.reportProblem != "" {
		t.Errorf("reportProblem = %q, want none: the plane's duplicate refusal IS the record being written", r.end.reportProblem)
	}
}

func TestApplyDoneReport_FixAnswersFindingsBeforeTheOutcome(t *testing.T) {
	rel := &reportReleaser{st: plane.TaskState{Outcome: "reviewed"}}
	r := doneRun(t, reportFixVerify, rel, resumed())
	at := doneAttempt(doneReportFor(`{"summary":"fixed it","tests":"go test ./...","findingUpdates":[{"findingId":"1.review-1.1","status":"fixed","fixCommit":"abc1234"}]}`))
	r.end.claim = &at.res

	r.applyDoneReport(context.Background(), at)

	if want := []string{callFindings, callOutcome}; !equalCalls(rel.calls, want) {
		t.Fatalf("writes = %v, want %v — a finding is answered before the outcome describes the fix", rel.calls, want)
	}
	if len(rel.updates) != 1 || len(rel.updates[0]) != 1 || rel.updates[0][0].FindingID != "1.review-1.1" {
		t.Errorf("finding updates = %+v, want the report's one update", rel.updates)
	}
	if len(rel.outcomes) != 1 || !strings.Contains(rel.outcomes[0], "### Fixes") {
		t.Fatalf("outcomes = %v, want one fixes section", rel.outcomes)
	}
	if !strings.Contains(rel.outcomes[0], "fixed by: harness opencode, model deepseek") {
		t.Errorf("the fixes are not attributed to the session: %s", rel.outcomes[0])
	}
	if strings.Contains(rel.outcomes[0], "record_review") {
		t.Errorf("the fixes outcome carries the wrong shape: %s", rel.outcomes[0])
	}
}

func TestApplyDoneReport_FixLastAnswersFindingsThenSubmits(t *testing.T) {
	rel := &reportReleaser{st: plane.TaskState{Outcome: "reviewed"}}
	r := doneRun(t, reportFixLast, rel, resumed())
	r.d.newVerifier = func(string, bool) deliveryVerifier { return passVerifier() }
	at := doneAttempt(doneReportFor(`{"summary":"fixed it","tests":"go test ./...","risk":{"touches":["db"]},"findingUpdates":[{"findingId":"1.review-1.1","status":"fixed","fixCommit":"abc1234"}]}`))
	r.end.claim = &at.res

	r.applyDoneReport(context.Background(), at)

	if want := []string{callFindings, callBranch, callSubmit}; !equalCalls(rel.calls, want) {
		t.Fatalf("writes = %v, want %v — the branch is recorded, and the hand-over carries the outcome", rel.calls, want)
	}
	if len(rel.submits) != 1 {
		t.Fatalf("submissions = %+v, want one", rel.submits)
	}
	sub := rel.submits[0]
	if !sub.RiskDeclared || len(sub.Touches) != 1 || sub.Touches[0] != "db" {
		t.Errorf("submission risk = %v/%v, want the report's declaration", sub.RiskDeclared, sub.Touches)
	}
	if !strings.Contains(sub.Outcome, "### Fixes") || !strings.Contains(sub.Outcome, "**Tests**") {
		t.Errorf("the hand-over outcome is not the fixes section: %s", sub.Outcome)
	}
	if !at.res.Claim.Settled {
		t.Error("a successful hand-over did not settle the claim — the driver would hand the task back over its own submission")
	}
}

func TestApplyDoneReport_FixLastDeclaresAnEmptyRisk(t *testing.T) {
	rel := &reportReleaser{}
	r := doneRun(t, reportFixLast, rel, resumed())
	r.d.newVerifier = func(string, bool) deliveryVerifier { return passVerifier() }
	at := doneAttempt(doneReportFor(`{"summary":"fixed it","risk":{"touches":[]}}`))
	r.end.claim = &at.res

	r.applyDoneReport(context.Background(), at)

	if len(rel.submits) != 1 {
		t.Fatalf("submissions = %+v, want one", rel.submits)
	}
	if !rel.submits[0].RiskDeclared || rel.submits[0].Touches == nil {
		t.Error("touches: [] is a DECLARATION that touches nothing; it must not read as undeclared")
	}
}

func TestApplyDoneReport_VerifyAnswersFindingsThenRecordsTheDelta(t *testing.T) {
	const base = "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111"
	rel := &reportReleaser{st: plane.TaskState{
		Outcome: "fixed",
		Status:  "in_progress",
		Reviews: []plane.ReviewRecordState{{Round: 1, ReviewerRole: "review-1", RunID: "r-0", ReviewedTip: base, State: "open"}},
	}}
	r := doneRun(t, reportVerify, rel, resumed())
	at := doneAttempt(doneReportFor(`{"verdict":"pass","reviewedTip":"` + doneTip + `","findingUpdates":[{"findingId":"1.review-1.1","status":"fixed","fixCommit":"abc1234"}]}`))
	r.end.claim = &at.res

	r.applyDoneReport(context.Background(), at)

	if want := []string{callFindings, callOutcome, callAppend}; !equalCalls(rel.calls, want) {
		t.Fatalf("writes = %v, want %v", rel.calls, want)
	}
	if len(rel.appends) != 1 {
		t.Fatalf("appended %d records, want one", len(rel.appends))
	}
	rec := rel.appends[0]
	if rec.ScopeKind != "fix-delta" || rec.ScopeBase != base || rec.ScopeRange != base+".."+doneTip {
		t.Errorf("scope = %s %s %s, want the fix delta on the newest released record", rec.ScopeKind, rec.ScopeBase, rec.ScopeRange)
	}
	if r.end.doneReport == nil {
		t.Error("the report was not kept for the hand-over judgeVerifyRound performs")
	}
}

func TestApplyDoneReport_MissingReportWritesNothing(t *testing.T) {
	rel := &reportReleaser{}
	r := doneRun(t, reportImplement, rel, nil)
	r.d.newVerifier = func(string, bool) deliveryVerifier { return passVerifier() }
	at := doneAttempt("I pushed it and stopped.")

	r.applyDoneReport(context.Background(), at)

	if len(rel.calls) != 0 {
		t.Fatalf("writes = %v, want none without a done report", rel.calls)
	}
	if r.end.reportProblem == "" || !strings.Contains(r.end.reportProblem, "without a done report") {
		t.Errorf("reportProblem = %q, want the missing report named for the gate's diagnostic", r.end.reportProblem)
	}
}

func TestApplyDoneReport_InvalidReportWritesNothing(t *testing.T) {
	cases := []struct{ name, report string }{
		{"not JSON", "not json at all"},
		{"two values", `{"summary":"a"}{"summary":"b"}`},
		{"a verdict its findings contradict", `{"verdict":"pass","reviewedTip":"` + doneTip + `","findings":[{"severity":"must","summary":"x"}]}`},
		{"a reviewed tip that is not a commit", `{"verdict":"pass","reviewedTip":"nope"}`},
		{"a tip that is not the verified one", `{"verdict":"pass","reviewedTip":"1111222233334444555566667777888899990000"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rel := &reportReleaser{}
			r := doneRun(t, reportReview, rel, resumed())
			at := doneAttempt(doneReportFor(tc.report))

			r.applyDoneReport(context.Background(), at)

			if len(rel.calls) != 0 {
				t.Fatalf("writes = %v, want none for a report the runner cannot use", rel.calls)
			}
			if r.end.reportProblem != "" && !strings.Contains(r.end.reportProblem, "done report") {
				t.Errorf("reportProblem = %q, want it to name the report", r.end.reportProblem)
			}
		})
	}
}

func TestApplyDoneReport_UntrustedStreamWritesNothing(t *testing.T) {
	rel := &reportReleaser{}
	r := doneRun(t, reportImplement, rel, nil)
	r.opts.runner = &runnerBriefing{task: plane.TaskBrief{Branch: "clanker/t-1"}}
	r.d.newVerifier = func(string, bool) deliveryVerifier { return passVerifier() }
	at := doneAttempt(doneReportFor(`{"summary":"done"}`))
	at.res.Untrusted = "the stream was cut short"

	r.applyDoneReport(context.Background(), at)

	if len(rel.calls) != 0 {
		t.Fatalf("writes = %v, want none from a stream that could not be read whole", rel.calls)
	}
	if r.end.reportProblem == "" {
		t.Error("reportProblem is empty; the operator is owed the reason nothing was written")
	}
}

func TestApplyDoneReport_HandoffWritesNothing(t *testing.T) {
	cases := []struct {
		name string
		mark func(*phaseAttempt)
	}{
		// An HONORED handoff: the successor continues this phase and owes the report.
		{"an honored handoff", func(at *phaseAttempt) { at.handoff = "finish it" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rel := &reportReleaser{}
			r := doneRun(t, reportReview, rel, resumed())
			at := doneAttempt(doneReportFor(`{"verdict":"pass","reviewedTip":"` + doneTip + `"}`))
			tc.mark(at)

			r.applyDoneReport(context.Background(), at)

			if len(rel.calls) != 0 {
				t.Fatalf("writes = %v, want none: a handoff's successor owes the record", rel.calls)
			}
			if r.end.reportProblem != "" {
				t.Errorf("reportProblem = %q, want none — a handoff is not a failed report", r.end.reportProblem)
			}
		})
	}
}

// A handoff the driver cleared (every verify round) or refused has no successor,
// so a report the session also wrote is the only record the phase gets.
func TestApplyDoneReport_ClearedHandoffStillApplies(t *testing.T) {
	rel := &reportReleaser{}
	r := doneRun(t, reportReview, rel, resumed())
	at := doneAttempt(doneReportFor(`{"verdict":"pass","reviewedTip":"` + doneTip + `"}`))
	at.emittedHandoff = true
	r.end.claim = &at.res

	r.applyDoneReport(context.Background(), at)

	if want := []string{callOutcome, callAppend}; !equalCalls(rel.calls, want) {
		t.Fatalf("writes = %v, want %v — no successor will write this record", rel.calls, want)
	}
}

// A fix that found the review record missing or stale parks and changes
// nothing: no finding answers, no Fixes section.
func TestApplyDoneReport_FixParkWritesTheParkOnly(t *testing.T) {
	for _, kind := range []reportKind{reportFixVerify, reportFixLast} {
		rel := &reportReleaser{st: plane.TaskState{Outcome: "reviewed"}}
		r := doneRun(t, kind, rel, resumed())
		r.d.newVerifier = func(string, bool) deliveryVerifier { return passVerifier() }
		at := doneAttempt(doneReportFor(`{"park":"reviewedTip 1234567 is not HEAD","findingUpdates":[{"findingId":"1.review-1.1","status":"fixed","fixCommit":"abc1234"}]}`))
		r.end.claim = &at.res

		r.applyDoneReport(context.Background(), at)

		if want := []string{callPark}; !equalCalls(rel.calls, want) {
			t.Fatalf("kind %v: writes = %v, want only %v", kind, rel.calls, want)
		}
		if !at.res.Claim.Settled {
			t.Errorf("kind %v: the park did not settle the claim", kind)
		}
	}
}

// A plane client that cannot write records is a configuration gap, not a phase
// failure: the report is simply not applied, and nothing is logged per attempt
// beyond the one note.
func TestApplyDoneReport_NoWriterClientWritesNothing(t *testing.T) {
	rel := &fakeReleaser{}
	r := doneRun(t, reportImplement, rel, nil)
	at := doneAttempt(doneReportFor(`{"summary":"done"}`))
	r.end.claim = &at.res

	r.applyDoneReport(context.Background(), at)

	if r.end.reportProblem != "" {
		t.Errorf("reportProblem = %q, want none: the phase is judged by its evidence gate", r.end.reportProblem)
	}
	if !r.reportWriterNoted {
		t.Error("the missing write surface was not noted")
	}
}

// The kind is what the RUNNER BRIEF of the captured phase decides — never the
// handoff-rewritten prompt, which is the predecessor's successor text.
func TestReportKindFor_UsesTheCapturedBrief(t *testing.T) {
	phases := []config.Phase{
		{Name: config.ImplementPhaseName, Prompt: config.BuiltinImplementBrief()},
		{Name: config.ReviewPhaseName, Prompt: config.ReviewOnlyBrief()},
		{Name: config.FixPhaseName, Prompt: config.FixBrief()},
		{Name: config.FixPhaseName, Prompt: config.FixVerifyBrief()},
		{Name: config.VerifyPhaseName, Prompt: config.VerifyBrief()},
	}
	rb := &runnerBriefing{task: plane.TaskBrief{Branch: "clanker/t-1"}}
	cases := []struct {
		name   string
		idx    int
		rb     *runnerBriefing
		panel  bool
		prev   *harness.Result
		noPrev bool
		want   reportKind
	}{
		{name: "implement", idx: 0, rb: rb, want: reportImplement},
		{name: "review-only", idx: 1, rb: rb, want: reportReview},
		{name: "fix is the last phase it runs", idx: 2, rb: rb, want: reportFixLast},
		{name: "fix before a verify", idx: 3, rb: rb, want: reportFixVerify},
		{name: "verify", idx: 4, rb: rb, want: reportVerify},
		{name: "a close-out reports nothing", idx: 0, rb: &runnerBriefing{closeOut: true}, want: reportNone},
		{name: "a session-claim drain", idx: 0, rb: nil, want: reportNone},
		{name: "a panel member", idx: 1, rb: rb, panel: true, want: reportNone},
		// The checkpoint variants still run shipped briefs that write their own records.
		{name: "a branchless review", idx: 1, rb: rb, prev: branchless(), want: reportNone},
		{name: "an unverified review", idx: 1, rb: rb, prev: unverified(), want: reportNone},
		{name: "a branchless fix", idx: 2, rb: rb, prev: branchless(), want: reportNone},
		{name: "no held claim to resume", idx: 1, rb: rb, noPrev: true, want: reportNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev := resumed()
			if tc.prev != nil {
				prev = tc.prev
			}
			if tc.noPrev {
				prev = nil
			}
			if got := reportKindFor(phases, tc.idx, tc.rb, tc.panel, prev); got != tc.want {
				t.Fatalf("reportKindFor = %v, want %v", got, tc.want)
			}
		})
	}
}

func branchless() *harness.Result {
	r := resumed()
	r.Claim.Branch = ""
	r.Claim.HasWIP = false
	return r
}

func unverified() *harness.Result {
	r := resumed()
	r.CheckpointUnverified = true
	return r
}

// A report-driven review makes no clankerbar call BY CONTRACT, so the no-MCP
// backstop must not read that silence as a missing MCP server.
func TestClassifyEmpty_DoesNotFlagAReportDrivenReviewAsNoMCP(t *testing.T) {
	rel := &reportReleaser{}
	newRun := func(kind reportKind) *phaseRun {
		r := doneRun(t, kind, rel, resumed())
		r.tc = &config.Config{}
		r.a = &fakeAdapter{}
		r.shape.reviewOnly = true
		return r
	}
	empty := func() *phaseAttempt {
		at := doneAttempt("")
		at.res.ClankerbarCalls = 0
		return at
	}

	reportDriven := newRun(reportReview)
	at := empty()
	reportDriven.classifyEmpty(at)
	if at.noMCPNow || at.retryNoMCP {
		t.Fatal("a runner-claimed review that made no clankerbar call was flagged no_mcp — its brief forbids exactly that call")
	}

	// The control: the same silence on a session that OWNS its writes still is
	// the infrastructure miss the backstop exists for.
	sessionRun := newRun(reportNone)
	at = empty()
	sessionRun.classifyEmpty(at)
	if !at.noMCPNow {
		t.Fatal("the control case did not flag no_mcp; the guard would hide a real missing MCP server")
	}
}

func TestHandOverVerifiedRound(t *testing.T) {
	openMust := plane.ReviewRecordState{
		Round: 1, ReviewerRole: "review-1", RunID: "r-0", ReviewedTip: doneTip, State: "open",
		Findings: []plane.ReviewFindingState{{ID: "1.review-1.1", Severity: "must", Status: "open"}},
	}
	voidMust := openMust
	voidMust.State = "void"

	cases := []struct {
		name   string
		kind   reportKind
		report *config.DoneReport
		state  plane.TaskState
		want   bool
	}{
		{name: "clean round, still in progress", kind: reportVerify, report: &config.DoneReport{},
			state: plane.TaskState{Status: "in_progress"}, want: true},
		{name: "an open MUST is work to do", kind: reportVerify, report: &config.DoneReport{},
			state: plane.TaskState{Status: "in_progress", Reviews: []plane.ReviewRecordState{openMust}}},
		{name: "a void record's MUST is not work", kind: reportVerify, report: &config.DoneReport{},
			state: plane.TaskState{Status: "in_progress", Reviews: []plane.ReviewRecordState{voidMust}}, want: true},
		{name: "the session already handed over", kind: reportVerify, report: &config.DoneReport{},
			state: plane.TaskState{Status: "in_review"}},
		{name: "no report was applied", kind: reportVerify,
			state: plane.TaskState{Status: "in_progress"}},
		{name: "not a verify phase", kind: reportReview, report: &config.DoneReport{},
			state: plane.TaskState{Status: "in_progress"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rel := &reportReleaser{}
			r := doneRun(t, tc.kind, rel, resumed())
			r.end.doneReport = tc.report
			at := doneAttempt("")
			r.end.claim = &at.res
			r.end.verifyStatus = tc.state.Status

			r.handOverVerifiedRound(context.Background(), at, tc.state)

			if got := len(rel.submits) == 1; got != tc.want {
				t.Fatalf("handed over = %v, want %v (writes %v)", got, tc.want, rel.calls)
			}
			if !tc.want {
				if at.res.Claim.Settled {
					t.Error("a task that was not handed over was settled anyway")
				}
				return
			}
			if r.end.verifyStatus != "in_review" {
				t.Errorf("verifyStatus = %q, want in_review", r.end.verifyStatus)
			}
			if !at.res.Claim.Settled {
				t.Error("the hand-over did not settle the claim")
			}
		})
	}
}

// The plane's in_review guard reads **Tests** from the stored outcome, so the
// verify hand-over supplies it only when the stored outcome lacks one.
func TestHandOverVerifiedRound_SuppliesTestsOnlyWhenMissing(t *testing.T) {
	for _, tc := range []struct {
		outcome  string
		wantSent bool
	}{
		{outcome: "reviewed, no tests section", wantSent: true},
		{outcome: "### Fixes\n\n**Tests**\ngo test ./...", wantSent: false},
	} {
		rel := &reportReleaser{}
		r := doneRun(t, reportVerify, rel, resumed())
		r.end.doneReport = &config.DoneReport{Tests: "re-ran the failing test"}
		at := doneAttempt("")
		r.end.claim = &at.res

		r.handOverVerifiedRound(context.Background(), at, plane.TaskState{Status: "in_progress", Outcome: tc.outcome})

		if len(rel.submits) != 1 {
			t.Fatalf("submissions = %+v, want one", rel.submits)
		}
		got := rel.submits[0].Outcome
		if tc.wantSent && (!strings.Contains(got, "re-ran the failing test") || !strings.Contains(got, tc.outcome)) {
			t.Errorf("outcome %q: sent %q, want the stored outcome plus the verifier's Tests", tc.outcome, got)
		}
		if !tc.wantSent && got != "" {
			t.Errorf("outcome %q: sent %q, want no outcome write", tc.outcome, got)
		}
	}
}

// The whole shape, driven through a drain: a runner-claimed review phase whose
// session writes nothing and ends with a done report is recorded by the runner,
// and the record the fake appended is what the seam then matches — so the walk
// advances to the fix phase.
func TestRunnerDoneReport_ReviewPhaseRecordsAndAdvances(t *testing.T) {
	brief := plane.TaskBrief{
		ID: walkClaimTaskID, Ref: "CLA-950", Branch: "clanker/11111111-review-me", HasWIP: true,
		PhasesDone: map[string]plane.PhaseDone{config.ImplementPhaseName: {Tip: testCheckpointTip}},
	}
	const runID = "run-uuid.secret"
	phases := []config.Phase{{Name: config.ImplementPhaseName}, {Name: config.ReviewPhaseName}, {Name: config.FixPhaseName}}

	rel := &reportReleaser{}
	rel.startReleaser = startReleaser{claimingReleaser: claimingReleaser{
		offers: []plane.Offer{{Next: &brief}},
		claimFn: func(string, bool) (plane.ClaimedTask, error) {
			return plane.ClaimedTask{Task: brief, RunID: runID}, nil
		},
	}}
	claim := harness.Claim{TaskID: brief.ID, Ref: brief.Ref, RunID: runID, HasWIP: true, Branch: brief.Branch}
	review := held(okResult(5, 0.50), claim)
	review.FinalMessage = doneReportFor(`{"verdict":"pass","reviewedTip":"` + testCheckpointTip + `"}`)
	fix := held(okResult(5, 0.50), claim)
	fix.FinalMessage = doneReportFor(`{"summary":"fixed it","tests":"go test ./...","risk":{"touches":[]}}`)
	h := &fakeAdapter{steps: []invokeStep{{res: review}, {res: fix}}}
	d := phaseDriverWithReleaser(t, h, phases, rel)
	// The recorded branch is on origin at the tip the implement phase was
	// marked done at, which is what selects the review start.
	d.originBranch = func(context.Context, string, string, string) originBranchFacts {
		return originBranchFacts{Known: true, OnOrigin: true, Tip: testCheckpointTip, DoneIsAncestor: true}
	}

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 2 {
		t.Fatalf("spawned %d sessions, want the review and then the fix: the seam refused a review whose report the runner wrote", h.invokeCalls)
	}
	if !strings.Contains(h.invocations[0].Prompt, config.DoneMarker) {
		t.Error("the review session was not given the runner's done-report brief")
	}
	if len(rel.appends) != 1 {
		t.Fatalf("appended %d review records, want the runner to write exactly one", len(rel.appends))
	}
	rec := rel.appends[0]
	if rec.Role != "review-1" || rec.Verdict != "pass" || rec.ReviewedTip != testCheckpointTip {
		t.Errorf("record = %+v, want this run's review-1 pass at the reviewed tip", rec)
	}
	if len(rel.outcomes) == 0 || !strings.Contains(rel.outcomes[0], config.ReviewRecordHeading) {
		t.Errorf("outcomes = %v, want the review block written before the record", rel.outcomes)
	}
	if len(rel.submits) != 1 {
		t.Errorf("submissions = %+v, want the fix phase to have handed the task over", rel.submits)
	}
}

// A failed write stops the rest of that kind: a review whose outcome could not
// be written must not append its record, because the plane would then hold a
// record whose block it never accepted.
func TestApplyDoneReport_AWriteFailureStopsTheRest(t *testing.T) {
	rel := &reportReleaser{errs: map[string]error{callOutcome: errors.New("the plane is not answering")}}
	r := doneRun(t, reportReview, rel, resumed())
	at := doneAttempt(doneReportFor(`{"verdict":"pass","reviewedTip":"` + doneTip + `"}`))
	r.end.claim = &at.res

	r.applyDoneReport(context.Background(), at)

	if want := []string{callOutcome}; !equalCalls(rel.calls, want) {
		t.Fatalf("writes = %v, want only the failed %v — the record must not follow a block that did not land", rel.calls, want)
	}
	if len(rel.appends) != 0 {
		t.Errorf("appended %d record(s) after the outcome write failed, want none", len(rel.appends))
	}
	if r.end.reportProblem == "" {
		t.Error("reportProblem is empty; the failed write is what the seam's diagnostic must name")
	}
}

// The handoff successor's report note comes from the captured phase, because a
// successor's own prompt is the predecessor's successor text, not a brief.
func TestRunnerDoneNote_FollowsTheCapturedPhase(t *testing.T) {
	phases := []config.Phase{
		{Name: config.ImplementPhaseName, Prompt: config.BuiltinImplementBrief()},
		{Name: config.ReviewPhaseName, Prompt: config.ReviewOnlyBrief()},
	}
	s := &drainSeq{phases: phases, rawPhases: phases, i: 1, start: 1, runnerClaimed: true, carried: resumed()}

	if got := s.runnerDoneNote(); !strings.Contains(got, config.DoneMarker) {
		t.Errorf("the review successor's note = %q, want the report format", got)
	}
	// A close-out starts at phase 0 and writes no report, so neither does its
	// successor.
	s.i, s.start, s.closeOut = 0, 0, true
	if got := s.runnerDoneNote(); got != "" {
		t.Errorf("the close-out successor's note = %q, want none", got)
	}
}

func equalCalls(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// An implement session that hands off still leaves its pushed branch recorded:
// whether the driver later honours or refuses the handoff, pushed work must not
// be released to ready as if there were none.
func TestApplyDoneReport_ImplementHandoffStillRecordsTheBranch(t *testing.T) {
	rel := &reportReleaser{}
	r := doneRun(t, reportImplement, rel, nil)
	r.opts.runner = &runnerBriefing{task: plane.TaskBrief{Branch: "clanker/t-1"}}
	r.d.newVerifier = func(string, bool) deliveryVerifier { return passVerifier() }
	at := doneAttempt("handing off\n" + config.HandoffMarker + "\ncarry on")
	at.handoff = "carry on"
	r.end.claim = &at.res

	r.applyDoneReport(context.Background(), at)

	if want := []string{callBranch}; !equalCalls(rel.calls, want) {
		t.Fatalf("writes = %v, want only %v — the delivery outcome is the successor's", rel.calls, want)
	}
	if !at.res.Claim.HasWIP {
		t.Error("the recorded branch was not folded onto the claim")
	}
}

// A park ends the walk: the claim is settled, so no later phase may spawn on a
// parked task.
func TestRunnerDoneReport_ImplementParkEndsTheWalk(t *testing.T) {
	brief := plane.TaskBrief{ID: walkClaimTaskID, Ref: "CLA-951", Branch: "clanker/11111111-park-me"}
	rel := &reportReleaser{}
	rel.startReleaser = startReleaser{claimingReleaser: claimingReleaser{
		offers: []plane.Offer{{Next: &brief}},
		claimFn: func(string, bool) (plane.ClaimedTask, error) {
			return plane.ClaimedTask{Task: brief, RunID: walkClaimRunID}, nil
		},
	}}
	claim := harness.Claim{TaskID: brief.ID, Ref: brief.Ref, RunID: walkClaimRunID}
	impl := held(okResult(5, 0.50), claim)
	impl.FinalMessage = doneReportFor(`{"park":"superseded by abc1234 on staging"}`)
	h := &fakeAdapter{steps: []invokeStep{{res: impl}}}
	d := phaseDriverWithReleaser(t, h, twoPhases(), rel)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 1 {
		t.Fatalf("spawned %d sessions, want 1: a parked task must not reach the review phase", h.invokeCalls)
	}
	if len(rel.parks) != 1 {
		t.Fatalf("parks = %v, want the runner to park on the session's reason", rel.parks)
	}
}
