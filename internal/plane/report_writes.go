// report_writes.go — the writes a runner makes ABOUT a session's own records:
// the outcome it declared, the reviewer's durable record, the fixer's finding
// updates, and the hand-over. They share one interface because all four sign
// with the holding run.
package plane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrDuplicateReviewer means the plane refused an appended record because its
// round already carries one for that reviewer role. That refusal IS the
// idempotency signal - the finding ids are `<round>.<role>.<n>`, so a re-send
// would shadow the first record - and the caller treats it as written, never as
// a write to retry.
var ErrDuplicateReviewer = errors.New("the round already carries this reviewer's record")

// ReportWriter writes the records a session produces during its run.
//
// A separate interface from Releaser, like Recorder and PhaseMarker, so a plane
// (or test fake) that does not offer these tools simply lacks the capability:
// the runner type-asserts and reports the gap rather than calling a method
// nothing implements.
type ReportWriter interface {
	// RecordOutcome writes the outcome text onto the task, signed by the run.
	RecordOutcome(ctx context.Context, taskID, runID, outcome string) error

	// AppendReview appends one reviewer's durable record and returns the round
	// the plane filed it in, or 0 when the receipt carries no round.
	AppendReview(ctx context.Context, taskID, runID string, rec ReviewAppend) (round int, err error)

	// UpdateFindings answers findings a released round carries: fixed, refuted
	// or filed elsewhere.
	UpdateFindings(ctx context.Context, taskID, runID string, updates []FindingUpdate) error

	// SubmitForReview hands the task over for review, optionally closing the
	// round in the same atomic write.
	SubmitForReview(ctx context.Context, taskID, runID string, s Submission) error
}

var _ ReportWriter = (*mcpReleaser)(nil)

// ReviewAppend is one reviewer's record as the reviewer declares it. The scope
// fields say what was reviewed; ScopeKind empty omits the scope object.
type ReviewAppend struct {
	Role, Verdict, Harness, Model, Branch, ReviewedTip string
	ScopeKind, ScopeBase, ScopeRange                   string
	Findings                                           []ReviewFinding
}

// ReviewFinding is one graded finding. Severity and Summary are the members the
// plane's schema requires; the rest are optional and omitted when empty.
type ReviewFinding struct {
	Severity, Summary, EvidenceKind, Evidence, File string
	Line                                            int
	Kind, RequiredFix, RaisedByModel                string
}

// FindingUpdate answers one finding by its plane-assigned id. Every member
// after FindingID is optional and omitted when empty.
type FindingUpdate struct {
	FindingID, Status, FixCommit, RefutedReason, FiledTaskRef string
}

// Submission is a hand-over. RiskDeclared distinguishes an UNDECLARED risk
// (omitted, which keeps a per-task human verdict) from a declaration that
// touches nothing (Touches nil or empty, sent as `[]`).
type Submission struct {
	Outcome      string // omitted when empty
	RiskDeclared bool   // send risk: {touches: Touches} only when true
	Touches      []string
	CloseRound   int // omitted when 0
}

// RecordOutcome records the run's outcome text. It is the standalone record a
// later reader works from, so an empty one is refused here rather than stored.
func (r *mcpReleaser) RecordOutcome(ctx context.Context, taskID, runID, outcome string) error {
	if taskID == "" || runID == "" {
		return errors.New("record outcome: taskId and runId are both required")
	}
	if outcome == "" {
		return errors.New("record outcome: refusing to record an empty outcome")
	}
	return r.call(ctx, "record_progress", map[string]any{
		"taskId":  taskID,
		"runId":   runID,
		"outcome": outcome,
	})
}

// AppendReview appends one reviewer's durable record through record_review and
// reports the round it landed in. The verdict is one of the plane's two words,
// so anything else is refused before it costs a round-trip; a duplicate
// reviewer refusal is typed as ErrDuplicateReviewer, which means the record is
// already there.
func (r *mcpReleaser) AppendReview(ctx context.Context, taskID, runID string, rec ReviewAppend) (int, error) {
	if taskID == "" || runID == "" {
		return 0, errors.New("append review: taskId and runId are both required")
	}
	if rec.Role == "" {
		return 0, errors.New("append review: reviewerRole is required")
	}
	if rec.Verdict != "pass" && rec.Verdict != "findings" {
		return 0, fmt.Errorf("append review: verdict %q must be pass or findings", rec.Verdict)
	}
	raw, err := r.callText(ctx, "record_review", reviewArgs(taskID, runID, rec))
	if err != nil {
		return 0, classifyReviewRefusal(err)
	}
	return reviewRound(raw), nil
}

// reviewArgs shapes one append. Only the two members the plane requires are
// unconditional; every other field is omitted when empty, because an empty
// value is an absent one to the plane's own normalisation.
func reviewArgs(taskID, runID string, rec ReviewAppend) map[string]any {
	args := map[string]any{
		"taskId":       taskID,
		"runId":        runID,
		"reviewerRole": rec.Role,
		"verdict":      rec.Verdict,
	}
	for key, value := range map[string]string{
		"harness":     rec.Harness,
		"model":       rec.Model,
		"branch":      rec.Branch,
		"reviewedTip": rec.ReviewedTip,
	} {
		if value != "" {
			args[key] = value
		}
	}
	if rec.ScopeKind != "" {
		scope := map[string]any{"kind": rec.ScopeKind}
		if rec.ScopeBase != "" {
			scope["base"] = rec.ScopeBase
		}
		if rec.ScopeRange != "" {
			scope["range"] = rec.ScopeRange
		}
		args["scope"] = scope
	}
	if len(rec.Findings) > 0 {
		args["findings"] = reviewFindingArgs(rec.Findings)
	}
	return args
}

// reviewFindingArgs shapes the findings. severity and summary are the two keys
// the plane's schema requires, so they go out even when blank - the plane is
// the one that refuses a blank one, with a code the caller can read.
func reviewFindingArgs(findings []ReviewFinding) []map[string]any {
	out := make([]map[string]any, 0, len(findings))
	for _, f := range findings {
		finding := map[string]any{
			"severity": f.Severity,
			"summary":  f.Summary,
		}
		for key, value := range map[string]string{
			"evidenceKind":  f.EvidenceKind,
			"evidence":      f.Evidence,
			"file":          f.File,
			"kind":          f.Kind,
			"requiredFix":   f.RequiredFix,
			"raisedByModel": f.RaisedByModel,
		} {
			if value != "" {
				finding[key] = value
			}
		}
		if f.Line != 0 {
			finding["line"] = f.Line
		}
		out = append(out, finding)
	}
	return out
}

// reviewRound reads the appended record's round out of record_review's receipt
// (`{"review":{"record":{"round":n}}}`). A receipt with no record is the
// findingUpdates or closeRound spelling, and an unreadable one is still a write
// that landed: both are round 0, because the call itself succeeded.
func reviewRound(raw []byte) int {
	var receipt struct {
		Review struct {
			Record struct {
				Round any `json:"round"`
			} `json:"record"`
		} `json:"review"`
	}
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return 0
	}
	return wireInt(receipt.Review.Record.Round)
}

// classifyReviewRefusal types the one review refusal the caller acts on
// differently: a duplicate reviewer means the record is already written. Codes
// this build does not name pass through untouched.
func classifyReviewRefusal(err error) error {
	code, message, ok := refusalCode(err)
	if !ok || code != "duplicate_reviewer" {
		return err
	}
	return fmt.Errorf("%w: %s", ErrDuplicateReviewer, refusalText(code, message))
}

// UpdateFindings answers the findings a released round carries. An empty list
// is not a write: the caller either had nothing to answer or already did, and
// the plane would refuse an empty array outright.
func (r *mcpReleaser) UpdateFindings(ctx context.Context, taskID, runID string, updates []FindingUpdate) error {
	if taskID == "" || runID == "" {
		return errors.New("update findings: taskId and runId are both required")
	}
	if len(updates) == 0 {
		return nil
	}
	return r.call(ctx, "record_review", map[string]any{
		"taskId":         taskID,
		"runId":          runID,
		"findingUpdates": findingUpdateArgs(updates),
	})
}

// findingUpdateArgs shapes the answers. findingId is the key the plane looks up;
// the rest are omitted when empty so an unchanged member is not overwritten.
func findingUpdateArgs(updates []FindingUpdate) []map[string]any {
	out := make([]map[string]any, 0, len(updates))
	for _, u := range updates {
		update := map[string]any{"findingId": u.FindingID}
		for key, value := range map[string]string{
			"status":        u.Status,
			"fixCommit":     u.FixCommit,
			"refutedReason": u.RefutedReason,
			"filedTaskRef":  u.FiledTaskRef,
		} {
			if value != "" {
				update[key] = value
			}
		}
		out = append(out, update)
	}
	return out
}

// SubmitForReview hands the task over. An undeclared risk is an ABSENT risk
// argument - that is what keeps a per-task human verdict - while a declared one
// that touches nothing must be an empty array, never a null or a missing list,
// which would read as undeclared.
func (r *mcpReleaser) SubmitForReview(ctx context.Context, taskID, runID string, s Submission) error {
	if taskID == "" || runID == "" {
		return errors.New("submit for review: taskId and runId are both required")
	}
	args := map[string]any{
		"taskId": taskID,
		"runId":  runID,
	}
	if s.Outcome != "" {
		args["outcome"] = s.Outcome
	}
	if s.CloseRound != 0 {
		args["closeRound"] = s.CloseRound
	}
	if s.RiskDeclared {
		touches := s.Touches
		if touches == nil {
			touches = []string{}
		}
		args["risk"] = map[string]any{"touches": touches}
	}
	return r.call(ctx, "submit_for_review", args)
}
