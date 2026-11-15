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
// the cause names the open window and IdleToolCall marks the kill as landing on
// it. The cause is the opencode shape - a step whose tool never returned (see
// idleStepCall) - because that is the death the adapter could not see before
// CLA-900.
func toolCapKilledResult() harness.Result {
	return harness.Result{
		ExitCode:     -1,
		IdleKilled:   "tool call step_start open for 30m0s",
		IdleToolCall: true,
		Tokens:       900, CostUSD: 0.09, UsageReported: true,
	}
}

// A tool-cap kill is still a death, so it still counts toward the fleet bound -
// a fleet-wide stuck call must not spin sessions forever. But the question the
// trip raises must say what was OBSERVED: the driver's own tool cap ended the
// last session while it waited inside one open call, which is a stuck call or a
// wait longer than the cap, not a provider that went silent (CLA-900).
func TestDrainPhases_ToolCapKillTripNamesWhatWasObserved(t *testing.T) {
	logs := captureLogs(t)
	h := &fakeAdapter{steps: []invokeStep{
		{res: held(toolCapKilledResult(), openClaim())},
	}}
	d, rel := deadReleaserDriver(t, h)
	d.fleetDead[0] = fleetDeadBound - 1 // the next death trips it

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if !d.fleetPaused[0] {
		t.Error("fleet pause not set — a run of deaths must still pause the project, whatever killed them")
	}
	if len(rel.questions) != 1 {
		t.Fatalf("filed %d questions, want 1: %+v", len(rel.questions), rel.questions)
	}
	q := rel.questions[0]
	if !strings.Contains(q.body, "killed waiting inside one open tool call") {
		t.Errorf("the fleet question does not name what was observed:\n%s", q.body)
	}
	if !strings.Contains(q.body, "stayed open (tool call step_start open for 30m0s)") {
		t.Errorf("the fleet question does not name the cap that fired:\n%s", q.body)
	}
	if strings.Contains(q.body, "provider or harness looks broken") {
		t.Errorf("the fleet question blames the provider for a tool-cap kill:\n%s", q.body)
	}
	if strings.Contains(q.body, "already known to be failing") {
		t.Errorf("the fleet question calls the provider failing for a tool-cap kill:\n%s", q.body)
	}
	// The daemon log line carries the same observation: a regression that drops
	// the tool-cap wording there leaves the operator grep-hunting (CLA-900).
	if out := logs.String(); !strings.Contains(out, "killed waiting inside one open tool call") {
		t.Errorf("the fleet trip's log line does not name what was observed:\n%s", out)
	}
}

// The adapter's verdict, not the cause string, is what classifies the death: an
// idle-clock cause that arrived with a call still open is still a session that
// was waiting inside one call, so the question names the observation and never
// blames the provider (CLA-900, finding 1.2).
func TestDrainPhases_IdleKillInsideAnOpenCallNamesTheWaitingCall(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: held(harness.Result{
			ExitCode:     -1,
			IdleKilled:   "no output for 15m0s",
			IdleToolCall: true,
			Tokens:       900, CostUSD: 0.09, UsageReported: true,
		}, openClaim())},
	}}
	d, rel := deadReleaserDriver(t, h)
	d.fleetDead[0] = fleetDeadBound - 1

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if !d.fleetPaused[0] {
		t.Error("fleet pause not set - a run of deaths must still pause the project, whatever killed them")
	}
	if len(rel.questions) != 1 {
		t.Fatalf("filed %d questions, want 1: %+v", len(rel.questions), rel.questions)
	}
	body := rel.questions[0].body
	if !strings.Contains(body, "killed waiting inside one open tool call") {
		t.Errorf("the fleet question does not name the open call:\n%s", body)
	}
	if !strings.Contains(body, "no output for 15m0s") {
		t.Errorf("the fleet question does not carry the cause the adapter reported:\n%s", body)
	}
	if strings.Contains(body, "provider or harness looks broken") || strings.Contains(body, "already known to be failing") {
		t.Errorf("an open-call death was filed as a provider failure:\n%s", body)
	}
}

// The contrast: a kill with NO call open is the idle clock finding a session
// that stopped answering, so the fleet question keeps the provider-broke wording
// and counts it. That is the death AFTER step_finish - the model's next request
// never answered - and it must NOT be filed as a stuck tool call (CLA-900).
func TestDrainPhases_IdleKillStillReadsAsAProviderFailure(t *testing.T) {
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
	body := rel.questions[0].body
	if !strings.Contains(body, "provider or harness looks broken") {
		t.Errorf("the idle-clock fleet question lost its provider-broke wording:\n%s", body)
	}
	if strings.Contains(body, "killed waiting inside one open tool call") || strings.Contains(body, "tool cap") {
		t.Errorf("a post-step provider silence was filed as a tool-cap kill — no call was open:\n%s", body)
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
