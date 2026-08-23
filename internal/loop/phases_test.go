package loop

import (
	"context"
	"errors"
	"log"

	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// phaseDriver builds a driver over one target carrying a releaser, a real state
// dir and the given phases — the shape every test below drives.
//
// Prompt is cleared whenever phases are set, because Validate refuses a config
// carrying both. A test that left it populated would pass while asserting
// nothing about which of the two the driver actually reads.
func phaseDriver(t *testing.T, h harness.Adapter, phases []config.Phase) (*Driver, *fakeReleaser) {
	t.Helper()
	cfg := fastCfg()
	cfg.Phases = phases
	if len(phases) > 0 {
		cfg.Prompt = ""
	}
	rel := &fakeReleaser{}
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: rel}})
	openTestStateDir(t, d)
	return d, rel
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

// checkpointed is a phase-1 result: a clean exit still holding the task, which
// is what "reached the checkpoint" looks like on the stream.
func checkpointed(tokens int, cost float64) harness.Result {
	return held(okResult(tokens, cost), openClaim())
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
			name:         "nothing pushed: handed straight back",
			claim:        openClaim(),
			wantReleases: 1,
		},
		{
			name: "work pushed (what phase 1 actually leaves): lease left to expire",
			// update_task(branch:) sets HasWIP; CLA-314 keeps such a claim
			// non-releasable so the task returns as a takeover with its branch.
			claim:        harness.Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true},
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
			claim:       harness.Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true},
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
			claim:       harness.Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true},
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

	ph := d.cfg.EffectivePhases()[0]
	inv := d.invocationFor(d.targets[0], 0, ph, nil)

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
func TestDrainPhases_AResumedPhaseDoesNotRetryATransientFailure(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(1, 0)},
		{res: held(transientResult(), openClaim())},
	}}
	d, _ := phaseDriver(t, h, twoPhases())

	_, _, stop, err := drainPhasesOnce(t, d)
	if err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	if !stop {
		t.Error("a resumed phase hit a transient failure and the run did not stop")
	}
	if h.invokeCalls != 2 {
		t.Errorf("spawned %d sessions; a resumed phase must not be retried on a lease nothing renews", h.invokeCalls)
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

// A resumed phase is working a live 30-minute lease that nothing driver-side
// heartbeats, so waiting hours would re-spawn it against a run the plane has
// swept or handed to a takeover.
func TestDrainPhases_AResumedPhaseDoesNotWaitOutAUsageLimit(t *testing.T) {
	h := &fakeAdapter{
		steps: []invokeStep{
			{res: checkpointed(1, 0)},
			{res: held(limitResult(), openClaim())},
		},
		limitResetAt: time.Now().Add(2 * time.Hour),
	}
	d, _ := phaseDriver(t, h, twoPhases())

	_, _, stop, err := drainPhasesOnce(t, d)
	if err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	if !stop {
		t.Error("a resumed phase hit a usage limit and the run did not stop")
	}
	if h.probeCalls != 0 {
		t.Errorf("supervisedWait probed %d times on a resumed phase; it must not wait out a reset on a lease it cannot renew", h.probeCalls)
	}
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
			claim:       harness.Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true},
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
	if got := d.invocationFor(d.targets[0], 0, phases[0], nil).MaxSessionWallClock; got != 20*time.Minute {
		t.Errorf("phase 1 invocation carries %s, want its own 20m", got)
	}
	if got := d.invocationFor(d.targets[0], 1, phases[1], nil).MaxSessionWallClock; got != time.Hour {
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

// The undeclared counter is only worth having if it means ONE thing, so these pin
// the cases that must NOT move it. They exist because the first cut of CLA-384
// incremented inside releaseHeldClaim, which every session exit reaches: the §5
// review of that commit demonstrated four non-failures inflating the count,
// including a drain that settled its task and still logged "the task was never
// moved on" twice. Direct unit tests on releaseHeldClaim could not have caught
// that - the discrimination lives in the callers - so these drive whole phase
// runs.
func TestUndeclared_CountsOnlyACleanFinalPhaseThatDidNotDeclare(t *testing.T) {
	wip := func() harness.Claim { return harness.Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true} }

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
	wip := harness.Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true}
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

// A session that completed its final answer ("stop") is untouched: whatever the
// seam did before this feature is what it keeps doing.
func TestDrainPhases_AStopReasonIsUntouched(t *testing.T) {
	ok := okResult(1, 0)
	ok.Raw[harness.FinishReasonKey] = "stop"
	h := &fakeAdapter{steps: []invokeStep{
		{res: held(ok, openClaim())},
		{res: okResult(1, 0)},
	}}
	d, _ := phaseDriver(t, h, twoPhases())

	_, _, stop, err := drainPhasesOnce(t, d)
	if err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 2 {
		t.Errorf("spawned %d sessions, want 2 — a reason: stop session is an ordinary checkpoint", h.invokeCalls)
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
	rel := &parkingReleaser{}
	cfg := fastCfg()
	cfg.Phases = three
	cfg.Prompt = ""
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: rel}})
	openTestStateDir(t, d)

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
