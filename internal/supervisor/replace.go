package supervisor

// Replace — phase 5c of the daemon-supervisor proposal
// (docs/proposals/daemon-supervisor.md in the clankerbar repo): the
// supervisor replaces ITSELF in place once its children are drained — the
// last phase-5 step, deliberately so, because the replacement is irreducibly
// local. The supervisor holds no work of its own (Decision 5): a fleet
// upgrade finishes with one operation that swaps the process every child
// respawns through, after the roll (5b) has already moved each child onto the
// new binary one at a time.
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
// The gate the phase is named for: the replacement happens ONLY after every
// child is drained. Each child gets a STOP marker (the same write a fleet
// stop performs) and exits at its iteration boundary — an in-flight session
// finishes — and the exec waits for all of them. The wait is bounded by
// VerifyTimeout: a child that never drains HALTS the replacement, the
// children that did drain are respawned (from the launch path, which carries
// the new build), the marker is consumed so nothing re-triggers, and the
// operator fixes the stuck child and re-runs the command. A child that fails
// to drain is never killed and never exec'd over: a fleet that is not drained
// keeps running, on the old supervisor.
//
// Scoping is the roll's (Decision 7): the drain acts on the supervisor's own
// instances, which reconcile builds from LOCAL placements only — remote
// entries never become instances, so a replacement never touches them.
//
// The launchd/systemd fallback (Decision 5): exec-in-place exists because the
// supervisor may run in a plain terminal, where no unit exists to restart it.
// Operators who run the supervisor under an OS-level unit have a second path
// with the same contract — install the new build at the launch path, let the
// fleet drain (a console flip of desired state, or the replacement's own
// drain), then restart the unit, which starts the fresh supervisor from the
// launch path. On a platform without exec(2) the unit path is the only one;
// the marker mechanism is identical either way.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
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
// The command itself does not drain anything and does not touch the roster:
// the drain and the exec are the supervisor's, from the fleet it holds in
// memory. What the command owes the operator is the pre-flight — a marker for
// a swap that never happened would drain a healthy fleet for nothing — and a
// loud statement of what happens next.
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
		dir, err := rosterCacheDir()
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
	log.Printf("supervise replace: REPLACE marker written into %s - the running supervisor drains the fleet and replaces itself at its next poll; a supervisor that starts later discards the marker as stale", cacheDir)
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
// drain — a drain that fails must not re-trigger on the next poll, and a
// process killed mid-drain must not leave a marker for the next supervisor to
// honour — and also when a marker is discarded. A failed removal is logged,
// never fatal: the stale marker is discarded again on the next poll.
func (d *Supervisor) removeReplaceMarker() {
	path := filepath.Join(d.cacheDir, replaceMarkerName)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("replace: cannot remove %s (%v) - it will be discarded again on the next poll", path, err)
	}
}

// checkReplace is the phase-5c gate, called at the top of every reconcile:
// a REPLACE marker written after this supervisor started is honoured by
// draining the fleet and exec'ing the launch path in place; a marker from
// before this process began is stale and is discarded without a touch.
//
// The gate's two refusals come before any drain: the launch binary must
// report the version the marker requests (the operator's install must have
// landed — the marker's own pre-flight checked it, and the honour-time re-
// probe closes the gap between the write and the poll), and the fleet must
// actually drain. A refused or failed replacement leaves the fleet running —
// the children that drained are respawned — with the marker consumed, so the
// operator's fix makes re-running the command the whole recovery.
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

	log.Print("replace: REPLACE honoured - writing STOP to every child; each drains at its iteration boundary")
	d.removeReplaceMarker()
	if stuck := d.drainForReplace(); stuck != "" {
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
// iteration boundary; nothing is ever killed) — bounded by VerifyTimeout so a
// child that never drains HALTS the replacement instead of hanging it, and
// aborted by context cancellation so the operator's stop lands as the fleet
// stop it is.
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
	done := map[*Instance]bool{}    // child exited (event consumed) or never spawned
	stopped := map[*Instance]bool{} // STOP marker written; awaiting the child's exit
	waitStart := time.Now()
	lastLog := waitStart
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for len(done) < len(d.instances) {
		select {
		case ev := <-d.exits:
			ev.inst.mu.Lock()
			ev.inst.exited = true
			ev.inst.exitErr = ev.err
			ev.inst.mu.Unlock()
			done[ev.inst] = true
		case <-d.ctx.Done():
			return "the fleet stop landed mid-drain - the children keep draining and the supervisor exits instead of replacing itself"
		case <-tick.C:
		}
		if now := time.Now(); !now.Before(deadline) {
			return fmt.Sprintf("the fleet did not drain within %s - still waiting for: %s", timeout, stuckNames(d, done))
		}
		if now := time.Now(); now.Sub(lastLog) >= d.o.StopLogEvery {
			lastLog = now
			log.Printf("replace: still waiting for %d instance(s) after %s: %s - they stop at their iteration boundary; the supervisor keeps waiting rather than killing them",
				len(d.instances)-len(done), time.Since(waitStart).Round(time.Second), stuckNames(d, done))
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
			// A STOP already sitting there is not an alarm: the drain is one
			// of several paths that may have written it, and a pending STOP
			// means the same wait applies.
			if _, err := os.Lstat(filepath.Join(dir, "STOP")); err == nil {
				stopped[inst] = true
				continue
			}
			d.writeStop(inst, dir)
			stopped[inst] = true
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
