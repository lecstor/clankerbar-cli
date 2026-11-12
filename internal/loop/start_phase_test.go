package loop

import (
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// Every row of the start-phase table, plus the overlaps and the mis-shaped
// sequences. The phases are built as name-only literals on purpose: selection
// must read Name, because it runs before EffectivePhases resolves a prompt.
func TestChooseStartPhase(t *testing.T) {
	seq := func(names ...string) []config.Phase {
		phases := make([]config.Phase, 0, len(names))
		for _, name := range names {
			phases = append(phases, config.Phase{Name: name})
		}
		return phases
	}
	// one review record with this seal state, reviewed at tip.
	record := func(state, tip string) []plane.ReviewRecordState {
		return []plane.ReviewRecordState{{State: state, ReviewedTip: tip}}
	}
	// one review record at tip carrying these findings.
	withFindings := func(state, tip string, findings ...plane.ReviewFindingState) []plane.ReviewRecordState {
		return []plane.ReviewRecordState{{State: state, ReviewedTip: tip, Findings: findings}}
	}
	must := func(status string) plane.ReviewFindingState {
		return plane.ReviewFindingState{ID: "1.review-1.1", Severity: "must", Status: status}
	}
	// one review record with this seal state, reviewed at tip, carrying MUST counts.
	recordCounts := func(state, tip string, musts int) []plane.ReviewRecordState {
		return []plane.ReviewRecordState{{State: state, ReviewedTip: tip, Counts: plane.ReviewSeverityCounts{Must: musts}}}
	}

	full := seq(config.ImplementPhaseName, config.ReviewPhaseName, config.FixPhaseName, config.VerifyPhaseName)
	panel := seq(config.ImplementPhaseName, config.ReviewPhaseName, config.ReviewPhaseName, config.FixPhaseName, config.VerifyPhaseName)
	noVerify := seq(config.ImplementPhaseName, config.ReviewPhaseName)

	type expect struct {
		index      int
		abort      bool
		branchless bool
		closeOut   bool
		branch     string
		tip        string
		why        string
	}
	tests := []struct {
		name   string
		phases []config.Phase
		facts  startFacts
		want   expect
	}{
		{
			name:   "close-out claim starts implement with the close-out brief",
			phases: full,
			facts: startFacts{
				CloseOut: true, HasWIP: true, Branch: "task/abc123", OriginKnown: true, OnOrigin: true,
				Tip: "tipA", Reviews: record("open", "tipA"),
			},
			want: expect{index: 0, closeOut: true, why: "close-out"},
		},
		{
			name:   "close-out outranks an unreadable origin",
			phases: full,
			facts: startFacts{
				CloseOut: true, HasWIP: true, Branch: "task/abc123", OriginKnown: false,
			},
			want: expect{index: 0, closeOut: true, why: "close-out"},
		},
		{
			name:   "close-out outranks a no-code delivery",
			phases: full,
			facts: startFacts{
				CloseOut: true, NoCodeDelivered: true, Branch: "task/abc123", OriginKnown: true,
			},
			want: expect{index: 0, closeOut: true, why: "close-out"},
		},
		{
			name:   "an unreadable origin with a recorded branch aborts",
			phases: full,
			facts:  startFacts{HasWIP: true, Branch: "task/abc123", OriginKnown: false},
			want:   expect{index: 0, abort: true, why: "transient"},
		},
		{
			name:   "an unreadable origin without a branch does not abort",
			phases: full,
			facts:  startFacts{Branch: "task/abc123", OriginKnown: false},
			want:   expect{index: 0, why: "no WIP"},
		},
		{
			name:   "no WIP and a no-code delivery starts the branchless review",
			phases: full,
			facts:  startFacts{NoCodeDelivered: true, Branch: "task/abc123", OriginKnown: true},
			want:   expect{index: 1, branchless: true, why: "no-code"},
		},
		{
			name:   "no WIP starts implement",
			phases: panel,
			facts:  startFacts{Branch: "task/abc123", OriginKnown: true},
			want:   expect{index: 0, why: "no WIP"},
		},
		{
			name:   "a branch not on origin restarts implement",
			phases: full,
			facts:  startFacts{HasWIP: true, Branch: "task/abc123", OriginKnown: true, OnOrigin: false},
			want:   expect{index: 0, why: "not on origin"},
		},
		{
			// Row 6 has to win over row 9's marker: the open MUSTs are the newer
			// fact, and re-reviewing work with unaddressed findings would lose them.
			name:   "open MUSTs on the released record at the tip outrank the implement marker",
			phases: full,
			facts: startFacts{
				HasWIP: true, Branch: "task/abc123", OriginKnown: true, OnOrigin: true, Tip: "tipB",
				Reviews: withFindings("open", "tipB", must("open")), OpenMustIDs: []string{"1.review-1.1"}, OpenMustAtTip: true,
				ImplementDoneTip: "tipA", ImplementDoneIsAncestor: true,
			},
			want: expect{index: 2, branch: "task/abc123", tip: "tipB", why: "fix"},
		},
		{
			// The MUST was raised on an older tip and the record at this tip is
			// clean: the reviewer at the tip did not re-raise it, but it is still
			// open on the plane, so the adjudicator starts, not the fixer.
			name:   "an older record's open MUST beside a clean record at the tip verifies",
			phases: full,
			facts: startFacts{
				HasWIP: true, Branch: "task/abc123", OriginKnown: true, OnOrigin: true, Tip: "tipB",
				Reviews: append(withFindings("open", "tipA", must("open")), record("open", "tipB")...), OpenMustIDs: []string{"1.review-1.1"},
			},
			want: expect{index: 3, branch: "task/abc123", tip: "tipB", why: "verify"},
		},
		{
			name:   "an older record's open MUST beside a clean record at the tip falls back to review",
			phases: noVerify,
			facts: startFacts{
				HasWIP: true, Branch: "task/abc123", OriginKnown: true, OnOrigin: true, Tip: "tipB",
				Reviews: append(withFindings("open", "tipA", must("open")), record("open", "tipB")...), OpenMustIDs: []string{"1.review-1.1"},
			},
			want: expect{index: 1, branch: "task/abc123", tip: "tipB", why: "review"},
		},
		{
			name:   "a released record at the tip with an open MUST starts fix",
			phases: full,
			facts: startFacts{
				HasWIP: true, Branch: "task/abc123", OriginKnown: true, OnOrigin: true, Tip: "tipB",
				Reviews: withFindings("open", "tipB", must("open")), OpenMustIDs: []string{"1.review-1.1"}, OpenMustAtTip: true,
			},
			want: expect{index: 2, branch: "task/abc123", tip: "tipB", why: "fix"},
		},
		{
			// A sealed round's findings are withheld, so it is not a record at the
			// tip and cannot select a fix session, nor hand over: there is nothing
			// for either to work from yet, so this falls through as unreviewed.
			name:   "a sealed record at the tip with MUST counts does not start fix",
			phases: full,
			facts: startFacts{
				HasWIP: true, Branch: "task/abc123", OriginKnown: true, OnOrigin: true, Tip: "tipA",
				Reviews: recordCounts("sealed", "tipA", 2),
			},
			want: expect{index: 0, why: "no implement marker"},
		},
		{
			// The implement marker is all there is: the sealed round says nothing
			// about the tip, so the marker's ancestor tip still selects review.
			name:   "a sealed record at the tip leaves the implement marker to select review",
			phases: full,
			facts: startFacts{
				HasWIP: true, Branch: "task/abc123", OriginKnown: true, OnOrigin: true, Tip: "tipA",
				Reviews:          record("sealed", "tipA"),
				ImplementDoneTip: "tipA", ImplementDoneIsAncestor: true,
			},
			want: expect{index: 1, branch: "task/abc123", tip: "tipA", why: "implement marked"},
		},
		{
			name:   "open MUSTs with no counted record at the tip verify the pushed fix",
			phases: full,
			facts: startFacts{
				HasWIP: true, Branch: "task/abc123", OriginKnown: true, OnOrigin: true, Tip: "tipB",
				Reviews: record("open", "tipA"), OpenMustIDs: []string{"1.review-1.1"},
			},
			want: expect{index: 3, branch: "task/abc123", tip: "tipB", why: "verify"},
		},
		{
			name:   "open MUSTs without a fix->verify pair fall back to review",
			phases: noVerify,
			facts: startFacts{
				HasWIP: true, Branch: "task/abc123", OriginKnown: true, OnOrigin: true, Tip: "tipB",
				Reviews: record("open", "tipA"), OpenMustIDs: []string{"1.review-1.1"},
			},
			want: expect{index: 1, branch: "task/abc123", tip: "tipB", why: "review"},
		},
		{
			name:   "counted records at the tip hand over to the verify owner",
			phases: full,
			facts: startFacts{
				HasWIP: true, Branch: "task/abc123", OriginKnown: true, OnOrigin: true, Tip: "tipA",
				Reviews: record("open", "tipA"),
			},
			want: expect{index: 3, branch: "task/abc123", tip: "tipA", why: "hand over"},
		},
		{
			// A sealed record withholds its findings, so it says nothing about
			// this tip: it cannot close the round out by handing over.
			name:   "a sealed record at the tip does not hand over",
			phases: full,
			facts: startFacts{
				HasWIP: true, Branch: "task/abc123", OriginKnown: true, OnOrigin: true, Tip: "tipA",
				Reviews: record("sealed", "tipA"),
			},
			want: expect{index: 0, why: "no implement marker"},
		},
		{
			name:   "the last phase owns the hand-over when there is no verify pair",
			phases: noVerify,
			facts: startFacts{
				HasWIP: true, Branch: "task/abc123", OriginKnown: true, OnOrigin: true, Tip: "tipA",
				Reviews: record("open", "tipA"),
			},
			want: expect{index: 1, branch: "task/abc123", tip: "tipA", why: "hand over"},
		},
		{
			// Row 8 has to win over row 9: a record at this tip is newer than the
			// implement marker further back.
			name:   "a counted record at the tip outranks the implement marker",
			phases: full,
			facts: startFacts{
				HasWIP: true, Branch: "task/abc123", OriginKnown: true, OnOrigin: true, Tip: "tipB",
				Reviews: record("open", "tipB"), ImplementDoneTip: "tipA", ImplementDoneIsAncestor: true,
			},
			want: expect{index: 3, branch: "task/abc123", tip: "tipB", why: "hand over"},
		},
		{
			name:   "the implement marker on an ancestor of the tip starts review",
			phases: full,
			facts: startFacts{
				HasWIP: true, Branch: "task/abc123", OriginKnown: true, OnOrigin: true, Tip: "tipB",
				ImplementDoneTip: "tipA", ImplementDoneIsAncestor: true,
			},
			want: expect{index: 1, branch: "task/abc123", tip: "tipB", why: "implement marked"},
		},
		{
			name:   "a marker that is not an ancestor does not start review",
			phases: full,
			facts: startFacts{
				HasWIP: true, Branch: "task/abc123", OriginKnown: true, OnOrigin: true, Tip: "tipB",
				ImplementDoneTip: "tipA", ImplementDoneIsAncestor: false,
			},
			want: expect{index: 0, why: "no implement marker"},
		},
		{
			name:   "WIP with no marker and no records starts implement",
			phases: full,
			facts: startFacts{
				HasWIP: true, Branch: "task/abc123", OriginKnown: true, OnOrigin: true, Tip: "tipB",
			},
			want: expect{index: 0, why: "no implement marker"},
		},
		{
			name:   "a void record with open MUSTs is not evidence at the tip",
			phases: full,
			facts: startFacts{
				HasWIP: true, Branch: "task/abc123", OriginKnown: true, OnOrigin: true, Tip: "tipA",
				Reviews: record("void", "tipA"), OpenMustIDs: []string{"1.review-1.1"},
			},
			want: expect{index: 3, branch: "task/abc123", tip: "tipA", why: "verify"},
		},
		{
			name:   "a void record with no open MUST is not a hand-over",
			phases: full,
			facts: startFacts{
				HasWIP: true, Branch: "task/abc123", OriginKnown: true, OnOrigin: true, Tip: "tipA",
				Reviews: record("void", "tipA"),
			},
			want: expect{index: 0, why: "no implement marker"},
		},
		{
			name:   "a panel picks the first of its review phases",
			phases: panel,
			facts: startFacts{
				HasWIP: true, Branch: "task/abc123", OriginKnown: true, OnOrigin: true, Tip: "tipB",
				ImplementDoneTip: "tipA", ImplementDoneIsAncestor: true,
			},
			want: expect{index: 1, branch: "task/abc123", tip: "tipB", why: "review"},
		},
		{
			name:   "a panel hands over to its verify phase",
			phases: panel,
			facts: startFacts{
				HasWIP: true, Branch: "task/abc123", OriginKnown: true, OnOrigin: true, Tip: "tipA",
				Reviews: record("open", "tipA"),
			},
			want: expect{index: 4, branch: "task/abc123", tip: "tipA", why: "hand over"},
		},
		{
			name:   "a panel's open MUSTs at a stale tip verify the pushed fix",
			phases: panel,
			facts: startFacts{
				HasWIP: true, Branch: "task/abc123", OriginKnown: true, OnOrigin: true, Tip: "tipB",
				Reviews: record("open", "tipA"), OpenMustIDs: []string{"1.review-1.1"},
			},
			want: expect{index: 4, branch: "task/abc123", tip: "tipB", why: "verify"},
		},
		{
			name:   "a sequence without a review phase falls back to index 0",
			phases: seq(config.ImplementPhaseName, config.FixPhaseName, config.VerifyPhaseName),
			facts: startFacts{
				HasWIP: true, Branch: "task/abc123", OriginKnown: true, OnOrigin: true, Tip: "tipB",
				ImplementDoneTip: "tipA", ImplementDoneIsAncestor: true,
			},
			want: expect{index: 0, why: "no review phase"},
		},
		{
			name:   "a sequence without a fix phase falls back to index 0",
			phases: seq(config.ImplementPhaseName, config.ReviewPhaseName, config.VerifyPhaseName),
			facts: startFacts{
				HasWIP: true, Branch: "task/abc123", OriginKnown: true, OnOrigin: true, Tip: "tipA",
				Reviews: withFindings("open", "tipA", must("open")), OpenMustIDs: []string{"1.review-1.1"}, OpenMustAtTip: true,
			},
			want: expect{index: 0, why: "no fix phase"},
		},
		{
			name:   "a single phase always starts at 0",
			phases: []config.Phase{{Name: config.ImplementPhaseName}},
			facts: startFacts{
				HasWIP: true, Branch: "task/abc123", OriginKnown: true, OnOrigin: true, Tip: "tipA",
				Reviews: record("open", "tipA"),
			},
			want: expect{index: 0, why: "single-phase"},
		},
		{
			name:   "an unphased run is the single-phase shape",
			phases: []config.Phase{{Prompt: "do the whole task"}},
			facts: startFacts{
				HasWIP: true, Branch: "task/abc123", OriginKnown: true, OnOrigin: true, Tip: "tipA",
				Reviews: record("open", "tipA"),
			},
			want: expect{index: 0, why: "single-phase"},
		},
		{
			// Abort is about the claim, not about a position: even a shape with
			// nothing to choose cannot resume a branch it cannot read.
			name:   "an unphased run still aborts on an unreadable origin with WIP",
			phases: []config.Phase{{Prompt: "do the whole task"}},
			facts:  startFacts{HasWIP: true, Branch: "task/abc123", OriginKnown: false},
			want:   expect{index: 0, abort: true, why: "transient"},
		},
		{
			name:   "no phases at all starts at 0",
			phases: nil,
			facts:  startFacts{},
			want:   expect{index: 0, why: "single-phase"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := chooseStartPhase(tc.phases, tc.facts)
			if got.Index != tc.want.index {
				t.Errorf("Index = %d, want %d (why: %s)", got.Index, tc.want.index, got.Why)
			}
			if got.Abort != tc.want.abort {
				t.Errorf("Abort = %v, want %v", got.Abort, tc.want.abort)
			}
			if got.Branchless != tc.want.branchless {
				t.Errorf("Branchless = %v, want %v", got.Branchless, tc.want.branchless)
			}
			if got.CloseOut != tc.want.closeOut {
				t.Errorf("CloseOut = %v, want %v", got.CloseOut, tc.want.closeOut)
			}
			if got.Checkpoint.Branch != tc.want.branch || got.Checkpoint.Tip != tc.want.tip {
				t.Errorf("Checkpoint = (%q, %q), want (%q, %q)",
					got.Checkpoint.Branch, got.Checkpoint.Tip, tc.want.branch, tc.want.tip)
			}
			if got.Why == "" {
				t.Error("Why is empty: every start has to be readable in the log")
			} else if !strings.Contains(got.Why, tc.want.why) {
				t.Errorf("Why = %q, want it to contain %q", got.Why, tc.want.why)
			}
		})
	}
}

// openMustIDs is what the caller must put in startFacts.OpenMustIDs: the
// plane's openMustIds is state-blind, so a void record's MUSTs would otherwise
// read as work to do.
func TestOpenMustIDs(t *testing.T) {
	finding := func(id, severity, status string) plane.ReviewFindingState {
		return plane.ReviewFindingState{ID: id, Severity: severity, Status: status}
	}
	tests := []struct {
		name    string
		records []plane.ReviewRecordState
		want    []string
	}{
		{
			name:    "no records",
			records: nil,
			want:    nil,
		},
		{
			name: "a released record's open MUSTs in finding order",
			records: []plane.ReviewRecordState{{State: "open", Findings: []plane.ReviewFindingState{
				finding("1.review-1.1", "must", "open"),
				finding("1.review-1.2", "should", "open"),
				finding("1.review-1.3", "must", "fixed"),
				finding("1.review-1.4", "must", "open"),
			}}},
			want: []string{"1.review-1.1", "1.review-1.4"},
		},
		{
			name: "record order is kept across records",
			records: []plane.ReviewRecordState{
				{State: "open", Findings: []plane.ReviewFindingState{finding("1.review-1.1", "must", "open")}},
				{State: "open", Findings: []plane.ReviewFindingState{finding("2.review-1.1", "must", "open")}},
			},
			want: []string{"1.review-1.1", "2.review-1.1"},
		},
		{
			name: "a void record's open MUST is excluded",
			records: []plane.ReviewRecordState{
				{State: "void", Findings: []plane.ReviewFindingState{finding("1.review-1.1", "must", "open")}},
			},
			want: nil,
		},
		{
			name: "a sealed record's findings are not read",
			records: []plane.ReviewRecordState{
				{State: "sealed", Findings: []plane.ReviewFindingState{finding("1.review-1.1", "must", "open")}},
			},
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := openMustIDs(tc.records)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("openMustIDs = %v, want %v", got, tc.want)
			}
		})
	}
}

// openMustAtTip is what the caller precomputes for startFacts.OpenMustAtTip, so
// its edges are the fix row's edges: which seal states, which tip, which findings.
func TestOpenMustAtTip(t *testing.T) {
	finding := func(id, severity, status string) plane.ReviewFindingState {
		return plane.ReviewFindingState{ID: id, Severity: severity, Status: status}
	}
	openMust := []plane.ReviewFindingState{finding("1.review-1.1", "must", "open")}
	// one released record at tip carrying these findings.
	released := func(tip string, findings ...plane.ReviewFindingState) []plane.ReviewRecordState {
		return []plane.ReviewRecordState{{State: "open", ReviewedTip: tip, Findings: findings}}
	}

	tests := []struct {
		name    string
		records []plane.ReviewRecordState
		tip     string
		want    bool
	}{
		{
			name:    "a released record at the tip with an open MUST",
			records: released("tipA", openMust...),
			tip:     "tipA",
			want:    true,
		},
		{
			name:    "a sealed record is ignored",
			records: []plane.ReviewRecordState{{State: "sealed", ReviewedTip: "tipA", Findings: openMust}},
			tip:     "tipA",
			want:    false,
		},
		{
			name:    "a void record is ignored",
			records: []plane.ReviewRecordState{{State: "void", ReviewedTip: "tipA", Findings: openMust}},
			tip:     "tipA",
			want:    false,
		},
		{
			name:    "a record that reviewed another tip is ignored",
			records: released("tipB", openMust...),
			tip:     "tipA",
			want:    false,
		},
		{
			// The reviewer at the tip found nothing; the older MUST is still open
			// but it is not this record's, so it cannot select a fix session.
			name:    "a clean released record at the tip beside an older open MUST",
			records: append(released("tipA", openMust...), plane.ReviewRecordState{State: "open", ReviewedTip: "tipB"}),
			tip:     "tipB",
			want:    false,
		},
		{
			name:    "findings present but every MUST is settled",
			records: released("tipA", finding("1.review-1.1", "must", "fixed"), finding("1.review-1.2", "must", "refuted")),
			tip:     "tipA",
			want:    false,
		},
		{
			name:    "an open SHOULD is not a MUST",
			records: released("tipA", finding("1.review-1.1", "should", "open")),
			tip:     "tipA",
			want:    false,
		},
		{
			// The read may trim the findings block to counts; a round that raised
			// MUSTs must still select fix rather than hand over.
			name:    "counts-only record with MUSTs falls back to the count",
			records: []plane.ReviewRecordState{{State: "open", ReviewedTip: "tipA", Counts: plane.ReviewSeverityCounts{Must: 1}}},
			tip:     "tipA",
			want:    true,
		},
		{
			name:    "counts-only record with no MUSTs",
			records: []plane.ReviewRecordState{{State: "open", ReviewedTip: "tipA", Counts: plane.ReviewSeverityCounts{Should: 2}}},
			tip:     "tipA",
			want:    false,
		},
		{
			name:    "an empty tip matches nothing",
			records: released("tipA", openMust...),
			tip:     "",
			want:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := openMustAtTip(tc.records, tc.tip); got != tc.want {
				t.Errorf("openMustAtTip = %v, want %v", got, tc.want)
			}
		})
	}
}
