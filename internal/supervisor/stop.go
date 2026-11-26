package supervisor

import (
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

// stopAll is the fleet-wide stop: every live child gets a STOP marker in its
// own state dir, and the supervisor waits for every child to exit. A child in
// backoff (dead, waiting to respawn) is simply never respawned — its restart
// timer is moot because the select loop is not running. The marker is the one
// `ctl`/a hand-written touch write: the daemon drains at its iteration
// boundary — an in-flight session finishes — and exits by itself.
//
// One loop does both halves — the settle wait and the exit wait — because a
// child that dies DURING its settle wait must be noticed via its exit event,
// not by polling a flag nothing sets. `exited` is only ever set by onExit,
// which runs on the main select loop; that loop is not running here, so a
// child whose event sits unconsumed in the channel still reads as live, and a
// separate settle loop would wait on it forever (it died before creating its
// state dir, so the dir-never-appears condition never clears either).
func (d *Supervisor) stopAll() error {
	log.Print("stop requested - writing STOP to every instance")
	d.stopping = true
	// Report the transition NOW, not on the first exit: a drain can run for
	// minutes (an in-flight session finishes at its boundary), and until the
	// first child exits the plane would otherwise keep the pre-stop report
	// and show ordinary `running`. Every row this queues says the supervisor
	// is stopping, which is true from this line on.
	d.reportStatus()
	done := map[*Instance]bool{}    // child exited (event consumed) or never spawned
	stopped := map[*Instance]bool{} // STOP marker written; awaiting the child's exit
	waitStart := time.Now()
	lastExit := waitStart
	lastLog := waitStart
	readyTick := time.NewTicker(50 * time.Millisecond)
	defer readyTick.Stop()
	for len(done) < len(d.instances) {
		select {
		case ev := <-d.exits:
			// A child that exits during the drain may still hold its claim: the
			// STOP marker only takes effect at a phase seam or iteration boundary, and a
			// daemon killed (or still settling) in the meantime leaves its
			// CURRENT_CLAIM behind. onExit would have handed it back, but this
			// loop is what consumes an exit once the fleet stop has begun, so
			// the handback must happen here too or the claim waits out its
			// 30-minute lease while the supervisor exits. A no-op when the
			// record is absent.
			d.handBackDeadClaim(ev.inst)
			// The flags onExit would have set are set here too: this loop is
			// what consumes an exit once the fleet stop has begun, and the
			// status report (and its final `stopped` rows) reads them.
			ev.inst.mu.Lock()
			ev.inst.exited = true
			ev.inst.exitErr = ev.err
			ev.inst.exitAt = ev.at
			ev.inst.mu.Unlock()
			done[ev.inst] = true
			lastExit = time.Now()
			d.reportStatus()
		case <-readyTick.C:
			// A drain can legitimately take minutes (an in-flight session
			// finishes), so the wait is never bounded — but a stop making no
			// progress at all must say so, or a wedged child reads as a hang.
			// lastLog gates the line to once per interval: a stuck stop must
			// not become a 20-lines-a-second log.
			now := time.Now()
			if quiet := now.Sub(lastExit); quiet >= d.o.StopLogEvery && now.Sub(lastLog) >= d.o.StopLogEvery {
				lastLog = now
				var stuck []string
				for _, inst := range d.instances {
					if !done[inst] {
						stuck = append(stuck, inst.name)
					}
				}
				sort.Strings(stuck)
				log.Printf("stop still waiting for %d instance(s) after %s (no exit in %s): %s - they stop at their next phase seam or iteration boundary; the supervisor keeps waiting rather than killing them",
					len(stuck), time.Since(waitStart).Round(time.Second), quiet.Round(time.Second), strings.Join(stuck, ", "))
			}
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
			if since >= d.o.SettleBeforeStop {
				if _, err := os.Lstat(dir); err == nil {
					d.writeStop(inst, dir)
					// written or not, one attempt is enough: a failure is
					// logged loudly and the child is left to drain (or not)
					// on its own; the supervisor keeps waiting for its exit.
					stopped[inst] = true
				}
			}
		}
	}
	log.Print("all instances stopped")
	return nil
}

// stopOutcome is what one writeStop call achieved: whether a marker the daemon
// can read now sits in the child's state dir, and whether this call put it
// there. Callers that retry use it to tell "pending, stop asking" from "failed,
// ask again on the next poll".
type stopOutcome int

const (
	// stopFailed means no readable marker is in place: the child keeps running,
	// and the operator was told so loudly.
	stopFailed stopOutcome = iota
	// stopWritten means this call wrote a fresh STOP marker.
	stopWritten
	// stopPending means a readable STOP marker was already there, and it was
	// left untouched.
	stopPending
)

// writeStop writes the STOP marker into one instance's state dir, or logs
// loudly why it could not, and reports which of the three happened. Two rules
// keep the write honest:
//
//   - A STOP the supervisor writes only lands if the child is PAST its STARTUP
//     marker consumption. A STOP the daemon finds at startup is consumed
//     WITHOUT stopping (CLA-491: a marker a previous run left behind must not
//     kill the next one), so a marker dropped into that window would be eaten
//     and the child would run on. The callers therefore only call this once the
//     child has been alive for the settle window, which guarantees it has
//     passed that consumption. That guarantee is about WHEN this call writes:
//     the supervisor cannot know when a marker it did not write was written.
//   - The state dir is never CREATED here: a dir the supervisor created for a
//     child that has not started yet would be adopted by that child, whose
//     startup would then eat the marker exactly as above. Writing into a dir
//     the child itself created (the Lstat gate in the callers) is what
//     guarantees the child is past its startup marker check.
//
// A readable regular STOP already sitting in the state dir is not a failed
// write: it is left untouched (its body records who asked first) and the daemon
// picks it up at its next phase seam or iteration boundary, so the line says only that —
// never that this child will drain, which the supervisor cannot know about a
// marker of unknown provenance. The occupancy alone proves nothing, though:
// WriteFile also reports ErrExists for a symlink or a directory at the name, and
// the daemon refuses to read either, so only a successful safe read of the
// marker counts as pending. Everything else — the state dir refuses to open, an
// unreadable occupant, any other marker write failure — is logged loudly and
// the child is left running: the supervisor keeps waiting for its exit, because
// stopping it by killing the process is exactly what this phase must not do.
func (d *Supervisor) writeStop(inst *Instance, dir string) stopOutcome {
	inst.mu.Lock()
	cfg := inst.cfg
	inst.mu.Unlock()
	// An adopted child can be alive before its first successful config build
	// (the roster fetch has not landed yet); the state-dir open must not depend
	// on a config that does not exist.
	var roots []string
	if cfg != nil {
		roots = cfg.SessionWorkDirs()
	}
	st, err := statedir.Open(dir, roots...)
	if err != nil {
		log.Printf("%s: cannot open its state dir to write STOP: %v - the child keeps running; stop it by hand (kill the supervisor's child pid %d)", inst.name, err, instPid(inst))
		return stopFailed
	}
	defer st.Close() //nolint:errcheck // read-side handle on the way out
	body := []byte(fmt.Sprintf("fleet-wide stop requested by the supervisor at %s\n", time.Now().UTC().Format(time.RFC3339)))
	if err := st.WriteFile("STOP", body); err != nil {
		if errors.Is(err, statedir.ErrExists) {
			// Something already occupies STOP. Only a marker the daemon can
			// actually read counts as a pending stop: the same ErrExists is
			// what WriteFile reports for a symlink or a directory at the name,
			// and the daemon's readMarker refuses both, so calling those
			// pending would tell the operator a child is draining while it
			// keeps running. The safe read is the only proof that counts.
			if existing, rerr := st.ReadFile("STOP"); rerr == nil {
				log.Printf("%s: STOP already present in %s%s - left in place for the daemon to read at its next phase seam or iteration boundary", inst.name, dir, markerRequestedBy(existing))
				return stopPending
			}
			// Occupied by something the daemon will not read (a symlink, a
			// directory, anything else non-regular): fall through to the loud
			// failure line. Nothing stops this child.
		}
		log.Printf("%s: cannot write STOP into %s: %v - the child keeps running; stop it by hand", inst.name, dir, err)
		return stopFailed
	}
	log.Printf("%s: STOP written to %s - draining at its next phase seam or iteration boundary", inst.name, dir)
	return stopWritten
}

// markerRequestedBy renders an existing STOP marker's first line for the
// pending stop log — its body is the record of who asked first, and the daemon
// discards the message when it honours the marker. %q keeps a hostile body from
// injecting log lines.
func markerRequestedBy(body []byte) string {
	line := strings.TrimSpace(string(body))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	if line == "" {
		return ""
	}
	return fmt.Sprintf(" (requested by %q)", line)
}

// instPid is the child's pid for a stop-failure log line, or 0 when there is
// no live child to name.
func instPid(inst *Instance) int {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if inst.cmd == nil || inst.cmd.Process == nil {
		return 0
	}
	return inst.cmd.Process.Pid
}
