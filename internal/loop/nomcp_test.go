package loop

// nomcp_test.go — the CLA-703 backstop at the driver seam.
//
// The adapter's server preflight is the primary fix; this is the driver's
// backstop for a review-only session that still ended with no record and no
// clankerbar MCP call. The rules these tests pin:
//
//   - it is re-run ONCE with the same brief, and the retry's record counts;
//   - a second such end is INCOMPLETE with the distinct `no_mcp` cause, booked
//     apart from dead and kept OFF the member's failure streak;
//   - a session that called clankerbar tools but recorded nothing is NOT
//     retried and is still a review failure;
//   - an adapter preflight failure (ErrMCPNotConnected) is retried once as
//     infrastructure, then fails the drain loudly.

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// noMCPPanelResult is a review-only session that ended cleanly, still holding
// the task, having made no clankerbar call and written no record.
func noMCPPanelResult() harness.Result {
	return held(okResult(5, 0.50), openClaim())
}

func TestDrainPhases_ReviewPanelMemberNoMCPIsRetriedOnce(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)}, // implement
		{res: noMCPPanelResult()},     // review-1: no record, no call
		{res: checkpointed(5, 0.50)},  // review-1 retry: records
		{res: checkpointed(5, 0.50)},  // review-2: records
		{res: checkpointed(5, 0.05)},  // fix
	}}
	h.onInvoke = func(i int) {
		switch i {
		case 2:
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-1", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{mustFinding("1.review-1.1", "open", "the seam is untested")},
			})
		case 3:
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-2", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{{ID: "1.review-2.1", Severity: "should", Status: "open", Summary: "handle the empty input"}},
			})
		case 4:
			rel.status = "in_review"
			rel.outcome += "\n### Fixes\n- 1.review-1.1 fixed\n"
		}
	}
	d, ff := panelDriverWithFleet(t, h, rel, panelMembers()[:2], panelPhases())
	logs := captureLogs(t)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 5 {
		t.Fatalf("spawned %d sessions, want 5 — implement, review-1, its ONE retry, review-2, fix", h.invokeCalls)
	}
	if p := h.invocations[1].Prompt; !strings.Contains(p, `reviewerRole: "review-1"`) {
		t.Errorf("the first review session is not review-1:\n%s", p)
	}
	if p := h.invocations[2].Prompt; !strings.Contains(p, `reviewerRole: "review-1"`) {
		t.Errorf("the retry is not the same brief (review-1):\n%s", p)
	}
	if p := h.invocations[3].Prompt; !strings.Contains(p, `reviewerRole: "review-2"`) {
		t.Errorf("the session after the retry is not the next member:\n%s", p)
	}
	if !strings.Contains(h.invocations[4].Prompt, "1.review-1.1") {
		t.Errorf("the retry's record did not reach the fix brief:\n%s", h.invocations[4].Prompt)
	}
	if out := logs.String(); !strings.Contains(out, "retrying it once (no_mcp") {
		t.Errorf("the driver did not log the no_mcp retry:\n%s", out)
	}
	// The retry recorded, so this member is not an infrastructure miss and not
	// a dead phase.
	if cell := d.deadTally[tallyKey{phase: "review-1", harness: "opencode"}]; cell != nil && (cell.noMCP != 0 || cell.dead != 0) {
		t.Errorf("review-1 tally = %+v, want no no-mcp and no dead", cell)
	}
	if n := d.panelFailStreak["review-1"]; n != 0 {
		t.Errorf("review-1 streak = %d, want 0 after a successful retry", n)
	}
	recs := ff.records()
	if len(recs) != 1 || strings.Contains(recs[0].Outcome, "INCOMPLETE") {
		t.Errorf("a recovered member left the panel incomplete: %+v", recs)
	}
}

func TestDrainPhases_ReviewPanelMemberNoMCPRepeatIsIncomplete(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)}, // implement
		{res: checkpointed(5, 0.50)},  // review-1: records
		{res: noMCPPanelResult()},     // review-2: no record, no call
		{res: noMCPPanelResult()},     // review-2 retry: same again
		{res: checkpointed(5, 0.05)},  // fix (the panel still has review-1's record)
	}}
	h.onInvoke = func(i int) {
		switch i {
		case 1:
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-1", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{mustFinding("1.review-1.1", "open", "the seam is untested")},
			})
		case 4:
			rel.status = "in_review"
			rel.outcome += "\n### Fixes\n- 1.review-1.1 fixed\n"
		}
	}
	d, ff := panelDriverWithFleet(t, h, rel, panelMembers()[:2], panelPhases())
	logs := captureLogs(t)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v — a no_mcp member must not fail the run", err, stop)
	}
	if h.invokeCalls != 5 {
		t.Fatalf("spawned %d sessions, want 5 — implement, review-1, review-2, its ONE retry, fix", h.invokeCalls)
	}
	fix := h.invocations[4].Prompt
	for _, want := range []string{"INCOMPLETE", "review-2", "no_mcp"} {
		if !strings.Contains(fix, want) {
			t.Errorf("the fix brief does not carry %q:\n%s", want, fix)
		}
	}
	out := logs.String()
	if !strings.Contains(out, "no_mcp") {
		t.Errorf("the driver did not name the no_mcp cause:\n%s", out)
	}
	recs := ff.records()
	if len(recs) != 1 {
		t.Fatalf("iteration records = %d, want 1", len(recs))
	}
	if recs[0].Outcome == fleet.OutcomeDead {
		t.Errorf("a no_mcp member marked the whole drain dead: %q", recs[0].Outcome)
	}
	if !strings.Contains(recs[0].Outcome, "no_mcp") {
		t.Errorf("the iteration outcome does not name no_mcp: %q", recs[0].Outcome)
	}
	cell := d.deadTally[tallyKey{phase: "review-2", harness: "opencode"}]
	if cell == nil || cell.noMCP != 1 || cell.dead != 0 {
		t.Errorf("review-2 tally = %+v, want 1 no-mcp and 0 dead", cell)
	}
	if n := d.panelFailStreak["review-2"]; n != 0 {
		t.Errorf("review-2 streak = %d, want 0 — an infrastructure miss is not the member's fault", n)
	}
}

func TestDrainPhases_ReviewPanelMemberThatReachedMCPButRecordedNothingIsNotRetried(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)},                           // implement
		{res: mcpReached(checkpointed(5, 0.50))},                // review-1 records
		{res: mcpReached(held(okResult(5, 0.50), openClaim()))}, // review-2 reached the backlog, records nothing
		{res: checkpointed(5, 0.05)},                            // fix
	}}
	h.onInvoke = func(i int) {
		switch i {
		case 1:
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-1", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{mustFinding("1.review-1.1", "open", "the seam is untested")},
			})
		case 3:
			rel.status = "in_review"
			rel.outcome += "\n### Fixes\n- 1.review-1.1 fixed\n"
		}
	}
	d, _ := panelDriverWithFleet(t, h, rel, panelMembers()[:2], panelPhases())

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 4 {
		t.Fatalf("spawned %d sessions, want 4 — a member that reached the backlog is not retried", h.invokeCalls)
	}
	fix := h.invocations[3].Prompt
	if strings.Contains(fix, "no_mcp") {
		t.Errorf("a member that called clankerbar tools was classified no_mcp:\n%s", fix)
	}
	if !strings.Contains(fix, "review-2") {
		t.Errorf("the fix brief does not name the failed member:\n%s", fix)
	}
}

func TestDrainPhases_ReviewOnlyNoMCPIsRetriedOnce(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)}, // implement
		{res: noMCPPanelResult()},     // review: no record, no call
		{res: noMCPPanelResult()},     // review retry: same again
		{res: okResult(5, 0.05)},      // fix must NEVER run
	}}
	d, ff := panelDriverWithFleet(t, h, rel, nil, panelPhases())
	logs := captureLogs(t)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 3 {
		t.Fatalf("spawned %d sessions, want 3 — implement, review, its ONE retry", h.invokeCalls)
	}
	if out := logs.String(); !strings.Contains(out, "no_mcp") || !strings.Contains(out, "infrastructure miss") {
		t.Errorf("the lone review's no_mcp end was not named:\n%s", out)
	}
	cell := d.deadTally[tallyKey{phase: "review", harness: "claude"}]
	if cell == nil || cell.noMCP != 1 || cell.dead != 0 {
		t.Errorf("review tally = %+v, want 1 no-mcp and 0 dead (all cells: %+v)", cell, d.deadTally)
	}
	recs := ff.records()
	if len(recs) != 1 || !strings.Contains(recs[0].Outcome, "no_mcp") {
		t.Errorf("the iteration outcome does not carry no_mcp: %+v", recs)
	}
	if recs[0].Outcome == fleet.OutcomeDead {
		t.Errorf("a no_mcp review was reported dead: %q", recs[0].Outcome)
	}
}

func TestDrainPhases_MCPPreflightFailureIsRetriedOnce(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	h := &fakeAdapter{steps: []invokeStep{
		{err: fmt.Errorf("%w: opencode serve did not report the clankerbar server", harness.ErrMCPNotConnected)},
		{res: checkpointed(10, 0.10)},            // implement retry
		{res: mcpReached(checkpointed(5, 0.50))}, // review records
		{res: checkpointed(5, 0.05)},             // fix
	}}
	h.onInvoke = func(i int) {
		switch i {
		case 2:
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-1", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{mustFinding("1.review-1.1", "open", "the seam is untested")},
			})
		case 3:
			rel.status = "in_review"
			rel.outcome += "\n### Fixes\n- 1.review-1.1 fixed\n"
		}
	}
	d, _ := panelDriverWithFleet(t, h, rel, nil, panelPhases())
	logs := captureLogs(t)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v — one preflight failure must be retried, not fail the run", err, stop)
	}
	if h.invokeCalls != 4 {
		t.Fatalf("spawned %d sessions, want 4 — the preflight failure, the implement retry, review, fix", h.invokeCalls)
	}
	if out := logs.String(); !strings.Contains(out, "MCP preflight failure") || !strings.Contains(out, "did not connect") {
		t.Errorf("the driver did not log the preflight failure:\n%s", out)
	}
}

func TestDrainPhases_MCPPreflightFailureTwiceFailsTheDrain(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	h := &fakeAdapter{steps: []invokeStep{
		{err: fmt.Errorf("%w: first", harness.ErrMCPNotConnected)},
		{err: fmt.Errorf("%w: second", harness.ErrMCPNotConnected)},
	}}
	d, _ := panelDriverWithFleet(t, h, rel, nil, panelPhases())

	_, _, stop, err := drainPhasesOnce(t, d)
	if err == nil {
		t.Fatal("two preflight failures did not fail the drain")
	}
	if !errors.Is(err, harness.ErrMCPNotConnected) {
		t.Errorf("drain error = %v, want ErrMCPNotConnected", err)
	}
	if stop {
		t.Error("an infrastructure failure must not read as a clean stop")
	}
	if h.invokeCalls != 2 {
		t.Fatalf("spawned %d sessions, want 2 — the retry is bounded at ONE", h.invokeCalls)
	}
}
