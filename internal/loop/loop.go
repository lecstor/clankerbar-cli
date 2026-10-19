// Package loop is the driver: a long-running daemon that gates each iteration on
// a cheap backlog read, spends a fresh harness session only when there is
// claimable work, and — crucially — stays alive and keeps polling when the queue
// is empty, so it reacts when questions are answered, items are promoted, or new
// work is filed. On a usage limit it pauses and polls for an early reset; on a
// transient blip it backs off and retries. All durable state lives in the backlog
// (over MCP), so a session killed mid-task is reclaimed by the next iteration.
package loop

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

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
	// carries only policy the plane may say (harness/model/tiers/budget/
	// escalation/backstops); phases, wiring, env and machine paths stay local,
	// so the overlay can never repoint a session at somebody else's dialect.
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
		paused:        make([]bool, n),
		quietTokens:   make([]int, n),
		spent:         make([]int, n),
		baseline:      make([]int, n),
		openQs:        make([]int, n),
		pending:       make([]bool, n),
		skipUntil:     make([]time.Time, n),
		harnessFails:  make([]int, n),
		harnessErrs:   make([]error, n),
		fleetDead:     make([]int, n),
		fleetPaused:   make([]bool, n),
		fleetRaised:   make([]bool, n),
		fleetOpenQ:    make([]int, n),
		startupFails:  make([]int, n),
		startupPaused: make([]bool, n),
		startupRaised: make([]bool, n),
		startupOpenQ:  make([]int, n),
		iter:          make([]iterState, n),
		rcVersions:    make([]int, n),
		rcAttempt:     make([]time.Time, n),
		rcAttemptVer:  make([]int, n),
		rcFails:       make([]int, n),
		tgtTokens:     make([]int, n),
		tgtCost:       make([]float64, n),
		waitGrace:     time.Minute,
		newVerifier: func(workdir string, allowUncheckedPR bool) deliveryVerifier {
			v := delivery.New(workdir, "")
			if allowUncheckedPR {
				v.AllowUncheckedPR()
			}
			return v
		},
		newSalvager: func(workdir string) workSalvager { return salvage.New(workdir, "") },
		newAdapter:  harness.Get,
		probePRBase: probeOriginPRBase,
	}
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

// Run drives the daemon until STOP/HALT, a ceiling (max-iterations / budget), or
// context cancellation. An empty queue is NOT a stop condition — it idles and
// keeps polling. Returns nil on a graceful stop; an error only on an unexpected,
// non-retryable failure.
//
// Two things hold on every way out (CLA-491): a STOP marker this run never
// consumed is removed rather than left where the next start would read it as a
// fresh stop request, and the last log line names why the run ended.
func (d *Driver) Run(ctx context.Context) (runErr error) {
	// A caller may have opened the state dir already (tests, and any future entry
	// point that wants to fail before spawning anything). Opening it here is the
	// normal path.
	if d.state == nil {
		dir, err := d.cfg.ResolveStateDir()
		if err != nil {
			return err
		}
		st, err := statedir.Open(dir, d.cfg.SessionWorkDirs()...)
		if err != nil {
			return err
		}
		d.state = st
		defer func() {
			_ = st.Close()
			d.state = nil
		}()
	}
	// Belt and braces on the way out (CLA-491). Every shutdown path that does not
	// pass a STOP read (a non-retryable phase failure, a budget or zero-spend
	// trip, max-iterations, HALT, a cancelled context) leaves a
	// pending STOP marker where it fell, and the next start used to read it as
	// fresh and stop itself seconds into its first cycle: the 2026-08-25 fleet
	// soft-stop left one behind clanker1, and the upgraded replacement DOA'd on
	// it. Registered here rather than at each return so a future shutdown path
	// cannot forget it. (The closure, not `defer d.sweepPendingStop(runErr)`:
	// arguments evaluate at registration, and runErr is not assigned yet.)
	defer func() { d.sweepPendingStop(runErr) }()
	if src := d.cfg.Source(); src != "" {
		log.Printf("config: %s", src)
	}
	// Recomputed every iteration below, so a RELOAD that edits the interval is
	// seen from the next idle poll on. This first value is only what the banner
	// announces.
	idle := d.cfg.IdlePollInterval.OrDefault(60 * time.Second)
	// Every harness this run will SPAWN, not just the configured one: a mixed
	// sequence's banner naming one of them is a log that disagrees with the
	// sessions underneath it from the first line (CLA-366). SpawnedHarnesses
	// rather than PhaseHarnesses, so a run-wide harness every phase overrides is
	// not announced as being driven when nothing will ever run on it - the banner
	// is the operator's first line of output, and it should describe the run.
	log.Printf("driving %s; state in %s; idle poll every %s", strings.Join(d.cfg.SpawnedHarnesses(), "+"), d.state.Path(), idle)
	// Say it once, loudly, if the pre-CLA-259 directory is still sitting in the
	// workdir: an operator who touches STOP there would otherwise watch the loop
	// ignore it and conclude the stop switch is broken.
	if legacy := d.cfg.LegacyStateDir(); legacy != "" {
		log.Printf("note: %s is left over from before the state dir moved out of the workdir — markers there are IGNORED, and its old transcripts are still world-readable inside your repo; move or delete it", legacy)
	}
	// A STOP marker already sitting here PREDATES this process: markers are only
	// ever read and consumed by a running daemon (the cycle top below, and
	// waitOrStop), so one found at start-up was left behind by a previous run
	// that exited without consuming it (CLA-491). Its intent died with that run;
	// acting on it here would stop this daemon before it did anything: exactly
	// what the upgraded clanker1 did seconds after its start banner. Consume it
	// with a loud note instead. A stop dropped AFTER this point is fresh and
	// still lands on every read below.
	if present, msg := d.readMarker("STOP"); present {
		_ = d.state.Remove("STOP")
		note := ""
		if msg != "" {
			note = ": " + msg
		}
		log.Printf("warning: STOP marker%s is left over from a previous run - consumed WITHOUT stopping this one; drop a fresh marker to stop THIS run", note)
	}

	start := time.Now()
	var totalTokens int
	var totalCost float64
	drains := 0

	// CLA-461: everything below runs on a context this Run owns, so a RESTART_NOW
	// marker can cancel it mid-session and ride the exact kill/cleanup/release
	// machinery a Ctrl-C rides. Cancelling the caller's ctx directly is not
	// available to us; deriving one is.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	d.watchRestartNow(runCtx, cancelRun)

	// CLA-466: whatever ends the run — STOP/HALT, a restart marker,
	// max-iterations, budget, signal or error — every target's final `stopping`
	// beacon goes out first. After it, console silence means the daemon is gone.
	defer d.fleetShutdown()

	for {
		if runCtx.Err() != nil {
			log.Print("cancelled — stopping")
			return nil
		}
		if present, msg := d.readMarker("HALT"); present {
			log.Printf("HALT present: %s — resolve and delete %s to resume", msg, filepath.Join(d.state.Path(), "HALT"))
			return nil
		}
		if present, _ := d.readMarker("STOP"); present {
			_ = d.state.Remove("STOP")
			log.Print("STOP requested — stopping")
			return nil
		}
		// The restart/reload markers (CLA-461), honoured exactly where STOP is:
		// the ITERATION boundary. Never a mid-drain phase seam — the run holds
		// the task there. Reaching this point means the previous drain has fully
		// unwound and handed its claim back, so a graceful restart holds no claim
		// across the boundary by construction (resume is the plane's job anyway).
		// A RESTART_NOW consumed here found no session in flight and acts as a
		// plain restart; when one IS in flight the watcher in watchRestartNow has
		// already cancelled the drain and this block is never reached.
		if present, msg := d.readMarker(MarkerRestart); present {
			_ = d.state.Remove(MarkerRestart)
			if msg != "" {
				log.Printf("RESTART requested: %s", msg)
			} else {
				log.Print("RESTART requested")
			}
			log.Print("re-executing at the iteration boundary")
			d.restartRequested.Store(true)
			return nil
		}
		if present, msg := d.readMarker(MarkerRestartNow); present {
			_ = d.state.Remove(MarkerRestartNow)
			if msg != "" {
				log.Printf("RESTART_NOW requested: %s", msg)
			} else {
				log.Print("RESTART_NOW requested")
			}
			log.Print("nothing in flight here — re-executing immediately")
			d.restartRequested.Store(true)
			return nil
		}
		if present, _ := d.readMarker(MarkerReload); present {
			_ = d.state.Remove(MarkerReload)
			log.Print("RELOAD requested — re-reading the config file at the iteration boundary")
			d.applyReload()
		}
		idle := d.cfg.IdlePollInterval.OrDefault(60 * time.Second)
		if d.cfg.MaxIterations > 0 && drains >= d.cfg.MaxIterations {
			log.Printf("reached max-iterations (%d) — stopping", d.cfg.MaxIterations)
			return nil
		}
		if dim := d.budgetTrip(totalTokens, totalCost, time.Since(start)); dim != "" {
			log.Printf("budget reached: %s — stopping to leave headroom (tokens=%d cost=$%.2f elapsed=%s)",
				dim, totalTokens, totalCost, time.Since(start).Round(time.Second))
			return nil
		}

		// Gate on cheap control-plane reads: only spend a session when some target
		// has work to spawn for — ready work, or abandoned work the plane would
		// offer for takeover (Summary.Spawnable, CLA-274). When idle, keep polling
		// (and logging) so the loop reacts to answered questions / promotions /
		// newly filed work. With several targets (CLA-142) every queue is polled
		// each cycle, and the drain goes to the next spawnable one after the last
		// drained (round-robin) — a busy queue can't starve a quiet one.
		var target Target
		if !d.blind {
			candidates := make([]bool, len(d.targets))
			anyCandidate := false
			for i := range d.targets {
				t := d.targets[i]
				sum, err := t.Poller.Poll(runCtx)
				// CLA-466: the presence beacon rides THIS poll — no new cadence.
				// It fires whatever the poll answered (even an error poll proves
				// the daemon is alive and looping), and carries the state derived
				// below, so a console-paused target reads `draining`, not `idle`.
				d.beacon(i, t, d.fleetState(i))
				switch {
				case errors.Is(err, backlog.ErrNotWired):
					log.Print("backlog polling not wired — blind mode: drain, then idle-poll by re-draining (wire backlog polling to gate on live counts cheaply)")
					d.blind = true
				case errors.Is(err, backlog.ErrUnauthorized):
					// A 401/403 means CLANKERBAR_API_KEY is bad — and the harness sessions
					// we'd spawn carry the SAME key, so blind-draining or idle-retrying just
					// burns dead work against a credential that won't self-heal. Hard-stop
					// LOUDLY with a non-nil error (non-zero exit — distinct from the graceful
					// nil stop STOP/budget return) so the operator fixes the key (CLA-132).
					log.Printf("%sbacklog auth failed (401/403) — check CLANKERBAR_API_KEY; stopping", d.prefix(i))
					return fmt.Errorf("backlog auth failed (401/403) — check CLANKERBAR_API_KEY: %w", err)
				case errors.Is(err, backlog.ErrProjectRequired):
					// A 400 project_required means the poll hit the LEGACY slug-less summary
					// route with an ACCOUNT-scoped key, which that route cannot bind to a
					// project. The remedy is a project selector, NOT abandoning the account
					// key (decision 2026-07-29): give the loop a slug to name in the path —
					// a `projects` list in the config, or an .mcp.json whose clankerbar URL
					// is /mcp/<slug> — or, for CI-style setups only, a project-scoped key.
					// Still a hard stop: the mismatch won't self-heal, and idle-retrying or
					// blind-draining would just burn doomed sessions (CLA-133/CLA-142).
					log.Printf("%sbacklog poll: the summary route needs a project selector — add a `projects` list to the config, or point the loop at an .mcp.json naming /mcp/<slug>; stopping", d.prefix(i))
					return fmt.Errorf("backlog poll needs a project selector (configure `projects` or an /mcp/<slug> .mcp.json; a project-scoped key also works for CI): %w", err)
				case err != nil:
					log.Printf("%sbacklog poll error: %v — retry in %s", d.prefix(i), err, idle)
				default:
					// The stored execution-config boundary (CLA-410): a moved
					// runConfigVersion — including the first poll, which is
					// "start of run" — refetches and re-overlays BEFORE the
					// spawn gate reads anything, so the drain that follows this
					// poll runs on the current document. Mid-session is
					// unreachable by construction: polls happen only here.
					d.checkRunConfigVersion(runCtx, i, &d.targets[i], sum.RunConfigVersion)
					// stale_claimable is reported beside claimable, not folded into it: an
					// operator watching the console has to be able to tell "spawning
					// because there is fresh work" from "spawning to recover an abandoned
					// branch", and before CLA-274 those were indistinguishable — the
					// second did not happen at all, and now that it does it must not look
					// like the first.
					log.Printf("%squeue: ready=%d claimable=%d stale_claimable=%d in_progress=%d open_questions=%d paused=%t (v%d)",
						d.prefix(i), sum.Ready, sum.Claimable, sum.StaleClaimable, sum.InProgress, sum.OpenQuestions, sum.Paused, sum.Version)
					// Console pause (CLA-76 plane / CLA-130 driver): the operator can pause
					// a run from the web console, PER PROJECT. Honour it BEFORE the spawn
					// gate so a paused project never gets a new session even when it has
					// work to spawn for — including a recovery session for abandoned work,
					// which a pause outranks like any other. Other projects keep draining.
					// Distinct from
					// STOP (which exits the instance) and from an empty queue. A pause
					// landing mid-drain is only seen here, between iterations, so it never
					// kills an in-flight session (exactly like STOP).
					if sum.Paused {
						if !d.paused[i] {
							d.paused[i] = true
							log.Printf("%sconsole pause active — not spawning new sessions; idle-polling until resumed", d.prefix(i))
						}
					} else {
						if d.paused[i] {
							d.paused[i] = false
							log.Printf("%sconsole pause cleared — resuming", d.prefix(i))
						}
						d.judgeProgress(i, sum)
						// CLA-396: a target the fleet counter paused stays
						// paused until the operator answers the raised question
						// — the open-question count falls back to what it was at
						// the raise (the same "something was resolved" signal
						// judgeProgress uses for the quiet back-off).
						if d.fleetPaused[i] && sum.OpenQuestions <= d.fleetOpenQ[i] {
							d.fleetPaused[i] = false
							d.fleetRaised[i] = false
							log.Printf("%sfleet pause cleared — the operator answered the dead-phase question; resuming", d.prefix(i))
						}
						// CLA-562: the same resume signal for the startup-failure
						// pause - answering resumes, and the next startup failure
						// re-arms (the counter stays at the bound, so the next
						// startup failure trips again and raises fresh).
						if d.startupPaused[i] && sum.OpenQuestions <= d.startupOpenQ[i] {
							d.startupPaused[i] = false
							d.startupRaised[i] = false
							log.Printf("%sstartup-failure pause cleared - the operator answered the harness question; resuming", d.prefix(i))
						}
						if sum.Spawnable() && !d.backedOff(i) && !d.fleetPaused[i] && !d.startupPaused[i] {
							candidates[i] = true
							anyCandidate = true
						}
					}
				}
				if d.blind {
					break
				}
			}
			if !d.blind {
				if !anyCandidate {
					if d.waitOrStop(runCtx, idle) {
						return nil
					}
					continue
				}
				for i := 1; i <= len(d.targets); i++ {
					idx := (d.cursor + i) % len(d.targets)
					if candidates[idx] {
						d.cursor = idx
						break
					}
				}
				target = d.targets[d.cursor]
			}
		}
		if d.blind {
			// Blind mode has no counts to route on - rotate across targets so every
			// queue still gets sessions. A target sitting out a harness failure
			// (CLA-507) is skipped here exactly as the wired gate skips it; the only
			// way skipUntil can be set in blind mode is that failure path, since
			// judgeProgress never runs without polls. Walking a full lap without
			// finding a drivable target means nothing is left, and that exit is the
			// honest one. The lap reads from the PRE-pass cursor: mutating d.cursor
			// per step would make later steps orbit the mutated value instead of
			// walking the ring.
			found := false
			orig := d.cursor
			for i := 1; i <= len(d.targets); i++ {
				idx := (orig + i) % len(d.targets)
				d.cursor = idx
				if !d.backedOff(idx) {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("every project's harness is failing - nothing left to drive, stopping:%s", d.sidelinedReport())
			}
			target = d.targets[d.cursor]
		}

		// There is work (or we're blind) — spend a session (or a phase sequence of
		// them, one task either way), retrying transient blips with backoff (a
		// fresh session reclaims any half-done task).
		drains++
		// The next poll of this target judges whether the drain settled anything.
		d.pending[d.cursor] = true
		tokens, cost, handoffs, stop, err := d.drainPhases(runCtx, drains, d.cursor, target,
			spend{start: start, tokens: totalTokens, cost: totalCost})
		// Count the spend BEFORE deciding what to do with the outcome: a drain that
		// stopped or failed still burned what it burned, and the accumulator is what
		// the next iteration's breaker and log line are measured against. Handoff
		// respawns are charged here too (CLA-352): unlike phase sessions, which the
		// operator priced in when they configured `phases`, each respawn is a
		// session the SESSION chose to add, so it consumes an iteration of the
		// operator's ceiling rather than extending the run for free.
		totalTokens += tokens
		totalCost += cost
		// The per-target ledger (CLA-410): where a stored budget is in force for
		// THIS target, its ceilings are judged on its own spend, not on a total
		// every target feeds. Checked here — the same between-iterations moment
		// the run-wide breaker reads — and only when an overlay exists, so an
		// un-overlaid target's behaviour is untouched.
		d.tgtTokens[d.cursor] += tokens
		d.tgtCost[d.cursor] += cost
		if eff := d.targets[d.cursor].Cfg; eff != nil {
			if dim := eff.Budget.ExceededBy(d.tgtTokens[d.cursor], d.tgtCost[d.cursor], time.Since(start)); dim != "" {
				log.Printf("%sbudget reached: %s — stopping to leave headroom (target tokens=%d cost=$%.2f)",
					d.prefix(d.cursor), dim, d.tgtTokens[d.cursor], d.tgtCost[d.cursor])
				return nil
			}
		}
		// Hand the drain's tokens to the verdict that will judge it: a fruitless
		// drain is charged to the token-denominated quiet accumulator (CLA-343), and
		// the figure must be this drain's, not whatever total happened to be in
		// scope when judgeProgress runs.
		d.spent[d.cursor] = tokens
		// Handoff respawns consume iterations of the operator's ceiling too
		// (CLA-352) — see the comment above the accumulator.
		drains += handoffs
		// CLA-402: report the run's dead-phase tally with its denominator, so
		// the operator watching the daemon log sees the rate the run is
		// producing (the last line before the run ends IS the run total).
		d.logDeadTally()
		if err != nil {
			// Classify before acting (CLA-507). The run-wide causes are the ones
			// the poll loop already hard-stops on - a bad key or a missing project
			// selector is shared by every target, so no target can make progress
			// and sidelining would just defer the same exit. They never originate
			// in a drain today; the guard keeps that boundary explicit rather than
			// incidental. Context cancellation arrives as stop=true with err=nil,
			// so it needs no classification here.
			if errors.Is(err, backlog.ErrUnauthorized) || errors.Is(err, backlog.ErrProjectRequired) {
				return err
			}
			// Everything else out of drainPhases happened while trying to run
			// THIS target's session, so it sidelines THIS target: it sits out on
			// the escalating ladder while every other project drains on, and
			// self-heals once its harness succeeds again.
			d.sidelineTarget(d.cursor, drains, err)
			// CLA-562: the startup-failure escalation rides on top of the
			// sideline, mirroring the fleet trip. A startup failure is an ERROR
			// with no usage reported (no session ever ran); a dead phase is
			// err == nil, so one failure never ticks both counters. The sideline
			// stays for the first startupFailBound-1 attempts; only the Nth
			// escalates to a pause-and-raise. An error WITH usage proves a
			// session ran, so it breaks startup consecutiveness without tripping.
			if tokens == 0 {
				d.startupFails[d.cursor]++
				if d.startupFails[d.cursor] >= startupFailBound {
					d.startupTrip(runCtx, target, d.cursor, drains, err)
				}
			} else {
				d.startupFails[d.cursor] = 0
			}
			if d.allSidelined() {
				return fmt.Errorf("every project's harness is failing - nothing left to drive, stopping:%s", d.sidelinedReport())
			}
		} else {
			if d.harnessFails[d.cursor] > 0 {
				log.Printf("%sthe harness succeeded again - rejoining the rotation", d.prefix(d.cursor))
				d.harnessFails[d.cursor] = 0
				d.harnessErrs[d.cursor] = nil
			}
			// CLA-562: any drain that spawned (err == nil) proves the harness
			// starts, so it resets startup consecutiveness — including a drain
			// that ended in a dead-phase park or fleet trip, which still ran
			// sessions. The fleet counter is untouched here; its reset lives in
			// drainPhases (implement success only).
			d.startupFails[d.cursor] = 0
		}
		if stop {
			return nil
		}

		// CLA-564: every task boundary is a stop boundary, not just idle waits.
		// The cycle top reads these before the next poll, and this repeats the
		// read after the drain has fully unwound so the stop lands here —
		// before the next poll's side effects and the next spawn — rather than
		// depending on a single read a future fast path could bypass. A
		// file-exists read, never a wait, so the wired "loop straight back"
		// path below stays fast. HALT stops without being consumed (the
		// operator deletes it to resume); STOP is consumed on stop so a
		// restart is not stillborn.
		//
		// A cancelled context outranks both markers here exactly as at the
		// cycle top: falling through lets the top report the cancel, and the
		// exit sweep clears any pending STOP (CLA-491).
		if runCtx.Err() == nil {
			if present, msg := d.readMarker("HALT"); present {
				log.Printf("HALT present: %s — resolve and delete %s to resume", msg, filepath.Join(d.state.Path(), "HALT"))
				return nil
			}
			if present, _ := d.readMarker("STOP"); present {
				_ = d.state.Remove("STOP")
				log.Print("STOP requested — stopping")
				return nil
			}
		}

		// Blind mode has no counts to gate on, so it idles between sessions rather
		// than spinning. Note this is a PER-TASK pause now: the default prompt asks
		// for one task, so a blind run alternates task / idle / task, where the old
		// drain default spent one idle on the whole queue. An operator tuning
		// `idle_poll_interval` on a blind run is setting the gap between tasks.
		// In wired mode, loop straight back — the next cheap poll decides whether
		// more work appeared.
		if d.blind {
			log.Printf("idle — re-checking in %s", idle)
			if d.waitOrStop(runCtx, idle) {
				return nil
			}
		}
	}
}

// sweepPendingStop is Run's exit sweep (CLA-491): it removes a STOP marker the
// run never consumed, and logs the reason the run ended.
//
// A pending marker at exit means the operator dropped a soft stop against a LIVE
// daemon that then exited through a path with no STOP read (a non-retryable
// phase failure, a budget or zero-spend trip inside the drain, HALT beside it,
// a signal). The marker's intent is satisfied by ANY exit: the daemon is down
// either way, but leaving it would make the next start read a ghost and stop
// itself before doing anything. runErr carries how this run ended: an error
// return names it in the closing line (and so does every graceful path's own
// log line above), so the daemon log always says why.
func (d *Driver) sweepPendingStop(runErr error) {
	if d.state == nil {
		return
	}
	present, _ := d.readMarker("STOP")
	if !present {
		if runErr != nil {
			log.Printf("run ended: %v", runErr)
		}
		return
	}
	_ = d.state.Remove("STOP")
	why := "stopped for another reason"
	if runErr != nil {
		why = runErr.Error()
	}
	log.Printf("run ended (%s) without consuming the STOP marker - removed it so the next start is not stopped by it", why)
}

// spend is what the run had already consumed when this drain began, plus when it
// began — the budget breaker's whole view, handed down from Run.
//
// It is handed down because the ceilings are CUMULATIVE across the run while a
// drain only sees its own attempts, and because a drain can wait for hours
// (supervised wait, transient backoff) without ever returning to the breaker
// between drains. Measuring a mid-drain decision against this drain's spend alone
// would let a run walk past a ceiling it had already reached.
type spend struct {
	start  time.Time
	tokens int
	cost   float64
}

// harnessSpend is one harness's accumulated spend across a run, for the
// per-harness breakers (config.Budget.PerHarness, CLA-367).
type harnessSpend struct {
	tokens int
	cost   float64
}

// charge books a session's spend against the harness that ran it, in the ledger
// the per-harness breakers read.
//
// It is charged at the two places a session's spend enters a run — a drain
// attempt's result and a supervised wait's probe — which are the same two places
// the run-wide accumulators are added to, so the two accounts cannot drift. The
// run-wide ones stay exactly where they were, threaded through the drain as
// locals: the global dials have to keep behaving byte for byte as they always
// have, so this ledger is additive rather than a replacement for them.
//
// An UNTRUSTED session is charged to neither, for the reason endUntrustedDrain
// gives: a figure parsed out of a stream with a hole in it is not a measurement.
//
// The key is the phase's harness NAME AS THE CONFIG SPELLS IT (HarnessFor), never
// the adapter's own Name(). The two agree in production - an adapter is fetched
// from the registry by that same key - but only one of them is the currency the
// breaker spends: budgetTrip looks these keys up in Budget.PerHarness, which the
// operator keyed by config name. An adapter whose Name() diverged would silently
// disable its own per-harness ceiling rather than failing, so the ledger asks the
// config what a session IS and asks the adapter only behavioural questions.
//
// A run's phases can each name their own harness (CLA-366), so the caller charges
// the PHASE's harness, not the run's: on a mixed sequence the two call sites split
// the run's spend across one entry per harness, and on a single-harness run they
// both land on the same key and it tracks the run total exactly as before. That is
// the whole reason the ledger is keyed by name — passing d.h.Name() from a phase
// running on some other harness would bill opencode's tokens to claude's breaker,
// which is precisely the mis-accounting the per-harness ceilings exist to prevent.
func (d *Driver) charge(harnessName string, tokens int, cost float64) {
	if d.spentBy == nil {
		d.spentBy = make(map[string]harnessSpend)
	}
	s := d.spentBy[harnessName]
	s.tokens += tokens
	s.cost += cost
	d.spentBy[harnessName] = s
}

// budgetTrip names the ceiling that has stopped this run, or "" if none has: the
// run-wide dials first, measured against the run totals the caller threads down,
// then each harness's own block against that harness's own spend.
//
// Any trip stops the whole run, so the order only decides which ceiling gets
// named in the log, and the global dials are named first because a config that
// sets both wants the run-wide promise reported as the one that was broken.
// Harnesses are consulted in name order so a run with two blocks reports the same
// dial every time rather than whichever the map yielded first.
func (d *Driver) budgetTrip(tokens int, cost float64, elapsed time.Duration) string {
	if dim := d.cfg.Budget.ExceededBy(tokens, cost, elapsed); dim != "" {
		return dim
	}
	for _, name := range slices.Sorted(maps.Keys(d.spentBy)) {
		s := d.spentBy[name]
		if dim := d.cfg.Budget.ExceededByHarness(name, s.tokens, s.cost); dim != "" {
			// That harness's OWN totals ride along, because every caller prints
			// the RUN's figures after the reason: in a mixed-harness run two
			// dollar figures on one line read as a contradiction, which is the
			// defect Budget.ExceededBy's doc comment exists to prevent, one level
			// down.
			return fmt.Sprintf("%s (%s so far: tokens=%d cost=$%.2f)", dim, name, s.tokens, s.cost)
		}
	}
	return ""
}

// drainPhases runs ONE drain as its configured sequence of phase sessions —
// each a separate harness process, so the context resets between them. With no
// phases configured that sequence is one session and this is exactly the old
// behaviour; see config.Phase for why the split exists and config.EffectivePhases
// for why "unphased" is expressed as a single phase rather than a second path.
//
// Two things have to be true across a seam, and they are the whole design:
//
//  1. The claim is NOT handed back. The next phase resumes the same run with
//     heartbeat(runId) rather than claiming, so the lease has to survive the gap.
//     A handback here would put the task in the queue while we are still on it.
//  2. If the next phase will never run, the handback skipped at the seam happens
//     here instead — otherwise a budget stop leaves the task leased to nobody.
func (d *Driver) drainPhases(ctx context.Context, drainNum, ti int, t Target, prior spend) (tokens int, cost float64, handoffs int, stop bool, err error) {
	// The target's own effective config (CLA-410): a stored run-config overlay
	// changes harness/model/budget here, not on the shared base.
	tc := d.targetCfg(t)
	phases := tc.EffectivePhases()
	// rawPhases is the same sequence with every EXPANSION collapsed: index-
	// aligned 1:1 with phases, but each review-panel member maps back to the one
	// `review` phase the operator wrote (CLA-639), so a member is recognised as
	// running the resolved built-in brief its configured phase asked for rather
	// than a prompt of its own. The handoff branch below reads it to tell a
	// built-in-brief phase from an operator's custom prompt, and it MUST be this
	// captured copy (CLA-664): a live read would let a RELOAD applied mid-drain
	// reshape the continuation of the sequence in flight — a 3 -> 2 reload drops
	// the in-flight fix successor's terminal step, and a custom-prompt phase
	// later covered by a built-in gains one.
	rawPhases := make([]config.Phase, len(phases))
	for i := range phases {
		rawPhases[i] = phases[i]
		if src := phases[i].ConfiguredIndex(); src < len(tc.Phases) {
			rawPhases[i] = tc.Phases[src]
		}
	}
	// CLA-638: where the fix→verify pair lives, when the sequence has one. The
	// verification round is last by validation, and the driver repeats the pair
	// by rewinding to fixIdx after a round with open MUSTs. Zero when the
	// sequence has no verification round (the legacy fix-is-terminal shape).
	fixIdx := -1
	for i := range phases {
		if i+1 < len(phases) && phases[i].Name == config.FixPhaseName && isVerifyPhase(phases[i+1]) {
			fixIdx = i
		}
	}
	// MUST ids a verifier has already marked resolved in THIS drain. An id open
	// again is non-convergence (A6). In-memory on purpose: it is a courtesy
	// layer over the durable round-prefix check, which survives a takeover on
	// its own because it reads the plane's round numbers.
	verifyResolved := map[string]bool{}
	// Verification rounds this drain has actually closed. The plane's counted is
	// the cap of record (it survives takeovers and ignores void rounds); this
	// local count is the last-resort bound that keeps the loop finite even if a
	// plane read serves no count at all, which would otherwise make "counted >=
	// 3" unreachable and the fix→verify pair unbounded.
	verifyRoundsRun := 0
	// CLA-639: the panel's members that wrote no record this round, and the round
	// the panel's released records landed in. A dead, capped or otherwise
	// record-less member does not stop the panel: it is listed here, the round
	// still closes, and the fix phase is told the panel was incomplete. The
	// round number is the one the seam MATCHED on a member that did record, so a
	// later member's failure can still release the records already written.
	var (
		panelIncomplete []string
		panelRound      int
	)
	// CLA-639 M4: whether ANY member of this panel recorded, and why each member
	// that did NOT failed. The first decides whether a failed LAST member hands
	// the round to the fix phase (there is something to apply) or ends the drain;
	// the second rides the fix brief, so "the panel was incomplete" names the
	// members AND what happened to them.
	panelRecorded := false
	panelFailed := map[string]string{}
	// CLA-639 M5: the branchless-panel skip says its piece once, not once per
	// skipped member — N-1 identical lines would read as N problems.
	branchlessPanelNoted := false
	// The claim a phase deliberately left held for its successor, and the source
	// of the task/run ids that successor's prompt needs.
	var carried *harness.Result

	// A session-authored successor prompt, pending from the last session's
	// handoff block (CLA-352). Non-empty means the next session is a handoff
	// respawn continuing the SAME phase, not the next configured one.
	nextPrompt := ""
	chain := 0   // consecutive handoff respawns, against handoffChainCap
	spawned := 0 // sessions this drain has launched, phase or respawn
	// Consecutive dead phases this TASK has produced (CLA-386). The retry budget
	// is per task, not per run: a single silent death is plausibly transient, a
	// second on the same task is not, and the same task reached fresh in a later
	// run earns its retry again. deadTask names which task the counter counts for,
	// because the retry is a fresh claiming session (the dead claim was handed
	// back): if next_task hands it a DIFFERENT task, that task has died once, not
	// twice, and must not be parked for a second death it did not have (review
	// finding). A dead phase always holds a claim, so deadTask is set whenever the
	// classification fires.
	deadRetries := 0
	deadTask := ""
	// Consecutive held-but-empty implement exits in this drain (CLA-457). Bounded
	// at ONE retry per the 2026-08-24 decision, deliberately smaller than the
	// dead-phase ladder: the empty shape returns to the queue rather than
	// parking, so the retry is a courtesy to a plausibly-transient silent EOT,
	// not a budget (see the end.empty branch below).
	emptyRetries := 0
	// Whether the checkpoint that opened the CURRENT phase was evidenced by the
	// plane's record rather than a verified branch (CLA-497). Set at each seam
	// from the claim carried INTO the phase; a handoff respawn continues the
	// same phase and must keep the value, so it is never recomputed at the loop
	// head (where `carried` has already become the phase's own claim).
	branchlessCheckpoint := false

	// CLA-466: one iteration record posts at this drain's boundary, whatever way
	// it ends — hence the defer, and hence locals the deferred closure reads.
	drainStart := time.Now()
	var (
		phasesRun []string // distinct phase labels attempted, in order
		// CLA-630: one entry per session this drain spawned, in spawn order —
		// every pass through the loop below (a configured phase, a handoff
		// respawn, a dead- or empty-retry re-run), with the harness and model
		// each actually spawned with. A retry ladder INSIDE one phase is one
		// session re-run, not a new one; see drainPhase's phaseEnd.session.
		sessionsRun []fleet.Session
		seen        harness.Claim
		parkFired   bool // the per-task dead budget parked the task
		tripFired   bool // the fleet dead counter tripped and paused this target
		finalDisp   string
		// CLA-631/CLA-651/CLA-662: the wait a RESUMED phase's sequence-end
		// signal asks the run for. A rolling limit (CLA-631) hands its reset
		// down here; the transient arm (CLA-662) hands its backoff rung down as
		// transientRung/transientKey instead. Both waits are owed AFTER the
		// sequence's handback, so both are taken holding no lease.
		limitWait *phaseLimitWait
		// CLA-662: the streak rung a transient sequence-end reached, and the
		// target it belongs to — zero when no transient end happened.
		transientRung int
		transientKey  resumedTransientKey
		// finalDead / lastProducedNothing are the FINAL attempt's dead
		// classifications (the seam's, and the tally's wider one): a death that
		// was retried past must not brand the record, and a LAST-phase death
		// never sets end.dead at all (no retry/park applies) yet the record must
		// still read `dead` — see phaseEnd.producedNothing.
		finalDead           bool
		lastProducedNothing bool
	)
	defer func() {
		// CLA-639: the iteration record says the panel was incomplete, by member,
		// whether the drain then reached the fix phase or stopped before it. The
		// reviewers that DID record are what the fix phase receives; this names the
		// ones that did not, so "the panel" never reads as the full panel when it
		// was not.
		if len(panelIncomplete) > 0 {
			note := fmt.Sprintf("review panel INCOMPLETE: %s", strings.Join(panelIncomplete, "; "))
			log.Printf("%siteration %d: %s", labelOf(t), drainNum, note)
			if finalDisp == "" {
				finalDisp = note
			} else {
				finalDisp += " — " + note
			}
		}
		outcome := finalDisp
		switch {
		case parkFired:
			outcome = fleet.OutcomeParked
		case tripFired || finalDead || lastProducedNothing || err != nil:
			outcome = fleet.OutcomeDead
		case outcome == "":
			// Nothing was ever observed — a launch failure with no claim state,
			// say. The record still posts (exactly one per iteration); its task
			// fields are simply empty and the outcome reads as released.
			outcome = fleet.OutcomeReleased
		}
		d.endIteration(ti, t, fleet.Iteration{
			TaskID:          seen.TaskID,
			TaskRef:         seen.Ref,
			Phases:          phasesRun,
			Sessions:        sessionsRun,
			Outcome:         outcome,
			DurationSeconds: time.Since(drainStart).Seconds(),
			Tokens:          tokens,
		})
	}()

	for i := 0; i < len(phases); {
		ph := phases[i]
		// CLA-639: whether this position is one reviewer of a review panel. The
		// members are consecutive review-shaped phases, so "before the next
		// configured phase" is the same test the shipped sequence already uses.
		// Captured BEFORE the handoff branch replaces ph.Prompt with a
		// continuation: a panel member never honours a handoff, but the
		// continuation's own review position must still be read from the phase
		// identity, not from a prompt that is no longer a brief.
		panelMember := ph.IsPanelMember()
		last := i == len(phases)-1
		// CLA-639 M5: a BRANCHLESS task cannot run a panel AT ALL, so its panel
		// is one reviewer. This shape's review record is the legacy `## Review`
		// block written to the task's outcome — the plane accepts a durable
		// record only from the task's live holder, and a settled task has none
		// (question 03a12578, option 1) — so a second member could record nothing
		// a durable round could hold, and the block it would append is the FIRST
		// member's own record. The first member runs with the brief it would have
		// had anyway; the members after it are skipped, with one log line.
		if panelMember && branchlessCheckpoint && i > 0 && phases[i-1].IsPanelMember() {
			if !branchlessPanelNoted {
				log.Printf("%siteration %d: this task's checkpoint is BRANCHLESS (its review record is the legacy %q block, not a durable one), so the review panel runs ONE reviewer: %s",
					labelOf(t), drainNum, config.ReviewRecordHeading, phases[i-1].Label(i-1))
				branchlessPanelNoted = true
			}
			i++
			continue
		}
		// CLA-639: the panel's last member is the last REVIEW of the sequence even
		// when a fix follows it, and the driver's plain-review seam must run for it
		// (that is the seam that carries the record gate). Only a PANEL member gets
		// this treatment: a shipped `review` before a fix is not "terminal", and
		// passing that word over would tell it there is nothing to checkpoint for.
		terminal := last
		if panelMember && !last && !config.ReviewShaped(phases[i+1].Prompt) {
			terminal = true
		}
		isHandoff := nextPrompt != ""
		promptBytes := 0
		if isHandoff {
			// The respawn continues this phase's job under its knobs — turn cap
			// and tier still apply. The session's own prompt is framed by
			// config.HandoffPreamble, not handed over bare: the successor needs
			// the same "resume, don't claim" contract a phase resume gets from
			// its brief, and a session-authored prompt cannot be trusted to
			// carry that forward on its own. promptBytes is the session's own
			// prompt alone — what the size cap was measured against — not the
			// preamble-inflated total, so the log line stays comparable to it.
			//
			// config.HandoffContinuation (CLA-353) is appended after it for the
			// same reason: the phase's OWN brief — not only the preamble's
			// contract — is otherwise dropped on a handoff, since ph.Prompt is
			// replaced wholesale rather than framing the built-in text. For the
			// review phase that would silently drop the PR-then-update_task
			// terminal step the moment a session hands off mid-review.
			//
			// Gated on the phase actually RUNNING the built-in brief (review
			// finding): HandoffContinuation matches on name alone, so an
			// operator who reuses the name "review" for a phase carrying their
			// own custom Prompt would otherwise have the shipped terminal step
			// force-injected into a handoff respawn of a brief that never
			// established that contract in the first place — clashing with
			// whatever terminal step their own prompt names, if any.
			// rawPhases (the configured sequence captured at drain start, not
			// EffectivePhases' output) is index-aligned with phases: a panel
			// member maps back to the `review` phase it expanded from, and every
			// other entry is its own configured phase. Never re-read live: a
			// RELOAD applied mid-drain must not reshape the in-flight sequence's
			// continuation (CLA-664) — the same rule the mode and the count follow.
			promptBytes = len(nextPrompt)
			continuation := ""
			if i < len(rawPhases) && rawPhases[i].Prompt == "" {
				// CLA-497: a handoff during a NO-CODE review must not append the
				// branch-shaped reviewTerminalStep, which tells the successor to
				// open a PR for a task that has no branch and no code.
				// branchlessCheckpoint records the checkpoint that opened this
				// phase (captured at the seam from the carried claim), so the
				// continuation matches the brief this phase's first session ran.
				//
				// The split adds the POSITION-dependent review mode: a handoff
				// during a review-only phase must carry the record-then-hold
				// ending, never the commit/push/PR/in_review terminal step. A
				// fix phase carries that terminal step by name.
				reviewOnly := ph.Name == config.ReviewPhaseName && !terminal
				// CLA-637: the checkpoint that OPENED this phase decides which
				// review-only variant the successor continues. A branchless
				// checkpoint's successor gets the no-tip shape; an UNVERIFIED
				// one has no driver-known tip to hand over either, so its
				// successor gets the variant that tells the reviewer to
				// establish the tip itself — the verified shape's {{reviewedTip}}
				// could not be filled and would leak as a literal.
				unverifiedCheckpoint := carried != nil && carried.CheckpointUnverified
				switch {
				case ph.Name == config.ReviewPhaseName && branchlessCheckpoint && reviewOnly:
					continuation = config.NoCodeReviewOnlyHandoffContinuation()
				case ph.Name == config.ReviewPhaseName && branchlessCheckpoint:
					continuation = config.NoCodeHandoffContinuation()
				case ph.Name == config.ReviewPhaseName && reviewOnly && unverifiedCheckpoint:
					continuation = config.UnverifiedReviewOnlyHandoffContinuation()
				case ph.Name == config.FixPhaseName && branchlessCheckpoint && i+1 < len(phases) && isVerifyPhase(phases[i+1]):
					// CLA-638: a BRANCHLESS fix whose verify round follows must
					// still stop before the hand-over — the legacy no-code fix
					// continuation would tell its successor to move the task to
					// in_review (noCodeReviewTerminalStep), which is the
					// verifier's move.
					continuation = config.NoCodeFixVerifyHandoffContinuation()
				case ph.Name == config.FixPhaseName && branchlessCheckpoint:
					continuation = config.NoCodeFixHandoffContinuation()
				case ph.Name == config.FixPhaseName && i+1 < len(phases) && isVerifyPhase(phases[i+1]):
					// CLA-638: the successor of a mid-fix handoff must still stop
					// before the PR; the verification round owns the hand-over.
					continuation = config.FixVerifyHandoffContinuation()
				default:
					continuation = config.HandoffContinuationFor(ph.Name, reviewOnly)
				}
			}
			ph.Prompt = config.HandoffPreamble + nextPrompt + continuation
			nextPrompt = ""
		}

		if spawned > 0 {
			// The breaker at a phase boundary — the third place a session ends and
			// the loop decides whether to spawn another, alongside between-drains
			// and between-attempts, and the finest granularity the ceiling has had.
			// It is not decoration: one measured task spent 92% of a whole run's
			// 75M ceiling inside a single session, which a between-drains check
			// cannot see coming and cannot interrupt. A handoff respawn gets the
			// same check, BEFORE the respawn: a session cannot spend its way past
			// the ceiling by handing off to itself.
			if dim := d.budgetTrip(prior.tokens+tokens, prior.cost+cost, time.Since(prior.start)); dim != "" {
				boundary := "the " + ph.Label(i) + " phase boundary"
				if isHandoff {
					boundary = "the handoff respawn"
				}
				log.Printf("iteration %d: budget reached at %s: %s — stopping (tokens=%d cost=$%.2f elapsed=%s)",
					drainNum, boundary, dim, prior.tokens+tokens, prior.cost+cost,
					time.Since(prior.start).Round(time.Second))
				stop = true
				break
			}
		}

		// Unphased runs keep their log names byte-identical: no phase tag. A
		// handoff respawn is tagged too, so the state dir shows the chain the way
		// the daemon log does.
		tag := ""
		if len(phases) > 1 {
			tag = "-p" + ph.Label(i)
		}
		if isHandoff {
			handoffs++
			chain++
			tag += fmt.Sprintf("-h%d", chain)
			log.Printf("%siteration %d: handoff respawn %d — spawning a fresh session on the predecessor's own %d-byte prompt, framed by the driver's resume preamble (counts as an iteration)",
				labelOf(t), drainNum, chain, promptBytes)
		}
		spawned++

		// CLA-466: announce the phase change NOW rather than at the next poll, so
		// the console sees which phase is running without waiting out an idle
		// interval. taskRef is empty until a claim has been observed (the first
		// phase claims inside drainPhase); from then on it rides every beacon.
		// CLA-510: a first-phase claim no longer waits for the next phase
		// boundary — the renewer's claim reflector (startLeaseRenewal) revises
		// the in-flight ref and beacons the moment the session's claim lands.
		ref := ""
		if carried != nil {
			ref = claimLabel(carried.Claim)
		}
		if len(phasesRun) == 0 || phasesRun[len(phasesRun)-1] != ph.Label(i) {
			phasesRun = append(phasesRun, ph.Label(i))
		}
		d.beginIteration(ti, t, drainNum, ref, ph.Label(i))

		// CLA-662: whether this session RESUMED a predecessor's claim — the
		// exact condition drainPhase's transient arm keys on (prev != nil), so
		// the streak follows the ends that arm produces. Captured BEFORE the
		// call: the seam below may replace `carried` with this phase's own
		// claim, which says nothing about how the phase was entered.
		phaseResumed := carried != nil
		ptokens, pcost, pstop, end, perr := d.drainPhase(ctx, drainNum, ti, i, tag, ph, terminal, carried, t, spend{
			start:  prior.start,
			tokens: prior.tokens + tokens,
			cost:   prior.cost + cost,
		}, phases, panelState{round: panelRound, failed: panelFailed})
		tokens += ptokens
		cost += pcost
		// CLA-630: one entry per session this pass spawned, in spawn order. nil
		// means the pass never launched a session (a harness or spawn-dir
		// refusal above), which is nothing to record.
		if end.session != nil {
			sessionsRun = append(sessionsRun, *end.session)
		}
		finalDead = end.dead
		lastProducedNothing = end.producedNothing
		// CLA-466: the record's task identity and claim disposition follow EVERY
		// claim the sequence observed — including one a dead phase already handed
		// back itself (end.released), which the switch below deliberately drops
		// from `carried`. The off-brief exception matches the switch's own first
		// arm: a claim on a DIFFERENT task does not re-attribute the iteration.
		if end.claim != nil && !(carried != nil && carried.Claim.TaskID != end.claim.Claim.TaskID) {
			seen = end.claim.Claim
			finalDisp = claimDisposition(end.claim)
		}
		// What, if anything, is still owed a handback. Three cases, and getting any
		// of them wrong is a task either stranded on a dead lease or posted back to
		// the queue over work already in review:
		//
		//   - the phase handed the claim back itself → nothing is owed;
		//   - the phase observed claim state → that is now the authoritative view,
		//     including Settled and HasWIP, which releaseHeldClaim reads;
		//   - the phase observed nothing (it never launched) → KEEP the predecessor's,
		//     or a phase 2 that failed to start would silently drop the handback for
		//     the task phase 1 is still holding.
		//
		// A phase that went off-brief and claimed a DIFFERENT task is its own case.
		// noteClaimed replaces the observed claim wholesale, so the seam releases
		// that task and reports `released` — which would drop the predecessor's
		// still-live claim and hand back a task the sequence was never meant to
		// touch. Keep what we had: the task phase 1 is holding is the one owed a
		// handback, whatever its successor went and did.
		seamClaim := carried
		switch {
		case carried != nil && end.claim != nil && end.claim.Claim.TaskID != carried.Claim.TaskID:
			log.Printf("iteration %d: the %s phase claimed %s, which is NOT the task it was resuming (%s) — it was told not to claim at all; keeping the original for the handback",
				drainNum, ph.Label(i), end.claim.Claim.TaskID, carried.Claim.TaskID)
		case end.released:
			carried = nil
		case end.claim != nil:
			carried = end.claim
		}
		// CLA-497: whether the checkpoint that opened THIS phase was evidenced
		// by the plane's record (no verified branch). Read from the PRE-switch
		// claim - the one the seam advanced with: after the switch, carried is
		// this phase's OWN claim, whose branch says nothing about the checkpoint
		// that opened the phase, and the handoff continuation must match the
		// brief this phase's FIRST session ran, not what its last one recorded.
		branchlessCheckpoint = seamClaim != nil && seamClaim.Claim.Branch == ""

		// CLA-639 M4: a PANEL MEMBER that produced no record leaves the run
		// resumable — the next member, or the fix phase, picks up the SAME run
		// from the SAME verified checkpoint. Its own result is what carries the
		// claim forward (the switch above), and a session that was turned away or
		// died before reading the task reports no branch: without this, the next
		// member's brief would be handed an empty branch and read as the
		// BRANCHLESS variant, whose record is the legacy block the panel cannot
		// use. So the checkpoint's resumption facts ride forward from the claim
		// that opened this member, onto a claim that is still live and short of
		// them. Only a member: every other phase's claim is its own.
		if panelMember && !end.reviewRecorded && seamClaim != nil && carried != nil && carried.Claim.Held() {
			if carried.Claim.Branch == "" {
				carried.Claim.Branch = seamClaim.Claim.Branch
			}
			if carried.CheckpointTip == "" {
				carried.CheckpointTip = seamClaim.CheckpointTip
			}
			carried.CheckpointUnverified = seamClaim.CheckpointUnverified
		}

		// A run-level stop wins over a dead phase. drainPhase returns stop=true
		// for a budget trip and credit exhaustion (lim.Stop) — and on those
		// paths end.dead can be set too (a credit-starved session is exactly the
		// shape that dies on reason "unknown" with no branch). Retrying a dead
		// phase against a stopped run burns another session into the same limit
		// and, on the second, parks a task for what was actually a run-wide
		// outage instead of stopping the daemon. A run stop is final; the dead
		// branch only decides retry vs park when the run is continuing. (A
		// resumed phase ending on a live lease is NOT one of those paths — see
		// the sequence-end break below, deliberately before the dead arm,
		// because that end leaves the run continuing.)
		if pstop {
			stop = true
			break
		}

		// CLA-631/CLA-651/CLA-662: a RESUMED phase ended the sequence without
		// completing it — a rolling usage limit (CLA-631) or a transient failure
		// (CLA-651) — and the RUN must not end with it. The sequence stops
		// exactly like the pstop above — the claim is not handed back to the
		// queue, so the lease lapses into a takeover with its branch recorded —
		// but the run carries on: the wait the signal owes is taken below,
		// after the sequence's deferred handback, holding no lease. Broken here
		// rather than flowing into the checks below because those read a session
		// that merely got turned away: the dead/fleet counters would tick on a
		// blip's exit shape (or a limit's "unknown" finish reason), and none of
		// that machinery applies to a sequence end the run is ordered to survive.
		//
		// The transient arm ALSO extends its target's streak here (CLA-662):
		// nothing else bounds a provider that keeps blipping the resumed phase
		// while the implementing harness stays healthy — the drain is over, so
		// drainPhase's retry ladder was never entered, and the re-drain would
		// otherwise land the very next instant. A rolling limit extends no
		// streak: its wait is a reset, not a rung, and the next drain is
		// scheduled after it anyway.
		// CLA-639: a PANEL member's sequence-end is judged by the panel handler
		// below first — it records the member as failed, closes the round that the
		// earlier members wrote into, and only then lets the run-level wait take
		// over. Every other resumed phase keeps the shipped break here.
		if end.sequenceEnd != nil && !end.panelMember {
			limitWait = end.sequenceEnd.limitWait
			if limitWait == nil {
				transientKey = resumedTransientKey{target: ti, harness: tc.HarnessFor(ph)}
				transientRung = d.noteResumedTransient(transientKey)
			}
			break
		}

		// CLA-662: a resumed phase that ended any OTHER way than the transient
		// sequence-end above — completed, settled, died — breaks its target's
		// streak: the next blip on that target+harness is not consecutive with
		// the last, and starts the ladder over. The other sequence-end arm, a
		// rolling limit, also breaks out above and so neither extends nor
		// clears: the reset the run waits out is its own bound. Reset on the
		// seam BETWEEN the arms, so "consecutive" means exactly what it says:
		// the arm above extends, this one clears.
		if phaseResumed {
			d.clearResumedTransient(resumedTransientKey{target: ti, harness: tc.HarnessFor(ph)})
		}

		// CLA-639: the panel's members are skippable where a lone review phase is
		// not. A member that recorded nothing — dead, capped before its record
		// write, empty under the evidence gate, or turned away by a non-retryable
		// error — is RECORDED AS FAILED and the panel moves on, instead of ending
		// the drain with three reviewers still unrun. That is the panel's version
		// of the doneWhen's "recorded or been recorded as failed/capped".
		//
		// Deliberately BEFORE the dead/fleet arms: those exist for a phase whose
		// death leaves the task going nowhere, but a panel member's death is one
		// reviewer lost, not the task lost. Counting it toward the fleet pause
		// would let a flaky reviewer bench the whole target.
		//
		// The round is NOT closed while another member follows: closing it
		// would release the earlier members' findings to that member, which is
		// exactly the panel independence the seal exists to protect. It is closed
		// when the panel is over, whichever way the last member ends.
		if end.panelMember {
			if round := end.reviewRound; round > 0 {
				panelRound = round
			}
			// The member's reviewer ROLE is positional (CLA-639): the durable
			// record and the seam gate are keyed on it, and the operator's `name`
			// is only the log label. The incomplete list and the fix brief must
			// therefore speak roles, or a named member would be reported missing
			// while its role-keyed record is sitting right there.
			role := reviewerRoleFor(phases, i)
			member := ph.Label(i)
			// CLA-639 M4: whether the NEXT member, or the fix phase after the
			// panel, could still resume this run. The member's own claim is the
			// live one when it observed any; a member that observed NOTHING (a
			// launch failure) leaves the predecessor's carried claim in place, and
			// that is still the run the panel is working. A claim the session
			// settled itself, or one already handed back, is not live: nothing may
			// be resumed from it, and only the live holder may close a round.
			live := carried != nil && (end.claim == nil || end.claim.Claim.Held())
			// A member ends the PANEL's stage when it recorded (its record passed
			// the seam gate) or when no next member can run — the last member, a
			// claim the failed member took with it, or a harness sequence-end.
			// Only a failed member with a live claim and a member after it is
			// skipped past.
			//
			// A member whose own round close failed DID record: the seam matched
			// a record and the close is a later, separate step. Counting that as
			// "recorded nothing" would name a reviewer INCOMPLETE and charge its
			// failure streak for the plane's failure to release the round.
			recorded := end.reviewRecorded || end.reviewCloseFailed
			seqEnd := end.sequenceEnd
			// A member that writes its durable record and only THEN ends the
			// sequence (a usage limit or a transient failure) returns before the
			// seam evaluates the record gate. Ask the plane the same question the
			// seam asks, so the member is counted as recorded instead of unnamed
			// INCOMPLETE and charged a failure for writing exactly what it owed
			// (CLA-639 fix pass 4). Only the seam's own matcher inputs qualify: a
			// verified tip (match it exactly) or the unverified mark (demand a
			// tip). A carried claim with neither — a branch with no tip and no
			// unverified mark, the shape the checkpoint-fact copy cannot fill
			// when the claim is not held — would make the matcher accept any
			// record by run and role, looser than the seam ever is, so it is left
			// to the failure arm (CLA-639 fix pass 5).
			if !recorded && seqEnd != nil && carried != nil && (carried.CheckpointTip != "" || carried.CheckpointUnverified) {
				if rec, matched := d.panelMemberRecordMatch(ctx, t, *carried, role); matched {
					recorded = true
					if rec.Round > 0 {
						panelRound = rec.Round
					}
				}
			}
			// endSequenceEnd is the shared tail of the panel's run-level endings:
			// a usage limit or transient failure owes the run its wait after the
			// sequence's handback, exactly as the single-review seam does.
			endSequenceEnd := func() {
				if seqEnd == nil {
					return
				}
				limitWait = seqEnd.limitWait
				if limitWait == nil {
					transientKey = resumedTransientKey{target: ti, harness: tc.HarnessFor(ph)}
					transientRung = d.noteResumedTransient(transientKey)
				}
			}
			if recorded {
				// The member recorded, so its failure streak is over (CLA-639
				// M4): the next failure, if any, is not consecutive with the last.
				d.panelMemberRecorded(member)
			}
			// A round that could not be closed is the single-review seam's
			// fail-closed shape, for the panel: the member recorded, but the fix
			// must not spawn against findings the plane still holds sealed. End
			// the drain here (the deferred handback releases the claim exactly as
			// the single-review close-failure path does) and say why.
			if end.reviewCloseFailed {
				log.Printf("%siteration %d: the %s panel member recorded, but the panel's round %d could not be closed (%s) — ending the drain without spawning the fix: a fix spawned now would run against findings the plane still has sealed",
					labelOf(t), drainNum, member, panelRound, end.emptyWhy)
				endSequenceEnd()
				break
			}
			if !recorded {
				reason := end.emptyWhy
				switch {
				case end.dead:
					reason = "died producing nothing"
				case seqEnd != nil && seqEnd.limitWait != nil:
					reason = "hit a usage limit before recording"
				case seqEnd != nil:
					reason = "ended on a transient failure before recording"
				case perr != nil:
					reason = failureDetail(perr.Error())
				case end.empty:
					reason = end.emptyWhy
				case reason == "":
					reason = "recorded no durable review record"
				}
				panelIncomplete = append(panelIncomplete, role+": "+reason)
				panelFailed[role] = reason
				log.Printf("%siteration %d: the %s panel member recorded nothing (%s) — recording it as failed", labelOf(t), drainNum, member, reason)
				// CLA-639 M4: the member is a failure the operator has to be able
				// to see, so it is counted in the run's per-harness dead-phase
				// tally (CLA-402) — the rate that exists for exactly this shape —
				// unless the seam's own classification already booked this exit
				// (a dead session, or a held-but-empty one). And a member that
				// keeps failing task after task is a fault in that member rather
				// than bad luck: the streak is counted across tasks and said
				// loudly once it reaches panelMemberFailBound. Deliberately NOT a
				// park: one reviewer of several failing is not a reason to strand
				// the panel's real findings.
				if !end.deadBooked {
					d.tallyEmpty(member, tc.HarnessFor(ph))
				}
				if n := d.panelMemberFailed(member); n >= panelMemberFailBound {
					model, _ := tc.ModelForPhase(ph)
					log.Printf("%siteration %d: WARNING — the %s review-panel member has recorded nothing on %d consecutive tasks (harness %s, model %s); the panel runs on and the fix applies what the other members found, but this member's harness, model or brief is not producing reviews — check its iteration logs",
						labelOf(t), drainNum, member, n, tc.HarnessFor(ph), attributionOr(model, "harness default"))
				}
				if live && !end.panelLast {
					// The next member resumes this claim: keep the panel going, and
					// keep the round sealed — a close here would release the earlier
					// members' findings to the next reviewer. ADVANCE, never re-run
					// this member: the loop has no post statement, so a `continue`
					// without the increment would re-spawn index i forever.
					i++
					continue
				}
				// The panel is over: nothing follows this member, or the claim it
				// would have resumed is gone, or the harness ended the sequence.
				// Release what the earlier members wrote rather than leave it
				// sealed behind a lease that will lapse — BEFORE any handback,
				// because the close is legal only while we are still the holder.
				if panelRound > 0 && live {
					if cerr := d.closeReviewRound(ctx, t, *carried, panelRound); cerr != nil {
						log.Printf("%siteration %d: the panel's round %d could not be closed after %s failed (%v) — ending the drain without spawning the fix: a fix spawned now would run against findings the plane still has sealed",
							labelOf(t), drainNum, panelRound, member, cerr)
						endSequenceEnd()
						break
					}
				}
				switch {
				case live && panelRecorded && i+1 < len(phases):
					// Earlier members DID record, so the panel hands over rather
					// than ending: the phase after it (the fix) applies the merged
					// list, which names this member as INCOMPLETE, and resumes the
					// same run. Checked BEFORE the sequence-end arm on purpose — a
					// member turned away by a usage limit is exactly the case the
					// bar names ("a usage-limited or dead member does not stop the
					// round"), and the sequence is not ending here: if the phase
					// that follows hits the same limit it says so itself, with the
					// wait that belongs to ITS end.
					i++
					continue
				case seqEnd != nil:
					// The run-level sequence-end break above was deferred for this
					// member so the round could be released first; end the attempt
					// here, with the run's wait machinery to follow.
					endSequenceEnd()
				}
				break
			}
			panelRecorded = true
			// This member recorded. A recorded mid-panel member whose session was
			// then turned away by a run-level limit or transient failure continues
			// the panel exactly as a failed one does: its deliverable is in the
			// round, and the wait belongs to the end that actually stops the
			// sequence.
			//
			// The LAST member is the panel's close, so its sequence-end must do
			// what the failed last member's arm does: release the round while the
			// holder is still live (a sealed round would be voided by the
			// takeover, losing every member's record), then hand over to the fix
			// that applies the merged list. A close that fails is fail-closed —
			// the drain ends with the wait and no fix runs against a sealed
			// round.
			if seqEnd != nil {
				if live && !end.panelLast && i+1 < len(phases) {
					i++
					continue
				}
				// Close only when the seam did not already close the round for
				// this member. A session the adapter ALSO classifies as a limit
				// or transient can exit cleanly (or be capped with WIP), so the
				// seam runs first, matches the record and closes the round; the
				// sequence-end is then read by the arm below. A second close is
				// the plane's no_open_round, and failing closed on it would skip
				// the fix against a round that is already released.
				if live && panelRound > 0 && !end.reviewRecorded {
					if cerr := d.closeReviewRound(ctx, t, *carried, panelRound); cerr != nil {
						log.Printf("%siteration %d: the panel's round %d could not be closed after %s recorded but ended the sequence (%v) — ending the drain without spawning the fix: a fix spawned now would run against findings the plane still has sealed",
							labelOf(t), drainNum, panelRound, member, cerr)
						endSequenceEnd()
						break
					}
				}
				if live && panelRecorded && i+1 < len(phases) {
					i++
					continue
				}
				endSequenceEnd()
				break
			}
		}

		// CLA-386: a dead phase is a FAILED phase, not a completed one, and it is
		// not "ended without holding the task" either — it never finished the job
		// it was spawned to do. The claim was released back in drainPhase (a retry
		// re-claims, exactly like a transient one), so the only question left is
		// retry vs park. Consecutive dead phases on the SAME task: retry up to the
		// per-task budget, then park — a task that can kill four full sessions
		// reaches the operator rather than a fifth (CLA-396, raised from two by
		// the 2026-08-20 operator decision).
		//
		// Deliberately BEFORE the error break below: the dead classification
		// names a phase that produced nothing whatever its exit code, so a dead
		// phase is retried-then-parked, never a run-failing error — the non-zero
		// exit that would otherwise stop the daemon is itself part of the silent
		// death, not a verdict.
		//
		// The retry is scoped to the FIRST phase (`i == 0`), the one that claimed
		// its own task: a retry cannot re-seed a resumed phase — invocationFor
		// only substitutes the task/run placeholders and sets ResumeClaim when the
		// predecessor claim is present, which the dead handback cleared — so
		// retrying a dead RESUMED phase would spawn a session whose brief still
		// carries the literal {{taskId}}/{{runId}} and whose handback, salvage and
		// delivery checks are all switched off. A resumed phase's dead signature
		// still vetoes its checkpoint below (the false-premise guard applies to
		// every phase); it just is not retried or parked here — the drain ends and
		// the task returns to the queue, where a fresh claiming session retries it
		// with a valid seed (review finding). The operator decision's "retry the
		// implement phase N times, then park" is exactly `i == 0` in the shipped
		// [implement, review] sequence.
		//
		// CLA-396: a dead phase feeds BOTH counters, but the per-task park is
		// checked and taken FIRST — before the fleet counter's trip is even
		// consulted. The fleet counter persists on the Driver across drains and
		// only resets on a successful implement phase, so it can carry a residual
		// count left over from an earlier, unrelated task's death into THIS
		// task's own run of deaths. Checking fleet first would let that leftover
		// tip the fleet bound one death before this task reaches its own —
		// pre-empting a park this task legitimately earned with a fleet-wide
		// pause that blames the provider for what is, this time, actually the
		// task. Per-task takes priority: a task that has itself earned a park is
		// parked, full stop; only once that is ruled out does a fleet-wide run
		// of deaths across DIFFERENT tasks get to trip the fleet pause. A session
		// that never got past its claim is excluded from both by construction:
		// `end.dead` requires `res.Claim.Held()`, and a refused claim (lease_live,
		// a lost race) observes no task id, so its death counts toward neither
		// counter. This is the discriminator CLA-402's retrospective scan must
		// agree with.
		if end.dead {
			taskID := ""
			if end.claim != nil {
				taskID = end.claim.Claim.TaskID
			}

			if !last && i == 0 && taskID != "" && taskID == deadTask && deadRetries >= perTaskDeadBound-1 {
				log.Printf("%siteration %d: the %s phase died producing nothing for the %dth consecutive time on this task — parking it per the 2026-08-20 decision (%d consecutive dead phases, then park)",
					labelOf(t), drainNum, ph.Label(i), deadRetries+1, perTaskDeadBound)
				d.parkDeadPhase(ctx, t, ph.Label(i), end.claim)
				parkFired = true // CLA-466: the record says parked, not dead
				// The task is parked for the operator and this drain is over; the
				// daemon carries on with the next task. Not a stop: the run did
				// not fail, one task reached a human.
				break
			}

			d.fleetDead[ti]++
			if d.fleetDead[ti] >= fleetDeadBound {
				d.fleetTrip(ctx, t, ti, drainNum, end.claim)
				tripFired = true // CLA-466: a fleet trip ends this drain on a death
				// The fleet trip pauses this target and this drain is over; the
				// in-flight task was already released with the dead phase. Not a
				// stop: the run itself did not fail.
				break
			}

			if !last && i == 0 {
				// Either the first dead phase this task produced, or a dead phase
				// on a task other than the one counted above: that task has died
				// once, and earns its own retry ladder rather than being parked
				// for deaths it did not have.
				if taskID != deadTask {
					deadRetries = 1
					deadTask = taskID
				} else {
					deadRetries++
				}
				log.Printf("%siteration %d: the %s phase died producing nothing (final step reason %q, no branch recorded) — retrying it (%d of %d) before any review brief sees the task",
					labelOf(t), drainNum, ph.Label(i), deadReason(end.claim), deadRetries, perTaskDeadBound)
				continue
			}
		}

		if perr != nil {
			err = perr
			break
		}

		// A handoff the session emitted and drainPhase judged safe (clean exit,
		// trusted stream, task still held, prompt within the size cap). Two guards
		// remain, and both refuse toward the STANDARD path — the next configured
		// brief, or the drain ending — never toward truncating or rewriting the
		// session's prompt (CLA-352).
		if end.handoff != "" {
			switch {
			case chain >= handoffChainCap:
				log.Printf("%siteration %d: handoff refused — this task has already chained %d consecutive handoff respawns, the cap; falling back to the standard brief so a session cannot chain itself indefinitely",
					labelOf(t), drainNum, chain)
			case d.cfg.MaxIterations > 0 && drainNum+handoffs >= d.cfg.MaxIterations:
				log.Printf("%siteration %d: handoff refused — a respawn counts as an iteration and max-iterations (%d) is already spent; falling back to the normal path",
					labelOf(t), drainNum, d.cfg.MaxIterations)
			default:
				// Same phase index: the successor continues this phase's job. The
				// budget breaker at the top of the loop runs before the spawn.
				nextPrompt = end.handoff
				continue
			}
			// The handoff was REFUSED, so its exemption from the evidence gate ends
			// here: the exit is re-judged as the bare exit it really is (CLA-457).
			// With no durable, origin-reachable branch evidence that is a FAILED
			// empty phase — logged, tallied, never handed to the review brief.
			//
			// CLA-637: the review stage is over after all. drainPhase deliberately
			// left the matched review round SEALED when it detected the handoff —
			// the successor was going to close it at its own seam — but the refused
			// respawn is the last session that continuation was ever going to get,
			// so the round must be released HERE, before the checkpoint advances: a
			// fix phase spawned against a sealed round can neither read nor answer
			// (finding_sealed) the findings it was told to apply. A close that
			// fails refuses the checkpoint, exactly as the seam's own close does.
			nextReviewShaped := i+1 < len(phases) && config.ReviewShaped(phases[i+1].Prompt)
			if end.reviewRound > 0 && end.checkpoint && end.claim != nil && !nextReviewShaped {
				if cerr := d.closeReviewRound(ctx, t, *end.claim, end.reviewRound); cerr != nil {
					log.Printf("%siteration %d: the %s phase's review round %d could not be closed after its handoff was refused (%v) — refusing the checkpoint, because the next phase would be spawned against findings the plane still has sealed",
						labelOf(t), drainNum, ph.Label(i), end.reviewRound, cerr)
					d.tallyEmpty(ph.Label(i), tc.HarnessFor(ph))
					break
				}
			}
			if !last && end.empty {
				log.Printf("%siteration %d: the %s phase ended holding a task with no recorded branch on the origin remote — not a checkpoint (%s), so no review phase spawns (its handoff was refused)",
					labelOf(t), drainNum, ph.Label(i), end.emptyWhy)
				d.tallyEmpty(ph.Label(i), tc.HarnessFor(ph))
				break
			}
		}
		// This session ended without a respawn being accepted, so any chain is
		// broken: a later handoff starts counting from a standard-brief session.
		chain = 0

		// A held-but-empty exit is a FAILED phase (2026-08-24), not a checkpoint
		// and not an ordinary "ended without holding the task" end: the claim was
		// held to the end, it just carried no durable, origin-reachable branch
		// evidence. It gets ONE bounded retry, and only when the claim was ACTUALLY
		// released back to the queue — a fresh claiming session can then pick the
		// task straight back up. A recorded-but-unreachable branch was left to the
		// takeover path by the handback (never released), and retrying would seed a
		// session against a live lease nothing here renews, so such an exit ends the
		// drain. This also covers a handoff the guards REFUSED above (chain cap,
		// max-iterations): the handoff exemption ends the moment the successor is
		// not going to run, so the exit must be re-judged as the bare exit it really
		// is. Deliberately OUTSIDE the dead-phase ladder: the dead shape
		// retries-then-parks a task that can kill four sessions, while the empty
		// shape returns to the queue for the next drain — the tally measures it
		// without tripping the park or fleet machinery, which is how a fleet-wide
		// silent EOT shows up in the rate the operator watches.
		if end.empty {
			if !last && i == 0 && emptyRetries < 1 && end.releasedToQueue {
				emptyRetries++
				continue
			}
			break
		}

		// A non-final phase that did not reach its checkpoint leaves the next one
		// nothing to resume: the session settled the task itself (worked past its
		// brief and finished it), its stream could not be trusted to say either way,
		// or the adapter does not observe claims at all. The sequence is over, and
		// none of those is a failure — a task that reached in_review in one session
		// is a task that got done.
		if !last && !end.checkpoint {
			log.Printf("iteration %d: the %s phase ended without holding the task — nothing for the next phase to resume, so this drain ends here",
				drainNum, ph.Label(i))
			break
		}
		// CLA-638: the verification round is the stopping rule's gate. It is the
		// LAST configured phase, so the generic `!last` branches above never
		// applied to it; what follows is decided HERE from the durable records
		// the round just released:
		//
		//   - no open MUST → the gate passed. The verifier carried the terminal
		//     step (push check, PR, in_review) itself; the sequence is done.
		//   - open MUST(s) → another fix is spawned, unless the round cap or the
		//     non-convergence breaker parks the task first. Never one more round
		//     on the driver's own initiative.
		if isVerifyPhase(ph) {
			vres := harness.Result{}
			if end.claim != nil {
				vres = *end.claim
			}
			if end.verifyFailure != "" {
				// The seam could not judge the round: no durable record this run
				// wrote, a round that would not close, or a task that could not
				// be read back. That is the same fail-closed shape CLA-637 gives
				// a refused review checkpoint — the drain ends without the
				// hand-over and the claim is released/lapses, so a takeover can
				// retry it. It is not the stopping rule parking a disagreement.
				log.Printf("%siteration %d: the %s phase's round could not be judged (%s) — refusing the sequence: no hand-over, and no PR was accepted from this round",
					labelOf(t), drainNum, ph.Label(i), end.verifyFailure)
				break
			}
			if !end.verifyChecked {
				// A verification session that died or produced no judgeable exit.
				// End the sequence; the normal takeover path retries it.
				log.Printf("%siteration %d: the %s phase produced no judgeable verification round — ending the sequence without a hand-over",
					labelOf(t), drainNum, ph.Label(i))
				break
			}
			if len(end.verifyOpenMusts) == 0 {
				if end.verifyStatus == "in_progress" || end.verifyStatus == "ready" {
					log.Printf("%siteration %d: the %s phase closed round %d with NO open MUST but left the task %q — it recorded a pass but did not carry the terminal step (push check, PR, in_review); the sequence ends without a hand-over for the operator to see",
						labelOf(t), drainNum, ph.Label(i), end.verifyRound, end.verifyStatus)
				}
				if end.verifyRound == 0 {
					// CLA-638 × CLA-670: a BRANCHLESS round. Its record is the
					// legacy `## Review` block, so there is no plane open-MUST set
					// for the id-based gate to read — the verifier's own block
					// carried the verdict, and the sequence ends without claiming
					// the id-based stopping rule ran (it cannot, on this shape).
					log.Printf("iteration %d: the %s phase's verification round rests on the legacy %q block (this shape writes no durable record) — no plane open-MUST set to judge; ending the sequence",
						drainNum, ph.Label(i), config.ReviewRecordHeading)
					break
				}
				log.Printf("iteration %d: the %s phase closed round %d with NO open MUST — the stopping rule is satisfied and the sequence is done",
					drainNum, ph.Label(i), end.verifyRound)
				break
			}
			verifyRoundsRun++
			reason := nonConvergenceReason(end.verifyOpenMusts, end.verifyRound, verifyResolved)
			// The local fallback counts the same ROUNDS the plane does (CLA-686
			// D2). A review cycle is one review round plus the verification
			// rounds that follow it, and the plane's `counted` includes the
			// review round — so comparing verification rounds ALONE against the
			// cap allowed a cycle one round longer than the counted path, and
			// the park then read "Round 4 of the 3-round cap". `+1` is the
			// review round that opened this cycle.
			localRounds := verifyRoundsRun + 1
			capHit := end.verifyCounted >= config.ReviewRoundCap || localRounds >= config.ReviewRoundCap
			if reason == "" && !capHit && fixIdx >= 0 && end.checkpoint && carried != nil {
				for _, id := range end.verifyResolvedIDs {
					verifyResolved[id] = true
				}
				log.Printf("iteration %d: the %s phase closed round %d (%d non-void round(s) this cycle) with open MUST(s) [%s] — spawning another %s round against them",
					drainNum, ph.Label(i), end.verifyRound, end.verifyCounted,
					strings.Join(end.verifyOpenMusts, ", "), phases[fixIdx].Label(fixIdx))
				// CLA-639: the merged panel list belongs to the FIRST fix after
				// the panel, before any verification round has run. A verify
				// rewind is a later fix round: its grading is the verifier's own
				// record (plus the plane's open-MUST list), and reprinting the
				// panel's list would replace the verifier's findings with a
				// stale round. Clear the panel round so drainPhase falls back to
				// the shipped single-review path unchanged.
				panelRound = 0
				i = fixIdx
				continue
			}
			if reason == "" {
				switch {
				case capHit:
					reason = fmt.Sprintf("the %d-round cap for this review cycle is reached", config.ReviewRoundCap)
				case fixIdx < 0:
					reason = "the sequence has no fix phase to run between verification rounds"
				default:
					reason = "the round ended without holding the task, so no fix can be spawned against its open MUSTs"
				}
			}
			d.parkVerification(ctx, t, vres, reason, end.verifyRound, end.verifyCounted, end.verifyOpenMusts, end.verifyOpenDetails)
			parkFired = true
			carried = nil
			break
		}
		// A phase that ended dead was retried above and, on the retry, either
		// parked or advanced — either way this phase's dead-phase budget is spent.
		// Resetting on the advance keeps the retry per-PHASE rather than per-run:
		// two dead phases separated by a healthy one are not "consecutive", and
		// each phase earns its own retry ladder (CLA-386).
		deadRetries = 0
		deadTask = ""
		emptyRetries = 0
		// CLA-396: a successful IMPLEMENT phase resets the FLEET-wide dead-phase
		// counter. Only the implement phase runs the harness that can exhibit the
		// zero-usage death — the review phase runs on the claude harness, which
		// cannot — so a claude success must not vouch for the opencode gateway:
		// letting any success reset the counter would hide a fleet fault from the
		// detector that exists to find it. Reaching here means this phase did not
		// die and did not fail, so for the implement phase it is the "successful
		// implement" the reset is keyed to.
		if ph.Name == config.ImplementPhaseName {
			d.fleetDead[ti] = 0
		}
		// CLA-639: a member that DID record leaves its round open for the next
		// member (the close belongs to the panel's last phase, not to each
		// reviewer), so remember it — a later member that fails must still be able
		// to release what the earlier ones wrote.
		if panelMember && end.reviewRound > 0 {
			panelRound = end.reviewRound
		}
		i++
	}

	// The deferred handback (2, above). Skipped at every seam that led to another
	// phase; owed at the one that did not — including a final-phase handoff the
	// guards above refused, whose claim was held for a successor that never ran.
	carried = d.releaseCarried(ctx, t, carried)

	// CLA-662: the transient arm's wait, in the same place and for the same
	// reason as the limit wait below — after the sequence's handback, with NO
	// lease held. The rung is the target's streak of consecutive transient
	// ends, so a provider that keeps blipping the resumed phase is slowed
	// between drains instead of being re-drained the instant the last sequence
	// died (and, with implement and the resumed phase on different harnesses,
	// the only momentum the first-phase backoff could ever have was this).
	// The wait is bounded by retry_cap like every backoff(n), and STOP /
	// RESTART / RELOAD are honoured through waitOrStop exactly as they are
	// during the limit wait; the drain returns when it ends and Run polls again.
	//
	// Deliberately NOT fed into sidelineTarget, which the task that added this
	// asked us to consider: a sideline says "this target's harness is broken,
	// bench it" — its counters and log line are built for a drain that FAILED
	// to run a session — and this end returns err == nil. Run's success branch
	// would immediately clear the failure counter it read while leaving the
	// 15m+ skipUntil in place, so the log would report "the harness succeeded
	// again — rejoining the rotation" against a target still benched by a
	// sit-out nothing would then account for. Doing it properly means
	// redefining what a successful drain proves about the harness, well beyond
	// this bound's need: the ladder's growth, capped by retry_cap, plus
	// max_iterations is the brake the un-waited re-drain was missing.
	if transientRung > 0 {
		wait := d.resumedTransientDuration(transientRung)
		log.Printf("iteration %d: the sequence is over and no lease is held — waiting %s before polling again (transient failure %d in a row on %s; the run stays up)",
			drainNum, wait, transientRung, transientKey.harness)
		if d.waitOrStop(ctx, wait) {
			stop = true
		}
	}

	// CLA-631: the sequence ended on a rolling usage limit and the run is owed
	// its wait — after the handback above, deliberately, so the wait is taken
	// with NO lease held: the claim is back in the queue or, with pushed work,
	// left to lapse into a takeover exactly as the old stop path left it. The
	// wait itself is the first-phase path's own machinery, reused: the same
	// budget breaker inside supervisedWait, the same STOP/RESTART/RELOAD reads
	// in waitOrStop, the same probes on the harness that hit the limit, and the
	// same stated-reset handling — the difference is only that there is no
	// session left to re-run when it lifts, so the drain returns and Run polls
	// again. waitPastBudget is consulted first, exactly as on the first-phase
	// path: a reset beyond the run's remaining ceiling is not worth sleeping
	// through, so the run stops and says when to start a fresh one.
	if limitWait != nil {
		remaining, bounded := tc.Budget.Remaining(time.Since(prior.start))
		if until, over := waitPastBudget(limitWait.lim.ResetAt, remaining, bounded); over {
			log.Printf("iteration %d: the limit resets %s, in %s — more than the %s left of this run's ceiling; stopping now rather than waiting, so start a fresh run after the reset",
				drainNum, until.Format("Mon 15:04"), time.Until(until).Round(time.Minute), remaining.Round(time.Minute))
			stop = true
		} else {
			log.Printf("iteration %d: the sequence is over and no lease is held — waiting for the reset%s before polling again (the run stays up)",
				drainNum, resetSuffix(limitWait.lim.ResetAt))
			ptokens, pcost, pstop := d.supervisedWait(ctx, limitWait.lim, limitWait.a, limitWait.ph, t, spend{
				start:  prior.start,
				tokens: prior.tokens + tokens,
				cost:   prior.cost + cost,
			})
			tokens += ptokens
			cost += pcost
			if pstop {
				stop = true
			}
		}
	}
	return tokens, cost, handoffs, stop, err
}

// panelState is what a phase following a review panel needs to know about it
// (CLA-639): the round the panel's members' records landed in, and — for the
// members that recorded NOTHING — why, keyed by reviewer role. Zero when the
// phase does not follow a panel, or when the panel never got a record into a
// round — which is exactly when there is no merged list to build.
type panelState struct {
	round int
	// failed maps a member's role to the reason it recorded nothing, so the fix
	// brief names the incomplete members AND what happened to them, not only
	// that the list is short.
	failed map[string]string
}

// phaseEnd is how a phase reports what it left behind, so drainPhases can decide
// whether to run the next one and what is still owed a handback.
//
// The three fields are deliberately independent. A phase can end holding a claim
// without it being a checkpoint (it was the last one), and can observe claim
// state without owing anything (it settled the task, or already handed it back).
type phaseEnd struct {
	// claim is the final attempt's result when that result carried claim state at
	// all — the thing a deferred handback acts on, and the source of the ids the
	// next phase's prompt is seeded with. nil when the phase observed nothing:
	// a launch failure, or an adapter that does not track claims. nil means "I
	// know nothing", NOT "nothing is owed" — the caller keeps what it had.
	claim *harness.Result

	// released reports that this phase already handed a genuinely-held claim back
	// itself, so nothing is owed and a second handback would post `ready` over
	// whatever came next.
	released bool

	// checkpoint reports an orderly end still holding the task, with the sequence
	// meant to continue into the next phase.
	checkpoint bool

	// dead reports the CLA-386 dead-phase signature: the session's final step
	// finished with reason "unknown" and no branch was recorded on the task, so
	// the phase produced nothing. It is a FAILED phase, not a completed one —
	// the seam must not hand it on to the next phase's brief. The claim has
	// already been released (a retry re-claims); drainPhases owns the retry-once-
	// then-park decision and its per-task budget.
	dead bool

	// producedNothing is the WIDER dead classification (the tally's, CLA-402):
	// the same signature WITHOUT the !last conjunct end.dead carries, because a
	// dead LAST phase is still a death even though no retry/park machinery can
	// apply to it. Only the fleet record reads this field (CLA-466) — the
	// iteration record must say `dead` when the drain ended on exactly such a
	// final-phase death — so it deliberately does not feed the counters or the
	// seam, whose !last gating is pinned by its own tests.
	producedNothing bool

	// deadBooked reports that the run's dead-phase tally (CLA-402) already
	// counted THIS exit as a dead phase: either tallyDead's own predicate fired
	// or the seam's held-but-empty branch booked it (tallyEmpty). The review
	// panel's member handler books a failed member into the same tally (CLA-639
	// M4), and this is how it avoids counting one exit twice — see
	// notePanelMemberFailure's caller.
	deadBooked bool

	// session is the harness session this pass spawned, for the fleet record's
	// sessions list (CLA-630): the phase label, the resolved harness and the
	// resolved model the spawn line named. Only the fleet record reads it.
	// nil means the pass spawned nothing (a harness or spawn-dir resolution
	// refused before any Invoke), which is not a session to report.
	session *fleet.Session

	// empty reports a held-but-empty exit — the 2026-08-24 fleet-incident shape:
	// the phase ended cleanly still holding the task with NO durable,
	// origin-reachable branch evidence on the plane record. It is a FAILED phase,
	// not a checkpoint: the seam must not hand it to the review brief. The claim
	// has already been released (a fresh session re-claims) or left to expire (a
	// recorded-but-unreachable branch keeps its takeover hand-off); drainPhases
	// owns the one-bounded retry. Deliberately distinct from dead, which keys on
	// the "unknown" finish reason — the incident's sessions finished with reason
	// "stop", so the dead signature alone did not catch them. Set on a HANDOFF
	// too, so a refused handoff is re-judged as the bare exit it really is.
	empty bool

	// emptyWhy is the evidence-gate reason when empty fired, for the "not a
	// checkpoint" log line ("no branch recorded on the task", the failing
	// branch check's detail, or — since CLA-497 — the plane record's answer
	// when the branch-less evidence forms were consulted and showed nothing).
	emptyWhy string

	// branch is the branch name when this phase checkpointed — the
	// plane-record evidence the checkpoint is gated on (CLA-457), carried here
	// so the successor's brief can name it instead of asserting an unverified
	// phase-1 success. Folded onto the carried claim by the seam, which marks
	// it UNVERIFIED (CLA-576) when the origin could not be read; the brief
	// selection reads that mark, so the branch reaches the successor named but
	// never presented as verified.
	branch string

	// releasedToQueue reports that this phase's handback ACTUALLY returned the
	// claim to the ready queue (releaseHeldClaim succeeded), as opposed to
	// leaving it to the takeover path or a failed release. It is the only signal
	// a retry may re-claim against: a release the plane refused leaves the lease
	// live, and a fresh claiming session would only be refused (CLA-457).
	releasedToQueue bool

	// handoff is the successor prompt the session emitted in its final message's
	// handoff block, already past the parse-time guards (marker present, prompt
	// non-empty and under the size cap, clean exit, trusted stream, task still
	// held — see detectHandoff). Non-empty implies checkpoint: the lease is
	// being held for the successor. drainPhases owns the remaining guards — the
	// chain cap, max-iterations, the budget breaker — and either respawns on it
	// or falls back to the standard path (CLA-352).
	handoff string

	// panelMember reports that this pass was one reviewer of a review panel
	// (CLA-639), and panelLast that it was the panel's last member. drainPhases
	// reads the pair to tell a skippable reviewer — a dead, capped or
	// record-less member is recorded as failed and the round still closes — from
	// a lone review phase, whose failure keeps the shipped fail-closed ending.
	panelMember bool
	panelLast   bool
	// reviewRecorded reports that this pass's review seam gate PASSED — the
	// member left a durable record behind. It is what the panel handler's
	// "recorded or been recorded as failed" decision reads.
	reviewRecorded bool

	// reviewCloseFailed reports that this pass's review seam gate passed far
	// enough to find a SEALED round, but the driver's own close of that round
	// failed. The record exists (the reviewer did record), so the panel must not
	// name the member INCOMPLETE or charge its failure streak; the drain must
	// end without spawning the fix, because the fix would run against findings
	// the plane still holds sealed. It is the close-failure half of
	// reviewRecorded, kept separate because the evidence gate sets both to the
	// same value everywhere else.
	reviewCloseFailed bool

	// reviewRound is the review round the seam gate MATCHED this pass (CLA-637),
	// zero when the phase is not review-shaped or no record matched. The round is
	// normally closed inside drainPhase the moment the stage is over — but a
	// DETECTED handoff defers that close to the successor's seam, and the
	// remaining handoff guards (chain cap, max-iterations) live in drainPhases:
	// when they REFUSE the respawn, the stage is over after all, and drainPhases
	// closes this round before advancing — otherwise the fix phase would spawn
	// against findings the plane still holds sealed, which it can neither read
	// nor answer (finding_sealed).
	reviewRound int

	// verifyChecked reports that this pass was a fix-verification round and the
	// driver evaluated its seam (CLA-638): matched the durable record, closed the
	// round, and read the task's open MUSTs back. It is set even when the
	// evaluation FAILED, so drainPhases can tell "verification was attempted and
	// could not be judged" from "this was an ordinary phase".
	verifyChecked bool
	// verifyFailure is the reason a verification seam could not be evaluated
	// (no record this run wrote, an unreadable task, a round that would not
	// close). Non-empty means the driver must park rather than hand over or
	// loop: an unverifiable round cannot be the gate.
	verifyFailure string
	// verifyRound is the round the verification record landed in — the plane
	// assigns it, the driver reads it back.
	verifyRound int
	// verifyOpenMusts is every open MUST on a RELEASED record, by plane finding
	// id, as of AFTER this verification round closed. Empty means the gate
	// passed: the verifier's own record was closed, so its new findings (if any)
	// are included. The driver loops another fix when it is non-empty.
	verifyOpenMusts []string
	// verifyOpenDetails is the same list with each finding's summary appended,
	// for the park's question: "carrying the records" means the operator reads
	// WHAT is open, not only its id.
	verifyOpenDetails []string
	// verifyCounted is the plane's count of NON-VOID rounds in the newest
	// record's cycle, read after the close. The round cap reads it; a void round
	// does not count (B4/B8).
	verifyCounted int
	// verifyResolvedIDs are the MUST ids on released records a verifier has
	// already marked resolved (fixed/filed/refuted). The driver remembers them
	// across the round loop: an id that is open AGAIN after being resolved is
	// non-convergence, and parks the task (A6).
	verifyResolvedIDs []string
	// verifyStatus is the task's plane status after the round closed: a clean
	// round must have handed the task off (in_review/done/...); one that left it
	// in_progress recorded a pass but did not carry the terminal step, which the
	// driver says loudly rather than mistaking for a hand-over.
	verifyStatus string

	// sequenceEnd, when non-nil, reports that this RESUMED phase ended the
	// sequence without completing it, and the RUN must not end with it. Two
	// arms produce it: a ROLLING usage limit (CLA-631) and a TRANSIENT failure
	// (CLA-651). In both cases the sequence ends exactly as the old stop did —
	// the claim is not handed back to the queue, so the lease lapses into a
	// takeover with its branch recorded — but drainPhases returns to Run, which
	// carries on polling. Both arms owe a wait, taken after the sequence's
	// deferred handback with no lease held: a set limitWait is the reset the
	// run waits out (CLA-631); a nil one is a transient blip, and the run waits
	// the target's growing backoff rung instead (CLA-662) — because with the
	// resumed phase on a DIFFERENT harness from the one that claims the queue
	// (implement opencode, review claude), there is no first-phase failure for
	// the in-drain ladder to see. Nil on every other path, including a hard
	// stop (lim.Stop), which still stops the run in drainPhase.
	sequenceEnd *phaseSequenceEnd
}

// phaseSequenceEnd is the signal the sequenceEnd field carries: a resumed
// phase could not continue its sequence, but the RUN must (CLA-631, CLA-651).
// It is deliberately not a stop — the sequence ends, the daemon does not.
type phaseSequenceEnd struct {
	// limitWait, when non-nil, is the rolling usage limit whose reset the run
	// owes a wait for after the sequence's handback, on the harness that hit
	// it (CLA-631). Nil means the TRANSIENT arm (CLA-651): there is no reset
	// to wait out, so drainPhases instead extends the target's consecutive-
	// transient streak and waits backoff(n) before polling again (CLA-662).
	limitWait *phaseLimitWait
}

// phaseLimitWait is the wait a rolling usage limit owes a run whose RESUMED
// phase was turned away: a request for drainPhases to wait the limit out after
// the sequence's handback, with no lease held — the shape the resumed phase
// itself must never take, because the wait would outlive its 30-minute lease
// (CLA-631).
type phaseLimitWait struct {
	lim harness.Limit   // the rolling limit, for the reset and its reason
	ph  config.Phase    // the phase whose harness was turned away
	a   harness.Adapter // that harness, so the probes run on the account that is limited
}

// deadPhase reports the CLA-386 dead-phase signature: the session's final step
// finished with reason "unknown" — opencode's marker for a session that died
// without producing a final answer — AND no branch is recorded on the task, so
// nothing durable came out of it. The two conjuncts together are what "produced
// nothing" means: the reason names the death, the missing branch rules out the
// one thing that would have made the phase survivable anyway. A session that
// pushed work and THEN died on an unknown reason has still produced something,
// and its phase keeps its checkpoint.
//
// It reads the reason off the adapter's Raw map rather than asking the adapter:
// the key is stable across the harnesses that observe it, and no Adapter method
// needs to grow for a signal only opencode emits today.
func deadPhase(res harness.Result) bool {
	r, _ := res.Raw[harness.FinishReasonKey].(string)
	return r == harness.FinishReasonUnknown && !res.Claim.HasWIP
}

// recordedBranches lists the branch names the plane's record carries for a
// phase result: every branch this run recorded via update_task(branch) that the
// plane accepted (the accepted Reports), plus the branch that came in on the
// claim itself (a resumed predecessor's recorded WIP, or the task's branch at
// claim time). These are the names a checkpoint can be verified against.
func recordedBranches(res harness.Result) []string {
	var out []string
	seen := map[string]bool{}
	add := func(b string) {
		if b != "" && !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	for _, rep := range res.Reports {
		if res.Claim.Names(rep.TaskID) || (rep.Ref != "" && res.Claim.Names(rep.Ref)) {
			add(rep.Branch)
		}
	}
	if res.Claim.HasWIP {
		add(res.Claim.Branch)
	}
	return out
}

// reviewGate is the review-shaped phase's seam contract (CLA-637): whether a
// durable review record is required, and the outcome as it stood BEFORE the
// phase spawned. The record requirement makes the review this-trip rather than
// last-trip the way B1 prescribes: the seam demands a record keyed on the run
// that wrote it, and a request_changes re-trip (or a takeover) runs under a
// fresh run id, so a record from a previous trip can never satisfy it. The
// baseline is kept for the pre-spawn read's tri-state and the refusal's
// diagnostic.
//
// read is the tri-state the freshness check needs: false means the baseline
// could not be read (no plane reader, a failed read, no task id on the carried
// claim). Since CLA-663 that is refused BEFORE the phase spawns — a full
// review session whose checkpoint the seam would refuse anyway is pure waste,
// and the claim would lapse into a takeover that re-runs implement and review.
// baseline is meaningful only when read is true.
//
// Since CLA-637 the gate itself is the DURABLE review record (B1), keyed on
// the run that wrote it: role names the record this phase's session writes
// (both sides read the same name out of the brief), and round is written back
// by checkpointEvidence with the round the matched record came from — the round
// the driver then closes to release the findings to the fix phase. Zero means
// no record matched.
type reviewGate struct {
	required bool
	read     bool
	baseline string
	role     string
	round    int
	// state is the matched record's plane state (sealed / open / void). The
	// driver closes only a SEALED round: a verifier releases its own round in
	// the same call that hands the task over (CLA-638), and a second close would
	// fail with no_open_round. Void records never match.
	state string
}

// reviewerRoleFor is the reviewer role of the review-shaped phase at idx
// (CLA-637): `review-N`, counting the review-shaped phases up to and including
// it, so two review phases in one sequence (a panel, CLA-639) write distinct
// records and the plane's one-row-per-role-per-round rule is not tripped. The
// role is a name both sides read: the brief hands it to the session and the
// seam gate keys the record on it. Empty for a phase that is not review-shaped.
func reviewerRoleFor(phases []config.Phase, idx int) string {
	if idx < 0 || idx >= len(phases) || !config.ReviewShaped(phases[idx].Prompt) {
		return ""
	}
	n := 0
	for i := 0; i <= idx; i++ {
		if config.ReviewShaped(phases[i].Prompt) {
			n++
		}
	}
	return "review-" + strconv.Itoa(n)
}

// matchingReviewRecord finds the durable review record a review-shaped phase's
// seam gate demands (CLA-637, B1): a record THIS RUN wrote under this reviewer
// role. The run id is the freshness key — a request_changes re-trip or a
// takeover runs under a fresh run id, so the previous trip's record can never
// satisfy the gate — and records voided by a lease expiry or takeover (B4)
// never match either. The tip requirement follows the checkpoint the driver
// made:
//
//   - verified: tip is the origin tip the driver read for the branch, and the
//     record must name exactly it — the revision reviewed must still BE the
//     tip the fix phase will find.
//   - unverified (CLA-576): the driver has no tip, so the reviewer established
//     one itself; the record must name one, because the fix phase compares it
//     with the worktree.
//
// A BRANCHLESS checkpoint does not reach this matcher at all: the plane accepts
// review writes only from the task's live holder, and a settled task has none,
// so that variant keeps the legacy `## Review` block gate (the CLA-634 decision
// on question 03a12578, option 1).
//
// The run ids are compared by runUUID: the claim hands the driver the run
// CREDENTIAL, while the plane stores and serves a record's run as the bare id.
//
// The newest match wins, so a retried attempt's record supersedes an earlier
// one in the same run.
func matchingReviewRecord(records []plane.ReviewRecordState, role, runID, tip string, requireTip bool) (plane.ReviewRecordState, bool) {
	var best plane.ReviewRecordState
	found := false
	want := runUUID(runID)
	for _, r := range records {
		if runUUID(r.RunID) != want || r.ReviewerRole != role || r.State == "void" {
			continue
		}
		switch {
		case tip != "":
			if r.ReviewedTip != tip {
				continue
			}
		case requireTip:
			if r.ReviewedTip == "" {
				continue
			}
		}
		if !found || r.Round > best.Round {
			best, found = r, true
		}
	}
	return best, found
}

// panelMemberRecordMatch asks the plane whether a panel member's durable record
// is already in the round, using the SAME evidence the review seam uses:
// matchingReviewRecord keyed by this run and the member's positional role, at
// the checkpoint's reviewed tip.
//
// The seam asks that question after a session ends. A member that writes its
// record and only THEN ends the sequence — a usage limit or a transient failure
// — returns from drainPhase before the seam runs, so the panel handler must ask
// it here or the member is recorded as a failure while its record sits in the
// round (CLA-639 fix pass 4). Best-effort: an unreadable task or an unwired
// reader leaves the member classified as failed, exactly as before this check.
func (d *Driver) panelMemberRecordMatch(ctx context.Context, t Target, res harness.Result, role string) (plane.ReviewRecordState, bool) {
	if role == "" || res.Claim.TaskID == "" {
		return plane.ReviewRecordState{}, false
	}
	st, ok := d.readTaskState(ctx, t, res.Claim.TaskID)
	if !ok {
		return plane.ReviewRecordState{}, false
	}
	return matchingReviewRecord(st.Reviews, role, res.Claim.RunID, res.CheckpointTip, res.CheckpointUnverified)
}

// runUUID is a run id's identity: the part before the first dot. Since CLA-509
// claim_task returns a run as the credential `<uuid>.<secret>`, but the plane
// keys everything it stores (review records, claimedByRun) on the bare uuid,
// so two ids name the same run exactly when their uuids match. A bare id (a
// run claimed before credentials, or one read back from the plane) is its own
// uuid. This is the plane's own split (parseRunCredential).
func runUUID(id string) string {
	if i := strings.IndexByte(id, '.'); i >= 0 {
		return id[:i]
	}
	return id
}

// isReviewHeading reports whether a line is the review block's heading, with
// trailing whitespace tolerated so a CRLF paste or a stray trailing space still
// reads as the heading.
func isReviewHeading(line string) bool {
	return strings.TrimRight(line, " \t\r") == config.ReviewRecordHeading
}

// reviewRecordLine reports whether outcome carries the review heading as its
// own line. The BRANCHLESS review-only checkpoint gates on it (CLA-634
// decision, question 03a12578, option 1): a settled task cannot write the
// durable record — the plane accepts review writes only from the live holder —
// so that variant keeps the legacy block gate. The exact line keeps a
// look-alike heading (`## Reviewer notes`, or prose that merely mentions the
// words) from standing in for the block. Trailing whitespace is ignored — the
// plane stores text as sent, and a CRLF paste or a stray trailing space is
// still the heading — while leading whitespace is not: a heading the session
// indented into a code block is not the block the fix phase will find.
func reviewRecordLine(outcome string) bool {
	for _, line := range strings.Split(outcome, "\n") {
		if isReviewHeading(line) {
			return true
		}
	}
	return false
}

// lastReviewBlock returns the outcome's text from its LAST `## Review` heading
// line to the end — the block a fix session is to read and apply. A task that
// went through a request_changes re-trip, or a takeover, can carry several
// blocks: both the review and the fix APPEND, so round 1's block stays in the
// outcome above round 2's, and "the `## Review` block" is ambiguous unless it
// means the last one (which is why every fix brief says so). The heading line
// itself is included: a block whose text is only its heading would otherwise
// decode to "", which is a substring of everything. It recognises the heading
// exactly as reviewRecordLine does, so the two agree on what a block is.
//
// The BRANCHLESS review-only checkpoint's stale-round guard reads it: a
// session that makes some other statusless outcome edit while leaving an
// earlier round's block as the last one recorded nothing this trip. Empty when
// the outcome carries no heading line at all; the gate checks reviewRecordLine
// first, so an empty return there is not a block, not an old one.
func lastReviewBlock(outcome string) string {
	start, offset := -1, 0
	for _, line := range strings.SplitAfter(outcome, "\n") {
		if isReviewHeading(strings.TrimSuffix(line, "\n")) {
			start = offset
		}
		offset += len(line)
	}
	if start < 0 {
		return ""
	}
	return outcome[start:]
}

// reviewFinding is one parsed line of a review record's `### Findings` list.
// The driver parses the block it told the review to write (CLA-635) to build
// the fix phase's graded list, so the fields here are an interface with the
// shape in config.reviewOnlyRecordShape, not a convenience.
type reviewFinding struct {
	ID       string // the record's own id, e.g. "R1"
	Severity string // canonical lowercase: must | should | nit
	Kind     string // MUST evidence kind (the token in parentheses), "" when absent
	Evidence string // MUST evidence payload (the text after the kind's colon), "" when absent
	// Rest is the finding line from just after the severity bracket onwards —
	// evidence prefix, location, summary and required fix — so the digest can
	// reprint the finding verbatim without re-joining fields it did not parse.
	Rest string
}

// downgraded reports whether B5's structural check demotes this finding: a
// `must` written without an evidence kind, or with one but no evidence payload,
// is treated as a `should`. Only presence is checked — whether the evidence is
// any GOOD is a verifier's call in the next round, never the driver's.
func (f reviewFinding) downgraded() bool {
	return f.Severity == "must" && (f.Kind == "" || f.Evidence == "")
}

// gradedSeverity is the severity the fix phase is held to, after the
// structural downgrade.
func (f reviewFinding) gradedSeverity() string {
	if f.downgraded() {
		return "should"
	}
	return f.Severity
}

var (
	// A finding line as config.reviewOnlyRecordShape prescribes it: an id, a
	// bracketed severity, then the rest of the line. Malformed lines are
	// skipped, not guessed at — the fix phase still reads the raw block.
	reviewFindingLineRE = regexp.MustCompile(`^\s*[-*]\s+(R\d+)\s+\[([A-Za-z]+)\](.*)$`)
	// The evidence prefix: `(<kind>): <evidence>`. A colon is required, so
	// `(gate) go test fails` — kind without the prescribed prefix — reads as
	// no kind at all and downgrades the MUST.
	reviewEvidenceRE = regexp.MustCompile(`^\s*\(([^)]*)\)\s*:\s*(.*)$`)
)

// parseReviewFindings extracts the findings of the NEWEST review block in
// outcome. Newest, not first: a request_changes re-trip or a takeover returns
// the task with an earlier round's block still in the outcome, and the fix
// phase applies THIS round's review. Empty when there is no block or no line
// in it parses.
func parseReviewFindings(outcome string) []reviewFinding {
	lines := strings.Split(outcome, "\n")
	start := -1
	for i, line := range lines {
		if isReviewHeading(line) {
			start = i
		}
	}
	if start < 0 {
		return nil
	}
	var out []reviewFinding
	for _, line := range lines[start+1:] {
		// A new level-2 heading ends the block. `### Findings` is
		// level-3 and does not match the "## " prefix.
		if strings.HasPrefix(strings.TrimRight(line, " \t\r"), "## ") {
			break
		}
		m := reviewFindingLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		sev := strings.ToLower(m[2])
		switch sev {
		case "must", "should", "nit":
		default:
			continue
		}
		f := reviewFinding{ID: m[1], Severity: sev, Rest: m[3]}
		if em := reviewEvidenceRE.FindStringSubmatch(f.Rest); em != nil {
			f.Kind = strings.TrimSpace(em[1])
			f.Evidence = strings.TrimSpace(em[2])
		}
		out = append(out, f)
	}
	return out
}

// reviewFindingsDigest is the driver's graded list of a review record's
// findings, prepended to the fix phase's prompt (CLA-635). It exists because
// B5's structural downgrade is the DRIVER's to apply: a `must` recorded without
// an evidence kind or payload is listed here as a SHOULD, so the fixer is held
// to the same grade the record will be read with later. The severities are the
// ones the fix brief's obligation speaks about; the raw block stays in the
// outcome for the record.
//
// Empty when the record carries no parseable findings — the fix brief then
// works from the raw block alone, exactly as before this existed.
func reviewFindingsDigest(outcome string) string {
	findings := parseReviewFindings(outcome)
	if len(findings) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("The driver read the review record and graded its findings structurally: a finding recorded as a " +
		"MUST with no evidence kind (a parenthesised token right after the severity) or no evidence text is listed " +
		"below as a SHOULD, not a MUST. The severities below are the ones this phase is held to:\n")
	for _, f := range findings {
		b.WriteString("- " + f.ID + " [" + strings.ToUpper(f.gradedSeverity()) + "]" + f.Rest)
		if f.downgraded() {
			b.WriteString(" [downgraded: recorded as a MUST with no evidence kind or payload]")
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	return b.String()
}

// checkpointEvidence reports whether the plane's record carries the phase's
// exit evidence, and returns the branch that evidence names (empty for the
// branch-less forms). Held-ness alone is NOT the evidence: since CLA-358 the
// driver's lease renewer renews a claim for a session's whole lifetime, so ANY
// session that claimed ends held, and "still holding" cannot distinguish a
// checkpoint from an empty exit (2026-08-24 fleet incident: two implement
// sessions claimed, cut a worktree, hit a silent model EOT, exited cleanly
// with real usage and no edits, and the seam advanced to review on the false
// premise).
//
// The evidence is ANY of three forms (CLA-497) — "the plane's record shows
// this phase did its job":
//
//   - (a) a branch recorded by this run whose tip really exists on the origin
//     remote — today's check, unchanged. A branch that was never pushed is not
//     evidence, and a check that could not run (no git, no repository
//     resolved) is not evidence either: a checkpoint is claimed only on the
//     verified present, never on the absence of a negative. That is the
//     deliberate difference from verifyDeliveries, which warns loudly on the
//     same failing check but does not gate anything on it.
//   - (b) the task having LEFT `ready` (in_review / done / parked / blocked):
//     its own session settled it on the plane. A no-code task can never
//     produce form (a) — there is no branch to record — and without this form
//     it is structurally incapable of checkpointing: the EZY-290 shape (a
//     one-token task-body edit) burned nine sessions in a claim -> empty-exit
//     -> release loop before this existed.
//   - (c) a DECLARED no-code delivery on the task: the same delivery.noCode
//     flag update_task requires to close such a task, read off the plane's
//     record. It counts whether or not the session still holds the lease.
//
// Forms (b) and (c) are read through the SAME single-task plane peek CLA-451
// introduced (planeExitRecord below), not a second implementation. The bounds
// CLA-457 set stay absolute: held-ness alone is still nothing (status
// in_progress proves only that someone holds the lease), and a session that
// claimed, recorded no branch, and left the task sitting in `ready` is STILL
// an empty exit — "nothing happened" — whatever this function would have said
// about its manners.
//
// CLA-576: an Unknown from form (a) is the ORIGIN being unreadable, not the
// branch being absent, and it is returned as its own outcome (unverified=true,
// ok=true) rather than flattened into the refusal. The phase is an UNVERIFIED
// checkpoint: the review phase spawns, carrying the recorded branch, and is
// told to verify the hand-off itself. A Fail — the origin was read and the
// branch is not there — keeps today's refusal exactly. The accepted cost is a
// narrow loosening of CLA-457: a session that recorded a branch it never
// pushed, during a remote outage, reaches a review phase, which is the
// backstop.
func (d *Driver) checkpointEvidence(ctx context.Context, t Target, res harness.Result, workdir string, review *reviewGate) (branch, tip string, ok bool, unverified bool, why string) {
	branches := recordedBranches(res)

	// Form (a): the recorded-branch check. Remember the first failure for the
	// diagnostic; fall through to the plane-record forms when nothing here
	// passes.
	firstB, firstWhy := "", ""
	// The first recorded branch whose check returned Unknown, kept apart from
	// the Fail diagnostic: it is the branch an unverified checkpoint carries
	// and the detail its log names (CLA-576). The bool is the discriminator, not
	// the detail string: an Unknown check with an empty Detail is still an
	// unreadable origin.
	unknownB, unknownWhy, unknownSeen := "", "", false
	// The branch form (a) accepted, and the origin tip the accepting check read
	// (CLA-637): the tip rides the checkpoint as the revision the review phase
	// verifies and records, and the seam matches the record's reviewedTip
	// against it. Recorded rather than returned on the spot: the review-record
	// gate below needs the plane read FIRST, and both the read and the gate are
	// one decision.
	foundBranch, foundTip, foundOK := "", "", false
	if len(branches) > 0 {
		if d.newVerifier == nil || workdir == "" {
			firstB, firstWhy = branches[0], fmt.Sprintf("branch %q recorded but could not be checked (no working tree)", branches[0])
		} else {
			v := d.newVerifier(workdir, d.cfg.AllowUncheckedPRFor(t.Name))
			vctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deliveryCheckTimeout)
			defer cancel()
			// ANY recorded branch that passes is evidence — a session may record a draft
			// branch early and the real one later. Remember the first failure for the
			// diagnostic, keep going, and only give up when no recorded branch could be
			// verified.
		branchScan:
			for _, b := range branches {
				out := v.Verify(vctx, delivery.Claim{Label: claimLabel(res.Claim), Branch: b})
				for _, c := range out.Checks {
					if c.Kind != delivery.BranchPushed {
						continue
					}
					if c.Status == delivery.Pass {
						foundBranch, foundTip, foundOK = b, c.Tip, true
						break branchScan
					}
					// Fail and Unknown both mean "not evidence" for THIS branch: an
					// unreachable branch, or one we could not check. Keep scanning; a
					// later recorded branch may be the one that is really there.
					if c.Status == delivery.Unknown && !unknownSeen {
						unknownB, unknownWhy, unknownSeen = b, c.Detail, true
					}
					if firstWhy == "" {
						firstB, firstWhy = b, c.Detail
					}
					break
				}
			}
		}
	}

	// Form (a) short-circuits for every phase that does not need the plane's
	// record: a verified branch is the strongest evidence there is, and asking
	// the plane anyway was never the contract. A review-shaped phase is the one
	// exception, because its checkpoint additionally needs the durable review
	// record (below) and its freshness baseline (reviewGate), so it pays the
	// extra reads.
	if foundOK && !review.required {
		return foundBranch, foundTip, true, false, ""
	}

	// Forms (b) and (c): the plane's own record of the task, read ONCE here so the
	// review-record gate below can use it without a second round-trip.
	record, recordRead, recordOK, recordWhy := d.planeExitRecord(ctx, t, res)

	// Resolve the pre-gate verdict from the strongest form that held. Form (a)
	// outranks the plane's record: a verified branch is the implementer's own
	// committed work, where a task that left `ready` may have been settled by
	// another channel.
	switch {
	case foundOK:
		branch, tip, ok, unverified = foundBranch, foundTip, true, false
	case recordOK:
		branch, tip, ok, unverified = "", "", true, false
	case unknownSeen:
		// CLA-576: the origin could not be READ for at least one recorded branch,
		// and no stronger form held. That is an unverified checkpoint, not an empty
		// exit. The unverified form is checked BEFORE the Fail-only diagnostic
		// because a branch absent from a readable origin and a branch whose origin
		// could not be read at all are different facts, and only the second one is a
		// checkpoint. The log line this feeds is the operator's whole account of why
		// the checkpoint is unverified, so an Unknown carrying no detail still gets a
		// sentence: an empty parenthetical would say nothing at exactly the moment
		// the log matters most. The bool, not this string, is the discriminator.
		if unknownWhy == "" {
			unknownWhy = "the branch check returned unknown without a detail"
		}
		branch, tip, ok, unverified, why = unknownB, "", true, true, unknownWhy
	}

	if ok {
		// The review-shaped gate (CLA-split; durable since CLA-637): a phase
		// whose ONLY deliverable is the review record must not checkpoint on the
		// implementer's branch alone. Its
		// inherited WIP (the branch the implement phase recorded) satisfies every
		// form above, so without this gate a review session that burned its turns,
		// hit its ceiling, or died on a silent EOT — having recorded nothing —
		// would still advance, and the next phase would be told to apply findings
		// that do not exist.
		//
		// The record must be THIS run's record, under THIS phase's reviewer
		// role (B1). Presence alone is not enough and was the shipped bug: a
		// task that already went implement -> review -> fix carries the
		// previous trip's `## Review` block in the outcome (and, since
		// CLA-636, its durable record too), and request_changes (or a takeover)
		// returns the task without clearing either. The run id is the freshness
		// key the plane embedded in the record and the criterion no stale
		// artefact can fake: a re-trip or a takeover is a NEW run, so only a
		// record written after this phase spawned can match. The reviewed tip
		// is checked against the tip the driver verified for the branch (or
		// demanded non-empty when the origin could not be read), so the fix
		// phase is only ever spawned on the revision that is still the tip.
		//
		// Fail-closed, like every other evidence form: a record that cannot be
		// read — or cannot be found — is not a review that happened. The cost
		// is a re-run of the review; the alternative is an unreviewed PR. An
		// unread BASELINE never reaches this gate: an unreadable baseline is
		// refused before the review spawns (CLA-663), so this gate judges
		// only tasks both reads could see.
		if review.required {
			if !recordRead {
				if branch == "" && !unverified {
					return branch, tip, false, false, fmt.Sprintf("the task could not be read after the review phase ended, so its %q block cannot be verified — the next phase would have nothing to apply", config.ReviewRecordHeading)
				}
				return branch, tip, false, false, "the task's review records could not be read after the review phase ended, so a durable review record cannot be verified — the next phase would risk running on a record that does not exist"
			}
			// CLA-634 decision (question 03a12578, option 1): a BRANCHLESS
			// (settled-task) review-only checkpoint cannot write the durable
			// record — the plane accepts review writes only from the task's
			// live holder, and no run holds a settled task — so this variant
			// keeps the legacy `## Review` block gate: changed since the spawn,
			// a heading on its own line, and a LAST block the spawn-time
			// baseline did not carry. Every branch-carrying variant still gates
			// on the record.
			if branch == "" && !unverified {
				switch {
				case record.Outcome == review.baseline:
					return branch, tip, false, false, fmt.Sprintf("the task's outcome is unchanged since the review phase spawned — the review recorded no new %q block, so the next phase would have nothing to apply", config.ReviewRecordHeading)
				case !reviewRecordLine(record.Outcome):
					return branch, tip, false, false, fmt.Sprintf("the review recorded no %q block in the task's outcome — the next phase would have nothing to apply", config.ReviewRecordHeading)
				case strings.Contains(review.baseline, lastReviewBlock(record.Outcome)):
					return branch, tip, false, false, fmt.Sprintf("the task's LAST %q block already appears in the outcome the review phase found — the review wrote no NEW block this trip, so the next phase would apply an earlier round's record", config.ReviewRecordHeading)
				}
				return branch, tip, true, false, ""
			}
			rec, matched := matchingReviewRecord(record.Reviews, review.role, res.Claim.RunID, tip, unverified)
			if !matched {
				expect := fmt.Sprintf("for reviewer role %q written by this run (%s)", review.role, runUUID(res.Claim.RunID))
				switch {
				case tip != "":
					expect += " with reviewed tip " + tip
				case unverified:
					expect += " that names a reviewed tip (the driver could not verify the origin remote, so the record must name the revision itself)"
				}
				detail := ""
				if record.Outcome == review.baseline {
					detail = " — the task's outcome is also unchanged since the phase spawned"
				}
				return branch, tip, false, false, "the task carries no durable review record " + expect + ", so the next phase would have nothing to apply" + detail
			}
			review.round = rec.Round
			review.state = rec.State
		}
		return branch, tip, true, unverified, why
	}

	// No form held. Compose the diagnostic from whichever legs ran: the plane's
	// answer when it gave one (prefixed by the branch leg's failure when both
	// ran), else the legacy branch-only reasons, unchanged.
	switch {
	case firstWhy != "" && recordWhy != "":
		return firstB, "", false, false, firstWhy + "; " + recordWhy
	case recordWhy != "":
		return "", "", false, false, recordWhy
	case firstWhy != "":
		return firstB, "", false, false, firstWhy
	case len(branches) > 0:
		return "", "", false, false, "none of the recorded branches could be verified on the origin remote"
	default:
		return "", "", false, false, "no branch recorded on the task"
	}
}

// planeExitRecord reads the plane's record of the claimed task for the two
// branch-independent exit-evidence forms (CLA-497): the task having LEFT
// `ready` (in_review / done / parked / blocked — its own session settled it),
// or a DECLARED no-code delivery (the plane's first-class "this shipped no
// code"). It is the SAME single-task get_task read CLA-451's checkpoint peek
// uses — plane.TaskStateSource, extended with the delivery flag — not a second
// implementation of the peek.
//
// Best-effort like every plane read: ok is false with an EMPTY why whenever
// the read cannot answer at all (no claimed task id, no wired reader, a plane
// blip), and the gate then judges on branch evidence alone, byte-for-byte as
// before this read existed. A read that ANSWERS but shows no evidence returns
// the reason the gate will log — the difference between "nothing happened"
// (task still ready or merely held) and a peek that never ran.
//
// The bounds are load-bearing: status "in_progress" proves only that SOMEONE
// holds the lease — held-ness alone was CLA-457's original false positive and
// stays worthless here — and a task still sitting in `ready` is still the
// empty exit, however orderly the session that said nothing about it.
func (d *Driver) planeExitRecord(ctx context.Context, t Target, res harness.Result) (st plane.TaskState, read bool, ok bool, why string) {
	taskID := res.Claim.TaskID
	src, wired := t.Releaser.(plane.TaskStateSource)
	if taskID == "" || !wired {
		return plane.TaskState{}, false, false, ""
	}
	sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	st, err := src.TaskState(sctx, taskID)
	switch {
	case err == nil:
		read = true
	case errors.Is(err, plane.ErrNotWired):
		return plane.TaskState{}, false, false, ""
	default:
		log.Printf("%scould not read the plane's record of %s for phase exit evidence (%v) — judging the exit on branch evidence alone", labelOf(t), taskID, err)
		return plane.TaskState{}, false, false, ""
	}
	switch st.Status {
	case "in_review", "done", "parked", "blocked":
		// Form (b): the task LEFT `ready`. Its session settled it on the plane,
		// which for a no-code task is the strongest statement there is.
		return st, read, true, ""
	}
	if st.DeliveryNoCode {
		// Form (c): a declared no-code delivery — the plane's own record that
		// this task's correct delivery is no code, held lease or not.
		return st, read, true, ""
	}
	return st, read, false, fmt.Sprintf("the plane's record shows no exit evidence either (status %q, no no-code delivery)", st.Status)
}

// readTaskOutcome reads the claimed task's outcome from the plane, best-effort:
// a failure returns read=false and the caller decides what to do without it.
// purpose and consequence compose the log line, because two callers read the
// same field for different reasons — the review seam's freshness baseline, and
// the fix phase's structural grading (CLA-635) — and a shared message would
// misname one of them.
func (d *Driver) readTaskOutcome(ctx context.Context, t Target, prev *harness.Result, purpose, consequence string) (string, bool) {
	if prev == nil || prev.Claim.TaskID == "" {
		return "", false
	}
	src, wired := t.Releaser.(plane.TaskStateSource)
	if !wired {
		return "", false
	}
	sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	st, err := src.TaskState(sctx, prev.Claim.TaskID)
	if err != nil {
		if !errors.Is(err, plane.ErrNotWired) {
			log.Printf("%scould not read the plane's record of %s %s (%v) — %s", labelOf(t), prev.Claim.TaskID, purpose, err, consequence)
		}
		return "", false
	}
	return st.Outcome, true
}

// planeSeamReadAttempts bounds the retries of a plane read whose failure fails
// the sequence CLOSED (CLA-663 for the review baseline, CLA-686 D3 for the
// verification round's spawn read): the initial attempt plus two more. The
// failure this recovers from is a plane blip — a dropped response, a read that
// timed out — and one retry pair around a 20-second read is enough for that
// without turning an outage into a long stall. Deliberately no sleep between
// attempts: the read's own timeout is the pacing, and the whole point of a
// bounded retry here is to avoid paying for a session (or discarding one
// already paid for), not to wait for a sick plane.
const planeSeamReadAttempts = 3

// planeTaskState reads the plane's record of one task, retrying a transient
// failure up to `attempts` times. It is the single implementation behind every
// seam read of the task — the review baseline and the verification round's
// fail-closed reads share it precisely so their retry behaviour cannot drift
// apart (CLA-686 D3: the verify spawn's read claimed to mirror the baseline's
// and did not, one attempt against three).
//
// ok is false when there is no task id, no wired reader, or every attempt
// failed; ErrNotWired is never retried, because asking an unwired client again
// cannot wire it. `purpose` names the read in the log line only; the caller
// decides what an unreadable task MEANS.
func (d *Driver) planeTaskState(ctx context.Context, t Target, taskID string, attempts int, purpose string) (plane.TaskState, bool) {
	if taskID == "" {
		return plane.TaskState{}, false
	}
	src, wired := t.Releaser.(plane.TaskStateSource)
	if !wired {
		return plane.TaskState{}, false
	}
	lastErr := ""
	for attempt := 1; attempt <= attempts; attempt++ {
		if ctx.Err() != nil {
			return plane.TaskState{}, false
		}
		sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		st, err := src.TaskState(sctx, taskID)
		cancel()
		switch {
		case err == nil:
			return st, true
		case errors.Is(err, plane.ErrNotWired):
			// No wiring to retry against; the caller refuses at its own guard.
			return plane.TaskState{}, false
		default:
			lastErr = err.Error()
		}
	}
	if attempts > 1 {
		log.Printf("%scould not read the plane's record of %s %s, after %d attempts (%s)", labelOf(t), taskID, purpose, attempts, lastErr)
	} else {
		log.Printf("%scould not read the plane's record of %s %s (%s)", labelOf(t), taskID, purpose, lastErr)
	}
	return plane.TaskState{}, false
}

// readReviewBaseline reads the task's outcome as it stands BEFORE the review
// phase spawns, so the seam can require the record to have changed. The task id
// comes from the carried claim (the predecessor phase's), which is exact for a
// resumed phase and is why this runs inside drainPhase, where `prev` is still
// in hand.
//
// Best-effort like every other plane read, with one difference CLA-663 added:
// the read is RETRIED up to planeSeamReadAttempts times, because a blip here
// does not degrade to "judge on other evidence" — it fails the whole sequence
// closed. A failure still returns read=false, and the caller refuses to spawn
// the review rather than letting it run a session whose checkpoint the seam
// would reject.
func (d *Driver) readReviewBaseline(ctx context.Context, t Target, prev *harness.Result) (string, bool) {
	if prev == nil {
		return "", false
	}
	st, ok := d.planeTaskState(ctx, t, prev.Claim.TaskID, planeSeamReadAttempts, "before the review phase spawned")
	if !ok {
		return "", false
	}
	return st.Outcome, true
}

// closeReviewRound releases a review round's sealed findings (CLA-637) by
// calling update_task(review: {closeRound: n}) as the holding run. The round is
// the one the seam gate MATCHED, never a number predicted at spawn time: the
// plane assigns rounds, and the driver reads them back off the record — which
// is also what keeps this correct across void rounds and panel members.
//
// The close is deliberately a no-op-with-an-error for a client that cannot
// close (an unwired plane, a test double): the caller turns this into a refused
// checkpoint, because a fix phase spawned against findings the plane still
// holds sealed is a session spawned to apply nothing.
func (d *Driver) closeReviewRound(ctx context.Context, t Target, res harness.Result, round int) error {
	closer, ok := t.Releaser.(plane.ReviewRoundCloser)
	if !ok {
		return errors.New("the plane client does not support closing review rounds")
	}
	if res.Claim.TaskID == "" || res.Claim.RunID == "" {
		return fmt.Errorf("no held claim to close review round %d with", round)
	}
	return closer.CloseReviewRound(ctx, res.Claim.TaskID, res.Claim.RunID, round)
}

// readTaskState reads the plane's record of one task, for the verification
// round's BEST-EFFORT reads (CLA-638): the fix phase's panel digest and its
// open-MUST digest, where a failure degrades to "judge on other evidence" — the
// fix brief's own instruction to read `reviews.openMustIds` itself. Best-effort
// like every other plane read: ok is false when there is no task id, no wired
// reader, or the read fails. The reads that fail the sequence CLOSED instead use
// readTaskStateRetry, which shares this implementation with a retry bound.
func (d *Driver) readTaskState(ctx context.Context, t Target, taskID string) (plane.TaskState, bool) {
	return d.planeTaskState(ctx, t, taskID, 1, "for the verification round")
}

// readTaskStateRetry is readTaskState for the verification round's FAIL-CLOSED
// reads (CLA-686 D3): the round's spawn read, and the read the driver makes
// after a closed round to judge its open-MUST set. A failure on any of them
// fail-closes the sequence — the round cannot be told what to adjudicate, or
// cannot be judged — so one plane blip there discards work that has already
// been paid for: at the spawn, the fix round that just ran (the sequence ends,
// the claim is released and the next claim re-runs the phases); after a closed
// round, the verification round itself. That is exactly why the review baseline
// read these claim parity with retries, and they now share its bound
// (planeSeamReadAttempts) rather than taking a single attempt.
func (d *Driver) readTaskStateRetry(ctx context.Context, t Target, taskID, purpose string) (plane.TaskState, bool) {
	return d.planeTaskState(ctx, t, taskID, planeSeamReadAttempts, purpose)
}

// findingIDRound parses the round prefix out of a plane finding id
// (`<round>.<reviewer role>.<n>`, B13). The plane assigns and serves these
// namespaced ids (CLA-636); the driver reads the round back rather than
// tracking one of its own, so the derivation survives a takeover.
func findingIDRound(id string) (int, bool) {
	dot := strings.Index(id, ".")
	if dot <= 0 {
		return 0, false
	}
	n, err := strconv.Atoi(id[:dot])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// verificationDigest is the driver's brief for a verification round (CLA-638):
// the prior open MUST ids, their findings, and the delta base, read from the
// plane's released records and PREPENDED to the verify brief the way the fix
// phase's graded list is. It exists because ids and rounds are plane state, not
// config: the driver reads them after the previous round closed and hands the
// session exactly what it must adjudicate.
//
// A record still sealed or void contributes nothing: a sealed round's findings
// are withheld by design (B2), and a void round never happened (B4). An empty
// open list is not an empty digest — the session is still told the delta base
// and that the gate currently reads as clear, so a verifier that sees a problem
// states it rather than silently passing a hand-over it cannot explain.
func verificationDigest(records []plane.ReviewRecordState, openMustIDs []string, recordsTotal int) string {
	byID := map[string]plane.ReviewFindingState{}
	newestRound, newestTip, newestCycle := 0, "", 0
	for _, r := range records {
		if r.State == "sealed" || r.State == "void" {
			continue
		}
		if r.Round > newestRound {
			newestRound, newestTip = r.Round, r.ReviewedTip
		}
		if r.Cycle > newestCycle {
			newestCycle = r.Cycle
		}
	}
	// cycleOf maps a finding to its record's cycle, so the settled list below is
	// this review cycle's alone: a human verdict starts a new cycle, and the
	// previous cycle's MUSTs were judged by that verdict, not by this round.
	cycleOf := map[string]int{}
	for _, r := range records {
		if r.State == "sealed" || r.State == "void" {
			continue
		}
		for _, f := range r.Findings {
			byID[f.ID] = f
			cycleOf[f.ID] = r.Cycle
		}
	}
	// The plane's openMustIds is the authoritative gate input (CLA-636); the
	// records supply the finding detail. Union them, so a findings block
	// truncated by the read budget still yields every id it must adjudicate.
	openIDs := append([]string(nil), openMustIDs...)
	seen := map[string]bool{}
	for _, id := range openMustIDs {
		seen[id] = true
	}
	for id, f := range byID {
		if f.Severity == "must" && f.Status == "open" && !seen[id] {
			openIDs = append(openIDs, id)
			seen[id] = true
		}
	}
	sort.Strings(openIDs)
	// CLA-671: the fix phase marks the MUSTs it answers `fixed` or `refuted`
	// itself, so a verifier handed only the OPEN list never sees a refutation —
	// it changes no code, so the delta does not show it either — and a wrong one
	// rode to in_review unexamined. Every MUST of this cycle already marked
	// settled is listed for the verifier to confirm (leave it) or reject (set it
	// back to open). Bounded: the round cap keeps a cycle to three rounds.
	var settledIDs []string
	for id, f := range byID {
		if f.Severity != "must" || f.Status == "open" || seen[id] {
			continue
		}
		if newestCycle > 0 && cycleOf[id] != newestCycle {
			continue
		}
		settledIDs = append(settledIDs, id)
	}
	sort.Strings(settledIDs)
	// The plane serves only the newest records, and their findings within a byte
	// budget (get_task's reviews block); open MUST ids survive the trim, settled
	// ones do not. Say what was left out, so a verifier never reads a partial
	// settled list as the whole of it.
	var unserved []string
	for _, r := range records {
		if r.State == "sealed" || r.State == "void" {
			continue
		}
		if newestCycle > 0 && r.Cycle != newestCycle {
			continue
		}
		if r.FindingsTotal > len(r.Findings) {
			unserved = append(unserved, fmt.Sprintf("round %d: %d of %d finding(s) not served", r.Round, r.FindingsTotal-len(r.Findings), r.FindingsTotal))
		}
	}
	sort.Strings(unserved)
	if recordsTotal > len(records) {
		unserved = append(unserved, fmt.Sprintf("%d older review record(s) not served", recordsTotal-len(records)))
	}
	var b strings.Builder
	b.WriteString("The driver read the released durable review records before this round. ")
	if newestRound > 0 {
		tip := newestTip
		if tip == "" {
			tip = "(the record names no tip)"
		}
		fmt.Fprintf(&b, "The newest released round is %d and its reviewed tip is %s — that tip is the fix delta's base (`git diff <reviewedTip>..HEAD`).\n", newestRound, tip)
	} else {
		b.WriteString("No RELEASED review record is on the task. If the review round did not run, that is a failed hand-off to report in your record and hand over to a human; do not invent findings.\n")
	}
	if len(openIDs) == 0 && len(settledIDs) == 0 && len(unserved) == 0 {
		b.WriteString("The plane currently shows NO OPEN MUST on a released record. Still verify the fix delta of the newest round and its regression surface; if it is clean, carry the terminal step. Everything you find goes in this round's own record.\n\n")
		return b.String()
	}
	switch {
	case len(openIDs) == 0 && len(settledIDs) == 0:
		b.WriteString("The plane currently shows NO OPEN MUST on a released record, but it did not serve every finding (below).\n")
	case len(openIDs) == 0:
		b.WriteString("The plane currently shows NO OPEN MUST, but the MUST(s) below are only MARKED settled; adjudicate each before you carry the terminal step.\n")
	default:
		b.WriteString("The following MUST(s) are OPEN and this round MUST adjudicate each BY ITS PLANE ID — resolved (status \"fixed\"), resolved by dispatch (status \"filed\"), refutation accepted (status \"refuted\"), or left \"open\" with a recorded reason:\n")
		for _, id := range openIDs {
			b.WriteString(digestFindingLine(id, byID))
		}
	}
	if len(settledIDs) > 0 {
		b.WriteString("The following MUST(s) of this review cycle are already MARKED settled on the plane — by the fix phase, or by an earlier verification round. A fixer's own marking is its claim, not a verdict, and a refutation changes no code, so the fix delta will not show it: check each one against HEAD and its stated evidence. CONFIRM it by leaving its status as it is. REJECT it by setting its status back to \"open\" through update_task(review: {findingUpdates: [...]}) and recording why in this round's record; a rejected MUST is open, so the gate does not pass and the driver spawns another fix:\n")
		for _, id := range settledIDs {
			b.WriteString(digestFindingLine(id, byID))
		}
	}
	if len(unserved) > 0 {
		b.WriteString("The plane did NOT serve every review finding (" + strings.Join(unserved, "; ") + "), so a MUST marked settled in what was left out is not listed above. Before you carry the terminal step, read the full records (export_project carries every record in full, and the task's console page renders them) and adjudicate every MUST of this cycle marked settled there too. If you cannot read them, do NOT carry the terminal step: record in this round's record that it could not see every settled MUST, and end, so a human looks.\n")
	}
	b.WriteString("\n")
	return b.String()
}

// planeUnparkOption is the plane's own unpark offer, byte for byte
// (ESCALATION_UNPARK_OPTION, apps/web/src/server/mcp/stale.ts). The console acts
// on an answered question by VALUE: when the operator picks exactly this string,
// it sets the task back to ready. Any other wording is prose the plane does not
// act on, so the round-cap park offers this one verbatim for the unpark path.
const planeUnparkOption = "Set it back to `ready` — the blocker is gone, let the fleet try again"

// mergedFindingsDigest is the fix phase's work list when the review ran as a
// PANEL (CLA-639, B11): every finding from every member's released record, in
// one list, with the union rule the design settled on (A1).
//
//   - Every MUST from ANY member is open and printed by its plane id. Consensus
//     never filters a MUST out — the evidence that motivated the panel (section
//     1.2) is two blockers a single reviewer missed entirely, so the union is the
//     gate.
//   - SHOULD and NIT are listed with the members that raised them, so the fixer
//     can see which are opinions repeated by two reviewers and which are one
//     reviewer's preference. They are recorded, never gating.
//
// The driver builds the list, never the fixer (B11/D3): the fixer applies it and
// has no vote on severity — a MUST it disagrees with is refuted through the
// plane, not downgraded here.
//
// expected is the panel's reviewer roles, in order, as the driver resolved them.
// A role with no record in round is named INCOMPLETE: that is the member a
// dead, capped or failed session never got to record, and the fixer must know
// the list is the union of the reviewers that RAN, not of the panel that was
// meant to. round is the panel's round; 0 means no record was matched at all.
//
// Round and status are BOTH filters (CLA-639 S1). The round is the one this
// panel actually wrote: listing every released record would relist an earlier
// cycle's findings after a verify rewind, and a member that recorded in an
// earlier round would count as having recorded THIS one. The status filter
// drops every finding the plane already shows settled — fixed, refuted or filed
// by the fix phase's own findingUpdates, or by a verifier — because handing the
// fixer a settled MUST back as open work is how a fix round spends itself
// re-fixing what already landed. An open (`""`/`open`) finding is the list.
//
// Empty only when there is nothing to grade (no member recorded and none is
// missing) — the shipped single-review path then keeps its own digest, so a
// config with no panel is byte-for-byte unchanged.
//
// failed carries, per reviewer role, the reason that member recorded nothing
// (CLA-639 M4): naming the missing member is not enough for the fixer to judge
// whether the short list is bad luck or a member that never ran.
func mergedFindingsDigest(records []plane.ReviewRecordState, expected []string, round int, failed map[string]string) string {
	type merged struct {
		finding plane.ReviewFindingState
		record  plane.ReviewRecordState
	}
	byID := map[string]merged{}
	var ids []string
	recorded := map[string]bool{}
	var unserved []string
	settled := 0
	for _, r := range records {
		if r.State == "sealed" || r.State == "void" || (round > 0 && r.Round != round) {
			continue
		}
		// Only a RELEASED record counts as this member having recorded — a sealed
		// one exists but its findings are withheld, so the fixer is seeing the
		// panel's shape without its content. It counts even when EVERY finding in
		// it is settled: the member did record, and saying it did not would read
		// as a lost reviewer.
		recorded[r.ReviewerRole] = true
		if r.FindingsTotal > len(r.Findings) {
			unserved = append(unserved, fmt.Sprintf("%s: %d of %d finding(s) not served", r.ReviewerRole, r.FindingsTotal-len(r.Findings), r.FindingsTotal))
		}
		for _, f := range r.Findings {
			// A finding the plane already shows settled is not open work. The
			// statuses are the plane's own (CLA-636): "", "open", "fixed",
			// "refuted", "filed", and any future one is treated as NOT open — a
			// status the driver does not know is not evidence that work remains.
			switch f.Status {
			case "", "open":
			default:
				settled++
				continue
			}
			if _, seen := byID[f.ID]; !seen {
				ids = append(ids, f.ID)
			}
			byID[f.ID] = merged{finding: f, record: r}
		}
	}
	var missing []string
	for _, role := range expected {
		if !recorded[role] {
			missing = append(missing, role)
		}
	}
	// Nothing to grade at all: no finding to list, no member missing, and nothing
	// the plane shows settled either — the shipped single-review path then keeps
	// its own digest, byte for byte. A round whose findings are ALL settled is
	// not nothing: the fixer is told what this round holds and that every one of
	// them is already answered.
	if len(ids) == 0 && len(missing) == 0 && settled == 0 {
		return ""
	}
	sort.Strings(ids)
	sort.Strings(unserved)

	var b strings.Builder
	b.WriteString("The driver read the released durable review records of this review panel before this fix round. ")
	b.WriteString("This is the MERGED list from every member that recorded: every MUST from ANY member is open and is yours to fix or refute by its plane id — a MUST raised by one reviewer is never dropped because another did not raise it. ")
	b.WriteString("SHOULD and NIT findings are annotated with the members that raised them; they are recorded, never gating.\n")
	if round > 0 {
		fmt.Fprintf(&b, "The list is round %d's records only — an earlier round's findings, and anything already marked fixed, refuted or filed on the plane, are not relisted here.\n", round)
	}
	if settled > 0 {
		fmt.Fprintf(&b, "%d finding(s) of this round are already MARKED settled on the plane and are not listed; `reviews.openMustIds` is the plane's own open set, and a settled marking you disagree with is refuted through update_task(review: {findingUpdates: [...]}), not by re-fixing it blind.\n", settled)
	}
	// CLA-639 M5: a panel member writes no `## Review` block — its record is the
	// durable one — so the fixer must not wait for a block to preserve, and its
	// own outcome is where this merged list reaches the task.
	b.WriteString("No panel member wrote a `## Review` block into the task's outcome (each writes only its sealed record), so there is no block to keep verbatim and no legacy copy to rely on: your outcome is the only place this round's result reaches the task. Keep everything the outcome already carries unchanged, answer EVERY finding above in it by its plane id (severity, one line on what it was, and what you did about it), and add your `### Fixes` list.\n")
	if len(missing) > 0 {
		fmt.Fprintf(&b, "The panel was INCOMPLETE: %s recorded no review this round (dead, capped or failed before recording), so the merged list below is the union of the reviewers that ran, not of the panel that was configured.\n", strings.Join(missing, ", "))
		for _, role := range missing {
			if why := failed[role]; why != "" {
				fmt.Fprintf(&b, "- %s: %s\n", role, why)
			}
		}
	}
	if len(unserved) > 0 {
		fmt.Fprintf(&b, "The plane did not serve every finding (%s); read the full records before you close the round.\n", strings.Join(unserved, "; "))
	}
	b.WriteString("\n")
	for _, sev := range []string{"must", "should", "nit"} {
		var group []merged
		for _, id := range ids {
			if strings.EqualFold(byID[id].finding.Severity, sev) {
				group = append(group, byID[id])
			}
		}
		if len(group) == 0 {
			continue
		}
		fmt.Fprintf(&b, "%s (%d):\n", strings.ToUpper(sev), len(group))
		for _, m := range group {
			b.WriteString(mergedFindingLine(m.finding, m.record))
			if sev != "must" {
				fmt.Fprintf(&b, "  [raised by %s]\n", m.record.ReviewerRole)
			}
		}
	}
	b.WriteString("\n")
	return b.String()
}

// mergedFindingLine is one finding as the merged list prints it: its plane id,
// reviewer, location, summary and required fix — the same fields the shipped
// digest prints, plus the member, because "who raised this" is the field the
// panel exists to make visible.
func mergedFindingLine(f plane.ReviewFindingState, r plane.ReviewRecordState) string {
	loc := f.File
	if f.Line > 0 {
		loc = fmt.Sprintf("%s:%d", f.File, f.Line)
	}
	if loc == "" {
		loc = "(no location)"
	}
	summary := strings.TrimSpace(f.Summary)
	required := strings.TrimSpace(f.RequiredFix)
	if required == "" {
		required = "(none stated)"
	}
	line := fmt.Sprintf("- %s [%s, round %d, %s] %s Required fix: %s", f.ID, r.ReviewerRole, r.Round, loc, summary, required)
	if evidence := strings.TrimSpace(f.Evidence); evidence != "" {
		kind := strings.TrimSpace(f.EvidenceKind)
		if kind == "" {
			kind = "evidence"
		}
		line += fmt.Sprintf(" [%s: %s]", kind, evidence)
	}
	return line + "\n"
}

// panelRoles lists the reviewer roles a fix phase's prompt should treat as the
// panel: the member phases immediately before it in the CAPTURED sequence.
// Empty when the phase does not follow a panel, which is the shipped
// single-review shape.
//
// The role is POSITIONAL (`reviewerRoleFor`), never the member's log label. The
// durable record and the seam gate are keyed on the positional role, so a named
// member (`alpha`) writes `review-1`; returning the label here would make the
// merged list look for an `alpha` record and report a member that recorded as
// INCOMPLETE (CLA-639).
func panelRoles(phases []config.Phase, fixIdx int) []string {
	var roles []string
	for i := fixIdx - 1; i >= 0 && phases[i].IsPanelMember(); i-- {
		roles = append([]string{reviewerRoleFor(phases, i)}, roles...)
	}
	return roles
}

// openMustFixDigest is the fix phase's work list across rounds (CLA-671): every
// MUST the plane shows open on any released record, with its detail. Empty when
// none is open, which on a first fix round is the review's own list anyway.
func openMustFixDigest(records []plane.ReviewRecordState, openMustIDs []string) string {
	if len(openMustIDs) == 0 {
		return ""
	}
	byID := map[string]plane.ReviewFindingState{}
	for _, r := range records {
		if r.State == "sealed" || r.State == "void" {
			continue
		}
		for _, f := range r.Findings {
			byID[f.ID] = f
		}
	}
	ids := append([]string(nil), openMustIDs...)
	sort.Strings(ids)
	var b strings.Builder
	b.WriteString("The driver read the plane before this fix round. These MUST(s) are OPEN on released review records, from ANY round, and every one is yours to fix or refute by its plane id — not only the findings in the newest record:\n")
	for _, id := range ids {
		b.WriteString(digestFindingLine(id, byID))
	}
	b.WriteString("\n")
	return b.String()
}

// digestFindingLine is one MUST as the verification digest lists it: id, round,
// location, what is wrong and the required fix, plus — for a finding already
// marked settled — its status and the evidence the marking carries.
func digestFindingLine(id string, byID map[string]plane.ReviewFindingState) string {
	f, ok := byID[id]
	if !ok {
		return fmt.Sprintf("- %s [finding detail not served — read it from `reviews.records`]\n", id)
	}
	loc := f.File
	if f.Line > 0 {
		loc = fmt.Sprintf("%s:%d", f.File, f.Line)
	}
	if loc == "" {
		loc = "(no location)"
	}
	round := "?"
	if r, ok := findingIDRound(f.ID); ok {
		round = strconv.Itoa(r)
	}
	summary := strings.TrimSpace(f.Summary)
	required := strings.TrimSpace(f.RequiredFix)
	if required == "" {
		required = "(none stated)"
	}
	line := fmt.Sprintf("- %s [round %s, %s] %s Required fix: %s", f.ID, round, loc, summary, required)
	switch f.Status {
	case "open", "":
	case "fixed":
		line += fmt.Sprintf(" MARKED fixed (fixCommit: %s).", orNone(f.FixCommit))
	case "refuted":
		line += fmt.Sprintf(" MARKED refuted (reason: %s).", orNone(f.RefutedReason))
	case "filed":
		line += fmt.Sprintf(" MARKED filed (task: %s).", orNone(f.FiledTaskRef))
	default:
		line += fmt.Sprintf(" MARKED %s.", f.Status)
	}
	return line + "\n"
}

// orNone is a digest field's text, or a visible placeholder when the plane
// carries none, so a marking with no evidence reads as exactly that.
func orNone(s string) string {
	if s = strings.TrimSpace(s); s == "" {
		return "none given"
	}
	return s
}

// branchlessVerificationDigest is verificationDigest for a BRANCHLESS task
// (CLA-638 × CLA-670): the branchless review writes no durable record (question
// 03a12578, option 1), so there are no plane finding ids for the round to
// adjudicate. The digest therefore hands the verifier the LAST `## Review`
// block itself — the block IS this shape's round record — and says so up front,
// because the empty record set is expected here and must not read as a failed
// hand-off.
func branchlessVerificationDigest(outcome string) string {
	block := lastReviewBlock(outcome)
	if block == "" {
		return "The driver read the task before this round. This is a BRANCHLESS task (its correct delivery is no code) and the outcome carries NO `" +
			config.ReviewRecordHeading + "` block: the review round either did not run or recorded nothing. Report that as a failed hand-off in your own block and hand over to a human; do not invent findings.\n\n"
	}
	return "The driver read the task before this round. This is a BRANCHLESS task (its correct delivery is no code): the branchless review writes no durable record and there are no plane finding ids (question 03a12578, option 1), so the LAST `" +
		config.ReviewRecordHeading + "` block below IS this round's record — adjudicate it in its own terms, not by a plane id. The fix round amended it; verify the amendment.\n\n" + block + "\n"
}

// resolvedFindingIDs lists the MUST ids on released records that a verifier has
// ALREADY marked resolved (fixed, filed or refuted). The driver remembers them
// across the round loop: an id that is open again in a later round was reopened
// after a verifier marked it resolved, which is non-convergence and parks the
// task (A6). Sealed and void records contribute nothing, exactly as in the
// digest.
func resolvedFindingIDs(records []plane.ReviewRecordState) []string {
	var out []string
	for _, r := range records {
		if r.State == "sealed" || r.State == "void" {
			continue
		}
		for _, f := range r.Findings {
			if f.Severity == "must" && f.Status != "" && f.Status != "open" {
				out = append(out, f.ID)
			}
		}
	}
	return out
}

// openMustDetails pairs each open MUST id with its served summary, so the park
// question carries what is actually open rather than only the ids. A finding
// the read did not serve is still named by id.
func openMustDetails(openIDs []string, records []plane.ReviewRecordState) []string {
	summary := map[string]string{}
	for _, r := range records {
		if r.State == "sealed" || r.State == "void" {
			continue
		}
		for _, f := range r.Findings {
			if s := strings.TrimSpace(f.Summary); s != "" {
				summary[f.ID] = s
			}
		}
	}
	out := make([]string, 0, len(openIDs))
	for _, id := range openIDs {
		if s, ok := summary[id]; ok {
			out = append(out, id+" — "+s)
		} else {
			out = append(out, id)
		}
	}
	return out
}

// nonConvergenceReason reports why the round loop must stop and park rather than
// spawn another fix (CLA-638, A6): a MUST a previous verifier marked resolved
// that the latest round has open again, or a MUST still open after two fix
// attempts (its id's round prefix is at least two behind the round that just
// closed). Empty means the loop may continue. It reads plane state plus the
// driver's in-memory resolved set, so a takeover (empty set) loses the reopened
// half but the round-prefix half still holds.
func nonConvergenceReason(open []string, round int, resolved map[string]bool) string {
	for _, id := range open {
		if resolved[id] {
			return fmt.Sprintf("finding %s was marked resolved by an earlier verification round and is open again", id)
		}
	}
	for _, id := range open {
		if r, ok := findingIDRound(id); ok && round-r >= 2 {
			return fmt.Sprintf("finding %s (raised in round %d) is still open in round %d after two fix attempts", id, r, round)
		}
	}
	return ""
}

// parkVerification parks a task whose verification round cannot proceed
// (CLA-638): the round cap with an open MUST (B7), the non-convergence breaker
// (A6), or a round the driver could not judge. The question CARRIES THE RECORDS
// — the open ids, the round, and where the durable records live — because the
// operator is being asked to overrule the stopping rule, and that needs the
// evidence.
//
// Non-blocking `clarification`, matching parkDeadPhase: the task is already
// parked, so a blocking question would be the wrong shape (any answer would
// un-block it to `ready`, handing the same open MUSTs straight back to a fleet
// whose cycle counter has not moved — the cap would park it again). The park's
// outcome stands alone if the question insert fails.
func (d *Driver) parkVerification(ctx context.Context, t Target, res harness.Result, reason string, round, counted int, open, details []string) {
	if res.Claim.TaskID == "" {
		log.Printf("%sverification park: no claim observed, so there is no task to park (%s)", labelOf(t), reason)
		return
	}
	pk, ok := t.Releaser.(plane.ParkAPI)
	if !ok {
		log.Printf("%sCANNOT PARK %s — the verification round cannot proceed (%s), but the releaser cannot park or raise a question; the task stays in the queue and the next claim will retry it again",
			labelOf(t), claimLabel(res.Claim), reason)
		return
	}
	ref := claimLabel(res.Claim)
	openList := "none"
	detailList := "none"
	if len(open) > 0 {
		openList = strings.Join(open, ", ")
	}
	if len(details) > 0 {
		detailList = "\n- " + strings.Join(details, "\n- ")
	}
	outcome := fmt.Sprintf(
		"Parked on the review stopping rule: %s. Round %d of the %d-round cap this cycle (non-void rounds counted by the plane); open MUSTs: %s. The durable review records are on the task (`reviews.records`). Setting this task back to ready does not start a new review cycle (the plane counts cycles by human verdicts, which it records only on an in_review task): a re-run gets one more review, fix and verification round, and parks again if any MUST is still open after it. The operator unparks it for that attempt, settles the open MUST(s) by hand, re-files the work as a new task, or drops it.",
		reason, round, config.ReviewRoundCap, openList)
	body := fmt.Sprintf(
		"**%s needs a human decision: the fix-verification loop stopped.** %s\n\n"+
			"Round %d closed; the plane has counted %d non-void round(s) in this review cycle and the cap is %d. "+
			"Open MUST finding(s): %s\n\n"+
			"The evidence is in the task's durable review records (`reviews.records`; every finding is `must`/`should`/`nit` with its status, and the legacy `## Review` block is in the outcome). "+
			"The stopping rule exists so two agents cannot argue past a bound: the reviewer's findings, the fixer's answers and the verifier's adjudication are all on the task, and the disagreement is the question.\n\n"+
			"Setting this task back to `ready` does NOT give it another review cycle. The plane opens a new cycle only on a human verdict, and it records a verdict only on an `in_review` task, so a re-run stays in this cycle: it gets exactly one more review, fix and verification round, and any MUST still open after that verification round parks the task again. For more rounds than that, re-file the work as a new task with whatever re-scoping the open MUST(s) need.",
		ref, reason, round, counted, config.ReviewRoundCap, detailList)
	options := []string{
		planeUnparkOption,
		"I will settle the open MUST(s) myself — fix it by hand and close the task",
		"Re-file it as a new task, re-scoped so the open MUST(s) can be met — archive this one",
		"Drop it — the task is not worth another round",
	}
	pctx, pcancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer pcancel()
	if err := pk.Park(pctx, res.Claim.TaskID, res.Claim.RunID, outcome); err != nil {
		log.Printf("%sverification park failed for %s: %v — the task is left for the next claim to retry it again",
			labelOf(t), ref, err)
		return
	}
	if err := pk.AskQuestion(pctx, res.Claim.TaskID, body, options, false, "clarification"); err != nil {
		log.Printf("%sparked %s — %s, but the question for the operator could not be filed: %v",
			labelOf(t), ref, reason, err)
		return
	}
	log.Printf("%sparked %s — %s; question filed for the operator", labelOf(t), ref, reason)
}

// verifiedBranch returns the branch the previous phase recorded — the hand-off
// the successor's brief names (CLA-457). The seam folds the branch onto the
// carried claim when it checkpoints, so for a checkpointed phase the claim's
// branch is the one the evidence gate judged; the recorded-Branches fallback
// covers a claim that was never re-judged (a resumed-with-WIP predecessor, or
// a defensive hole). The predecessor's checkpoint is gated on this same
// evidence, so an empty return here is a misconfigured sequence announcing
// itself via a standing {{branch}} placeholder rather than a plausible runtime
// state.
//
// CLA-576: for an UNVERIFIED checkpoint — the origin could not be read — the
// returned branch is recorded but NOT verified, and the caller that builds the
// successor's brief must read Result.CheckpointUnverified and select the
// unverified brief: this function names the branch, it does not vouch for it.
func verifiedBranch(res *harness.Result) string {
	if res == nil {
		return ""
	}
	if res.Claim.Branch != "" {
		return res.Claim.Branch
	}
	if bs := recordedBranches(*res); len(bs) > 0 {
		return bs[0]
	}
	return ""
}

// attributionOr renders a harness/model value for a phase brief's record block.
// An empty model is normal (the harness default) and must not render as the bare
// word "model" followed by nothing, which reads like a truncated write.
func attributionOr(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// peekCheckpointClaim asks the plane whether the task this phase was BRIEFED on
// is now held, with a branch recorded, by some run of this project's key — the
// claim a session took through a channel the driver cannot observe (CLA-451:
// a raw API call with the same credential, a harness quirk, a renamed tool).
// It returns that claim as a synthesized Result for the seam to gate and carry,
// or nil when today's ending should stand.
//
// nil is returned — and the standard ending follows — when: the driver never
// knew which task it briefed (empty id; nothing to ask about); the target has
// no plane reads wired; the read fails (a plane blip degrades exactly like the
// spawn-dir peek does); or the answer is not unambiguous custody — status
// still "in_progress", a holding run, AND a recorded branch, all three. A task
// that moved on (reviewed, done, released) was settled by its own session and
// the sequence is over; a hold without a branch is the empty exit CLA-457
// already refuses to advance on.
//
// The three fields together are also the honest boundary of what this can
// know: the plane does not expose WHICH key holds a task, so "some run of this
// project's credential" is approximated by "held at all". The driver shares
// one key with every session it spawns, the briefed task came from this
// driver's own dispatch, and a wrong attribution fails safe — a foreign run's
// id seeds a heartbeat the plane refuses, ending the drain loudly rather than
// silently stranding work.
func (d *Driver) peekCheckpointClaim(ctx context.Context, t Target, drainNum int, phaseLabel, briefedTask string) *harness.Result {
	if briefedTask == "" {
		return nil
	}
	src, ok := t.Releaser.(plane.TaskStateSource)
	if !ok {
		return nil
	}
	sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	st, err := src.TaskState(sctx, briefedTask)
	switch {
	case err == nil:
	case errors.Is(err, plane.ErrNotWired):
		return nil
	default:
		log.Printf("%siteration %d: %s ended without an observed claim; the plane read of %s failed (%v) — keeping today's ending rather than guessing",
			labelOf(t), drainNum, phaseLabel, briefedTask, err)
		return nil
	}
	if st.Status != "in_progress" || st.ClaimedByRun == "" || st.Branch == "" {
		return nil
	}
	return &harness.Result{Claim: harness.Claim{
		TaskID: briefedTask,
		Ref:    st.Ref,
		RunID:  st.ClaimedByRun,
		HasWIP: true,
		Branch: st.Branch,
	}}
}

// drainWithRetries runs a single unphased drain — the whole task in one session.
// It is what a config with no `phases` gets, and it is kept as its own entry
// point because that is the shape every existing test drives.
//
// The phase comes from EffectivePhases, not an inline literal, so the unphased
// path carries the same resolution every other path does — the top-level or
// default turn cap (CLA-343), which is precisely what the inline zero-value
// version failed to do.
func (d *Driver) drainWithRetries(ctx context.Context, drainNum int, t Target, prior spend) (tokens int, cost float64, stop bool, err error) {
	phases := d.targetCfg(t).EffectivePhases()
	ph := phases[0]
	tokens, cost, stop, _, err = d.drainPhase(ctx, drainNum, 0, 0, "", ph, true, nil, t, prior, phases, panelState{})
	return tokens, cost, stop, err
}

// drainPhase runs ONE phase to a clean finish, absorbing usage-limit pauses
// (supervised wait) and transient blips (exponential backoff) by re-running the
// SAME session — neither costs a drain count. Returns the tokens/cost consumed on
// a clean finish; stop=true if a STOP/cancel landed during a wait; err only on a
// genuine, non-retryable failure (or exhausted retries).
//
// held is non-nil only when this phase ended cleanly, is not the last, and left
// the task's claim open for its successor — see drainPhases, which either carries
// it to the next phase or hands it back.
//
// prev is the claim the PREVIOUS phase left held, and is what fills the task/run
// placeholders in this phase's prompt. nil for the first phase, which has no run
// to resume and claims one of its own.
//
// prior carries the run's ceilings down into the drain, because the budget breaker
// in Run only runs BETWEEN drains and every wait in here happens inside one — see
// the check at the top of the loop, and waitPastBudget.
// phaseIdx is this phase's 0-based position in the sequence, carried only so a
// phase with no name can still be NAMED in a log line — ph.Label(phaseIdx) falls
// back to "phase 2". An unphased drain passes 0 and never reaches those lines.
// ti is the target's index, for the fleet claim reflector wired into the lease
// renewer below (CLA-510).
//
// phases is the sequence as CAPTURED when the drain started — the same slice
// drainPhases iterates. It is what this phase's IDENTITY is read from (its
// built-in brief, its position in the sequence), never the live config
// (CLA-664): a RELOAD marker is applied inside waitOrStop, which runs during a
// first-phase limit wait or a transient backoff in THIS drain, so the live
// config can be a DIFFERENT sequence than the one in flight. Reading it here
// would let the two disagree — the in-flight review gated by the reloaded
// position, the "PHASE i of N" text rewritten — while drainPhases still walks
// the captured slice. The rule the capture enforces: a RELOAD takes effect at
// the next drain, never mid-sequence.
//
// panel is what the panel stage this phase follows left behind (CLA-639): the
// round its members' records landed in, zero when no panel ran or no member
// recorded. It is the fix phase's grading input (mergedFindingsDigest reads
// that round and no other); the unphased drain and a phase that does not follow
// a panel pass the zero value.
func (d *Driver) drainPhase(ctx context.Context, drainNum int, ti int, phaseIdx int, tag string, ph config.Phase, last bool, prev *harness.Result, t Target, prior spend, phases []config.Phase, panel panelState) (tokens int, cost float64, stop bool, end phaseEnd, err error) {
	tc := d.targetCfg(t)
	// captured is this phase as the sequence was captured at drain start, before
	// a handoff respawn replaced ph.Prompt with the successor's continuation: the
	// mode must survive a handoff exactly as the record requirement does.
	captured := config.Phase{}
	if phaseIdx < len(phases) {
		captured = phases[phaseIdx]
	}
	// Whether this phase is a review-SHAPED gate (CLA-637): its deliverable is
	// the durable review record on the task, and the seam below refuses it a
	// checkpoint without one. Read off the CAPTURED sequence, never the live
	// config (CLA-664) and never ph.Prompt (a handoff respawn replaces that
	// wholesale). The predicate is config.ReviewShaped, so every review-only
	// variant is covered — an operator's own custom prompt owns its own contract
	// and is not one. nextReviewShaped is the same question about the NEXT
	// configured phase: the driver closes a round only when the review stage is
	// over, so a later panel member (CLA-639) never sees an earlier member's
	// findings.
	nextReviewShaped := false
	if phaseIdx+1 < len(phases) {
		nextReviewShaped = config.ReviewShaped(phases[phaseIdx+1].Prompt)
	}
	requireReviewRecord := config.ReviewShaped(captured.Prompt)
	isVerify := config.VerifyShaped(captured.Prompt)
	// CLA-639: whether this pass is one reviewer of a review panel, and whether
	// it is the LAST one. The last member is the panel's seam: it runs the same
	// record gate as the shipped single review, and it must run it even when the
	// sequence ends there (a panel with no fix phase after it) — the round close
	// and the merged list are owed to whoever reads the records next.
	//
	// A panel member is a review-shaped phase the operator did not write one at
	// a time: EffectivePhases expanded one `review` into N. The distinction
	// matters because the members are skippable — a dead or capped member is
	// recorded as failed and the round still closes — while a lone review phase
	// is the sequence's only record and keeps the shipped fail-closed path.
	panelMember := captured.IsPanelMember()
	panelLast := false
	// A BRANCHLESS checkpoint runs exactly ONE reviewer: the record this shape
	// can write is the legacy `## Review` block, not a durable one, so
	// drainPhases skips the members after the first. That first member is the
	// panel's last member in every sense the seam cares about — its record is
	// the panel's only record and the "no member recorded → no fix" rule must
	// apply to it — so the position test alone (`no review-shaped phase follows`)
	// is not enough here.
	branchlessPanel := panelMember && prev != nil && prev.Claim.Branch == ""
	if panelMember && (!nextReviewShaped || branchlessPanel) {
		panelLast = true
	}
	// The review seam's freshness baseline, read ONCE before the attempt ladder:
	// the phase's outcome as the review found it. A handoff respawn re-enters
	// drainPhase with the same carried claim and takes a fresh baseline, so a
	// successor must still write the record its own session is responsible for.
	//
	// CLA-663: an unreadable baseline refuses the phase BEFORE it spawns. The
	// seam cannot prove a record is this trip's from a baseline it never read —
	// so the old order spawned the full review session and then threw its
	// checkpoint away, and the claim (HasWIP) lapsed into a takeover that
	// re-ran implement AND review. Refusing here pays the read — already
	// retried by readReviewBaseline — and nothing else, and leaves the claim
	// with the same disposition a seam refusal leaves it: released when it is
	// releasable, left to lapse into a takeover when it carries pushed work.
	//
	// The reviewer role is the other half of the gate's key (CLA-637): one
	// name, read from the same resolved config the brief is built from, so the
	// session records under exactly the role the seam will match.
	review := reviewGate{required: requireReviewRecord}
	end.panelMember = panelMember
	end.panelLast = panelLast
	if review.required {
		review.role = reviewerRoleFor(phases, phaseIdx)
		review.baseline, review.read = d.readReviewBaseline(ctx, t, prev)
		if !review.read {
			log.Printf("%siteration %d: refusing to spawn the %s phase: the task could not be read before it spawned, so this phase's record cannot be judged against the task it found — the sequence ends here without the review; the claim is released when it is releasable, else left to lapse into a takeover",
				labelOf(t), drainNum, ph.Label(phaseIdx))
			end.empty = true
			end.emptyWhy = "the task's outcome could not be read before the review phase spawned"
			if prev != nil {
				end.claim = prev
				end.released = prev.Claim.Held()
				end.releasedToQueue = d.releaseHeldClaim(ctx, t, *prev, false)
			}
			return 0, 0, false, end, nil
		}
	}
	// The adapter for THIS phase, resolved once: everything below — the spawn, the
	// classification of what came back, and the probe of any usage limit it hit —
	// has to be the harness that actually ran, not the run's default. Resolved
	// here rather than per attempt so a retry ladder cannot straddle two harnesses.
	a, aerr := d.adapterFor(ph)
	if aerr != nil {
		return 0, 0, false, end, fmt.Errorf("iteration %d: phase %q: %w", drainNum, ph.Label(phaseIdx), aerr)
	}
	// Where this phase's sessions run, resolved ONCE per phase (CLA-437): the
	// task repo's checkout when it resolves, the target's workdir when nothing
	// does. Before the attempt ladder, so a repo that resolves to nothing fails
	// the phase with repo_not_found before anything is spawned — a session that
	// starts in the multi-repo parent reads a sibling's rule files and, on a
	// cwd-scoped grant, hits the wall this replaces. The same directory then
	// serves every retry: the task does not change between attempts.
	//
	// The second value is the task this phase was briefed on (CLA-451): exact for
	// a resumed phase, the spawn-time queue-head peek for a fresh one. Held onto
	// here because the seam needs it AFTER the session ends — it is the one task
	// the driver can ask the plane about when the stream carried no claim state.
	spawnDir, briefedTask, briefedRepo, serr := d.sessionDir(ctx, t, prev)
	if serr != nil {
		return 0, 0, false, end, fmt.Errorf("iteration %d: phase %q: %w", drainNum, ph.Label(phaseIdx), serr)
	}
	// The branch this task's repo PRs target, resolved ONCE per phase like the
	// spawn directory above, and for the same reason: it does not change between
	// attempts. Filled into the prompt's {{prBase}} placeholder by invocationFor
	// (CLA-608). Never fails here: an unresolvable probe falls back to the
	// configured integration branch, which is the text the briefs carried
	// before this feature existed.
	prBase := d.prBaseFor(ctx, t, spawnDir, briefedRepo)
	// CLA-635: the FIX phase's brief carries the driver's structural grading of
	// the review record (B5). Read once per phase, before the attempt ladder —
	// the record cannot change while this phase runs except by this phase
	// itself, and every attempt must see the same grade. A read that fails
	// leaves the digest empty and the brief falls back to the raw block, which
	// the fix phase reads with get_task anyway.
	//
	// Like requireReviewRecord above, the signal is the CAPTURED phase, not
	// ph.Prompt and not the live config: a handoff respawn replaces ph.Prompt
	// with the successor's continuation wholesale, and the successor of a
	// mid-fix handoff needs the same graded list its predecessor got — its
	// continuation (fixTerminalRecord) already holds it to the grade, so the
	// grade itself must be the one thing the respawn does not drop. The resolved
	// prompt is the builtin fix brief exactly when the operator left the phase's
	// own Prompt empty; a custom fix prompt owns its wording and gets no digest,
	// the same convention the variant swap in invocationFor follows.
	fixGrading := ""
	if captured.Prompt == config.FixBrief() || captured.Prompt == config.FixVerifyBrief() {
		if outcome, read := d.readTaskOutcome(ctx, t, prev,
			"to grade the review record for the fix phase",
			"the fix brief will carry the raw record without the driver's structural grading"); read {
			fixGrading = reviewFindingsDigest(outcome)
		}
		// CLA-639: when the fix follows a review PANEL, the driver's list is the
		// merged one (B11) — every member's released record for the panel's round,
		// MUSTs open from any member, SHOULD/NIT annotated with who raised them,
		// and the members that never recorded named as incomplete. It REPLACES the
		// legacy block's grading: the panel writes no block, so the legacy digest
		// above has nothing to grade, and the fixer must not be handed one
		// reviewer's list as though it were the panel's. The read is the same
		// get_task; a failed read leaves the legacy digest above in place, exactly
		// as before the panel existed.
		//
		// panel.round is the round the panel's records landed in (CLA-639 S1), and
		// it is the ONLY round the merged list may read: with round 0 the digest
		// would relist an earlier cycle's findings after a verify rewind and count
		// a member that recorded in an earlier round as having recorded this one.
		if roles := panelRoles(phases, phaseIdx); len(roles) > 0 && panel.round > 0 && prev != nil && prev.Claim.TaskID != "" {
			if st, read := d.readTaskState(ctx, t, prev.Claim.TaskID); read {
				if merged := mergedFindingsDigest(st.Reviews, roles, panel.round, panel.failed); merged != "" {
					fixGrading = merged
				}
			}
		}
	}
	// CLA-671: on a fix round after a verification round, the newest record
	// holds only the verifier's NEW findings, so a MUST an earlier round left
	// open is not in the block the grading above reads; the fixer used to leave
	// it untouched and the breaker parked the task as non-convergence. Prepend
	// the plane's own open list. Best-effort: an unreadable task state leaves
	// the brief's instruction to read reviews.openMustIds itself.
	if captured.Prompt == config.FixVerifyBrief() && prev != nil && prev.Claim.TaskID != "" {
		if st, read := d.readTaskState(ctx, t, prev.Claim.TaskID); read {
			fixGrading = openMustFixDigest(st.Reviews, st.ReviewOpenMustIDs) + fixGrading
		}
	}
	// CLA-638: the VERIFICATION round's brief carries the prior open MUST ids and
	// their findings, read from the plane's released records before the session
	// spawns. Fail-closed: a round that cannot be told what it must adjudicate is
	// refused before it is paid for, the same disposition CLA-663 gives the
	// review baseline — which includes its RETRY (CLA-686 D3): one plane blip
	// here would otherwise discard the fix round that just ran and hand the
	// sequence back to the queue, so this read shares the baseline's bound
	// instead of taking a single attempt. The task id comes from the carried
	// claim; the phase is resumed, so it is exact.
	verifyDigest := ""
	if isVerify {
		taskID := ""
		if prev != nil {
			taskID = prev.Claim.TaskID
		}
		st, read := d.readTaskStateRetry(ctx, t, taskID, "before the verification round spawned")
		if !read {
			log.Printf("%siteration %d: refusing to spawn the %s phase: the task's record could not be read, so the round would not know what to adjudicate — the sequence ends here; the claim is released when it is releasable, else left to lapse into a takeover",
				labelOf(t), drainNum, ph.Label(phaseIdx))
			end.empty = true
			end.emptyWhy = "the task's record could not be read before the verification round spawned"
			if prev != nil {
				end.claim = prev
				end.released = prev.Claim.Held()
				end.releasedToQueue = d.releaseHeldClaim(ctx, t, *prev, false)
			}
			return 0, 0, false, end, nil
		}
		if prev != nil && prev.Claim.Branch == "" {
			// CLA-638 × CLA-670: a BRANCHLESS verification round has no plane
			// finding ids to adjudicate (the branchless review writes no durable
			// record), so the digest hands it the LAST legacy block instead.
			verifyDigest = branchlessVerificationDigest(st.Outcome)
		} else {
			verifyDigest = verificationDigest(st.Reviews, st.ReviewOpenMustIDs, st.ReviewsTotal)
		}
	}
	retries := 0
	// Consecutive attempts that ended without the harness reporting any usage -
	// the spend the budget breaker above can never see (CLA-288).
	//
	// Deliberately scoped to THIS phase's attempt ladder, which is where the
	// unbounded loop lives: every re-spawn of an attempt comes back through the
	// top of this loop, and it is the ladder - not the phase sequence - that has
	// no other bound under `max_retries: 0`. A phased run therefore allows the
	// bound per phase, and a handoff respawn (which needs a clean exit and a held
	// claim, so a silently dying attempt cannot produce one) starts a fresh count
	// under drainPhases' own chain cap.
	noUsage := 0
	// Retries this ladder has taken on a failure the adapter judged retryable
	// WITHOUT recognising it (harness.Adapter.IsUnclassifiedTransient) - see
	// errUnclassifiedRetryLoop. Scoped per phase like noUsage, and for the same
	// reason: the ladder is what has no other bound.
	//
	// Never reset. A classified retry in between does not make the next guess any
	// better informed, and resetting would let a failure that alternates between a
	// recognised blip and an unrecognised one loop forever - which is the exact
	// shape of a provider degrading: real 5xx blips interleaved with stream drops
	// nothing has a pattern for.
	unclassified := 0
	// The spend of THIS phase pass, summed across the ladder's attempts
	// (CLA-691). One fleet-record entry is published per pass, and a pass that
	// retried is several paid sessions, so every attempt the ladder spawned is
	// folded in here where the entry is built.
	pass := sessionSpend{}
	for {
		// The breaker, from inside the drain. Every path that loops back here has
		// just waited — a supervised wait on a usage limit, an exponential backoff
		// on a transient blip — and each of those waits used to be unbounded by
		// anything but the wall clock, which is only one of three dials and the one
		// an operator is least likely to have set. A drain that re-spawns a paid
		// session on a loop is the expensive failure this stops, whatever put it
		// there (CLA-258).
		if dim := d.budgetTrip(prior.tokens+tokens, prior.cost+cost, time.Since(prior.start)); dim != "" {
			log.Printf("iteration %d: budget reached mid-drain: %s — stopping (tokens=%d cost=$%.2f elapsed=%s)",
				drainNum, dim, prior.tokens+tokens, prior.cost+cost, time.Since(prior.start).Round(time.Second))
			return tokens, cost, true, end, nil
		}

		// The breaker's blind spot, bounded. The check above can only stop spend it
		// was told about; an attempt that died before its harness reported usage
		// added nothing to either accumulator, so a ladder of those attempts leaves
		// a token or cost ceiling permanently unreachable and only max_wall_clock -
		// the dial an operator is least likely to have set as a spend bound - can
		// end the run. Reached only on an attempt re-spawn (the first pass has
		// counted nothing), so a single silent attempt that ends the phase some
		// other way never trips it.
		if bound := d.cfg.ZeroSpendAttemptBound(); noUsage >= bound {
			return tokens, cost, false, end, fmt.Errorf(
				"iteration %d: %w: %d consecutive attempts died before %s reported any usage, so nothing the budget breaker reads was ever fed - the sessions are failing early rather than working (see the attempt logs in the state dir; raise max_zero_spend_attempts to allow more)",
				drainNum, errZeroSpendLoop, noUsage, a.Name())
		}

		// Each attempt streams live to the terminal and to its own logfile. The name
		// carries the drain number and attempt counter as well as the timestamp: two
		// attempts in the same second (a sub-second backoff) would otherwise share a
		// name, and a name already taken is REFUSED rather than truncated.
		//
		// The random tail is not decoration. This log carries the session's whole
		// output — prompts, tool arguments, tool output, occasionally a token — and
		// the rest of the name is derivable to within a second by the session that
		// produced it. Unguessable names mean a session that CAN write to the state
		// dir (an operator who pointed state_dir back inside the workdir) still
		// cannot pre-plant a decoy at the name we are about to use. statedir.Create
		// refuses an existing path either way; this stops it turning into a denial
		// of the log itself.
		inv, ierr := d.invocationFor(t, phaseIdx, ph, prev, spawnDir, prBase, phases)
		if ierr != nil {
			// The session env refused to resolve (a failing or empty-output
			// fromCommand, an unreadable @path file — the error names the
			// variable). REFUSING THE SPAWN is the fail-closed contract: no
			// session starts without its declared env (CLA-462). This is not a
			// retryable failure — nothing was spawned and the cause is config,
			// not a blip — so it fails the phase like a bad spawnDir does.
			return 0, 0, false, end, fmt.Errorf("iteration %d: phase %q: refusing to spawn: %w", drainNum, ph.Label(phaseIdx), ierr)
		}
		// The graded list is PREPENDED after placeholder substitution, never
		// run through the one-pass replacer: it is driver-read text from the
		// plane, and a finding quoting `{{model}}` must stay a quote.
		if fixGrading != "" {
			inv.Prompt = fixGrading + inv.Prompt
		}
		// The verification digest is the same contract for the verify phase
		// (CLA-638): plane-read ids and findings, prepended verbatim.
		if verifyDigest != "" {
			inv.Prompt = verifyDigest + inv.Prompt
		}
		logName := fmt.Sprintf("iteration-%s-d%d%s-a%d-%s.log",
			time.Now().Format("20060102-150405"), drainNum, tag, retries, randomTail())
		f, ferr := d.state.Create(logName)
		logPath := filepath.Join(d.state.Path(), logName)
		if ferr == nil {
			inv.Console = io.MultiWriter(os.Stderr, f)
		} else {
			inv.Console = os.Stderr
			log.Printf("could not open iteration log %s: %v", logPath, ferr)
		}
		// The spawn line stamps what the session was actually spawned WITH:
		// phase and the resolved model id inv.Model carries (ModelForPhase's
		// output, never the tier alias). A token/cost line in an old log is then
		// attributable to the settings of THAT day rather than inferred from
		// whatever the config file says now (CLA-345). An empty model= is itself
		// evidence: no explicit model was configured at spawn time, and the
		// harness ran its own default.
		if retries == 0 {
			log.Printf("iteration %d %s- spawning %s (phase=%s model=%s log: %s)", drainNum, labelOf(t), a.Name(), ph.Label(phaseIdx), inv.Model, logPath)
		} else {
			log.Printf("iteration %d %s- retry %d, spawning %s (phase=%s model=%s log: %s)", drainNum, labelOf(t), retries, a.Name(), ph.Label(phaseIdx), inv.Model, logPath)
		}

		// CLA-358: while THIS child runs, renew whatever task-lease it holds.
		// Started before Invoke so a resumed phase's seeded claim renews from
		// the first tick; stopped the moment Invoke returns, which is what makes
		// renewal pause whenever no child process is running - between attempts,
		// across supervised waits, after exit. Inside the window the adapter may
		// run its own resurrection machinery (opencode CLA-406): those processes
		// continue the SAME session on the SAME claim, so renewal spans them.
		renewer := d.startLeaseRenewal(ctx, ti, t, &inv, fmt.Sprintf("%siteration %d: ", labelOf(t), drainNum))
		res, ierr := a.Invoke(ctx, inv)
		renewer.stop()
		if f != nil {
			_ = f.Close()
		}
		// The orderly-cap classifications, computed here (before the salvage) so
		// the salvage can tell whether a clean tree means "produced nothing" — a
		// question the caps answer: a capped session is an orderly end with its
		// own marker, not a silent death, and an untrusted stream's finish reason
		// cannot be read at all. Purely adapter reads of `res`, so moving them up
		// changes nothing else (review finding).
		capped := a.TurnCapped(res)
		ceiling := a.TokenCeilingHit(res)
		wallclock := a.WallClockCapped(res)
		zeroUsage := a.ZeroUsageUnknown(res)
		producedNothing := res.Untrusted == "" && !capped && !ceiling && !wallclock && deadPhase(res)
		// Rescue whatever the session left uncommitted, FIRST — before the handback,
		// because a successful salvage changes what the handback should do: a task
		// with a branch recorded on it is no longer safe to release (CLA-314).
		//
		// NOT for the review-only gate (CLA-split): its deliverable is the record
		// on the PLANE, not the tree, and anything uncommitted in its worktree is
		// generated output from running checks — which the brief forbids it to
		// commit. Salvaging it would (a) commit the unattributable change the
		// review's own record forbids, and (b) push a new tip AFTER the reviewed
		// tip it just recorded, so the fix phase's tip comparison would fail and
		// it would park a healthy task for a mismatch the DRIVER created. A
		// review that genuinely dies leaving source edits loses them, which is
		// exactly what "review only, change nothing" means.
		if !requireReviewRecord {
			d.salvageStrandedWork(ctx, t, &res, producedNothing, inv.WorkDir)
		}
		// Hand back anything the session was still holding, BEFORE deciding what to
		// do next — every branch below either waits, retries or returns, and all of
		// them leave the lease unattended. Above the ierr check too: on every
		// adapter, an Invoke whose run failed with something other than an exit
		// status returns whatever the stream had announced by then, fully parsed,
		// alongside the error (CLA-299) — claude's drain path parses its stream as
		// it arrives; codex and opencode run their parsers to completion before
		// classifying the failure — so a claim observed there is real and must not
		// be dropped just because the process died untidily. A launch failure
		// yields a zero Result, which releases nothing.
		//
		// UNLESS this phase reached its checkpoint with another phase still to run.
		// Then the sequence is not over: the next phase resumes THIS run with
		// heartbeat(runId) instead of claiming, so handing the lease back here
		// would post the task to the queue while we are still working it, and the
		// next phase would find someone else's task or none at all. drainPhases
		// owns the handback in that case and performs it the moment the sequence
		// ends for any other reason.
		//
		// Only an ORDERLY end holds it open, and each conjunct is load-bearing: a
		// retry deliberately re-claims a half-done task with a fresh session
		// (which is why an ordinary non-zero exit still releases), and an untrusted
		// stream must not be read for claim state at all, because the settle we
		// never saw may be in the bytes that never arrived (CLA-262).
		//
		// A turn-capped end CAN be orderly — but only if something durable came out
		// of it. The salvage above is not a guarantee: it commits nothing on a
		// clean worktree, and refuses outright on a diverged remote or a tree in
		// the middle of a merge. A phase 1 that burned its turns reading and
		// planning without writing therefore leaves no branch at all, and calling
		// that a checkpoint spawns a session told "an earlier session has already
		// implemented, committed and pushed" and pointed at a branch that is not
		// there — a paid session that can only fail, or worse, move the task to
		// review over no work.
		//
		// HasWIP is exactly that signal: it is set by the only two things that make
		// a checkpoint real — the phase recording its own branch, or the salvage
		// recording one for it and the PLANE accepting the record.
		// A session the adapter ended on its wall-clock cap is the third member of
		// the same family: an orderly cut-off mid-thought, whose survivability rests
		// on the salvage exactly as a turn cap's does — so it earns a checkpoint on
		// the same terms, HasWIP and all (CLA-368). Classified by the PHASE's adapter
		// (`a`), never the run's: "did this session end on its own wall clock" is a
		// question only the harness that RAN it can answer, and under per-phase
		// harnesses (CLA-366) that harness is not necessarily d.h.
		checkpointable := res.ExitCode == 0 || ((capped || ceiling || wallclock) && res.Claim.HasWIP)
		// CLA-386: a session whose final step finished with reason "unknown" and
		// left no branch recorded produced NOTHING — a dead phase, not a completed
		// one. It must not earn a checkpoint even on a zero exit, because a
		// checkpoint is what hands the next phase a brief that claims "an earlier
		// session has already implemented, committed and pushed". Two real
		// implement phases died exactly this way and the seam advanced to review
		// on the false premise; the signal has to veto the checkpoint.
		//
		// A capped session is excluded even when its stream carries "unknown": the
		// wall-clock kill can land mid-turn, but a cap is an orderly end with its
		// own marker — retrying it would re-spend against the same cap and parking
		// it would read a doing-its-job backstop as a silent death.
		dead := !last && res.Untrusted == "" && res.Claim.Held() && !capped && !ceiling && !wallclock && deadPhase(res)
		// CLA-402: book this session into the run's dead-phase tally, per phase
		// label and harness, whatever the seam decided about retrying it. The
		// tally's dead classification deliberately drops the `!last` conjunct
		// that guards the checkpoint veto — a dead LAST phase is still a dead
		// phase, and the rate is a measurement, not a seam decision (see
		// deadtally.go). The report rides the end (CLA-639): the panel's member
		// handler books a failed member into the same tally, and must not count
		// an exit this call already booked.
		bookedDead := d.tallyDead(ph.Label(phaseIdx), tc.HarnessFor(ph), res, capped, ceiling, wallclock)
		end = phaseEnd{}
		end.deadBooked = bookedDead
		// CLA-639: the panel identity of this pass, for drainPhases' member
		// handler. Set from the CAPTURED phase (a handoff respawn replaced
		// ph.Prompt, never its identity), so a skipped member is recorded against
		// the reviewer that was meant to run.
		end.panelMember = panelMember
		end.panelLast = panelLast
		if res.Claim.TaskID != "" {
			end.claim = &res
		}
		// CLA-630: the session this pass actually spawned, in exactly the words
		// the spawn line above used — the phase's label, the phase's resolved
		// harness, and the resolved model (empty when the harness default
		// applied; never guessed). Set on every attempt, so the LAST attempt's
		// values are the ones that ride: the entry describes the PASS, and a
		// retry within the ladder cannot change what the pass ran — only a
		// reload landing mid-ladder can.
		//
		// CLA-691: its spend is filled in later, at the point the loop counts the
		// attempt's spend toward the iteration (recordPassSpend), so the two can
		// never disagree about which attempts were counted. The pass sums every
		// counted attempt, because the loop counts each one as its own paid
		// session: an entry reporting only the surviving attempt's money would
		// not add up to the iteration total.
		end.session = &fleet.Session{
			Phase:   ph.Label(phaseIdx),
			Harness: a.Name(),
			Model:   inv.Model,
			RunID:   sessionRunID(res, prev),
		}
		// The wider classification rides the end for the fleet record only — see
		// phaseEnd.producedNothing.
		end.producedNothing = producedNothing
		// A handoff block ending the session's final message (CLA-352) holds the
		// task open exactly like a phase seam — even on the LAST phase, where
		// there is otherwise no successor to hold it for. The remaining guards
		// live in drainPhases; if they refuse, the deferred handback there
		// releases what this held.
		handoff := detectHandoff(drainNum, t, res)
		// CLA-638: a verification round does not chain via a handoff. Its record
		// is the deliverable, and the DRIVER decides what follows (another fix, a
		// park, or nothing) from the open MUSTs the record releases; a successor
		// prompt here would have nothing to continue that this round has not
		// already written, and honoring it would defer the close/evaluation the
		// driver owes the round.
		if isVerify && handoff != "" {
			log.Printf("%siteration %d: the verification round emitted a handoff block; verification rounds do not chain — the driver judges the round's record and decides whether another fix runs", labelOf(t), drainNum)
			handoff = ""
		}
		if panelMember && handoff != "" {
			log.Printf("%siteration %d: the %s panel member emitted a handoff block; panel members do not chain — a successor would continue this reviewer's role, but the phase after it is the next member, and the member's record is its deliverable", labelOf(t), drainNum, ph.Label(phaseIdx))
			handoff = ""
		}
		// The seam contender: an orderly end still holding the task, minus the dead
		// signature. This is the gate the incident put in question — see below.
		canCheckpoint := res.Untrusted == "" && checkpointable && res.Claim.Held() && !dead
		// CLA-639: a panel member may be the LAST phase of the sequence when the
		// operator wrote no fix after the review, and its record gate must still
		// run — that is how the round closes and how the merged list is released.
		// `panelLast` (rather than !last) admits it, and only its review record
		// gate: a settled or non-checkpointable exit is excluded exactly as before.
		reviewable := (!last || panelLast) && canCheckpoint
		// CLA-457: held-ness alone must never advance the seam. Since CLA-358 the
		// driver's lease renewer renews a claim for a session's whole lifetime, so
		// ANY session that claimed ends held — "still holding" is a constant, not
		// a checkpoint signal. A non-final phase reaches its checkpoint ONLY when
		// the plane's record carries the phase's exit evidence: a branch recorded
		// by this run whose tip really exists on the origin remote (the implement
		// phase in the shipped sequence). The evidence is computed even on a
		// handoff: an HONORED handoff continues the same phase regardless (its
		// successor has nothing to falsely resume), but a REFUSED handoff must be
		// re-judged as the bare exit it really is, and this is the only place the
		// verification can run.
		evidenceOK := true
		evidenceUnverified := false
		evidenceWhy := ""
		evidenceTip := ""
		if isVerify && res.Untrusted == "" && (res.ExitCode == 0 || capped || ceiling || wallclock) {
			// The verification seam runs even on the LAST phase. Unlike every
			// other terminal phase its evidence is not a checkpoint (nothing
			// follows to resume) but the durable record plus the open-MUST set:
			// that set is what decides whether ANOTHER fix is spawned, whether
			// the sequence is done, or whether the round cap parks the task
			// (CLA-638, B12).
			end.verifyChecked = true
			end.branch, evidenceTip, evidenceOK, evidenceUnverified, evidenceWhy = d.checkpointEvidence(ctx, t, res, inv.WorkDir, &review)
			if review.state == "sealed" {
				end.reviewRound = review.round
			}
			if evidenceOK && review.required && review.round > 0 {
				if review.state == "sealed" {
					// The verifier did not release its own round; the driver does,
					// exactly as for the review gate.
					if cerr := d.closeReviewRound(ctx, t, res, review.round); cerr != nil {
						evidenceOK = false
						evidenceWhy = fmt.Sprintf("verification round %d could not be closed (%v) — its findings would stay sealed and the next fix would have nothing to apply", review.round, cerr)
					}
				}
				if evidenceOK {
					// Closed (or already released by the verifier's terminal call);
					// nothing downstream may try to close it again.
					end.reviewRound = 0
					end.verifyRound = review.round
					// Fail-closed, so retried like the spawn read (CLA-686 D3):
					// one blip here ends a sequence the verification round has
					// already been paid for.
					if st, read := d.readTaskStateRetry(ctx, t, res.Claim.TaskID, "after the verification round closed"); !read {
						evidenceOK = false
						evidenceWhy = "the task could not be read after the verification round closed, so its open MUSTs cannot be judged"
					} else {
						end.verifyOpenMusts = append([]string(nil), st.ReviewOpenMustIDs...)
						end.verifyOpenDetails = openMustDetails(st.ReviewOpenMustIDs, st.Reviews)
						end.verifyCounted = st.ReviewCounted
						end.verifyResolvedIDs = resolvedFindingIDs(st.Reviews)
						end.verifyStatus = st.Status
					}
				}
			} else if evidenceOK && review.required {
				// CLA-638 × CLA-670: the checkpoint rested on the legacy `## Review`
				// block (the branchless arm of the gate), so there is no durable
				// round to release and no plane open-MUST set to judge — the block
				// is this shape's record. Read the task back anyway, for the status
				// the hand-over log needs: a branchless verifier that recorded its
				// pass but left the task in_progress must be named loudly, exactly
				// as the id-based shape names it. A failed read ends the sequence
				// fail-closed, like every other unreadable seam — so it retries
				// like them too (CLA-686 D3).
				end.verifyRound = 0
				if st, read := d.readTaskStateRetry(ctx, t, res.Claim.TaskID, "after the branchless verification round"); !read {
					evidenceOK = false
					evidenceWhy = "the task could not be read after the branchless verification round, so its hand-over cannot be judged"
				} else {
					end.verifyStatus = st.Status
				}
			}
			if !evidenceOK {
				end.verifyFailure = evidenceWhy
			}
			// Fold the verified branch/tip onto the claim exactly as the generic
			// checkpoint path below does, so a fix spawned after an open-MUST
			// round resumes with the verified hand-off.
			if end.claim != nil && evidenceOK {
				if end.branch != "" {
					end.claim.Claim.Branch = end.branch
				} else {
					end.claim.Claim.Branch = ""
				}
				end.claim.CheckpointUnverified = evidenceUnverified
				end.claim.CheckpointTip = evidenceTip
			}
		} else if reviewable {
			end.branch, evidenceTip, evidenceOK, evidenceUnverified, evidenceWhy = d.checkpointEvidence(ctx, t, res, inv.WorkDir, &review)
			// The matched round rides the end (CLA-637): drainPhases closes it if
			// the handoff guards refuse the respawn this detected handoff asked
			// for — the one advance path drainPhase cannot close on itself. Only a
			// still-sealed round is ours to close: a verifier closes its own in the
			// terminal call (CLA-638).
			if review.state == "sealed" {
				end.reviewRound = review.round
			}
			// CLA-637: the review phase's durable record is SEALED — the plane
			// withholds its findings from every reader until the driver closes
			// the round. The close is the driver's, made when the review stage
			// is OVER: not while the session hands off to a successor reviewer
			// (the successor continues this round), and not while the next
			// configured phase is another review-shaped one (a panel member,
			// whose independence depends on not reading the earlier member's
			// findings). A close that fails REFUSES the checkpoint, fail-closed:
			// the alternative is a fix phase spawned against findings the plane
			// still has sealed, which can only park the task.
			//
			// CLA-639: for a panel, the stage is the whole panel. Every member
			// but the last leaves the round sealed here; the last member's close
			// is what releases them all to the fix phase, and it happens in the
			// SAME condition below because nextReviewShaped is false only for
			// the last member (the panel's last phase, when no fix follows).
			if evidenceOK && review.required && review.round > 0 && review.state == "sealed" && handoff == "" && !nextReviewShaped {
				if cerr := d.closeReviewRound(ctx, t, res, review.round); cerr != nil {
					evidenceOK = false
					end.reviewCloseFailed = true
					evidenceWhy = fmt.Sprintf("review round %d could not be closed (%v) — refusing the checkpoint, because the fix phase would be spawned against findings the plane still has sealed", review.round, cerr)
				}
			}
		}
		// A held-but-empty exit (2026-08-24): a would-be checkpoint refused by the
		// evidence gate — not by any of the older vetoes (dead, untrusted, not
		// checkpointable) — so this is a FAILED phase, not a completed one. It is
		// carried on the result in BOTH branches on purpose: for a bare exit it is
		// the classification of record, and for a handoff it is what lets
		// drainPhases downgrade the exit when the handoff is refused. An honored
		// handoff never reaches that downgrade — the phase continues first.
		//
		// CLA-639: a panel member is not special-cased here. A member whose
		// review record did not pass the gate is a FAILED member, and the panel
		// handler in drainPhases is what records it as failed and keeps the round
		// going; the flag is shared with the shipped single-review path, which
		// still refuses to advance on it.
		empty := reviewable && !evidenceOK
		end.empty = empty
		end.emptyWhy = evidenceWhy
		// CLA-639: whether this pass left a review RECORD behind, for the panel's
		// member handler. It is the seam gate's verdict, not the round number: a
		// member whose envelope was sealed and closed still recorded.
		end.reviewRecorded = reviewable && review.required && evidenceOK
		// A handoff holds the seam OPEN even without evidence: its successor
		// continues THIS phase's job, so it is not the false-premise advance the
		// gate exists to stop. The evidence gate governs only the bare seam
		// advance to the NEXT configured phase (handoff == ""); a refused handoff
		// is downgraded to that bare judgement in drainPhases.
		//
		// CLA-638: a verification round that left an OPEN MUST holds the seam
		// even though it is the last phase, because the driver spawns another
		// fix against it. A clean round does not hold.
		verifyNeedsHold := isVerify && end.verifyChecked && end.verifyFailure == "" && len(end.verifyOpenMusts) > 0
		if ((!last || panelLast || handoff != "") && canCheckpoint && (handoff != "" || evidenceOK)) ||
			(verifyNeedsHold && canCheckpoint) {
			end.checkpoint = true
			end.handoff = handoff
			if end.claim != nil {
				// The invariant "the carried claim's branch is the VERIFIED one"
				// is held by construction, whatever form the evidence took
				// (CLA-497): the branch form folds the verified branch on, and
				// the plane-record forms clear whatever the session recorded -
				// a recorded branch that FAILED origin verification must not
				// ride into the successor's brief, which names {{branch}} as
				// verified on the origin remote (CLA-457), and the no-code
				// brief is selected exactly on an empty claim branch.
				//
				// CLA-576: an UNVERIFIED checkpoint is the one exception, and it
				// is marked rather than hidden. Its branch rides the claim (the
				// review phase needs the name to verify it) with
				// CheckpointUnverified set, so the successor's brief is the
				// unverified variant - never the verified wording, which this
				// checkpoint cannot support.
				if end.branch != "" {
					end.claim.Claim.Branch = end.branch
				} else {
					end.claim.Claim.Branch = ""
				}
				end.claim.CheckpointUnverified = evidenceUnverified
				// CLA-637: the origin tip the verified branch check read. The
				// review brief hands it to the reviewer as the revision to
				// check out and record, and the seam matches the record's
				// reviewedTip against it. Empty exactly when no tip could be
				// read (branchless or unverified), which is when no tip can be
				// demanded of the reviewer either.
				end.claim.CheckpointTip = evidenceTip
				// CLA-685: a HANDOFF is the one checkpoint this session's own
				// exit did not earn — it holds only because the successor
				// continues THIS phase, which the claim carried in as `prev`
				// opened. So when the exit evidenced NOTHING (evidenceOK false)
				// the verdict just folded above is its REFUSAL's, not a
				// checkpoint's: the record gate zeroes the unverified mark even
				// though the branch leg had established it. The successor then
				// reads carried.CheckpointUnverified as false, is handed the
				// VERIFIED continuation - "The driver verified the origin
				// remote's tip ... as {{reviewedTip}}" - for a tip this driver
				// never read, and the placeholder stays literal, since it is
				// filled only when the tip is non-empty.
				//
				// The exit's verdict about the checkpoint is empty exactly when
				// it read no tip of its own: an evidenced branch leg always
				// brings a tip (the verified form), and a non-empty tip implies
				// unverified=false — the verified continuation, whose
				// {{reviewedTip}} that tip then fills. So the opening
				// checkpoint's mark is the only true one there, and it rides
				// forward, as the panel-member block in drainPhases rides a
				// claim's resumption facts onto the claim it hands on.
				//
				// The tip is carried only for the branch it was read for — it
				// is `prev`'s, and a claim that now names another one must not
				// be handed a tip that never belonged to it. The BRANCH stays as
				// folded: it is the live truth about where the work is. An exit
				// that WAS evidenced keeps its own verdict untouched, so the
				// verified path is unaffected.
				if handoff != "" && !evidenceOK && prev != nil && end.claim.CheckpointTip == "" {
					end.claim.CheckpointUnverified = prev.CheckpointUnverified
					if end.claim.Claim.Branch == prev.Claim.Branch {
						end.claim.CheckpointTip = prev.CheckpointTip
					}
				}
			}
			if handoff != "" {
				log.Printf("%siteration %d: the session ended its final message with a handoff block, still holding %s — keeping the lease for its successor",
					labelOf(t), drainNum, res.Claim.TaskID)
			} else if end.branch != "" && evidenceUnverified {
				// The cause is the CHECK not completing, named by evidenceWhy
				// (the Unknown check's own detail) — never "the origin remote
				// could not be read" asserted as fact: delivery.Unknown also
				// covers a check that never reached the remote (a branch no
				// local repository has, an ambiguous repo, no git on PATH), and
				// the log must not contradict the detail it prints.
				log.Printf("%siteration %d: phase reached an UNVERIFIED checkpoint holding %s (branch %s recorded, but it could not be verified on the origin remote: %s) — keeping the lease for the next phase; the review phase must verify the branch itself",
					labelOf(t), drainNum, res.Claim.TaskID, end.branch, evidenceWhy)
			} else if end.branch != "" {
				log.Printf("%siteration %d: phase reached its checkpoint holding %s (branch %s verified on the origin remote) — keeping the lease for the next phase",
					labelOf(t), drainNum, res.Claim.TaskID, end.branch)
			} else {
				// The plane-record forms of exit evidence (CLA-497): a no-code or
				// already-settled task checkpoints with no branch to name.
				log.Printf("%siteration %d: phase reached its checkpoint holding %s (exit evidenced by the plane's record — task left ready or declared a no-code delivery; no branch) — keeping the lease for the next phase",
					labelOf(t), drainNum, res.Claim.TaskID)
			}
		} else {
			// `released` records only a handback that could actually have happened:
			// a zero Result from a failed launch releases nothing, and reporting it
			// as released would erase a predecessor's claim that is still live.
			//
			// CLA-639 M4: a PANEL MEMBER that ended without a record does NOT hand
			// its claim back here. Everything the panel handler must do about a
			// failed member needs the claim to still be live: closing the round (only
			// the LIVE holder may, and a failed close would leave the earlier
			// members' findings sealed behind a lease that is already gone) and
			// handing the next member — or the fix phase — the same run to resume.
			// A member that genuinely lost the claim is not special-cased: Held() is
			// false, so there is nothing to release either way. The handler owns the
			// handback for this shape, and takes it AFTER the close (see
			// drainPhases).
			if !panelMember {
				end.released = res.Claim.Held()
			}
			end.dead = dead
			if end.empty {
				log.Printf("%siteration %d: the %s phase ended holding %s with no recorded branch on the origin remote — not a checkpoint (%s), so no review phase spawns",
					labelOf(t), drainNum, ph.Label(phaseIdx), claimLabel(res.Claim), evidenceWhy)
				// Book it into the dead-phase tally (CLA-457): this is the
				// "produced nothing" class the rate exists to measure, and a
				// fleet-wide silent EOT is exactly the incident in question.
				//
				// Unless the session's exit was already booked as the CLA-402
				// SILENT death: a LAST phase can wear both shapes at once (its
				// finish reason is "unknown" AND its evidence gate failed), and
				// one session may not read as two dead ones out of one run —
				// CLA-639 M4 needs this booking to be exactly one per failed
				// panel member, and the operator's rate needs it to be honest.
				//
				// And unless this is a panel member whose review close failed
				// (CLA-639 fix pass 3): a member that WROTE its record and whose
				// round the plane then refused to release produced a review — the
				// empty shape here would report it as a session that died
				// producing nothing, which is the opposite of what happened. The
				// skip is PANEL-ONLY: the plain single review keeps the untaxed
				// CLA-457 booking it has had since the base commit, so an
				// operator's dead-phase rate reads the same with no panel
				// (CLA-639 fix pass 4).
				if !bookedDead && !(panelMember && end.reviewCloseFailed) {
					d.tallyEmpty(ph.Label(phaseIdx), tc.HarnessFor(ph))
					// CLA-639 M4: this exit is booked, so the panel's member
					// handler must not book it again when it records the member
					// as failed.
					end.deadBooked = true
				}
			}
			// last && a clean exit is the shape CLA-384 is about: the phase ran to
			// completion, pushed, and simply did not declare. A non-zero exit reached
			// the same place by crashing, which is a different failure with its own
			// reporting, and an earlier phase reaching here did not have declaring as
			// its job at all.
			//
			// The return tells whether the claim is actually back in the ready queue
			// — the thing a bounded retry can re-claim. A release the plane refused
			// (or that never ran) leaves the lease live, so a retry session would
			// only be refused and burned.
			if !panelMember {
				end.releasedToQueue = d.releaseHeldClaim(ctx, t, res, last && res.ExitCode == 0)
			}

			// CLA-451: the blind-spot convergence. Everything above learns about
			// claims by parsing clankerbar tool calls out of the stream; a session
			// that claimed by another channel (a raw API call with the same key,
			// a harness quirk, a renamed tool) ends checkpointable with NO observed
			// claim — and used to end the sequence here, stranding its pushed work
			// behind an expiring lease until a takeover recovered it at a reclaim's
			// cost. Before declaring the phase empty, ask the plane about the one
			// task this driver can name: the task it BRIEFED. If that task is held,
			// with a branch recorded, by some run of this project's key, treat it
			// as the phase's checkpoint exactly as an observed claim would be.
			//
			// Every conjunct below keeps an existing guard absolute:
			//
			//   - end.claim == nil && !res.Claim.Held() — an OBSERVED claim,
			//     settled or not, stays the authoritative view; the peek never
			//     overwrites or second-guesses one.
			//   - res.Untrusted == "" — a stream that could not be read whole is
			//     never read for claim state, and the plane is not asked to fill
			//     the hole either: the settle we never saw may be in the missing
			//     bytes (CLA-262).
			//   - !producedNothing — the dead-phase veto (CLA-386): a final step
			//     that finished "unknown" having produced nothing is not a
			//     checkpoint no matter what the plane says, so it is not even
			//     consulted.
			//   - the orderly-end disjunct mirrors `checkpointable` exactly, with
			//     the plane's recorded branch standing in for the HasWIP the cap
			//     path needs — a branch the PLANE confirms is stronger evidence
			//     than one read off a stream.
			//   - !last — a last phase has no successor to hold the lease for;
			//     a handoff cannot widen this either, because detectHandoff
			//     refuses on no observed claim before we get here.
			//
			// A claim the peek recovers then faces the SAME evidence gate as an
			// observed one: the recorded branch must really be on the origin
			// remote before the seam advances (CLA-457), because a plane record
			// alone cannot tell a pushed branch from a phantom one. Evidence that
			// fails keeps today's behavior — the drain ends, and the live lease
			// expires with its recorded branch intact as the takeover hand-off.
			// The peek itself is a READ of the briefed task (get_task), never a
			// claim and never a heartbeat: it cannot race a session out of work,
			// and a peek that fails or comes back not-held simply falls through
			// to the standard ending below.
			if end.claim == nil && !last && res.Untrusted == "" && !producedNothing &&
				!res.Claim.Held() && (res.ExitCode == 0 || capped || ceiling || wallclock) {
				if claim := d.peekCheckpointClaim(ctx, t, drainNum, ph.Label(phaseIdx), briefedTask); claim != nil {
					end.claim = claim
					var evidenceTip string
					end.branch, evidenceTip, evidenceOK, evidenceUnverified, evidenceWhy = d.checkpointEvidence(ctx, t, *claim, inv.WorkDir, &review)
					end.reviewRound = review.round
					// The review round's close rides this path too (CLA-637): a
					// recovered review checkpoint whose round stayed sealed would
					// spawn the fix phase against findings it cannot read. No
					// handoff is possible here (detectHandoff refuses an
					// unobserved claim), so the only skip is a following
					// review-shaped phase.
					if evidenceOK && review.required && review.round > 0 && review.state == "sealed" && !nextReviewShaped {
						if cerr := d.closeReviewRound(ctx, t, *claim, review.round); cerr != nil {
							evidenceOK = false
							evidenceWhy = fmt.Sprintf("review round %d could not be closed (%v) — refusing the checkpoint, because the fix phase would be spawned against findings the plane still has sealed", review.round, cerr)
						}
					}
					if evidenceOK {
						// Fold the VERIFIED branch onto the carried claim, exactly as
						// the observed-claim checkpoint above does: a recovered claim
						// carries the plane's recorded branch and checkpointEvidence
						// re-verified it, so the two already agree today — but the
						// invariant "the carried claim's branch is the verified one"
						// is what verifiedBranch and the takeover hand-off depend on,
						// so hold it by construction rather than by coincidence of a
						// single-source read. CLA-576: an unverified recovery rides
						// the same way, marked so its brief is the unverified variant.
						end.claim.Claim.Branch = end.branch
						end.claim.CheckpointUnverified = evidenceUnverified
						end.claim.CheckpointTip = evidenceTip
						end.checkpoint = true
						if evidenceUnverified {
							log.Printf("%siteration %d: %s ended with no claim observed in its stream, but the plane holds %s (run %s) with branch %s recorded — it could not be verified on the origin remote (%s), so this is an UNVERIFIED checkpoint: keeping the lease for the next phase; the review phase must verify the branch itself",
								labelOf(t), drainNum, ph.Label(phaseIdx), claim.Claim.TaskID, claim.Claim.RunID, end.branch, evidenceWhy)
						} else {
							log.Printf("%siteration %d: %s ended with no claim observed in its stream, but the plane holds %s (run %s) with branch %s verified on the origin remote — treating it as the phase checkpoint and keeping the lease for the next phase",
								labelOf(t), drainNum, ph.Label(phaseIdx), claim.Claim.TaskID, claim.Claim.RunID, end.branch)
						}
					} else {
						log.Printf("%siteration %d: the plane holds %s (run %s) with branch %q recorded, but that branch could not be verified on the origin remote (%s) — not treating it as a checkpoint; the lease expires with its hand-off intact",
							labelOf(t), drainNum, claim.Claim.TaskID, claim.Claim.RunID, end.branch, evidenceWhy)
					}
				}
			}
		}
		// Then check what it said it delivered. After the handback, because a dead
		// lease is time-sensitive and a git check is not; on every exit, because a
		// session that pushed nothing and died is exactly as likely to have recorded
		// a branch as one that finished cleanly.
		d.verifyDeliveries(ctx, t, res, inv.WorkDir)

		if ierr != nil {
			if ctx.Err() != nil {
				return tokens, cost, true, end, nil
			}
			// Count this attempt's spend before giving up on it. A non-exit run
			// failure is either a launch failure (nothing emitted, nothing parsed —
			// adding zeroes changes nothing) or a session that ran, announced its
			// spend and died in Wait (CLA-299): the Result was parsed precisely so
			// the budget would not lose what the attempt actually cost. That holds
			// only while the stream was read whole, which is what the guard below
			// makes binding: a failure whose stream ALSO came back untrusted
			// carries a floor, not a total, and gets the same refusal as the
			// untrusted branch below and endUntrustedDrain (CLA-262) - counted
			// nowhere, so no breaker ever acts on a figure that cannot be known.
			if res.Untrusted == "" {
				tokens += res.Tokens
				cost += res.CostUSD
				recordPassSpend(end.session, a, &pass, &res)
				d.charge(tc.HarnessFor(ph), res.Tokens, res.CostUSD)
			}
			// A run failure that was not an exit status — a launch failure, or one
			// of the deaths in Wait above — ends the attempt without any retry
			// classification. Not a blip.
			return tokens, cost, false, end, fmt.Errorf("invoke %s: %w", a.Name(), ierr)
		}

		// A session whose stream could not be read whole (CLA-262). Everything below
		// this line reads a figure parsed out of that stream, so counting the spend,
		// classifying the exit or retrying on the strength of it are three ways to
		// make a confident decision on data with a hole in it.
		if res.Untrusted != "" {
			// held is nil here by construction — an untrusted stream never holds a
			// phase open — so the sequence ends and nothing is carried forward.
			//
			// CLA-691: no spend figures were recorded on this pass's session entry
			// either (recordPassSpend runs only where the loop counts an attempt),
			// so the record says "unknown" instead of publishing a FLOOR from a
			// stream with a hole in it as if it were a report.
			utokens, ucost, ustop, uerr := d.endUntrustedDrain(t, drainNum, tc.HarnessFor(ph), res, tokens, cost)
			return utokens, ucost, ustop, end, uerr
		}

		// Count THIS attempt's spend toward the budget breaker regardless of how it
		// ends — usage-limit, transient, stop, or clean. A failed/retried attempt
		// still burned tokens, and a "leave headroom" breaker must err toward seeing
		// real spend, not under-counting it. Each attempt is a distinct session, so
		// summing per attempt (and returning the accumulator, not the final res)
		// counts every session exactly once.
		tokens += res.Tokens
		cost += res.CostUSD
		// The pass's own session entry takes the same figures, in the same breath
		// (CLA-691): every attempt the loop counts is money the record attributes
		// to the pass, and none it refuses is.
		recordPassSpend(end.session, a, &pass, &res)
		d.charge(tc.HarnessFor(ph), res.Tokens, res.CostUSD)

		// A usage limit. A rolling-window subscription cap is waited out and the
		// session re-run; a hard budget/credit exhaustion (Stop) has no reset to
		// poll for, so the run stops cleanly and the operator resumes it once
		// they've topped up.
		if lim := a.DetectLimit(res); lim.Limited {
			if lim.Stop {
				log.Printf("iteration %d stopped: %s — no reset to wait for, stopping (resume once resolved)",
					drainNum, limitReason(lim))
				return tokens, cost, true, end, nil
			}
			log.Printf("iteration %d hit a usage limit", drainNum)
			// A RESUMED phase must not wait one out (CLA-631). It is working a
			// live lease — 30 minutes, and nothing driver-side heartbeats it — so
			// a supervised wait of hours would re-spawn the phase against a run
			// the plane has long since swept or handed to another clanker, with a
			// runId in its prompt that no longer means anything. End the sequence
			// instead: the branch is pushed, the lease lapses, and the task comes
			// back as a takeover, which is the designed recovery for exactly
			// this. The RUN, though, must not end with it — exiting the daemon
			// left a whole fleet idle until an operator noticed — so the wait is
			// handed to drainPhases, which owes it only once the sequence's
			// handback has run and no lease is held.
			if prev != nil {
				log.Printf("iteration %d: a resumed phase hit a usage limit — ending the sequence rather than waiting out a reset on a 30-minute lease; the task returns as a takeover with its branch recorded, and the run stays up to wait for the reset instead of exiting",
					drainNum)
				end.sequenceEnd = &phaseSequenceEnd{limitWait: &phaseLimitWait{lim: lim, ph: ph, a: a}}
				return tokens, cost, false, end, nil
			}
			// Whatever this attempt held open is not held open any more: every path
			// below either waits or retries with a FRESH session, which re-claims.
			// Pre-phases the handback ran on every attempt, so this could not arise.
			end = d.releaseCarriedEnd(ctx, t, end)
			// Waiting out a reset that lands past the wall-clock ceiling buys
			// nothing: the budget check runs between drains, so the loop would
			// sleep through the window, run one session on the freshly reset quota
			// and stop on the very next check — having spent the night waiting for
			// headroom it then declines to use. Stop now and say when the quota
			// returns, so the operator can start a fresh run against it.
			remaining, bounded := tc.Budget.Remaining(time.Since(prior.start))
			if until, over := waitPastBudget(lim.ResetAt, remaining, bounded); over {
				log.Printf("iteration %d: the limit resets %s, in %s — more than the %s left of this run's ceiling; stopping now rather than waiting, so start a fresh run after the reset",
					drainNum, until.Format("Mon 15:04"), time.Until(until).Round(time.Minute), remaining.Round(time.Minute))
				return tokens, cost, true, end, nil
			}
			// The wait's own probe spend lands in the SAME accumulator as the
			// sessions', so it reaches the breaker at the top of this loop, the one in
			// Run between drains, and the iteration's cost line — rather than a
			// separate figure nothing is measured against (CLA-287).
			// Probed on the harness that hit the limit, in that harness's own
			// session shape: a usage limit is a property of one provider's account,
			// so probing the run's default harness would answer a question nobody
			// asked, and spend on it.
			ptokens, pcost, pstop := d.supervisedWait(ctx, lim, a, ph, t, spend{
				start:  prior.start,
				tokens: prior.tokens + tokens,
				cost:   prior.cost + cost,
			})
			tokens += ptokens
			cost += pcost
			if pstop {
				return tokens, cost, true, end, nil
			}
			continue
		}

		// Below the limit branch, so a usage-limit attempt is counted NEITHER way.
		// A session the subscription cap turned away often reports nothing at all
		// - under claude the notice can arrive on stderr with no stream, and under
		// codex as free text - so counting it as silence would end the run on the
		// ordinary overnight quota wait, and blame a harness that is starting
		// perfectly well. It is not a reset either: the cap is a known cause with
		// its own breakers (the budget check inside supervisedWait, waitPastBudget,
		// and the probes' own spend), and clearing the count on it would let a
		// limit between silences hide the loop this bounds.
		//
		// Everything from here down is an attempt that RAN and ended on its own
		// terms, which is where "did it tell us anything?" is the right question -
		// a different question from what it told us, since a report of zero is
		// still a report (CLA-288). Counted here, enforced at the top of the loop,
		// so the bound fires on the decision to spawn again rather than on this
		// attempt's exit.
		if res.UsageReported {
			noUsage = 0
		} else {
			noUsage++
		}

		if res.ExitCode == 0 {
			// A clean exit is the shape the CLA-398 quiet death wears, so the
			// marker gets named here rather than inferred downstream: a final
			// step_finish with reason "unknown" and all-zero usage used to read
			// as "iteration done (tokens=0 cost=$0.00)" — indistinguishable from
			// a cheap clean run, which is exactly what made these deaths
			// invisible. The adapter's own marker is the only thing that can
			// tell them apart.
			if zeroUsage {
				log.Printf("iteration %d: the session ended with the zero-usage-unknown signature (final step reason %q, all-zero usage) — a quiet death, not a clean run",
					drainNum, harness.FinishReasonUnknown)
			} else {
				log.Printf("iteration %d done (tokens=%d cost=$%.4f)", drainNum, tokens, cost)
			}
			return tokens, cost, false, end, nil
		}

		// A turn-capped end is this PHASE ending, never the run failing. It exits
		// non-zero and matches neither the limit scan nor the transient one, so
		// without this it would fall through to the non-retryable branch below and
		// stop the daemon — the backstop killing what it was added to protect.
		// Not retried either: the cap would fire again at the same place.
		if capped {
			log.Printf("iteration %d: the session hit its turn cap (tokens=%d cost=$%.4f) — ending this phase; anything uncommitted was salvaged above",
				drainNum, tokens, cost)
			return tokens, cost, false, end, nil
		}

		// A session the ADAPTER killed for crossing its per-session token ceiling
		// is the same shape as a turn cap: an orderly end with the salvage's
		// problem to handle, never a failure and never a retry. Retrying would
		// re-spend against the same runaway ceiling; failing would stop the run
		// over a kill that did its job (CLA-343).
		//
		// The cost is reported as unmeasured, never as a number: the result event
		// — the only carrier of total_cost_usd — is deliberately not parsed on a
		// killed stream, so res.CostUSD is 0 and printing it would under-report
		// the drain's spend as exactly nothing. The tokens are the honest figure.
		if ceiling {
			log.Printf("iteration %d: the session crossed its per-session token ceiling (tokens=%d; cost not captured — killed mid-stream) — ending this phase; anything uncommitted was salvaged above",
				drainNum, tokens)
			return tokens, cost, false, end, nil
		}

		// A session the ADAPTER ended for outliving its wall-clock cap is the same
		// shape again: the phase ends, and neither a retry nor a failure is right -
		// a retry would spend the same hours over again and reach the same deadline,
		// and failing would stop the run over a cap doing its job (CLA-368).
		//
		// Spend IS reported here, unlike the token-ceiling branch: opencode's usage
		// arrives per step_finish and is summed all the way to the kill, so the
		// figures are the honest cost of the session up to the moment it ended.
		//
		// Since CLA-365 opencode has BOTH HonoursSessionWallClock and
		// TracksClaims (Capabilities.TracksClaims: true), so a capped opencode
		// session holding its claim IS salvaged - the res.Claim.Held() arm above
		// is the one that fires today. The else branch (nothing salvaged) applies
		// only if an adapter enforcing this cap ever lacks claim observation
		// again; it is kept defensive, not descriptive of the present. The claim-held
		// wording was earned by CLA-365, not reserved for a future adapter.
		if wallclock {
			if res.Claim.Held() {
				log.Printf("iteration %d: the session outlived its wall-clock cap (tokens=%d cost=$%.4f) — ending this phase; anything uncommitted was salvaged above",
					drainNum, tokens, cost)
			} else {
				log.Printf("iteration %d: the session outlived its wall-clock cap (tokens=%d cost=$%.4f) — ending this phase. NOTHING was salvaged: this harness does not observe the session's task claim, so whatever the session left uncommitted is still in the worktree",
					drainNum, tokens, cost)
			}
			return tokens, cost, false, end, nil
		}

		// Non-zero exit, not the usage cap: a transient server/network blip backs
		// off and retries; anything else is a genuine failure and stops.
		if a.IsTransient(res) {
			// A RESUMED phase does not retry, for the same reason it does not wait
			// out a usage limit — and this path is the more dangerous of the two,
			// because MaxRetries defaults to 0, meaning never give up. Seeding the
			// resumed claim made the seam's handback live for phase 2, so a blip
			// here hands the task back mid-sequence while the retry is handed the
			// very same heartbeat(runId) brief; and where the handback is declined
			// (HasWIP), the backoff ladder walks past the 30-minute lease in about
			// six steps and then re-spawns, indefinitely, against a run the plane
			// has swept. End the sequence: the branch is pushed, the lease lapses,
			// and the task comes back as a takeover. The RUN, though, must not end
			// with it (CLA-651) — exiting the daemon on one provider blip left the
			// fleet idle until an operator noticed, the same failure CLA-631 fixed
			// for limits — so the sequence-end signal goes to drainPhases, which
			// owes it a wait (CLA-662): there is no reset for a blip, so the run
			// waits the target's growing backoff rung after the handback and then
			// polls again. The rung matters precisely when this phase's harness is
			// NOT the one the queue drains on: an opencode implement start can
			// never see the claude blip, so without the run-level wait the same
			// implement-and-lose loop would repeat with nothing bounding it.
			if prev != nil {
				log.Printf("iteration %d: a resumed phase hit a transient failure (%s) — ending the sequence rather than retrying on a 30-minute lease nothing renews; the task returns as a takeover with its branch recorded, and the run stays up to poll again instead of exiting",
					drainNum, res.ExitString())
				end.sequenceEnd = &phaseSequenceEnd{}
				return tokens, cost, false, end, nil
			}
			// A retry the adapter could not NAME is bounded on its own, because
			// nothing else bounds it: the dials are off by default and the
			// zero-spend counter is reset by the usage this heuristic requires. The
			// diagnostic goes in the error because it is the whole remedy - the
			// adapter has no pattern for this text, so an operator who can see it
			// can say whether one should be added (CLA-381).
			if a.IsUnclassifiedTransient(res) {
				unclassified++
				if unclassified > unclassifiedRetryBound {
					return tokens, cost, false, end, fmt.Errorf(
						"iteration %d: %w: %d retries on %s failures that matched no known-transient pattern - retried because the session had reported usage, but the same unrecognised failure keeps happening, so this is a real fault rather than a blip%s",
						drainNum, errUnclassifiedRetryLoop, unclassified-1, a.Name(), failureDetail(a.Diagnostic(res)))
				}
			}
			// As in the limit branch: the retry is a fresh session that re-claims,
			// so nothing may stay held open across it.
			end = d.releaseCarriedEnd(ctx, t, end)
			retries++
			if d.cfg.MaxRetries > 0 && retries > d.cfg.MaxRetries {
				return tokens, cost, false, end, fmt.Errorf(
					"iteration %d: transient failures persisted after %d retries (check https://status.claude.com; rerun to resume)",
					drainNum, d.cfg.MaxRetries)
			}
			wait := d.backoff(retries)
			log.Printf("iteration %d transient failure (%s) — %s in %s (a fresh session reclaims any half-done task)",
				drainNum, res.ExitString(), retryLabel(retries, d.cfg.MaxRetries), wait)
			if d.waitOrStop(ctx, wait) {
				return tokens, cost, true, end, nil
			}
			continue
		}

		// Stopping here ends the whole run, so say WHAT was judged non-retryable.
		// Without it the operator gets "exited 1 (non-retryable)" and no way to
		// tell a genuine failure from a blip the classifier merely does not know
		// yet — which is the one thing they need in order to report the gap. The
		// text is the harness's own diagnostic scope, never the raw stream, so
		// the agent's narration is not quoted back at them (CLA-258).
		return tokens, cost, false, end, fmt.Errorf("iteration %d: %s %s (non-retryable) — stopping%s%s",
			drainNum, a.Name(), res.ExitString(), failureDetail(a.Diagnostic(res)), droppedNote(res.OutputDropped))
	}
}

// endUntrustedDrain closes out a drain whose session output could not be read
// whole, without acting on anything that stream said (CLA-262).
//
// # Why the spend is not counted
//
// It is not that the figure is inconvenient — it is that it is not a measurement.
// The `result` event carrying a claude session's whole tokens-and-cost total is
// the LAST thing on the stream, so a stream cut short reports zero for a session
// that may have cost hundreds of dollars, and a partial sum from an adapter that
// accumulates per step is a lower bound of unknown looseness. Adding either to the
// accumulator would put a made-up number in front of the breaker and in the
// iteration's cost line.
//
// # Why a spend ceiling then STOPS the run
//
// Because the ceiling is a promise, and this is the driver saying it can no longer
// keep it. An operator who set `max_tokens` or `max_cost_usd` has asked to be
// protected from exactly the shape this is: sessions whose cost nothing can see.
// One unaccounted session is survivable; a night of them against an inert ceiling
// is the failure CLA-287 and CLA-258 were both about. Stopping is clean, loud, and
// resumable — the operator restarts the run once they know why.
//
// With no spend ceiling set there is nothing to break: the wall clock does not
// depend on anything the child said, so the run carries on and the no-progress
// back-off is what bounds a target that keeps producing these.
//
// The harness NAME is the phase's own, not the run's: whether a spend ceiling is
// live is a per-harness question since CLA-367, and under per-phase harnesses
// (CLA-366) the session that went unreadable is not necessarily on d.h. Asking
// d.h would let an unreadable opencode session be waved through on the grounds
// that CLAUDE has no ceiling set.
func (d *Driver) endUntrustedDrain(t Target, drainNum int, harnessName string, res harness.Result, tokens int, cost float64) (int, float64, bool, error) {
	log.Printf("iteration %d UNTRUSTED — %s", drainNum, res.Untrusted)
	log.Printf("iteration %d: not counting this session's parsed spend (tokens=%d cost=$%.4f — a floor, not a total), not classifying its exit (%s), and not handing back any claim it appeared to hold",
		drainNum, res.Tokens, res.CostUSD, res.ExitString())
	if !d.targetCfg(t).Budget.CountsSpendFor(harnessName) {
		return tokens, cost, false, nil
	}
	log.Printf("iteration %d: stopping — a token/cost ceiling is set and this session's real spend cannot be known, so the ceiling can no longer be honoured. Check the iteration log, then rerun to resume.", drainNum)
	return tokens, cost, true, nil
}

// salvageStrandedWork commits and pushes the work a dead session left in its
// worktree, and records the branch on the task so another machine can pick it up
// (CLA-314).
//
// workdir is where the session actually ran — the resolved spawn directory, not
// rebuilt from the target — because that is where its worktree lives once
// sessions start in per-task checkouts (CLA-437). Salvaging from anywhere else
// would report "nothing to salvage" over real stranded work.
//
// # Why it runs on EVERY ending, not only on a usage limit
//
// The worktree is what is being rescued, and it looks identical whether the
// session was killed by a limit, died in a crash, or simply stopped with the work
// unfinished — while the cost of losing it is the same in all three. The limit,
// besides, is detected by parsing the stream AFTER the fact, so gating on it
// would skip exactly the sessions whose stream could not be read: the ones we
// know least about, and the likeliest to have stranded something. Running it when
// it was not needed costs nothing — a clean worktree commits nothing, pushes
// nothing and records nothing.
//
// # What it will not save (CLA-695)
//
// Untracked files alone, when the session DELIVERED — the branch it recorded for
// the task is on the origin at exactly the worktree's HEAD. There is nothing to
// rescue, and committing a generated file (a `wrangler types` dump, a report, a
// build artifact) pushes a NEW tip at the phase boundary: every review-panel
// member, the fix phase and the PR then ride a commit nobody chose, which is
// exactly what happened on the first review-panel run. Tracked edits are still
// salvaged on top of a delivered tip, and an undelivered session's untracked files
// are still salvaged in full — new source files it never added are the work
// CLA-314 exists to save. Delivery is the discriminator, and internal/salvage
// checks it against the origin rather than taking the session's word for it.
//
// # Why it is safe on the untrusted path
//
// CLA-262 forbids acting on a truncated stream's CLAIM-STATE: a settle we never
// saw may be in the bytes that never arrived, so handing the task back could post
// `ready` over work already in review. This does something different in kind.
// Recording a branch carries no status, so it cannot move a task, clear a holder
// or revert a review — it is additive, and its worst case on a task that really
// did reach review is that the branch field names the branch the work is on
// anyway. What it acts on is the local worktree, which no truncation can
// misreport. A claim we SAW settle is skipped entirely: Held() is false, and this
// is a rescue, not a tidy-up.
//
// # What a success does to the handback
//
// Claim.HasWIP is set only when the plane ACCEPTED the branch, and that is what
// makes the claim non-releasable below: the lease is left to expire so the task
// stays a takeover and the hand-off survives. If the record failed, the task has
// no branch on it, releasing to `ready` is still the better move, and the old
// path runs unchanged.
func (d *Driver) salvageStrandedWork(ctx context.Context, t Target, res *harness.Result, producedNothing bool, workdir string) {
	if d.newSalvager == nil || !res.Claim.Held() {
		return
	}
	// Detached from ctx, like the handback and unlike the delivery checks: a
	// cancelled run (Ctrl-C, SIGTERM) is precisely a session killed mid-task, so
	// this is when work goes missing, not when it can be skipped. Bounded, so a
	// wedged git or an unreachable remote cannot hold up the shutdown.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), salvageTimeout)
	defer cancel()

	out := d.newSalvager(workdir).Salvage(sctx, res.Claim.TaskID, res.Claim.Ref, recordedBranches(*res)...)
	label := claimLabel(res.Claim)
	switch out.Status {
	case salvage.Nothing:
		// Silent on the ordinary case. A session that committed its own work leaves
		// a clean tree, and a log line every iteration saying so would bury the ones
		// that matter.
		if out.Worktree != "" {
			if producedNothing {
				// CLA-386: the tree is clean because nothing was ever written, not
				// because the work landed. The line must say so plainly — "nothing to
				// salvage" reads identically either way, and the only other
				// discriminator (a `verified <ref>: origin/<branch>` line) is an
				// absence, which is the weakest possible signal. Gated on the driver's
				// own classification (not the raw deadPhase predicate): a capped or
				// untrusted session is an orderly end whose finish reason is its own
				// marker or cannot be read, and must not be handed the emphatic line.
				log.Printf("%sphase produced nothing for %s — the worktree is clean because no work was ever written to it, not because it was committed: %s",
					labelOf(t), label, out.Detail)
			} else {
				log.Printf("%snothing to salvage for %s: %s", labelOf(t), label, out.Detail)
			}
		}
		return
	case salvage.Untracked:
		// CLA-695: the session DELIVERED — the branch it recorded is on the origin
		// at exactly the worktree's HEAD — and left nothing but untracked files.
		// There is nothing to rescue and nothing to record, so this is a report
		// rather than an alarm: the paths are named because an untracked file can
		// still be work (a source file written after the last commit), and only a
		// reader can tell that from the generated output this rule exists for. The
		// claim is left exactly as the Refused and Failed cases leave it — no
		// branch recorded, so the ordinary handback rules apply unchanged.
		left := strings.Join(out.Paths, ", ")
		if left == "" {
			// A salvager that names the paths only in its prose still gets them in
			// front of the operator: a log line ending in a colon says nothing.
			left = out.Detail
		}
		log.Printf("%sdelivered %s with only untracked files left in %s — committing them would move the branch later readers review, so they are left alone: %s",
			labelOf(t), label, out.Worktree, left)
		return
	case salvage.Refused:
		log.Printf("%sSTRANDED WORK LEFT AS IS — %s: %s", labelOf(t), label, out.Detail)
		return
	case salvage.Failed:
		log.Printf("%sSALVAGE FAILED — %s: %s", labelOf(t), label, out.Detail)
		return
	}

	log.Printf("%ssalvaged %s: %s", labelOf(t), label, out.Detail)
	rec, ok := t.Releaser.(plane.Recorder)
	if !ok {
		log.Printf("%sthe branch %s is pushed but cannot be recorded on %s (no plane writes configured) — a clanker on this machine can still find it by name",
			labelOf(t), out.Branch, label)
		return
	}
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer rcancel()

	if err := rec.RecordBranch(rctx, res.Claim.TaskID, res.Claim.RunID, out.Branch); err != nil {
		if !errors.Is(err, plane.ErrNotWired) {
			log.Printf("%scould not record branch %s on %s: %v — the work IS pushed, so fetch that branch by name rather than starting again",
				labelOf(t), out.Branch, label, err)
		}
		return
	}
	// Only now. HasWIP is a statement about the TASK, not about the disk: it means
	// the next clanker will be told there is work to fetch.
	res.Claim.HasWIP = true
	log.Printf("%srecorded %s as the hand-off branch for %s — the next clanker takes it over instead of starting again",
		labelOf(t), out.Branch, label)
}

// salvageTimeout bounds one rescue: a status read, a commit, and a push over the
// network. Longer than the delivery checks' minute because this one uploads.
const salvageTimeout = 3 * time.Minute

// claimLabel is the task's most human-readable identifier.
func claimLabel(c harness.Claim) string {
	if c.Ref != "" {
		return c.Ref
	}
	return c.TaskID
}

// deadReason renders the finish reason a dead phase was classified on, for a log
// line. The classification fired on FinishReasonUnknown, so a nil claim — a
// session that observed no task — still has a reason to name.
//
// The adapter's OWN terminal_reason marker wins when present: ZeroUsageReason
// (CLA-398) names the quiet-death signature more precisely than the bare
// "unknown" it rides on, so the operator's log says which flavour of death this
// was instead of the driver re-inferring it from the zero usage downstream.
func deadReason(res *harness.Result) string {
	if res != nil {
		if r, ok := res.Raw[harness.TerminalReasonKey].(string); ok && r != "" {
			return r
		}
		if r, ok := res.Raw[harness.FinishReasonKey].(string); ok && r != "" {
			return r
		}
	}
	return harness.FinishReasonUnknown
}

// outputCapStallReasoning reports whether this session ended on the CLA-584
// output-cap stall, and the stalled final step's own reasoning-token count when
// it did. The question is asked of the adapter's own terminal_reason marker —
// the same channel deadReason reads — never re-derived from the finish reason,
// so a harness that cannot produce the stall simply never answers true.
//
// It exists so the hand-back can say WHY a session stopped holding a task it
// never moved on: the MAK-123 post-mortem found only "the phase finished but
// never moved the task on", with nothing in the daemon log saying the model had
// spent its whole output budget thinking (reasoning=32000) and emitted nothing.
func outputCapStallReasoning(res harness.Result) (int, bool) {
	if r, _ := res.Raw[harness.TerminalReasonKey].(string); r != harness.OutputCapReason {
		return 0, false
	}
	n, _ := res.Raw[harness.OutputCapReasoningKey].(int)
	return n, true
}

// fleetTrip pauses a target whose fleet dead-phase counter has reached
// fleetDeadBound (CLA-396): that many consecutive dead phases across tasks mean
// the harness or provider is broken right now, not the tasks, and every further
// session would pay the same death rate for nothing. The target stops spawning
// until the operator answers the raised project-level question (the open-question
// count falls back to what it was at the raise — see the poll gate in Run).
//
// The question is PROJECT-level — ask_question with no taskId — because a fleet
// trip is not one task's triage: pinning it to whichever task happened to be in
// flight would make the operator answer about a bystander. It is filed as a
// non-blocking `decision` — there is no task to block (the loop's pause is the
// enforcement), and the operator's ruling on a fleet incident is a project-level
// judgment worth standing in the decision log, unlike the park's one-task
// clarification.
//
// "Exactly one" is guaranteed by fleetRaised: while the pause is in force the
// flag is set, so a later trip in the same episode does not re-raise. The flag
// clears with the pause, so a NEW episode — the operator answered, the loop
// resumed, and the fleet died again — raises a fresh question.
//
// If the question cannot be filed, the pause is NOT set: a pause the operator
// can never be told about and can never resume is a permanent stall, which is
// worse than the fall-through (per-task parking still bounds each task). The
// dead-phase counter stays at the bound, so the next dead phase tries again —
// loudly.
func (d *Driver) fleetTrip(ctx context.Context, t Target, ti, drainNum int, res *harness.Result) {
	pk, ok := t.Releaser.(plane.ParkAPI)
	if !ok {
		log.Printf("%siteration %d: %d consecutive dead phases across tasks, but the releaser cannot file a project question — NOT pausing (the operator could never resume it); the per-task budget still bounds each individual task",
			d.prefix(ti), drainNum, d.fleetDead[ti])
		return
	}
	if !d.fleetRaised[ti] {
		d.fleetRaised[ti] = true
		// The baseline the resume signal is measured against: the open-question
		// count at the last poll, BEFORE the question below raises it by one.
		d.fleetOpenQ[ti] = d.openQs[ti]
		body := fmt.Sprintf(
			"**%d consecutive dead phases across tasks — the provider or harness looks broken, not the tasks.** Each task died producing nothing (final step reason %q, no branch recorded) before this, so the driver paused this project rather than burning more sessions against a provider that is already known to be failing.\n\nThe loop is PAUSED for this project until you answer. Answering resumes it; the next dead phase will pause it again and raise a fresh question. Iteration logs are in %s.",
			d.fleetDead[ti], deadReason(res), d.state.Path())
		options := []string{
			"The provider recovered — resume draining this project",
			"Still investigating — I will answer when it is safe to resume",
			"Switch or restart the harness — try again",
			"Something else is wrong — stop the daemon",
		}
		pctx, pcancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer pcancel()
		if err := pk.AskProjectQuestion(pctx, body, options, "decision"); err != nil {
			log.Printf("%siteration %d: the fleet dead-phase question could not be filed: %v — NOT pausing (the operator could never resume it); the next dead phase will retry the raise",
				d.prefix(ti), drainNum, err)
			d.fleetRaised[ti] = false
			return
		}
	}
	d.fleetPaused[ti] = true
	log.Printf("%siteration %d: %d consecutive dead phases across tasks — the provider or harness looks broken, not the tasks; pausing this project until the operator answers the raised question",
		d.prefix(ti), drainNum, d.fleetDead[ti])
}

// startupTrip pauses a target whose harness-startup counter has reached
// startupFailBound (CLA-562): that many consecutive spawns failing before any
// usage report means the harness or provider is broken right now, not the
// tasks, and every further spawn would pay the same sideline-and-retry for
// nothing. Same shape as fleetTrip: the target stops spawning until the
// operator answers the raised project-level question (the open-question count
// falls back to what it was at the raise - see the poll gate in Run).
//
// The question is PROJECT-level - ask_question with no taskId - because a
// startup trip is not one task's triage: pinning it to whichever drain happened
// to be in flight would make the operator answer about a bystander. It is filed
// as a non-blocking `decision` - there is no task to block (the loop's pause is
// the enforcement), and the ruling is project-level judgment for the decision
// log.
//
// The body carries the harness exit signature (the sideline cause), as the
// sideline log line does: "provider looks broken" without the error string
// sends the operator grep-hunting through state-dir logs.
//
// "Exactly one" is guaranteed by startupRaised, cleared with the pause so a new
// episode raises fresh. If the question cannot be filed, the pause is NOT set
// and the counter stays at the bound, so the next startup failure retries the
// raise - loudly - while the sideline backoff keeps bounding each attempt.
func (d *Driver) startupTrip(ctx context.Context, t Target, ti, drainNum int, cause error) {
	pk, ok := t.Releaser.(plane.ParkAPI)
	if !ok {
		log.Printf("%siteration %d: %d consecutive harness-startup failures, but the releaser cannot file a project question - NOT pausing (the operator could never resume it); the sideline backoff still bounds each attempt",
			d.prefix(ti), drainNum, d.startupFails[ti])
		return
	}
	if !d.startupRaised[ti] {
		d.startupRaised[ti] = true
		// The baseline the resume signal is measured against: the open-question
		// count at the last poll, BEFORE the question below raises it by one.
		d.startupOpenQ[ti] = d.openQs[ti]
		body := fmt.Sprintf(
			"**%d consecutive harness-startup failures - the harness or provider looks broken, not the tasks.** Each spawn failed before reporting any usage (workdir %s), so the driver paused this project rather than sidelining and retrying forever against a harness that is already known to be failing.\n\nCause: %v\n\nThe loop is PAUSED for this project until you answer. Answering resumes it; the next startup failure will pause it again and raise a fresh question. Iteration logs are in %s.",
			d.startupFails[ti], d.workdirOf(t), cause, d.state.Path())
		options := []string{
			"The harness recovered - resume draining this project",
			"Still investigating - I will answer when it is safe to resume",
			"Switch or restart the harness - try again",
			"Something else is wrong - stop the daemon",
		}
		pctx, pcancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer pcancel()
		if err := pk.AskProjectQuestion(pctx, body, options, "decision"); err != nil {
			log.Printf("%siteration %d: the harness-startup question could not be filed: %v - NOT pausing (the operator could never resume it); the next startup failure will retry the raise",
				d.prefix(ti), drainNum, err)
			d.startupRaised[ti] = false
			return
		}
	}
	d.startupPaused[ti] = true
	log.Printf("%siteration %d: %d consecutive harness-startup failures - the harness looks broken, not the tasks; pausing this project until the operator answers the raised question. Cause: %v",
		d.prefix(ti), drainNum, d.startupFails[ti], cause)
}

// parkDeadPhase parks the task after four consecutive dead phases, filing an
// OPEN question so the park reaches the operator instead of vanishing into the
// Done tab (CLA-395). The question is a non-blocking `clarification` — the task
// is already parked, so there is nothing left to block, and a `decision` would
// read as project-wide standing law rather than one task's triage. Its body and
// options are modelled on the plane's own escalateExhausted shape: what
// happened, the dead-phase signature, how many sessions it cost, where the
// iteration logs are, and options covering retry / re-scope / leave parked /
// drop (CLA-386, per the 2026-08-20 operator decision: retry the implement
// phase four times, then park; the budget is per task).
//
// The phase is dead — its sessions are gone — so nobody is left to declare the
// failure; the driver must. The claim was released before this ran (the retry
// re-claimed, then died again), so the park is a plain status write signed by
// the dead phase's own run.
func (d *Driver) parkDeadPhase(ctx context.Context, t Target, phaseLabel string, res *harness.Result) {
	if res == nil || res.Claim.TaskID == "" {
		log.Printf("%sdead-phase park: no claim observed, so there is no task to park", labelOf(t))
		return
	}
	pk, ok := t.Releaser.(plane.ParkAPI)
	if !ok {
		log.Printf("%sCANNOT PARK %s — four consecutive dead phases (final step reason %q, no branch recorded), but the releaser cannot park or raise a question; the task stays in the queue and the next claim will retry it again",
			labelOf(t), claimLabel(res.Claim), deadReason(res))
		return
	}
	sig := deadReason(res)
	ref := claimLabel(res.Claim)
	outcome := fmt.Sprintf(
		"Parked after four consecutive dead phases: the %s session died producing nothing (final step reason %q, no branch recorded) on each attempt. Per the 2026-08-20 operator decision, a task that can kill four full sessions reaches the operator rather than a fifth; the retry budget is per task. Iteration logs are in %s.",
		phaseLabel, sig, d.state.Path())
	body := fmt.Sprintf(
		"**%s has defeated four sessions and is now parked.** The %s phase died producing nothing (final step reason %q, no branch recorded) on four consecutive attempts, so the driver stopped rather than spending a fifth session on it.\n\n"+
			"The task is `parked` — nothing will pick it up until an operator decides what to do with it. The iteration logs are in %s.",
		ref, phaseLabel, sig, d.state.Path())
	options := []string{
		"Set it back to `ready` — the blocker is gone, let the fleet try again",
		"Leave it parked — I will work it myself",
		"It needs re-scoping before anyone tries again",
		"Drop it — this is not worth another attempt",
	}

	pctx, pcancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer pcancel()
	if err := pk.Park(pctx, res.Claim.TaskID, res.Claim.RunID, outcome); err != nil {
		log.Printf("%spark failed for %s: %v — the task is left for the next claim to retry it again",
			labelOf(t), claimLabel(res.Claim), err)
		return
	}
	if err := pk.AskQuestion(pctx, res.Claim.TaskID, body, options, false, "clarification"); err != nil {
		// The task IS parked — it will not be retried — so saying it was "left
		// for the next claim" would be the opposite of what happened. The park
		// outcome stands alone; the missing question is the failure the operator
		// needs to know about (review finding).
		log.Printf("%sparked %s — four consecutive dead phases, but the question for the operator could not be filed: %v",
			labelOf(t), claimLabel(res.Claim), err)
		return
	}
	log.Printf("%sparked %s — four consecutive dead phases, question filed for the operator",
		labelOf(t), claimLabel(res.Claim))
}

// releaseHeldClaim hands a task the session was still holding back to the queue,
// instead of leaving its lease to run out with nobody heartbeating it.
//
// It runs on EVERY exit from a session, deliberately. A usage limit, a transient
// blip, an outright failure and a clean finish with the work unfinished all leave
// the same dead lease behind — and the usage-limit case is the worst of the four,
// because the driver then goes to sleep for as long as the reset takes. One real
// run slept ninety minutes on a live claim and cost the task a reclaim it had not
// earned; the budget is two before the plane parks the task for the operator.
//
// A claim with pushed work is left alone; Claim.Releasable says why.
// releaseCarried hands back a claim a phase was holding open for a successor
// that is not going to run, and returns nil so the caller cannot hand it back
// twice. A nil carried claim is a no-op, so callers need not check first.
//
// It exists because "held open across the seam" and "retry this phase with a
// fresh session" are contradictory: before phases, releaseHeldClaim ran on every
// attempt and neither the limit wait nor the transient backoff could leave a
// lease unattended. Both loop back to a session that re-claims, so both have to
// undo the hold first.
func (d *Driver) releaseCarried(ctx context.Context, t Target, carried *harness.Result) *harness.Result {
	if carried == nil {
		return nil
	}
	d.releaseHeldClaim(ctx, t, *carried, false)
	return nil
}

// releaseCarriedEnd is releaseCarried for a phase about to loop back to a FRESH
// session — a usage-limit wait, a transient backoff. It hands back whatever the
// attempt was holding open and returns an end that owes nothing further, so a
// seam's hold cannot survive into a retry that is going to re-claim anyway.
func (d *Driver) releaseCarriedEnd(ctx context.Context, t Target, end phaseEnd) phaseEnd {
	// Only a hold that actually happened needs undoing. Every other end has
	// ALREADY been through the handback in drainPhase's seam block, and releasing
	// again would hand the task back twice — which on a usage-limit exit is the
	// common path, not a corner: the seam releases (the phase did not reach its
	// checkpoint), and then the limit branch loops back to wait.
	if !end.checkpoint || end.claim == nil {
		return end
	}
	// Read BEFORE the call, for the same reason drainPhase reads it: a zero or
	// settled claim releases nothing, and reporting it as released would erase a
	// live claim upstream.
	held := end.claim.Claim.Held()
	d.releaseCarried(ctx, t, end.claim)
	// The session rides the reconstruction (CLA-630): it is record data for the
	// fleet iteration record, not a hold to undo, and the pass it names really
	// did spawn. The limit branch calls this BEFORE its stop paths return — a
	// reset past the run's ceiling, a stop during the wait, a retry that fails
	// to launch — and dropping the field there would omit a spawned session
	// from the record. On the paths that loop back, the next attempt's own end
	// replaces this one, so carrying it costs nothing.
	return phaseEnd{released: held, session: end.session}
}

// countUndeclared says whether THIS call site is the one where a held-with-work
// claim means the CLA-384 failure: a phase that finished cleanly and did not hand
// the task over. Only the seam's final-phase handback passes true. Every other
// caller reaches the same branch for a reason that is not a failure to declare -
// a budget trip between phases, a crash, a transient retry, a usage-limit wait, a
// refused handoff - and counting those would make the number mean nothing, which
// is the only thing it has going for it.
//
// It returns whether the claim is ACTUALLY back in the ready queue at the end —
// that is, whether releaseHeldClaim really released it. The seam uses that to
// decide whether a bounded retry may re-claim (a claim left to the takeover path,
// or a release the plane refused, is still live and a fresh session would only be
// refused); the other callers ignore it.
func (d *Driver) releaseHeldClaim(ctx context.Context, t Target, res harness.Result, countUndeclared bool) bool {
	// A stream read only in part tells us which calls we SAW, never which the
	// session made: the settle that released the task may simply be in the bytes
	// that never arrived. Handing the task back on that reading posts `ready` over
	// work already in review, which is the one outcome worse than a lease expiring
	// (CLA-262). Let it expire instead — the plane's own sweep does the right thing
	// with it, and keeps the takeover hand-off if there is one.
	if res.Untrusted != "" {
		if res.Claim.Held() {
			log.Printf("%ssession appeared to end holding %s, but its output could not be read whole — leaving the lease to expire rather than releasing a task that may already be settled",
				labelOf(t), res.Claim.TaskID)
		}
		return false
	}
	if !res.Claim.Held() {
		return false
	}
	if !res.Claim.Releasable() {
		// Leaving the lease alone is right — see Releasable. Whether it is also a
		// FAILURE depends entirely on which call site got here; only that caller
		// knows, so only that caller says so.
		//
		// CLA-584: a session the adapter marked as an output-cap stall is named
		// by its cause, in place of the generic line — the operator reading
		// daemon.log after MAK-123 could only see "the phase finished but never
		// moved the task on", which says nothing about what stopped it.
		stallReason, stalled := outputCapStallReasoning(res)
		if countUndeclared {
			d.undeclared++
			if stalled {
				log.Printf("%sstalled: output cap hit with no output (reasoning=%d) — session ended holding %s with pushed work; leaving the lease to expire so the takeover hand-off survives (undeclared hand-offs this run: %d)",
					labelOf(t), stallReason, claimLabel(res.Claim), d.undeclared)
				return false
			}
			log.Printf("%ssession ended holding %s, which has pushed work — leaving the lease to expire so the takeover hand-off survives (undeclared hand-offs this run: %d — the phase finished but never moved the task on, so the next clanker pays to rediscover it)",
				labelOf(t), res.Claim.TaskID, d.undeclared)
			return false
		}
		if stalled {
			log.Printf("%sstalled: output cap hit with no output (reasoning=%d) — session ended holding %s with pushed work; leaving the lease to expire so the takeover hand-off survives",
				labelOf(t), stallReason, claimLabel(res.Claim))
			return false
		}
		log.Printf("%ssession ended holding %s, which has pushed work — leaving the lease to expire so the takeover hand-off survives",
			labelOf(t), res.Claim.TaskID)
		return false
	}
	if t.Releaser == nil {
		return false
	}
	// A stalled session is named by its cause on THIS path too (CLA-584), not
	// only on the held-with-work branch above: a stall that never pushed a
	// branch is the shape that leaves nothing behind, so "handed back to the
	// queue" alone would send the next clanker to rediscover the cause — the
	// MAK-123 post-mortem's complaint. Computed before the release so the line
	// that reports its outcome can carry it; the named line REPLACES the
	// generic one on success, as it does above.
	stallReason, stalled := outputCapStallReasoning(res)
	// Detach from ctx: a cancelled run (Ctrl-C, SIGTERM) is exactly when a claim
	// would otherwise be abandoned, so the handback has to outlive the signal that
	// prompted it. Bounded, so a wedged plane cannot hold up the shutdown.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()

	if err := t.Releaser.Release(rctx, res.Claim.TaskID, res.Claim.RunID); err != nil {
		if !errors.Is(err, plane.ErrNotWired) {
			log.Printf("%scould not hand %s back: %v — its lease will expire instead", labelOf(t), res.Claim.TaskID, err)
		}
		return false
	}
	if stalled {
		log.Printf("%sstalled: output cap hit with no output (reasoning=%d) — session ended holding %s; handed it back to the queue for another clanker",
			labelOf(t), stallReason, claimLabel(res.Claim))
		return true
	}
	log.Printf("%shanded %s back to the queue (the session ended still holding it)", labelOf(t), res.Claim.TaskID)
	return true
}

// verifyDeliveries checks, against local git, what the session told the plane it
// delivered — and says so out loud (CLA-253).
//
// The plane is a pipe: it stores a recorded branch and a declared merge and takes
// the clanker at its word, because it has no access to a git host and, by the
// standing decision behind this, is not being given any. The driver is already
// local and already in the tree, so it is the one place the claim can simply be
// looked at.
//
// # Warn, do not refuse
//
// A failed check is logged loudly and names what is unpushed or unmerged. It does
// NOT override the session's report, and this is a deliberate first cut rather
// than an oversight: refusing would mean the driver reverting a status a session
// chose, on the strength of a check that can be wrong about which tree it is
// looking at. Loud logging is the floor, it is cheap, and it is reversible.
// Recorded as a decision on CLA-253.
//
// "Refusing the delivery" (CLA-310) lives inside that shape: a PR check that
// fails is a Fail with the failing condition and its remedy named in the log,
// and no verified attestation behind it — the driver's verdict on the delivery
// is refusal, not a rewrite of what the session told the plane.
//
// # Fail open
//
// A check that could not run reports "could not verify" and the run carries on.
// Blocking a legitimate closure because the tree could not be found would be worse
// than the gap this replaces — and an Unknown is never allowed to read as a pass:
// the attestation below is written only for a check that actually ran.
func (d *Driver) verifyDeliveries(ctx context.Context, t Target, res harness.Result, workdir string) {
	if len(res.Reports) == 0 || d.newVerifier == nil {
		return
	}
	// Unlike the handback, this is NOT worth outliving a Ctrl-C. The handback races
	// a lease that is already ticking; a delivery claim will still be wrong in the
	// morning, and the checks reach the network, so insisting on them would add a
	// visible stall to every interactive shutdown.
	if ctx.Err() != nil {
		return
	}

	// workdir is the session's actual spawn directory (CLA-437), for the same
	// reason salvageStrandedWork takes it: the commits being checked live in the
	// repo the session worked in, which is no longer always the target's.
	v := d.newVerifier(workdir, d.cfg.AllowUncheckedPRFor(t.Name))
	for _, rep := range res.Reports {
		// Resolve the claim's repo AUTHORITATIVELY from the task's own record —
		// "the task's repo field (primary repo when unset)" is the doneWhen's
		// words, and the session-declared value on update_task is secondary: a
		// session can omit it (nothing in the served protocol tells it to pass
		// `repo`), while the plane's task row always carries it (CLA-437). Mirror
		// sessionDir's degradation exactly: an unwired plane falls back to the
		// session's declaration, then to this target's primary repo.
		repo := rep.Repo
		if src, ok := t.Releaser.(plane.TaskRepoSource); ok && rep.TaskID != "" {
			r, err := src.TaskRepo(ctx, rep.TaskID)
			switch {
			case err == nil && r != "":
				repo = r
			case err == nil && repo == "":
				repo = t.PrimaryRepo
			case errors.Is(err, plane.ErrNotWired):
				if repo == "" {
					repo = t.PrimaryRepo
				}
			default:
				log.Printf("%scould not read the repo of task %s (%v) — the claim's session-declared repo is used as-is", labelOf(t), rep.Ref, err)
			}
		} else if repo == "" {
			repo = t.PrimaryRepo
		}
		claim := delivery.Claim{Label: rep.Label(), Branch: rep.Branch, PR: rep.PR, Repo: repo}
		if rep.ClaimsMerge() {
			claim.Commit, claim.IntegrationBranch = rep.Commit, rep.IntegrationBranch
		}
		// Detached and bounded PER REPORT: a shared deadline would silently degrade
		// every claim after the first slow one to "cannot check". Detached because a
		// cancel arriving mid-check should not turn a real answer into a half one —
		// the guard above is what makes cancellation prompt. The PR check's own
		// polling budget sits well inside this deadline, so one slow GitHub read
		// cannot starve the git checks that share the report.
		vctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deliveryCheckTimeout)
		out := v.Verify(vctx, claim)
		cancel()

		for _, c := range out.Checks {
			where := labelOf(t)
			if out.Repo != "" && out.Repo != workdir {
				where += "[" + out.Repo + "] "
			}
			switch c.Status {
			case delivery.Fail:
				log.Printf("%sDELIVERY UNVERIFIED — %s: %s", where, rep.Label(), c.Detail)
			case delivery.Warn:
				log.Printf("%sDELIVERY WARN — %s: %s", where, rep.Label(), c.Detail)
			case delivery.Unknown:
				log.Printf("%scould not verify %s: %s — carrying on", where, rep.Label(), c.Detail)
			default:
				log.Printf("%sverified %s: %s", where, rep.Label(), c.Detail)
			}
		}
		d.attestMerge(ctx, t, rep, out)
	}
}

// deliveryCheckTimeout bounds one session's worth of git checks, including the
// `ls-remote` that goes over the network.
const deliveryCheckTimeout = 60 * time.Second

// attestMerge writes the driver's verdict onto the task's delivery record, so the
// answer survives the log. Only for a check that RAN: `mergeVerified` means "I ran
// the ancestor check and this is what it said", and writing it off the back of a
// check that could not run would be the same false attestation this exists to
// catch. Best-effort throughout — an unreachable or unwired plane costs the record,
// not the run, and the loud log above already carries the finding.
func (d *Driver) attestMerge(ctx context.Context, t Target, rep harness.Report, out delivery.Report) {
	verified, ran := out.MergeVerified()
	if !ran || rep.TaskID == "" || rep.RunID == "" {
		return
	}
	a, ok := t.Releaser.(plane.Attester)
	if !ok {
		return
	}
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()

	if err := a.AttestMergeVerified(actx, rep.TaskID, rep.RunID, plane.Delivery{
		Commit:            rep.Commit,
		IntegrationBranch: rep.IntegrationBranch,
		PR:                rep.PR,
	}, verified); err != nil {
		if !errors.Is(err, plane.ErrNotWired) {
			log.Printf("%scould not record the merge check for %s: %v", labelOf(t), rep.Label(), err)
		}
		return
	}
	log.Printf("%srecorded delivery.mergeVerified=%t for %s", labelOf(t), verified, rep.Label())
}

// quietBackoff is how long a target sits out once its fruitless spend crosses
// the quiet-token ladder: 15m, 30m, 1h, then 2h. It never reaches "never" — the
// blocker is usually an operator answering a question, and a target that can
// never come back would need a restart to notice.
//
// rung counts how many quietTokenThresholds the accumulator has crossed, offset
// by quietThreshold so the FIRST crossing lands on the same 15m the old
// session-count ladder started at.
func quietBackoff(rung int) time.Duration {
	const (
		base = 15 * time.Minute
		cap_ = 2 * time.Hour
	)
	d := base
	for i := quietThreshold; i < rung; i++ {
		if d *= 2; d >= cap_ {
			return cap_
		}
	}
	return d
}

// quietThreshold is how many quietTokenThresholds of fruitless spend it takes
// to back a target off — kept as the ladder's base rung so quietBackoff's shape
// is unchanged from the session-count version.
//
// The old "three, not one or two" rationale survives in token terms: a single
// fruitless drain is ordinary, and so is a second — a genuinely large task can
// span several sessions before anything reaches a reviewer, and backing off
// then would punish exactly the deep work this loop exists to do. ~80M of
// fruitless spend is a different claim, and against the run this was written
// for — which repeated the same no-op ten times at ~26M each — the threshold
// still saves most of them.
const quietThreshold = 3

// quietTokenThreshold is the token-denominated replacement for the old
// session-count trigger (CLA-343). Calibration: the old threshold of 3
// fruitless SESSIONS fired at ~78M tokens (~26M per measured iteration — the
// audit's own figure); 80M lands within one drain of that (the third drain at
// 26M each totals 78M, just under, and the fourth crosses). The drift is
// deliberate and lenient, and it buys the property the session count could
// never give: ONE huge fruitless drain trips the first 15-minute sit-out on its
// own — the 285.9M runaway would have earned it at under a third of its spend
// instead of running three sessions past it.
const quietTokenThreshold = 80_000_000

// judgeProgress reads this poll as the verdict on the target's last drain, and
// backs the target off once its fruitless drains have spent enough tokens
// achieving nothing.
//
// Progress is `Settled()` rising — work reaching a reviewer or finishing. A drain
// that only recorded why it could not proceed bumps `version` but settles
// nothing, which is exactly the run this exists to stop repeating. A fruitless
// drain charges its tokens (Driver.spent) to the accumulator; progress clears
// the accumulator.
//
// An ANSWERED QUESTION is the one kind of progress `Settled()` cannot see, and it
// is the very thing the back-off's own message asks the operator to go and do
// (CLA-248). Answering a BLOCKING question takes its task `blocked -> ready` on
// the plane, and a non-blocking one never moved its task at all; either way
// neither `InReview` nor `Done` shifts, so a falling `OpenQuestions` is the only
// trace of it a poll can read. Both signals are watched, and both baselines move
// on every poll: the verdict is always about the change since the last look.
func (d *Driver) judgeProgress(i int, sum backlog.Summary) {
	settled := sum.Settled()
	openQs, wasOpen := sum.OpenQuestions, d.openQs[i]

	// IDLE IS NOT FRUITLESS. `quiet` counts consecutive DRAINS that settled
	// nothing, and a poll with nothing claimable is not a drain — the gate below
	// will not spend a session, so this target has not failed at anything. Without
	// this the count is immortal: it survives the blocker being parked (`parked`
	// is not in Settled(), so nothing else clears it), an arbitrarily long idle
	// stretch, and a complete turnover of the queue. A task filed a week later
	// then inherits a back-off it did nothing to earn and serves the escalated
	// 2h wait on its FIRST fruitless drain — defeating quietThreshold, which
	// exists precisely so a large task spanning sessions is not punished.
	//
	// ONLY WITH NO VERDICT OUTSTANDING, and that condition is load-bearing rather
	// than defensive. `claimable == 0` does not only mean "the queue is empty" — it
	// also means "everything ready is claimed right now", and the poll immediately
	// after a drain is exactly when that is most likely, because a session that
	// ends still holding a task it pushed work on is deliberately NOT handed back
	// (see releaseHeldClaim). Forgetting the count there would not merely decline
	// to charge the drain, it would CANCEL the verdict on one that already ran — so
	// a target whose every session ends holding its task would alternate spawn /
	// forget forever and never back off at all. Requiring the verdict to be settled
	// first costs one extra poll of an idle stretch and nothing else: the drain is
	// judged, and the NEXT poll — still idle — does the forgetting.
	//
	// Clearing skipUntil too, and not only the count: an in-flight wait is moot
	// while there is nothing to spawn for, and leaving it set would make the next
	// task filed sit out the tail of a wait somebody else's blocker earned.
	//
	// RESIDUAL, stated rather than implied. A target whose only claimable task is
	// held ELSEWHERE for more than one poll — another driver on the same project,
	// an agent working it by hand — reads as idle for that stretch, so it forgets
	// and lifts. Such a target backs off at the base 15m every quietThreshold
	// fruitless drains instead of escalating toward 2h: the breaker still bounds
	// the spend, it just stops tightening. That is accepted rather than narrowed
	// away, because the two obvious narrowings are both worse. Requiring
	// `in_progress == 0` reinstates the immortal count for any project holding
	// abandoned WIP (CLA-274), which is precisely the population most likely to
	// have some. Requiring N consecutive idle polls does not discriminate at all:
	// a task held elsewhere is held for a lease, which is many poll intervals.
	// Nothing in a summary count separates "quiet because there is no work" from
	// "quiet because somebody else has it" — only whether WE are still spending
	// sessions does, and the guard above is the part of that signal worth having.
	//
	// This cannot spin. A drain needs a poll where the target IS spawnable, so the
	// reset never applies to the poll that authorises a session; the count climbs
	// from zero the ordinary way, and reaching the threshold again takes
	// quietThreshold fresh fruitless drains.
	if !sum.Spawnable() && !d.pending[i] {
		if d.quietTokens[i] > 0 {
			log.Printf("%snothing to spawn for — idle, not fruitless; forgetting %d tokens of drain(s) that settled nothing and clearing any no-progress back-off", d.prefix(i), d.quietTokens[i])
		}
		d.quietTokens[i] = 0
		// An idle stretch clears the no-progress (quiet) back-off - with no work
		// there is nothing to judge, and the blocker may have resolved itself.
		// A CLA-507 harness sit-out is different: the harness itself is broken,
		// and an empty queue does not fix it. Clearing skipUntil here would cut
		// the laddered sit-out short the moment the queue momentarily empties
		// (a sibling daemon consuming the work), so it is preserved until it
		// elapses or the harness succeeds again.
		if d.harnessFails[i] == 0 {
			d.skipUntil[i] = time.Time{}
		}
		d.baseline[i], d.openQs[i] = settled, openQs
		return
	}

	if !d.pending[i] {
		// No drain outstanding. Progress can still arrive from elsewhere — the
		// operator merging a PR, another machine's loop — and it means whatever was
		// stuck may now be unstuck, so let the target back in immediately.
		//
		// Note what the merge case actually rides on, because it is not obvious and
		// nothing here can pin it: the plane approves a task back to `ready` BEFORE
		// its PR is merged and it is marked `done`. The first write DROPS `Settled()`
		// by one and lowers the baseline with it here, so the later `done` reads as a
		// rise. It is that two-step, not the merge, that this branch sees, and only
		// because a poll lands between the two writes - which an idle poll of a minute
		// against a human-paced approve-then-merge ordinarily does. `in_review -> done`
		// with no poll in between is invisible, exactly like `blocked -> ready`.
		if d.quietTokens[i] > 0 {
			switch {
			case settled > d.baseline[i]:
				log.Printf("%sprogress from elsewhere — clearing the no-progress back-off", d.prefix(i))
				d.quietTokens[i], d.skipUntil[i] = 0, time.Time{}
			case openQs < wasOpen:
				log.Printf("%sopen questions fell %d -> %d, so something was resolved: clearing the no-progress back-off",
					d.prefix(i), wasOpen, openQs)
				d.quietTokens[i], d.skipUntil[i] = 0, time.Time{}
			}
		}
		d.baseline[i], d.openQs[i] = settled, openQs
		return
	}

	d.pending[i] = false
	spent := d.spent[i]
	d.spent[i] = 0
	if settled > d.baseline[i] {
		d.quietTokens[i] = 0
		d.baseline[i], d.openQs[i] = settled, openQs
		return
	}

	// The strike is earned whatever else moved: a session that answered its own
	// question has not thereby delivered anything, and the drain is being judged on
	// what it SETTLED. But an answer that landed while the drain was running is the
	// operator doing the very thing the back-off message asked for, and this poll is
	// the only one that can ever see it: the baseline advances just below, so a fall
	// consumed here is gone rather than deferred. Take the strike, skip the sit-out.
	// The target gets one immediate retry against the answer; if that settles
	// nothing too, the retry's spend is charged like any other, so the rung it
	// reaches depends on how big it was (a whole-threshold retry escalates at once,
	// a smaller one repeats the band) — the retry is never an exemption.
	answered := openQs < wasOpen
	d.quietTokens[i] += spent
	d.baseline[i], d.openQs[i] = settled, openQs
	// Bands, matching the old session-count ladder at the ~26M-per-drain
	// calibration: 3 fruitless drains (~78M) earned 15m, 4 (~104M) earned 30m,
	// 5 (~130M) earned 1h, 6+ (~156M) earned the 2h cap. In whole thresholds:
	// acc in [80M,160M) → rung 3 → 15m; [160M,240M) → 30m; [240M,320M) → 1h;
	// 320M+ → 2h. One huge fruitless drain (≥80M) trips the FIRST band on its
	// own — the property the session count could never give (CLA-343).
	if d.quietTokens[i] < quietTokenThreshold {
		return
	}
	if answered {
		log.Printf("%s%d tokens spent on sessions that settled nothing, but open questions fell %d -> %d while the last one ran - retrying once before backing off",
			d.prefix(i), d.quietTokens[i], wasOpen, openQs)
		return
	}
	wait := quietBackoff(2 + d.quietTokens[i]/quietTokenThreshold)
	d.skipUntil[i] = time.Now().Add(wait)
	// Name the RIGHT cause. The original sentence asserts there is claimable work
	// and lists the reasons a ready task resists being finished — all of which are
	// wrong, and misdirecting, when the only thing that opened the gate was an
	// abandoned branch (CLA-274). Sending an operator to look for an unanswered
	// question when the real subject is stranded WIP costs them the diagnosis.
	if sum.Claimable == 0 && sum.StaleClaimable > 0 {
		log.Printf("%s%d tokens spent on sessions that settled nothing — backing off for %s. Nothing is READY; the sessions were spawned to recover %d abandoned branch(es), and are not finishing them. Check the last iteration log, and the branch itself.",
			d.prefix(i), d.quietTokens[i], wait, sum.StaleClaimable)
		return
	}
	log.Printf("%s%d tokens spent on sessions that settled nothing — backing off for %s. The queue says there is claimable work, so something is stopping it being DONE: an unanswered question, a gate, a toolchain the session cannot run. Check the last iteration log.",
		d.prefix(i), d.quietTokens[i], wait)
}

// backedOff reports whether a target is currently sitting out, logging the moment
// it becomes eligible again so the silence has a visible end.
func (d *Driver) backedOff(i int) bool {
	if d.skipUntil[i].IsZero() {
		return false
	}
	if time.Now().Before(d.skipUntil[i]) {
		return true
	}
	d.skipUntil[i] = time.Time{}
	log.Printf("%sback-off elapsed — trying again", d.prefix(i))
	return false
}

// sidelineTarget marks target i failed after drainPhases returned an error: the
// failure happened while running THIS project's session, so this project sits out
// while the others drain on. The sit-out uses the quiet ladder's shape: 15m,
// 30m, 1h, then a 2h cap, keyed on consecutive failures rather than fruitless
// tokens, and it self-heals: the operator fixes the config (or the checkout), the
// next attempt after the sit-out succeeds, and Run resets the count.
//
// The log line is the operator's whole diagnosis for the exact incident that
// motivated this (2026-08-26): which project died, in which workdir, the
// harness's own error text - and the reassurance that the fleet carried on.
func (d *Driver) sidelineTarget(i int, drainNum int, err error) {
	d.harnessFails[i]++
	d.harnessErrs[i] = err
	wait := failureBackoff(d.harnessFails[i])
	d.skipUntil[i] = time.Now().Add(wait)
	log.Printf("%siteration %d: sidelining this project for %s (harness failure %d in a row; workdir %s) - the OTHER projects keep draining. Cause: %v",
		d.prefix(i), drainNum, wait, d.harnessFails[i], d.workdirOf(d.targets[i]), err)
}

// failureBackoff maps a run of consecutive harness failures onto the same ladder
// quietBackoff serves. judgeProgress's first trip lands at rung quietThreshold
// (15m); failure N starts one rung earlier than N+1 so each repeat escalates one
// band: 15m, 30m, 1h, then quietBackoff's own 2h cap.
func failureBackoff(fails int) time.Duration {
	return quietBackoff(quietThreshold - 1 + fails)
}

// allSidelined reports whether every target is CURRENTLY sitting out a harness
// failure: nothing left to drive, so ending the run beats idle-polling forever.
// A target counts only while its failure back-off is live - skipUntil zeroed by
// an elapsed wait means it is drivable again, whatever its history, and a target
// that has never failed since its last success never counts.
//
// A target paused for an operator answer (fleet or startup trip, CLA-396/562)
// never counts as sidelined: the pause is meant to wait for the answer, not to
// exit. Without this a fleet of one that trips its startup bound would exit on
// the same iteration it paused, losing the wait it just asked for.
func (d *Driver) allSidelined() bool {
	if len(d.targets) == 0 {
		return false
	}
	now := time.Now()
	for i := range d.targets {
		if d.fleetPaused[i] || d.startupPaused[i] {
			return false
		}
		if d.harnessFails[i] == 0 || d.skipUntil[i].IsZero() || !now.Before(d.skipUntil[i]) {
			return false
		}
	}
	return true
}

// sidelinedReport renders one line per target with the error that benched it, for
// the run-wide exit when every target is sidelined.
func (d *Driver) sidelinedReport() string {
	var b strings.Builder
	for i := range d.targets {
		fmt.Fprintf(&b, "\n  [%s] (workdir %s): %v", d.targets[i].Name, d.workdirOf(d.targets[i]), d.harnessErrs[i])
	}
	return b.String()
}

// workdirOf resolves where a target's sessions run, for log lines: the target's
// own workdir when it declares one, the config's otherwise - the same resolution
// the invocation gets, so the log names the directory the session actually died in.
func (d *Driver) workdirOf(t Target) string {
	if t.WorkDir != "" {
		return t.WorkDir
	}
	return d.cfg.WorkDir
}

// sleepStall reports how much real time a wait lost to system suspension, and
// whether that is worth saying out loud.
//
// The threshold is absolute rather than proportional on purpose: five minutes of
// unexplained wall clock is worth one log line whether it interrupted a one-minute
// idle poll or a thirty-minute limit wait, and a proportional rule would either
// spam the short waits or stay silent through the long ones. Small overshoots are
// ordinary scheduling and say nothing.
func sleepStall(intended, wall time.Duration) (lost time.Duration, stalled bool) {
	const floor = 5 * time.Minute
	if lost = wall - intended; lost < floor {
		return 0, false
	}
	return lost, true
}

// waitPastBudget reports whether waiting for resetAt would cost more time than
// the run has left. An unknown reset (zero) is waited out as before, because the
// supervised wait polls for an EARLY lift and the limit may clear long before any
// stated time; with no ceiling set there is nothing to be past.
//
// remaining comes from the breaker's own clock — MONOTONIC elapsed, which does
// not advance while the machine is suspended. The wait is measured on the wall
// clock, because a quota reset is a wall-clock event. Those are the right two
// clocks for this question and the mixture is deliberate.
//
// The earlier form compared the reset against a wall-clock DEADLINE while the
// breaker counted monotonic, so the two diverged by exactly the time the machine
// spent asleep — the very thing this file's sleep detection exists to report. An
// eight-hour run that lost five hours to a closed lid would stop with seven hours
// of budget unspent, and tell the operator it had reached a ceiling it had not.
func waitPastBudget(resetAt time.Time, remaining time.Duration, bounded bool) (time.Time, bool) {
	if resetAt.IsZero() || !bounded {
		return time.Time{}, false
	}
	return resetAt, time.Until(resetAt) > remaining
}

// supervisedWait pauses on a usage limit, then polls for an early reset rather
// than sleeping blindly to the stated reset. Returns the probe spend it incurred,
// and stop=true if the loop should stop (STOP marker, context cancel, or a budget
// ceiling reached during the wait).
//
// sofar is everything the run has spent, including this drain's attempts. The
// breaker is consulted HERE and not only by the caller, because this loop does not
// return to the caller while the limit persists: it waits, probes, and waits again
// for as long as it takes. Without this, the one shape the task names — a drain
// stuck in a supervised wait — was the one shape no ceiling could end, and it is
// the shape codex always produces, since it states no reset for waitPastBudget to
// measure (CLA-258).
//
// The probes are themselves paid sessions, so their spend is added to `sofar` as it
// accrues and returned to the caller for the run's accumulator. A breaker that can
// only be tripped by spend it can SEE was blind to exactly the spend this loop
// generates: a week-long cap polled every 30 minutes is ~336 unaccounted sessions,
// and this is the one loop with no other way out (CLA-287).
// probeWait returns how long the paused loop sleeps before its next probe.
//
// A known reset still ahead of us is worth waiting for precisely: sleep until
// just past it (reset + grace) instead of to the next tick of the poll grid,
// so a cap that lifts on time costs one grace period of idleness rather than
// up to a whole interval (CLA-305: 22 minutes dead on 2026-08-10, work queued,
// nothing wrong). The cap at the interval stays: waitOrStop is what keeps a
// paused loop responsive to a STOP marker, and an aligned multi-hour sleep
// would make the stop switch feel dead - so the loop wakes on min(reset+grace,
// the grid) and simply laps again when it wakes short of the reset. An unknown
// or already-past reset falls back to the blind grid: codex states no reset at
// all, and after CLA-258 claude parses one only from typed sources, so an
// unknown reset is normal rather than exceptional.
func probeWait(now, resetAt time.Time, interval, grace time.Duration) time.Duration {
	if resetAt.IsZero() || !now.Before(resetAt) {
		return interval
	}
	untilReset := resetAt.Add(grace).Sub(now)
	if untilReset < interval {
		return untilReset
	}
	return interval
}

func (d *Driver) supervisedWait(ctx context.Context, lim harness.Limit, a harness.Adapter, ph config.Phase, t Target, sofar spend) (tokens int, cost float64, stop bool) {
	tc := d.targetCfg(t)
	interval := d.cfg.PollInterval.OrDefault(30 * time.Minute)

	// waitingForReset says the stated reset was still ahead of us as this wait
	// began; it flips once the loop has slept across it. waitedOnReset snapshots
	// that entry state, because a reset we waited on and a reset we arrived
	// after are owed different endings (see the branch below).
	waitingForReset := !lim.ResetAt.IsZero() && time.Now().Before(lim.ResetAt)
	waitedOnReset := waitingForReset
	if waitingForReset {
		firstWake := probeWait(time.Now(), lim.ResetAt, interval, d.waitGrace)
		log.Printf("paused%s - waiting out the stated reset (first probe in ~%s, capped at the poll interval)", resetSuffix(lim.ResetAt), firstWake.Round(time.Second))
	} else {
		log.Printf("paused%s — probing every %s for an early reset", resetSuffix(lim.ResetAt), interval)
	}

	for {
		if dim := d.budgetTrip(sofar.tokens+tokens, sofar.cost+cost, time.Since(sofar.start)); dim != "" {
			log.Printf("budget reached while paused: %s — stopping rather than waiting out the limit (tokens=%d cost=$%.2f elapsed=%s)",
				dim, sofar.tokens+tokens, sofar.cost+cost, time.Since(sofar.start).Round(time.Second))
			return tokens, cost, true
		}
		waitDur := probeWait(time.Now(), lim.ResetAt, interval, d.waitGrace)
		if d.waitOrStop(ctx, waitDur) {
			return tokens, cost, true
		}
		// A stated reset is the provider's claim, not a guarantee, so crossing it
		// mid-wait does NOT resume the run the way a blind-grid wake used to: it
		// falls back to interval polling, and the very next probe is what confirms
		// or refutes the claim. Only a reset that was ALREADY behind us at entry
		// resumes on the stated time alone - the legacy contract pinned by
		// TestDrainWithRetries_StatedResetPassed, where the caller sent us here
		// knowing the reset had passed and one more interval of idleness is the
		// price of not re-probing a phase that just told us it was capped.
		if !lim.ResetAt.IsZero() && !time.Now().Before(lim.ResetAt) && waitingForReset {
			log.Print("stated reset passed - falling back to interval polling (a stated reset is a claim, not a guarantee)")
			waitingForReset = false
		} else if !waitedOnReset && !lim.ResetAt.IsZero() && !time.Now().Before(lim.ResetAt) {
			log.Print("stated reset already past — resuming")
			return tokens, cost, false
		}
		inv, ierr := d.invocationOn(t, tc.HarnessFor(ph), true)
		if ierr != nil {
			// A refused probe spawn spends nothing and learns nothing about the
			// limit, so it reads exactly like any other probe error: log the
			// named variable (the error carries it) and poll again next
			// interval. The wait stays bounded by its own dials.
			log.Printf("probe error: %v — will retry next interval", ierr)
			continue
		}
		got, err := a.Probe(ctx, inv)
		// Count the probe BEFORE reading its verdict, and before the error branch: a
		// probe that failed still spawned the harness and still spent. Every path out
		// of here — resume, stop, or another lap — carries it.
		tokens += got.Tokens
		cost += got.CostUSD
		// The probe spawned on the TARGET's effective harness above (invocationOn
		// with tc.HarnessFor), so its spend is booked under that same name —
		// keying on the base harness would bill an overlay-redirected phase to
		// the wrong per-harness account (CLA-410).
		d.charge(tc.HarnessFor(ph), got.Tokens, got.CostUSD)
		if err != nil {
			if ctx.Err() != nil {
				return tokens, cost, true
			}
			// A probe whose own output could not be read is not a blip that clears
			// itself: it reports no verdict AND no spend, so a wait that keeps polling
			// on it re-spawns a paid session every interval against a ceiling that can
			// never see the cost — the one shape this loop's breaker exists to end
			// (CLA-262, and the same reasoning as endUntrustedDrain). With no spend
			// ceiling there is nothing to protect, so it waits on as before.
			if errors.Is(err, harness.ErrUntrusted) && tc.Budget.CountsSpendFor(tc.HarnessFor(ph)) {
				log.Printf("paused, and the probe's own output cannot be read (%v) — stopping rather than polling on: its spend cannot be counted, so a token/cost ceiling can no longer be honoured. Rerun after the reset.", err)
				return tokens, cost, true
			}
			log.Printf("probe error: %v — will retry next interval", err)
			continue
		}
		if !got.Limit.Limited {
			log.Print("limit lifted — resuming")
			return tokens, cost, false
		}
		log.Print("still limited — continuing to wait")
	}
}

// backoff is the exponential delay before transient retry n (1-based): 30s, 60s,
// 120s, ..., capped at RetryCap. Computed by doubling to avoid shift overflow.
func (d *Driver) backoff(n int) time.Duration {
	ceil := d.cfg.RetryCap.OrDefault(300 * time.Second)
	b := 30 * time.Second
	for i := 1; i < n; i++ {
		b *= 2
		if b >= ceil {
			return ceil
		}
	}
	if b > ceil {
		return ceil
	}
	return b
}

// noteResumedTransient records one more consecutive transient sequence-end on
// key and returns the new streak — the backoff rung the run owes after the
// sequence's handback (CLA-662). The map is lazily built: a run with no
// resumed-phase blip never allocates it.
func (d *Driver) noteResumedTransient(key resumedTransientKey) int {
	if d.resumedTransients == nil {
		d.resumedTransients = map[resumedTransientKey]int{}
	}
	d.resumedTransients[key]++
	return d.resumedTransients[key]
}

// clearResumedTransient resets key's streak: a resumed phase on that
// target+harness ended some other way, so the next blip starts the ladder
// over (CLA-662). Deleting from a nil map is a no-op — a run that never had a
// streak has nothing to break.
func (d *Driver) clearResumedTransient(key resumedTransientKey) {
	delete(d.resumedTransients, key)
}

// resumedTransientDuration is the wait a resumed phase's transient
// sequence-end owes before the next drain: backoff(n) in production, where n
// is noteResumedTransient's streak. The field is a test seam so the ladder can
// be observed and driven without sleeping the real 30s/60s/... in the suite —
// the same reason waitGrace and restartPoll are fields.
func (d *Driver) resumedTransientDuration(n int) time.Duration {
	if d.resumedTransientWait != nil {
		return d.resumedTransientWait(n)
	}
	return d.backoff(n)
}

func retryLabel(n, max int) string {
	if max > 0 {
		return fmt.Sprintf("retry %d/%d", n, max)
	}
	return fmt.Sprintf("retry %d", n)
}

// failureDetailMax bounds how much of the harness's diagnostic text reaches the
// stop message. A session's stderr can run to megabytes; the operator needs
// enough to recognise the failure and quote it in a bug report, not the log.
const failureDetailMax = 400

// failureDetail renders a harness diagnostic for a one-line stop message: the
// TAIL of the text, non-printables stripped, whitespace collapsed, bounded.
//
// The tail rather than the head, deliberately. The scope starts with stderr,
// which on a real run leads with startup noise that is identical on every
// session; the thing that killed this one is the last thing said. Truncating
// from the front would reliably show the operator the one part that is never
// the answer.
//
// Non-printables are stripped because this changes what the text is FOR. Up to
// now a diagnostic was only ever fed to regexp.MatchString, where a control byte
// is inert; here it is rendered to a terminal, where ESC is executed. The
// scoping rules inherited from CLA-258 answer "what may a classifier READ", which
// is a different question from "what may be PRINTED", and stderr reaches this
// verbatim from the harness. Stripping is cheap and the answer does not depend
// on trusting the upstream CLI's output.
//
// Empty in, empty out — and the caller appends it, so a harness with nothing to
// say leaves the existing message exactly as it was rather than trailing a
// colon and a blank.
func failureDetail(diag string) string {
	printable := strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) || unicode.IsSpace(r) {
			return r
		}
		return -1
	}, diag)

	s := strings.Join(strings.Fields(printable), " ")
	if s == "" {
		return ""
	}
	// Counted and sliced in RUNES, not bytes: the harness's own text carries "·"
	// in every usage-limit notice and "→" in rendered tool markers, so a byte
	// slice would routinely cut a rune in half and emit U+FFFD at the seam.
	//
	// "..." rather than U+2026: this string exists to be pasted into a bug
	// report, and a single-character ellipsis is exactly what arrives as
	// mojibake when terminal output is copied out.
	if r := []rune(s); len(r) > failureDetailMax {
		s = "..." + string(r[len(r)-failureDetailMax:])
	}
	return ": " + s
}

// droppedNote says how much of a session's output the retained window did not
// keep, when it kept less than all of it.
//
// It rides on the message that ENDS a run, because that is the message whose
// weight depends on it: "exited 1 (non-retryable): <text>" reads as "this was the
// failure", when what the classifier actually saw was the last couple of MiB. An
// operator deciding whether the classifier merely does not know this failure yet
// needs to know which of those they are looking at. Empty when nothing was
// dropped, which is the ordinary case.
func droppedNote(dropped int64) string {
	if dropped <= 0 {
		return ""
	}
	return fmt.Sprintf(" (note: %.1f MiB of this session's output was past the retained window, so the classification read only the tail)",
		float64(dropped)/(1<<20))
}

// invocationFor builds the invocation for one PHASE: the target's invocation,
// carrying this phase's prompt and turn cap, with the previous phase's claim
// substituted into the prompt's placeholders.
//
// spawnDir is where this phase's sessions run, resolved once per phase by
// sessionDir — the task repo's checkout when one is known, else the target's
// workdir exactly as before. It is resolved by the CALLER rather than here so a
// retry ladder re-spawns into the SAME directory it started in (the task has not
// changed between attempts), and so a resolution failure fails the phase before
// anything is spawned.
//
// Unlike the directory, the session env is resolved per CALL, not once per
// phase: every attempt re-runs its fromCommand declarations fresh, so a token
// rotated mid-ladder still reaches the next attempt (CLA-462).
//
// The substitution is what lets a later phase RESUME a run rather than claim a
// task. A session cannot know its own run id — it never called claim_task — so
// the driver, which watched the earlier phase's stream go by, is the only thing
// that can tell it. That is also why the ids come from the observed claim and
// not from config: they are per-task facts, discovered at runtime.
//
// A placeholder with no claim to fill it is LEFT STANDING rather than blanked.
// A literal {{runId}} in the log is a misconfigured sequence announcing itself;
// an empty string is a session quietly deciding to claim fresh work instead.
//
// prBase is the branch this task's repo PRs target, resolved per phase by
// drainPhase (CLA-608), and fills {{prBase}} in EVERY phase: the other three
// placeholders are per-run facts only a claim can carry, while the implement
// brief — phase 1, claim-free at spawn time — is itself one of the carriers, in
// its resumed-work rule. Empty leaves the placeholder standing, like the rest.
//
// phases is the sequence as CAPTURED when the drain started, and supplies every
// positional/attribution fact here — the "PHASE i of N" total and the previous
// phase's harness/model. A RELOAD applied inside a wait swaps the live config
// under an in-flight sequence (CLA-664), and the sequence must keep its own
// shape and attribution until the next drain.
func (d *Driver) invocationFor(t Target, phaseIdx int, ph config.Phase, prev *harness.Result, spawnDir, prBase string, phases []config.Phase) (harness.Invocation, error) {
	tc := d.targetCfg(t)
	inv, err := d.invocationOn(t, tc.HarnessFor(ph), false)
	if err != nil {
		return inv, err
	}
	if spawnDir != "" {
		inv.WorkDir = spawnDir
	}
	inv.ExtraDirs = d.extraDirsFor(t, spawnDir)
	inv.MaxTurns = ph.MaxTurns
	// The per-session runaway ceiling, resolved once per drain from the budget:
	// the operator's own dial, else 2x max_tokens, else the documented floor.
	// 0 never reaches here — SessionTokenCeiling has no disabled state, because
	// the whole point of CLA-343 is that nothing was able to stop the 285.9M
	// session.
	// Asked PER HARNESS since CLA-367: a token ceiling that lives in this
	// harness's own per_harness block derives the detector exactly as the global
	// dial does, so moving a ceiling into a block does not loosen it. And the
	// harness asked about is THIS PHASE's (CLA-366), not the run's — the ceiling
	// is compiled into the invocation that is about to spawn, so keying it on d.h
	// would hand an opencode phase the ceiling derived from claude's block.
	inv.MaxSessionTokens = tc.Budget.SessionTokenCeilingFor(tc.HarnessFor(ph))
	// The per-session wall-clock cap, resolved by EffectivePhases: this phase's
	// own, else the run-wide one, else zero — and zero is OFF, which is the
	// shipped default (CLA-368). Unlike the token ceiling this one HAS a disabled
	// state, because no default number is honest across models and providers.
	inv.MaxSessionWallClock = ph.MaxWallClock.Duration()
	inv.Prompt = ph.Prompt
	// The phase's tier, or the run-wide model when it names none. Reported when a
	// tier was named and resolved to nothing, because that is a typo in the
	// operator's map and the session otherwise runs on the default with nothing
	// said — the quiet failure, since a tier is set precisely when the default is
	// not what was wanted.
	//
	// "resolves to no model" rather than "the map does not define it": ModelForTier
	// also says not-ok for a bucket that IS in the map and is mapped to blank or
	// whitespace, and telling that operator their map lacks a bucket they can see
	// sitting in it would send them looking in the wrong place — the more so
	// because the whitespace is the thing they cannot see. Label(), not Name, so a
	// phase carrying a custom prompt and no name is still identified.
	//
	// Resolved against the tier map of the harness THIS PHASE runs on, not the
	// run's: a bucket name is policy and travels, the alias inside it is a
	// provider's and does not. An opencode phase whose harness block names no
	// models therefore runs on opencode's own configured model — which is where
	// opencode's model lives — rather than on a claude alias its CLI would die on.
	model, ok := tc.ModelForPhase(ph)
	if !ok {
		log.Printf("%sphase %q names tier %q, which resolves to no model on harness %q — running on that harness's default model instead",
			labelOf(t), ph.Label(phaseIdx), ph.Tier, tc.HarnessFor(ph))
	}

	// CLA-379: review-tier escalation evaluated at review-spawn time.
	// The CLI reads the operator's escalation rules from config and applies
	// them mechanically. Only raises; never lowers. The matching rule
	// is logged so the spend is attributable, per the 2026-08-23 decision.
	if tc.Escalation.PathRules != nil || tc.Escalation.CategoryRules != nil {
		if isReviewPhase(ph) {
			// Resolve the branch name for the diff: from the previous claim's
			// branch record (CLA-379 plumbing), falling back to reading the
			// workdir's git refs if the claim carries none.
			branchName := ""
			if prev != nil && prev.Claim.Held() && prev.Claim.Branch != "" {
				branchName = prev.Claim.Branch
			}
			changedPaths, _ := d.diffAgainstIntegration(t, branchName, spawnDir)
			category := ""
			if prev != nil && prev.Claim.Held() && prev.Claim.Category != "" {
				category = prev.Claim.Category
			}
			tier, rule := tc.Escalation.Evaluate(changedPaths, category)
			if tier != "" {
				log.Printf("%sreview escalated to tier %q: %s (changed paths: %v, category=%q, branch=%q)", labelOf(t), tier, rule, changedPaths, category, branchName)
				ph.Tier = tier
				model, ok = tc.ModelForPhase(ph)
				if !ok {
					log.Printf("%sescalated tier %q resolves to no model — running on harness default", labelOf(t), tier)
				}
			}
		}
	}

	inv.Model = model
	if prev != nil && prev.Claim.Held() {
		// The {{branch}} placeholder is filled from the recorded, VERIFIED hand-off
		// (CLA-457): the checkpoint the driver ran against this claim already
		// confirmed the branch is on the origin remote, so the review brief can
		// name it instead of asserting an unverified phase-1 success. verifiedBranch
		// reads the accepted reports first (the plane record of what THIS run
		// recorded), falling back to the claim's own branch for a resumed-with-WIP
		// predecessor.
		//
		// CLA-497: a checkpoint evidenced by the PLANE'S RECORD - the task left
		// `ready` or declared a no-code delivery - names no branch, and the
		// branch-shaped builtin brief would tell its successor the hand-off
		// FAILED ("an empty branch field ... is a FAILED hand-off to report"),
		// the exact opposite of the checkpoint the driver just recorded. Swap in
		// the no-code brief for the BUILT-IN prompt only; an operator's custom
		// prompt owns its own wording. The empty claim branch is the
		// discriminator because drainPhase holds "claim.Branch is the verified
		// branch, or empty when the evidence was branch-less" by construction.
		//
		// CLA-576: an UNVERIFIED checkpoint carries its recorded branch but no
		// verification - the origin could not be read - so the branch-shaped
		// builtin brief's "a branch the driver verified to exist on the origin
		// remote" is the one claim that must not reach the successor. It gets
		// the unverified variant instead, again for the built-in prompt only.
		//
		// The split adds a THIRD mode: a non-final review runs the review-only
		// brief, whose variants must stay review-only (record, never fix or
		// hand over). The mode is selected by the resolved prompt text, not by a
		// flag, so a custom prompt is never rewritten into a shipped one.
		prompt := inv.Prompt
		// CLA-639 M5: a panel MEMBER writes the durable record and nothing else
		// on the task, so its brief is the panel variant — same core, no outcome
		// write. A branchless checkpoint has no panel variant and must not be
		// given one: its record IS the legacy block (no live holder accepts a
		// durable one), so the first member keeps the shipped no-code brief and
		// the driver skips the members after it (see drainPhases).
		panelMember := phaseIdx < len(phases) && phases[phaseIdx].IsPanelMember()
		switch {
		case prompt == config.ReviewOnlyBrief():
			switch {
			case prev.Claim.Branch == "":
				prompt = config.NoCodeReviewOnlyBrief()
			case prev.CheckpointUnverified:
				if panelMember {
					prompt = config.PanelMemberUnverifiedReviewOnlyBrief()
				} else {
					prompt = config.UnverifiedReviewOnlyBrief()
				}
			case panelMember:
				prompt = config.PanelMemberReviewOnlyBrief()
			}
		case prompt == config.BuiltinReviewBrief():
			switch {
			case prev.Claim.Branch == "":
				prompt = config.NoCodeReviewBrief()
			case prev.CheckpointUnverified:
				prompt = config.UnverifiedReviewBrief()
			}
		case prompt == config.FixBrief() && prev.Claim.Branch == "":
			// The FIX phase has no unverified variant: its brief asserts no
			// verification at all. It reads the review's record (which names the
			// hand-off's real state) and its reviewed-tip comparison fails closed
			// when the branch is missing, so a variant would only restate what the
			// record already says.
			prompt = config.NoCodeFixBrief()
		case prompt == config.FixVerifyBrief() && prev.Claim.Branch == "":
			// Same for the fix phase that a verification round follows, in the
			// shape whose fix is a record amendment.
			prompt = config.NoCodeFixVerifyBrief()
		case prompt == config.VerifyBrief() && prev.Claim.Branch == "":
			// A verification round on a branchless checkpoint has no code delta
			// to re-review; its brief adjudicates the record's findings.
			prompt = config.NoCodeVerifyBrief()
		}
		inv.Prompt = prompt
		// Seed the claim as well as the prompt. The session is told not to claim,
		// so the adapter would observe none — and Result.Claim.Held() is what gates
		// the handback, the salvage and the delivery check. Without this, the phase
		// that pushes the branch and opens the PR is the one running with all three
		// switched off.
		inv.ResumeClaim = prev.Claim
	}
	// Every placeholder is filled in ONE pass (CLA-split review finding). Three
	// sequential replacers ran over each other's OUTPUT, so a claim-derived value
	// that happened to contain another placeholder — a branch named
	// `clanker/{{model}}`, which is a legal git ref — would be corrupted by the
	// later pass. One replacer cannot rescan its own output, so a substituted
	// value is final whatever it contains.
	//
	// The attribution/position values are config- and spawn-derived facts only the
	// driver has: a session cannot reliably report its own model, and it cannot
	// see the previous phase's at all. prevModel is the PREVIOUS phase's resolved
	// model from the static config, which is exact for the shipped shapes: tier
	// escalation applies only to a review phase, and the shipped consumer of
	// {{prevModel}} is a review brief naming the IMPLEMENT phase.
	// The positional/attribution values are facts of the sequence that was
	// captured at drain start (CLA-664), not of the live config: a RELOAD landing
	// mid-drain must not rewrite the "PHASE i of N" text of the sequence already
	// in flight, nor attribute the previous phase to a shape it never ran.
	phaseTotal := len(phases)
	prevHarness, prevModel := "", ""
	prevIdx := phaseIdx - 1
	// CLA-639: inside a review panel every member's predecessor is the member
	// before it, but the review briefs name {{prevHarness}}/{{prevModel}} as the
	// IMPLEMENTER whose work is being judged. Walk back past the panel to the
	// last non-review phase, so member 2 does not accuse member 1 of having
	// implemented the branch. For the shipped single review this changes
	// nothing: the phase before it is the implement phase.
	for prevIdx >= 0 && config.ReviewShaped(phases[prevIdx].Prompt) && phases[prevIdx].IsPanelMember() {
		prevIdx--
	}
	if prevIdx >= 0 && prevIdx < len(phases) {
		prevPh := phases[prevIdx]
		prevHarness = tc.HarnessFor(prevPh)
		prevModel, _ = tc.ModelForPhase(prevPh)
	}
	repl := []string{
		config.PhaseHarnessPlaceholder, attributionOr(tc.HarnessFor(ph), "unknown harness"),
		config.PhaseModelPlaceholder, attributionOr(model, "harness default"),
		config.PhasePrevHarnessPlaceholder, attributionOr(prevHarness, "unknown harness"),
		config.PhasePrevModelPlaceholder, attributionOr(prevModel, "harness default"),
		config.PhaseNumberPlaceholder, strconv.Itoa(phaseIdx + 1),
		config.PhaseTotalPlaceholder, strconv.Itoa(phaseTotal),
	}
	if prBase != "" {
		repl = append(repl, config.PhasePRBasePlaceholder, prBase)
	}
	if prev != nil && prev.Claim.Held() {
		repl = append(repl,
			config.PhaseTaskPlaceholder, prev.Claim.TaskID,
			config.PhaseRunPlaceholder, prev.Claim.RunID,
			config.PhaseBranchPlaceholder, verifiedBranch(prev),
		)
		// CLA-637: the durable review record's identity. The role is the
		// driver's assignment for THIS phase, and the tip is the origin commit
		// the driver verified at the checkpoint that opened the phase — the two
		// facts a session cannot see for itself. Filled for every phase rather
		// than keyed on the prompt text: a handoff respawn's prompt is the
		// continuation, not one of the briefs, and replacing an absent
		// placeholder is a no-op either way. The tip is only filled when the
		// checkpoint has one: the unverified and branchless variants carry no
		// {{reviewedTip}} at all, and the verified variant is only ever chosen
		// for a checkpoint whose verified branch check read a tip.
		if role := reviewerRoleFor(phases, phaseIdx); role != "" {
			repl = append(repl, config.ReviewerRolePlaceholder, role)
		}
		if prev.CheckpointTip != "" {
			repl = append(repl, config.ReviewedTipPlaceholder, prev.CheckpointTip)
		}
	}
	inv.Prompt = strings.NewReplacer(repl...).Replace(inv.Prompt)
	return inv, nil
}

// invocationOn builds the invocation for one target ON A NAMED HARNESS: the
// project's workdir and MCP config over that harness's own fields.
//
// The harness-shaped fields (config dir, MCP config, settings, model) come from
// config.SessionFor, which fills the run-wide values in for the top-level
// harness only — so a single-harness run resolves to exactly what it always did,
// and a phase on another harness gets that harness's block or nothing, never the
// top level's claude-shaped paths.
//
// The MCP config is the one field with two claimants, because it carries two
// facts at once: which PROJECT (its /mcp/<slug>) and which SCHEMA (the
// harness's). The project's per-harness file answers both and wins; the
// project's own file is the top-level harness's, so it only applies there; and
// the harness block's path is the single-project fallback. config.Validate
// refuses the combination that would silently resolve to the wrong project.
//
// The session env is composed HERE, at spawn time, fresh every call (CLA-462):
// the four-level env overlay with every fromCommand executed now, so a rotated
// token reaches this session without a daemon restart. An error — a failing,
// timed-out or empty-output command, an unreadable @path file — REFUSES the
// spawn: the caller reports it and no session starts, because a session without
// its declared env is the incident, not a degraded mode.
func (d *Driver) invocationOn(t Target, harnessName string, probe bool) (harness.Invocation, error) {
	// The target's effective config supplies the model/tier half of SessionFor
	// and decides which harness the project's own MCP file belongs to (CLA-410);
	// the env, prompt and workdir fallbacks stay on the base config — they are
	// machine-local core the plane never carries.
	tc := d.targetCfg(t)
	workdir := t.WorkDir
	if workdir == "" {
		workdir = d.cfg.WorkDir
	}
	hc := tc.SessionFor(harnessName)
	env, err := d.cfg.SessionEnv(harnessName, t.Name)
	if err != nil {
		return harness.Invocation{}, err
	}
	return harness.Invocation{
		Prompt:        d.cfg.Prompt,
		Model:         hc.Model,
		WorkDir:       workdir,
		MCPConfigPath: tc.ResolveMCPConfig(harnessName, t.MCPConfigPath, t.MCPConfigPaths),
		ConfigDir:     hc.ConfigDir,
		SettingsPath:  hc.SettingsPath,
		Env:           env,
		Probe:         probe,
	}, nil
}

// sessionDir resolves where a PHASE's sessions run: the checkout of the task
// they will work, not the multi-repo parent (CLA-437, agent-rule-scoping pieces
// 2 and 3). It also returns the id of the task the phase is BRIEFED on — exact
// for a resumed phase (the predecessor's claim), best-effort for a fresh one
// (the queue-head peek, the same read the session is about to make). The seam's
// checkpoint peek reads it after the phase ends (CLA-451); empty means the
// driver never knew which task to ask about, and the peek is skipped.
//
// Which task that is depends on the phase. A RESUMED phase (prev carries a held
// claim) knows its exact task, so its repo is read straight off the plane with
// get_task. A FRESH phase has claimed nothing yet — the claim happens inside the
// session — so the driver peeks at next_task, the same queue-head read the
// session is about to make, and starts in that task's repo. The peek cannot race
// a session out of work (it holds nothing), and if another clanker does claim the
// peeked task first, the spawned session claims elsewhere and still works: the
// permission policy covers every declared repo regardless of cwd, which is what
// makes a best-effort cwd safe rather than load-bearing.
//
// Resolution then runs config.ResolveCheckout over what the target declared:
// explicit repos entries, then the workdir-is-the-checkout convention, then
// <workdir>/<name>. An identity that matches nothing fails the iteration with
// repo_not_found — starting in the parent instead is exactly the wrong-repo-
// reads and cwd-scoped-grant shape this exists to kill — while an empty result
// (nothing configured anywhere) keeps the legacy workdir behaviour byte for byte.
//
// Lookup failures are degradation, not failure: a plane blip costs the spawn its
// per-task directory (it falls through to the primary-repo chain), never the
// iteration — the session about to spawn needs the plane too, and it can resolve
// nothing either way.
//
// The same read yields the task's repo IDENTITY, returned alongside so the
// caller can key the pr_base override by it (CLA-608) without a second plane
// read; "" is the same best-effort empty the checkout resolution already
// tolerates, and means only "no override can match".
func (d *Driver) sessionDir(ctx context.Context, t Target, prev *harness.Result) (string, string, string, error) {
	repo := ""
	briefed := ""
	if prev != nil && prev.Claim.Held() {
		briefed = prev.Claim.TaskID
		if src, ok := t.Releaser.(plane.TaskRepoSource); ok {
			r, err := src.TaskRepo(ctx, prev.Claim.TaskID)
			switch {
			case err == nil:
				repo = r
			case errors.Is(err, plane.ErrNotWired):
				// No plane endpoint/key: nothing to read, the fallback chain runs.
			default:
				log.Printf("%scould not read the repo of resumed task %s (%v) — falling back to the primary repo", labelOf(t), claimLabel(prev.Claim), err)
			}
		}
	} else if peeker, ok := t.Releaser.(plane.TaskPeeker); ok {
		next, err := peeker.PeekNextTask(ctx)
		switch {
		case err == nil:
			briefed = next.TaskID
			repo = next.Repo
			if next.Repo != "" {
				log.Printf("%squeue head %s names repo %q — resolving it to this phase's start directory", labelOf(t), next.TaskID, next.Repo)
			}
		case errors.Is(err, plane.ErrNotWired):
		default:
			log.Printf("%scould not peek the next task's repo (%v) — falling back to the primary repo", labelOf(t), err)
		}
	}
	workdir := t.WorkDir
	if workdir == "" {
		workdir = d.cfg.WorkDir
	}
	dir, err := config.ResolveCheckout(t.Repos, t.PrimaryRepo, workdir, repo)
	if err != nil {
		return dir, briefed, repo, err
	}
	// With the per-project layout declared, the session must START inside the
	// project root its permission scope is bounded to. ResolveCheckout prefers
	// an explicit `repos` path, so a stale override pointing at a developer
	// checkout would otherwise run the session outside the project and hand
	// that path to the policy (CLA-678 review round 2, finding 3). The
	// materialized config already rewrites the overrides; this is the
	// driver-side refusal that holds even for a config that adopted the layout
	// by hand.
	if t.ProjectRoot && dir != "" && !insideProjectRoot(workdir, dir) {
		return dir, briefed, repo, fmt.Errorf("repo checkout %s resolves outside the project root %s - refusing to start the session outside its declared scope", dir, workdir)
	}
	return dir, briefed, repo, nil
}

// prBaseProbeTimeout bounds the one network round-trip prBaseFor runs. It is a
// var so tests can shrink it rather than waiting out the real bound, the same
// discipline deployGitTimeout sets in internal/cli.
var prBaseProbeTimeout = 30 * time.Second

// prBaseFor resolves the branch this task's repo PRs target, substituted into
// the phase briefs as {{prBase}} (CLA-608). Resolution order, first match wins:
//
//  1. the operator's pr_base override for the repo, which needs no probe — the
//     one way to name a branch that is not derivable (lecstor/makespdf PRs
//     merge into `integration` while it deploys `staging`);
//  2. the project's integration_branch, when that branch EXISTS on origin
//     (repos that integrate through staging: clankerbar-cli, ezyapp);
//  3. origin's default branch (the makesPDF plugins carry only main);
//  4. the project's integration_branch anyway — the pre-CLA-608 text — when the
//     probe could not run at all (no dir, no origin, offline, no git). The
//     degradation is deliberate: failing the spawn would be worse than a brief
//     naming today's default, and a probe that cannot read origin has no
//     better answer to offer.
//
// It never fails, so a spawn is never blocked on it. The probe runs afresh per
// phase rather than caching: a phase is spawned at most a few times per task,
// and a cache keyed on remote state would outlive the branch creations it is
// supposed to observe.
func (d *Driver) prBaseFor(ctx context.Context, t Target, dir, repo string) string {
	tc := d.targetCfg(t)
	if b := tc.PRBaseFor(t.Name, repo); b != "" {
		log.Printf("%spr_base override: %s PRs target %q", labelOf(t), displayRepo(dir, repo), b)
		return b
	}
	integration := tc.IntegrationBranchFor(t.Name)
	if d.probePRBase == nil || dir == "" {
		return integration
	}
	onOrigin, def, err := d.probePRBase(ctx, dir, integration)
	switch {
	case err != nil:
		log.Printf("%scould not read origin to resolve the PR base of %s (%v) — the brief names the integration branch %q",
			labelOf(t), displayRepo(dir, repo), err, integration)
		return integration
	case onOrigin:
		return integration
	case def != "":
		log.Printf("%s%s has no %q on origin — its PRs target origin's default branch %q",
			labelOf(t), displayRepo(dir, repo), integration, def)
		return def
	default:
		return integration
	}
}

// displayRepo names the subject of a prBaseFor log line: the task's repo
// identity when the plane supplied one, else the checkout the probe ran in.
func displayRepo(dir, repo string) string {
	if repo != "" {
		return repo
	}
	return dir
}

// probeOriginPRBase asks the origin remote of dir two questions in ONE network
// round-trip: whether refs/heads/<integration> exists there, and what its
// symbolic HEAD points at — the answers prBaseFor's resolution order needs.
//
// `git ls-remote --symref origin HEAD refs/heads/<integration>` prints the
// symref line ("ref: refs/heads/main	HEAD") plus each matching ref, so one call
// answers both. Reading origin is deliberate and the local origin/HEAD symref
// is NOT an alternative: it is unset in some of this fleet's clones (the
// makespdf-zed-extension and clankerbar-cli checkouts the task that filed this
// recorded), and where it is set it can be stale. A remote whose HEAD is
// detached prints no symref line: the default is then unknown and prBaseFor
// falls back.
//
// The read is scoped to the account owning the remote (delivery.ScopeRemoteEnv,
// CLA-458) exactly as the delivery verifier's own ls-remote is: without it an
// HTTPS github origin that names no account is read as whichever gh account is
// ACTIVE, which on the operator's machine cannot see the private lecstor repos,
// and every probe would silently degrade to the integration-branch fallback —
// the feature would never fire where it was filed from. Any scoping failure
// degrades to running unscoped, the same fail-open shape delivery keeps; the
// probe has no correctness cliff on top of it because its own failure mode is
// prBaseFor's fallback.
func probeOriginPRBase(ctx context.Context, dir, integration string) (bool, string, error) {
	ctx, cancel := context.WithTimeout(ctx, prBaseProbeTimeout)
	defer cancel()
	remoteURL, _ := originRemoteURL(ctx, dir)
	extraEnv := delivery.ScopeRemoteEnv(ctx, dir, "gh", remoteURL)
	return runOriginLsRemote(ctx, dir, integration, extraEnv)
}

// originRemoteURL reads origin's URL in dir for the credential-scoping lookup.
// A local read: it never consults credentials, and an error (no origin, not a
// repository) leaves the scoping empty, which means "run unscoped".
func originRemoteURL(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// runOriginLsRemote is probeOriginPRBase's one git exec, split out so the probe
// reads as its two answers. The environment discipline is delivery.Verifier's
// gitRun: an unattended run has no terminal, and a git that decides to ask for
// a password would hang the phase rather than answer. WaitDelay reaps the
// helpers ls-remote spawns — they inherit the pipes behind these buffers, and a
// killed git can otherwise leave the call sitting past its deadline.
func runOriginLsRemote(ctx context.Context, dir, integration string, extraEnv []string) (bool, string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "ls-remote", "--symref", "origin", "HEAD", "refs/heads/"+integration)
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=echo",
		"SSH_ASKPASS=echo",
		"GIT_SSH_COMMAND=ssh -oBatchMode=yes -oConnectTimeout=10",
		"GCM_INTERACTIVE=never",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return false, "", fmt.Errorf("git ls-remote: %w: %s", err, firstLineOf(msg))
		}
		return false, "", fmt.Errorf("git ls-remote: %w", err)
	}
	onOrigin, defaultBranch := parseOriginRefs(stdout.String(), integration)
	return onOrigin, defaultBranch, nil
}

// parseOriginRefs reads runOriginLsRemote's output: the "ref: refs/heads/X
// HEAD" symref line when the remote's HEAD is symbolic, and any line naming
// refs/heads/<integration>. A detached remote HEAD prints no symref line, so
// the default comes back empty and prBaseFor falls back. Exact-field matching,
// never substring: "refs/heads/staging" must not be read as a match for an
// integration branch named "ging".
func parseOriginRefs(out, integration string) (onOrigin bool, defaultBranch string) {
	want := "refs/heads/" + integration
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) == 3 && fields[0] == "ref:" && fields[2] == "HEAD":
			if target, ok := strings.CutPrefix(fields[1], "refs/heads/"); ok {
				defaultBranch = target
			}
		case len(fields) == 2 && fields[1] == want:
			onOrigin = true
		}
	}
	return onOrigin, defaultBranch
}

// firstLineOf trims a git stderr message to its first line, so a two-line
// remote error does not run into the log's next sentence.
func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// extraDirsFor lists the directories a session's permission policy must cover
// BESIDES where it runs: every checkout the project declares, whatever directory
// the session starts in (agent-rule-scoping piece 2). This is the piece that
// makes a best-effort spawn cwd safe — a two-repo project whose session started
// in one repo can still reach the other.
//
// One implicit entry joins the declared ones: the conventional worktree area
// beside the spawn checkout (<checkout>-wt as its sibling, this fleet's
// documented layout). Task worktrees are separate git roots OUTSIDE every
// declared repo, and a session that can create one but not edit in it is worse
// than one never started. It is added only when the directory already exists —
// no speculation, no widening beyond what is on disk.
//
// With nothing declared and no worktree area found this returns nil, which leaves
// each adapter's policy exactly as it shipped before CLA-437.
func (d *Driver) extraDirsFor(t Target, spawnDir string) []string {
	workdir := t.WorkDir
	if workdir == "" {
		workdir = d.cfg.WorkDir
	}
	// With the per-project layout declared, every grant must stay INSIDE the
	// project root. The scope below is additive: declared checkouts are added
	// before it, so a stale `repos` override pointing at a developer checkout
	// would be granted outright even though the session is scoped to the
	// project — the escape the layout exists to close (CLA-678 review round 2,
	// finding 3). sessionDir refuses such a checkout as a spawn dir; this also
	// keeps it out of the reach list, alongside the conventional worktree area.
	root := ""
	if t.ProjectRoot && workdir != "" {
		root = workdir
	}
	declared := config.DeclaredCheckouts(t.Repos, t.PrimaryRepo, workdir)
	var out []string
	seen := map[string]bool{}
	add := func(dir string) {
		if dir == "" || dir == spawnDir || seen[dir] {
			return
		}
		seen[dir] = true
		out = append(out, dir)
	}
	for _, dir := range declared {
		if root != "" && !insideProjectRoot(root, dir) {
			log.Printf("%sdeclared checkout %s is outside the project root %s - not adding it to the session's permissions", labelOf(t), dir, workdir)
			continue
		}
		add(dir)
	}
	if spawnDir != "" {
		wt := filepath.Join(filepath.Dir(spawnDir), filepath.Base(spawnDir)+"-wt")
		if fi, err := os.Stat(wt); err == nil && fi.IsDir() {
			if root == "" || insideProjectRoot(root, wt) {
				add(wt)
			}
		}
	}
	// The PROJECT ROOT and its children (CLA-678), when the config declares the
	// per-project layout (`project_root`): a session starts in
	// <project root>/<repo name>, and the rest of its project — the other repos,
	// and the worktrees directory every task worktree lives in — sits beside it
	// under the same root. Granting the children is what makes cross-repo work
	// inside a project possible at all, and the project root itself is granted
	// so `external_directory` — the gate that decides a path-taking tool's reach
	// — is carved out for the same subtree rather than for one checkout.
	//
	// The flag is what keeps this off a developer's multi-repo parent: without
	// it a session starting in one of `~/dev`'s ~150 trees would be handed every
	// sibling it happens to have there. Nothing is granted on a guess.
	if t.ProjectRoot {
		if scope := projectScope(workdir, spawnDir); scope != "" {
			add(scope)
			for _, child := range childDirs(scope) {
				add(child)
			}
		}
	}
	return out
}

// projectScope reports the PROJECT ROOT a session's reach is bounded to: the
// workdir, when the session starts strictly INSIDE it — the per-project layout,
// where the session runs in <project root>/<repo name> and the project's other
// repos and its worktrees sit beside that checkout. "" means there is no scope
// to widen, which is every legacy shape (the workdir is the checkout itself, or
// the session starts nowhere near it).
func projectScope(workdir, spawnDir string) string {
	if workdir == "" || spawnDir == "" {
		return ""
	}
	wd, sd := absOrSelfPath(workdir), absOrSelfPath(spawnDir)
	if wd == sd {
		return ""
	}
	rel, err := filepath.Rel(wd, sd)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(filepath.ToSlash(rel), "../") {
		return ""
	}
	return wd
}

// insideProjectRoot reports whether path is the project root itself or lies
// inside it. Both sides are compared on canonical paths, so a symlinked
// checkout cannot masquerade as inside the root — the supervisor's derivation
// refuses symlinks at the derived checkout, and this is the driver-side
// containment the additive permission list needs.
func insideProjectRoot(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	r, p := canonicalPath(root), canonicalPath(path)
	if r == p {
		return true
	}
	rel, err := filepath.Rel(r, p)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	return rel != ".." && !strings.HasPrefix(rel, "../")
}

// canonicalPath resolves a path for containment comparison: absolute, cleaned,
// and with symlinks resolved when the path exists. A path that exists only
// lexically (a missing entry) is returned absolute-but-unresolved — the
// comparison then treats it by name, never as some other directory.
func canonicalPath(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	abs = filepath.Clean(abs)
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// childDirs lists the immediate subdirectories of dir that exist, sorted. It is
// how a project root's repos and its worktrees directory are found without a
// declaration: the layout is the declaration.
func childDirs(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out
}

// absOrSelfPath resolves a path against the process cwd, returning it unchanged
// when it cannot be resolved (the same posture as the harness package's
// absOrSelf: an unresolvable path must not become a different path).
func absOrSelfPath(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
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

// waitOrStop waits up to dur, but stays responsive to a STOP marker (consuming it)
// and to context cancellation. Reports whether the loop should stop.
//
// CLA-461: the restart/reload markers get the same responsiveness, so a control
// request is never stuck behind a multi-hour idle poll or a supervised limit
// wait. RESTART / RESTART_NOW return true (stop) with restartRequested set, so
// the caller's caller re-execs; RELOAD re-reads the config in place and lets the
// CURRENT wait run out on its existing schedule — ending a supervised limit
// wait early would turn a dial edit into an immediate paid probe against the
// very limit being waited out. A changed poll_interval applies from the NEXT
// pause, which recomputes it.
func (d *Driver) waitOrStop(ctx context.Context, dur time.Duration) bool {
	const chunk = 3 * time.Second
	// Round(0) strips the monotonic reading, so wallStart measures REAL elapsed
	// time. Everything else here runs on the monotonic clock, which does not
	// advance while the machine is suspended — so comparing the two is the only
	// way a slept-through wait becomes visible. Without it, a laptop that idle
	// sleeps turns a 30-minute pause into a multi-hour silence that looks exactly
	// like a hung process.
	wallStart := time.Now().Round(0)
	end := time.Now().Add(dur)
	for {
		if present, _ := d.readMarker("STOP"); present {
			_ = d.state.Remove("STOP")
			log.Print("STOP requested during wait — stopping")
			return true
		}
		if present, msg := d.readMarker(MarkerRestart); present {
			_ = d.state.Remove(MarkerRestart)
			if msg != "" {
				log.Printf("RESTART requested during wait: %s", msg)
			} else {
				log.Print("RESTART requested during wait")
			}
			d.restartRequested.Store(true)
			return true
		}
		if present, msg := d.readMarker(MarkerRestartNow); present {
			_ = d.state.Remove(MarkerRestartNow)
			if msg != "" {
				log.Printf("RESTART_NOW requested during wait: %s", msg)
			} else {
				log.Print("RESTART_NOW requested during wait")
			}
			d.restartRequested.Store(true)
			return true
		}
		if present, _ := d.readMarker(MarkerReload); present {
			_ = d.state.Remove(MarkerReload)
			log.Print("RELOAD requested during wait — reloading config; this wait keeps its schedule")
			d.applyReload()
		}
		remaining := time.Until(end)
		if remaining <= 0 {
			if lost, stalled := sleepStall(dur, time.Now().Round(0).Sub(wallStart)); stalled {
				log.Printf("wait of %s took %s of wall clock — the machine was suspended for ~%s; timers are frozen while it sleeps, so an unattended run stalls silently (macOS: `caffeinate -i clankerbar run …`, and start it on AC — plugging in later does not wake a sleeping Mac)",
					dur, time.Now().Round(0).Sub(wallStart).Round(time.Second), lost.Round(time.Second))
			}
			return false
		}
		w := chunk
		if remaining < w {
			w = remaining
		}
		select {
		case <-ctx.Done():
			return true
		case <-time.After(w):
		}
	}
}

// readMarker reports whether a control marker file exists, and its first line.
//
// A marker is the operator's switch, so it is read through the state dir handle:
// a symlink at the name is refused rather than followed, and the read is capped.
// Following one would let whoever planted it choose a file for us to open and a
// line of it to echo into the daemon's log.
func (d *Driver) readMarker(name string) (bool, string) {
	b, err := d.state.ReadFile(name)
	if err != nil {
		return false, ""
	}
	line := strings.TrimSpace(string(b))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return true, line
}

// randomTail is the unguessable part of an iteration log's name. crypto/rand,
// not math/rand: the whole point is that the session whose transcript this is
// cannot predict it, and a seeded PRNG in a daemon it can watch start is not
// unpredictable. rand.Read never fails on any supported platform (it panics
// internally rather than returning an error), so there is no fallback branch to
// get wrong.
func randomTail() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func resetSuffix(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return " (stated reset " + t.Format("Mon 15:04") + ")"
}

// limitReason renders a limit's reason for a log line, defaulting when a harness
// leaves it blank.
func limitReason(lim harness.Limit) string {
	if lim.Reason != "" {
		return lim.Reason
	}
	return "usage limit"
}

// isReviewPhase reports whether a phase is a review-phase (the gate where
// escalation applies). Named phases whose name is "review" are review phases;
// custom prompts with no name are never treated as review for escalation.
func isReviewPhase(ph config.Phase) bool {
	return ph.Name == config.ReviewPhaseName
}

// isVerifyPhase reports whether a RESOLVED phase is the fix-verification round
// (CLA-638). It keys on the resolved prompt, like every other review-shaped
// predicate: a handoff respawn replaces the prompt, and an operator's custom
// prompt named "verify" owns its own contract and is not one.
func isVerifyPhase(ph config.Phase) bool {
	return config.VerifyShaped(ph.Prompt)
}

// diffAgainstIntegration compares a pushed branch to the integration branch
// (staging) for changed paths, using git in the work directory.
// It returns the list of changed file paths, or an error if the diff fails.
func (d *Driver) diffAgainstIntegration(t Target, branchName string, workDir string) ([]string, error) {
	if branchName == "" {
		return nil, errors.New("no branch recorded for diff")
	}
	wd := workDir
	if wd == "" {
		wd = d.cfg.WorkDir
		if wd == "" {
			wd = "."
		}
	}
	// Use git diff against the integration branch (staging by default).
	// The command compares the branch tip to origin/staging.
	cmd := exec.Command("git", "-C", wd, "diff", "--name-only", "origin/staging..."+branchName)
	out, err := cmd.Output()
	if err != nil {
		// If the branch is new or the diff command fails, fall back to comparing
		// the branch to the local staging branch.
		cmd2 := exec.Command("git", "-C", wd, "diff", "--name-only", "staging..."+branchName)
		out, err = cmd2.Output()
		if err != nil {
			// If both fail, return empty paths rather than failing the spawn.
			return nil, nil
		}
	}
	paths := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(paths) == 1 && paths[0] == "" {
		return nil, nil
	}
	result := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result, nil
}
