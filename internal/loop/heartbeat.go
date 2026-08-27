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
// OnClaim seam that feeds renewal also feeds that reflector.
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
	r := &leaseRenewer{
		hb:       hb,
		interval: heartbeatInterval(planeLeaseDuration),
		prefix:   prefix,
		claims:   make(chan harness.Claim, 4),
		done:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}
	if t.Fleet != nil {
		// CLA-510: the fleet card's task ref rides the same observation seam
		// as lease renewal - one watcher, two consumers. Only when there is a
		// reporter to see it; a nil Fleet makes the reflector pure overhead.
		// done is handed over so the reflector can refuse writes after stop()
		// at its own write site, not only at apply's guard.
		r.reflect = d.reflectClaim(ti, t, r.done)
	}
	inv.OnClaim = r.observe
	go r.run(ctx, inv.ResumeClaim)
	return r
}

// observe receives one claim snapshot from an adapter's parser. Latest wins:
// if the renewer has not drained the previous snapshot yet, it is dropped in
// favour of this one - only the newest claim state can be true.
//
// Invocation.OnClaim asks for a non-blocking implementation, and this is one
// except at one bound: if FOUR snapshots pile up undrained - which takes the
// renewer stuck inside one Heartbeat call while four claim transitions stream
// past - the fifth waits for that call's heartbeatBeatTimeout rather than drop
// the newest state. Dropping the newest is the one loss this channel cannot
// tolerate; the wait is bounded by stop() regardless.
func (r *leaseRenewer) observe(c harness.Claim) {
	if r == nil {
		return
	}
	select {
	case <-r.done:
		return
	default:
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
// renewal state folds in - one choke point for both consumers of the snapshot.
// The done-guard at the top bounds what can reach the reflector after stop():
// stop()'s wait is bounded, so the goroutine can outlive it wedged in a
// Heartbeat call, and a snapshot queued before the stop and picked up after it
// must not revise the fleet row - the driver may already be into the next phase.
// The guard is a check-then-act; the reflector's own write site re-checks done
// under iterMu (see Driver.reflectClaim), so the no-reflect-after-stop
// invariant holds by construction at the write, not only by interleaving luck.
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
// next, and stand down on cancellation, stop(), a not-wired plane, or
// heartbeatGiveUp consecutive failures.
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
			if !current.Held() {
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
				// then the renewer stands down rather than log the same
				// refusal every tick for the rest of the session.
				log.Printf("%slease renewal unavailable: %v - standing down (the lease behaves as before CLA-358: it expires if the session outlives it)", r.prefix, err)
				return
			case ctx.Err() != nil:
				return
			default:
				failures++
				log.Printf("%slease renewal failed (%d in a row): %v", r.prefix, failures, err)
				if failures >= heartbeatGiveUp {
					log.Printf("%sgiving up on lease renewal after %d consecutive failures - the lease may expire mid-session", r.prefix, failures)
					return
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
