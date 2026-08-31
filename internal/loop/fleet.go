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
	"encoding/json"
	"errors"
	"log"
	"os"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
	"github.com/lecstor/clankerbar-cli/internal/version"
)

// iterState is one target's mid-drain presence: what `iteration {n, taskRef,
// phase}` the Fleet page should show while this drain runs.
//
// Accesses are guarded by Driver.iterMu: the loop writes at phase boundaries
// (beginIteration / endIteration), and since CLA-510 the lease renewer's
// goroutine ALSO revises the ref mid-session (reflectClaim), so these are no
// longer touched by the loop alone.
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
	d.iterMu.Lock()
	s := d.iterAt(ti)
	d.iterMu.Unlock()
	if s.on {
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
	d.writeLocalBeacon(st)
	if t.Fleet == nil {
		return
	}
	t.Fleet.Send(fleet.Report{
		Identity:   d.fleetIdentity(),
		State:      st,
		Iterations: iterations,
	})
}

// LocalBeaconName is the local presence file the daemon keeps beside its plane
// beacons: the same identity+state the next POST carries, written into the
// state dir so the supervisor's roll (phase 5b) can verify a restarted child
// is reporting the new version WITHOUT a plane read. It is the "beacon" the
// roll's verify-before-next gate reads.
const LocalBeaconName = "BEACON"

// localBeacon is the JSON shape of LocalBeaconName. The supervisor's roll
// mirrors it for reading (the same roster-style lockstep: two packages, one
// contract), so a new field here must land in internal/supervisor/roll.go too.
type localBeacon struct {
	Version string    `json:"version"`
	State   string    `json:"state"`
	At      time.Time `json:"at"`
}

// writeLocalBeacon refreshes the local beacon file. Fail-soft exactly like the
// plane beacon: telemetry is never control flow, so a write failure is logged
// (once per streak) and dropped — the daemon's own life never depends on it.
// The write is remove-then-create, like the roster cache write: a concurrent
// reader (the roll) sees either the absent name or complete bytes, never a
// torn file, and a symlink planted at the name is removed, not followed.
func (d *Driver) writeLocalBeacon(st fleet.State) {
	if d.state == nil {
		return
	}
	body, err := json.Marshal(localBeacon{
		Version: version.Current,
		State:   st.Kind,
		At:      time.Now().UTC(),
	})
	if err != nil {
		return // the four state kinds and a version string cannot fail to marshal
	}
	_ = d.state.Remove(LocalBeaconName)
	if err := d.state.WriteFile(LocalBeaconName, body); err != nil {
		if errors.Is(err, statedir.ErrExists) {
			// A concurrent beacon won the remove-then-create race — the claim
			// reflector (CLA-510) beacons from the lease-renewer goroutine, so
			// two beacons can write this file at once. The winner's file is a
			// complete, fresh beacon from the same daemon, which is exactly
			// what this write was going to produce: not a failure, and not a
			// reason to tell the operator the roll cannot verify.
			return
		}
		if !d.beaconWriteFailed.Swap(true) {
			log.Printf("fleet: cannot write the local beacon into %s (%v) - the roll cannot verify a restarted child without it", d.state.Path(), err)
		}
		return
	}
	if d.beaconWriteFailed.Swap(false) {
		log.Printf("fleet: local beacon write recovered")
	}
}

// reflectClaim returns the claim reflector for target ti (CLA-510): it keeps
// the mid-drain presence ref in step with the CURRENT claim the session holds
// — set when a claim is actually observed, cleared when the session settles it
// or holds nothing — and beacons immediately on a change, so the fleet card
// fills in within seconds of a first-phase claim instead of at the next phase
// boundary. It is a REVISION of the in-flight state, never a second iteration
// record: the one record per drain posted by endIteration is unchanged.
//
// It rides the SAME observation seam as lease renewal — the renewer calls it
// from apply for every snapshot its OnClaim hook receives — so there is no
// second watcher to keep in agreement, and a claim that was REFUSED never
// reaches it: a refused claim records no ids, so the adapter fires nothing
// (harness.Invocation.OnClaim). Only a ref CHANGE beacons; a session that
// churns claims cannot turn this into a stream.
//
// Runs on the renewer goroutine, which is exactly why iterMu exists: the loop
// writes d.iter at phase boundaries while this revises it mid-session.
//
// The renewer never reflects after stop() returns: leaseRenewer.apply drops
// snapshots once done is closed, and the write site re-checks done under
// iterMu below, so a snapshot that slipped past the apply guard while done was
// still open cannot land on a later phase's row even if something between the
// two checks were ever made blocking. No revision can land on a cleared row or
// a later phase's row: close(done) precedes the loop's own row writes, and the
// write site re-checks done under the same mutex those writes serialize on.
// The beacon that follows a write is a report send, governed by the reporter's
// own non-blocking contract, not driver state.
func (d *Driver) reflectClaim(ti int, t Target, done <-chan struct{}) func(harness.Claim) {
	return func(c harness.Claim) {
		ref := ""
		if c.Held() {
			ref = claimLabel(c)
		}
		d.iterMu.Lock()
		select {
		case <-done:
			// stop() closed done while this snapshot was in flight: the
			// session is over and the row is about to be cleared or handed to
			// the next phase — revising it now would attribute work the
			// session no longer owns.
			d.iterMu.Unlock()
			return
		default:
		}
		s := d.iterAt(ti)
		if !s.on || s.ref == ref {
			d.iterMu.Unlock()
			return
		}
		s.ref = ref
		d.iter[ti] = s
		st := fleet.State{Kind: fleet.StateIteration, N: s.n, TaskRef: s.ref, Phase: s.phase}
		d.iterMu.Unlock()
		d.beacon(ti, t, st)
	}
}

// fleetShutdown delivers each target's final `stopping` beacon synchronously.
// It runs deferred from Run, so EVERY exit path says goodbye — STOP/HALT, a
// restart marker, max-iterations, budget, signal, even a hard error return —
// which is what makes the console's silence-after-this-point meaningful: after
// a stopping beacon, quiet means the daemon is gone, not that it stopped talking.
func (d *Driver) fleetShutdown() {
	for _, t := range d.targets {
		d.writeLocalBeacon(fleet.State{Kind: fleet.StateStopping})
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
	d.iterMu.Lock()
	if ti >= 0 && ti < len(d.iter) {
		d.iter[ti] = iterState{on: true, n: drainNum, ref: ref, phase: phase}
	}
	d.iterMu.Unlock()
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
	d.iterMu.Lock()
	if ti >= 0 && ti < len(d.iter) {
		d.iter[ti] = iterState{}
	}
	d.iterMu.Unlock()
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
