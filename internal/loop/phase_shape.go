package loop

import (
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// phaseShape is what the CAPTURED sequence says a phase is, computed once before anything is read
// or spawned. Every field is a pure function of the arguments to phaseShapeOf: no I/O, no live
// config.
type phaseShape struct {
	captured            config.Phase
	nextReviewShaped    bool
	requireReviewRecord bool
	isVerify            bool
	reviewOnly          bool
	panelMember         bool
	panelOptional       bool
	panelLast           bool
}

func phaseShapeOf(phases []config.Phase, phaseIdx int, prev *harness.Result) phaseShape {
	// captured is this phase as the sequence was captured at drain start, before a handoff respawn
	// replaced ph.Prompt with the successor's continuation: the mode must survive a handoff exactly as
	// the record requirement does.
	captured := config.Phase{}
	if phaseIdx < len(phases) {
		captured = phases[phaseIdx]
	}
	// Whether this phase is a review-SHAPED gate: its deliverable is the durable review record on the
	// task, and the seam below refuses it a checkpoint without one.
	nextReviewShaped := false
	if phaseIdx+1 < len(phases) {
		nextReviewShaped = config.ReviewShaped(phases[phaseIdx+1].Prompt)
	}
	requireReviewRecord := config.ReviewShaped(captured.Prompt)
	isVerify := config.VerifyShaped(captured.Prompt)
	// CLA-703: the no-MCP backstop is for a REVIEW-ONLY session — one whose deliverable is the
	// durable record and whose brief tells it to reach the backlog.
	reviewOnly := requireReviewRecord && !isVerify
	// CLA-639: whether this pass is one reviewer of a review panel, and whether it is the LAST one.
	panelMember := captured.IsPanelMember()
	// CLA-784: an OPTIONAL member is a trial seat. Its end is recorded against that seat and nothing
	// more: it may not end the sequence, hand the panel's shared claim back, or stop the run.
	panelOptional := captured.IsOptionalPanelMember()
	panelLast := false
	// A BRANCHLESS checkpoint runs exactly ONE reviewer: the record this shape can write is the legacy
	// `## Review` block, not a durable one, so drainPhases skips the members after the first.
	branchlessPanel := panelMember && prev != nil && prev.Claim.Branch == ""
	if panelMember && (!nextReviewShaped || branchlessPanel) {
		panelLast = true
	}
	return phaseShape{
		captured:            captured,
		nextReviewShaped:    nextReviewShaped,
		requireReviewRecord: requireReviewRecord,
		isVerify:            isVerify,
		reviewOnly:          reviewOnly,
		panelMember:         panelMember,
		panelOptional:       panelOptional,
		panelLast:           panelLast,
	}
}
