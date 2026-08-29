// Package supervisor runs the fleet supervisor (CLA-525, phases 1-3 and 5a of
// docs/proposals/daemon-supervisor.md): bare `clankerbar` — or
// `clankerbar supervise` — reconciles the machine against the plane's
// account-scoped roster: every entry with `desired: running` has a supervised
// child up, every entry with `desired: stopped` has its stop marker written and
// drains at its iteration boundary, and flipping desired state in the console
// lands on the machine within one poll, with no operator command.
//
// The supervisor deliberately invents nothing. Each child is
// `clankerbar run -c <file>` — the exact command the operator used to run by
// hand — except the file is the GENERATED effective config in the child's
// state dir (phase 2b): the operator's machine-layer local config with the
// roster entry's project and its one permitted policy override (`harness` and
// its per-harness block, Decision 1) materialized over it, regenerated on
// every reconcile. Instance identity (`instance_name`), per-instance state
// dirs and the fleet beacon all behave exactly as before — identity because
// the generated file pins the roster name, and the state dir because the
// generated file pins that too.
//
// The one-directional merge is an ALLOWLIST (roster.go): plane keys may set
// policy and may NEVER reach `env`, `settings_path`, `workdir` or
// `mcp_config_path`, and no run-config key other than `harness` and its block
// is per-instance. A roster entry carrying one is refused loudly and named in
// the log, never silently dropped — a silent drop is how a daemon ends up
// looking configured and not being.
//
// Stopping a child means writing the STOP marker into its state dir, the same
// marker `clankerbar ctl stop` writes, never killing the process: the daemon
// drains at its iteration boundary and exits by itself. No new daemon-side
// control mechanism exists — the supervisor is a translator, not a second
// control plane. The one rule the supervisor adds is the restart decision: a
// child that exits for any reason other than a stop it was told about is
// respawned, with backoff. The permission-policy gate (phase 2c) runs at the
// child-start gate: a config whose settings_path names an absent file is
// refused rather than started without it — at reconcile, and again at every
// spawn.
//
// The plane is polled, not held (Decision 2 — desired state, not commands: a
// missed poll is caught by the next one, and nothing a reconnect could lose is
// lost). An unreachable plane does not stop the fleet: a warm supervisor keeps
// reconciling against the last-known-good roster, and a cold start reconciles
// from the cached roster + materialized configs on disk (phase 2b wrote them
// for exactly this) rather than starting nothing.
//
// Since phase 5a the supervisor's own log is a status surface: on every change
// of the fleet it lists the version it itself runs and, per instance, the
// state and the version of the child serving it — so version skew between
// children (and between a child and the supervisor) is observable on the
// machine, not only on the fleet page.
package supervisor

import (
	"context"
	"encoding/json"
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
	"github.com/lecstor/clankerbar-cli/internal/version"
)

// Defaults for the restart backoff, the stop settle window, and the roster
// poll. All are overridable through Options, which is what the tests do.
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
	// defaultPollInterval is how often the account-scoped roster is polled. A
	// console flip lands within one poll, so this is the steering latency of
	// the whole fleet; 15s is a toggle, not a queue.
	defaultPollInterval = 15 * time.Second
)

// Options configures one supervisor run. Zero values take the defaults above.
type Options struct {
	// Binary is the executable children are spawned with — the supervisor's
	// own binary, so a child's restart (RESTART marker -> re-exec) stays on
	// the same launch path the operator's shell uses.
	Binary string

	// WorkdirRoot is the machine-stated root each child's workdir derives
	// from (phase 2a of the daemon-supervisor proposal): <root>/<repo name>
	// for the repo the instance's project names, taken from the roster entry
	// (the plane's `project.primary_repo`). Empty = derivation is OFF and
	// children run on the base config's own workdirs. When set, EVERY
	// instance is derived at reconcile, and a failed derivation — the derived
	// directory missing, or not a checkout of the expected repo — refuses
	// that daemon: it is never spawned, the path tried is reported, and the
	// supervisor's own working directory is never used as a fallback (the
	// CLA-441 failure).
	WorkdirRoot string

	// RosterURL is the account-scoped roster endpoint the supervisor polls
	// (`<origin>/api/daemon-roster`). Empty = not wired: Supervise refuses to
	// run, because a supervisor without the roster is a supervisor with no
	// desired state.
	RosterURL string

	// APIKey is the account key the roster is fetched with (CLANKERBAR_API_KEY
	// in the environment). Empty = not wired, like RosterURL.
	APIKey string

	// BaseCfg is the machine-layer config every instance is built from: the
	// operator's local config carrying env, settings_path, config_dir,
	// mcp_config_path and the rest of what can never come from the plane.
	// Nil = the discovered local config, or bare defaults when none exists.
	BaseCfg *config.Config

	// RosterCacheDir is where the supervisor keeps the last-known-good roster
	// and the per-instance state dirs (with their materialized configs).
	// Empty = the default under the loop state root.
	RosterCacheDir string

	// PollInterval is how often the roster is polled. 0 = defaultPollInterval.
	PollInterval time.Duration

	BackoffBase       time.Duration
	BackoffCap        time.Duration
	BackoffResetAfter time.Duration
	SettleBeforeStop  time.Duration
	// StopLogEvery is how long a fleet stop may sit without progress before
	// the supervisor logs what it is still waiting for. 0 = defaultStopLogEvery.
	StopLogEvery time.Duration

	// VersionOf returns the version recorded for a child at the moment it is
	// spawned: the binary version the child was just launched with. Nil =
	// version.Current — the supervisor's own build, which is exactly the
	// binary every child is spawned from (phase 5a discovers without a
	// per-daemon file: no query, no version file, the spawn is the
	// derivation). The hook exists so tests can simulate the phase-5b roll —
	// children spawned before a binary swap stay on the old version while
	// later spawns carry the new one — the one situation in which a single
	// supervisor's children can carry different versions.
	VersionOf func() string
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
	if o.PollInterval <= 0 {
		o.PollInterval = defaultPollInterval
	}
	return o
}

// Instance is one supervised roster entry: the child process serving it, and
// the bookkeeping that decides whether it gets respawned.
type Instance struct {
	name  string       // the roster entry's name (also the fleet identity)
	entry *RosterEntry // the roster row this instance serves

	cfg      *config.Config
	stateDir string

	// desired is the entry's last reconciled desired state. Anything other
	// than "running" — stopped, or an entry that vanished or was refused —
	// means the child must not be respawned once it is gone.
	desired string
	// removed marks an instance whose entry is no longer on the roster: stop
	// the child, never respawn, drop the instance once it has exited.
	removed bool
	// refused marks an instance whose entry failed the allowlist or shape
	// checks: refused loudly, child stopped, never respawned.
	refused bool
	// stopRequested is set once a STOP marker has been written for a stopped
	// instance's live child, so repeated polls write nothing (idempotent
	// reconcile). Cleared on the next spawn.
	stopRequested bool
	// policyRefused is set when the last config build hit the permission-
	// policy gate: the spawn is refused (even on a last-known-good config
	// whose own policy file still exists), and the next poll retries the
	// build — a restored policy file brings the daemon back without a
	// supervisor restart (phase 2c).
	policyRefused bool

	mu         sync.Mutex
	cmd        *exec.Cmd // current child, nil while in backoff
	exited     bool      // the current child has exited
	aliveSince time.Time // when the current child was spawned
	exitErr    error
	// childVersion is the binary version of the current child, recorded at
	// its spawn (phase 5a): the version of the binary it was launched from.
	// It is the version the status listing reports; a respawn after a binary
	// swap records the new version. Empty = the instance has never spawned a
	// child.
	childVersion string

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
	// resolved once at startup.
	hostname string

	// roster is the account-scoped poll client; entries is the last-known-good
	// roster the supervisor reconciles against when the plane is unreachable.
	roster  *RosterClient
	entries []RosterEntry

	// cacheDir holds the cached roster and the per-instance state dirs.
	cacheDir string

	// loggedRemote remembers remote entries already reported, so the "ignored
	// without error" line is said once per entry, not once per poll.
	loggedRemote map[string]bool

	instances []*Instance
	exits     chan exitEvent
	ctx       context.Context

	timer   *time.Timer
	timerCh <-chan time.Time

	// lastStatus is the last fleet-status listing printed, so logFleetStatus
	// prints once per CHANGE: idempotent polls print nothing, exactly as
	// reconcile itself writes nothing.
	lastStatus string
}

// Supervise runs the fleet until ctx is cancelled. Cancellation IS the
// fleet-wide stop: every child gets a STOP marker in its own state dir, each
// drains at its iteration boundary (possibly taking a while — an in-flight
// session finishes), and Supervise waits for all of them before returning.
//
// The first reconcile happens before the select loop; the plane is polled
// from then on. A roster with nothing local on it (empty, or all remote) is
// not an error: the supervisor says so and returns, rather than idling forever
// over nothing — and an unreachable plane with no cached roster is the same
// shape: nothing to reconcile from, so nothing to supervise.
func Supervise(ctx context.Context, o Options) error {
	host, _ := os.Hostname()
	d := &Supervisor{
		o:            o.withDefaults(),
		ctx:          ctx,
		hostname:     host,
		roster:       NewRosterClient(o.RosterURL, o.APIKey),
		loggedRemote: map[string]bool{},
	}

	if ctx.Err() != nil {
		// Cancelled before any child was spawned: nothing is running, so
		// there is nothing to drain. Returning here is not an optimisation —
		// stopAll would deadlock: every never-spawned instance still reads as
		// live (exited is false until a Wait goroutine reports) and stopAll
		// would block forever on exit events no goroutine will ever post.
		log.Print("stop requested before any child was started - nothing to stop")
		return nil
	}

	if o.RosterCacheDir != "" {
		d.cacheDir = o.RosterCacheDir
	} else {
		dir, err := rosterCacheDir()
		if err != nil {
			return fmt.Errorf("supervise: %w", err)
		}
		d.cacheDir = dir
	}

	// The exit channel must exist BEFORE the first reconcile spawns children:
	// a child that crashes between its spawn and the channel's creation would
	// block forever on a nil-channel send, and its exit event would never
	// reach the loop — no respawn, and stopAll would wait on a child that was
	// already gone. The loop consumes constantly, so a modest fixed buffer is
	// all a burst of simultaneous exits needs.
	d.exits = make(chan exitEvent, 16)

	if err := d.reconcile(); err != nil {
		return err
	}
	if d.entries == nil {
		// The plane was unreachable at start and no cached roster exists;
		// reconcile already said so. Nothing to supervise.
		return nil
	}
	if len(d.instances) == 0 {
		log.Print("roster holds no local instances - nothing to supervise")
		return nil
	}

	pollTicker := time.NewTicker(d.o.PollInterval)
	defer pollTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return d.stopAll()
		case ev := <-d.exits:
			d.onExit(ev.inst, ev.err)
		case <-d.timerCh:
			d.respawnDue()
			d.armRestart()
		case <-pollTicker.C:
			d.reconcile()
		}
	}
}

// reconcile is one poll-and-apply pass: fetch the roster (or fall back to the
// last-known-good one when the plane is unreachable), refresh every instance's
// config from its entry, refuse what the allowlist refuses, and act — spawn
// the running, stop the stopped, drop the gone. It is idempotent by
// construction: unchanged state spawns nothing and writes nothing.
func (d *Supervisor) reconcile() error {
	if d.ctx.Err() != nil {
		// The fleet stop is landing: the select loop may have picked this poll
		// tick over ctx.Done, and acting on the roster now could spawn a child
		// stopAll is about to drain. A cancelled context reconciles to nothing.
		return nil
	}
	entries, err := d.roster.Fetch(d.ctx)
	if err != nil {
		if errors.Is(err, ErrNotWired) {
			return err
		}
		if d.entries == nil {
			// Cold start, plane unreachable: the last-known-good roster on
			// disk is the fallback — a network blip must not be an outage
			// (the proposal's offline seam, which phase 2b wrote materialized
			// configs to disk for).
			if cached := loadCachedRoster(d.cacheDir); len(cached) > 0 {
				log.Printf("plane unreachable at start (%v) - starting from the cached roster", err)
				d.entries = cached
			} else {
				log.Printf("plane unreachable at start (%v) and no cached roster - nothing to supervise", err)
				return nil
			}
		} else {
			log.Printf("plane unreachable (%v) - reconciling against the last-known-good roster", err)
		}
		entries = d.entries
	} else {
		d.entries = entries
		writeCachedRoster(d.cacheDir, entries)
	}

	byName := make(map[string]*Instance, len(d.instances))
	for _, inst := range d.instances {
		byName[inst.name] = inst
	}
	live := make(map[string]bool, len(entries))
	for i := range entries {
		e := &entries[i]
		live[e.Name] = true
		if e.Placement == RosterPlacementRemote {
			// Decision 7: remote placement is not implemented, and the plane
			// refuses it at write time anyway — but the supervisor must not
			// fail on one, so it is ignored. Said once per entry, not once
			// per poll.
			if !d.loggedRemote[e.Name] {
				log.Printf("roster: ignoring entry %q - placement remote is not implemented (Decision 7)", e.Name)
				d.loggedRemote[e.Name] = true
			}
			continue
		}
		if err := checkEntry(e); err != nil {
			// Refused loudly, named in the log, never silently dropped. The
			// plane's own write-time gate should make this unreachable; the
			// supervisor's copy is the boundary's last line.
			log.Print(err)
			if inst := byName[e.Name]; inst != nil {
				inst.refused = true
			}
			continue
		}
		inst := byName[e.Name]
		if inst == nil {
			inst = &Instance{name: e.Name, entry: e}
			d.instances = append(d.instances, inst)
		}
		// The entry is the desired state: refresh the pointer every poll, so
		// an edit on the plane (a harness override, a project list, the
		// desired state) is what the next build reads. Holding the first
		// poll's entry would freeze the instance on the configuration that
		// created it.
		inst.entry = e
		inst.refused = false
		inst.removed = false
		inst.desired = e.DesiredState
		d.refreshInstance(inst)
	}

	// An instance whose entry is gone (deleted in the console) is a stop, not
	// a mystery: the operator removed its configuration, and the supervisor
	// must not keep the daemon it was configuration for.
	for _, inst := range d.instances {
		if inst.refused || live[inst.name] {
			continue
		}
		if !inst.removed {
			inst.removed = true
			log.Printf("%s: no longer on the roster - writing STOP and leaving it stopped", inst.name)
		}
	}

	// Act on the fleet.
	for _, inst := range d.instances {
		switch {
		case inst.refused || inst.removed || inst.desired == RosterDesiredStopped:
			d.ensureStopped(inst)
		default:
			d.ensureRunning(inst)
		}
	}

	// Drop instances that are gone and have already exited; the ones still
	// draining stay until their child's exit event arrives.
	kept := d.instances[:0]
	for _, inst := range d.instances {
		inst.mu.Lock()
		alive := inst.cmd != nil && !inst.exited
		inst.mu.Unlock()
		if (inst.removed || inst.refused) && !alive {
			continue
		}
		kept = append(kept, inst)
	}
	d.instances = kept

	// The fleet changed (or not): the status surface follows reconcile, so a
	// spawn, a stop, a flip or a removal is reflected within the same poll —
	// and an unchanged pass prints nothing.
	d.logFleetStatus()

	d.armRestart()
	return nil
}

// refreshInstance rebuilds one instance's effective config from its roster
// entry. The result is what the next spawn materializes. A build that fails:
//
//   - with ErrPolicyRefused (phase 2c) refuses the spawn — even on the last
//     known config, because that config's own policy file may be the one the
//     operator replaced — and the next poll retries the build;
//   - for any other reason (workdir derivation, validation, an unknown
//     harness) keeps the last-known-good: the previous config, or the
//     materialized config on disk, so a blip never takes a running daemon
//     down. Loud, never silent: the log says why the child will run on the
//     old config.
func (d *Supervisor) refreshInstance(inst *Instance) {
	fresh, sd, err := d.buildConfig(inst.entry)
	if err == nil {
		inst.mu.Lock()
		inst.cfg = fresh
		inst.stateDir = sd
		inst.mu.Unlock()
		inst.policyRefused = false
		return
	}
	if errors.Is(err, ErrPolicyRefused) {
		log.Printf("%s: refused: %v - retrying on the next poll", inst.name, err)
		inst.policyRefused = true
		return
	}
	// The state dir is deterministic from the entry name, so a cold start
	// whose build fails (the plane unreachable and the base config no longer
	// resolvable — a moved workdir, a corrupt edit) can still find the
	// last-known-good materialized config phase 2b wrote for exactly this.
	inst.mu.Lock()
	if inst.stateDir == "" {
		inst.stateDir = rosterStateDir(d.cacheDir, inst.entry.Name)
	}
	inst.mu.Unlock()
	if loaded := d.loadMaterialized(inst); loaded != nil {
		inst.mu.Lock()
		inst.cfg = loaded
		inst.mu.Unlock()
		log.Printf("%s: building its config failed (%v) - spawning on the last-known-good materialized config", inst.name, err)
		return
	}
	if inst.cfg != nil {
		log.Printf("%s: building its config failed (%v) - keeping the previous config", inst.name, err)
		return
	}
	log.Printf("%s: building its config failed (%v) and no last-known-good exists - not started", inst.name, err)
}

// buildConfig materializes one roster entry into an effective config: the
// machine-layer base (the operator's local config), with the entry's project
// list, its one permitted policy override (`harness` and the per-harness
// block's model/models half), and the pinned identity and state dir written
// over it — then the same resolution `run` performs: the phase-2a workdir
// derivation, Validate, the phase-2c permission-policy gate, and the state-dir
// resolution.
func (d *Supervisor) buildConfig(e *RosterEntry) (*config.Config, string, error) {
	base := d.o.BaseCfg
	if base == nil {
		var err error
		base, err = config.Load("")
		if err != nil {
			return nil, "", err
		}
	} else if src := base.Source(); src != "" {
		// The machine layer is the operator's local config, and edits to it
		// apply at the next reconcile exactly as source-file edits did in
		// phase 2: re-read it on every build. A re-read that fails (a corrupt
		// edit, a deleted file) keeps the last good base — the same fallback
		// posture as every other failed re-read.
		if re, err := config.Load(src); err == nil {
			base = re
		} else {
			log.Printf("re-reading the supervisor's local config (%s) failed (%v) - keeping the last good one", src, err)
		}
	}
	cfg := base.Clone()
	// The roster owns the projects: each entry names the project(s) it
	// drives, and each project's primary repo is what the workdir derivation
	// derives from (the plane's `project.primary_repo` in place of the local
	// declaration phase 2a read).
	cfg.InstanceName = e.Name
	cfg.StateDir = rosterStateDir(d.cacheDir, e.Name)
	cfg.Projects = make([]config.Project, 0, len(e.Projects))
	for _, p := range e.Projects {
		cfg.Projects = append(cfg.Projects, config.Project{
			Slug:        strings.TrimSpace(p.Slug),
			PrimaryRepo: strings.TrimSpace(p.PrimaryRepo),
		})
	}
	// Decision 1: harness and its per-harness block are the ONLY per-instance
	// policy overrides. checkEntry has already refused every other key; what
	// lands here is exactly the two allowed ones.
	if raw, ok := e.Overrides["harness"]; ok {
		var h string
		if err := json.Unmarshal(raw, &h); err != nil {
			return nil, "", fmt.Errorf("entry %q: harness override is not a string: %v", e.Name, err)
		}
		cfg.Harness = strings.TrimSpace(h)
	}
	if raw, ok := e.Overrides["harnesses"]; ok {
		var blocks map[string]config.RunConfigHarnessBlock
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return nil, "", fmt.Errorf("entry %q: harnesses override is not a per-harness block: %v", e.Name, err)
		}
		for name, b := range blocks {
			hc := cfg.Harnesses[name]
			hc.Model = strings.TrimSpace(b.Model)
			if b.Models != nil {
				hc.Models = b.Models
			}
			cfg.Harnesses[name] = hc
		}
	}
	return d.resolveInstance(cfg)
}

// loadMaterialized loads an instance's last-known-good materialized config
// from its state dir — the file phase 2b wrote and the child last ran on. It
// is the cold-start fallback: a config the roster entry can no longer produce
// (the workdir moved while the plane was unreachable) still has its last
// materialization on disk, and the daemon runs on that rather than not at all.
func (d *Supervisor) loadMaterialized(inst *Instance) *config.Config {
	inst.mu.Lock()
	dir := inst.stateDir
	inst.mu.Unlock()
	if dir == "" {
		return nil
	}
	cfg, err := config.Load(filepath.Join(dir, statedir.MaterializedConfigName))
	if err != nil {
		return nil
	}
	if err := cfg.Validate(); err != nil {
		return nil
	}
	return cfg
}

// ensureRunning brings one instance to its desired running state: a live child
// needs nothing (idempotent), a backoff respawn already scheduled needs
// nothing (the timer owns it), a refused or config-less instance starts
// nothing.
func (d *Supervisor) ensureRunning(inst *Instance) {
	if inst.policyRefused || inst.halted {
		// Refused at build time: the spawn must not fall back to the last
		// config, and the next poll retries the build. HALTed: the operator
		// planted the per-instance stop switch; the poll must not undo it.
		return
	}
	inst.mu.Lock()
	alive := inst.cmd != nil && !inst.exited
	pending := !inst.restartAt.IsZero()
	inst.mu.Unlock()
	if alive || pending {
		return
	}
	if inst.cfg == nil {
		return // nothing to start yet: the build failed and no last-known-good exists
	}
	d.spawn(inst)
}

// ensureStopped brings one instance to its desired stopped state: STOP is
// written exactly once per live child (past the settle window, so the marker
// cannot be eaten by the child's startup consumption — CLA-491), and the
// child drains at its iteration boundary. Idempotent: a STOP already written
// writes nothing more.
func (d *Supervisor) ensureStopped(inst *Instance) {
	inst.mu.Lock()
	alive := inst.cmd != nil && !inst.exited
	since := time.Since(inst.aliveSince)
	dir := inst.stateDir
	inst.mu.Unlock()
	if !alive {
		return
	}
	if inst.stopRequested {
		return // STOP already written; waiting for the drain
	}
	if since < d.o.SettleBeforeStop {
		return // inside the startup window; the next poll writes it
	}
	if dir == "" {
		return
	}
	if _, err := os.Lstat(dir); err != nil {
		return // no state dir yet; the next poll writes it
	}
	d.writeStop(inst, dir)
	inst.stopRequested = true
}

// resolveInstance resolves the machine conventions one instance runs under,
// in the order the materialized config is built from them: the phase-2a
// workdir derivation (when a machine root is stated) FIRST, so validation
// resolves every relative path and the mcp discovery against the DERIVED
// workdir rather than against wherever the supervisor happened to start; then
// the same Load + Validate + ResolveStateDir the daemon itself performs. The
// returned config is the one the materialized file is built from.
//
// The permission-policy gate (phase 2c) runs after Validate, on the RESOLVED
// settings_path: an absent policy file refuses the instance, so it is never
// spawned. Like the workdir gate, there is no fallback branch — a daemon whose
// policy cannot be verified is not started at all.
//
// The derivation applies only when WorkdirRoot is set — with no machine root
// stated, the base config's own workdir governs and the config validates
// exactly as `run` would validate it.
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
	if err := checkPermissionPolicy(cfg); err != nil {
		return nil, "", err
	}
	stateDir, err := cfg.ResolveStateDir()
	if err != nil {
		return nil, "", err
	}
	return cfg, stateDir, nil
}

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
	inst.childVersion = childVersion(d.o.VersionOf)
	inst.mu.Unlock()
	// A fresh child starts clean: the next stopped flip must write its STOP
	// again, it must not inherit the previous child's marker state.
	inst.stopRequested = false
	log.Printf("%s: spawned (pid %d, %s, version %s)", inst.name, cmd.Process.Pid, cfgPath, inst.childVersion)
	d.logFleetStatus()
	go func() {
		d.exits <- exitEvent{inst, cmd.Wait()}
	}()
}

// onExit classifies one child exit: a stop the supervisor ordered (desired
// state, roster removal, refusal), a HALT the operator planted, or an
// unexpected exit. Only the last respawns.
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

	// A child that was told to stop — desired stopped, entry removed,
	// entry refused — is not restarted. The desired flip is reconciled on the
	// next poll, so a child that dies between the flip and the STOP write is
	// also left stopped: the instance's desired state is authoritative, not
	// the marker's arrival.
	if inst.desired != RosterDesiredRunning || inst.removed || inst.refused {
		// The fleet changed: the listing flips this line now — "stopped",
		// or "removed"/"refused" for an entry that is about to leave the
		// fleet — rather than on the next poll.
		d.logFleetStatus()
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
		d.logFleetStatus()
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
	d.logFleetStatus()
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

// fleetStatus builds the supervisor's status surface (phase 5a): the header
// names the version the supervisor itself runs, and each instance line names
// its state and the version of the child serving it — the version recorded at
// THAT child's spawn, which is what makes version skew observable on the
// machine: a child spawned before a binary swap stays on the old version
// while later spawns carry the new one, and both sit next to the
// supervisor's own version in the same listing.
func (d *Supervisor) fleetStatus() string {
	var b strings.Builder
	fmt.Fprintf(&b, "fleet status: supervisor %s, %d instance(s)", version.Current, len(d.instances))
	for _, inst := range d.instances {
		inst.mu.Lock()
		alive := inst.cmd != nil && !inst.exited
		pid := 0
		if alive && inst.cmd.Process != nil {
			pid = inst.cmd.Process.Pid
		}
		v := inst.childVersion
		restartAt := inst.restartAt
		b.WriteString("\n  " + inst.name + ": ")
		switch {
		case alive && inst.stopRequested:
			fmt.Fprintf(&b, "stopping (pid %d, version %s)", pid, v)
		case alive:
			fmt.Fprintf(&b, "running (pid %d, version %s)", pid, v)
		case inst.removed:
			b.WriteString("removed")
		case inst.refused:
			b.WriteString("refused")
		case inst.halted:
			b.WriteString("halted")
		case inst.policyRefused:
			b.WriteString("refused (permission policy)")
		case inst.desired != RosterDesiredRunning:
			b.WriteString("stopped")
		case !restartAt.IsZero():
			// The SCHEDULED delay, not the remaining time: the listing text
			// must not drift between polls, or logFleetStatus would re-print
			// it on every poll for the whole backoff window. scheduleRestart
			// sets lastBackoff and restartAt together, so the pending
			// restart's delay is exactly what lastBackoff holds.
			fmt.Fprintf(&b, "restarting in %s (last version %s)", inst.lastBackoff, v)
		default:
			if v != "" {
				fmt.Fprintf(&b, "down (last version %s)", v)
			} else {
				b.WriteString("down")
			}
		}
		inst.mu.Unlock()
	}
	return b.String()
}

// logFleetStatus prints the fleet status listing once per CHANGE: an
// unchanged listing prints nothing, so idempotent polls stay silent (the
// status surface is as idempotent as the reconcile it follows) and a
// crash-looping child costs at most one listing per transition, not one per
// poll. An empty fleet prints nothing — the empty-roster line already says
// everything.
func (d *Supervisor) logFleetStatus() {
	if len(d.instances) == 0 {
		return
	}
	text := d.fleetStatus()
	if text == d.lastStatus {
		return
	}
	d.lastStatus = text
	log.Print(text)
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
		// The schedule was made when the instance still wanted a child; a poll
		// may have flipped it since (stopped, removed, refused, policy-refused,
		// HALTed) and the timer path must not spawn against the current desired
		// state any more than the reconcile path would. Dropping the stale
		// schedule is the idempotent move: the next reconcile re-evaluates the
		// instance from the roster.
		if inst.desired != RosterDesiredRunning || inst.removed || inst.refused || inst.policyRefused || inst.halted {
			continue
		}
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

// writeStop writes the STOP marker into one instance's state dir, or logs
// loudly why it could not. Two rules keep the write honest:
//
//   - The child must be past its STARTUP marker consumption before a STOP
//     lands. A STOP the daemon finds at startup is consumed WITHOUT stopping
//     (CLA-491: a marker a previous run left behind must not kill the next
//     one), so a stop written into that window would be eaten and the child
//     would run on. The callers only call this once the child has been alive
//     for the settle window, which guarantees it has passed that consumption.
//   - The state dir is never CREATED here: a dir the supervisor created for a
//     child that has not started yet would be adopted by that child, whose
//     startup would then eat the marker exactly as above. Writing into a dir
//     the child itself created (the Lstat gate in the callers) is what
//     guarantees the child is past its startup marker check.
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
