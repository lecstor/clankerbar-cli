// Package loop is the driver: a long-running daemon that gates each iteration on
// a cheap backlog read, spends a fresh harness session only when there is
// claimable work, and — crucially — stays alive and keeps polling when the queue
// is empty, so it reacts when questions are answered, items are promoted, or new
// work is filed. On a usage limit it pauses and polls for an early reset; on a
// transient blip it backs off and retries. All durable state lives in the backlog
// (over MCP), so a session killed mid-task is reclaimed by the next iteration.
package loop

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/delivery"
	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
	"github.com/lecstor/clankerbar-cli/internal/salvage"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

// errZeroSpendLoop marks the one give-up the budget breaker could not have
// reached: a run of attempts that each ended before the harness reported any
// usage, so nothing fed the token or cost ceiling and the retry ladder would have
// gone on indefinitely (CLA-288).
//
// It is a distinct, wrapped sentinel rather than a plain string because the three
// ways a run can end early are three different things for an operator to do next:
// a budget stop means retune the ceiling, a wall-clock stop means the window was
// too short, and this one means the harness is not running - none of which is
// legible from "the run stopped".
var errZeroSpendLoop = errors.New("zero-spend attempt loop")

// errUnclassifiedRetryLoop marks the other give-up no ceiling would have reached:
// a ladder of retries taken on a failure the adapter could not NAME, only judge
// by heuristic (harness.Adapter.IsUnclassifiedTransient).
//
// It is the sibling of errZeroSpendLoop and exists for the mirror-image reason.
// That one bounds attempts that spend NOTHING, so the budget breaker never sees
// them. This one bounds attempts that spend NORMALLY on a failure that will
// happen again: a deterministic post-usage crash satisfies the heuristic every
// time, and none of the ordinary dials stop it — max_retries and budget are off
// by default, and max_zero_spend_attempts is reset by the very usage the
// heuristic requires. Left unbounded it re-spawns a full paid session on the
// same task at the backoff ceiling, all night (CLA-381).
//
// Also distinct because the operator's next move is distinct: this one means the
// adapter has no pattern for what went wrong, so the fix is to read the
// diagnostic in the error and — if it is a real blip — teach the adapter's
// transient patterns about it.
var errUnclassifiedRetryLoop = errors.New("unclassified retry loop")

// unclassifiedRetryBound is how many retries one phase's ladder may take on
// heuristically-judged failures before it gives up.
//
// Deliberately a constant and not a config dial. The dials an operator sets are
// about how much they are willing to spend on failures the tool UNDERSTANDS;
// this bounds guesses, and the honest number of guesses is small at any budget.
// Two leaves room for a genuine one-off blip the patterns have not caught up
// with — the case the heuristic exists for — while a deterministic failure costs
// three paid sessions instead of a night's worth.
const unclassifiedRetryBound = 2

// perTaskDeadBound is how many consecutive dead phases on the SAME task park it
// (CLA-396, raised from 2 by the 2026-08-20 operator decision). A task really
// can be cursed — a brief that reliably kills every session sent at it — but at
// 2 the budget fired on ordinary bad luck: at the observed 37.5% per-session
// death rate, two consecutive deaths is a 14% roll, and CLA-390 was parked on
// exactly that and had to be un-parked by hand. Four consecutive deaths is a
// ~2% roll — a signal rather than noise — while a task that dies once per drain
// and succeeds between (the retry re-claims the same task within the drain)
// never accumulates past one here, because the counter resets when a phase of
// the task advances (see the reset below).
const perTaskDeadBound = 4

// fleetDeadBound is how many consecutive dead phases ACROSS tasks pause the
// loop for that target and raise one project-level question (CLA-396). One
// more than the per-task bound, on purpose: a single cursed task parks at 4
// (its own counter fires first, and the drain breaks before any fifth death),
// so the fleet counter can only reach 5 across DIFFERENT tasks — the shape
// only a fleet-wide fault produces. A task that dies once looks identical to
// a healthy task hit by a provider outage, and only a run of deaths spanning
// tasks distinguishes the two; 5 consecutive deaths at the observed 37.5% rate
// is a ~0.7% roll, and it stays conservative after CLA-406 removes the
// retryable class from the numerator.
const fleetDeadBound = 5

// startupFailBound is how many consecutive harness-startup failures on one
// target pause it and raise one project-level question (CLA-562). Mirrors
// fleetDeadBound on purpose: the dead-phase escalation (CLA-396) never fires
// for a session that dies at startup - that counter only sees sessions that
// started - so the startup-failure sideline path (CLA-507) was unbounded by
// design: 50 consecutive failures over 4 days on a bad reasoningEffort value,
// sidelining 2h and retrying, with nothing filed and nothing paused. Five is
// the same conservative signal as the fleet bound, and triage may move it.
const startupFailBound = 5

// Target is one backlog a Driver drives: a project's cheap poller plus where that
// project's drain sessions run (CLA-142). A single-project Driver has exactly one,
// unnamed target; a multi-project Driver has one per configured project.
type Target struct {
	// Name is the project slug, used to prefix log lines when there is more than
	// one target. Empty for the single default target.
	Name string

	// Poller is this project's cheap backlog read.
	Poller backlog.Poller

	// WorkDir is where this project's sessions run. Empty = the config's workdir.
	WorkDir string

	// MCPConfigPath points this project's sessions at its .mcp.json (the file
	// whose /mcp/<slug> URL selects the project). Empty = the config's path.
	MCPConfigPath string

	// MCPConfigPaths is this project's MCP config PER HARNESS, for a sequence
	// whose phases are not all on one harness (CLA-366). MCPConfigPath above is
	// the top-level harness's file; a phase on another harness needs the same
	// project in that harness's schema, which is a different file. Empty on every
	// single-harness run, and config.Validate refuses the combination that would
	// need an entry here and lacks one.
	MCPConfigPaths map[string]string

	// Repos is this project's repo -> local-checkout map (CLA-437, agent-rule-
	// scoping pieces 2 and 3), and PrimaryRepo the fallback for a task that
	// carries no repo. Between them they decide where each session STARTS: the
	// task's own repo resolved to a checkout, never blindly the workdir — so the
	// session reads its repo's rule files instead of a sibling's, and a grant
	// scoped per-cwd cannot wall off the tree the task needs. Resolution order,
	// the loud repo_not_found failure and the nothing-configured legacy path are
	// config.ResolveCheckout's; this field only carries what the operator
	// declared. Both empty = the feature is off and every spawn behaves exactly
	// as before.
	Repos map[string]string

	// PrimaryRepo backs a session whose task names no repo (a fresh phase before
	// the plane returns repos for everything). See Repos above.
	PrimaryRepo string

	// ProjectRoot says this target's workdir is a FLEET PROJECT ROOT (CLA-678):
	// a directory holding this project's clones and its worktrees directory,
	// with each session's checkout inside it. It is the config's own
	// `project_root` flag, carried here because the session, not the config, is
	// what the permission policy is built for. See extraDirsFor for what it
	// widens.
	ProjectRoot bool

	// Releaser hands back a claim a session left holding (CLA-242). Nil disables
	// the handback for this target, which costs only the reclaim an expiring lease
	// would have cost anyway — so a target without one degrades, it does not break.
	Releaser plane.Releaser

	// Fleet reports this project's activity to the plane (CLA-466): a presence
	// beacon on every backlog poll, state-change beacons at phase boundaries, and
	// exactly one iteration record per drain. Nil (not wired) disables reporting,
	// which costs only console visibility — reporting is telemetry and is never
	// load-bearing.
	Fleet fleet.Reporter

	// RCfg fetches this project's stored execution config (CLA-410). Nil means
	// not wired: the target runs the local file rules exactly as before, which
	// is also what the plane answers (version 0) until a document is ratified.
	RCfg plane.RunConfigReader

	// Cfg is this target's EFFECTIVE config — the driver's base config with the
	// project's stored run-config document overlaid — or nil when no document is
	// in force, in which case every read falls through to d.cfg unchanged. It
	// carries only policy the plane may say (the pipeline — prompt, phases,
	// review_panel — plus harness/model/tiers/budget/escalation/backstops);
	// wiring, env and machine paths stay local, so the overlay can never
	// repoint a session at somebody else's dialect.
	// Rebuilt between iterations when the poll's runConfigVersion moves — never
	// mid-session.
	Cfg *config.Config
}

// Driver runs the loop for one harness against one or more backlogs.
type Driver struct {
	cfg     *config.Config
	h       harness.Adapter
	targets []Target
	paused  []bool // per-target console-pause state, to log transitions once
	cursor  int    // round-robin position over targets (last drained)
	state   *statedir.Dir
	blind   bool // no cheap backlog read available — drain then idle-poll

	// now is the clock the scheduled-pause check reads, and ONLY that: the
	// back-off ladder in progress.go still reads the wall clock. Nil falls back
	// to time.Now, so a hand-built test Driver works. schedPaused is whether a
	// window is active — kept so the enter and exit are logged once, not per poll.
	now         func() time.Time
	schedPaused bool

	// echoOut is where an ENABLED session echo writes (cfg.EchoSessions true).
	// Nil = os.Stderr, the real terminal. The field exists so a test can watch
	// what the terminal would have received without capturing the process's
	// own stderr; with EchoSessions false nothing is echoed regardless.
	echoOut io.Writer

	// panelLaunch, when non-nil, is called with the launching member's reviewer
	// role at a PARALLEL panel's launch boundary: the first moment that member's
	// session launch has actually begun, immediately before the panel releases
	// its successor. It is a test seam, and it exists because the launch ORDER
	// cannot be observed soundly from inside a fake adapter: the adapter is
	// entered one call after the boundary, so a goroutine descheduled in between
	// lets a correctly ordered panel look out of order, which is exactly the
	// assertion that flaked on CI (CLA-755 review 1.review-5.1). Recording here,
	// before the release, is the order the panel guarantees.
	//
	// Nil in every production build — nothing but a test sets it — so it costs
	// one nil check and changes nothing the panel does.
	panelLaunch func(role string)

	// panelRelease, when non-nil, is called with the member's index each time a
	// PARALLEL panel member releases its successor, before that successor is
	// unblocked. panelLaunch pins the launch ORDER; this pins the release POINT,
	// which is the guarantee itself: a member must release from its launch
	// boundary (phaseRunOpts.onSessionLaunch), not from its worktree being
	// ready. At member k's launch exactly members 0..k-1 have been released, so
	// a panel that released at worktree-ready — where release_k always precedes
	// launch_k — reports k+1 releases at that launch. Deterministic, no
	// scheduling window (CLA-757).
	//
	// It is wired at the scheduler's started wrapper rather than at the panel's
	// call site, so a regression that calls started() earlier still reports the
	// release where it really happened. Nil in every production build, like
	// panelLaunch: a test seam, no behaviour change.
	panelRelease func(k int)

	// undeclared counts the sessions this run that ended still holding a task
	// whose work they had already pushed — the phase did its job and then did not
	// hand the task over (CLA-384). Nothing is lost: the branch is recorded and a
	// takeover resumes. What is lost is the money, twice, because the next clanker
	// pays to rediscover where the last one got to. Measured 2026-08-19 on v0.8.1,
	// this was 3 of 4 review phases in one evening and $31.30 of that run's spend,
	// and the only way to notice was diffing task statuses against the log by hand.
	// So it is counted and the count is said out loud, per occurrence: a run that
	// is silently repeating its own work should be visible without forensics.
	undeclared int

	// Per-target no-progress state. `claimable > 0` is a claim that work is
	// available, not that it can be DONE: a task can be claimable and still
	// unworkable — gated on an unanswered question, needing a toolchain the
	// harness may not run, blocked on something no session can resolve. The gate
	// cannot tell, so it spawns, the session correctly declines, and the operator
	// pays for the same report every cycle. One real run spent ten iterations
	// re-writing the same two questions. These back a repeatedly fruitless target
	// off instead.
	//
	// The accumulator is denominated in TOKENS, not sessions (CLA-343): the old
	// count of 3 fruitless sessions cost ~78M tokens before it first fired (~26M
	// per measured iteration), and one huge fruitless drain could repeat that
	// spend without ever climbing the count. Tokens since last progress make the
	// first sit-out land at the same ~80M while letting a single runaway drain
	// trip it on its own.
	quietTokens []int       // tokens spent on this target's drains that settled nothing
	spent       []int       // the last drain's tokens, awaiting its progress verdict
	baseline    []int       // settled count when this target's last drain began
	openQs      []int       // open-question count at this target's last poll
	pending     []bool      // a drain of this target is awaiting its progress verdict
	skipUntil   []time.Time // a backed-off target is ineligible until this time

	// Per-target harness-failure state (CLA-507). A drain that returns an ERROR
	// failed while trying to run THIS target's session - a malformed opencode.json
	// in its workdir, a missing checkout, a harness binary that will not start.
	// That failure belongs to the project, not to the run: ending the run took
	// every healthy sibling down with it on 2026-08-26 (three of four fleet
	// daemons died on one project's bad config). So the target sits out on the
	// same ladder the no-progress breaker shapes, escalating while it keeps
	// failing and rejoining once it succeeds again, while the others drain on.
	//
	// The quiet breaker cannot stand in for this: its accumulator is denominated
	// in TOKENS, and a session killed by a bad config reports ~0 of them, so a
	// target failing this way would never climb it. harnessFails counts
	// consecutive failed drains since the last success (reset only by success,
	// so a target that keeps failing escalates: an idle stretch does NOT clear
	// the sit-out (judgeProgress's idle reset preserves skipUntil while
	// harnessFails > 0), so the ladder holds until the back-off elapses or the
	// harness succeeds again); harnessErrs keeps each failure's text for the
	// log line and for the run-wide report when nothing is left to drive.
	harnessFails []int
	harnessErrs  []error

	// The resumed-phase transient streak (CLA-662), keyed by the target that
	// failed. A resumed phase's transient failure ends its sequence, and every
	// counter the dead/fleet machinery keeps is deliberately skipped on that
	// path — so without this the run re-drains AT ONCE, and when the failing
	// phase runs on a different harness from the one the queue drains on
	// (implement on opencode, review on claude — the README's own example),
	// the first-phase transient arm never sees the failure at all. The streak
	// is the rung of the backoff the run owes after the sequence's handback,
	// before polling again. Per target AND per phase harness: the failure is
	// the provider's, so a claude streak must climb while opencode stays
	// healthy, and an opencode success must not vouch for claude. Reset the
	// moment a resumed phase on the same target+harness ends any other way
	// than the transient sequence-end — only a run of consecutive transient
	// ends is what the growing wait is there to bound.
	resumedTransients map[resumedTransientKey]int

	// routeMarks is this instance's sticky memory of model routes that hit a usage
	// limit (CLA-782): route alias -> the instant the mark expires. A run moved
	// off its free route stays on the paid one for later PHASES until the mark
	// lifts, so the fleet does not flip between routes at every phase boundary;
	// it goes back to free only when the free route's reset has passed.
	//
	// Persisted in the state dir (ROUTE_MARKS) so a restart keeps them; per
	// instance, not per host (operator decision 10164e9e): a sibling clanker
	// discovers the same limit on its own next request and switches mid-session.
	// Guarded by routeMarksMu because the review panel runs its members
	// concurrently and each may record a switch.
	routeMarks   map[string]time.Time
	routeMarksMu sync.Mutex

	// resumedTransientWait computes that wait from the streak: backoff(n) in
	// production. A field so tests can observe the ladder and drive it without
	// sleeping the real 30s/60s/... in the suite — the same reason waitGrace
	// and restartPoll are fields. Nil means d.backoff.
	resumedTransientWait func(n int) time.Duration

	// The fleet dead-phase counter (CLA-396). Per-target because a multi-target
	// driver runs several projects, and "the fleet" is one project's sessions:
	// a run of dead phases on project A must not pause project B. The per-task
	// budget in drainPhases cannot see a provider failing for every task — each
	// individual task dies only once, so no per-task counter ever trips — and
	// these exist to catch exactly that: consecutive dead phases across
	// DIFFERENT tasks mean the harness or provider is broken, not the tasks.
	// The counter resets ONLY on a successful implement phase (see the reset in
	// drainPhases), because review phases run on the claude harness and cannot
	// exhibit the zero-usage death: a claude success vouching for the opencode
	// gateway would hide the very fault the detector exists to find.
	fleetDead []int // consecutive dead phases across all tasks on this target

	// fleetPaused marks a target whose fleet counter has tripped: the loop stops
	// spawning sessions for it until the operator answers the raised question —
	// a known-failing provider must stop burning sessions. Measured as the
	// open-question count falling back to what it was at the raise (fleetOpenQ),
	// the same "something was resolved" signal judgeProgress uses.
	fleetPaused []bool

	// fleetRaised records that this target's fleet question is OPEN, so a trip
	// raises exactly ONE project-level question rather than one per poll while
	// the pause is in force. Cleared when the pause clears, so a later trip —
	// a new episode — raises a fresh question.
	fleetRaised []bool

	// fleetOpenQ is the open-question count at the raise: the baseline the
	// resume signal is measured against. Captured from the last poll (d.openQs)
	// at trip time, so it does not include the question just raised; the pause
	// clears once the count falls back to it or below.
	fleetOpenQ []int

	// Per-target harness-startup-failure state (CLA-562). A startup failure is
	// a drain that returned an ERROR having reported no usage (tokens == 0):
	// no session ever ran - a bad reasoningEffort value exiting 1 at spawn, a
	// missing checkout, a harness binary that will not start - exit before any
	// usage report. A dead phase (CLA-396) is the opposite: the session ran and
	// produced nothing. The two counters are deliberately distinct and one
	// failure never ticks both: a dead phase returns err == nil (handled inside
	// drainPhases, never reaching the sideline path), and a startup failure
	// returns err != nil with tokens == 0 (never satisfying end.dead, which
	// requires a held claim). startupFails counts consecutive startup failures
	// since the last drain that spawned anything (reset on any err == nil, and
	// on any err with tokens > 0, which proves a session ran); the backoff /
	// sideline (harnessFails) stays for the first startupFailBound-1 attempts
	// and only the Nth escalates to a pause-and-raise, mirroring fleetTrip.
	startupFails []int

	// startupPaused marks a target whose startup counter has tripped: the loop
	// stops spawning for it until the operator answers the raised question -
	// measured as the open-question count falling back to startupOpenQ, the
	// same signal the fleet pause uses.
	startupPaused []bool

	// startupRaised records that this target's startup question is OPEN, so a
	// trip raises exactly ONE project-level question per episode. Cleared with
	// the pause, so a new episode raises fresh.
	startupRaised []bool

	// startupOpenQ is the open-question baseline captured at raise, before the
	// question itself raises the count.
	startupOpenQ []int

	// waitGrace is how far past a stated reset the paused loop waits before its
	// confirming probe: a reset time is a boundary, and arriving exactly on it
	// invites an off-by-a-second retry against a cap that has not lifted yet.
	// A field so tests can shrink it (CLA-305) - production always gets a
	// minute, and no test should sleep a minute to watch a boundary land.
	waitGrace time.Duration

	// limitBeaconInterval is how often a paused wait re-beacons `limited`
	// (CLA-728). The plane calls an instance dead once its last beacon is
	// INSTANCE_STALE_AFTER_MS (15 minutes) old, and a limit wait can be hours,
	// so the wait speaks at half that window — 5 minutes, since half is 7.5 and
	// the task takes the shorter of the two. A field for the same reason as
	// waitGrace: no test should sleep five minutes to watch a beacon repeat.
	limitBeaconInterval time.Duration

	// leaseRenewalInterval overrides the in-session renewal cadence, which
	// carries the CLA-771 presence beacon as well as the lease heartbeat.
	// Production leaves it zero: heartbeatInterval(planeLeaseDuration), a third
	// of the plane's 30-minute lease. A field for the same reason as
	// limitBeaconInterval — no test should wait ten minutes to watch a tick.
	leaseRenewalInterval time.Duration

	// newVerifier builds the delivery checker for a workdir (CLA-253). A field so
	// tests can substitute one; production always gets internal/delivery. The
	// second argument is CLA-310's empty-check-rollup opt-out for the target
	// being verified — resolved from config by the caller, which knows the
	// target, so the verifier itself stays project-blind.
	newVerifier func(workdir string, allowUncheckedPR bool) deliveryVerifier

	// newSalvager builds the stranded-work rescuer for a workdir (CLA-314). Same
	// shape and the same reason as newVerifier.
	newSalvager func(workdir string) workSalvager

	// probePRBase reads the origin remote of the checkout in dir to resolve a
	// repo's PR base when no pr_base override names one (CLA-608): whether
	// refs/heads/<integration> exists on origin, and origin's default branch
	// (its symbolic HEAD target). A field so tests can substitute one;
	// production always runs git. Errors are NOT failures — the caller falls
	// back to the configured integration_branch, exactly the text the briefs
	// carried before this feature — so a probe that cannot run degrades to
	// today's behaviour, never blocks a spawn.
	probePRBase func(ctx context.Context, dir, integration string) (integrationOnOrigin bool, defaultBranch string, err error)

	// originBranch reads what origin says about the task's branch, for the
	// start-phase choice. A field because tests stub the git the start-phase
	// prep does.
	originBranch func(ctx context.Context, checkout, branch, doneTip string) originBranchFacts

	// prepareWorktree prepares the task worktree every phase of a runner-claimed
	// drain runs in. A field because tests stub the git the start-phase prep does.
	prepareWorktree func(ctx context.Context, req worktreeRequest) (worktreePrep, error)

	// worktreeCheck is the READ-ONLY pre-claim verdict on the task's existing
	// worktree, made before the claim so a deterministic refusal costs no
	// reclaim. It returns the refusal reason ("" when the claim is worth
	// taking) and the origin facts its fetch produced for that task. A field
	// for the same reason as the two above.
	worktreeCheck func(ctx context.Context, req worktreeRequest, doneTip string) (string, originBranchFacts, error)

	// newAdapter resolves a harness NAME to its adapter, for a sequence whose
	// phases are not all on one harness (CLA-366). Production is harness.Get; a
	// field so tests can hand out their own fakes per name.
	//
	// It is only consulted for a phase naming some OTHER harness — a phase on the
	// run's own harness gets d.h, the adapter the caller constructed the Driver
	// with. That is what keeps every existing test (which passes a fake and never
	// names a harness on a phase) driving its fake rather than reaching for a real
	// binary through the registry.
	newAdapter func(name string) (harness.Adapter, error)

	// spentBy is this run's spend per harness name, for the per-harness breakers
	// (CLA-367). It hangs off the Driver rather than riding in `spend` because it
	// is the RUN's account: a per-harness total outlives the drain that produced
	// it, while the drain-local accumulators threaded through `spend` do not. See
	// charge and budgetTrip. Lazily built; a run that configures no per-harness
	// block still fills it, which costs one map and keeps the accounting honest if
	// a block is added mid-file.
	spentBy map[string]harnessSpend

	// stateMu serialises the run-scoped maps below. They are written by whatever
	// goroutine a session's result is processed on: with a review panel's members
	// running concurrently (CLA-697) that is more than one at a time, and a Go map
	// written from two goroutines is a data race, not a lost update. Every
	// read-modify-write of spentBy, startRefusals, deadTally and panelFailStreak
	// holds it; nothing that calls out to the plane or a harness may hold it.
	stateMu sync.Mutex

	// startRefusals counts consecutive worktree refusals per task id, so a tree
	// that refuses every claim deterministically reaches the operator instead of
	// the queue. Lazily built for a Driver built without the constructor.
	startRefusals map[string]int

	// claimFileMu serialises CURRENT_CLAIM writes (CLA-708). A review panel's
	// members run concurrently (CLA-697) and each renewer persists the same
	// claim; WriteFileAtomic uses a fixed `.tmp` name, so unsynchronised writers
	// would race there rather than merely duplicate work.
	claimFileMu sync.Mutex

	// controlMu serialises control-marker CONSUMPTION and the RELOAD action
	// (CLA-697 review finding 1.review-1.2). A parallel panel puts N members
	// inside waitOrStop's loop at once, and reading a marker then removing it is
	// not atomic: two members could both find RELOAD before either removed it
	// and both would run applyReload, swapping d.cfg and invalidating the
	// run-config overlays from two goroutines. Holding it across
	// consume-then-act means the second consumer finds the marker already gone —
	// exactly what a single-threaded loop would see.
	controlMu sync.Mutex

	// deadTally is this run's dead-phase tally, keyed by phase label and harness
	// name (CLA-402): how many phase sessions ran and how many of those died
	// producing nothing. Like spentBy it is the RUN's account — the rate is
	// measured over a run, not a drain — so it hangs off the Driver and is
	// reported from Run. Lazily built; see tallyDead and logDeadTally.
	deadTally map[tallyKey]*phaseTally

	// panelFailStreak is how many consecutive TASKS each review-panel member has
	// recorded nothing on (CLA-639 M4), keyed by the member's log label (the
	// positional `review-N` when unnamed). Like deadTally it is the run's
	// account: one flaky member is a blip, but the same member failing task
	// after task is a fault in that member's harness, model or brief, and the
	// warning it earns must not depend on the drain boundary. Cleared for a
	// member the moment that member records. See panelMemberFailed /
	// panelMemberRecorded.
	panelFailStreak map[string]int

	// restartRequested (CLA-461) flips when a RESTART / RESTART_NOW marker has
	// been consumed and the run is unwinding toward a re-exec. Atomic because
	// the RESTART_NOW watcher goroutine writes it while the loop reads it after
	// Run returns.
	restartRequested atomic.Bool

	// expireUnsupportedOnce says once, not once per abandoned phase, that the
	// plane does not serve this build's task tools at all — it is older than the
	// protocol this release requires (CLA-707/708 predate that check). Every
	// such handback falls back to leaving the lease to expire, which is the old
	// behaviour, so repeating the line adds nothing.
	expireUnsupportedOnce sync.Once

	// beaconWriteFailed is the streak latch for the LOCAL beacon write
	// (loop/fleet.go): the first consecutive failure is logged, further ones
	// stay silent, and recovery is logged once. Atomic because the claim
	// reflector (CLA-510) beacons from the lease-renewer goroutine.
	beaconWriteFailed atomic.Bool

	// reloadConfig rebuilds the config for a RELOAD marker (CLA-461). Production
	// wires Load+ApplyFlagOverrides+Validate via SetReloader; nil falls back to
	// Load(Source())+Validate. See control.go.
	reloadConfig func() (*config.Config, error)

	// restartPoll is how often the RESTART_NOW watcher polls the state dir. A
	// field so tests can shrink it; production always gets restartCheckInterval.
	restartPoll time.Duration

	// iter is one target's mid-drain presence state (CLA-466), written by
	// beginIteration / endIteration around each drain and read by fleetState when
	// a poll beacons. Guarded by iterMu: since CLA-510 the lease renewer's
	// goroutine also revises the ref mid-session (reflectClaim), so these are no
	// longer touched by the loop alone.
	iter   []iterState
	iterMu sync.Mutex

	// hostname/hostOnce cache os.Hostname for the fleet beacon identity.
	hostname string
	hostOnce sync.Once

	// Per-target run-config state (CLA-410). rcVersions holds the version each
	// target's effective config was built from, -1 until the first fetch; the
	// poll's runConfigVersion differing from it is the re-read signal, honoured
	// exactly where STOP/RELOAD are: the ITERATION boundary (the poll only
	// happens there), never mid-session. rcAttempt/rcAttemptVer rate-limit a
	// FAILED refetch of one version to one try per backoff window (a new version
	// always fetches), so a blip at ratify time retries without spamming.
	// rcFails counts consecutive transient failures for rcAttemptVer, so the
	// window widens exponentially (CLA-481) instead of fixed 30s spam; a
	// permanent latch (ErrNoConfig/ErrNotWired) or any success resets it.
	rcVersions   []int
	rcAttempt    []time.Time
	rcAttemptVer []int
	rcFails      []int

	// tgtTokens / tgtCost accumulate each target's own spend for the per-target
	// budget breaker: a stored config's budget must bite on ITS project's spend,
	// not on a run total shared with every other target. The base budget keeps
	// its existing run-wide duty unchanged; this breaker exists only where an
	// overlay is actually in force.
	tgtTokens []int
	tgtCost   []float64

	// overrides are the run's flag overrides, re-applied after every overlay so
	// an explicit --harness/--model still wins over a stored document. cli.Run
	// sets them via SetOverrides; the zero value applies nothing.
	overrides config.Overrides
}

// resumedTransientKey names the target a resumed phase's transient failure is
// charged to (CLA-662): the project (its index in targets) and the harness the
// failed phase runs on. The harness is part of the key because the failure is
// the provider's, not the project's — the per-phase shape (implement on
// opencode, review on claude) drains a healthy opencode while claude blips,
// and a project-keyed streak would let an opencode success reset it.
type resumedTransientKey struct {
	target  int
	harness string
}

// deliveryVerifier is the driver's view of internal/delivery, narrowed to the one
// call it makes.
type deliveryVerifier interface {
	Verify(ctx context.Context, c delivery.Claim) delivery.Report
}

// workSalvager is the driver's view of internal/salvage, narrowed the same way.
type workSalvager interface {
	// Salvage rescues the work a dead session left in a worktree. recorded are
	// the branches the session reported for the task, which is what "the session
	// delivered" is judged against (CLA-695).
	Salvage(ctx context.Context, taskID, label string, recorded ...string) salvage.Outcome
}

// New builds a single-project Driver — the original mode, driven entirely by the
// top-level config fields.
func New(cfg *config.Config, h harness.Adapter, poller backlog.Poller) *Driver {
	return NewMulti(cfg, h, []Target{{Poller: poller}})
}

// NewMulti builds a Driver over several targets (CLA-142): one loop instance, one
// account key, many project queues — round-robin over whichever have claimable work.
func NewMulti(cfg *config.Config, h harness.Adapter, targets []Target) *Driver {
	n := len(targets)
	d := &Driver{
		cfg: cfg, h: h, targets: targets,
		paused:              make([]bool, n),
		quietTokens:         make([]int, n),
		spent:               make([]int, n),
		baseline:            make([]int, n),
		openQs:              make([]int, n),
		pending:             make([]bool, n),
		skipUntil:           make([]time.Time, n),
		harnessFails:        make([]int, n),
		harnessErrs:         make([]error, n),
		fleetDead:           make([]int, n),
		fleetPaused:         make([]bool, n),
		fleetRaised:         make([]bool, n),
		fleetOpenQ:          make([]int, n),
		startupFails:        make([]int, n),
		startupPaused:       make([]bool, n),
		startupRaised:       make([]bool, n),
		startupOpenQ:        make([]int, n),
		iter:                make([]iterState, n),
		rcVersions:          make([]int, n),
		rcAttempt:           make([]time.Time, n),
		rcAttemptVer:        make([]int, n),
		rcFails:             make([]int, n),
		tgtTokens:           make([]int, n),
		tgtCost:             make([]float64, n),
		waitGrace:           time.Minute,
		limitBeaconInterval: 5 * time.Minute,
		startRefusals:       make(map[string]int),
		newVerifier: func(workdir string, allowUncheckedPR bool) deliveryVerifier {
			v := delivery.New(workdir, "")
			if allowUncheckedPR {
				v.AllowUncheckedPR()
			}
			return v
		},
		newSalvager: func(workdir string) workSalvager { return salvage.New(workdir, "") },
		newAdapter:  harness.Get,
		probePRBase: ProbeOriginPRBase,

		originBranch:    readOriginBranch,
		prepareWorktree: prepareTaskWorktree,
		worktreeCheck:   claimWorktreeRefusal,
	}
	// The clock the scheduled pause reads; a test moves it to enter a window.
	d.now = time.Now
	for i := range d.rcVersions {
		d.rcVersions[i] = -1 // unfetched; 0 is a REAL answer (nothing stored)
	}
	return d
}

// adapterFor resolves the harness that runs one phase.
//
// The run's own harness resolves to d.h without consulting the registry, which
// is both cheaper and the thing that keeps a Driver built with a substituted
// adapter honest: a phase that names nothing, or names the configured harness,
// runs on exactly the adapter the caller handed over.
//
// An unresolvable name is an error rather than a fallback to d.h. config.Validate
// has already refused an unregistered harness, so reaching here means the
// registry and the config disagree, and quietly running the phase on the other
// harness would be the wrong half of a mixed sequence — the review phase spawning
// on the implement harness, with nothing in the log to say so.
func (d *Driver) adapterFor(ph config.Phase) (harness.Adapter, error) {
	name := ph.Harness
	if name == "" || name == d.cfg.Harness {
		return d.h, nil
	}
	return d.newAdapter(name)
}

// originBranchFn is the origin read, falling back to the default when a Driver
// was built without the constructor (a test's &Driver{}).
func (d *Driver) originBranchFn() func(ctx context.Context, checkout, branch, doneTip string) originBranchFacts {
	if d.originBranch != nil {
		return d.originBranch
	}
	return readOriginBranch
}

// prepareWorktreeFn is prepareTaskWorktree, with the same nil fallback.
func (d *Driver) prepareWorktreeFn() func(ctx context.Context, req worktreeRequest) (worktreePrep, error) {
	if d.prepareWorktree != nil {
		return d.prepareWorktree
	}
	return prepareTaskWorktree
}

// worktreeCheckFn is claimWorktreeRefusal, with the same nil fallback.
func (d *Driver) worktreeCheckFn() func(ctx context.Context, req worktreeRequest, doneTip string) (string, originBranchFacts, error) {
	if d.worktreeCheck != nil {
		return d.worktreeCheck
	}
	return claimWorktreeRefusal
}

// orRef was folded into sessionDir's log line; kept out to avoid a helper with
// one caller and one branch.

// prefix returns the per-target log prefix ("[slug] "), or "" when the instance
// drives a single unnamed target — so single-project logs read exactly as before.
func (d *Driver) prefix(i int) string {
	return labelOf(d.targets[i])
}

func labelOf(t Target) string {
	if t.Name == "" {
		return ""
	}
	return "[" + t.Name + "] "
}
