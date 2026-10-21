package loop

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// The tests in this file pin CLA-358's two halves: the cadence arithmetic
// (heartbeatInterval) and the claim-tracking lifecycle of leaseRenewer - what
// makes it beat, what makes it pause, what makes it follow a new claim, and
// what makes it stand down. Every wait is deadline-bounded so a wedged renewer
// fails a test instead of hanging the suite.

// recordingHeartbeat is a plane.Heartbeat that records every renewal call.
// failuresLeft beats return err (default: never fail); stall, when non-nil,
// blocks every call until closed, to exercise the bounded-stop path. entered,
// when non-nil, is closed the first time a call reaches the stall point, so a
// test can know the renewer is parked INSIDE a call (out of its select).
type recordingHeartbeat struct {
	mu           sync.Mutex
	runIDs       []string
	err          error
	failuresLeft int
	stall        chan struct{}
	entered      chan struct{}
}

func (f *recordingHeartbeat) Heartbeat(_ context.Context, runID string) error {
	if f.stall != nil {
		if f.entered != nil {
			select {
			case <-f.entered:
			default:
				close(f.entered)
			}
		}
		<-f.stall
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runIDs = append(f.runIDs, runID)
	if f.failuresLeft > 0 {
		f.failuresLeft--
		return f.err
	}
	return nil
}

func (f *recordingHeartbeat) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.runIDs...)
}

func (f *recordingHeartbeat) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runIDs)
}

// heartbeatWaitFor polls cond until it holds or the (short) deadline passes,
// so a renewer that never reaches its expected state fails rather than hangs.
func heartbeatWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(500 * time.Microsecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// newStartedRenewer builds a renewer on a fast tick and runs it against seed,
// with a context the test owns. The caller must stop it (directly or via ctx).
func newStartedRenewer(t *testing.T, hb plane.Heartbeat, seed harness.Claim) (*leaseRenewer, context.CancelFunc) {
	t.Helper()
	return newStartedRenewerWithPresence(t, hb, seed, nil)
}

// newStartedRenewerWithPresence is newStartedRenewer with the CLA-771 presence
// seam wired. The hook has to be installed BEFORE run starts, or writing it
// afterwards would race the goroutine reading it.
func newStartedRenewerWithPresence(t *testing.T, hb plane.Heartbeat, seed harness.Claim, presence func()) (*leaseRenewer, context.CancelFunc) {
	t.Helper()
	r := &leaseRenewer{
		hb:       hb,
		interval: time.Millisecond,
		prefix:   "test: ",
		claims:   make(chan harness.Claim, 4),
		done:     make(chan struct{}),
		stopped:  make(chan struct{}),
		presence: presence,
	}
	ctx, cancel := context.WithCancel(context.Background())
	go r.run(ctx, seed)
	return r, cancel
}

func heartbeatClaim(runID string) harness.Claim {
	return harness.Claim{TaskID: "t-1", Ref: "CLA-1", RunID: runID}
}

// --- cadence arithmetic -----------------------------------------------------

func TestHeartbeatIntervalIsAThirdOfTheLease(t *testing.T) {
	if got := heartbeatInterval(planeLeaseDuration); got != 10*time.Minute {
		t.Fatalf("heartbeatInterval(30m) = %s, want 10m (a third of the protocol's lease)", got)
	}
	if got := heartbeatInterval(time.Second); got != time.Second/3 {
		t.Fatalf("heartbeatInterval(1s) = %s, want %s", got, time.Second/3)
	}
}

func TestHeartbeatIntervalDegenerateLeaseStaysPositive(t *testing.T) {
	// time.NewTicker panics on a non-positive interval, and lease/3 rounds DOWN:
	// for a lease of one or two nanoseconds that division is zero. The degenerate
	// branch must catch every such value, boundary inclusive, or a future edit
	// reintroduces a panic reachable from startLeaseRenewal.
	for _, lease := range []time.Duration{0, -time.Minute, 1, 2, 2 * time.Nanosecond} {
		if got := heartbeatInterval(lease); got <= 0 {
			t.Fatalf("heartbeatInterval(%d) = %d, want strictly positive", lease, got)
		}
	}
	if got := heartbeatInterval(3); got != 1 {
		t.Fatalf("heartbeatInterval(3ns) = %d, want 1ns (the exact division boundary)", got)
	}
}

// --- startLeaseRenewal's wiring conditions ----------------------------------

// renewingReleaser is a Releaser that CAN heartbeat, which is what the driver
// type-asserts for; without this shape startLeaseRenewal correctly declines.
type renewingReleaser struct {
	*fakeReleaser
	hb *recordingHeartbeat
}

func (r renewingReleaser) Heartbeat(ctx context.Context, runID string) error {
	return r.hb.Heartbeat(ctx, runID)
}

func TestStartLeaseRenewalSkipsWhenThereIsNothingToRenewWith(t *testing.T) {
	cases := []struct {
		name     string
		target   Target
		probe    bool
		wantNil  bool
		wantHook bool // inv.OnClaim wired?
	}{
		{"no releaser", Target{}, false, true, false},
		{"releaser cannot heartbeat", Target{Releaser: &fakeReleaser{}}, false, true, false},
		{"probe invocation", Target{Releaser: renewingReleaser{hb: &recordingHeartbeat{}}}, true, true, false},
		{"capable releaser, real invocation", Target{Releaser: renewingReleaser{hb: &recordingHeartbeat{}}}, false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Driver{}
			inv := harness.Invocation{Probe: tc.probe}
			r := d.startLeaseRenewal(context.Background(), 0, tc.target, &inv, "")
			if (r == nil) != tc.wantNil {
				t.Fatalf("startLeaseRenewal returned %T, wantNil=%v", r, tc.wantNil)
			}
			if (inv.OnClaim != nil) != tc.wantHook {
				t.Fatalf("inv.OnClaim wired = %v, want %v", inv.OnClaim != nil, tc.wantHook)
			}
			if r != nil {
				r.stop()
			}
		})
	}
}

// TestStartLeaseRenewalPinsTheProductionCadence pins the value the zero
// leaseRenewalInterval fallback produces (CLA-771 review): the doneWhen's "at
// most 10 minutes apart, inside the plane's 15-minute staleness window" is
// decided HERE, and the two CLA-771 tests override the field with milliseconds,
// so nothing else observes it. A fallback of, say, heartbeatInterval(2 *
// planeLeaseDuration) would push presence outside the plane's window and bring
// the bug straight back with every other test still green.
func TestStartLeaseRenewalPinsTheProductionCadence(t *testing.T) {
	d := &Driver{} // leaseRenewalInterval zero: the production path
	inv := harness.Invocation{}
	r := d.startLeaseRenewal(context.Background(), 0, Target{Releaser: renewingReleaser{hb: &recordingHeartbeat{}}}, &inv, "")
	if r == nil {
		t.Fatal("startLeaseRenewal declined a heartbeat-capable releaser; nothing below can be checked")
	}
	defer r.stop()

	want := heartbeatInterval(planeLeaseDuration)
	if r.interval != want {
		t.Fatalf("production renewal interval = %s, want %s (a third of the plane's lease)", r.interval, want)
	}
	// 10 minutes is the cadence itself, and it must stay inside the plane's
	// INSTANCE_STALE_AFTER_MS window (15 minutes, FleetSignal on the plane side)
	// with room for one missed tick.
	if want != 10*time.Minute {
		t.Fatalf("heartbeatInterval(planeLeaseDuration) = %s, want 10m", want)
	}
	if r.interval >= 15*time.Minute {
		t.Fatalf("production renewal interval = %s, outside the plane's 15-minute presence window", r.interval)
	}
}

// --- the beat lifecycle ------------------------------------------------------

func TestLeaseRenewerBeatsASeededClaimFromItsFirstTick(t *testing.T) {
	// A resumed phase seeds the claim it never claimed (invocationFor): renewal
	// must begin with no observation at all.
	hb := &recordingHeartbeat{}
	r, cancel := newStartedRenewer(t, hb, heartbeatClaim("r-1"))
	defer cancel()
	defer r.stop()

	heartbeatWaitFor(t, "three beats on the seeded run id", func() bool { return hb.count() >= 3 })
	for _, id := range hb.calls() {
		if id != "r-1" {
			t.Fatalf("beat carried runID %q, want r-1", id)
		}
	}
}

func TestLeaseRenewerBeatsOnlyFromTheMomentAClaimIsObserved(t *testing.T) {
	// The CLA-331 gap: a fresh session claims twenty minutes into its stream.
	// Until OnClaim fires there is nothing to renew - and no invented run id.
	hb := &recordingHeartbeat{}
	r, cancel := newStartedRenewer(t, hb, harness.Claim{})
	defer cancel()
	defer r.stop()

	time.Sleep(20 * time.Millisecond)
	if n := hb.count(); n != 0 {
		t.Fatalf("renewed %d time(s) before any claim was observed, want 0", n)
	}

	r.observe(heartbeatClaim("r-live"))
	heartbeatWaitFor(t, "a beat carrying the observed claim", func() bool {
		cs := hb.calls()
		return len(cs) > 0 && cs[len(cs)-1] == "r-live"
	})
}

func TestLeaseRenewerPausesWhenTheSessionSettlesItsClaim(t *testing.T) {
	// THE regression this test pins: a settle keeps RunID and flips Settled, so a
	// pause keyed off RunID going empty is unreachable and the renewer would go
	// on beating a lease the plane already released.
	hb := &recordingHeartbeat{}
	r, cancel := newStartedRenewer(t, hb, heartbeatClaim("r-1"))
	defer cancel()
	defer r.stop()

	heartbeatWaitFor(t, "beats before the settle", func() bool { return hb.count() >= 3 })
	r.observe(harness.Claim{TaskID: "t-1", Ref: "CLA-1", RunID: "r-1", Settled: true})

	settled := hb.count()
	time.Sleep(40 * time.Millisecond) // ~40 ticks' worth of opportunity to misbehave
	if n := hb.count(); n != settled {
		t.Fatalf("beats after settle = %d (was %d): renewal did not pause on a settled claim", n, settled)
	}
}

// TestLeaseRenewerSettleQueuedBeforeATickNeverLosesToTheBeat pins the ordering
// rule the drain enforces: a settle that observe() has queued is applied
// before any beat decision, so a tick ready at the same moment must not beat.
//
// The renewer is driven by a hand-fired tick channel (the `tick` seam) and its
// heartbeat is stalled to park it OUT of the main select while both a settle
// and a tick are placed in their channels; only then is the stall released, so
// the loop re-enters select with the claim and the tick ready together. The
// drain makes the settle win every time - the fixed code never beats here.
// A regression that drops the drain races the settle against the tick and lets
// the beat through roughly half the runs, which CI catches fast.
func TestLeaseRenewerSettleQueuedBeforeATickNeverLosesToTheBeat(t *testing.T) {
	hb := &recordingHeartbeat{stall: make(chan struct{}), entered: make(chan struct{})}
	ticks := make(chan time.Time, 4)
	r := &leaseRenewer{
		hb:       hb,
		interval: time.Minute, // cadence is hand-driven via r.tick, not the timer
		prefix:   "test: ",
		claims:   make(chan harness.Claim, 4),
		done:     make(chan struct{}),
		stopped:  make(chan struct{}),
		tick:     ticks,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer r.stop()
	go r.run(ctx, heartbeatClaim("r-1"))

	// Fire one tick and wait for the renewer to be PARKED INSIDE the stalled
	// Heartbeat call - entered is only closed once the call has reached the
	// stall point, so by then the loop is out of the main select. (Waiting on
	// tick consumption instead is not enough: the loop can be descheduled
	// between receiving the tick and entering Heartbeat, and a settle queued
	// in that gap is drained by the same tick case - a false 'no beats'.)
	ticks <- time.Now()
	heartbeatWaitFor(t, "the renewer to park inside the stalled heartbeat", func() bool {
		select {
		case <-hb.entered:
			return true
		default:
			return false
		}
	})

	// Queue the settle and a tick while the renewer is parked, so both are
	// sitting in their channels the moment it re-enters select.
	r.observe(harness.Claim{TaskID: "t-1", Ref: "CLA-1", RunID: "r-1", Settled: true})
	ticks <- time.Now()

	// Release the parked heartbeat. The loop now re-enters select with the
	// settle AND the tick ready together: the drain must fold the settle in
	// before the beat decision - the tick must not beat.
	close(hb.stall)

	// Wait until the queued tick has been consumed and the beat count holds
	// across a small quiet window, so the check below cannot pass merely
	// because the tick is still queued.
	heartbeatWaitFor(t, "no renewal beat after the settle was queued first", func() bool {
		if len(ticks) != 0 {
			return false
		}
		n := hb.count()
		time.Sleep(5 * time.Millisecond)
		return hb.count() == n
	})
	if n := hb.count(); n != 1 {
		t.Fatalf("queued a settle then a tick, yet %d renewal beat(s) landed (want 1): renewal must never beat a settled lease", n)
	}
}

func TestLeaseRenewerFollowsTheNextClaimAfterASettle(t *testing.T) {
	// A multi-task session settles one task and claims the next mid-stream;
	// renewal must retarget without being restarted.
	hb := &recordingHeartbeat{}
	r, cancel := newStartedRenewer(t, hb, harness.Claim{})
	defer cancel()
	defer r.stop()

	r.observe(heartbeatClaim("r-1"))
	heartbeatWaitFor(t, "beats for the first claim", func() bool { return hb.count() >= 2 })

	r.observe(harness.Claim{TaskID: "t-1", Ref: "CLA-1", RunID: "r-1", Settled: true})
	time.Sleep(5 * time.Millisecond)
	r.observe(harness.Claim{TaskID: "t-2", Ref: "CLA-2", RunID: "r-2"})

	heartbeatWaitFor(t, "beats for the second claim", func() bool {
		cs := hb.calls()
		return len(cs) > 0 && cs[len(cs)-1] == "r-2"
	})
}

func TestLeaseRenewerNeverBeatsWithoutAClaim(t *testing.T) {
	hb := &recordingHeartbeat{}
	r, cancel := newStartedRenewer(t, hb, harness.Claim{})
	defer cancel()

	time.Sleep(30 * time.Millisecond)
	if n := hb.count(); n != 0 {
		t.Fatalf("made %d renewal call(s) with no claim held, want 0", n)
	}
	r.stop()
}

// --- standing down -----------------------------------------------------------

func TestLeaseRenewerStandsDownOnANotWiredPlane(t *testing.T) {
	// One refusal said once, then no more beats for the rest of the session: the
	// lease behaves exactly as before CLA-358. The TICK does not stand down - it
	// keeps carrying presence (CLA-771), which is a different transport and may
	// be perfectly healthy on the same plane.
	hb := &recordingHeartbeat{err: plane.ErrNotWired, failuresLeft: 999}
	var beacons atomic.Int64
	r, cancel := newStartedRenewerWithPresence(t, hb, heartbeatClaim("r-1"), func() { beacons.Add(1) })
	defer cancel()
	defer r.stop()

	heartbeatWaitFor(t, "the not-wired refusal", func() bool { return hb.count() >= 1 })
	beat := hb.count()
	heartbeatWaitFor(t, "presence to keep flowing after the beat stood down", func() bool {
		return beacons.Load() >= 4
	})
	time.Sleep(10 * time.Millisecond)
	if n := hb.count(); n != beat {
		t.Fatalf("made %d call(s) on a not-wired plane, want %d and no more", n, beat)
	}
	if beat != 1 {
		t.Fatalf("refused %d time(s) on a not-wired plane, want exactly 1 before standing down", beat)
	}
	select {
	case <-r.stopped:
		t.Fatal("the renewer returned after the beat stood down; its tick must keep firing so presence keeps flowing (CLA-771)")
	default:
	}
}

func TestLeaseRenewerGivesUpAfterThreeConsecutiveFailures(t *testing.T) {
	hb := &recordingHeartbeat{err: errors.New("plane unreachable"), failuresLeft: 999}
	var beacons atomic.Int64
	r, cancel := newStartedRenewerWithPresence(t, hb, heartbeatClaim("r-1"), func() { beacons.Add(1) })
	defer cancel()
	defer r.stop()

	heartbeatWaitFor(t, "the third consecutive failure", func() bool { return hb.count() >= heartbeatGiveUp })
	// Give up the BEAT, not the tick: presence must keep going out every
	// interval for the rest of the session (CLA-771).
	heartbeatWaitFor(t, "presence to keep flowing after the beat gave up", func() bool {
		return beacons.Load() >= 4
	})
	time.Sleep(10 * time.Millisecond)
	if n := hb.count(); n != heartbeatGiveUp {
		t.Fatalf("made %d call(s) before giving up, want %d and no more", n, heartbeatGiveUp)
	}
	select {
	case <-r.stopped:
		t.Fatal("the renewer returned after the beat gave up; its tick must keep firing so presence keeps flowing (CLA-771)")
	default:
	}
}

func TestLeaseRenewerFailureCounterResetsOnSuccess(t *testing.T) {
	// Two blips then a success must NOT stand down: only CONSECUTIVE failures
	// count, or one bad minute ends renewal for a long-lived session.
	hb := &recordingHeartbeat{err: errors.New("blip"), failuresLeft: 2}
	r, cancel := newStartedRenewer(t, hb, heartbeatClaim("r-1"))
	defer cancel()
	defer r.stop()

	heartbeatWaitFor(t, "beating past the two blips plus margin", func() bool { return hb.count() >= 5 })
	select {
	case <-r.stopped:
		t.Fatal("renewer stood down despite an intervening success")
	default:
	}
}

func TestLeaseRenewerStopsWhenTheContextIsCancelled(t *testing.T) {
	hb := &recordingHeartbeat{}
	r, cancel := newStartedRenewer(t, hb, heartbeatClaim("r-1"))
	defer r.stop()

	heartbeatWaitFor(t, "at least one beat", func() bool { return hb.count() >= 1 })
	cancel()

	select {
	case <-r.stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not exit on context cancellation")
	}
}

func TestStopEndsARenewerStuckInsideABeat(t *testing.T) {
	// stop()'s bound exists so a wedged renewal call cannot stall the attempt
	// ladder forever: stop must return even though the beat never will.
	hb := &recordingHeartbeat{stall: make(chan struct{})}
	r, _ := newStartedRenewer(t, hb, heartbeatClaim("r-1"))

	done := make(chan struct{})
	go func() { r.stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(heartbeatBeatTimeout + 6*time.Second):
		close(hb.stall)
		t.Fatal("stop() outlived its own bound")
	}
	close(hb.stall)
}

// --- presence on the renewal tick (CLA-771) ---------------------------------

// TestLeaseRenewerBeaconsOnEveryRenewalTick pins CLA-771's cadence: the renewal
// tick carries a presence beacon, whether or not the session holds a claim yet.
// That is the bug's own shape - an instance whose last beacon was the moment its
// session started falls off the plane's liveness window mid-session - and the
// pre-claim tick is not decoration: a first-phase session claims minutes into
// its stream, and the instance must read alive while it is still reading.
//
// The tick channel is hand-driven, so "one beacon per tick" is an exact count
// rather than a race against a timer.
func TestLeaseRenewerBeaconsOnEveryRenewalTick(t *testing.T) {
	hb := &recordingHeartbeat{}
	var beacons atomic.Int64
	ticks := make(chan time.Time, 4)
	r := &leaseRenewer{
		hb:       hb,
		interval: time.Minute, // cadence is hand-driven via r.tick, not the timer
		prefix:   "test: ",
		claims:   make(chan harness.Claim, 4),
		done:     make(chan struct{}),
		stopped:  make(chan struct{}),
		tick:     ticks,
		presence: func() { beacons.Add(1) },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer r.stop()
	go r.run(ctx, harness.Claim{})

	// No claim yet: the tick beacons presence and, correctly, beats nothing.
	ticks <- time.Now()
	heartbeatWaitFor(t, "a presence beacon on a tick with no claim", func() bool {
		return beacons.Load() == 1
	})
	if n := hb.count(); n != 0 {
		t.Fatalf("renewed %d lease(s) with no claim held, want 0", n)
	}

	// A held claim: every further tick beacons AND beats.
	r.observe(heartbeatClaim("r-1"))
	for i := int64(2); i <= 3; i++ {
		ticks <- time.Now()
		heartbeatWaitFor(t, "the tick's beat and its presence beacon", func() bool {
			return hb.count() >= int(i-1) && beacons.Load() >= i
		})
	}
	if got, want := beacons.Load(), int64(hb.count()+1); got != want {
		t.Fatalf("%d presence beacon(s) for %d beat(s), want exactly one beacon per tick (%d)", got, hb.count(), want)
	}
}

// TestLeaseRenewerKeepsBeaconingAfterTheBeatStandsDown is the stand-down half of
// CLA-771's cadence: the beat may give up (a not-wired plane, or
// heartbeatGiveUp consecutive failures) while the session keeps running, and
// presence must not go with it - the fleet report is a different transport, and
// an instance going dark mid-session is the exact symptom this task fixes.
//
// Hand-driven ticks make the counts exact: N ticks to stand the beat down, then
// further ticks that must beacon and must NOT beat.
func TestLeaseRenewerKeepsBeaconingAfterTheBeatStandsDown(t *testing.T) {
	cases := []struct {
		name string
		err  error
		// beats is how many Heartbeat CALLS the stand-down takes: the single
		// refusal on ErrNotWired, heartbeatGiveUp consecutive failures otherwise.
		beats int
	}{
		{"not wired plane", plane.ErrNotWired, 1},
		{"three consecutive failures", errors.New("plane unreachable"), heartbeatGiveUp},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hb := &recordingHeartbeat{err: tc.err, failuresLeft: 999}
			var beacons atomic.Int64
			ticks := make(chan time.Time, 16)
			r := &leaseRenewer{
				hb:       hb,
				interval: time.Minute, // cadence is hand-driven via r.tick
				prefix:   "test: ",
				claims:   make(chan harness.Claim, 4),
				done:     make(chan struct{}),
				stopped:  make(chan struct{}),
				tick:     ticks,
				presence: func() { beacons.Add(1) },
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			defer r.stop()
			go r.run(ctx, heartbeatClaim("r-1"))

			// The ticks that stand the beat down: one beacon each, one beat each.
			for i := 1; i <= tc.beats; i++ {
				ticks <- time.Now()
				heartbeatWaitFor(t, "the tick's beat and its presence beacon", func() bool {
					return hb.count() >= i && beacons.Load() >= int64(i)
				})
			}
			// Further ticks while the session runs: presence only.
			for i := 0; i < 3; i++ {
				before := beacons.Load()
				ticks <- time.Now()
				heartbeatWaitFor(t, "a presence beacon on a tick after the beat stood down", func() bool {
					return beacons.Load() > before
				})
			}
			if n := hb.count(); n != tc.beats {
				t.Fatalf("made %d heartbeat call(s), want %d and no more after standing down", n, tc.beats)
			}
			select {
			case <-r.stopped:
				t.Fatal("run returned after the beat stood down; the tick must keep carrying presence for the rest of the session")
			default:
			}
			if got, want := beacons.Load(), int64(tc.beats+3); got != want {
				t.Fatalf("%d presence beacon(s) over %d ticks, want one per tick (%d)", got, tc.beats+3, want)
			}
		})
	}
}

// --- snapshot plumbing --------------------------------------------------------

func TestObserveShedsTheOldestSnapshotWhenFull(t *testing.T) {
	r := &leaseRenewer{claims: make(chan harness.Claim, 4)}
	var cs []harness.Claim
	for _, id := range []string{"r-1", "r-2", "r-3", "r-4"} {
		r.observe(heartbeatClaim(id))
	}
	r.observe(heartbeatClaim("r-5")) // buffer full: shed r-1, land r-5
	close(r.claims)
	for c := range r.claims {
		cs = append(cs, c)
	}
	if len(cs) != 4 || cs[0].RunID != "r-2" || cs[3].RunID != "r-5" {
		t.Fatalf("buffer after shed = %v, want [r-2 r-3 r-4 r-5] (oldest shed, newest kept)", cs)
	}
}

func TestNilRenewerIsSafeToPoke(t *testing.T) {
	var r *leaseRenewer
	r.observe(heartbeatClaim("r-1")) // must not panic
	r.stop()                         // must not panic
}
