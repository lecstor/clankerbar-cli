package loop

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// planeLeaseDuration is the lease the plane grants a claim_task: thirty
// minutes, per the served clankerbar protocol ("The lease is 30 minutes"). The
// driver does not read it off the plane; it is protocol furniture, like the
// third-of-lease cadence the same protocol prescribes for renewals.
const planeLeaseDuration = 30 * time.Minute

// heartbeatBeatTimeout bounds ONE renewal call. A renewal that hangs must not
// outlive its usefulness by much: the renewer holds no state a late response
// would corrupt, but a stuck call would otherwise pin the goroutine past
// stop() and delay the attempt ladder behind it.
const heartbeatBeatTimeout = 20 * time.Second

// heartbeatGiveUp is how many consecutive failed renewals the renewer
// tolerates before standing down. One failure is a blip (the plane restarting,
// a network blip); three in a row across a third-of-lease cadence means the
// plane is unreachable for this session's remaining lifetime, and retrying
// every tick would only fill the log. The lease may then expire mid-session -
// which is the pre-CLA-358 behaviour, not a worse one.
const heartbeatGiveUp = 3

// heartbeatInterval reports how often a live claim's lease should be renewed:
// a third of the lease, the cadence the served clankerbar protocol itself
// prescribes ("heartbeat every ~10 minutes - a third of the 30-minute lease").
// Beating faster costs a round-trip per tick and buys nothing; beating slower
// spends the margin the protocol built in. A lease under three ticks wide
// degrades to one millisecond rather than zero, so a degenerate value still
// renews (and a test can pin the arithmetic without waiting).
func heartbeatInterval(lease time.Duration) time.Duration {
	if lease < 3 {
		return time.Millisecond
	}
	return lease / 3
}

// leaseRenewer renews the lease of whatever backlog task the spawned session
// is holding, for exactly as long as the session's Invoke runs (CLA-358).
//
// The run id to renew is learned one of two ways: seeded from the invocation's
// ResumeClaim (a resumed phase continues a live run it never claimed), or
// observed live as the session's own claim_task lands - both adapters that
// track claims fire Invocation.OnClaim the moment the stream carries the ids,
// so an opencode session that claims twenty minutes in is renewed from that
// moment, which is the exact gap that used to kill its lease (the CLA-331
// measurement this task exists for).
//
// The lifecycle is the point. The renewer lives from just before Invoke to
// just after it returns, so renewal PAUSES whenever no child process is
// running - between attempts, across supervised waits, after exit - and never
// beats on a lease the driver is about to release or hand to the next phase.
// Within the window, the adapter's own resurrection machinery (opencode
// CLA-406) is spanned deliberately: those processes continue the SAME session
// on the SAME claim, and the claim's lease does not pause just because the
// child between them died.
type leaseRenewer struct {
	hb       plane.Heartbeat
	interval time.Duration
	prefix   string
	// claims carries the latest observed claim snapshot; observe keeps only
	// the newest if the renewer has not caught up.
	claims  chan harness.Claim
	done    chan struct{}
	stopped chan struct{}
	// tick is a test seam: when non-nil, run drives its cadence from this
	// channel instead of constructing a time.Ticker (see run). Tests inject
	// a channel they fire by hand so a claim queued through observe() can be
	// raced against a tick deterministically.
	tick chan time.Time
	// reflect, when non-nil, is called on the renewer goroutine with every
	// observed claim snapshot, so the fleet presence ref follows the CURRENT
	// claim mid-phase (CLA-510 — see Driver.reflectClaim). Nil for a target
	// with no fleet reporter, and nil in the heartbeat unit tests, which
	// construct the renewer directly.
	reflect func(harness.Claim)
	// claimFile, when non-nil, is called synchronously from observe with every
	// observed claim snapshot to maintain CURRENT_CLAIM (CLA-708) - the record
	// a supervisor uses to hand a dead child's claim back. It must run before
	// observe returns so a branch is never left queued behind a parked
	// Heartbeat. Nil in the heartbeat unit tests, which construct the renewer
	// directly.
	claimFile func(harness.Claim)
	// presence, when non-nil, sends one lap of presence beacons on every
	// renewal tick (CLA-771), exactly what the poll's per-target lap sends: the
	// running target's live state and every other served target's own. The
	// renewal tick is the only cadence that still fires while a session runs -
	// the backlog poll that otherwise carries presence does not run mid-drain -
	// so without this a session longer than the plane's
	// INSTANCE_STALE_AFTER_MS (15 minutes) reads on the console as a dead
	// instance. It is called BEFORE the beat decision, so a session that holds
	// no claim yet (a first-phase session claims minutes in) is still visibly
	// alive, and the tick itself keeps firing after the BEAT stands down (see
	// run), so presence never depends on the heartbeat's health: the fleet
	// report is a different transport from the MCP heartbeat, and a plane that
	// refuses renewals may still accept presence. Nil for a target with no
	// fleet reporter, and nil in the heartbeat unit tests, which construct the
	// renewer directly.
	//
	// Its signature carries no error on purpose: reporting is fail-soft by
	// contract, so a failed report is the Reporter's to log and drop, and there
	// is nothing a caller could do with the failure anyway.
	presence func()
}

// startLeaseRenewal begins renewing the spawned session's claim lease and
// wires the observation callback into inv. It returns nil - and touches
// nothing - when there is nothing to renew with: no Releaser on the target,
// or one that cannot heartbeat. A probe invocation is skipped too: a probe
// never claims, so there is nothing to renew and the callback would be dead
// weight on a liveness check.
//
// ti is the target's index, so the fleet claim reflector can revise the right
// presence row (CLA-510). When the target carries a fleet reporter, the same
// OnClaim seam that feeds renewal also feeds that reflector, and every renewal
// tick also sends a presence beacon (CLA-771).
//
// The returned renewer's stop() must be called when Invoke returns.
func (d *Driver) startLeaseRenewal(ctx context.Context, ti int, t Target, inv *harness.Invocation, prefix string) *leaseRenewer {
	if t.Releaser == nil || inv.Probe {
		return nil
	}
	hb, ok := t.Releaser.(plane.Heartbeat)
	if !ok {
		return nil
	}
	interval := d.leaseRenewalInterval
	if interval <= 0 {
		interval = heartbeatInterval(planeLeaseDuration)
	}
	r := &leaseRenewer{
		hb:       hb,
		interval: interval,
		prefix:   prefix,
		claims:   make(chan harness.Claim, 4),
		done:     make(chan struct{}),
		stopped:  make(chan struct{}),
		// CURRENT_CLAIM (CLA-708): the same observation seam, a third consumer.
		// The project rides along because a claim file outlives this process and
		// the supervisor needs the endpoint the claim belongs to.
		claimFile: func(c harness.Claim) { d.noteCurrentClaim(c, t.Name) },
	}
	if t.Fleet != nil {
		// CLA-510: the fleet card's task ref rides the same observation seam
		// as lease renewal - one watcher, two consumers. Only when there is a
		// reporter to see it; a nil Fleet makes the reflector pure overhead.
		// done is handed over so the reflector can refuse writes after stop()
		// at its own write site, not only at apply's guard.
		r.reflect = d.reflectClaim(ti, t, r.done)
		// CLA-771: presence on the renewal cadence. The state is read fresh at
		// each tick so it carries the CURRENT claim ref and phase; every target
		// gets its lap, because the poll that covers the sibling projects does
		// not run while a session does (see Driver.beaconSessionTargets). A nil
		// Fleet has nothing to send it to, so it is not wired.
		r.presence = func() { d.beaconSessionTargets(ti, t) }
	}
	inv.OnClaim = r.observe
	go r.run(ctx, inv.ResumeClaim)
	return r
}

// observe receives one claim snapshot from an adapter's parser. Latest wins:
// if the renewer has not drained the previous snapshot yet, it is dropped in
// favour of this one - only the newest claim state can be true.
//
// CURRENT_CLAIM (CLA-708) is persisted HERE, synchronously, before this
// callback returns, rather than from the renewer goroutine in apply. The
// difference is a lost takeover: `record_progress(branch: ...)` succeeds, then the
// daemon dies while the renewer is still parked inside a Heartbeat call with
// the branch snapshot queued, and the supervisor reads the old no-branch record
// and sends `release` over pushed work. A local state-dir write is the one
// thing this callback cannot defer; the queueing below stays non-blocking.
//
// Invocation.OnClaim asks for a non-blocking implementation, and this is one
// except at that write and one bound: if FOUR snapshots pile up undrained -
// which takes the renewer stuck inside one Heartbeat call while four claim
// transitions stream past - the fifth waits for that call's
// heartbeatBeatTimeout rather than drop the newest state. Dropping the newest
// is the one loss this channel cannot tolerate; the wait is bounded by stop()
// regardless.
func (r *leaseRenewer) observe(c harness.Claim) {
	if r == nil {
		return
	}
	select {
	case <-r.done:
		return
	default:
	}
	if r.claimFile != nil {
		r.claimFile(c)
	}
	select {
	case r.claims <- c:
		return
	default:
	}
	// Full: shed the stale snapshot, then try once more.
	select {
	case <-r.claims:
	default:
	}
	select {
	case r.claims <- c:
	case <-r.done:
	}
}

// apply folds one observed claim snapshot into the renewer's running state,
// logging the transitions that matter. The newest snapshot is the truth: a
// settle keeps the run id and flips Settled, so it arrives as a snapshot whose
// RunID is unchanged - which is exactly why the pause below keys off Held.
//
// The fleet reflector (CLA-510) runs here, on the renewer goroutine, before the
// renewal state folds in. The done-guard at the top bounds what can reach the
// reflector after stop(): stop()'s wait is bounded, so the goroutine can
// outlive it wedged in a Heartbeat call, and a snapshot queued before the stop
// and picked up after it must not revise the fleet row - the driver may already
// be into the next phase. The guard is a check-then-act; the reflector's own
// write site re-checks done under iterMu (see Driver.reflectClaim), so the
// no-reflect-after-stop invariant holds by construction at the write, not only
// by interleaving luck.
//
// CURRENT_CLAIM is NOT written here: observe persists it synchronously so a
// branch cannot be lost behind a parked Heartbeat (CLA-708, review 1.1).
func (r *leaseRenewer) apply(c, current harness.Claim) harness.Claim {
	select {
	case <-r.done:
		return current
	default:
	}
	if r.reflect != nil {
		r.reflect(c)
	}
	switch {
	case c.Settled && !current.Settled:
		log.Printf("%sthe session settled %s - pausing lease renewal until it holds a task again", r.prefix, claimName(c))
	case !c.Settled && c.RunID != "" && c.RunID != current.RunID:
		log.Printf("%srenewing the lease of %s every %s while the session runs", r.prefix, claimName(c), r.interval)
	}
	return c
}

// run is the renewer's loop: beat every interval for as long as the current
// claim is HELD (Claim.Held: a task held and not yet settled by the session
// itself), follow the claim as the session settles one task and claims the
// next, and exit on cancellation or stop().
//
// The BEAT stands down - permanently, for this renewer - on a not-wired plane
// or heartbeatGiveUp consecutive failures. The TICK does not: it keeps firing
// for the rest of the Invoke so the presence beacon still goes out every
// interval (CLA-771). Two transports, two fates: the MCP heartbeat may be
// refusing (an older plane with no tool, a broken MCP transport) while the
// fleet-report POST is perfectly healthy, and a session that goes dark on the
// console for the rest of its life is exactly the symptom CLA-771 exists to
// fix. Standing the whole goroutine down here would bring it back.
//
// Held is the gate, not RunID != "": a settle keeps the run id on the tracked
// claim (the driver reads it after exit) and flips Settled instead, so a
// settle arrives as a snapshot whose RunID is unchanged. Keying the pause off
// RunID would make it unreachable and keep beating a lease the plane already
// released.
func (r *leaseRenewer) run(ctx context.Context, current harness.Claim) {
	defer close(r.stopped)
	// Testing seam: a test-provided tick channel subordinates the renewer to
	// ticks the test fires by hand, so a claim observation can be interleaved
	// with a tick on a spine the test fully controls. Nil keeps the real
	// time-based cadence.
	var tick <-chan time.Time
	stopTicker := func() {}
	if r.tick != nil {
		tick = r.tick
	} else {
		t := time.NewTicker(r.interval)
		tick = t.C
		stopTicker = t.Stop
	}
	defer stopTicker()
	beats := 0
	failures := 0
	// stoodDown disables the BEAT only. The tick keeps firing, so the presence
	// beacon keeps flowing every interval for the rest of the session (CLA-771):
	// a plane that will not renew this lease may still accept the fleet report,
	// which is a different transport, and a healthy long session must not read
	// as a dead instance just because the heartbeat endpoint is refusing.
	stoodDown := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.done:
			if beats > 0 {
				log.Printf("%slease renewal stopping after %d beat(s)", r.prefix, beats)
			}
			return
		case c := <-r.claims:
			current = r.apply(c, current)
		case <-tick:
			// A claim transition queued by observe() wins over the tick,
			// every time. Without draining here, select's random pick can
			// hand the tick to a loop whose `current` is still the pre-settle
			// claim, and beat ONCE MORE on a lease the session already
			// settled - the flake that made the pause test fail on CI. The
			// settle is newer truth than the cadence the ticker carries.
		drain:
			for {
				select {
				case c := <-r.claims:
					current = r.apply(c, current)
				default:
					break drain
				}
			}
			// CLA-771: the presence beacon rides THIS tick - the one cadence
			// that still fires while a session runs. It goes out before the
			// beat decision so a session that has not claimed yet (a
			// first-phase session claims minutes into its stream) still shows
			// its instance alive; a held claim's tick then beats as before.
			// It also goes out BEFORE the stand-down guards below, so a beat
			// that is disabled (or a claim that is settled) never takes the
			// presence cadence with it.
			// The beacon is fail-soft by contract (fleet.Reporter.Send drops a
			// failed report and returns), so it cannot stop or delay renewal.
			// The done-guard is the reflector's rule: a tick that loses the
			// race with stop() must not revise a row the next phase is about
			// to own.
			select {
			case <-r.done:
				return
			default:
			}
			if r.presence != nil {
				r.presence()
			}
			if stoodDown || !current.Held() {
				continue
			}
			bctx, cancel := context.WithTimeout(ctx, heartbeatBeatTimeout)
			err := r.hb.Heartbeat(bctx, current.RunID)
			cancel()
			switch {
			case err == nil:
				beats++
				failures = 0
			case errors.Is(err, plane.ErrNotWired):
				// A not-wired plane cannot renew anything, ever. Said once,
				// then the BEAT stands down rather than log the same refusal
				// every tick for the rest of the session. The tick itself does
				// not: presence keeps flowing (CLA-771).
				log.Printf("%slease renewal unavailable: %v - standing down on the beat (the lease behaves as before CLA-358: it expires if the session outlives it); presence beacons continue every %s", r.prefix, err, r.interval)
				stoodDown = true
			case ctx.Err() != nil:
				return
			default:
				failures++
				log.Printf("%slease renewal failed (%d in a row): %v", r.prefix, failures, err)
				if failures >= heartbeatGiveUp {
					// Same rule as ErrNotWired: no more beats, but the tick
					// keeps carrying presence for the rest of the session.
					log.Printf("%sgiving up on lease renewal after %d consecutive failures - the lease may expire mid-session; presence beacons continue every %s", r.prefix, failures, r.interval)
					stoodDown = true
				}
			}
		}
	}
}

// stop ends the renewer and waits - bounded, so a wedged renewal call cannot
// stall the attempt ladder - for the goroutine to exit. Nil-safe, so the
// Invoke call site needs no conditional.
func (r *leaseRenewer) stop() {
	if r == nil {
		return
	}
	close(r.done)
	select {
	case <-r.stopped:
	case <-time.After(heartbeatBeatTimeout + 5*time.Second):
	}
}

// claimName is the human name for a claim in the renewer's log lines: the
// qualified ref when the plane supplied one, the bare task id when not.
func claimName(c harness.Claim) string {
	if c.Ref != "" {
		return c.Ref
	}
	return c.TaskID
}
