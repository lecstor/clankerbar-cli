package loop

import (
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/delivery"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// CLA-576: an ls-remote failure reads as "branch absent on origin" unless the
// two are kept apart. The delivery package already separates them — Unknown
// means the origin could not be READ, Fail means it was read and the branch is
// not there — but the checkpoint gate flattened both into "not evidence", so a
// finished implement phase was discarded as an empty exit, no review phase
// spawned, and the task burned sessions on expired leases until the plane
// parked it (the MAK-122 shape, 2026-09-23). The 2026-09-24 decision: an
// Unknown with a recorded branch is an UNVERIFIED checkpoint — the review
// phase spawns and is told to verify the hand-off itself — while a Fail keeps
// today's refusal exactly. Unknown is never booked into the CLA-457
// dead-phase tally.

// unreadableOriginVerifier answers every branch with an Unknown whose detail
// is the shape the real check emits when git cannot reach the remote.
func unreadableOriginVerifier() *fakeVerifier {
	return &fakeVerifier{report: delivery.Report{Checks: []delivery.Check{{
		Kind:   delivery.BranchPushed,
		Status: delivery.Unknown,
		Detail: "could not read origin: git ls-remote: exit status 128: git@github.com: Permission denied (publickey)",
	}}}}
}

// The Unknown arm: a recorded branch whose origin check could not run is an
// UNVERIFIED checkpoint. The review phase spawns; the log states the
// checkpoint is unverified with the Unknown detail and does NOT read as
// "no recorded branch on the origin remote"; the successor's brief does not
// claim the branch was verified on origin and tells it to verify the hand-off
// itself; the phase is not booked into the dead-phase tally.
func TestDrainPhases_AnUnreadableOriginIsAnUnverifiedCheckpoint(t *testing.T) {
	logged := captureLogs(t)
	// The real update_task(branch:) sets HasWIP on the claim AND arms the
	// report; the verifier cannot read origin at all.
	recorded := reported(held(okResult(1, 0),
		harness.Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true, Branch: "clanker/x"}), branchReport())
	h := &fakeAdapter{steps: []invokeStep{
		{res: recorded},
		{res: okResult(5, 0.05)},
	}}
	d, rel := phaseDriver(t, h, twoPhases())
	d.newVerifier = func(string, bool) deliveryVerifier { return unreadableOriginVerifier() }

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	// The unverified checkpoint advances the seam: the review phase is the
	// backstop the decision accepted.
	if h.invokeCalls != 2 {
		t.Fatalf("spawned %d sessions, want 2 — an unreadable origin is an unverified checkpoint, not an empty exit", h.invokeCalls)
	}

	// The trap CLA-576 names: the branch-shaped brief's "a branch the driver
	// verified to exist on the origin remote" is the one claim an unverified
	// checkpoint cannot support, and it must not reach the successor.
	p2 := h.invocations[1].Prompt
	if strings.Contains(p2, "verified to exist on the origin remote") {
		t.Errorf("the successor brief claims the branch was verified on the origin remote:\n%s", p2)
	}
	if !strings.Contains(p2, "UNVERIFIED") {
		t.Errorf("the successor brief does not state the checkpoint is unverified:\n%s", p2)
	}
	if !strings.Contains(p2, "verify the hand-off yourself") {
		t.Errorf("the successor brief does not tell the review phase to verify the hand-off itself:\n%s", p2)
	}
	if !strings.Contains(p2, "clanker/x") {
		t.Errorf("the successor brief does not name the recorded branch it must verify: %q", p2)
	}
	if strings.Contains(p2, config.PhaseBranchPlaceholder) {
		t.Errorf("the {{branch}} placeholder survived into the brief: %q", p2)
	}
	// The recorded branch rides the claim — the review phase needs the name to
	// verify it — and the claim is held across the seam, not released.
	if got := h.invocations[1].ResumeClaim.Branch; got != "clanker/x" {
		t.Errorf("phase 2 ResumeClaim.Branch = %q, want the recorded branch", got)
	}
	if len(rel.calls) != 0 {
		t.Errorf("released the claim at the seam: %+v — an unverified checkpoint holds the lease for the review phase", rel.calls)
	}

	out := logged.String()
	if !strings.Contains(out, "UNVERIFIED checkpoint") || !strings.Contains(out, "could not read origin") {
		t.Errorf("the log does not state the checkpoint is unverified with the Unknown detail:\n%s", out)
	}
	if strings.Contains(out, "branch clanker/x verified on the origin remote") {
		t.Errorf("the log names the unverified branch as verified on the origin remote:\n%s", out)
	}
	if strings.Contains(out, "no recorded branch on the origin remote") {
		t.Errorf("the log reads as a branch-less exit when the branch IS recorded and the origin was unreadable:\n%s", out)
	}
	// Unknown is never booked into the CLA-457 dead-phase tally.
	cell := d.deadTally[tallyKey{phase: "implement", harness: "claude"}]
	if cell == nil || cell.dead != 0 {
		t.Errorf("tally cell = %+v, want dead=0 — an unreadable origin is not a produced-nothing phase", cell)
	}
}

// The Fail arm, kept exactly as today: the origin WAS read and the branch is
// not there, so the phase is a held-but-empty exit — no review phase, the
// dead-phase tally books it, and the log keeps the not-a-checkpoint shape.
// This extends TestDrainPhases_RecordedButUnreachableBranchIsNotACheckpoint
// with the tally assertion the CLA-576 decision calls out as the other half of
// the split.
func TestDrainPhases_ARefutedBranchIsStillTalliedAsEmpty(t *testing.T) {
	logged := captureLogs(t)
	recorded := reported(held(okResult(1, 0),
		harness.Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true, Branch: "clanker/x"}), branchReport())
	h := &fakeAdapter{steps: []invokeStep{
		{res: recorded},
		{res: okResult(1, 0)},
	}}
	d, rel := phaseDriver(t, h, twoPhases())
	d.newVerifier = func(string, bool) deliveryVerifier {
		return &fakeVerifier{report: delivery.Report{Checks: []delivery.Check{{
			Kind: delivery.BranchPushed, Status: delivery.Fail,
			Detail: `branch "clanker/x" is NOT on origin`,
		}}}}
	}

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 1 {
		t.Errorf("spawned %d sessions, want 1 — an unpushed branch must not advance the seam to review", h.invokeCalls)
	}
	if len(rel.calls) != 0 {
		t.Errorf("released a claim with a recorded (unpushed) branch: %+v — that would destroy the takeover hand-off", rel.calls)
	}
	out := logged.String()
	if !strings.Contains(out, "not a checkpoint") || !strings.Contains(out, "NOT on origin") {
		t.Errorf("the refuted-branch exit is not named with its reason:\n%s", out)
	}
	if strings.Contains(out, "UNVERIFIED checkpoint") {
		t.Errorf("a refuted branch was logged as an unverified checkpoint:\n%s", out)
	}
	cell := d.deadTally[tallyKey{phase: "implement", harness: "claude"}]
	if cell == nil || cell.run != 1 || cell.dead != 1 {
		t.Errorf("tally cell = %+v, want run=1 dead=1 — a refuted branch stays in the CLA-457 dead-phase rate", cell)
	}
}

// The unverified checkpoint carries a branch the origin could not be READ,
// never one the origin refuted: with a draft that failed and a final that
// could not be read (or the reverse), the branch that rides the claim is the
// Unknown one — the one the review phase might still fetch.
func TestDrainPhases_UnverifiedCheckpointCarriesTheUnknownBranchNotTheRefutedOne(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status map[string]delivery.Status
		want   string
	}{
		{
			"a refuted draft before an unreadable final",
			map[string]delivery.Status{"clanker/draft": delivery.Fail, "clanker/final": delivery.Unknown},
			"clanker/final",
		},
		{
			"an unreadable draft before a refuted final",
			map[string]delivery.Status{"clanker/draft": delivery.Unknown, "clanker/final": delivery.Fail},
			"clanker/draft",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			draft := harness.Report{TaskID: "t-1", Ref: "CLA-253", RunID: "r-1", Branch: "clanker/draft"}
			final := harness.Report{TaskID: "t-1", Ref: "CLA-253", RunID: "r-1", Branch: "clanker/final"}
			h := &fakeAdapter{steps: []invokeStep{
				{res: reported(held(okResult(1, 0), openClaim()), draft, final)},
				{res: okResult(1, 0)},
			}}
			d, _ := phaseDriver(t, h, twoPhases())
			d.newVerifier = func(string, bool) deliveryVerifier { return branchStatusVerifier(tc.status) }

			if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
				t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
			}
			if h.invokeCalls != 2 {
				t.Fatalf("spawned %d sessions, want 2 — at least one Unknown branch is an unverified checkpoint", h.invokeCalls)
			}
			if got := h.invocations[1].ResumeClaim.Branch; got != tc.want {
				t.Errorf("phase 2 ResumeClaim.Branch = %q, want %q — the carried branch is the one whose origin could not be read, never one the origin refuted", got, tc.want)
			}
			if p2 := h.invocations[1].Prompt; !strings.Contains(p2, tc.want) {
				t.Errorf("the successor brief does not name the branch it must verify (%q):\n%s", tc.want, p2)
			}
		})
	}
}

// The plane-record forms still outrank an unreadable branch: when the task
// itself LEFT ready, the settlement is the evidence and the checkpoint is the
// branch-less no-code one — not an unverified branch checkpoint. This mirrors
// TestDrainPhases_ASettledPlaneRecordOutranksAFailedBranchCheck for the
// Unknown leg.
func TestDrainPhases_ASettledPlaneRecordOutranksAnUnreadableOrigin(t *testing.T) {
	logged := captureLogs(t)
	recorded := reported(held(okResult(1, 0),
		harness.Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true, Branch: "clanker/x"}), branchReport())
	h := &fakeAdapter{steps: []invokeStep{
		{res: recorded},
		{res: okResult(5, 0.05)},
	}}
	rel := &peekReleaser{
		next:  plane.NextTask{TaskID: "t-1"},
		state: map[string]plane.TaskState{"t-1": {Status: "in_review"}},
	}
	d := blindSpotDriver(t, h, rel)
	d.newVerifier = func(string, bool) deliveryVerifier { return unreadableOriginVerifier() }

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 2 {
		t.Fatalf("spawned %d sessions, want 2 — the plane-record form holds even when the origin could not be read", h.invokeCalls)
	}
	if got := h.invocations[1].ResumeClaim.Branch; got != "" {
		t.Errorf("phase 2 ResumeClaim.Branch = %q, want empty — the branch-less plane-record form clears the recorded branch", got)
	}
	p2 := h.invocations[1].Prompt
	if !strings.Contains(p2, "evidenced by the PLANE'S RECORD") || strings.Contains(p2, "UNVERIFIED") {
		t.Errorf("the unreadable branch outranked the plane record, or the no-code brief was lost:\n%s", p2)
	}
	if out := logged.String(); strings.Contains(out, "not a checkpoint") {
		t.Errorf("form (b) did not outrank the unreadable branch check:\n%s", out)
	}
}

// The peek path faces the same split (CLA-451): a claim recovered from the
// plane whose branch cannot be read is an unverified checkpoint too, and its
// log line names the unreadable origin rather than reading as a refusal.
func TestDrainPhases_ARecoveredClaimWithAnUnreadableOriginCheckpointsUnverified(t *testing.T) {
	logged := captureLogs(t)
	h := &fakeAdapter{steps: []invokeStep{
		{res: okResult(1, 0)},
		{res: okResult(5, 0.05)},
	}}
	rel := &peekReleaser{
		next:  plane.NextTask{TaskID: "t-1"},
		state: map[string]plane.TaskState{"t-1": {Status: "in_progress", ClaimedByRun: "r-curl", Branch: "clanker/phantom"}},
	}
	d := blindSpotDriver(t, h, rel)
	d.newVerifier = func(string, bool) deliveryVerifier { return unreadableOriginVerifier() }

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 2 {
		t.Fatalf("spawned %d sessions, want 2 — a recovered claim with an unreadable origin is an unverified checkpoint", h.invokeCalls)
	}
	if got := h.invocations[1].ResumeClaim.Branch; got != "clanker/phantom" {
		t.Errorf("phase 2 ResumeClaim.Branch = %q, want the recovered branch", got)
	}
	if p2 := h.invocations[1].Prompt; !strings.Contains(p2, "UNVERIFIED") || strings.Contains(p2, "verified to exist on the origin remote") {
		t.Errorf("the recovered unverified checkpoint did not get the unverified brief:\n%s", p2)
	}
	out := logged.String()
	if !strings.Contains(out, "UNVERIFIED checkpoint") || !strings.Contains(out, "could not read origin") {
		t.Errorf("the recovered checkpoint is not logged as unverified with the Unknown detail:\n%s", out)
	}
	if strings.Contains(out, "not treating it as a checkpoint") {
		t.Errorf("a recovered claim with an unreadable origin was refused as a checkpoint:\n%s", out)
	}
}
