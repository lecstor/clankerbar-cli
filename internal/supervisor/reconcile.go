package supervisor

import (
	"context"
	"errors"
	"log"
	"os"
	"time"
)

// reconcile is one poll-and-apply pass: fetch the roster (or fall back to the
// last-known-good one when the plane is unreachable), refresh every instance's
// config from its entry, refuse what the allowlist refuses, and act — spawn
// the running, stop the stopped, drop the gone. It is idempotent by
// construction: unchanged state spawns nothing and writes nothing.
func (d *Supervisor) reconcile() error {
	if d.ctx.Err() != nil {
		// The fleet stop is landing: the select loop may have picked this poll
		// tick over ctx.Done, and acting on the roster now could spawn a child
		// stopAll is about to drain. A cancelled context reconciles to nothing.
		return nil
	}
	// The phase-5c replacement gate runs before the roster is even fetched:
	// the handoff acts on the fleet this process already holds, and an
	// unreachable plane must not postpone a replacement the operator asked
	// for. On unix it execs at once; on a no-exec platform it drains here.
	d.checkReplace()
	if d.ctx.Err() != nil {
		// The fleet stop landed while the replacement was in progress (the
		// no-exec drain or the conflict-session wait can hold this pass for up
		// to VerifyTimeout): the replacement aborted, and acting on the roster
		// now could spawn a child stopAll is about to drain. A cancelled
		// context reconciles to nothing, exactly as at the top of this pass.
		return nil
	}
	entries, err := d.roster.Fetch(d.ctx)
	if err != nil {
		if errors.Is(err, ErrNotWired) {
			return err
		}
		if d.entries == nil {
			// Cold start, plane unreachable: the last-known-good roster on
			// disk is the fallback — a network blip must not be an outage
			// (the proposal's offline seam, which phase 2b wrote materialized
			// configs to disk for).
			if cached := loadCachedRoster(d.cacheDir); len(cached) > 0 {
				log.Printf("plane unreachable at start (%v) - starting from the cached roster", err)
				d.entries = cached
			} else if len(d.instances) > 0 {
				// Adopted children keep the process alive with no roster to
				// reconcile them against; the next successful fetch picks
				// them up again.
				log.Printf("plane unreachable at start (%v) and no cached roster - the %d adopted instance(s) keep running without a roster and will be re-reconciled when the plane returns", err, len(d.instances))
				return nil
			} else {
				log.Printf("plane unreachable at start (%v) and no cached roster - nothing to supervise", err)
				return nil
			}
		} else {
			log.Printf("plane unreachable (%v) - reconciling against the last-known-good roster", err)
		}
		entries = d.entries
	} else {
		d.entries = entries
		writeCachedRoster(d.cacheDir, entries)
	}

	byName := make(map[string]*Instance, len(d.instances))
	for _, inst := range d.instances {
		byName[inst.name] = inst
	}
	live := make(map[string]bool, len(entries))
	for i := range entries {
		e := &entries[i]
		live[e.Name] = true
		if e.Placement == RosterPlacementRemote {
			// Decision 7: remote placement is not implemented, and the plane
			// refuses it at write time anyway — but the supervisor must not
			// fail on one, so it is ignored. Said once per entry, not once
			// per poll.
			if !d.loggedRemote[e.Name] {
				log.Printf("roster: ignoring entry %q - placement remote is not implemented (Decision 7)", e.Name)
				d.loggedRemote[e.Name] = true
			}
			continue
		}
		if err := checkEntry(e); err != nil {
			// Refused loudly, named in the log, never silently dropped. The
			// plane's own write-time gate should make this unreachable; the
			// supervisor's copy is the boundary's last line.
			log.Print(err)
			if inst := byName[e.Name]; inst != nil {
				inst.refused = true
			}
			continue
		}
		inst := byName[e.Name]
		if inst == nil {
			inst = &Instance{name: e.Name, entry: e}
			d.instances = append(d.instances, inst)
		}
		// The entry is the desired state: refresh the pointer every poll, so
		// an edit on the plane (a harness override, a project list, the
		// desired state) is what the next build reads. Holding the first
		// poll's entry would freeze the instance on the configuration that
		// created it.
		inst.entry = e
		inst.refused = false
		inst.removed = false
		inst.desired = e.DesiredState
		d.refreshInstance(inst)
	}

	// An instance whose entry is gone (deleted in the console) is a stop, not
	// a mystery: the operator removed its configuration, and the supervisor
	// must not keep the daemon it was configuration for.
	for _, inst := range d.instances {
		if inst.refused || live[inst.name] {
			continue
		}
		if !inst.removed {
			inst.removed = true
			log.Printf("%s: no longer on the roster - writing STOP and leaving it stopped", inst.name)
		}
	}

	// Act on the fleet.
	for _, inst := range d.instances {
		switch {
		case inst.refused || inst.removed || inst.desired == RosterDesiredStopped:
			d.ensureStopped(inst)
		default:
			d.ensureRunning(inst)
		}
	}

	// Reach the children that are already up with an edit (CLA-724). This runs
	// AFTER the act loop on purpose: a child stopped in this pass already
	// carries its STOP (so the refresh leaves it alone), and a child spawned in
	// it is compared against the very file its spawn just wrote (so an
	// unchanged fleet writes nothing).
	for _, inst := range d.instances {
		d.refreshMaterialized(inst)
	}

	// Drop instances that are gone and have already exited; the ones still
	// draining stay until their child's exit event arrives.
	kept := d.instances[:0]
	for _, inst := range d.instances {
		inst.mu.Lock()
		alive := inst.cmd != nil && !inst.exited
		inst.mu.Unlock()
		if (inst.removed || inst.refused) && !alive {
			continue
		}
		kept = append(kept, inst)
	}
	d.instances = kept

	// The fleet changed (or not): the status surface follows reconcile, so a
	// spawn, a stop, a flip or a removal is reflected within the same poll —
	// and an unchanged pass prints nothing.
	d.publishStatus()

	// The deterministic close-out (CLA-758) starts last, on configs this pass just
	// refreshed, and on its OWN goroutine: an approved task whose PR is clean is
	// merged and closed with no agent session, and everything it cannot finish
	// deterministically is left exactly where the agent close-out expects it. The
	// goroutine matters because a judgement can poll a `gh` read for 90 seconds and
	// the loop below reaps children, respawns and restarts — it must never wait
	// behind a close-out.
	d.startCloseOutPass()

	d.armRestart()
	return nil
}

// refreshInstance rebuilds one instance's effective config from its roster
// entry. The result is what the next spawn materializes. A build that fails:
//
//   - with ErrPolicyRefused (phase 2c) refuses the spawn — even on the last
//     known config, because that config's own policy file may be the one the
//     operator replaced — and the next poll retries the build;
//   - for any other reason (workdir derivation, validation, an unknown
//     harness) keeps the last-known-good: the previous config, or the
//     materialized config on disk, so a blip never takes a running daemon
//     down. Loud, never silent: the log says why the child will run on the
//     old config.
func (d *Supervisor) refreshInstance(inst *Instance) {
	fresh, sd, err := d.buildConfig(inst.entry)
	if err == nil {
		inst.mu.Lock()
		inst.cfg = fresh
		inst.stateDir = sd
		inst.mu.Unlock()
		inst.policyRefused = false
		return
	}
	if errors.Is(err, ErrPolicyRefused) {
		log.Printf("%s: refused: %v - retrying on the next poll", inst.name, err)
		inst.policyRefused = true
		return
	}
	// The state dir is deterministic from the entry name, so a cold start
	// whose build fails (the plane unreachable and the base config no longer
	// resolvable — a moved workdir, a corrupt edit) can still find the
	// last-known-good materialized config phase 2b wrote for exactly this.
	inst.mu.Lock()
	if inst.stateDir == "" {
		inst.stateDir = rosterStateDir(d.cacheDir, inst.entry.Name)
	}
	inst.mu.Unlock()
	if loaded := d.loadMaterialized(inst); loaded != nil {
		inst.mu.Lock()
		inst.cfg = loaded
		inst.mu.Unlock()
		log.Printf("%s: building its config failed (%v) - spawning on the last-known-good materialized config", inst.name, err)
		return
	}
	if inst.cfg != nil {
		log.Printf("%s: building its config failed (%v) - keeping the previous config", inst.name, err)
		return
	}
	log.Printf("%s: building its config failed (%v) and no last-known-good exists - not started", inst.name, err)
}

// ensureRunning brings one instance to its desired running state: a live child
// needs nothing (idempotent), a backoff respawn already scheduled needs
// nothing (the timer owns it), a refused or config-less instance starts
// nothing.
func (d *Supervisor) ensureRunning(inst *Instance) {
	if inst.policyRefused || inst.halted {
		// Refused at build time: the spawn must not fall back to the last
		// config, and the next poll retries the build. HALTed: the operator
		// planted the per-instance stop switch; the poll must not undo it.
		return
	}
	inst.mu.Lock()
	alive := inst.cmd != nil && !inst.exited
	pending := !inst.restartAt.IsZero()
	inst.mu.Unlock()
	if alive || pending {
		return
	}
	if d.limitStopHolds(inst) {
		return
	}
	if inst.cfg == nil {
		return // nothing to start yet: the build failed and no last-known-good exists
	}
	d.spawn(inst)
}

// ensureStopped brings one instance to its desired stopped state: STOP is
// written exactly once per live child (past the settle window, so the marker
// cannot be eaten by the child's startup consumption — CLA-491), and the
// child drains at its next phase seam or iteration boundary. Idempotent: a STOP already written
// writes nothing more.
func (d *Supervisor) ensureStopped(inst *Instance) {
	// A retry scheduled before the stop will never fire: respawnDue refuses to
	// spawn against a desired state that is no longer running. Drop the
	// timestamp here so the report cannot carry a `nextRetryAt` that passes
	// with no spawn — a phantom `backing_off` for up to the whole backoff
	// window.
	inst.restartAt = time.Time{}
	inst.mu.Lock()
	alive := inst.cmd != nil && !inst.exited
	since := time.Since(inst.aliveSince)
	dir := inst.stateDir
	inst.mu.Unlock()
	if !alive {
		return
	}
	if inst.stopRequested {
		return // STOP already written; waiting for the drain
	}
	if since < d.o.SettleBeforeStop {
		return // inside the startup window; the next poll writes it
	}
	if dir == "" {
		return
	}
	if _, err := os.Lstat(dir); err != nil {
		return // no state dir yet; the next poll writes it
	}
	// Set only when a marker the daemon can read is in place. A genuine write
	// failure leaves it unset, so the next reconcile retries instead of the
	// status card reporting a drain that is not happening (CLA-765 review
	// finding 1.review-1.3).
	if d.writeStop(inst, dir) != stopFailed {
		inst.stopRequested = true
	}
}

func (d *Supervisor) baseCtx() context.Context {
	if d.ctx != nil {
		return d.ctx
	}
	return context.Background()
}

// respawnDue starts every instance whose restart time has arrived.
func (d *Supervisor) respawnDue() {
	now := time.Now()
	for _, inst := range d.instances {
		if inst.restartAt.IsZero() || inst.restartAt.After(now) {
			continue
		}
		inst.restartAt = time.Time{}
		// The schedule was made when the instance still wanted a child; a poll
		// may have flipped it since (stopped, removed, refused, policy-refused,
		// HALTed) and the timer path must not spawn against the current desired
		// state any more than the reconcile path would. Dropping the stale
		// schedule is the idempotent move: the next reconcile re-evaluates the
		// instance from the roster.
		if inst.desired != RosterDesiredRunning || inst.removed || inst.refused || inst.policyRefused || inst.halted || inst.limitStopped {
			continue
		}
		if inst.entry == nil {
			// An adopted child whose exit landed before the plane was ever
			// reached has nothing to spawn from (buildConfig would have no
			// entry). Drop the schedule rather than retrying in a busy loop;
			// the next reconcile re-evaluates the instance once it can see a
			// roster again.
			inst.restartAt = time.Time{}
			log.Printf("%s: cannot respawn - it has no roster entry yet (the plane has not been reached since adoption); the next poll re-evaluates it", inst.name)
			continue
		}
		d.spawn(inst)
	}
}

// armRestart points the select loop's timer at the earliest pending restart,
// or disarms it when nothing is pending.
func (d *Supervisor) armRestart() {
	if d.timer != nil {
		d.timer.Stop()
		d.timerCh = nil
	}
	var next time.Time
	for _, inst := range d.instances {
		if !inst.restartAt.IsZero() && (next.IsZero() || inst.restartAt.Before(next)) {
			next = inst.restartAt
		}
	}
	if next.IsZero() {
		return
	}
	d.timer = time.NewTimer(time.Until(next))
	d.timerCh = d.timer.C
}
