// Package harness abstracts a coding-agent CLI (Claude Code, Codex, ...) behind the small contract
// the loop needs; the fuller adapter rationale is in docs/harness-rationale.md.
package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/events"
)

type Adapter interface {
	Name() string

	Invoke(ctx context.Context, in Invocation) (Result, error)

	DetectLimit(Result) Limit

	IsTransient(Result) bool

	IsUnclassifiedTransient(Result) bool

	// TurnCapped reports whether the session ended because it hit Invocation.MaxTurns, rather than
	// finishing or failing.
	TurnCapped(Result) bool

	// TokenCeilingHit reports whether the adapter itself killed this session because its cumulative
	// usage crossed Invocation.MaxSessionTokens mid-stream.
	TokenCeilingHit(Result) bool

	// WallClockCapped reports whether the adapter itself ended this session because it outlived
	// Invocation.MaxSessionWallClock.
	WallClockCapped(Result) bool

	// ZeroUsageUnknown reports whether the session ended with a FINAL step_finish carrying reason
	// "unknown" and all-zero usage — the quiet-death signature: no error event, no tokens, no cost,
	// nothing produced, which is otherwise indistinguishable from a cheap clean run.
	ZeroUsageUnknown(Result) bool

	Capabilities() Capabilities

	// Diagnostic returns the harness-authored text IsTransient and DetectLimit were judged on —
	// stderr, the CLI's own non-event output, typed error events — with the agent's narration
	// excluded exactly as those scans exclude it.
	Diagnostic(Result) string

	Probe(ctx context.Context, in Invocation) (ProbeResult, error)

	// ReadUsage returns current window usage if the harness exposes it headless.
	ReadUsage(ctx context.Context, in Invocation) (Usage, error)

	// MCPConfigUse declares what this adapter does with Invocation.MCPConfigPath, so a preflight can
	// say something TRUE about the operator's `.mcp.json` instead of inferring one from the field's
	// existence.
	MCPConfigUse() MCPConfigUse
}

type MCPConfigUse struct {
	Schema MCPConfigSchema

	// Note is operator-facing prose naming where this harness's sessions ACTUALLY get their MCP
	// servers.
	Note string
}

type MCPConfigSchema int

const (
	// MCPConfigUnused: the path never reaches the session.
	MCPConfigUnused MCPConfigSchema = iota

	MCPConfigClaudeJSON

	// MCPConfigNative: the path is handed over, but read in the HARNESS'S OWN schema.
	MCPConfigNative
)

// Capabilities are the adapter facts the driver has to branch on, so a feature that silently
// half-works on an adapter that cannot support it is refused up front instead.
type Capabilities struct {
	// TracksClaims reports whether this adapter watches the session's clankerbar tool calls and
	// populates Result.Claim.
	TracksClaims bool

	HonoursMaxTurns bool

	HonoursSessionWallClock bool

	// ReportsCost reports whether this adapter ever populates Result.CostUSD.
	ReportsCost bool

	HasSessionTokenCeiling bool

	// RunsTurnSequences reports whether this adapter can run Invocation.Turns: follow-up messages in
	// the SAME session the first turn created.
	RunsTurnSequences bool

	// EmitsEvents reports whether this adapter normalizes its structured tool events into the agent
	// event channel — the activity tier always, and the transcript tier while its dial is on.
	EmitsEvents bool

	// DeliversMessages reports whether this adapter can receive console -> agent messages.
	DeliversMessages bool

	// ResumesSessions reports whether this adapter can continue an existing session by id
	// (studio-chat.md §3): claude's `--resume <id>` and opencode's `run -s <id>`.
	ResumesSessions bool
}

func CapabilitiesOf(name string) (Capabilities, bool) {
	a, ok := registry[name]
	if !ok {
		return Capabilities{}, false
	}
	return a.Capabilities(), true
}

type Invocation struct {
	Prompt        string
	Model         string
	Routes        []string
	WorkDir       string
	MCPConfigPath string
	ConfigDir     string
	SettingsPath  string
	// ExtraDirs names additional directories the session must be able to read and edit BESIDES its
	// working directory: the project's other declared repos, plus any conventional worktree area
	// beside the spawn checkout (agent-rule-scoping piece 2 — permissions follow the project, not
	// the cwd).
	ExtraDirs []string
	Console   io.Writer
	Env       []string // extra env, appended to the process environment
	// EnvAllowlist names the INHERITED variables this session may keep.
	EnvAllowlist []string

	// Nice is how much LOWER than the daemon this session's processes should be scheduled: 0 (the
	// default) is unchanged, and a positive value runs the harness and everything it forks — test
	// runners, builds, dev servers — at that much worse a priority.
	Nice int

	Probe    bool
	MaxTurns int

	// MaxSessionTokens is the per-session runaway ceiling: when the session's own cumulative usage
	// crosses it, the ADAPTER kills the process mid-stream and marks the Result so the driver treats
	// the end like a turn cap (end the phase, salvage what it left, do not retry or fail).
	MaxSessionTokens int

	// MaxSessionWallClock bounds this ONE session by elapsed time: when it is exceeded the ADAPTER
	// kills the process and marks the Result so the driver treats the end like a turn cap (end the
	// phase, salvage what it left, do not retry or fail).
	MaxSessionWallClock time.Duration

	MaxRepeatedToolFailures int

	// MaxSessionIdle bounds how long a session may go without ANY stream output before the adapter
	// kills it as a silent hang: the adapter kills the process mid-session and records the cause on
	// Result.IdleKilled. 0 disables the watchdog for this invocation; probes never arm it.
	MaxSessionIdle time.Duration

	// MaxToolCallDuration bounds one tool call while MaxSessionIdle's watchdog is armed: a call left
	// open longer than this kills the session with cause "tool call <id> open for <duration>".
	MaxToolCallDuration time.Duration

	ResumeClaim Claim

	// CheckReport is the runner's check of a done report in the session's final message; nil means the
	// phase owes none.
	CheckReport func(finalMessage string) error

	// AllowUnclaimed runs this session without a clankerbar claim.
	AllowUnclaimed bool

	// OnClaim, when non-nil, is called by the adapter's parser the moment it observes the session's
	// clankerbar claim state change: a claim_task whose result carried both ids (the tracked claim),
	// and a settle of that claim. Called on the parser's goroutine; implementations must not block.
	OnClaim func(Claim)

	// Turns is an optional ordered list of follow-up turns to run in the SAME session as the first
	// turn (Prompt/Model): each is sent as a continuation of the session the first turn created, so
	// later turns reuse everything the earlier ones read.
	Turns []Turn

	// NewEventSink, when non-nil, builds this session's agent-event sink.
	NewEventSink func() *events.Emitter

	// Inbox wires the console -> agent message channel for this session.
	Inbox *InboxConfig

	SessionRef string

	// ResumeSession continues Invocation.SessionRef instead of creating it.
	ResumeSession bool
}

// InboxConfig is the plane half of the console -> agent message channel: where the run inbox lives
// and the account key that authorises the poll.
type InboxConfig struct {
	// Origin is the trusted plane origin, e.g. https://clankerbar.com (no path).
	Origin string
	// APIKey is the same account key every other plane write carries.
	APIKey string
	// PollInterval overrides the v1 2 s cadence; tests set a short one.
	PollInterval time.Duration
}

// ModelArg is the alias to put after an adapter's model flag, or "" for "emit no flag at all and
// let the harness pick".
func (in Invocation) ModelArg() string { return strings.TrimSpace(in.Model) }

// RouteList is the ordered, de-duplicated route list a fallback-capable adapter runs through.
func (in Invocation) RouteList() []string {
	if len(in.Routes) == 0 {
		if m := in.ModelArg(); m != "" {
			return []string{m}
		}
		return nil
	}
	out := make([]string, 0, len(in.Routes))
	seen := make(map[string]bool, len(in.Routes))
	for _, route := range in.Routes {
		r := strings.TrimSpace(route)
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}

// OnRoute returns a copy of the invocation pinned to one route of its list, for the resume process
// a fallback starts.
func (in Invocation) OnRoute(route string) Invocation {
	in.Model = strings.TrimSpace(route)
	return in
}

const FinishReasonKey = "finish_reason"

// TerminalReasonKey is the Result.Raw key an adapter uses for its OWN end-of- session marker —
// the wall-clock cap (wallClockReason) or the zero-usage-unknown quiet-death signature
// (ZeroUsageReason).
const TerminalReasonKey = "terminal_reason"

const FinishReasonUnknown = "unknown"

const FinishReasonLength = "length"

const ZeroUsageReason = "zero_usage_unknown"

// RecoveredErrorsKey is the Result.Raw key an adapter uses to record that its session's stream
// carried a typed error event the session then RECOVERED from — it retried and carried on —
// with no error event ending the stream.
const RecoveredErrorsKey = "recovered_error_events"

// RecoveredErrors reports how many typed error events the session's stream RECOVERED from —
// events with further transcript after them, and no error event ending the stream.
func RecoveredErrors(res Result) int {
	if res.Raw == nil {
		return 0
	}
	n, _ := res.Raw[RecoveredErrorsKey].(int)
	return n
}

// OutputCapReason is the terminal_reason the opencode adapter writes into a Result when the
// session's final step_finish carried reason "length" with ZERO output tokens — the CLA-584
// output-cap stall, read back through opencode.OutputCapNoOutput.
const OutputCapReason = "output_cap_no_output"

// OutputCapReasoningKey is the Result.Raw key carrying the stalled final step's OWN reasoning-token
// count, which is what makes the stall legible in a log line — how much thinking bought nothing.
const OutputCapReasoningKey = "output_cap_reasoning"

type TokenUsage struct {
	Input      int // fresh (uncached) prompt tokens
	Output     int // generated tokens, minus any reasoning the harness counts separately
	Reasoning  int // thinking tokens the harness counts as their own bucket
	CacheRead  int // prompt tokens served from a prompt cache
	CacheWrite int // prompt tokens written to a prompt cache
}

// Add folds another run's usage into this one.
func (u *TokenUsage) Add(o TokenUsage) {
	u.Input += o.Input
	u.Output += o.Output
	u.Reasoning += o.Reasoning
	u.CacheRead += o.CacheRead
	u.CacheWrite += o.CacheWrite
}

func (u TokenUsage) Total() int {
	return u.Input + u.Output + u.Reasoning + u.CacheRead + u.CacheWrite
}

type Result struct {
	ExitCode     int
	ExitSignal   int
	Stdout       string
	Stderr       string
	FinalMessage string         // the agent's final message, when parseable
	Tokens       int            // tokens this session consumed (for the Budget)
	CostUSD      float64        // $ this session consumed
	Raw          map[string]any // adapter-specific parsed fields

	SessionRef string

	// SessionMissing reports that this invocation asked to RESUME a session the harness no longer has
	// (a pruned local store), so the planner must re-prime a fresh session from the plane's transcript
	// within the same turn.
	SessionMissing bool

	TurnMessages []string

	// TurnStopped is the 1-based index of the turn whose end stopped a multi-turn sequence, counting
	// the first (Invocation.Prompt) turn as 1. It is 0 when every planned turn ran, which is also the
	// only value a single-turn invocation can have.
	TurnStopped int

	TurnStopReason string

	Usage TokenUsage

	CostReported bool

	// UsageReported says the adapter actually SAW a usage-bearing event on this session's stream, as
	// distinct from Tokens/CostUSD merely being zero.
	UsageReported bool

	// Untrusted, when non-empty, says why this Result's PARSED fields cannot be believed: the child's
	// stream could not be read whole, so an unknown number of events never reached the parser.
	Untrusted string

	ToolLoop string

	// IdleKilled is the cause when the ADAPTER killed this session for silence: "no output for
	// <MaxSessionIdle>", or "tool call <id> open for <MaxToolCallDuration>" when a call was open.
	IdleKilled string

	IdleToolCall bool

	// ReportResumeFailed is the cause when the adapter's bounded re-emission of a refused done report
	// failed: it stalled on a silent provider and was killed by the REPORT RESUME's own short idle
	// bound, its process errored before producing a reply, its reply was still not a report the runner
	// could apply, or the phase's wall clock left no room to resume at all.
	ReportResumeFailed string

	// OutputDropped is how many bytes of this session's output were NOT retained, across both streams
	// — the ordinary cost of the rolling window, not a fault.
	OutputDropped int64

	Claim Claim

	// ClankerbarCalls is how many clankerbar MCP tool calls this session got a SUCCESSFUL result for.
	ClankerbarCalls int

	// CheckpointUnverified marks this Result as the hand-off for a phase that reached its checkpoint
	// on an UNVERIFIED branch: the phase recorded Claim.Branch on the plane, but the driver's read of
	// the origin remote failed (network, a GitHub outage, a credential problem), so it is unknown
	// whether the branch was ever pushed.
	CheckpointUnverified bool

	// CheckpointTip is the origin tip the driver VERIFIED for Claim.Branch at the checkpoint: the
	// remote commit the branch was at when the evidence gate read it.
	CheckpointTip string

	// onClaim is the Invocation.OnClaim callback bound into this Result at construction
	// (newSessionResult), so the shared observer can notify it without threading the Invocation
	// through every parse helper.
	onClaim func(Claim)

	onTurnEnd func()

	Reports []Report

	events *events.Emitter

	// pending maps a tool_use id to what its result will mean.
	pending map[string]pendingKind

	clankerbarCalls map[string]bool

	// pendingReports is the same discipline for delivery claims: a progress or terminal call REFUSED
	// by the plane (a missing Tests header, a superseded run) recorded nothing, so there is nothing to
	// check and nothing to complain about.
	pendingReports map[string]Report

	// claimRequests maps a claim_task tool_use id to the task it ASKED for.
	claimRequests map[string]string

	gotResult bool

	// toolLoop is claude's repeated-failure guard, armed by newSessionResult from the Invocation; nil
	// when the check is off.
	toolLoop  *toolLoopGuard
	toolNames map[string]string

	// envReadCalls names the tool_use ids that read a `.env*` path, so their results can be withheld
	// from the transcript tier.
	envReadCalls map[string]bool

	idle *idleGuard

	maxIdle, maxToolCall time.Duration

	scans *scanCache
}

func (r *Result) noteClaimRequest(toolUseID, taskID string) {
	if toolUseID == "" || taskID == "" {
		return
	}
	if r.claimRequests == nil {
		r.claimRequests = map[string]string{}
	}
	r.claimRequests[toolUseID] = taskID
}

// markUntrusted records why this Result's figures cannot be believed.
func (r *Result) markUntrusted(reason string) {
	if r.Untrusted == "" {
		r.Untrusted = reason
	}
}

func (r Result) ExitString() string {
	if r.ExitSignal != 0 {
		if name := SignalName(r.ExitSignal); name != "" {
			return fmt.Sprintf("killed by %s (signal %d)", name, r.ExitSignal)
		}
		return fmt.Sprintf("killed by signal %d", r.ExitSignal)
	}
	return fmt.Sprintf("exit %d", r.ExitCode)
}

const scanErrorText = 0

type scanCache struct {
	m map[int]string
}

func newScanCache() *scanCache { return &scanCache{m: map[int]string{}} }

// scan returns the text for key, building it on first use.
func (r Result) scan(key int, build func() string) string {
	if r.scans == nil {
		return build()
	}
	if s, ok := r.scans.m[key]; ok {
		return s
	}
	s := build()
	r.scans.m[key] = s
	return s
}

type pendingKind int

const (
	pendingClaim pendingKind = iota
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

// settleReport resolves an armed delivery claim on its tool_result.
func (r *Result) settleReport(toolUseID string, accepted bool) {
	rep, armed := r.pendingReports[toolUseID]
	if !armed {
		return
	}
	delete(r.pendingReports, toolUseID)
	if !accepted {
		return
	}
	for i, prior := range r.Reports {
		if prior.sameClaim(rep) {
			if rep.Repo == "" {
				rep.Repo = prior.Repo
			}
			r.Reports[i] = rep
			return
		}
	}
	r.Reports = append(r.Reports, rep)
}

// Report is a delivery claim the plane accepted: the branch a session recorded as its hand-off,
// and/or the commit it declared landed on an integration branch.
type Report struct {
	TaskID string
	Ref    string
	RunID  string

	// Status is the status the same call carried, if any ("in_review", "done").
	Status string

	// Branch is a recorded work-in-progress branch.
	Branch string

	// Repo is the repository the session declared its work lives in ("owner/name").
	Repo string

	Commit            string
	IntegrationBranch string
	PR                string
}

// Empty reports that this claim asserts nothing checkable.
func (r Report) Empty() bool {
	return r.Branch == "" && (r.Commit == "" || r.IntegrationBranch == "")
}

func (r Report) sameClaim(o Report) bool {
	return r.TaskID == o.TaskID && r.Branch == o.Branch &&
		r.Commit == o.Commit && r.IntegrationBranch == o.IntegrationBranch
}

// ClaimsMerge reports whether this is a claim that the delivery has LANDED, which is the only thing
// the ancestor check can sensibly be run against.
func (r Report) ClaimsMerge() bool {
	if r.Commit == "" || r.IntegrationBranch == "" {
		return false
	}
	return r.Status == "" || r.Status == "done"
}

// Label is the task's most human-readable identifier.
func (r Report) Label() string {
	if r.Ref != "" {
		return r.Ref
	}
	return r.TaskID
}

// Claim is what a session did with the backlog: the task it most recently claimed, and whether it
// let go of that task deliberately.
type Claim struct {
	// TaskID is the task's UUID and Ref is its qualified form.
	TaskID string
	Ref    string
	RunID  string

	Settled bool

	HasWIP bool

	Category string

	// Branch is the work-in-progress branch name recorded on the task (e.g. "clanker/x-...").
	Branch string
}

// Held reports whether this session ended still holding a task — a claim that was made, never
// settled, and therefore backed by a lease now ticking down with nobody heartbeating it.
func (c Claim) Held() bool { return c.TaskID != "" && !c.Settled }

// Releasable reports whether the driver may hand this claim straight back to the queue.
func (c Claim) Releasable() bool { return c.Held() && !c.HasWIP }

// Names reports whether id refers to this claim's task.
func (c Claim) Names(id string) bool {
	if id == "" || c.TaskID == "" {
		return false
	}
	return id == c.TaskID || (c.Ref != "" && strings.EqualFold(id, c.Ref))
}

func (r *Result) notifyClaim() {
	if r.onClaim != nil {
		r.onClaim(r.Claim)
	}
}

// settlesTask reports whether a terminal call carrying this status would release the task
// plane-side.
func settlesTask(status string) bool {
	return status != "" && status != "in_progress"
}

// Limit describes a usage/rate-limit state.
type Limit struct {
	Limited bool
	// Stop marks a HARD limit the loop cannot wait out: a budget/credit exhaustion (out of credits,
	// spend-cap or monthly-limit reached) with no rolling-window reset to poll for.
	Stop    bool
	ResetAt time.Time // zero = unknown
	Reason  string
	// Window is the usage window the limit names, as a human label: "5-hour", "weekly" or "daily".
	Window string
}

// ProbeResult is what a liveness probe learned, and what it cost.
type ProbeResult struct {
	// Limit is what the probe found: still limited, or lifted.
	Limit Limit

	Tokens  int
	CostUSD float64
}

// Usage is current window consumption, when a harness can report it headless.
type Usage struct {
	FiveHourUsedPct float64
	WeeklyUsedPct   float64
	ResetAt         time.Time
}

var ErrUsageUnsupported = errors.New("usage introspection not supported by this harness")

// ErrUntrusted marks a probe whose own output could not be read, so it answered nothing.
var ErrUntrusted = errors.New("untrusted harness output")

// ErrMCPNotConnected marks a session that never reached its first model turn because the clankerbar
// MCP server did not report connected in time.
var ErrMCPNotConnected = errors.New("the clankerbar MCP server did not connect")

func probeVerdict(out ProbeResult, res Result, limit func(Result) Limit) (ProbeResult, error) {
	if res.Untrusted != "" {
		return out, fmt.Errorf("%w: %s", ErrUntrusted, res.Untrusted)
	}
	out.Limit = limit(res)
	return out, nil
}

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

// Known reports whether name is a registered harness.
func Known(name string) bool {
	_, ok := registry[name]
	return ok
}

func Names() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
