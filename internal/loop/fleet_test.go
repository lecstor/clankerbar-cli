package loop

// CLA-466: the driver's fleet activity reporting. These tests pin the three
// seams the doneWhen names: a beacon accompanies every poll (and carries the
// identity), state-change beacons fire at phase boundaries without waiting out
// a poll interval, exactly ONE iteration record posts per drain with the right
// outcome across all four paths (checkpoint / released / parked / dead), and an
// outage of the report endpoint leaves loop timing and outcomes untouched with
// the failure logged once.

import (
	"context"
	"net/http"
	"net/http/httptest"
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
