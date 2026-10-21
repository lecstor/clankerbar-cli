package loop

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// The review panel (CLA-639): one configured `review` phase expands into one
// review phase per member, on the same reviewed tip, each writing its own sealed
// durable record under its own reviewer role; the round closes only after every
// member has recorded or been recorded as failed, and the driver merges the
// released records into the fix phase's work list.
//
// These fixtures drive the driver directly and mutate a reviewReleaser from the
// fake adapter's onInvoke, exactly as a real session's update_task(review: ...)
// lands between two spawns. The member list is deliberately two models with one
// repeated name shape: the duplicate is an independent run, not a duplicate to
// deduplicate. The panel is SET on every one of them — an absent `review_panel`
// is the single reviewer, so nothing here relies on a default.

// panelPhases is the shipped three-phase sequence. EffectivePhases expands the
// review phase into the panel below; the fix phase stays one phase and receives
// the merged list.
func panelPhases() []config.Phase {
	return []config.Phase{
		{Name: config.ImplementPhaseName},
		{Name: config.ReviewPhaseName},
		{Name: config.FixPhaseName},
	}
}

// panelMembers is the operator's own fleet as an example, smaller: the same
// model twice plus another model once.
func panelMembers() []config.ReviewPanelMember {
	return []config.ReviewPanelMember{
		{Name: "review-1", Harness: "opencode", Model: "opencode-go/gpt-6-luna"},
		{Name: "review-2", Harness: "opencode", Model: "opencode-go/gpt-6-luna"},
		{Name: "review-3", Harness: "opencode", Model: "opencode-go/glm-5.3-flash"},
	}
}

// panelDriver wires a phased driver over the given members, on one fake adapter
// for every harness (so the invocation list is the spawn order). A nil member
// list leaves the config's panel absent, which is the single-reviewer shape the
// driver must run exactly as it did before CLA-639.
func panelDriver(t *testing.T, h *fakeAdapter, rel plane.Releaser, members []config.ReviewPanelMember, phases []config.Phase) *Driver {
	t.Helper()
	d, _ := panelDriverWithFleet(t, h, rel, members, phases)
	return d
}

// panelDriverWithFleet is panelDriver plus the fakeFleet the iteration-record
// assertions need: the outcome that must name an incomplete panel rides the
// fleet beacon (CLA-466), not the state dir.
func panelDriverWithFleet(t *testing.T, h *fakeAdapter, rel plane.Releaser, members []config.ReviewPanelMember, phases []config.Phase) (*Driver, *fakeFleet) {
	t.Helper()
	cfg := fastCfg()
	cfg.Prompt = ""
	cfg.Phases = phases
	cfg.ReviewPanel = members
	ff := &fakeFleet{}
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: rel, Fleet: ff}})
	// One fake adapter answers every harness name, so the invocation list is the
	// spawn order and no real CLI is reached.
	d.newAdapter = func(string) (harness.Adapter, error) { return h, nil }
	openTestStateDir(t, d)
	d.cfg.WorkDir = t.TempDir()
	d.newVerifier = func(string, bool) deliveryVerifier { return passVerifier() }
	d.probePRBase = func(context.Context, string, string) (bool, string, error) { return true, "", nil }
	return d, ff
}

// testCheckpointTip and the records below share the run r-1 the fixtures'
// claims carry, so the seam's freshness key matches exactly as in production.

// TestDrainPhases_ReviewPanelRunsOneSealedMemberPerPhase is the happy path: N
// spawns for N members on the same tip, no close between them, one close before
// the fix, and the fix receives every member's findings merged with the
// consensus annotation.
func TestDrainPhases_ReviewPanelRunsOneSealedMemberPerPhase(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)}, // implement
		{res: checkpointed(5, 0.50)},  // review-1
		{res: checkpointed(5, 0.50)},  // review-2
		{res: checkpointed(5, 0.50)},  // review-3
		{res: checkpointed(5, 0.05)},  // fix
	}}
	var closesAtFix []int
	type inPanelRead struct {
		sealed   bool
		findings int
	}
	var recordsWhenEachMemberRan [][]inPanelRead
	snapshot := func() []inPanelRead {
		// Read through TaskState, exactly as the session's get_task would: the
		// fake withholds a sealed record's findings there, so this is what a later
		// member can actually see.
		st, _ := rel.TaskState(context.Background(), "t-1")
		out := make([]inPanelRead, 0, len(st.Reviews))
		for _, r := range st.Reviews {
			out = append(out, inPanelRead{sealed: r.State == "sealed", findings: len(r.Findings)})
		}
		return out
	}
	h.onInvoke = func(i int) {
		switch i {
		case 1: // review-1 records a MUST and a NIT
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-1", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{
					mustFinding("1.review-1.1", "open", "the seam is untested"),
					{ID: "1.review-1.2", Severity: "nit", Status: "open", Summary: "name the local variable better"},
				},
			})
		case 2: // review-2 records a SHOULD; the ROUND MUST STILL BE SEALED
			recordsWhenEachMemberRan = append(recordsWhenEachMemberRan, snapshot())
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-2", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{
					{ID: "1.review-2.1", Severity: "should", Status: "open", Summary: "handle the empty input"},
				},
			})
		case 3: // review-3 records a MUST of its own
			recordsWhenEachMemberRan = append(recordsWhenEachMemberRan, snapshot())
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-3", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{
					mustFinding("1.review-3.1", "open", "the guard is bypassed on the retry path"),
				},
			})
		case 4: // fix: the panel's round is already released
			closesAtFix = append(closesAtFix, len(rel.closes))
			rel.status = "in_review"
			rel.outcome += "\n## Review\n- verdict: findings\n\n### Fixes\n- 1.review-1.1 fixed\n- 1.review-3.1 fixed\n"
		}
	}
	d := panelDriver(t, h, rel, panelMembers(), panelPhases())

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 5 {
		t.Fatalf("spawned %d sessions, want 5 — implement, three reviewers, fix", h.invokeCalls)
	}

	// One review phase per member, sequential, on the SAME reviewed tip.
	for i := 1; i <= 3; i++ {
		p := h.invocations[i].Prompt
		if !strings.Contains(p, "REVIEW-ONLY gate") {
			t.Errorf("panel session %d is not running the review-only brief:\n%s", i, p)
		}
		if !strings.Contains(p, testCheckpointTip) {
			t.Errorf("panel session %d does not name the tip the driver verified:\n%s", i, p)
		}
	}
	if !strings.Contains(h.invocations[1].Prompt, `harness: "opencode", model: "opencode-go/gpt-6-luna"`) {
		t.Errorf("review-1's brief does not name its own harness/model:\n%s", h.invocations[1].Prompt)
	}
	if !strings.Contains(h.invocations[3].Prompt, `harness: "opencode", model: "opencode-go/glm-5.3-flash"`) {
		t.Errorf("review-3's brief does not name the model that member runs:\n%s", h.invocations[3].Prompt)
	}

	// Independence: when member 2 and member 3 ran, every record already on the
	// task was still SEALED — the plane withholds a sealed record's findings, and
	// no close between members is what keeps that true.
	if len(recordsWhenEachMemberRan) != 2 {
		t.Fatalf("captured %d in-panel reads, want 2", len(recordsWhenEachMemberRan))
	}
	for n, recs := range recordsWhenEachMemberRan {
		for _, r := range recs {
			if !r.sealed {
				t.Errorf("member %d ran with a record in state open — the panel closed a round before every member had recorded, releasing findings to a later reviewer", n+2)
			}
			if r.findings > 0 {
				t.Errorf("member %d ran with findings already readable — a later member must not see an earlier member's findings", n+2)
			}
		}
	}

	// Exactly one close, and it happened before the fix spawned.
	if len(rel.closes) != 1 || rel.closes[0] != 1 {
		t.Fatalf("closed rounds = %v, want exactly [1] — one round for the whole panel, released once to the fix", rel.closes)
	}
	if len(closesAtFix) != 1 || closesAtFix[0] != 1 {
		t.Fatalf("the fix phase spawned with %v closes recorded, want 1 — the fixer may not run against a sealed round", closesAtFix)
	}

	// The merged list: every MUST from any member open, SHOULD/NIT annotated
	// with their member, and no member's finding lost.
	fix := h.invocations[4].Prompt
	for _, want := range []string{
		"1.review-1.1",
		"1.review-3.1",
		"1.review-2.1",
		"raised by review-2",
		"raised by review-1",
	} {
		if !strings.Contains(fix, want) {
			t.Errorf("the fix brief does not carry %q — the merged list must union every member's findings:\n%s", want, fix)
		}
	}
	if strings.Contains(fix, "INCOMPLETE") {
		t.Errorf("a panel whose every member recorded was reported incomplete:\n%s", fix)
	}
	// The fixer never grades reviews: it is handed the driver's list, with the
	// union rule stated, and no instruction to decide what stays open.
	if !strings.Contains(fix, "every MUST from ANY member is open") {
		t.Errorf("the fix brief does not state the union rule:\n%s", fix)
	}
	// And each member's record is durable under its own role, one row per role.
	roles := map[string]int{}
	for _, r := range rel.records {
		roles[r.ReviewerRole]++
	}
	for _, want := range []string{"review-1", "review-2", "review-3"} {
		if roles[want] != 1 {
			t.Errorf("durable records by role = %v, want exactly one %s record", roles, want)
		}
	}
}

// TestDrainPhases_ReviewPanelMemberFailureDoesNotStopTheRound: a member that
// records nothing — dead, capped or failed — is recorded as failed and the
// panel MOVES ON. A failure in the MIDDLE leaves the next member a claim to
// resume and the later members still run; a failure at the END lets the fix run
// on what the earlier members found. This is the doneWhen's "recorded or been
// recorded as failed/capped", and the bar's "a usage-limited or dead member
// does not stop the round, and the outcome says the panel was incomplete".
func TestDrainPhases_ReviewPanelMemberFailureDoesNotStopTheRound(t *testing.T) {
	t.Run("a mid-panel member that dies producing nothing does not stop the panel", func(t *testing.T) {
		rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
		h := &fakeAdapter{steps: []invokeStep{
			{res: checkpointed(10, 0.10)}, // implement
			{res: checkpointed(5, 0.50)},  // review-1 records
			// review-2 dies holding the claim, having recorded nothing. The claim
			// is STILL LIVE: the driver must keep it (the next member resumes the
			// same run) and must not hand it back.
			{res: held(deadResult(), openClaim())},
			{res: checkpointed(5, 0.50)}, // review-3 records
			{res: checkpointed(5, 0.05)}, // fix: the merged list, naming review-2
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
					Findings: []plane.ReviewFindingState{mustFinding("1.review-3.1", "open", "the guard is bypassed on the retry path")},
				})
			case 4:
				rel.status = "in_review"
				rel.outcome += "\n### Fixes\n- 1.review-1.1 fixed\n- 1.review-3.1 fixed\n"
			}
		}
		d, ff := panelDriverWithFleet(t, h, rel, panelMembers(), panelPhases())
		logs := captureLogs(t)

		if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
			t.Fatalf("drainPhases: err=%v stop=%v — a dead member must not fail the run", err, stop)
		}
		// Every member ran and the fix ran: nothing stopped at review-2, and no
		// index was re-spawned (that would be 6 sessions, not 5).
		if h.invokeCalls != 5 {
			t.Fatalf("spawned %d sessions, want 5 — implement, three members, fix; a failed member must be skipped PAST, never re-run", h.invokeCalls)
		}
		// The advance is to the NEXT member, not the same one: member 3's own
		// role and model in the third review session's prompt.
		if p := h.invocations[3].Prompt; !strings.Contains(p, `reviewerRole: "review-3"`) {
			t.Errorf("the session after the failed member is not review-3 — the panel re-ran the failed index:\n%s", p)
		}
		if p := h.invocations[2].Prompt; !strings.Contains(p, `reviewerRole: "review-2"`) {
			t.Errorf("the failed member's own session is not review-2:\n%s", p)
		}
		// One close for the whole panel, made while the claim was still held.
		if len(rel.closes) != 1 || rel.closes[0] != 1 {
			t.Fatalf("closed rounds = %v, want exactly [1] — the round closes after the last member, whichever way it ends", rel.closes)
		}
		// The fix runs on the merged list and is told the panel was incomplete,
		// by member.
		fix := h.invocations[4].Prompt
		for _, want := range []string{"1.review-1.1", "1.review-3.1", "INCOMPLETE", "review-2"} {
			if !strings.Contains(fix, want) {
				t.Errorf("the fix brief does not carry %q — the merged list must name the incomplete member:\n%s", want, fix)
			}
		}
		// The operator sees it too, but in the LOG — never in the iteration
		// outcome, which stays one of the four words the plane accepts (CLA-738:
		// free text there made the plane refuse the whole report).
		recs := ff.records()
		if len(recs) != 1 {
			t.Fatalf("iteration records = %d, want 1", len(recs))
		}
		requireFleetOutcome(t, recs[0].Outcome, fleet.OutcomeReleased)
		out := logs.String()
		for _, want := range []string{"review panel INCOMPLETE", "review-2", "died producing nothing"} {
			if !strings.Contains(out, want) {
				t.Errorf("the daemon log does not carry %q:\n%s", want, out)
			}
		}
		// And the run's dead-phase tally books the member that produced nothing,
		// per member and harness — a failing member has to be visible to the
		// operator, not only in one drain's log.
		cell := d.deadTally[tallyKey{phase: "review-2", harness: "opencode"}]
		if cell == nil || cell.dead != 1 {
			t.Errorf("dead-phase tally for review-2/opencode = %+v, want 1 dead", cell)
		}
		if recs[0].Outcome == fleet.OutcomeDead {
			t.Errorf("a single failed member marked the whole drain dead: %q", recs[0].Outcome)
		}
	})

	t.Run("a failed last member still closes and the fix runs with the incomplete panel", func(t *testing.T) {
		rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
		h := &fakeAdapter{steps: []invokeStep{
			{res: checkpointed(10, 0.10)},           // implement
			{res: checkpointed(5, 0.50)},            // review-1 records
			{res: checkpointed(5, 0.50)},            // review-2 records
			{res: held(limitResult(), openClaim())}, // review-3 is turned away, last member
			{res: checkpointed(5, 0.05)},            // fix
		}}
		h.onInvoke = func(i int) {
			switch i {
			case 1:
				rel.records = append(rel.records, plane.ReviewRecordState{
					Round: 1, ReviewerRole: "review-1", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
					Findings: []plane.ReviewFindingState{mustFinding("1.review-1.1", "open", "the seam is untested")},
				})
			case 2:
				rel.records = append(rel.records, plane.ReviewRecordState{
					Round: 1, ReviewerRole: "review-2", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
					Findings: []plane.ReviewFindingState{mustFinding("1.review-2.1", "open", "the retry path bypasses the guard")},
				})
			case 4:
				rel.status = "in_review"
				rel.outcome += "\n### Fixes\n- 1.review-1.1 fixed\n- 1.review-2.1 fixed\n"
			}
		}
		d, ff := panelDriverWithFleet(t, h, rel, panelMembers(), panelPhases())
		logs := captureLogs(t)

		if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
			t.Fatalf("drainPhases: err=%v stop=%v — a usage-limited last member must not fail the run", err, stop)
		}
		// Three reviewers run, then the panel hands over: the fix applies what the
		// two recording members found, and the third is named as incomplete.
		if h.invokeCalls != 5 {
			t.Fatalf("spawned %d sessions, want 5 — implement, three members, fix; a failed LAST member must not skip the fix", h.invokeCalls)
		}
		if len(rel.closes) != 1 || rel.closes[0] != 1 {
			t.Fatalf("closed rounds = %v, want exactly [1] — the round closes once, before the fix", rel.closes)
		}
		for _, r := range rel.records {
			if r.State == "sealed" {
				t.Errorf("record %s/%s stayed sealed after the panel closed", r.ReviewerRole, r.ID)
			}
		}
		fix := h.invocations[4].Prompt
		for _, want := range []string{"INCOMPLETE", "review-3", "hit a usage limit before recording", "1.review-1.1"} {
			if !strings.Contains(fix, want) {
				t.Errorf("the fix brief does not carry %q:\n%s", want, fix)
			}
		}
		recs := ff.records()
		if len(recs) != 1 {
			t.Fatalf("iteration records = %d, want 1", len(recs))
		}
		// The record keeps the four-word enum (CLA-738); the log names the
		// failed member.
		requireFleetOutcome(t, recs[0].Outcome, fleet.OutcomeReleased)
		if out := logs.String(); !strings.Contains(out, "review-3") || !strings.Contains(out, "review panel INCOMPLETE") {
			t.Errorf("the daemon log does not name the failed member:\n%s", out)
		}
	})

	t.Run("a member that loses the claim ends the panel as before", func(t *testing.T) {
		rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
		h := &fakeAdapter{steps: []invokeStep{
			{res: checkpointed(10, 0.10)}, // implement
			// review-1 ends by SETTLING the task itself: the claim is gone, so
			// there is no live holder to close the round and no run for the next
			// member to resume. The panel ends here, as it always has.
			{res: held(okResult(5, 0.50), harness.Claim{TaskID: "t-1", RunID: "r-1", Settled: true})},
			{res: okResult(0, 0)}, // must NEVER run
		}}
		d := panelDriver(t, h, rel, panelMembers(), panelPhases())

		if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
			t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
		}
		if h.invokeCalls != 2 {
			t.Fatalf("spawned %d sessions, want 2 — a member that loses the claim leaves no run for the next member", h.invokeCalls)
		}
		if len(rel.closes) != 0 {
			t.Errorf("closed rounds = %v, want none — a run that no longer holds the task may not close", rel.closes)
		}
	})
}

// TestDrainPhases_ReviewPanelAllFailuresEndWithoutAFix: when every member fails
// there is nothing to review and nothing for a fix to apply. The panel ends, the
// round that earlier members wrote (if any) is still released, and no fix is
// spawned.
func TestDrainPhases_ReviewPanelAllFailuresEndWithoutAFix(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)},          // implement
		{res: held(deadResult(), openClaim())}, // review-1 fails
		{res: held(deadResult(), openClaim())}, // review-2 fails
		{res: held(deadResult(), openClaim())}, // review-3 fails (the last)
		{res: okResult(0, 0)},                  // must NEVER run: no record to apply
	}}
	d, ff := panelDriverWithFleet(t, h, rel, panelMembers(), panelPhases())
	logs := captureLogs(t)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 4 {
		t.Fatalf("spawned %d sessions, want 4 — three members run and fail; no fix follows a panel that recorded nothing", h.invokeCalls)
	}
	if len(rel.closes) != 0 {
		t.Fatalf("closed rounds = %v, want none — no member recorded, so no round was ever opened", rel.closes)
	}
	// The iteration record keeps its enum (the drain ended on deaths), and the
	// panel's incompleteness — by member and reason — is in the daemon log.
	recs := ff.records()
	if len(recs) != 1 {
		t.Fatalf("iteration records = %d, want 1", len(recs))
	}
	if recs[0].Outcome != fleet.OutcomeDead {
		t.Errorf("iteration outcome = %q, want %q — every member died producing nothing", recs[0].Outcome, fleet.OutcomeDead)
	}
	out := logs.String()
	for _, want := range []string{"review-1", "review-2", "review-3", "review panel INCOMPLETE", "died producing nothing"} {
		if !strings.Contains(out, want) {
			t.Errorf("the driver never said %q about the all-failed panel:\n%s", want, out)
		}
	}
	// Every member is booked in the per-harness dead-phase tally.
	for _, member := range []string{"review-1", "review-2", "review-3"} {
		cell := d.deadTally[tallyKey{phase: member, harness: "opencode"}]
		if cell == nil || cell.dead != 1 {
			t.Errorf("dead-phase tally for %s/opencode = %+v, want 1 dead", member, cell)
		}
	}
}

// TestDrainPhases_IncompletePanelCheckpointKeepsTheFleetOutcome is CLA-738: an
// incomplete review panel is a LOG fact, never the iteration outcome. Before the
// fix the driver appended "review panel INCOMPLETE: ..." to the claim's
// disposition, so a drain that otherwise reached a checkpoint recorded
// `checkpoint — review panel INCOMPLETE: ...`; marshalReport then refused the
// WHOLE report, dropping the iteration record and the presence refresh for
// exactly the drains the operator most needs to see. The dead-final-phase
// sibling is TestDrainPhases_ReviewPanelAllFailuresEndWithoutAFix.
func TestDrainPhases_IncompletePanelCheckpointKeepsTheFleetOutcome(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	// wip is the claim a session ends holding when its work is pushed but not
	// delivered: the plane reads it as a takeover, so the drain's disposition at
	// the boundary is a checkpoint.
	wip := harness.Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true, Branch: "clanker/x"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)},                       // implement
		{res: checkpointed(5, 0.50)},                        // review-1 records
		{res: held(mcpReached(deadResult()), openClaim())},  // review-2 dies producing nothing
		{res: held(mcpReached(limitResult()), openClaim())}, // review-3 is turned away: the last member
		{res: held(mcpReached(okResult(5, 0.05)), wip)},     // fix ends still holding pushed work
	}}
	h.onInvoke = func(i int) {
		if i == 1 {
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-1", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{mustFinding("1.review-1.1", "open", "the seam is untested")},
			})
		}
	}
	d, ff := panelDriverWithFleet(t, h, rel, panelMembers(), panelPhases())
	logs := captureLogs(t)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v — a limited last member must not fail the run", err, stop)
	}
	if h.invokeCalls != 5 {
		t.Fatalf("spawned %d sessions, want 5 (implement, three members, fix) — a failed LAST member hands over to the fix; log:\n%s", h.invokeCalls, logs.String())
	}
	recs := ff.records()
	if len(recs) != 1 {
		t.Fatalf("iteration records = %d, want 1", len(recs))
	}
	// The record's one word survives the note: checkpoint, so the plane accepts
	// the report and keeps the hand-off visible.
	requireFleetOutcome(t, recs[0].Outcome, fleet.OutcomeCheckpoint)
	// The operator still sees the panel's incompleteness, by member, in the log.
	out := logs.String()
	for _, want := range []string{"review panel INCOMPLETE", "review-2", "review-3", "died producing nothing", "hit a usage limit before recording"} {
		if !strings.Contains(out, want) {
			t.Errorf("the daemon log does not carry %q:\n%s", want, out)
		}
	}
}

// TestDrainPhases_ReviewPanelMemberFailureStreakWarnsLoudly: one flaky member is
// a blip; the SAME member failing task after task is a fault in that member's
// harness, model or brief. After panelMemberFailBound consecutive tasks that
// recorded nothing, the driver says so by name — and never parks the task for
// it.
func TestDrainPhases_ReviewPanelMemberFailureStreakWarnsLoudly(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	// Three drains, each: implement, review-1 records, review-2 fails, review-3
	// records, fix.
	var steps []invokeStep
	for n := 0; n < 3; n++ {
		steps = append(steps,
			invokeStep{res: checkpointed(10, 0.10)},
			invokeStep{res: checkpointed(5, 0.50)},
			invokeStep{res: held(deadResult(), openClaim())},
			invokeStep{res: checkpointed(5, 0.50)},
			invokeStep{res: checkpointed(5, 0.05)},
		)
	}
	h := &fakeAdapter{steps: steps}
	h.onInvoke = func(i int) {
		switch i % 5 {
		case 1:
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-1", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "open",
				Findings: []plane.ReviewFindingState{mustFinding("1.review-1.1", "open", "the seam is untested")},
			})
		case 3:
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-3", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "open",
				Findings: []plane.ReviewFindingState{{ID: "1.review-3.1", Severity: "should", Status: "open", Summary: "tidy the error"}},
			})
		case 4:
			rel.status = "in_review"
		}
	}
	d, ff := panelDriverWithFleet(t, h, rel, panelMembers(), panelPhases())
	logs := captureLogs(t)

	for n := 1; n <= 3; n++ {
		if _, _, _, stop, err := drainPhasesHandoffs(t, d, n); err != nil || stop {
			t.Fatalf("drain %d: err=%v stop=%v", n, err, stop)
		}
	}
	out := logs.String()
	if !strings.Contains(out, "review-2") || !strings.Contains(out, "3 consecutive tasks") {
		t.Errorf("the third consecutive failure of the same member was not said loudly:\n%s", out)
	}
	if !strings.Contains(out, "harness opencode, model opencode-go/gpt-6-luna") {
		t.Errorf("the warning does not name the member's harness and model:\n%s", out)
	}
	// Never a park: a failing reviewer is not a reason to strand the panel's real
	// findings, and no drain ended dead over it.
	if len(rel.parks) != 0 {
		t.Errorf("a failed member parked the task: %v", rel.parks)
	}
	for _, rec := range ff.records() {
		if rec.Outcome == fleet.OutcomeParked {
			t.Errorf("a failed member parked the iteration: %+v", rec)
		}
	}
}

// TestMergedFindingsDigest: the driver's merge is mechanical, and the rules
// that matter are pinned directly — a MUST from any single member stays open,
// SHOULD/NIT carry the members that raised them, only the PANEL'S round is
// listed, and findings the plane already shows settled are not relisted as open
// work (CLA-639 S1).
func TestMergedFindingsDigest(t *testing.T) {
	records := []plane.ReviewRecordState{
		{
			Round: 1, ReviewerRole: "review-1", State: "open",
			Findings: []plane.ReviewFindingState{
				mustFinding("1.review-1.1", "open", "the seam is untested"),
				{ID: "1.review-1.2", Severity: "nit", Status: "open", Summary: "rename the temp"},
			},
		},
		{
			Round: 1, ReviewerRole: "review-2", State: "open",
			Findings: []plane.ReviewFindingState{
				{ID: "1.review-2.1", Severity: "should", Status: "open", Summary: "handle the empty input"},
			},
		},
	}
	got := mergedFindingsDigest(records, []string{"review-1", "review-2"}, 1, nil, config.ReviewToolProtocol)

	// A MUST only review-1 raised is still listed, alone in its severity.
	if !strings.Contains(got, "MUST (1):") || !strings.Contains(got, "1.review-1.1") {
		t.Errorf("the single-reviewer MUST was filtered out (A1):\n%s", got)
	}
	if !strings.Contains(got, "SHOULD (1):") || !strings.Contains(got, "raised by review-2") {
		t.Errorf("the SHOULD is not annotated with the member that raised it (B11):\n%s", got)
	}
	if !strings.Contains(got, "NIT (1):") || !strings.Contains(got, "raised by review-1") {
		t.Errorf("the NIT is not annotated with the member that raised it:\n%s", got)
	}
	if strings.Contains(got, "INCOMPLETE") {
		t.Errorf("a complete panel was reported incomplete:\n%s", got)
	}
	// M5: the fixer is told, in the driver's own words, that no block exists for
	// it to preserve and that its outcome is where the round's result lands.
	for _, want := range []string{"No panel member wrote a `## Review` block", "answer EVERY finding above in it by its plane id"} {
		if !strings.Contains(got, want) {
			t.Errorf("the merged list does not carry %q:\n%s", want, got)
		}
	}

	// A member with no record is named incomplete, so the fixer knows the list
	// is the union of the reviewers that RAN — and with its reason, when the
	// panel handler has one (M4).
	got = mergedFindingsDigest(records, []string{"review-1", "review-2", "review-3"}, 1, map[string]string{"review-3": "hit a usage limit before recording"}, config.ReviewToolProtocol)
	if !strings.Contains(got, "INCOMPLETE") || !strings.Contains(got, "review-3") {
		t.Errorf("the missing member is not named:\n%s", got)
	}
	if !strings.Contains(got, "review-3: hit a usage limit before recording") {
		t.Errorf("the missing member's reason is not carried to the fixer:\n%s", got)
	}

	// A sealed record contributes no FINDING: the merge reads only what the
	// plane released. Its member is therefore named incomplete — the panel's
	// records exist but the fixer cannot see their content — and no finding text
	// can leak.
	got = mergedFindingsDigest([]plane.ReviewRecordState{{Round: 1, ReviewerRole: "review-1", State: "sealed"}}, []string{"review-1"}, 1, nil, config.ReviewToolProtocol)
	if !strings.Contains(got, "INCOMPLETE") {
		t.Errorf("a sealed record left the panel reading complete:\n%s", got)
	}
	if strings.Contains(got, "the seam is untested") {
		t.Errorf("a sealed record's findings reached the merged list:\n%s", got)
	}

	// CLA-639 S1: only the PANEL'S round is listed. An earlier cycle's released
	// record — a member that recorded then, or a MUST a verify rewind left
	// behind — is not this round's work, and its member has not recorded THIS
	// round even though it recorded one.
	older := plane.ReviewRecordState{
		Round: 4, ReviewerRole: "review-3", RunID: "r-0", State: "open",
		Findings: []plane.ReviewFindingState{mustFinding("4.review-3.1", "open", "an earlier cycle's MUST")},
	}
	got = mergedFindingsDigest(append(append([]plane.ReviewRecordState{}, records...), older), []string{"review-1", "review-2", "review-3"}, 1, nil, config.ReviewToolProtocol)
	if strings.Contains(got, "4.review-3.1") || strings.Contains(got, "MUST (2)") {
		t.Errorf("an earlier round's finding was relisted as this round's open work:\n%s", got)
	}
	if !strings.Contains(got, "review-3") || !strings.Contains(got, "INCOMPLETE") {
		t.Errorf("a member that recorded only in an earlier round read as having recorded this one:\n%s", got)
	}
	// ... and the round filter is the round it is told, not a fixed one.
	got = mergedFindingsDigest(append(append([]plane.ReviewRecordState{}, records...), older), []string{"review-3"}, 4, nil, config.ReviewToolProtocol)
	if !strings.Contains(got, "4.review-3.1") || strings.Contains(got, "1.review-1.1") {
		t.Errorf("round 4 did not select only round 4's records:\n%s", got)
	}
	if !strings.Contains(got, "round 4's records only") {
		t.Errorf("the list does not say which round it read:\n%s", got)
	}

	// CLA-639 S1: a finding the plane already shows settled — fixed, refuted or
	// filed by the fix phase or a verifier — is not relisted as open work, and
	// saying so is cheaper than a fix round spent re-fixing it.
	settled := []plane.ReviewRecordState{{
		Round: 1, ReviewerRole: "review-1", State: "open",
		Findings: []plane.ReviewFindingState{
			mustFinding("1.review-1.1", "open", "still open"),
			mustFinding("1.review-1.2", "fixed", "already fixed"),
			mustFinding("1.review-1.3", "refuted", "already refuted"),
			mustFinding("1.review-1.4", "filed", "already dispatched"),
		},
	}}
	got = mergedFindingsDigest(settled, []string{"review-1"}, 1, nil, config.ReviewToolProtocol)
	if !strings.Contains(got, "1.review-1.1") {
		t.Errorf("the open MUST is not listed:\n%s", got)
	}
	for _, id := range []string{"1.review-1.2", "1.review-1.3", "1.review-1.4"} {
		if strings.Contains(got, id) {
			t.Errorf("the settled finding %s was relisted as open work:\n%s", id, got)
		}
	}
	if !strings.Contains(got, "3 finding(s) of this round are already MARKED settled") {
		t.Errorf("the digest does not say it left settled findings out:\n%s", got)
	}
	if !strings.Contains(got, "MUST (1):") {
		t.Errorf("the settled MUSTs were counted in the open group:\n%s", got)
	}
	// A record whose every finding is settled still means its member RECORDED:
	// the member is not named incomplete (it ran and it wrote), and the digest
	// says what it left out rather than reading as an empty round.
	got = mergedFindingsDigest([]plane.ReviewRecordState{{
		Round: 1, ReviewerRole: "review-1", State: "open",
		Findings: []plane.ReviewFindingState{mustFinding("1.review-1.1", "fixed", "already fixed")},
	}}, []string{"review-1"}, 1, nil, config.ReviewToolProtocol)
	if strings.Contains(got, "INCOMPLETE") {
		t.Errorf("a member whose findings are all settled read as not having recorded:\n%s", got)
	}
	if !strings.Contains(got, "already MARKED settled") {
		t.Errorf("the digest does not admit what it left out:\n%s", got)
	}
}

// TestDrainPhases_ReviewPanelMembersWriteOnlyTheirRecord (CLA-639 M5): a panel
// member never writes the task's outcome. Its brief says so, and the plane
// agrees — the legacy write is refused the moment the member's own record seals
// the round — so what member 2 can read carries no member-1 findings anywhere:
// not in the outcome, not in the reviews block (a sealed record serves no
// findings).
func TestDrainPhases_ReviewPanelMembersWriteOnlyTheirRecord(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)}, // implement
		{res: checkpointed(5, 0.50)},  // review-1 records
		{res: checkpointed(5, 0.50)},  // review-2 records
		{res: checkpointed(5, 0.05)},  // fix
	}}
	var (
		legacyErr  error
		member2Saw plane.TaskState
	)
	h.onInvoke = func(i int) {
		switch i {
		case 1:
			// The member records, in the one form its brief prescribes. The
			// record is what SEALS the round, and from that moment this run's
			// outcome writes are illegal on the plane — which is why the member
			// must not be told to write one (the old brief put the legacy block
			// FIRST, and that block is exactly what member 2 used to read). The
			// write is attempted here to show the plane refusing it.
			if err := rel.writeReviewRecord(plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-1", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{mustFinding("1.review-1.1", "open", "the seam is untested")},
			}); err != nil {
				t.Errorf("review-1's own record was refused: %v", err)
			}
			legacyErr = rel.writeOutcome("r-1", rel.outcome+"\n"+config.ReviewRecordHeading+"\n- verdict: findings\n- R1 [must] the seam is untested\n")
		case 2:
			// What member 2 actually reads: get_task, exactly as the session does.
			st, _ := rel.TaskState(context.Background(), "t-1")
			member2Saw = st
			if err := rel.writeReviewRecord(plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-2", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{{ID: "1.review-2.1", Severity: "should", Status: "open", Summary: "handle the empty input"}},
			}); err != nil {
				t.Errorf("review-2's own record was refused: %v", err)
			}
		case 3:
			rel.status = "in_review"
			rel.outcome += "\n### Fixes\n- 1.review-1.1 fixed\n"
		}
	}
	d := panelDriver(t, h, rel, panelMembers()[:2], panelPhases())

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 4 {
		t.Fatalf("spawned %d sessions, want 4 — implement, two members, fix", h.invokeCalls)
	}

	// The BRIEF: no outcome-block instruction for any member, and the record the
	// seam gates on is the durable one.
	for i := 1; i <= 2; i++ {
		p := h.invocations[i].Prompt
		for _, banned := range []string{
			"update_task(taskId, runId, outcome: ...)",
			config.ReviewRecordHeading,
			"Record the LEGACY block",
		} {
			if strings.Contains(p, banned) {
				t.Errorf("panel member %d's brief instructs %q, which it must not write:\n%s", i, banned, p)
			}
		}
		for _, want := range []string{"update_task(taskId, runId, review: {...})", "Do NOT write the task's outcome"} {
			if !strings.Contains(p, want) {
				t.Errorf("panel member %d's brief does not say %q:\n%s", i, want, p)
			}
		}
	}
	// The FIX is told the truth about the shape it inherits, and its own outcome
	// is where the panel's round reaches the task.
	fix := h.invocations[3].Prompt
	for _, want := range []string{
		"No panel member wrote a `## Review` block",
		"answer EVERY finding above in it by its plane id",
		"1.review-1.1",
	} {
		if !strings.Contains(fix, want) {
			t.Errorf("the fix brief does not carry %q:\n%s", want, fix)
		}
	}

	// And what member 2 could SEE while it ran: nothing of member 1's.
	if legacyErr == nil {
		t.Error("the fake plane accepted a legacy outcome write after the member's record sealed the round; the seal is what makes M5 a leak, not only a style choice")
	}
	if strings.Contains(member2Saw.Outcome, config.ReviewRecordHeading) || strings.Contains(member2Saw.Outcome, "the seam is untested") {
		t.Errorf("member 2's readable task outcome carries member 1's findings: %q", member2Saw.Outcome)
	}
	if len(member2Saw.Reviews) != 1 || member2Saw.Reviews[0].ReviewerRole != "review-1" {
		t.Fatalf("member 2's readable records = %+v, want member 1's record alone", member2Saw.Reviews)
	}
	if len(member2Saw.Reviews[0].Findings) != 0 {
		t.Errorf("member 2 could read member 1's findings through the reviews block: %+v", member2Saw.Reviews[0].Findings)
	}
	if member2Saw.Reviews[0].State != "sealed" {
		t.Errorf("member 1's record was not sealed while member 2 ran: %q", member2Saw.Reviews[0].State)
	}
}

// TestDrainPhases_ReviewPanelOnABranchlessTaskRunsOneReviewer (CLA-639 M5): a
// branchless task's review record is the legacy `## Review` block — the plane
// takes no durable record from a settled task — so a panel cannot run that way
// at all. ONE reviewer runs, with the brief it would have had anyway, the
// members after it are skipped, and the driver says so.
func TestDrainPhases_ReviewPanelOnABranchlessTaskRunsOneReviewer(t *testing.T) {
	settled := plane.TaskState{Status: "in_review", Outcome: "no-code delivery: the task body was updated\n"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: held(okResult(1, 0), openClaim())}, // implement: no branch, the plane's record is the evidence
		{res: held(okResult(1, 0), openClaim())}, // review-1: appends its block and holds
		{res: okResult(1, 0)},                    // the fix
	}}
	rel := &peekReleaser{state: map[string]plane.TaskState{"t-1": settled}}
	h.onInvoke = func(i int) {
		if i == 1 {
			st := rel.state["t-1"]
			st.Outcome += "\n" + config.ReviewRecordHeading + "\n- reviewed tip: none (no branch - the plane record was reviewed)\n- verdict: pass\n"
			rel.state["t-1"] = st
		}
	}
	d := panelDriver(t, h, rel, panelMembers(), panelPhases())
	logs := captureLogs(t)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	// ONE reviewer, not three: implement, the first member, the fix.
	if h.invokeCalls != 3 {
		t.Fatalf("spawned %d sessions, want 3 — a branchless task runs ONE reviewer, not the panel", h.invokeCalls)
	}
	if !strings.Contains(h.invocations[1].Prompt, "There is NO branch, because this task's correct delivery is no code") {
		t.Errorf("the single reviewer did not get today's branchless brief:\n%s", h.invocations[1].Prompt)
	}
	if strings.Contains(h.invocations[1].Prompt, "ONE MEMBER of a review PANEL") {
		t.Errorf("the branchless reviewer got the panel's durable-record brief, whose record this shape cannot write:\n%s", h.invocations[1].Prompt)
	}
	if len(rel.closes) != 0 {
		t.Errorf("closed rounds = %v, want none — the branchless shape writes no durable round", rel.closes)
	}
	out := logs.String()
	if !strings.Contains(out, "BRANCHLESS") || !strings.Contains(out, "runs ONE reviewer") {
		t.Errorf("the driver did not say the branchless panel runs one reviewer:\n%s", out)
	}
}

// TestDrainPhases_ReviewPanelFixReadsOnlyThePanelsRound (CLA-639 S1): the fix's
// merged list is the panel's own round. A released record from an earlier cycle
// — another run's round, a round a verify rewind left behind — is not this
// round's work, and it must not reach the fixer as an open MUST.
func TestDrainPhases_ReviewPanelFixReadsOnlyThePanelsRound(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	// An older cycle's released record, from a DIFFERENT run (so the panel's own
	// seam cannot mistake it for this trip's record).
	rel.records = append(rel.records, plane.ReviewRecordState{
		Round: 4, ReviewerRole: "review-2", RunID: "r-0", ReviewedTip: testCheckpointTip, State: "open",
		Findings: []plane.ReviewFindingState{mustFinding("4.review-2.1", "open", "an earlier cycle's MUST")},
	})
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)}, // implement
		{res: checkpointed(5, 0.50)},  // review-1 records
		{res: checkpointed(5, 0.50)},  // review-2 records
		{res: checkpointed(5, 0.05)},  // fix
	}}
	h.onInvoke = func(i int) {
		switch i {
		case 1:
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-1", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{mustFinding("1.review-1.1", "open", "this round's MUST")},
			})
		case 2:
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-2", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{{ID: "1.review-2.1", Severity: "should", Status: "open", Summary: "this round's SHOULD"}},
			})
		case 3:
			rel.status = "in_review"
			rel.outcome += "\n### Fixes\n- 1.review-1.1 fixed\n"
		}
	}
	d := panelDriver(t, h, rel, panelMembers()[:2], panelPhases())

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	fix := h.invocations[3].Prompt
	if !strings.Contains(fix, "1.review-1.1") || !strings.Contains(fix, "1.review-2.1") {
		t.Errorf("the fix brief does not carry this round's findings:\n%s", fix)
	}
	if strings.Contains(fix, "4.review-2.1") {
		t.Errorf("the fix brief relists an earlier round's MUST as open work:\n%s", fix)
	}
	if !strings.Contains(fix, "The list is round 1's records only") {
		t.Errorf("the fix brief does not say which round it read:\n%s", fix)
	}
	if strings.Contains(fix, "INCOMPLETE") {
		t.Errorf("a member that recorded in THIS round was named incomplete:\n%s", fix)
	}
}

// TestDrainPhases_ReviewPanelPrevAttributionIsTheImplementer (CLA-639 S3): the
// review briefs name {{prevHarness}}/{{prevModel}} as the IMPLEMENTER whose work
// is being judged. Inside a panel every member's predecessor is another member,
// so the walk-back past the panel is what keeps member 2 from naming member 1 as
// the implementer. Removing it makes this test fail.
func TestDrainPhases_ReviewPanelPrevAttributionIsTheImplementer(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)}, // implement (claude, tier strong)
		{res: checkpointed(5, 0.50)},  // review-1 (opencode)
		{res: checkpointed(5, 0.50)},  // review-2 (opencode)
		{res: checkpointed(5, 0.05)},  // fix
	}}
	h.onInvoke = func(i int) {
		switch i {
		case 1, 2:
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-" + strconv.Itoa(i), RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{},
			})
		case 3:
			rel.status = "in_review"
		}
	}
	phases := []config.Phase{
		{Name: config.ImplementPhaseName, Tier: "strong"},
		{Name: config.ReviewPhaseName},
		{Name: config.FixPhaseName},
	}
	d := panelDriver(t, h, rel, panelMembers()[:2], phases)
	d.cfg.Models = map[string]string{"strong": "opus-test"}

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	// Member 1's predecessor IS the implement phase.
	if p := h.invocations[1].Prompt; !strings.Contains(p, "The implement phase (harness claude, model opus-test)") {
		t.Errorf("member 1's brief does not name the implement phase:\n%s", p)
	}
	// Member 2's is still the implement phase, never member 1.
	p2 := h.invocations[2].Prompt
	if !strings.Contains(p2, "The implement phase (harness claude, model opus-test)") {
		t.Errorf("member 2's brief does not name the implement phase — the walk-back past the panel is gone:\n%s", p2)
	}
	if strings.Contains(p2, "model opencode-go/gpt-6-luna") || strings.Contains(p2, "harness opencode") {
		t.Errorf("member 2's brief names another member as the implementer:\n%s", p2)
	}
}

// TestDrainPhases_ReviewPanelMemberHandoffIsRefused (CLA-639 S3): a panel member
// that emits a handoff block is not respawned. Its successor would continue the
// same reviewer ROLE while the phase after it is the next MEMBER, and the
// member's record is its deliverable; the driver says so and moves on. Removing
// the suppression makes this test fail — the member's handoff respawns.
func TestDrainPhases_ReviewPanelMemberHandoffIsRefused(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)},                            // implement
		{res: handoffResult("Resume: finish judging the diff.")}, // review-1: clean exit + a handoff block
		{res: checkpointed(5, 0.50)},                             // review-2
		{res: checkpointed(5, 0.05)},                             // fix
	}}
	h.onInvoke = func(i int) {
		switch i {
		case 1, 2:
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-" + strconv.Itoa(i), RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
			})
		case 3:
			rel.status = "in_review"
		}
	}
	d := panelDriver(t, h, rel, panelMembers()[:2], panelPhases())
	logs := captureLogs(t)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 4 {
		t.Fatalf("spawned %d sessions, want 4 — a member's handoff must not respawn it (implement, two members, fix)", h.invokeCalls)
	}
	if p := h.invocations[2].Prompt; !strings.Contains(p, `reviewerRole: "review-2"`) {
		t.Errorf("the session after the handing-off member is not the next member:\n%s", p)
	}
	out := logs.String()
	if !strings.Contains(out, "panel member emitted a handoff block; panel members do not chain") {
		t.Errorf("the driver did not say the member's handoff was refused:\n%s", out)
	}
}

// TestReviewReleaser_EnforcesThePlanesWriteRules (CLA-639 S4): the panel tests'
// fake is only as good as the rules it enforces. These are the three the failure
// arms depend on — a close by anyone but the live holder, an outcome write by a
// run whose record is sealed, and any write or close of a round a takeover
// voided.
func TestReviewReleaser_EnforcesThePlanesWriteRules(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented\n"}
	rec := plane.ReviewRecordState{
		Round: 1, ReviewerRole: "review-1", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
		Findings: []plane.ReviewFindingState{mustFinding("1.review-1.1", "open", "the seam is untested")},
	}
	if err := rel.writeReviewRecord(rec); err != nil {
		t.Fatalf("the member's own record was refused: %v", err)
	}
	// A duplicate role in the open round is the plane's idempotency signal, not
	// an error the driver must route around.
	if err := rel.writeReviewRecord(rec); err == nil || !strings.Contains(err.Error(), "duplicate_reviewer") {
		t.Errorf("a duplicate role was accepted: %v", err)
	}
	// The seal: this run's outcome writes are refused while its record is sealed.
	if err := rel.writeOutcome("r-1", "a legacy block\n"); err == nil || !strings.Contains(err.Error(), "review_sealed") {
		t.Errorf("a sealed run's outcome write was accepted: %v", err)
	}
	// Another run may not write either, and the raw field is untouched.
	if err := rel.writeOutcome("r-2", "somebody else's outcome\n"); err == nil {
		t.Errorf("a run that does not hold the task wrote to it: %v", err)
	}
	if rel.outcome != "implemented\n" {
		t.Errorf("an outcome write that should have been refused landed: %q", rel.outcome)
	}
	// Only the live holder may close the round.
	if err := rel.CloseReviewRound(context.Background(), "t-1", "r-2", 1, false); err == nil || !strings.Contains(err.Error(), "not the live holder") {
		t.Errorf("a non-holder closed the round: %v", err)
	}
	if err := rel.CloseReviewRound(context.Background(), "t-1", "r-1", 1, false); err != nil {
		t.Fatalf("the live holder could not close the round: %v", err)
	}
	// Which is what releases the findings to a reader.
	if st, _ := rel.TaskState(context.Background(), "t-1"); len(st.Reviews[0].Findings) != 1 {
		t.Errorf("the close did not release the round's findings: %+v", st.Reviews)
	}
	// ... and a second close has nothing to release.
	if err := rel.CloseReviewRound(context.Background(), "t-1", "r-1", 1, false); err == nil || !strings.Contains(err.Error(), "no_open_round") {
		t.Errorf("a second close of the same round was accepted: %v", err)
	}

	// A takeover: another run becomes the holder and every OPEN round is voided,
	// so the displaced run can neither write nor close.
	rel2 := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented\n"}
	if err := rel2.writeReviewRecord(rec); err != nil {
		t.Fatalf("writeReviewRecord: %v", err)
	}
	rel2.takeover("r-9")
	if len(rel2.records) != 1 || rel2.records[0].State != "void" {
		t.Fatalf("the takeover did not void the round's records: %+v", rel2.records)
	}
	if err := rel2.writeOutcome("r-1", "a displaced run's outcome\n"); err == nil || !strings.Contains(err.Error(), "round_voided") {
		t.Errorf("a displaced run wrote to the task: %v", err)
	}
	if err := rel2.CloseReviewRound(context.Background(), "t-1", "r-1", 1, false); err == nil || !strings.Contains(err.Error(), "void") {
		t.Errorf("a displaced run closed the round: %v", err)
	}
}

// TestDrainPhases_ReviewPanelBeforeAVerificationRound: the panel is upstream of
// the whole stopping rule. Two members write one round, the panel's last member
// closes it, the fix runs from the merged list, and the verifier adjudicates the
// same round afterwards — no second panel and no unclosed round in between.
func TestDrainPhases_ReviewPanelBeforeAVerificationRound(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)}, // implement
		{res: checkpointed(5, 0.50)},  // review-1
		{res: checkpointed(5, 0.50)},  // review-2 (last member, closes round 1)
		{res: checkpointed(5, 0.05)},  // fix
		{res: checkpointed(5, 0.05)},  // verify (round 2, no open MUST)
	}}
	h.onInvoke = func(i int) {
		switch i {
		case 1:
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-1", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{mustFinding("1.review-1.1", "open", "the seam is untested")},
			})
		case 2:
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-2", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{{ID: "1.review-2.1", Severity: "should", Status: "open", Summary: "tidy the error"}},
			})
			rel.openMusts = []string{"1.review-1.1"}
		case 4:
			// The verifier writes round 2 SEALED and leaves the close to the driver,
			// the variant this fixture's steps exercise. Its role is the next
			// review-N after the two panel members.
			rel.records = append(rel.records, recordFor(2, "review-3", "r-1", testCheckpointTip, "sealed"))
			rel.counted = 2
			rel.openMusts = nil
			rel.status = "in_review"
		}
	}
	phases := append(panelPhases(), config.Phase{Name: config.VerifyPhaseName})
	d := panelDriver(t, h, rel, panelMembers()[:2], phases)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 5 {
		t.Fatalf("spawned %d sessions, want 5 — implement, two reviewers, fix, verify", h.invokeCalls)
	}
	if len(rel.closes) != 2 || rel.closes[0] != 1 || rel.closes[1] != 2 {
		t.Fatalf("closed rounds = %v, want [1 2] — the panel closes its round before the fix, the verifier its own", rel.closes)
	}
	if len(rel.parks) != 0 {
		t.Fatalf("a clean panel + verification parked the task: %v", rel.parks)
	}
	if !strings.Contains(h.invocations[4].Prompt, "1.review-1.1") {
		t.Errorf("the verifier was not handed the panel's MUST to adjudicate:\n%s", h.invocations[4].Prompt)
	}
}

// TestDrainPhases_NoPanelIsTheLegacySingleReviewer (CLA-639, decision
// a0b1373b): a config that sets NO review_panel — the default, and the shape
// every config had before this feature — runs exactly one review phase, on the
// harness/model the config gives it, with the historical brief and the
// historical `review-1` role, and the fix gets the legacy block grading, not
// the panel's merged list. This is the fixture-level proof the driver reads
// nothing panel-shaped when the key is absent.
func TestDrainPhases_NoPanelIsTheLegacySingleReviewer(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)}, // implement
		{res: checkpointed(5, 0.50)},  // review: ONE reviewer, the legacy record
		{res: okResult(5, 0.05)},      // fix
	}}
	h.onInvoke = func(i int) {
		switch i {
		case 1:
			// The single reviewer writes the legacy `## Review` block the shipped
			// brief prescribes (its durable record alongside it is the CLA-637
			// half; the fix's grading below reads the block).
			rel.outcome += "\n" + config.ReviewRecordHeading + "\n- reviewed tip: " + testCheckpointTip +
				"\n- verdict: findings\n\n### Findings\n- R1 [must] (test): it is untested. internal/x.go:1 - the seam. Required fix: add a test.\n"
			rel.records = append(rel.records, recordFor(1, "review-1", "r-1", testCheckpointTip, "sealed"))
		case 2:
			if len(rel.closes) != 1 {
				t.Errorf("the fix spawned with the review round not yet closed (closes=%v)", rel.closes)
			}
			rel.status = "in_review"
		}
	}
	// panelDriver's member list is nil: the config's ReviewPanel keeps its zero
	// value, which is exactly what "the key is absent" unmarshals to.
	d := panelDriver(t, h, rel, nil, panelPhases())

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 3 {
		t.Fatalf("spawned %d sessions, want 3 — one review phase, not a panel (implement, review, fix)", h.invokeCalls)
	}
	// The single reviewer: the historical review-only brief, its own role, its
	// own harness/model — and no panel context anywhere.
	p := h.invocations[1].Prompt
	for _, want := range []string{"REVIEW-ONLY gate", `reviewerRole: "review-1"`, `harness: "claude"`} {
		if !strings.Contains(p, want) {
			t.Errorf("the single reviewer's brief does not carry %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "ONE MEMBER of a review PANEL") {
		t.Errorf("a config with no review_panel got the panel member's brief:\n%s", p)
	}
	// The fix gets the LEGACY grading: reviewFindingsDigest's own preamble, and
	// none of the merged list's panel-shaped prose.
	fix := h.invocations[2].Prompt
	if !strings.Contains(fix, "The driver read the review record and graded its findings structurally") ||
		!strings.Contains(fix, "R1 [MUST]") {
		t.Errorf("the fix did not receive the legacy review-record grading:\n%s", fix)
	}
	for _, banned := range []string{"This is the MERGED list from every member that recorded", "No panel member wrote a `## Review` block"} {
		if strings.Contains(fix, banned) {
			t.Errorf("the fix of a single-review config carries panel prose %q:\n%s", banned, fix)
		}
	}
	if len(rel.closes) != 1 || rel.closes[0] != 1 {
		t.Errorf("closed rounds = %v, want exactly [1] — the single reviewer's round is closed before the fix", rel.closes)
	}
}

// TestDrainPhases_ReviewPanelCloseFailureEndsWithoutAFix (CLA-639 fix pass 3):
// a round the driver cannot close must end the drain, not spawn the fix against
// findings the plane still holds sealed. The single-review seam has always
// failed closed here; the panel's last member must too.
//
// Two shapes: the last member RECORDED and the seam's own close failed (the
// member is not INCOMPLETE — it recorded; the plane failed to release), and the
// last member was usage-limited so the panel's failure arm attempted the close
// and it failed. Neither may spawn the fix.
func TestDrainPhases_ReviewPanelCloseFailureEndsWithoutAFix(t *testing.T) {
	closeErr := errors.New("the plane is unreachable")

	t.Run("the last member recorded", func(t *testing.T) {
		rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n", closeErr: closeErr}
		h := &fakeAdapter{steps: []invokeStep{
			{res: checkpointed(10, 0.10)}, // implement
			{res: checkpointed(5, 0.50)},  // review-1 records
			{res: checkpointed(5, 0.50)},  // review-2 records (last member; the seam's close fails)
		}}
		h.onInvoke = func(i int) {
			if i == 1 || i == 2 {
				role := "review-" + strconv.Itoa(i)
				rel.records = append(rel.records, plane.ReviewRecordState{
					Round: 1, ReviewerRole: role, RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
					Findings: []plane.ReviewFindingState{mustFinding("1."+role+".1", "open", "a "+role+" MUST")},
				})
			}
		}
		d, ff := panelDriverWithFleet(t, h, rel, panelMembers()[:2], panelPhases())
		logs := captureLogs(t)

		if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
			t.Fatalf("drainPhases: err=%v stop=%v — a close failure must end the drain, not fail the run", err, stop)
		}
		if h.invokeCalls != 3 {
			t.Fatalf("spawned %d sessions, want 3 (implement, two members, NO fix) — the fix must not run against a sealed round", h.invokeCalls)
		}
		if len(rel.closes) != 0 {
			t.Errorf("a close was recorded despite the injected failure: %v", rel.closes)
		}
		out := logs.String()
		if !strings.Contains(out, "could not be closed") || !strings.Contains(out, "without spawning the fix") {
			t.Errorf("the driver did not say why the fix was skipped:\n%s", out)
		}
		// The member that RECORDED is not named incomplete and is not charged a
		// failure streak: the record exists; the plane failed to release it.
		recs := ff.records()
		if len(recs) != 1 {
			t.Fatalf("iteration records = %d, want 1", len(recs))
		}
		requireFleetOutcome(t, recs[0].Outcome, "")
		if strings.Contains(out, "INCOMPLETE") {
			t.Errorf("a member that recorded was reported incomplete:\n%s", out)
		}
		for _, role := range []string{"review-1", "review-2"} {
			if n := d.panelFailStreak[role]; n != 0 {
				t.Errorf("panelFailStreak[%s] = %d, want 0 — the member recorded", role, n)
			}
			if cell := d.deadTally[tallyKey{phase: role, harness: "opencode"}]; cell != nil && cell.dead != 0 {
				t.Errorf("the recorded member %s was booked dead: %+v — it produced a record; the close is the plane's failure", role, cell)
			}
		}
		// Nothing reports success: the drain ended on a released claim, and no
		// round was released for a reader.
		if recs[0].Outcome == fleet.OutcomeCheckpoint {
			t.Errorf("the drain reported a checkpoint after the panel's round could not be closed: %q", recs[0].Outcome)
		}
		for _, r := range rel.records {
			if r.State != "sealed" {
				t.Errorf("a record was released despite the failed close: %+v", r)
			}
		}
	})

	t.Run("the last member is usage-limited", func(t *testing.T) {
		rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n", closeErr: closeErr}
		h := &fakeAdapter{
			steps: []invokeStep{
				{res: checkpointed(10, 0.10)},           // implement
				{res: checkpointed(5, 0.50)},            // review-1 records
				{res: checkpointed(5, 0.50)},            // review-2 records
				{res: held(limitResult(), openClaim())}, // review-3 is turned away, last member
			},
			// A reset already past, so the run-level wait resumes at once and
			// this test stays about the panel's close.
			limitResetAt: time.Now().Add(-time.Hour),
		}
		h.onInvoke = func(i int) {
			if i == 1 || i == 2 {
				role := "review-" + strconv.Itoa(i)
				rel.records = append(rel.records, plane.ReviewRecordState{
					Round: 1, ReviewerRole: role, RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
					Findings: []plane.ReviewFindingState{mustFinding("1."+role+".1", "open", "a "+role+" MUST")},
				})
			}
		}
		d, ff := panelDriverWithFleet(t, h, rel, panelMembers(), panelPhases())
		logs := captureLogs(t)

		if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
			t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
		}
		if h.invokeCalls != 4 {
			t.Fatalf("spawned %d sessions, want 4 (implement, three members, NO fix) — the close failed and the fix must not run", h.invokeCalls)
		}
		out := logs.String()
		if !strings.Contains(out, "could not be closed") || !strings.Contains(out, "without spawning the fix") {
			t.Errorf("the driver did not say why the fix was skipped:\n%s", out)
		}
		recs := ff.records()
		if len(recs) != 1 {
			t.Fatalf("iteration records = %d, want 1", len(recs))
		}
		// The usage-limited member is the one named incomplete in the LOG; the
		// iteration record itself keeps the four-word enum (CLA-738) — free text
		// there made the plane refuse the whole report.
		requireFleetOutcome(t, recs[0].Outcome, fleet.OutcomeReleased)
		for _, want := range []string{"review panel INCOMPLETE", "review-3", "hit a usage limit before recording"} {
			if !strings.Contains(out, want) {
				t.Errorf("the daemon log does not carry %q:\n%s", want, out)
			}
		}
		for _, role := range []string{"review-1:", "review-2:"} {
			if strings.Contains(out, role) {
				t.Errorf("a member that recorded was named incomplete:\n%s", out)
			}
			short := strings.TrimSuffix(role, ":")
			if n := d.panelFailStreak[short]; n != 0 {
				t.Errorf("panelFailStreak[%s] = %d, want 0 — the member recorded", short, n)
			}
		}
		if n := d.panelFailStreak["review-3"]; n != 1 {
			t.Errorf("panelFailStreak[review-3] = %d, want 1 — it recorded nothing", n)
		}
	})
}

// TestDrainPhases_BranchlessPanelWithNoRecordEndsWithoutAFix (CLA-639 fix pass
// 3): a branchless task runs the panel as ONE reviewer, so that reviewer is the
// panel's last member. When it records nothing, no fix may run — the same end a
// config with no panel reaches. Before the fix, the lone member was "not last"
// (another member followed in the effective sequence) and the panel handed over
// to the fix anyway.
func TestDrainPhases_BranchlessPanelWithNoRecordEndsWithoutAFix(t *testing.T) {
	settled := plane.TaskState{Status: "in_review", Outcome: "no-code delivery: the task body was updated\n"}
	run := func(t *testing.T, members []config.ReviewPanelMember) (*fakeAdapter, bool, error) {
		t.Helper()
		h := &fakeAdapter{steps: []invokeStep{
			{res: held(okResult(1, 0), openClaim())},             // implement: no branch (the plane's record is the evidence)
			{res: mcpReached(held(okResult(1, 0), openClaim()))}, // the only reviewer: reached the backlog, records NOTHING and holds
			{res: okResult(1, 0)},                                // would be the fix
			{res: okResult(1, 0)},
		}}
		rel := &peekReleaser{state: map[string]plane.TaskState{"t-1": settled}}
		d := panelDriver(t, h, rel, members, panelPhases())
		_, _, stop, err := drainPhasesOnce(t, d)
		return h, stop, err
	}

	panel, panelStop, panelErr := run(t, panelMembers())
	legacy, legacyStop, legacyErr := run(t, nil)

	if panelErr != nil || panelStop {
		t.Fatalf("the panel drain: err=%v stop=%v, want a clean end", panelErr, panelStop)
	}
	if legacyErr != nil || legacyStop {
		t.Fatalf("the no-panel drain: err=%v stop=%v, want a clean end", legacyErr, legacyStop)
	}
	if panel.invokeCalls != 2 {
		t.Fatalf("the branchless panel spawned %d sessions, want 2 (implement + ONE reviewer, no fix)", panel.invokeCalls)
	}
	if legacy.invokeCalls != 2 {
		t.Fatalf("the no-panel drain spawned %d sessions, want 2 (implement + review)", legacy.invokeCalls)
	}
}

// TestDrainPhases_ReviewPanelNamedMembersKeepPositionalRoles (CLA-639 fix pass
// 3): a member's `name` is a log label; its reviewer role is positional
// (review-1..review-N). A named panel must record and merge by role exactly as
// an unnamed one does. Before the fix, panelRoles matched records by label, so
// alpha/beta were reported INCOMPLETE while review-1/review-2 sat in the round.
func TestDrainPhases_ReviewPanelNamedMembersKeepPositionalRoles(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)}, // implement
		{res: checkpointed(5, 0.50)},  // alpha (role review-1) records
		{res: checkpointed(5, 0.50)},  // beta (role review-2) records
		{res: checkpointed(5, 0.05)},  // fix
	}}
	h.onInvoke = func(i int) {
		switch i {
		case 1:
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-1", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{mustFinding("1.review-1.1", "open", "alpha's MUST")},
			})
		case 2:
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-2", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{{ID: "1.review-2.1", Severity: "should", Status: "open", Summary: "beta's SHOULD"}},
			})
		case 3:
			rel.status = "in_review"
			rel.outcome += "\n### Fixes\n- 1.review-1.1 fixed\n"
		}
	}
	members := []config.ReviewPanelMember{
		{Name: "alpha", Harness: "opencode", Model: "opencode-go/gpt-6-luna"},
		{Name: "beta", Harness: "opencode", Model: "opencode-go/glm-5.3-flash"},
	}
	d := panelDriver(t, h, rel, members, panelPhases())

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 4 {
		t.Fatalf("spawned %d sessions, want 4 (implement, alpha, beta, fix)", h.invokeCalls)
	}
	// The briefs name the POSITIONAL role, never the label.
	for i, want := range []string{`reviewerRole: "review-1"`, `reviewerRole: "review-2"`} {
		if p := h.invocations[i+1].Prompt; !strings.Contains(p, want) {
			t.Errorf("member %d's brief does not name %s:\n%s", i+1, want, p)
		}
	}
	// Both records reach the fix under their roles, and no member is incomplete.
	fix := h.invocations[3].Prompt
	for _, want := range []string{"1.review-1.1", "1.review-2.1", "raised by review-2"} {
		if !strings.Contains(fix, want) {
			t.Errorf("the fix brief does not carry %q — the roles are not positional:\n%s", want, fix)
		}
	}
	if strings.Contains(fix, "INCOMPLETE") {
		t.Errorf("a member that recorded under its positional role was reported incomplete:\n%s", fix)
	}
}

// TestDrainPhases_ReviewPanelVerifyRewindUsesTheVerifiersGrading (CLA-639 fix
// pass 3): the merged panel list belongs to the FIRST fix after the panel. A
// fix spawned by a verify rewind is a later round: its grading must be the
// verifier's own record (the legacy block plus the plane's open-MUST list), not
// the panel's stale merged list again.
func TestDrainPhases_ReviewPanelVerifyRewindUsesTheVerifiersGrading(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)}, // implement
		{res: checkpointed(5, 0.50)},  // review-1 records a MUST and a SHOULD
		{res: checkpointed(5, 0.50)},  // review-2 records (last member, closes round 1)
		{res: checkpointed(5, 0.05)},  // fix 1 (the panel's merged list)
		{res: checkpointed(5, 0.05)},  // verify 1: an open MUST -> rewind
		{res: checkpointed(5, 0.05)},  // fix 2 (the brief under test)
		{res: checkpointed(5, 0.05)},  // verify 2: clean
	}}
	h.onInvoke = func(i int) {
		switch i {
		case 1:
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-1", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{
					mustFinding("1.review-1.1", "open", "panel-only must finding"),
					{ID: "1.review-1.2", Severity: "should", Status: "open", Summary: "panel-only should finding"},
				},
			})
		case 2:
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-2", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
			})
			rel.openMusts = []string{"1.review-1.1"}
			rel.counted = 1
		case 3: // fix 1 applies the merged list
			rel.records[0].Findings[0].Status = "fixed"
			rel.openMusts = nil
			rel.outcome += "\n### Fixes\n- 1.review-1.1 fixed\n"
			rel.status = "in_review"
		case 4: // verify 1: its own block and record, with a NEW open MUST
			rel.outcome += "\n" + config.ReviewRecordHeading + "\n- reviewed tip: " + testCheckpointTip +
				"\n- verdict: findings\n\n### Findings\n- R1 [should] (gate): verifier-should-text internal/x.go:1 - fix it. Required fix: do it.\n" +
				"- R2 [nit] verifier-nit-text internal/x.go:2 - tidy it. Required fix: tidy.\n"
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 2, ReviewerRole: "review-3", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{mustFinding("2.review-3.1", "open", "verifier's new MUST")},
			})
			rel.openMusts = []string{"2.review-3.1"}
			rel.counted = 2
		case 5: // fix 2 answers the verifier's MUST
			for n := range rel.records {
				if rel.records[n].Round == 2 {
					rel.records[n].Findings[0].Status = "fixed"
				}
			}
			rel.openMusts = nil
			rel.outcome += "\n### Fixes\n- 2.review-3.1 fixed\n"
		case 6: // verify 2: clean, carries the terminal step
			rel.records = append(rel.records, recordFor(3, "review-4", "r-1", testCheckpointTip, "sealed"))
			rel.counted = 3
			rel.openMusts = nil
			rel.status = "in_review"
		}
	}
	d := panelDriver(t, h, rel, panelMembers()[:2], append(panelPhases(), config.Phase{Name: config.VerifyPhaseName}))

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 7 {
		t.Fatalf("spawned %d sessions, want 7 (implement, 2 members, fix, verify, fix, verify)", h.invokeCalls)
	}
	if first := h.invocations[3].Prompt; !strings.Contains(first, "This is the MERGED list from every member that recorded") {
		t.Fatalf("the first fix did not receive the panel's merged list:\n%s", first)
	}
	second := h.invocations[5].Prompt
	for _, want := range []string{"verifier-should-text", "verifier-nit-text", "R1 [SHOULD]", "R2 [NIT]"} {
		if !strings.Contains(second, want) {
			t.Errorf("the second fix brief does not carry the verifier's finding %q:\n%s", want, second)
		}
	}
	for _, banned := range []string{"This is the MERGED list from every member that recorded", "panel-only should finding", "panel-only must finding"} {
		if strings.Contains(second, banned) {
			t.Errorf("the second fix brief repeats the panel's stale merged list (%q):\n%s", banned, second)
		}
	}
}

// TestDrainPhases_ReviewPanelMemberLimitedAfterRecordingIsRecorded (CLA-639 fix
// pass 4): a member that appends its durable record and is THEN turned away by a
// usage limit is not a failure. The seam never judged the record (the resumed
// phase's limit path returns before the gate), so the panel handler asks the
// plane the same question the seam asks and counts the member as recorded: no
// INCOMPLETE, no failure streak, no dead booking — and the panel continues, so
// its findings reach the fix.
func TestDrainPhases_ReviewPanelMemberLimitedAfterRecordingIsRecorded(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	h := &fakeAdapter{
		steps: []invokeStep{
			{res: checkpointed(10, 0.10)},           // implement
			{res: held(limitResult(), openClaim())}, // review-1 records, THEN is turned away
			{res: checkpointed(5, 0.50)},            // review-2 records
			{res: checkpointed(5, 0.50)},            // review-3 records (last, closes)
			{res: checkpointed(5, 0.05)},            // fix
		},
		// The record means the panel continues and the wait is not owed here;
		// a reset already past keeps the fixture deterministic if one is taken.
		limitResetAt: time.Now().Add(-time.Hour),
	}
	h.onInvoke = func(i int) {
		switch i {
		case 1:
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-1", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{mustFinding("1.review-1.1", "open", "review-1's MUST")},
			})
		case 2:
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-2", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
			})
		case 3:
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: "review-3", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
			})
		case 4:
			rel.status = "in_review"
			rel.outcome += "\n### Fixes\n- 1.review-1.1 fixed\n"
		}
	}
	d, ff := panelDriverWithFleet(t, h, rel, panelMembers(), panelPhases())
	logs := captureLogs(t)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 5 {
		t.Fatalf("spawned %d sessions, want 5 (implement, three members, fix) — a member that recorded must not stop the panel", h.invokeCalls)
	}
	fix := h.invocations[4].Prompt
	if strings.Contains(fix, "INCOMPLETE") {
		t.Errorf("a member that recorded before the limit was reported incomplete:\n%s", fix)
	}
	if !strings.Contains(fix, "1.review-1.1") {
		t.Errorf("review-1's findings are not in the fix brief:\n%s", fix)
	}
	if n := d.panelFailStreak["review-1"]; n != 0 {
		t.Errorf("panelFailStreak[review-1] = %d, want 0 — the member recorded", n)
	}
	if cell := d.deadTally[tallyKey{phase: "review-1", harness: "opencode"}]; cell != nil && cell.dead != 0 {
		t.Errorf("the member that recorded was booked dead: %+v", cell)
	}
	recs := ff.records()
	if len(recs) != 1 {
		t.Fatalf("iteration records = %d, want 1", len(recs))
	}
	if strings.Contains(logs.String(), "review-1:") {
		t.Errorf("the daemon log names review-1 as a failed member:\n%s", logs.String())
	}
}

// TestDrainPhases_ReviewPanelLastMemberEndsSequenceAfterRecording (CLA-639 fix
// pass 5): the LAST member appends its record and is then turned away by a
// usage limit or a transient failure. The seam never ran, so the round is still
// sealed; the panel handler must release it while the holder is live and hand
// over to the fix exactly as the failed last member's arm does. Otherwise the
// fix never runs and a takeover voids every member's record.
func TestDrainPhases_ReviewPanelLastMemberEndsSequenceAfterRecording(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  harness.Result
	}{
		{name: "a usage limit", res: held(limitResult(), openClaim())},
		{name: "a transient failure", res: held(transientResult(), openClaim())},
		{
			// A session the adapter also classifies as limited can still exit
			// cleanly: the seam runs first, matches the record and closes the
			// round itself, and the sequence-end is read afterwards. The arm must
			// not close a second time (CLA-639 fix pass 6).
			name: "a usage limit on a clean exit the seam already closed",
			res:  reported(held(harness.Result{ExitCode: 0, Tokens: 5, CostUSD: 0.05, Raw: map[string]any{"kind": "limit"}}, openClaim()), branchReport()),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
			h := &fakeAdapter{
				steps: []invokeStep{
					{res: checkpointed(10, 0.10)}, // implement
					{res: checkpointed(5, 0.50)},  // review-1 records
					{res: tc.res},                 // review-2 records, THEN the sequence ends
					{res: checkpointed(5, 0.05)},  // fix: must spawn on the released round
				},
				limitResetAt: time.Now().Add(-time.Hour),
			}
			h.onInvoke = func(i int) {
				switch i {
				case 1:
					rel.records = append(rel.records, plane.ReviewRecordState{
						Round: 1, ReviewerRole: "review-1", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
						Findings: []plane.ReviewFindingState{mustFinding("1.review-1.1", "open", "review-1's MUST")},
					})
				case 2:
					rel.records = append(rel.records, plane.ReviewRecordState{
						Round: 1, ReviewerRole: "review-2", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
						Findings: []plane.ReviewFindingState{mustFinding("1.review-2.1", "open", "review-2's MUST")},
					})
				case 3:
					rel.status = "in_review"
					rel.outcome += "\n### Fixes\n- 1.review-1.1 fixed\n- 1.review-2.1 fixed\n"
				}
			}
			d := panelDriver(t, h, rel, panelMembers()[:2], panelPhases())

			if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
				t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
			}
			if h.invokeCalls != 4 {
				t.Fatalf("spawned %d sessions, want 4 (implement, two members, fix) — the last member's record must hand over", h.invokeCalls)
			}
			if len(rel.closes) != 1 || rel.closes[0] != 1 {
				t.Fatalf("closed rounds = %v, want [1] — the last member's round must be released before the fix", rel.closes)
			}
			fix := h.invocations[3].Prompt
			for _, want := range []string{"1.review-1.1", "1.review-2.1"} {
				if !strings.Contains(fix, want) {
					t.Errorf("the fix brief does not carry %q:\n%s", want, fix)
				}
			}
			if strings.Contains(fix, "INCOMPLETE") {
				t.Errorf("a member that recorded was reported incomplete:\n%s", fix)
			}
			for _, r := range rel.records {
				if r.State != "open" {
					t.Errorf("the record of %s is %q after the panel closed, want open — a sealed round would be voided on takeover", r.ReviewerRole, r.State)
				}
			}
			if n := d.panelFailStreak["review-2"]; n != 0 {
				t.Errorf("panelFailStreak[review-2] = %d, want 0 — the member recorded", n)
			}
		})
	}
}

// TestDrainPhases_ReviewPanelUnheldClaimDoesNotMatchALooseTip (CLA-639 fix pass
// 5): when the member's own claim is not Held, the checkpoint-fact copy cannot
// fill `CheckpointTip`, so the carried claim carries a branch and no tip. The
// seam's matcher only ever runs with a verified tip or the unverified mark; with
// neither, matching by run and role alone would accept ANY tip, looser than the
// seam. The panel must leave such a member to the failure arm.
func TestDrainPhases_ReviewPanelUnheldClaimDoesNotMatchALooseTip(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	// The member's claim is held-looking except that it is SETTLED (not Held),
	// and it names a branch: exactly the shape the checkpoint-fact copy skips.
	unheld := harness.Claim{TaskID: "t-1", RunID: "r-1", Settled: true, Branch: "clanker/x"}
	h := &fakeAdapter{
		steps: []invokeStep{
			{res: checkpointed(10, 0.10)},      // implement
			{res: held(limitResult(), unheld)}, // review-1 records, then is turned away
			{res: okResult(1, 0)},              // must never run
			{res: okResult(1, 0)},              // must never run
		},
		limitResetAt: time.Now().Add(-time.Hour),
	}
	h.onInvoke = func(i int) {
		if i == 1 {
			rel.records = append(rel.records, recordFor(1, "review-1", "r-1", testCheckpointTip, "sealed"))
		}
	}
	d, ff := panelDriverWithFleet(t, h, rel, panelMembers(), panelPhases())
	logs := captureLogs(t)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 2 {
		t.Fatalf("spawned %d sessions, want 2 (implement, review-1) — the drain must end with the failed member", h.invokeCalls)
	}
	recs := ff.records()
	if len(recs) != 1 {
		t.Fatalf("iteration records = %d, want 1", len(recs))
	}
	// The record keeps the enum (CLA-738); the log names the failed member.
	requireFleetOutcome(t, recs[0].Outcome, fleet.OutcomeReleased)
	out := logs.String()
	for _, want := range []string{"review panel INCOMPLETE", "review-1", "hit a usage limit before recording"} {
		if !strings.Contains(out, want) {
			t.Errorf("the daemon log does not carry %q:\n%s", want, out)
		}
	}
	if n := d.panelFailStreak["review-1"]; n != 1 {
		t.Errorf("panelFailStreak[review-1] = %d, want 1 — without a verified tip the record is not this member's exit evidence", n)
	}
}

// TestDrainPhases_ReviewPanelLastMemberCloseFailureEndsWithoutAFix (CLA-639 fix
// pass 6): the close the recorded + sequence-end arm performs is fail-closed. A
// close error must end the drain with no fix and no success report, exactly as
// the failed last member's arm does. This pins the `break`; replacing it with a
// fall-through spawns the fix and fails this test.
func TestDrainPhases_ReviewPanelLastMemberCloseFailureEndsWithoutAFix(t *testing.T) {
	rel := &reviewReleaser{
		status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n",
		closeErr: errors.New("the plane is unreachable"),
	}
	h := &fakeAdapter{
		steps: []invokeStep{
			{res: checkpointed(10, 0.10)},           // implement
			{res: checkpointed(5, 0.50)},            // review-1 records
			{res: held(limitResult(), openClaim())}, // review-2 records, then is turned away
			{res: checkpointed(5, 0.05)},            // the fix: must NEVER run
		},
		limitResetAt: time.Now().Add(-time.Hour),
	}
	h.onInvoke = func(i int) {
		switch i {
		case 1, 2:
			role := "review-" + strconv.Itoa(i)
			rel.records = append(rel.records, plane.ReviewRecordState{
				Round: 1, ReviewerRole: role, RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
				Findings: []plane.ReviewFindingState{mustFinding("1."+role+".1", "open", "a "+role+" MUST")},
			})
		}
	}
	d, ff := panelDriverWithFleet(t, h, rel, panelMembers()[:2], panelPhases())
	logs := captureLogs(t)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 3 {
		t.Fatalf("spawned %d sessions, want 3 (implement, two members, NO fix) — a failed close must not hand over", h.invokeCalls)
	}
	if len(rel.closes) != 0 {
		t.Errorf("a close was recorded despite the injected failure: %v", rel.closes)
	}
	out := logs.String()
	if !strings.Contains(out, "could not be closed") || !strings.Contains(out, "without spawning the fix") {
		t.Errorf("the driver did not say why the fix was skipped:\n%s", out)
	}
	recs := ff.records()
	if len(recs) != 1 {
		t.Fatalf("iteration records = %d, want 1", len(recs))
	}
	if recs[0].Outcome == fleet.OutcomeCheckpoint {
		t.Errorf("the drain reported a checkpoint after the close failed: %q", recs[0].Outcome)
	}
	requireFleetOutcome(t, recs[0].Outcome, "")
	if strings.Contains(out, "INCOMPLETE") {
		t.Errorf("a member that recorded was reported incomplete:\n%s", out)
	}
}

// requireFleetOutcome asserts an iteration record's Outcome is one of the four
// words the plane knows (CLA-738), and — when want is non-empty — exactly that
// word. Free text here makes marshalReport refuse the WHOLE report, dropping
// the iteration record and the presence refresh riding it; prose about a panel
// belongs in the daemon log.
func requireFleetOutcome(t *testing.T, outcome, want string) {
	t.Helper()
	switch outcome {
	case fleet.OutcomeCheckpoint, fleet.OutcomeReleased, fleet.OutcomeParked, fleet.OutcomeDead:
	default:
		t.Fatalf("iteration outcome = %q, want one of the four fleet.Outcome* constants", outcome)
	}
	if want != "" && outcome != want {
		t.Errorf("iteration outcome = %q, want %q", outcome, want)
	}
}
