package loop

// CLA-1033: a phase that DELIVERED its work but failed to re-emit its done
// report is retried inside the claim it still holds (CLA-877) — and that has to
// hold on the LAST phase too. A verify round routinely pushes the branch and
// opens the PR before its report resume stalls; expiring the lease there both
// burns a reclaim slot and hands a finished task to a takeover. The guards that
// excluded the last phase are classifyExit's `!last` and judgeDeadPhase's
// `retryable`, and these tests pin both sides of the narrow widening: a
// resume-only failure retries (bounded by the per-task budget), while a genuine
// dead phase or a tool-loop kill on the last phase stays as non-retryable as it
// was.

import (
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// lastPhasePhases is the shape under test: two phases whose SECOND is the
// verification round, so the last phase is the one CLA-1033 is about.
func lastPhasePhases() []config.Phase {
	return []config.Phase{{Name: config.ImplementPhaseName}, {Name: config.VerifyPhaseName}}
}

// lastPhaseClaim is the claim every scripted result in these tests carries: the
// runner's own, with pushed work on a branch — the state a verify round is in
// when its report resume stalls.
func lastPhaseClaim() harness.Claim {
	return harness.Claim{
		TaskID: walkClaimTaskID, Ref: "CLA-1033", RunID: walkClaimRunID,
		HasWIP: true, Branch: "clanker/walk",
	}
}

// lastPhaseDriver is the runner-claimed drain over lastPhasePhases and a
// report-writing plane client, so the retry's done report really lands.
func lastPhaseDriver(t *testing.T, h harness.Adapter, rel *reportReleaser) *Driver {
	t.Helper()
	if rel.st.Status == "" {
		// The verify round's hand-over is only for a task still in_progress.
		rel.st.Status = "in_progress"
	}
	return phaseDriverWithReleaser(t, h, lastPhasePhases(), rel)
}

// claimingReportReleaser is the plane client these tests drive: the runner's
// claim surface and every write a done report and a dead-phase park make.
func claimingReportReleaser(brief plane.TaskBrief) *reportReleaser {
	rel := &reportReleaser{}
	rel.offers = []plane.Offer{{Next: &brief}}
	rel.claimFn = func(string, bool) (plane.ClaimedTask, error) {
		return plane.ClaimedTask{Task: brief, RunID: walkClaimRunID}, nil
	}
	return rel
}

// The retry runs INSIDE the claim the stalled verify round still holds: no
// handback, no ExpireLease, no re-claim — and the retry's done report is applied
// and hands the task over, which is the work the stalled attempt delivered.
func TestRunnerClaim_LastPhaseReportResumeFailureRetriesInTheSameClaim(t *testing.T) {
	b := walkClaimBrief("CLA-1033")
	f := claimingReportReleaser(b)
	claim := lastPhaseClaim()
	// The implement phase reaches its checkpoint on the runner's claim.
	implemented := reported(held(okResult(1, 0), claim), harness.Report{
		TaskID: walkClaimTaskID, Ref: "CLA-1033", RunID: walkClaimRunID, Branch: "clanker/walk",
	})
	// The verify round delivered — branch pushed, PR opened — but its report
	// resume went silent. Its own end is otherwise clean.
	stalled := held(okResult(1, 0), claim)
	stalled.ReportResumeFailed = "no output for 3m0s"
	// The retry re-emits the report the stalled attempt owed.
	retried := held(okResult(1, 0), claim)
	retried.FinalMessage = doneReportFor(`{"verdict":"pass","reviewedTip":"` + testCheckpointTip + `","tests":"go test -race ./..."}`)
	h := &fakeAdapter{steps: []invokeStep{
		{res: implemented},
		{res: stalled},
		{res: retried},
	}}
	logged := captureLogs(t)
	d := lastPhaseDriver(t, h, f)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v — a resumed report on the last phase is retryable, not fatal", err, stop)
	}
	if h.invokeCalls != 3 {
		t.Fatalf("spawned %d sessions, want 3 (implement, the stalled verify, its retry) — the last phase must be retried in-claim", h.invokeCalls)
	}
	if len(f.claimCalls) != 1 {
		t.Errorf("claim calls = %v, want only the runner's initial fresh claim — the retry holds the claim and must not re-claim it", f.claimCalls)
	}
	if got := handbacks(&f.startReleaser.claimingReleaser, walkClaimTaskID); got != 0 {
		t.Errorf("handed the task back %d time(s), want none — a resume-only failure keeps the claim: %+v / %+v",
			got, f.startReleaser.claimingReleaser.calls, f.startReleaser.claimingReleaser.expireCalls)
	}
	if got := h.invocations[2].ResumeClaim; got.TaskID != walkClaimTaskID || got.RunID != walkClaimRunID {
		t.Errorf("the retry's ResumeClaim = %+v, want the SAME %s/%s the runner holds", got, walkClaimTaskID, walkClaimRunID)
	}
	if !strings.Contains(h.invocations[2].Prompt, "PHASE 2 of 2") {
		t.Errorf("the retry did not re-run the verify position:\n%q", h.invocations[2].Prompt)
	}
	if !strings.Contains(h.invocations[2].Prompt, "Branch: clanker/walk") {
		t.Errorf("the retry's context block dropped the checkpoint's branch the verify brief needs:\n%q", h.invocations[2].Prompt)
	}
	// The retry's report was applied and the round handed the task over: the
	// retry SUCCEEDED at the job the stalled attempt had already done.
	if len(f.appends) != 1 {
		t.Errorf("appended %d review record(s), want 1 from the retry's done report: %+v", len(f.appends), f.appends)
	}
	if len(f.submits) != 1 {
		t.Errorf("handed the task over %d time(s), want 1 — the retry's report is the verify round's terminal step", len(f.submits))
	}
	if f.st.Status != "in_review" {
		t.Errorf("task status = %q, want in_review after the retried verify round", f.st.Status)
	}
	if out := logged.String(); !strings.Contains(out, "SAME claim") {
		t.Errorf("the retry was not named as running inside the held claim:\n%s", out)
	}
}

// The in-claim retry is still bounded: repeated resume failures on the LAST
// phase reach the per-task dead budget and park the task, rather than looping
// sessions on it forever.
func TestRunnerClaim_LastPhaseReportResumeFailuresParkAtTheDeadBound(t *testing.T) {
	b := walkClaimBrief("CLA-1033")
	f := claimingReportReleaser(b)
	claim := lastPhaseClaim()
	implemented := reported(held(okResult(1, 0), claim), harness.Report{
		TaskID: walkClaimTaskID, Ref: "CLA-1033", RunID: walkClaimRunID, Branch: "clanker/walk",
	})
	stalled := held(okResult(1, 0), claim)
	stalled.ReportResumeFailed = "no output for 3m0s"
	// implement + the four verify attempts the per-task budget allows.
	steps := []invokeStep{{res: implemented}}
	for i := 0; i < perTaskDeadBound; i++ {
		steps = append(steps, invokeStep{res: stalled})
	}
	h := &fakeAdapter{steps: steps}
	logged := captureLogs(t)
	d := lastPhaseDriver(t, h, f)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v — reaching the dead bound parks the task, it does not fail the run", err, stop)
	}
	if h.invokeCalls != perTaskDeadBound+1 {
		t.Fatalf("spawned %d sessions, want %d (implement + %d verify attempts, then the park) — the retry must be bounded",
			h.invokeCalls, perTaskDeadBound+1, perTaskDeadBound)
	}
	if len(f.parks) != 1 {
		t.Fatalf("parked %d time(s), want 1 — four resume failures on the last phase must reach the operator: %+v\n%s",
			len(f.parks), f.parks, logged.String())
	}
	if !strings.Contains(f.parks[0], "could not re-emit its done report") {
		t.Errorf("the park's outcome does not name the resume failure the retries kept hitting:\n%s", f.parks[0])
	}
	if len(f.startReleaser.questions) != 1 {
		t.Errorf("filed %d question(s), want 1 — the park must reach the operator: %+v",
			len(f.startReleaser.questions), f.startReleaser.questions)
	}
	if len(f.claimCalls) != 1 {
		t.Errorf("claim calls = %v, want only the runner's initial fresh claim — every retry ran inside the held claim", f.claimCalls)
	}
	if out := logged.String(); !strings.Contains(out, "parking it per the 2026-08-20 decision") {
		t.Errorf("the park was not named in the log:\n%s", out)
	}
}

// The widening is deliberately narrow: a genuine last-phase death and a
// tool-loop kill keep their existing (non-retryable) behaviour — the claim is
// handed back exactly as before, and no in-claim retry runs.
func TestRunnerClaim_LastPhaseGenuineDeathsStayNonRetryable(t *testing.T) {
	cases := []struct {
		name string
		res  harness.Result
	}{
		{name: "dead phase", res: held(deadResult(), harness.Claim{TaskID: walkClaimTaskID, RunID: walkClaimRunID})},
		{name: "tool loop", res: held(toolLoopResult(0), harness.Claim{TaskID: walkClaimTaskID, RunID: walkClaimRunID})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := walkClaimBrief("CLA-1033")
			f := claimingReportReleaser(b)
			claim := lastPhaseClaim()
			implemented := reported(held(okResult(1, 0), claim), harness.Report{
				TaskID: walkClaimTaskID, Ref: "CLA-1033", RunID: walkClaimRunID, Branch: "clanker/walk",
			})
			h := &fakeAdapter{steps: []invokeStep{{res: implemented}, {res: tc.res}}}
			logged := captureLogs(t)
			d := lastPhaseDriver(t, h, f)

			if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
				t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
			}
			if h.invokeCalls != 2 {
				t.Fatalf("spawned %d sessions, want 2 — a last-phase %s is not retried in-claim", h.invokeCalls, tc.name)
			}
			if got := handbacks(&f.startReleaser.claimingReleaser, walkClaimTaskID); got != 1 {
				t.Errorf("handed the task back %d time(s), want exactly 1 — a non-retryable last-phase death keeps the ordinary handback: %+v / %+v",
					got, f.startReleaser.claimingReleaser.calls, f.startReleaser.claimingReleaser.expireCalls)
			}
			if out := logged.String(); strings.Contains(out, "SAME claim") {
				t.Errorf("a genuine last-phase %s was retried in-claim; only a report-resume failure may be:\n%s", tc.name, out)
			}
		})
	}
}

// classifyExit is the first of the two guards: `at.dead` is what judgeDeadPhase
// reads. On the last phase it must be true for a resume-only failure and false
// for the deaths that have no claim-staying retry to offer.
func TestClassifyExit_OnlyResumeFailureIsDeadOnTheLastPhase(t *testing.T) {
	claim := lastPhaseClaim()
	// A genuine dead phase is "no branch recorded": deadPhase keys on a claim
	// with pushed work cleared.
	spent := harness.Claim{TaskID: walkClaimTaskID, RunID: walkClaimRunID}
	resumeOnly := held(okResult(1, 0), claim)
	resumeOnly.ReportResumeFailed = "no output for 3m0s"
	cases := []struct {
		name     string
		last     bool
		res      harness.Result
		toolLoop string
		want     bool
	}{
		{name: "last phase: report resume failed", last: true, res: resumeOnly, want: true},
		{name: "last phase: dead phase", last: true, res: held(deadResult(), spent)},
		{name: "last phase: tool loop", last: true, res: held(toolLoopResult(0), claim), toolLoop: loopCause},
		{name: "not last: report resume failed", res: resumeOnly, want: true},
		{name: "not last: dead phase", res: held(deadResult(), spent), want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &phaseRun{last: tc.last}
			at := &phaseAttempt{res: tc.res, toolLoop: tc.toolLoop}
			r.classifyExit(at)
			if at.dead != tc.want {
				t.Errorf("classifyExit dead = %v, want %v", at.dead, tc.want)
			}
		})
	}
}
