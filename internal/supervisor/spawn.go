package supervisor

import (
	"errors"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/version"
)

// spawn starts one instance's child on its effective config, materialized
// into the instance's state dir. The permission-policy gate (phase 2c) runs
// here as the hard child-start gate, over a config RE-RESOLVED at the spawn:
// the entry is built fresh from the current roster row and the current base
// config, so an edit that lands between polls is seen at the child-start gate,
// not on the next reconcile. A ErrPolicyRefused re-resolve refuses with backoff
// and never falls back — the last materialized config may name the very policy
// file the operator replaced. Any other re-resolve failure keeps the
// last-known-good (the previous build, or the materialized config loaded by
// refreshInstance), and the gate re-checks THAT config's policy file too: a
// fallback whose own policy is gone is also refused. Retry with backoff, so a
// policy file restored while the supervisor runs is picked up by the next
// respawn.
//
// The child is spawned with `run -c` pointing at the MATERIALIZED config in
// its state dir — never the operator's local config — so the config a daemon
// runs on is always the generated artifact. A materialization that cannot be
// written (an unwritable or unadoptable state dir) is as unexpected as a
// spawn failure: same backoff, same ladder, no child.
func (d *Supervisor) spawn(inst *Instance) {
	fresh, sd, err := d.buildConfig(inst.entry)
	if err != nil {
		if errors.Is(err, ErrPolicyRefused) {
			log.Printf("%s: refusing to start: %v - retrying with backoff", inst.name, err)
			inst.scheduleRestart(d, err, time.Since(inst.aliveSince))
			return
		}
		log.Printf("%s: re-resolving its config failed (%v) - spawning on the last-known-good config", inst.name, err)
	} else {
		inst.mu.Lock()
		inst.cfg = fresh
		inst.stateDir = sd
		inst.mu.Unlock()
		inst.policyRefused = false
	}
	inst.mu.Lock()
	cfg := inst.cfg
	inst.mu.Unlock()
	if cfg == nil {
		// The build failed and no last-known-good exists — nothing to start;
		// the next poll retries the build.
		log.Printf("%s: nothing to start - the config build failed and no last-known-good exists", inst.name)
		return
	}
	if err := checkPermissionPolicy(cfg); err != nil {
		log.Printf("%s: refusing to start: %v - retrying with backoff", inst.name, err)
		inst.scheduleRestart(d, err, time.Since(inst.aliveSince))
		return
	}
	cfgPath, err := d.materializeConfig(inst)
	if err != nil {
		inst.mu.Lock()
		dir := inst.stateDir
		inst.mu.Unlock()
		log.Printf("%s: cannot write its generated config into %s (%v) - retrying with backoff", inst.name, dir, err)
		inst.scheduleRestart(d, err, time.Since(inst.aliveSince))
		return
	}
	// The child is about to start on the current effective config, so a
	// restart-marker retry left pending by an earlier poll has been satisfied
	// (CLA-724 review findings 1.review-3.1 / 1.review-4.1).
	inst.configRestartPending = false
	v := childVersion(d.o.VersionOf)
	cmd := exec.Command(d.o.Binary, "run", "-c", cfgPath)
	// The version recorded for this child rides the spawn environment (phase
	// 5b): the fake daemon in tests reports it as its own version, and a
	// wrapper can read what the supervisor believes the child runs. The real
	// daemon ignores it — its beacon reports its own build, which is the
	// ground truth the roll verifies.
	cmd.Env = spawnEnv(v, d.o.NoEvents)
	// The children's logs stream to the same stdout/stderr the supervisor's
	// do — the operator watches daemons today by watching one terminal per
	// daemon, and the fleet must not be quieter than that. Adoption across an
	// exec(2) replacement depends on this direct inheritance: a pipe owned by
	// the supervisor would not survive the image swap, so do not turn these
	// into pipes without teaching adoption to pass the fds across.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		// A spawn failure is as unexpected as an exit: same backoff, same
		// ladder. There is no child to wait on, so schedule directly.
		log.Printf("%s: cannot spawn (%v) - retrying with backoff", inst.name, err)
		inst.scheduleRestart(d, err, time.Since(inst.aliveSince))
		return
	}
	inst.mu.Lock()
	inst.cmd = cmd
	inst.aliveSince = time.Now()
	inst.exited = false
	inst.childVersion = v
	inst.mu.Unlock()
	// A fresh child starts clean: the next stopped flip must write its STOP
	// again, it must not inherit the previous child's marker state.
	inst.stopRequested = false
	log.Printf("%s: spawned (pid %d, %s, version %s)", inst.name, cmd.Process.Pid, cfgPath, inst.childVersion)
	d.publishStatus()
	go func() {
		err := cmd.Wait()
		d.exits <- exitEvent{inst: inst, err: err, at: time.Now()}
	}()
}

// onExit classifies one child exit: a stop the supervisor ordered (desired
// state, roster removal, refusal), a HALT the operator planted, or an
// unexpected exit. Only the last respawns. at is when the Wait goroutine saw
// the process end, carried onto the status row as `lastExit.at`.
func (d *Supervisor) onExit(inst *Instance, err error, at time.Time) {
	// A child that exited without letting go may have left its claim behind
	// (CLA-708). Read it BEFORE deciding whether to respawn — the hand-back is
	// what turns a dead session into an immediate takeover instead of a
	// 30-minute lease. It applies to EVERY exit, including one the supervisor
	// ordered: a drained child normally holds nothing (the record is removed on
	// every let-go path), and a record found anyway means the drain did not let
	// go, which is still the dead-session case.
	d.handBackDeadClaim(inst)
	// The exited flag is set BEFORE the ctx check, not after: the select loop
	// may have picked this event in the same instant the fleet stop landed
	// (ctx.Done), and stopAll counts live children from this flag. If the
	// event is consumed here, nothing will ever post another one for this
	// child, so the flag must already say "exited" when stopAll looks.
	inst.mu.Lock()
	inst.exited = true
	inst.exitErr = err
	inst.exitAt = at
	uptime := time.Since(inst.aliveSince)
	inst.mu.Unlock()

	if d.ctx.Err() != nil {
		return // the fleet is stopping; stopAll owns the outcome now
	}

	// A child that was told to stop — desired stopped, entry removed,
	// entry refused — is not restarted. The desired flip is reconciled on the
	// next poll, so a child that dies between the flip and the STOP write is
	// also left stopped: the instance's desired state is authoritative, not
	// the marker's arrival.
	if inst.desired != RosterDesiredRunning || inst.removed || inst.refused {
		// The fleet changed: the listing flips this line now — "stopped",
		// or "removed"/"refused" for an entry that is about to leave the
		// fleet — rather than on the next poll.
		d.publishStatus()
		return
	}

	if inst.halted {
		return
	}
	// HALT is the one marker the daemon does NOT consume (loop.go: the loop
	// returns on it, the marker stays), so it is the operator's per-instance
	// stop switch even under a supervisor: a child that exits with HALT
	// sitting in its state dir was told to stop, and the supervisor must not
	// undo it. STOP cannot serve this role — the daemon consumes it on
	// honouring, so nothing is left for the supervisor to see.
	if d.haltPresent(inst) {
		inst.halted = true
		log.Printf("%s: HALT marker present in %s - leaving it stopped; delete the marker and restart the supervisor to resume it", inst.name, inst.stateDir)
		d.publishStatus()
		return
	}

	// An explicit signal from the child, not an inference from uptime: the loop
	// wrote LIMIT_STOP before it exited.
	if d.limitStopHolds(inst) {
		d.publishStatus()
		return
	}

	inst.scheduleRestart(d, err, uptime)
}

// scheduleRestart computes the backoff for one unexpected exit and arms the
// respawn timer. uptime is how long the previous run lasted; a run at least as
// long as BackoffResetAfter resets the ladder.
func (inst *Instance) scheduleRestart(d *Supervisor, err error, uptime time.Duration) {
	delay := backoffDelay(inst.lastBackoff, uptime, d.o.BackoffBase, d.o.BackoffCap, d.o.BackoffResetAfter)
	inst.lastBackoff = delay
	inst.restartAt = time.Now().Add(delay)
	log.Printf("%s: exited unexpectedly (%v, up %s) - restarting in %s", inst.name, exitErrString(err), uptime.Round(time.Millisecond), delay)
	d.armRestart()
	// The fleet changed: the status listing now shows the child down and its
	// restart scheduled. Called from every restart-scheduling path (an exit,
	// a refused or failed spawn), so no transition goes unlisted.
	d.publishStatus()
}

// backoffDelay is the pure restart schedule: the previous delay doubles
// (starting at base) up to cap; a previous run that lasted at least resetAfter
// counts as healthy and resets the ladder to base, so a one-off crash after
// hours of good behaviour gets a fast restart instead of the accumulated cap.
func backoffDelay(prev, uptime, base, cap, resetAfter time.Duration) time.Duration {
	if uptime >= resetAfter {
		return base
	}
	if prev <= 0 {
		return base
	}
	if next := prev * 2; next > cap {
		return cap
	}
	return prev * 2
}

// exitErrString renders a child exit for the log: nil becomes "exit 0", an
// *exec.ExitError keeps its standard rendering.
func exitErrString(err error) string {
	if err == nil {
		return "exit 0"
	}
	return err.Error()
}

// childVersion returns the version to record for a child at spawn: the
// Options hook when set, else the supervisor's own build — every child is
// spawned from this binary, so its version IS the version this process was
// built as (phase 5a: discovery without a per-daemon file — no query, no
// version file; the spawn itself is the derivation).
func childVersion(hook func() string) string {
	if hook != nil {
		return hook()
	}
	return version.Current
}

// spawnEnv builds a child's environment: the parent's, with any pre-existing
// CLANKERBAR_CHILD_VERSION dropped and the supervisor's recorded value
// appended. The drop is load-bearing: getenv returns the FIRST match when a
// key appears twice in an environment, so an operator env carrying the
// reserved name would otherwise shadow the supervisor's value — and the
// wrapper that "can read what the supervisor believes the child runs" would
// read a stranger's number instead.
//
// noEvents does the same for CLANKERBAR_EVENTS: the inherited value is dropped
// and `off` appended, so the flag (the explicit statement) cannot be outranked
// by a stale value in the other direction.
func spawnEnv(v string, noEvents bool) []string {
	prefix := childVersionEnv + "="
	env := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		if noEvents && strings.HasPrefix(kv, "CLANKERBAR_EVENTS=") {
			// The flag is the explicit statement; a stale inherited value must not
			// outrank it in the other direction.
			continue
		}
		env = append(env, kv)
	}
	env = append(env, prefix+v)
	if noEvents {
		env = append(env, "CLANKERBAR_EVENTS=off")
	}
	return env
}
