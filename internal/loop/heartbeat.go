package loop

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// planeLeaseDuration is the lease the plane grants a claim_task: thirty minutes, per the served
// clankerbar protocol ("The lease is 30 minutes").
const planeLeaseDuration = 30 * time.Minute

// heartbeatBeatTimeout bounds ONE renewal call. A renewal that hangs must not outlive its
// usefulness by much: the renewer holds no state a late response would corrupt, but a stuck call
// would otherwise pin the goroutine past stop() and delay the attempt ladder behind it.
const heartbeatBeatTimeout = 20 * time.Second

// heartbeatGiveUp is how many consecutive failed renewals the renewer tolerates before standing
// down. One failure is a blip (the plane restarting, a network blip); three in a row across a
// third-of-lease cadence means the plane is unreachable for this session's remaining lifetime, and
// retrying every tick would only fill the log.
const heartbeatGiveUp = 3

// heartbeatInterval reports how often a live claim's lease should be renewed: a third of the lease,
// the cadence the served clankerbar protocol itself prescribes ("heartbeat every ~10 minutes - a
// third of the 30-minute lease").
func heartbeatInterval(lease time.Duration) time.Duration {
	if lease < 3 {
		return time.Millisecond
	}
	return lease / 3
}

// leaseRenewer renews the lease of whatever backlog task the spawned session is holding, for
// exactly as long as the session's Invoke runs. The run id to renew is learned one of two ways:
// seeded from the invocation's ResumeClaim (a resumed phase continues a live run it never claimed),
// or observed live as the session's own claim_task lands - both adapters that track claims fire
// Invocation.OnClaim the moment the stream carries the ids, so an opencode session that claims
// twenty minutes in is renewed from that moment, which is the exact gap that used to kill its
// lease.
type leaseRenewer struct {
	hb       plane.Heartbeat
	interval time.Duration
	prefix   string
	// claims carries the latest observed claim snapshot; observe keeps only the newest if the renewer
	// has not caught up.
	claims  chan harness.Claim
	done    chan struct{}
	stopped chan struct{}
	// tick is a test seam: when non-nil, run drives its cadence from this channel instead of
	// constructing a time.Ticker (see run). Receive-only: the renewer only ever reads it, and a
	// send here would be a bug, so the type says so.
	tick <-chan time.Time
	// reflect, when non-nil, is called on the renewer goroutine with every observed claim snapshot, so
	// the fleet presence ref follows the CURRENT claim mid-phase.
	reflect func(harness.Claim)
	// claimFile, when non-nil, is called synchronously from observe with every observed claim snapshot
	// to maintain CURRENT_CLAIM - the record a supervisor uses to hand a dead child's claim back.
	claimFile func(harness.Claim)
	// presence, when non-nil, sends one lap of presence beacons on every renewal tick, exactly what
	// the poll's per-target lap sends: the running target's live state and every other served target's
	// own. The renewal tick is the only cadence that still fires while a session runs - the backlog
	// poll that otherwise carries presence does not run mid-drain - so without this a session longer
	// than the plane's INSTANCE_STALE_AFTER_MS (15 minutes) reads on the console as a dead instance.
	presence func()
	// onLost, when non-nil, is called once when the plane says the run is gone (plane.ErrRunLost): the
	// session is working a task that is not its own, so the caller ends it instead of waiting for the
	// lease to lapse.
	onLost func(error)
}

// startLeaseRenewal begins renewing the spawned session's claim lease and wires the observation
// callback into inv. A probe invocation is skipped too: a probe never claims, so there is nothing
// to renew and the callback would be dead weight on a liveness check.
func (d *Driver) startLeaseRenewal(ctx context.Context, ti int, t Target, inv *harness.Invocation, prefix string, onLost func(error)) *leaseRenewer {
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
		onLost:   onLost,
		// CURRENT_CLAIM: the same observation seam, a third consumer.
		claimFile: func(c harness.Claim) { d.noteCurrentClaim(c, t.Name) },
	}
	if t.Fleet != nil {
		r.reflect = d.reflectClaim(ti, t, r.done)
		// CLA-771: presence on the renewal cadence. The state is read fresh at each tick so it carries
		// the CURRENT claim ref and phase; every target gets its lap, because the poll that covers the
		// sibling projects does not run while a session does (see Driver.beaconSessionTargets).
		r.presence = func() { d.beaconSessionTargets(ti, t) }
	}
	inv.OnClaim = r.observe
	go r.run(ctx, inv.ResumeClaim)
	return r
}

// observe receives one claim snapshot from an adapter's parser. Latest wins: if the renewer has not
// drained the previous snapshot yet, it is dropped in favour of this one - only the newest claim
// state can be true.
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

// apply folds one observed claim snapshot into the renewer's running state, logging the transitions
// that matter. The done-guard at the top bounds what can reach the reflector after stop(): stop()'s
// wait is bounded, so the goroutine can outlive it wedged in a Heartbeat call, and a snapshot
// queued before the stop and picked up after it must not revise the fleet row - the driver may
// already be into the next phase.
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

// run is the renewer's loop: beat every interval for as long as the current claim is HELD
// (Claim.Held: a task held and not yet settled by the session itself), follow the claim as the
// session settles one task and claims the next, and exit on cancellation or stop().
func (r *leaseRenewer) run(ctx context.Context, current harness.Claim) {
	defer close(r.stopped)
	// Testing seam: a test-provided tick channel subordinates the renewer to ticks the test fires by
	// hand, so a claim observation can be interleaved with a tick on a spine the test fully controls.
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
	// stoodDown disables the BEAT only. The tick keeps firing, so the presence beacon keeps flowing
	// every interval for the rest of the session: a plane that will not renew this lease may still
	// accept the fleet report, which is a different transport, and a healthy long session must not
	// read as a dead instance just because the heartbeat endpoint is refusing.
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
			// A claim transition queued by observe() wins over the tick, every time.
		drain:
			for {
				select {
				case c := <-r.claims:
					current = r.apply(c, current)
				default:
					break drain
				}
			}
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
				// A not-wired plane cannot renew anything, ever. Said once, then the BEAT stands down rather
				// than log the same refusal every tick for the rest of the session.
				log.Printf("%slease renewal unavailable: %v - standing down on the beat (the lease behaves as before CLA-358: it expires if the session outlives it); presence beacons continue every %s", r.prefix, err, r.interval)
				stoodDown = true
			case ctx.Err() != nil:
				return
			case errors.Is(err, plane.ErrRunLost):
				log.Printf("%sthe plane refused the lease renewal: %v - this run no longer holds the task, ending the session", r.prefix, err)
				stoodDown = true
				if r.onLost != nil {
					r.onLost(err)
				}
			default:
				failures++
				log.Printf("%slease renewal failed (%d in a row): %v", r.prefix, failures, err)
				if failures >= heartbeatGiveUp {
					// Same rule as ErrNotWired: no more beats, but the tick keeps carrying presence for the rest
					// of the session.
					log.Printf("%sgiving up on lease renewal after %d consecutive failures - the lease may expire mid-session; presence beacons continue every %s", r.prefix, failures, r.interval)
					stoodDown = true
				}
			}
		}
	}
}

// leaseLoss records what the renewer's onLost fired with.
type leaseLoss struct {
	mu  sync.Mutex
	val error
}

func (l *leaseLoss) record(err error) {
	l.mu.Lock()
	l.val = err
	l.mu.Unlock()
}

func (l *leaseLoss) cause() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.val
}

// renewalCause is why the renewer ended this session, nil when it did not. The plane's refusal wins
// over the derived cause, and an unjoined renewer fails closed: it may still say the run was lost,
// so the attempt is not salvageable.
func renewalCause(sctx context.Context, joined bool, lost error) error {
	switch {
	case lost != nil:
		return lost
	case sctx.Err() != nil:
		return context.Cause(sctx)
	case !joined:
		return errRenewalUnresolved
	}
	return nil
}

// errRenewalUnresolved is why an attempt is treated as run-lost when the renewer could not be
// joined: fail closed - no salvage, no handback - because whether the run still holds the task is
// unknown.
var errRenewalUnresolved = errors.New("the lease renewal could not be joined, so whether the run still holds the task is unknown")

// stop ends the renewer and waits - bounded, so a wedged renewal call cannot stall the attempt
// ladder - for the goroutine to exit.
func (r *leaseRenewer) stop() bool {
	if r == nil {
		return true
	}
	close(r.done)
	select {
	case <-r.stopped:
		return true
	case <-time.After(heartbeatBeatTimeout + 5*time.Second):
		return false
	}
}

// claimName is the human name for a claim in the renewer's log lines: the qualified ref when the
// plane supplied one, the bare task id when not.
func claimName(c harness.Claim) string {
	if c.Ref != "" {
		return c.Ref
	}
	return c.TaskID
}
