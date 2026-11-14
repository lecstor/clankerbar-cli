//go:build !unix

package supervisor

// The no-exec(2) replacement path. Without exec there is nothing for the
// children to survive — the process image cannot be replaced in place — so the
// REPLACE marker keeps its original contract here: drain the fleet, then let
// the unit (launchd/systemd) restart the supervisor from the launch path. The
// adopt-on-exec path in adopt.go is the unix contract; this file exists so a
// marker still means "replace me" on the platforms where a unit owns the
// process.
//
// The drain is the same STOP-marker drain the fleet stop performs: each child
// exits at its next phase seam or iteration boundary (an in-flight session
// finishes), nothing is ever killed, and the wait is bounded by VerifyTimeout
// so a child that never drains HALTS the replacement rather than hanging it.

import (
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"
)

// replaceByDrain drains the fleet and then execs the launch binary. It runs
// only on a platform without exec(2), where the exec returns an error and the
// unit owns the actual restart.
func (d *Supervisor) replaceByDrain() {
	log.Print("replace: REPLACE honoured - writing STOP to every child; each drains at its next phase seam or iteration boundary")
	// The drain is a stop the supervisor itself ordered, so for its duration
	// every row it reports says stopped (not exited, the crash vocabulary) —
	// this flag, deliberately not d.stopping, whose meaning ("the supervisor
	// is exiting") must not stick when a timed-out drain resumes the loop.
	d.replacing = true
	d.reportStatus()
	stuck := d.drainForReplace()
	d.replacing = false
	if stuck != "" {
		if d.ctx.Err() != nil {
			// The fleet stop landed mid-drain: the supervisor exits instead
			// of replacing itself, and the stop owns the children now — the
			// marker is already consumed, so nothing re-triggers.
			log.Printf("replace: %s", stuck)
			return
		}
		log.Printf("replace: %s - the fleet is not drained, so no replacement happens; the children that stopped are respawned, and once the stuck child is fixed, re-running `clankerbar supervise replace` is safe", stuck)
		return
	}
	if d.ctx.Err() != nil {
		return // the fleet stop landed mid-drain; the select loop owns the stop now
	}
	log.Print("replace: the fleet is drained - replacing myself in place with the launch binary")
	if err := d.o.execSelf(); err != nil {
		log.Printf("replace: the replacement exec failed (%v) - the supervisor keeps running and the fleet is respawned; fix the launch path and re-run `clankerbar supervise replace`", err)
	}
}

// drainForReplace stops every live child and waits for the fleet to drain —
// the same STOP-marker mechanics stopAll performs (each child drains at its
// next phase seam or iteration boundary; nothing is ever killed) — bounded by
// VerifyTimeout so a child that never drains HALTS the replacement instead of
// hanging it, and aborted by context cancellation so the operator's stop
// lands as the fleet stop it is.
//
// The returned string is empty when every child exited; otherwise it is a
// line naming what the drain was still waiting for, which the caller logs.
// Every exit event consumed here also marks its instance exited (the flag
// onExit would set), because the replacement may be called off after all —
// a timed-out drain resumes the loop, and the children that did drain must
// read as dead so the next reconcile respawns them.
func (d *Supervisor) drainForReplace() string {
	timeout := d.o.VerifyTimeout
	if timeout <= 0 {
		timeout = defaultVerifyTimeout
	}
	deadline := time.Now().Add(timeout)
	done := map[*Instance]bool{}      // child exited (event consumed) or never spawned
	stopped := map[*Instance]bool{}   // a readable STOP marker is in place; awaiting the child's exit
	attempted := map[*Instance]bool{} // writeStop already ran once for this child this drain
	waitStart := time.Now()
	lastLog := waitStart
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	// A conflict-only session is fleet work too (CLA-759): it is not a child
	// process, so the exit loop cannot see it, and exec'ing over it would kill
	// a session holding a task claim. Wait for it as well, within the same
	// bound.
	for len(done) < len(d.instances) || d.conflictBusy.Load() {
		select {
		case ev := <-d.exits:
			ev.inst.mu.Lock()
			ev.inst.exited = true
			ev.inst.exitErr = ev.err
			ev.inst.exitAt = ev.at
			ev.inst.mu.Unlock()
			done[ev.inst] = true
			d.reportStatus()
		case <-d.ctx.Done():
			return "the fleet stop landed mid-drain - the children keep draining and the supervisor exits instead of replacing itself"
		case <-tick.C:
		}
		if now := time.Now(); !now.Before(deadline) {
			return fmt.Sprintf("the fleet did not drain within %s - still waiting for: %s", timeout, drainStuck(d, done))
		}
		if now := time.Now(); now.Sub(lastLog) >= d.o.StopLogEvery {
			lastLog = now
			log.Printf("replace: still waiting after %s: %s - children stop at their next phase seam or iteration boundary and a conflict-only session finishes on its own; the supervisor keeps waiting rather than killing them",
				time.Since(waitStart).Round(time.Second), drainStuck(d, done))
		}
		for _, inst := range d.instances {
			if done[inst] || stopped[inst] {
				continue
			}
			inst.mu.Lock()
			alive := inst.cmd != nil && !inst.exited
			since := time.Since(inst.aliveSince)
			dir := inst.stateDir
			inst.mu.Unlock()
			if !alive {
				done[inst] = true
				continue
			}
			if since < d.o.SettleBeforeStop {
				continue // inside the startup window; the next tick writes it
			}
			if _, err := os.Lstat(dir); err != nil {
				continue // no state dir yet; the next tick writes it
			}
			// writeStop both writes the marker and checks one already there,
			// so the drain does not have to know why a STOP is present — but
			// it only counts a marker the daemon can read as pending. One
			// attempt per child: a failure already logged loudly, and retrying
			// every 50ms would bury that line; the timeout bounds the wait and
			// the "still waiting" log names the child.
			if !attempted[inst] {
				attempted[inst] = true
				if d.writeStop(inst, dir) != stopFailed {
					stopped[inst] = true
				}
			}
		}
	}
	return ""
}

// stuckNames lists the instances a drain is still waiting on, sorted.
func stuckNames(d *Supervisor, done map[*Instance]bool) string {
	var stuck []string
	for _, inst := range d.instances {
		if !done[inst] {
			stuck = append(stuck, inst.name)
		}
	}
	sort.Strings(stuck)
	return strings.Join(stuck, ", ")
}

// drainStuck is stuckNames plus the conflict-only session (CLA-759) when one is
// running: it is fleet work the exit loop cannot see, and a drain that execs
// over it would kill a session holding a task claim.
func drainStuck(d *Supervisor, done map[*Instance]bool) string {
	stuck := stuckNames(d, done)
	if !d.conflictBusy.Load() {
		return stuck
	}
	if stuck == "" {
		return "a conflict-only session"
	}
	return stuck + ", a conflict-only session"
}
