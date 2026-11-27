// fleet.go — the driver's half of activity reporting: presence beacons riding the backlog-summary
// poll, state-change beacons at phase boundaries, and one iteration record per drain at the
// boundary the loop just crossed.

package loop

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
	"github.com/lecstor/clankerbar-cli/internal/version"
)

// iterState is one target's mid-drain presence: what `iteration {n, taskRef, phase}` the Fleet page
// should show while this drain runs, and the route its session is actually on (CLA-1026).
type iterState struct {
	on    bool
	n     int
	ref   string
	phase string
	model string
}

// hostnameOnce caches os.Hostname for the life of the driver.
func (d *Driver) hostnameOnce() string {
	d.hostOnce.Do(func() {
		h, err := os.Hostname()
		if err != nil {
			log.Printf("fleet: could not read hostname: %v", err)
			return
		}
		d.hostname = h
	})
	return d.hostname
}

// fleetIdentity composes what every beacon for target ti carries: who it is, where it runs, what
// build, which config. The fingerprint is the target's EFFECTIVE config — the CLA-410 overlay
// when one is in force, else the base file, the same rule the drain reads through d.targetCfg —
// because that is what the drain runs on.
func (d *Driver) fleetIdentity(ti int) fleet.Identity {
	// A target INDEX, not a captured Target: the poll beacons a target before checkRunConfigVersion
	// rebuilds its overlay, so a copy would fingerprint the document being replaced.
	d.controlMu.Lock()
	base := d.cfg
	eff := base
	if ti >= 0 && ti < len(d.targets) && d.targets[ti].Cfg != nil {
		eff = d.targets[ti].Cfg
	}
	// Explicit instance_name wins; otherwise hostname plus config basename, unique per running daemon,
	// so co-located daemons no longer collapse into one presence row.
	inst := config.ResolveInstanceName("", "", d.hostnameOnce())
	if base != nil {
		inst = base.ResolvedInstanceName(d.hostnameOnce())
	}
	d.controlMu.Unlock()
	cfgID := ""
	role := config.RoleWorker
	if eff != nil {
		cfgID = eff.Identity()
		role = eff.EffectiveRole()
	}
	return fleet.Identity{
		Instance:       inst,
		Host:           d.hostnameOnce(),
		Version:        version.Current,
		ConfigIdentity: cfgID,
		Role:           role,
	}
}

// holdState is a phase boundary the schedule is holding for one target: the task whose claim is
// kept and the phase that will run when the window ends.
type holdState struct {
	on    bool
	ref   string
	phase string
	// until/zone are the held window's own end and IANA zone. The daemon-wide PauseWindows is re-read
	// from the config in scheduleState; a ROUTE window has no daemon-wide schedule to re-read, so the
	// hold carries its instant instead of the presence row falling back to plain draining.
	until time.Time
	zone  string
	// routes is the per-route hold breakdown a ROUTE-window hold carries (CLA-1026): every configured
	// route the window list withholds, and why. It is empty for the daemon-wide pause, which has no
	// per-route cause to name.
	routes []fleet.RouteHold
}

// fleetState derives the presence State for target ti from what the loop already tracks — the
// mid-drain marker set in drainPhases, the hold marker set in holdThroughPause, and the waiting
// marker set when a drain ends by asking:
//   - held at a phase boundary by a pause window -> draining with the schedule reason, the window's
//     end, and the held task's ref and phase
//   - schedule-paused -> draining naming the window and its end
//   - an own blocking question unanswered -> waiting with the newest such ask's task ref
//   - otherwise -> idle
//
// A backed-off target (skipUntil still in the future) contributes no presence state of its own: it
// reports idle, or waiting while its own question stands. The back-off is minutes-long and
// self-clearing, and flapping the state to draining for every quiet spell would blur the one signal
// draining exists for — "something is holding spawns closed until an operator acts".
func (d *Driver) fleetState(ti int) fleet.State {
	d.iterMu.Lock()
	s := d.iterAt(ti)
	h := d.holdAt(ti)
	w := d.waitingAt(ti)
	d.iterMu.Unlock()
	// A running session outranks every pause flag: the flags only ever hold the NEXT spawn, and the
	// iteration is what the Fleet page shows while work is in flight.
	if s.on && !h.on {
		return fleet.State{Kind: fleet.StateIteration, N: s.n, TaskRef: s.ref, Phase: s.phase, Model: s.model}
	}
	// A pause the operator has to act on wins over the schedule: report one cause, and it is the one
	// waiting on a human.
	if reason := d.operatorPauseReason(ti); reason != "" {
		return fleet.State{Kind: fleet.StateDraining, PauseReason: reason}
	}
	if d.schedPaused {
		return d.scheduleState(h)
	}
	// A blocking question of this target's own is the one quiet state that needs the operator, so
	// it says so rather than reading as a daemon with nothing to do. It ranks below a pause the
	// operator set deliberately: draining names the reason spawns are closed, waiting only what the
	// poll is waiting on.
	if w.on {
		return fleet.State{Kind: fleet.StateWaiting, TaskRef: w.ref()}
	}
	return fleet.State{Kind: fleet.StateIdle}
}

// operatorPauseReason names the strongest non-schedule cause holding target ti's spawns closed, or
// "" when none does. Console outranks fleet outranks startup outranks budget: that is the order an
// operator works through, from the lever on the page they are looking at to the one that needs a
// decision or a config edit.
func (d *Driver) operatorPauseReason(ti int) string {
	switch {
	case d.pausedAt(ti):
		return fleet.PauseReasonConsole
	case d.fleetPausedAt(ti):
		return fleet.PauseReasonFleet
	case d.startupPausedAt(ti):
		return fleet.PauseReasonStartup
	case d.exhaustedAt(ti):
		return fleet.PauseReasonBudget
	}
	return ""
}

// scheduleState is the scheduled pause's presence: draining, why, until when, and — when a phase
// sequence is held — what is waiting behind it. A stale flag reports plain draining, never a
// reason with no resume instant.
func (d *Driver) scheduleState(h holdState) fleet.State {
	st := fleet.State{Kind: fleet.StateDraining}
	active, until, pw := d.pauseState()
	switch {
	case active:
		st.PauseReason = fleet.PauseReasonSchedule
		st.PauseUntil = until
		if pw != nil {
			st.PauseZone = pw.Timezone
		}
	case h.on && !h.until.IsZero():
		// CLA-947: a per-route pricing window is holding the spawn. It is the same reason word — a
		// schedule that clears itself — and the hold's own instant is the truth because there is no
		// daemon-wide window to re-read.
		st.PauseReason = fleet.PauseReasonSchedule
		st.PauseUntil = h.until
		st.PauseZone = h.zone
		// CLA-1026: name every route the window list withholds, so the console can show both the
		// usage-limited routes and the ones with quota that the pricing window is holding.
		st.Routes = h.routes
	default:
		return st
	}
	if h.on {
		st.TaskRef, st.Phase = h.ref, h.phase
	}
	return st
}

// iterAt/pausedAt/fleetPausedAt/startupPausedAt are bounds-checked reads of the per-target slices.
// A Driver built by hand in a test carries empty slices; reporting must degrade to idle for it, not
// panic.
func (d *Driver) iterAt(ti int) iterState {
	if ti < 0 || ti >= len(d.iter) {
		return iterState{}
	}
	return d.iter[ti]
}

// holdAt is the bounds-checked read of the per-target hold marker, mirroring iterAt: a hand-built
// test Driver carries an empty slice and reports no hold.
func (d *Driver) holdAt(ti int) holdState {
	if ti < 0 || ti >= len(d.hold) {
		return holdState{}
	}
	return d.hold[ti]
}

// holdPause and clearHoldPause mark a target as held at a phase boundary by the schedule and drop
// the mark when it ends.
func (d *Driver) holdPause(ti int, ref, phase string, until time.Time, zone string, routes []fleet.RouteHold) {
	d.iterMu.Lock()
	defer d.iterMu.Unlock()
	if ti < 0 || ti >= len(d.hold) {
		return
	}
	d.hold[ti] = holdState{on: true, ref: ref, phase: phase, until: until, zone: zone, routes: routes}
}

func (d *Driver) clearHoldPause(ti int) {
	d.iterMu.Lock()
	defer d.iterMu.Unlock()
	if ti < 0 || ti >= len(d.hold) {
		return
	}
	d.hold[ti] = holdState{}
}

func (d *Driver) pausedAt(ti int) bool {
	return ti >= 0 && ti < len(d.paused) && d.paused[ti]
}

func (d *Driver) fleetPausedAt(ti int) bool {
	return ti >= 0 && ti < len(d.fleetPaused) && d.fleetPaused[ti]
}

// startupPausedAt is the startup-trip half of the draining signal: a target paused awaiting the
// operator's answer to the harness-startup question is alive and spawning nothing new, exactly like
// a fleet-paused one, and must not read idle on the console.
func (d *Driver) startupPausedAt(ti int) bool {
	return ti >= 0 && ti < len(d.startupPaused) && d.startupPaused[ti]
}

// beacon sends one presence report for target ti. Nil-safe on the reporter so tests and not-wired
// targets need no branching at the call sites.
func (d *Driver) beacon(ti int, t Target, st fleet.State, iterations ...fleet.Iteration) {
	d.writeLocalBeacon(st)
	d.sendBeacon(ti, t, st, iterations...)
}

// sendBeacon is beacon without the local file: the plane half alone. Split out for the callers that
// report one daemon-wide state to several targets, so the local BEACON file (which takes no target)
// is written ONCE per lap instead of once per target.
func (d *Driver) sendBeacon(ti int, t Target, st fleet.State, iterations ...fleet.Iteration) {
	if t.Fleet == nil {
		return
	}
	t.Fleet.Send(fleet.Report{
		Identity:   d.fleetIdentity(ti),
		State:      st,
		Iterations: iterations,
	})
}

// beaconWait publishes one wait state to every target this daemon serves. The supervised wait's own
// state is derived live (waitBeaconState): `limited` with the earliest concrete instant a route
// lifts, or `draining` with the schedule's resume time when a pause window — daemon-wide or a route
// pricing window — is what the wait is actually waiting for (CLA-1024). The wait blocks the whole
// loop, so no other project's queue is being serviced either: beaconing only the target whose
// harness hit the limit would leave the other projects' presence rows silent, and each of those
// reads as a dead instance for exactly the reason this state exists to correct. The route breakdown
// rides only ti's row: the sibling projects are paused by the same wait, but the route names and
// schedules belong to the waiting project's config, and another project's row must not show routes
// it never configured (review 1.review-1.4).
func (d *Driver) beaconWait(ti int, st fleet.State) {
	d.writeLocalBeacon(st)
	for i := range d.targets {
		s := st
		if i != ti {
			s.Routes = nil
		}
		d.sendBeacon(i, d.targets[i], s)
	}
}

// beaconSessionTargets is the in-session presence lap: the poll's own per-target beacon, for the
// window in which the poll does not run. The running target reports what fleetState derives for it
// (iteration with the current claim's ref and phase); every other target the daemon serves reports
// its own state — idle, waiting while its own blocking question stands, or draining while a pause
// holds its spawns — because no session is running there.
func (d *Driver) beaconSessionTargets(running int, runningTarget Target) {
	d.beacon(running, runningTarget, d.fleetState(running))
	for i := range d.targets {
		if i == running {
			continue
		}
		d.sendBeacon(i, d.targets[i], d.fleetState(i))
	}
}

// pollBeaconState folds one target's presence into the daemon-wide state the poll pass writes to
// the local BEACON. The file takes no target, so with several projects one of them has to speak for
// the daemon, and target order must not decide which: a scheduled pause states the resume instant
// the console's banner shows, so an idle or console-paused sibling polled later must not overwrite
// it.
func pollBeaconState(cur, next fleet.State) fleet.State {
	switch {
	case !isSchedulePause(cur) && isSchedulePause(next):
		return next
	case isSchedulePause(cur) && isSchedulePause(next) && cur.TaskRef == "" && next.TaskRef != "":
		return next
	case cur.Kind == fleet.StateIdle && next.Kind != fleet.StateIdle:
		return next
	}
	return cur
}

// isSchedulePause reports whether st is the schedule's draining payload. The schedule is the one
// cause the local file has a sentence for (supervise's listing), which is why it outranks the
// others there.
func isSchedulePause(st fleet.State) bool {
	return st.Kind == fleet.StateDraining && st.PauseReason == fleet.PauseReasonSchedule
}

// LocalBeaconName is the local presence file the daemon keeps beside its plane beacons: the same
// identity+state the next POST carries, written into the state dir so the supervisor's roll (phase
// 5b) can verify a restarted child is reporting the new version WITHOUT a plane read.
const LocalBeaconName = "BEACON"

// LocalBeacon is the JSON shape of LocalBeaconName. The supervisor's roll reads it with this same
// type — one contract, not two mirrored structs — so a field or tag renamed here cannot leave
// the reader behind.
type LocalBeacon struct {
	Version     string     `json:"version"`
	State       string     `json:"state"`
	At          time.Time  `json:"at"`
	ResetAt     *time.Time `json:"resetAt,omitempty"`
	PauseReason string     `json:"pauseReason,omitempty"`
	PauseUntil  *time.Time `json:"pauseUntil,omitempty"`
	PauseZone   string     `json:"pauseZone,omitempty"`
	TaskRef     string     `json:"taskRef,omitempty"`
	Phase       string     `json:"phase,omitempty"`
}

// localBeaconResetAt is the reset instant the local file carries: the stated one on a `limited`
// state, and nothing on any other kind (the plane drops it there too), so one representation of "no
// reset" rather than two.
func localBeaconResetAt(st fleet.State) *time.Time {
	if st.Kind != fleet.StateLimited || st.ResetAt.IsZero() {
		return nil
	}
	at := st.ResetAt.UTC()
	return &at
}

// localBeaconPause is the pause payload the local file carries: the stated reason and, for the
// schedule, the window's end and zone, and nothing on any other state — the same restriction the
// wire applies, so the file and the next POST cannot disagree about whether a pause is on.
func localBeaconPause(st fleet.State) (reason string, until *time.Time, zone string) {
	if st.Kind != fleet.StateDraining || st.PauseReason == "" {
		return "", nil, ""
	}
	reason = st.PauseReason
	if reason != fleet.PauseReasonSchedule {
		return reason, nil, ""
	}
	if !st.PauseUntil.IsZero() {
		at := st.PauseUntil.UTC()
		until = &at
	}
	return reason, until, st.PauseZone
}

// writeLocalBeacon refreshes the local beacon file. Fail-soft exactly like the plane beacon:
// telemetry is never control flow, so a write failure is logged (once per streak) and dropped —
// the daemon's own life never depends on it.
func (d *Driver) writeLocalBeacon(st fleet.State) {
	if d.state == nil {
		return
	}
	reason, until, zone := localBeaconPause(st)
	body, err := json.Marshal(LocalBeacon{
		Version:     version.Current,
		State:       st.Kind,
		At:          time.Now().UTC(),
		ResetAt:     localBeaconResetAt(st),
		PauseReason: reason,
		PauseUntil:  until,
		PauseZone:   zone,
		TaskRef:     st.TaskRef,
		Phase:       st.Phase,
	})
	if err != nil {
		return // the state kinds and string fields cannot fail to marshal
	}
	_ = d.state.Remove(LocalBeaconName)
	if err := d.state.WriteFile(LocalBeaconName, body); err != nil {
		if errors.Is(err, statedir.ErrExists) {
			// A concurrent beacon won the remove-then-create race — the claim reflector beacons from the
			// lease-renewer goroutine, so two beacons can write this file at once.
			return
		}
		if !d.beaconWriteFailed.Swap(true) {
			log.Printf("fleet: cannot write the local beacon into %s (%v) - the roll cannot verify a restarted child without it", d.state.Path(), err)
		}
		return
	}
	if d.beaconWriteFailed.Swap(false) {
		log.Printf("fleet: local beacon write recovered")
	}
}

// reflectClaim returns the claim reflector for target ti: it keeps the mid-drain presence ref in
// step with the CURRENT claim the session holds — set when a claim is actually observed, cleared
// when the session settles it or holds nothing — and beacons immediately on a change, so the
// fleet card fills in within seconds of a first-phase claim instead of at the next phase boundary.
// Only a ref CHANGE beacons; a session that churns claims cannot turn this into a stream.
func (d *Driver) reflectClaim(ti int, t Target, done <-chan struct{}) func(harness.Claim) {
	return func(c harness.Claim) {
		ref := ""
		if c.Held() {
			ref = claimLabel(c)
		}
		d.iterMu.Lock()
		select {
		case <-done:
			d.iterMu.Unlock()
			return
		default:
		}
		s := d.iterAt(ti)
		if !s.on || s.ref == ref {
			d.iterMu.Unlock()
			return
		}
		s.ref = ref
		d.iter[ti] = s
		st := fleet.State{Kind: fleet.StateIteration, N: s.n, TaskRef: s.ref, Phase: s.phase, Model: s.model}
		d.iterMu.Unlock()
		d.beacon(ti, t, st)
	}
}

// fleetShutdown delivers each target's final `stopping` beacon concurrently, under ONE deadline for
// the whole set. It runs deferred from Run, so EVERY exit path says goodbye — STOP/HALT, a restart
// marker, max-iterations, budget, signal, even a hard error return — which is what makes the
// console's silence-after-this-point meaningful: after a stopping beacon, quiet means the daemon is
// gone, not that it stopped talking.
//
// The shared deadline is load-bearing, not tidiness: against a hanging plane a serial Close per target
// would hold Ctrl-C, the ctl re-exec and the supervisor's restart for one per-target worst case each.
func (d *Driver) fleetShutdown() {
	// One daemon-wide state, one daemon-wide file: writeLocalBeacon takes no
	// target, so writing it per target would remove-then-create identical bytes.
	d.writeLocalBeacon(fleet.State{Kind: fleet.StateStopping})

	budget := d.shutdownBudget
	if budget <= 0 {
		budget = fleet.CloseBudget
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	beacons := make([]fleet.ShutdownBeacon, 0, len(d.targets))
	for i := range d.targets {
		if d.targets[i].Fleet == nil {
			continue
		}
		beacons = append(beacons, fleet.ShutdownBeacon{
			Reporter: d.targets[i].Fleet,
			Report: fleet.Report{
				Identity: d.fleetIdentity(i),
				State:    fleet.State{Kind: fleet.StateStopping},
			},
		})
	}
	fleet.Shutdown(ctx, beacons)
}

// beginIteration marks target ti mid-drain and beacons the phase change NOW, so the console does
// not wait out a poll interval to learn which phase is about to run. model is that phase's
// resolved starting route (CLA-1026), shown on the card while the session runs.
func (d *Driver) beginIteration(ti int, t Target, drainNum int, ref, phase, model string) {
	// A drain of the very task this target was waiting on is evidence its answer landed — the ask
	// leaves that task blocked, so it cannot be claimed while the question stands. A drain of any
	// OTHER task says nothing about the open question, so the marker stays until the poll sees the
	// count fall back. Reporting only — the spawn decisions have already been made.
	d.clearWaiting(ti, ref)
	d.iterMu.Lock()
	if ti >= 0 && ti < len(d.iter) {
		d.iter[ti] = iterState{on: true, n: drainNum, ref: ref, phase: phase, model: model}
	}
	d.iterMu.Unlock()
	d.beacon(ti, t, d.fleetState(ti))
}

// noteIterationModel revises the live route the card shows, when a mid-session route fallback
// switched to another route (CLA-1026). Only a CHANGE beacons, and only during a live iteration: a
// session that reports no switch must not turn the card into a stream.
func (d *Driver) noteIterationModel(ti int, t Target, model string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	d.iterMu.Lock()
	s := d.iterAt(ti)
	if !s.on || s.model == model {
		d.iterMu.Unlock()
		return
	}
	s.model = model
	d.iter[ti] = s
	st := fleet.State{Kind: fleet.StateIteration, N: s.n, TaskRef: s.ref, Phase: s.phase, Model: s.model}
	d.iterMu.Unlock()
	d.beacon(ti, t, st)
}

// endIteration clears the mid-drain marker and posts ONE iteration record with the outcome mapped
// from what the loop already knows, riding the same report as the back-to-normal presence state.
// Outcome precedence, decided once here so all four words mean one thing: - parked the dead-phase
// budget parked the task (parkDeadPhase ran) - dead the drain ended on a death: a dead phase that
// exhausted its retry/park paths, a fleet trip, or a returned error (zero-spend ladder,
// unclassified ladder, repo_not_found...) - else the CLAIM's disposition at the boundary: pushed
// work left for a successor takeover -> checkpoint; anything else (handed back to ready, or settled
// by the session itself) -> released - nothing observed at all -> released, best-effort (the
// iteration ran; exactly one record posts per iteration, always) Free text is NEVER an outcome:
// marshalReport refuses the whole report on a word it does not know, so a note about the drain
// belongs in the log.
func (d *Driver) endIteration(ti int, t Target, rec fleet.Iteration) {
	switch rec.Outcome {
	case fleet.OutcomeCheckpoint, fleet.OutcomeReleased, fleet.OutcomeParked, fleet.OutcomeDead:
	default:
		task := ""
		if rec.TaskRef != "" {
			task = " (task " + rec.TaskRef + ")"
		}
		log.Printf("%sBUG: iteration outcome %q%s is not one of the four the plane knows (checkpoint/released/parked/dead) - reporting it as %q so the fleet report and its presence refresh are not dropped; the caller must put prose in the log, never the outcome",
			labelOf(t), rec.Outcome, task, fleet.OutcomeReleased)
		rec.Outcome = fleet.OutcomeReleased
	}
	d.iterMu.Lock()
	if ti >= 0 && ti < len(d.iter) {
		d.iter[ti] = iterState{}
	}
	d.iterMu.Unlock()
	d.beacon(ti, t, d.fleetState(ti), rec)
}

// claimDisposition maps a claim still held at the drain boundary onto the two non-fatal outcomes:
// HasWIP means the lease was deliberately left for a takeover (checkpoint); anything else either
// goes back to the queue via the deferred handback or was settled by the session itself (released).
func claimDisposition(res *harness.Result) string {
	if res == nil {
		return ""
	}
	if res.Claim.HasWIP {
		return fleet.OutcomeCheckpoint
	}
	return fleet.OutcomeReleased
}

// sessionRunID is the run the session worked the task under, or "" when it never held one. The
// value is the run's UUID, never the credential the session holds.
func sessionRunID(res harness.Result, prev *harness.Result) string {
	if res.Claim.RunID != "" {
		return runUUID(res.Claim.RunID)
	}
	if prev != nil {
		return runUUID(prev.Claim.RunID)
	}
	return ""
}

// sessionCost is the harness-reported dollar figure for one session's Result, or nil when there is
// no figure to carry. And THIS session has to have actually reported one: Result.CostReported,
// because a session whose stream ended before its harness's accounting reported tokens and no
// dollars, and a zero taken from that silence would read on the console as "$0 spent" for a session
// that cost real money.
func sessionCost(a harness.Adapter, res *harness.Result) *float64 {
	if res == nil || !res.CostReported || !a.Capabilities().ReportsCost {
		return nil
	}
	cost := res.CostUSD
	return &cost
}

// sessionSpend is what one PHASE PASS spent, summed across the attempt ladder's sessions. Cost is
// summed only while EVERY attempt that ran produced a figure; one attempt whose dollars were never
// reported leaves the pass's cost UNKNOWN rather than partial, because a partial figure presented
// as the session's cost understates it.
type sessionSpend struct {
	usage         harness.TokenUsage
	usageReported bool
	costUSD       float64
	costKnown     bool
	attempted     bool
}

// add folds one attempt's Result in. Called from recordPassSpend, which the loop runs at each place
// it counts an attempt's spend — exactly once per counted attempt.
func (s *sessionSpend) add(a harness.Adapter, res harness.Result) {
	// sessionCost answers both questions this needs: whether the adapter reports money at all, and
	// whether THIS attempt actually reported some.
	known := sessionCost(a, &res)
	if !s.attempted {
		// The first attempt seeds whether the pass has a dollar figure at all.
		s.costKnown = known != nil
	} else if known == nil {
		s.costKnown = false
	}
	s.attempted = true
	s.usage.Add(res.Usage)
	s.usageReported = s.usageReported || res.UsageReported
	if known != nil {
		s.costUSD += *known
	}
}

// cost returns the pass's dollar figure as a pointer, or nil when it is unknown. A figure only
// counts as one when the pass actually REPORTED usage.
func (s *sessionSpend) cost() *float64 {
	if !s.costKnown || !s.usageReported {
		return nil
	}
	cost := s.costUSD
	return &cost
}

// recordPassSpend folds one attempt's Result into the phase pass's running spend and publishes it
// on the pass's session entry.
func recordPassSpend(s *fleet.Session, a harness.Adapter, pass *sessionSpend, res *harness.Result) {
	if s == nil || res == nil {
		return
	}
	pass.add(a, *res)
	s.Usage = fleet.TokenUsage(pass.usage)
	s.UsageReported = pass.usageReported
	s.CostUSD = pass.cost()
}
