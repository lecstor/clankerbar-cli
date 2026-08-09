// Package harness abstracts a coding-agent CLI (Claude Code, Codex, ...) behind
// the small contract the loop needs. Each method maps to a row of the capability
// table in the design memo (docs/proposals/looping.md). Capabilities that not
// every harness supports — usage introspection — return a sentinel so the loop
// degrades gracefully rather than assuming.
package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Adapter is one harness. Implementations register themselves via Register in an
// init() so the loop can select by name.
type Adapter interface {
	// Name is the harness identifier ("claude", "codex").
	Name() string

	// Invoke runs one fresh, non-interactive session and returns its outcome.
	// This is the process the loop respawns each iteration; it must honour ctx
	// cancellation (Ctrl-C / SIGTERM / a supervised-wait deadline).
	Invoke(ctx context.Context, in Invocation) (Result, error)

	// DetectLimit decides whether a finished Result died on the subscription usage
	// cap (5-hour / weekly), and if so when it is expected to reset (best-effort;
	// the reset is an upper bound, not a wake signal — the loop polls for an early
	// reset). This is the long-pause case, distinct from a transient blip.
	DetectLimit(Result) Limit

	// IsTransient reports whether a non-zero exit is a retryable server/network
	// blip (API 5xx/408/429, overloaded, connection reset, ...) rather than a real
	// failure — so the loop backs off and retries the same iteration instead of
	// dying. Detection is anchored (e.g. on Claude's "API Error:" prefix) so a task
	// log that merely mentions an HTTP 500 is not mistaken for a dead session.
	IsTransient(Result) bool

	// Diagnostic returns the harness-authored text IsTransient and DetectLimit
	// were judged on — stderr, the CLI's own non-event output, typed error
	// events — with the agent's narration excluded exactly as those scans
	// exclude it. It exists so a caller that STOPS on a classification can name
	// the message it stopped on: an unrecognised non-zero exit ends the whole
	// run, and "exited 1 (non-retryable)" tells an operator nothing about which
	// failure that was. Implementations must return the same scope IsTransient
	// reads, never a wider one.
	Diagnostic(Result) string

	// Probe runs the cheapest possible request to answer "am I still limited?"
	// without doing real work — used while paused to catch an early reset.
	Probe(ctx context.Context, in Invocation) (Limit, error)

	// ReadUsage returns current window usage if the harness exposes it headless.
	// NO harness does today (see the memo), so implementations return
	// ErrUsageUnsupported and the loop falls back to the self-accounted Budget.
	// Kept in the contract because it is the right seam the day one adds it.
	ReadUsage(ctx context.Context, in Invocation) (Usage, error)
}

// Invocation is everything a harness needs to run one session. The loop builds it
// from config; adapters translate it into their own CLI dialect.
type Invocation struct {
	Prompt        string
	Model         string
	WorkDir       string
	MCPConfigPath string
	// ConfigDir sets the harness config dir (CLAUDE_CONFIG_DIR / CODEX_HOME) so a
	// headless session loads the same skills, plugins, and auth as the interactive
	// one. Empty inherits the ambient environment.
	ConfigDir string
	// SettingsPath is an extra settings file (Claude Code --settings) carrying the
	// headless permission policy. Merges with the config-dir's settings; deny wins.
	// Empty = no extra file. Claude-specific; other adapters ignore it.
	SettingsPath string
	// Console is where the adapter streams live, human-readable progress (the
	// terminal and/or a per-iteration logfile). Nil → os.Stderr.
	Console io.Writer
	Env     []string // extra env, appended to the process environment
	// Probe marks this as a cheap liveness check, not real work — adapters run
	// the smallest possible request instead of the drain prompt.
	Probe bool
}

// Result is the outcome of one session, both raw and parsed.
type Result struct {
	ExitCode     int
	Stdout       string
	Stderr       string
	FinalMessage string         // the agent's final message, when parseable
	Tokens       int            // tokens this session consumed (for the Budget)
	CostUSD      float64        // $ this session consumed
	Raw          map[string]any // adapter-specific parsed fields

	// Claim is the backlog task this session was still holding when it ended, so
	// the driver can hand it back rather than leave the lease to die (CLA-242).
	Claim Claim

	// Reports are the delivery claims this session got the plane to ACCEPT — a
	// branch recorded as the hand-off, a commit declared landed — for the driver
	// to check against local git (CLA-253). Order is the order they were made.
	Reports []Report

	// pending maps a tool_use id to what its result will mean. Parser state, not
	// output: every clankerbar call that matters is judged on the plane's ANSWER,
	// never on the request, because a refused call changes nothing.
	pending map[string]pendingKind

	// pendingReports is the same discipline for delivery claims: an `update_task`
	// REFUSED by the plane (a missing Tests header, a superseded run) recorded
	// nothing, so there is nothing to check and nothing to complain about.
	pendingReports map[string]Report
}

// pendingKind is what a tool_result is expected to tell us.
type pendingKind int

const (
	// pendingClaim: a claim_task whose result carries the task and run ids.
	pendingClaim pendingKind = iota
	// pendingSettle: a call that, if it SUCCEEDS, ends this run plane-side and
	// releases the task — so there is nothing left for the driver to hand back.
	pendingSettle
)

func (r *Result) expect(toolUseID string, k pendingKind) {
	if toolUseID == "" {
		return
	}
	if r.pending == nil {
		r.pending = map[string]pendingKind{}
	}
	r.pending[toolUseID] = k
}

// expectReport arms a delivery claim, to be kept only if the plane accepts it.
func (r *Result) expectReport(toolUseID string, rep Report) {
	if toolUseID == "" || rep.Empty() {
		return
	}
	if r.pendingReports == nil {
		r.pendingReports = map[string]Report{}
	}
	r.pendingReports[toolUseID] = rep
}

// settleReport resolves an armed delivery claim on its tool_result. accepted=false
// (the plane refused the call) drops it: nothing was recorded, so there is nothing
// to verify.
//
// Identical claims collapse. A clanker that carries `branch` on every progress
// update — which the protocol encourages, since the branch is the hand-off record
// — restates the same claim many times in one session, and each restatement would
// otherwise cost a network round trip, a duplicate log line, and a duplicate write
// to the plane.
func (r *Result) settleReport(toolUseID string, accepted bool) {
	rep, armed := r.pendingReports[toolUseID]
	if !armed {
		return
	}
	delete(r.pendingReports, toolUseID)
	if !accepted {
		return
	}
	// Deduplicated on the CLAIM, not on the whole call, and the later one wins: two
	// updates can restate the same branch under different statuses, and it is the
	// most recent status that says whether the work is being handed to review or
	// declared landed.
	for i, prior := range r.Reports {
		if prior.sameClaim(rep) {
			r.Reports[i] = rep
			return
		}
	}
	r.Reports = append(r.Reports, rep)
}

// Report is a delivery claim the plane accepted: the branch a session recorded as
// its hand-off, and/or the commit it declared landed on an integration branch.
//
// Both are claims the plane cannot check — it holds the backlog and takes a
// clanker at its word — and both have been wrong in ways that cost real work: a
// branch recorded but never pushed past its first commits reads as a hand-off to
// the next clanker and is a dead end. The driver is local and in the git tree, so
// it can simply look (CLA-253).
type Report struct {
	// TaskID and Ref identify the task, and RunID signs any write the driver makes
	// off the back of the check.
	TaskID string
	Ref    string
	RunID  string

	// Status is the status the same call carried, if any ("in_review", "done").
	Status string

	// Branch is a recorded work-in-progress branch.
	Branch string

	// Commit and IntegrationBranch are a declared delivery, and PR is the pull
	// request that carried it. PR is captured only so the driver can echo the whole
	// declaration back when it attests to the merge, rather than posting a partial
	// `delivery` object that could drop it.
	Commit            string
	IntegrationBranch string
	PR                string
}

// Empty reports that this claim asserts nothing checkable.
func (r Report) Empty() bool {
	return r.Branch == "" && (r.Commit == "" || r.IntegrationBranch == "")
}

// sameClaim reports whether two reports assert the same thing about the same task,
// ignoring the status the call happened to carry.
func (r Report) sameClaim(o Report) bool {
	return r.TaskID == o.TaskID && r.Branch == o.Branch &&
		r.Commit == o.Commit && r.IntegrationBranch == o.IntegrationBranch
}

// ClaimsMerge reports whether this is a claim that the delivery has LANDED, which
// is the only thing the ancestor check can sensibly be run against.
//
// A hand-off to `in_review` routinely carries the delivery it is proposing, and at
// that moment the commit has by definition not merged — nobody has reviewed it
// yet. Checking it anyway would fire the loudest log line in the feature on the
// happy path and stamp `mergeVerified: false` on a task that is perfectly fine,
// which is how a warning system trains its operator to ignore it.
func (r Report) ClaimsMerge() bool {
	if r.Commit == "" || r.IntegrationBranch == "" {
		return false
	}
	// `done` is the closure that declares it landed; a status-less revision that
	// carries a delivery is making the same declaration on its own.
	return r.Status == "" || r.Status == "done"
}

// Label is the task's most human-readable identifier.
func (r Report) Label() string {
	if r.Ref != "" {
		return r.Ref
	}
	return r.TaskID
}

// Claim is what a session did with the backlog: the task it most recently
// claimed, and whether it let go of that task deliberately.
//
// Only the LATEST claim is tracked, which is the one that matters — a drain
// claims tasks one after another, and every earlier one was necessarily settled
// (or lost to a race) before the next claim was made. An unsettled latest claim
// is exactly the lease that would otherwise expire in silence.
type Claim struct {
	// TaskID is the task's UUID and Ref is its qualified form ("CLA-242"). BOTH
	// are kept because the plane accepts either as `taskId`, so a session that
	// settles its task by ref must not look like a session settling somebody
	// else's — that mistake reverts an in_review task to `ready`.
	TaskID string
	Ref    string
	RunID  string

	// Settled reports that the session moved TaskID to a terminal status of its
	// own accord (done / in_review / parked / blocked). The plane has already
	// released the task, so there is nothing for the driver to hand back.
	Settled bool

	// HasWIP reports that a work-in-progress branch is recorded against the task —
	// either carried in on the claim (a predecessor's work) or recorded by this
	// session after it pushed. It makes the claim UNSAFE to release: see
	// Releasable.
	HasWIP bool
}

// Held reports whether this session ended still holding a task — a claim that
// was made, never settled, and therefore backed by a lease now ticking down with
// nobody heartbeating it.
func (c Claim) Held() bool { return c.TaskID != "" && !c.Settled }

// Releasable reports whether the driver may hand this claim straight back to the
// queue.
//
// A held claim with WIP is deliberately excluded, and the asymmetry is the point.
// With no branch recorded, releasing to `ready` is exactly what the plane's own
// expiry sweep would have done half an hour later, minus the reclaim it would
// have charged — strictly better. With a branch recorded it is strictly WORSE:
// `requiresTakeover` is computed only for an `in_progress` task whose lease has
// died, so moving the task to `ready` throws away the flag that tells the next
// clanker there is pushed work waiting. Letting that lease expire preserves the
// handoff and costs one reclaim; releasing it saves the reclaim and strands the
// work. Until the plane grows a release that keeps the handoff, the driver takes
// the expiry.
func (c Claim) Releasable() bool { return c.Held() && !c.HasWIP }

// Names reports whether id refers to this claim's task. The plane resolves a
// `taskId` argument as either a UUID or a qualified ref, and its ref parsing
// upper-cases the key, so both spellings must match and the ref must match
// case-insensitively.
func (c Claim) Names(id string) bool {
	if id == "" || c.TaskID == "" {
		return false
	}
	return id == c.TaskID || (c.Ref != "" && strings.EqualFold(id, c.Ref))
}

// settlesTask reports whether an update_task carrying this status would release
// the task plane-side.
//
// Stated as "anything but in_progress" rather than as a list of terminal
// statuses, because that is the plane's own rule: `updateStatus` clears the
// holder for every status except `in_progress`. An allowlist here drifts the
// moment a status is added — and a status this side has not heard of would be
// read as "still held", which is the reading that produces a wrong write.
func settlesTask(status string) bool {
	return status != "" && status != "in_progress"
}

// Limit describes a usage/rate-limit state.
type Limit struct {
	Limited bool
	// Stop marks a HARD limit the loop cannot wait out: a budget/credit
	// exhaustion (out of credits, spend-cap or monthly-limit reached) with no
	// rolling-window reset to poll for. The loop stops the run cleanly instead of
	// entering the supervised wait. Zero value (false) keeps the wait-and-poll
	// behaviour for the rolling-window subscription caps that claude/codex hit.
	Stop    bool
	ResetAt time.Time // zero = unknown
	Reason  string
}

// Usage is current window consumption, when a harness can report it headless.
type Usage struct {
	FiveHourUsedPct float64
	WeeklyUsedPct   float64
	ResetAt         time.Time
}

// ErrUsageUnsupported is returned by ReadUsage on harnesses without headless
// quota introspection — which, today, is all of them.
var ErrUsageUnsupported = errors.New("usage introspection not supported by this harness")

var registry = map[string]Adapter{}

// Register adds an adapter to the registry (called from adapter init()s).
func Register(a Adapter) { registry[a.Name()] = a }

// Get resolves an adapter by name.
func Get(name string) (Adapter, error) {
	if a, ok := registry[name]; ok {
		return a, nil
	}
	return nil, fmt.Errorf("unknown harness %q (have: %s)", name, strings.Join(Names(), ", "))
}

// Known reports whether name is a registered harness. It is the single source of
// truth for the accepted set, so config validation can never drift from what the
// registry actually offers (see config.Validate).
func Known(name string) bool {
	_, ok := registry[name]
	return ok
}

// Names returns the registered harness names, sorted — for validation errors and
// flag help that should list exactly what is registered.
func Names() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
