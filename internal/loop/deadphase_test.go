package loop

// CLA-396: the dead-phase retry budget split into TWO counters with distinct
// scopes. The per-task counter (raised from two to four by the 2026-08-20
// operator decision) parks a task that can kill four sessions; the fleet
// counter counts consecutive dead phases across ALL tasks on a target and, on
// tripping, PAUSES the loop for that target and raises one project-level
// question — because a run of deaths across different tasks means the provider
// or harness is broken, not the tasks. These tests pin the split: the budget
// boundaries, the reset rules (per-task resets on that task's success; fleet
// resets ONLY on a successful implement phase), the pause-and-raise-once
// behaviour, the in-flight task being released rather than parked, and the
// claim-refused session incrementing neither counter.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// claimFor is openClaim with a scripted task/run, for the tests that need a
// different task to die in a later drain.
func claimFor(taskID, runID string) harness.Claim { return harness.Claim{TaskID: taskID, RunID: runID} }

// deadReleaserDriver builds the shape every dead-phase test drives: two phases,
// a parkingReleaser, a real state dir.
func deadReleaserDriver(t *testing.T, h harness.Adapter) (*Driver, *parkingReleaser) {
	t.Helper()
	rel := &parkingReleaser{}
	cfg := fastCfg()
	cfg.Phases = twoPhases()
	cfg.Prompt = ""
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: rel}})
	openTestStateDir(t, d)
	return d, rel
}

// The per-task budget is FOUR: three deaths earn three retries and the third
// dead phase is still retried, so a task that dies three times and then
// succeeds is never parked — the boundary "not at three". (The fleet counter
// sees the three deaths, but the implement success that follows resets it.)
func TestDrainPhases_PerTaskBudgetDoesNotParkAtThree(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: held(deadResult(), openClaim())},
		{res: held(deadResult(), openClaim())},
		{res: held(deadResult(), openClaim())},
		{res: checkpointed(1, 0)}, // the third retry succeeds
		{res: okResult(1, 0)},     // review runs normally
	}}
	d, rel := deadReleaserDriver(t, h)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v — a task that survived its retries is not a failure", err, stop)
	}
	if h.invokeCalls != 5 {
		t.Fatalf("spawned %d sessions, want 5 (three dead phases retried, then implement success + review) — the budget must not park at three", h.invokeCalls)
	}
	if len(rel.parks) != 0 {
		t.Fatalf("parked %d times, want 0 — three consecutive deaths must be retried, not parked: %+v", len(rel.parks), rel.parks)
	}
	if got := d.fleetDead[0]; got != 0 {
		t.Errorf("fleet counter = %d, want 0 — the implement success after the retries must reset it", got)
	}
}

// A residual fleet count left over from an EARLIER, unrelated task's death must
// not pre-empt a park this task has legitimately earned. The fleet counter
// persists on the Driver across drains and only resets on a successful implement
// phase, so a task dying its fourth consecutive time can find the fleet counter
// already primed by someone else's death. The per-task park must still win: a
// task that has itself earned a park is parked, not swept into a project-wide
// fleet pause that blames the provider for what is, this time, actually the task.
func TestDrainPhases_PerTaskParkTakesPriorityOverAResidualFleetCount(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: held(deadResult(), openClaim())},
		{res: held(deadResult(), openClaim())},
		{res: held(deadResult(), openClaim())},
		{res: held(deadResult(), openClaim())},
	}}
	d, rel := deadReleaserDriver(t, h)
	// A death on some other task earlier left the fleet counter at 1 — one death
	// short of tipping the fleet bound on this task's own fourth death, if the
	// fleet check were consulted before the per-task one.
	d.fleetDead[0] = 1

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 4 {
		t.Errorf("spawned %d sessions, want 4 (three retries, then the fourth dead phase parks)", h.invokeCalls)
	}
	if len(rel.parks) != 1 {
		t.Fatalf("parked %d times, want 1 — the per-task budget must win over a residual fleet count: %+v", len(rel.parks), rel.parks)
	}
	if len(rel.questions) != 1 {
		t.Fatalf("filed %d questions, want 1 — a per-task park's own clarification, not a fleet-level decision: %+v", len(rel.questions), rel.questions)
	}
	if rel.questions[0].taskID == "" {
		t.Error("the filed question has no taskId — this was a per-task park, not a project-level fleet trip")
	}
	if d.fleetPaused[0] {
		t.Error("the fleet pause was set — a residual count must not let the fleet trip pre-empt a task's own earned park")
	}
}

// The per-task counter resets on a successful phase of that task. A task that
// dies ONCE per drain and succeeds between drains is retried every time and
// never accumulates toward the park — four separate drains each carrying one
// death of t-1 must leave it un-parked, where a counter that failed to reset
// would have parked it at the fourth.
func TestDrainPhases_PerTaskCounterResetsOnTaskSuccess(t *testing.T) {
	// Four drains, each: t-1 dies once, the retry checkpoints (a successful
	// implement phase), review runs. The SAME task across all four.
	steps := []invokeStep{}
	for i := 0; i < 4; i++ {
		steps = append(steps,
			invokeStep{res: held(deadResult(), openClaim())},
			invokeStep{res: checkpointed(1, 0)},
			invokeStep{res: okResult(1, 0)},
		)
	}
	h := &fakeAdapter{steps: steps}
	d, rel := deadReleaserDriver(t, h)

	for i := 0; i < 4; i++ {
		if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
			t.Fatalf("drain %d: err=%v stop=%v", i+1, err, stop)
		}
	}
	if h.invokeCalls != 12 {
		t.Fatalf("spawned %d sessions, want 12", h.invokeCalls)
	}
	if len(rel.parks) != 0 {
		t.Fatalf("parked %d times, want 0 — four one-death drains of the same task must never accumulate to a park, because each drain's success reset the counter: %+v", len(rel.parks), rel.parks)
	}
	if got := d.fleetDead[0]; got != 0 {
		t.Errorf("fleet counter = %d, want 0 — each drain's implement success reset it", got)
	}
}

// The FLEET counter is not per-task: it climbs across DIFFERENT tasks, because
// a provider outage kills each task only once (so no per-task counter ever
// trips) and only a run of deaths spanning tasks reveals it. Two drains with
// two different tasks each dying once — each drain ending before any implement
// success (so nothing resets) — must leave the fleet counter at two, with
// neither task parked.
func TestDrainPhases_FleetCounterIncrementsAcrossDifferentTasks(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		// Drain 1: t-1 dies once; the retry ends without holding the task (a
		// no-claim result), so the drain ends and the fleet count survives.
		{res: held(deadResult(), openClaim())},
		{res: okResult(0, 0)},
		// Drain 2: a DIFFERENT task, t-2, dies once; same shape.
		{res: held(deadResult(), claimFor("t-2", "r-2"))},
		{res: okResult(0, 0)},
	}}
	d, rel := deadReleaserDriver(t, h)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drain 1: err=%v stop=%v", err, stop)
	}
	if got := d.fleetDead[0]; got != 1 {
		t.Fatalf("fleet counter after drain 1 = %d, want 1", got)
	}
	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drain 2: err=%v stop=%v", err, stop)
	}
	if got := d.fleetDead[0]; got != 2 {
		t.Errorf("fleet counter after drain 2 = %d, want 2 — it must climb across DIFFERENT tasks", got)
	}
	if len(rel.parks) != 0 {
		t.Errorf("parked %d times, want 0 — each task died only once, so neither is parked: %+v", len(rel.parks), rel.parks)
	}
}

// The fleet counter resets ONLY on a successful implement phase — never on a
// successful review phase, because review runs on the claude harness and cannot
// exhibit the zero-usage death, so a claude success vouching for the opencode
// gateway would hide the very fault the detector exists to find. And it is keyed
// to the IMPLEMENT phase by name, so any other phase's success leaves it alone.
func TestDrainPhases_FleetResetsOnlyOnImplementSuccess(t *testing.T) {
	t.Run("implement success resets", func(t *testing.T) {
		h := &fakeAdapter{steps: []invokeStep{
			{res: checkpointed(1, 0)},
			{res: okResult(1, 0)},
		}}
		d, _ := deadReleaserDriver(t, h)
		d.fleetDead[0] = fleetDeadBound - 1 // just under the trip

		if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
			t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
		}
		if got := d.fleetDead[0]; got != 0 {
			t.Errorf("fleet counter = %d, want 0 — a successful implement phase must reset it", got)
		}
	})
	t.Run("review success does not reset", func(t *testing.T) {
		// The first phase is NOT named implement, so even though it succeeds,
		// the fleet counter must not reset — pinning the name-keyed rule and,
		// with it, that a review-phase success alone never resets.
		h := &fakeAdapter{steps: []invokeStep{
			{res: checkpointed(1, 0)},
			{res: okResult(1, 0)},
		}}
		rel := &parkingReleaser{}
		cfg := fastCfg()
		cfg.Phases = []config.Phase{{Name: "build"}, {Name: "review"}}
		cfg.Prompt = ""
		d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: rel}})
		openTestStateDir(t, d)
		d.fleetDead[0] = fleetDeadBound - 1

		if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
			t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
		}
		if got := d.fleetDead[0]; got != fleetDeadBound-1 {
			t.Errorf("fleet counter = %d, want %d — a non-implement phase's success must not reset it", got, fleetDeadBound-1)
		}
	})
}

// A fleet trip PAUSES the target's loop and raises exactly ONE project-level
// question. The question has no taskId (it is not one task's triage), is
// non-blocking (there is no task to block; the pause is the enforcement), and
// is a `decision` (a fleet incident is a project-level judgment worth standing
// in the decision log). A second trip while the pause is in force raises
// nothing more — one question per episode.
func TestDrainPhases_FleetTripPausesAndRaisesOnce(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: held(deadResult(), openClaim())},            // drain 1: trip
		{res: held(deadResult(), claimFor("t-2", "r-2"))}, // drain 2: trip again
	}}
	d, rel := deadReleaserDriver(t, h)
	d.fleetDead[0] = fleetDeadBound - 1 // the next death trips it

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drain 1: err=%v stop=%v", err, stop)
	}
	if !d.fleetPaused[0] {
		t.Error("fleet pause not set on the trip — the loop must stop spawning for this target")
	}
	if !d.fleetRaised[0] {
		t.Error("fleet question not marked raised")
	}
	if len(rel.questions) != 1 {
		t.Fatalf("filed %d questions, want 1: %+v", len(rel.questions), rel.questions)
	}
	q := rel.questions[0]
	if q.taskID != "" {
		t.Errorf("fleet question taskId = %q, want empty — the question is project-level, not pinned to the in-flight task", q.taskID)
	}
	if q.blocking {
		t.Error("fleet question is blocking; there is no task to block, the loop's pause is the enforcement")
	}
	if q.kind != "decision" {
		t.Errorf("fleet question kind = %q, want decision — a fleet incident is a project-level judgment", q.kind)
	}
	if !strings.Contains(q.body, "consecutive dead phases") {
		t.Errorf("fleet question body does not say what tripped it: %q", q.body)
	}

	// A second trip while the pause is in force raises nothing more.
	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drain 2: err=%v stop=%v", err, stop)
	}
	if len(rel.questions) != 1 {
		t.Errorf("filed %d questions after a second trip, want still 1 — the pause-and-raise happens exactly once per episode", len(rel.questions))
	}
}

// A fleet trip does NOT park the in-flight task: it is a bystander. The task's
// claim was already released with the dead phase (the same handback any dead
// phase gets), so it returns to the queue like any released claim — and the
// driver must not additionally park it for a fault that is not its own.
func TestDrainPhases_FleetTripReleasesInFlightTaskNotParks(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: held(deadResult(), openClaim())},
	}}
	d, rel := deadReleaserDriver(t, h)
	d.fleetDead[0] = fleetDeadBound - 1

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if len(rel.parks) != 0 {
		t.Fatalf("parked %d times, want 0 — the in-flight task is a bystander and must not be parked for a fleet fault: %+v", len(rel.parks), rel.parks)
	}
	released := false
	for _, c := range rel.calls {
		if c.taskID == "t-1" {
			released = true
		}
	}
	if !released {
		t.Errorf("t-1 was not released back to the queue (releases: %+v)", rel.calls)
	}
}

// A session that never got past its claim counts toward NEITHER counter. The
// claim-refused takeover (lease_live) ends with reason "unknown" and no branch
// — the raw deadPhase predicate is satisfied — but it observed no claim, so
// `res.Claim.Held()` is false and the dead classification (which requires a held
// claim) excludes it. This is the discriminator: "got past its claim". CLA-402's
// retrospective scan must apply the same rule.
func TestDrainPhases_ClaimRefusedSessionCountsNeither(t *testing.T) {
	if !deadPhase(deadResult()) {
		t.Fatal("test setup: deadResult() must satisfy the raw deadPhase predicate — a claim-refused session has exactly this signature")
	}
	if held(deadResult(), openClaim()).Claim.Held() == false {
		t.Fatal("test setup: a held claim must be held")
	}
	h := &fakeAdapter{steps: []invokeStep{
		// The claim-refused session: reason "unknown", no branch, but NO claim
		// held (claim_task was refused, so no task id was ever observed).
		{res: deadResult()},
	}}
	d, rel := deadReleaserDriver(t, h)
	d.fleetDead[0] = fleetDeadBound - 1 // would trip if this death were counted

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if got := d.fleetDead[0]; got != fleetDeadBound-1 {
		t.Errorf("fleet counter = %d, want %d — a session that never got past its claim must not increment it", got, fleetDeadBound-1)
	}
	if d.fleetPaused[0] {
		t.Error("a claim-refused session paused the loop — it is not a dead phase")
	}
	if len(rel.parks) != 0 {
		t.Errorf("parked %d times, want 0 — no task was ever held to park: %+v", len(rel.parks), rel.parks)
	}
	if len(rel.questions) != 0 {
		t.Errorf("filed %d questions, want 0: %+v", len(rel.questions), rel.questions)
	}
}

// The fleet pause RESUMES when the operator answers the raised question: the
// open-question count falls back to what it was at the raise. This drives the
// whole Run loop — the trip pauses the target, a poll while the count is up
// stays paused (no new session), and the count falling to the baseline clears
// the pause and lets the next drain run, whose successful implement phase then
// resets the fleet counter.
func TestRun_FleetPauseResumesWhenTheQuestionIsAnswered(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: held(deadResult(), openClaim())}, // drain 1: trips the fleet
		{res: checkpointed(1, 0)},              // drain 2: implement succeeds
		{res: okResult(1, 0)},                  // drain 2: review succeeds
	}}
	rel := &parkingReleaser{}
	cfg := fastCfg()
	cfg.Phases = twoPhases()
	cfg.Prompt = ""
	spawnable := func(openQ int) backlog.Summary {
		return backlog.Summary{Claimable: 1, OpenQuestions: openQ}
	}
	p := &fakePoller{sums: []backlog.Summary{
		spawnable(3), // poll 1: drain 1 runs, trips, raises (the count becomes 4)
		spawnable(4), // poll 2: the raised question is open — stays paused
		spawnable(3), // poll 3: answered (fell back to the baseline) — resume
	}, sum: backlog.Summary{Claimable: 0, OpenQuestions: 3}} // after: idle
	d := NewMulti(cfg, h, []Target{{Poller: p, Releaser: rel}})
	openTestStateDir(t, d)
	d.fleetDead[0] = fleetDeadBound - 1 // the first death trips it

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if d.fleetPaused[0] {
		t.Error("fleet pause did not clear after the question was answered")
	}
	if got := d.fleetDead[0]; got != 0 {
		t.Errorf("fleet counter = %d, want 0 — the resumed drain's implement success reset it", got)
	}
	if len(rel.questions) != 1 {
		t.Errorf("filed %d questions, want 1 — one question for the one trip", len(rel.questions))
	}
}
