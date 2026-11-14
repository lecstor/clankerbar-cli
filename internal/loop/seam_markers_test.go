package loop

// CLA-879: STOP and RESTART act at a VERIFIED phase seam as well as between
// iterations, so a drain waits one phase session instead of a whole task
// sequence. The seam requires the resume mechanism (a runner-claimed drain
// writes `phasesDone`) and a verified checkpoint; these tests pin those guards,
// the handback, and everything that must keep today's behaviour.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// seamBrief is the task a seam fixture claims. newStartFixture grants the claim
// with run "run-"+ID; seamClaim mirrors it.
func seamBrief(ref string) plane.TaskBrief {
	return plane.TaskBrief{ID: walkClaimTaskID, Ref: ref, Branch: "clanker/11111111-seam"}
}

// seamClaim is the claim the runner plants for seamBrief.
func seamClaim(b plane.TaskBrief) harness.Claim {
	return harness.Claim{TaskID: b.ID, Ref: b.Ref, RunID: "run-" + b.ID}
}

// seamCheckpoint is a phase result that resumes claim and reaches its verified
// checkpoint: a clean exit still holding the task, with a branch recorded.
func seamCheckpoint(claim harness.Claim) harness.Result {
	return reported(held(okResult(1, 0), claim), harness.Report{
		TaskID: claim.TaskID, Ref: claim.Ref, RunID: claim.RunID, Branch: "clanker/seam-work",
	})
}

// runPhaseDriver drives the driver through Run, which is where the daemon-level
// consequence lives: STOP ends the run and RESTART leaves the re-exec flag set.
func runPhaseDriver(t *testing.T, d *Driver) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return d.Run(ctx)
}

func TestRun_StopAndRestartActAtThePhaseSeam(t *testing.T) {
	cases := []struct {
		name        string
		marker      string
		wantRestart bool
		wantLine    string
	}{
		{name: "STOP", marker: "STOP", wantLine: "STOP at the phase seam after implement"},
		{name: "RESTART", marker: MarkerRestart, wantRestart: true, wantLine: "RESTART at the phase seam after implement"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			brief := seamBrief("CLA-879")
			claim := seamClaim(brief)
			f := newStartFixture(t, brief, plane.TaskState{}, twoPhases(), []invokeStep{
				{res: seamCheckpoint(claim)},
				{res: seamCheckpoint(claim)}, // a second phase would land here; it must not
			})
			f.d.cfg.MaxIterations = 10 // generous: the marker, not the ceiling, ends this run
			logs := captureLogs(t)
			f.h.onInvoke = func(call int) {
				if call == 0 {
					writeMarker(t, f.d.state.Path(), tc.marker, "dropped mid-sequence")
				}
			}

			if err := runPhaseDriver(t, f.d); err != nil {
				t.Fatalf("Run returned error: %v; log was:\n%s", err, logs.String())
			}
			if f.h.invokeCalls != 1 {
				t.Fatalf("spawned %d sessions, want 1 — the %s must take effect at the seam, before phase 2 spawns", f.h.invokeCalls, tc.marker)
			}
			if got := f.d.RestartRequested(); got != tc.wantRestart {
				t.Errorf("RestartRequested = %t, want %t", got, tc.wantRestart)
			}
			if len(f.rel.calls) != 1 || f.rel.calls[0] != (releaseCall{claim.TaskID, claim.RunID}) {
				t.Errorf("the held claim was not released at the seam: releaser saw %v, want exactly [{%s %s}]",
					f.rel.calls, claim.TaskID, claim.RunID)
			}
			if len(f.rel.expireCalls) != 0 {
				t.Errorf("a claim with no pushed work must be released, not expired: %v", f.rel.expireCalls)
			}
			markerAbsent(t, f.d.state.Path(), tc.marker)
			out := logs.String()
			if !strings.Contains(out, tc.wantLine) {
				t.Errorf("the log must name the seam it acted on (%q); log was:\n%s", tc.wantLine, out)
			}
			// A handback at the seam is not an undeclared hand-off: the phase DID
			// declare its checkpoint, the operator's stop simply took it from
			// there. The CLA-384 counter must not be charged for it.
			if strings.Contains(out, "undeclared hand-offs this run") {
				t.Errorf("a seam stop was counted as an undeclared hand-off; log was:\n%s", out)
			}
			// The resume marker was written BEFORE the release: that is what
			// makes the next claim start at the following phase.
			wantDone := phaseDoneCall{claim.TaskID, claim.RunID, config.ImplementPhaseName, testCheckpointTip}
			if len(f.rel.done) != 1 || f.rel.done[0] != wantDone {
				t.Errorf("RecordPhaseDone = %+v, want exactly [%+v] — the seam must write the marker it promises", f.rel.done, wantDone)
			}
		})
	}
}

// TestRun_SeamHandsPushedWorkBackByEndingTheLease: a checkpoint means the phase
// pushed, so the handback must end the lease rather than post `ready` over the
// branch — that is what preserves the takeover hand-off.
func TestRun_SeamHandsPushedWorkBackByEndingTheLease(t *testing.T) {
	brief := seamBrief("CLA-884")
	claim := seamClaim(brief)
	pushed := claim
	pushed.HasWIP = true
	pushed.Branch = "clanker/11111111-work"
	f := newStartFixture(t, brief, plane.TaskState{}, twoPhases(), []invokeStep{
		{res: seamCheckpoint(pushed)},
		{res: seamCheckpoint(pushed)}, // must not land
	})
	f.d.cfg.MaxIterations = 10
	logs := captureLogs(t)
	f.h.onInvoke = func(call int) {
		if call == 0 {
			writeMarker(t, f.d.state.Path(), "STOP", "dropped mid-sequence")
		}
	}

	if err := runPhaseDriver(t, f.d); err != nil {
		t.Fatalf("Run returned error: %v; log was:\n%s", err, logs.String())
	}
	if f.h.invokeCalls != 1 {
		t.Fatalf("spawned %d sessions, want 1 — the STOP must act before phase 2", f.h.invokeCalls)
	}
	if len(f.rel.calls) != 0 {
		t.Errorf("Release was called %v on a claim with pushed work — that posts ready over the branch and drops the takeover hand-off", f.rel.calls)
	}
	if len(f.rel.expireCalls) != 1 || f.rel.expireCalls[0] != (releaseCall{claim.TaskID, claim.RunID}) {
		t.Errorf("ExpireLease = %v, want exactly [{%s %s}]", f.rel.expireCalls, claim.TaskID, claim.RunID)
	}
	if out := logs.String(); !strings.Contains(out, "STOP at the phase seam after implement") {
		t.Errorf("the seam action was not logged; log was:\n%s", out)
	}
}

// TestDrainPhases_NoCheckpointKeepsTodaysHandling: a phase that ends without a
// verified checkpoint leaves the walk nothing to resume, so it ends where it
// always did and the seam must NOT consume the marker. The marker stays for the
// iteration boundary (or Run's task-boundary read), which is today's route to
// the stop.
func TestDrainPhases_NoCheckpointKeepsTodaysHandling(t *testing.T) {
	brief := seamBrief("CLA-881")
	claim := seamClaim(brief)
	settled := claim
	settled.Settled = true
	f := newStartFixture(t, brief, plane.TaskState{}, twoPhases(), []invokeStep{
		// The session settled the task itself: a clean exit holding nothing,
		// so there is no checkpoint and nothing for a successor to resume.
		{res: held(okResult(1, 0), settled)},
		{res: seamCheckpoint(claim)}, // phase 2 would land here; it must not
	})
	logs := captureLogs(t)
	f.h.onInvoke = func(call int) {
		if call == 0 {
			writeMarker(t, f.d.state.Path(), "STOP", "dropped mid-sequence")
		}
	}

	_, _, stop, err := drainPhasesOnce(t, f.d)
	if err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	if stop {
		t.Error("the seam must not act on a marker when no verified checkpoint opened it")
	}
	if f.h.invokeCalls != 1 {
		t.Fatalf("spawned %d sessions, want 1 — a phase with no checkpoint has no successor to resume", f.h.invokeCalls)
	}
	if !markerPresent(t, f.d.state.Path(), "STOP") {
		t.Error("the seam consumed a STOP with no verified checkpoint; the marker must keep today's handling (the iteration boundary)")
	}
	if len(f.rel.calls) != 0 || len(f.rel.expireCalls) != 0 {
		t.Errorf("a settled claim owes no handback: Release=%v ExpireLease=%v", f.rel.calls, f.rel.expireCalls)
	}
	if out := logs.String(); strings.Contains(out, "at the phase seam") {
		t.Errorf("no seam line may be logged without a verified checkpoint; log was:\n%s", out)
	}
}

// TestDrainPhases_SessionClaimedSequenceKeepsTheIterationBoundary: a drain the
// runner did not claim writes no `phasesDone` marker, so a seam stop would make
// the next claim re-run phase 1. It must keep today's iteration-boundary
// handling: the sequence runs, and the marker waits for the boundary.
func TestDrainPhases_SessionClaimedSequenceKeepsTheIterationBoundary(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(1, 0)},
		{res: held(okResult(1, 0), openClaim())},
	}}
	d, _ := phaseDriver(t, h, twoPhases()) // no TaskClaimer: the session claims
	logs := captureLogs(t)
	h.onInvoke = func(call int) {
		if call == 0 {
			writeMarker(t, d.state.Path(), "STOP", "dropped mid-sequence")
		}
	}

	_, _, stop, err := drainPhasesOnce(t, d)
	if err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 2 {
		t.Fatalf("spawned %d sessions, want 2 — without a runner-written resume marker the sequence must keep its iteration-boundary handling", h.invokeCalls)
	}
	if !markerPresent(t, d.state.Path(), "STOP") {
		t.Error("the seam consumed a STOP on a session-claimed drain, where the next claim would re-run phase 1")
	}
	if out := logs.String(); strings.Contains(out, "at the phase seam") {
		t.Errorf("a session-claimed drain must not act at the phase seam; log was:\n%s", out)
	}
}

// TestDrainPhases_UnverifiedCheckpointKeepsTodaysHandling: a CLA-576 checkpoint
// whose branch the origin check could not verify writes no `phasesDone` marker,
// so the next claim would re-run this phase instead of resuming the next — the
// seam must leave the marker for the iteration boundary.
func TestDrainPhases_UnverifiedCheckpointKeepsTodaysHandling(t *testing.T) {
	brief := seamBrief("CLA-882")
	claim := seamClaim(brief)
	// The session recorded a branch, but the verifier cannot read origin.
	recorded := harness.Claim{TaskID: claim.TaskID, Ref: claim.Ref, RunID: claim.RunID, HasWIP: true, Branch: "clanker/x"}
	unverified := reported(held(okResult(1, 0), recorded), harness.Report{
		TaskID: recorded.TaskID, Ref: recorded.Ref, RunID: recorded.RunID, Branch: recorded.Branch,
	})
	f := newStartFixture(t, brief, plane.TaskState{}, twoPhases(), []invokeStep{
		{res: unverified},
		{res: seamCheckpoint(claim)}, // the review phase still spawns on the held claim
	})
	f.d.newVerifier = func(string, bool) deliveryVerifier { return unreadableOriginVerifier() }
	logs := captureLogs(t)
	f.h.onInvoke = func(call int) {
		if call == 0 {
			writeMarker(t, f.d.state.Path(), "STOP", "dropped mid-sequence")
		}
	}

	_, _, stop, err := drainPhasesOnce(t, f.d)
	if err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	if stop {
		t.Error("the seam must not act on an unverified checkpoint")
	}
	if f.h.invokeCalls != 2 {
		t.Fatalf("spawned %d sessions, want 2 — an unverified checkpoint still advances to the review phase on the held claim", f.h.invokeCalls)
	}
	if !markerPresent(t, f.d.state.Path(), "STOP") {
		t.Error("the seam consumed the STOP; an unverified checkpoint must keep today's iteration-boundary handling")
	}
	if out := logs.String(); strings.Contains(out, "at the phase seam") {
		t.Errorf("no seam line may be logged without a verified checkpoint; log was:\n%s", out)
	}
	// markPhaseDone skips the unverified checkpoint, which is exactly why the
	// seam must not release there: there is no marker for the next claim.
	if len(f.rel.done) != 0 {
		t.Errorf("RecordPhaseDone = %+v, want none for an unverified checkpoint", f.rel.done)
	}
	if got := f.h.invocations[1].ResumeClaim; got.TaskID != claim.TaskID || got.RunID != claim.RunID {
		t.Errorf("the review phase did not resume the held claim: %+v", got)
	}
}

// TestStopAtSeamMarker_RefusesAnUnresumableCheckpoint drives the guard directly
// for the handoff-only form, which the refusal ladder above never lets reach the
// seam: it must not consume the marker or release the claim either.
func TestStopAtSeamMarker_RefusesAnUnresumableCheckpoint(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*drainSeq, *seqStep)
	}{
		{
			name: "a CLA-576 unverified checkpoint",
			mutate: func(s *drainSeq, _ *seqStep) {
				s.carried.CheckpointUnverified = true
			},
		},
		{
			name: "a handoff checkpoint with no branch evidence",
			mutate: func(_ *drainSeq, st *seqStep) {
				st.end.branch = ""
				st.end.handoff = "carry on with the tests"
			},
		},
		{
			name: "a handoff checkpoint WITH branch evidence",
			mutate: func(_ *drainSeq, st *seqStep) {
				st.end.handoff = "carry on with the tests"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, rel := phaseDriver(t, &fakeAdapter{}, twoPhases())
			s := &drainSeq{
				d: d, t: d.targets[0], phases: twoPhases(), runnerClaimed: true, i: 0,
				carried: &harness.Result{Claim: openClaim()},
			}
			st := &seqStep{end: phaseEnd{checkpoint: true, branch: "clanker/x"}}
			writeMarker(t, d.state.Path(), "STOP", "dropped mid-sequence")
			tc.mutate(s, st)

			if s.stopAtSeamMarker(context.Background(), st, 1, "") {
				t.Error("the seam acted on a checkpoint with nothing for the next claim to resume")
			}
			if !markerPresent(t, d.state.Path(), "STOP") {
				t.Error("the seam consumed the STOP")
			}
			if len(rel.calls) != 0 || len(rel.expireCalls) != 0 {
				t.Errorf("the seam handed the claim back: Release=%v ExpireLease=%v", rel.calls, rel.expireCalls)
			}
		})
	}
}

// TestDrainPhases_NoMarkerStillRunsTheSequenceOnOneClaim: the seam read must be
// invisible when no marker is there. Every phase spawns, the claim survives the
// seam, and the only handback is the drain's ordinary one at the end.
func TestDrainPhases_NoMarkerStillRunsTheSequenceOnOneClaim(t *testing.T) {
	brief := seamBrief("CLA-880")
	claim := seamClaim(brief)
	f := newStartFixture(t, brief, plane.TaskState{}, twoPhases(), []invokeStep{
		{res: seamCheckpoint(claim)},
		{res: held(okResult(1, 0), claim)},
	})
	logs := captureLogs(t)
	var invokesAtRelease []int
	f.rel.onCall = func() { invokesAtRelease = append(invokesAtRelease, f.h.invokeCalls) }

	_, _, stop, err := drainPhasesOnce(t, f.d)
	if err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if f.h.invokeCalls != 2 {
		t.Fatalf("spawned %d sessions, want 2 — a sequence with no marker runs every phase", f.h.invokeCalls)
	}
	if len(f.rel.calls) != 1 || len(invokesAtRelease) != 1 || invokesAtRelease[0] != 2 {
		t.Errorf("releases = %v (at invokes %v), want exactly one after both phases — the claim must survive the seam", f.rel.calls, invokesAtRelease)
	}
	// The implement checkpoint is the only phase marker: the review position
	// ended without one.
	wantDone := phaseDoneCall{claim.TaskID, claim.RunID, config.ImplementPhaseName, testCheckpointTip}
	if len(f.rel.done) != 1 || f.rel.done[0] != wantDone {
		t.Errorf("RecordPhaseDone = %+v, want exactly [%+v]", f.rel.done, wantDone)
	}
	if out := logs.String(); strings.Contains(out, "at the phase seam") {
		t.Errorf("no marker, but a seam action was logged; log was:\n%s", out)
	}
}

// TestDrainPhases_PauseDoesNotStopAtThePhaseSeam pins the operator decision
// 6019ea26: a project pause only gates NEW claims, so a pause landing (or
// already in force) during phase 1 must let every remaining phase run on the
// held claim. Pausing is how the operator gets in-flight tasks completed.
func TestDrainPhases_PauseDoesNotStopAtThePhaseSeam(t *testing.T) {
	t.Run("console pause flag", func(t *testing.T) {
		brief := seamBrief("CLA-885")
		claim := seamClaim(brief)
		f := newStartFixture(t, brief, plane.TaskState{}, twoPhases(), []invokeStep{
			{res: seamCheckpoint(claim)},
			{res: held(okResult(1, 0), claim)},
		})
		logs := captureLogs(t)
		f.h.onInvoke = func(call int) {
			if call == 0 {
				f.d.paused[0] = true // a console poll flipped mid-sequence
			}
		}
		_, _, stop, err := drainPhasesOnce(t, f.d)
		if err != nil || stop {
			t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
		}
		if f.h.invokeCalls != 2 {
			t.Errorf("a console pause stopped the sequence after %d session(s); every remaining phase must run on the held claim", f.h.invokeCalls)
		}
		if out := logs.String(); strings.Contains(out, "at the phase seam") {
			t.Errorf("a pause must not be read at the phase seam; log was:\n%s", out)
		}
	})

	t.Run("scheduled pause window opens mid-sequence", func(t *testing.T) {
		brief := seamBrief("CLA-886")
		claim := seamClaim(brief)
		f := newStartFixture(t, brief, plane.TaskState{}, twoPhases(), []invokeStep{
			{res: seamCheckpoint(claim)},
			{res: held(okResult(1, 0), claim)},
		})
		logs := captureLogs(t)
		// peakSchedule is mon-fri 11:00-14:00 Australia/Brisbane; flip the
		// clock into it from inside phase 1.
		f.d.cfg.PauseWindows = peakSchedule(t)
		inside := brisbaneAt(t, 9, 12, 0)
		f.h.onInvoke = func(call int) {
			if call == 0 {
				f.d.now = func() time.Time { return inside }
			}
		}
		_, _, stop, err := drainPhasesOnce(t, f.d)
		if err != nil || stop {
			t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
		}
		if f.h.invokeCalls != 2 {
			t.Errorf("a scheduled pause window stopped the sequence after %d session(s); every remaining phase must run on the held claim", f.h.invokeCalls)
		}
		if out := logs.String(); strings.Contains(out, "at the phase seam") {
			t.Errorf("a pause window must not be read at the phase seam; log was:\n%s", out)
		}
	})
}

// seamVerifyReleaser is reviewReleaser plus the plane's TaskClaimer: the verify
// seam only fires on a runner-claimed drain, so its fixture has to be able to
// claim (and is what proves the phaseDone marker exists on this path).
type seamVerifyReleaser struct {
	*reviewReleaser
	offer   plane.Offer
	claimFn func() (plane.ClaimedTask, error)
}

func (r *seamVerifyReleaser) NextOffer(context.Context) (plane.Offer, error) {
	o := r.offer
	r.offer = plane.Offer{}
	return o, nil
}

func (r *seamVerifyReleaser) ClaimTask(context.Context, string, bool) (plane.ClaimedTask, error) {
	return r.claimFn()
}

// TestDrainPhases_StopAtAVerifySeamStopsBeforeTheFixRound: the verification
// round's open-MUST rewind is a phase seam too. A STOP dropped during verify
// must stop before the fix round it would have spawned, not after that round
// (and the verify it would then owe) has run.
func TestDrainPhases_StopAtAVerifySeamStopsBeforeTheFixRound(t *testing.T) {
	brief := seamBrief("CLA-883")
	claim := seamClaim(brief)
	inner := &reviewReleaser{status: "in_progress", holder: claim.RunID, outcome: "implemented: pushed the branch\n"}
	rel := &seamVerifyReleaser{
		reviewReleaser: inner,
		offer:          plane.Offer{Next: &brief},
		claimFn: func() (plane.ClaimedTask, error) {
			return plane.ClaimedTask{Task: brief, RunID: claim.RunID}, nil
		},
	}
	h := &fakeAdapter{steps: []invokeStep{
		{res: seamCheckpoint(claim)}, // implement
		{res: seamCheckpoint(claim)}, // review
		{res: seamCheckpoint(claim)}, // fix
		{res: seamCheckpoint(claim)}, // verify: leaves a MUST open, so a fix would respawn
		{res: seamCheckpoint(claim)}, // the respawned fix would land here
	}}
	d := phaseDriverWithReleaser(t, h, verifyPhases(), rel)
	logs := captureLogs(t)
	h.onInvoke = func(i int) {
		switch i {
		case 1: // the review raises the MUST the verifier will adjudicate
			inner.records = append(inner.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-1", RunID: claim.RunID, ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{mustFinding("1.review-1.1", "open", "the seam is untested")},
			})
			inner.openMusts = []string{"1.review-1.1"}
		case 3: // the verify round leaves it open, and the operator drops STOP
			inner.records = append(inner.records, recordFor(2, "review-2", claim.RunID, testCheckpointTip, "sealed"))
			inner.counted = 2
			inner.openMusts = []string{"1.review-1.1"}
			writeMarker(t, d.state.Path(), "STOP", "dropped during verify")
		}
	}

	_, _, stop, err := drainPhasesOnce(t, d)
	if err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	if !stop {
		t.Error("a STOP during verify with an open MUST must stop the sequence")
	}
	if h.invokeCalls != 4 {
		t.Fatalf("spawned %d sessions, want 4 — the fix round after verify must not spawn", h.invokeCalls)
	}
	if len(inner.calls) != 1 || inner.calls[0] != (releaseCall{claim.TaskID, claim.RunID}) {
		t.Errorf("the held claim must be released at the verify seam: releaser saw %v", inner.calls)
	}
	markerAbsent(t, d.state.Path(), "STOP")
	if out := logs.String(); !strings.Contains(out, "STOP at the phase seam after verify") {
		t.Errorf("the log must name the verify seam; log was:\n%s", out)
	}
}
