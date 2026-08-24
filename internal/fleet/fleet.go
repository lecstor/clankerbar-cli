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

// State is the presence half of a report: what the daemon is doing right now.
type State struct {
	Kind    string // one of the State* constants
	N       int    // iteration number, when Kind is iteration
	TaskRef string // task being worked ("CLA-466"), when known
	Phase   string // phase label, when Kind is iteration
}

// Iteration is one finished loop step: what ran, how it ended, what it cost.
type Iteration struct {
	TaskID          string   // task UUID, when a claim was observed
	TaskRef         string   // qualified ref ("CLA-466"), when known
	Phases          []string // distinct phase labels attempted, in order
	Outcome         string   // one of the Outcome* constants
	DurationSeconds float64  // wall clock for the whole drain
	Tokens          int      // the drain's total reported token spend
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

type wireIteration struct {
	TaskID          string   `json:"taskId,omitempty"`
	TaskRef         string   `json:"taskRef,omitempty"`
	Phases          []string `json:"phases,omitempty"`
	Outcome         string   `json:"outcome"`
	DurationSeconds float64  `json:"durationSeconds,omitempty"`
	Tokens          int      `json:"tokens,omitempty"`
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
			Outcome:         it.Outcome,
			DurationSeconds: it.DurationSeconds,
			Tokens:          it.Tokens,
		})
	}
	return json.Marshal(w)
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
