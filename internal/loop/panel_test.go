package loop

import (
	"context"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
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
// repeated name shape (the trial panel's own pattern): the duplicate is an
// independent run, not a duplicate to deduplicate.

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

// panelMembers is the trial panel's shape, smaller: the same model twice plus
// another model once.
func panelMembers() []config.ReviewPanelMember {
	return []config.ReviewPanelMember{
		{Name: "review-1", Harness: "opencode", Model: "opencode-go/gpt-6-luna"},
		{Name: "review-2", Harness: "opencode", Model: "opencode-go/gpt-6-luna"},
		{Name: "review-3", Harness: "opencode", Model: "opencode-go/glm-5.3-flash"},
	}
}

// panelDriver wires a phased driver over the given members, on one fake adapter
// for every harness (so the invocation list is the spawn order).
func panelDriver(t *testing.T, h *fakeAdapter, rel *reviewReleaser, members []config.ReviewPanelMember, phases []config.Phase) *Driver {
	t.Helper()
	cfg := fastCfg()
	cfg.Prompt = ""
	cfg.Phases = phases
	cfg.ReviewPanel = members
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: rel}})
	// One fake adapter answers every harness name, so the invocation list is the
	// spawn order and no real CLI is reached.
	d.newAdapter = func(string) (harness.Adapter, error) { return h, nil }
	openTestStateDir(t, d)
	d.cfg.WorkDir = t.TempDir()
	d.newVerifier = func(string, bool) deliveryVerifier { return passVerifier() }
	return d
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
	if !strings.Contains(h.invocations[1].Prompt, "harness opencode, model opencode-go/gpt-6-luna") {
		t.Errorf("review-1's brief does not name its own harness/model:\n%s", h.invocations[1].Prompt)
	}
	if !strings.Contains(h.invocations[3].Prompt, "harness opencode, model opencode-go/glm-5.3-flash") {
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
// panel still closes. A failure in the MIDDLE leaves the next member a claim to
// resume; a failure at the END closes the round with whatever the earlier
// members wrote. This is the doneWhen's "recorded or been recorded as
// failed/capped".
func TestDrainPhases_ReviewPanelMemberFailureDoesNotStopTheRound(t *testing.T) {
	t.Run("a mid-panel usage limit closes the round and ends this run's panel", func(t *testing.T) {
		rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
		h := &fakeAdapter{steps: []invokeStep{
			{res: checkpointed(10, 0.10)},           // implement
			{res: checkpointed(5, 0.50)},            // review-1 records
			{res: held(limitResult(), openClaim())}, // review-2 is turned away
		}}
		h.onInvoke = func(i int) {
			if i == 1 {
				rel.records = append(rel.records, plane.ReviewRecordState{
					Round: 1, ReviewerRole: "review-1", RunID: "r-1", ReviewedTip: testCheckpointTip, State: "sealed",
					Findings: []plane.ReviewFindingState{mustFinding("1.review-1.1", "open", "the seam is untested")},
				})
			}
		}
		d := panelDriver(t, h, rel, panelMembers(), panelPhases())

		if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
			t.Fatalf("drainPhases: err=%v stop=%v — a usage-limited member must not fail the run", err, stop)
		}
		if h.invokeCalls != 3 {
			t.Fatalf("spawned %d sessions, want 3 — implement, the recording member, the limited one; no third reviewer is spawned into the same limit", h.invokeCalls)
		}
		if len(rel.closes) != 1 || rel.closes[0] != 1 {
			t.Fatalf("closed rounds = %v, want exactly [1] — the panel closes after the failed member, not never", rel.closes)
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
				rel.outcome += "\n## Review\n- verdict: findings\n"
			}
		}
		d := panelDriver(t, h, rel, panelMembers(), panelPhases())

		if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
			t.Fatalf("drainPhases: err=%v stop=%v — a usage-limited last member must not fail the run", err, stop)
		}
		// Three reviewers run, then the panel ends: a usage-limited LAST member
		// has no fix phase to hand over to (the next drain re-claims the task as
		// a takeover), but the round still closes and releases the two records
		// that DID land.
		if h.invokeCalls != 4 {
			t.Fatalf("spawned %d sessions, want 4 — implement and three members", h.invokeCalls)
		}
		if len(rel.closes) != 1 || rel.closes[0] != 1 {
			t.Fatalf("closed rounds = %v, want exactly [1] — the round closes after EVERY member has recorded or been recorded as failed", rel.closes)
		}
		for _, r := range rel.records {
			if r.State == "sealed" {
				t.Errorf("record %s/%s stayed sealed after the panel closed", r.ReviewerRole, r.ID)
			}
		}
	})
}

// TestDrainPhases_ReviewPanelAllFailuresEndWithoutAFix: when every member fails
// and the last one takes the claim with it, there is nothing to review and
// nothing for a fix to apply — the sequence ends, and the round that earlier
// members wrote is still released rather than left sealed.
func TestDrainPhases_ReviewPanelAllFailuresEndWithoutAFix(t *testing.T) {
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented: pushed the branch\n"}
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)}, // implement
		{res: held(deadResult(), openClaim())},
		{res: okResult(0, 0)}, // must NEVER run: the claim is gone
	}}
	d := panelDriver(t, h, rel, panelMembers(), panelPhases())

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 2 {
		t.Fatalf("spawned %d sessions, want 2 — a member that releases the claim leaves no run for the next member", h.invokeCalls)
	}
}

// TestMergedFindingsDigest: the driver's merge is mechanical, and the two rules
// that matter are pinned directly — a MUST from any single member stays open,
// and SHOULD/NIT carry the members that raised them.
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
	got := mergedFindingsDigest(records, []string{"review-1", "review-2"}, 1)

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

	// A member with no record is named incomplete, so the fixer knows the list
	// is the union of the reviewers that RAN.
	got = mergedFindingsDigest(records, []string{"review-1", "review-2", "review-3"}, 1)
	if !strings.Contains(got, "INCOMPLETE") || !strings.Contains(got, "review-3") {
		t.Errorf("the missing member is not named:\n%s", got)
	}

	// A sealed record contributes no FINDING: the merge reads only what the
	// plane released. Its member is therefore named incomplete — the panel's
	// records exist but the fixer cannot see their content — and no finding text
	// can leak.
	got = mergedFindingsDigest([]plane.ReviewRecordState{{Round: 1, ReviewerRole: "review-1", State: "sealed"}}, []string{"review-1"}, 1)
	if !strings.Contains(got, "INCOMPLETE") {
		t.Errorf("a sealed record left the panel reading complete:\n%s", got)
	}
	if strings.Contains(got, "the seam is untested") {
		t.Errorf("a sealed record's findings reached the merged list:\n%s", got)
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
