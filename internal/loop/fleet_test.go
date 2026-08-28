package loop

// CLA-466: the driver's fleet activity reporting. These tests pin the three
// seams the doneWhen names: a beacon accompanies every poll (and carries the
// identity), state-change beacons fire at phase boundaries without waiting out
// a poll interval, exactly ONE iteration record posts per drain with the right
// outcome across all four paths (checkpoint / released / parked / dead), and an
// outage of the report endpoint leaves loop timing and outcomes untouched with
// the failure logged once.
//
// CLA-510: the mid-phase claim. A first-phase session claims INSIDE the drain,
// after its boundary beacon has already gone out; the claim reflector riding
// the lease-renewal seam revises the in-flight ref and beacons immediately —
// so the card fills within seconds of the claim, follows the current claim,
// and never invents a ref from a refused claim.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// fakeFleet records every report the driver hands it, synchronously, so
// assertions need no waiting.
type fakeFleet struct {
	mu     sync.Mutex
	sent   []fleet.Report
	closed []fleet.Report
}

func (f *fakeFleet) Send(r fleet.Report) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, r)
}

func (f *fakeFleet) Close(r fleet.Report) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, r)
}

func (f *fakeFleet) reports() ([]fleet.Report, []fleet.Report) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent, f.closed
}

func (f *fakeFleet) records() []fleet.Iteration {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fleet.Iteration
	for _, r := range f.sent {
		out = append(out, r.Iterations...)
	}
	return out
}

// fleetDriver is phaseDriver plus a fakeFleet on the target: the shape the
// reporting tests drive. cfg comes pre-built so callers can set phases.
func fleetDriver(t *testing.T, cfg *config.Config, h harness.Adapter) (*Driver, *fakeFleet) {
	t.Helper()
	ff := &fakeFleet{}
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: &fakeReleaser{}, Fleet: ff}})
	openTestStateDir(t, d)
	d.cfg.WorkDir = t.TempDir()
	d.newVerifier = func(string, bool) deliveryVerifier { return passVerifier() }
	return d, ff
}

// Log capture reuses captureLogs/syncBuf from delivery_test.go — the same
// synchronized buffer, so reading it while the reporter's pump writes is safe.

// waitFor polls cond until it holds or the deadline passes — the same discipline
// heartbeatWaitFor uses, for the reporter's background pump.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// --- presence rides the poll --------------------------------------------------

func TestRun_ABeaconAccompaniesEachPoll(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.InstanceName = "box-a"
	// Idle queue: the loop keeps polling; the hook cancels after three polls so
	// the test bounds at exactly those three beacons plus nothing else.
	p := &fakePoller{sum: backlog.Summary{Ready: 0, Claimable: 0}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p.onCall = func(i int) {
		if i >= 2 {
			cancel()
		}
	}
	h := &fakeAdapter{}
	d, ff := fleetDriver(t, cfg, h)
	d.targets[0].Poller = p

	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	sent, closed := ff.reports()
	if len(sent) < 3 {
		t.Fatalf("expected a beacon per poll (3 polls), got %d", len(sent))
	}
	for i, r := range sent {
		if r.State.Kind != fleet.StateIdle {
			t.Errorf("beacon %d state = %q, want idle (queue empty, nothing paused)", i, r.State.Kind)
		}
		if r.Instance != "box-a" {
			t.Errorf("beacon %d instance = %q, want the configured instance_name", i, r.Instance)
		}
		if r.Host == "" {
			t.Errorf("beacon %d carries no host", i)
		}
		if r.Version == "" {
			t.Errorf("beacon %d carries no binary version", i)
		}
		if r.ConfigIdentity == "" {
			t.Errorf("beacon %d carries no config identity", i)
		}
	}
	// Run's deferred shutdown says goodbye on the way out — that is what makes
	// silence after this point mean "gone" rather than "stopped talking".
	if len(closed) != 1 {
		t.Fatalf("expected exactly one closing beacon, got %d", len(closed))
	}
	if closed[0].State.Kind != fleet.StateStopping {
		t.Errorf("closing beacon state = %q, want stopping", closed[0].State.Kind)
	}
}

// The state mapping, directly: paused targets read draining (alive, spawning
// nothing new), mid-drain reads iteration {n, taskRef, phase}, everything else
// idle.
func TestFleetState_MapsWhatTheLoopKnows(t *testing.T) {
	d := NewMulti(fastCfg(), &fakeAdapter{}, []Target{{Poller: busyPoller()}})
	if got := d.fleetState(0); got.Kind != fleet.StateIdle {
		t.Errorf("fresh target state = %v, want idle", got)
	}
	d.paused[0] = true
	if got := d.fleetState(0); got.Kind != fleet.StateDraining {
		t.Errorf("console-paused state = %v, want draining", got)
	}
	d.paused[0] = false
	d.fleetPaused[0] = true
	if got := d.fleetState(0); got.Kind != fleet.StateDraining {
		t.Errorf("fleet-paused state = %v, want draining", got)
	}
	d.fleetPaused[0] = false
	d.iter = []iterState{{on: true, n: 7, ref: "CLA-9", phase: "review"}}
	got := d.fleetState(0)
	if got.Kind != fleet.StateIteration || got.N != 7 || got.TaskRef != "CLA-9" || got.Phase != "review" {
		t.Errorf("mid-drain state = %+v, want iteration{n:7 taskRef:CLA-9 phase:review}", got)
	}
}

// --- phase-boundary beacons -----------------------------------------------------

func TestDrainPhases_StateChangeBeaconsFireAtPhaseBoundaries(t *testing.T) {
	cfg := fastCfg()
	cfg.Phases = twoPhases()
	cfg.Prompt = ""
	cfg.InstanceName = "box-b"
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)},
		// Review SETTLES the task, so the drain ends with the claim consumed and
		// the record's outcome is released (not the predecessor's checkpoint).
		{res: held(okResult(5, 0.05), harness.Claim{TaskID: "t-1", Ref: "CLA-253", RunID: "r-1", Settled: true})},
	}}
	d, ff := fleetDriver(t, cfg, h)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}

	sent, _ := ff.reports()
	type key struct {
		kind  string
		phase string
	}
	var got []key
	for _, r := range sent {
		got = append(got, key{r.State.Kind, r.State.Phase})
	}
	want := []key{{fleet.StateIteration, "implement"}, {fleet.StateIteration, "review"}, {fleet.StateIdle, ""}}
	if len(got) != len(want) {
		t.Fatalf("state beacon sequence = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("state beacon sequence = %v, want %v", got, want)
		}
	}
	// The boundary beacons carry the task once it is known (phase 2 resumes the
	// claim phase 1 left held). openClaim carries only the UUID, so the label is
	// the task id until a ref arrives.
	if sent[0].State.TaskRef != "" {
		t.Errorf("first boundary beacon names taskRef %q before any claim was observed", sent[0].State.TaskRef)
	}
	if sent[1].State.TaskRef != "t-1" || sent[1].State.N != 1 {
		t.Errorf("second boundary beacon = {n:%d ref:%q}, want {n:1 ref:t-1}", sent[1].State.N, sent[1].State.TaskRef)
	}
}

// --- the mid-phase claim (CLA-510) ------------------------------------------

// fleetRenewingDriver is fleetDriver with a releaser that CAN heartbeat, so
// startLeaseRenewal wires Invocation.OnClaim — the seam the CLA-510 claim
// reflector rides (fleetDriver's plain fakeReleaser cannot).
func fleetRenewingDriver(t *testing.T, cfg *config.Config, h harness.Adapter, ff *fakeFleet) *Driver {
	t.Helper()
	d := NewMulti(cfg, h, []Target{{
		Poller:   busyPoller(),
		Releaser: renewingReleaser{fakeReleaser: &fakeReleaser{}, hb: &recordingHeartbeat{}},
		Fleet:    ff,
	}})
	openTestStateDir(t, d)
	d.cfg.WorkDir = t.TempDir()
	d.newVerifier = func(string, bool) deliveryVerifier { return passVerifier() }
	return d
}

// waitForBeaconRef polls the fake fleet until a report whose state carries the
// given task ref has been sent. Returns false on timeout. Used by the fake's
// afterClaims hook to hold a session open until the renewer has folded its
// claim stream into the fleet ref — the thing that makes the claim beacon
// deterministic instead of a race against stop().
func waitForBeaconRef(ff *fakeFleet, ref string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sent, _ := ff.reports()
		for _, r := range sent {
			if r.State.TaskRef == ref {
				return true
			}
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// iterationRefs lists the task ref of every iteration-state beacon, in order —
// the card's history through one drain. "" means the card shows no task.
func iterationRefs(ff *fakeFleet) []string {
	sent, _ := ff.reports()
	var refs []string
	for _, r := range sent {
		if r.State.Kind == fleet.StateIteration {
			refs = append(refs, r.State.TaskRef)
		}
	}
	return refs
}

// A phase-1 drain whose session claims MID-phase: the reflector revises the
// in-flight ref and beacons at the claim — the card fills within seconds, not
// at the next phase boundary — and what the NEXT poll beacon would render
// (fleetState, read mid-session) already carries the ref, so the revision is
// durable state, not a one-shot beacon.
func TestDrainPhases_MidPhaseClaimBeaconsTheRefAndPollBeaconsCarryIt(t *testing.T) {
	cfg := fastCfg()
	cfg.InstanceName = "box-ref"
	claim := harness.Claim{TaskID: "t-1", Ref: "CLA-1", RunID: "r-1"}
	ff := &fakeFleet{}
	var d *Driver
	var midDrain fleet.State
	h := &fakeAdapter{steps: []invokeStep{{
		claims: []harness.Claim{claim},
		afterClaims: func() {
			// Hold the session open until the renewer has folded the claim in —
			// the claim beacon landing in the fake reporter is that proof — then
			// read what the next poll beacon WOULD have rendered. A poll beacon
			// cannot fire mid-drain by construction: the poll loop and the drain
			// share one goroutine (loop.go), so fleetState is the durable state
			// the next poll would read — pinning it here is what proves the
			// revision is not a one-shot beacon.
			if !waitForBeaconRef(ff, "CLA-1", 2*time.Second) {
				t.Error("the renewer never folded the mid-phase claim into the fleet ref")
			}
			midDrain = d.fleetState(0)
		},
		res: held(okResult(7, 0.5), claim),
	}}}
	d = fleetRenewingDriver(t, cfg, h, ff)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}

	sent, _ := ff.reports()
	// The card's history through the drain: boundary beacon with NO ref (the
	// claim has not been observed yet — no guessing from next_task), the claim
	// beacon, then the back-to-normal close.
	if got := iterationRefs(ff); !slices.Equal(got, []string{"", "CLA-1"}) {
		t.Fatalf("iteration beacon refs = %v, want [\"\" CLA-1]", got)
	}
	if len(sent) < 3 || sent[len(sent)-1].State.Kind != fleet.StateIdle {
		t.Fatalf("last beacon = %+v, want the back-to-normal idle close", sent[len(sent)-1])
	}
	// The claim beacon itself names the task and the phase it was claimed in.
	if sent[1].State.TaskRef != "CLA-1" || sent[1].State.N != 1 {
		t.Errorf("claim beacon = {n:%d ref:%q}, want {n:1 ref:CLA-1}", sent[1].State.N, sent[1].State.TaskRef)
	}
	// What the next poll beacon renders mid-session: the ref must already be
	// there, or a poll-driven beacon would wipe the card back to blank.
	if midDrain.Kind != fleet.StateIteration || midDrain.TaskRef != "CLA-1" || midDrain.N != 1 {
		t.Errorf("mid-drain poll state = %+v, want iteration {n:1 ref:CLA-1}", midDrain)
	}
	// The revision is not a second record: exactly one iteration still posts.
	recs := ff.records()
	if len(recs) != 1 {
		t.Fatalf("exactly one record must post per drain, got %d", len(recs))
	}
	if recs[0].TaskRef != "CLA-1" {
		t.Errorf("record taskRef = %q, want CLA-1", recs[0].TaskRef)
	}
}

// A session that settles one task and claims another mid-stream: the card
// follows the CURRENT claim, and clears while nothing is held — it must not
// latch the first claim it ever saw.
func TestDrainPhases_RefFollowsTheCurrentClaimAndClears(t *testing.T) {
	cfg := fastCfg()
	cfg.InstanceName = "box-follow"
	claimA := harness.Claim{TaskID: "t-1", Ref: "CLA-1", RunID: "r-1"}
	claimB := harness.Claim{TaskID: "t-2", Ref: "CLA-2", RunID: "r-2"}
	ff := &fakeFleet{}
	h := &fakeAdapter{steps: []invokeStep{{
		claims: []harness.Claim{
			claimA,
			{TaskID: "t-1", Ref: "CLA-1", RunID: "r-1", Settled: true},
			claimB,
		},
		afterClaims: func() {
			if !waitForBeaconRef(ff, "CLA-2", 2*time.Second) {
				t.Error("the renewer never folded the settle-and-reclaim stream into the fleet ref")
			}
		},
		res: held(okResult(7, 0.5), claimB),
	}}}
	d := fleetRenewingDriver(t, cfg, h, ff)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}

	if got := iterationRefs(ff); !slices.Equal(got, []string{"", "CLA-1", "", "CLA-2"}) {
		t.Fatalf("iteration beacon refs = %v, want [\"\" CLA-1 \"\" CLA-2] — the card must follow the current claim and clear while nothing is held", got)
	}
	if recs := ff.records(); len(recs) != 1 {
		t.Fatalf("exactly one record must post per drain, got %d", len(recs))
	}
}

// A refused claim is not a claim: it records no ids, so a real adapter fires
// nothing at OnClaim (harness.Invocation). Drive the defensive half — even if
// an empty snapshot reached the reflector, no ref may appear on the card.
func TestDrainPhases_RefusedClaimCarriesNoRef(t *testing.T) {
	cfg := fastCfg()
	cfg.InstanceName = "box-refused"
	ff := &fakeFleet{}
	h := &fakeAdapter{steps: []invokeStep{{
		claims: []harness.Claim{{}},
		res:    okResult(7, 0.5),
	}}}
	d := fleetRenewingDriver(t, cfg, h, ff)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}

	sent, _ := ff.reports()
	for i, r := range sent {
		if r.State.Kind == fleet.StateIteration && r.State.TaskRef != "" {
			t.Errorf("beacon %d carries taskRef %q from a refused claim — no ref may be invented", i, r.State.TaskRef)
		}
	}
	recs := ff.records()
	if len(recs) != 1 {
		t.Fatalf("exactly one record must post per drain, got %d", len(recs))
	}
	if recs[0].TaskID != "" || recs[0].TaskRef != "" {
		t.Errorf("record task fields = {%s %s}, want empty — a refused claim is not a claim", recs[0].TaskID, recs[0].TaskRef)
	}
}

// The reflector directly: a beacon only on a ref CHANGE — a session that
// churns claims must not turn the card into a stream — and never outside a
// live iteration.
func TestReflectClaim_BeaconsOnlyOnARefChange(t *testing.T) {
	ff := &fakeFleet{}
	d := NewMulti(fastCfg(), &fakeAdapter{}, []Target{{Fleet: ff}})
	d.iter = []iterState{{on: true, n: 3, phase: "implement"}}
	// A never-closed done: the unit test drives the reflector directly, with no
	// renewer lifecycle to race.
	reflect := d.reflectClaim(0, d.targets[0], make(chan struct{}))

	// The refused shape (no ids): nothing held, nothing changed, no beacon.
	reflect(harness.Claim{})
	if got := d.fleetState(0); got.TaskRef != "" {
		t.Fatalf("ref after an empty snapshot = %q, want empty — a refused claim is not a claim", got.TaskRef)
	}
	sent, _ := ff.reports()
	if len(sent) != 0 {
		t.Fatalf("a no-change snapshot beaconed %d time(s), want 0", len(sent))
	}

	// A real claim fills the card and beacons exactly once.
	reflect(harness.Claim{TaskID: "t-1", Ref: "CLA-1", RunID: "r-1"})
	if got := d.fleetState(0); got.TaskRef != "CLA-1" || got.N != 3 {
		t.Fatalf("state after the claim = %+v, want iteration {n:3 ref:CLA-1}", got)
	}
	sent, _ = ff.reports()
	if len(sent) != 1 || sent[0].State.TaskRef != "CLA-1" {
		t.Fatalf("claim beacon = %+v, want exactly one carrying CLA-1", sent)
	}

	// The same claim again: no churn.
	reflect(harness.Claim{TaskID: "t-1", Ref: "CLA-1", RunID: "r-1"})
	sent, _ = ff.reports()
	if len(sent) != 1 {
		t.Fatalf("a duplicate claim snapshot beaconed again; want the single change beacon, got %d", len(sent))
	}

	// The settle clears the card and beacons.
	reflect(harness.Claim{TaskID: "t-1", Ref: "CLA-1", RunID: "r-1", Settled: true})
	if got := d.fleetState(0); got.TaskRef != "" {
		t.Fatalf("ref after the settle = %q, want cleared", got.TaskRef)
	}
	sent, _ = ff.reports()
	if len(sent) != 2 || sent[1].State.TaskRef != "" {
		t.Fatalf("settle beacon = %+v, want exactly one clearing the ref", sent)
	}

	// A closed done channel refuses the write: a snapshot that reaches the
	// reflector after renewer stop() must not revise the row, whatever claim
	// it carries — the refusal branch that makes the no-cross-row invariant
	// hold by construction.
	closedDone := make(chan struct{})
	close(closedDone)
	reflectClosed := d.reflectClaim(0, d.targets[0], closedDone)
	reflectClosed(harness.Claim{TaskID: "t-2", Ref: "CLA-2", RunID: "r-2"})
	if got := d.fleetState(0); got.TaskRef != "" {
		t.Fatalf("a post-stop snapshot wrote ref %q; the refusal branch must keep the row unchanged", got.TaskRef)
	}
	sent, _ = ff.reports()
	if len(sent) != 2 {
		t.Fatalf("a post-stop snapshot beaconed; want the 2 prior beacons, got %d", len(sent))
	}

	// A live iteration is required: no beacon outside a drain.
	d.iter[0] = iterState{}
	reflect(harness.Claim{TaskID: "t-9", Ref: "CLA-9", RunID: "r-9"})
	sent, _ = ff.reports()
	if len(sent) != 2 {
		t.Fatalf("a snapshot with no live iteration beaconed; want no change, got %d", len(sent))
	}
}

// --- one record per iteration, four outcome paths -------------------------------

func TestDrainPhases_RecordOutcomeReleasedWhenTheClaimGoesBack(t *testing.T) {
	cfg := fastCfg()
	cfg.InstanceName = "box-c"
	h := &fakeAdapter{steps: []invokeStep{
		// A clean end still holding a releasable claim (no WIP): the seam's
		// deferred handback returns it to ready.
		{res: held(okResult(7, 0.5), harness.Claim{TaskID: "t-1", Ref: "CLA-1", RunID: "r-1"})},
	}}
	d, ff := fleetDriver(t, cfg, h)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	recs := ff.records()
	if len(recs) != 1 {
		t.Fatalf("exactly one record must post per iteration, got %d: %+v", len(recs), recs)
	}
	r := recs[0]
	if r.Outcome != fleet.OutcomeReleased {
		t.Errorf("outcome = %q, want released", r.Outcome)
	}
	if r.TaskID != "t-1" || r.TaskRef != "CLA-1" {
		t.Errorf("task fields = {%s %s}, want t-1/CLA-1", r.TaskID, r.TaskRef)
	}
	if r.Tokens != 7 {
		t.Errorf("tokens = %d, want 7", r.Tokens)
	}
	if r.DurationSeconds <= 0 {
		t.Errorf("duration = %v, want > 0", r.DurationSeconds)
	}
}

func TestDrainPhases_RecordOutcomeCheckpointWhenWorkIsLeftForATakeover(t *testing.T) {
	cfg := fastCfg()
	cfg.Phases = twoPhases()
	cfg.Prompt = ""
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(9, 0.9)},
		// The review phase ALSO ends holding the task with pushed work — the
		// drain's boundary leaves the lease for a successor takeover, which is
		// the loop's own definition of checkpoint (see WhatTheSeamOwes).
		{res: held(okResult(2, 0.1), harness.Claim{TaskID: "t-1", Ref: "CLA-253", RunID: "r-1", HasWIP: true})},
	}}
	d, ff := fleetDriver(t, cfg, h)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	recs := ff.records()
	if len(recs) != 1 {
		t.Fatalf("exactly one record must post per iteration, got %d", len(recs))
	}
	if recs[0].Outcome != fleet.OutcomeCheckpoint {
		t.Errorf("outcome = %q, want checkpoint", recs[0].Outcome)
	}
	if len(recs[0].Phases) != 2 || recs[0].Phases[0] != "implement" || recs[0].Phases[1] != "review" {
		t.Errorf("phases = %v, want [implement review]", recs[0].Phases)
	}
}

func TestDrainPhases_RecordOutcomeParkedWhenTheDeadBudgetTrips(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: held(deadResult(), openClaim())},
		{res: held(deadResult(), openClaim())},
		{res: held(deadResult(), openClaim())},
		{res: held(deadResult(), openClaim())}, // the fourth parks
	}}
	rel := &parkingReleaser{}
	cfg := fastCfg()
	cfg.Phases = twoPhases()
	cfg.Prompt = ""
	ff := &fakeFleet{}
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: rel, Fleet: ff}})
	openTestStateDir(t, d)
	d.cfg.WorkDir = t.TempDir()
	d.newVerifier = func(string, bool) deliveryVerifier { return passVerifier() }

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if len(rel.parks) != 1 {
		t.Fatalf("parked %d times, want 1: %+v", len(rel.parks), rel.parks)
	}
	recs := ff.records()
	if len(recs) != 1 {
		t.Fatalf("exactly one record must post even on the park path, got %d", len(recs))
	}
	if recs[0].Outcome != fleet.OutcomeParked {
		t.Errorf("outcome = %q, want parked", recs[0].Outcome)
	}
	if recs[0].TaskID != "t-1" {
		t.Errorf("taskID = %q, want t-1", recs[0].TaskID)
	}
}

func TestDrainPhases_RecordOutcomeDeadWhenTheFinalPhaseDiesProducingNothing(t *testing.T) {
	cfg := fastCfg()
	h := &fakeAdapter{steps: []invokeStep{
		// A dead phase on the FINAL (only) phase: not retried (retries belong to
		// a first phase that can re-claim), not parked — the drain just ends on
		// the death, which is exactly the dead outcome.
		{res: held(deadResult(), openClaim())},
	}}
	d, ff := fleetDriver(t, cfg, h)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	recs := ff.records()
	if len(recs) != 1 {
		t.Fatalf("exactly one record must post, got %d", len(recs))
	}
	if recs[0].Outcome != fleet.OutcomeDead {
		t.Errorf("outcome = %q, want dead", recs[0].Outcome)
	}
}

// A death that was RETRIED past must not brand the iteration dead when a later
// attempt delivers: the record describes how the iteration ENDED.
func TestDrainPhases_ARetriedDeathDoesNotOutcomeTheRecordDead(t *testing.T) {
	cfg := fastCfg()
	cfg.Phases = twoPhases()
	cfg.Prompt = ""
	h := &fakeAdapter{steps: []invokeStep{
		{res: held(deadResult(), openClaim())}, // dies once...
		{res: checkpointed(1, 0)},              // ...retry reaches its checkpoint
		{res: held(okResult(1, 0), harness.Claim{TaskID: "t-1", Ref: "CLA-253", RunID: "r-1", Settled: true})},
	}}
	d, ff := fleetDriver(t, cfg, h)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	recs := ff.records()
	if len(recs) != 1 {
		t.Fatalf("exactly one record must post, got %d", len(recs))
	}
	if recs[0].Outcome != fleet.OutcomeReleased {
		t.Errorf("outcome = %q, want released (the retry delivered; the early death is the tally's business, not the record's)", recs[0].Outcome)
	}
}

// --- fail-soft: an outage touches neither timing nor outcomes -------------------

func TestRun_ReportEndpointOutageLeavesTheLoopAlone(t *testing.T) {
	var mu sync.Mutex
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	buf := captureLogs(t)

	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 1
	h := &fakeAdapter{steps: []invokeStep{{res: okResult(3, 0)}}}
	rel := &fakeReleaser{}
	ff := fleet.New(srv.URL, "k") // the REAL reporter, pointed at a 500-ing plane
	p := busyPoller()
	d := NewMulti(cfg, h, []Target{{Poller: p, Releaser: rel, Fleet: ff}})
	openTestStateDir(t, d)

	start := time.Now()
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	elapsed := time.Since(start)

	// Loop outcomes untouched: one session spawned, the drain counted normally,
	// and the run ended gracefully rather than failing on the reporting path.
	if h.invokeCalls != 1 {
		t.Errorf("spawned %d sessions, want 1", h.invokeCalls)
	}
	// Loop TIMING untouched: every report is enqueued, never awaited. Had Send
	// blocked even on the report timeout alone, the several beacons this run
	// makes would cost more than twice the budget below.
	if elapsed > 2*time.Second {
		t.Errorf("Run took %s — reporting must never delay the loop", elapsed.Round(time.Millisecond))
	}
	// The failure is logged ONCE for the whole outage, not once per report.
	waitFor(t, "the background posts to finish", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return hits >= 2 // poll beacon + shutdown close, at least
	})
	time.Sleep(50 * time.Millisecond) // let the last failure land in the buffer
	lines := countLines(buf.String(), "fleet report failed and was dropped")
	if lines != 1 {
		t.Errorf("%d 'fleet report failed' lines for a whole outage, want exactly 1:\n%s", lines, buf.String())
	}
}

// countLines counts occurrences of substr in s, line-wise.
func countLines(s, substr string) int {
	n := 0
	for _, ln := range strings.Split(s, "\n") {
		if strings.Contains(ln, substr) {
			n++
		}
	}
	return n
}

// A healthy endpoint sees the recovery line say how much was lost — the one
// trace a dropped report gets.
func TestReporter_OutageThenRecoveryLogsOnceWithTheDropCount(t *testing.T) {
	fail := true
	var mu sync.Mutex
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		wasFail := fail
		hits++
		mu.Unlock()
		if wasFail {
			http.Error(w, "nope", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	buf := captureLogs(t)

	rep := fleet.New(srv.URL, "k")
	for i := 0; i < 5; i++ {
		rep.Send(fleet.Report{Identity: fleet.Identity{Instance: "x"}, State: fleet.State{Kind: fleet.StateIdle}})
	}
	waitFor(t, "all five outage reports to be served", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return hits >= 5
	})
	waitFor(t, "the first streak to be logged", func() bool {
		return countLines(buf.String(), "fleet report failed and was dropped") == 1
	})
	mu.Lock()
	fail = false
	mu.Unlock()
	rep.Send(fleet.Report{Identity: fleet.Identity{Instance: "x"}})
	waitFor(t, "the recovery line", func() bool {
		return strings.Contains(buf.String(), "fleet reporting recovered after")
	})
	if n := countLines(buf.String(), "fleet report failed and was dropped"); n != 1 {
		t.Errorf("%d failure lines, want exactly one for the whole streak", n)
	}
	line := ""
	for _, ln := range strings.Split(buf.String(), "\n") {
		if strings.Contains(ln, "fleet reporting recovered after") {
			line = ln
		}
	}
	if !strings.Contains(line, "5 failed") {
		t.Errorf("recovery line %q does not name the whole silent streak", line)
	}
}

// --- CLA-501: the resolved instance identity ----------------------------------

// writeDaemonConfig writes one owner-only daemon config body to dir/base and
// loads it, so the config's source path (the half the default identity embeds)
// is real.
func writeDaemonConfig(t *testing.T, dir, base string) *config.Config {
	t.Helper()
	p := filepath.Join(dir, base)
	body := `{"harness":"claude","prompt":"Work the backlog."}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("config.Load(%s): %v", p, err)
	}
	return cfg
}

// identityOf builds the minimal driver and reports what it would beacon.
func identityOf(t *testing.T, cfg *config.Config) fleet.Identity {
	t.Helper()
	d := NewMulti(cfg, &fakeAdapter{}, []Target{{Poller: busyPoller(), Releaser: &fakeReleaser{}}})
	return d.fleetIdentity()
}

// The bug CLA-501 fixes: four daemons on one host all beaconed the bare
// hostname, so the plane's unique (project, instance_name) key collapsed them
// into one Fleet row. Two daemons started from different config files in the
// same directory must resolve to two DISTINCT identities.
func TestFleetIdentityDistinctPerConfigFile(t *testing.T) {
	dir := t.TempDir()
	a := identityOf(t, writeDaemonConfig(t, dir, "clanker1.json"))
	b := identityOf(t, writeDaemonConfig(t, dir, "clanker2.json"))
	host, err := os.Hostname()
	if err != nil {
		t.Fatalf("os.Hostname: %v", err)
	}
	if a.Instance == b.Instance {
		t.Fatalf("distinct config files must not share an instance identity; both %q", a.Instance)
	}
	if want := host + "/clanker1"; a.Instance != want {
		t.Errorf("a.Instance = %q, want %q (hostname + config basename)", a.Instance, want)
	}
	if want := host + "/clanker2"; b.Instance != want {
		t.Errorf("b.Instance = %q, want %q", b.Instance, want)
	}
	if a.Instance == host || b.Instance == host {
		t.Error("the bare hostname alone must never be the identity when a config file names this daemon")
	}
}

// An explicit instance_name still wins over the new default: the operator who
// already named their daemons keeps exactly the names they chose.
func TestFleetIdentityExplicitNameWins(t *testing.T) {
	dir := t.TempDir()
	cfg := writeDaemonConfig(t, dir, "clanker1.json")
	cfg.InstanceName = "box-a"
	id := identityOf(t, cfg)
	if id.Instance != "box-a" {
		t.Errorf("Instance = %q, want the explicit name verbatim", id.Instance)
	}
}

// ctl reload re-reads the SAME config file, so the basename half of the default
// cannot change under a live daemon: the Fleet page must keep seeing one row,
// not watch its daemon rename itself mid-run.
func TestFleetIdentityStableAcrossReload(t *testing.T) {
	dir := t.TempDir()
	cfg := writeDaemonConfig(t, dir, "clanker1.json")
	d := NewMulti(cfg, &fakeAdapter{}, []Target{{Poller: busyPoller(), Releaser: &fakeReleaser{}}})
	before := d.fleetIdentity()

	path := cfg.Source()
	d.SetReloader(func() (*config.Config, error) {
		fresh, err := loadValidatedConfig(path)
		if err != nil {
			return nil, err
		}
		fresh.Prompt = "reloaded brief" // proves the swap actually happened
		return fresh, nil
	})
	d.applyReload()
	if d.cfg.Prompt != "reloaded brief" {
		t.Fatal("the reload never swapped the config in; this test would pass vacuously")
	}
	after := d.fleetIdentity()
	if after.Instance != before.Instance {
		t.Errorf("identity changed across a reload: %q -> %q; a live daemon must keep its name",
			before.Instance, after.Instance)
	}
}
