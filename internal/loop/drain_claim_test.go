package loop

// The runner claims the drain's task before the phase walk, so the tests here
// drive the claim helpers directly on a hand-built drainSeq: what makes a drain
// eligible, which of the three claim endings each queue answer produces, and
// how a retry re-takes the task the previous session handed back.

import (
	"context"
	"fmt"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// claimCall is one ClaimTask the fake saw.
type claimCall struct {
	taskID   string
	takeover bool
}

// claimingReleaser is fakeReleaser plus the plane's TaskClaimer: offers are
// served in order and ClaimTask is the test's own function, so one fake covers
// a fresh claim, a race and a withheld task.
type claimingReleaser struct {
	fakeReleaser
	offers     []plane.Offer
	offerErrs  []error // per call, positionally; nil entries succeed
	claimFn    func(taskID string, takeover bool) (plane.ClaimedTask, error)
	offerCalls int
	claimCalls []claimCall
}

func (c *claimingReleaser) NextOffer(context.Context) (plane.Offer, error) {
	i := c.offerCalls
	c.offerCalls++
	if i < len(c.offerErrs) && c.offerErrs[i] != nil {
		return plane.Offer{}, c.offerErrs[i]
	}
	if i >= len(c.offers) {
		return plane.Offer{}, nil
	}
	return c.offers[i], nil
}

func (c *claimingReleaser) ClaimTask(_ context.Context, taskID string, takeover bool) (plane.ClaimedTask, error) {
	c.claimCalls = append(c.claimCalls, claimCall{taskID, takeover})
	if c.claimFn == nil {
		return plane.ClaimedTask{}, fmt.Errorf("%w: no claim scripted", plane.ErrClaimLost)
	}
	return c.claimFn(taskID, takeover)
}

// claimedBrief is a claim the fake grants: the brief the queue offered and the
// run credential the plane returns.
func claimedBrief(b plane.TaskBrief) func(string, bool) (plane.ClaimedTask, error) {
	return func(string, bool) (plane.ClaimedTask, error) {
		return plane.ClaimedTask{Task: b, RunID: "run-" + b.ID}, nil
	}
}

// claimSeq is the drain the helpers are tested on: two built-in phases and a
// target whose Releaser is the fake.
func claimSeq(rel plane.Releaser) *drainSeq {
	phases := twoPhases()
	return &drainSeq{
		d:         &Driver{},
		t:         Target{Name: "t", Releaser: rel},
		phases:    phases,
		rawPhases: phases,
	}
}

func TestRunnerClaimEligible(t *testing.T) {
	custom := []config.Phase{{Name: "implement"}, {Name: "review", Prompt: "review it yourself"}}
	single := []config.Phase{{Name: "implement"}}
	cases := []struct {
		name   string
		rel    plane.Releaser
		phases []config.Phase
		blind  bool
		want   bool
	}{
		{name: "two built-in phases and a claimer", rel: &claimingReleaser{}, phases: twoPhases(), want: true},
		{name: "one phase is the session's own claim", rel: &claimingReleaser{}, phases: single},
		{name: "a custom prompt keeps the session path", rel: &claimingReleaser{}, phases: custom},
		{name: "a releaser that cannot claim", rel: &fakeReleaser{}, phases: twoPhases()},
		{name: "no releaser", rel: nil, phases: twoPhases()},
		{name: "a blind run cannot read the queue", rel: &claimingReleaser{}, phases: twoPhases(), blind: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := claimSeq(tc.rel)
			s.phases, s.rawPhases = tc.phases, tc.phases
			s.d.blind = tc.blind
			if _, ok := s.runnerClaimEligible(); ok != tc.want {
				t.Fatalf("runnerClaimEligible = %v, want %v", ok, tc.want)
			}
		})
	}
}

func TestClaimForDrain_FreshClaimPlantsHeldClaim(t *testing.T) {
	b := plane.TaskBrief{ID: "t-1", Ref: "CLA-1", Branch: "clanker/unrecorded"}
	f := &claimingReleaser{
		offers:  []plane.Offer{{Next: &b}},
		claimFn: claimedBrief(b),
	}
	s := claimSeq(f)

	if got := s.claimForDrain(context.Background()); got != claimHeld {
		t.Fatalf("claimForDrain = %v, want claimHeld", got)
	}
	if s.carried == nil {
		t.Fatal("no claim seeded into carried")
	}
	c := s.carried.Claim
	if c.TaskID != "t-1" || c.Ref != "CLA-1" || c.RunID != "run-t-1" {
		t.Errorf("carried claim = %+v, want t-1/CLA-1/run-t-1", c)
	}
	if c.Branch != "" {
		t.Errorf("carried branch = %q, want empty: the brief's branch is not recorded", c.Branch)
	}
	if !s.runnerClaimed {
		t.Error("runnerClaimed = false, want the runner to own the claim")
	}
	if s.seen.TaskID != "t-1" {
		t.Errorf("seen task = %q, want the claimed task", s.seen.TaskID)
	}
	if s.claimedTask.ID != "t-1" {
		t.Errorf("claimedTask = %q, want the brief kept for the start-phase wiring", s.claimedTask.ID)
	}
	if len(f.claimCalls) != 1 || f.claimCalls[0] != (claimCall{"t-1", false}) {
		t.Errorf("claim calls = %v, want one plain claim", f.claimCalls)
	}
}

func TestClaimForDrain_BranchOnlyWhenRecorded(t *testing.T) {
	b := plane.TaskBrief{ID: "t-1", Ref: "CLA-1", Branch: "clanker/recorded", HasWIP: true}
	f := &claimingReleaser{
		offers:  []plane.Offer{{Next: &b}},
		claimFn: claimedBrief(b),
	}
	s := claimSeq(f)

	if got := s.claimForDrain(context.Background()); got != claimHeld {
		t.Fatalf("claimForDrain = %v, want claimHeld", got)
	}
	if got := s.carried.Claim.Branch; got != "clanker/recorded" {
		t.Errorf("carried branch = %q, want the recorded branch", got)
	}
	if !s.carried.Claim.HasWIP {
		t.Error("carried HasWIP = false, want the brief's recorded branch signal")
	}
}

func TestClaimForDrain_TakeoverOfferClaimsWithTakeover(t *testing.T) {
	b := plane.TaskBrief{ID: "t-1", Ref: "CLA-1", RequiresTakeover: true}
	f := &claimingReleaser{
		offers:  []plane.Offer{{Next: &b}},
		claimFn: claimedBrief(b),
	}
	s := claimSeq(f)

	if got := s.claimForDrain(context.Background()); got != claimHeld {
		t.Fatalf("claimForDrain = %v, want claimHeld", got)
	}
	if len(f.claimCalls) != 1 || !f.claimCalls[0].takeover {
		t.Errorf("claim calls = %v, want a takeover claim", f.claimCalls)
	}
}

func TestClaimForDrain_LostClaimRereads(t *testing.T) {
	lost := plane.TaskBrief{ID: "lost", Ref: "CLA-2"}
	won := plane.TaskBrief{ID: "won", Ref: "CLA-3"}
	f := &claimingReleaser{
		offers: []plane.Offer{{Next: &lost}, {Next: &won}},
		claimFn: func(taskID string, _ bool) (plane.ClaimedTask, error) {
			if taskID == "lost" {
				return plane.ClaimedTask{}, fmt.Errorf("%w: another clanker took it", plane.ErrClaimLost)
			}
			return plane.ClaimedTask{Task: won, RunID: "run-won"}, nil
		},
	}
	s := claimSeq(f)

	if got := s.claimForDrain(context.Background()); got != claimHeld {
		t.Fatalf("claimForDrain = %v, want claimHeld", got)
	}
	if got := s.carried.Claim.TaskID; got != "won" {
		t.Errorf("carried task = %q, want the task the reread won", got)
	}
	if f.offerCalls != 2 || len(f.claimCalls) != 2 {
		t.Errorf("offers %d / claims %d, want 2 and 2", f.offerCalls, len(f.claimCalls))
	}
}

func TestClaimForDrain_NotClaimableFallsBackToReadyNext(t *testing.T) {
	taken := plane.TaskBrief{ID: "taken", Ref: "CLA-4", RequiresTakeover: true}
	ready := plane.TaskBrief{ID: "ready", Ref: "CLA-5"}
	f := &claimingReleaser{
		offers: []plane.Offer{{Next: &taken, ReadyNext: &ready}},
		claimFn: func(taskID string, takeover bool) (plane.ClaimedTask, error) {
			if taskID == "taken" {
				return plane.ClaimedTask{}, fmt.Errorf("%w: human only", plane.ErrNotClaimable)
			}
			return plane.ClaimedTask{Task: ready, RunID: "run-ready"}, nil
		},
	}
	s := claimSeq(f)

	if got := s.claimForDrain(context.Background()); got != claimHeld {
		t.Fatalf("claimForDrain = %v, want claimHeld", got)
	}
	if got := s.carried.Claim.TaskID; got != "ready" {
		t.Errorf("carried task = %q, want the ready fallback", got)
	}
	want := []claimCall{{"taken", true}, {"ready", false}}
	if len(f.claimCalls) != len(want) {
		t.Fatalf("claim calls = %v, want %v", f.claimCalls, want)
	}
	for i := range want {
		if f.claimCalls[i] != want[i] {
			t.Errorf("claim call %d = %v, want %v", i, f.claimCalls[i], want[i])
		}
	}
	if f.offerCalls != 1 {
		t.Errorf("offers read = %d, want 1: the fallback uses the same offer", f.offerCalls)
	}
}

func TestClaimForDrain_NotWiredFallsBackToTheSession(t *testing.T) {
	f := &claimingReleaser{offerErrs: []error{plane.ErrNotWired}}
	s := claimSeq(f)

	if got := s.claimForDrain(context.Background()); got != claimSessionPath {
		t.Fatalf("claimForDrain = %v, want claimSessionPath", got)
	}
	if s.carried != nil || s.runnerClaimed {
		t.Error("an unwired plane must leave the claim to the first session")
	}
	if len(f.claimCalls) != 0 {
		t.Errorf("claim calls = %v, want none", f.claimCalls)
	}
}

func TestClaimForDrain_NothingOfferedClaimsNothing(t *testing.T) {
	f := &claimingReleaser{offers: []plane.Offer{{}}}
	s := claimSeq(f)

	if got := s.claimForDrain(context.Background()); got != claimNothing {
		t.Fatalf("claimForDrain = %v, want claimNothing", got)
	}
	if s.carried != nil || s.runnerClaimed {
		t.Error("a drain with nothing to claim must not hold one")
	}
}

func TestClaimForDrain_EveryRoundLostClaimsNothing(t *testing.T) {
	b := plane.TaskBrief{ID: "t-1", Ref: "CLA-1"}
	f := &claimingReleaser{
		offers: []plane.Offer{{Next: &b}, {Next: &b}, {Next: &b}},
		claimFn: func(string, bool) (plane.ClaimedTask, error) {
			return plane.ClaimedTask{}, fmt.Errorf("%w: raced", plane.ErrClaimLost)
		},
	}
	s := claimSeq(f)

	if got := s.claimForDrain(context.Background()); got != claimNothing {
		t.Fatalf("claimForDrain = %v, want claimNothing", got)
	}
	if len(f.claimCalls) != runnerClaimAttempts {
		t.Errorf("claim calls = %d, want the %d-attempt bound", len(f.claimCalls), runnerClaimAttempts)
	}
	if s.carried != nil {
		t.Error("no claim should be planted after every attempt was lost")
	}
}

func TestReclaimForRetry_PlainClaim(t *testing.T) {
	b := plane.TaskBrief{ID: "t-1", Ref: "CLA-1"}
	f := &claimingReleaser{claimFn: claimedBrief(b)}
	s := claimSeq(f)

	if !s.reclaimForRetry(context.Background(), "t-1") {
		t.Fatal("reclaimForRetry = false, want the plain claim to succeed")
	}
	if got := s.carried.Claim.TaskID; got != "t-1" {
		t.Errorf("carried task = %q, want the re-claimed task", got)
	}
	if len(f.claimCalls) != 1 || f.claimCalls[0] != (claimCall{"t-1", false}) {
		t.Errorf("claim calls = %v, want one plain claim", f.claimCalls)
	}
}

func TestReclaimForRetry_LostThenTakeover(t *testing.T) {
	b := plane.TaskBrief{ID: "t-1", Ref: "CLA-1"}
	f := &claimingReleaser{
		claimFn: func(taskID string, takeover bool) (plane.ClaimedTask, error) {
			if !takeover {
				return plane.ClaimedTask{}, fmt.Errorf("%w: the lease expired but is now takeable", plane.ErrClaimLost)
			}
			return plane.ClaimedTask{Task: b, RunID: "run-t-1"}, nil
		},
	}
	s := claimSeq(f)

	if !s.reclaimForRetry(context.Background(), "t-1") {
		t.Fatal("reclaimForRetry = false, want the takeover claim to succeed")
	}
	want := []claimCall{{"t-1", false}, {"t-1", true}}
	if len(f.claimCalls) != len(want) {
		t.Fatalf("claim calls = %v, want %v", f.claimCalls, want)
	}
	for i := range want {
		if f.claimCalls[i] != want[i] {
			t.Errorf("claim call %d = %v, want %v", i, f.claimCalls[i], want[i])
		}
	}
}

func TestReclaimForRetry_FallsBackToAFreshOffer(t *testing.T) {
	fresh := plane.TaskBrief{ID: "fresh", Ref: "CLA-9"}
	f := &claimingReleaser{
		offers: []plane.Offer{{Next: &fresh}},
		claimFn: func(taskID string, takeover bool) (plane.ClaimedTask, error) {
			if taskID == "gone" {
				return plane.ClaimedTask{}, fmt.Errorf("%w: the task moved on", plane.ErrClaimLost)
			}
			return plane.ClaimedTask{Task: fresh, RunID: "run-fresh"}, nil
		},
	}
	s := claimSeq(f)

	if !s.reclaimForRetry(context.Background(), "gone") {
		t.Fatal("reclaimForRetry = false, want the fresh offer to stand in")
	}
	if got := s.carried.Claim.TaskID; got != "fresh" {
		t.Errorf("carried task = %q, want the freshly offered task", got)
	}
	want := []claimCall{{"gone", false}, {"gone", true}, {"fresh", false}}
	if len(f.claimCalls) != len(want) {
		t.Fatalf("claim calls = %v, want %v", f.claimCalls, want)
	}
	for i := range want {
		if f.claimCalls[i] != want[i] {
			t.Errorf("claim call %d = %v, want %v", i, f.claimCalls[i], want[i])
		}
	}
}
