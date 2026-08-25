// fleet.go — the driver's half of activity reporting (CLA-466): presence
// beacons riding the backlog-summary poll, state-change beacons at phase
// boundaries, and one iteration record per drain at the boundary the loop just
// crossed.
//
// Everything here leans on the same design rule the reporter itself serves:
// telemetry is never control flow. Every helper is a nil-safe no-op when the
// target carries no Reporter (not wired, or a test), and nothing in the loop
// branches on whether a report happened.

package loop

import (
	"log"
	"os"

	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/version"
)

// iterState is one target's mid-drain presence: what `iteration {n, taskRef,
// phase}` the Fleet page should show while this drain runs. The loop is
// single-goroutine, so these are plain fields — no locking, because a poll (the
// only other reader) can never run concurrently with the drain that writes them.
type iterState struct {
	on    bool
	n     int
	ref   string
	phase string
}

// hostnameOnce caches os.Hostname for the life of the driver. It is identity
// decoration on every beacon; a per-report syscall would be waste, and an error
// simply leaves the host blank plane-side.
func (d *Driver) hostnameOnce() string {
	d.hostOnce.Do(func() {
		h, err := os.Hostname()
		if err != nil {
			log.Printf("fleet: could not read hostname: %v", err)
			return
		}
		d.hostname = h
	})
	return d.hostname
}

// fleetIdentity composes what every beacon from THIS daemon carries: who it is,
// where it runs, what build, which config. Read fresh per report so a RELOAD
// (which swaps d.cfg) is visible to the console as a changed configIdentity
// rather than requiring a restart.
func (d *Driver) fleetIdentity() fleet.Identity {
	// Explicit instance_name wins; otherwise hostname plus config basename,
	// unique per running daemon, so co-located daemons no longer collapse into
	// one presence row (CLA-501). Stable across a RELOAD: the reloader re-reads
	// the same path, so the basename half cannot change under a live daemon.
	inst := d.cfg.ResolvedInstanceName(d.hostnameOnce())
	cfgID := ""
	if d.cfg != nil {
		cfgID = d.cfg.Identity()
	}
	return fleet.Identity{
		Instance:       inst,
		Host:           d.hostnameOnce(),
		Version:        version.Current,
		ConfigIdentity: cfgID,
	}
}

// fleetState derives the presence State for target ti from what the loop already
// tracks — no new bookkeeping beyond the mid-drain marker set in drainPhases:
//
//   - mid-drain                -> iteration {n, taskRef, phase}
//   - console- or fleet-paused -> draining (alive, spawning nothing new)
//   - otherwise                -> idle
//
// A backed-off target (skipUntil still in the future) reports idle on purpose:
// the back-off is minutes-long and self-clearing, and flapping the state to
// draining for every quiet spell would blur the one signal draining exists for —
// "something is holding spawns closed until an operator acts".
func (d *Driver) fleetState(ti int) fleet.State {
	if s := d.iterAt(ti); s.on {
		return fleet.State{Kind: fleet.StateIteration, N: s.n, TaskRef: s.ref, Phase: s.phase}
	}
	if d.pausedAt(ti) || d.fleetPausedAt(ti) {
		return fleet.State{Kind: fleet.StateDraining}
	}
	return fleet.State{Kind: fleet.StateIdle}
}

// iterAt/pausedAt/fleetPausedAt are bounds-checked reads of the per-target
// slices. A Driver built by hand in a test carries empty slices; reporting must
// degrade to idle for it, not panic.
func (d *Driver) iterAt(ti int) iterState {
	if ti < 0 || ti >= len(d.iter) {
		return iterState{}
	}
	return d.iter[ti]
}

func (d *Driver) pausedAt(ti int) bool {
	return ti >= 0 && ti < len(d.paused) && d.paused[ti]
}

func (d *Driver) fleetPausedAt(ti int) bool {
	return ti >= 0 && ti < len(d.fleetPaused) && d.fleetPaused[ti]
}

// beacon sends one presence report for target ti. Nil-safe on the reporter so
// tests and not-wired targets need no branching at the call sites.
func (d *Driver) beacon(ti int, t Target, st fleet.State, iterations ...fleet.Iteration) {
	if t.Fleet == nil {
		return
	}
	t.Fleet.Send(fleet.Report{
		Identity:   d.fleetIdentity(),
		State:      st,
		Iterations: iterations,
	})
}

// fleetShutdown delivers each target's final `stopping` beacon synchronously.
// It runs deferred from Run, so EVERY exit path says goodbye — STOP/HALT, a
// restart marker, max-iterations, budget, signal, even a hard error return —
// which is what makes the console's silence-after-this-point meaningful: after
// a stopping beacon, quiet means the daemon is gone, not that it stopped talking.
func (d *Driver) fleetShutdown() {
	for _, t := range d.targets {
		if t.Fleet == nil {
			continue
		}
		t.Fleet.Close(fleet.Report{
			Identity: d.fleetIdentity(),
			State:    fleet.State{Kind: fleet.StateStopping},
		})
	}
}

// beginIteration marks target ti mid-drain and beacons the phase change NOW, so
// the console does not wait out a poll interval to learn which phase is about to
// run. Called just before each spawn — including handoff respawns of the same
// phase, whose repeat beacon is harmless (presence is last-writer-wins).
func (d *Driver) beginIteration(ti int, t Target, drainNum int, ref, phase string) {
	if ti >= 0 && ti < len(d.iter) {
		d.iter[ti] = iterState{on: true, n: drainNum, ref: ref, phase: phase}
	}
	d.beacon(ti, t, d.fleetState(ti))
}

// endIteration clears the mid-drain marker and posts ONE iteration record with
// the outcome mapped from what the loop already knows, riding the same report as
// the back-to-normal presence state.
//
// Outcome precedence, decided once here so all four words mean one thing:
//
//   - parked  the dead-phase budget parked the task (parkDeadPhase ran)
//   - dead    the drain ended on a death: a dead phase that exhausted its
//     retry/park paths, a fleet trip, or a returned error (zero-spend ladder,
//     unclassified ladder, repo_not_found...)
//   - else the CLAIM's disposition at the boundary: pushed work left for a
//     successor takeover -> checkpoint; anything else (handed back to ready, or
//     settled by the session itself) -> released
//   - nothing observed at all -> released, best-effort (the iteration ran;
//     exactly one record posts per iteration, always)
func (d *Driver) endIteration(ti int, t Target, rec fleet.Iteration) {
	if ti >= 0 && ti < len(d.iter) {
		d.iter[ti] = iterState{}
	}
	d.beacon(ti, t, d.fleetState(ti), rec)
}

// claimDisposition maps a claim still held at the drain boundary onto the two
// non-fatal outcomes: HasWIP means the lease was deliberately left for a
// takeover (checkpoint); anything else either goes back to the queue via the
// deferred handback or was settled by the session itself (released).
func claimDisposition(res *harness.Result) string {
	if res == nil {
		return ""
	}
	if res.Claim.HasWIP {
		return fleet.OutcomeCheckpoint
	}
	return fleet.OutcomeReleased
}
