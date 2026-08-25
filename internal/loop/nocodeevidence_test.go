package loop

import (
	"errors"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/delivery"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// CLA-497: phase exit evidence is ANY of three forms — (a) a branch verified
// on the origin remote (CLA-457, unchanged), (b) the task having LEFT `ready`
// on the plane's record, or (c) a declared no-code delivery. Before this, a
// task whose correct delivery is no code could NEVER checkpoint: no branch to
// record, so every phase-1 session read as an empty exit and released the
// task back into the queue (the EZY-290 nine-session loop).
//
// The bounds stay absolute throughout: held-ness alone is nothing, and the
// silent-EOT shape — claimed, no branch, still sitting in `ready` — is STILL
// an empty exit that spawns no review phase.

// settledStates are the form-(b) statuses: every way a task can leave `ready`.
var settledStates = []string{"in_review", "done", "parked", "blocked"}

// A no-code phase whose session SETTLED the task — moved it out of `ready` —
// reaches its checkpoint: the plane's record shows the phase did its job, so
// the review phase spawns exactly as after a verified branch.
func TestDrainPhases_ANoCodeSessionThatSettledTheTaskCheckpoints(t *testing.T) {
	for _, status := range settledStates {
		t.Run(status, func(t *testing.T) {
			logs := captureLogs(t)
			h := &fakeAdapter{steps: []invokeStep{
				// Clean exit, observed claim, NO branch recorded — the no-code shape.
				{res: held(okResult(1, 0), openClaim())},
				{res: okResult(5, 0.05)},
			}}
			rel := &peekReleaser{
				next:  plane.NextTask{TaskID: "t-1"},
				state: map[string]plane.TaskState{"t-1": {Status: status}},
			}
			d := blindSpotDriver(t, h, rel)

			if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
				t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
			}
			if h.invokeCalls != 2 {
				t.Fatalf("spawned %d sessions, want 2 — a task the session settled IS exit evidence, so the review phase runs", h.invokeCalls)
			}
			if got := h.invocations[1].ResumeClaim; got.TaskID != "t-1" || got.RunID != "r-1" {
				t.Errorf("phase 2 ResumeClaim = %+v, want the observed t-1/r-1 claim carried through the seam", got)
			}
			// The successor's brief is the NO-CODE variant (CLA-497): the
			// branch-shaped builtin brief would assert a branch "the driver
			// verified to exist" that does not exist and tell the successor its
			// hand-off FAILED - the exact opposite of the checkpoint just
			// recorded, which would restart the release loop this task kills.
			p2 := h.invocations[1].Prompt
			if !strings.Contains(p2, "evidenced by the PLANE'S RECORD") {
				t.Errorf("phase 2 does not carry the no-code review brief:\n%s", p2)
			}
			if strings.Contains(p2, "FAILED hand-off") {
				t.Errorf("phase 2 tells a no-code checkpoint its hand-off FAILED:\n%s", p2)
			}
			if strings.Contains(p2, "Work in the worktree") {
				t.Errorf("phase 2 tells a no-code checkpoint to work in a worktree:\n%s", p2)
			}
			out := logs.String()
			if !strings.Contains(out, "phase reached its checkpoint holding t-1") ||
				!strings.Contains(out, "exit evidenced by the plane's record") {
				t.Errorf("the log does not name the plane-record checkpoint:\n%s", out)
			}
			if strings.Contains(out, "not a checkpoint") {
				t.Errorf("a settled task was judged an empty exit:\n%s", out)
			}
		})
	}
}

// Form (c): a DECLARED no-code delivery counts as evidence even while the task
// is still held in_progress — the delivery.noCode flag update_task requires to
// close such a task is the plane's own record that this phase shipped what it
// had to ship.
func TestDrainPhases_ADeclaredNoCodeDeliveryCountsAsEvidence(t *testing.T) {
	logs := captureLogs(t)
	h := &fakeAdapter{steps: []invokeStep{
		{res: held(okResult(1, 0), openClaim())}, // still-held clean exit, NO branch
		{res: okResult(5, 0.05)},
	}}
	rel := &peekReleaser{
		next: plane.NextTask{TaskID: "t-1"},
		state: map[string]plane.TaskState{
			"t-1": {Status: "in_progress", ClaimedByRun: "r-1", DeliveryNoCode: true},
		},
	}
	d := blindSpotDriver(t, h, rel)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 2 {
		t.Fatalf("spawned %d sessions, want 2 — a declared no-code delivery IS exit evidence on a still-held task", h.invokeCalls)
	}
	// The declared-no-code checkpoint spawns the no-code review brief, not the
	// branch-shaped one that would call the empty branch a failed hand-off.
	if p2 := h.invocations[1].Prompt; !strings.Contains(p2, "evidenced by the PLANE'S RECORD") ||
		strings.Contains(p2, "FAILED hand-off") {
		t.Errorf("phase 2 does not carry the no-code review brief:\n%s", p2)
	}
	out := logs.String()
	if !strings.Contains(out, "phase reached its checkpoint holding t-1") {
		t.Errorf("the log does not name the checkpoint:\n%s", out)
	}
	if strings.Contains(out, "not a checkpoint") {
		t.Errorf("a declared no-code delivery was judged an empty exit:\n%s", out)
	}
}

// The union is real: a RECORDED branch that fails origin verification does not
// mask a plane record that settles the task — any one form is evidence.
func TestDrainPhases_ASettledPlaneRecordOutranksAFailedBranchCheck(t *testing.T) {
	logs := captureLogs(t)
	recorded := reported(held(okResult(1, 0),
		harness.Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true, Branch: "clanker/unpushed"}), branchReport())
	h := &fakeAdapter{steps: []invokeStep{
		{res: recorded},
		{res: okResult(5, 0.05)},
	}}
	rel := &peekReleaser{
		next:  plane.NextTask{TaskID: "t-1"},
		state: map[string]plane.TaskState{"t-1": {Status: "in_review"}},
	}
	d := blindSpotDriver(t, h, rel)
	d.newVerifier = func(string, bool) deliveryVerifier {
		return &fakeVerifier{report: delivery.Report{Checks: []delivery.Check{{
			Kind: delivery.BranchPushed, Status: delivery.Fail, Detail: `branch "clanker/unpushed" is NOT on origin`,
		}}}}
	}

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 2 {
		t.Fatalf("spawned %d sessions, want 2 — the plane-record form holds even when the branch leg failed", h.invokeCalls)
	}
	// The carried claim must NOT seed the successor with the branch that FAILED
	// verification: the review brief names {{branch}} as "verified to exist on
	// the origin remote", so an unverified branch riding the claim would be the
	// exact false-verification assertion CLA-457's recomposition exists to kill.
	// The no-code brief is selected on the empty claim branch, so this also
	// asserts the phase-2 brief is the no-code one.
	if got := h.invocations[1].ResumeClaim.Branch; got != "" {
		t.Errorf("phase 2 ResumeClaim.Branch = %q, want empty - the unverified branch must not ride the seam", got)
	}
	if p2 := h.invocations[1].Prompt; strings.Contains(p2, "clanker/unpushed") ||
		!strings.Contains(p2, "evidenced by the PLANE'S RECORD") {
		t.Errorf("phase 2 names the unverified branch or lost the no-code brief:\n%s", p2)
	}
	if out := logs.String(); strings.Contains(out, "not a checkpoint") {
		t.Errorf("form (b) did not outrank the failed branch check:\n%s", logs.String())
	}
}

// The CLA-457 regression, now against a plane the gate can actually ASK:
// a session that claimed, recorded no branch, and left the task still reading
// as not-settled (`ready`, or merely held with nothing declared) is STILL the
// empty exit — released back to the queue, retried once, tallied, and never
// handed to the review brief. The gate consulted the record and refused.
func TestDrainPhases_SilentEOTStillReadsEmptyEvenWithThePlaneWired(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state plane.TaskState
	}{
		{"still ready (released or never claimed)", plane.TaskState{Status: "ready"}},
		{"held with nothing declared", plane.TaskState{Status: "in_progress", ClaimedByRun: "r-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			empty := func() harness.Result {
				ok := okResult(1, 0)
				ok.Raw[harness.FinishReasonKey] = "stop"
				return held(ok, openClaim())
			}
			h := &fakeAdapter{steps: []invokeStep{{res: empty()}, {res: empty()}}}
			rel := &peekReleaser{
				next:  plane.NextTask{TaskID: "t-1"},
				state: map[string]plane.TaskState{"t-1": tc.state},
			}
			d := blindSpotDriver(t, h, rel)

			if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
				t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
			}
			if h.invokeCalls != 2 {
				t.Fatalf("spawned %d sessions, want 2 (the empty phase and its single bounded retry) — %s is not evidence", h.invokeCalls, tc.name)
			}
			for _, inv := range h.invocations {
				if strings.Contains(inv.Prompt, "PHASE 2") {
					t.Errorf("the review phase spawned after a silent-EOT exit: %q", inv.Prompt)
				}
			}
			// The gate actually consulted the plane — once per empty session —
			// and refused anyway. That consultation is what this test adds over
			// its pre-CLA-497 sibling.
			if len(rel.asked) != 2 || rel.asked[0] != "t-1" || rel.asked[1] != "t-1" {
				t.Errorf("asked the plane about %v, want t-1 twice (once per session)", rel.asked)
			}
			if len(rel.calls) != 2 || rel.calls[0] != (releaseCall{"t-1", "r-1"}) || rel.calls[1] != (releaseCall{"t-1", "r-1"}) {
				t.Errorf("released %+v, want {t-1 r-1} twice — the empty claim goes back to the queue", rel.calls)
			}
			out := logs.String()
			if !strings.Contains(out, "not a checkpoint") || !strings.Contains(out, "no exit evidence either") {
				t.Errorf("the empty exit does not name why the plane record was refused:\n%s", out)
			}
			cell := d.deadTally[tallyKey{phase: "implement", harness: "claude"}]
			if cell == nil || cell.run != 2 || cell.dead != 2 {
				t.Errorf("tally cell = %+v, want run=2 dead=2 — both sessions still count as produced-nothing", cell)
			}
		})
	}
}

// Form (a) unchanged: when a recorded branch verifies on the origin remote,
// that is evidence and the plane is never asked — the ordinary healthy path
// costs no extra read.
func TestDrainPhases_AVerifiedBranchCheckpointsWithoutAskingThePlane(t *testing.T) {
	logs := captureLogs(t)
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(1, 0)},
		{res: okResult(5, 0.05)},
	}}
	rel := &peekReleaser{
		next: plane.NextTask{TaskID: "t-1"},
		state: map[string]plane.TaskState{
			// Would NOT be evidence if asked — proving it was not asked.
			"t-1": {Status: "ready"},
		},
	}
	d := blindSpotDriver(t, h, rel)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 2 {
		t.Fatalf("spawned %d sessions, want 2 — the verified-branch checkpoint is unchanged", h.invokeCalls)
	}
	if len(rel.asked) != 0 {
		t.Errorf("asked the plane about %v despite a passing branch check; form (a) short-circuits", rel.asked)
	}
	// The verified-branch checkpoint keeps the BRANCH-SHAPED review brief: the
	// no-code variant is selected only when the carried claim names no branch.
	if p2 := h.invocations[1].Prompt; !strings.Contains(p2, "clanker/x") ||
		!strings.Contains(p2, "PHASE 2") || strings.Contains(p2, "PLANE'S RECORD") {
		t.Errorf("phase 2 lost the branch-shaped review brief for a verified branch:\n%s", p2)
	}
	if out := logs.String(); !strings.Contains(out, "branch clanker/x verified on the origin remote") {
		t.Errorf("the log does not name the verified-branch checkpoint:\n%s", out)
	}
}

// A plane blip degrades to the branch-only judgement: an unreadable record is
// no evidence, and the exit keeps today's (pre-CLA-497) refusal verbatim.
func TestDrainPhases_AFailedPlaneReadDegradesToBranchOnlyEvidence(t *testing.T) {
	logs := captureLogs(t)
	h := &fakeAdapter{steps: []invokeStep{
		{res: held(okResult(1, 0), openClaim())},
		{res: held(okResult(1, 0), openClaim())},
	}}
	rel := &peekReleaser{
		next:     plane.NextTask{TaskID: "t-1"},
		stateErr: errors.New("plane 503"),
	}
	d := blindSpotDriver(t, h, rel)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 2 {
		t.Fatalf("spawned %d sessions, want 2 — a failed plane read is no evidence, so the exit stays empty", h.invokeCalls)
	}
	out := logs.String()
	if !strings.Contains(out, "could not read the plane's record of t-1") {
		t.Errorf("the log does not name the degraded read:\n%s", out)
	}
	if !strings.Contains(out, "not a checkpoint") || !strings.Contains(out, "no branch recorded on the task") {
		t.Errorf("the degraded exit does not keep the legacy refusal:\n%s", out)
	}
}

// A handoff during a NO-CODE review must not append the branch-shaped
// reviewTerminalStep to the successor: it would tell a session reviewing a
// task with no branch and no code to "Open a PR targeting the repo's
// integration branch (staging)". The no-code continuation carries the no-code
// terminal step instead, and the rerun bound rides forward exactly as it does
// on the branch-shaped path (CLA-497).
func TestDrainPhases_AHandoffDuringNoCodeReviewCarriesTheNoCodeTerminalStep(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: held(okResult(1, 0), openClaim())}, // no-code phase-1 checkpoint (plane record)
		{res: handoffResult("Record reviewed: the settlement is honest; verifying the outcome next.")},
		{res: okResult(1, 0)},
	}}
	rel := &peekReleaser{
		next:  plane.NextTask{TaskID: "t-1"},
		state: map[string]plane.TaskState{"t-1": {Status: "in_review"}},
	}
	d := blindSpotDriver(t, h, rel)

	if _, _, handoffs, stop, err := drainPhasesHandoffs(t, d, 1); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	} else if handoffs != 1 {
		t.Errorf("handoffs = %d, want 1", handoffs)
	}
	if h.invokeCalls != 3 {
		t.Fatalf("spawned %d sessions, want 3: implement, review, its handoff successor", h.invokeCalls)
	}
	p3 := h.invocations[2].Prompt
	if !strings.Contains(p3, "no-code delivery declared") {
		t.Errorf("the no-code review handoff successor lost the no-code terminal step:\n%s", p3)
	}
	if strings.Contains(p3, "Open a PR") {
		t.Errorf("the no-code review handoff successor was told to open a PR for a branch-less task:\n%s", p3)
	}
	if !strings.Contains(p3, "RERUN BOUND") {
		t.Errorf("the no-code review handoff successor lost the rerun bound:\n%s", p3)
	}
}
