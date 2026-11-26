// Package supervisor runs the fleet supervisor (CLA-525, phases 1-3 and 5a of
// docs/proposals/daemon-supervisor.md): bare `clankerbar` — or
// `clankerbar supervise` — reconciles the machine against the plane's
// account-scoped roster: every entry with `desired: running` has a supervised
// child up, every entry with `desired: stopped` has its stop marker written and
// drains at its next phase seam or iteration boundary, and flipping desired state in the console
// lands on the machine within one poll, with no operator command.
//
// The supervisor deliberately invents nothing. Each child is
// `clankerbar run -c <file>` — the exact command the operator used to run by
// hand — except the file is the GENERATED effective config in the child's
// state dir (phase 2b): the operator's machine-layer local config with the
// roster entry's project and its one permitted policy override (`harness` and
// its per-harness block, Decision 1) materialized over it, written when the
// child is spawned and rewritten at reconcile when an edit changes it
// (CLA-724). Instance identity (`instance_name`), per-instance state
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
// drains at its next phase seam or iteration boundary and exits by itself. No new daemon-side
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
//
// CLA-677 adds the plane-facing half of that surface (status.go): every poll,
// and every child state change, POSTs the same view —
// `<backlog_url>/api/daemon-roster/status` — so the console can tell a HALTed
// instance from a crashed one without reading this machine's log. Reporting is
// fail-soft telemetry and never control flow.
//
// CLA-758 adds the deterministic half of INTEGRATION: an approved-awaiting-merge
// task whose pull request is mergeable, green and based on the project's
// integration branch is claimed, merged as a merge commit and closed done by this
// process, with no harness session at all. Everything it cannot do without
// judgement — a conflict above all — is left exactly where the agent close-out
// already picks it up. See closeout.go.
package supervisor

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/delivery"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
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
	// from (phase 2a of the daemon-supervisor proposal): the per-project layout
	// <root>/<project slug>/<repo name> with the project's worktrees under
	// <root>/<project slug>/worktrees (CLA-678). Empty = derivation is OFF and
	// children run on the base config's own workdirs. When set, EVERY
	// instance is derived at reconcile: each repo the project names is cloned
	// if it is not there yet, a directory that exists but is not a checkout of
	// the expected repo refuses that daemon, and a failed derivation — the path
	// tried is reported — refuses the daemon too: it is never spawned, and the
	// supervisor's own working directory is never used as a fallback (the
	// CLA-441 failure).
	WorkdirRoot string

	// CloneURLBase is the URL prefix a derived clone is made from:
	// <base><owner>/<name>.git. Empty = config.DefaultCloneURLBase
	// (`https://github.com/`), which is what every config that does not set
	// clone_url_base keeps using. Set it through the base config's
	// clone_url_base; the CLI passes that value here.
	CloneURLBase string

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

	// NoEvents is the supervisor's machine-local agent-event opt-out
	// (`supervise --no-events`): children are spawned with CLANKERBAR_EVENTS=off
	// so every daemon under this supervisor has the channel disabled. It can only
	// turn the channel OFF; the project's dial is still the only way on. A
	// supervisor launched with CLANKERBAR_EVENTS=off already passes it through
	// the inherited environment; this makes the flag equivalent.
	NoEvents bool

	// RosterCacheDir is where the supervisor keeps the last-known-good roster
	// and the per-instance state dirs (with their materialized configs).
	// Empty = the default under the loop state root.
	RosterCacheDir string

	// LogToFile writes the supervisor's own lines to supervise.log in that
	// state dir, beside them and rotated at a size bound (CLA-701), as well as
	// to stderr. Default FALSE, deliberately: this redirects the PROCESS-WIDE
	// standard logger, and a library caller (every test) that never asked for
	// a file should not have its log capture taken over. The CLI sets it; the
	// supervisor is a long-lived process, so its lines need the same durable
	// home the daemons' got.
	LogToFile bool

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

	// VerifyTimeout is how long the phase-5b roll waits for ONE child's next
	// beacon to report the target version before halting the roll. It also
	// bounds the phase-5c replacement's one unavoidable wait: a conflict-only
	// session (CLA-759) is an in-process goroutine that exec cannot carry
	// across, so the replace waits for it to finish before the handoff. The
	// fleet itself no longer drains for a replacement — the children are
	// adopted across the exec. 0 = defaultVerifyTimeout.
	VerifyTimeout time.Duration

	// LaunchVersion reports the version of the binary at the fleet's launch
	// path — the file children re-exec (RESTART) and respawn from. The roll
	// (phase 5b) compares it against its own build BEFORE touching any child:
	// the operator installs the new build at the launch path and runs the
	// roll from it, and a roll whose launch path still carries another
	// version refuses rather than rolling nothing. The replace (phase 5c)
	// compares it against the marker's requesting version BEFORE handing the
	// fleet to the new image. Nil = run `<Binary> version` and parse the line.
	LaunchVersion func() (string, error)

	// ExecInPlace replaces the supervisor's process image in place with the
	// binary at the fleet's launch path, invoked with the same argv and env
	// (phase 5c). Nil = the platform's exec(2). The hook exists so tests can
	// simulate the replacement without exec'ing the test binary over itself:
	// the handoff is written before it, and a returned error keeps the old
	// image and its children and deletes the handoff.
	ExecInPlace func(bin string, argv []string, env []string) error

	// PlaneFor builds the write client the supervisor uses to hand a dead
	// child's claim back (CLA-708). Nil = plane.New. The hook exists because the
	// endpoint the supervisor resolves is the CHILD's production endpoint, which
	// a test's fake plane is not; a test supplies a client pointed at its own
	// server so the wire shape is exercised for real.
	PlaneFor func(endpoint, apiKey string) plane.Releaser

	// CloseOutVerifier builds the local verifier a close-out judges one repo with.
	// Nil = delivery.New(repo, "") with production defaults. The hook exists
	// because the PR judgement polls within a 30-second window and shells out to
	// `gh`: a test cannot wait that window out, and must substitute its own gh.
	CloseOutVerifier func(repo string) *delivery.Verifier

	// ConflictRun launches the conflict-only session (CLA-759) for a conflicted
	// approved task: the session that merges the PR's base into its branch,
	// resolves only the conflicting hunks, runs the repo-wide gate and pushes.
	// Nil = defaultConflictRun, which composes the real harness Invocation from
	// the instance config. The hook exists so a test can drive the whole
	// close-out pass without spending a paid session, and assert exactly what the
	// fleet would have started.
	ConflictRun func(ctx context.Context, cfg *config.Config, slug string, req ConflictRunRequest, step config.ConflictStep) (harness.Result, error)

	// AfterConfigRewrite is called after a reconcile-time rewrite of a child's
	// generated config and before the RESTART that applies it is queued
	// (CLA-724). It is the deterministic seam for the rewrite-to-marker window,
	// which is otherwise a few filesystem syscalls wide: a test can plant a
	// STOP or HALT here and assert the stop wins, or make the marker write fail
	// and assert the retry queues it on the next poll. Nil in production.
	AfterConfigRewrite func(instName string)
}

// childVersionEnv is the environment variable the supervisor sets on every
// child spawn carrying the version it recorded for that child (phase 5b). The
// real daemon ignores it — its beacon reports its OWN build, which is the
// ground truth the roll verifies — and the fake daemon in tests reports it as
// its own version, so a test can simulate a binary swap by changing the
// VersionOf hook between spawns.
const childVersionEnv = "CLANKERBAR_CHILD_VERSION"

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
	if o.ConflictRun == nil {
		o.ConflictRun = defaultConflictRun
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
	// stopRequested is set once a readable STOP marker is in place for a
	// stopped instance's live child, so repeated polls write nothing
	// (idempotent reconcile) and a failed write is retried instead. Cleared on
	// the next spawn.
	stopRequested bool
	// policyRefused is set when the last config build hit the permission-
	// policy gate: the spawn is refused (even on a last-known-good config
	// whose own policy file still exists), and the next poll retries the
	// build — a restored policy file brings the daemon back without a
	// supervisor restart (phase 2c).
	policyRefused bool
	// configRestartPending is set when a reconcile rewrote this child's
	// generated config but could not queue the RESTART that applies it (a
	// failed marker write). The byte comparison alone cannot see that
	// outstanding work — the file already holds the new bytes — so the retry
	// lives here and every reconcile re-attempts the marker while it is set.
	// Cleared when a RESTART/RESTART_NOW is confirmed pending, and on the next
	// spawn, which materializes the current config and therefore satisfies it
	// (CLA-724 review findings 1.review-3.1 / 1.review-4.1).
	configRestartPending bool

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
	// limitStopped: the child ended on a Stop-class usage limit and left a
	// LIMIT_STOP marker; limitReason is its first line. Not respawned until the
	// operator deletes the marker (docs/limit-stop.md).
	limitStopped bool
	limitReason  string
	// exitAt is when the last child's Wait goroutine saw it end (CLA-677). It
	// is what the status report's `lastExit.at` and `since` carry: the
	// supervisor stored the exit ERROR for its own logging, but a time has to be
	// captured at the exit itself, because the status row needs to say WHEN.
	exitAt time.Time

	// adoptSettled is closed by an adopted child's reaper once its first
	// non-blocking wait4 has run: the adopted roll waits on it so it never
	// writes a RESTART into a generation whose exit is already pending. Nil for
	// a spawned child, whose Wait goroutine the roll does not need to consult.
	adoptSettled chan struct{}
	// exitPending is set by an adopted child's reaper when wait4 has returned,
	// before the exit event is posted, so the adopted roll can skip a child
	// that is already gone even though onExit has not consumed its event yet.
	exitPending bool
}

// exitEvent is posted by a child's Wait goroutine when its process ends.
type exitEvent struct {
	inst *Instance
	err  error
	at   time.Time // when the Wait goroutine saw the process end
}

// Supervisor holds the fleet state. Everything except the per-child Wait
// goroutines and the close-out pass (CLA-758) runs on the single Supervise
// goroutine, so no locks guard the scheduling fields — the mutex on Instance
// exists only for the hand-off between a Wait goroutine (exited/exitErr) and the
// main goroutine, and for the config pointer the pass snapshots before it leaves
// that goroutine.
type Supervisor struct {
	o Options

	// hostname is the machine identity the fleet names resolve against,
	// resolved once at startup.
	hostname string

	// startedAt is when this supervisor process began — the staleness gate
	// for the REPLACE marker (phase 5c): a marker written before this process
	// started is not a request THIS supervisor owes and is discarded.
	startedAt time.Time

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

	// status is the plane-facing status reporter (CLA-677). It is fail-soft by
	// construction: a failed POST is logged once per outage and dropped, and
	// never changes what the supervisor does.
	status StatusReporter

	// stopping is set at the top of stopAll: from then on an instance that is
	// down is reported "stopped" (the supervisor is taking it down), not
	// "exited" (which would imply a crash left it down).
	stopping bool

	// replacing is set only by the no-exec(2) drain path (replace_drain.go,
	// behind the build tag): a child the supervisor itself drained for a
	// REPLACE is reported "stopped" (a planned upgrade), not "exited" (a
	// crash). It must NOT be d.stopping: a timed-out drain resumes the loop
	// and respawns the fleet, so the flag cannot mean "this supervisor is
	// exiting". On platforms with exec(2) the fleet never drains for a
	// replacement, so this stays false.
	replacing bool

	// closeOutSaid remembers the last close-out line logged per key, so an
	// unchanged stand-down is said once rather than on every poll (CLA-758). It
	// is written only by the close-out pass goroutine.
	closeOutSaid map[string]string
	// closeOutConflicts remembers the head@base pair already reported as
	// conflicting per task, so the conflicting-path evidence is logged once per
	// pair rather than on every reconcile — and the paths it computed, so a
	// conflict session started later in the same pair does not have to recompute
	// (or lose) them. Also pass-goroutine only.
	closeOutConflicts map[string]conflictEvidence
	// closeOutSeenTasks is the approved task set the PREVIOUS pass read, so the
	// end-of-pass sweep gives a task one pass of grace: a conflict session's claim
	// takes its task out of the approved queue while its retry budget must
	// survive. Pass-goroutine only, like the maps the sweep prunes.
	closeOutSeenTasks map[string]bool
	// closeOutBusy is set while a close-out pass runs on its own goroutine, so the
	// next poll skips instead of starting a second pass over the same queue.
	closeOutBusy atomic.Bool
	// closeOutWG tracks that pass, so a fleet stop can wait (bounded) for it to
	// hand back a claim it had already taken.
	closeOutWG sync.WaitGroup

	// adoptWG tracks the reapers and the roll goroutine started by adoption
	// across an exec(2) replacement, so a fleet stop can wait for them to
	// finish after stopAll has drained every child.
	adoptWG sync.WaitGroup

	// conflictBusy is set while a conflict-only session runs (CLA-759): at most
	// one at a time on this supervisor, so a second conflicting task waits for the
	// next poll rather than spending a second paid session. It is only the FAST
	// in-process gate: the bound that holds across supervisors is the shared slot
	// in conflictlock.go, taken before the claim.
	conflictBusy atomic.Bool
	// conflictWG tracks the session goroutine, so a fleet stop can wait
	// (bounded) for it to release a claim it holds.
	conflictWG sync.WaitGroup
	// conflictSaid remembers the last conflict stand-down line per key, the
	// close-out's say-it-once discipline. Guarded by conflictMu because the pass
	// goroutine and the session goroutine both write it.
	conflictMu   sync.Mutex
	conflictSaid map[string]string
	// conflictActive is the task a conflict session currently works, empty when
	// none. Under conflictMu: the sweep keeps that task's state even though its
	// claim takes it out of the approved queue.
	conflictActive string
	// conflictTried bounds the paid retries per task (CLA-759): a failed session
	// leaves the task approved, so without it the next poll would start another
	// session forever instead of falling back to the agent close-out. Also under
	// conflictMu.
	conflictTried map[string]conflictAttempt
}

// Supervise runs the fleet until ctx is cancelled. Cancellation IS the
// fleet-wide stop: every child gets a STOP marker in its own state dir, each
// drains at its next phase seam or iteration boundary (possibly taking a while — an in-flight
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
		startedAt:    time.Now(),
		roster:       NewRosterClient(o.RosterURL, o.APIKey),
		loggedRemote: map[string]bool{},
		status:       newStatusReporter(statusEndpoint(o.RosterURL), o.APIKey),
	}
	defer func() { d.status.Close(d.statusReport()) }()

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
		dir, err := CacheDir()
		if err != nil {
			return fmt.Errorf("supervise: %w", err)
		}
		d.cacheDir = dir
	}
	// The supervisor's own lines get the same durable home the daemons' have
	// (CLA-701): a supervisor is the process an operator asks "why did that
	// child not come back?" about, and its answer was only ever in a terminal.
	// Best-effort by the same contract — observability never gates the fleet.
	if o.LogToFile {
		if restore, err := installLog(d.cacheDir); err != nil {
			log.Printf("supervise: cannot write its log into %s (%v) - its own lines go to stderr only", d.cacheDir, err)
		} else {
			defer restore()
		}
	}

	// The exit channel must exist BEFORE the first reconcile spawns children:
	// a child that crashes between its spawn and the channel's creation would
	// block forever on a nil-channel send, and its exit event would never
	// reach the loop — no respawn, and stopAll would wait on a child that was
	// already gone. The loop consumes constantly, so a modest fixed buffer is
	// all a burst of simultaneous exits needs.
	d.exits = make(chan exitEvent, 16)

	// A re-exec'd supervisor starts here: the previous image wrote the handoff
	// before exec'ing this one, and its children are still our children. Adopt
	// them BEFORE the first reconcile, so the reconcile sees them alive and
	// spawns no duplicate; the returned instances are rolled onto this binary
	// after the first roster refresh.
	adopted := d.adoptReplaceHandoff()

	if err := d.reconcile(); err != nil {
		return err
	}
	if d.entries == nil && len(d.instances) == 0 {
		// The plane was unreachable at start and no cached roster exists;
		// reconcile already said so. Nothing to supervise. Adopted children
		// keep the loop alive below, because their processes are still ours.
		return nil
	}
	if len(d.instances) == 0 {
		log.Print("roster holds no local instances - nothing to supervise")
		return nil
	}
	d.startAdoptedRoll(adopted)

	pollTicker := time.NewTicker(d.o.PollInterval)
	defer pollTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			err := d.stopAll()
			// The close-out pass runs off this loop; its context is cancelled
			// already, and this bounded wait is what lets it hand back a claim it
			// had taken before the process exits.
			d.waitCloseOut()
			// A conflict-only session is likewise cancelled already; the wait is
			// what lets it release the claim it holds rather than leaving it to
			// expire.
			d.waitConflict()
			// Adopted reapers and the adopted-child roll stop with the context;
			// stopAll has already drained every child, so their Wait4 calls are
			// returning and this wait only keeps the process alive long enough
			// for them to finish cleanly.
			d.adoptWG.Wait()
			return err
		case ev := <-d.exits:
			d.onExit(ev.inst, ev.err, ev.at)
		case <-d.timerCh:
			d.respawnDue()
			d.armRestart()
		case <-pollTicker.C:
			d.reconcile()
		}
	}
}
