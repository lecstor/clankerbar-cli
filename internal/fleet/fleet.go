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
	"encoding/json"
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
// judge, so the CLI owns the mapping from what its loop is doing to these four:
//
//   - idle      alive and polling, holding nothing, spawning normally
//   - iteration mid-drain: n is the 1-based iteration number, taskRef the task
//     being worked (empty until a claim has been observed), phase the label of
//     the phase session about to spawn or running
//   - draining  alive but not spawning new sessions (console pause, fleet pause)
//   - stopping  shutting down on purpose (STOP/HALT, a marker restart, budget or
//     max-iterations reached, signal); sent as the final beacon
const (
	StateIdle      = "idle"
	StateIteration = "iteration"
	StateDraining  = "draining"
	StateStopping  = "stopping"
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

// State is the presence half of a report: what the daemon is doing right now.
type State struct {
	Kind    string // one of the State* constants
	N       int    // iteration number, when Kind is iteration
	TaskRef string // task being worked ("CLA-466"), when known
	Phase   string // phase label, when Kind is iteration
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
	// (Capabilities.ReportsCost false — codex today), or this session reported
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
}

// Report is one POST body: identity + current state + any iterations that
// finished since the last report.
type Report struct {
	Identity
	State      State
	Iterations []Iteration
}

// Reporter sends activity reports. Both methods are fail-soft by contract:
// neither blocks meaningfully, neither returns an error, and neither may be
// load-bearing for the caller.
type Reporter interface {
	// Send enqueues one report and returns at once. Under an unresponsive
	// endpoint the queue fills and the report is dropped (counted, mentioned in
	// the next line the reporter logs) rather than awaited. After Close it is a
	// no-op.
	Send(r Report)
	// Close ends reporting. Everything still queued is delivered first (newest
	// first, bounded — a run that ends at a drain boundary must not lose the
	// iteration record that boundary just posted), then r is POSTed
	// synchronously — the run is over, so a short blocking wait costs nothing —
	// and the reporter goes inert: r is the LAST POST it ever makes, so after
	// it, console silence means the daemon is gone rather than stopped talking.
	// Idempotent: only the first call speaks. Used for the `stopping` beacon on
	// shutdown paths.
	Close(r Report)
}

// notWired is the Reporter for an operator who configured no reachable plane
// surface for reports: every call is a no-op, exactly like backlog.New's
// not-wired poller degrades the gate rather than stopping the run.
type notWired struct{}

func (notWired) Send(Report)  {}
func (notWired) Close(Report) {}

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
		client:   &http.Client{Timeout: reportTimeout, CheckRedirect: noDowngradeRedirect},
		queue:    make(chan Report, queueCap),
		done:     make(chan struct{}),
	}
}

// reportTimeout bounds ONE POST. Short on purpose: the pump drops rather than
// accumulates, so a hung endpoint burns timeouts in the background while the
// loop never notices.
const reportTimeout = 5 * time.Second

// closeFlushBudget bounds how long Close may spend delivering what is still
// queued before the `stopping` beacon. One report's worth of patience: it lets
// a run that ends right at a drain boundary get that boundary's iteration
// record out (the newest queued report, posted first) while a wedged plane
// still cannot hold process exit hostage for the whole queue.
const closeFlushBudget = 5 * time.Second

// queueCap is how many reports may sit unread while the pump works through a
// slow endpoint. Generous next to the production cadence (one per idle poll);
// anything beyond this during an outage is dropped by design.
const queueCap = 16

type httpReporter struct {
	endpoint string
	apiKey   string
	client   *http.Client
	queue    chan Report

	// done is closed by Close; the pump's select on queue-vs-done then parks
	// it for good. postMu serialises every POST so that after Close takes the
	// lock nothing can land after the `stopping` beacon. closed makes Send a
	// no-op once Close has run, and guards Close's own idempotence.
	done       chan struct{}
	postMu     sync.Mutex
	closed     atomic.Bool
	startOnce  sync.Once
	dropped    atomic.Int64
	failStreak atomic.Int64
}

func (r *httpReporter) Send(rep Report) {
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
// what is still queued (newest first, within closeFlushBudget), then POSTs rep
// itself — the last POST this reporter ever makes. Serialising on postMu across
// the whole sequence is what makes that "last" claim hold: the pump takes the
// same lock to receive, so once Close holds it the pump can neither deliver
// nor receive, and any report the pump was already delivering has landed
// before Close's critical section begins.
func (r *httpReporter) Close(rep Report) {
	if !r.closed.CompareAndSwap(false, true) {
		return // idempotent: only the first Close speaks
	}
	close(r.done) // the pump delivers nothing further — its in-flight one first
	r.postMu.Lock()
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
	flushStart := time.Now()
	delivered, lost := 0, 0
	for i := len(queued) - 1; i >= 0; i-- {
		if time.Since(flushStart) > closeFlushBudget {
			break
		}
		if err := r.sendOnce(queued[i]); err != nil {
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
	r.deliver(rep)
}

// pump serialises every queued report through deliver, one goroutine for the
// life of the process. Order preserved: the plane's presence upsert is
// last-writer-wins, so arrival order is the truth. The lock is taken around
// the RECEIVE, not just the POST, so Close can park the pump: once Close holds
// postMu the pump can no longer take a report, and any report it already holds
// has been delivered before Close's critical section starts.
func (r *httpReporter) pump() {
	for {
		r.postMu.Lock()
		select {
		case rep := <-r.queue:
			r.deliver(rep)
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
func (r *httpReporter) deliver(rep Report) {
	if err := r.sendOnce(rep); err != nil {
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

func (r *httpReporter) sendOnce(rep Report) error {
	body, err := marshalReport(rep)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
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

// ── Wire shape ────────────────────────────────────────────────────────────────
// Exactly what apps/web/src/server/fleet-report.ts parseFleetReport reads
// (CLA-465). Kept as a separate marshalling step with its own tags so the Go
// field names can say what they mean without leaking into the JSON.

type wireState struct {
	Kind    string `json:"kind"`
	N       int    `json:"n,omitempty"`
	TaskRef string `json:"taskRef,omitempty"`
	Phase   string `json:"phase,omitempty"`
}

// wireUsage is the normalised breakdown on the wire. Each key is omitted when
// zero, so a session that reported usage with an empty bucket stays a compact
// object rather than five zeroes.
type wireUsage struct {
	Input      int `json:"input,omitempty"`
	Output     int `json:"output,omitempty"`
	Reasoning  int `json:"reasoning,omitempty"`
	CacheRead  int `json:"cacheRead,omitempty"`
	CacheWrite int `json:"cacheWrite,omitempty"`
}

type wireSession struct {
	Phase   string `json:"phase"`
	Harness string `json:"harness"`
	// Model is deliberately not omitempty: the plane stores the empty string as
	// null ("not resolved"), and sending it explicitly is CLA-630's contract.
	Model string `json:"model"`
	// UsageReported, Usage and CostUSD are the CLA-691 additions, and each is
	// omitted when there is nothing to say: an older CLI's entry — and a session
	// that died before its harness reported anything — stays the pre-CLA-691
	// shape byte for byte, so the plane's "no usage" case is representable.
	UsageReported bool       `json:"usageReported,omitempty"`
	Usage         *wireUsage `json:"usage,omitempty"`
	// CostUSD is a pointer so a reported ZERO survives marshalling
	// (omitempty drops a nil pointer, never a pointer to 0).
	CostUSD *float64 `json:"costUsd,omitempty"`
	// RunID omits itself when the session held no claim, like the rest.
	RunID string `json:"runId,omitempty"`
	// Cause is omitted for an ordinary session, so an older plane sees the same
	// bytes it always did. The current plane ignores unknown session keys; the
	// classification is carried for the durable phase_session record to read
	// once its parser stores it (CLA-703 follow-up).
	Cause string `json:"cause,omitempty"`
}

type wireIteration struct {
	TaskID          string        `json:"taskId,omitempty"`
	TaskRef         string        `json:"taskRef,omitempty"`
	Phases          []string      `json:"phases,omitempty"`
	Sessions        []wireSession `json:"sessions,omitempty"`
	Outcome         string        `json:"outcome"`
	DurationSeconds float64       `json:"durationSeconds,omitempty"`
	Tokens          int           `json:"tokens,omitempty"`
	// CostUSD is the iteration's dollar spend; always present — UNLIKE Tokens
	// (omitempty) — so a zero (nothing reported) stays distinguishable from a
	// missing key (an older CLI). CLA-677.
	CostUSD float64 `json:"costUsd"`
}

type wireReport struct {
	Instance       string          `json:"instance"`
	Host           string          `json:"host,omitempty"`
	Version        string          `json:"version,omitempty"`
	ConfigIdentity string          `json:"configIdentity,omitempty"`
	State          *wireState      `json:"state,omitempty"`
	Iterations     []wireIteration `json:"iterations,omitempty"`
}

func marshalReport(rep Report) ([]byte, error) {
	// An unnamed instance would upsert a garbage row keyed on "" — refuse here,
	// once, rather than teach the plane to defend against it.
	if rep.Instance == "" {
		return nil, errors.New("fleet report has no instance name")
	}
	w := wireReport{
		Instance:       rep.Instance,
		Host:           rep.Host,
		Version:        rep.Version,
		ConfigIdentity: rep.ConfigIdentity,
		State:          &wireState{Kind: rep.State.Kind, N: rep.State.N, TaskRef: rep.State.TaskRef, Phase: rep.State.Phase},
	}
	if w.State.Kind == "" {
		w.State.Kind = StateIdle
	}
	for _, it := range rep.Iterations {
		switch it.Outcome {
		case OutcomeCheckpoint, OutcomeReleased, OutcomeParked, OutcomeDead:
		default:
			// A typo'd outcome would 400 the WHOLE report plane-side
			// (all-or-nothing batch), losing the presence refresh too. Drop the
			// row here instead; the loop only ever names the four constants, so
			// reaching this means a programming error worth one loud line.
			return nil, fmt.Errorf("iteration outcome %q is not one of the four the plane knows", it.Outcome)
		}
		w.Iterations = append(w.Iterations, wireIteration{
			TaskID:          it.TaskID,
			TaskRef:         it.TaskRef,
			Phases:          it.Phases,
			Sessions:        wireSessions(it.Sessions),
			Outcome:         it.Outcome,
			DurationSeconds: it.DurationSeconds,
			Tokens:          it.Tokens,
			CostUSD:         it.CostUSD,
		})
	}
	return json.Marshal(w)
}

// maxSessionsPerIteration mirrors the plane's MAX_SESSIONS_PER_ITERATION
// (apps/web/src/server/fleet-report.ts). The plane refuses the WHOLE report —
// all-or-nothing, presence refresh included — when an iteration carries more,
// so the cap is an interface this side has to honour rather than a bound the
// plane silently sheds. The shipped phase sequences cannot reach it (handoff
// respawns are capped per phase and dead retries per task), but a many-phase
// config plus respawns on each could; clamping the tail keeps the report — and
// with it the iteration record and presence — valid, at the cost of the
// least-important entries.
const maxSessionsPerIteration = 20

// wireSessions converts one iteration's sessions to the wire shape, clamping at
// the plane's per-iteration cap. The FIRST entries in spawn order are kept: the
// configured phases lead the list, and it is the tail of respawns that a run
// long enough to cross the cap has accumulated. The clamp is logged because it
// means the reporter overran a contract the plane will not forgive — a loop bug
// to see, not a normal path.
func wireSessions(sessions []Session) []wireSession {
	if len(sessions) == 0 {
		return nil // no key on the wire: the pre-CLA-630 shape, which stays valid
	}
	if len(sessions) > maxSessionsPerIteration {
		log.Printf("fleet: iteration spawned %d sessions, over the plane's per-iteration cap of %d — reporting the first %d in spawn order and dropping %d, or the plane would refuse the whole report",
			len(sessions), maxSessionsPerIteration, maxSessionsPerIteration, len(sessions)-maxSessionsPerIteration)
		sessions = sessions[:maxSessionsPerIteration]
	}
	out := make([]wireSession, 0, len(sessions))
	for _, s := range sessions {
		w := wireSession{Phase: s.Phase, Harness: s.Harness, Model: s.Model, RunID: s.RunID, Cause: s.Cause}
		if s.UsageReported {
			w.UsageReported = true
			w.Usage = &wireUsage{
				Input:      s.Usage.Input,
				Output:     s.Usage.Output,
				Reasoning:  s.Usage.Reasoning,
				CacheRead:  s.Usage.CacheRead,
				CacheWrite: s.Usage.CacheWrite,
			}
		}
		// CLA-737: the plane refuses a session carrying costUsd without
		// usageReported: true ("session.costUsd requires session.usageReported"),
		// and it refuses the WHOLE report with it. sessionSpend already withholds
		// the figure at the aggregated source, but Session is constructible by any
		// caller and the wire is where the contract lives — so the guarantee is
		// made here too, rather than trusting every future producer to remember a
		// rule whose violation costs unrelated iterations and presence as well.
		if s.UsageReported && s.CostUSD != nil {
			w.CostUSD = s.CostUSD
		}
		out = append(out, w)
	}
	return out
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

	client := &http.Client{Timeout: reportTimeout, CheckRedirect: noDowngradeRedirect}
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

// noDowngradeRedirect refuses a redirect that would put the bearer token on the
// wire in cleartext — the same guard the backlog poller and the plane client
// carry (CLA-257): Go forwards Authorization on an https -> http hop to the SAME
// host, which is exactly the exposure the credential-origin rule exists to
// prevent.
func noDowngradeRedirect(req *http.Request, _ []*http.Request) error {
	if _, err := secureurl.Origin(req.URL.String()); err != nil {
		return fmt.Errorf("refusing redirect: %w", err)
	}
	return nil
}
