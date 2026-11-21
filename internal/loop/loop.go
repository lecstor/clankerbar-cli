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
// (harness.Adapter.IsUnclassifiedTransient).
var errUnclassifiedRetryLoop = errors.New("unclassified retry loop")

// unclassifiedRetryBound is how many retries one phase's ladder may take on heuristically-judged
// failures before it gives up. Deliberately a constant and not a config dial.
const unclassifiedRetryBound = 2

// perTaskDeadBound is how many consecutive dead phases on the SAME task park it.
const perTaskDeadBound = 4

// fleetDeadBound is how many consecutive dead phases ACROSS tasks pause the loop for that target
// and raise one project-level question.
const fleetDeadBound = 5

const startupFailBound = 5

// Target is one backlog a Driver drives: a project's cheap poller plus where that project's drain
// sessions run.
type Target struct {
	// Name is the project slug, used to prefix log lines when there is more than one target.
	Name string

	// Poller is this project's cheap backlog read.
	Poller backlog.Poller

	// WorkDir is where this project's sessions run. Empty = the config's workdir.
	WorkDir string

	// MCPConfigPath points this project's sessions at its .mcp.json (the file whose /mcp/<slug> URL
	// selects the project). Empty = the config's path.
	MCPConfigPath string

	// MCPConfigPaths is this project's MCP config PER HARNESS, for a sequence whose phases are not all
	// on one harness.
	MCPConfigPaths map[string]string

	// Repos is this project's repo -> local-checkout map, and PrimaryRepo the fallback for a task that
	// carries no repo.
	Repos map[string]string

	// PrimaryRepo backs a session whose task names no repo (a fresh phase before the plane returns
	// repos for everything). See Repos above.
	PrimaryRepo string

	// ProjectRoot says this target's workdir is a FLEET PROJECT ROOT: a directory holding this
	// project's clones and its worktrees directory, with each session's checkout inside it.
	ProjectRoot bool

	// Releaser hands back a claim a session left holding.
	Releaser plane.Releaser

	// Fleet reports this project's activity to the plane: a presence beacon on every backlog poll,
	// state-change beacons at phase boundaries, and exactly one iteration record per drain.
	Fleet fleet.Reporter

	// RCfg fetches this project's stored execution config.
	RCfg plane.RunConfigReader

	// Cfg is this target's EFFECTIVE config — the driver's base config with the project's stored
	// run-config document overlaid — or nil when no document is in force, in which case every read
	// falls through to d.cfg unchanged.
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

	// now is the clock the scheduled-pause check reads (at the poll gate and at a phase boundary), the
	// route-window filter (routableRoutes) and route-mark expiry, so a pricing window and a mark can
	// both be driven from a fixed clock in tests.
	now         func() time.Time
	schedPaused bool

	// echoOut is where an ENABLED session echo writes (cfg.EchoSessions true).
	echoOut io.Writer

	panelLaunch func(role string)

	// panelRelease, when non-nil, is called with the member's index each time a PARALLEL panel member
	// releases its successor, before that successor is unblocked.
	panelRelease func(k int)

	// undeclared counts the sessions this run that ended still holding a task whose work they had
	// already pushed — the phase did its job and then did not hand the task over.
	undeclared int

	// Per-target no-progress state.
	quietTokens []int       // tokens spent on this target's drains that settled nothing
	spent       []int       // the last drain's tokens, awaiting its progress verdict
	baseline    []int       // settled count when this target's last drain began
	openQs      []int       // open-question count at this target's last poll
	pending     []bool      // a drain of this target is awaiting its progress verdict
	skipUntil   []time.Time // a backed-off target is ineligible until this time

	// Per-target harness-failure state.
	harnessFails []int
	harnessErrs  []error

	// The resumed-phase transient streak, keyed by the target that failed.
	resumedTransients map[resumedTransientKey]int

	// routeMarks is this instance's sticky memory of model routes that hit a usage limit: route alias
	// -> the instant the mark expires.
	routeMarks   map[string]time.Time
	routeMarksMu sync.Mutex

	// resumedTransientWait computes that wait from the streak: backoff(n) in production.
	resumedTransientWait func(n int) time.Duration

	// The fleet dead-phase counter.
	fleetDead []int // consecutive dead phases across all tasks on this target

	// fleetPaused marks a target whose fleet counter has tripped: the loop stops spawning sessions for
	// it until the operator answers the raised question — a known-failing provider must stop burning
	// sessions.
	fleetPaused []bool

	fleetRaised []bool

	fleetOpenQ []int

	// Per-target harness-startup-failure state.
	startupFails []int

	startupPaused []bool

	startupRaised []bool

	startupOpenQ []int

	waitGrace time.Duration

	// limitBeaconInterval is how often a paused wait re-beacons `limited`.
	limitBeaconInterval time.Duration

	leaseRenewalInterval time.Duration

	// newVerifier builds the delivery checker for a workdir.
	newVerifier func(workdir string, allowUncheckedPR bool) deliveryVerifier

	// newSalvager builds the stranded-work rescuer for a workdir.
	newSalvager func(workdir string) workSalvager

	// probePRBase reads the origin remote of the checkout in dir to resolve a repo's PR base when no
	// pr_base override names one: whether refs/heads/<integration> exists on origin, and origin's
	// default branch (its symbolic HEAD target).
	probePRBase func(ctx context.Context, dir, integration string) (integrationOnOrigin bool, defaultBranch string, err error)

	// originBranch reads what origin says about the task's branch, for the start-phase choice.
	originBranch func(ctx context.Context, checkout, branch, doneTip string) originBranchFacts

	// prepareWorktree prepares the task worktree every phase of a runner-claimed drain runs in.
	prepareWorktree func(ctx context.Context, req worktreeRequest) (worktreePrep, error)

	// worktreeCheck is the READ-ONLY pre-claim verdict on the task's existing worktree, made before
	// the claim so a deterministic refusal costs no reclaim.
	worktreeCheck func(ctx context.Context, req worktreeRequest, doneTip string) (string, originBranchFacts, error)

	newAdapter func(name string) (harness.Adapter, error)

	// stateMu serialises the run-scoped ledgers and maps below.
	stateMu sync.Mutex

	// The agent event channel's wiring: the trusted plane origin, the account key and the process-wide
	// per-run sequence counter.
	eventMu     sync.Mutex
	eventOrigin string
	eventKey    string
	eventSeq    *events.SeqCounter

	inboxOrigin string
	inboxKey    string

	// startRefusals counts consecutive worktree refusals per task id, so a tree that refuses every
	// claim deterministically reaches the operator instead of the queue.
	startRefusals map[string]int

	// claimFileMu serialises CURRENT_CLAIM writes.
	claimFileMu sync.Mutex

	// controlMu serialises control-marker CONSUMPTION and the RELOAD action.
	controlMu sync.Mutex

	deadTally map[tallyKey]*phaseTally

	// panelFailStreak is how many consecutive TASKS each review-panel member has recorded nothing on,
	// keyed by the member's log label (the positional `review-N` when unnamed).
	panelFailStreak map[string]int

	// restartRequested flips when a RESTART / RESTART_NOW marker has been consumed and the run is
	// unwinding toward a re-exec.
	restartRequested atomic.Bool

	expireUnsupportedOnce sync.Once

	beaconWriteFailed atomic.Bool

	// reloadConfig rebuilds the config for a RELOAD marker.
	reloadConfig func() (*config.Config, error)

	// restartPoll is how often the RESTART_NOW watcher polls the state dir.
	restartPoll time.Duration

	iter   []iterState
	iterMu sync.Mutex

	hold []holdState

	// hostname/hostOnce cache os.Hostname for the fleet beacon identity.
	hostname string
	hostOnce sync.Once

	// Per-target run-config state.
	rcVersions   []int
	rcAttempt    []time.Time
	rcAttemptVer []int
	rcFails      []int

	tgtSpend []targetSpend

	tgtStart []time.Time

	pausedRun     time.Duration
	pausedTargets []time.Duration

	// overlayBudget marks a target whose stored run-config document SET a budget: only that target's
	// own spend is judged by it.
	overlayBudget []bool

	// exhausted marks a target that has reached the budget in force for it: no more sessions for it
	// this run, while its siblings keep draining. Run stops only when every target is exhausted.
	exhausted     []bool
	budgetStopped []bool

	overrides config.Overrides
}

// resumedTransientKey names the target a resumed phase's transient failure is charged to: the
// project (its index in targets) and the harness the failed phase runs on.
type resumedTransientKey struct {
	target  int
	harness string
}

// deliveryVerifier is the driver's view of internal/delivery, narrowed to the one call it makes.
type deliveryVerifier interface {
	Verify(ctx context.Context, c delivery.Claim) delivery.Report
}

// workSalvager is the driver's view of internal/salvage, narrowed the same way.
type workSalvager interface {
	// Salvage rescues the work a dead session left in a worktree.
	Salvage(ctx context.Context, taskID, label string, recorded ...string) salvage.Outcome
}

// New builds a single-project Driver — the original mode, driven entirely by the top-level config
// fields.
func New(cfg *config.Config, h harness.Adapter, poller backlog.Poller) *Driver {
	return NewMulti(cfg, h, []Target{{Poller: poller}})
}

// NewMulti builds a Driver over several targets: one loop instance, one account key, many project
// queues — round-robin over whichever have claimable work.
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

// adapterFor resolves the harness that runs one phase.
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

func (d *Driver) prefix(i int) string {
	return labelOf(d.targets[i])
}

func labelOf(t Target) string {
	if t.Name == "" {
		return ""
	}
	return "[" + t.Name + "] "
}
