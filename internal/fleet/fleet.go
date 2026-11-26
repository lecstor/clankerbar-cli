// Package fleet reports a daemon's activity to the plane: presence (one
// upserted row per instance, refreshed on every backlog poll) and iteration
// history (one append-only row per drain, posted at the boundary the loop just
// crossed). It is the CLI half of CLA-466; the plane half (CLA-465) stores and
// renders it on the console's Fleet page.
//
// The one design rule everything here serves: reporting is TELEMETRY, never
// control flow. A failed or slow report is logged once and dropped; it must
// never block, delay, or fail the loop, a claim, or a phase. So Send enqueues
// onto a bounded channel and returns immediately — a wedged endpoint cannot
// slow the poll it rode in on — and a single background pump does the POSTs, so
// reports arrive in order and the plane's last-writer-wins presence row is
// always the newest thing this daemon said.
//
// There is deliberately no retry queue in v1. Presence is corrected by the next
// poll (at most one idle interval later); an iteration record lost to an outage
// is gone, which is the honest price of keeping telemetry off the loop's critical
// path.
package fleet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/secureurl"
)

// The presence states a daemon can report. They are the plane's vocabulary
// (@clankerbar/shared INSTANCE_STATES) verbatim — the plane is a pipe, not a
// judge, so the CLI owns the mapping from what its loop is doing to these:
//
//   - idle      alive and polling, holding nothing, spawning normally
//   - iteration mid-drain: n is the 1-based iteration number, taskRef the task
//     being worked (empty until a claim has been observed), phase the label of
//     the phase session about to spawn or running
//   - waiting   alive and polling, with a blocking question OF ITS OWN standing
//     unanswered (CLA-527/CLA-535): the plane set the task `blocked` and the
//     operator is the only one who can move it. taskRef names the task whose
//     answer is outstanding. Distinct from idle on purpose — this is the one
//     quiet state that needs a human.
//   - draining  alive but not spawning new sessions (console pause, fleet pause,
//     startup-failure pause, budget ceiling, scheduled pause window). It carries
//     the one cause the operator has to act on (State.PauseReason), and the
//     schedule alone adds the window's end (PauseUntil).
//   - stopping  shutting down on purpose (STOP/HALT, a marker restart, budget or
//     max-iterations reached, signal); sent as the final beacon
//   - limited   parked by a harness usage limit and waiting it out (CLA-728):
//     the daemon is healthy, it simply cannot spend anything until the cap
//     lifts. ResetAt carries the instant the harness stated, when it stated
//     one, so the console can say which moment it is waiting for instead of
//     showing the instance as dead.
const (
	StateIdle      = "idle"
	StateIteration = "iteration"
	StateWaiting   = "waiting"
	StateDraining  = "draining"
	StateStopping  = "stopping"
	StateLimited   = "limited"
)

// How one iteration ended — the driver's own words for how its loop step
// finished, mapped from what the loop already knows at the drain boundary
// (@clankerbar/shared ITERATION_OUTCOMES):
//
//   - checkpoint the drain ended with pushed work recorded and the lease left
//     for a successor session to take over
//   - released   the claim came back to the queue (handed back by the driver, or
//     consumed by the session settling the task itself)
//   - parked     the dead-phase retry budget parked the task for the operator
//   - dead       the drain died: a dead phase ended it, or it failed outright
const (
	OutcomeCheckpoint = "checkpoint"
	OutcomeReleased   = "released"
	OutcomeParked     = "parked"
	OutcomeDead       = "dead"
)

// Per-session causes that are not an outcome of the phase's own work. They ride
// the session entry so the durable phase_session record can say WHY a session
// that ran produced nothing, and analytics can exclude an infrastructure miss
// from review-quality numbers (CLA-703).
const (
	// CauseNoMCP: the session made no successful clankerbar MCP call and wrote
	// no review record — the backlog tools were not available to it.
	CauseNoMCP = "no_mcp"
)

// The draining state's stated causes (CLA-908): why spawns are closed, in the
// token the console renders as "paused by <reason>". The set is closed — the
// beacon carries ONE reason, and the driver reports the cause the operator has
// to act on when several apply at once.
const (
	// PauseReasonSchedule: an operator-configured pause window (`pause_windows`).
	// The only reason that also carries a resume instant, and the only one the
	// console raises its fleet-wide pause banner for.
	PauseReasonSchedule = "schedule"
	// PauseReasonConsole: the project is paused from the web console.
	PauseReasonConsole = "console"
	// PauseReasonFleet: the fleet counter tripped — a dead-phase question waits
	// for the operator's answer.
	PauseReasonFleet = "fleet"
	// PauseReasonStartup: repeated harness startup failures stopped the target
	// until the operator answers the harness question.
	PauseReasonStartup = "startup"
	// PauseReasonBudget: the target's own budget ceiling was reached.
	PauseReasonBudget = "budget"
)

// The per-route hold reasons a paused multi-route daemon reports (CLA-1026), the
// plane's vocabulary (ROUTE_HOLD_REASONS) verbatim: a usage limit is quota the
// provider reported, a window is the CLI's own pricing schedule. The console
// keeps them distinct so "why isn't it using the other subscriptions?" has an
// answer.
const (
	// RouteHoldUsageLimit: the route carries an active usage-limit mark.
	RouteHoldUsageLimit = "usage_limit"
	// RouteHoldWindow: the route is inside its pricing/availability window.
	RouteHoldWindow = "window"
)

// RouteHold is one configured route a paused daemon cannot start a session on,
// and why (CLA-1026). It mirrors the plane's RouteHold: the route's own
// `provider/model` name, one closed reason, and — for a window — the schedule
// that holds it. Until is zero when the hold has no stated end.
type RouteHold struct {
	Route    string
	Reason   string    // one of RouteHoldUsageLimit / RouteHoldWindow
	Schedule string    // the pricing schedule's name, only with Reason == RouteHoldWindow
	Until    time.Time // when the hold is expected to lift; zero means unstated
}

// State is the presence half of a report: what the daemon is doing right now.
type State struct {
	Kind    string // one of the State* constants
	N       int    // iteration number, when Kind is iteration
	TaskRef string // task being worked ("CLA-466"), when known; the task whose answer is outstanding on waiting; the HELD task on a schedule pause
	Phase   string // phase label, when Kind is iteration, or the held phase on a schedule pause
	// Model is the route the running iteration's session is actually on
	// (`provider/model`), only meaningful on Kind == iteration (CLA-1026). It is
	// the phase's resolved starting route, updated when a mid-session route
	// fallback switches. Empty when no route was resolved; the plane then shows
	// nothing rather than a guessed name.
	Model string
	// Routes is the per-route hold breakdown of a paused multi-route daemon
	// (CLA-1026), only meaningful on Kind == limited or a route-window draining.
	// Every configured route held right now appears once, in configured order.
	Routes []RouteHold
	// ResetAt is the harness's stated limit-reset instant, only meaningful on
	// Kind == limited (CLA-728). The zero value means the harness stated no
	// reset — the console then says "probing for a reset" rather than guessing
	// a moment it was never told.
	ResetAt time.Time

	// PauseReason/PauseUntil/PauseZone are the draining state's pause payload
	// (CLA-908): why spawns are closed, and — for the schedule, the only cause
	// that states one — until when. A beacon carries ONE reason: the cause the
	// operator has to act on when several apply (see the PauseReason*
	// constants).
	//
	// PauseZone is the schedule's IANA zone. It is LOCAL render metadata only
	// (the supervisor prints the resume time in the operator's own zone) and is
	// deliberately absent from the wire: the plane receives an instant with an
	// explicit offset, which is all it needs.
	PauseReason string
	PauseUntil  time.Time
	PauseZone   string
}

// Usage is one session's token accounting, normalised across harnesses: the
// same five DISJOINT buckets the CLI's adapters fill (harness.TokenUsage),
// carried in the wire shape so the durable phase_session record is repriceable
// later without knowing which harness ran the session (CLA-691).
//
// Kept as this package's own type rather than importing the harness one, the way
// every other field of Session is: the loop is the boundary that translates. The
// two are structurally identical and the loop converts with a struct conversion,
// so a bucket added on one side and not the other stops compiling rather than
// drifting.
type TokenUsage struct {
	Input      int
	Output     int
	Reasoning  int
	CacheRead  int
	CacheWrite int
}

// Session is one harness session an iteration spawned: which phase it ran,
// which harness ran it, the model it was spawned with (CLA-630), and what it
// spent (CLA-691). The phase/harness/model values are exactly what the loop's
// spawn line prints — the phase label, the phase's resolved harness, and the
// resolved model after tier resolution and any run-config overlay. Model is the
// EMPTY string when no model was resolved (the harness default applied); the
// plane stores that as "not resolved", never a guessed name, so an empty value
// must ride as empty rather than be filled in.
type Session struct {
	Phase   string // phase label the session ran under
	Harness string // the harness that ran it
	Model   string // resolved model id; "" when the harness default applied

	// Usage is the session's normalised token breakdown, and UsageReported says
	// the harness reported usage AT ALL — the same distinction Result carries,
	// because a session killed before its harness said anything leaves every
	// bucket zero and that zero is not an answer. The plane records both.
	//
	// A session that spanned a stall continuation (CLA-584) or a resurrection
	// (CLA-406) reports its parts SUMMED here, because the CLI reports it as one
	// session.
	Usage         TokenUsage
	UsageReported bool

	// Cause names a non-work reason this session produced nothing, using the
	// Cause* vocabulary above. Empty for an ordinary session. It is carried on
	// the session (not the iteration) because one reviewer can be an
	// infrastructure miss while the rest of the panel and the fix run normally
	// (CLA-703).
	Cause string `json:"cause,omitempty"`

	// RunID is the run's UUID the session worked the task under, when there is
	// one — empty for a session that never held a claim. It is deliberately NOT
	// the run credential the session holds: that string is `<uuid>.<secret>` and
	// the secret half authorises a write as that run, so telemetry the plane
	// stores and serves back must not carry it (the loop's runUUID splits it off).
	//
	// It is what makes a durable per-session record join EXACTLY to the per-run
	// records already on the plane: a review record carries the run that wrote it
	// (task_review.run_id, itself a bare UUID), so "which model raised these
	// findings, on what did it spend" is answerable per ROUND rather than only per
	// task and phase label — a re-review runs the same phase labels again, so the
	// label alone cannot tell two rounds apart.
	//
	// A session's own claim carries its run id. A later phase's session never
	// calls claim_task (it heartbeats the run the predecessor left held), so the
	// driver supplies the id it already wrote into that session's brief — the
	// same value, from the same source.
	RunID string

	// CostUSD is the harness-reported dollar figure for this session, or nil
	// when there is no figure to carry: the adapter never reports cost at all
	// (Capabilities.ReportsCost false — codex and copilot today), or this session reported
	// no usage, so a zero would claim a spend nobody measured. A harness that
	// DID report zero is a zero here, which is why it is a pointer.
	//
	// Labelled as harness-reported on purpose: on a Claude subscription this is
	// the API-rate equivalent, not what the operator paid, and the operator's
	// own view is that the token breakdown above is the durable half because
	// prices change.
	CostUSD *float64
}

// Iteration is one finished loop step: what ran, how it ended, what it cost.
type Iteration struct {
	TaskID          string    // task UUID, when a claim was observed
	TaskRef         string    // qualified ref ("CLA-466"), when known
	Phases          []string  // distinct phase labels attempted, in order
	Sessions        []Session // one per session the drain spawned, in spawn order
	Outcome         string    // one of the Outcome* constants
	DurationSeconds float64   // wall clock for the whole drain
	Tokens          int       // the drain's total reported token spend
	// CostUSD is the drain's total harness-reported dollar spend (CLA-677),
	// from the same accumulator the budget breaker reads. It is always a
	// figure for an iteration that ran: zero means the harnesses reported no
	// cost, and the plane stores the number rather than reading absence as
	// unknown.
	CostUSD float64
}

// Identity is what every beacon carries about THIS daemon: who and where it is,
// what build it runs, and which config is in force. Identity is composed per
// report by the caller (the loop), not baked into the reporter, because one of
// its fields — the config identity — changes when a RELOAD swaps the config.
type Identity struct {
	Instance       string // the daemon's name; required
	Host           string // hostname
	Version        string // binary version
	ConfigIdentity string // fingerprint of the effective config in force
	// Role is the role this instance serves (studio-chat.md §2): "worker" or
	// "planner". It is a machine fact, reported on every beacon so the console
	// can render the marker and count planners from presence. An empty value is
	// omitted from the wire, which an older plane reads exactly as before.
	Role string
}

// Report is one POST body: identity + current state + any iterations that
// finished since the last report.
type Report struct {
	Identity
	State      State
	Iterations []Iteration
}

// Reporter sends activity reports. Both methods are fail-soft by contract:
// Send returns at once, Close returns by its context's deadline at the latest,
// and neither returns an error or may be load-bearing for the caller.
type Reporter interface {
	// Send enqueues one report and returns at once. Under an unresponsive
	// endpoint the queue fills and the report is dropped (counted, mentioned in
	// the next line the reporter logs) rather than awaited. After Close it is a
	// no-op.
	Send(r Report)
	// Close ends reporting. Everything still queued is delivered first (newest
	// first), then r is POSTed synchronously — the run is over, so a short
	// blocking wait costs nothing — and the reporter goes inert: r is the LAST
	// POST it ever makes, so after it, console silence means the daemon is gone
	// rather than stopped talking.
	//
	// ctx bounds the whole goodbye: Close returns at the deadline rather than
	// waiting out a pump POST that started before it, and every POST it does
	// make takes its timeout from what the deadline leaves. A longer deadline
	// is capped at CloseBudget. A daemon with several targets passes ONE
	// deadline shared by all of them (Shutdown), so shutdown costs one budget
	// rather than one per target. After the deadline nothing more is posted —
	// that is what keeps an old daemon's late `stopping` beacon from
	// overwriting its replacement's presence row.
	//
	// Idempotent: only the first call speaks. Used for the `stopping` beacon on
	// shutdown paths.
	Close(ctx context.Context, r Report)
}

// notWired is the Reporter for an operator who configured no reachable plane
// surface for reports: every call is a no-op, exactly like backlog.New's
// not-wired poller degrades the gate rather than stopping the run.
type notWired struct{}

func (notWired) Send(Report)                   {}
func (notWired) Close(context.Context, Report) {}

// ShutdownBeacon is one target's goodbye: the reporter and the report it must
// carry.
type ShutdownBeacon struct {
	Reporter Reporter
	Report   Report
}

// Shutdown delivers every beacon concurrently and returns when they have all
// been delivered or ctx expires, whichever is first. One deadline for the whole
// set is the point: a daemon serving N projects must not spend N times the
// goodbye on a hanging plane, and the operator's Ctrl-C waits for this.
//
// The deadline is the guarantee for the reporters this package builds — each
// Close returns by it. A ctx carrying no deadline is capped at closeBudget, so
// the bound holds for a direct caller too. The abandon below is a backstop for
// a reporter that does not honour it: one must never be able to hold process
// exit past the budget.
func Shutdown(ctx context.Context, beacons []ShutdownBeacon) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, closeBudget)
		defer cancel()
	}
	var wg sync.WaitGroup
	for _, b := range beacons {
		if b.Reporter == nil {
			continue
		}
		wg.Add(1)
		go func(b ShutdownBeacon) {
			defer wg.Done()
			b.Reporter.Close(ctx, b.Report)
		}(b)
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		// The last Close may have returned at the same instant the deadline
		// fired; report an abandoned wait only if one is still running.
		select {
		case <-done:
			return
		default:
		}
		log.Printf("fleet: shutdown deadline reached with a stopping beacon still unsent")
	}
}

// New builds a Reporter. Missing either the endpoint or the key yields a
// not-wired one, so an operator running without a configured plane is degraded
// rather than broken.
//
// endpoint is the project-scoped fleet-report URL
// (`https://…/api/projects/<slug>/fleet/report`).
func New(endpoint, apiKey string) Reporter {
	if endpoint == "" || apiKey == "" {
		return notWired{}
	}
	return &httpReporter{
		endpoint: endpoint,
		apiKey:   apiKey,
		client:   &http.Client{Timeout: reportTimeout, CheckRedirect: secureurl.NoDowngradeRedirect},
		queue:    make(chan Report, queueCap),
		done:     make(chan struct{}),
		postMu:   newCtxMutex(),
	}
}

// reportTimeout bounds ONE POST. Short on purpose: the pump drops rather than
// accumulates, so a hung endpoint burns timeouts in the background while the
// loop never notices.
const reportTimeout = 5 * time.Second

// CloseBudget is the wall-clock ceiling on a daemon's whole goodbye: ONE
// deadline shared by every target's Close (via Shutdown), so a hanging plane
// costs it once per daemon rather than once per project. A healthy plane
// finishes in milliseconds; this is patience, not a timeout the loop needs.
// Nothing is posted after it — a late `stopping` beacon would overwrite the
// presence row of the daemon that replaced this one.
const CloseBudget = 5 * time.Second

// closeBudget is the value Close and Shutdown enforce. A var so a test can
// shrink it and pin the cap without waiting out the production five seconds.
var closeBudget = CloseBudget

// queueCap is how many reports may sit unread while the pump works through a
// slow endpoint. Generous next to the production cadence (one per idle poll);
// anything beyond this during an outage is dropped by design.
const queueCap = 16

// ctxMutex is a mutex whose acquisition can be abandoned at a deadline.
// sync.Mutex cannot be: Close must not spend the entire shutdown budget waiting
// out a pump POST that started before the budget did, or the bound it promises
// is a fiction. Construct with newCtxMutex: the zero value's nil token blocks
// forever.
type ctxMutex struct{ token chan struct{} }

func newCtxMutex() *ctxMutex {
	m := &ctxMutex{token: make(chan struct{}, 1)}
	m.token <- struct{}{}
	return m
}

// Lock acquires the mutex, waiting as long as it takes. The pump uses this: it
// has no deadline to honour.
func (m *ctxMutex) Lock() { <-m.token }

// LockContext acquires the mutex unless ctx is done first. False means the
// caller does not hold it and must not Unlock.
func (m *ctxMutex) LockContext(ctx context.Context) bool {
	select {
	case <-m.token:
		return true
	case <-ctx.Done():
		return false
	}
}

// Unlock releases an acquisition. Only called by a holder, so it cannot block:
// the token buffer is empty exactly while it is held.
func (m *ctxMutex) Unlock() { m.token <- struct{}{} }

type httpReporter struct {
	endpoint string
	apiKey   string
	client   *http.Client
	queue    chan Report

	// done is closed by Close; the pump's select on queue-vs-done then parks it
	// for good. postMu serialises every POST so that after Close takes the lock
	// nothing can land after the `stopping` beacon. stateMu orders Send's
	// enqueue against Close's transition: a Send that has not enqueued when
	// Close takes it is refused, so the queue Close drains is final and a late
	// Send cannot start the pump behind Close's back. closed is read by the
	// pump under postMu, which is what guarantees it starts no POST after Close
	// holds that lock.
	done       chan struct{}
	postMu     *ctxMutex
	stateMu    sync.Mutex
	closed     atomic.Bool
	startOnce  sync.Once
	dropped    atomic.Int64
	failStreak atomic.Int64
}

func (r *httpReporter) Send(rep Report) {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if r.closed.Load() {
		return // Close has ended reporting — the stopping beacon already went out
	}
	r.startOnce.Do(func() { go r.pump() })
	select {
	case r.queue <- rep:
	default:
		// The pump is wedged (or the plane is slower than the poll cadence).
		// Drop and count; the next completed POST says how many were lost.
		r.dropped.Add(1)
	}
}

// Close ends the reporter: it waits out whatever POST is in flight, delivers
// what is still queued (newest first), then POSTs rep itself — the last POST
// this reporter ever makes. Serialising on postMu across the whole sequence is
// what makes that "last" claim hold: the pump takes the same lock to receive, so
// once Close holds it the pump can neither deliver nor receive, and any report
// the pump was already delivering has landed before Close's critical section
// begins.
//
// The whole sequence is bounded by ctx: the wait for the pump, each queued
// POST, and the final beacon all draw on what the deadline leaves, so a hanging
// plane cannot stretch one target past it. See Reporter.Close.
func (r *httpReporter) Close(ctx context.Context, rep Report) {
	r.stateMu.Lock()
	if !r.closed.CompareAndSwap(false, true) {
		r.stateMu.Unlock()
		return // idempotent: only the first Close speaks
	}
	close(r.done) // the pump delivers nothing further — its in-flight one first
	r.stateMu.Unlock()

	// A caller passing an unbounded context (a direct Close outside Shutdown)
	// still gets the ceiling; a caller sharing a tighter deadline keeps it.
	ctx, cancel := context.WithTimeout(ctx, closeBudget)
	defer cancel()

	if !r.postMu.LockContext(ctx) {
		log.Printf("fleet: shutdown: the pump was still delivering at the shutdown deadline - no stopping beacon sent")
		return
	}
	defer r.postMu.Unlock()

	// Everything still queued is what the pump did not deliver before the run
	// ended. Newest first: presence is last-writer-wins, so the newest queued
	// state is the one worth having before `stopping`, and the newest report is
	// the one carrying this boundary's iteration record — the thing a run
	// ending at the boundary must not lose.
	var queued []Report
drain:
	for {
		select {
		case q := <-r.queue:
			queued = append(queued, q)
		default:
			break drain
		}
	}
	delivered, lost := 0, 0
	for i := len(queued) - 1; i >= 0; i-- {
		if ctx.Err() != nil {
			break
		}
		if err := r.sendOnce(ctx, queued[i]); err != nil {
			lost++
		} else {
			delivered++
		}
	}
	if undelivered := len(queued) - delivered; undelivered > 0 {
		log.Printf("fleet: shutdown: %d queued report(s) not delivered (%d failed, %d dropped by budget)", undelivered, lost, undelivered-lost)
	}

	// The stopping beacon itself — the last POST this reporter makes; after it,
	// console silence means the daemon is gone, not that it stopped talking.
	// Skipping it once the deadline has passed is deliberate: a beacon that
	// lands after the daemon is gone would mark the replacement as stopping.
	if ctx.Err() != nil {
		log.Printf("fleet: shutdown: deadline reached before the stopping beacon - not sent")
		return
	}
	r.deliver(ctx, rep)
}

// pump serialises every queued report through deliver, one goroutine for the
// life of the process. Order preserved: the plane's presence upsert is
// last-writer-wins, so arrival order is the truth. The lock is taken around
// the RECEIVE, not just the POST, so Close can park the pump: once Close holds
// postMu the pump can no longer take a report, and any report it already holds
// has been delivered before Close's critical section starts.
//
// The done pre-check before the select narrows the window: a pump that took a
// queued report after Close was already waiting on postMu would deliver it
// before the beacon, which is ordered safely, but it would be a POST nobody
// needed. A non-blocking select cannot promise priority — Go picks randomly
// when both are ready — so what actually keeps the beacon last is postMu: the
// closed check and the deliver both happen under it, and Close holds it across
// its drain and the beacon.
func (r *httpReporter) pump() {
	for {
		r.postMu.Lock()
		// Close may be waiting for this lock with a deadline. Checking closed
		// under it gives the invariant that matters: no POST starts after Close
		// returns. A report already selected when Close sets closed may still
		// land while Close waits for the lock — bounded by reportTimeout, and
		// inside the accepted "pump POST started before Close" tradeoff.
		if r.closed.Load() {
			r.postMu.Unlock()
			return
		}
		select {
		case <-r.done:
			r.postMu.Unlock()
			return
		default:
		}
		select {
		case rep := <-r.queue:
			r.deliver(context.Background(), rep)
			r.postMu.Unlock()
		case <-r.done:
			r.postMu.Unlock()
			return
		}
	}
}

// deliver performs one authenticated POST and applies the fail-soft logging
// rule: the FIRST failure of a streak is logged, further consecutive failures
// are silent (an overnight outage must not write a line per poll), and recovery
// is logged once with how many reports were lost. Callers hold postMu.
func (r *httpReporter) deliver(ctx context.Context, rep Report) {
	if err := r.sendOnce(ctx, rep); err != nil {
		if r.failStreak.Add(1) == 1 {
			log.Printf("fleet report failed and was dropped (further consecutive failures stay silent): %v", err)
		}
		return
	}
	streak := r.failStreak.Swap(0)
	dropped := r.dropped.Swap(0)
	if streak > 0 || dropped > 0 {
		log.Printf("fleet reporting recovered after %d failed report(s), %d dropped", streak, dropped)
	}
}

func (r *httpReporter) sendOnce(ctx context.Context, rep Report) error {
	body, err := marshalReport(rep)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, reportTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST %s: HTTP %d: %s", r.endpoint, resp.StatusCode, firstLine(string(respBody)))
	}
	return nil
}

// ── Probe ─────────────────────────────────────────────────────────────────────

// ProbeError carries the HTTP status of a probe that got an answer the doctor
// needs to branch on.
type ProbeError struct {
	Status int
	Body   string
}

func (e *ProbeError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
}

// Probe asks whether activity reporting is wired and reachable WITHOUT writing
// anything. It exploits the route's own ordering: auth is checked before body
// validation, and an empty body is refused 400 after it. So one harmless POST of
// `{}` distinguishes everything doctor needs:
//
//	400            wired: reachable, routed, and the key accepted -> nil
//	401/403        reachable but the key is rejected/forbidden -> *ProbeError
//	404            reachable but the plane predates fleet reporting -> *ProbeError
//	other status   reachable, unexpected answer -> *ProbeError
//	transport err  unreachable -> plain error
//
// A real beacon is NOT used as the probe: it would upsert a presence row under
// this machine's instance name with doctor's own state, fighting whatever a live
// daemon on the same host is reporting.
func Probe(ctx context.Context, endpoint, apiKey string) error {
	if apiKey == "" {
		return errors.New("no API key (CLANKERBAR_API_KEY unset)")
	}
	if endpoint == "" {
		return errors.New("no fleet-report endpoint could be derived")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: reportTimeout, CheckRedirect: secureurl.NoDowngradeRedirect}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusBadRequest {
		return nil // refused for the empty body: auth AND routing both passed
	}
	return &ProbeError{Status: resp.StatusCode, Body: firstLine(string(body))}
}

// firstLine trims a response body to its first line, for error text — a JSON
// error object is one line, and a whole HTML error page is noise.
func firstLine(s string) string {
	for i, r := range s {
		if r == '\n' || r == '\r' {
			return s[:i]
		}
	}
	return s
}
