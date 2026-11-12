package loop

import (
	"errors"
	"fmt"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// reportCheck is the check an adapter runs on a runner-claimed session's final
// message before the runner acts on it (harness.Invocation.CheckReport), so a
// session whose report would be refused can be steered while it is still alive.
// nil means the phase owes no report at all.
func (r *phaseRun) reportCheck() func(finalMessage string) error {
	if r.kind == reportNone {
		return nil
	}
	return func(finalMessage string) error {
		// A handoff defers the report to its successor, and a resume would replace
		// the final message that carries it. Verify rounds never chain, so theirs
		// still owes the report.
		if _, found, refusal := parseHandoff(finalMessage); found && refusal == "" && r.kind != reportVerify {
			return nil
		}
		rep, found, err := config.ParseDone(finalMessage)
		switch {
		case err != nil:
			return fmt.Errorf("the done report could not be parsed: %w", err)
		case !found:
			return errors.New("the final message has no done report: end it with a line that is exactly " +
				config.DoneMarker + " followed by the JSON object")
		}
		return r.checkDoneReport(rep)
	}
}

// checkDoneReport applies the kind's PURE validation: exactly what its apply*
// function checks before its first plane read, and no more. Anything needing the
// task or the plane is answered later, by the apply* function itself.
func (r *phaseRun) checkDoneReport(rep config.DoneReport) error {
	switch r.kind {
	case reportReview, reportVerify:
		return r.checkReviewRecord(rep)
	case reportFixVerify, reportFixLast:
		return r.checkFixReport(rep)
	}
	return nil
}

// checkReviewRecord is the review-shaped record check applyReviewReport and
// applyVerifyReport run before their first write; both validate against the tip
// the brief handed this session.
func (r *phaseRun) checkReviewRecord(rep config.DoneReport) error {
	return rep.ValidateReview(r.checkpointTip())
}

// checkFixReport is the pure part of a fix report: a park replaces every other
// record, so it carries no batch to validate, exactly as applyFixReport and
// applyFixLastReport return on fixParked before touching findingUpdates.
func (r *phaseRun) checkFixReport(rep config.DoneReport) error {
	if strings.TrimSpace(rep.Park) != "" {
		return nil
	}
	return config.ValidateFindingUpdates(rep.FindingUpdates)
}
