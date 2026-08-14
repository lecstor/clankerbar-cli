package harness

import (
	"io"
	"strings"
	"testing"
)

// A resumed phase is told not to claim anything — it is continuing a run, not
// starting one — so it never calls claim_task and the adapter observes no claim
// of its own. Result.Claim.Held() gates the driver's handback, the CLA-314
// salvage AND the CLA-253 delivery check, so an unseeded resumed phase runs with
// all three inert, and it is the phase that pushes the branch and opens the PR.
//
// The behavioural payload of the seed is that noteToolUse's update_task arm
// matches (`res.Claim.Names(taskID)`) — the arm that arms the settle, the
// delivery Report, and HasWIP. That is what these assert, so deleting the seed in
// Invoke turns them red rather than leaving the whole suite green.
func TestSeededResumeClaimArmsTheUpdateTaskArm(t *testing.T) {
	const seededTask = "task-abc"

	stream := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu-1","name":"mcp__clankerbar__update_task",` +
			`"input":{"taskId":"task-abc","branch":"clanker/task-abc-work"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu-1","is_error":false}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu-2","name":"mcp__clankerbar__update_task",` +
			`"input":{"taskId":"task-abc","status":"in_review","delivery":{"pr":"#42"}}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu-2","is_error":false}]}}`,
		`{"type":"result","subtype":"success","total_cost_usd":1.5,"usage":{"input_tokens":10,"output_tokens":20}}`,
	}, "\n") + "\n"

	// Built through the same constructor Invoke uses, so deleting the seed there
	// turns this red rather than leaving the suite green.
	res := newSessionResult(Invocation{ResumeClaim: Claim{TaskID: seededTask, RunID: "run-xyz"}})
	res.scans = newScanCache()
	(claude{}).consume(strings.NewReader(stream), io.Discard, newTail(), &res, 0, func() {})

	if !res.Claim.HasWIP {
		t.Error("update_task(branch:) did not set HasWIP on the seeded claim — the claim would look releasable, so the driver would post a task with pushed work back to the queue instead of leaving the takeover hand-off")
	}
	if !res.Claim.Settled {
		t.Error("update_task(status: in_review) did not settle the seeded claim — the driver would then hand back a task already in review, which is the one outcome CLA-262 calls worse than a lease expiring")
	}
	if len(res.Reports) == 0 {
		t.Fatal("no delivery Report was armed for the seeded claim, so CLA-253's branch/commit/PR verification is silently off for the phase that actually opens the PR")
	}
	// Only the BRANCH claim survives, and that is the design rather than a gap:
	// Report.Empty drops a claim with nothing locally checkable, and a status plus
	// a PR ref is not something the driver can hold up against git. The branch is.
	var sawBranch bool
	for _, r := range res.Reports {
		if r.TaskID != seededTask {
			t.Errorf("Report is against %q, want the seeded task %q", r.TaskID, seededTask)
		}
		if r.RunID != "run-xyz" {
			t.Errorf("Report carries run %q, want the seeded run", r.RunID)
		}
		if r.Branch == "clanker/task-abc-work" {
			sawBranch = true
		}
	}
	if !sawBranch {
		t.Errorf("no Report carried the recorded branch, so nothing would verify it is really on the remote; got %+v", res.Reports)
	}
}

// The seed must not swallow a call about some OTHER task — that is the guard
// `Claim.Names` exists for, and seeding is what makes it reachable at all here.
func TestSeededResumeClaimIgnoresAnUnrelatedTask(t *testing.T) {
	stream := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu-1","name":"mcp__clankerbar__update_task",` +
		`"input":{"taskId":"some-other-task","branch":"clanker/not-ours"}}]}}` + "\n"

	res := newSessionResult(Invocation{ResumeClaim: Claim{TaskID: "task-abc", RunID: "run-xyz"}})
	res.scans = newScanCache()
	(claude{}).consume(strings.NewReader(stream), io.Discard, newTail(), &res, 0, func() {})

	if res.Claim.HasWIP {
		t.Error("a branch recorded against a DIFFERENT task set HasWIP on ours")
	}
	if len(res.Reports) != 0 {
		t.Errorf("armed a delivery Report from another task's update_task: %+v", res.Reports)
	}
}

// An unseeded session (phase 1, or any unphased run) is unchanged: with no claim
// yet, the update_task arm must stay shut, or a session's first act could forge
// claim state it never earned.
func TestAnUnseededSessionIgnoresUpdateTaskUntilItClaims(t *testing.T) {
	stream := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu-1","name":"mcp__clankerbar__update_task",` +
		`"input":{"taskId":"task-abc","branch":"clanker/task-abc-work"}}]}}` + "\n"

	res := newSessionResult(Invocation{})
	res.scans = newScanCache()
	(claude{}).consume(strings.NewReader(stream), io.Discard, newTail(), &res, 0, func() {})

	if res.Claim.HasWIP || res.Claim.TaskID != "" {
		t.Errorf("an unseeded session picked up claim state from an update_task it never claimed for: %+v", res.Claim)
	}
}
