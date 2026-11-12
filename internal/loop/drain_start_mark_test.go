package loop

// drain_start_mark_test.go — the conditions markPhaseDone decides on, each one
// driven directly on a hand-built step: a full drain cannot cheaply produce an
// unverified checkpoint, a panel member or a missing marker interface at once.

import (
	"context"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// markCheckpoint is the phase end every case starts from: an orderly checkpoint
// still holding the task, with a verified branch tip.
func markCheckpoint() phaseEnd {
	return phaseEnd{checkpoint: true, claim: &harness.Result{
		Claim:         harness.Claim{TaskID: "t-1", RunID: "r-1"},
		CheckpointTip: startTip,
	}}
}

func TestMarkPhaseDone(t *testing.T) {
	cases := []struct {
		name     string
		ph       config.Phase
		panel    bool
		terminal bool
		session  bool // the drain is not runner-claimed
		noMarker bool // the releaser does not implement plane.PhaseMarker
		mutate   func(*phaseEnd)
		want     int
	}{
		{name: "an implement checkpoint", ph: config.Phase{Name: config.ImplementPhaseName}, want: 1},
		{name: "a fix checkpoint", ph: config.Phase{Name: config.FixPhaseName}, want: 1},
		{name: "a review checkpoint", ph: config.Phase{Name: config.ReviewPhaseName}, want: 1},
		{
			name: "the last member of a review panel", ph: config.Phase{Name: config.ReviewPhaseName},
			panel: true, terminal: true, want: 1,
		},
		{
			name: "a handoff to a successor", ph: config.Phase{Name: config.ImplementPhaseName},
			mutate: func(e *phaseEnd) { e.handoff = "carry on with the tests" },
		},
		{
			name: "an unverified checkpoint", ph: config.Phase{Name: config.ImplementPhaseName},
			mutate: func(e *phaseEnd) { e.claim.CheckpointUnverified = true },
		},
		{
			name: "a verify checkpoint holds the seam for another fix round",
			ph:   config.Phase{Name: config.VerifyPhaseName},
		},
		{
			name: "a non-terminal panel member", ph: config.Phase{Name: config.ReviewPhaseName},
			panel: true, terminal: false,
		},
		{
			name: "no checkpoint", ph: config.Phase{Name: config.ImplementPhaseName},
			mutate: func(e *phaseEnd) { e.checkpoint = false },
		},
		{
			name: "no claim observed", ph: config.Phase{Name: config.ImplementPhaseName},
			mutate: func(e *phaseEnd) { e.claim = nil },
		},
		{
			name: "a settled claim", ph: config.Phase{Name: config.ImplementPhaseName},
			mutate: func(e *phaseEnd) { e.claim.Claim.Settled = true },
		},
		{
			name: "a checkpoint that names no tip", ph: config.Phase{Name: config.ImplementPhaseName},
			mutate: func(e *phaseEnd) { e.claim.CheckpointTip = "" },
		},
		{name: "a session-claimed drain", ph: config.Phase{Name: config.ImplementPhaseName}, session: true},
		{name: "a releaser that cannot mark", ph: config.Phase{Name: config.ImplementPhaseName}, noMarker: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			marker := &startReleaser{}
			var rel plane.Releaser = marker
			if tc.noMarker {
				rel = &claimingReleaser{}
			}
			s := &drainSeq{
				d:             &Driver{},
				t:             Target{Name: "t", Releaser: rel},
				drainNum:      1,
				runnerClaimed: !tc.session,
			}
			end := markCheckpoint()
			if tc.mutate != nil {
				tc.mutate(&end)
			}
			st := &seqStep{ph: tc.ph, panelMember: tc.panel, terminal: tc.terminal, end: end}

			s.markPhaseDone(context.Background(), st)

			if len(marker.done) != tc.want {
				t.Fatalf("RecordPhaseDone calls = %+v, want %d", marker.done, tc.want)
			}
			if tc.want == 0 {
				return
			}
			want := phaseDoneCall{"t-1", "r-1", tc.ph.Name, startTip}
			if marker.done[0] != want {
				t.Errorf("RecordPhaseDone = %+v, want %+v", marker.done[0], want)
			}
		})
	}
}
