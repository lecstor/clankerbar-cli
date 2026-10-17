package loop

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"

	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/delivery"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// phaseDriver builds a driver over one target carrying a releaser, a real state
// dir and the given phases — the shape every test below drives.
//
// Prompt is cleared whenever phases are set, because Validate refuses a config
// carrying both. A test that left it populated would pass while asserting
// nothing about which of the two the driver actually reads.
func phaseDriver(t *testing.T, h harness.Adapter, phases []config.Phase) (*Driver, *fakeReleaser) {
	t.Helper()
	rel := &fakeReleaser{}
	return phaseDriverWithReleaser(t, h, phases, rel), rel
}

// phaseDriverWithReleaser is phaseDriver with the Releaser supplied by the
// caller — the seam the review-split tests use to wire the plane's TaskState
// read (wiredReleaser) while every other phase test stays on the plain,
// unwired fake.
func phaseDriverWithReleaser(t *testing.T, h harness.Adapter, phases []config.Phase, rel plane.Releaser) *Driver {
	t.Helper()
	cfg := fastCfg()
	cfg.Phases = phases
	if len(phases) > 0 {
		cfg.Prompt = ""
	}
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: rel}})
	openTestStateDir(t, d)
	// The implement phase's checkpoint since CLA-457 requires the recorded branch
	// to be VERIFIED on the origin remote, and the evidence check needs a working
	// tree to search. The phase-seam tests stub both: a temp workdir (so the
	// check has somewhere to run) and a verifier that passes whatever branch the
	// session recorded (so a healthy checkpoint fixture advances exactly as it
	// used to). The delivery tests override newVerifier with their own fakes and
	// drive real repositories, and are untouched by this default.
	d.cfg.WorkDir = t.TempDir()
	d.newVerifier = func(string, bool) deliveryVerifier { return passVerifier() }
	// The PR-base probe (CLA-608) is stubbed to "the configured integration
	// branch exists on origin", so a phase prompt fills {{prBase}} with the
	// config's own branch and no test reaches git or the network. The tests
	// that pin the RESOLUTION override this with their own fake.
	d.probePRBase = func(context.Context, string, string) (bool, string, error) { return true, "", nil }
	return d
}

// drainPhasesOnce drives one phase sequence under the same safety timeout the
// other drain helpers use, so a control-flow regression fails fast. The handoff
// count is dropped here — the tests that care about it use drainPhasesHandoffs.
func drainPhasesOnce(t *testing.T, d *Driver) (int, float64, bool, error) {
	t.Helper()
	tokens, cost, _, stop, err := drainPhasesHandoffs(t, d, 1)
	return tokens, cost, stop, err
}

// drainPhasesHandoffs is drainPhasesOnce with the handoff-respawn count kept
// and the drain number scriptable, for the CLA-352 tests: the count is what Run
// charges against max_iterations, and the guard inside drainPhases reads the
// drain number it was handed.
func drainPhasesHandoffs(t *testing.T, d *Driver, drainNum int) (int, float64, int, bool, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return d.drainPhases(ctx, drainNum, 0, d.targets[0], spend{start: time.Now()})
}

// twoPhases is the shipped sequence, by name, so the tests exercise the built-in
// briefs an operator actually gets rather than strings invented here.
func twoPhases() []config.Phase {
	return []config.Phase{{Name: "implement"}, {Name: "review"}}
}

// checkpointed is a phase-1 result that REACHES the checkpoint: a clean exit
// still holding the task, with a branch recorded on the plane and verified on
// the origin remote — the exit evidence a checkpoint now means (CLA-457). A
// clean exit ALONE, still holding the task, is no longer a checkpoint: that is
// the empty shape the 2026-08-24 fleet incident is built on.
func checkpointed(tokens int, cost float64) harness.Result {
	return reported(held(okResult(tokens, cost), openClaim()), branchReport())
}

// testCheckpointTip is the origin tip the default verifier reports for a
// passing branch check (CLA-637). It is what the implement checkpoint carries
// into the review brief as the revision to review, and what a healthy review
// fixture records as its reviewedTip — the seam matches the two by string
// equality, exactly as the real driver matches the plane's SHAs.
const testCheckpointTip = "cafe1234cafe1234cafe1234cafe1234cafe1234"

// passVerifier is the default delivery verifier for the phase-seam tests: a
// BranchPushed check that passes for every branch a fixture recorded, because a
// checkpoint now MEANS a branch verified on the origin remote (CLA-457). The
// tip rides the check because the review seam compares the durable record's
// reviewedTip against it (CLA-637): a passing check that names no tip would be
// a checkpoint the review brief cannot describe. The tests that care what the
// verifier says override newVerifier with their own fake (or a real repository).
func passVerifier() *fakeVerifier {
	return &fakeVerifier{report: delivery.Report{Checks: []delivery.Check{{
		Kind: delivery.BranchPushed, Status: delivery.Pass, Tip: testCheckpointTip,
		Detail: "branch verified on the origin remote",
	}}}}
}

func TestDrainPhases_RunsOneSessionPerPhase(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)},
		{res: okResult(5, 0.05)},
	}}
	d, _ := phaseDriver(t, h, twoPhases())

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}

	if h.invokeCalls != 2 {
		t.Fatalf("expected one session per phase (2), got %d — the whole point is that the second starts on a fresh process", h.invokeCalls)
	}
	p1, p2 := h.invocations[0].Prompt, h.invocations[1].Prompt
	if p1 == p2 {
		t.Fatal("both phases were handed the same prompt; splitting the task buys nothing if each session is asked to do the same thing")
	}
	if !strings.Contains(p1, "PHASE 1") {
		t.Errorf("phase 1 prompt does not scope the session to phase 1: %q", p1)
	}
	if !strings.Contains(p2, "PHASE 2") {
		t.Errorf("phase 2 prompt does not scope the session to phase 2: %q", p2)
	}

	// Distinct logs, and named for their phase — an operator reading the state dir
	// has to be able to tell which session was which.
	logs := iterationLogs(t, d.state.Path())
	if len(logs) != 2 {
		t.Fatalf("expected one iteration log per phase, got %d: %v", len(logs), logs)
	}
	both := logs[0] + " " + logs[1]
	for _, tag := range []string{"-pimplement-", "-preview-"} {
		if !strings.Contains(both, tag) {
			t.Errorf("no iteration log carries the phase tag %q: %v", tag, logs)
		}
	}
}

// The seam. A handback here would post the task to the queue while the driver is
// still working it, and phase 2 would resume a run somebody else may hold.
func TestDrainPhases_HoldsTheClaimAcrossTheSeamAndReleasesOnceAtTheEnd(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(1, 0)},
		// Phase 2 also ends holding the task, so there is something left to hand
		// back and "released once, at the end" is observable rather than vacuous.
		{res: held(okResult(1, 0), openClaim())},
	}}
	d, rel := phaseDriver(t, h, twoPhases())

	var invokesAtRelease []int
	rel.onCall = func() { invokesAtRelease = append(invokesAtRelease, h.invokeCalls) }

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if len(rel.calls) != 1 {
		t.Fatalf("expected exactly one handback for the whole sequence, got %d: %+v", len(rel.calls), rel.calls)
	}
	if invokesAtRelease[0] != 2 {
		t.Errorf("the task was handed back after %d session(s); it must survive the seam and be released only once the sequence ends", invokesAtRelease[0])
	}
}

// The ids a resuming session cannot know for itself: it never called claim_task.
func TestDrainPhases_FillsTheResumePlaceholdersFromTheHeldClaim(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(1, 0)},
		{res: okResult(1, 0)},
	}}
	d, _ := phaseDriver(t, h, twoPhases())

	if _, _, _, err := drainPhasesOnce(t, d); err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	p2 := h.invocations[1].Prompt
	if !strings.Contains(p2, "t-1") || !strings.Contains(p2, "r-1") {
		t.Errorf("phase 2 was not told which run to resume; prompt = %q", p2)
	}
	if strings.Contains(p2, config.PhaseTaskPlaceholder) || strings.Contains(p2, config.PhaseRunPlaceholder) {
		t.Errorf("a placeholder survived into the prompt handed to the session: %q", p2)
	}
}

// A phase-1 session that worked past its brief and finished the task leaves the
// next phase nothing to resume. That is not a failure — the task got done.
func TestDrainPhases_ASettledFirstPhaseEndsTheSequence(t *testing.T) {
	settled := harness.Claim{TaskID: "t-1", RunID: "r-1", Settled: true}
	h := &fakeAdapter{steps: []invokeStep{{res: held(okResult(1, 0), settled)}}}
	d, rel := phaseDriver(t, h, twoPhases())

	_, _, stop, err := drainPhasesOnce(t, d)
	if err != nil {
		t.Fatalf("a settled first phase is not an error, got: %v", err)
	}
	if stop {
		t.Error("a settled first phase ended the sequence AND stopped the run; it should only end the sequence")
	}
	if h.invokeCalls != 1 {
		t.Errorf("spawned %d sessions; phase 2 has nothing to resume once the task is settled", h.invokeCalls)
	}
	if len(rel.calls) != 0 {
		t.Errorf("handed back a settled task: %+v", rel.calls)
	}
}

// The finest granularity the spend ceiling has ever had. One measured task spent
// 92% of a whole run's ceiling inside a single session, which a between-drains
// check can neither see coming nor interrupt.
func TestDrainPhases_BudgetAtTheSeamStopsBeforePhaseTwo(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(500, 0)},
		{res: okResult(1, 0)},
	}}
	d, _ := phaseDriver(t, h, twoPhases())
	d.cfg.Budget = config.Budget{MaxTokens: 100}

	_, _, stop, err := drainPhasesOnce(t, d)
	if err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	if !stop {
		t.Error("the budget was already blown at the seam and the run did not stop")
	}
	if h.invokeCalls != 1 {
		t.Errorf("spawned phase 2 past the ceiling: %d sessions", h.invokeCalls)
	}
}

// What the deferred handback does at a seam the sequence never crosses, in BOTH
// dispositions — because the two differ and only one of them is what production
// actually hits.
//
// The shipped phase-1 brief mandates update_task(taskId, runId, branch), which
// sets HasWIP, which makes the claim non-releasable under CLA-314 so the takeover
// hand-off survives. So the realistic case is "left to expire", not "handed
// back". An earlier version of this test asserted the handback using a fixture
// with HasWIP unset — a claim phase 1 cannot produce — and was green for a
// behaviour the code would never perform.
func TestDrainPhases_WhatTheSeamOwesDependsOnWhetherWorkWasPushed(t *testing.T) {
	for _, tc := range []struct {
		name         string
		claim        harness.Claim
		wantReleases int
	}{
		{
			// Since CLA-457 a clean-exit no-branch phase 1 is the EMPTY shape, not
			// a seam checkpoint: it is released immediately in drainPhase (never
			// held across the seam), so there is nothing for the deferred handback
			// to do here — the release already happened, and the one bounded retry
			// is stopped by the same budget that used to stop the seam.
			name:         "nothing pushed: released as empty, never held across the seam",
			claim:        openClaim(),
			wantReleases: 1,
		},
		{
			name: "work pushed (what phase 1 actually leaves): lease left to expire",
			// update_task(branch:) sets HasWIP; CLA-314 keeps such a claim
			// non-releasable so the task returns as a takeover with its branch.
			claim:        harness.Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true, Branch: "clanker/x"},
			wantReleases: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &fakeAdapter{steps: []invokeStep{
				{res: held(okResult(500, 0), tc.claim)},
				{res: okResult(1, 0)},
			}}
			d, rel := phaseDriver(t, h, twoPhases())
			d.cfg.Budget = config.Budget{MaxTokens: 100}

			if _, _, stop, err := drainPhasesOnce(t, d); err != nil || !stop {
				t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
			}
			if len(rel.calls) != tc.wantReleases {
				t.Errorf("handed the task back %d times, want %d: %+v", len(rel.calls), tc.wantReleases, rel.calls)
			}
		})
	}
}

// The backstop must not become the thing that kills the daemon. A turn-capped
// session exits NON-ZERO and matches neither the limit scan nor the transient
// one, so without its own classification it lands in the non-retryable branch
// and ends the whole run — with the task released half-implemented.
func TestDrainPhases_ATurnCappedPhaseIsACheckpointNotAFatalExit(t *testing.T) {
	for _, tc := range []struct {
		name        string
		claim       harness.Claim
		wantInvokes int
	}{
		{
			// The salvage recorded a branch (or the phase did), so a checkpoint
			// genuinely exists for phase 2 to resume from.
			name:        "capped with work pushed: phase 2 resumes it",
			claim:       harness.Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true, Branch: "clanker/x"},
			wantInvokes: 2,
		},
		{
			// Nothing durable came out of it: a phase that burned its turns
			// reading and planning leaves a CLEAN tree, and the salvage commits
			// nothing on a clean tree (and refuses outright mid-merge). Spawning
			// phase 2 here would hand a review brief a branch that is not there
			// and tell it to move the task to in_review.
			name:        "capped with nothing pushed: no checkpoint to hand on",
			claim:       openClaim(),
			wantInvokes: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &fakeAdapter{steps: []invokeStep{
				{res: held(turnCappedResult(), tc.claim)},
				{res: okResult(1, 0)},
			}}
			d, _ := phaseDriver(t, h, []config.Phase{
				{Name: "implement", MaxTurns: 5},
				{Name: "review"},
			})

			_, _, stop, err := drainPhasesOnce(t, d)
			if err != nil {
				t.Fatalf("a turn-capped phase ended the RUN: %v — a hit cap is the phase ending, never the daemon failing", err)
			}
			if stop {
				t.Error("a turn-capped phase stopped the run")
			}
			if h.invokeCalls != tc.wantInvokes {
				t.Errorf("spawned %d sessions, want %d", h.invokeCalls, tc.wantInvokes)
			}
		})
	}
}

// The per-session token ceiling (CLA-343) is the same shape as the turn cap: the
// adapter killed the session mid-stream for crossing Invocation.MaxSessionTokens,
// and the driver must read that as the phase ending — never a failure that stops
// the daemon, never a retry that re-spends against the same runaway ceiling.
// With work pushed, a ceiling-hit phase is even a legitimate checkpoint.
func TestDrainPhases_ATokenCeilingHitIsACheckpointNotAFatalExit(t *testing.T) {
	for _, tc := range []struct {
		name        string
		claim       harness.Claim
		wantInvokes int
	}{
		{
			name:        "killed with work pushed: phase 2 resumes it",
			claim:       harness.Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true, Branch: "clanker/x"},
			wantInvokes: 2,
		},
		{
			name:        "killed with nothing pushed: no checkpoint to hand on",
			claim:       openClaim(),
			wantInvokes: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &fakeAdapter{steps: []invokeStep{
				{res: held(tokenCeilingResult(), tc.claim)},
				{res: okResult(1, 0)},
			}}
			d, _ := phaseDriver(t, h, []config.Phase{
				{Name: "implement", MaxTurns: 5},
				{Name: "review"},
			})

			_, _, stop, err := drainPhasesOnce(t, d)
			if err != nil {
				t.Fatalf("a ceiling-killed phase ended the RUN: %v — the kill was the point, not a fault", err)
			}
			if stop {
				t.Error("a ceiling-killed phase stopped the run")
			}
			if h.invokeCalls != tc.wantInvokes {
				t.Errorf("spawned %d sessions, want %d", h.invokeCalls, tc.wantInvokes)
			}
		})
	}
}

// The ceiling must not be retried, in either direction the driver could go
// wrong: not as a transient blip (a fresh session would re-spend against the
// same ceiling) and not as a launch failure.
func TestDrainPhases_ATokenCeilingFinalPhaseEndsTheDrainWithoutFailingTheRun(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{{res: held(tokenCeilingResult(), openClaim())}}}
	d, _ := phaseDriver(t, h, nil)
	d.cfg.Prompt = "Work the next backlog item."

	_, _, stop, err := drainPhasesOnce(t, d)
	if err != nil {
		t.Fatalf("a ceiling-killed final phase ended the run: %v", err)
	}
	if stop {
		t.Error("a ceiling-killed final phase stopped the run")
	}
	if h.invokeCalls != 1 {
		t.Errorf("spawned %d sessions; a ceiling kill must never be retried", h.invokeCalls)
	}
}

// The resolved ceiling reaches every session's invocation: invocationFor reads
// Budget.SessionTokenCeiling, so the operator's dial and the 2x/default fallbacks
// all arrive at the adapter. Pinned because a wire that silently drops the field
// makes the whole mid-stream kill inert while every test stays green.
func TestDrainPhases_CarriesTheResolvedSessionTokenCeiling(t *testing.T) {
	d, _ := phaseDriver(t, &fakeAdapter{}, twoPhases())
	d.cfg.Budget = config.Budget{MaxTokens: 75_000_000}

	phases := d.cfg.EffectivePhases()
	inv, err := d.invocationFor(d.targets[0], 0, phases[0], nil, "", "", phases)
	if err != nil {
		t.Fatalf("invocationFor: %v", err)
	}

	if want := d.cfg.Budget.SessionTokenCeiling(); inv.MaxSessionTokens != want {
		t.Errorf("invocation carries MaxSessionTokens %d, want the resolved %d", inv.MaxSessionTokens, want)
	}
	if inv.MaxSessionTokens != 150_000_000 {
		t.Errorf("MaxSessionTokens = %d, want 2x the 75M run ceiling (150M)", inv.MaxSessionTokens)
	}
}

// Seeding made the seam's handback live for a resumed phase, which turned a
// transient blip into a task handed back mid-sequence while the retry is given
// the same heartbeat(runId) brief — and MaxRetries defaults to 0, "never give
// up", so the ladder walks past the 30-minute lease and re-spawns indefinitely
// against a run the plane has swept.
// CLA-651: a resumed phase must not RETRY a transient failure — the backoff
// ladder walks past the 30-minute lease nothing driver-side renews — and it
// must not exit the DAEMON either: a single provider blip landing on the
// review phase of every task used to take the whole fleet down, the same
// failure CLA-631 fixed for usage limits. The sequence ends on its own, the
// run carries on, and the claim is disposed of exactly as the usage-limit
// arm's is: pushed work keeps its takeover hand-off, a releasable claim goes
// back to the queue.
func TestDrainPhases_AResumedPhaseDoesNotRetryATransientFailure(t *testing.T) {
	t.Run("pushed work keeps its takeover hand-off", func(t *testing.T) {
		wip := harness.Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true, Branch: "clanker/x"}
		h := &fakeAdapter{steps: []invokeStep{
			{res: checkpointed(1, 0)},
			{res: held(transientResult(), wip)},
		}}
		d, rel := phaseDriver(t, h, twoPhases())

		_, _, stop, err := drainPhasesOnce(t, d)
		if err != nil {
			t.Fatalf("drainPhases: %v", err)
		}
		if stop {
			t.Error("a resumed phase hit a transient failure and the run stopped; the sequence must end without the daemon exiting")
		}
		if h.invokeCalls != 2 {
			t.Errorf("spawned %d sessions; a resumed phase must not be retried on a lease nothing renews", h.invokeCalls)
		}
		if len(rel.calls) != 0 {
			t.Errorf("handed the task back to the queue: %+v — pushed work must keep its takeover hand-off", rel.calls)
		}
	})

	t.Run("a claim with nothing pushed is released", func(t *testing.T) {
		h := &fakeAdapter{steps: []invokeStep{
			{res: checkpointed(1, 0)},
			{res: held(transientResult(), openClaim())},
		}}
		d, rel := phaseDriver(t, h, twoPhases())

		_, _, stop, err := drainPhasesOnce(t, d)
		if err != nil {
			t.Fatalf("drainPhases: %v", err)
		}
		if stop {
			t.Error("a resumed phase hit a transient failure and the run stopped")
		}
		if h.invokeCalls != 2 {
			t.Errorf("spawned %d sessions; a resumed phase must not be retried on a lease nothing renews", h.invokeCalls)
		}
		if len(rel.calls) != 1 {
			t.Fatalf("handed the task back %d times, want 1: %+v — a claim with nothing pushed is releasable, exactly as on the limit arm", len(rel.calls), rel.calls)
		}
	})
}

// The sequence-end break lands BEFORE the dead/fleet/empty arms, so a blip's
// exit shape must not feed the dead-phase machinery: those counters would tick
// on a transient failure that happens to wear the "unknown" finish reason, and
// the park/fleet machinery exists for silent deaths the provider did not own.
// Three phases so the resumed phase is not last, where the dead signature is
// live; the exit is BOTH transient and dead-shaped.
func TestDrainPhases_AResumedPhaseTransientExitDoesNotTickTheDeadCounters(t *testing.T) {
	blip := transientResult()
	blip.Raw[harness.FinishReasonKey] = harness.FinishReasonUnknown
	three := []config.Phase{{Name: "implement"}, {Name: "review"}, {Name: "publish"}}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(1, 0)},
		{res: held(blip, openClaim())},
		{res: okResult(1, 0)}, // phase 3 would land here; it must not
	}}
	// The resumed phase is the review-only one, whose spawn reads the task's
	// outcome for the seam's freshness baseline (CLA-663): a wired plane is the
	// production shape, and an unwired one would be refused before the blip
	// this test is about could happen.
	rel := &wiredReleaser{state: func() plane.TaskState {
		return plane.TaskState{Status: "in_progress", ClaimedByRun: "r-1", Outcome: "implemented: pushed clanker/x\n"}
	}}
	d := phaseDriverWithReleaser(t, h, three, rel)

	_, _, stop, err := drainPhasesOnce(t, d)
	if err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	if stop {
		t.Error("a resumed phase hit a transient failure and the run stopped")
	}
	if h.invokeCalls != 2 {
		t.Errorf("spawned %d sessions; the sequence must end at the blipped phase, not advance past it", h.invokeCalls)
	}
	if got := d.fleetDead[0]; got != 0 {
		t.Errorf("fleet dead counter = %d, want 0 — the break must land before the dead arm, or a provider blip ticks a counter that exists to catch silent deaths", got)
	}
	if len(rel.calls) != 1 {
		t.Fatalf("handed the task back %d times, want 1: %+v — a claim with nothing pushed is releasable", len(rel.calls), rel.calls)
	}
}

// CLA-262 applied to the seam. A phase whose stream could not be read whole may
// have settled its task in the bytes that never arrived, so its claim state is
// not evidence of anything — handing it across would spawn a session to resume a
// run that may already be in review.
func TestDrainPhases_AnUntrustedStreamNeverHandsOnACheckpoint(t *testing.T) {
	res := held(okResult(1, 0), openClaim())
	res.Untrusted = "a line overran the reader"
	h := &fakeAdapter{steps: []invokeStep{{res: res}, {res: okResult(1, 0)}}}
	d, rel := phaseDriver(t, h, twoPhases())

	if _, _, _, err := drainPhasesOnce(t, d); err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	if h.invokeCalls != 1 {
		t.Errorf("spawned %d sessions; a stream we could not read whole is not a checkpoint to build on", h.invokeCalls)
	}
	if len(rel.calls) != 0 {
		t.Errorf("released a task from an unreadable stream: %+v — CLA-262 says let the lease expire", rel.calls)
	}
}

// An off-brief phase 2 that claims a DIFFERENT task must not strand the one it
// was resuming, nor hand back a task the sequence was never meant to touch.
func TestDrainPhases_AnOffBriefClaimDoesNotStrandThePredecessorsTask(t *testing.T) {
	other := harness.Claim{TaskID: "t-999", RunID: "r-999"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(1, 0)},
		{res: held(okResult(1, 0), other)},
	}}
	d, rel := phaseDriver(t, h, twoPhases())

	if _, _, _, err := drainPhasesOnce(t, d); err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	var sawOriginal bool
	for _, c := range rel.calls {
		if c.taskID == "t-1" {
			sawOriginal = true
		}
	}
	if !sawOriginal {
		t.Errorf("phase 1's task t-1 was never handed back: %+v — an off-brief claim replaced the observed claim wholesale, so the task the sequence was actually working got stranded on a lease nobody renews", rel.calls)
	}
}

func TestDrainPhases_ATurnCappedFinalPhaseEndsTheDrainWithoutFailingTheRun(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{{res: held(turnCappedResult(), openClaim())}}}
	d, _ := phaseDriver(t, h, nil)
	d.cfg.Prompt = "Work the next backlog item."

	_, _, stop, err := drainPhasesOnce(t, d)
	if err != nil {
		t.Fatalf("a turn-capped final phase ended the run: %v", err)
	}
	if stop {
		t.Error("a turn-capped final phase stopped the run")
	}
}

// Everything the driver does about a task hangs off Result.Claim.Held(): the
// handback, the CLA-314 salvage, the CLA-253 delivery check. A resumed session is
// told not to claim, so without seeding, the phase that pushes the branch and
// opens the PR is the one running with all three switched off.
func TestDrainPhases_SeedsTheResumedSessionsClaim(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(1, 0)},
		{res: okResult(1, 0)},
	}}
	d, _ := phaseDriver(t, h, twoPhases())

	if _, _, _, err := drainPhasesOnce(t, d); err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	if got := h.invocations[1].ResumeClaim; got.TaskID != "t-1" || got.RunID != "r-1" {
		t.Errorf("phase 2 was invoked with ResumeClaim %+v, want the claim phase 1 held; without it the salvage, the handback and the delivery check are all inert for the phase that does the pushing", got)
	}
	if got := h.invocations[0].ResumeClaim; got.TaskID != "" {
		t.Errorf("phase 1 was seeded with a claim %+v; it claims for itself", got)
	}
}

// The clobbering case: a phase that reports no claim of its own must not erase
// the one its predecessor is still holding, or the handback silently goes missing
// for exactly the failure it exists to cover.
func TestDrainPhases_APhaseThatNeverLaunchedKeepsThePredecessorsClaim(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(1, 0)},
		// A launch failure: a zero Result alongside an error, carrying no claim.
		{res: harness.Result{}, err: errors.New("exec: claude not found")},
	}}
	d, rel := phaseDriver(t, h, twoPhases())

	if _, _, _, err := drainPhasesOnce(t, d); err == nil {
		t.Fatal("a launch failure should still be an error")
	}
	if len(rel.calls) != 1 {
		t.Fatalf("phase 1's task was handed back %d times, want 1: %+v — phase 2 never launched, so it observed no claim, and its silence must not erase one that is still live", len(rel.calls), rel.calls)
	}
	if rel.calls[0].taskID != "t-1" {
		t.Errorf("handed back %q, want the task phase 1 was holding", rel.calls[0].taskID)
	}
}

// Pre-phases, the handback ran on EVERY attempt, so no wait could leave a lease
// unattended. A seam hold must not survive into a retry that re-claims.
func TestDrainPhases_AHoldDoesNotSurviveAUsageLimitRetry(t *testing.T) {
	// Exit 0 AND a usage limit: the loop checks the limit before the exit code,
	// and no adapter's DetectLimit consults the exit code at all.
	capped := held(okResult(1, 0), openClaim())
	capped.Raw = map[string]any{"kind": "limit"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: capped},
		{res: checkpointed(1, 0)},
		{res: okResult(1, 0)},
	}}
	d, rel := phaseDriver(t, h, twoPhases())

	if _, _, _, err := drainPhasesOnce(t, d); err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	if len(rel.calls) == 0 {
		t.Error("the claim held open by the limited attempt was never handed back; the retry re-claims, so the hold had to be undone first")
	}
}

// CLA-631: a resumed phase is working a live 30-minute lease that nothing
// driver-side heartbeats, so the PHASE must not wait the limit out — waiting
// hours would re-spawn it against a run the plane has swept or handed to a
// takeover. But the RUN must not exit with the sequence either: the fleet sat
// idle until an operator noticed. The sequence ends, the wait moves to
// drainPhases' post-handback stage (holding no lease, on the harness that hit
// the limit), and Run carries on polling when it lifts.
func TestDrainPhases_AResumedPhaseLimitEndsTheSequenceButNotTheRun(t *testing.T) {
	// The realistic claim: phase 1 pushed work and recorded the branch, so the
	// task's takeover hand-off must survive — releaseHeldClaim declines to
	// release a claim with WIP, and the lease is left to expire.
	wip := harness.Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true, Branch: "clanker/x"}
	h := &fakeAdapter{
		steps: []invokeStep{
			{res: checkpointed(1, 0)},
			{res: held(limitResult(), wip)},
		},
		limitResetAt: time.Now().Add(2 * time.Hour),
	}
	d, rel := phaseDriver(t, h, twoPhases())

	_, _, stop, err := drainPhasesOnce(t, d)
	if err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	if stop {
		t.Error("a resumed phase hit a rolling usage limit and the run stopped; the sequence must end without the daemon exiting")
	}
	if h.invokeCalls != 2 {
		t.Errorf("spawned %d sessions; the resumed phase must not be re-run on a lease nothing renews", h.invokeCalls)
	}
	if h.probeCalls == 0 {
		t.Error("the limit was never waited out — the run returned to poll with the harness still down")
	}
	if len(rel.calls) != 0 {
		t.Errorf("handed the task back to the queue: %+v — pushed work must keep its takeover hand-off", rel.calls)
	}
}

// A resumed phase that is NOT the last must end the sequence too: the wait is
// owed, but no later phase may spawn against a run whose lease is lapsing.
func TestDrainPhases_AResumedPhaseLimitDoesNotAdvanceToTheNextPhase(t *testing.T) {
	three := []config.Phase{{Name: "implement"}, {Name: "review"}, {Name: "publish"}}
	h := &fakeAdapter{
		steps: []invokeStep{
			{res: checkpointed(1, 0)},
			{res: held(limitResult(), openClaim())},
			{res: okResult(1, 0)}, // phase 3 would land here; it must not
		},
		limitResetAt: time.Now().Add(2 * time.Hour),
	}
	// The resumed phase is the review-only one, whose spawn reads the task's
	// outcome for the seam's freshness baseline (CLA-663): the wired plane is
	// the production shape.
	rel := &wiredReleaser{state: func() plane.TaskState {
		return plane.TaskState{Status: "in_progress", ClaimedByRun: "r-1", Outcome: "implemented: pushed clanker/x\n"}
	}}
	d := phaseDriverWithReleaser(t, h, three, rel)

	_, _, stop, err := drainPhasesOnce(t, d)
	if err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	if stop {
		t.Error("the run stopped on a rolling limit a resumed phase hit")
	}
	if h.invokeCalls != 2 {
		t.Errorf("spawned %d sessions; the sequence must end at the limited resumed phase, not advance to phase 3", h.invokeCalls)
	}
	if h.probeCalls == 0 {
		t.Error("the limit was never waited out")
	}
}

// The other disposition of the same path, unchanged from the old stop: a
// resumed phase's limit turn-away with NO pushed work still hands the claim
// back, because the takeover hand-off exists only to carry a branch.
func TestDrainPhases_AResumedPhaseLimitStillReleasesAClaimWithNoWork(t *testing.T) {
	h := &fakeAdapter{
		steps: []invokeStep{
			{res: checkpointed(1, 0)},
			{res: held(limitResult(), openClaim())},
		},
		limitResetAt: time.Now().Add(2 * time.Hour),
	}
	d, rel := phaseDriver(t, h, twoPhases())

	_, _, stop, err := drainPhasesOnce(t, d)
	if err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	if stop {
		t.Error("the run stopped on a rolling limit a resumed phase hit")
	}
	if len(rel.calls) != 1 {
		t.Fatalf("handed the task back %d times, want 1: %+v — a claim with nothing pushed is releasable, exactly as before", len(rel.calls), rel.calls)
	}
}

// A hard stop is not a rolling limit and still stops the run: there is no
// reset to wait for, so a resumed phase's sequence ends and the daemon exits
// for the operator to resume once they have topped up (lim.Stop).
func TestDrainPhases_AResumedPhaseHardStopStillStopsTheRun(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(1, 0)},
		{res: held(limitStopResult(), openClaim())},
	}}
	d, _ := phaseDriver(t, h, twoPhases())

	_, _, stop, err := drainPhasesOnce(t, d)
	if err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	if !stop {
		t.Error("a hard limit stop on a resumed phase did not stop the run")
	}
	if h.probeCalls != 0 {
		t.Errorf("probed %d times for a reset a hard stop does not have", h.probeCalls)
	}
}

// The stated reset is checked against the run's remaining ceiling exactly as on
// the first-phase path (waitPastBudget): a reset beyond it is not worth
// sleeping through, so the run stops then and there rather than waiting hours
// to run one session against the fresh quota.
func TestDrainPhases_AResumedPhaseLimitPastTheBudgetStops(t *testing.T) {
	h := &fakeAdapter{
		steps: []invokeStep{
			{res: checkpointed(1, 0)},
			{res: held(limitResult(), openClaim())},
		},
		limitResetAt: time.Now().Add(3 * time.Hour),
	}
	d, _ := phaseDriver(t, h, twoPhases())
	d.cfg.Budget = config.Budget{MaxWallClock: config.Duration(30 * time.Minute)}

	logs := captureLogs(t)
	_, _, stop, err := drainPhasesOnce(t, d)
	if err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	if !stop {
		t.Error("a reset past the run's remaining ceiling did not stop the run")
	}
	if h.probeCalls != 0 {
		t.Errorf("entered the wait (%d probes) for a reset waitPastBudget says not to wait for", h.probeCalls)
	}
	if out := logs.String(); !strings.Contains(out, "start a fresh run after the reset") {
		t.Errorf("the stop must say when to start a fresh run; log was:\n%s", out)
	}
}

// STOP lands mid-wait and is consumed there, not at some later boundary: the
// wait is a waitOrStop, exactly as on the first-phase path.
func TestDrainPhases_AResumedPhaseLimitWaitHonoursStop(t *testing.T) {
	h := &fakeAdapter{
		steps: []invokeStep{
			{res: checkpointed(1, 0)},
			{res: held(limitResult(), openClaim())},
		},
		limitResetAt: time.Now().Add(2 * time.Hour),
	}
	d, _ := phaseDriver(t, h, twoPhases())
	// Plant the soft stop during the review session, against the live daemon —
	// the wait's own waitOrStop is the first read that can see it.
	const dirMarker = "STOP"
	h.onInvoke = func(call int) {
		if call == 1 {
			writeMarker(t, d.state.Path(), dirMarker, "stop the paused daemon")
		}
	}

	logs := captureLogs(t)
	_, _, stop, err := drainPhasesOnce(t, d)
	if err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	if !stop {
		t.Error("a STOP dropped during the resumed-phase limit wait did not stop the run")
	}
	if h.probeCalls != 0 {
		t.Errorf("the wait probed %d times past a STOP it should have consumed first", h.probeCalls)
	}
	if markerPresent(t, d.state.Path(), dirMarker) {
		t.Error("STOP must be consumed on stop so a restart is not stillborn")
	}
	if out := logs.String(); !strings.Contains(out, "STOP requested during wait") {
		t.Errorf("the stop must be logged as landing IN the wait; log was:\n%s", out)
	}
}

// The incident this fix exists for (CLA-631), end to end: the fleet's review
// phase hit the Claude session limit and every daemon exited with the queue
// full, idling until an operator noticed. The run must instead wait the limit
// out and poll again — a second sequence spawning after the wait is the proof
// it is still alive.
func TestRun_ResumedPhaseLimitKeepsTheDaemonDraining(t *testing.T) {
	h := &fakeAdapter{
		steps: []invokeStep{
			{res: checkpointed(1, 0)},               // iteration 1: implement checkpoints
			{res: held(limitResult(), openClaim())}, // iteration 1: review turned away by a rolling limit
			{res: checkpointed(1, 0)},               // iteration 2: implement spawns again after the wait
			{res: okResult(1, 0)},                   // iteration 2: review runs
		},
		limitResetAt: time.Now().Add(2 * time.Hour),
	}
	d, _ := phaseDriver(t, h, twoPhases())
	d.cfg.MaxIterations = 2
	ff := &fakeFleet{}
	d.targets[0].Fleet = ff

	logs := captureLogs(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if h.invokeCalls != 4 {
		t.Fatalf("spawned %d sessions, want 4 — the daemon must wait the limit out and run a second sequence rather than exiting with the first", h.invokeCalls)
	}
	if h.probeCalls == 0 {
		t.Error("the limit was never waited out before the second sequence spawned")
	}
	if got := h.invocations[2].Prompt; !strings.Contains(got, "PHASE 1") {
		t.Errorf("the session spawned after the limit is not a fresh sequence: %q", got)
	}
	if out := logs.String(); !strings.Contains(out, "waiting for the reset") {
		t.Errorf("the log must say the run is waiting for the reset, not only that the sequence ended; log was:\n%s", out)
	}
	// The wait must not swallow the iteration record (CLA-466): one record per
	// drain, and the limited drain's record still names the sequence it ran.
	recs := ff.records()
	if len(recs) != 2 {
		t.Fatalf("posted %d iteration records, want one per drain (2): %+v", len(recs), recs)
	}
	if recs[0].TaskID != "t-1" || len(recs[0].Phases) != 2 {
		t.Errorf("the limited drain's record is not the two-phase sequence it ran: %+v", recs[0])
	}
}

// The wait is the first-phase path's own waitOrStop, so the control markers
// that path honours land here too — STOP is pinned above; RESTART is tested
// because it sets the re-exec flag, and HALT because it is deliberately NOT
// read during a wait: it must survive the wait and land at the boundary after
// it, exactly as on the first-phase path.
func TestRun_ResumedPhaseLimitWaitHonoursMarkers(t *testing.T) {
	t.Run("RESTART lands in the wait and asks for the re-exec", func(t *testing.T) {
		h := &fakeAdapter{
			steps: []invokeStep{
				{res: checkpointed(1, 0)},
				{res: held(limitResult(), openClaim())},
			},
			limitResetAt: time.Now().Add(2 * time.Hour),
		}
		d, _ := phaseDriver(t, h, twoPhases())
		d.cfg.MaxIterations = 10
		h.onInvoke = func(call int) {
			if call == 1 {
				writeMarker(t, d.state.Path(), MarkerRestart, "new build installed")
			}
		}

		logs := captureLogs(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := d.Run(ctx); err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if !d.restartRequested.Load() {
			t.Error("a RESTART dropped during the resumed-phase limit wait did not ask for the re-exec")
		}
		if h.probeCalls != 0 {
			t.Errorf("the wait probed %d times past a RESTART it should have consumed first", h.probeCalls)
		}
		if markerPresent(t, d.state.Path(), MarkerRestart) {
			t.Error("RESTART must be consumed when honoured")
		}
		if out := logs.String(); !strings.Contains(out, "RESTART requested during wait") {
			t.Errorf("the restart must be logged as landing IN the wait; log was:\n%s", out)
		}
	})

	t.Run("HALT survives the wait and lands at the boundary after it", func(t *testing.T) {
		h := &fakeAdapter{
			steps: []invokeStep{
				{res: checkpointed(1, 0)},
				{res: held(limitResult(), openClaim())},
			},
			limitResetAt: time.Now().Add(2 * time.Hour),
		}
		d, _ := phaseDriver(t, h, twoPhases())
		d.cfg.MaxIterations = 10
		h.onInvoke = func(call int) {
			if call == 1 {
				writeMarker(t, d.state.Path(), "HALT", "wedged — needs a human")
			}
		}

		logs := captureLogs(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := d.Run(ctx); err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		// The limit was waited out (the run stayed up and probed) and only then
		// did the boundary read find the HALT. If the wait had swallowed it, the
		// daemon would have spawned another sequence; if the run had exited at
		// the limit instead, no probe would have run at all.
		if h.probeCalls == 0 {
			t.Error("the run exited at the limit instead of waiting it out")
		}
		if h.invokeCalls != 2 {
			t.Errorf("spawned %d sessions; the HALT must land before the next sequence", h.invokeCalls)
		}
		if !markerPresent(t, d.state.Path(), "HALT") {
			t.Error("HALT must be left in place for the operator")
		}
		out := logs.String()
		if !strings.Contains(out, "waiting for the reset") || !strings.Contains(out, "HALT present") {
			t.Errorf("the wait and the HALT must both appear in the log; log was:\n%s", out)
		}
	})
}

// The incident CLA-651 exists for, end to end: one transient blip (a claude
// 5xx, a stream drop) landing on a resumed phase — the review phase of every
// task — must not take the whole daemon down. The sequence ends, the run stays
// up and polls again; a second sequence spawning is the proof. Since CLA-662
// the poll is not immediate: the run owes the target's transient backoff rung,
// taken after the sequence's handback with no lease held.
func TestRun_ResumedPhaseTransientFailureKeepsTheDaemonDraining(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(1, 0)},                   // iteration 1: implement checkpoints
		{res: held(transientResult(), openClaim())}, // iteration 1: review blips
		{res: checkpointed(1, 0)},                   // iteration 2: implement spawns again
		{res: okResult(1, 0)},                       // iteration 2: review runs
	}}
	d, rel := phaseDriver(t, h, twoPhases())
	d.cfg.MaxIterations = 2

	var waitAt time.Time
	d.resumedTransientWait = func(n int) time.Duration {
		if n != 1 {
			t.Errorf("the first blip waited rung %d, want 1", n)
		}
		waitAt = time.Now()
		return time.Millisecond
	}
	var firstRelease time.Time
	rel.onCall = func() {
		if firstRelease.IsZero() {
			firstRelease = time.Now()
		}
	}

	logs := captureLogs(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if h.invokeCalls != 4 {
		t.Fatalf("spawned %d sessions, want 4 — the daemon must poll on after the blip rather than exiting with the first sequence", h.invokeCalls)
	}
	if got := h.invocations[2].Prompt; !strings.Contains(got, "PHASE 1") {
		t.Errorf("the session spawned after the blip is not a fresh sequence: %q", got)
	}
	if h.probeCalls != 0 {
		t.Errorf("probed %d times — a blip has no reset to wait out; the wait owed is the transient ladder, not a supervised limit wait", h.probeCalls)
	}
	if waitAt.IsZero() {
		t.Fatal("the run re-drained without a wait of its own — with the resumed phase on another harness nothing else bounds the loop")
	}
	// The handback for the failed sequence runs BEFORE the wait (drainPhases'
	// deferred release, then the wait block): the wait is taken holding no
	// lease. The drain's own final handback releases again later in the run,
	// so the FIRST release is the one the wait must follow.
	if firstRelease.IsZero() || !firstRelease.Before(waitAt) {
		t.Errorf("the wait must come after the failed sequence's handback, holding no lease; first release at %s, wait at %s",
			firstRelease, waitAt)
	}
	if out := logs.String(); !strings.Contains(out, "the run stays up to poll again") || !strings.Contains(out, "transient failure 1 in a row") {
		t.Errorf("the log must say the run is staying up and name the streak the wait is on; log was:\n%s", out)
	}
}

// CLA-662, the gap CLA-651 left: the resumed phase runs on a DIFFERENT harness
// from the one that claims the queue — the README's own per-phase example
// (implement opencode, review claude) — so the first-phase transient arm never
// sees the failure and nothing in the run bounds the re-drain. Each drain
// claims a task, implements and pushes it, and the review blips instantly; with
// no wait owed, the run re-drains at once and strands a task per cycle. The run
// must instead wait a backoff that GROWS with the consecutive blips, and start
// the ladder over as soon as a resumed phase on that target completes.
func TestRun_ResumedPhaseTransientBackoffGrowsAndResets(t *testing.T) {
	impl := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(1, 0)},
		{res: checkpointed(1, 0)},
		{res: checkpointed(1, 0)},
		{res: checkpointed(1, 0)},
		{res: checkpointed(1, 0)},
		{res: checkpointed(1, 0)},
	}}
	blip := func() invokeStep { return invokeStep{res: held(transientResult(), openClaim())} }
	review := &fakeAdapter{steps: []invokeStep{
		blip(),
		blip(),
		blip(),
		{res: okResult(1, 0)}, // review completes: the streak resets...
		blip(),                // ...so this blip is the first rung again
		{res: okResult(1, 0)}, // and the run ends on a completed sequence
	}}
	d := mixedDriver(t, impl, review)
	d.cfg.MaxIterations = 6

	var rungs []int
	type waitRec struct {
		rung int
		at   time.Time
	}
	var waits []waitRec
	d.resumedTransientWait = func(n int) time.Duration {
		rungs = append(rungs, n)
		waits = append(waits, waitRec{n, time.Now()})
		return time.Duration(n) * 50 * time.Millisecond
	}
	var implAt []time.Time
	impl.onInvoke = func(int) { implAt = append(implAt, time.Now()) }

	logs := captureLogs(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	want := []int{1, 2, 3, 1}
	if len(rungs) != len(want) {
		t.Fatalf("waited backoff rungs %v, want %v — each consecutive blip on the target owes the next rung, and a completed review resets the ladder", rungs, want)
	}
	for i := range want {
		if rungs[i] != want[i] {
			t.Fatalf("waited backoff rungs %v, want %v — the ladder must grow with consecutive blips and reset when a resumed phase on the target completes", rungs, want)
		}
	}
	if impl.invokeCalls != 6 || review.invokeCalls != 6 {
		t.Fatalf("implement ran %d session(s), review %d, want 6 each — the run must keep draining while the blips persist", impl.invokeCalls, review.invokeCalls)
	}
	// The waits are real, not bookkeeping: the implement of the NEXT drain
	// starts only after the rung's floor has elapsed (50ms a rung). A rung
	// that did not grow would fail the floor below, not just the rung list.
	// The rung list alone cannot map waits to drains — drain 4 completes and
	// owes none — so each wait is judged against the next implement after it.
	for _, w := range waits {
		next := time.Time{}
		for _, at := range implAt {
			if !at.Before(w.at) {
				next = at
				break
			}
		}
		if next.IsZero() {
			t.Fatalf("rung %d's wait was never followed by another implement — the fixture does not cover the wait it owes", w.rung)
		}
		if gap := next.Sub(w.at); gap < time.Duration(w.rung)*50*time.Millisecond {
			t.Errorf("the implement after rung %d's wait started %s after the wait began, less than the rung's floor %s",
				w.rung, gap.Round(time.Millisecond), time.Duration(w.rung)*50*time.Millisecond)
		}
	}
	if out := logs.String(); !strings.Contains(out, "transient failure 3 in a row") {
		t.Errorf("the log must name the streak the wait is on; log was:\n%s", out)
	}
}

// The transient wait is waitOrStop's — the same machinery the limit wait uses —
// so the control markers land here too: STOP/RESTART stop the run through it
// (consumed by the WAIT, which is what the log line distinguishes), and RELOAD
// applies in place and lets the wait finish so the run keeps draining.
func TestRun_ResumedPhaseTransientWaitHonoursMarkers(t *testing.T) {
	t.Run("STOP lands in the wait and stops the run", func(t *testing.T) {
		h := &fakeAdapter{steps: []invokeStep{
			{res: checkpointed(1, 0)},
			{res: held(transientResult(), openClaim())},
		}}
		d, _ := phaseDriver(t, h, twoPhases())
		d.cfg.MaxIterations = 10
		h.onInvoke = func(call int) {
			if call == 0 {
				writeMarker(t, d.state.Path(), "STOP", "")
			}
		}

		logs := captureLogs(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := d.Run(ctx); err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if h.invokeCalls != 2 {
			t.Errorf("spawned %d sessions; a STOP dropped during the failing sequence must land in the wait, before the next sequence", h.invokeCalls)
		}
		if markerPresent(t, d.state.Path(), "STOP") {
			t.Error("STOP must be consumed when honoured")
		}
		if out := logs.String(); !strings.Contains(out, "STOP requested during wait") {
			t.Errorf("the STOP must be consumed by the WAIT, not by a boundary after it; log was:\n%s", out)
		}
	})

	t.Run("RESTART lands in the wait and asks for the re-exec", func(t *testing.T) {
		h := &fakeAdapter{steps: []invokeStep{
			{res: checkpointed(1, 0)},
			{res: held(transientResult(), openClaim())},
		}}
		d, _ := phaseDriver(t, h, twoPhases())
		d.cfg.MaxIterations = 10
		h.onInvoke = func(call int) {
			if call == 0 {
				writeMarker(t, d.state.Path(), MarkerRestart, "new build installed")
			}
		}

		logs := captureLogs(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := d.Run(ctx); err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if !d.restartRequested.Load() {
			t.Error("a RESTART dropped during the resumed-phase transient wait did not ask for the re-exec")
		}
		if h.invokeCalls != 2 {
			t.Errorf("spawned %d sessions; the RESTART must stop the run at the wait", h.invokeCalls)
		}
		if markerPresent(t, d.state.Path(), MarkerRestart) {
			t.Error("RESTART must be consumed when honoured")
		}
		if out := logs.String(); !strings.Contains(out, "RESTART requested during wait") {
			t.Errorf("the restart must be logged as landing IN the wait; log was:\n%s", out)
		}
	})

	t.Run("RELOAD applies in the wait and the run keeps draining", func(t *testing.T) {
		h := &fakeAdapter{steps: []invokeStep{
			{res: checkpointed(1, 0)},
			{res: held(transientResult(), openClaim())},
			{res: checkpointed(1, 0)},
			{res: okResult(1, 0)},
		}}
		d, _ := phaseDriver(t, h, twoPhases())
		d.cfg.MaxIterations = 10
		reloads := 0
		d.SetReloader(func() (*config.Config, error) {
			reloads++
			fresh := *d.cfg
			// Proof the reload really landed mid-wait: the fresh document
			// stops the run after the second drain.
			fresh.MaxIterations = 2
			return &fresh, nil
		})
		h.onInvoke = func(call int) {
			if call == 0 {
				writeMarker(t, d.state.Path(), MarkerReload, "")
			}
		}

		logs := captureLogs(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := d.Run(ctx); err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if reloads != 1 {
			t.Errorf("the reloader ran %d time(s), want 1 — the wait must consume the marker and apply it", reloads)
		}
		if h.invokeCalls != 4 {
			t.Errorf("spawned %d sessions, want 4 — a reload must not cut the wait's run short; it applies in place and the run keeps draining", h.invokeCalls)
		}
		if markerPresent(t, d.state.Path(), MarkerReload) {
			t.Error("RELOAD must be consumed when honoured")
		}
		if out := logs.String(); !strings.Contains(out, "RELOAD requested during wait") {
			t.Errorf("the reload must be logged as landing IN the wait; log was:\n%s", out)
		}
	})
}

// CLA-664: a RELOAD landing inside a drain's OWN first-phase wait (waitOrStop,
// during the transient backoff) swaps d.cfg while the sequence the drain
// captured at start is still in flight. The in-flight sequence must keep its
// own shape — each phase's MODE, its salvage decision and its "PHASE i of N"
// text — and the reloaded shape must take effect at the NEXT drain, not this
// one. Before the fix, requireReviewRecord and the phase total were re-read
// from the live config at each phase spawn (invocationFor too), while
// drainPhases still walked the captured slice: a 2 -> 3 reload turned the
// in-flight FINAL review into a gated review-only phase (salvage skipped,
// "PHASE 2 of 3"), and a 3 -> 2 reload un-gated the review-only phase (it
// checkpointed on the branch alone and spawned the fix over no record).
func TestDrainPhases_AReloadMidDrainDoesNotReshapeTheInFlightSequence(t *testing.T) {
	impl := config.Phase{Name: config.ImplementPhaseName}
	review := config.Phase{Name: config.ReviewPhaseName}
	fix := config.Phase{Name: config.FixPhaseName}

	t.Run("2 to 3: the in-flight final review keeps its brief, salvage and count", func(t *testing.T) {
		h := &fakeAdapter{steps: []invokeStep{
			{res: transientResult()},  // implement attempt 1 blips; the backoff wait applies the RELOAD
			{res: checkpointed(1, 0)}, // implement attempt 2: checkpoint
			{res: checkpointed(1, 0)}, // in-flight FINAL review of the captured 2-phase shape
			{res: checkpointed(1, 0)}, // next drain, new 3-phase shape: implement
			{res: checkpointed(1, 0)}, // next drain: review-only (records its record below)
			{res: okResult(1, 0)},     // next drain: fix
		}}
		rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
		d := phaseDriverWithReleaser(t, h, []config.Phase{impl, review}, rel)
		s := &fakeSalvager{out: savedOutcome()}
		d.newSalvager = func(string) workSalvager { return s }
		d.SetReloader(func() (*config.Config, error) {
			fresh := *d.cfg
			fresh.Phases = []config.Phase{impl, review, fix}
			return &fresh, nil
		})
		h.onInvoke = func(call int) {
			switch call {
			case 0:
				writeMarker(t, d.state.Path(), MarkerReload, "")
			case 4: // the next drain's review-only session records the durable record the seam gate requires (CLA-637)
				rel.outcome += "\n" + config.ReviewRecordHeading + "\n- reviewed tip: " + testCheckpointTip + "\n- verdict: pass\n"
				rel.records = append(rel.records, recordFor(1, "review-1", "r-1", testCheckpointTip, "sealed"))
			}
		}

		if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
			t.Fatalf("first drain: err=%v stop=%v", err, stop)
		}
		if h.invokeCalls != 3 {
			t.Fatalf("the first drain spawned %d sessions, want 3 — the transient retry of implement, then the in-flight final review", h.invokeCalls)
		}
		p := h.invocations[2].Prompt
		if !strings.Contains(p, "PHASE 2 of 2") || strings.Contains(p, "PHASE 2 of 3") {
			t.Errorf("the in-flight review's count came from the reloaded config, not the captured sequence:\n%s", p)
		}
		if !strings.Contains(p, "Open a PR") {
			t.Errorf("the in-flight FINAL review lost the review-and-fix brief its captured position selects:\n%s", p)
		}
		if len(s.calls) != 2 {
			t.Fatalf("the driver salvaged %d time(s), want 2 (the implement phase and the in-flight FINAL review) — a final review is review-AND-fix and its uncommitted work must still be rescued: %+v", len(s.calls), s.calls)
		}

		if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
			t.Fatalf("second drain: err=%v stop=%v", err, stop)
		}
		if h.invokeCalls != 6 {
			t.Fatalf("after the reload the second drain ran to %d sessions total, want 6 — the NEXT drain must run the reloaded shape", h.invokeCalls)
		}
		if p2 := h.invocations[4].Prompt; !strings.Contains(p2, "PHASE 2 of 3") || !strings.Contains(p2, "REVIEW-ONLY gate") {
			t.Errorf("the drain after the reload did not run the new shape's review-only phase:\n%s", p2)
		}
		if p3 := h.invocations[5].Prompt; !strings.Contains(p3, "PHASE 3 of 3") {
			t.Errorf("the drain after the reload did not run the new shape's fix phase:\n%s", p3)
		}
		if len(s.calls) != 3 {
			t.Errorf("salvage ran %d time(s) in total, want 3 — the next drain's review-only phase must NOT salvage: %+v", len(s.calls), s.calls)
		}
	})

	t.Run("3 to 2: the in-flight review-only keeps its gate and its skip-salvage", func(t *testing.T) {
		outcome := "implemented: pushed the branch\n"
		h := &fakeAdapter{steps: []invokeStep{
			{res: transientResult()},  // implement attempt 1 blips; the backoff wait applies the RELOAD
			{res: checkpointed(1, 0)}, // implement attempt 2: checkpoint
			{res: checkpointed(1, 0)}, // in-flight review-only: records NOTHING — the gate must refuse it
			{res: checkpointed(1, 0)}, // next drain, new 2-phase shape: implement
			{res: checkpointed(1, 0)}, // next drain: FINAL review
		}}
		rel := &wiredReleaser{state: func() plane.TaskState {
			return plane.TaskState{Status: "in_progress", ClaimedByRun: "r-1", Outcome: outcome}
		}}
		d := phaseDriverWithReleaser(t, h, []config.Phase{impl, review, fix}, rel)
		s := &fakeSalvager{out: savedOutcome()}
		d.newSalvager = func(string) workSalvager { return s }
		d.SetReloader(func() (*config.Config, error) {
			fresh := *d.cfg
			fresh.Phases = []config.Phase{impl, review}
			return &fresh, nil
		})
		h.onInvoke = func(call int) {
			if call == 0 {
				writeMarker(t, d.state.Path(), MarkerReload, "")
			}
		}

		if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
			t.Fatalf("first drain: err=%v stop=%v", err, stop)
		}
		if h.invokeCalls != 3 {
			t.Fatalf("the first drain spawned %d sessions, want 3 — the review-only phase recorded nothing, so the gate must refuse it the checkpoint and no fix may spawn", h.invokeCalls)
		}
		p := h.invocations[2].Prompt
		if !strings.Contains(p, "PHASE 2 of 3") || strings.Contains(p, "PHASE 2 of 2") {
			t.Errorf("the in-flight review-only's count came from the reloaded config, not the captured sequence:\n%s", p)
		}
		if !strings.Contains(p, "REVIEW-ONLY gate") {
			t.Errorf("the in-flight review lost the review-only brief its captured position selects:\n%s", p)
		}
		if len(s.calls) != 1 {
			t.Fatalf("the driver salvaged %d time(s), want 1 (the implement phase only) — the review-only phase's deliverable is the plane record, not the tree: %+v", len(s.calls), s.calls)
		}

		if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
			t.Fatalf("second drain: err=%v stop=%v", err, stop)
		}
		if h.invokeCalls != 5 {
			t.Fatalf("after the reload the second drain ran to %d sessions total, want 5 — the NEXT drain must run the reloaded 2-phase shape", h.invokeCalls)
		}
		p2 := h.invocations[4].Prompt
		if !strings.Contains(p2, "PHASE 2 of 2") || strings.Contains(p2, "REVIEW-ONLY") {
			t.Errorf("the drain after the reload did not run the new shape's FINAL review:\n%s", p2)
		}
		if !strings.Contains(p2, "Open a PR") {
			t.Errorf("the next drain's review did not keep the review-and-fix brief its final position selects:\n%s", p2)
		}
		if len(s.calls) != 3 {
			t.Errorf("salvage ran %d time(s) in total, want 3 — the next drain's FINAL review must salvage (implement twice, review once): %+v", len(s.calls), s.calls)
		}
	})

	t.Run("3 to 2: a handoff in the in-flight fix keeps its terminal step", func(t *testing.T) {
		h := &fakeAdapter{steps: []invokeStep{
			{res: transientResult()},  // implement attempt 1 blips; the backoff wait applies the RELOAD
			{res: checkpointed(1, 0)}, // implement attempt 2: checkpoint
			{res: checkpointed(1, 0)}, // in-flight review-only: records the record below
			{res: handoffResult("R1 is fixed and committed; the PR is next.")}, // in-flight fix hands off
			{res: okResult(1, 0)}, // the fix successor ends the sequence
		}}
		rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
		d := phaseDriverWithReleaser(t, h, []config.Phase{impl, review, fix}, rel)
		d.SetReloader(func() (*config.Config, error) {
			fresh := *d.cfg
			fresh.Phases = []config.Phase{impl, review}
			return &fresh, nil
		})
		h.onInvoke = func(call int) {
			switch call {
			case 0:
				writeMarker(t, d.state.Path(), MarkerReload, "")
			case 2: // the review-only session records the durable record the seam gate requires (CLA-637)
				rel.outcome += "\n" + config.ReviewRecordHeading + "\n- reviewed tip: " + testCheckpointTip + "\n- verdict: pass\n"
				rel.records = append(rel.records, recordFor(1, "review-1", "r-1", testCheckpointTip, "sealed"))
			}
		}

		if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
			t.Fatalf("drain: err=%v stop=%v", err, stop)
		}
		if h.invokeCalls != 5 {
			t.Fatalf("the drain spawned %d sessions, want 5 — implement's retry, the in-flight review-only, the fix, and the fix's handoff successor", h.invokeCalls)
		}
		// The continuation gate must read the CAPTURED sequence: the reloaded
		// 2-phase config has no index 2, so a live read drops the fix phase's
		// terminal step from the successor — the in-flight sequence reshaped
		// by the reload (CLA-664), one phase later than the mode and count.
		successor := h.invocations[4].Prompt
		if !strings.Contains(successor, `status: "in_review"`) {
			t.Errorf("the in-flight fix's handoff successor lost the terminal step its captured position selects:\n%s", successor)
		}
		if strings.Contains(successor, "review-only ending") {
			t.Errorf("the in-flight fix's successor was handed the review-only ending:\n%s", successor)
		}
	})
}

func TestDrainPhases_SumsSpendAcrossPhases(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.50)},
		{res: okResult(7, 0.25)},
	}}
	d, _ := phaseDriver(t, h, twoPhases())

	tokens, cost, _, err := drainPhasesOnce(t, d)
	if err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	if tokens != 17 {
		t.Errorf("tokens = %d, want 17 — an iteration's spend is the sum of its phases", tokens)
	}
	if cost != 0.75 {
		t.Errorf("cost = %v, want 0.75", cost)
	}
}

// Back-compat, and the reason phases are opt-in: a config written before any of
// this existed must behave exactly as it did.
func TestDrainPhases_NoPhasesConfiguredRunsExactlyOneSession(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{{res: checkpointed(1, 0)}}}
	d, _ := phaseDriver(t, h, nil)
	d.cfg.Prompt = "Work the next backlog item."

	if _, _, _, err := drainPhasesOnce(t, d); err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	if h.invokeCalls != 1 {
		t.Fatalf("an unphased config spawned %d sessions, want exactly 1", h.invokeCalls)
	}
	if got := h.invocations[0].Prompt; got != "Work the next backlog item." {
		t.Errorf("unphased session was asked %q, not the configured prompt", got)
	}
	// The log name is byte-identical to what it always was: no phase tag.
	logs := iterationLogs(t, d.state.Path())
	if len(logs) != 1 {
		t.Fatalf("expected one iteration log, got %v", logs)
	}
	if strings.Contains(logs[0], "-p") && strings.Contains(logs[0], "-a0-") && strings.Index(logs[0], "-p") < strings.Index(logs[0], "-a0-") {
		t.Errorf("an unphased run grew a phase tag in its log name: %q", logs[0])
	}
}

// The cap has to actually reach the harness. Without it the boundary is
// voluntary — and with CLA-343's resolution chain, "a phase with no cap" no
// longer means "uncapped": it means "defer upward", and phase 2 of this config
// defers to the built-in default. The phase's own cap still wins.
func TestDrainPhases_CarriesTheTurnCapToTheHarness(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(1, 0)},
		{res: okResult(1, 0)},
	}}
	d, _ := phaseDriver(t, h, []config.Phase{
		{Name: "implement", MaxTurns: 40},
		{Name: "review"},
	})

	if _, _, _, err := drainPhasesOnce(t, d); err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	if got := h.invocations[0].MaxTurns; got != 40 {
		t.Errorf("phase 1 MaxTurns = %d, want 40 — without the cap the boundary is voluntary", got)
	}
	// CLA-343, deliberately: this used to assert 0 ("uncapped"), the reading that
	// left an unphased run with no cap anywhere in the chain. Phase 2 now defers
	// upward to the built-in default instead.
	if got := h.invocations[1].MaxTurns; got != config.DefaultMaxTurns {
		t.Errorf("phase 2 MaxTurns = %d, want the built-in default %d — a phase with no cap defers upward, never to uncapped", got, config.DefaultMaxTurns)
	}
}

// A session ended by the adapter's wall-clock cap (CLA-368) is the third member
// of the same family as the turn cap and the token ceiling: an orderly cut-off
// whose survivability rests on the salvage, so it ends the PHASE and never the
// run. With work pushed it is a legitimate checkpoint; with nothing pushed
// there is no branch for a phase 2 to resume from, exactly as for the other
// two.
func TestDrainPhases_AWallClockCapIsACheckpointNotAFatalExit(t *testing.T) {
	for _, tc := range []struct {
		name        string
		claim       harness.Claim
		wantInvokes int
	}{
		{
			name:        "capped with work pushed: phase 2 resumes it",
			claim:       harness.Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true, Branch: "clanker/x"},
			wantInvokes: 2,
		},
		{
			name:        "capped with nothing pushed: no checkpoint to hand on",
			claim:       openClaim(),
			wantInvokes: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &fakeAdapter{steps: []invokeStep{
				{res: held(wallClockResult(), tc.claim)},
				{res: okResult(1, 0)},
			}}
			d, _ := phaseDriver(t, h, []config.Phase{
				{Name: "implement", MaxWallClock: config.Duration(time.Minute)},
				{Name: "review"},
			})

			_, _, stop, err := drainPhasesOnce(t, d)
			if err != nil {
				t.Fatalf("a wall-clock-capped phase ended the RUN: %v — the cap is the phase ending, never the daemon failing", err)
			}
			if stop {
				t.Error("a wall-clock-capped phase stopped the run")
			}
			if h.invokeCalls != tc.wantInvokes {
				t.Errorf("spawned %d sessions, want %d", h.invokeCalls, tc.wantInvokes)
			}
		})
	}
}

// The cap must not be retried in either direction the driver could go wrong:
// not as a transient blip (a fresh session would spend the same hours over
// again and reach the same deadline) and not as a launch failure. Its spend is
// counted, unlike the token ceiling's: opencode's usage is summed per step all
// the way to the kill.
func TestDrainPhases_AWallClockCappedFinalPhaseEndsTheDrainWithoutFailingTheRun(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{{res: held(wallClockResult(), openClaim())}}}
	d, _ := phaseDriver(t, h, nil)
	d.cfg.Prompt = "Work the next backlog item."

	tokens, cost, stop, err := drainPhasesOnce(t, d)
	if err != nil {
		t.Fatalf("a wall-clock-capped final phase ended the run: %v", err)
	}
	if stop {
		t.Error("a wall-clock-capped final phase stopped the run")
	}
	if h.invokeCalls != 1 {
		t.Errorf("spawned %d sessions; a wall-clock cap must never be retried", h.invokeCalls)
	}
	if tokens != 120_000 || cost != 1.25 {
		t.Errorf("counted tokens=%d cost=%v, want the 120000/1.25 the session spent before the cap — a capped session's spend still hits the budget", tokens, cost)
	}
}

// The resolved cap has to REACH the adapter, which is the link a config-only
// test cannot see: the phase's own cap, and the run-wide one for a phase that
// sets none.
func TestInvocationForCarriesTheResolvedWallClockCap(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{{res: okResult(1, 0)}}}
	d, _ := phaseDriver(t, h, []config.Phase{
		{Name: "implement", MaxWallClock: config.Duration(20 * time.Minute)},
		{Name: "review"},
	})
	d.cfg.MaxSessionWallClock = config.Duration(time.Hour)

	phases := d.cfg.EffectivePhases()
	inv1, err := d.invocationFor(d.targets[0], 0, phases[0], nil, "", "", phases)
	if err != nil {
		t.Fatalf("invocationFor phase 1: %v", err)
	}
	if got := inv1.MaxSessionWallClock; got != 20*time.Minute {
		t.Errorf("phase 1 invocation carries %s, want its own 20m", got)
	}
	inv2, err := d.invocationFor(d.targets[0], 1, phases[1], nil, "", "", phases)
	if err != nil {
		t.Fatalf("invocationFor phase 2: %v", err)
	}
	if got := inv2.MaxSessionWallClock; got != time.Hour {
		t.Errorf("phase 2 invocation carries %s, want the run-wide 1h it inherits", got)
	}
}

// The operator-facing line has to be TRUE, so the driver branches on the claim
// it actually observed rather than asserting a salvage: since CLA-365 the only
// adapter that enforces this cap (opencode) also observes claims, so BOTH arms
// below are reachable today and each pins its own wording. Reporting a salvage
// that never ran would be the reassuring falsehood CLA-290 removed from
// doctor, in the one place a run leaves work behind.
func TestDrainPhases_AWallClockCapDoesNotClaimASalvageThatCouldNotRun(t *testing.T) {
	for _, tc := range []struct {
		name     string
		claim    harness.Claim
		want     string
		unwanted string
	}{
		{
			name:     "no claim observed: say the work is still in the worktree",
			claim:    harness.Claim{},
			want:     "NOTHING was salvaged",
			unwanted: "was salvaged above",
		},
		{
			name:     "claim held: the salvage really did run",
			claim:    harness.Claim{TaskID: "t-1", RunID: "r-1"},
			want:     "was salvaged above",
			unwanted: "NOTHING was salvaged",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged strings.Builder
			prev := log.Writer()
			log.SetOutput(&logged)
			t.Cleanup(func() { log.SetOutput(prev) })

			h := &fakeAdapter{steps: []invokeStep{{res: held(wallClockResult(), tc.claim)}}}
			d, _ := phaseDriver(t, h, nil)
			d.cfg.Prompt = "Work the next backlog item."

			if _, _, _, err := drainPhasesOnce(t, d); err != nil {
				t.Fatalf("drainPhases: %v", err)
			}
			if !strings.Contains(logged.String(), tc.want) {
				t.Errorf("the cap line does not say %q: %s", tc.want, logged.String())
			}
			if strings.Contains(logged.String(), tc.unwanted) {
				t.Errorf("the cap line says %q, which is not true here: %s", tc.unwanted, logged.String())
			}
		})
	}
}

// CLA-345: the spawn line stamps the settings the session was spawned WITH,
// because an old token/cost line is attributable only by evidence - what was
// resolved at spawn time - not by inference from whatever the config file says
// now. The stamped model must be inv.Model, the concrete id the harness was
// handed (ModelForPhase's output), never the tier bucket name that produced it.
// An empty model= is itself the honest stamp for "nothing configured; the
// harness ran its own default".
func TestDrainPhases_SpawnLineStampsTheResolvedModel(t *testing.T) {
	t.Run("the run-wide model is stamped", func(t *testing.T) {
		logs := captureLogs(t)
		h := &fakeAdapter{steps: []invokeStep{{res: okResult(1, 0)}}}
		d, _ := phaseDriver(t, h, nil)
		d.cfg.Model = "claude-test-model"

		if _, _, _, err := drainPhasesOnce(t, d); err != nil {
			t.Fatalf("drainPhases: %v", err)
		}
		want := "spawning fake (phase=1 model=claude-test-model log: "
		if !strings.Contains(logs.String(), want) {
			t.Errorf("spawn line does not carry %q: %s", want, logs.String())
		}
	})

	t.Run("a tier is stamped as its resolved alias, never the bucket name", func(t *testing.T) {
		logs := captureLogs(t)
		h := &fakeAdapter{steps: []invokeStep{{res: okResult(1, 0)}}}
		d, _ := phaseDriver(t, h, []config.Phase{{Name: "implement", Tier: "strong"}})
		d.cfg.Models = map[string]string{"strong": "claude-opus-5"}

		if _, _, _, err := drainPhasesOnce(t, d); err != nil {
			t.Fatalf("drainPhases: %v", err)
		}
		logged := logs.String()
		if !strings.Contains(logged, "(phase=implement model=claude-opus-5 ") {
			t.Errorf("spawn line does not stamp the resolved alias claude-opus-5: %s", logged)
		}
		if strings.Contains(logged, "model=strong") {
			t.Errorf("spawn line stamped the tier bucket name instead of its alias: %s", logged)
		}
	})

	t.Run("a retry attempt is stamped too", func(t *testing.T) {
		logs := captureLogs(t)
		h := &fakeAdapter{steps: []invokeStep{
			{res: transientResult()},
			{res: okResult(1, 0)},
		}}
		d, _ := phaseDriver(t, h, nil)
		d.cfg.Model = "claude-test-model"

		if _, _, _, err := drainPhasesOnce(t, d); err != nil {
			t.Fatalf("drainPhases: %v", err)
		}
		want := "retry 1, spawning fake (phase=1 model=claude-test-model log: "
		if !strings.Contains(logs.String(), want) {
			t.Errorf("retry spawn line does not carry %q: %s", want, logs.String())
		}
	})

	t.Run("an unconfigured model stamps the empty value the harness was handed", func(t *testing.T) {
		logs := captureLogs(t)
		h := &fakeAdapter{steps: []invokeStep{{res: okResult(1, 0)}}}
		d, _ := phaseDriver(t, h, nil)

		if _, _, _, err := drainPhasesOnce(t, d); err != nil {
			t.Fatalf("drainPhases: %v", err)
		}
		// model= followed by nothing IS the evidence: no explicit model existed
		// at spawn time, so the session ran the harness's own default.
		if !strings.Contains(logs.String(), "model= log:") {
			t.Errorf("spawn line carries no bare model= for an unconfigured model: %s", logs.String())
		}
	})
}

// The undeclared counter is only worth having if it means ONE thing, so these pin
// the cases that must NOT move it. They exist because the first cut of CLA-384
// incremented inside releaseHeldClaim, which every session exit reaches: the §5
// review of that commit demonstrated four non-failures inflating the count,
// including a drain that settled its task and still logged "the task was never
// moved on" twice. Direct unit tests on releaseHeldClaim could not have caught
// that - the discrimination lives in the callers - so these drive whole phase
// runs.
func TestUndeclared_CountsOnlyACleanFinalPhaseThatDidNotDeclare(t *testing.T) {
	wip := func() harness.Claim {
		return harness.Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true, Branch: "clanker/x"}
	}

	t.Run("the CLA-384 shape counts", func(t *testing.T) {
		captureLogs(t)
		h := &fakeAdapter{steps: []invokeStep{
			{res: held(okResult(10, 0.10), wip())},
			{res: held(okResult(5, 0.05), wip())},
		}}
		d, _ := phaseDriver(t, h, twoPhases())
		drainPhasesOnce(t, d)
		if d.undeclared != 1 {
			t.Errorf("undeclared = %d, want 1 — a clean review phase that held a pushed task IS the failure", d.undeclared)
		}
	})

	// Phase 1 checkpointing and the run then stopping on budget is phase 1 doing
	// exactly what its brief says. Counting it would report an undeclared hand-off
	// on every budget-limited run.
	t.Run("a budget trip at the seam does not", func(t *testing.T) {
		captureLogs(t)
		h := &fakeAdapter{steps: []invokeStep{
			{res: held(okResult(10_000, 0.10), wip())},
			{res: okResult(5, 0.05)},
		}}
		d, _ := phaseDriver(t, h, twoPhases())
		d.cfg.Budget = config.Budget{MaxTokens: 1_000}
		drainPhasesOnce(t, d)
		if d.undeclared != 0 {
			t.Errorf("undeclared = %d on a budget trip between phases, want 0", d.undeclared)
		}
	})

	// A crash is a different failure with its own reporting; folding it in here
	// would make the number mean "something went wrong", which the log already says.
	t.Run("a crashed phase does not", func(t *testing.T) {
		captureLogs(t)
		h := &fakeAdapter{steps: []invokeStep{{res: held(nonRetryableResult(), wip())}}}
		d, _ := phaseDriver(t, h, twoPhases())
		drainPhasesOnce(t, d)
		if d.undeclared != 0 {
			t.Errorf("undeclared = %d on a crashed phase, want 0", d.undeclared)
		}
	})

	// The sharpest of the four the review found: the drain SETTLES the task, and
	// the old code still announced twice that it had never been moved on.
	t.Run("transient retries on a task that then settles do not", func(t *testing.T) {
		captureLogs(t)
		h := &fakeAdapter{steps: []invokeStep{
			{res: held(transientResult(), wip())},
			{res: held(transientResult(), wip())},
			{res: held(okResult(5, 0.05), harness.Claim{TaskID: "t-1", RunID: "r-1", Settled: true})},
		}}
		d, _ := phaseDriver(t, h, twoPhases())
		d.cfg.MaxRetries = 5
		drainPhasesOnce(t, d)
		if d.undeclared != 0 {
			t.Errorf("undeclared = %d after retries on a task that settled, want 0", d.undeclared)
		}
	})

	// A usage limit on the resumed phase is the driver's designed recovery, not a
	// session declining to declare.
	t.Run("a usage limit on the resumed phase does not", func(t *testing.T) {
		captureLogs(t)
		h := &fakeAdapter{steps: []invokeStep{
			{res: held(okResult(10, 0.10), wip())},
			{res: held(limitResult(), wip())},
		}}
		d, _ := phaseDriver(t, h, twoPhases())
		drainPhasesOnce(t, d)
		if d.undeclared != 0 {
			t.Errorf("undeclared = %d on a usage limit mid-phases, want 0", d.undeclared)
		}
	})
}

// --- the zero-usage-unknown marker (CLA-398) -------------------------------

// The CLA-398 quiet-death signature — a final step_finish with reason "unknown"
// and all-zero usage — used to be logged as "iteration done (tokens=0
// cost=$0.00)", indistinguishable from a cheap clean run. The adapter now writes
// its own terminal_reason marker for it, and the driver must log the end BY
// NAME so a dead session stops reading as a successful one.
func TestDrain_ZeroUsageUnknownEndIsLoggedByName(t *testing.T) {
	logs := captureLogs(t)
	h := &fakeAdapter{steps: []invokeStep{
		{res: zeroUsageResult()},
	}}
	d := New(fastCfg(), h, &fakePoller{})
	openTestStateDir(t, d)

	_, _, stop, err := d.drainWithRetries(context.Background(), 1, d.targets[0], spend{start: time.Now()})
	if err != nil || stop {
		t.Fatalf("drainWithRetries: stop=%v err=%v", stop, err)
	}

	out := logs.String()
	if !strings.Contains(out, "zero-usage-unknown signature") {
		t.Errorf("the driver did not log the marker by name:\n%s", out)
	}
	if strings.Contains(out, "iteration 1 done (tokens=") {
		t.Errorf("the quiet death was logged as an ordinary clean run:\n%s", out)
	}
}

// The driver also names the marker in the dead-phase PARK path: when a phased
// sequence's first phase dies twice with the quiet-death signature, the park
// outcome and the question body must say which flavour of death this was.
func TestDrainPhases_ZeroUsageUnknownParkNamesTheMarker(t *testing.T) {
	logs := captureLogs(t)
	h := &fakeAdapter{steps: []invokeStep{
		{res: held(zeroUsageResult(), openClaim())},
		{res: held(zeroUsageResult(), openClaim())},
		{res: held(zeroUsageResult(), openClaim())},
		{res: held(zeroUsageResult(), openClaim())},
	}}
	rel := &parkingReleaser{}
	cfg := fastCfg()
	cfg.Phases = twoPhases()
	cfg.Prompt = ""
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: rel}})
	openTestStateDir(t, d)

	_, _, stop, err := drainPhasesOnce(t, d)
	if err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	if stop {
		t.Error("a parked task stopped the whole run")
	}
	if len(rel.parks) != 1 {
		t.Fatalf("parked %d times, want 1: %+v", len(rel.parks), rel.parks)
	}
	// deadReason() prefers TerminalReasonKey over the bare FinishReasonKey, so
	// the park outcome carries the marker in place of the bare "unknown" finish
	// reason, not alongside it — the operator sees which flavour of death this
	// was. (A second Contains(harness.FinishReasonUnknown) check would pass
	// vacuously here since "unknown" is a substring of "zero_usage_unknown";
	// it would not independently verify anything beyond the check below.)
	got := rel.parks[0]
	if !strings.Contains(got.outcome, harness.ZeroUsageReason) {
		t.Errorf("the park outcome does not name the marker %q: %q", harness.ZeroUsageReason, got.outcome)
	}
	if out := logs.String(); !strings.Contains(out, harness.ZeroUsageReason) {
		t.Errorf("the driver log does not name the marker:\n%s", out)
	}
}

// --- the dead-phase signature (CLA-386) ------------------------------------

// deadResult is a CLA-386 dead-phase result: the session's final step finished
// with reason "unknown" — opencode's marker for a session that died without a
// final answer — and (via the caller's claim) no branch recorded. Together they
// mean the phase produced nothing.
func deadResult() harness.Result {
	return harness.Result{ExitCode: 0, Raw: map[string]any{harness.FinishReasonKey: harness.FinishReasonUnknown}}
}

// The seam must not advance to review on a dead phase: it would hand the review
// brief a task with no branch on it and a false premise ("an earlier session has
// already implemented, committed and pushed"). The dead phase is retried once;
// only a retry that reaches a healthy checkpoint lets the sequence advance.
func TestDrainPhases_ADeadPhaseIsRetriedNotAdvancedToReview(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		// Phase 1 dies producing nothing (no branch). Under the bug this was a
		// checkpoint — exit 0, claim still held — so the review phase ran next on
		// a task with nothing pushed.
		{res: held(deadResult(), openClaim())},
		// The retry is phase 1 again: a healthy checkpoint.
		{res: checkpointed(1, 0)},
		// And only now does the review phase run.
		{res: okResult(1, 0)},
	}}
	d, _ := phaseDriver(t, h, twoPhases())

	_, _, stop, err := drainPhasesOnce(t, d)
	if err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 3 {
		t.Fatalf("spawned %d sessions, want 3 (dead phase, its one retry, then review) — a dead phase must be retried before the review brief is ever spawned",
			h.invokeCalls)
	}
	// The retry is the SAME phase's job, and the review brief only ever sees a
	// task that a healthy phase handed over.
	if !strings.Contains(h.invocations[1].Prompt, "PHASE 1") {
		t.Errorf("the retry was not a phase-1 session: %q", h.invocations[1].Prompt)
	}
	if !strings.Contains(h.invocations[2].Prompt, "PHASE 2") {
		t.Errorf("the review phase did not run after the healthy retry: %q", h.invocations[2].Prompt)
	}
}

// The second conjunct of the signature is load-bearing: a phase that pushed work
// and THEN died on an unknown reason has produced something, so it keeps its
// checkpoint and the seam advances exactly as a healthy phase's does.
func TestDrainPhases_DeadWithABranchRecordedStillAdvances(t *testing.T) {
	wip := harness.Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true, Branch: "clanker/x"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: held(deadResult(), wip)},
		{res: okResult(1, 0)},
	}}
	d, _ := phaseDriver(t, h, twoPhases())

	_, _, stop, err := drainPhasesOnce(t, d)
	if err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 2 {
		t.Errorf("spawned %d sessions, want 2 — an unknown finish reason with a branch recorded is still a checkpoint, so the review phase runs", h.invokeCalls)
	}
}

// A clean stop with nothing recorded is the 2026-08-24 fleet-incident shape:
// the session claimed, explored, hit a silent EOT, and ended cleanly still
// holding the task with no branch on the plane. It is a held-but-empty FAILED
// phase, not a checkpoint — released, retried at most once (bounded at 1),
// counted in the dead-phase tally, and never handed to the review brief. Before
// CLA-457 this shape was an "ordinary checkpoint" purely because the lease
// renewer (CLA-358) keeps every session that claimed holding — which is exactly
// how two silent EOTs advanced the seam to review on a false premise.
func TestDrainPhases_HeldButEmptyIsANotCheckpointAndRetriesOnce(t *testing.T) {
	empty := func() harness.Result {
		ok := okResult(1, 0)
		ok.Raw[harness.FinishReasonKey] = "stop"
		return held(ok, openClaim())
	}
	logged := captureLogs(t)
	h := &fakeAdapter{steps: []invokeStep{{res: empty()}, {res: empty()}}}
	d, rel := phaseDriver(t, h, twoPhases())

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	// The empty implement exit is a FAILED phase: released, retried at most once,
	// and the review brief never spawns.
	if h.invokeCalls != 2 {
		t.Fatalf("spawned %d sessions, want 2 (the empty phase and its single bounded retry) — held-but-empty is not a checkpoint and cannot spawn review", h.invokeCalls)
	}
	for _, inv := range h.invocations {
		if strings.Contains(inv.Prompt, "PHASE 2") {
			t.Errorf("the review phase spawned after a held-but-empty implement exit: %q", inv.Prompt)
		}
	}
	// Released to the queue so a fresh session can pick the task back up — once
	// per empty session (the original and its one retry each held and released
	// the task).
	if len(rel.calls) != 2 || rel.calls[0] != (releaseCall{"t-1", "r-1"}) || rel.calls[1] != (releaseCall{"t-1", "r-1"}) {
		t.Errorf("released %+v, want {t-1 r-1} twice (one release per empty session) — the empty claim goes back to the queue, never to review", rel.calls)
	}
	// A distinct line names why, so the operator can tell an empty exit from a
	// healthy one.
	out := logged.String()
	if !strings.Contains(out, "no recorded branch") || !strings.Contains(out, "not a checkpoint") {
		t.Errorf("the empty exit is not named as a not-a-checkpoint with its reason:\n%s", out)
	}
	// And it counts toward the dead-phase tally — the measurement the operator
	// watches for a fleet-wide silent EOT.
	cell := d.deadTally[tallyKey{phase: "implement", harness: "claude"}]
	if cell == nil || cell.run != 2 || cell.dead != 2 {
		t.Errorf("tally cell = %+v, want run=2 dead=2 — both empty sessions count as dead phases", cell)
	}
}

// Its sibling: a "stop" finish WITH a recorded, origin-verified branch IS a
// checkpoint — the normal healthy implement end.
func TestDrainPhases_AStopReasonWithARecordedBranchStillCheckpoints(t *testing.T) {
	ck := checkpointed(1, 0)
	ck.Raw[harness.FinishReasonKey] = "stop"
	h := &fakeAdapter{steps: []invokeStep{
		{res: ck},
		{res: okResult(1, 0)},
	}}
	d, _ := phaseDriver(t, h, twoPhases())

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 2 {
		t.Errorf("spawned %d sessions, want 2 — a reason:stop session WITH a verified branch is an ordinary checkpoint", h.invokeCalls)
	}
}

// A branch recorded but NOT reachable on the origin remote is not a checkpoint
// either — the second half of the evidence gate (CLA-457). The claim carries
// WIP, so it is NEVER released: the takeover hand-off must survive. But the
// review phase must not spawn on a branch nothing can fetch.
func TestDrainPhases_RecordedButUnreachableBranchIsNotACheckpoint(t *testing.T) {
	logged := captureLogs(t)
	// The real update_task(branch:) sets HasWIP on the claim AND arms the report;
	// the branch is recorded but the verifier will say it was never pushed.
	recorded := reported(held(okResult(1, 0),
		harness.Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true, Branch: "clanker/x"}), branchReport())
	h := &fakeAdapter{steps: []invokeStep{
		{res: recorded},
		{res: okResult(1, 0)},
	}}
	d, rel := phaseDriver(t, h, twoPhases())
	// The verifier says the recorded branch was never pushed.
	d.newVerifier = func(string, bool) deliveryVerifier {
		return &fakeVerifier{report: delivery.Report{Checks: []delivery.Check{{
			Kind: delivery.BranchPushed, Status: delivery.Fail,
			Detail: `branch "clanker/x" is NOT on origin`,
		}}}}
	}

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 1 {
		t.Errorf("spawned %d sessions, want 1 — an unpushed branch must not advance the seam to review", h.invokeCalls)
	}
	// HasWIP means the claim is never released: the hand-off stays a takeover
	// once the lease dies, and retrying against a live claim is refused.
	if len(rel.calls) != 0 {
		t.Errorf("released a claim with a recorded (unpushed) branch: %+v — that would destroy the takeover hand-off", rel.calls)
	}
	out := logged.String()
	if !strings.Contains(out, "not a checkpoint") || !strings.Contains(out, "NOT on origin") {
		t.Errorf("the unreachable-branch exit is not named with its reason:\n%s", out)
	}
}

// The review brief is recomposed from plane state: it names the branch phase 1
// recorded and the driver verified, instead of asserting an unverified
// phase-1 success (CLA-457 part 3).
func TestDrainPhases_ReviewBriefNamesTheRecordedBranch(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: reported(held(okResult(1, 0), openClaim()), branchReport())},
		{res: okResult(1, 0)},
	}}
	d, _ := phaseDriver(t, h, twoPhases())

	if _, _, _, err := drainPhasesOnce(t, d); err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	if h.invokeCalls != 2 {
		t.Fatalf("spawned %d sessions, want 2 — the review phase must run on the verified branch", h.invokeCalls)
	}
	p2 := h.invocations[1].Prompt
	if !strings.Contains(p2, "clanker/x") {
		t.Errorf("the review brief does not name the branch phase 1 recorded: %q", p2)
	}
	if strings.Contains(p2, config.PhaseBranchPlaceholder) {
		t.Errorf("the {{branch}} placeholder survived into the review brief: %q", p2)
	}
	if strings.Contains(p2, "an earlier session has already") {
		t.Errorf("the review brief still asserts an unverified phase-1 success: %q", p2)
	}
}

// branchStatusVerifier answers each branch by name with a scripted verdict, for
// the evidence-gate tests where the verifier must DISAGREE per branch (a draft
// recorded first that was never pushed, and the real branch that was).
type branchStatusVerifier map[string]delivery.Status

func (m branchStatusVerifier) Verify(_ context.Context, c delivery.Claim) delivery.Report {
	rep := delivery.Report{Claim: c}
	st, ok := m[c.Branch]
	if !ok {
		st = delivery.Unknown
	}
	rep.Checks = []delivery.Check{{Kind: delivery.BranchPushed, Status: st, Detail: "branch " + c.Branch + " judges " + string(st)}}
	return rep
}

// A session that records a DRAFT branch early (never pushed) and the REAL branch
// later (pushed and verified) must checkpoint on the REAL one: ANY recorded
// branch that verifies on origin is evidence, not only the first recorded
// (CLA-457). An earlier cut returned on the first branch's verdict, which failed
// the phase over a draft even though a verified hand-off exists — and the review
// brief must name the verified branch, never the draft.
func TestDrainPhases_AnyVerifiedBranchIsEvidenceNotJustTheFirstRecorded(t *testing.T) {
	draft := harness.Report{TaskID: "t-1", Ref: "CLA-253", RunID: "r-1", Branch: "clanker/draft"}
	final := harness.Report{TaskID: "t-1", Ref: "CLA-253", RunID: "r-1", Branch: "clanker/final"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: reported(held(okResult(1, 0), openClaim()), draft, final)},
		{res: okResult(1, 0)},
	}}
	d, _ := phaseDriver(t, h, twoPhases())
	d.newVerifier = func(string, bool) deliveryVerifier {
		return branchStatusVerifier{"clanker/draft": delivery.Fail, "clanker/final": delivery.Pass}
	}

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 2 {
		t.Fatalf("spawned %d sessions, want 2 — the phase checkpoints on the VERIFIED final branch even though a draft was recorded first", h.invokeCalls)
	}
	if !strings.Contains(h.invocations[1].Prompt, "clanker/final") {
		t.Errorf("the review brief should name the verified branch clanker/final:\n%q", h.invocations[1].Prompt)
	}
	if strings.Contains(h.invocations[1].Prompt, "clanker/draft") {
		t.Errorf("the review brief named the unverified draft branch: %q", h.invocations[1].Prompt)
	}
}

// A handoff the driver REFUSES (max-iterations already spent) ends the carve-out
// the handoff exempted: the exit is re-judged as the bare exit it really is, so
// a no-evidence implement session must NOT advance to review on a brief that
// claims a verified branch (CLA-457). The claim was held for a successor that is
// not coming, so the deferred handback releases it.
func TestDrainPhases_RefusedHandoffWithNoEvidenceDoesNotAdvanceToReview(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: handoffResult("Continue implementing: the design is settled, write the code.")},
		{res: okResult(1, 0)},
	}}
	d, rel := phaseDriver(t, h, twoPhases())
	d.cfg.MaxIterations = 1 // the respawn would be iteration 2: refused
	logged := captureLogs(t)

	_, _, handoffs, stop, err := drainPhasesHandoffs(t, d, 1)
	if err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	// The handoff was refused and the exit had no evidence: only the one session
	// ran, and the review brief never spawned.
	if h.invokeCalls != 1 {
		t.Errorf("spawned %d sessions, want 1 — a refused no-evidence handoff exits the drain without spawning review", h.invokeCalls)
	}
	if handoffs != 0 {
		t.Errorf("handoffs = %d, want 0 — the refused handoff never spawned a successor", handoffs)
	}
	for _, inv := range h.invocations {
		if strings.Contains(inv.Prompt, "PHASE 2") {
			t.Errorf("the review phase spawned after a refused no-evidence handoff: %q", inv.Prompt)
		}
	}
	// The held claim is released by the deferred handback (no WIP).
	if len(rel.calls) != 1 || rel.calls[0] != (releaseCall{"t-1", "r-1"}) {
		t.Errorf("released %+v, want exactly {t-1 r-1} via the deferred handback", rel.calls)
	}
	out := logged.String()
	if !strings.Contains(out, "handoff refused") || !strings.Contains(out, "not a checkpoint") {
		t.Errorf("the refusal and the empty judgement are not both named:\n%s", out)
	}
}

// A task that can kill four full implement phases reaches the operator rather
// than a fifth session — parked, with an OPEN question so the operator actually
// sees it. The retry budget is per task: three deaths earn three retries, and
// the FOURTH consecutive dead phase parks instead of retrying again (CLA-396,
// raised from two by the 2026-08-20 operator decision).
func TestDrainPhases_AFourthConsecutiveDeadPhaseParks(t *testing.T) {
	logs := captureLogs(t)
	h := &fakeAdapter{steps: []invokeStep{
		{res: held(deadResult(), openClaim())},
		{res: held(deadResult(), openClaim())},
		{res: held(deadResult(), openClaim())},
		{res: held(deadResult(), openClaim())},
	}}
	rel := &parkingReleaser{}
	cfg := fastCfg()
	cfg.Phases = twoPhases()
	cfg.Prompt = ""
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: rel}})
	openTestStateDir(t, d)

	_, _, stop, err := drainPhasesOnce(t, d)
	if err != nil {
		t.Fatalf("drainPhases: %v — a parked task is the phase ending, not the run failing", err)
	}
	if stop {
		t.Error("a parked task stopped the whole run; the daemon should carry on with the next task")
	}
	if h.invokeCalls != 4 {
		t.Errorf("spawned %d sessions, want 4 (three retries, then the fourth dead phase parks)", h.invokeCalls)
	}
	if len(rel.parks) != 1 {
		t.Fatalf("parked %d times, want 1: %+v", len(rel.parks), rel.parks)
	}
	got := rel.parks[0]
	if got.taskID != "t-1" || got.runID != "r-1" {
		t.Errorf("parked %s/%s, want t-1/r-1", got.taskID, got.runID)
	}
	// The park carries NO decision: since CLA-395 the record is the outcome plus
	// the open question. The outcome must still stand alone, naming the signature.
	if !strings.Contains(got.outcome, harness.FinishReasonUnknown) {
		t.Errorf("the park outcome does not name the signature: %q", got.outcome)
	}
	if !strings.Contains(got.outcome, "retry") {
		t.Errorf("the park outcome does not reference the operator's retry-then-park ruling: %q", got.outcome)
	}

	// The park filed ONE open question: non-blocking, a clarification, carrying
	// the signature, the task ref, the four sessions it cost, and where the logs
	// are — the shape that makes the park reach the operator.
	if len(rel.questions) != 1 {
		t.Fatalf("filed %d questions, want 1: %+v", len(rel.questions), rel.questions)
	}
	q := rel.questions[0]
	if q.taskID != "t-1" {
		t.Errorf("question taskId = %s, want t-1", q.taskID)
	}
	if q.blocking {
		t.Error("question is blocking; the task is already parked, so a blocking question would un-block to ready on any answer")
	}
	if q.kind != "clarification" {
		t.Errorf("question kind = %q, want clarification — a decision would read as project-wide standing law", q.kind)
	}
	for _, want := range []string{harness.FinishReasonUnknown, "no branch recorded", "four", "t-1", "iteration logs"} {
		if !strings.Contains(q.body, want) {
			t.Errorf("question body does not carry %q: %q", want, q.body)
		}
	}
	// Options cover retry / leave parked / re-scope / drop, like the plane's own
	// escalation options.
	if len(q.options) != 4 {
		t.Fatalf("question options = %+v, want the four operator choices", q.options)
	}
	var joined = strings.Join(q.options, "\n")
	for _, want := range []string{"ready", "parked", "re-scop", "Drop"} {
		if !strings.Contains(joined, want) {
			t.Errorf("question options do not cover %q: %+v", want, q.options)
		}
	}

	if out := logs.String(); !strings.Contains(out, "died producing nothing for the 4th consecutive time") {
		t.Errorf("the operator's log does not say why the task was parked:\n%s", out)
	}
}

// The park's outcome stands ALONE if the question insert fails: the status write
// commits first, so a task that could not raise a question is still parked — the
// log must say "parked, and the question is missing", never "left for the next
// claim" (the same ordering the plane's escalateExhausted reasons about).
func TestDrainPhases_AParkOutcomeStandsAloneWhenTheQuestionFails(t *testing.T) {
	logs := captureLogs(t)
	h := &fakeAdapter{steps: []invokeStep{
		{res: held(deadResult(), openClaim())},
		{res: held(deadResult(), openClaim())},
		{res: held(deadResult(), openClaim())},
		{res: held(deadResult(), openClaim())},
	}}
	rel := &parkingReleaser{questionErr: errors.New("ask_question refused")}
	cfg := fastCfg()
	cfg.Phases = twoPhases()
	cfg.Prompt = ""
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: rel}})
	openTestStateDir(t, d)

	_, _, stop, err := drainPhasesOnce(t, d)
	if err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	if stop {
		t.Error("a parked task stopped the whole run")
	}
	if len(rel.parks) != 1 {
		t.Fatalf("parked %d times, want 1 — the park must commit even when the question insert fails: %+v", len(rel.parks), rel.parks)
	}
	if out := logs.String(); !strings.Contains(out, "parked t-1 — four consecutive dead phases, but the question for the operator could not be filed") {
		t.Errorf("the log does not say the task IS parked with a missing question:\n%s", out)
	}
	if out := logs.String(); strings.Contains(out, "left for the next claim") {
		t.Errorf("the log says the task was left for the next claim when it is actually parked:\n%s", out)
	}
}

// The dead classification names "produced nothing" whatever the exit code, so a
// dead phase that ALSO exits non-zero is still retried once then parked — never
// a run-failing error. The non-zero exit is part of the silent death, not a
// verdict (CLA-386).
func TestDrainPhases_ANonZeroExitDeadPhaseIsRetriedNotARunFailure(t *testing.T) {
	dead := func() harness.Result {
		return harness.Result{ExitCode: 1, Raw: map[string]any{
			"kind": "fail", harness.FinishReasonKey: harness.FinishReasonUnknown,
		}}
	}
	h := &fakeAdapter{steps: []invokeStep{
		{res: held(dead(), openClaim())},
		{res: held(dead(), openClaim())},
		{res: held(dead(), openClaim())},
		{res: held(dead(), openClaim())},
	}}
	rel := &parkingReleaser{}
	cfg := fastCfg()
	cfg.Phases = twoPhases()
	cfg.Prompt = ""
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: rel}})
	openTestStateDir(t, d)

	_, _, stop, err := drainPhasesOnce(t, d)
	if err != nil {
		t.Fatalf("drainPhases: %v — a dead phase must be retried then parked, never fail the run", err)
	}
	if stop {
		t.Error("a dead phase stopped the whole run")
	}
	if h.invokeCalls != 4 {
		t.Errorf("spawned %d sessions, want 4 (three retries, then the fourth dead phase parks) — the dead classification must win over the non-retryable error", h.invokeCalls)
	}
	if len(rel.parks) != 1 {
		t.Errorf("parked %d times, want 1: %+v", len(rel.parks), rel.parks)
	}
}

// A run-level stop wins over a dead phase. A credit-starved session is exactly
// the shape that dies on reason "unknown" with no branch — so under a provider
// outage the dead retry and the hard-limit stop can fire together, and the retry
// must not win: it would burn another session into the same limit and, on the
// second, park a task for what was actually a run-wide outage instead of
// stopping the daemon (review finding).
func TestDrainPhases_ARunStopWinsOverADeadPhase(t *testing.T) {
	deadStop := limitStopResult()
	deadStop.Raw[harness.FinishReasonKey] = harness.FinishReasonUnknown
	h := &fakeAdapter{steps: []invokeStep{
		{res: held(deadStop, openClaim())},
	}}
	rel := &parkingReleaser{}
	cfg := fastCfg()
	cfg.Phases = twoPhases()
	cfg.Prompt = ""
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: rel}})
	openTestStateDir(t, d)

	_, _, stop, err := drainPhasesOnce(t, d)
	if err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	if !stop {
		t.Error("a phase that was both dead and a hard limit stop did not stop the run")
	}
	if h.invokeCalls != 1 {
		t.Errorf("spawned %d sessions, want 1 — a dead phase on a stopped run must not be retried into the same limit", h.invokeCalls)
	}
	if len(rel.parks) != 0 {
		t.Errorf("parked %d times, want 0 — a run-wide outage must not be recorded as a task that killed two sessions", len(rel.parks))
	}
}

// The retry budget is PER TASK. The dead retry is a fresh claiming session (the
// dead claim was handed back), and next_task can hand it a different task — so a
// task that has died only once must not be parked because some OTHER task died
// after it. The counter follows the task id: t-1's single death counts for t-1
// alone, and when the retry lands on t-2 the count starts fresh for t-2 (review
// finding, updated for the CLA-396 budget of four). Under the new budget the
// discriminating shape is t-1 dying once then t-2 dying THREE times — four
// consecutive deaths, below the fleet bound of five, so the fleet does not trip
// — and t-2's third death must still be RETRIED, not parked: had the counter
// carried t-1's death forward, t-2 would sit at four and park here.
func TestDrainPhases_ADeadOnADifferentTaskDoesNotParkTheSecondTask(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: held(deadResult(), openClaim())},                                // t-1 dies once
		{res: held(deadResult(), harness.Claim{TaskID: "t-2", RunID: "r-2"})}, // retry lands on t-2
		{res: held(deadResult(), harness.Claim{TaskID: "t-2", RunID: "r-2"})}, // t-2's second death
		{res: held(deadResult(), harness.Claim{TaskID: "t-2", RunID: "r-2"})}, // t-2's third death: RETRY, not park
		{res: checkpointed(1, 0)},                                             // the retry succeeds and the phase advances
		{res: okResult(1, 0)},                                                 // review runs normally
	}}
	rel := &parkingReleaser{}
	cfg := fastCfg()
	cfg.Phases = twoPhases()
	cfg.Prompt = ""
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: rel}})
	openTestStateDir(t, d)
	// The implement-success retry's checkpoint (CLA-457) needs the evidence gate
	// stubbed: a working tree and a passing verifier, like the other helpers.
	d.cfg.WorkDir = t.TempDir()
	d.newVerifier = func(string, bool) deliveryVerifier { return passVerifier() }

	_, _, stop, err := drainPhasesOnce(t, d)
	if err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 6 {
		t.Fatalf("spawned %d sessions, want 6 (t-1 dead, t-2 dead x3 with three retries, then implement success + review) — the retry budget must reset for a different task", h.invokeCalls)
	}
	if len(rel.parks) != 0 {
		t.Fatalf("parked %d times, want 0 — t-2 inherited no deaths from t-1: %+v", len(rel.parks), rel.parks)
	}
	if got := d.fleetDead[0]; got != 0 {
		t.Errorf("fleet counter = %d, want 0 — the implement success after the retry must reset it", got)
	}
}

// wiredParkingReleaser is parkingReleaser plus the plane's single-task read
// (plane.TaskStateSource). The resumed phase in the test below is the
// REVIEW-only phase, whose spawn reads the task's outcome for the seam's
// freshness baseline (CLA-663), so a target with no state reader is refused
// before the phase runs — and the park behaviour this test pins could never be
// reached.
type wiredParkingReleaser struct {
	parkingReleaser
	outcome string
}

func (w *wiredParkingReleaser) TaskState(context.Context, string) (plane.TaskState, error) {
	return plane.TaskState{Status: "in_progress", ClaimedByRun: "r-1", Outcome: w.outcome}, nil
}

// A dead phase on a RESUMED middle phase is not retried: the retry cannot be
// re-seeded — invocationFor only substitutes the task/run placeholders when the
// predecessor claim is present, which the dead handback cleared — so retrying it
// would spawn a session whose brief still carries the literal {{taskId}}/{{runId}}.
// Its dead signature still vetoes the checkpoint (no false advance to the next
// phase), so the drain ends and the task returns to the queue, where a fresh
// claiming session retries it with a valid seed. Unreachable in the shipped
// two-phase sequence (phase 2 is last and cannot be dead); a three-phase config
// reaches it (review finding).
func TestDrainPhases_AResumedPhaseDeathIsNotDeadClassified(t *testing.T) {
	three := []config.Phase{{Name: "implement"}, {Name: "review"}, {Name: "publish"}}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(1, 0)},              // phase 1 checkpoints, holding the claim
		{res: held(deadResult(), openClaim())}, // resumed phase 2 dies producing nothing
	}}
	rel := &wiredParkingReleaser{outcome: "implemented: pushed clanker/x\n"}
	cfg := fastCfg()
	cfg.Phases = three
	cfg.Prompt = ""
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: rel}})
	openTestStateDir(t, d)
	// Phase 1's checkpoint (CLA-457) needs the evidence gate stubbed.
	d.cfg.WorkDir = t.TempDir()
	d.newVerifier = func(string, bool) deliveryVerifier { return passVerifier() }

	_, _, stop, err := drainPhasesOnce(t, d)
	if err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	if stop {
		t.Error("a resumed phase dying producing nothing stopped the whole run")
	}
	if h.invokeCalls != 2 {
		t.Errorf("spawned %d sessions, want 2 (phase 1, then the dead resumed phase) — a resumed dead phase must not be retried with a broken resume seed, and must not advance to phase 3", h.invokeCalls)
	}
	if len(rel.parks) != 0 {
		t.Errorf("parked %d times, want 0 — the retry-then-park budget is the implement phase's", len(rel.parks))
	}
}

// CLA-608: the phase briefs used to hard-code "(staging)" as the PR base for
// every repo of every project — wrong for the makesPDF plugin repos (no staging
// branch; PRs target main), wrong for lecstor/makespdf and lecstor/clankerbar
// (PRs target integration), and raised as a decision on every task in them.
// prBaseFor resolves it per repo at spawn time: the operator's pr_base override
// wins outright; else the project's integration_branch when origin has it; else
// origin's default branch; and a probe that cannot run falls back to the
// integration branch, the text the briefs carried before this feature.
//
// The assertion is on the PROMPT a session would receive — both phases, since
// the implement brief's resumed-work rule names the branch too — driven end to
// end rather than through the helper alone, so a resolution that never reaches
// invocationFor fails here.
func TestPRBaseResolution_ShapesBothPhaseBriefsPerRepo(t *testing.T) {
	const pluginRepo = "makesPDF/makespdf-obsidian-plugin"
	cases := []struct {
		name      string
		repo      string
		prBase    map[string]string
		onOrigin  bool
		defaultBr string
		probeErr  error
		want      string
		// probe calls expected: once per phase, and NONE when an override
		// answers the question without touching the network.
		wantProbes int
	}{
		{
			name:       "no override and no staging on origin: origin's default branch",
			repo:       pluginRepo,
			onOrigin:   false,
			defaultBr:  "main",
			want:       "main",
			wantProbes: 2,
		},
		{
			name:       "the per-repo override wins outright, no probe",
			repo:       "lecstor/makespdf",
			prBase:     map[string]string{"lecstor/makespdf": "integration"},
			onOrigin:   true,
			defaultBr:  "main",
			want:       "integration",
			wantProbes: 0,
		},
		{
			name:       "integration branch present on origin still wins over the default",
			repo:       "lecstor/ezyapp",
			onOrigin:   true,
			defaultBr:  "main",
			want:       "staging",
			wantProbes: 2,
		},
		{
			name:       "a probe that cannot run falls back to the integration branch",
			repo:       pluginRepo,
			probeErr:   errors.New("could not read remote"),
			want:       "staging",
			wantProbes: 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &fakeAdapter{steps: []invokeStep{
				{res: checkpointed(1, 0)},
				{res: okResult(1, 0)},
			}}
			cfg := fastCfg()
			cfg.Phases = twoPhases()
			cfg.Prompt = ""
			cfg.PRBase = tc.prBase
			parent := t.TempDir()
			checkout := gitCheckout(t, filepath.Join(parent, "checkout"))
			rel := &repoReleaser{
				next:     plane.NextTask{TaskID: "t-1", Repo: tc.repo},
				taskRepo: map[string]string{"t-1": tc.repo},
			}
			d := NewMulti(cfg, h, []Target{{
				Poller:   busyPoller(),
				WorkDir:  parent,
				Repos:    map[string]string{tc.repo: checkout},
				Releaser: rel,
			}})
			openTestStateDir(t, d)
			d.newVerifier = func(string, bool) deliveryVerifier { return passVerifier() }
			probes := 0
			d.probePRBase = func(_ context.Context, dir, integration string) (bool, string, error) {
				probes++
				if dir != checkout {
					t.Errorf("probe ran in %q, want the task repo's checkout %q", dir, checkout)
				}
				if integration != config.DefaultIntegrationBranch {
					t.Errorf("probe asked about %q, want the configured integration branch %q", integration, config.DefaultIntegrationBranch)
				}
				return tc.onOrigin, tc.defaultBr, tc.probeErr
			}

			if _, _, _, err := drainPhasesOnce(t, d); err != nil {
				t.Fatalf("drainPhases: %v", err)
			}
			if h.invokeCalls != 2 {
				t.Fatalf("spawned %d sessions, want 2", h.invokeCalls)
			}
			for i, label := range []string{"phase 1 (implement)", "phase 2 (review)"} {
				prompt := h.invocations[i].Prompt
				if !strings.Contains(prompt, "PR base branch ("+tc.want+")") {
					t.Errorf("%s brief does not name %q as the PR base:\n%s", label, tc.want, prompt)
				}
				if strings.Contains(prompt, config.PhasePRBasePlaceholder) {
					t.Errorf("%s brief still carries the unfilled %s placeholder:\n%s", label, config.PhasePRBasePlaceholder, prompt)
				}
			}
			if probes != tc.wantProbes {
				t.Errorf("origin was probed %d times, want %d — once per phase, never when the override answers", probes, tc.wantProbes)
			}
		})
	}
}

// A handoff respawn rebuilds the phase's prompt from the predecessor's own text
// plus config.HandoffContinuation, which carries reviewTerminalStep — itself a
// {{prBase}} carrier since CLA-608. So the substitution must reach handoff
// prompts too, or the successor gets a literal placeholder where the PR target
// should be. The existing handoff test pins the terminal step's presence; this
// pins that its branch has actually been resolved.
func TestPRBaseResolution_ReachesHandoffRespawnPrompts(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(1, 0)},
		{res: handoffResult("finish the review")},
		{res: okResult(1, 0)},
	}}
	cfg := fastCfg()
	cfg.Phases = twoPhases()
	cfg.Prompt = ""
	parent := t.TempDir()
	checkout := gitCheckout(t, filepath.Join(parent, "checkout"))
	rel := &repoReleaser{
		next:     plane.NextTask{TaskID: "t-1", Repo: "makesPDF/makespdf-obsidian-plugin"},
		taskRepo: map[string]string{"t-1": "makesPDF/makespdf-obsidian-plugin"},
	}
	d := NewMulti(cfg, h, []Target{{
		Poller:   busyPoller(),
		WorkDir:  parent,
		Repos:    map[string]string{"makesPDF/makespdf-obsidian-plugin": checkout},
		Releaser: rel,
	}})
	openTestStateDir(t, d)
	d.newVerifier = func(string, bool) deliveryVerifier { return passVerifier() }
	d.probePRBase = func(context.Context, string, string) (bool, string, error) { return false, "main", nil }

	if _, _, _, _, err := drainPhasesHandoffs(t, d, 1); err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	if h.invokeCalls != 3 {
		t.Fatalf("spawned %d sessions, want 3 (implement, review, handoff respawn)", h.invokeCalls)
	}
	successor := h.invocations[2].Prompt
	if !strings.Contains(successor, "PR base branch (main)") {
		t.Errorf("the handoff successor's PR step does not name the resolved branch:\n%s", successor)
	}
	if strings.Contains(successor, config.PhasePRBasePlaceholder) {
		t.Errorf("the handoff successor still carries the unfilled %s placeholder:\n%s", config.PhasePRBasePlaceholder, successor)
	}
}

// reviewReleaser is the CLA-637 seam's fake: the plane's single-task read
// (TaskStateSource, serving the durable review records), and the round close
// the driver performs at the review→fix seam (ReviewRoundCloser). State is read
// through the helper fields so a test can mutate it from fakeAdapter.onInvoke —
// the way a real review session's update_task(review: ...) lands — and the
// closes record what the driver actually sent.
type reviewReleaser struct {
	fakeReleaser
	status  string
	outcome string
	holder  string
	records []plane.ReviewRecordState

	// counted and openMusts are the plane's phase-4 answers (CLA-636/638): the
	// non-void round count in the newest record's cycle and the open MUST ids on
	// released records. A test mutates them from fakeAdapter.onInvoke, the way a
	// real verify session's record append and a round close land.
	counted   int
	openMusts []string

	// readErr, when set, is consulted on every TaskState read: a test injects a
	// failing read (consumed once, say) to drive the CLA-663 baseline-read retry
	// without a second fake. reads counts every read served.
	readErr func() error
	reads   int

	closes   []int
	closeErr error

	// parks and questions are what the driver sent when the stopping rule parked
	// the task (CLA-638), so a test asserts the disposition, not only that
	// something happened.
	parks     []string
	questions []string
}

func (r *reviewReleaser) TaskState(_ context.Context, _ string) (plane.TaskState, error) {
	r.reads++
	if r.readErr != nil {
		if err := r.readErr(); err != nil {
			return plane.TaskState{}, err
		}
	}
	return plane.TaskState{
		Status:            r.status,
		ClaimedByRun:      r.holder,
		Outcome:           r.outcome,
		Reviews:           r.records,
		ReviewCounted:     r.counted,
		ReviewOpenMustIDs: r.openMusts,
	}, nil
}

func (r *reviewReleaser) CloseReviewRound(_ context.Context, _, _ string, round int) error {
	if r.closeErr != nil {
		return r.closeErr
	}
	// The real plane releases the round's findings on close (state sealed -> open,
	// CLA-636); the fake must too, or a later digest/resolved read would see the
	// records exactly as the seal left them.
	for i := range r.records {
		if r.records[i].Round == round && r.records[i].State == "sealed" {
			r.records[i].State = "open"
		}
	}
	r.closes = append(r.closes, round)
	return nil
}

func (r *reviewReleaser) Park(_ context.Context, taskID, runID, outcome string) error {
	r.parks = append(r.parks, outcome)
	return nil
}

func (r *reviewReleaser) AskQuestion(_ context.Context, _, body string, _ []string, _ bool, _ string) error {
	r.questions = append(r.questions, body)
	return nil
}

func (r *reviewReleaser) AskProjectQuestion(_ context.Context, _ string, _ []string, _ string) error {
	return nil
}

// recordFor is one durable review record as the plane would serve it after the
// review session appended it: identity, the reviewed tip, the seal state, and
// nothing else the gate reads.
func recordFor(round int, role, run, tip, state string) plane.ReviewRecordState {
	return plane.ReviewRecordState{Round: round, ReviewerRole: role, RunID: run, ReviewedTip: tip, State: state}
}

// The split this feature ships: implement, review-only, fix. The review phase
// must be read-only (no PR, no commit, no in_review) and must record its findings
// on the task; the fix phase must be the last one, carry the terminal step, and
// name both sides' attribution. The count in each brief is the SEQUENCE's, not
// the old hard-coded "of 2".
func TestDrainPhases_RunsTheThreePhaseSplit(t *testing.T) {
	phases := []config.Phase{
		{Name: config.ImplementPhaseName, Tier: "cheap"},
		{Name: config.ReviewPhaseName, Tier: "strong"},
		{Name: config.FixPhaseName, Tier: "cheap"},
	}
	// The plane state the fixture serves, mutated where the review session
	// writes: since CLA-637 the seam gate requires a durable review record
	// written by THIS run, so a healthy fixture must show the review session
	// appending one — and keeping the legacy block alongside it, which the
	// shipped brief still prescribes for one release.
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)},
		{res: checkpointed(5, 0.50)},
		{res: okResult(5, 0.05)},
	}}
	h.onInvoke = func(i int) {
		if i == 1 { // the review-only session records both halves
			rel.outcome += "\n" + config.ReviewRecordHeading + "\n- reviewed tip: " + testCheckpointTip + "\n- verdict: pass\n"
			rel.records = append(rel.records, recordFor(1, "review-1", "r-1", testCheckpointTip, "sealed"))
		}
		if i == 2 { // the fix session: the round must ALREADY be closed
			if len(rel.closes) != 1 {
				t.Errorf("the fix phase spawned with the review round not yet closed (closes=%v) — it would be unable to read the findings it was told to apply", rel.closes)
			}
		}
	}
	d := phaseDriverWithReleaser(t, h, phases, rel)
	d.cfg.Models = map[string]string{"strong": "opus-test", "cheap": "cheap-test"}

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 3 {
		t.Fatalf("spawned %d sessions, want 3 — implement, review-only, fix", h.invokeCalls)
	}
	// The driver closed exactly the round the review record landed in — the
	// round it MATCHED, not a predicted one (CLA-637).
	if len(rel.closes) != 1 || rel.closes[0] != 1 {
		t.Fatalf("closed rounds = %v, want exactly [1] — the fix phase depends on the round being released", rel.closes)
	}

	p1, p2, p3 := h.invocations[0].Prompt, h.invocations[1].Prompt, h.invocations[2].Prompt
	for i, p := range []string{p1, p2, p3} {
		if strings.Contains(p, "{{") {
			t.Errorf("phase %d's prompt still carries an unresolved placeholder:\n%s", i+1, p)
		}
	}
	if !strings.Contains(p1, "PHASE 1 of 3") {
		t.Errorf("the implement brief does not count the three-phase sequence:\n%s", p1)
	}
	if !strings.Contains(p2, "PHASE 2 of 3") || !strings.Contains(p2, "REVIEW-ONLY gate") {
		t.Errorf("phase 2 is not the review-only gate of a 3-phase sequence:\n%s", p2)
	}
	// The review must not be told to do the fix phase's job. "Open a PR" in the
	// positive is the tell; the review's own "do not open a PR" is lowercase.
	if strings.Contains(p2, "Open a PR") || strings.Contains(p2, `status: "in_review"`) {
		t.Errorf("the review-only phase carries the fix phase's terminal actions:\n%s", p2)
	}
	if !strings.Contains(p2, "reviewed by: harness claude, model opus-test") ||
		!strings.Contains(p2, "implemented by: harness claude, model cheap-test") {
		t.Errorf("the review record does not name both sides' resolved attribution:\n%s", p2)
	}
	// CLA-637: the review brief carries the durable record's identity — the
	// reviewer role the seam keys on and the verified tip the reviewer must
	// check out and record — filled from the checkpoint, not left as text.
	if !strings.Contains(p2, `"review-1"`) || !strings.Contains(p2, testCheckpointTip) {
		t.Errorf("the review brief does not carry the durable record's role and reviewed tip:\n%s", p2)
	}
	if !strings.Contains(p2, "sealed durable") {
		t.Errorf("the review brief does not describe the durable record it must append:\n%s", p2)
	}

	if !strings.Contains(p3, "PHASE 3 of 3") || !strings.Contains(p3, "FIX phase and the LAST phase") {
		t.Errorf("phase 3 is not the fix phase of a 3-phase sequence:\n%s", p3)
	}
	for _, want := range []string{
		"Open a PR",
		`status: "in_review"`,
		"fixed by: harness claude, model cheap-test",
		"### Fixes",
		// CLA-637: the fix brief must tell the session to answer the released
		// durable record by plane finding id before the hand-over.
		"update every finding's status on the DURABLE review record",
		// The rendered fix prompt must also point at the LAST `## Review` block:
		// a re-trip can carry several (CLA-663).
		"LAST `" + config.ReviewRecordHeading + "`",
	} {
		if !strings.Contains(p3, want) {
			t.Errorf("the fix brief does not carry %q:\n%s", want, p3)
		}
	}

	logs := strings.Join(iterationLogs(t, d.state.Path()), " ")
	for _, tag := range []string{"-pimplement-", "-preview-", "-pfix-"} {
		if !strings.Contains(logs, tag) {
			t.Errorf("no iteration log carries the phase tag %q: %s", tag, logs)
		}
	}
}

// CLA-635: the fix phase's prompt carries the driver's graded list of the
// review's findings, prepended before the brief — a grounded MUST keeps its
// grade, an evidence-less MUST is listed as a SHOULD (B5's structural check is
// the driver's to run), and the fix brief itself states the obligations the
// grade imposes.
func TestDrainPhases_FixBriefCarriesTheGradedFindings(t *testing.T) {
	phases := []config.Phase{
		{Name: config.ImplementPhaseName, Tier: "cheap"},
		{Name: config.ReviewPhaseName, Tier: "strong"},
		{Name: config.FixPhaseName, Tier: "cheap"},
	}
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)},
		{res: checkpointed(5, 0.50)},
		{res: okResult(5, 0.05)},
	}}
	h.onInvoke = func(i int) {
		if i == 1 { // the review session records both halves
			rel.outcome += "\n" + config.ReviewRecordHeading + "\n- reviewed tip: " + testCheckpointTip + "\n- verdict: findings\n### Findings\n" +
				"- R1 [must] (gate): `go test ./...` fails on the new seam at loop.go:42 - the guard is untested. Required fix: pin it.\n" +
				"- R2 [must] internal/loop/loop.go:77 - this line names no evidence. Required fix: give it a test.\n" +
				"- R3 [should] internal/loop/loop.go:90 - tidy the wording. Required fix: tidy it.\n" +
				"- R4 [nit] internal/loop/loop.go:97 - a nit. Required fix: none.\n"
			rel.records = append(rel.records, recordFor(1, "review-1", "r-1", testCheckpointTip, "sealed"))
		}
	}
	d := phaseDriverWithReleaser(t, h, phases, rel)
	d.cfg.Models = map[string]string{"strong": "opus-test", "cheap": "cheap-test"}

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 3 {
		t.Fatalf("spawned %d sessions, want 3 — implement, review-only, fix", h.invokeCalls)
	}

	p3 := h.invocations[2].Prompt
	for _, want := range []string{
		"R1 [MUST]", "R2 [SHOULD]", "R3 [SHOULD]", "R4 [NIT]",
		"downgraded: recorded as a MUST with no evidence kind or payload",
	} {
		if !strings.Contains(p3, want) {
			t.Errorf("the fix phase's prompt does not carry %q:\n%s", want, p3)
		}
	}
	if strings.Contains(p3, "R2 [MUST]") {
		t.Errorf("the evidence-less MUST was not downgraded in the fix phase's prompt:\n%s", p3)
	}
	// The graded list PRECEDES the brief: it is the driver's read of the record
	// the session is about to act on, not an afterthought to it.
	if i, j := strings.Index(p3, "R1 [MUST]"), strings.Index(p3, "the FIX phase and the LAST phase"); i < 0 || j < 0 || i > j {
		t.Errorf("the graded findings do not sit before the fix brief (finding at %d, brief at %d):\n%s", i, j, p3)
	}
	// And the brief itself states the obligations the grade imposes (A1/B5):
	// every MUST fixed or refuted, never downgraded, a lone reviewer's MUST
	// never dropped.
	for _, want := range []string{
		"fixed, or REFUTED with evidence",
		"may not downgrade a MUST",
		"a MUST raised by a single reviewer is never dropped",
	} {
		if !strings.Contains(p3, want) {
			t.Errorf("the fix brief does not carry %q:\n%s", want, p3)
		}
	}
}

// CLA-635 fix-pass finding: a handoff respawn of the FIX phase replaces
// ph.Prompt with the successor's continuation, so keying the driver's graded
// list on the per-call prompt text would drop it exactly where the
// continuation (fixTerminalRecord) still holds the successor to the grade. The
// successor of a mid-fix handoff must carry the same digest its predecessor
// got — an evidence-less MUST must reach it already downgraded, not as a
// gating MUST it may not downgrade itself.
func TestDrainPhases_FixHandoffSuccessorKeepsTheGradedFindings(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)},
		{res: checkpointed(5, 0.50)},
		{res: handoffResult("R1 is fixed and committed; grading R2's evidence is next, then the PR.")},
		{res: okResult(5, 0.05)},
	}}
	h.onInvoke = func(i int) {
		if i == 1 { // the review session records both halves
			rel.outcome += "\n" + config.ReviewRecordHeading + "\n- reviewed tip: " + testCheckpointTip + "\n- verdict: findings\n### Findings\n" +
				"- R1 [must] (gate): `go test ./...` fails on the new seam at loop.go:42 - the guard is untested. Required fix: pin it.\n" +
				"- R2 [must] internal/loop/loop.go:77 - this line names no evidence. Required fix: give it a test.\n"
			rel.records = append(rel.records, recordFor(1, "review-1", "r-1", testCheckpointTip, "sealed"))
		}
	}
	d := phaseDriverWithReleaser(t, h, []config.Phase{
		{Name: config.ImplementPhaseName},
		{Name: config.ReviewPhaseName},
		{Name: config.FixPhaseName},
	}, rel)

	if _, _, handoffs, stop, err := drainPhasesHandoffs(t, d, 1); err != nil || stop || handoffs != 1 {
		t.Fatalf("drainPhases: err=%v stop=%v handoffs=%d (want 1)", err, stop, handoffs)
	}
	if h.invokeCalls != 4 {
		t.Fatalf("spawned %d sessions, want 4 — implement, review, fix, its handoff successor", h.invokeCalls)
	}
	successor := h.invocations[3].Prompt
	for _, want := range []string{
		"R1 [MUST]",
		"R2 [SHOULD]", // the evidence-less MUST reaches the successor already downgraded
		"downgraded: recorded as a MUST with no evidence kind or payload",
	} {
		if !strings.Contains(successor, want) {
			t.Errorf("the fix handoff successor's prompt does not carry %q:\n%s", want, successor)
		}
	}
	if strings.Contains(successor, "R2 [MUST]") {
		t.Errorf("the evidence-less MUST reached the fix handoff successor ungraded:\n%s", successor)
	}
}

// A handoff inside the review-only phase must carry the record-then-hold ending,
// never the commit/push/PR/in_review terminal step. This is the seam a wording
// change in the first session's brief cannot reach.
func TestDrainPhases_AHandoffDuringReviewOnlyKeepsTheReviewMode(t *testing.T) {
	// The handoff successor continues the SAME phase, so it takes a fresh
	// freshness baseline: the first review session records nothing (it hands
	// off mid-review), and only the successor's record satisfies the gate —
	// same run, same role, and the round stays SEALED until the successor's
	// own seam closes it.
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	// The handoff session is a RESUMED phase, so the adapters seed its claim
	// from ResumeClaim (claude.go's Result{Claim: in.ResumeClaim}): the branch
	// and WIP the implement checkpoint handed over survive into its result,
	// which is what lets the successor's brief still name the verified branch
	// and tip. handoffResult alone models a claim-less stream.
	midReview := handoffResult("Review half done: the API surface is cleared, the storage layer is next.")
	midReview.Claim.Branch = "clanker/x"
	midReview.Claim.HasWIP = true
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)},
		{res: midReview},
		{res: checkpointed(5, 0.50)},
		{res: okResult(1, 0)},
	}}
	h.onInvoke = func(i int) {
		if i == 2 { // the handoff successor records the durable record the seam requires
			rel.outcome += "\n" + config.ReviewRecordHeading + "\n- reviewed tip: " + testCheckpointTip + "\n- verdict: pass\n"
			rel.records = append(rel.records, recordFor(1, "review-1", "r-1", testCheckpointTip, "sealed"))
		}
	}
	d := phaseDriverWithReleaser(t, h, []config.Phase{
		{Name: config.ImplementPhaseName},
		{Name: config.ReviewPhaseName},
		{Name: config.FixPhaseName},
	}, rel)

	if _, _, _, stop, err := drainPhasesHandoffs(t, d, 1); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 4 {
		t.Fatalf("spawned %d sessions, want 4: implement, review, its handoff successor, fix", h.invokeCalls)
	}
	successor := h.invocations[2].Prompt
	if !strings.Contains(successor, "review-only ending") {
		t.Errorf("the review-only handoff successor did not get the review-only ending:\n%s", successor)
	}
	if strings.Contains(successor, "Open a PR") || strings.Contains(successor, `status: "in_review"`) {
		t.Errorf("the review-only handoff successor was told to finish the task:\n%s", successor)
	}
	if !strings.Contains(successor, `"review-1"`) || !strings.Contains(successor, testCheckpointTip) {
		t.Errorf("the handoff successor's brief does not carry the record's role and reviewed tip:\n%s", successor)
	}
	// The round is closed once — at the successor's seam, when the review
	// stage is actually over — not at the session that handed off.
	if len(rel.closes) != 1 || rel.closes[0] != 1 {
		t.Fatalf("closed rounds = %v, want exactly [1] at the successor's seam", rel.closes)
	}
	if !strings.Contains(h.invocations[3].Prompt, "PHASE 3 of 3") {
		t.Errorf("the fix phase did not run after the review-only handoff chain:\n%s", h.invocations[3].Prompt)
	}
}

// resumedCheckpointed is checkpointed with the claim a RESUMED phase really
// carries: HasWIP, from the branch its predecessor recorded. That flag is
// load-bearing for the refusal tests below — it is why the driver leaves the
// lease to lapse into a takeover instead of handing the task back to the queue
// over pushed work (Claim.Releasable).
func resumedCheckpointed(tokens int, cost float64) harness.Result {
	return reported(held(okResult(tokens, cost), harness.Claim{
		TaskID: "t-1", RunID: "r-1", HasWIP: true, Branch: "clanker/x",
	}), branchReport())
}

// The review-shaped seam must not advance to fix on the implementer's branch
// alone. That branch is INHERITED (HasWIP from phase 1), so every existing
// evidence form passes for a review session that recorded nothing at all — a
// session that burned its turns, hit its ceiling, or died on a silent EOT.
// Since CLA-637 the gate is the DURABLE record: this run's, under this
// reviewer role, naming the tip the driver verified. Every way a record can
// fail to be that — a different run (a request_changes re-trip), a voided
// round (a takeover), a different role, a stale tip — must refuse here, or the
// fixer runs over an unreviewed revision and an unreviewed PR reaches
// in_review: the regression the split must not introduce.
func TestDrainPhases_ReviewOnlySeamRefusesACheckpointWithoutANewRecord(t *testing.T) {
	// The stale artefacts every case carries: a task that already went
	// implement -> review -> fix keeps the previous trip's `## Review` block in
	// its outcome AND its durable record in the reviews block, and a human
	// request_changes (or a takeover after the lease lapsed) returns the task
	// to the queue without clearing either.
	staleBlock := "delivered: pushed clanker/x\n\n" + config.ReviewRecordHeading +
		"\n- reviewed tip: oldsha\n- verdict: findings\n### Findings\n- R1 [must] a.go:1 - stale. Required fix: none.\n### Fixes\n- R1 fixed in deadbeef\n"
	staleRecord := recordFor(1, "review-1", "r-old", "oldsha", "open")
	takeover := harness.Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true, Branch: "clanker/x"}

	tests := []struct {
		name     string
		prior    []plane.ReviewRecordState // records the plane already carries
		baseline string                    // the outcome the review phase finds on the task
		first    harness.Claim             // phase 1's claim: a takeover seeds WIP
		write    func(rel *reviewReleaser) // what the review session records (identity records nothing)
	}{
		{
			name:  "the review records nothing",
			first: openClaim(),
			write: func(*reviewReleaser) {},
		},
		{
			name:     "a request_changes re-trip's previous record is a different run",
			prior:    []plane.ReviewRecordState{staleRecord},
			baseline: staleBlock,
			first:    openClaim(),
			write:    func(*reviewReleaser) {},
		},
		{
			name:     "a takeover leaves the previous round void",
			prior:    []plane.ReviewRecordState{recordFor(1, "review-1", "r-old", testCheckpointTip, "void")},
			baseline: staleBlock,
			first:    takeover,
			write:    func(*reviewReleaser) {},
		},
		{
			name:  "a record this run wrote under another reviewer role is not the gate's record",
			first: openClaim(),
			write: func(rel *reviewReleaser) {
				rel.records = append(rel.records, recordFor(1, "review-2", "r-1", testCheckpointTip, "sealed"))
			},
		},
		{
			name:  "a record naming a tip the driver did not verify is stale",
			first: openClaim(),
			write: func(rel *reviewReleaser) {
				rel.records = append(rel.records, recordFor(1, "review-1", "r-1", "some-other-tip", "sealed"))
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rel := &reviewReleaser{
				status: "in_progress", holder: "r-1", outcome: tc.baseline,
				records: append([]plane.ReviewRecordState(nil), tc.prior...),
			}
			h := &fakeAdapter{steps: []invokeStep{
				// Phase 1's session: on a takeover its claim already carries the
				// branch and WIP flag of the found work.
				{res: reported(held(okResult(10, 0.10), tc.first), branchReport())},
				{res: resumedCheckpointed(5, 0.50)}, // holds the task and phase 1's branch, but records nothing the gate accepts
				{res: okResult(5, 0.05)},            // must NEVER run
			}}
			h.onInvoke = func(i int) {
				if i == 1 {
					tc.write(rel)
				}
			}
			d := phaseDriverWithReleaser(t, h, []config.Phase{
				{Name: config.ImplementPhaseName},
				{Name: config.ReviewPhaseName},
				{Name: config.FixPhaseName},
			}, rel)

			_, _, stop, err := drainPhasesOnce(t, d)
			if err != nil || stop {
				t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
			}
			if h.invokeCalls != 2 {
				t.Fatalf("spawned %d sessions, want 2 — the fix phase must not run without this run's review record", h.invokeCalls)
			}
			if len(rel.closes) != 0 {
				t.Errorf("the refused review checkpoint closed a round (%v); nothing was released to close", rel.closes)
			}
			// The resumed review claim carries HasWIP, so the refusal must NOT
			// hand the task back to the queue: releasing it would throw away
			// the takeover flag that tells the next clanker pushed work is
			// waiting. The lease lapses and a takeover picks it up.
			if len(rel.calls) != 0 {
				t.Errorf("the refused review checkpoint released the task (%+v); a HasWIP claim must be left to lapse into a takeover", rel.calls)
			}
		})
	}
}

// The reverse of the stale record, and the shape a request_changes re-trip
// produces when the review does its job: the task still carries round 1's
// record and the outcome still carries round 1's block, and the review APPENDS
// round 2's block below it and writes a NEW durable record under THIS run
// (CLA-637). The gate must advance on the new record — refusing merely because
// an earlier record exists would stall every re-trip — and the driver closes
// the round the NEW record landed in.
func TestDrainPhases_ReviewSeamAcceptsANewRecordOnARetrip(t *testing.T) {
	round1 := "delivered: pushed clanker/x\n\n" + config.ReviewRecordHeading +
		"\n- reviewed tip: oldsha\n- verdict: findings\n### Findings\n- R1 [must] a.go:1 - stale. Required fix: none.\n### Fixes\n- R1 fixed in deadbeef\n"
	rel := &reviewReleaser{
		status: "in_progress", holder: "r-1",
		outcome: round1,
		records: []plane.ReviewRecordState{recordFor(1, "review-1", "r-old", "oldsha", "open")},
	}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)},
		{res: resumedCheckpointed(5, 0.50)}, // the review appends round 2 below round 1, and records under this run
		{res: okResult(5, 0.05)},            // the fix runs on the new record
	}}
	h.onInvoke = func(i int) {
		if i == 1 {
			rel.outcome += "\n" + config.ReviewRecordHeading + "\n- reviewed tip: " + testCheckpointTip + "\n- verdict: pass\n"
			rel.records = append(rel.records, recordFor(2, "review-1", "r-1", testCheckpointTip, "sealed"))
		}
	}
	d := phaseDriverWithReleaser(t, h, []config.Phase{
		{Name: config.ImplementPhaseName},
		{Name: config.ReviewPhaseName},
		{Name: config.FixPhaseName},
	}, rel)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 3 {
		t.Fatalf("spawned %d sessions, want 3 — a NEW record this run must advance a re-trip", h.invokeCalls)
	}
	if len(rel.closes) != 1 || rel.closes[0] != 2 {
		t.Fatalf("closed rounds = %v, want exactly [2] — the round the new record landed in", rel.closes)
	}
}

// CLA-663: a baseline read that fails would once spawn the full review session
// anyway, and the seam then refused its checkpoint — the review's whole spend
// thrown away, the HasWIP claim lapsed into a takeover, and the takeover re-ran
// implement AND review. The read is retried a bounded number of times; if it is
// still unread the review must not spawn at all, the reason must be logged, and
// the claim must get the disposition a seam refusal leaves it.
func TestDrainPhases_AnUnreadableBaselineRefusesBeforeTheReviewSpawns(t *testing.T) {
	phases := []config.Phase{
		{Name: config.ImplementPhaseName},
		{Name: config.ReviewPhaseName},
		{Name: config.FixPhaseName},
	}

	t.Run("a HasWIP claim is left to lapse into a takeover", func(t *testing.T) {
		logs := captureLogs(t)
		h := &fakeAdapter{steps: []invokeStep{
			{res: resumedCheckpointed(10, 0.10)},
			{res: resumedCheckpointed(5, 0.50)}, // must NEVER run: no readable baseline
			{res: okResult(5, 0.05)},            // must NEVER run: nothing to apply
		}}
		rel := &wiredReleaser{
			state: func() plane.TaskState {
				return plane.TaskState{Status: "in_progress", ClaimedByRun: "r-1"}
			},
			err: func() error { return errors.New("plane blip: read timeout") },
		}
		d := phaseDriverWithReleaser(t, h, phases, rel)

		if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
			t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
		}
		if h.invokeCalls != 1 {
			t.Fatalf("spawned %d sessions, want 1 — the review must not spawn with an unreadable baseline, and the fix has nothing to apply", h.invokeCalls)
		}
		if rel.reads != reviewBaselineReadAttempts {
			t.Errorf("the baseline read was attempted %d times, want the bound (%d) before refusing", rel.reads, reviewBaselineReadAttempts)
		}
		if len(rel.calls) != 0 {
			t.Errorf("the refusal released the task (%+v); a HasWIP claim must be left to lapse into a takeover, exactly as a seam refusal leaves it", rel.calls)
		}
		if out := logs.String(); !strings.Contains(out, "refusing to spawn the review") {
			t.Errorf("the refusal is not logged in the daemon log:\n%s", out)
		}
	})

	t.Run("a releasable claim is handed back", func(t *testing.T) {
		h := &fakeAdapter{steps: []invokeStep{
			{res: checkpointed(10, 0.10)},
			{res: resumedCheckpointed(5, 0.50)}, // must NEVER run
			{res: okResult(5, 0.05)},            // must NEVER run
		}}
		rel := &wiredReleaser{
			state: func() plane.TaskState {
				return plane.TaskState{Status: "in_progress", ClaimedByRun: "r-1"}
			},
			err: func() error { return errors.New("plane blip") },
		}
		d := phaseDriverWithReleaser(t, h, phases, rel)

		if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
			t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
		}
		if h.invokeCalls != 1 {
			t.Fatalf("spawned %d sessions, want 1 — the review must not spawn with an unreadable baseline", h.invokeCalls)
		}
		if len(rel.calls) != 1 || rel.calls[0].taskID != "t-1" {
			t.Errorf("a releasable claim was not handed back (%+v); a seam refusal releases what the plane will accept and leaves only pushed work to lapse", rel.calls)
		}
	})
}

// The retry is the point, not the refusal: a baseline read that fails once and
// succeeds on the next attempt must let the whole sequence run — review and fix
// included — exactly as if the blip had not happened (CLA-663).
func TestDrainPhases_AFailedBaselineReadIsRetried(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)},
		{res: resumedCheckpointed(5, 0.50)},
		{res: okResult(5, 0.05)},
	}}
	h.onInvoke = func(i int) {
		if i == 1 { // the review records both halves: its legacy block and the durable record
			rel.outcome += "\n" + config.ReviewRecordHeading + "\n- reviewed tip: " + testCheckpointTip + "\n- verdict: pass\n"
			rel.records = append(rel.records, recordFor(1, "review-1", "r-1", testCheckpointTip, "sealed"))
		}
	}
	fails := 1
	rel.readErr = func() error {
		if fails > 0 {
			fails--
			return errors.New("plane blip")
		}
		return nil
	}
	d := phaseDriverWithReleaser(t, h, []config.Phase{
		{Name: config.ImplementPhaseName},
		{Name: config.ReviewPhaseName},
		{Name: config.FixPhaseName},
	}, rel)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 3 {
		t.Fatalf("spawned %d sessions, want 3 — a retried baseline read must not stop the sequence", h.invokeCalls)
	}
	if fails != 0 {
		t.Errorf("the injected blip was never consumed by a retry (fails=%d, reads=%d)", fails, rel.reads)
	}
}

// The freshness gate on the BRANCHLESS path: form (b)/(c) evidence has no
// branch at all, so the review record is the only proof the review phase did
// anything, and the same "changed since spawn" rule applies. The no-code fix
// variant must spawn on a fresh record and not without one.
func TestDrainPhases_NoCodeReviewSeamRequiresANewRecord(t *testing.T) {
	phases := []config.Phase{
		{Name: config.ImplementPhaseName},
		{Name: config.ReviewPhaseName},
		{Name: config.FixPhaseName},
	}
	settled := plane.TaskState{Status: "in_review", Outcome: "no-code delivery: the task body was updated\n"}

	t.Run("a fresh record advances to the no-code fix", func(t *testing.T) {
		h := &fakeAdapter{steps: []invokeStep{
			{res: held(okResult(1, 0), openClaim())}, // no branch: form (b) settles the task
			{res: held(okResult(1, 0), openClaim())}, // review-only: records and holds
			{res: okResult(1, 0)},                    // no-code fix
		}}
		rel := &peekReleaser{state: map[string]plane.TaskState{"t-1": settled}}
		h.onInvoke = func(i int) {
			if i == 1 { // the review records both halves: the block, then the durable record with no tip
				st := rel.state["t-1"]
				st.Outcome += "\n" + config.ReviewRecordHeading + "\n- reviewed tip: none (no branch - the plane record was reviewed)\n- verdict: pass\n"
				st.Reviews = append(st.Reviews, recordFor(1, "review-1", "r-1", "", "sealed"))
				rel.state["t-1"] = st
			}
		}
		d := phaseDriverWithReleaser(t, h, phases, rel)

		if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
			t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
		}
		if h.invokeCalls != 3 {
			t.Fatalf("spawned %d sessions, want 3 — a fresh record advances the no-code seam", h.invokeCalls)
		}
		p3 := h.invocations[2].Prompt
		if !strings.Contains(p3, "there is NO branch") {
			t.Errorf("the successor is not the no-code fix brief:\n%s", p3)
		}
		if len(rel.closes) != 1 || rel.closes[0] != 1 {
			t.Fatalf("closed rounds = %v, want exactly [1] — the no-code round must be released too", rel.closes)
		}
	})

	t.Run("the legacy block alone no longer carries the seam", func(t *testing.T) {
		h := &fakeAdapter{steps: []invokeStep{
			{res: held(okResult(1, 0), openClaim())},
			{res: held(okResult(1, 0), openClaim())}, // review appends the legacy block but NO durable record
			{res: okResult(1, 0)},                    // must NEVER run
		}}
		rel := &peekReleaser{state: map[string]plane.TaskState{"t-1": settled}}
		h.onInvoke = func(i int) {
			if i == 1 {
				st := rel.state["t-1"]
				st.Outcome += "\n" + config.ReviewRecordHeading + "\n- verdict: pass\n"
				rel.state["t-1"] = st
			}
		}
		d := phaseDriverWithReleaser(t, h, phases, rel)

		if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
			t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
		}
		if h.invokeCalls != 2 {
			t.Fatalf("spawned %d sessions, want 2 — the fix must not run on a block with no durable record behind it", h.invokeCalls)
		}
	})
}

// The seam's second read is as load-bearing as its first: the baseline the
// gate reads at spawn is only worth having if the task can be read AGAIN once
// the review session has ended. A plane that blips between the two reads — the
// read clean at spawn, the seam read failing after a review that DID record —
// must be refused fail-closed: an unreadable reviews block cannot prove this
// run's record, and the alternative is advancing on the branch the review
// phase merely inherited. This is the `!recordRead` arm of the gate, and the
// fixture has to fail BETWEEN the reads: since CLA-663 a plane that is unwired
// or erroring at spawn is refused before the review ever runs, so only a
// mid-sequence blip reaches this arm at all.
func TestDrainPhases_ReviewSeamRefusesACheckpointItCannotReread(t *testing.T) {
	outcome := "implemented: pushed the branch\n"
	var reviews []plane.ReviewRecordState
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)},
		{res: resumedCheckpointed(5, 0.50)}, // records the durable record, healthy in every other way
		{res: okResult(5, 0.05)},            // must NEVER run
	}}
	// The plane blips between the review's own session and the seam's read:
	// the baseline read (before the attempt ladder) still succeeds, the
	// review session records its record, and only the reread fails.
	blip := false
	h.onInvoke = func(i int) {
		if i == 1 {
			outcome += "\n" + config.ReviewRecordHeading + "\n- reviewed tip: " + testCheckpointTip + "\n- verdict: pass\n"
			reviews = append(reviews, recordFor(1, "review-1", "r-1", testCheckpointTip, "sealed"))
			blip = true
		}
	}
	rel := &wiredReleaser{
		state: func() plane.TaskState {
			return plane.TaskState{Status: "in_progress", ClaimedByRun: "r-1", Outcome: outcome, Reviews: reviews}
		},
		err: func() error {
			if blip {
				return errors.New("plane blip: read timeout")
			}
			return nil
		},
	}
	d := phaseDriverWithReleaser(t, h, []config.Phase{
		{Name: config.ImplementPhaseName},
		{Name: config.ReviewPhaseName},
		{Name: config.FixPhaseName},
	}, rel)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 2 {
		t.Fatalf("spawned %d sessions, want 2 — a checkpoint whose outcome cannot be reread cannot prove the record the fix phase would apply", h.invokeCalls)
	}
}

// The CLA-637 seam gate's record matcher (B1): the durable record must be THIS
// run's, under this reviewer role, and name the revision the driver verified.
// Each subtest is one way a record can FAIL to satisfy the gate — the shapes
// the doneWhen names (a request_changes re-trip, a takeover) plus the tip
// mismatches, a voided round, and the unverified checkpoint's non-empty-tip
// requirement. The happy cases pin the newest-match rule and the branchless
// form's identity-only check.
func TestMatchingReviewRecord(t *testing.T) {
	rec := func(round int, role, run, tip, state string) plane.ReviewRecordState {
		return plane.ReviewRecordState{Round: round, ReviewerRole: role, RunID: run, ReviewedTip: tip, State: state}
	}
	tests := []struct {
		name     string
		records  []plane.ReviewRecordState
		role     string
		run      string
		tip      string
		needsTip bool
		want     int // the round of the expected match; 0 means no match
	}{
		{
			name:    "this run's record with the verified tip",
			records: []plane.ReviewRecordState{rec(1, "review-1", "r-1", "sha1", "sealed")},
			role:    "review-1", run: "r-1", tip: "sha1",
			want: 1,
		},
		{
			name:    "a request_changes re-trip's previous record is a different run",
			records: []plane.ReviewRecordState{rec(1, "review-1", "r-old", "sha1", "open")},
			role:    "review-1", run: "r-1", tip: "sha1",
			want: 0,
		},
		{
			name:    "a takeover leaves the previous record visible but not matching",
			records: []plane.ReviewRecordState{rec(1, "review-1", "r-old", "sha1", "void")},
			role:    "review-1", run: "r-1", tip: "sha1",
			want: 0,
		},
		{
			name:    "this run's record under another reviewer role does not satisfy the gate",
			records: []plane.ReviewRecordState{rec(1, "review-2", "r-1", "sha1", "open")},
			role:    "review-1", run: "r-1", tip: "sha1",
			want: 0,
		},
		{
			name:    "a record whose reviewed tip is not the verified tip is stale",
			records: []plane.ReviewRecordState{rec(1, "review-1", "r-1", "sha-old", "open")},
			role:    "review-1", run: "r-1", tip: "sha1",
			want: 0,
		},
		{
			name:    "a void record never matches",
			records: []plane.ReviewRecordState{rec(1, "review-1", "r-1", "sha1", "void")},
			role:    "review-1", run: "r-1", tip: "sha1",
			want: 0,
		},
		{
			name: "the newest matching record wins",
			records: []plane.ReviewRecordState{
				rec(1, "review-1", "r-1", "sha1", "open"),
				rec(2, "review-1", "r-1", "sha1", "sealed"),
			},
			role: "review-1", run: "r-1", tip: "sha1",
			want: 2,
		},
		{
			name:    "an unverified checkpoint demands a named tip",
			records: []plane.ReviewRecordState{rec(1, "review-1", "r-1", "", "sealed")},
			role:    "review-1", run: "r-1", needsTip: true,
			want: 0,
		},
		{
			name:    "an unverified checkpoint accepts the tip the reviewer established",
			records: []plane.ReviewRecordState{rec(1, "review-1", "r-1", "sha1", "sealed")},
			role:    "review-1", run: "r-1", needsTip: true,
			want: 1,
		},
		{
			name:    "a branchless checkpoint checks the record's identity only",
			records: []plane.ReviewRecordState{rec(1, "review-1", "r-1", "", "sealed")},
			role:    "review-1", run: "r-1",
			want: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := matchingReviewRecord(tc.records, tc.role, tc.run, tc.tip, tc.needsTip)
			if tc.want == 0 {
				if ok {
					t.Fatalf("matched round %d, want no match", got.Round)
				}
				return
			}
			if !ok || got.Round != tc.want {
				t.Fatalf("matched (round %d, ok=%v), want round %d", got.Round, ok, tc.want)
			}
		})
	}
}

// reviewerRoleFor (CLA-637): `review-N`, counting the review-shaped phases up
// to and including this one — so the shipped single review phase is review-1,
// and a second review phase (the panel shape, CLA-639) writes review-2, a
// distinct role and therefore a distinct record in the same round. A phase
// that is not review-shaped has no role at all.
func TestReviewerRoleFor(t *testing.T) {
	shipped := (&config.Config{Phases: []config.Phase{
		{Name: config.ImplementPhaseName}, {Name: config.ReviewPhaseName}, {Name: config.FixPhaseName},
	}}).EffectivePhases()
	if got := reviewerRoleFor(shipped, 1); got != "review-1" {
		t.Errorf("the shipped review phase's role = %q, want review-1", got)
	}
	if got := reviewerRoleFor(shipped, 0); got != "" {
		t.Errorf("the implement phase's role = %q, want none", got)
	}
	if got := reviewerRoleFor(shipped, 2); got != "" {
		t.Errorf("the fix phase's role = %q, want none", got)
	}

	panel := (&config.Config{Phases: []config.Phase{
		{Name: config.ImplementPhaseName}, {Name: config.ReviewPhaseName}, {Name: config.ReviewPhaseName}, {Name: config.FixPhaseName},
	}}).EffectivePhases()
	if got := reviewerRoleFor(panel, 1); got != "review-1" {
		t.Errorf("the first panel member's role = %q, want review-1", got)
	}
	if got := reviewerRoleFor(panel, 2); got != "review-2" {
		t.Errorf("the second panel member's role = %q, want review-2", got)
	}
}

// CLA-637: the round is closed by the DRIVER, and a close that fails refuses
// the checkpoint. The findings stay sealed when the close does not land, so a
// fix phase spawned here would be told to apply findings it cannot read — the
// refusal keeps it from running at all, and the task is re-drained.
func TestDrainPhases_ReviewSeamRefusesWhenTheRoundCannotClose(t *testing.T) {
	rel := &reviewReleaser{
		status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n",
		closeErr: errors.New("plane refused: no_open_round"),
	}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)},
		{res: checkpointed(5, 0.50)}, // records a healthy durable record
		{res: okResult(5, 0.05)},     // must NEVER run
	}}
	h.onInvoke = func(i int) {
		if i == 1 {
			rel.outcome += "\n" + config.ReviewRecordHeading + "\n- reviewed tip: " + testCheckpointTip + "\n- verdict: pass\n"
			rel.records = append(rel.records, recordFor(1, "review-1", "r-1", testCheckpointTip, "sealed"))
		}
	}
	d := phaseDriverWithReleaser(t, h, []config.Phase{
		{Name: config.ImplementPhaseName},
		{Name: config.ReviewPhaseName},
		{Name: config.FixPhaseName},
	}, rel)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 2 {
		t.Fatalf("spawned %d sessions, want 2 — a fix phase must not spawn against a round the driver could not release", h.invokeCalls)
	}
	if len(rel.closes) != 0 {
		t.Fatalf("closes = %v, want none — the injected close error must not be recorded as a close", rel.closes)
	}
}

// CLA-637: a review session that records its durable record and THEN emits a
// handoff block leaves the round sealed on purpose — its successor was going to
// close it at its own seam. When the handoff guards REFUSE that successor (the
// chain cap, max-iterations spent), the stage is over after all, and the round
// must be closed before the checkpoint advances: a fix phase spawned against a
// sealed round can neither read the findings (get_task withholds them) nor
// answer them (finding_sealed) — it would be spawned to apply nothing.
func TestDrainPhases_RefusedReviewHandoffClosesTheRoundBeforeAdvancing(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	// The review session both checkpointed (branch verified, record written) AND
	// handed off — the shape that defers the close to the successor.
	midReview := checkpointed(5, 0.50)
	midReview.FinalMessage = "The record is written; the storage layer is next.\n" + config.HandoffMarker + "\nContinue the review."
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)},
		{res: midReview},         // review: records, then hands off (refused below)
		{res: okResult(5, 0.05)}, // fix: must spawn only after the round is released
	}}
	h.onInvoke = func(i int) {
		if i == 1 {
			rel.outcome += "\n" + config.ReviewRecordHeading + "\n- reviewed tip: " + testCheckpointTip + "\n- verdict: pass\n"
			rel.records = append(rel.records, recordFor(1, "review-1", "r-1", testCheckpointTip, "sealed"))
		}
		if i == 2 { // the fix session: the round must ALREADY be closed
			if len(rel.closes) != 1 {
				t.Errorf("the fix phase spawned with the review round not yet closed (closes=%v) — it would be unable to read or answer the findings it was told to apply", rel.closes)
			}
		}
	}
	d := phaseDriverWithReleaser(t, h, []config.Phase{
		{Name: config.ImplementPhaseName},
		{Name: config.ReviewPhaseName},
		{Name: config.FixPhaseName},
	}, rel)
	d.cfg.MaxIterations = 1 // the respawn would be iteration 2: refused

	if _, _, handoffs, stop, err := drainPhasesHandoffs(t, d, 1); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	} else if handoffs != 0 {
		t.Errorf("handoffs = %d, want 0 — the refused handoff never spawned a successor", handoffs)
	}
	if h.invokeCalls != 3 {
		t.Fatalf("spawned %d sessions, want 3 — implement, review, fix; the refused handoff advances on the recorded review", h.invokeCalls)
	}
	// The driver closed exactly the round the review record landed in, BEFORE the
	// fix phase spawned (asserted from the fix session's onInvoke above).
	if len(rel.closes) != 1 || rel.closes[0] != 1 {
		t.Fatalf("closed rounds = %v, want exactly [1] — the fix phase depends on the round being released", rel.closes)
	}
}

// CLA-576 + CLA-637: when the driver could not verify the origin tip, the
// review brief has the reviewer establish the revision itself, and the seam
// then demands the durable record NAME a tip — the fix phase compares it with
// the worktree, so an unnamed tip is an unverifiable hand-off. A record naming
// the tip the reviewer established advances; one naming none is refused.
func TestDrainPhases_UnverifiedReviewSeamDemandsANamedTip(t *testing.T) {
	phases := []config.Phase{
		{Name: config.ImplementPhaseName},
		{Name: config.ReviewPhaseName},
		{Name: config.FixPhaseName},
	}
	tests := []struct {
		name    string
		tip     string // what the review session records as reviewedTip
		wantFix bool
	}{
		{name: "the reviewer's established tip advances", tip: "self-found-tip", wantFix: true},
		{name: "an unnamed tip is refused", tip: "", wantFix: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
			h := &fakeAdapter{steps: []invokeStep{
				{res: checkpointed(10, 0.10)},
				{res: resumedCheckpointed(5, 0.50)},
				{res: okResult(5, 0.05)},
			}}
			h.onInvoke = func(i int) {
				if i == 1 {
					rel.records = append(rel.records, recordFor(1, "review-1", "r-1", tc.tip, "sealed"))
				}
			}
			d := phaseDriverWithReleaser(t, h, phases, rel)
			// Every branch check answers Unknown: the origin cannot be read, so
			// the implement checkpoint is unverified (CLA-576) and the reviewer
			// must establish the tip itself.
			d.newVerifier = func(string, bool) deliveryVerifier { return branchStatusVerifier{} }

			if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
				t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
			}
			want := 2
			if tc.wantFix {
				want = 3
			}
			if h.invokeCalls != want {
				t.Fatalf("spawned %d sessions, want %d", h.invokeCalls, want)
			}
			p2 := h.invocations[1].Prompt
			if !strings.Contains(p2, "could NOT verify") {
				t.Errorf("the review phase did not get the unverified brief:\n%s", p2)
			}
			if strings.Contains(p2, "{{") {
				t.Errorf("the unverified review brief carries an unresolved placeholder:\n%s", p2)
			}
			if tc.wantFix && (len(rel.closes) != 1 || rel.closes[0] != 1) {
				t.Fatalf("closed rounds = %v, want exactly [1]", rel.closes)
			}
		})
	}
}

// CLA-635/B5: the driver's structural read of the review record. Whatever the
// reviewer wrote, the fix phase is listed the GRADE the driver computed: a MUST
// with no evidence kind (the parenthesised token right after the severity) or
// no evidence payload is a SHOULD; grounded MUSTs, SHOULDs and NITs keep their
// grade. The digest is empty when there is nothing parseable to list, and it
// reads the NEWEST block only, because that is the review this round is
// applying.
func TestReviewFindingsDigest(t *testing.T) {
	const h = config.ReviewRecordHeading
	round := func(findings ...string) string {
		return "implemented: pushed\n\n" + h + "\n- reviewed tip: sha1\n- reviewed by: harness claude, model opus-test\n- verdict: findings\n### Findings\n" +
			strings.Join(findings, "\n") + "\n"
	}
	tests := []struct {
		name     string
		outcome  string
		want     []string // substrings the digest must carry
		wantNone []string // substrings it must not
	}{
		{
			name:    "a grounded MUST keeps its grade",
			outcome: round("- R1 [must] (gate): `go test ./...` fails at loop.go:42 - the guard is unpinned. Required fix: pin it."),
			want:    []string{"R1 [MUST]", "gate"},
		},
		{
			name:     "a MUST with no evidence kind is listed as a SHOULD",
			outcome:  round("- R1 [must] internal/loop/loop.go:7 - no evidence here. Required fix: add some."),
			want:     []string{"R1 [SHOULD]", "downgraded"},
			wantNone: []string{"R1 [MUST]"},
		},
		{
			name:     "a MUST whose kind carries no payload is listed as a SHOULD",
			outcome:  round("- R1 [must] (gate):"),
			want:     []string{"R1 [SHOULD]", "downgraded"},
			wantNone: []string{"R1 [MUST]"},
		},
		{
			name:     "a kind with only whitespace after the colon is no payload",
			outcome:  round("- R1 [must] (gate):   "),
			want:     []string{"R1 [SHOULD]", "downgraded"},
			wantNone: []string{"R1 [MUST]"},
		},
		{
			name:     "a kind without the prescribed colon is no kind at all",
			outcome:  round("- R1 [must] (gate) go test fails - internal/loop/loop.go:7 - the prefix is missing. Required fix: use `(gate):`."),
			want:     []string{"R1 [SHOULD]"},
			wantNone: []string{"R1 [MUST]"},
		},
		{
			name:    "the severity token is case-insensitive and gets canonicalised",
			outcome: round("- R1 [MUST] (doneWhen): \"PR into staging\" - x.go:1 - not met. Required fix: open it."),
			want:    []string{"R1 [MUST]", "doneWhen"},
		},
		{
			name:     "a downgraded uppercase MUST is still downgraded",
			outcome:  round("- R1 [MUST] x.go:1 - no evidence. Required fix: add it."),
			want:     []string{"R1 [SHOULD]", "downgraded"},
			wantNone: []string{"R1 [MUST]"},
		},
		{
			name:    "SHOULDs and NITs keep their grade and carry no downgrade note",
			outcome: round("- R2 [should] x.go:2 - tidy. Required fix: tidy it.", "- R3 [nit] x.go:3 - wording. Required fix: none."),
			want:    []string{"R2 [SHOULD]", "R3 [NIT]"},
			// The raw finding text may itself say "downgraded"; the note only
			// ever rides a demoted MUST, and neither line is one.
			wantNone: []string{"[downgraded:"},
		},
		{
			name:     "a malformed severity line is skipped, not guessed",
			outcome:  round("- R1 [blocker] x.go:1 - not a grade. Required fix: none."),
			wantNone: []string{"R1"},
		},
		{
			name:     "the verdict-pass placeholder is not a finding",
			outcome:  "done\n\n" + h + "\n- verdict: pass\n### Findings\n- none\n",
			wantNone: []string{"The driver read"},
		},
		{
			name:     "no block, no digest",
			outcome:  "implemented: pushed\n",
			wantNone: []string{"The driver read"},
		},
		{
			name: "only the newest block is graded",
			outcome: round("- R9 [must] old.go:1 - a previous round's finding. Required fix: ignore me.") +
				"\n\nmore work\n\n" + h + "\n- reviewed tip: sha2\n- verdict: findings\n### Findings\n- R1 [should] new.go:2 - this round. Required fix: tidy.",
			want:     []string{"R1 [SHOULD]"},
			wantNone: []string{"R9"},
		},
		{
			name:     "a later level-2 heading ends the block",
			outcome:  round("- R1 [should] x.go:1 - tidy. Required fix: tidy.") + "\n## Fixes\n- R9 [must] x.go:9 - not in the review block. Required fix: ignore.",
			want:     []string{"R1 [SHOULD]"},
			wantNone: []string{"R9"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			digest := reviewFindingsDigest(tc.outcome)
			for _, want := range tc.want {
				if !strings.Contains(digest, want) {
					t.Errorf("the digest does not carry %q:\n%s", want, digest)
				}
			}
			for _, banned := range tc.wantNone {
				if strings.Contains(digest, banned) {
					t.Errorf("the digest carries %q, which it must not:\n%s", banned, digest)
				}
			}
		})
	}
}

// The unwired plane, pinned explicitly. fakeReleaser is a Releaser and nothing
// more, so every test on it exercises the branch-evidence path the driver had
// before the plane's task read existed. That path must stay intact for the
// ordinary two-phase flow, and the review-only gate must fail CLOSED — not
// open — when a target has no task-state reader to ask.
func TestDrainPhases_UnwiredPlaneJudgesOnBranchEvidence(t *testing.T) {
	t.Run("a non-final checkpoint still advances on the verified branch", func(t *testing.T) {
		h := &fakeAdapter{steps: []invokeStep{
			{res: checkpointed(10, 0.10)},
			{res: okResult(5, 0.05)},
		}}
		d, rel := phaseDriver(t, h, twoPhases()) // final review: no record requirement
		if _, wired := plane.Releaser(rel).(plane.TaskStateSource); wired {
			t.Fatal("fakeReleaser implements TaskStateSource, so this test no longer exercises the unwired plane")
		}
		if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
			t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
		}
		if h.invokeCalls != 2 {
			t.Fatalf("spawned %d sessions, want 2 — a verified branch is the whole checkpoint on an unwired plane", h.invokeCalls)
		}
	})

	t.Run("the review-only phase refuses at spawn rather than advancing on the branch alone", func(t *testing.T) {
		h := &fakeAdapter{steps: []invokeStep{
			{res: checkpointed(10, 0.10)},
			{res: resumedCheckpointed(5, 0.50)}, // must NEVER run: no reader, so no baseline
			{res: okResult(5, 0.05)},            // must NEVER run
		}}
		d, _ := phaseDriver(t, h, []config.Phase{
			{Name: config.ImplementPhaseName},
			{Name: config.ReviewPhaseName},
			{Name: config.FixPhaseName},
		})
		if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
			t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
		}
		if h.invokeCalls != 1 {
			t.Fatalf("spawned %d sessions, want 1 — with no task-state reader the review's freshness baseline can never be read, so it must be refused before a session is spent on it (CLA-663)", h.invokeCalls)
		}
	})
}

// The variant arms of the review/fix selection are prompt-invisible to the
// content-pinning tests: only a driver call shows which brief a branchless or
// unverified checkpoint actually spawns with. Each arm is asserted by a phrase
// unique to its brief.
func TestInvocationForSelectsTheReviewAndFixVariants(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{{res: okResult(1, 0)}}}
	d, _ := phaseDriver(t, h, []config.Phase{
		{Name: config.ImplementPhaseName},
		{Name: config.ReviewPhaseName},
		{Name: config.FixPhaseName},
	})
	phases := d.cfg.EffectivePhases()

	// A verified branch: the plain review-only brief, and the branch-shaped fix.
	// The checkpoint's verified tip is what the brief must hand the reviewer as
	// the revision to review and record (CLA-637).
	verified := &harness.Result{
		Claim:         harness.Claim{TaskID: "t-1", RunID: "r-1", Branch: "clanker/x"},
		CheckpointTip: testCheckpointTip,
	}
	inv, err := d.invocationFor(d.targets[0], 1, phases[1], verified, "", "staging", phases)
	if err != nil {
		t.Fatalf("invocationFor review: %v", err)
	}
	if !strings.Contains(inv.Prompt, "REVIEW-ONLY gate") || strings.Contains(inv.Prompt, "could NOT verify") {
		t.Errorf("a verified checkpoint did not get the plain review-only brief:\n%s", inv.Prompt)
	}
	if !strings.Contains(inv.Prompt, `"review-1"`) || !strings.Contains(inv.Prompt, testCheckpointTip) {
		t.Errorf("the review brief does not carry the durable record's role and verified tip:\n%s", inv.Prompt)
	}
	if strings.Contains(inv.Prompt, "{{") {
		t.Errorf("the review brief carries an unresolved placeholder:\n%s", inv.Prompt)
	}

	// Branchless (the plane's record): the no-code review-only brief, and the
	// no-code fix brief (which must NOT tell the session to open a PR).
	branchless := &harness.Result{Claim: harness.Claim{TaskID: "t-1", RunID: "r-1"}}
	inv, err = d.invocationFor(d.targets[0], 1, phases[1], branchless, "", "staging", phases)
	if err != nil {
		t.Fatalf("invocationFor branchless review: %v", err)
	}
	if !strings.Contains(inv.Prompt, "PLANE'S RECORD") || strings.Contains(inv.Prompt, "verified to exist on the origin") {
		t.Errorf("a branchless checkpoint did not get the no-code review-only brief:\n%s", inv.Prompt)
	}
	inv, err = d.invocationFor(d.targets[0], 2, phases[2], branchless, "", "staging", phases)
	if err != nil {
		t.Fatalf("invocationFor branchless fix: %v", err)
	}
	if !strings.Contains(inv.Prompt, "there is NO branch") || strings.Contains(inv.Prompt, "Open a PR") {
		t.Errorf("a branchless checkpoint did not get the no-code fix brief:\n%s", inv.Prompt)
	}
	if !strings.Contains(inv.Prompt, "LAST `"+config.ReviewRecordHeading+"`") {
		t.Errorf("the no-code fix prompt does not point at the LAST %q block:\n%s", config.ReviewRecordHeading, inv.Prompt)
	}
	inv, err = d.invocationFor(d.targets[0], 2, phases[2], verified, "", "staging", phases)
	if err != nil {
		t.Fatalf("invocationFor fix: %v", err)
	}
	if !strings.Contains(inv.Prompt, "LAST `"+config.ReviewRecordHeading+"`") {
		t.Errorf("the branch-shaped fix prompt does not point at the LAST %q block:\n%s", config.ReviewRecordHeading, inv.Prompt)
	}

	// Unverified (the origin could not be read): the review verifies the hand-off
	// itself; the fix keeps the branch-shaped brief (the review already verified).
	unverified := &harness.Result{
		Claim:                harness.Claim{TaskID: "t-1", RunID: "r-1", Branch: "clanker/x"},
		CheckpointUnverified: true,
	}
	inv, err = d.invocationFor(d.targets[0], 1, phases[1], unverified, "", "staging", phases)
	if err != nil {
		t.Fatalf("invocationFor unverified review: %v", err)
	}
	if !strings.Contains(inv.Prompt, "could NOT verify") || !strings.Contains(inv.Prompt, "FIRST verify the hand-off yourself") {
		t.Errorf("an unverified checkpoint did not get the unverified review-only brief:\n%s", inv.Prompt)
	}
	if strings.Contains(inv.Prompt, "verified to exist on the origin remote") {
		t.Errorf("the unverified review-only brief asserts the verification it cannot support:\n%s", inv.Prompt)
	}
	// The unverified variant has no driver-known tip to name: it must not carry
	// {{reviewedTip}} at all, filled or literal — the reviewer establishes the
	// revision itself (CLA-637).
	if strings.Contains(inv.Prompt, config.ReviewedTipPlaceholder) || strings.Contains(inv.Prompt, "{{") {
		t.Errorf("the unverified review brief carries an unresolved placeholder:\n%s", inv.Prompt)
	}
	if !strings.Contains(inv.Prompt, `"review-1"`) {
		t.Errorf("the unverified review brief does not carry the reviewer role the seam keys on:\n%s", inv.Prompt)
	}
}

// The review-only phase's deliverable is the plane record, not the tree. The
// driver's salvage commits and pushes whatever a session leaves uncommitted, so
// running it for a review phase would (a) commit the unattributable change the
// review's own record forbids and (b) push a tip past the reviewed one, making
// the fix phase park a healthy task on a mismatch the DRIVER created.
func TestSalvage_SkipsTheReviewOnlyPhase(t *testing.T) {
	outcome := "implemented: pushed the branch\n"
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(1, 0)}, // implement: salvage runs
		{res: checkpointed(1, 0)}, // review-only: must NOT salvage
		{res: okResult(1, 0)},     // fix (no claim on the fixture, so nothing to salvage)
	}}
	h.onInvoke = func(i int) {
		if i == 1 { // the review session records the block the seam requires
			outcome += "\n" + config.ReviewRecordHeading + "\n- reviewed tip: sha1\n- verdict: pass\n"
		}
	}
	cfg := fastCfg()
	cfg.Prompt = ""
	cfg.Phases = []config.Phase{
		{Name: config.ImplementPhaseName},
		{Name: config.ReviewPhaseName},
		{Name: config.FixPhaseName},
	}
	rel := &wiredReleaser{state: func() plane.TaskState {
		return plane.TaskState{Status: "in_progress", ClaimedByRun: "r-1", Outcome: outcome}
	}}
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: rel}})
	openTestStateDir(t, d)
	d.cfg.WorkDir = t.TempDir()
	d.newVerifier = func(string, bool) deliveryVerifier { return passVerifier() }
	d.probePRBase = func(context.Context, string, string) (bool, string, error) { return true, "", nil }
	s := &fakeSalvager{out: savedOutcome()}
	d.newSalvager = func(string) workSalvager { return s }

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if len(s.calls) != 1 {
		t.Fatalf("salvaged %d times, want exactly 1 (the implement phase): %+v", len(s.calls), s.calls)
	}
}

// ── CLA-638: fix-verification rounds and the stopping rule ──────────────────

// mustFinding is one plane-served finding, shapes for the verification fixtures.
func mustFinding(id, status, summary string) plane.ReviewFindingState {
	return plane.ReviewFindingState{ID: id, Severity: "must", Status: status, Summary: summary}
}

// verifyPhases is the shipped four-phase shape the CLA-638 tests drive.
func verifyPhases() []config.Phase {
	return []config.Phase{
		{Name: config.ImplementPhaseName, Tier: "cheap"},
		{Name: config.ReviewPhaseName, Tier: "strong"},
		{Name: config.FixPhaseName, Tier: "cheap"},
		{Name: config.VerifyPhaseName},
	}
}

// TestDrainPhases_VerificationRoundPassesWithNoOpenMust: a verify round that
// closes with no open MUST ends the sequence; the verifier carried the terminal
// step itself, and the driver neither spawns another fix nor parks.
func TestDrainPhases_VerificationRoundPassesWithNoOpenMust(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)},
		{res: checkpointed(5, 0.50)},
		{res: checkpointed(5, 0.05)},
		{res: checkpointed(5, 0.05)},
	}}
	h.onInvoke = func(i int) {
		if i == 1 { // the review gate records round 1
			rel.records = append(rel.records, recordFor(1, "review-1", "r-1", testCheckpointTip, "sealed"))
		}
		if i == 3 { // the verify round records round 2 with a non-gating SHOULD
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 2, ReviewerRole: "review-2", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{{ID: "2.review-2.1", Severity: "should", Status: "open", Summary: "tidy this"}},
			})
			rel.counted = 2
			rel.openMusts = nil // the plane's gate lists MUSTs only
			rel.status = "in_review"
		}
	}
	d := phaseDriverWithReleaser(t, h, verifyPhases(), rel)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 4 {
		t.Fatalf("spawned %d sessions, want 4 — implement, review, fix, verify", h.invokeCalls)
	}
	if len(rel.closes) != 2 || rel.closes[0] != 1 || rel.closes[1] != 2 {
		t.Fatalf("closed rounds = %v, want [1 2] — the review and the verification round must both be released", rel.closes)
	}
	if len(rel.parks) != 0 || len(rel.questions) != 0 {
		t.Fatalf("a clean verification round parked the task: parks=%v questions=%v", rel.parks, rel.questions)
	}
}

// TestDrainPhases_VerificationOpenMustSpawnsAnotherFix: a verify round with an
// open MUST does NOT hand over — the driver spawns another fix and verifies
// again, closing each round as it goes. It also pins the brief split: the fix
// phase must not carry the terminal step when a verifier follows, and the
// verifier must be handed the prior open MUST ids.
func TestDrainPhases_VerificationOpenMustSpawnsAnotherFix(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)},
		{res: checkpointed(5, 0.50)},
		{res: checkpointed(5, 0.05)},
		{res: checkpointed(5, 0.05)},
		{res: checkpointed(5, 0.05)},
		{res: checkpointed(5, 0.05)},
	}}
	h.onInvoke = func(i int) {
		switch i {
		case 1: // the review raises the MUST the verifier will adjudicate
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-1", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{mustFinding("1.review-1.1", "open", "the seam is untested")},
			})
			rel.openMusts = []string{"1.review-1.1"}
		case 3: // verify round 2 leaves the MUST open
			rel.records = append(rel.records, recordFor(2, "review-2", "r-1", testCheckpointTip, "sealed"))
			rel.counted = 2
			rel.openMusts = []string{"1.review-1.1"}
		case 5: // verify round 3 resolves it
			rel.records = append(rel.records, recordFor(3, "review-2", "r-1", testCheckpointTip, "sealed"))
			rel.counted = 3
			rel.openMusts = nil
		}
	}
	d := phaseDriverWithReleaser(t, h, verifyPhases(), rel)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 6 {
		t.Fatalf("spawned %d sessions, want 6 — implement, review, fix, verify, fix, verify", h.invokeCalls)
	}
	if len(rel.parks) != 0 {
		t.Fatalf("the loop parked before the cap: %v", rel.parks)
	}
	if len(rel.closes) != 3 {
		t.Fatalf("closed rounds = %v, want three closes (1, 2, 3)", rel.closes)
	}
	// The fix prompt is the no-PR variant while a verifier follows.
	fix1, fix2 := h.invocations[2].Prompt, h.invocations[4].Prompt
	for i, p := range []string{fix1, fix2} {
		if strings.Contains(p, "Open a PR targeting") {
			t.Errorf("fix session %d carries the PR terminal step even though a verification round follows:\n%s", i+1, p)
		}
		if !strings.Contains(p, "Do NOT open a PR") {
			t.Errorf("fix session %d does not tell the fixer the verifier owns the hand-over:\n%s", i+1, p)
		}
	}
	// The verifier is handed the prior open MUST ids and the gate contract.
	v1, v2 := h.invocations[3].Prompt, h.invocations[5].Prompt
	if !strings.Contains(v1, "1.review-1.1") {
		t.Errorf("the verification brief does not list the prior open MUST id:\n%s", v1)
	}
	for i, p := range []string{v1, v2} {
		for _, want := range []string{
			"ADJUDICATE EVERY PRIOR OPEN MUST BY ITS PLANE ID",
			"fix-delta",
			"NO OPEN MUST",
			"status: \"in_review\"",
		} {
			if !strings.Contains(p, want) {
				t.Errorf("the verification brief %d does not carry %q:\n%s", i+1, want, p)
			}
		}
	}
}

// TestDrainPhases_VerificationCapParksWithTheRecords: at the round cap (the
// plane counts non-void rounds), an open MUST parks the task with a question
// carrying the records — never one more round. The cap is read from the plane,
// so a takeover that starts with the counter already at 3 parks immediately.
func TestDrainPhases_VerificationCapParksWithTheRecords(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)},
		{res: checkpointed(5, 0.50)},
		{res: checkpointed(5, 0.05)},
		{res: checkpointed(5, 0.05)},
		{res: checkpointed(5, 0.05)}, // a fifth session would be the forbidden "one more round"
	}}
	h.onInvoke = func(i int) {
		if i == 1 { // the review raises the MUST
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-1", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{mustFinding("1.review-1.1", "open", "the seam is untested")},
			})
			rel.openMusts = []string{"1.review-1.1"}
		}
		if i == 3 { // round 3 raises a FRESH MUST and the cap is spent
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 3, ReviewerRole: "review-2", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{mustFinding("3.review-2.1", "open", "a fresh delta-only MUST")},
			})
			rel.counted = 3
			rel.openMusts = []string{"3.review-2.1"}
		}
	}
	d := phaseDriverWithReleaser(t, h, verifyPhases(), rel)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 4 {
		t.Fatalf("spawned %d sessions after the cap, want 4 — never one more round", h.invokeCalls)
	}
	if len(rel.parks) != 1 || len(rel.questions) != 1 {
		t.Fatalf("cap did not park with a question: parks=%v questions=%v", rel.parks, rel.questions)
	}
	if !strings.Contains(rel.parks[0], "3.review-2.1") || !strings.Contains(rel.parks[0], "3-round cap") {
		t.Errorf("the park outcome does not carry the records:\n%s", rel.parks[0])
	}
	if !strings.Contains(rel.questions[0], "3.review-2.1") || !strings.Contains(rel.questions[0], "reviews.records") {
		t.Errorf("the question does not carry the records:\n%s", rel.questions[0])
	}
	if !strings.Contains(rel.questions[0], "a fresh delta-only MUST") {
		t.Errorf("the question names the id but not the finding's substance:\n%s", rel.questions[0])
	}
}

// TestDrainPhases_VerificationNonConvergenceParks: a MUST left open after two
// fix attempts (its id's round prefix is two or more behind the closing round)
// parks the task even when the plane's count is below the cap — the void-round
// case where the cap alone would not stop the loop.
func TestDrainPhases_VerificationNonConvergenceParks(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)},
		{res: checkpointed(5, 0.50)},
		{res: checkpointed(5, 0.05)},
		{res: checkpointed(5, 0.05)},
		{res: checkpointed(5, 0.05)}, // a fifth session would be the forbidden "one more round"
	}}
	h.onInvoke = func(i int) {
		if i == 1 { // the review raises the MUST
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-1", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{mustFinding("1.review-1.1", "open", "the seam is untested")},
			})
			rel.openMusts = []string{"1.review-1.1"}
		}
		if i == 3 { // round 3 (a round 2 was voided): the round-1 MUST is still open
			rel.records = append(rel.records, recordFor(3, "review-2", "r-1", testCheckpointTip, "sealed"))
			rel.counted = 2
			rel.openMusts = []string{"1.review-1.1"}
		}
	}
	d := phaseDriverWithReleaser(t, h, verifyPhases(), rel)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 4 {
		t.Fatalf("spawned %d sessions, want 4 — the two-attempt breaker must stop the loop", h.invokeCalls)
	}
	if len(rel.parks) != 1 || !strings.Contains(rel.parks[0], "after two fix attempts") {
		t.Fatalf("the non-convergence breaker did not park: parks=%v", rel.parks)
	}
}

// TestDrainPhases_VerificationReopenedFindingParks: an id a previous verifier
// marked resolved that the next verifier reopens parks the task (A6's second
// arm). It needs two open findings to keep the loop alive between the resolve
// and the reopen, which is exactly the shape the breaker exists for.
func TestDrainPhases_VerificationReopenedFindingParks(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)},
		{res: checkpointed(5, 0.50)},
		{res: checkpointed(5, 0.05)},
		{res: checkpointed(5, 0.05)},
		{res: checkpointed(5, 0.05)},
		{res: checkpointed(5, 0.05)},
		{res: checkpointed(5, 0.05)}, // would be round 4: forbidden
	}}
	h.onInvoke = func(i int) {
		switch i {
		case 1: // the review raises A and B
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-1", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{mustFinding("1.review-1.1", "open", "A"), mustFinding("1.review-1.2", "open", "B")},
			})
		case 3: // verify round 2 marks A fixed, leaves B open
			rel.records = append(rel.records, recordFor(2, "review-2", "r-1", testCheckpointTip, "sealed"))
			rel.records[0].Findings[0].Status = "fixed"
			rel.counted = 2
			rel.openMusts = []string{"1.review-1.2"}
		case 5: // verify round 3 marks B fixed but REOPENS A
			rel.records = append(rel.records, recordFor(3, "review-2", "r-1", testCheckpointTip, "sealed"))
			rel.records[0].Findings[0].Status = "open"
			rel.records[0].Findings[1].Status = "fixed"
			rel.counted = 3
			rel.openMusts = []string{"1.review-1.1"}
		}
	}
	d := phaseDriverWithReleaser(t, h, verifyPhases(), rel)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 6 {
		t.Fatalf("spawned %d sessions, want 6 — the loop runs to the round that reopens the finding", h.invokeCalls)
	}
	if len(rel.parks) != 1 || !strings.Contains(rel.parks[0], "open again") {
		t.Fatalf("a reopened finding did not park with the non-convergence reason: parks=%v", rel.parks)
	}
}

// TestDrainPhases_VerificationWithoutARecordEndsWithoutHandover: a verify
// session that wrote no durable record cannot be judged. The driver refuses the
// sequence fail-closed (no hand-over, no PR accepted from the round) and leaves
// the claim to the takeover path — the same disposition CLA-637 gives a refused
// review checkpoint. It does not PARK: a recordless round is a failed session,
// not the reviewer/fixer disagreement the stopping rule parks for.
func TestDrainPhases_VerificationWithoutARecordEndsWithoutHandover(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)},
		{res: checkpointed(5, 0.50)},
		{res: checkpointed(5, 0.05)},
		{res: checkpointed(5, 0.05)}, // the verifier records nothing
	}}
	h.onInvoke = func(i int) {
		if i == 1 {
			rel.records = append(rel.records, recordFor(1, "review-1", "r-1", testCheckpointTip, "sealed"))
		}
	}
	d := phaseDriverWithReleaser(t, h, verifyPhases(), rel)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 4 {
		t.Fatalf("spawned %d sessions, want 4", h.invokeCalls)
	}
	if len(rel.parks) != 0 || len(rel.questions) != 0 {
		t.Fatalf("a recordless verification round parked instead of ending the sequence fail-closed: parks=%v questions=%v", rel.parks, rel.questions)
	}
}

// TestNonConvergenceReason pins the two arms directly, including the void-round
// case the driver's round-prefix arithmetic exists for.
func TestNonConvergenceReason(t *testing.T) {
	resolved := map[string]bool{"1.review-1.1": true}
	tests := []struct {
		name     string
		open     []string
		round    int
		want     string
		resolved map[string]bool
	}{
		{"a fresh id is not non-convergence", []string{"2.review-2.1"}, 2, "", resolved},
		{"reopened after resolved", []string{"1.review-1.1"}, 3, "open again", resolved},
		{"unresolved after two attempts", []string{"1.review-1.2"}, 3, "after two fix attempts", map[string]bool{}},
		{"one attempt is not two", []string{"2.review-1.1"}, 3, "", map[string]bool{}},
		{"unparseable id never fires the round arm", []string{"legacy-id"}, 3, "", map[string]bool{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := nonConvergenceReason(tc.open, tc.round, tc.resolved)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("nonConvergenceReason = %q, want empty", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("nonConvergenceReason = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

// TestFindingIDRound pins the namespace parse (B13): the driver reads the round
// back off the plane-assigned `<round>.<role>.<n>` rather than tracking one.
func TestFindingIDRound(t *testing.T) {
	if r, ok := findingIDRound("2.review-1.3"); !ok || r != 2 {
		t.Errorf("findingIDRound(2.review-1.3) = %d,%v", r, ok)
	}
	if _, ok := findingIDRound("legacy"); ok {
		t.Error("a prefix-less id parsed as a round")
	}
}

// TestVerificationDigest: the driver's brief lists exactly the OPEN MUSTs on
// released records, with their plane ids, and names the delta base. Sealed and
// void records contribute nothing; the empty-open case still names the base so
// the round knows what delta to verify.
func TestVerificationDigest(t *testing.T) {
	records := []plane.ReviewRecordState{
		{Round: 1, ReviewerRole: "review-1", ReviewedTip: "aaaa", State: "open", Findings: []plane.ReviewFindingState{
			mustFinding("1.review-1.1", "open", "the seam is untested"),
			mustFinding("1.review-1.2", "fixed", "already fixed"),
		}},
		{Round: 2, ReviewerRole: "review-2", ReviewedTip: "bbbb", State: "sealed", Findings: []plane.ReviewFindingState{
			mustFinding("2.review-2.1", "open", "sealed, must not leak"),
		}},
	}
	got := verificationDigest(records, []string{"1.review-1.1"})
	if !strings.Contains(got, "1.review-1.1") || !strings.Contains(got, "the seam is untested") {
		t.Errorf("the digest omits the open MUST:\n%s", got)
	}
	if strings.Contains(got, "1.review-1.2") || strings.Contains(got, "2.review-2.1") {
		t.Errorf("the digest lists a resolved or sealed finding:\n%s", got)
	}
	if !strings.Contains(got, "aaaa") {
		t.Errorf("the digest does not name the newest RELEASED tip as the delta base:\n%s", got)
	}
	if strings.Contains(got, "bbbb") {
		t.Errorf("the digest names a sealed record's tip as the delta base (a sealed round's findings are withheld):\n%s", got)
	}
	clean := verificationDigest([]plane.ReviewRecordState{
		{Round: 1, ReviewerRole: "review-1", ReviewedTip: "aaaa", State: "open"},
	}, nil)
	if !strings.Contains(clean, "NO OPEN MUST") || !strings.Contains(clean, "aaaa") {
		t.Errorf("a clean digest must still name the delta base and the clear gate:\n%s", clean)
	}
}

// TestDrainPhases_VerificationLocalRoundBound: if the plane serves no round
// count at all (an older plane, a degraded read), the driver's own count of
// closed verification rounds still terminates the fix→verify pair at the cap.
// The loop must never be unbounded because a read came back zero.
func TestDrainPhases_VerificationLocalRoundBound(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	steps := []invokeStep{
		{res: checkpointed(10, 0.10)}, // implement
		{res: checkpointed(5, 0.50)},  // review
	}
	for i := 0; i < 3; i++ {
		steps = append(steps, invokeStep{res: checkpointed(5, 0.05)}) // fix
		steps = append(steps, invokeStep{res: checkpointed(5, 0.05)}) // verify
	}
	steps = append(steps, invokeStep{res: checkpointed(5, 0.05)}) // would be a fourth fix
	h := &fakeAdapter{steps: steps}
	h.onInvoke = func(i int) {
		switch i {
		case 1:
			rel.records = append(rel.records, recordFor(1, "review-1", "r-1", testCheckpointTip, "sealed"))
		case 3, 5, 7:
			round := 2 + (i-3)/2
			rel.records = append(rel.records, recordFor(round, "review-2", "r-1", testCheckpointTip, "sealed"))
			// A NEW id each round: no id is unresolved twice, so only the cap
			// can stop the loop.
			rel.openMusts = []string{fmt.Sprintf("%d.review-2.1", round)}
			rel.counted = 0 // the degraded plane read
		}
	}
	d := phaseDriverWithReleaser(t, h, verifyPhases(), rel)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	// implement + review + three fix/verify pairs = 8; a 9th would be the
	// forbidden "one more round".
	if h.invokeCalls != 8 {
		t.Fatalf("spawned %d sessions, want 8 — the local round bound must stop the pair", h.invokeCalls)
	}
	if len(rel.parks) != 1 || !strings.Contains(rel.parks[0], "cap") {
		t.Fatalf("the local round bound did not park at the cap: parks=%v", rel.parks)
	}
}
