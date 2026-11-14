package supervisor

// Adopt-on-exec — the phase-5c replacement mechanism (CLA-878).
//
// exec(2) keeps the pid, so a supervisor that replaces its image in place is
// still the parent of every child it spawned. That is what makes the old drain
// unnecessary: before exec'ing, the supervisor writes a handoff file naming
// its live children, and the new image adopts them rather than waiting for
// each in-flight session to finish.
//
// The handoff is accepted only when the supervisor pid it records is the
// current process's own. A file left by an earlier supervisor (the process
// died between the write and the exec) is stale and discarded, exactly as a
// REPLACE marker is: no other process may inherit these children.
//
// Reaping is the one piece exec genuinely loses: exec.Cmd's Wait goroutine
// died with the old image. The new image waits on each adopted pid directly
// (wait4 on unix) and feeds the same exitEvent spawn posts, so exit handling,
// backoff and respawn are unchanged. A pid that is not our child — reused, or
// already reaped — comes back ECHILD and reads as an exit; the next reconcile
// respawns it.
//
// Version: an adopted child keeps its recorded version (it is still the old
// image). Each one whose BEACON does not yet report this image is moved onto it
// through the same RESTART roll the one-shot command performs, one at a time,
// idle children first. The beacon is the decision, not the recorded spawn
// version: a child that re-exec'd in place on an earlier roll keeps its pid, so
// it already reports the new version and must not be restarted a second time.
// A child spawned after the swap is already on the new binary.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/statedir"
	"github.com/lecstor/clankerbar-cli/internal/version"
)

// replaceHandoffName is the handoff file the outgoing supervisor writes into
// its cache dir beside the REPLACE marker, and the incoming image consumes at
// startup.
const replaceHandoffName = "REPLACE_HANDOFF"

// replaceHandoffMaxAge bounds how old a handoff may be and still describe the
// exec that is starting this process: write-to-adopt is one exec(2), so
// minutes of slack is generous. An older file is a leftover from a process
// whose pid has since been reused, which the pid gate alone cannot catch.
const replaceHandoffMaxAge = 5 * time.Minute

// replaceHandoff is the file's JSON shape: the process that wrote it, and one
// entry per live child it held.
type replaceHandoff struct {
	SupervisorPID int                   `json:"supervisorPid"`
	Version       string                `json:"version"`
	WrittenAt     time.Time             `json:"writtenAt"`
	Children      []replaceHandoffChild `json:"children"`
}

// replaceHandoffChild is one live child the new image must adopt. ConfigPath
// is the materialized config the child is running; it is recorded for the
// operator (and for a future verify), not needed to keep the process alive.
type replaceHandoffChild struct {
	Name       string    `json:"name"`
	PID        int       `json:"pid"`
	ConfigPath string    `json:"configPath"`
	Version    string    `json:"version"`
	SpawnedAt  time.Time `json:"spawnedAt"`
}

// errNotAChild marks wait4's ECHILD: the pid is not this process's child
// (reused, or already reaped), which reads as an exit.
var errNotAChild = errors.New("not a child of this process")

// adoptedExitError renders an adopted child's non-zero exit the way
// exec.ExitError does: statusExitCode reads ExitCode, so the console sees the
// real code after a replace rather than -1.
type adoptedExitError struct {
	code int
	msg  string
}

func (e *adoptedExitError) Error() string { return e.msg }
func (e *adoptedExitError) ExitCode() int { return e.code }

// writeReplaceHandoff records every live child for the incoming image. The
// write is atomic (temp file + rename in the same directory): a new image that
// starts while the old one is writing sees the absent name or complete bytes,
// never a torn file. A child in backoff is not live and is deliberately
// absent — the new image spawns it fresh, on its own binary.
func (d *Supervisor) writeReplaceHandoff() (int, error) {
	h := replaceHandoff{
		SupervisorPID: os.Getpid(),
		Version:       version.Current,
		WrittenAt:     time.Now().UTC(),
	}
	for _, inst := range d.instances {
		inst.mu.Lock()
		alive := inst.cmd != nil && !inst.exited && inst.cmd.Process != nil
		if alive {
			h.Children = append(h.Children, replaceHandoffChild{
				Name:       inst.name,
				PID:        inst.cmd.Process.Pid,
				ConfigPath: filepath.Join(inst.stateDir, statedir.MaterializedConfigName),
				Version:    inst.childVersion,
				SpawnedAt:  inst.aliveSince,
			})
		}
		inst.mu.Unlock()
	}
	if err := writeReplaceHandoffFile(d.cacheDir, h); err != nil {
		return 0, err
	}
	return len(h.Children), nil
}

// writeReplaceHandoffFile writes one handoff document atomically. The file is
// created 0600, in the directory it will be renamed into, then renamed over
// the destination; a failed write removes the temp file and leaves any
// previous handoff untouched.
func writeReplaceHandoffFile(dir string, h replaceHandoff) error {
	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", replaceHandoffName, err)
	}
	data = append(data, '\n')
	f, err := os.CreateTemp(dir, replaceHandoffName+".tmp-")
	if err != nil {
		return fmt.Errorf("create %s: %w", replaceHandoffName, err)
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", replaceHandoffName, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync %s: %w", replaceHandoffName, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", replaceHandoffName, err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, replaceHandoffName)); err != nil {
		return fmt.Errorf("rename %s: %w", replaceHandoffName, err)
	}
	tmp = ""
	return nil
}

// readReplaceHandoff parses the handoff file. Absent, a symlink planted at the
// name, or any body that cannot be understood reads as nil — the same posture
// every other file in the state surface takes. The shape is validated:
// a handoff naming a pid-less child is not adoptable.
func readReplaceHandoff(dir string) *replaceHandoff {
	path := filepath.Join(dir, replaceHandoffName)
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var h replaceHandoff
	if err := json.Unmarshal(data, &h); err != nil {
		return nil
	}
	if h.SupervisorPID <= 0 {
		return nil
	}
	for _, c := range h.Children {
		if c.Name == "" || c.PID <= 0 {
			return nil
		}
	}
	return &h
}

// removeReplaceHandoff deletes the handoff file, whether it was consumed or
// discarded. A failure is logged, never fatal: the supervisor pid gate makes a
// leftover file harmless to every process except its writer.
func (d *Supervisor) removeReplaceHandoff() {
	path := filepath.Join(d.cacheDir, replaceHandoffName)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("replace: cannot remove %s (%v) - a later supervisor will discard it", path, err)
	}
}

// adoptReplaceHandoff consumes the handoff file at startup and turns each live
// child into a running instance, without respawning it. It returns the
// adopted instances so the first reconcile can start rolling them onto this
// image once the roster has been refreshed.
//
// A child whose process is already gone is not adopted: the first reconcile
// spawns it from the roster, on this binary. A child that is alive but not
// ours is adopted and then reaped to ECHILD — one code path, "not a child
// means exited", exactly as a reused pid must read.
func (d *Supervisor) adoptReplaceHandoff() []*Instance {
	path := filepath.Join(d.cacheDir, replaceHandoffName)
	if _, err := os.Lstat(path); err != nil {
		return nil
	}
	h := readReplaceHandoff(d.cacheDir)
	d.removeReplaceHandoff()
	if h == nil {
		log.Printf("replace: discarding an unreadable handoff file in %s - any children it named are still this process's and will run unreaped, and the next reconcile can spawn duplicates on the same state dirs; inspect the file before re-running the replacement", d.cacheDir)
		return nil
	}
	if h.SupervisorPID != os.Getpid() {
		log.Printf("replace: discarding a handoff file written by supervisor pid %d - this process is pid %d, so the children are not ours to adopt", h.SupervisorPID, os.Getpid())
		return nil
	}
	if !h.WrittenAt.IsZero() && time.Since(h.WrittenAt) > replaceHandoffMaxAge {
		log.Printf("replace: discarding a handoff file written at %s - more than %s before this process adopted it, so it cannot describe the exec that started this image", h.WrittenAt.UTC().Format(time.RFC3339), replaceHandoffMaxAge)
		return nil
	}
	if len(h.Children) == 0 {
		log.Print("replace: the handoff file names no live children - nothing to adopt")
		return nil
	}
	log.Printf("replace: handoff from supervisor %s (pid %d, written %s) accepted - adopting %d live child(ren)", h.Version, h.SupervisorPID, h.WrittenAt.UTC().Format(time.RFC3339), len(h.Children))

	var adopted []*Instance
	seen := make(map[string]bool, len(h.Children))
	for _, c := range h.Children {
		if seen[c.Name] {
			log.Printf("replace: handoff names %s more than once - keeping the first entry and ignoring the duplicate (pid %d)", c.Name, c.PID)
			continue
		}
		seen[c.Name] = true
		if !processAlive(c.PID) {
			log.Printf("replace: handoff names %s (pid %d, version %s) but the process is gone - it will be spawned afresh on the next reconcile", c.Name, c.PID, c.Version)
			continue
		}
		// A child that died after the handoff was written is a zombie: kill(0)
		// still reports it alive, but it is already this process's to reap. Reap
		// it here, before the roll starts, rather than adopting a dead
		// generation the roll could then hand a spurious RESTART.
		if reaped, werr := probeExited(c.PID); reaped {
			log.Printf("replace: handoff names %s (pid %d) but it exited during the exec window (%s) - it is reaped and the next reconcile spawns it afresh on this image", c.Name, c.PID, exitErrString(werr))
			continue
		}
		cmd := adoptedCmd(c.PID)
		if cmd == nil {
			log.Printf("replace: cannot attach to %s (pid %d) - it will be spawned afresh on the next reconcile", c.Name, c.PID)
			continue
		}
		inst := &Instance{
			name:         c.Name,
			desired:      RosterDesiredRunning,
			stateDir:     rosterStateDir(d.cacheDir, c.Name),
			cmd:          cmd,
			aliveSince:   c.SpawnedAt,
			childVersion: c.Version,
			adoptSettled: make(chan struct{}),
		}
		d.instances = append(d.instances, inst)
		adopted = append(adopted, inst)
		log.Printf("replace: adopted %s (pid %d, version %s, %s) - it keeps running across the swap", c.Name, c.PID, c.Version, c.ConfigPath)
		d.adoptWG.Add(1)
		go func(inst *Instance, pid int) {
			defer d.adoptWG.Done()
			d.reapAdopted(inst, pid)
		}(inst, c.PID)
	}
	return adopted
}

// adoptedCmd wraps an inherited pid in the *exec.Cmd shape every existing
// alive/pid check keys off. No Wait is ever called on it: the adopted reaper
// owns wait4 for this pid.
func adoptedCmd(pid int) *exec.Cmd {
	p, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	return &exec.Cmd{Process: p}
}

// reapAdopted waits on an adopted child's pid directly and posts the same
// exitEvent spawn's Wait goroutine does, so onExit, backoff and respawn need
// no adoption-specific branch. ECHILD (a reused pid, or one already reaped)
// arrives as an ordinary exit error; the next reconcile respawns the instance.
//
// The non-blocking probe runs BEFORE the adopted roll decides anything about
// this instance (adoptSettled): a child that was already a zombie at adoption
// is marked exitPending before the roll can see it as alive, so the roll never
// writes a RESTART into a dead generation. For a live child the probe settles
// the roll's decision immediately and the blocking wait follows.
func (d *Supervisor) reapAdopted(inst *Instance, pid int) {
	reaped, err := probeExited(pid)
	if reaped {
		inst.mu.Lock()
		inst.exitPending = true
		inst.mu.Unlock()
		inst.settleAdopted()
	} else {
		inst.settleAdopted()
		err = waitPid(pid)
	}
	if errors.Is(err, errNotAChild) {
		err = fmt.Errorf("pid %d is not this supervisor's child: %w", pid, err)
	}
	if !reaped {
		inst.mu.Lock()
		inst.exitPending = true
		inst.mu.Unlock()
	}
	d.exits <- exitEvent{inst: inst, err: err, at: time.Now()}
}

// settleAdopted releases the adopted roll's decision for this instance: its
// reaper's first wait4 has run, so an exit that was already pending has set
// exitPending. Called exactly once per adopted child, by its reaper.
func (inst *Instance) settleAdopted() {
	close(inst.adoptSettled)
}

// waitAdoptSettled waits for the instance's reaper to settle the roll's
// decision. A spawned instance has no reaper and settles immediately.
func (inst *Instance) waitAdoptSettled() {
	if inst.adoptSettled != nil {
		<-inst.adoptSettled
	}
}

// adoptedOnTarget reports whether an adopted child is already on this image.
// The decision comes from the child's own beacon, exactly as Roll makes it: a
// child that re-exec'd in place on an earlier roll keeps its pid, so the
// supervisor never observes the new version through a respawn and its recorded
// childVersion stays stale. Updating childVersion here is what makes the
// listing report what the child ACTUALLY runs.
func adoptedOnTarget(inst *Instance) bool {
	inst.mu.Lock()
	dir := inst.stateDir
	v := inst.childVersion
	inst.mu.Unlock()
	if b := readLocalBeacon(dir); b != nil && b.Version == version.Current {
		inst.mu.Lock()
		inst.childVersion = version.Current
		inst.mu.Unlock()
		return true
	}
	return v == version.Current
}

// adoptedRollCandidate is one adopted child plus the idle rank computed for
// the sort. The rank is read ONCE: a firmer read inside the comparator could
// see a beacon change mid-sort, and a non-transitive comparator is not a sort.
type adoptedRollCandidate struct {
	inst *Instance
	idle int
}

// startAdoptedRoll moves the adopted children still on the old binary onto
// this image through the RESTART roll, one at a time. It runs on its own
// goroutine so adoption never holds up a reconcile. Idle children are rolled
// first: a busy child's in-flight session must not delay a child that could
// come back immediately.
func (d *Supervisor) startAdoptedRoll(adopted []*Instance) {
	var candidates []adoptedRollCandidate
	for _, inst := range adopted {
		inst.mu.Lock()
		alive := inst.cmd != nil && !inst.exited
		inst.mu.Unlock()
		if !alive || inst.removed || inst.refused || inst.desired != RosterDesiredRunning {
			continue
		}
		if adoptedOnTarget(inst) {
			log.Printf("replace: adopted %s already runs %s - not rolled", inst.name, version.Current)
			continue
		}
		candidates = append(candidates, adoptedRollCandidate{inst: inst, idle: adoptedIdleRank(inst)})
	}
	if len(candidates) == 0 {
		return
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].idle < candidates[j].idle
	})
	roll := make([]*Instance, len(candidates))
	for i, c := range candidates {
		roll[i] = c.inst
	}
	d.adoptWG.Add(1)
	go func() {
		defer d.adoptWG.Done()
		d.rollAdopted(roll)
	}()
}

// adoptedIdleRank orders the roll: a child whose beacon says idle comes back
// at once, so it goes first; a busy, unknown or beacon-less child waits its
// turn behind it.
func adoptedIdleRank(inst *Instance) int {
	inst.mu.Lock()
	dir := inst.stateDir
	inst.mu.Unlock()
	if b := readLocalBeacon(dir); b != nil && (b.State == "" || b.State == "idle") {
		return 0
	}
	return 1
}

// rollAdopted restarts adopted children onto this image one at a time,
// waiting for each one's next beacon to report the target version before
// touching the next — the same gate and the same marker the one-shot roll
// uses. A failure halts the roll and leaves the remaining children untouched;
// `clankerbar supervise roll` can finish them.
func (d *Supervisor) rollAdopted(insts []*Instance) {
	for _, inst := range insts {
		if d.baseCtx().Err() != nil {
			return
		}
		// Never decide before the reaper's first wait4 has run: a child whose
		// exit is already pending (the exec-window zombie) must not have a
		// RESTART written into its dead generation.
		inst.waitAdoptSettled()
		inst.mu.Lock()
		alive := inst.cmd != nil && !inst.exited
		pending := inst.exitPending
		dir := inst.stateDir
		inst.mu.Unlock()
		if !alive || pending {
			continue
		}
		if adoptedOnTarget(inst) {
			// A child that came back on this image while the roll waited on its
			// predecessor (or that an earlier roll already moved) needs nothing;
			// restarting it again would be a swap nobody asked for.
			log.Printf("replace: adopted %s already reports %s - not rolled", inst.name, version.Current)
			continue
		}
		inst.mu.Lock()
		was := inst.childVersion
		inst.mu.Unlock()
		log.Printf("replace: rolling adopted %s (was %s) - writing RESTART; it comes back on %s at its next phase seam or iteration boundary", inst.name, was, version.Current)
		if err := writeRestartMarker(dir, inst.name); err != nil {
			log.Printf("replace: %s: %v - halting the adopted roll; the remaining adopted children keep running their current version and `clankerbar supervise roll` can finish them", inst.name, err)
			return
		}
		if err := waitForNextBeacon(d.baseCtx(), dir, inst.name, version.Current, d.verifyTimeout()); err != nil {
			log.Printf("replace: %v - halting the adopted roll; the remaining adopted children keep running their current version and `clankerbar supervise roll` can finish them", err)
			return
		}
		// The beacon is the child's own report: record it, so the supervisor's
		// listing shows the version the adopted child ACTUALLY runs even when
		// it re-exec'd in place and kept its pid (no respawn would update it).
		// The next reconcile publishes the change; statusReport reads the field
		// under inst.mu, so writing it from this goroutine is safe.
		inst.mu.Lock()
		inst.childVersion = version.Current
		inst.mu.Unlock()
	}
	log.Printf("replace: every adopted child now reports %s", version.Current)
}

// verifyTimeout is Options.VerifyTimeout with its default.
func (d *Supervisor) verifyTimeout() time.Duration {
	if d.o.VerifyTimeout > 0 {
		return d.o.VerifyTimeout
	}
	return defaultVerifyTimeout
}

// waitForConflictSession waits for the one piece of fleet work exec cannot
// carry across: a conflict-only session (CLA-759) is an in-process goroutine,
// not an instance, so replacing the image would orphan the harness session it
// started while the session still holds a task claim. Bounded by
// VerifyTimeout; a timeout refuses the replacement rather than killing the
// session. Returns false when the replacement must not proceed.
func (d *Supervisor) waitForConflictSession() bool {
	if !d.conflictBusy.Load() {
		return true
	}
	timeout := d.verifyTimeout()
	logEvery := d.o.StopLogEvery
	if logEvery <= 0 {
		logEvery = defaultStopLogEvery
	}
	log.Printf("replace: a conflict-only session is running - it cannot be adopted across exec, so the replacement waits up to %s for it to finish", timeout)
	deadline := time.Now().Add(timeout)
	lastLog := time.Now()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for d.conflictBusy.Load() {
		if d.baseCtx().Err() != nil {
			return false
		}
		now := time.Now()
		if !now.Before(deadline) {
			log.Printf("replace: the conflict-only session did not finish within %s - refusing the replacement; the fleet keeps running and re-running `clankerbar supervise replace` after it finishes is safe", timeout)
			return false
		}
		if now.Sub(lastLog) >= logEvery {
			lastLog = now
			log.Print("replace: still waiting for the conflict-only session to finish before replacing myself")
		}
		select {
		case <-d.baseCtx().Done():
			return false
		case <-tick.C:
		}
	}
	log.Print("replace: the conflict-only session finished - replacing myself with the launch binary")
	return true
}
