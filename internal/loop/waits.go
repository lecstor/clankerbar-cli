package loop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
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
// again for as long as it takes.
func (d *Driver) supervisedWait(ctx context.Context, lim harness.Limit, a harness.Adapter, ph config.Phase, t Target, sofar spend) (tokens int, cost float64, stop bool) {
	tc := d.targetCfg(t)
	interval := d.cfg.PollInterval.OrDefault(30 * time.Minute)

	// waitingForReset says the stated reset was still ahead of us as this wait began; it flips once
	// the loop has slept across it.
	waitingForReset := !lim.ResetAt.IsZero() && time.Now().Before(lim.ResetAt)
	waitedOnReset := waitingForReset
	if waitingForReset {
		firstWake := probeWait(time.Now(), lim.ResetAt, interval, d.waitGrace)
		log.Printf("paused%s%s - waiting out the stated reset (first probe in ~%s, capped at the poll interval)", limitWindowSuffix(lim), resetSuffix(lim.ResetAt), firstWake.Round(time.Second))
	} else {
		log.Printf("paused%s%s — probing every %s for an early reset", limitWindowSuffix(lim), resetSuffix(lim.ResetAt), interval)
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

	d.beaconLimited(currentReset())

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
				d.beaconLimited(currentReset())
			}
		}
	}()
	defer func() {
		close(beaconStop)
		<-beaconDone
	}()

	// The FIRST probe's deadline is the only one measured from the wake: it is the wait's entry, and
	// probeWait is what aims it at a stated reset.
	nextProbe := time.Now().Add(probeWait(time.Now(), lim.ResetAt, interval, d.waitGrace))

	for {
		// The target's own account: charge() books this wait's probes as they land, so a stored overlay's
		// budget is judged on its project's spend and a local-file budget on the projects it governs.
		if v := d.budgetStop(sofar.ti, time.Since(sofar.start)); v.dim != "" {
			log.Printf("budget reached while paused: %s — ending the wait rather than waiting out the limit (tokens=%d cost=$%.2f elapsed=%s)",
				v.dim, v.tokens, v.cost, v.elapsed.Round(time.Second))
			return tokens, cost, true
		}
		// waitOrStopHalt stays the sleep, so STOP / RESTART / RELOAD - and, in this wait, HALT - keep
		// ending it promptly however long the limit lasts.
		if waitDur := time.Until(nextProbe); waitDur > 0 {
			if d.waitOrStopHalt(ctx, waitDur) {
				return tokens, cost, true
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
		inv, ierr := d.probeInvocation(t, tc, ph)
		if ierr != nil {
			// A refused probe spawn spends nothing and learns nothing about the limit, so it reads exactly
			// like any other probe error: log the named variable (the error carries it) and poll again next
			// interval.
			log.Printf("probe error: %v — will retry next interval", ierr)
			// A refusal is still a probe ATTEMPT: the next one is a full probeWait from the end of this one,
			// never sooner.
			nextProbe = time.Now().Add(probeWait(time.Now(), lim.ResetAt, interval, d.waitGrace))
			continue
		}
		got, err := a.Probe(ctx, inv)
		// The successor deadline is measured from HERE, the end of the attempt, and only here.
		nextProbe = time.Now().Add(probeWait(time.Now(), lim.ResetAt, interval, d.waitGrace))
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
				return tokens, cost, true
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
					return tokens, cost, true
				}
				log.Printf("paused, and the probe's own output cannot be read (%v) — stopping rather than polling on: its spend cannot be counted, so a token/cost ceiling can no longer be honoured. Rerun after the reset.", err)
				return tokens, cost, true
			}
			log.Printf("probe error: %v — will retry next interval", err)
			continue
		}
		if !got.Limit.Limited {
			log.Print("limit lifted — resuming")
			return tokens, cost, false
		}
		log.Print("still limited — continuing to wait")
	}
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
// next drain: backoff(n) in production, where n is noteResumedTransient's streak.
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
	return d.waitResponsive(ctx, dur, false)
}

// waitOrStopHalt is waitOrStop with one addition: a HALT marker ends the wait too. HALT is
// deliberately NOT consumed here — it is the operator's note, and the next run's start-up read
// reports it again without clearing it either.
func (d *Driver) waitOrStopHalt(ctx context.Context, dur time.Duration) bool {
	return d.waitResponsive(ctx, dur, true)
}

// waitResponsive is the shared body of waitOrStop and waitOrStopHalt.
func (d *Driver) waitResponsive(ctx context.Context, dur time.Duration, haltEndsWait bool) bool {
	const chunk = 3 * time.Second
	// Round(0) strips the monotonic reading, so wallStart measures REAL elapsed time.
	wallStart := time.Now().Round(0)
	end := time.Now().Add(dur)
	for {
		// The marker pass is serialised against every other consumer in this process: a parallel panel's
		// members all reach this loop, and two of them reading one RELOAD before either removed it would
		// apply the reload twice, from two goroutines.
		d.controlMu.Lock()
		stop := d.consumeWaitControls(haltEndsWait)
		d.controlMu.Unlock()
		if stop {
			return true
		}
		remaining := time.Until(end)
		if remaining <= 0 {
			if lost, stalled := sleepStall(dur, time.Now().Round(0).Sub(wallStart)); stalled {
				log.Printf("wait of %s took %s of wall clock — the machine was suspended for ~%s; timers are frozen while it sleeps, so an unattended run stalls silently (macOS: `caffeinate -i clankerbar run …`, and start it on AC — plugging in later does not wake a sleeping Mac)",
					dur, time.Now().Round(0).Sub(wallStart).Round(time.Second), lost.Round(time.Second))
			}
			return false
		}
		w := chunk
		if remaining < w {
			w = remaining
		}
		select {
		case <-ctx.Done():
			return true
		case <-time.After(w):
		}
	}
}

// consumeWaitControls reads and consumes the control markers waitOrStop honours: STOP, the restart
// family, and RELOAD (which is APPLIED here, in place, as control.go describes). HALT is READ but
// never consumed: it is the operator's note, so it is still there for the next run's start-up read.
func (d *Driver) consumeWaitControls(haltEndsWait bool) bool {
	if present, _ := d.readMarker("STOP"); present {
		_ = d.state.Remove("STOP")
		log.Print("STOP requested during wait — stopping")
		return true
	}
	if haltEndsWait {
		if present, msg := d.readMarker("HALT"); present {
			// Left in place on purpose: it is the operator's note, and the next run's start-up read is what
			// reports it again.
			log.Printf("HALT present during wait: %s — ending the wait; resolve and delete %s to resume", msg, filepath.Join(d.state.Path(), "HALT"))
			return true
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
		return true
	}
	if present, msg := d.readMarker(MarkerRestartNow); present {
		_ = d.state.Remove(MarkerRestartNow)
		if msg != "" {
			log.Printf("RESTART_NOW requested during wait: %s", msg)
		} else {
			log.Print("RESTART_NOW requested during wait")
		}
		d.restartRequested.Store(true)
		return true
	}
	if present, _ := d.readMarker(MarkerReload); present {
		_ = d.state.Remove(MarkerReload)
		log.Print("RELOAD requested during wait — reloading config; this wait keeps its schedule")
		d.applyReload()
	}
	return false
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
