package loop

import (
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// The no-output watchdog's kill is a DRIVER kill, like the repeated-tool-call
// kill: the adapter records it on Result.IdleKilled and the driver must treat
// the end as dead - no delivery, never a checkpoint - so the phase goes down the
// dead-phase ladder exactly as a ToolLoop result does.

// idleKilledResult is what an adapter hands back after a silence kill: the
// cause, the kill's exit, and the spend it saw before it.
func idleKilledResult() harness.Result {
	return harness.Result{ExitCode: -1, IdleKilled: "no output for 15m0s", Tokens: 900, CostUSD: 0.09, UsageReported: true}
}

// toolCapKilledResult is the same, killed by the TOOL CAP while a call was open:
// the cause names the call and IdleToolCall marks the kill as landing on it.
func toolCapKilledResult() harness.Result {
	return harness.Result{
		ExitCode:     -1,
		IdleKilled:   "tool call step_finish(tool-calls) open for 30m0s",
		IdleToolCall: true,
		Tokens:       900, CostUSD: 0.09, UsageReported: true,
	}
}

// A tool-cap kill is DEAD, but it is not evidence the provider broke: it means
// the driver's own per-call bound fired while the session was waiting inside one
// legitimate call (a CI wait, a long test run). It must not tip the fleet
// breaker, which pauses a project on the theory that the provider or harness is
// broken. The per-task ladder still bounds a task that keeps doing it (CLA-900).
func TestDrainPhases_ToolCapKillDoesNotCountTowardTheFleetBreaker(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: held(toolCapKilledResult(), openClaim())},
		{res: held(toolCapKilledResult(), openClaim())},
		{res: held(toolCapKilledResult(), openClaim())},
		{res: held(toolCapKilledResult(), openClaim())},
	}}
	d, rel := deadReleaserDriver(t, h)
	d.fleetDead[0] = fleetDeadBound - 1 // would trip if one more death counted

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	// Four tool-cap kills on one task: three retries, then the per-task park.
	if h.invokeCalls != 4 {
		t.Fatalf("spawned %d sessions, want 4 (three retries, then the fourth kill parks the task)", h.invokeCalls)
	}
	if got := d.fleetDead[0]; got != fleetDeadBound-1 {
		t.Errorf("fleet counter = %d, want %d — a tool-cap kill is not provider failure evidence and must not increment it", got, fleetDeadBound-1)
	}
	if d.fleetPaused[0] {
		t.Error("the fleet pause was set — a task's tool-cap kills must not pause the project as a provider fault")
	}
	if len(rel.questions) != 1 {
		t.Fatalf("filed %d questions, want 1 — the per-task park's clarification, not a fleet question: %+v", len(rel.questions), rel.questions)
	}
	if rel.questions[0].taskID == "" {
		t.Error("the filed question has no taskId — this was a per-task park, not a project-level fleet trip")
	}
}

// The contrast: a kill with NO call open is the idle clock finding a session
// that stopped answering, which IS the provider-failure evidence the fleet
// breaker exists for — so it still counts and still trips (CLA-900).
func TestDrainPhases_IdleKillStillCountsTowardTheFleetBreaker(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: held(idleKilledResult(), openClaim())},
	}}
	d, rel := deadReleaserDriver(t, h)
	d.fleetDead[0] = fleetDeadBound - 1

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if !d.fleetPaused[0] {
		t.Error("fleet pause not set — an idle-clock kill is still provider-failure evidence and must trip at the bound")
	}
	if len(rel.questions) != 1 {
		t.Fatalf("filed %d fleet questions, want 1: %+v", len(rel.questions), rel.questions)
	}
}

func TestDrainPhases_IdleKilledPhaseIsDeadNotACheckpoint(t *testing.T) {
	logs := captureLogs(t)
	h := &fakeAdapter{steps: []invokeStep{
		{res: held(idleKilledResult(), openClaim())},
		{res: checkpointed(1, 0)}, // the dead-phase retry: phase 1 again
		{res: okResult(1, 0)},     // and only now the review phase
	}}
	d, _ := phaseDriver(t, h, twoPhases())

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v - the kill did its job; it is not a run failure", err, stop)
	}
	if h.invokeCalls != 3 {
		t.Fatalf("spawned %d sessions, want 3 (killed phase, its dead-phase retry, review)", h.invokeCalls)
	}
	if !strings.Contains(h.invocations[1].Prompt, "PHASE 1") {
		t.Errorf("the session after the kill was not phase 1 again - a killed phase advanced to review: %q", h.invocations[1].Prompt)
	}
	// The cause is named as a driver kill, so the classification came from the
	// silence kill rather than from whatever else the exit happened to match.
	if out := logs.String(); !strings.Contains(out, "killed by the driver: idle: no output for 15m0s") {
		t.Errorf("the kill was not logged as an idle kill:\n%s", out)
	}
}

// The resolved bounds reach the adapter on the invocation, per phase harness:
// opencode gets the longer idle bound and its own per-call cap, claude a shorter
// idle bound plus a per-call cap, with default config.
func TestInvocationForArmsTheSessionIdleWatchdog(t *testing.T) {
	phases := []config.Phase{
		{Name: config.ImplementPhaseName},
		{Name: config.ReviewPhaseName, Harness: "opencode"},
	}
	d, _ := phaseDriver(t, &fakeAdapter{}, phases)

	// claude: 10m idle, 30m per call. opencode: 15m idle, 30m per call.
	for _, tc := range []struct {
		idx      int
		idle     time.Duration
		toolCall time.Duration
	}{
		{0, 10 * time.Minute, 30 * time.Minute},
		{1, 15 * time.Minute, 30 * time.Minute},
	} {
		inv, err := d.invocationFor(d.targets[0], tc.idx, phases[tc.idx], nil, "", "", phases, nil, "")
		if err != nil {
			t.Fatalf("phase %d: invocationFor: %v", tc.idx, err)
		}
		if inv.MaxSessionIdle != tc.idle {
			t.Errorf("phase %d: MaxSessionIdle = %v, want %v", tc.idx, inv.MaxSessionIdle, tc.idle)
		}
		if inv.MaxToolCallDuration != tc.toolCall {
			t.Errorf("phase %d: MaxToolCallDuration = %v, want %v", tc.idx, inv.MaxToolCallDuration, tc.toolCall)
		}
	}
}
