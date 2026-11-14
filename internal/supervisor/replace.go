package supervisor

// Replace — phase 5c of the daemon-supervisor proposal
// (docs/proposals/daemon-supervisor.md in the clankerbar repo): the
// supervisor replaces ITSELF in place, WITHOUT idling the fleet. The
// supervisor holds no work of its own (Decision 5): a fleet upgrade finishes
// with one operation that swaps the process every child respawns through,
// after the roll (5b) has already moved each child onto the new binary one at
// a time.
//
// The mechanism is exec-in-place (exec(2)): the running supervisor process
// replaces its own image with the binary at the fleet's launch path, invoked
// with the same arguments and the same environment — the operator's launch
// arrangement (a symlink on PATH, a unit's ProgramArguments) survives the
// swap. The operator installs the new build at the launch path and runs
// `clankerbar supervise replace` from it — the same install surface and the
// same pre-flight the roll performs. That one-shot command writes a REPLACE
// marker into the supervisor's own state (the cache dir it reconciles from);
// the running supervisor honours the marker at its next poll, and a marker a
// supervisor starts later finds stale and discards.
//
// The running supervisor, at its next poll:
//
//  1. Writes a handoff file into its cache dir, atomically: every live child's
//     name, pid, materialized config path, recorded version and spawn time,
//     plus its own pid and version (adopt.go).
//  2. Execs the launch binary AT ONCE. It writes no STOP to any child and
//     waits for no drain. exec(2) keeps the pid, so the new image is still the
//     children's parent and the child sessions are never interrupted.
//  3. The new image consumes the handoff only when the supervisor pid it
//     records is its own; a file from an earlier process is stale and
//     discarded, exactly as a REPLACE marker is. Each live child is adopted as
//     a running instance, not respawned, and reports `running` with its pid
//     and version from the first status report — the console never sees a live
//     child as offline during a replace. The new image reaps the adopted pid
//     itself (exec.Cmd's Wait died with the old image) and feeds the ordinary
//     exit path, so backoff and respawn are unchanged. A handoff pid that is
//     not our child (`wait4` returns ECHILD) reads as an exit, and the next
//     reconcile respawns it.
//  4. Each adopted child whose recorded version is older than this image is
//     moved onto it through the same RESTART roll the one-shot command
//     performs, one at a time and idle children first: an idle child comes
//     back at once, a busy one at its own phase seam or iteration boundary. A
//     child spawned after the swap is already on the new binary.
//
// An exec failure is the old image continuing with its children untouched: it
// logs the failure and removes the handoff, so no later process can adopt it.
// The marker was already consumed, so installing/fixing the launch path and
// re-running the command is the whole retry.
//
// A conflict-only session (CLA-759) is the one piece of fleet work adoption
// cannot carry across: it is an in-process goroutine, not an instance, so
// replacing the image would orphan the harness session it started while that
// session holds a task claim. When one is running the replacement waits for it
// (bounded by VerifyTimeout) before the handoff.
//
// Scoping is the roll's (Decision 7): the replacement acts on the supervisor's
// own instances, which reconcile builds from LOCAL placements only — remote
// entries never become instances, so a replacement never touches them.
//
// The launchd/systemd fallback (Decision 5) still exists, and is the only path
// on a platform without exec(2) (replace_drain.go, behind the build tag): a
// unit stop is a SIGTERM -> stopAll, which drains the fleet, and the unit then
// restarts the process from the launch path. That path is no longer how a
// VERSION swap is performed — the adopt-on-exec path above is — but a unit
// stop still drains, exactly as it did before.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/version"
)

// replaceMarkerName is the supervisor's own control marker: a REPLACE file in
// the cache dir requests a replacement. It is the supervisor-side analogue of
// the children's RESTART marker — one file, planted by a one-shot command,
// honoured at the supervisor's next poll boundary.
const replaceMarkerName = "REPLACE"

// Replace is the one-shot command behind `clankerbar supervise replace`: it
// verifies that the fleet's launch path carries the version THIS binary runs
// (the replacement execs the launch path, so the new build must already be
// installed there — the roll's own pre-flight), verifies that a supervisor
// has state on this machine at all, and writes the REPLACE marker the running
// supervisor honours at its next poll.
//
// The command itself touches no child and no roster: the handoff and the exec
// are the supervisor's, from the fleet it holds in memory. What the command
// owes the operator is the pre-flight — a marker for a swap that never
// happened would exec over a healthy fleet for nothing — and a loud statement
// of what happens next.
func Replace(ctx context.Context, o Options) error {
	launchV, err := o.launchVersion()
	if err != nil {
		return fmt.Errorf("supervise replace: cannot read the version of the fleet launch binary %s: %w", o.Binary, err)
	}
	if launchV != version.Current {
		return fmt.Errorf("supervise replace: the fleet launch binary %s reports version %s, but this replace runs %s - install the new build at the launch path and run `clankerbar supervise replace` from it", o.Binary, launchV, version.Current)
	}

	cacheDir := o.RosterCacheDir
	if cacheDir == "" {
		dir, err := CacheDir()
		if err != nil {
			return fmt.Errorf("supervise replace: %w", err)
		}
		cacheDir = dir
	}
	if _, err := os.Lstat(cacheDir); err != nil {
		return fmt.Errorf("supervise replace: no supervisor state at %s (%v) - no supervisor has run on this machine, so there is nothing to replace; start one first", cacheDir, err)
	}

	if err := writeReplaceMarker(cacheDir, version.Current); err != nil {
		return fmt.Errorf("supervise replace: cannot request the replacement: %w", err)
	}
	log.Printf("supervise replace: REPLACE marker written into %s - the running supervisor hands its live children to the new image and replaces itself at its next poll; the fleet keeps running across the swap. A supervisor that starts later discards the marker as stale", cacheDir)
	return nil
}

// writeReplaceMarker writes the REPLACE marker into the supervisor's cache
// dir. The write is remove-then-create through O_EXCL, like the cached-roster
// write: a symlink planted at the name is removed, never followed. An
// existing marker is overwritten — re-running the command refreshes the
// request (a fresh timestamp is what makes the marker honourable to a
// supervisor that had already discarded an older one).
func writeReplaceMarker(dir, v string) error {
	path := filepath.Join(dir, replaceMarkerName)
	body := []byte(fmt.Sprintf("supervisor replace requested at %s by %s\n", time.Now().UTC().Format(time.RFC3339), v))
	if _, err := os.Lstat(path); err == nil {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("cannot replace %s: %w", path, err)
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("cannot write %s: %w", path, err)
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		return fmt.Errorf("cannot write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("cannot write %s: %w", path, err)
	}
	return nil
}

// replaceMarker is a parsed REPLACE marker: the version that requested the
// replacement and the moment it was written.
type replaceMarker struct {
	version string
	at      time.Time
}

// readReplaceMarker reads the supervisor's REPLACE marker. Absent, a symlink
// planted at the name, or a body that cannot be parsed all read as nil — the
// marker is a control file like every other in the state surface, and one
// that cannot be understood is never honoured.
func readReplaceMarker(dir string) *replaceMarker {
	path := filepath.Join(dir, replaceMarkerName)
	fi, err := os.Lstat(path)
	if err != nil {
		return nil
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	v, at, err := parseReplaceMarker(data)
	if err != nil {
		return nil
	}
	return &replaceMarker{version: v, at: at}
}

// parseReplaceMarker extracts the requesting version and write time from a
// marker body ("supervisor replace requested at <RFC3339> by <version>\n").
// Anything else is a marker that cannot be understood.
func parseReplaceMarker(body []byte) (string, time.Time, error) {
	line := strings.TrimSpace(string(body))
	rest, ok := strings.CutPrefix(line, "supervisor replace requested at ")
	if !ok {
		return "", time.Time{}, errors.New("unrecognized replace marker")
	}
	atS, v, ok := strings.Cut(rest, " by ")
	if !ok || strings.TrimSpace(v) == "" {
		return "", time.Time{}, errors.New("unrecognized replace marker")
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(atS))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("unrecognized replace marker: %w", err)
	}
	return strings.TrimSpace(v), at, nil
}

// removeReplaceMarker deletes the REPLACE marker. It is consumed before the
// replacement is attempted — a refused or failed replacement must not
// re-trigger on the next poll, and a process killed mid-swap must not leave a
// marker for the next supervisor to honour — and also when a marker is
// discarded. A failed removal is logged, never fatal: the stale marker is
// discarded again on the next poll.
func (d *Supervisor) removeReplaceMarker() {
	path := filepath.Join(d.cacheDir, replaceMarkerName)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("replace: cannot remove %s (%v) - it will be discarded again on the next poll", path, err)
	}
}

// checkReplace is the phase-5c gate, called at the top of every reconcile: a
// REPLACE marker written after this supervisor started is honoured, a marker
// from before this process began is stale and is discarded without a touch.
//
// The gate's refusals come before any swap: the launch binary must report the
// version the marker requests (the operator's install must have landed — the
// marker's own pre-flight checked it, and the honour-time re-probe closes the
// gap between the write and the poll). A refused replacement leaves the fleet
// running with the marker consumed, so the operator's fix makes re-running the
// command the whole recovery.
func (d *Supervisor) checkReplace() {
	m := readReplaceMarker(d.cacheDir)
	if m == nil {
		return
	}
	// The staleness slack is load-bearing, exactly as in the roll's beacon
	// wait: the marker's timestamp rides the wire at RFC3339 SECOND precision
	// while startedAt carries nanoseconds in memory, so a marker written in
	// the same second the supervisor started truncates to an instant BEFORE
	// the start and must not read as stale. One second of slack — a marker
	// older than that cannot be a request this process owes.
	if m.at.Before(d.startedAt.Add(-time.Second)) {
		log.Printf("replace: discarding a REPLACE marker from %s - it predates this supervisor's start (%s) and is not a request this process owes; run `clankerbar supervise replace` again to request the replacement", m.at.UTC().Format(time.RFC3339), d.startedAt.UTC().Format(time.RFC3339))
		d.removeReplaceMarker()
		return
	}
	launchV, err := d.o.launchVersion()
	if err != nil {
		log.Printf("replace: cannot read the version of the launch binary %s (%v) - refusing the replacement and discarding the marker; fix the launch path and re-run `clankerbar supervise replace`", d.o.Binary, err)
		d.removeReplaceMarker()
		return
	}
	if launchV != m.version {
		log.Printf("replace: refusing - the marker requests version %s but the launch binary reports %s; install the requested build at the launch path and re-run `clankerbar supervise replace`", m.version, launchV)
		d.removeReplaceMarker()
		return
	}
	d.removeReplaceMarker()
	d.replaceNow()
}

// replaceByAdoption is the exec(2) path: hand the live fleet to the incoming
// image, then replace this process at once. Nothing is signalled and nothing
// is waited for; the new image adopts the children by pid.
func (d *Supervisor) replaceByAdoption() {
	if !d.waitForConflictSession() {
		return
	}
	if d.baseCtx().Err() != nil {
		return // the fleet stop owns the children now
	}
	n, err := d.writeReplaceHandoff()
	if err != nil {
		log.Printf("replace: cannot write the handoff file (%v) - refusing the replacement; the fleet keeps running and re-running `clankerbar supervise replace` is safe", err)
		d.removeReplaceHandoff()
		return
	}
	log.Printf("replace: handoff written for %d live child(ren) - replacing myself in place with the launch binary; the new image adopts them and rolls each onto it at its own phase seam or iteration boundary", n)
	if err := d.o.execSelf(); err != nil {
		// The hook returned (production exec never does): the old image keeps
		// running with its children untouched, and the handoff must not be
		// adopted by any process other than the one it was written for.
		log.Printf("replace: the replacement exec failed (%v) - the supervisor keeps running with its children untouched; the handoff is discarded and re-running `clankerbar supervise replace` is safe", err)
		d.removeReplaceHandoff()
	}
	// A nil return means the process image was replaced: nothing after this
	// point runs in the old image.
}

// execSelf replaces the supervisor's process image in place with the binary
// at the fleet's launch path, invoked with the same arguments and the same
// environment. On success the call never returns: the fresh supervisor starts
// from the same command line, with the same unit watching the same pid. An
// error leaves the OLD supervisor running, which the caller logs and resumes
// the loop from — a failed replacement degrades to a logged no-op, never to a
// half-replaced hybrid.
func (o Options) execSelf() error {
	if o.ExecInPlace != nil {
		return o.ExecInPlace(o.Binary, os.Args, os.Environ())
	}
	return execInPlace(o.Binary, os.Args, os.Environ())
}
