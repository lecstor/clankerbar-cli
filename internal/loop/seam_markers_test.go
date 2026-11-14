package loop

// CLA-879: STOP and RESTART are honoured at a VERIFIED phase seam, not only
// between iterations. One iteration runs a task's whole phase sequence, so a
// marker dropped during phase 1 used to wait for implement -> review -> fix ->
// verify to finish (one real drain ran 2h+ and was SIGKILLed by systemd's
// TimeoutStopSec 2.5 minutes into verify). At a seam the phase has already
// ended, so the runner releases the held claim and stops (STOP) or re-execs
// (RESTART) without killing anything, and the next claim resumes the next phase
// from the checkpoint.
//
// The four behaviours pinned here:
//  1. STOP mid-sequence: phase 2 never spawns, the claim is released, the
//     marker is consumed and the daemon stops.
//  2. RESTART mid-sequence: the same release, with the re-exec path taken.
//  3. A phase ending WITHOUT a verified checkpoint keeps today's handling: the
//     marker is not consumed at the seam (the walk has already ended for want
//     of anything to resume) and lands at the iteration boundary as before.
//  4. No marker: the sequence runs every phase on the one claim, exactly as
//     before.
//
// A project pause is NOT a seam stop, and is pinned separately: pausing may not
// stop an in-flight sequence (decision 6019ea26).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

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
			h := &fakeAdapter{steps: []invokeStep{
				{res: checkpointed(1, 0)},
				{res: checkpointed(1, 0)}, // a second phase would land here; it must not
			}}
			d := phaseDriverWithReleaser(t, h, twoPhases(), &fakeReleaser{})
			rel := d.targets[0].Releaser.(*fakeReleaser)
			d.cfg.MaxIterations = 10 // generous: the marker, not the ceiling, ends this run
			logs := captureLogs(t)
			h.onInvoke = func(call int) {
				if call == 0 {
					writeMarker(t, d.state.Path(), tc.marker, "dropped mid-sequence")
				}
			}

			if err := runPhaseDriver(t, d); err != nil {
				t.Fatalf("Run returned error: %v; log was:\n%s", err, logs.String())
			}
			if h.invokeCalls != 1 {
				t.Fatalf("spawned %d sessions, want 1 — the %s must take effect at the seam, before phase 2 spawns", h.invokeCalls, tc.marker)
			}
			if got := d.RestartRequested(); got != tc.wantRestart {
				t.Errorf("RestartRequested = %t, want %t", got, tc.wantRestart)
			}
			if len(rel.calls) != 1 || rel.calls[0] != (releaseCall{"t-1", "r-1"}) {
				t.Errorf("the held claim was not released at the seam: releaser saw %v, want exactly [{t-1 r-1}]", rel.calls)
			}
			markerAbsent(t, d.state.Path(), tc.marker)
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
		})
	}
}

// TestDrainPhases_NoCheckpointKeepsTodaysHandling: a phase that ends without a
// verified checkpoint leaves the walk nothing to resume, so it ends where it
// always did and the seam must NOT consume the marker. The empty exit's own
// handback runs; the marker stays for the iteration boundary (or Run's
// task-boundary read), which is today's route to the stop.
func TestDrainPhases_NoCheckpointKeepsTodaysHandling(t *testing.T) {
	settled := harness.Claim{TaskID: "t-1", RunID: "r-1", Settled: true}
	h := &fakeAdapter{steps: []invokeStep{
		// The session settled the task itself: a clean exit holding nothing,
		// so there is no checkpoint and nothing for a successor to resume.
		{res: held(okResult(1, 0), settled)},
		{res: okResult(1, 0)}, // phase 2 would land here; it must not
	}}
	d, rel := phaseDriver(t, h, twoPhases())
	logs := captureLogs(t)
	h.onInvoke = func(call int) {
		if call == 0 {
			writeMarker(t, d.state.Path(), "STOP", "dropped mid-sequence")
		}
	}

	_, _, stop, err := drainPhasesOnce(t, d)
	if err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	if stop {
		t.Error("the seam must not act on a marker when no verified checkpoint opened it")
	}
	if h.invokeCalls != 1 {
		t.Fatalf("spawned %d sessions, want 1 — a phase with no checkpoint has no successor to resume", h.invokeCalls)
	}
	if !markerPresent(t, d.state.Path(), "STOP") {
		t.Error("the seam consumed a STOP with no verified checkpoint; the marker must keep today's handling (the iteration boundary)")
	}
	if len(rel.calls) != 0 {
		t.Errorf("a settled claim owes no handback; releaser saw %v", rel.calls)
	}
	if out := logs.String(); strings.Contains(out, "at the phase seam") {
		t.Errorf("no seam line may be logged without a verified checkpoint; log was:\n%s", out)
	}
}

// TestDrainPhases_NoMarkerStillRunsTheSequenceOnOneClaim: the seam read must be
// invisible when no marker is there. Every phase spawns, the claim survives the
// seam, and the only handback is the drain's ordinary one at the end.
func TestDrainPhases_NoMarkerStillRunsTheSequenceOnOneClaim(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(1, 0)},
		{res: held(okResult(1, 0), openClaim())},
	}}
	d, rel := phaseDriver(t, h, twoPhases())
	logs := captureLogs(t)
	var invokesAtRelease []int
	rel.onCall = func() { invokesAtRelease = append(invokesAtRelease, h.invokeCalls) }

	_, _, stop, err := drainPhasesOnce(t, d)
	if err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 2 {
		t.Fatalf("spawned %d sessions, want 2 — a sequence with no marker runs every phase", h.invokeCalls)
	}
	if len(rel.calls) != 1 || len(invokesAtRelease) != 1 || invokesAtRelease[0] != 2 {
		t.Errorf("releases = %v (at invokes %v), want exactly one after both phases — the claim must survive the seam", rel.calls, invokesAtRelease)
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
		h := &fakeAdapter{steps: []invokeStep{
			{res: checkpointed(1, 0)},
			{res: held(okResult(1, 0), openClaim())},
		}}
		d, _ := phaseDriver(t, h, twoPhases())
		logs := captureLogs(t)
		h.onInvoke = func(call int) {
			if call == 0 {
				d.paused[0] = true // a console poll flipped mid-sequence
			}
		}
		_, _, stop, err := drainPhasesOnce(t, d)
		if err != nil || stop {
			t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
		}
		if h.invokeCalls != 2 {
			t.Errorf("a console pause stopped the sequence after %d session(s); every remaining phase must run on the held claim", h.invokeCalls)
		}
		if out := logs.String(); strings.Contains(out, "at the phase seam") {
			t.Errorf("a pause must not be read at the phase seam; log was:\n%s", out)
		}
	})

	t.Run("scheduled pause window opens mid-sequence", func(t *testing.T) {
		h := &fakeAdapter{steps: []invokeStep{
			{res: checkpointed(1, 0)},
			{res: held(okResult(1, 0), openClaim())},
		}}
		d, _ := phaseDriver(t, h, twoPhases())
		logs := captureLogs(t)
		// peakSchedule is mon-fri 11:00-14:00 Australia/Brisbane; flip the
		// clock into it from inside phase 1.
		d.cfg.PauseWindows = peakSchedule(t)
		inside := brisbaneAt(t, 9, 12, 0)
		h.onInvoke = func(call int) {
			if call == 0 {
				d.now = func() time.Time { return inside }
			}
		}
		_, _, stop, err := drainPhasesOnce(t, d)
		if err != nil || stop {
			t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
		}
		if h.invokeCalls != 2 {
			t.Errorf("a scheduled pause window stopped the sequence after %d session(s); every remaining phase must run on the held claim", h.invokeCalls)
		}
		if out := logs.String(); strings.Contains(out, "at the phase seam") {
			t.Errorf("a pause window must not be read at the phase seam; log was:\n%s", out)
		}
	})
}

// TestDrainPhases_StopAtAVerifySeamStopsBeforeTheFixRound: the verification
// round's open-MUST rewind is a phase seam too. A STOP dropped during verify
// must stop before the fix round it would have spawned, not after that round
// (and the verify it would then owe) has run.
func TestDrainPhases_StopAtAVerifySeamStopsBeforeTheFixRound(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)}, // implement
		{res: checkpointed(5, 0.50)},  // review
		{res: checkpointed(5, 0.05)},  // fix
		{res: checkpointed(5, 0.05)},  // verify: leaves a MUST open, so a fix would respawn
		{res: checkpointed(5, 0.05)},  // the respawned fix would land here
	}}
	d := phaseDriverWithReleaser(t, h, verifyPhases(), rel)
	logs := captureLogs(t)
	h.onInvoke = func(i int) {
		switch i {
		case 1: // the review raises the MUST the verifier will adjudicate
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-1", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{mustFinding("1.review-1.1", "open", "the seam is untested")},
			})
			rel.openMusts = []string{"1.review-1.1"}
		case 3: // the verify round leaves it open, and the operator drops STOP
			rel.records = append(rel.records, recordFor(2, "review-2", "r-1", testCheckpointTip, "sealed"))
			rel.counted = 2
			rel.openMusts = []string{"1.review-1.1"}
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
	if len(rel.calls) != 1 || rel.calls[0] != (releaseCall{"t-1", "r-1"}) {
		t.Errorf("the held claim must be released at the verify seam: releaser saw %v", rel.calls)
	}
	markerAbsent(t, d.state.Path(), "STOP")
	if out := logs.String(); !strings.Contains(out, "STOP at the phase seam after verify") {
		t.Errorf("the log must name the verify seam; log was:\n%s", out)
	}
}
