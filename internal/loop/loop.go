// Package loop is the driver: a long-running daemon that gates each iteration on a cheap backlog
// read, spends a fresh harness session only when there is claimable work, and stays alive and
// keeps polling when the queue is empty, so it reacts when questions are answered, items are
// promoted, or new work is filed. Fuller rationale: docs/loop-rationale.md.
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
	"github.com/lecstor/clankerbar-cli/internal/events"
	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
	"github.com/lecstor/clankerbar-cli/internal/salvage"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

var errZeroSpendLoop = errors.New("zero-spend attempt loop")

// errUnclassifiedRetryLoop marks the other give-up no ceiling would have reached: a ladder of
// retries taken on a failure the adapter could not NAME, only judge by heuristic
// (harness.Adapter.IsUnclassifiedTransient). Also distinct because the operator's next move is
// distinct: this one means the adapter has no pattern for what went wrong, so the fix is to read
// the diagnostic in the error and — if it is a real blip — teach the adapter's transient
// patterns about it.
var errUnclassifiedRetryLoop = errors.New("unclassified retry loop")

// unclassifiedRetryBound is how many retries one phase's ladder may take on heuristically-judged
// failures before it gives up. Deliberately a constant and not a config dial.
const unclassifiedRetryBound = 2

// perTaskDeadBound is how many consecutive dead phases on the SAME task park it. Four consecutive
// deaths is a ~2% roll — a signal rather than noise — while a task that dies once per drain and
// succeeds between (the retry re-claims the same task within the drain) never accumulates past one
// here, because the counter resets when a phase of the task advances (see the reset below).
const perTaskDeadBound = 4

// fleetDeadBound is how many consecutive dead phases ACROSS tasks pause the loop for that target
// and raise one project-level question. One more than the per-task bound, on purpose: a single
// cursed task parks at 4 (its own counter fires first, and the drain breaks before any fifth
// death), so the fleet counter can only reach 5 across DIFFERENT tasks — the shape only a
// fleet-wide fault produces.
const fleetDeadBound = 5

// startupFailBound is how many consecutive harness-startup failures on one target pause it and
// raise one project-level question.
const startupFailBound = 5

type Target struct {
	Name string

	Poller backlog.Poller

	WorkDir string

	MCPConfigPath string

	// MCPConfigPaths is this project's MCP config PER HARNESS, for a sequence whose phases are not all
	// on one harness. Empty on every single-harness run, and config.Validate refuses the combination
	// that would need an entry here and lacks one.
	MCPConfigPaths map[string]string

	// Repos is this project's repo -> local-checkout map, and PrimaryRepo the fallback for a task that
	// carries no repo. Between them they decide where each session STARTS: the task's own repo
	// resolved to a checkout, never blindly the workdir — so the session reads its repo's rule files
	// instead of a sibling's, and a grant scoped per-cwd cannot wall off the tree the task needs.
	Repos map[string]string

	PrimaryRepo string

	ProjectRoot bool

	// Releaser hands back a claim a session left holding. Nil disables the handback for this target,
	// which costs only the reclaim an expiring lease would have cost anyway — so a target without
	// one degrades, it does not break.
	Releaser plane.Releaser

	// Fleet reports this project's activity to the plane: a presence beacon on every backlog poll,
	// state-change beacons at phase boundaries, and exactly one iteration record per drain. Nil (not
	// wired) disables reporting, which costs only console visibility — reporting is telemetry and is
	// never load-bearing.
	Fleet fleet.Reporter

	RCfg plane.RunConfigReader

	// Cfg is this target's EFFECTIVE config — the driver's base config with the project's stored
	// run-config document overlaid — or nil when no document is in force, in which case every read
	// falls through to d.cfg unchanged. Rebuilt between iterations when the poll's runConfigVersion
	// moves — never mid-session.
	Cfg *config.Config
}

type Driver struct {
	cfg     *config.Config
	h       harness.Adapter
	targets []Target
	paused  []bool // per-target console-pause state, to log transitions once
	cursor  int    // round-robin position over targets (last drained)
	state   *statedir.Dir
	blind   bool // no cheap backlog read available — drain then idle-poll

	// now is the clock the scheduled-pause check reads (at the poll gate and at a phase boundary), the
	// route-window filter (routableRoutes) and route-mark expiry, so a pricing window and a mark can
	// both be driven from a fixed clock in tests. Three readers deliberately stay on the WALL clock:
	// the back-off ladder in progress.go, loadRouteMarks (a persisted mark is an absolute instant
	// written by a previous process, so it is loaded against real time) and routeRetryLabel (it
	// renders an operator's clock).
	now         func() time.Time
	schedPaused bool

	echoOut io.Writer

	panelLaunch func(role string)

	// panelRelease, when non-nil, is called with the member's index each time a PARALLEL panel member
	// releases its successor, before that successor is unblocked. panelLaunch pins the launch ORDER;
	// this pins the release POINT, which is the guarantee itself: a member must release from its
	// launch boundary (phaseRunOpts.onSessionLaunch), not from its worktree being ready.
	panelRelease func(k int)

	undeclared int

	// Per-target no-progress state. The gate cannot tell, so it spawns, the session correctly
	// declines, and the operator pays for the same report every cycle.
	quietTokens []int       // tokens spent on this target's drains that settled nothing
	spent       []int       // the last drain's tokens, awaiting its progress verdict
	baseline    []int       // settled count when this target's last drain began
	openQs      []int       // open-question count at this target's last poll
	pending     []bool      // a drain of this target is awaiting its progress verdict
	skipUntil   []time.Time // a backed-off target is ineligible until this time

	harnessFails []int
	harnessErrs  []error

	// The resumed-phase transient streak, keyed by the target that failed.
	resumedTransients map[resumedTransientKey]int

	// routeMarks is this instance's sticky memory of model routes that hit a usage limit: route alias
	// -> the instant the mark expires. A run moved off its free route stays on the paid one for later
	// PHASES until the mark lifts, so the fleet does not flip between routes at every phase boundary;
	// it goes back to free only when the free route's reset has passed.
	routeMarks   map[string]time.Time
	routeMarksMu sync.Mutex

	// resumedTransientWait overrides the backoff for a resumed phase's transient wait. Nil in
	// production (nil means backoff); a test seam so the ladder can be driven without sleeping.
	resumedTransientWait func(n int) time.Duration

	// The fleet dead-phase counter. Per-target because a multi-target driver runs several projects,
	// and "the fleet" is one project's sessions: a run of dead phases on project A must not pause
	// project B.
	fleetDead []int // consecutive dead phases across all tasks on this target

	// fleetPaused marks a target whose fleet counter has tripped: the loop stops spawning sessions for
	// it until the operator answers the raised question — a known-failing provider must stop burning
	// sessions.
	fleetPaused []bool

	fleetRaised []bool

	fleetOpenQ []int

	// Per-target harness-startup-failure state. The two counters are deliberately distinct and one
	// failure never ticks both: a dead phase returns err == nil (handled inside drainPhases, never
	// reaching the sideline path), and a startup failure returns err != nil with tokens == 0 (never
	// satisfying end.dead, which requires a held claim).
	startupFails []int

	startupPaused []bool

	startupRaised []bool

	startupOpenQ []int

	waitGrace time.Duration

	limitBeaconInterval time.Duration

	leaseRenewalInterval time.Duration

	newVerifier func(workdir string, allowUncheckedPR bool) deliveryVerifier

	newSalvager func(workdir string) workSalvager

	// probePRBase reads the origin remote of the checkout in dir to resolve a repo's PR base when no
	// pr_base override names one: whether refs/heads/<integration> exists on origin, and origin's
	// default branch (its symbolic HEAD target). Errors are NOT failures — the caller falls back to
	// the configured integration_branch, exactly the text the briefs carried before this feature —
	// so a probe that cannot run degrades to today's behaviour, never blocks a spawn.
	probePRBase func(ctx context.Context, dir, integration string) (integrationOnOrigin bool, defaultBranch string, err error)

	originBranch func(ctx context.Context, checkout, branch, doneTip string) originBranchFacts

	prepareWorktree func(ctx context.Context, req worktreeRequest) (worktreePrep, error)

	// worktreeCheck is the READ-ONLY pre-claim verdict on the task's existing worktree, made before
	// the claim so a deterministic refusal costs no reclaim.
	worktreeCheck func(ctx context.Context, req worktreeRequest, doneTip string) (string, originBranchFacts, error)

	newAdapter func(name string) (harness.Adapter, error)

	stateMu sync.Mutex

	eventMu     sync.Mutex
	eventOrigin string
	eventKey    string
	eventSeq    *events.SeqCounter

	inboxOrigin string
	inboxKey    string

	// startRefusals counts consecutive worktree refusals per task id, so a tree that refuses every
	// claim deterministically reaches the operator instead of the queue.
	startRefusals map[string]int

	claimFileMu sync.Mutex

	controlMu sync.Mutex

	deadTally map[tallyKey]*phaseTally

	// panelFailStreak is how many consecutive TASKS each review-panel member has recorded nothing on,
	// keyed by the member's log label (the positional `review-N` when unnamed). Like deadTally it is
	// the run's account: one flaky member is a blip, but the same member failing task after task is a
	// fault in that member's harness, model or brief, and the warning it earns must not depend on the
	// drain boundary.
	panelFailStreak map[string]int

	restartRequested atomic.Bool

	expireUnsupportedOnce sync.Once

	beaconWriteFailed atomic.Bool

	reloadConfig func() (*config.Config, error)

	restartPoll time.Duration

	iter   []iterState
	iterMu sync.Mutex

	hold []holdState

	hostname string
	hostOnce sync.Once

	// Per-target run-config state. rcVersions holds the version each target's effective config was
	// built from, -1 until the first fetch; the poll's runConfigVersion differing from it is the
	// re-read signal, honoured exactly where STOP/RELOAD are: the ITERATION boundary (the poll only
	// happens there), never mid-session.
	rcVersions   []int
	rcAttempt    []time.Time
	rcAttemptVer []int
	rcFails      []int

	tgtSpend []targetSpend

	tgtStart []time.Time

	pausedRun     time.Duration
	pausedTargets []time.Duration

	// overlayBudget marks a target whose stored run-config document SET a budget: only that target's
	// own spend is judged by it. A document that sets no budget leaves the target in the local pool,
	// because the README's exception is the document's budget, not its mere existence (see budget.go).
	overlayBudget []bool

	// exhausted marks a target that has reached the budget in force for it: no more sessions for it
	// this run, while its siblings keep draining. Run stops only when every target is exhausted.
	exhausted     []bool
	budgetStopped []bool

	overrides config.Overrides
}

type resumedTransientKey struct {
	target  int
	harness string
}

type deliveryVerifier interface {
	Verify(ctx context.Context, c delivery.Claim) delivery.Report
}

type workSalvager interface {
	Salvage(ctx context.Context, taskID, label string, recorded ...string) salvage.Outcome
}

func New(cfg *config.Config, h harness.Adapter, poller backlog.Poller) *Driver {
	return NewMulti(cfg, h, []Target{{Poller: poller}})
}

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
		hold:                make([]holdState, n),
		rcVersions:          make([]int, n),
		rcAttempt:           make([]time.Time, n),
		rcAttemptVer:        make([]int, n),
		rcFails:             make([]int, n),
		tgtSpend:            make([]targetSpend, n),
		tgtStart:            make([]time.Time, n),
		pausedTargets:       make([]time.Duration, n),
		overlayBudget:       make([]bool, n),
		exhausted:           make([]bool, n),
		budgetStopped:       make([]bool, n),
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
	d.now = time.Now
	for i := range d.rcVersions {
		d.rcVersions[i] = -1 // unfetched; 0 is a REAL answer (nothing stored)
	}
	return d
}

// adapterFor resolves the harness that runs one phase. config.Validate has already refused an
// unregistered harness, so reaching here means the registry and the config disagree, and quietly
// running the phase on the other harness would be the wrong half of a mixed sequence — the review
// phase spawning on the implement harness, with nothing in the log to say so.
func (d *Driver) adapterFor(ph config.Phase) (harness.Adapter, error) {
	name := ph.Harness
	if name == "" || name == d.cfg.Harness {
		return d.h, nil
	}
	return d.newAdapter(name)
}

func (d *Driver) originBranchFn() func(ctx context.Context, checkout, branch, doneTip string) originBranchFacts {
	if d.originBranch != nil {
		return d.originBranch
	}
	return readOriginBranch
}

func (d *Driver) prepareWorktreeFn() func(ctx context.Context, req worktreeRequest) (worktreePrep, error) {
	if d.prepareWorktree != nil {
		return d.prepareWorktree
	}
	return prepareTaskWorktree
}

func (d *Driver) worktreeCheckFn() func(ctx context.Context, req worktreeRequest, doneTip string) (string, originBranchFacts, error) {
	if d.worktreeCheck != nil {
		return d.worktreeCheck
	}
	return claimWorktreeRefusal
}

func (d *Driver) prefix(i int) string {
	return labelOf(d.targets[i])
}

func labelOf(t Target) string {
	if t.Name == "" {
		return ""
	}
	return "[" + t.Name + "] "
}
