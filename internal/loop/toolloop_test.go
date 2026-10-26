package loop

import (
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// CLA-739: a session the adapter killed for repeating a failing tool call is
// DEAD - no delivery - with a cause that names the tool, the count and the
// error. The harness tests pin the kill; these pin what the driver does with it.

const loopCause = "repeated failing tool call: clankerbar_update_task x5: invalid_release: `driverReleased` marks a run the DRIVER ended on a session's behalf - pass it only with..."

// toolLoopResult is what an adapter hands back after the kill: the cause, the
// kill's exit, and the spend it saw before it. exit is a parameter because the
// session can exit 0 on its own in the instant before the kill lands, and the
// cause must win either way.
func toolLoopResult(exit int) harness.Result {
	return harness.Result{ExitCode: exit, ToolLoop: loopCause, Tokens: 900, CostUSD: 0.09, UsageReported: true}
}

// A panel member killed this way is recorded as a FAILED member through the
// existing path, named by its cause: the panel runs on, the fix is told the
// member is INCOMPLETE and why, the SESSION's fleet entry carries the cause, the
// dead tally books it exactly once, and the daemon log says what happened. The
// iteration Outcome itself stays one of the four words the plane accepts
// (CLA-738) — the cause rides the session, never the outcome.
func TestDrainPhases_ToolLoopKilledPanelMemberIsAFailedMember(t *testing.T) {
	logs := captureLogs(t)
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)},                // implement
		{res: checkpointed(5, 0.50)},                 // review-1 records
		{res: held(toolLoopResult(-1), openClaim())}, // review-2 loops and is killed
		{res: checkpointed(5, 0.50)},                 // review-3 records
		{res: checkpointed(5, 0.05)},                 // fix
	}}
	h.onInvoke = func(i int) {
		switch i {
		case 1:
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-1", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{mustFinding("1.review-1.1", "open", "the seam is untested")},
			})
		case 3:
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-3", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{mustFinding("1.review-3.1", "open", "the guard is bypassed")},
			})
		case 4:
			rel.status = "in_review"
			rel.outcome += "\n### Fixes\n- 1.review-1.1 fixed\n- 1.review-3.1 fixed\n"
		}
	}
	d, ff := panelDriverWithFleet(t, h, rel, panelMembers(), panelPhases())

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v - a killed member must not fail the run", err, stop)
	}
	if h.invokeCalls != 5 {
		t.Fatalf("spawned %d sessions, want 5 - implement, three members, fix; the killed member is skipped past, never re-run", h.invokeCalls)
	}
	fix := h.invocations[4].Prompt
	for _, want := range []string{"INCOMPLETE", "review-2", "repeated failing tool call: clankerbar_update_task x5: invalid_release"} {
		if !strings.Contains(fix, want) {
			t.Errorf("the fix brief does not carry %q:\n%s", want, fix)
		}
	}
	recs := ff.records()
	if len(recs) != 1 {
		t.Fatalf("iteration records = %d, want 1", len(recs))
	}
	// The record keeps the four-word enum (CLA-738); the killed member's cause
	// rides the SESSION entry below and the daemon log, never the outcome.
	requireFleetOutcome(t, recs[0].Outcome, fleet.OutcomeReleased)
	if got := len(recs[0].Sessions); got != 5 {
		t.Fatalf("sessions on the record = %d, want 5", got)
	}
	if got := recs[0].Sessions[2].Cause; got != loopCause {
		t.Errorf("review-2's fleet session cause = %q, want %q", got, loopCause)
	}
	for i, s := range recs[0].Sessions {
		if i != 2 && s.Cause != "" {
			t.Errorf("session %d (%s) carries cause %q; only the killed member should", i, s.Phase, s.Cause)
		}
	}
	cell := d.deadTally[tallyKey{phase: "review-2", harness: "opencode"}]
	if cell == nil || cell.dead != 1 {
		t.Errorf("dead tally for review-2/opencode = %+v, want exactly 1 dead - booked once, not again by the panel handler", cell)
	}
	out := logs.String()
	for _, want := range []string{"killed by the driver: " + loopCause, "classified dead, no delivery", "recorded nothing (" + loopCause + ")"} {
		if !strings.Contains(out, want) {
			t.Errorf("daemon log does not carry %q:\n%s", want, out)
		}
	}
}

// The same verdict when the members run concurrently (the shape the incident
// actually ran in): the killed member is a failed member and the fix still runs
// on the others' findings.
func TestDrainPhases_ParallelPanelToolLoopKilledMemberIsAFailedMember(t *testing.T) {
	repo, branch, tip := panelParallelRepo(t)
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented\n"}
	members := panelMembers()
	a := newPanelParallelAdapter(rel, branch, tip, len(members))
	a.failLabel = "review-2"
	a.failToolLoop = loopCause
	a.wantRecords = len(members) - 1
	d, ff := panelParallelDriver(t, a, rel, members, repo, tip, 0)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	a.mu.Lock()
	for _, e := range a.errs {
		t.Error(e)
	}
	fixCalls, fix := a.fixCalls, a.fixPrompt
	a.mu.Unlock()
	if fixCalls != 1 {
		t.Fatalf("fix sessions = %d, want 1", fixCalls)
	}
	if !strings.Contains(fix, "INCOMPLETE") || !strings.Contains(fix, "repeated failing tool call: clankerbar_update_task x5") {
		t.Errorf("the fix brief does not name the killed member by its cause:\n%s", fix)
	}
	// The killed member's cause rides its SESSION entry, never the iteration
	// outcome, which stays one of the four the plane accepts (CLA-738).
	rec := requireOneFleetRecord(t, ff)
	found := false
	for _, s := range rec.Sessions {
		if s.Cause == loopCause {
			found = true
		}
	}
	if !found {
		t.Errorf("no session on the iteration record carries the kill cause %q: %+v", loopCause, rec.Sessions)
	}
}

// An implement phase killed this way is dead, not a checkpoint - even when the
// process exited 0 in the instant before the kill landed. It goes down the
// dead-phase ladder (retried, never advanced to review on a false premise), and
// it is neither retried as a transient failure nor a run-ending failure.
func TestDrainPhases_ToolLoopKilledPhaseIsDeadNotACheckpoint(t *testing.T) {
	for _, exit := range []int{-1, 0} {
		h := &fakeAdapter{steps: []invokeStep{
			{res: held(toolLoopResult(exit), openClaim())},
			{res: checkpointed(1, 0)}, // the dead-phase retry: phase 1 again
			{res: okResult(1, 0)},     // and only now the review phase
		}}
		d, _ := phaseDriver(t, h, twoPhases())

		if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
			t.Fatalf("exit %d: drainPhases: err=%v stop=%v - the kill did its job; it is not a run failure", exit, err, stop)
		}
		if h.invokeCalls != 3 {
			t.Fatalf("exit %d: spawned %d sessions, want 3 (killed phase, its dead-phase retry, review)", exit, h.invokeCalls)
		}
		if !strings.Contains(h.invocations[1].Prompt, "PHASE 1") {
			t.Errorf("exit %d: the session after the kill was not phase 1 again - a killed phase advanced to review: %q", exit, h.invocations[1].Prompt)
		}
	}
}

// The resolved bound reaches every working session's Invocation.
func TestInvocationCarriesTheRepeatedToolFailureBound(t *testing.T) {
	for _, tc := range []struct {
		set  *int
		want int
	}{{nil, 5}, {intPtr(0), 0}, {intPtr(3), 3}} {
		h := &fakeAdapter{steps: []invokeStep{{res: okResult(1, 0)}}}
		d, _ := phaseDriver(t, h, []config.Phase{{Name: config.ImplementPhaseName}})
		d.cfg.MaxRepeatedToolFailures = tc.set
		if _, _, _, err := drainPhasesOnce(t, d); err != nil {
			t.Fatalf("drainPhases: %v", err)
		}
		if got := h.invocations[0].MaxRepeatedToolFailures; got != tc.want {
			t.Errorf("Invocation.MaxRepeatedToolFailures = %d, want %d", got, tc.want)
		}
	}
}

func intPtr(n int) *int { return &n }

// Four tool-loop kills on one task park it, and the park names the cause rather
// than the dead signature's "final step reason ... no branch recorded", which a
// tool-loop kill need not match (review finding, CLA-739).
func TestDrainPhases_FourToolLoopKillsParkWithTheCause(t *testing.T) {
	logs := captureLogs(t)
	h := &fakeAdapter{steps: []invokeStep{
		{res: held(toolLoopResult(-1), openClaim())},
		{res: held(toolLoopResult(-1), openClaim())},
		{res: held(toolLoopResult(-1), openClaim())},
		{res: held(toolLoopResult(-1), openClaim())},
	}}
	rel := &parkingReleaser{}
	cfg := fastCfg()
	cfg.Phases = twoPhases()
	cfg.Prompt = ""
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: rel}})
	openTestStateDir(t, d)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if len(rel.parks) != 1 || len(rel.questions) != 1 {
		t.Fatalf("parks=%d questions=%d, want 1 and 1", len(rel.parks), len(rel.questions))
	}
	for name, text := range map[string]string{"outcome": rel.parks[0].outcome, "question": rel.questions[0].body} {
		if !strings.Contains(text, loopCause) {
			t.Errorf("the park %s does not name the cause: %q", name, text)
		}
		if strings.Contains(text, "no branch recorded") {
			t.Errorf("the park %s claims no branch was recorded, which a tool-loop kill does not establish: %q", name, text)
		}
	}
	if out := logs.String(); !strings.Contains(out, "was killed ("+loopCause+"), dead for the 4th consecutive time") {
		t.Errorf("the park log line does not name the cause:\n%s", out)
	}
	// The fake reports no cost, as a killed claude stream does: the log must
	// say so rather than print $0.
	if out := logs.String(); !strings.Contains(out, "cost not captured") {
		t.Errorf("the kill line printed a cost nobody measured:\n%s", out)
	}
}
