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

	// IsUnclassifiedTransient reports that IsTransient said yes on a HEURISTIC rather than on a
	// recognised pattern — "this exit looks more like a blip than a stop" — so a caller can bound
	// those retries separately. A caller cannot bound what it cannot distinguish, and both used to
	// arrive as a bare `true`.
	IsUnclassifiedTransient(Result) bool

	TurnCapped(Result) bool

	TokenCeilingHit(Result) bool

	// WallClockCapped reports whether the adapter itself ended this session because it outlived
	// Invocation.MaxSessionWallClock. The two capabilities are independent on purpose: the day an
	// adapter has both, this same path hands it the checkpoint too - exactly the shipped present since
	// CLA-365, when opencode gained claim observation.
	WallClockCapped(Result) bool

	// ZeroUsageUnknown reports whether the session ended with a FINAL step_finish carrying reason
	// "unknown" and all-zero usage — the quiet-death signature: no error event, no tokens, no cost,
	// nothing produced, which is otherwise indistinguishable from a cheap clean run. The marker is the
	// adapter's own (a terminal_reason it writes, never text the CLI or an agent could emit), so it
	// exists so the driver can NAME the end instead of logging "iteration done (tokens=0 cost=$0.00)".
	ZeroUsageUnknown(Result) bool

	Capabilities() Capabilities

	// Diagnostic returns the harness-authored text IsTransient and DetectLimit were judged on —
	// stderr, the CLI's own non-event output, typed error events — with the agent's narration
	// excluded exactly as those scans exclude it. Implementations must return the same scope
	// IsTransient reads, never a wider one.
	Diagnostic(Result) string

	Probe(ctx context.Context, in Invocation) (ProbeResult, error)

	ReadUsage(ctx context.Context, in Invocation) (Usage, error)

	// MCPConfigUse declares what this adapter does with Invocation.MCPConfigPath, so a preflight can
	// say something TRUE about the operator's `.mcp.json` instead of inferring one from the field's
	// existence. Here the compiler asks the question at the moment an adapter is written, which is the
	// only moment the answer is known.
	MCPConfigUse() MCPConfigUse
}

type MCPConfigUse struct {
	Schema MCPConfigSchema

	// Note is operator-facing prose naming where this harness's sessions ACTUALLY get their MCP
	// servers. `doctor` prints it verbatim, so it must read as a sentence and must not assume the
	// reader knows the adapter.
	Note string
}

type MCPConfigSchema int

const (
	// MCPConfigUnused: the path never reaches the session.
	MCPConfigUnused MCPConfigSchema = iota

	MCPConfigClaudeJSON

	// MCPConfigNative: the path is handed over, but read in the HARNESS'S OWN schema. A Claude-shaped
	// `.mcp.json` here is not merely useless, it is a config error the harness refuses to start on -
	// so pointing this harness at one is worse than pointing it at nothing.
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

	ReportsCost bool

	// HasSessionTokenCeiling reports whether this adapter enforces Invocation.MaxSessionTokens
	// mid-session - i.e. whether TokenCeilingHit can ever fire.
	HasSessionTokenCeiling bool

	// RunsTurnSequences reports whether this adapter can run Invocation.Turns: follow-up messages in
	// the SAME session the first turn created.
	RunsTurnSequences bool

	// EmitsEvents reports whether this adapter normalizes its structured tool events into the agent
	// event channel — the activity tier always, and the transcript tier while its dial is on. False
	// means the loop must not build a sink: there is no mapping, and an unclosed sink would leak its
	// batch goroutine.
	EmitsEvents bool

	// DeliversMessages reports whether this adapter can receive console -> agent messages. The loop
	// sets Invocation.Inbox only when this is true.
	DeliversMessages bool

	// ResumesSessions reports whether this adapter can continue an existing session by id
	// (studio-chat.md §3): claude's `--resume <id>` and opencode's `run -s <id>`. False means a
	// conversation cannot keep one session across turns, so `doctor` says a planner on this harness
	// re-primes every turn.
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
	ExtraDirs     []string
	Console       io.Writer
	Env           []string // extra env, appended to the process environment
	EnvAllowlist  []string

	// Nice is how much LOWER than the daemon this session's processes should be scheduled: 0 (the
	// default) is unchanged, and a positive value runs the harness and everything it forks — test
	// runners, builds, dev servers — at that much worse a priority. Only a supported platform
	// accepts a positive value (SessionPrioritySupported); adapters go through sessionCommand, which
	// applies it at spawn so the whole tree inherits it.
	Nice int

	Probe    bool
	MaxTurns int

	// MaxSessionTokens is the per-session runaway ceiling: when the session's own cumulative usage
	// crosses it, the ADAPTER kills the process mid-stream and marks the Result so the driver treats
	// the end like a turn cap (end the phase, salvage what it left, do not retry or fail). The driver
	// resolves it from Budget.SessionTokenCeiling, so an unset value never reaches here.
	MaxSessionTokens int

	// MaxSessionWallClock bounds this ONE session by elapsed time: when it is exceeded the ADAPTER
	// kills the process and marks the Result so the driver treats the end like a turn cap (end the
	// phase, salvage what it left, do not retry or fail). Only an adapter reporting
	// Capabilities.HonoursSessionWallClock enforces it; opencode and copilot are those adapters today.
	MaxSessionWallClock time.Duration

	MaxRepeatedToolFailures int

	MaxSessionIdle time.Duration

	MaxToolCallDuration time.Duration

	ResumeClaim Claim

	CheckReport func(finalMessage string) error

	// AllowUnclaimed runs this session without a clankerbar claim.
	AllowUnclaimed bool

	OnClaim func(Claim)

	// Turns is an optional ordered list of follow-up turns to run in the SAME session as the first
	// turn (Prompt/Model): each is sent as a continuation of the session the first turn created, so
	// later turns reuse everything the earlier ones read. Only an adapter that can continue a live
	// session may honour it; one that cannot must REFUSE an invocation with Turns rather than silently
	// drop the later turns, because a half-run sequence is a wrong answer, not a shorter one.
	Turns []Turn

	// NewEventSink, when non-nil, builds this session's agent-event sink.
	NewEventSink func() *events.Emitter

	// Inbox wires the console -> agent message channel for this session. The loop sets it only for an
	// adapter whose Capabilities.DeliversMessages is true and only when the plane origin and key are
	// configured.
	Inbox *InboxConfig

	SessionRef string

	// ResumeSession continues Invocation.SessionRef instead of creating it. An adapter that cannot
	// resume must not silently start fresh: claude reports the missing session on
	// Result.SessionMissing, and the planner re-primes within the same turn.
	ResumeSession bool
}

type InboxConfig struct {
	Origin       string
	APIKey       string
	PollInterval time.Duration
}

func (in Invocation) ModelArg() string { return strings.TrimSpace(in.Model) }

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
// a fallback starts. Set the whole field on the COPY rather than threading a model argument through
// every argv builder: the builders keep reading ModelArg, so a route switch cannot grow a second
// dialect for `--model`.
func (in Invocation) OnRoute(route string) Invocation {
	in.Model = strings.TrimSpace(route)
	return in
}

const FinishReasonKey = "finish_reason"

// TerminalReasonKey is the Result.Raw key an adapter uses for its OWN end-of- session marker —
// the wall-clock cap (wallClockReason) or the zero-usage-unknown quiet-death signature
// (ZeroUsageReason). The value is always adapter-authored, never text the CLI or an agent could
// emit, so the driver can read it by name.
const TerminalReasonKey = "terminal_reason"

const FinishReasonUnknown = "unknown"

const FinishReasonLength = "length"

const ZeroUsageReason = "zero_usage_unknown"

// RecoveredErrorsKey is the Result.Raw key an adapter uses to record that its session's stream
// carried a typed error event the session then RECOVERED from — it retried and carried on —
// with no error event ending the stream. The loop reads it off Raw rather than growing Adapter, the
// same way it reads FinishReasonKey: it is the adapter-to-driver channel, and only opencode emits
// this signal today.
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
// output-cap stall, read back through opencode.OutputCapNoOutput. The driver names the end with it
// in place of its generic "never moved the task on" line.
const OutputCapReason = "output_cap_no_output"

const OutputCapReasoningKey = "output_cap_reasoning"

type TokenUsage struct {
	Input      int // fresh (uncached) prompt tokens
	Output     int // generated tokens, minus any reasoning the harness counts separately
	Reasoning  int // thinking tokens the harness counts as their own bucket
	CacheRead  int // prompt tokens served from a prompt cache
	CacheWrite int // prompt tokens written to a prompt cache
}

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

	// CostReported says the adapter actually SAW a cost figure for this session, as distinct from
	// CostUSD merely being zero — the same question UsageReported answers for tokens, and NOT the
	// same event. claude is why this is its own field: it reports per-turn TOKENS on the stream but
	// the session's DOLLARS only on its terminal result event, so a claude session killed mid-stream
	// (the token ceiling, the turn cap) has UsageReported true with CostUSD still 0 — a zero nobody
	// measured.
	CostReported bool

	// UsageReported says the adapter actually SAW a usage-bearing event on this session's stream, as
	// distinct from Tokens/CostUSD merely being zero. A session killed before the harness reports
	// anything leaves Tokens and CostUSD at zero, so it contributes nothing to the Budget - and a
	// retry ladder made of such attempts can never reach a token or cost ceiling, whatever the
	// operator set.
	UsageReported bool

	// Untrusted, when non-empty, says why this Result's PARSED fields cannot be believed: the child's
	// stream could not be read whole, so an unknown number of events never reached the parser.
	Untrusted string

	ToolLoop string

	IdleKilled string

	IdleToolCall bool

	// ReportResumeFailed is the cause when the adapter's bounded re-emission of a refused done report
	// failed: it stalled on a silent provider and was killed by the REPORT RESUME's own short idle
	// bound, its process errored before producing a reply, its reply was still not a report the runner
	// could apply, or the phase's wall clock left no room to resume at all. The adapter therefore
	// leaves the original process outcome in place and sets this marker instead, so the driver can
	// tell a resume-only failure from a dead session and retry the phase inside the claim it still
	// holds, rather than ending the lease and paying a reclaim.
	ReportResumeFailed string

	OutputDropped int64

	Claim Claim

	// ClankerbarCalls is how many clankerbar MCP tool calls this session got a SUCCESSFUL result for.
	// A refused call (the tool was not connected yet, the plane rejected it) does not count: it says
	// nothing about whether the backlog was reachable, and the driver's no-MCP classification is
	// exactly the question "was the backlog reachable at all?".
	ClankerbarCalls int

	// CheckpointUnverified marks this Result as the hand-off for a phase that reached its checkpoint
	// on an UNVERIFIED branch: the phase recorded Claim.Branch on the plane, but the driver's read of
	// the origin remote failed (network, a GitHub outage, a credential problem), so it is unknown
	// whether the branch was ever pushed. The branch is still carried — the review phase is the
	// backstop — but the successor's brief must not describe it as verified on the origin remote,
	// and the review session is told to verify the hand-off itself.
	CheckpointUnverified bool

	// CheckpointTip is the origin tip the driver VERIFIED for Claim.Branch at the checkpoint: the
	// remote commit the branch was at when the evidence gate read it. The review phase's brief carries
	// it as the tip the reviewer must check out and record, and the durable-record seam gate matches
	// the record's reviewedTip against it.
	CheckpointTip string

	onClaim func(Claim)

	onTurnEnd func()

	Reports []Report

	events *events.Emitter

	pending map[string]pendingKind

	clankerbarCalls map[string]bool

	pendingReports map[string]Report

	claimRequests map[string]string

	gotResult bool

	toolLoop  *toolLoopGuard
	toolNames map[string]string

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

func (r *Result) expectReport(toolUseID string, rep Report) {
	if toolUseID == "" || rep.Empty() {
		return
	}
	if r.pendingReports == nil {
		r.pendingReports = map[string]Report{}
	}
	r.pendingReports[toolUseID] = rep
}

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
// and/or the commit it declared landed on an integration branch. Both are claims the plane cannot
// check — it holds the backlog and takes a clanker at its word — and both have been wrong in
// ways that cost real work: a branch recorded but never pushed past its first commits reads as a
// hand-off to the next clanker and is a dead end.
type Report struct {
	TaskID string
	Ref    string
	RunID  string

	Status string

	Branch string

	Repo string

	Commit            string
	IntegrationBranch string
	PR                string
}

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

func (r Report) Label() string {
	if r.Ref != "" {
		return r.Ref
	}
	return r.TaskID
}

// Claim is what a session did with the backlog: the task it most recently claimed, and whether it
// let go of that task deliberately. Only the LATEST claim is tracked, which is the one that matters
// — a drain claims tasks one after another, and every earlier one was necessarily settled (or
// lost to a race) before the next claim was made.
type Claim struct {
	// TaskID is the task's UUID and Ref is its qualified form. BOTH are kept because the plane accepts
	// either as `taskId`, so a session that settles its task by ref must not look like a session
	// settling somebody else's — that mistake reverts an in_review task to `ready`.
	TaskID string
	Ref    string
	RunID  string

	Settled bool

	HasWIP bool

	Category string

	Branch string
}

// Held reports whether this session ended still holding a task — a claim that was made, never
// settled, and therefore backed by a lease now ticking down with nobody heartbeating it.
func (c Claim) Held() bool { return c.TaskID != "" && !c.Settled }

// Releasable reports whether the driver may hand this claim straight back to the queue. A held
// claim with WIP is deliberately excluded, and the asymmetry is the point.
func (c Claim) Releasable() bool { return c.Held() && !c.HasWIP }

// Names reports whether id refers to this claim's task. The plane resolves a `taskId` argument as
// either a UUID or a qualified ref, and its ref parsing upper-cases the key, so both spellings must
// match and the ref must match case-insensitively.
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

func settlesTask(status string) bool {
	return status != "" && status != "in_progress"
}

type Limit struct {
	Limited bool
	// Stop marks a HARD limit the loop cannot wait out: a budget/credit exhaustion (out of credits,
	// spend-cap or monthly-limit reached) with no rolling-window reset to poll for.
	Stop    bool
	ResetAt time.Time // zero = unknown
	Reason  string
	Window  string
}

// ProbeResult is what a liveness probe learned, and what it cost. Returning only the Limit threw
// the spend away, so the one loop that cannot reach the caller — the supervised wait, which polls
// for as long as the cap lasts — was spending money no ceiling could count.
type ProbeResult struct {
	Limit Limit

	Tokens  int
	CostUSD float64
}

type Usage struct {
	FiveHourUsedPct float64
	WeeklyUsedPct   float64
	ResetAt         time.Time
}

var ErrUsageUnsupported = errors.New("usage introspection not supported by this harness")

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

func Register(a Adapter) { registry[a.Name()] = a }

func Get(name string) (Adapter, error) {
	if a, ok := registry[name]; ok {
		return a, nil
	}
	return nil, fmt.Errorf("unknown harness %q (have: %s)", name, strings.Join(Names(), ", "))
}

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
