// Package supervisor runs the fleet supervisor (CLA-525, phase 1 of
// docs/proposals/daemon-supervisor.md): bare `clankerbar` — or
// `clankerbar supervise` — starts every daemon whose config file is in the
// config dir as a supervised child process, restarts a child that exits
// unexpectedly (with backoff), and forwards the supervisor's own
// SIGINT/SIGTERM as a fleet-wide stop.
//
// The supervisor deliberately invents nothing. Each child is
// `clankerbar run -c <file>` — the exact command the operator used to run by
// hand — except the file is now the GENERATED effective config in the child's
// state dir (phase 2b): the operator's declared intent with the machine
// conventions materialized over it, regenerated on every reconcile. Instance
// identity (`hostname/basename(config)`, ResolveInstanceName), per-instance
// state dirs and the fleet beacon all behave exactly as before — identity
// because the generated file pins it, and the state dir because the generated
// file pins that too.
// Stopping a child means writing the STOP marker into its state dir, the same
// marker a hand-written `touch STOP` writes, never killing the process: the
// daemon drains at its iteration boundary and exits by itself. The one rule
// the supervisor adds is the restart decision: a child that exits for any
// reason other than a stop it was told about is respawned, with backoff.
//
// What this phase does NOT do: no plane call, no roster, no desired state, no
// version check, no self-update. The config dir is enumerated once at startup
// — add a config file and restart the supervisor to pick it up.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

// Defaults for the restart backoff and the stop settle window. All four are
// overridable through Options, which is what the tests do.
const (
	// defaultBackoffBase is the first delay after an unexpected exit.
	defaultBackoffBase = 2 * time.Second
	// defaultBackoffCap is the ceiling the doubling ladder stops at: a
	// crash-looping child costs at most one respawn per minute.
	defaultBackoffCap = 60 * time.Second
	// defaultBackoffResetAfter is the uptime that counts as healthy: a child
	// that stayed up at least this long resets its ladder to base, so a
	// one-off crash after hours of good behaviour is answered with a fast
	// restart, not the accumulated cap.
	defaultBackoffResetAfter = 2 * time.Minute
	// defaultSettleBeforeStop is how long a child must have been alive before
	// the supervisor will write its STOP marker. The daemon consumes a STOP
	// marker found AT STARTUP without stopping (CLA-491 — a marker left behind
	// by a previous run must not kill the next one), so a stop landing in the
	// startup window would be eaten and the child would run on. A child alive
	// for the settle window has long passed that consumption, so its STOP is
	// guaranteed to be honoured.
	defaultSettleBeforeStop = 2 * time.Second
	// defaultStopLogEvery is how long a fleet stop may make no progress —
	// no child exit, no child becoming stoppable — before the supervisor
	// says so on the log. Drains can legitimately take minutes (an in-flight
	// session finishes), so the wait itself is never bounded; only its
	// silence is.
	defaultStopLogEvery = 30 * time.Second
)

// Options configures one supervisor run. Zero values take the defaults above.
type Options struct {
	// ConfigDir is the directory whose `*.json` files are the instances.
	ConfigDir string
	// Binary is the executable children are spawned with — the supervisor's
	// own binary, so a child's restart (RESTART marker -> re-exec) stays on
	// the same launch path the operator's shell uses.
	Binary string

	// WorkdirRoot is the machine-stated root each child's workdir derives
	// from (phase 2a of the daemon-supervisor proposal): <root>/<repo name>
	// for the repo the instance's project names. Empty = derivation is OFF
	// and children run on their config files' own workdirs — the phase-1
	// behaviour, unchanged. When set, EVERY instance is derived at
	// enumeration, and a failed derivation — the derived directory missing,
	// or not a checkout of the expected repo — refuses that daemon: it is
	// never spawned, the path tried is reported, and the supervisor's own
	// working directory is never used as a fallback (the CLA-441 failure).
	WorkdirRoot string

	BackoffBase       time.Duration
	BackoffCap        time.Duration
	BackoffResetAfter time.Duration
	SettleBeforeStop  time.Duration
	// StopLogEvery is how long a fleet stop may sit without progress before
	// the supervisor logs what it is still waiting for. 0 = defaultStopLogEvery.
	StopLogEvery time.Duration
}

func (o Options) withDefaults() Options {
	if o.BackoffBase <= 0 {
		o.BackoffBase = defaultBackoffBase
	}
	if o.BackoffCap <= 0 {
		o.BackoffCap = defaultBackoffCap
	}
	if o.BackoffResetAfter <= 0 {
		o.BackoffResetAfter = defaultBackoffResetAfter
	}
	if o.SettleBeforeStop <= 0 {
		o.SettleBeforeStop = defaultSettleBeforeStop
	}
	if o.StopLogEvery <= 0 {
		o.StopLogEvery = defaultStopLogEvery
	}
	return o
}

// Instance is one supervised config file: the child process serving it, and
// the bookkeeping that decides whether it gets respawned.
type Instance struct {
	path string // the config file
	name string // resolved fleet identity, for logs
	cfg  *config.Config

	mu         sync.Mutex
	stateDir   string    // resolved at the last spawn (may move across edits)
	cmd        *exec.Cmd // current child, nil while in backoff
	exited     bool      // the current child has exited
	aliveSince time.Time // when the current child was spawned
	exitErr    error

	restartAt   time.Time // zero = not scheduled for a restart
	lastBackoff time.Duration
	halted      bool // HALT marker seen: never respawn
}

// exitEvent is posted by a child's Wait goroutine when its process ends.
type exitEvent struct {
	inst *Instance
	err  error
}

// Supervisor holds the fleet state. Everything except the per-child Wait
// goroutines runs on the single Supervise goroutine, so no locks guard the
// scheduling fields — the mutex on Instance exists only for the hand-off
// between a Wait goroutine (exited/exitErr) and the main goroutine.
type Supervisor struct {
	o Options

	// hostname is the machine identity the fleet names resolve against,
	// resolved once at startup (ResolveInstanceName).
	hostname string

	instances []*Instance
	exits     chan exitEvent
	ctx       context.Context

	timer   *time.Timer
	timerCh <-chan time.Time
}

// Supervise runs the fleet until ctx is cancelled. Cancellation IS the
// fleet-wide stop: every child gets a STOP marker in its own state dir, each
// drains at its iteration boundary (possibly taking a while — an in-flight
// session finishes), and Supervise waits for all of them before returning.
//
// A config dir holding no instance configs is not an error: the supervisor
// says so and returns, rather than idling forever over nothing.
func Supervise(ctx context.Context, o Options) error {
	host, _ := os.Hostname()
	d := &Supervisor{o: o.withDefaults(), ctx: ctx, hostname: host}
	if err := d.enumerate(); err != nil {
		return err
	}
	if len(d.instances) == 0 {
		log.Printf("no instance configs in %s - nothing to supervise", d.o.ConfigDir)
		return nil
	}
	names := make([]string, len(d.instances))
	for i, inst := range d.instances {
		names[i] = inst.name
	}
	log.Printf("supervising %d instance(s) from %s: %s", len(d.instances), d.o.ConfigDir, strings.Join(names, ", "))
	d.exits = make(chan exitEvent, len(d.instances))

	if ctx.Err() != nil {
		// Cancelled before any child was spawned: nothing is running, so
		// there is nothing to drain. Returning here is not an optimisation —
		// stopAll would deadlock: every never-spawned instance still reads as
		// live (exited is false until a Wait goroutine reports) and stopAll
		// would block forever on exit events no goroutine will ever post.
		log.Print("stop requested before any child was started - nothing to stop")
		return nil
	}
	for _, inst := range d.instances {
		d.spawn(inst)
	}

	for {
		select {
		case <-ctx.Done():
			return d.stopAll()
		case ev := <-d.exits:
			d.onExit(ev.inst, ev.err)
		case <-d.timerCh:
			d.respawnDue()
			d.armRestart()
		}
	}
}

// enumerate reads every `*.json` in the config dir and keeps the ones that are
// actually instance configs. A file is skipped — loudly, so a file that stops
// being supervised is never a surprise — when it is not JSON, carries no
// recognized clankerbar key (MCP configs, headless permission policies and
// other JSON share this directory), fails to load, fails to resolve the
// machine conventions (the phase-2a workdir derivation and the same Load +
// Validate + state-dir resolution `run` performs), or fails validation. A
// file that fails here would either never have started a daemon or would
// crash-loop the child at startup; skipping it with the reason is strictly
// more useful than supervising a crash.
func (d *Supervisor) enumerate() error {
	files, err := filepath.Glob(filepath.Join(d.o.ConfigDir, "*.json"))
	if err != nil {
		return fmt.Errorf("list %s: %w", d.o.ConfigDir, err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			log.Printf("skipping %s: %v", f, err)
			continue
		}
		if !config.LooksLikeConfig(data) {
			log.Printf("skipping %s: not a clankerbar instance config (no recognized keys) - the config dir also holds MCP configs and permission policies, and those are not daemons", filepath.Base(f))
			continue
		}
		cfg, err := config.Load(f)
		if err != nil {
			log.Printf("skipping %s: %v", filepath.Base(f), err)
			continue
		}
		resolved, stateDir, err := d.resolveInstance(cfg)
		if err != nil {
			if d.o.WorkdirRoot != "" && errors.Is(err, ErrWorkdirRefused) {
				// The fail-closed workdir derivation (phase 2a). A refusal here
				// is like every other enumerate skip — the file would either
				// never have started a daemon or would run one somewhere
				// unverified — and the line reports the derived path that was
				// tried. There is deliberately NO fallback to the supervisor's
				// own working directory: that fallback is the CLA-441 failure,
				// and a daemon whose workdir cannot be derived is not started
				// at all.
				log.Printf("skipping %s: workdir derivation refused: %v", f, err)
			} else {
				log.Printf("skipping %s: %v", filepath.Base(f), err)
			}
			continue
		}
		d.instances = append(d.instances, &Instance{
			path:     f,
			name:     resolved.ResolvedInstanceName(d.hostname),
			cfg:      resolved,
			stateDir: stateDir,
		})
	}
	return nil
}

// resolveInstance resolves the machine conventions one instance runs under,
// in the order the materialized config is built from them: the phase-2a
// workdir derivation (when a machine root is stated) FIRST, so validation
// resolves every relative path and the mcp discovery against the DERIVED
// workdir rather than against wherever the supervisor happened to start; then
// the same Load + Validate + ResolveStateDir the daemon itself performs. The
// returned config is the one the materialized file is built from.
//
// The derivation applies only when WorkdirRoot is set — with no machine root
// stated, the config's own workdir governs (the phase-1 behaviour, unchanged)
// and the config validates exactly as `run` would validate it.
func (d *Supervisor) resolveInstance(cfg *config.Config) (*config.Config, string, error) {
	if d.o.WorkdirRoot != "" {
		derived, err := deriveInstanceWorkdirs(cfg, d.o.WorkdirRoot)
		if err != nil {
			return nil, "", err
		}
		applyDerivedWorkdirs(cfg, derived)
	}
	if err := cfg.Validate(); err != nil {
		return nil, "", err
	}
	stateDir, err := cfg.ResolveStateDir()
	if err != nil {
		return nil, "", err
	}
	return cfg, stateDir, nil
}

// spawn starts one instance's child. The source config is re-read and
// re-resolved first (same derivation + Load + Validate + ResolveStateDir the
// enumerate performed), so the supervisor's record of the state dir and the
// materialized config follow edits the operator made since the last spawn —
// the RUNNING child still reads the file it started with, and a RELOAD keeps
// that handle, so the freshly materialized file is exactly what the next
// child starts from. A source that no longer loads, derives or validates
// leaves the last good config in place: the child is spawned on the previous
// materialization, which is exactly what the on-disk cache is for.
//
// The child is spawned with `run -c` pointing at the MATERIALIZED config in
// its state dir — never the source file, so the config a daemon runs on is
// always the generated artifact. A materialization that cannot be written (an
// unwritable or unadoptable state dir) is as unexpected as a spawn failure:
// same backoff, same ladder, no child.
func (d *Supervisor) spawn(inst *Instance) {
	if fresh, err := config.Load(inst.path); err == nil {
		if resolved, sd, err := d.resolveInstance(fresh); err == nil {
			inst.mu.Lock()
			inst.cfg = resolved
			inst.stateDir = sd
			inst.mu.Unlock()
		} else {
			log.Printf("%s: re-resolving its config failed (%v) - spawning on the last materialized config", inst.name, err)
		}
	} else {
		log.Printf("%s: re-reading its config failed (%v) - spawning on the last materialized config", inst.name, err)
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
	cmd := exec.Command(d.o.Binary, "run", "-c", cfgPath)
	// The children's logs stream to the same stdout/stderr the supervisor's
	// do — the operator watches daemons today by watching one terminal per
	// daemon, and the fleet must not be quieter than that.
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
	inst.exitErr = nil
	inst.mu.Unlock()
	log.Printf("%s: spawned (pid %d, %s)", inst.name, cmd.Process.Pid, cfgPath)
	go func() {
		d.exits <- exitEvent{inst, cmd.Wait()}
	}()
}

// onExit classifies one child exit: a stop the supervisor ordered, a HALT the
// operator planted, or an unexpected exit. Only the last respawns.
func (d *Supervisor) onExit(inst *Instance, err error) {
	// The exited flag is set BEFORE the ctx check, not after: the select loop
	// may have picked this event in the same instant the fleet stop landed
	// (ctx.Done), and stopAll counts live children from this flag. If the
	// event is consumed here, nothing will ever post another one for this
	// child, so the flag must already say "exited" when stopAll looks.
	inst.mu.Lock()
	inst.exited = true
	inst.exitErr = err
	uptime := time.Since(inst.aliveSince)
	inst.mu.Unlock()

	if d.ctx.Err() != nil {
		return // the fleet is stopping; stopAll owns the outcome now
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

// haltPresent reports whether a HALT marker sits in the instance's state dir.
// The dir may not exist yet (first spawn) or the marker may be absent — both
// read as false.
func (d *Supervisor) haltPresent(inst *Instance) bool {
	inst.mu.Lock()
	dir := inst.stateDir
	inst.mu.Unlock()
	if dir == "" {
		return false
	}
	_, err := os.Lstat(filepath.Join(dir, "HALT"))
	return err == nil
}

// respawnDue starts every instance whose restart time has arrived.
func (d *Supervisor) respawnDue() {
	now := time.Now()
	for _, inst := range d.instances {
		if inst.restartAt.IsZero() || inst.restartAt.After(now) {
			continue
		}
		inst.restartAt = time.Time{}
		d.spawn(inst)
	}
}

// armRestart points the select loop's timer at the earliest pending restart,
// or disarms it when nothing is pending.
func (d *Supervisor) armRestart() {
	if d.timer != nil {
		d.timer.Stop()
		d.timerCh = nil
	}
	var next time.Time
	for _, inst := range d.instances {
		if !inst.restartAt.IsZero() && (next.IsZero() || inst.restartAt.Before(next)) {
			next = inst.restartAt
		}
	}
	if next.IsZero() {
		return
	}
	d.timer = time.NewTimer(time.Until(next))
	d.timerCh = d.timer.C
}

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
			done[ev.inst] = true
			lastExit = time.Now()
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
				log.Printf("stop still waiting for %d instance(s) after %s (no exit in %s): %s - they stop at their iteration boundary; the supervisor keeps waiting rather than killing them",
					len(stuck), time.Since(waitStart).Round(time.Second), quiet.Round(time.Second), strings.Join(stuck, ", "))
			}
		}
		for _, inst := range d.instances {
			if done[inst] || stopped[inst] {
				continue
			}
			inst.mu.Lock()
			alive := !inst.exited
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

// writeStop writes the STOP marker into one instance's state dir, or logs
// loudly why it could not. Two rules keep the write honest:
//
//   - The child must be past its STARTUP marker consumption before a STOP
//     lands. A STOP the daemon finds at startup is consumed WITHOUT stopping
//     (CLA-491: a marker a previous run left behind must not kill the next
//     one), so a stop written into that window would be eaten and the child
//     would run on. stopAll only calls this once the child has been alive for
//     the settle window, which guarantees it has passed that consumption.
//   - The state dir is never CREATED here: a dir the supervisor created for a
//     child that has not started yet would be adopted by that child, whose
//     startup would then eat the marker exactly as above. Writing into a dir
//     the child itself created (the Lstat gate in stopAll) is what guarantees
//     the child is past its startup marker check.
//
// A child that cannot be stopped this way — the state dir refuses to open, or
// the marker write fails — is logged loudly and left running: the supervisor
// keeps waiting for its exit, because stopping it by killing the process is
// exactly what this phase must not do.
func (d *Supervisor) writeStop(inst *Instance, dir string) {
	inst.mu.Lock()
	cfg := inst.cfg
	inst.mu.Unlock()
	st, err := statedir.Open(dir, cfg.SessionWorkDirs()...)
	if err != nil {
		log.Printf("%s: cannot open its state dir to write STOP: %v - the child keeps running; stop it by hand (kill the supervisor's child pid %d)", inst.name, err, instPid(inst))
		return
	}
	defer st.Close() //nolint:errcheck // read-side handle on the way out
	body := []byte(fmt.Sprintf("fleet-wide stop requested by the supervisor at %s\n", time.Now().UTC().Format(time.RFC3339)))
	if err := st.WriteFile("STOP", body); err != nil {
		log.Printf("%s: cannot write STOP into %s: %v - the child keeps running; stop it by hand", inst.name, dir, err)
		return
	}
	log.Printf("%s: STOP written to %s - draining at its iteration boundary", inst.name, dir)
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
