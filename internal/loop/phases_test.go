package loop

import (
	"context"

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
// other drain helpers use, so a control-flow regression fails fast.
func drainPhasesOnce(t *testing.T, d *Driver) (int, float64, bool, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return d.drainPhases(ctx, 1, d.targets[0], spend{start: time.Now()})
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
func TestDrainPhases_BudgetAtTheSeamStopsBeforePhaseTwoAndHandsTheTaskBack(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(500, 0)},
		{res: okResult(1, 0)},
	}}
	d, rel := phaseDriver(t, h, twoPhases())
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
	// The handback skipped at the seam is owed here, or the task sits leased to a
	// sequence that will never continue.
	if len(rel.calls) != 1 {
		t.Errorf("stopped at the seam without handing the task back, got %d releases: %+v", len(rel.calls), rel.calls)
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
	if got := h.invocations[1].MaxTurns; got != 0 {
		t.Errorf("phase 2 MaxTurns = %d, want 0 (uncapped)", got)
	}
}
