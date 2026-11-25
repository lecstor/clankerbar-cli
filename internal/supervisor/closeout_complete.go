package supervisor

// The writes that finish a merged close-out (CLA-969, CLA-1030).
//
// A merge that landed is the one point of no return in this path: the PR is no
// longer OPEN, so releasing the claim would return the task to approved with
// nothing for either close-out path to find. Everything here exists to keep that
// from happening — the bounded retry of the final close, the ONE evidence-bearing
// retry the plane's `evidence_required` refusal earns, and the recovery record
// that names the merge SHA and PR when even those cannot finish the job.
//
// The evidence retry is deliberately narrow. It fires only on that refusal code,
// only once, and only with facts the close-out itself verified: the delivery check
// it gated the merge on, and the merge commit it read back from the remote. It
// keeps the stored outcome's text and appends the section; it never rewrites what
// a reviewer approved, and it never invents a result for a check it did not see.

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/delivery"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// closeOutCompleteAttempts bounds the retries of the final close after a merge
// landed. It is the one write whose failure must not fall back to a release: the
// PR is gone from the open list, so a release strands the task. Four attempts
// spend 1.75s of backoff inside the single cleanup timeout.
const closeOutCompleteAttempts = 4

// closeOutCompleteBackoff is the wait before the second close attempt; it doubles
// each retry. A var so a test can shrink it rather than wait the real bound out,
// the same discipline closeOutBaseProbeTimeout keeps.
var closeOutCompleteBackoff = 250 * time.Millisecond

// retryCloseOutCall runs one plane write, retrying a TRANSIENT plane failure
// (transport or 5xx) within ctx. A tool-level refusal is the plane's considered
// answer and is returned at once: retrying it would only repeat it. what names the
// write for the retry line.
func retryCloseOutCall(ctx context.Context, what string, fn func() error) error {
	wait := closeOutCompleteBackoff
	var err error
	for attempt := 1; attempt <= closeOutCompleteAttempts; attempt++ {
		if err = fn(); err == nil {
			return nil
		}
		if !plane.Transient(err) || attempt == closeOutCompleteAttempts {
			return err
		}
		log.Printf("close-out: %s failed transiently (attempt %d of %d: %v) - retrying",
			what, attempt, closeOutCompleteAttempts, err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
		wait *= 2
	}
	return err
}

// completeCloseOut closes the task done, retrying a TRANSIENT plane failure
// (transport or 5xx) within one cleanup budget. A tool-level refusal is the
// plane's answer and is returned at once, with one exception: the close-out's
// own evidence refusal (evidence_required) earns the single retry that asserts
// the checks this pass verified. It reports whether that retry was attempted.
func (d *Supervisor) completeCloseOut(client plane.CloseOutAPI, label, key, taskID, runID string, decl plane.CloseOutDelivery, gated *closeOutGatedCheck) (bool, error) {
	ctx, cancel := d.closeOutCleanupCtx()
	defer cancel()
	what := fmt.Sprintf("%s PR %s merged as %s, but closing the task", label, decl.PR, decl.Commit)
	err := retryCloseOutCall(ctx, what, func() error {
		return client.CompleteCloseOut(ctx, taskID, runID, decl)
	})
	if err == nil || !plane.CloseOutEvidenceRefused(err) {
		return false, err
	}
	return d.retryCloseOutWithEvidence(ctx, client, label, key, taskID, runID, decl, gated, err)
}

// recordMergedDelivery is the recovery write for a merge that landed but could
// not be closed. The claim is LEFT HELD on purpose: releasing it would return a
// merged task to approved with no open PR, and the lease expiry is the safe
// hand-back - it offers the task as a takeover, and this record tells the
// successor the merge SHA and PR are already merged.
//
// It gets the same bounded retry as the close: a plane that has just answered 502
// to every close attempt will often fail this write too, and this record is the
// only durable place the merge SHA and PR are written down.
func (d *Supervisor) recordMergedDelivery(client plane.CloseOutAPI, label, key, taskID, runID string, decl plane.CloseOutDelivery, closeErr error, retried bool) {
	ctx, cancel := d.closeOutCleanupCtx()
	what := fmt.Sprintf("%s PR %s merged as %s, but recording the delivery", label, decl.PR, decl.Commit)
	recErr := retryCloseOutCall(ctx, what, func() error {
		return client.RecordCloseOutDelivery(ctx, taskID, runID, decl)
	})
	cancel()
	retryNote := ""
	if retried {
		retryNote = "; the evidence_required retry was attempted and refused too"
	}
	if recErr == nil {
		d.closeOutSay(key, "close-out: %s PR %s merged as %s into %s, but closing the task failed (%v)%s - delivery RECORDED (mergeVerified:%t) so a successor can close it; the claim is left held rather than returning a merged PR to approved",
			label, decl.PR, decl.Commit, decl.IntegrationBranch, closeErr, retryNote, decl.MergeVerified)
		return
	}
	d.closeOutSay(key, "close-out: %s PR %s merged as %s into %s, but closing the task failed (%v)%s AND the delivery could not be recorded (%v) - the claim is left held until its lease expires; a successor must close the task from that merge commit and PR (if a close attempt was processed and its answer was lost, the task may already be closed)",
		label, decl.PR, decl.Commit, decl.IntegrationBranch, closeErr, retryNote, recErr)
}

// closeOutGatedCheck is the check the close-out gated its merge on, in the terms
// the evidence section and the ci attestation both cite: the driver's own check
// (name and conclusion), the PR it was read from, and the head revision it ran
// against.
type closeOutGatedCheck struct {
	Check      string
	Conclusion string
	Branch     string
	PR         string
	HeadOID    string
}

// closeOutGatedCheckFor builds the citation a merged close's evidence may carry.
// It reports false when the driver has no gated check result to stand behind — a
// verdict that is not a pass, a check with no name, no PR number, or no judged
// head revision — and the retry must then be skipped rather than padded with
// words.
func closeOutGatedCheckFor(state delivery.PRState) (closeOutGatedCheck, bool) {
	head := strings.TrimSpace(state.HeadOID)
	if state.Check.Status != delivery.Pass || state.Check.Kind == "" || state.Number <= 0 || head == "" {
		return closeOutGatedCheck{}, false
	}
	return closeOutGatedCheck{
		Check:      string(state.Check.Kind),
		Conclusion: string(state.Check.Status),
		Branch:     strings.TrimSpace(state.Head),
		PR:         fmt.Sprintf("#%d", state.Number),
		HeadOID:    head,
	}, true
}

// retryCloseOutWithEvidence is the ONE retry a complete_task refusal with
// `evidence_required` earns. It reads the stored outcome, keeps its text, and
// appends a `**Tests**` section built only from the gated check and the merge
// the driver verified. It reports whether the retry was attempted, so the
// close-out line can say so.
func (d *Supervisor) retryCloseOutWithEvidence(ctx context.Context, client plane.CloseOutAPI, label, key, taskID, runID string, decl plane.CloseOutDelivery, gated *closeOutGatedCheck, refusal error) (bool, error) {
	if gated == nil {
		d.closeOutSay(key, "close-out: %s PR %s merged as %s, but the plane refused the close with evidence_required and this close-out verified no check it could cite - not retrying", label, decl.PR, decl.Commit)
		return false, refusal
	}
	stored, err := client.TaskOutcome(ctx, taskID)
	if err != nil {
		d.closeOutSay(key, "close-out: %s PR %s merged as %s, but the plane refused the close with evidence_required and the stored outcome could not be read (%v) - not retrying", label, decl.PR, decl.Commit, err)
		return false, refusal
	}
	// An empty outcome leaves nothing to keep: appending the section alone would
	// replace the record this retry exists to preserve, whether the plane served
	// no body or the task genuinely carries no outcome.
	if strings.TrimSpace(stored) == "" {
		d.closeOutSay(key, "close-out: %s PR %s merged as %s, but the plane refused the close with evidence_required and the stored outcome is empty - not retrying, since there is no reviewed baseline to keep", label, decl.PR, decl.Commit)
		return false, refusal
	}
	// The plane refuses an outcome with no **Tests** section; if the stored one
	// already has one, this refusal is a different problem and repeating the call
	// with more words under the same heading cannot settle it.
	if config.HasTestsSection(stored) {
		d.closeOutSay(key, "close-out: %s PR %s merged as %s, but the plane refused the close with evidence_required although the stored outcome already carries a **Tests** section - not retrying", label, decl.PR, decl.Commit)
		return false, refusal
	}

	evidence := plane.CloseOutEvidence{
		Outcome:    config.AppendOutcome(stored, closeOutTestsSection(*gated, decl)),
		Check:      gated.Check,
		Conclusion: gated.Conclusion,
		Branch:     gated.Branch,
		PR:         gated.PR,
	}
	d.closeOutSay(key, "close-out: %s PR %s merged as %s, but the plane refused the close with evidence_required - retrying once with the checks this close-out verified (%s, PR %s, head %s)", label, decl.PR, decl.Commit, gated.Check, gated.PR, shortSHA(gated.HeadOID))
	what := fmt.Sprintf("%s PR %s merged as %s, but closing the task with the driver's verified checks", label, decl.PR, decl.Commit)
	err = retryCloseOutCall(ctx, what, func() error {
		return client.CompleteCloseOutWithEvidence(ctx, taskID, runID, decl, evidence)
	})
	if err != nil {
		d.closeOutSay(key, "close-out: %s PR %s merged as %s, but the evidence retry was refused too (%v)", label, decl.PR, decl.Commit, err)
		return true, err
	}
	return true, nil
}

// closeOutTestsSection renders the `**Tests**` evidence the deterministic
// close-out can actually stand behind. Every line is a fact it read itself: the
// check it gated the merge on, and the merge commit with its ancestry verdict
// from the same delivery the call sends. Nothing else belongs here — a result
// this path did not see is not evidence.
func closeOutTestsSection(gated closeOutGatedCheck, decl plane.CloseOutDelivery) string {
	var b strings.Builder
	b.WriteString(config.TestsHeading)
	b.WriteString("\n")
	fmt.Fprintf(&b, "- %s (PR %s, head %s): %s — the delivery check this merge was gated on\n",
		gated.Check, gated.PR, gated.HeadOID, gated.Conclusion)
	if decl.MergeVerified {
		fmt.Fprintf(&b, "- merge %s into %s: verified as an ancestor of the remote branch (mergeVerified: true)\n",
			decl.Commit, decl.IntegrationBranch)
	} else {
		fmt.Fprintf(&b, "- merge %s into %s: the merge landed but the ancestor check did not settle (mergeVerified: false)\n",
			decl.Commit, decl.IntegrationBranch)
	}
	return b.String()
}
