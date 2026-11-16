// fleet.go — the driver's half of activity reporting (CLA-466): presence
// beacons riding the backlog-summary poll, state-change beacons at phase
// boundaries, and one iteration record per drain at the boundary the loop just
// crossed.
//
// Everything here leans on the same design rule the reporter itself serves:
// telemetry is never control flow. Every helper is a nil-safe no-op when the
// target carries no Reporter (not wired, or a test), and nothing in the loop
// branches on whether a report happened.

package loop

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
	"github.com/lecstor/clankerbar-cli/internal/version"
)

// iterState is one target's mid-drain presence: what `iteration {n, taskRef,
// phase}` the Fleet page should show while this drain runs.
//
// Accesses are guarded by Driver.iterMu: the loop writes at phase boundaries
// (beginIteration / endIteration), and since CLA-510 the lease renewer's
// goroutine ALSO revises the ref mid-session (reflectClaim), so these are no
// longer touched by the loop alone.
type iterState struct {
	on    bool
	n     int
	ref   string
	phase string
}

// hostnameOnce caches os.Hostname for the life of the driver. It is identity
// decoration on every beacon; a per-report syscall would be waste, and an error
// simply leaves the host blank plane-side.
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

// fleetIdentity composes what every beacon for target ti carries: who it is,
// where it runs, what build, which config. The fingerprint is the target's
// EFFECTIVE config — the CLA-410 overlay when one is in force, else the base
// file, the same rule the drain reads through d.targetCfg — because that is
// what the drain runs on. Read fresh per report, so a RELOAD moves it without a
// restart: at once for a target with no overlay in force, and for an overlaid
// one when the refetch at the next poll boundary lands (d.invalidateRunConfigs).
func (d *Driver) fleetIdentity(ti int) fleet.Identity {
	// A target INDEX, not a captured Target: the poll beacons a target before
	// checkRunConfigVersion rebuilds its overlay, so a copy would fingerprint
	// the document being replaced. d.cfg is read under controlMu because a
	// RELOAD swaps it on another goroutine (a panel member parked in
	// waitResponsive); the pointer taken there is immutable — a reload installs
	// a new config rather than editing this one.
	d.controlMu.Lock()
	base := d.cfg
	eff := base
	if ti >= 0 && ti < len(d.targets) && d.targets[ti].Cfg != nil {
		eff = d.targets[ti].Cfg
	}
	// Explicit instance_name wins; otherwise hostname plus config basename,
	// unique per running daemon, so co-located daemons no longer collapse into
	// one presence row (CLA-501). Stable across a RELOAD: the reloader re-reads
	// the same path, so the basename half cannot change under a live daemon.
	// A hand-built test Driver may carry no config at all.
	inst := config.ResolveInstanceName("", "", d.hostnameOnce())
	if base != nil {
		inst = base.ResolvedInstanceName(d.hostnameOnce())
	}
	d.controlMu.Unlock()
	cfgID := ""
	if eff != nil {
		cfgID = eff.Identity()
	}
	return fleet.Identity{
		Instance:       inst,
		Host:           d.hostnameOnce(),
		Version:        version.Current,
		ConfigIdentity: cfgID,
	}
}

// holdState is a phase boundary the schedule is holding for one target
// (CLA-908): the task whose claim is kept and the phase that will run when the
// window ends. Presence names both, the way the console banner does.
type holdState struct {
	on    bool
	ref   string
	phase string
}

// fleetState derives the presence State for target ti from what the loop already
// tracks — no new bookkeeping beyond the mid-drain marker set in drainPhases and
// the hold marker set in holdThroughPause:
//
//   - held at a phase boundary by a pause window -> draining with the schedule
//     reason, the window's end, and the held task's ref and phase (CLA-908).
//     This outranks the iteration marker because the walk's iteration stays open
//     across the hold while no session is running.
//   - mid-drain                -> iteration {n, taskRef, phase}
//   - console-, fleet-, startup- or schedule-paused -> draining (alive, spawning nothing new),
//     carrying the cause the operator has to act on: a console, fleet, startup
//     or budget pause names itself and wins over the schedule.
//   - budget-exhausted           -> draining with the budget reason
//   - otherwise                -> idle
//
// A backed-off target (skipUntil still in the future) reports idle on purpose:
// the back-off is minutes-long and self-clearing, and flapping the state to
// draining for every quiet spell would blur the one signal draining exists for —
// "something is holding spawns closed until an operator acts".
func (d *Driver) fleetState(ti int) fleet.State {
	d.iterMu.Lock()
	s := d.iterAt(ti)
	h := d.holdAt(ti)
	d.iterMu.Unlock()
	// A running session outranks every pause flag: the flags only ever hold the
	// NEXT spawn, and the iteration is what the Fleet page shows while work is
	// in flight. The one exception is the hold, above, where no session runs.
	if s.on && !h.on {
		return fleet.State{Kind: fleet.StateIteration, N: s.n, TaskRef: s.ref, Phase: s.phase}
	}
	// A pause the operator has to act on wins over the schedule: report one
	// cause, and it is the one waiting on a human. A scheduled window clears
	// itself; these do not.
	if reason := d.operatorPauseReason(ti); reason != "" {
		return fleet.State{Kind: fleet.StateDraining, PauseReason: reason}
	}
	if d.schedPaused {
		return d.scheduleState(h)
	}
	return fleet.State{Kind: fleet.StateIdle}
}

// operatorPauseReason names the strongest non-schedule cause holding target ti's
// spawns closed, or "" when none does. Console outranks fleet outranks startup
// outranks budget: that is the order an operator works through, from the lever on
// the page they are looking at to the one that needs a decision or a config edit.
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

// scheduleState is the scheduled pause's presence: draining, why, until when,
// and — when a phase sequence is held — what is waiting behind it.
//
// The window is re-read rather than trusted from the flag: schedPaused is
// daemon bookkeeping updated at the poll gate and at a boundary, so it can
// still be set for one idle interval after the window ended (and after a STOP
// landed during a hold). A stale flag reports plain draining, never a reason
// with no resume instant.
func (d *Driver) scheduleState(h holdState) fleet.State {
	st := fleet.State{Kind: fleet.StateDraining}
	active, until, pw := d.pauseState()
	if !active {
		return st
	}
	st.PauseReason = fleet.PauseReasonSchedule
	st.PauseUntil = until
	if pw != nil {
		st.PauseZone = pw.Timezone
	}
	if h.on {
		st.TaskRef, st.Phase = h.ref, h.phase
	}
	return st
}

// iterAt/pausedAt/fleetPausedAt/startupPausedAt are bounds-checked reads of the per-target
// slices. A Driver built by hand in a test carries empty slices; reporting must
// degrade to idle for it, not panic.
func (d *Driver) iterAt(ti int) iterState {
	if ti < 0 || ti >= len(d.iter) {
		return iterState{}
	}
	return d.iter[ti]
}

// holdAt is the bounds-checked read of the per-target hold marker, mirroring
// iterAt: a hand-built test Driver carries an empty slice and reports no hold.
func (d *Driver) holdAt(ti int) holdState {
	if ti < 0 || ti >= len(d.hold) {
		return holdState{}
	}
	return d.hold[ti]
}

// holdPause and clearHoldPause mark a target as held at a phase boundary by the
// schedule and drop the mark when it ends (CLA-908). The caller owns the pair:
// holdThroughPause defers the clear, so every way the hold can end — resume,
// STOP, a lost run — leaves no stale hold on the presence row.
func (d *Driver) holdPause(ti int, ref, phase string) {
	d.iterMu.Lock()
	defer d.iterMu.Unlock()
	if ti < 0 || ti >= len(d.hold) {
		return
	}
	d.hold[ti] = holdState{on: true, ref: ref, phase: phase}
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

// startupPausedAt is the startup-trip (CLA-562) half of the draining signal:
// a target paused awaiting the operator's answer to the harness-startup
// question is alive and spawning nothing new, exactly like a fleet-paused one,
// and must not read idle on the console.
func (d *Driver) startupPausedAt(ti int) bool {
	return ti >= 0 && ti < len(d.startupPaused) && d.startupPaused[ti]
}

// beacon sends one presence report for target ti. Nil-safe on the reporter so
// tests and not-wired targets need no branching at the call sites.
func (d *Driver) beacon(ti int, t Target, st fleet.State, iterations ...fleet.Iteration) {
	d.writeLocalBeacon(st)
	d.sendBeacon(ti, t, st, iterations...)
}

// sendBeacon is beacon without the local file: the plane half alone. Split out
// for the callers that report one daemon-wide state to several targets, so the
// local BEACON file (which takes no target) is written ONCE per lap instead of
// once per target.
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

// beaconLimited tells the plane this daemon is parked on a usage limit
// (CLA-728), for EVERY target it serves. The wait blocks the whole loop, so no
// other project's queue is being serviced either — beaconing only the target
// whose harness hit the limit would leave the other projects' presence rows
// silent, and each of those reads as a dead instance for exactly the reason
// this state exists to correct.
//
// It rides the ordinary beacon path, so the local BEACON file in the state dir
// carries the same state: `supervise` status and the console agree by
// construction rather than by two writers that could drift. resetAt is the
// harness's stated instant, or the zero time when it stated none (or when the
// wait has already measured that instant false).
func (d *Driver) beaconLimited(resetAt time.Time) {
	st := fleet.State{Kind: fleet.StateLimited, ResetAt: resetAt}
	// One daemon-wide state, one daemon-wide file: writeLocalBeacon takes no
	// target, so writing it inside the per-target loop below would
	// remove-then-create the identical bytes once per target on every lap.
	d.writeLocalBeacon(st)
	for i := range d.targets {
		d.sendBeacon(i, d.targets[i], st)
	}
}

// beaconSessionTargets is the in-session presence lap (CLA-771): the poll's own
// per-target beacon, for the window in which the poll does not run. The running
// target reports what fleetState derives for it (iteration with the current
// claim's ref and phase); every other target the daemon serves reports its own
// state, which is idle - or draining while a pause holds its spawns - because no
// session is running there.
//
// The other targets are not decoration. Each project has its own Fleet reporter
// and its own project-scoped presence row, so beaconing only the running target
// would leave every sibling project's row to go stale for the whole session, and
// that project's console would show the daemon as a dead instance (the same
// symptom CLA-771 exists to fix, one project over). beaconLimited beacons every
// target for the same reason.
//
// running and runningTarget are handed in rather than indexed out of d.targets
// so a hand-built driver - the unit tests - still beacons the target it was
// given; the lap then adds any other target, skipping that index.
//
// Only the running target writes the local BEACON file: it takes no target, and
// its last writer wins. The running target's row is the informative one - the
// iteration, or the task and phase a schedule is holding (CLA-908) - so the
// siblings' beacons are plane-only rather than letting whichever project is
// last in the list decide what `supervise` prints about this daemon.
func (d *Driver) beaconSessionTargets(running int, runningTarget Target) {
	d.beacon(running, runningTarget, d.fleetState(running))
	for i := range d.targets {
		if i == running {
			continue
		}
		d.sendBeacon(i, d.targets[i], d.fleetState(i))
	}
}

// LocalBeaconName is the local presence file the daemon keeps beside its plane
// beacons: the same identity+state the next POST carries, written into the
// state dir so the supervisor's roll (phase 5b) can verify a restarted child
// is reporting the new version WITHOUT a plane read. It is the "beacon" the
// roll's verify-before-next gate reads.
const LocalBeaconName = "BEACON"

// localBeacon is the JSON shape of LocalBeaconName. The supervisor's roll
// mirrors it for reading (the same roster-style lockstep: two packages, one
// contract), so a new field here must land in internal/supervisor/roll.go too.
//
// ResetAt is the `limited` state's payload (CLA-728): the instant the harness
// stated, present only while the wait still believes it — exactly the field the
// plane POST carries, so the local file keeps mirroring the next report rather
// than saying `limited` with no moment while the console shows a countdown. A
// pointer rather than a time.Time so `omitempty` can actually omit it (the
// encoder never treats a time.Time's zero value as empty).
//
// PauseReason/PauseUntil are the same contract for a scheduled pause (CLA-908):
// the reason and the window's end, so `supervise` prints what the console's
// banner shows without a plane read. PauseZone is the schedule's IANA zone —
// local only, so the listing can say "14:00 AEST" as the operator configured it
// rather than the machine's zone. TaskRef/Phase mirror the plane POST's pair
// (the running iteration's, or the task and phase a hold is waiting behind the
// window).
type localBeacon struct {
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

// localBeaconResetAt is the reset instant the local file carries: the stated
// one on a `limited` state, and nothing on any other kind (the plane drops it
// there too), so one representation of "no reset" rather than two.
func localBeaconResetAt(st fleet.State) *time.Time {
	if st.Kind != fleet.StateLimited || st.ResetAt.IsZero() {
		return nil
	}
	at := st.ResetAt.UTC()
	return &at
}

// localBeaconPause is the pause payload the local file carries: the stated
// reason and, for the schedule, the window's end and zone, and nothing on any
// other state — the same restriction the wire applies, so the file and the next
// POST cannot disagree about whether a pause is on.
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

// writeLocalBeacon refreshes the local beacon file. Fail-soft exactly like the
// plane beacon: telemetry is never control flow, so a write failure is logged
// (once per streak) and dropped — the daemon's own life never depends on it.
// The write is remove-then-create, like the roster cache write: a concurrent
// reader (the roll) sees either the absent name or complete bytes, never a
// torn file, and a symlink planted at the name is removed, not followed.
func (d *Driver) writeLocalBeacon(st fleet.State) {
	if d.state == nil {
		return
	}
	reason, until, zone := localBeaconPause(st)
	body, err := json.Marshal(localBeacon{
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
			// A concurrent beacon won the remove-then-create race — the claim
			// reflector (CLA-510) beacons from the lease-renewer goroutine, so
			// two beacons can write this file at once. The winner's file is a
			// complete, fresh beacon from the same daemon, which is exactly
			// what this write was going to produce: not a failure, and not a
			// reason to tell the operator the roll cannot verify.
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

// reflectClaim returns the claim reflector for target ti (CLA-510): it keeps
// the mid-drain presence ref in step with the CURRENT claim the session holds
// — set when a claim is actually observed, cleared when the session settles it
// or holds nothing — and beacons immediately on a change, so the fleet card
// fills in within seconds of a first-phase claim instead of at the next phase
// boundary. It is a REVISION of the in-flight state, never a second iteration
// record: the one record per drain posted by endIteration is unchanged.
//
// It rides the SAME observation seam as lease renewal — the renewer calls it
// from apply for every snapshot its OnClaim hook receives — so there is no
// second watcher to keep in agreement, and a claim that was REFUSED never
// reaches it: a refused claim records no ids, so the adapter fires nothing
// (harness.Invocation.OnClaim). Only a ref CHANGE beacons; a session that
// churns claims cannot turn this into a stream.
//
// Runs on the renewer goroutine, which is exactly why iterMu exists: the loop
// writes d.iter at phase boundaries while this revises it mid-session.
//
// The renewer never reflects after stop() returns: leaseRenewer.apply drops
// snapshots once done is closed, and the write site re-checks done under
// iterMu below, so a snapshot that slipped past the apply guard while done was
// still open cannot land on a later phase's row even if something between the
// two checks were ever made blocking. No revision can land on a cleared row or
// a later phase's row: close(done) precedes the loop's own row writes, and the
// write site re-checks done under the same mutex those writes serialize on.
// The beacon that follows a write is a report send, governed by the reporter's
// own non-blocking contract, not driver state.
func (d *Driver) reflectClaim(ti int, t Target, done <-chan struct{}) func(harness.Claim) {
	return func(c harness.Claim) {
		ref := ""
		if c.Held() {
			ref = claimLabel(c)
		}
		d.iterMu.Lock()
		select {
		case <-done:
			// stop() closed done while this snapshot was in flight: the
			// session is over and the row is about to be cleared or handed to
			// the next phase — revising it now would attribute work the
			// session no longer owns.
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
		st := fleet.State{Kind: fleet.StateIteration, N: s.n, TaskRef: s.ref, Phase: s.phase}
		d.iterMu.Unlock()
		d.beacon(ti, t, st)
	}
}

// fleetShutdown delivers each target's final `stopping` beacon synchronously.
// It runs deferred from Run, so EVERY exit path says goodbye — STOP/HALT, a
// restart marker, max-iterations, budget, signal, even a hard error return —
// which is what makes the console's silence-after-this-point meaningful: after
// a stopping beacon, quiet means the daemon is gone, not that it stopped talking.
func (d *Driver) fleetShutdown() {
	for i := range d.targets {
		t := d.targets[i]
		d.writeLocalBeacon(fleet.State{Kind: fleet.StateStopping})
		if t.Fleet == nil {
			continue
		}
		t.Fleet.Close(fleet.Report{
			Identity: d.fleetIdentity(i),
			State:    fleet.State{Kind: fleet.StateStopping},
		})
	}
}

// beginIteration marks target ti mid-drain and beacons the phase change NOW, so
// the console does not wait out a poll interval to learn which phase is about to
// run. Called just before each spawn — including handoff respawns of the same
// phase, whose repeat beacon is harmless (presence is last-writer-wins).
func (d *Driver) beginIteration(ti int, t Target, drainNum int, ref, phase string) {
	d.iterMu.Lock()
	if ti >= 0 && ti < len(d.iter) {
		d.iter[ti] = iterState{on: true, n: drainNum, ref: ref, phase: phase}
	}
	d.iterMu.Unlock()
	d.beacon(ti, t, d.fleetState(ti))
}

// endIteration clears the mid-drain marker and posts ONE iteration record with
// the outcome mapped from what the loop already knows, riding the same report as
// the back-to-normal presence state.
//
// Outcome precedence, decided once here so all four words mean one thing:
//
//   - parked  the dead-phase budget parked the task (parkDeadPhase ran)
//   - dead    the drain ended on a death: a dead phase that exhausted its
//     retry/park paths, a fleet trip, or a returned error (zero-spend ladder,
//     unclassified ladder, repo_not_found...)
//   - else the CLAIM's disposition at the boundary: pushed work left for a
//     successor takeover -> checkpoint; anything else (handed back to ready, or
//     settled by the session itself) -> released
//   - nothing observed at all -> released, best-effort (the iteration ran;
//     exactly one record posts per iteration, always)
//
// Free text is NEVER an outcome (CLA-738): marshalReport refuses the whole
// report on a word it does not know, so a note about the drain belongs in the
// log. A caller that appends prose to the disposition loses the record AND the
// presence refresh riding the same report.
//
// That invariant is enforced here as a last line of defence, not left to
// convention: an outcome outside the four is posted as released with one loud
// line naming the caller's mistake. The alternative is what CLA-738's incident
// did — the reporter refuses the WHOLE report (every iteration plus the presence
// refresh) and then logs only the first failure of the streak, so one prose
// outcome would silently starve the plane for the rest of the session. One
// degraded iteration is the cheaper failure, and the loud line is what gets the
// convention fixed.
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

// claimDisposition maps a claim still held at the drain boundary onto the two
// non-fatal outcomes: HasWIP means the lease was deliberately left for a
// takeover (checkpoint); anything else either goes back to the queue via the
// deferred handback or was settled by the session itself (released).
func claimDisposition(res *harness.Result) string {
	if res == nil {
		return ""
	}
	if res.Claim.HasWIP {
		return fleet.OutcomeCheckpoint
	}
	return fleet.OutcomeReleased
}

// sessionRunID is the run the session worked the task under, or "" when it never
// held one (CLA-691). The value is the run's UUID, never the credential the
// session holds.
//
// The split is load-bearing, not cosmetic: the id `claim_task` returns — and
// seeds into a resumed phase's brief — is the COMPOUND credential
// `<run uuid>.<secret>`, and the secret half is what AUTHORISES a write as that
// run (the plane refuses a bare uuid for a credential-bound run). Telemetry must
// not carry a bearer token: this report is stored on the plane and served back to
// every key in the project, while the plane keeps the plaintext secret out of
// every durable record (CLA-509). The uuid is also the only form the per-round
// join can use, since the plane's own records hold bare uuids. `runUUID` is the
// existing CLA-509 helper for exactly this split, so both sides parse the
// credential the same way; the plane splits again on arrival, because a reporter
// is never trusted.
//
// A session that CLAIMED the task observed its own run id, and that is the
// value to record. A later phase's session never calls claim_task — the driver
// resumes the run the predecessor left held — so it has no observation of its
// own, and the id comes from the claim carried INTO the phase. That is the same
// claim the driver substitutes into the session's brief as {{runId}}, so the
// record and the session's own writes agree by construction rather than by two
// lookups that could drift.
func sessionRunID(res harness.Result, prev *harness.Result) string {
	if res.Claim.RunID != "" {
		return runUUID(res.Claim.RunID)
	}
	if prev != nil {
		return runUUID(prev.Claim.RunID)
	}
	return ""
}

// sessionCost is the harness-reported dollar figure for one session's Result, or
// nil when there is no figure to carry.
//
// Two questions have to be answered yes, and they are different ones. The
// adapter has to be one that reports money at all — Capabilities.ReportsCost,
// false for codex and copilot today — or there is no figure by construction. And THIS
// session has to have actually reported one: Result.CostReported, because a
// session whose stream ended before its harness's accounting reported tokens and
// no dollars, and a zero taken from that silence would read on the console as
// "$0 spent" for a session that cost real money. Absence is "unknown", which is
// what nil means, and it is why the wire field is nullable rather than a zero.
//
// A harness that DID report a zero (a free local model, a cached-only turn)
// keeps its zero: the pointer survives marshalling.
func sessionCost(a harness.Adapter, res *harness.Result) *float64 {
	if res == nil || !res.CostReported || !a.Capabilities().ReportsCost {
		return nil
	}
	cost := res.CostUSD
	return &cost
}

// sessionSpend is what one PHASE PASS spent, summed across the attempt ladder's
// sessions (CLA-691).
//
// The fleet record publishes one session entry per pass, and a pass that
// retried is several paid processes: the loop counts every attempt toward the
// iteration's total (its own comment: "each attempt is a distinct session")
// while the entry keeps the LAST attempt's identity. Summing the money here is
// what keeps the per-session figures adding up to the iteration figure — the
// property the operator's cost analysis reads them for — instead of dropping
// every retried attempt's spend on the floor.
//
// Cost is summed only while EVERY attempt that ran produced a figure; one
// attempt whose dollars were never reported leaves the pass's cost UNKNOWN
// rather than partial, because a partial figure presented as the session's cost
// understates it. A pass that reported no usage at all has no figure either,
// however many attempts carried one — see cost(). The token buckets have no such
// problem: they sum exactly.
type sessionSpend struct {
	usage         harness.TokenUsage
	usageReported bool
	costUSD       float64
	costKnown     bool
	attempted     bool
}

// add folds one attempt's Result in. Called from recordPassSpend, which the loop
// runs at each place it counts an attempt's spend — exactly once per counted
// attempt.
func (s *sessionSpend) add(a harness.Adapter, res harness.Result) {
	// sessionCost answers both questions this needs: whether the adapter reports
	// money at all, and whether THIS attempt actually reported some.
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

// cost returns the pass's dollar figure as a pointer, or nil when it is unknown.
//
// A figure only counts as one when the pass actually REPORTED usage (CLA-737).
// `CostReported` and `UsageReported` answer different questions, and an adapter
// that reports money can still set the first with no usage report at all:
// opencode's export path does exactly that for a session killed before its first
// answered request (a provider.quota error is the common one), where the run's
// export is all zeroes but still a document with a cost. Its zero is silence,
// not "$0 spent", so the pass has no figure to publish — and the wire must not
// carry one, because the plane refuses the WHOLE report with
// "session.costUsd requires session.usageReported" and every other iteration in
// that batch, presence refresh included, is lost with it.
func (s *sessionSpend) cost() *float64 {
	if !s.costKnown || !s.usageReported {
		return nil
	}
	cost := s.costUSD
	return &cost
}

// recordPassSpend folds one attempt's Result into the phase pass's running spend
// and publishes it on the pass's session entry (CLA-691).
//
// It is called from the SAME places the loop adds this attempt's Tokens/CostUSD
// to the iteration's accumulators, and ONLY from there: the loop skips counting an
// unreadable stream and (on teardown) a cancelled run, and the per-session figures
// have to skip those too, or the sessions would add up to more than the iteration
// they belong to. One call site, one rule.
func recordPassSpend(s *fleet.Session, a harness.Adapter, pass *sessionSpend, res *harness.Result) {
	if s == nil || res == nil {
		return
	}
	pass.add(a, *res)
	s.Usage = fleet.TokenUsage(pass.usage)
	s.UsageReported = pass.usageReported
	s.CostUSD = pass.cost()
}
