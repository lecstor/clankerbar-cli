package loop

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/delivery"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// A runner-claimed session makes NO plane writes: it ends its final message with
// a done report and the runner turns that report into the task's records here.
// See docs/runner-owned-mechanics.md ("B8 design") for what each phase writes.

// reportKind is which done report a phase expects. It is decided from the runner
// brief the phase runs, so a phase whose session still owns its writes (a
// close-out, a panel member, a custom prompt) reports nothing at all.
type reportKind int

const (
	reportNone reportKind = iota
	reportImplement
	reportReview
	reportFixVerify
	reportFixLast
	reportVerify
)

// reportKindFor decides the kind from the CAPTURED phase's prompt, never the
// live one: a handoff respawn replaces ph.Prompt with the predecessor's
// successor text, and the successor still owes the same report its phase does.
// The checkpoint variant is resolved first, exactly as invocationFor does, so a
// branchless or unverified phase - which still runs a shipped brief that writes
// its own records - expects no report.
func reportKindFor(phases []config.Phase, phaseIdx int, rb *runnerBriefing, panelMember bool, prev *harness.Result) reportKind {
	if phaseIdx < 0 || phaseIdx >= len(phases) {
		return reportNone
	}
	return reportKindOf(resolvedRunnerBrief(phases[phaseIdx].Prompt, rb, panelMember, prev))
}

// resolvedRunnerBrief is the brief invocationFor gives a phase: the checkpoint
// variant of its captured prompt, then the runner swap, both only for a resumed
// (held) claim, exactly as invocationFor applies them.
func resolvedRunnerBrief(captured string, rb *runnerBriefing, panelMember bool, prev *harness.Result) string {
	if rb == nil || panelMember || prev == nil || !prev.Claim.Held() {
		return ""
	}
	return runnerPrompt(variantPrompt(captured, prev, panelMember), rb, panelMember)
}

// reportKindOf maps a resolved runner brief to the report it asks for.
func reportKindOf(brief string) reportKind {
	switch brief {
	case config.RunnerImplementBrief():
		return reportImplement
	case config.RunnerReviewOnlyBrief():
		return reportReview
	case config.RunnerFixBrief():
		return reportFixLast
	case config.RunnerFixVerifyBrief():
		return reportFixVerify
	case config.RunnerVerifyBrief():
		return reportVerify
	}
	return reportNone
}

// doneAttribution resolves who a done report is attributed to: this phase's
// harness and model, the implementer's for a phase that judges its work, and the
// reviewer role a review record is filed under. It is the SAME resolution
// invocationFor fills the attribution placeholders with, so the record the
// runner writes names exactly what the session's brief did.
func (d *Driver) doneAttribution(t Target, ph config.Phase, phases []config.Phase, phaseIdx int, inv harness.Invocation) (config.Attribution, string) {
	tc := d.targetCfg(t)
	attr := config.Attribution{Harness: tc.HarnessFor(ph), Model: inv.Model}
	prevIdx := phaseIdx - 1
	// CLA-639: inside a review panel every member's predecessor is the member
	// before it, but the review briefs name {{prevHarness}}/{{prevModel}} as the
	// IMPLEMENTER whose work is being judged. Walk back past the panel to the
	// last non-review phase, so member 2 does not accuse member 1 of having
	// implemented the branch.
	for prevIdx >= 0 && config.ReviewShaped(phases[prevIdx].Prompt) && phases[prevIdx].IsPanelMember() {
		prevIdx--
	}
	if prevIdx >= 0 && prevIdx < len(phases) {
		attr.PrevHarness = tc.HarnessFor(phases[prevIdx])
		attr.PrevModel, _ = tc.ModelForPhase(phases[prevIdx])
	}
	return attr, reviewerRoleFor(phases, phaseIdx)
}

// applyDoneReport turns the session's done report into the task's records. It
// runs after the handoff classification — a session that handed off owes its
// successor's report, not its own — and before the evidence gate, because that
// gate judges what this has just written.
func (r *phaseRun) applyDoneReport(ctx context.Context, at *phaseAttempt) {
	if r.kind == reportNone || at.runLost != nil {
		return
	}
	// The implement branch is a fact about origin, not part of the report: it is
	// recorded on every trusted end, so a handoff the driver later refuses, or a
	// session that died after pushing, still leaves the WIP a takeover resumes
	// instead of releasing the task to ready over pushed work.
	if r.kind == reportImplement && at.res.Untrusted == "" && at.res.Claim.Held() {
		r.recordDoneBranch(ctx, at)
	}
	// Only a handoff the driver will honour defers the report to its successor. A
	// refused or cleared one (every verify round clears it) has no successor, so
	// a report the session also wrote is the phase's only record.
	if at.handoff != "" {
		return
	}
	w, ok := r.t.Releaser.(plane.ReportWriter)
	if !ok {
		if !r.reportWriterNoted {
			r.reportWriterNoted = true
			log.Printf("%siteration %d: the %s phase runs a runner brief, but the plane client cannot write records (no record_progress/record_review write surface) — nothing is written from its done report",
				labelOf(r.t), r.drainNum, r.phaseLabel())
		}
		return
	}
	if at.res.Untrusted != "" {
		r.noteReportProblem("the stream could not be read whole (%s), so its done report is not trusted", at.res.Untrusted)
		return
	}
	if !at.orderlyExit && !at.recoveredExit {
		r.noteReportProblem("the session ended %s, which is not an orderly end, so its done report is not applied", at.res.ExitString())
		return
	}
	if !at.res.Claim.Held() {
		r.noteReportProblem("the session's claim is not held (settled, or never observed), so the report cannot be written under it")
		return
	}
	rep, found, err := config.ParseDone(at.res.FinalMessage)
	switch {
	case err != nil:
		r.noteReportProblem("its done report could not be used: %v", err)
		return
	case !found:
		r.noteReportProblem("the session ended without a done report, so the runner wrote no record for the %s phase", r.phaseLabel())
		return
	}
	switch r.kind {
	case reportImplement:
		r.applyImplementReport(ctx, w, at, rep)
	case reportReview:
		r.applyReviewReport(ctx, w, at, rep)
	case reportFixVerify:
		r.applyFixReport(ctx, w, at, rep)
	case reportFixLast:
		r.applyFixLastReport(ctx, w, at, rep)
	case reportVerify:
		r.applyVerifyReport(ctx, w, at, rep)
	}
}

// applyImplementReport writes an implement phase's delivery: the branch the
// session pushed, then the outcome section its report becomes.
func (r *phaseRun) applyImplementReport(ctx context.Context, w plane.ReportWriter, at *phaseAttempt, rep config.DoneReport) {
	st, ok := r.doneTask(ctx, at, "for the implement phase's done report")
	if !ok {
		return
	}
	// A park replaces settle_task: the session re-read the task, found its work
	// superseded, and the park's reason is the whole record.
	if park := strings.TrimSpace(rep.Park); park != "" {
		r.parkReported(ctx, at, st, park)
		return
	}
	if !r.recordDoneBranch(ctx, at) {
		return
	}
	outcome := config.AppendOutcome(st.Outcome, config.RenderDeliverySection(at.attr, rep))
	if outcome == st.Outcome {
		return
	}
	if err := w.RecordOutcome(ctx, at.res.Claim.TaskID, at.res.Claim.RunID, outcome); err != nil {
		r.noteReportWrite("recording the delivery outcome", err)
		return
	}
	log.Printf("%siteration %d: the %s phase: recorded the session's delivery on %s",
		labelOf(r.t), r.drainNum, r.phaseLabel(), claimLabel(at.res.Claim))
}

// applyReviewReport writes a review phase's record: the legacy `## Review` block
// ONTO the outcome first, then the durable record. The order is load-bearing —
// the plane refuses the run's outcome write once its review record is sealed.
func (r *phaseRun) applyReviewReport(ctx context.Context, w plane.ReportWriter, at *phaseAttempt, rep config.DoneReport) {
	tip := r.checkpointTip()
	if err := r.checkReviewRecord(rep); err != nil {
		r.noteReportProblem("its done report is not a valid review record: %v", err)
		return
	}
	st, ok := r.doneTask(ctx, at, "to check the review record its done report describes")
	if !ok {
		return
	}
	// Idempotent on the plane's own terms: a record this run already wrote under
	// this role at this tip IS the record, so a retried attempt writes nothing.
	if rec, matched := matchingReviewRecord(st.Reviews, at.role, at.res.Claim.RunID, tip, r.requireReviewTip()); matched {
		log.Printf("%siteration %d: the %s phase: the task already carries this run's %s record (round %d) — nothing to write",
			labelOf(r.t), r.drainNum, r.phaseLabel(), at.role, rec.Round)
		return
	}
	outcome := config.AppendOutcome(st.Outcome, config.RenderReviewBlock(at.attr, rep))
	if outcome != st.Outcome {
		if err := w.RecordOutcome(ctx, at.res.Claim.TaskID, at.res.Claim.RunID, outcome); err != nil {
			r.noteReportWrite("recording the review block on the outcome", err)
			return
		}
	}
	rec := plane.ReviewAppend{
		Role:        at.role,
		Verdict:     rep.Verdict,
		Harness:     at.attr.Harness,
		Model:       at.attr.Model,
		Branch:      r.doneBranch(at),
		ReviewedTip: rep.ReviewedTip,
		ScopeKind:   "full-diff",
		ScopeBase:   r.prBase,
		ScopeRange:  r.prBase + ".." + rep.ReviewedTip,
		Findings:    doneFindings(rep, at.attr.Model),
	}
	round, err := w.AppendReview(ctx, at.res.Claim.TaskID, at.res.Claim.RunID, rec)
	switch {
	case errors.Is(err, plane.ErrDuplicateReviewer):
		// The plane's idempotency signal: the record is already there.
		log.Printf("%siteration %d: the %s phase: the plane already carries the %s record — treating the append as written",
			labelOf(r.t), r.drainNum, r.phaseLabel(), at.role)
	case err != nil:
		r.noteReportWrite("appending the review record", err)
	default:
		log.Printf("%siteration %d: the %s phase: appended the %s record (verdict %s, round %d) to %s",
			labelOf(r.t), r.drainNum, r.phaseLabel(), at.role, rep.Verdict, round, claimLabel(at.res.Claim))
	}
}

// applyFixReport writes a fix phase's finding answers, then the outcome section
// its report becomes. It is the fix that a verification round follows.
func (r *phaseRun) applyFixReport(ctx context.Context, w plane.ReportWriter, at *phaseAttempt, rep config.DoneReport) {
	if r.fixParked(ctx, at, rep) {
		return
	}
	if err := r.checkFixReport(rep); err != nil {
		r.noteReportProblem("its done report carries finding updates the plane would refuse: %v", err)
		return
	}
	if err := w.UpdateFindings(ctx, at.res.Claim.TaskID, at.res.Claim.RunID, doneFindingUpdates(rep)); err != nil {
		r.noteReportWrite("answering the round's findings", err)
		return
	}
	st, ok := r.doneTask(ctx, at, "for the fix phase's done report")
	if !ok {
		return
	}
	outcome := config.AppendOutcome(st.Outcome, config.RenderFixesSection(at.attr, rep))
	if outcome == st.Outcome {
		return
	}
	if err := w.RecordOutcome(ctx, at.res.Claim.TaskID, at.res.Claim.RunID, outcome); err != nil {
		r.noteReportWrite("recording the fixes outcome", err)
		return
	}
	log.Printf("%siteration %d: the %s phase: recorded the session's fixes on %s",
		labelOf(r.t), r.drainNum, r.phaseLabel(), claimLabel(at.res.Claim))
}

// applyFixLastReport is the LAST fix: the same finding answers, then the
// hand-over the session used to make itself.
func (r *phaseRun) applyFixLastReport(ctx context.Context, w plane.ReportWriter, at *phaseAttempt, rep config.DoneReport) {
	if r.fixParked(ctx, at, rep) {
		return
	}
	if err := r.checkFixReport(rep); err != nil {
		r.noteReportProblem("its done report carries finding updates the plane would refuse: %v", err)
		return
	}
	if err := w.UpdateFindings(ctx, at.res.Claim.TaskID, at.res.Claim.RunID, doneFindingUpdates(rep)); err != nil {
		r.noteReportWrite("answering the round's findings", err)
		return
	}
	st, ok := r.doneTask(ctx, at, "for the fix phase's done report")
	if !ok {
		return
	}
	// The hand-over names a branch the next clanker can actually fetch, so origin
	// must have it before the task leaves this run's hands.
	branch := r.doneBranch(at)
	if onOrigin, why := r.branchOnOrigin(ctx, at, branch); !onOrigin {
		r.noteReportProblem("the task branch %q is not verified on the origin remote (%s), so the task was not handed over", branch, why)
		return
	}
	// A takeover start may have the branch on origin but not on the task, and
	// the reviewer reads an empty branch field as a failed hand-off.
	if !r.recordVerifiedBranch(ctx, at, branch) {
		return
	}
	sub := plane.Submission{
		Outcome:      config.AppendOutcome(st.Outcome, config.RenderFixesSection(at.attr, rep)),
		RiskDeclared: rep.RiskDeclared(),
		Touches:      riskTouches(rep),
	}
	if err := w.SubmitForReview(ctx, at.res.Claim.TaskID, at.res.Claim.RunID, sub); err != nil {
		// Not folded, not stopped: the claim stays live and the ordinary handback
		// gives the task back with its branch recorded.
		r.noteReportWrite("handing the task over for review", err)
		return
	}
	at.res.Claim.Settled = true
	log.Printf("%siteration %d: the %s phase: handed %s over for review",
		labelOf(r.t), r.drainNum, r.phaseLabel(), claimLabel(at.res.Claim))
}

// applyVerifyReport writes a verification round's record: the finding answers,
// the review block, then the durable record scoped to the fix delta. The record
// is kept for the hand-over judgeVerifyRound performs once the round is closed.
func (r *phaseRun) applyVerifyReport(ctx context.Context, w plane.ReportWriter, at *phaseAttempt, rep config.DoneReport) {
	if err := r.checkReviewRecord(rep); err != nil {
		r.noteReportProblem("its done report is not a valid verification record: %v", err)
		return
	}
	if err := w.UpdateFindings(ctx, at.res.Claim.TaskID, at.res.Claim.RunID, doneFindingUpdates(rep)); err != nil {
		r.noteReportWrite("answering the findings this round adjudicates", err)
		return
	}
	st, ok := r.doneTask(ctx, at, "for the verification round's done report")
	if !ok {
		return
	}
	outcome := config.AppendOutcome(st.Outcome, config.RenderReviewBlock(at.attr, rep))
	if outcome != st.Outcome {
		if err := w.RecordOutcome(ctx, at.res.Claim.TaskID, at.res.Claim.RunID, outcome); err != nil {
			r.noteReportWrite("recording the verification block on the outcome", err)
			return
		}
	}
	rec := plane.ReviewAppend{
		Role:        at.role,
		Verdict:     rep.Verdict,
		Harness:     at.attr.Harness,
		Model:       at.attr.Model,
		Branch:      r.doneBranch(at),
		ReviewedTip: rep.ReviewedTip,
		ScopeKind:   "fix-delta",
		Findings:    doneFindings(rep, at.attr.Model),
	}
	// What this round verified is the delta on top of the newest record the
	// plane has already released; with none to name, the scope is the kind alone.
	if base := newestReleasedReviewTip(st.Reviews); base != "" {
		rec.ScopeBase = base
		rec.ScopeRange = base + ".." + rep.ReviewedTip
	}
	if _, err := w.AppendReview(ctx, at.res.Claim.TaskID, at.res.Claim.RunID, rec); err != nil && !errors.Is(err, plane.ErrDuplicateReviewer) {
		r.noteReportWrite("appending the verification record", err)
		return
	}
	r.end.doneReport = &rep
	log.Printf("%siteration %d: the %s phase: appended the %s verification record (verdict %s) to %s",
		labelOf(r.t), r.drainNum, r.phaseLabel(), at.role, rep.Verdict, claimLabel(at.res.Claim))
}

// handOverVerifiedRound is the verify phase's terminal step, which a runner
// brief forbids its session to take: once the round is closed with no open MUST
// and the task was left in_progress, the runner hands the task to review.
func (r *phaseRun) handOverVerifiedRound(ctx context.Context, at *phaseAttempt, st plane.TaskState) {
	if r.kind != reportVerify || r.end.doneReport == nil {
		return
	}
	// openMustIDs, not the plane's openMustIds: the plane's list includes void
	// records, whose MUSTs are not work left to do.
	if len(openMustIDs(st.Reviews)) != 0 || st.Status != "in_progress" {
		return
	}
	w, ok := r.t.Releaser.(plane.ReportWriter)
	if !ok {
		return
	}
	rep := *r.end.doneReport
	sub := plane.Submission{RiskDeclared: rep.RiskDeclared(), Touches: riskTouches(rep)}
	// The plane's in_review guard reads **Tests** from the stored outcome. The fix
	// phase normally wrote it, but a verify-start run may inherit an outcome
	// without one; the round is closed by now, so an outcome write is legal.
	if !config.HasTestsSection(st.Outcome) {
		sub.Outcome = config.AppendOutcome(st.Outcome, config.RenderTestsSection(rep.Tests))
	}
	if err := w.SubmitForReview(ctx, at.res.Claim.TaskID, at.res.Claim.RunID, sub); err != nil {
		r.noteReportWrite("handing the verified task over for review", err)
		return
	}
	r.end.verifyStatus = "in_review"
	at.res.Claim.Settled = true
	log.Printf("%siteration %d: the %s phase closed its round with no open MUST and the task in_progress — the runner handed %s over for review",
		labelOf(r.t), r.drainNum, r.phaseLabel(), claimLabel(at.res.Claim))
}

// parkReported parks the task on the session's own reason, appended to the
// outcome it found so the operator reads both. A successful park settles the
// claim exactly as the session's settle_task would have.
func (r *phaseRun) parkReported(ctx context.Context, at *phaseAttempt, st plane.TaskState, park string) {
	parker, ok := r.t.Releaser.(plane.ParkAPI)
	if !ok {
		r.noteReportProblem("the session asked for a park, but the plane client cannot park")
		return
	}
	outcome := config.AppendOutcome(st.Outcome, "### Parked by the session\n\n"+park)
	if err := parker.Park(ctx, at.res.Claim.TaskID, at.res.Claim.RunID, outcome); err != nil {
		r.noteReportWrite("the park the session asked for", err)
		return
	}
	at.res.Claim.Settled = true
	log.Printf("%siteration %d: the %s phase asked for a park and the plane accepted it (%s) — nothing else is written for this phase",
		labelOf(r.t), r.drainNum, r.phaseLabel(), park)
}

// fixParked handles a fix report's park before any write: the session found the
// review record missing or stale and changed nothing, so answering findings or
// writing a Fixes section would report a fix that never happened.
func (r *phaseRun) fixParked(ctx context.Context, at *phaseAttempt, rep config.DoneReport) bool {
	park := strings.TrimSpace(rep.Park)
	if park == "" {
		return false
	}
	if st, ok := r.doneTask(ctx, at, "for the fix phase's park"); ok {
		r.parkReported(ctx, at, st, park)
	}
	return true
}

// recordDoneBranch records the task's branch when the session did not, so a
// phase that pushed work but wrote nothing still leaves the hand-off a takeover
// needs. It reports whether the caller may carry on to its remaining writes.
func (r *phaseRun) recordDoneBranch(ctx context.Context, at *phaseAttempt) bool {
	if len(recordedBranches(at.res)) > 0 {
		return true
	}
	branch := r.doneBranch(at)
	if branch == "" {
		log.Printf("%siteration %d: the %s phase's task names no branch to record",
			labelOf(r.t), r.drainNum, r.phaseLabel())
		return true
	}
	if onOrigin, why := r.branchOnOrigin(ctx, at, branch); !onOrigin {
		log.Printf("%siteration %d: the %s phase: the task branch %s is not verified on the origin remote (%s) — not recording it",
			labelOf(r.t), r.drainNum, r.phaseLabel(), branch, why)
		return true
	}
	return r.recordVerifiedBranch(ctx, at, branch)
}

// recordVerifiedBranch writes a branch the caller has already verified on
// origin and folds it onto the claim, as salvage does.
func (r *phaseRun) recordVerifiedBranch(ctx context.Context, at *phaseAttempt, branch string) bool {
	if len(recordedBranches(at.res)) > 0 {
		return true
	}
	rec, ok := r.t.Releaser.(plane.Recorder)
	if !ok {
		log.Printf("%siteration %d: the %s phase: branch %s is pushed but cannot be recorded (no plane writes configured)",
			labelOf(r.t), r.drainNum, r.phaseLabel(), branch)
		return true
	}
	if err := rec.RecordBranch(ctx, at.res.Claim.TaskID, at.res.Claim.RunID, branch); err != nil {
		r.noteReportWrite("recording branch "+branch, err)
		return false
	}
	at.res.Claim.HasWIP = true
	at.res.Claim.Branch = branch
	r.d.noteCurrentClaim(at.res.Claim, r.t.Name)
	log.Printf("%siteration %d: the %s phase: recorded %s as the hand-off branch for %s",
		labelOf(r.t), r.drainNum, r.phaseLabel(), branch, claimLabel(at.res.Claim))
	return true
}

// branchOnOrigin runs the same delivery check the phase evidence gate runs: a
// branch may only be recorded, or handed over, when origin really has it.
func (r *phaseRun) branchOnOrigin(ctx context.Context, at *phaseAttempt, branch string) (bool, string) {
	if branch == "" {
		return false, "the task names no branch"
	}
	if r.d.newVerifier == nil || at.inv.WorkDir == "" {
		return false, "there is no working tree to check it from"
	}
	v := r.d.newVerifier(at.inv.WorkDir, r.d.cfg.AllowUncheckedPRFor(r.t.Name))
	vctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deliveryCheckTimeout)
	defer cancel()
	out := v.Verify(vctx, delivery.Claim{Label: claimLabel(at.res.Claim), Branch: branch})
	for _, c := range out.Checks {
		if c.Kind != delivery.BranchPushed {
			continue
		}
		if c.Status == delivery.Pass {
			return true, ""
		}
		return false, c.Detail
	}
	return false, "the check returned no branch-push result"
}

// doneTask reads the task a report's writes are appended to. A failed read is a
// reportProblem, not a retry: the phase's evidence gate fails closed on its own.
func (r *phaseRun) doneTask(ctx context.Context, at *phaseAttempt, purpose string) (plane.TaskState, bool) {
	st, ok := r.d.readTaskStateRetry(ctx, r.t, at.res.Claim.TaskID, purpose)
	if !ok {
		r.noteReportProblem("the task could not be read, so its done report was not written")
	}
	return st, ok
}

// doneBranch is the task branch as this session was told it: the verified
// hand-off, else the runner's own name for a fresh claim (the same rule the
// context block follows), else whatever the claim itself carries.
func (r *phaseRun) doneBranch(at *phaseAttempt) string {
	if r.prev != nil {
		if b := verifiedBranch(r.prev); b != "" {
			return b
		}
	}
	if r.opts.runner != nil && r.opts.runner.task.Branch != "" {
		return r.opts.runner.task.Branch
	}
	return at.res.Claim.Branch
}

// checkpointTip is the origin tip the brief handed this session as the revision
// to review, "" when the driver verified none.
func (r *phaseRun) checkpointTip() string {
	if r.prev == nil {
		return ""
	}
	return r.prev.CheckpointTip
}

// requireReviewTip is matchingReviewRecord's tip rule for the idempotency check:
// with no verified tip, an unverified checkpoint still demands a record that
// names its own.
func (r *phaseRun) requireReviewTip() bool {
	return r.checkpointTip() == "" && r.prev != nil && r.prev.CheckpointUnverified
}

func (r *phaseRun) phaseLabel() string { return r.ph.Label(r.phaseIdx) }

// noteReportProblem records why a done report became no records, and says so:
// "the phase recorded nothing" reads very differently when the session's own
// report was missing or invalid.
func (r *phaseRun) noteReportProblem(format string, args ...any) {
	problem := fmt.Sprintf(format, args...)
	log.Printf("%siteration %d: the %s phase: %s", labelOf(r.t), r.drainNum, r.phaseLabel(), problem)
	r.end.reportProblem = problem
}

func (r *phaseRun) noteReportWrite(what string, err error) {
	r.noteReportProblem("%s failed (%v) — the rest of this phase's records were not written", what, err)
}

// withReportProblem adds the done-report problem to a gate's reason, so the
// operator can tell "the session wrote nothing" from "the record did not land".
func (r *phaseRun) withReportProblem(why string) string {
	if r.end.reportProblem == "" {
		return why
	}
	return why + "; the session's done report: " + r.end.reportProblem
}

// newestReleasedReviewTip is the reviewed tip of the newest record the plane has
// released: the base a verification round's scope names. Sealed and void records
// speak for nothing.
func newestReleasedReviewTip(records []plane.ReviewRecordState) string {
	base, round := "", 0
	for _, rec := range records {
		if rec.State != "open" || rec.ReviewedTip == "" {
			continue
		}
		if rec.Round >= round {
			base, round = rec.ReviewedTip, rec.Round
		}
	}
	return base
}

func doneFindings(rep config.DoneReport, model string) []plane.ReviewFinding {
	if len(rep.Findings) == 0 {
		return nil
	}
	out := make([]plane.ReviewFinding, 0, len(rep.Findings))
	for _, f := range rep.Findings {
		out = append(out, plane.ReviewFinding{
			Severity:      f.Severity,
			Summary:       f.Summary,
			EvidenceKind:  f.EvidenceKind,
			Evidence:      f.Evidence,
			File:          f.File,
			Line:          f.Line,
			Kind:          f.Kind,
			RequiredFix:   f.RequiredFix,
			RaisedByModel: model,
		})
	}
	return out
}

func doneFindingUpdates(rep config.DoneReport) []plane.FindingUpdate {
	if len(rep.FindingUpdates) == 0 {
		return nil
	}
	out := make([]plane.FindingUpdate, 0, len(rep.FindingUpdates))
	for _, u := range rep.FindingUpdates {
		out = append(out, plane.FindingUpdate{
			FindingID:     u.FindingID,
			Status:        u.Status,
			FixCommit:     u.FixCommit,
			RefutedReason: u.RefutedReason,
			FiledTaskRef:  u.FiledTaskRef,
		})
	}
	return out
}

func riskTouches(rep config.DoneReport) []string {
	if rep.Risk == nil {
		return nil
	}
	return rep.Risk.Touches
}
