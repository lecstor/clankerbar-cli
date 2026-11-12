package loop

import (
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// phaseShape is what the CAPTURED sequence says a phase is, computed once before
// anything is read or spawned. Every field is a pure function of the arguments to
// phaseShapeOf: no I/O, no live config.
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
	// captured is this phase as the sequence was captured at drain start, before
	// a handoff respawn replaced ph.Prompt with the successor's continuation: the
	// mode must survive a handoff exactly as the record requirement does.
	captured := config.Phase{}
	if phaseIdx < len(phases) {
		captured = phases[phaseIdx]
	}
	// Whether this phase is a review-SHAPED gate (CLA-637): its deliverable is
	// the durable review record on the task, and the seam below refuses it a
	// checkpoint without one. Read off the CAPTURED sequence, never the live
	// config (CLA-664) and never ph.Prompt (a handoff respawn replaces that
	// wholesale). The predicate is config.ReviewShaped, so every review-only
	// variant is covered — an operator's own custom prompt owns its own contract
	// and is not one. nextReviewShaped is the same question about the NEXT
	// configured phase: the driver closes a round only when the review stage is
	// over, so a later panel member (CLA-639) never sees an earlier member's
	// findings.
	nextReviewShaped := false
	if phaseIdx+1 < len(phases) {
		nextReviewShaped = config.ReviewShaped(phases[phaseIdx+1].Prompt)
	}
	requireReviewRecord := config.ReviewShaped(captured.Prompt)
	isVerify := config.VerifyShaped(captured.Prompt)
	// CLA-703: the no-MCP backstop is for a REVIEW-ONLY session — one whose
	// deliverable is the durable record and whose brief tells it to reach the
	// backlog. A verification round is review-shaped but adjudicates prior
	// findings and has its own fail-closed seam; it is deliberately out of
	// scope here.
	reviewOnly := requireReviewRecord && !isVerify
	// CLA-639: whether this pass is one reviewer of a review panel, and whether
	// it is the LAST one. The last member is the panel's seam: it runs the same
	// record gate as the shipped single review, and it must run it even when the
	// sequence ends there (a panel with no fix phase after it) — the round close
	// and the merged list are owed to whoever reads the records next.
	//
	// A panel member is a review-shaped phase the operator did not write one at
	// a time: EffectivePhases expanded one `review` into N. The distinction
	// matters because the members are skippable — a dead or capped member is
	// recorded as failed and the round still closes — while a lone review phase
	// is the sequence's only record and keeps the shipped fail-closed path.
	panelMember := captured.IsPanelMember()
	// CLA-784: an OPTIONAL member is a trial seat. Its end is recorded against
	// that seat and nothing more: it may not end the sequence, hand the panel's
	// shared claim back, or stop the run. The member handler below reads this
	// flag instead of the member's end shape, so every end the task names
	// (transient, usage limit, hard stop, death, tool-loop kill, setup error) is
	// covered by one branch rather than a list that can rot.
	panelOptional := captured.IsOptionalPanelMember()
	panelLast := false
	// A BRANCHLESS checkpoint runs exactly ONE reviewer: the record this shape
	// can write is the legacy `## Review` block, not a durable one, so
	// drainPhases skips the members after the first. That first member is the
	// panel's last member in every sense the seam cares about — its record is
	// the panel's only record and the "no member recorded → no fix" rule must
	// apply to it — so the position test alone (`no review-shaped phase follows`)
	// is not enough here.
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
