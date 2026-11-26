package loop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// sleepStall reports how much real time a wait lost to system suspension, and whether that is worth
// saying out loud. The threshold is absolute rather than proportional on purpose: five minutes of
// unexplained wall clock is worth one log line whether it interrupted a one-minute idle poll or a
// thirty-minute limit wait, and a proportional rule would either spam the short waits or stay
// silent through the long ones.
func sleepStall(intended, wall time.Duration) (lost time.Duration, stalled bool) {
	const floor = 5 * time.Minute
	if lost = wall - intended; lost < floor {
		return 0, false
	}
	return lost, true
}

// waitPastBudget reports whether waiting for resetAt would cost more time than the run has left. An
// unknown reset (zero) is waited out as before, because the supervised wait polls for an EARLY lift
// and the limit may clear long before any stated time; with no ceiling set there is nothing to be
// past.
func waitPastBudget(resetAt time.Time, remaining time.Duration, bounded bool) (time.Time, bool) {
	if resetAt.IsZero() || !bounded {
		return time.Time{}, false
	}
	return resetAt, time.Until(resetAt) > remaining
}

// probeWait returns how long the paused loop sleeps before its next probe. An unknown or
// already-past reset falls back to the blind grid: codex states no reset at all, and after CLA-258
// claude parses one only from typed sources, so an unknown reset is normal rather than exceptional.
func probeWait(now, resetAt time.Time, interval, grace time.Duration) time.Duration {
	if resetAt.IsZero() || !now.Before(resetAt) {
		return interval
	}
	untilReset := resetAt.Add(grace).Sub(now)
	if untilReset < interval {
		return untilReset
	}
	return interval
}

// supervisedWait pauses on a usage limit, then polls for an early reset rather than sleeping
// blindly to the stated reset. The breaker is consulted HERE and not only by the caller, because
// this loop does not return to the caller while the limit persists: it waits, probes, and waits
// again for as long as it takes. routes is the resolved phase's configured route list (escalation
// applied): the per-route holds on the limited beacons describe THOSE routes, including ones a mark
// already dropped from the adapter's fallback chain.
//
// Everything the wait decides from is read LIVE: the config (a RELOAD that adds a route, removes a
// pricing window or shortens the poll interval unblocks the wait instead of sleeping out the old
// timer), the daemon-wide pause_windows gate (no probe spawns while one holds), and the per-route
// marks on every beacon (CLA-1024).
func (d *Driver) supervisedWait(ctx context.Context, lim harness.Limit, a harness.Adapter, ph config.Phase, routes []string, t Target, sofar spend) (tokens int, cost float64, stop bool) {
	// waitingForReset says the stated reset was still ahead of us as this wait began; it flips once
	// the loop has slept across it.
	waitingForReset := !lim.ResetAt.IsZero() && d.clock().Before(lim.ResetAt)
	waitedOnReset := waitingForReset
	if waitingForReset {
		firstWake := probeWait(time.Now(), lim.ResetAt, d.pollIntervalLive(), d.waitGrace)
		log.Printf("paused%s%s - waiting out the stated reset (first probe in ~%s, capped at the poll interval)", limitWindowSuffix(lim), resetSuffix(lim.ResetAt), firstWake.Round(time.Second))
	} else {
		log.Printf("paused%s%s — probing every %s for an early reset", limitWindowSuffix(lim), resetSuffix(lim.ResetAt), d.pollIntervalLive())
	}

	// CLA-728: the wait can outlast the plane's liveness window many times over, and a healthy daemon
	// that stops talking reads on the console as dead. Say `limited` on entry and keep saying it at
	// limitBeaconInterval; the next ordinary beacon (`iteration` or `idle`) is what replaces it on
	// resume, so there is no exit call.
	beaconInterval := d.limitBeaconInterval
	if beaconInterval <= 0 {
		// A Driver built by hand in a test carries the zero value; the default is the production window,
		// never "beacon in a tight loop".
		beaconInterval = 5 * time.Minute
	}

	// What the beacon carries is the harness's stated reset only while the wait still believes it. The
	// fallback below measures a stated reset false, and from that moment the console must read
	// "probing for a reset" rather than a countdown to an instant this loop already knows did not lift
	// the limit.
	var resetMu sync.Mutex
	beaconReset := lim.ResetAt
	currentReset := func() time.Time {
		resetMu.Lock()
		defer resetMu.Unlock()
		return beaconReset
	}
	dropBeaconReset := func() {
		resetMu.Lock()
		beaconReset = time.Time{}
		resetMu.Unlock()
	}

	// The route list a RELOAD may replace: the beacon goroutine reads it while the wait goroutine
	// adopts the live config's list, so it is guarded.
	var routesMu sync.Mutex
	entryRoutes := append([]string(nil), routes...)
	currentRoutes := func() []string {
		routesMu.Lock()
		defer routesMu.Unlock()
		return append([]string(nil), entryRoutes...)
	}
	setRoutes := func(next []string) {
		routesMu.Lock()
		entryRoutes = next
		routesMu.Unlock()
	}

	// CLA-1026/CLA-1024: every beacon derives what it says from the LIVE verdict — each route's mark,
	// pricing window and Stop sideline, the daemon-wide pause and the config in force now — so a
	// RELOAD is reflected by the next beacon instead of describing a set the wait no longer uses.
	currentState := func() fleet.State {
		return d.waitBeaconState(d.liveCfg(t), currentRoutes(), currentReset())
	}
	d.beaconWait(sofar.ti, currentState())

	// The re-beacon runs on its OWN goroutine, not as another wake in the probe loop below.
	beaconStop := make(chan struct{})
	beaconDone := make(chan struct{})
	go func() {
		defer close(beaconDone)
		tk := time.NewTicker(beaconInterval)
		defer tk.Stop()
		for {
			select {
			case <-beaconStop:
				return
			case <-tk.C:
				d.beaconWait(sofar.ti, currentState())
			}
		}
	}()
	defer func() {
		close(beaconStop)
		<-beaconDone
	}()

	// A phase whose every configured route is already Stop-blocked has nothing to wait for: a budget
	// or credit exhaustion does not lift on its own, so the run ends for the operator instead of
	// probing dead subscriptions (CLA-1024).
	if refusal := d.stopRefusalFor(currentRoutes(), ph.Label(0)); refusal != nil {
		logStopBlocked(ph, refusal)
		d.writeStopBlocked(refusal)
		return tokens, cost, true
	}

	// The FIRST probe's deadline is the wait's entry, and it takes the same route hint as every
	// successor: a route window that ends before the poll interval is when the usable route comes
	// back, so sleeping the whole interval from the entry is the same 29-minute idle CLA-986 fixed
	// for successors (CLA-1024).
	nextProbe := time.Now().Add(d.waitWakeDelay(d.liveCfg(t), ph, lim))
	pauseSaid := false

	for {
		// The target's own account: charge() books this wait's probes as they land, so a stored overlay's
		// budget is judged on its project's spend and a local-file budget on the projects it governs.
		if v := d.budgetStop(sofar.ti, time.Since(sofar.start)); v.dim != "" {
			log.Printf("budget reached while paused: %s — ending the wait rather than waiting out the limit (tokens=%d cost=$%.2f elapsed=%s)",
				v.dim, v.tokens, v.cost, v.elapsed.Round(time.Second))
			return tokens, cost, true
		}
		// CLA-1024: a daemon-wide pause_windows window spawns no probe while it holds. A probe is a
		// paid session, and a pause scheduled to keep the machine (or the invoice) quiet would
		// otherwise be charged for every poll interval. Sleep in bounded chunks so STOP / RESTART /
		// HALT / RELOAD stay responsive and the window's end is never overslept.
		if paused, pauseUntil, pw := d.pauseState(); paused {
			if !pauseSaid {
				pauseSaid = true
				log.Printf("paused%s: a scheduled pause window is active until %s (%s) — spawning no probes until it ends",
					limitWindowSuffix(lim), pauseUntil.Format("Mon 15:04 MST"), pw.Describe())
				d.beaconWait(sofar.ti, currentState())
			}
			// waitOrStopHalt stays the sleep, so STOP / RESTART / RELOAD - and, in this wait, HALT - keep
			// ending it promptly however long the limit lasts.
			if d.waitOrStopHalt(ctx, d.pauseChunk(pauseUntil)) {
				return tokens, cost, true
			}
			continue
		}
		if pauseSaid {
			pauseSaid = false
			log.Print("scheduled pause ended — resuming limit probes")
			d.beaconWait(sofar.ti, currentState())
		}
		if waitDur := time.Until(nextProbe); waitDur > 0 {
			out := d.waitOrStopHaltReload(ctx, waitDur)
			if out.stop {
				return tokens, cost, true
			}
			if out.reload {
				// CLA-1024: the config this wait probes with has just been replaced. Re-read it and
				// re-aim the deadline from now rather than sleeping out a timer the old config set.
				tc := d.liveCfg(t)
				if live, _ := tc.ModelRoutesForPhase(ph); len(live) > 0 {
					// A RELOAD is the operator saying "use this config now", so its route list wins
					// over the entry one when it resolves any (the entry list can be an
					// escalation-resolved set the configured phase does not name).
					setRoutes(live)
				}
				nextProbe = time.Now().Add(d.reloadWakeDelay(tc, currentRoutes()))
				d.beaconWait(sofar.ti, currentState())
				continue
			}
		}
		// Probe time. A stated reset is the provider's claim, not a guarantee, so crossing it mid-wait
		// does NOT resume the run the way a blind-grid wake used to: it falls back to interval polling,
		// and the very next probe is what confirms or refutes the claim.
		if !lim.ResetAt.IsZero() && !time.Now().Before(lim.ResetAt) && waitingForReset {
			log.Print("stated reset passed - falling back to interval polling (a stated reset is a claim, not a guarantee)")
			waitingForReset = false
			// The loop has just measured the claim false: stop repeating it to the console.
			dropBeaconReset()
		} else if !waitedOnReset && !lim.ResetAt.IsZero() && !time.Now().Before(lim.ResetAt) {
			log.Print("stated reset already past — resuming")
			return tokens, cost, false
		}
		// Every wake probes with the config in force NOW, never one captured when the wait began.
		tc := d.liveCfg(t)
		got, cst, verdict, end := d.probeWake(ctx, a, t, tc, ph, sofar)
		tokens += got
		cost += cst
		switch verdict {
		case wakeStop:
			return tokens, cost, true
		case wakeLifted:
			return tokens, cost, false
		}
		// The successor deadline is measured from HERE, the end of the wake's last attempt, and only
		// here. A refused spawn is still an attempt, so the next is a full probeWait away — unless a
		// blocked route clears before that, which is the instant to wake at (CLA-986). A walk that
		// ran out of routes may also have ended with a route ALREADY clear, which the hint declines
		// (there is no future instant): that route is probed one grace out rather than a whole
		// interval later (CLA-988). A probe error must keep the full interval, or a failing probe
		// that marks nothing would tight-loop.
		delay := probeWait(time.Now(), lim.ResetAt, d.pollIntervalLive(), d.waitGrace)
		if verdict == wakeStillLimited && end == wakeExhausted && d.routeClearNow(tc, ph) {
			// Never lengthen the deadline: a stated reset or a short poll interval may already be
			// sooner than one grace. A Driver built by hand in a test carries the zero value, and a
			// zero grace would schedule the next paid probe with no sleep at all.
			grace := d.waitGrace
			if grace <= 0 {
				grace = time.Minute
			}
			delay = min(delay, grace)
		}
		if hint, ok := d.routeWakeHint(tc, ph); ok && hint < delay {
			delay = hint
		}
		nextProbe = time.Now().Add(delay)
	}
}

// pollIntervalLive is the configured probe interval, read fresh and under the lock a RELOAD swaps the
// config with. Read per deadline: a shortened interval applies to the wait in progress.
func (d *Driver) pollIntervalLive() time.Duration {
	d.controlMu.Lock()
	cfg := d.cfg
	d.controlMu.Unlock()
	if cfg == nil {
		return defaultRouteMarkInterval
	}
	return cfg.PollInterval.OrDefault(defaultRouteMarkInterval)
}

// pauseChunk bounds one sleep taken inside a daemon-wide pause: the poll interval at most, so the
// gate is re-read often enough to notice the window ending, and a floor so a fake clock sitting on
// the window's end cannot spin.
func (d *Driver) pauseChunk(until time.Time) time.Duration {
	wait := d.pollIntervalLive()
	if remaining := until.Sub(d.clock()); remaining < wait {
		wait = remaining
	}
	if wait <= 0 {
		wait = time.Millisecond
	}
	return wait
}

// waitWakeDelay is how long the wait sleeps from NOW before its next probe: the stated reset or the
// poll interval, shorter when a route the wait left blocked clears sooner (CLA-986). The entry sleep
// takes this same delay (CLA-1024), so a wait entered at 13:45 with a route windowed until 14:00
// wakes at 14:00 rather than 14:15.
func (d *Driver) waitWakeDelay(tc *config.Config, ph config.Phase, lim harness.Limit) time.Duration {
	delay := probeWait(time.Now(), lim.ResetAt, d.pollIntervalLive(), d.waitGrace)
	if hint, ok := d.routeWakeHint(tc, ph); ok && hint < delay {
		delay = hint
	}
	return delay
}

// reloadWakeDelay is the deadline after a RELOAD: zero when a route is clear NOW, because the config
// just admitted one (an added route, a removed window) and the operator's edit is what the next probe
// should act on. A still-blocked wait takes the ordinary delay measured from now, so the reload
// never fires an extra paid probe against an already-marked route.
func (d *Driver) reloadWakeDelay(tc *config.Config, routes []string) time.Duration {
	if d.routeClearNowFor(tc, routes) {
		return 0
	}
	if hint, ok := d.routeWakeHintFor(tc, routes); ok {
		return hint
	}
	return d.pollIntervalLive()
}

// wakeVerdict is what one wake's probes concluded.
type wakeVerdict int

const (
	wakeStillLimited wakeVerdict = iota
	wakeLifted
	wakeStop
)

// wakeEnd is why a still-limited wake stopped probing. It decides the successor deadline: only a
// walk that ran out of routes can leave one usable NOW, and that route is worth re-probing one
// grace out; a probe error marks nothing, so the full interval must stand or it cannot settle. The
// value is meaningful only when the verdict is wakeStillLimited.
type wakeEnd int

const (
	wakeExhausted   wakeEnd = iota // every route was probed, tried, marked, or window-blocked
	wakeProbeFailed                // a probe could not be spawned, or its own output could not be read
)

// probeWake runs one wake's probes. A phase with a route list walks it: a route that probes limited
// is marked exactly as a session limit marks it (stated reset, else the mark interval), and the next
// unmarked routable route is probed in the same wake, so a route with no stated reset cannot mask
// the routes behind it. The walk ends at the first unlimited probe, or when the next route is
// already marked, tried, or window-blocked; at most one probe per route per wake. A route that
// reports a Stop-class limit is SIDELINED for the rest of the process (CLA-1024): it has no reset
// to poll for, so the walk moves on and never comes back to it. It also reports whether that end was
// walk exhaustion or a probe failure, which decides the successor deadline.
func (d *Driver) probeWake(ctx context.Context, a harness.Adapter, t Target, tc *config.Config, ph config.Phase, sofar spend) (tokens int, cost float64, verdict wakeVerdict, end wakeEnd) {
	routes, _ := tc.ModelRoutesForPhase(ph)
	tried := map[string]bool{}
	for {
		// Nothing to wait for: every configured route is Stop-blocked, so no probe can lift anything.
		// The run ends cleanly for the operator rather than polling dead subscriptions forever.
		if refusal := d.stopRefusalFor(routes, ph.Label(0)); refusal != nil {
			logStopBlocked(ph, refusal)
			d.writeStopBlocked(refusal)
			return tokens, cost, wakeStop, wakeExhausted
		}
		// A daemon-wide pause window that opened mid-walk stops the rest of the walk: the remaining
		// probes are paid sessions, and the wait's own gate re-reads the window before the next wake.
		if paused, _, _ := d.pauseState(); paused {
			log.Print("scheduled pause active — not probing further in this wake")
			return tokens, cost, wakeStillLimited, wakeExhausted
		}
		if len(tried) > 0 {
			// Each walk probe is another paid session, so the ceiling is re-read before it, not only at the wake.
			if v := d.budgetStop(sofar.ti, time.Since(sofar.start)); v.dim != "" {
				log.Printf("budget reached while paused: %s — ending the wait rather than waiting out the limit (tokens=%d cost=$%.2f elapsed=%s)",
					v.dim, v.tokens, v.cost, v.elapsed.Round(time.Second))
				return tokens, cost, wakeStop, wakeExhausted
			}
		}
		inv, ierr := d.probeInvocation(t, tc, ph)
		if ierr != nil {
			var blocked *routeWindowBlocked
			if errors.As(ierr, &blocked) {
				// A window is a scheduled condition, not a probe error: nothing spawned, and the next
				// wake is aimed at the window's end (routeWakeHint) — so name the routes it holds.
				// The disjunction is load-bearing: routableRoutesAt drops usage-marked routes before
				// the window filter, so a route from an earlier wake can still carry a mark.
				if len(tried) > 0 {
					log.Printf("still limited on %s, and every other route is usage-marked or window-blocked (%v) — continuing to wait", strings.Join(sortedKeys(tried), ", "), ierr)
				} else {
					log.Printf("no route for the phase may start: each is usage-marked or window-blocked (%v) — continuing to wait", ierr)
				}
				return tokens, cost, wakeStillLimited, wakeExhausted
			}
			var stopped *routeStopBlocked
			if errors.As(ierr, &stopped) {
				logStopBlocked(ph, stopped)
				d.writeStopBlocked(stopped)
				return tokens, cost, wakeStop, wakeExhausted
			}
			// A refused probe spawn spends nothing and learns nothing about the limit, so it reads exactly
			// like any other probe error: log the named variable (the error carries it) and poll again next
			// interval.
			log.Printf("probe error: %v — will retry next interval", ierr)
			return tokens, cost, wakeStillLimited, wakeProbeFailed
		}
		if len(tried) > 0 {
			if _, stopped := d.routeStopped(inv.Model); tried[inv.Model] || stopped {
				log.Printf("still limited on every routable route — continuing to wait")
				return tokens, cost, wakeStillLimited, wakeExhausted
			}
			if until, marked := d.routeMarkExpiry(inv.Model); marked && time.Now().Before(until) {
				log.Printf("still limited on every routable route — continuing to wait")
				return tokens, cost, wakeStillLimited, wakeExhausted
			}
		}
		got, err := a.Probe(ctx, inv)
		// Count the probe BEFORE reading its verdict, and before the error branch: a probe that failed
		// still spawned the harness and still spent.
		tokens += got.Tokens
		cost += got.CostUSD
		// The probe spawned on the TARGET's effective harness above (invocationOn with tc.HarnessFor), so
		// its spend is booked under that same name — keying on the base harness would bill an
		// overlay-redirected phase to the wrong per-harness account.
		d.charge(sofar.ti, tc.HarnessFor(ph), got.Tokens, got.CostUSD)
		if err != nil {
			if ctx.Err() != nil {
				return tokens, cost, wakeStop, wakeProbeFailed
			}
			// A probe whose own output could not be read is not a blip that clears itself: it reports no
			// verdict AND no spend, so a wait that keeps polling on it re-spawns a paid session every
			// interval against a ceiling that can never see the cost — the one shape this loop's breaker
			// exists to end.
			if errors.Is(err, harness.ErrUntrusted) && tc.Budget.CountsSpendFor(tc.HarnessFor(ph)) {
				// The ceiling this breaks may be the target's own stored budget, in which case only that
				// project stops draining and the siblings keep going; the local pool keeps the daemon-wide
				// stop.
				if d.exhaustUnmeasurable(sofar.ti, fmt.Sprintf("the pause probe's spend cannot be read (%v)", err)) {
					return tokens, cost, wakeStop, wakeProbeFailed
				}
				log.Printf("paused, and the probe's own output cannot be read (%v) — stopping rather than polling on: its spend cannot be counted, so a token/cost ceiling can no longer be honoured. Rerun after the reset.", err)
				return tokens, cost, wakeStop, wakeProbeFailed
			}
			log.Printf("probe error: %v — will retry next interval", err)
			return tokens, cost, wakeStillLimited, wakeProbeFailed
		}
		if !got.Limit.Limited {
			if len(tried) > 0 {
				log.Printf("limit lifted on %s — resuming", inv.Model)
			} else {
				log.Print("limit lifted — resuming")
			}
			return tokens, cost, wakeLifted, wakeExhausted
		}
		// CLA-1024: a Stop-class limit has no reset to poll for. A route with other routes behind it is
		// sidelined and the walk moves on; when it was the last route that could start, the wait stops.
		if got.Limit.Stop {
			d.stopRoute(inv.Model, got.Limit)
			tried[inv.Model] = true
			if len(routes) == 0 {
				// The legacy single-route phase: the model just probed is the only route there is.
				log.Printf("probe on %s hit a Stop-class limit (%s) — no reset to wait for, stopping (resume once resolved)",
					inv.Model, limitReason(got.Limit))
				d.writeLimitStop(limitStopLine(got.Limit, inv.Model, ""))
				return tokens, cost, wakeStop, wakeExhausted
			}
			log.Printf("probe on %s hit a Stop-class limit (%s) — sidelining it until RELOAD or restart rather than re-probing a dead subscription",
				inv.Model, limitReason(got.Limit))
			continue
		}
		if len(routes) == 0 || inv.Model == "" || len(tried) >= len(routes) {
			log.Print("still limited — continuing to wait")
			return tokens, cost, wakeStillLimited, wakeExhausted
		}
		tried[inv.Model] = true
		until := got.Limit.ResetAt
		if !until.After(time.Now()) {
			until = time.Now().Add(d.routeMarkInterval())
		}
		d.markRouteLimited(inv.Model, until)
		// An existing later mark wins, so name the stored one.
		if stored, ok := d.routeMarkExpiry(inv.Model); ok {
			until = stored
		}
		log.Printf("probe on %s still limited — retrying it after %s", inv.Model, routeRetryLabel(until))
	}
}

// sortedKeys is a set's members in a stable order, for a log line.
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// backoff is the exponential delay before transient retry n (1-based): 30s, 60s, 120s, ..., capped
// at RetryCap. Computed by doubling to avoid shift overflow.
func (d *Driver) backoff(n int) time.Duration {
	ceil := d.cfg.RetryCap.OrDefault(300 * time.Second)
	b := 30 * time.Second
	for i := 1; i < n; i++ {
		b *= 2
		if b >= ceil {
			return ceil
		}
	}
	if b > ceil {
		return ceil
	}
	return b
}

// resumedTransientRetryBackoff is the in-claim backoff ladder for a resumed phase's transient
// retry: short on purpose, because the claim is still held and the run-level CLA-662 backoff
// follows if the provider keeps failing.
var resumedTransientRetryBackoff = []time.Duration{15 * time.Second, 45 * time.Second}

// inClaimTransientRetryDelay is the wait before in-claim retry n (1-based), with the test seam
// overriding it so the ladder can be driven without sleeping.
func (d *Driver) inClaimTransientRetryDelay(n int) time.Duration {
	if d.resumedTransientRetryWait != nil {
		return d.resumedTransientRetryWait(n)
	}
	if n < 1 {
		n = 1
	}
	if n > len(resumedTransientRetryBackoff) {
		n = len(resumedTransientRetryBackoff)
	}
	return resumedTransientRetryBackoff[n-1]
}

// noteResumedTransient records one more consecutive transient sequence-end on key and returns the
// new streak — the backoff rung the run owes after the sequence's handback. The map is lazily
// built: a run with no resumed-phase blip never allocates it.
func (d *Driver) noteResumedTransient(key resumedTransientKey) int {
	if d.resumedTransients == nil {
		d.resumedTransients = map[resumedTransientKey]int{}
	}
	d.resumedTransients[key]++
	return d.resumedTransients[key]
}

// clearResumedTransient resets key's streak: a resumed phase on that target+harness ended some
// other way, so the next blip starts the ladder over. Deleting from a nil map is a no-op — a run
// that never had a streak has nothing to break.
func (d *Driver) clearResumedTransient(key resumedTransientKey) {
	delete(d.resumedTransients, key)
}

// resumedTransientDuration is the wait a resumed phase's transient sequence-end owes before the
// next drain: backoff(n) in production, where n is noteResumedTransient's streak, with
// resumedTransientWait overriding it so tests can drive the ladder without sleeping.
func (d *Driver) resumedTransientDuration(n int) time.Duration {
	if d.resumedTransientWait != nil {
		return d.resumedTransientWait(n)
	}
	return d.backoff(n)
}

func retryLabel(n, max int) string {
	if max > 0 {
		return fmt.Sprintf("retry %d/%d", n, max)
	}
	return fmt.Sprintf("retry %d", n)
}

// waitOrStop waits up to dur, but stays responsive to a STOP marker (consuming it) and to context
// cancellation. CLA-461: the restart/reload markers get the same responsiveness, so a control
// request is never stuck behind a multi-hour idle poll or a supervised limit wait.
func (d *Driver) waitOrStop(ctx context.Context, dur time.Duration) bool {
	return d.waitResponsive(ctx, dur, false, false).stop
}

// waitOrStopHalt is waitOrStop with one addition: a HALT marker ends the wait too. HALT is
// deliberately NOT consumed here — it is the operator's note, and the next run's start-up read
// reports it again without clearing it either.
func (d *Driver) waitOrStopHalt(ctx context.Context, dur time.Duration) bool {
	return d.waitResponsive(ctx, dur, true, false).stop
}

// waitOrStopHaltReload is waitOrStopHalt for the supervised wait: a RELOAD also ends the sleep, so
// the wait re-reads the config and re-aims its deadline instead of sleeping out a timer the config
// no longer sets (CLA-1024).
func (d *Driver) waitOrStopHaltReload(ctx context.Context, dur time.Duration) waitOutcome {
	return d.waitResponsive(ctx, dur, true, true)
}

// waitOutcome says how a responsive sleep ended. reload is reported only to a caller that asked for
// it (reloadEndsWait): the supervised wait re-reads the config and re-aims its deadline at once,
// while every other sleep applies the reload in place and keeps sleeping.
type waitOutcome struct {
	stop   bool
	reload bool
}

// waitResponsive is the shared body of waitOrStop and waitOrStopHalt.
func (d *Driver) waitResponsive(ctx context.Context, dur time.Duration, haltEndsWait, reloadEndsWait bool) waitOutcome {
	const chunk = 3 * time.Second
	// Round(0) strips the monotonic reading, so wallStart measures REAL elapsed time.
	wallStart := time.Now().Round(0)
	end := time.Now().Add(dur)
	for {
		// The marker pass is serialised against every other consumer in this process: a parallel panel's
		// members all reach this loop, and two of them reading one RELOAD before either removed it would
		// apply the reload twice, from two goroutines.
		d.controlMu.Lock()
		stop, reloaded := d.consumeWaitControls(haltEndsWait)
		d.controlMu.Unlock()
		if stop {
			return waitOutcome{stop: true}
		}
		if reloaded && reloadEndsWait {
			return waitOutcome{reload: true}
		}
		remaining := time.Until(end)
		if remaining <= 0 {
			if lost, stalled := sleepStall(dur, time.Now().Round(0).Sub(wallStart)); stalled {
				log.Printf("wait of %s took %s of wall clock — the machine was suspended for ~%s; timers are frozen while it sleeps, so an unattended run stalls silently (macOS: `caffeinate -i clankerbar run …`, and start it on AC — plugging in later does not wake a sleeping Mac)",
					dur, time.Now().Round(0).Sub(wallStart).Round(time.Second), lost.Round(time.Second))
			}
			return waitOutcome{}
		}
		w := chunk
		if remaining < w {
			w = remaining
		}
		select {
		case <-ctx.Done():
			return waitOutcome{stop: true}
		case <-time.After(w):
		}
	}
}

// consumeWaitControls reads and consumes the control markers waitOrStop honours: STOP, the restart
// family, and RELOAD (which is APPLIED here, in place, as control.go describes). HALT is READ but
// never consumed: it is the operator's note, so it is still there for the next run's start-up read.
// reloaded reports a RELOAD that was applied, for the one caller that re-aims its deadline on it.
func (d *Driver) consumeWaitControls(haltEndsWait bool) (stop, reloaded bool) {
	if present, _ := d.readMarker("STOP"); present {
		_ = d.state.Remove("STOP")
		log.Print("STOP requested during wait — stopping")
		return true, false
	}
	if haltEndsWait {
		if present, msg := d.readMarker("HALT"); present {
			// Left in place on purpose: it is the operator's note, and the next run's start-up read is what
			// reports it again.
			log.Printf("HALT present during wait: %s — ending the wait; resolve and delete %s to resume", msg, filepath.Join(d.state.Path(), "HALT"))
			return true, false
		}
	}
	if present, msg := d.readMarker(MarkerRestart); present {
		_ = d.state.Remove(MarkerRestart)
		if msg != "" {
			log.Printf("RESTART requested during wait: %s", msg)
		} else {
			log.Print("RESTART requested during wait")
		}
		d.restartRequested.Store(true)
		return true, false
	}
	if present, msg := d.readMarker(MarkerRestartNow); present {
		_ = d.state.Remove(MarkerRestartNow)
		if msg != "" {
			log.Printf("RESTART_NOW requested during wait: %s", msg)
		} else {
			log.Print("RESTART_NOW requested during wait")
		}
		d.restartRequested.Store(true)
		return true, false
	}
	if present, _ := d.readMarker(MarkerReload); present {
		_ = d.state.Remove(MarkerReload)
		log.Print("RELOAD requested during wait — reloading config")
		d.applyReload()
		return false, true
	}
	return false, false
}

// readMarker reports whether a control marker file exists, and its first line. A marker is the
// operator's switch, so it is read through the state dir handle: a symlink at the name is refused
// rather than followed, and the read is capped.
func (d *Driver) readMarker(name string) (bool, string) {
	b, err := d.state.ReadFile(name)
	if err != nil {
		return false, ""
	}
	line := strings.TrimSpace(string(b))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return true, line
}

// randomTail is the unguessable part of an iteration log's name. crypto/rand, not math/rand: the
// whole point is that the session whose transcript this is cannot predict it, and a seeded PRNG in
// a daemon it can watch start is not unpredictable.
func randomTail() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func resetSuffix(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return " (stated reset " + formatReset(t) + ")"
}

// formatReset renders a stated reset for a log line in the machine's local zone, with date and zone
// name. Providers send UTC, and a weekly window runs up to 7 days out, so a bare weekday and clock
// read as the wrong hour or the wrong week.
func formatReset(t time.Time) string {
	return t.Local().Format("Mon 2 Jan 15:04 MST")
}

// limitReason renders a limit's reason for a log line, defaulting when a harness leaves it blank.
// The window rides along when the adapter named one: a multi-day weekly wait should say which cap
// it is waiting out, not just "usage limit".
func limitReason(lim harness.Limit) string {
	reason := lim.Reason
	if reason == "" {
		reason = "usage limit"
	}
	return reason + limitWindowSuffix(lim)
}

// limitWindowSuffix names the usage window for a log line that already says what it is doing, or ""
// when the adapter named none.
func limitWindowSuffix(lim harness.Limit) string {
	if lim.Window == "" {
		return ""
	}
	return " (" + lim.Window + " window)"
}
