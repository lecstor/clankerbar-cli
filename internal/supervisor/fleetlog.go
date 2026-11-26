package supervisor

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/version"
)

// fleetStatus builds the supervisor's status surface (phase 5a): the header
// names the version the supervisor itself runs, and each instance line names
// its state and the version of the child serving it — the version recorded at
// THAT child's spawn, which is what makes version skew observable on the
// machine: a child spawned before a binary swap stays on the old version
// while later spawns carry the new one, and both sit next to the
// supervisor's own version in the same listing.
func (d *Supervisor) fleetStatus() string {
	var b strings.Builder
	fmt.Fprintf(&b, "fleet status: supervisor %s, %d instance(s)", version.Current, len(d.instances))
	for _, inst := range d.instances {
		inst.mu.Lock()
		alive := inst.cmd != nil && !inst.exited
		pid := 0
		if alive && inst.cmd.Process != nil {
			pid = inst.cmd.Process.Pid
		}
		v := inst.childVersion
		restartAt := inst.restartAt
		dir := inst.stateDir
		b.WriteString("\n  " + inst.name + ": ")
		switch {
		case alive && inst.stopRequested:
			fmt.Fprintf(&b, "stopping (pid %d, version %s)", pid, v)
		case alive:
			fmt.Fprintf(&b, "running (pid %d, version %s)", pid, v)
			// The child is alive but its loop is holding spawns closed: say why
			// and until when, from the same beacon payload the console reads
			// (CLA-908). The file lags the child by up to one poll, and a child
			// respawned since its predecessor's last write shows that line until
			// its own first beacon lands; an end already past is the one case the
			// listing can settle on its own, and schedulePauseText drops it.
			if pause := schedulePauseText(dir, time.Now()); pause != "" {
				b.WriteString(" - " + pause)
			}
		case inst.removed:
			b.WriteString("removed")
		case inst.refused:
			b.WriteString("refused")
		case inst.halted:
			b.WriteString("halted")
		case inst.limitStopped:
			fmt.Fprintf(&b, "stopped: %s", inst.limitReason)
		case inst.policyRefused:
			b.WriteString("refused (permission policy)")
		case inst.desired != RosterDesiredRunning:
			b.WriteString("stopped")
		case !restartAt.IsZero():
			// The SCHEDULED delay, not the remaining time: the listing text
			// must not drift between polls, or logFleetStatus would re-print
			// it on every poll for the whole backoff window. scheduleRestart
			// sets lastBackoff and restartAt together, so the pending
			// restart's delay is exactly what lastBackoff holds.
			fmt.Fprintf(&b, "restarting in %s (last version %s)", inst.lastBackoff, v)
		default:
			if v != "" {
				fmt.Fprintf(&b, "down (last version %s)", v)
			} else {
				b.WriteString("down")
			}
		}
		inst.mu.Unlock()
	}
	return b.String()
}

// logFleetStatus prints the fleet status listing once per CHANGE: an
// unchanged listing prints nothing, so idempotent polls stay silent (the
// status surface is as idempotent as the reconcile it follows) and a
// crash-looping child costs at most one listing per transition, not one per
// poll. An empty fleet prints nothing — the empty-roster line already says
// everything.
func (d *Supervisor) logFleetStatus() {
	if len(d.instances) == 0 {
		return
	}
	text := d.fleetStatus()
	if text == d.lastStatus {
		return
	}
	d.lastStatus = text
	log.Print(text)
}

// haltPresent reports whether a HALT marker sits in the instance's state dir.
// The dir may not exist yet (first spawn) or the marker may be absent — both
// read as false.
func (d *Supervisor) haltPresent(inst *Instance) bool {
	inst.mu.Lock()
	dir := inst.stateDir
	inst.mu.Unlock()
	return markerPresent(dir, "HALT")
}
