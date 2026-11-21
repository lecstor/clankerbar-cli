// Package planner runs the CLI's planner role (studio-chat.md §3/§4): it watches
// a project's conversations over a cheap process-level poll, and for each pending
// turn spawns ONE harness session that answers it.
//
// The split that makes idle cost zero is the whole design: the 2-second poll is
// plain HTTP and never reaches a model, so a planner between turns runs no
// process and spends nothing. Only when the poll names a claimable conversation
// does the loop claim it, resolve the project's stored `roles.planner`, and
// spawn a session. The session resumes its predecessor when the plane says this
// instance owns the conversation's session, and is re-primed from the plane's
// message history when it does not — or when the harness reports the session
// gone, or when the session's token budget is spent.
//
// Nothing here decides a verdict: the session answers the operator through
// `reply_conversation` itself, and the loop only observes the process end. A
// turn whose session never replies leaves the messages pending and its claim to
// expire; the next poll offers the turn again.
package planner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
	"github.com/lecstor/clankerbar-cli/internal/version"
)

// DefaultPollInterval is the wake cadence studio-chat.md §4 pins: 2 seconds,
// the same as the run inbox's poll, and cheap enough that an operator message
// feels answered promptly without any process running between turns.
const DefaultPollInterval = 2 * time.Second

// DefaultConfigTTL is how long a project's resolved run config is reused before
// the next turn re-fetches it. Long enough not to fetch per turn in a burst of
// messages, short enough that a ratified change lands within a minute.
const DefaultConfigTTL = 60 * time.Second

// DefaultIdleBeacon is how often a planner between turns refreshes its presence
// row. The plane treats presence older than 15 minutes as stale, so a minute is
// ample; beaconing every 2-second poll would be thirty POSTs a minute for a
// daemon that is doing nothing.
const DefaultIdleBeacon = time.Minute

// Target is one project a planner instance serves: its wake poll, its
// conversation surface, its stored run config, its presence reporter and the
// wiring its sessions start with.
type Target struct {
	// Name is the project slug, and the instance name's project half for logs.
	Name string
	// Poller is the 2-second pending poll. Nil is treated as not wired.
	Poller backlog.ConversationPoller
	// Conversations claims and reads turns. Nil is treated as not wired.
	Conversations plane.ConversationAPI
	// RCfg reads the stored execution config, where `roles.planner` lives.
	RCfg plane.RunConfigReader
	// Fleet reports presence for this project. Nil is a telemetry no-op.
	Fleet fleet.Reporter

	// WorkDir, MCPConfigPath and MCPConfigPaths are this project's session
	// wiring, exactly as a drain target carries them. Empty falls back to the
	// base config.
	WorkDir        string
	MCPConfigPath  string
	MCPConfigPaths map[string]string
}

// Config is everything a planner needs. The base config supplies the machine
// wiring (env, config dirs, paths, session bounds) and the instance identity;
// each target's own stored document supplies the role.
type Config struct {
	Base    *config.Config
	Targets []Target

	// StateDir is where the session store lives. Empty means the config's
	// resolved state dir, opened by Run.
	StateDir string
	// Instance is the beaconed instance name. Empty resolves exactly as a drain
	// beacon does (hostname + config basename).
	Instance string

	// AdapterFor resolves a harness by name. Nil is harness.Get; tests inject a
	// fake.
	AdapterFor func(name string) (harness.Adapter, error)
	// Console is where sessions write their progress. Nil is os.Stderr.
	Console io.Writer
	// PollInterval and ConfigTTL default to the constants above.
	PollInterval time.Duration
	ConfigTTL    time.Duration
	// PresenceRefresh is how often presence is refreshed WHILE a turn runs, so a
	// planner answering a long turn never reads as offline. Defaults to
	// DefaultIdleBeacon, far inside the plane's 15-minute stale window; tests
	// shorten it.
	PresenceRefresh time.Duration
	// Now is the clock, for tests. Nil is time.Now.
	Now func() time.Time
	// NewSessionRef mints a fresh session handle. Nil is newSessionRef; tests
	// inject a deterministic sequence.
	NewSessionRef func() string
}

// Planner is one instance's planner loop. It is single-threaded by design: one
// role per instance, no in-process concurrency (operator decision 96ac5e8d), so
// a turn always finishes before the next poll acts on the next conversation.
type Planner struct {
	base     *config.Config
	targets  []Target
	instance string
	host     string

	adapterFor   func(string) (harness.Adapter, error)
	console      io.Writer
	poll         time.Duration
	configTTL    time.Duration
	presence     time.Duration
	now          func() time.Time
	stateDirPath string
	newRef       func() string

	// state is the open state dir, where the control markers live. Nil until
	// Run opens it (or a test installs one).
	state *statedir.Dir
	// restartRequested is set by a RESTART marker; cli reads it after Run.
	restartRequested bool
	// reloadConfig rebuilds and validates the local config for a RELOAD marker.
	reloadConfig func() (*config.Config, error)

	sessions *SessionStore

	mu       sync.Mutex
	resolved []*projectRole
	turns    int
	// lastBeacon throttles idle presence, per target.
	lastBeacon []time.Time
	// log throttles repeated poll/harness errors, per target.
	pollFailures []int
	turnFailures []int
}

// projectRole is one target's cached effective config and resolved planner
// session. It is refreshed just before a turn, honouring ConfigTTL.
type projectRole struct {
	config  *config.Config
	session config.PlannerSession
	fetched time.Time
	// usable is false when the project has no planner or the role cannot be
	// served here. reason says why, once, in the log.
	usable bool
	reason string
}

// New builds a Planner. It does not open the state dir or contact the plane;
// Run does both, so a construction error stays a config error.
func New(cfg Config) (*Planner, error) {
	if cfg.Base == nil {
		return nil, errors.New("planner: no base config")
	}
	if len(cfg.Targets) == 0 {
		return nil, errors.New("planner: no targets")
	}
	p := &Planner{
		base:         cfg.Base,
		targets:      cfg.Targets,
		instance:     strings.TrimSpace(cfg.Instance),
		adapterFor:   cfg.AdapterFor,
		console:      cfg.Console,
		poll:         firstDuration(cfg.PollInterval, DefaultPollInterval),
		configTTL:    firstDuration(cfg.ConfigTTL, DefaultConfigTTL),
		presence:     firstDuration(cfg.PresenceRefresh, DefaultIdleBeacon),
		now:          cfg.Now,
		stateDirPath: strings.TrimSpace(cfg.StateDir),
		newRef:       cfg.NewSessionRef,
	}
	if p.adapterFor == nil {
		p.adapterFor = harness.Get
	}
	if p.console == nil {
		p.console = os.Stderr
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.newRef == nil {
		p.newRef = newSessionRef
	}
	p.resolved = make([]*projectRole, len(cfg.Targets))
	p.lastBeacon = make([]time.Time, len(cfg.Targets))
	p.pollFailures = make([]int, len(cfg.Targets))
	p.turnFailures = make([]int, len(cfg.Targets))
	if host, err := os.Hostname(); err == nil {
		p.host = host
	}
	if p.instance == "" {
		p.instance = cfg.Base.ResolvedInstanceName(p.host)
	}
	return p, nil
}

func firstDuration(v, d time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return d
}

// Run is the planner loop: one poll tick at a time, one turn at a time.
//
// A permanent wiring failure (a rejected key, a project-key mismatch) stops the
// run exactly as it stops a worker drain: the sessions would fail the same way,
// so idling would only hide it. A transient poll or turn failure is logged once
// per streak and retried on the next tick — the conversation stays pending
// plane-side, and the claim, if one was taken, expires on its own.
func (p *Planner) Run(ctx context.Context) error {
	closeState, err := p.openState(p.base)
	if err != nil {
		return err
	}
	defer closeState()

	// A STOP left over from a previous run predates this process; consuming it
	// with a note (rather than stopping) mirrors the drain loop, which learned
	// that a ghost marker otherwise kills the replacement daemon at startup.
	p.consumeStaleStop()

	p.beaconAll(fleet.State{Kind: fleet.StateIdle})
	defer p.closeBeacons()

	ticker := time.NewTicker(p.poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			// The control markers are honoured at the turn boundary, exactly as
			// the drain honours them at its iteration boundary.
			if p.checkControl() {
				return nil
			}
			if err := p.lap(ctx); err != nil {
				return err
			}
		}
	}
}

// RestartRequested reports whether a RESTART marker ended the run. The caller
// (cli.runPlanner) turns this into a re-exec, exactly as cli.runDriver does.
func (p *Planner) RestartRequested() bool { return p.restartRequested }

// SetReloader wires the closure a RELOAD marker uses to rebuild the config from
// disk. It must return a VALIDATED config, because applyReload swaps it in
// without re-validating.
func (p *Planner) SetReloader(load func() (*config.Config, error)) {
	p.reloadConfig = load
}

// consumeStaleStop removes a STOP marker that was already present at startup.
func (p *Planner) consumeStaleStop() {
	if p.state == nil {
		return
	}
	if present, msg := p.readMarker("STOP"); present {
		_ = p.state.Remove("STOP")
		note := ""
		if msg != "" {
			note = ": " + msg
		}
		log.Printf("planner: warning: STOP marker%s is left over from a previous run - consumed WITHOUT stopping this one; drop a fresh marker to stop THIS run", note)
	}
}

// lap polls every target once and runs at most one turn per target that has one.
// Sequential on purpose: one conversation at a time is the v1 rule, and it keeps
// the harness session count at one per instance.
func (p *Planner) lap(ctx context.Context) error {
	for i := range p.targets {
		if ctx.Err() != nil {
			return nil
		}
		t := p.targets[i]
		if t.Poller == nil {
			continue
		}
		pending, err := t.Poller.PollPending(ctx)
		if err != nil {
			if stop := p.notePollError(i, t, err); stop != nil {
				return stop
			}
			continue
		}
		p.pollFailures[i] = 0
		if pending == nil {
			p.beaconIdle(i, t)
			continue
		}
		if err := p.turn(ctx, i, t, pending); err != nil {
			// A turn failure is not fatal: the claim's lease expires and the next
			// poll re-offers the turn. Say it once per streak.
			if p.turnFailures[i] == 0 {
				log.Printf("%splanner: turn on conversation %s failed: %v - the conversation stays pending and will be offered again", labelOf(t), pending.ID, err)
			}
			p.turnFailures[i]++
			p.beaconIdle(i, t)
			continue
		}
		p.turnFailures[i] = 0
		p.beaconIdle(i, t)
	}
	return nil
}

// notePollError classifies one poll failure: a nil return means "retry next tick"
// (transient, or a rate limit we slow down for), and a non-nil return ends the
// run for a wiring failure a worker drain would stop on too.
func (p *Planner) notePollError(i int, t Target, err error) error {
	switch {
	case errors.Is(err, backlog.ErrNotWired):
		// No endpoint or key. The wiring check at startup already says so; there
		// is nothing to poll, and a planner with no poll cannot serve chat.
		return nil
	case errors.Is(err, backlog.ErrUnauthorized):
		return fmt.Errorf("%splanner: %w - the same key the sessions would use, so no turn can succeed", labelOf(t), err)
	case errors.Is(err, backlog.ErrProjectRequired):
		return fmt.Errorf("%splanner: %w", labelOf(t), err)
	case errors.Is(err, backlog.ErrConversationPollRateLimited):
		// Expected under a shared key; the next tick is the retry.
		if p.pollFailures[i] == 0 {
			log.Printf("%splanner: pending poll rate limited - backing off to the poll cadence", labelOf(t))
		}
	default:
		if p.pollFailures[i] == 0 {
			log.Printf("%splanner: pending poll failed: %v - retrying every %s", labelOf(t), err, p.poll)
		}
	}
	p.pollFailures[i]++
	return nil
}

// openState opens (or adopts) the state dir and loads the session store.
func (p *Planner) openState(base *config.Config) (func(), error) {
	if p.sessions != nil {
		return func() {}, nil
	}
	dir, err := p.stateDir(base)
	if err != nil {
		return nil, err
	}
	store, closeStore, err := OpenSessionStore(dir)
	if err != nil {
		return nil, err
	}
	p.sessions = store
	p.state = dir
	return func() { _ = closeStore() }, nil
}

// resolve refreshes the target's effective config and planner session when the
// cache is cold. A project whose document has no planner, or whose role cannot
// be served here, is reported once and then served as "no chat" until the TTL
// lapses.
func (p *Planner) resolve(ctx context.Context, i int, t Target) *projectRole {
	cur := p.resolved[i]
	if cur != nil && p.now().Sub(cur.fetched) < p.configTTL {
		return cur
	}
	eff := p.base.Clone()
	role := &projectRole{config: eff, fetched: p.now()}
	if t.RCfg == nil {
		role.reason = "no run-config reader"
	} else {
		st, err := t.RCfg.RunConfig(ctx)
		switch {
		case errors.Is(err, plane.ErrNoConfig):
			role.reason = "the project has no stored run config"
		case errors.Is(err, plane.ErrNotWired):
			role.reason = "run-config reads are not wired"
		case err != nil:
			role.reason = fmt.Sprintf("the stored run config could not be read (%v)", err)
			// Cache the FAILURE like any other answer: a pending conversation
			// makes every 2-second poll reach resolve, so a backdated fetch
			// would turn a failing plane into an MCP request every tick. The
			// TTL is the retry cadence; the transition is logged once below.
		default:
			role.reason = ""
			var doc config.RunConfigDoc
			if err := json.Unmarshal(st.Config, &doc); err != nil {
				role.reason = fmt.Sprintf("the stored run config could not be decoded (%v)", err)
				break
			}
			if doc.SchemaNewer() {
				role.reason = fmt.Sprintf("the stored run config uses schema v%d, newer than this build understands (v%d)", doc.SchemaVersion, config.RunConfigSchemaVersion)
				break
			}
			if err := eff.ApplyRunConfig(&doc); err != nil {
				role.reason = fmt.Sprintf("the stored run config was refused (%v)", err)
			}
		}
	}
	if role.reason == "" {
		if !eff.PlannerEnabled() {
			role.reason = "the resolved run config has no roles.planner"
		} else if sess, err := eff.ResolvePlanner(); err != nil {
			role.reason = fmt.Sprintf("roles.planner cannot be served: %v", err)
		} else if _, err := p.adapterFor(sess.Harness); err != nil {
			role.reason = fmt.Sprintf("roles.planner names harness %q, which this build cannot run: %v", sess.Harness, err)
		} else {
			role.session = sess
			role.usable = true
		}
	}
	if !role.usable {
		if prev := p.resolved[i]; prev == nil || prev.usable || prev.reason != role.reason {
			log.Printf("%splanner: chat off - %s", labelOf(t), role.reason)
		}
	}
	p.resolved[i] = role
	return role
}

// startPresence refreshes presence on a timer for as long as a turn runs. A
// turn can occupy the whole wall-clock bound, which is the plane's 15-minute
// stale window, and the console filters stale rows: without this, a planner
// that is answering reads as offline. It refreshes EVERY target, because the
// loop serves one turn at a time and the others would otherwise go stale behind
// a long turn on one project. The returned func stops the timer and waits for
// it, so the turn's final idle beacon cannot be overtaken by a late iteration.
func (p *Planner) startPresence(i, n int) func() {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(p.presence)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				for j := range p.targets {
					st := fleet.State{Kind: fleet.StateIdle}
					if j == i {
						st = fleet.State{Kind: fleet.StateIteration, N: n, Phase: "planner"}
					}
					p.sendBeacon(j, p.targets[j], st)
				}
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
	}
}

// beaconIdle refreshes presence at most once per DefaultIdleBeacon.
func (p *Planner) beaconIdle(i int, t Target) {
	if p.now().Sub(p.lastBeacon[i]) < DefaultIdleBeacon {
		return
	}
	p.beaconIdleNow(i, t)
}

// beaconIdleNow reports idle immediately, regardless of the throttle: the end of
// a turn must move the row off `iteration` at once, or the console shows a
// session that has already finished.
func (p *Planner) beaconIdleNow(i int, t Target) {
	p.lastBeacon[i] = p.now()
	p.sendBeacon(i, t, fleet.State{Kind: fleet.StateIdle})
}

func (p *Planner) beaconAll(st fleet.State) {
	for i := range p.targets {
		p.lastBeacon[i] = p.now()
		p.sendBeacon(i, p.targets[i], st)
	}
}

func (p *Planner) sendBeacon(i int, t Target, st fleet.State) {
	if t.Fleet == nil {
		return
	}
	t.Fleet.Send(fleet.Report{Identity: p.identity(i), State: st})
}

// identity is the same shape a drain beacon carries, with the role the local
// config declares and the target's effective config fingerprint — per target,
// because each project's stored document can differ.
func (p *Planner) identity(i int) fleet.Identity {
	eff := p.base
	if i >= 0 && i < len(p.resolved) && p.resolved[i] != nil {
		eff = p.resolved[i].config
	}
	id := ""
	if eff != nil {
		id = eff.Identity()
	}
	return fleet.Identity{
		Instance:       p.instance,
		Host:           p.host,
		Version:        version.Current,
		ConfigIdentity: id,
		Role:           config.RolePlanner,
	}
}

// closeBeacons sends each target's final `stopping` beacon, concurrently under
// ONE deadline for the whole set, so the console's silence after it means the
// instance is gone rather than stopped talking. The shared deadline matters for
// the same reason as the drain loop's: a hanging plane must not cost the process
// one report timeout per project.
func (p *Planner) closeBeacons() {
	ctx, cancel := context.WithTimeout(context.Background(), fleet.CloseBudget)
	defer cancel()
	beacons := make([]fleet.ShutdownBeacon, 0, len(p.targets))
	for i := range p.targets {
		if p.targets[i].Fleet == nil {
			continue
		}
		beacons = append(beacons, fleet.ShutdownBeacon{
			Reporter: p.targets[i].Fleet,
			Report: fleet.Report{
				Identity: p.identity(i),
				State:    fleet.State{Kind: fleet.StateStopping},
			},
		})
	}
	fleet.Shutdown(ctx, beacons)
}

// labelOf is the log prefix a target's lines carry, empty for the unnamed
// single-project target so it reads exactly as before.
func labelOf(t Target) string {
	if t.Name == "" {
		return ""
	}
	return t.Name + ": "
}
