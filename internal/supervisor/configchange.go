package supervisor

// Reaching a RUNNING child with a config edit (CLA-724).
//
// The generated effective config is written at spawn (materialize.go), while
// the machine layer is re-read on every reconcile. Before this, an operator's
// edit to the base config therefore changed what the NEXT spawn would run and
// nothing else: `ctl reload` re-read the child's own `-c` file — the stale
// materialization — `ctl restart` re-exec'd the child onto that same stale
// file, and `supervise roll` skipped a child already on the target version. A
// hand-written STOP (read as an unexpected exit, so it respawns) or restarting
// the whole supervisor were the only ways to apply an edit.
//
// Now every reconcile rebuilds each running child's effective config the way
// the spawn path does and compares its bytes with the file on disk. A
// difference means the operator's declared intent (a base-config edit, or a
// roster edit — projects, a permitted override) no longer matches what the
// child runs: the file is rewritten and ONE graceful RESTART is queued, which
// the daemon honours at its iteration boundary and re-execs in place onto the
// new file.
//
// Byte-exact comparison is what keeps this from becoming a restart loop: one
// config serializes to one byte string (materializedBytes), so an unchanged
// fleet writes nothing, and once the file has been rewritten later reconciles
// see no diff while the restart is still pending.
//
// Two hazards the byte comparison alone cannot cover, both handled here:
//
//   - the marker write can FAIL after the file has been rewritten. The file
//     then already matches, so a later reconcile sees no diff and the restart
//     that applies the edit is never queued. Instance.configRestartPending
//     carries that outstanding work across polls and retries the marker until
//     one is confirmed pending.
//   - a STOP/HALT can arrive in the window between the rewrite and the marker
//     write, and a RESTART queued past a stop survives it — the daemon
//     prioritises STOP/HALT, so the stale marker re-execs the child once the
//     operator resumes it. The stop is re-checked after the rewrite and after
//     the marker write, and a RESTART this function queued is withdrawn when a
//     stop wins (withdrawQueuedRestart).

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/loop"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

// supervisorRestartBodyPrefix opens the RESTART body this file writes. It is
// what tells a marker this function queued from one a ctl or a roll wrote:
// only the former may be withdrawn when a stop marker wins the race.
const supervisorRestartBodyPrefix = "supervisor: effective config changed at "

// refreshMaterialized applies an effective-config change to one RUNNING child:
// when the config built at this reconcile no longer serializes to the bytes of
// the child's generated file, it rewrites that file and queues one graceful
// RESTART so the edit lands at the child's next iteration boundary.
//
// It is deliberately conservative about what it will touch:
//
//   - Only a live, plainly running child. A child in backoff is materialized by
//     its next spawn; a stopped, removed, refused, HALTed or policy-refused
//     instance is not re-armed here (a rewrite must never make a running child
//     less constrained than a fresh spawn would be, and the build that the
//     policy gate refused is never the build this function sees — the poll
//     keeps the last good config, so the bytes match and nothing is written).
//     None of those states may leave a RESTART this function queued behind: it
//     is withdrawn, because the re-exec it would cause is no longer wanted.
//   - Never past a STOP or HALT marker: both are the operator's stop switches
//     and keep their meaning. Neither the file nor a new marker is written, and
//     a RESTART this function queued for an earlier edit is withdrawn, so the
//     stop is not followed by a re-exec nobody asked for.
//   - Never through a file it cannot read: the generated file is a cache (safe
//     to delete while the child runs), so an absent or untrusted file is left
//     for the next spawn rather than treated as evidence of an edit.
//   - Never RESTART_NOW: an edit applies at the iteration boundary, never
//     mid-session.
func (d *Supervisor) refreshMaterialized(inst *Instance) {
	inst.mu.Lock()
	alive := inst.cmd != nil && !inst.exited
	dir := inst.stateDir
	cfg := inst.cfg
	inst.mu.Unlock()

	if dir == "" || cfg == nil {
		return
	}
	// A restart this function queued is only wanted while the instance is in
	// service and running. Everything that takes it out of service — a stop
	// switch, a desired-state flip, a removal, a refusal, a HALT the child
	// already honoured, or simply a child that is no longer running — makes the
	// queued re-exec stale: the daemon prioritises STOP/HALT, so a marker left
	// behind survives the stop and re-execs the child on the next resume, and a
	// respawn materializes the current config anyway. Withdrawing it here is
	// what makes the stop win across the rewrite-to-marker window (CLA-724
	// review finding 1.review-5.1).
	inService := alive &&
		inst.desired == RosterDesiredRunning &&
		!inst.removed && !inst.refused && !inst.halted && !inst.policyRefused &&
		!stopOrHalt(dir)
	if !inService {
		d.withdrawQueuedRestart(inst, dir, cfg)
		return
	}

	// The generated file is a cache, safe to delete while the child runs: an
	// absent or untrusted one has nothing to compare and is left for the next
	// spawn (which regenerates it). Checked before opening the state dir so a
	// deleted cache never has its directory recreated just to read it.
	path := filepath.Join(dir, statedir.MaterializedConfigName)
	if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
		return
	}

	want, err := d.materializedBytes(cfg, dir)
	if err != nil {
		log.Printf("%s: cannot encode its effective config (%v) - leaving its generated config as it is", inst.name, err)
		return
	}

	// Both halves — the comparison and the rewrite — go through the state-dir
	// handle, so neither resolves a path outside the directory the child's own
	// state is confined to. The config is a whole document, not a one-line
	// marker, so the read is ReadFileAll rather than the size-capped ReadFile.
	st, err := statedir.Open(dir, cfg.SessionWorkDirs()...)
	if err != nil {
		log.Printf("%s: cannot open its state dir to compare its generated config (%v) - leaving it as it is", inst.name, err)
		return
	}
	defer st.Close() //nolint:errcheck // read-side handle on the way out

	have, err := st.ReadFileAll(statedir.MaterializedConfigName)
	if err != nil {
		// Gone or unreadable between the check and the open: still the cache
		// case above, not evidence of an edit.
		return
	}
	changed := !bytes.Equal(have, want)
	// configRestartPending is the retry for a marker write that failed after an
	// earlier rewrite: the file already holds the new bytes, so the comparison
	// alone cannot see the restart that was never queued.
	if !changed && !inst.configRestartPending {
		return
	}

	if changed {
		// The rewrite is a same-directory rename, NOT the spawn path's
		// remove-then-create (materializeConfig): a live child may read this
		// file at the instant of the rewrite — a RESTART already pending means
		// its re-exec can land at any boundary — and a reader that found the
		// file missing would die and need a backoff respawn. A rename leaves a
		// reader the old file or the new one, never an absent one, and replaces
		// a symlink planted at the name rather than following it.
		if err := st.WriteFileAtomic(statedir.MaterializedConfigName, want); err != nil {
			log.Printf("%s: its effective config changed but %s could not be rewritten (%v) - it keeps running on its current file", inst.name, path, err)
			return
		}
		if hook := d.o.AfterConfigRewrite; hook != nil {
			hook(inst.name)
		}
	}

	// A restart may already be queued (a roll, a ctl, an earlier edit): it will
	// re-exec onto the file just written, so one marker is enough. Checked
	// AFTER the rewrite on purpose — a pending marker can be consumed while the
	// file is being replaced, and if it is gone by now the write below re-queues
	// the restart the operator's switch consumed, so the change is never lost.
	if st.Exists(loop.MarkerRestart) || st.Exists(loop.MarkerRestartNow) {
		inst.configRestartPending = false
		log.Printf("%s: effective config changed - %s; its restart is already queued and lands at its iteration boundary", inst.name, rewriteClause(changed, path))
		return
	}
	// STOP/HALT again, now that the file has actually been replaced: the check
	// at the top of this function cannot be atomic with the rewrite, so a stop
	// that landed during it must still win over the marker about to be written.
	if stopOrHalt(dir) {
		d.stopWonTheRace(inst, changed, path)
		d.withdrawQueuedRestart(inst, dir, cfg)
		return
	}
	// Never RESTART_NOW: an edit applies at the iteration boundary, never
	// mid-session.
	body := []byte(fmt.Sprintf("%s%s\n", supervisorRestartBodyPrefix, time.Now().UTC().Format(time.RFC3339)))
	switch err := st.WriteFile(loop.MarkerRestart, body); {
	case err == nil:
		// And once more after the write: a stop that landed while the marker
		// was being created must not leave the marker behind, or the child
		// re-execs once for a restart the stop already cancelled.
		if stopOrHalt(dir) {
			d.stopWonTheRace(inst, changed, path)
			d.withdrawQueuedRestart(inst, dir, cfg)
			return
		}
		inst.configRestartPending = false
		if changed {
			log.Printf("%s: effective config changed - rewrote %s and queued a restart at its iteration boundary", inst.name, path)
		} else {
			log.Printf("%s: effective config changed - queued the restart an earlier failed write could not, for its next iteration boundary", inst.name)
		}
	case errors.Is(err, statedir.ErrExists):
		// A marker landed between the check and this write (ctl, a roll): one
		// pending request is enough, and the rewrite above still rides it.
		inst.configRestartPending = false
		log.Printf("%s: effective config changed - %s; its restart is already queued and lands at its iteration boundary", inst.name, rewriteClause(changed, path))
	default:
		// The edit is IN the file but no restart will pick it up, and the next
		// reconcile's byte comparison cannot see that. Keep the intent here so
		// the next poll retries the marker instead of the edit being lost.
		inst.configRestartPending = true
		log.Printf("%s: effective config changed and %s, but the restart marker could not be written (%v) - retrying it on the next reconcile", inst.name, rewriteClause(changed, path), err)
	}
}

// rewriteClause says what this poll did to the file — which differs on the
// retry after a failed marker write, where the bytes already match.
func rewriteClause(rewrote bool, path string) string {
	if rewrote {
		return "rewrote " + path
	}
	return "its generated config already held the edit"
}

// stopOrHalt reports whether either operator stop switch is present in dir.
func stopOrHalt(dir string) bool {
	return markerPresent(dir, "STOP") || markerPresent(dir, "HALT")
}

// stopWonTheRace records that the config change was written but its restart
// must not be queued: the child is stopping, and leaving the intent pending
// keeps it alive for the case where the operator clears the stop and the child
// is still running (a spawn instead satisfies it — see Instance.configRestartPending).
func (d *Supervisor) stopWonTheRace(inst *Instance, rewrote bool, path string) {
	inst.configRestartPending = rewrote || inst.configRestartPending
	log.Printf("%s: effective config changed and %s, but a stop marker arrived first - not queueing a restart; it is retried once the stop is cleared", inst.name, rewriteClause(rewrote, path))
}

// withdrawQueuedRestart removes the RESTART this supervisor queued for a config
// change when the instance is no longer meant to keep running — a STOP/HALT
// that won the race, a desired-state flip, a removal, a refusal, or a child
// that has exited. Only a marker carrying this file's body prefix is touched: a
// ctl's or a roll's request is somebody else's decision and is left exactly as
// it is. Best effort throughout — a marker already consumed is a no-op, and a
// marker that cannot be read or removed is reported rather than guessed at.
func (d *Supervisor) withdrawQueuedRestart(inst *Instance, dir string, cfg *config.Config) {
	if !markerPresent(dir, loop.MarkerRestart) {
		return
	}
	st, err := statedir.Open(dir, cfg.SessionWorkDirs()...)
	if err != nil {
		log.Printf("%s: a queued config-change RESTART could not be checked (%v) - leaving it in place", inst.name, err)
		return
	}
	defer st.Close() //nolint:errcheck // read-side handle on the way out
	body, err := st.ReadFile(loop.MarkerRestart)
	if err != nil || !bytes.HasPrefix(body, []byte(supervisorRestartBodyPrefix)) {
		return
	}
	if err := st.Remove(loop.MarkerRestart); err != nil {
		log.Printf("%s: the RESTART queued for its config change could not be removed (%v)", inst.name, err)
		return
	}
	log.Printf("%s: not being kept running, so the RESTART queued for its config change was withdrawn", inst.name)
}

// markerPresent reports whether one control marker sits in dir. An absent dir
// or absent marker reads as false; a planted symlink reads as present — it is
// somebody's request for that outcome, and every reader of these markers
// refuses to follow one anyway.
func markerPresent(dir, name string) bool {
	if dir == "" {
		return false
	}
	_, err := os.Lstat(filepath.Join(dir, name))
	return err == nil
}
