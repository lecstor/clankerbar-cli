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
// opencode gets the longer idle bound and no per-call cap, claude a shorter idle
// bound plus a per-call cap, with default config.
func TestInvocationForArmsTheSessionIdleWatchdog(t *testing.T) {
	phases := []config.Phase{
		{Name: config.ImplementPhaseName},
		{Name: config.ReviewPhaseName, Harness: "opencode"},
	}
	d, _ := phaseDriver(t, &fakeAdapter{}, phases)

	// claude: 10m idle, 30m per call. opencode: 15m idle, no per-call cap.
	for _, tc := range []struct {
		idx      int
		idle     time.Duration
		toolCall time.Duration
	}{
		{0, 10 * time.Minute, 30 * time.Minute},
		{1, 15 * time.Minute, 0},
	} {
		inv, err := d.invocationFor(d.targets[0], tc.idx, phases[tc.idx], nil, "", "", phases, nil)
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
