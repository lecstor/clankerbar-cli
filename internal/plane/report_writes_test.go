package plane

// report_writes_test.go — the records a session writes about itself: what goes
// on the wire, which values never reach the wire, and the two review answers a
// caller must tell apart (a first record from one already written).

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// The capabilities are opt-in: the wired client offers the report writes, and a
// not-wired plane must not pretend it does.
func TestReportWriterCapability(t *testing.T) {
	srv, _ := serve(t, http.StatusOK, okBody)
	if _, ok := New(srv.URL+"/mcp/demo", "k-1").(ReportWriter); !ok {
		t.Fatal("the wired plane client does not implement ReportWriter, so no session record can ever be written")
	}
	if _, ok := New("", "").(ReportWriter); ok {
		t.Error("the not-wired client claims to write a session's records")
	}
}

func TestRecordOutcome_RequestShape(t *testing.T) {
	srv, got := serve(t, http.StatusOK, okBody)

	if err := mustClient(t, srv).RecordOutcome(context.Background(), "t-1", "r-1", "Shipped the thing."); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}
	if name := toolName(t, got.body); name != "record_progress" {
		t.Errorf("tool = %q, want record_progress", name)
	}
	want := map[string]any{
		"taskId":  "t-1",
		"runId":   "r-1",
		"outcome": "Shipped the thing.",
	}
	args := toolArgs(t, got.body)
	if !reflect.DeepEqual(args, want) {
		t.Errorf("arguments = %+v, want %+v", args, want)
	}
	// An outcome write reports words; it must not carry another write's payload.
	for _, forbidden := range []string{"branch", "delivery", "status", "phaseDone"} {
		if _, present := args[forbidden]; present {
			t.Errorf("the outcome write carried %q: %+v", forbidden, args)
		}
	}
}

func TestRecordOutcome_RefusesLocally(t *testing.T) {
	cases := []struct {
		name          string
		taskID, runID string
		outcome       string
	}{
		{name: "empty task id", runID: "r-1", outcome: "done"},
		{name: "empty run id", taskID: "t-1", outcome: "done"},
		{name: "empty outcome", taskID: "t-1", runID: "r-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, got := serve(t, http.StatusOK, okBody)
			if err := mustClient(t, srv).RecordOutcome(context.Background(), tc.taskID, tc.runID, tc.outcome); err == nil {
				t.Fatal("a value the plane would refuse must be refused locally")
			}
			if len(got.bodies) != 0 {
				t.Errorf("sent %d request(s), want none: %+v", len(got.bodies), got.bodies)
			}
		})
	}
}

func TestAppendReview_RequestShape(t *testing.T) {
	srv, got := serve(t, http.StatusOK, okBody)

	rec := ReviewAppend{
		Role:        "review-1",
		Verdict:     "findings",
		Harness:     "opencode",
		Model:       "model-1",
		Branch:      "clanker/t-1",
		ReviewedTip: "abc1234",
		ScopeKind:   "range",
		ScopeBase:   "main",
		ScopeRange:  "main..HEAD",
		Findings: []ReviewFinding{{
			Severity:      "must",
			Summary:       "The guard is missing.",
			EvidenceKind:  "file",
			Evidence:      "internal/plane/report_writes.go:1",
			File:          "internal/plane/report_writes.go",
			Line:          42,
			Kind:          "bug",
			RequiredFix:   "Guard the empty id.",
			RaisedByModel: "model-1",
		}},
	}
	if _, err := mustClient(t, srv).AppendReview(context.Background(), "t-1", "r-1", rec); err != nil {
		t.Fatalf("AppendReview: %v", err)
	}

	if name := toolName(t, got.body); name != "record_review" {
		t.Errorf("tool = %q, want record_review", name)
	}
	want := map[string]any{
		"taskId":       "t-1",
		"runId":        "r-1",
		"reviewerRole": "review-1",
		"verdict":      "findings",
		"harness":      "opencode",
		"model":        "model-1",
		"branch":       "clanker/t-1",
		"reviewedTip":  "abc1234",
		"scope":        map[string]any{"kind": "range", "base": "main", "range": "main..HEAD"},
		"findings": []any{map[string]any{
			"severity":      "must",
			"summary":       "The guard is missing.",
			"evidenceKind":  "file",
			"evidence":      "internal/plane/report_writes.go:1",
			"file":          "internal/plane/report_writes.go",
			"line":          float64(42),
			"kind":          "bug",
			"requiredFix":   "Guard the empty id.",
			"raisedByModel": "model-1",
		}},
	}
	if args := toolArgs(t, got.body); !reflect.DeepEqual(args, want) {
		t.Errorf("arguments = %+v, want %+v", args, want)
	}
}

// A pass with nothing else to say sends only what the plane requires: an empty
// scope is not a scope, and an empty findings list is not a findings list.
func TestAppendReview_OmitsEmptyMembers(t *testing.T) {
	srv, got := serve(t, http.StatusOK, okBody)

	if _, err := mustClient(t, srv).AppendReview(context.Background(), "t-1", "r-1",
		ReviewAppend{Role: "review-2", Verdict: "pass"}); err != nil {
		t.Fatalf("AppendReview: %v", err)
	}
	args := toolArgs(t, got.body)
	want := map[string]any{
		"taskId":       "t-1",
		"runId":        "r-1",
		"reviewerRole": "review-2",
		"verdict":      "pass",
	}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("arguments = %+v, want %+v", args, want)
	}
}

func TestAppendReview_RefusesLocally(t *testing.T) {
	cases := []struct {
		name          string
		taskID, runID string
		rec           ReviewAppend
	}{
		{name: "empty task id", runID: "r-1", rec: ReviewAppend{Role: "review-1", Verdict: "pass"}},
		{name: "empty run id", taskID: "t-1", rec: ReviewAppend{Role: "review-1", Verdict: "pass"}},
		{name: "empty role", taskID: "t-1", runID: "r-1", rec: ReviewAppend{Verdict: "pass"}},
		{name: "empty verdict", taskID: "t-1", runID: "r-1", rec: ReviewAppend{Role: "review-1"}},
		{name: "unknown verdict", taskID: "t-1", runID: "r-1", rec: ReviewAppend{Role: "review-1", Verdict: "approved"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, got := serve(t, http.StatusOK, okBody)
			if _, err := mustClient(t, srv).AppendReview(context.Background(), tc.taskID, tc.runID, tc.rec); err == nil {
				t.Fatal("a value the plane would refuse must be refused locally")
			}
			if len(got.bodies) != 0 {
				t.Errorf("sent %d request(s), want none: %+v", len(got.bodies), got.bodies)
			}
		})
	}
}

// The round is not an argument the caller can know: it comes back in the
// receipt, and the fix phase's finding ids are built from it.
func TestAppendReview_DecodesTheRound(t *testing.T) {
	const receipt = `{"task":{"id":"t-1","ref":"CLA-1","status":"in_review"},` +
		`"review":{"record":{"round":3,"cycle":1,"findings":[{"id":"3.review-1.1","severity":"must"}]}}}`
	srv, _ := serve(t, http.StatusOK, mcpTextBody(t, receipt))

	round, err := mustClient(t, srv).AppendReview(context.Background(), "t-1", "r-1",
		ReviewAppend{Role: "review-1", Verdict: "findings"})
	if err != nil {
		t.Fatalf("AppendReview: %v", err)
	}
	if round != 3 {
		t.Errorf("round = %d, want 3", round)
	}
}

// A receipt with no record is round 0, not an error: the write landed, and a
// caller that only needs the round must not treat a terse receipt as a failure.
func TestAppendReview_NoRoundInTheReceiptIsZero(t *testing.T) {
	srv, _ := serve(t, http.StatusOK, okBody)

	round, err := mustClient(t, srv).AppendReview(context.Background(), "t-1", "r-1",
		ReviewAppend{Role: "review-1", Verdict: "pass"})
	if err != nil {
		t.Fatalf("AppendReview: %v", err)
	}
	if round != 0 {
		t.Errorf("round = %d, want 0", round)
	}
}

// A duplicate reviewer is the ONE review refusal with a caller-visible meaning:
// the record is already written. It must be matchable by code, not prose.
func TestAppendReview_DuplicateReviewerIsTyped(t *testing.T) {
	srv, _ := serve(t, http.StatusOK, refusalBody(t, "duplicate_reviewer",
		"Round 1 already carries a record for reviewer role 'review-1'"))

	_, err := mustClient(t, srv).AppendReview(context.Background(), "t-1", "r-1",
		ReviewAppend{Role: "review-1", Verdict: "pass"})
	if !errors.Is(err, ErrDuplicateReviewer) {
		t.Fatalf("err = %v, want ErrDuplicateReviewer", err)
	}
	if !strings.Contains(err.Error(), "already carries a record") {
		t.Errorf("err = %v, want the plane's message carried", err)
	}
}

// Every other refusal stays an ordinary error: classifying one as a duplicate
// would report a record as written when the plane refused it outright.
func TestAppendReview_OtherRefusalsAreNotDuplicates(t *testing.T) {
	srv, _ := serve(t, http.StatusOK, refusalBody(t, "run_superseded", "a later run owns this task"))

	_, err := mustClient(t, srv).AppendReview(context.Background(), "t-1", "r-1",
		ReviewAppend{Role: "review-1", Verdict: "pass"})
	if err == nil {
		t.Fatal("a refused append must be an error")
	}
	if errors.Is(err, ErrDuplicateReviewer) {
		t.Errorf("err = %v was classified as a duplicate reviewer", err)
	}
	if !strings.Contains(err.Error(), "a later run owns this task") {
		t.Errorf("err = %v, want the plane's refusal carried through", err)
	}
}

func TestUpdateFindings_RequestShape(t *testing.T) {
	srv, got := serve(t, http.StatusOK, okBody)

	updates := []FindingUpdate{
		{FindingID: "1.review-1.1", Status: "fixed", FixCommit: "abc1234"},
		{FindingID: "1.review-1.2", Status: "refuted", RefutedReason: "the guard already exists"},
		{FindingID: "1.review-1.3", Status: "filed", FiledTaskRef: "CLA-9"},
	}
	if err := mustClient(t, srv).UpdateFindings(context.Background(), "t-1", "r-1", updates); err != nil {
		t.Fatalf("UpdateFindings: %v", err)
	}

	if name := toolName(t, got.body); name != "record_review" {
		t.Errorf("tool = %q, want record_review", name)
	}
	want := map[string]any{
		"taskId": "t-1",
		"runId":  "r-1",
		"findingUpdates": []any{
			map[string]any{"findingId": "1.review-1.1", "status": "fixed", "fixCommit": "abc1234"},
			map[string]any{"findingId": "1.review-1.2", "status": "refuted", "refutedReason": "the guard already exists"},
			map[string]any{"findingId": "1.review-1.3", "status": "filed", "filedTaskRef": "CLA-9"},
		},
	}
	if args := toolArgs(t, got.body); !reflect.DeepEqual(args, want) {
		t.Errorf("arguments = %+v, want %+v", args, want)
	}
}

// Nothing to answer is not a write: the plane refuses an empty array, and a
// no-op round-trip on every clean review would be noise.
func TestUpdateFindings_EmptyListMakesNoRequest(t *testing.T) {
	for _, updates := range [][]FindingUpdate{nil, {}} {
		srv, got := serve(t, http.StatusOK, okBody)
		if err := mustClient(t, srv).UpdateFindings(context.Background(), "t-1", "r-1", updates); err != nil {
			t.Fatalf("UpdateFindings(%v): %v", updates, err)
		}
		if len(got.bodies) != 0 {
			t.Errorf("sent %d request(s), want none: %+v", len(got.bodies), got.bodies)
		}
	}
}

func TestUpdateFindings_RefusesEmptyIdentity(t *testing.T) {
	updates := []FindingUpdate{{FindingID: "1.review-1.1", Status: "fixed"}}
	for _, tc := range []struct{ name, taskID, runID string }{
		{"no task", "", "r-1"},
		{"no run", "t-1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, got := serve(t, http.StatusOK, okBody)
			if err := mustClient(t, srv).UpdateFindings(context.Background(), tc.taskID, tc.runID, updates); err == nil {
				t.Fatal("expected an error")
			}
			if len(got.bodies) != 0 {
				t.Errorf("sent %d request(s), want none: %+v", len(got.bodies), got.bodies)
			}
		})
	}
}

func TestSubmitForReview_RequestShape(t *testing.T) {
	srv, got := serve(t, http.StatusOK, okBody)

	s := Submission{Outcome: "Done. **Tests**: go test ./...", RiskDeclared: true, Touches: []string{"api", "docs"}, CloseRound: 2}
	if err := mustClient(t, srv).SubmitForReview(context.Background(), "t-1", "r-1", s); err != nil {
		t.Fatalf("SubmitForReview: %v", err)
	}

	if name := toolName(t, got.body); name != "submit_for_review" {
		t.Errorf("tool = %q, want submit_for_review", name)
	}
	want := map[string]any{
		"taskId":     "t-1",
		"runId":      "r-1",
		"outcome":    "Done. **Tests**: go test ./...",
		"closeRound": float64(2),
		"risk":       map[string]any{"touches": []any{"api", "docs"}},
	}
	if args := toolArgs(t, got.body); !reflect.DeepEqual(args, want) {
		t.Errorf("arguments = %+v, want %+v", args, want)
	}
}

// An undeclared risk is no risk argument at all - that is what keeps a
// per-task human verdict - and an unset close or outcome is absent too.
func TestSubmitForReview_OmitsWhatWasNotDeclared(t *testing.T) {
	srv, got := serve(t, http.StatusOK, okBody)

	if err := mustClient(t, srv).SubmitForReview(context.Background(), "t-1", "r-1", Submission{}); err != nil {
		t.Fatalf("SubmitForReview: %v", err)
	}
	args := toolArgs(t, got.body)
	want := map[string]any{"taskId": "t-1", "runId": "r-1"}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("arguments = %+v, want %+v", args, want)
	}
}

// A declaration that touches nothing must be an empty ARRAY: absent or null
// would read as UNDECLARED, which silently loses the auto-approval signal.
func TestSubmitForReview_SendsAnEmptyTouchList(t *testing.T) {
	srv, got := serve(t, http.StatusOK, okBody)

	if err := mustClient(t, srv).SubmitForReview(context.Background(), "t-1", "r-1",
		Submission{Outcome: "No code.", RiskDeclared: true}); err != nil {
		t.Fatalf("SubmitForReview: %v", err)
	}
	args := toolArgs(t, got.body)
	risk, ok := args["risk"].(map[string]any)
	if !ok {
		t.Fatalf("risk = %v, want an object", args["risk"])
	}
	touches, ok := risk["touches"].([]any)
	if !ok {
		t.Fatalf("touches = %v, want a JSON array rather than null or absent", risk["touches"])
	}
	if len(touches) != 0 {
		t.Errorf("touches = %+v, want an empty array", touches)
	}
}

func TestSubmitForReview_RefusesEmptyIdentity(t *testing.T) {
	for _, tc := range []struct{ name, taskID, runID string }{
		{"no task", "", "r-1"},
		{"no run", "t-1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, got := serve(t, http.StatusOK, okBody)
			if err := mustClient(t, srv).SubmitForReview(context.Background(), tc.taskID, tc.runID, Submission{}); err == nil {
				t.Fatal("expected an error")
			}
			if len(got.bodies) != 0 {
				t.Errorf("sent %d request(s), want none: %+v", len(got.bodies), got.bodies)
			}
		})
	}
}
