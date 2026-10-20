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

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/loop"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

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
//   - Never past a STOP or HALT marker: both are the operator's stop switches
//     and keep their meaning; neither the file nor a marker is touched.
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

	if !alive || dir == "" || cfg == nil {
		return
	}
	if inst.desired != RosterDesiredRunning || inst.removed || inst.refused || inst.halted || inst.policyRefused {
		return
	}
	if markerPresent(dir, "STOP") || markerPresent(dir, "HALT") {
		return
	}

	want, err := d.materializedBytes(cfg, dir)
	if err != nil {
		log.Printf("%s: cannot encode its effective config (%v) - leaving its generated config as it is", inst.name, err)
		return
	}
	have, err := readMaterialized(dir)
	if err != nil || bytes.Equal(have, want) {
		return
	}

	path := filepath.Join(dir, statedir.MaterializedConfigName)
	st, err := statedir.Open(dir, cfg.SessionWorkDirs()...)
	if err != nil {
		log.Printf("%s: its effective config changed but its state dir could not be opened (%v) - it keeps running on its current file", inst.name, err)
		return
	}
	defer st.Close() //nolint:errcheck // read-side handle on the way out

	// The rewrite is a same-directory rename, NOT the spawn path's
	// remove-then-create (materializeConfig): a live child may read this file
	// at the instant of the rewrite — a RESTART already pending means its
	// re-exec can land at any boundary — and a reader that found the file
	// missing would die and need a backoff respawn. A rename leaves a reader
	// the old file or the new one, never an absent one, and replaces a symlink
	// planted at the name rather than following it.
	if err := st.WriteFileAtomic(statedir.MaterializedConfigName, want); err != nil {
		log.Printf("%s: its effective config changed but %s could not be rewritten (%v) - it keeps running on its current file", inst.name, path, err)
		return
	}
	// A restart may already be queued (a roll, a ctl, an earlier edit): it will
	// re-exec onto the file just written, so one marker is enough. Checked
	// AFTER the rewrite on purpose — a pending marker can be consumed while the
	// file is being replaced, and if it is gone by now the write below re-queues
	// the restart the operator's switch consumed, so the change is never lost.
	if st.Exists(loop.MarkerRestart) || st.Exists(loop.MarkerRestartNow) {
		log.Printf("%s: effective config changed - rewrote %s; its restart is already queued and lands at its iteration boundary", inst.name, path)
		return
	}
	// Never RESTART_NOW: an edit applies at the iteration boundary, never
	// mid-session.
	body := []byte(fmt.Sprintf("supervisor: effective config changed at %s\n", time.Now().UTC().Format(time.RFC3339)))
	switch err := st.WriteFile(loop.MarkerRestart, body); {
	case err == nil:
		log.Printf("%s: effective config changed - rewrote %s and queued a restart at its iteration boundary", inst.name, path)
	case errors.Is(err, statedir.ErrExists):
		// A marker landed between the check and this write (ctl, a roll): one
		// pending request is enough, and the rewrite above still rides it.
		log.Printf("%s: effective config changed - rewrote %s; its restart is already queued and lands at its iteration boundary", inst.name, path)
	default:
		log.Printf("%s: effective config changed and %s was rewritten, but the restart marker could not be written (%v) - restart it with `clankerbar ctl restart -c %s`", inst.name, path, err, path)
	}
}

// readMaterialized reads one child's generated config IN FULL, for the
// byte-exact comparison refreshMaterialized performs. It cannot go through
// statedir.ReadFile: that caps at maxMarkerBytes because a control marker is a
// one-line reason, while this is a whole config document. The open discipline
// is mirrored instead — a symlink planted at the name is refused, never
// followed — and an absent or untrusted file is an error, which the caller
// treats as "nothing to compare", not as a change.
func readMaterialized(dir string) ([]byte, error) {
	path := filepath.Join(dir, statedir.MaterializedConfigName)
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink - refusing to read a generated config through one", path)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return os.ReadFile(path)
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
