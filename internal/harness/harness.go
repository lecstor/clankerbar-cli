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
	// without doing real work — used while paused to catch an early reset. It
	// reports what the probe COST as well as what it found: a probe is a real
	// session against the harness binary, so a wait that polls for a week is real
	// spend and the loop's ceilings have to be able to see it (CLA-287).
	Probe(ctx context.Context, in Invocation) (ProbeResult, error)

	// ReadUsage returns current window usage if the harness exposes it headless.
	// NO harness does today (see the memo), so implementations return
	// ErrUsageUnsupported and the loop falls back to the self-accounted Budget.
	// Kept in the contract because it is the right seam the day one adds it.
	ReadUsage(ctx context.Context, in Invocation) (Usage, error)

	// MCPConfigUse declares what this adapter does with Invocation.MCPConfigPath,
	// so a preflight can say something TRUE about the operator's `.mcp.json`
	// instead of inferring one from the field's existence.
	//
	// It is on the interface, not a lookup table in cli, because a table is not
	// coupled to the registry: the first version of this lived in doctor as a
	// switch with `default: return true`, and registering an adapter that did not
	// carry the config would have silently earned a green workdir with no test
	// failing (CLA-263). Here the compiler asks the question at the moment an
	// adapter is written, which is the only moment the answer is known.
	MCPConfigUse() MCPConfigUse
}

// MCPConfigUse is one adapter's answer about Invocation.MCPConfigPath: whether
// the path reaches the session at all, in which schema it is read if it does,
// and where that harness's sessions really get their MCP servers.
type MCPConfigUse struct {
	Schema MCPConfigSchema

	// Note is operator-facing prose naming where this harness's sessions ACTUALLY
	// get their MCP servers. `doctor` prints it verbatim, so it must read as a
	// sentence and must not assume the reader knows the adapter. Required for
	// every schema except MCPConfigClaudeJSON, where there is nothing surprising
	// to explain.
	Note string
}

// MCPConfigSchema is what a harness makes of the file at Invocation.MCPConfigPath.
//
// The distinction that matters is NOT "does the adapter hand the path over" -
// that is the question doctor used to ask, and opencode answers it yes while
// still dying on the file. It is "does that file MEAN anything to that harness".
type MCPConfigSchema int

const (
	// MCPConfigUnused: the path never reaches the session. The adapter drops the
	// field, so the file on disk is inert - neither its presence nor its absence
	// tells an operator anything about the tools their sessions will have. codex
	// is this: no per-run MCP flag, servers come from config.toml under CODEX_HOME.
	MCPConfigUnused MCPConfigSchema = iota

	// MCPConfigClaudeJSON: the path is handed over and read as Claude Code's
	// `.mcp.json` - servers under `mcpServers`. This is the only case where a
	// present-and-Claude-shaped file really does give the session its clankerbar
	// tools, and so the only case where checking for one is a true check.
	MCPConfigClaudeJSON

	// MCPConfigNative: the path is handed over, but read in the HARNESS'S OWN
	// schema. A Claude-shaped `.mcp.json` here is not merely useless, it is a
	// config error the harness refuses to start on - so pointing this harness at
	// one is worse than pointing it at nothing. opencode is this: the path becomes
	// OPENCODE_CONFIG, whose servers live under `mcp`, and opencode rejects an
	// unrecognised top-level key outright.
	MCPConfigNative
)

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
//
// Stdout and Stderr are the retained TAIL of each stream, not the whole of it: a
// session's output is unbounded and the supervisor has to survive it (see
// output.go). Everything parsed here is taken from the stream as it arrives, so
// the trimming costs the classifiers context they do not use — unless a single
// line overran maxStreamLine, which is what Untrusted is for.
type Result struct {
	ExitCode     int
	Stdout       string
	Stderr       string
	FinalMessage string         // the agent's final message, when parseable
	Tokens       int            // tokens this session consumed (for the Budget)
	CostUSD      float64        // $ this session consumed
	Raw          map[string]any // adapter-specific parsed fields

	// Untrusted, when non-empty, says why this Result's PARSED fields cannot be
	// believed: the child's stream could not be read whole, so an unknown number
	// of events never reached the parser.
	//
	// It is a claim about the FIGURES, not about the child. Everything here is
	// derived from events, and the ones that matter most arrive last: the `result`
	// event carries the whole session's tokens and cost, so a stream cut short
	// reports ZERO SPEND for a session that may have cost hundreds of dollars, and
	// the settle of a claim never observed leaves Claim.Held() true on a task the
	// session actually handed to review. Both are wrong decisions rather than
	// missing ones, which is why this is a field and not a log line: a caller acting
	// on the numbers has to be able to see that they are not answers.
	//
	// The driver's contract, in loop.drainWithRetries: do not count the spend, do
	// not release the claim, do not classify the exit. See CLA-262.
	Untrusted string

	// OutputDropped is how many bytes of this session's output were NOT retained,
	// across both streams — the ordinary cost of the rolling window, not a fault.
	//
	// Worth carrying because it is the difference between "the classifier read this
	// session" and "the classifier read the last couple of MiB of it". A run that
	// stops on a non-retryable exit says which message it stopped on; without this
	// the operator cannot tell whether the message they are being shown was the only
	// candidate or merely the last one to survive the window.
	OutputDropped int64

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

	// scans memoizes the harness-authored text a classifier reads. See scan.
	scans *scanCache
}

// markUntrusted records why this Result's figures cannot be believed. The FIRST
// reason wins: the earliest thing that went wrong is the one that explains
// everything after it.
func (r *Result) markUntrusted(reason string) {
	if r.Untrusted == "" {
		r.Untrusted = reason
	}
}

// scanErrorText is the scan key for the adapters with a single classification
// width (codex, opencode). claude has two and keys on its own claudeScope — whose
// narrow width happens to share this number, which is harmless for the reason
// Result.scan gives: one Result comes from one adapter.
const scanErrorText = 0

// scanCache holds the scoped text a classifier scans, built once per session
// instead of once per call.
//
// DetectLimit, IsTransient and Diagnostic are each handed the whole Result and
// each used to rebuild the same scoped copy of it, three times per session for
// text that never changes. Bounding the retained output (output.go) already turned
// that from hundreds of megabytes into a couple, and this makes it once.
//
// Not safe for concurrent use, and it does not need to be: one Result belongs to
// one finished session, and the driver classifies it in sequence on the goroutine
// that ran it.
type scanCache struct {
	m map[int]string
}

func newScanCache() *scanCache { return &scanCache{m: map[int]string{}} }

// scan returns the text for key, building it on first use.
//
// A POINTER on Result, deliberately: every Adapter classification method takes
// Result BY VALUE, so a cache stored inline would be filled in on a copy and
// thrown away. Nil is a fully working, simply uncached Result — which is what a
// hand-built literal in a test is, and why nothing may depend on the memo being
// there. `key` is the adapter's own notion of scope (claude has two widths; codex
// and opencode have one), and a Result only ever comes from ONE adapter, so the
// key spaces cannot collide.
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

// ProbeResult is what a liveness probe learned, and what it cost.
//
// The cost half is the reason this type exists at all. Every adapter implements
// Probe as Invoke-then-DetectLimit, so each poll is a genuine paid session — cheap
// (the prompt is "." with tools denied) but not free, and a cap that lasts a week
// polled every 30 minutes is ~336 of them. Returning only the Limit threw the spend
// away, so the one loop that cannot reach the caller — the supervised wait, which
// polls for as long as the cap lasts — was spending money no ceiling could count
// (CLA-287).
type ProbeResult struct {
	// Limit is what the probe found: still limited, or lifted.
	Limit Limit

	// Tokens and CostUSD are this probe session's spend, for the same Budget
	// accumulator an ordinary session's Result feeds.
	//
	// Carried on the error return too, so the loop counts whatever the adapter
	// managed to parse rather than discarding a session that ended untidily. Note
	// what that is worth TODAY, which changed with CLA-262: codex and opencode now
	// parse as the stream arrives, so a session that emitted its usage and then died
	// in Wait carries that spend out through the error return. claude's DRAIN path
	// always did (it streams). claude's PROBE path still returns before parsing, so
	// it is zero there — the remaining half of CLA-299, and the accumulator here is
	// ready for it. A non-zero EXIT is not that path: it parses normally either way.
	Tokens  int
	CostUSD float64
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

// ErrUntrusted marks a probe whose own output could not be read, so it answered
// nothing. Distinct from an ordinary probe failure — which the loop waits out,
// because a network blip clears itself — because this one will not clear: it says
// the harness's output is unreadable, and a wait that keeps polling on it is
// spending money no ceiling can see. loop.supervisedWait tells them apart.
var ErrUntrusted = errors.New("untrusted harness output")

// probeVerdict turns a finished probe session into what the caller may act on.
//
// A probe exists to answer one question — am I still limited? — and the supervised
// wait resumes spending real sessions on a "no". So an UNTRUSTED probe must not
// answer it: a Limit{} read out of output the adapter could not read whole is
// indistinguishable from a lifted cap, and acting on it resumes the run on a
// reading the drain path would have refused (CLA-262).
//
// It comes back as an error rather than as a third state because the loop already
// waits another interval on one, which is exactly "I still do not know" — but it
// wraps ErrUntrusted, because "I cannot read this harness" does not clear itself
// the way a network blip does, and a wait that polls on it forever is unaccounted
// spend. The spend is on `out` regardless, since a probe that could not be read
// still cost what it cost.
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
