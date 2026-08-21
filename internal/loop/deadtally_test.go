package loop

// deadtally_test.go — the live dead-phase tally (CLA-402).
//
// The tally is the driver's own measurement of the dead-phase rate: every
// phase session that got past its claim is a run, and one that then died
// producing nothing is a dead. These tests pin the three properties the
// doneWhen names: the tally increments only on a dead phase, it does NOT
// increment for a session that recorded a branch before dying, and it keeps
// its per-phase and per-harness breakdown. The operator's exclusion — a
// session that never got past its claim increments neither counter — is the
// fourth, because the refused-claim session is exactly the false positive the
// measurement exists to separate (2026-08-20 06:32, decision f518a454).

import (
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/harness"
)

func TestDeadTally_CountsADeadPhasePerPhaseAndHarness(t *testing.T) {
	// The shipped [implement, review] sequence on mixed harnesses (the review
	// phase on the run's claude harness, implement on opencode): the implement
	// session dies producing nothing, its one retry reaches the checkpoint, the
	// review session completes.
	impl := &fakeAdapter{steps: []invokeStep{
		{res: held(deadResult(), openClaim())},
		{res: checkpointed(1, 0)},
	}}
	// The review phase runs RESUMED: it holds the claim the implement phase
	// handed over (the seed the real adapter would read), so its Result carries
	// the claim like a real resumed session's does.
	review := &fakeAdapter{steps: []invokeStep{{res: held(okResult(1, 0), openClaim())}}}
	d := mixedDriver(t, impl, review)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}

	if len(d.deadTally) != 2 {
		t.Fatalf("tally has %d cells, want 2 (implement/opencode and review/claude): %+v", len(d.deadTally), d.deadTally)
	}
	implCell, ok := d.deadTally[tallyKey{phase: "implement", harness: "opencode"}]
	if !ok {
		t.Fatalf("no implement/opencode cell in %+v", d.deadTally)
	}
	if implCell.run != 2 || implCell.dead != 1 {
		t.Errorf("implement/opencode cell = %+v, want run=2 dead=1 — the dead phase and its retry are two sessions, one dead", implCell)
	}
	reviewCell, ok := d.deadTally[tallyKey{phase: "review", harness: "claude"}]
	if !ok {
		t.Fatalf("no review/claude cell in %+v", d.deadTally)
	}
	if reviewCell.run != 1 || reviewCell.dead != 0 {
		t.Errorf("review/claude cell = %+v, want run=1 dead=0 — a completed phase is a run, never a dead", reviewCell)
	}
}

// The tally is reported with its denominator, always — "6 dead" without the
// "of 23" is not a measurement, it is a headline.
func TestDeadTally_ReportCarriesTheDenominator(t *testing.T) {
	logs := captureLogs(t)
	h := &fakeAdapter{steps: []invokeStep{
		{res: held(deadResult(), openClaim())},
		{res: checkpointed(1, 0)},
	}}
	d, _ := phaseDriver(t, h, twoPhases())
	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}

	d.logDeadTally()
	out := logs.String()
	if !strings.Contains(out, "1 dead of 2") {
		t.Errorf("tally report does not carry the denominator: %q", out)
	}
	if !strings.Contains(out, "implement/claude") {
		t.Errorf("tally report does not name the phase/harness cell: %q", out)
	}
}

// The second conjunct of deadPhase is load-bearing in the tally too: a session
// that recorded a branch and THEN died on an unknown reason produced something,
// so it is a run but not a dead.
func TestDeadTally_DoesNotCountABranchBeforeDying(t *testing.T) {
	wip := harness.Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true}
	h := &fakeAdapter{steps: []invokeStep{
		{res: held(deadResult(), wip)},
		{res: okResult(1, 0)},
	}}
	d, _ := phaseDriver(t, h, twoPhases())

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}

	cell := d.deadTally[tallyKey{phase: "implement", harness: "claude"}]
	if cell == nil {
		t.Fatalf("no implement/claude cell in %+v", d.deadTally)
	}
	if cell.run != 1 || cell.dead != 0 {
		t.Errorf("cell = %+v, want run=1 dead=0 — the branch made the phase survivable, so it must not count as dead", cell)
	}
}

// A session that never got past its claim — the refused takeover shape — must
// not appear in the tally AT ALL: it increments neither counter, because a
// correctly-refused session satisfies both conjuncts of deadPhase() while
// being a correct refusal, not a death.
func TestDeadTally_RefusedClaimIncrementsNeitherCounter(t *testing.T) {
	// deadResult() with a ZERO claim: the session observed no task — exactly
	// what a claim refused with lease_live leaves on the Result. With nothing
	// held, the phase ends without a checkpoint and the drain stops after it
	// (there is nothing for a review phase to resume) — so the whole tally
	// stays empty: the refused session was not a run, and nothing else ran.
	h := &fakeAdapter{steps: []invokeStep{{res: deadResult()}}}
	d, _ := phaseDriver(t, h, twoPhases())

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}

	if len(d.deadTally) != 0 {
		t.Errorf("tally = %+v, want empty — a session that never got past its claim increments NEITHER counter, and with no held claim the sequence stops before any other phase runs", d.deadTally)
	}
}

// The tally counts SESSIONS, so a dead phase that is retried books two runs
// and one dead — the same view the retrospective scan has of the two logs.
func TestDeadTally_RetriedDeadPhaseCountsBothSessions(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: held(deadResult(), openClaim())}, // first attempt dies
		{res: checkpointed(1, 0)},              // retry reaches the checkpoint
		{res: okResult(1, 0)},                  // review completes
	}}
	d, _ := phaseDriver(t, h, twoPhases())

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}

	cell := d.deadTally[tallyKey{phase: "implement", harness: "claude"}]
	if cell == nil || cell.run != 2 || cell.dead != 1 {
		t.Errorf("implement/claude cell = %+v, want run=2 dead=1 (dead attempt + its healthy retry) — the tally counts sessions, matching the scan's two logs", cell)
	}
}
