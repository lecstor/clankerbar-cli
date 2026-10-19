package supervisor

// The supervisor's status report (CLA-677): the write half of the account-scoped
// roster. Until this file existed the supervisor only READ the plane
// (`GET /api/daemon-roster` every poll), so the console could not tell a HALTed
// instance from a crashed one, a machine asleep from a machine with no daemon,
// and could not show the supervisor's own host and version at all.
//
// One POST per reconcile pass (every poll), plus one immediately after any child
// state change, carries the supervisor's identity and one row per instance:
// `{state, reason, since, childVersion, lastExit, nextRetryAt}`. Every field has
// a source the supervisor already tracks — this file serializes, it does not add
// state.
//
// The rule is the same as internal/fleet's: reporting is TELEMETRY, never control
// flow. A failed or slow POST is logged once per outage and dropped; it must
// never block, delay, fail, or respawn a child. Send enqueues onto a bounded
// channel and returns immediately, and a single background pump serializes the
// POSTs. Nothing here retries: the next poll corrects the row, and the plane
// treats each report as last-writer-wins.
//
// The endpoint lands with CLA-676. Until then a 404 on every POST is expected —
// it is a failure like any other (one log line per outage, then silence), never
// a reason to change supervision.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/version"
)

// The five states the plane's console renders. They are the reported vocabulary
// (@clankerbar/shared INSTANCE_STATES for the roster status endpoint), and every
// instance maps onto exactly one:
//
//   - running     a live child is serving the entry (including while it drains
//     for a requested stop: it is still running)
//   - halted      the child exited with a HALT marker present: the operator's
//     per-instance stop switch, never respawned
//   - backing_off the child is down and a restart is scheduled; nextRetryAt
//     says when
//   - exited      the child is down and nothing is scheduled to bring it back
//   - stopped     the supervisor is not keeping a child up by design: the entry
//     is desired stopped, removed, refused, or the supervisor itself is stopping
const (
	StatusRunning    = "running"
	StatusHalted     = "halted"
	StatusBackingOff = "backing_off"
	StatusExited     = "exited"
	StatusStopped    = "stopped"
)

// StatusExit is the last child exit the supervisor observed for an instance.
type StatusExit struct {
	Code int       // process exit code; -1 when the child was killed by a signal
	At   time.Time // when the Wait goroutine saw it end
}

// StatusInstance is one instance's row in the report. Zero times mean "not
// applicable" and ride as absent/null rather than as the year 1.
type StatusInstance struct {
	Name         string
	State        string // one of the Status* constants
	Reason       string // human-readable, for the console; may be empty
	Since        time.Time
	ChildVersion string // version recorded at the child's spawn; "" if never spawned
	LastExit     *StatusExit
	NextRetryAt  time.Time // zero = no restart scheduled
}

// StatusReport is one POST body: the supervisor's identity plus every instance
// it currently holds.
type StatusReport struct {
	Host              string
	SupervisorVersion string
	StartedAt         time.Time
	Instances         []StatusInstance
}

// StatusReporter sends supervisor status reports. Both methods are fail-soft by
// contract: neither blocks meaningfully, neither returns an error, and neither
// may be load-bearing for supervision. The shape mirrors fleet.Reporter, whose
// reasoning applies unchanged.
type StatusReporter interface {
	// Send enqueues one report and returns at once. Under an unresponsive
	// endpoint the queue fills and the report is dropped rather than awaited.
	// After Close it is a no-op.
	Send(r StatusReport)
	// Close ends reporting: what is queued is delivered first (newest first,
	// bounded), then r is POSTed synchronously — the supervisor is exiting, so
	// a short blocking wait costs nothing — and the reporter goes inert.
	// Idempotent: only the first call speaks.
	Close(r StatusReport)
}

// statusEndpoint derives the status URL from the roster URL: the plane's route
// is the roster's own path with `/status` on the end
// (`<backlog_url>/api/daemon-roster/status`), so the supervisor never needs a
// second operator-stated URL.
func statusEndpoint(rosterURL string) string {
	rosterURL = strings.TrimRight(strings.TrimSpace(rosterURL), "/")
	if rosterURL == "" {
		return ""
	}
	return rosterURL + "/status"
}

type notWiredStatus struct{}

func (notWiredStatus) Send(StatusReport)  {}
func (notWiredStatus) Close(StatusReport) {}

// newStatusReporter builds a reporter. A missing endpoint or key yields a
// not-wired one, so a supervisor whose roster is not wired does not invent a
// second failure mode.
func newStatusReporter(endpoint, apiKey string) StatusReporter {
	if endpoint == "" || apiKey == "" {
		return notWiredStatus{}
	}
	return &httpStatusReporter{
		endpoint: endpoint,
		apiKey:   apiKey,
		client:   &http.Client{Timeout: statusReportTimeout, CheckRedirect: noDowngradeRedirect},
		queue:    make(chan StatusReport, statusQueueCap),
		done:     make(chan struct{}),
	}
}

// statusReportTimeout bounds ONE POST. Short on purpose: the pump drops rather
// than accumulates, so a hung endpoint burns timeouts in the background while
// supervision never notices.
const statusReportTimeout = 5 * time.Second

// statusCloseBudget bounds how long Close may spend delivering what is still
// queued before its own report. One report's worth of patience, exactly as
// fleet.closeFlushBudget.
const statusCloseBudget = 5 * time.Second

// statusQueueCap is how many reports may sit unread while the pump works through
// a slow endpoint. Generous next to the cadence (one per poll, plus transitions).
const statusQueueCap = 16

type httpStatusReporter struct {
	endpoint string
	apiKey   string
	client   *http.Client
	queue    chan StatusReport

	done       chan struct{}
	postMu     sync.Mutex
	closed     atomic.Bool
	startOnce  sync.Once
	dropped    atomic.Int64
	failStreak atomic.Int64
}

func (r *httpStatusReporter) Send(rep StatusReport) {
	if r.closed.Load() {
		return // Close has ended reporting
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
// what is still queued (newest first, within statusCloseBudget), then POSTs rep
// itself. Serialising on postMu across the whole sequence is what makes that
// "last" claim hold — the pump takes the same lock around its receive.
func (r *httpStatusReporter) Close(rep StatusReport) {
	if !r.closed.CompareAndSwap(false, true) {
		return // idempotent: only the first Close speaks
	}
	close(r.done)
	r.postMu.Lock()
	defer r.postMu.Unlock()

	var queued []StatusReport
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
		if time.Since(flushStart) > statusCloseBudget {
			break
		}
		if err := r.sendOnce(queued[i]); err != nil {
			lost++
		} else {
			delivered++
		}
	}
	if undelivered := len(queued) - delivered; undelivered > 0 {
		log.Printf("supervisor status: shutdown: %d queued report(s) not delivered (%d failed, %d dropped by budget)", undelivered, lost, undelivered-lost)
	}
	r.deliver(rep)
}

// pump serialises every queued report through deliver, one goroutine for the
// life of the process. The lock is taken around the RECEIVE, not just the POST,
// so Close can park the pump for good.
func (r *httpStatusReporter) pump() {
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
// stay silent (an overnight outage must not write a line per poll), and recovery
// is logged once with how many reports were lost.
func (r *httpStatusReporter) deliver(rep StatusReport) {
	if err := r.sendOnce(rep); err != nil {
		if r.failStreak.Add(1) == 1 {
			log.Printf("supervisor status report failed and was dropped (further consecutive failures stay silent until it recovers): %v", err)
		}
		return
	}
	streak := r.failStreak.Swap(0)
	dropped := r.dropped.Swap(0)
	if streak > 0 || dropped > 0 {
		log.Printf("supervisor status reporting recovered after %d failed report(s), %d dropped", streak, dropped)
	}
}

func (r *httpStatusReporter) sendOnce(rep StatusReport) error {
	body, err := marshalStatus(rep)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), statusReportTimeout)
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
		return fmt.Errorf("POST %s: HTTP %d: %s", r.endpoint, resp.StatusCode, statusFirstLine(string(respBody)))
	}
	return nil
}

// ── Wire shape ────────────────────────────────────────────────────────────────
// Exactly what CLA-676's POST /api/daemon-roster/status stores. Times are
// RFC3339 (seconds), and the two optional times are explicit JSON null when
// absent so a console can tell "never" from "not served".

type wireStatusExit struct {
	Code int    `json:"code"`
	At   string `json:"at"`
}

type wireStatusInstance struct {
	Name         string          `json:"name"`
	State        string          `json:"state"`
	Reason       string          `json:"reason,omitempty"`
	Since        string          `json:"since,omitempty"`
	ChildVersion string          `json:"childVersion,omitempty"`
	LastExit     *wireStatusExit `json:"lastExit"`
	NextRetryAt  *string         `json:"nextRetryAt"`
}

type wireStatus struct {
	Host              string               `json:"host"`
	SupervisorVersion string               `json:"supervisorVersion"`
	StartedAt         string               `json:"startedAt"`
	Instances         []wireStatusInstance `json:"instances"`
}

func marshalStatus(rep StatusReport) ([]byte, error) {
	w := wireStatus{
		Host:              rep.Host,
		SupervisorVersion: rep.SupervisorVersion,
		StartedAt:         statusTime(rep.StartedAt),
		Instances:         make([]wireStatusInstance, 0, len(rep.Instances)),
	}
	for _, inst := range rep.Instances {
		// An unnamed or unknown-state row would store garbage plane-side; refuse
		// here, once, rather than teach the plane to defend against a reporter
		// bug. The supervisor only ever names the constants above.
		if inst.Name == "" {
			return nil, errors.New("supervisor status report has an instance with no name")
		}
		switch inst.State {
		case StatusRunning, StatusHalted, StatusBackingOff, StatusExited, StatusStopped:
		default:
			return nil, fmt.Errorf("instance %q: state %q is not one of the five the plane knows", inst.Name, inst.State)
		}
		w.Instances = append(w.Instances, wireStatusInstance{
			Name:         inst.Name,
			State:        inst.State,
			Reason:       inst.Reason,
			Since:        statusTime(inst.Since),
			ChildVersion: inst.ChildVersion,
			LastExit:     wireExit(inst.LastExit),
			NextRetryAt:  statusOptionalTime(inst.NextRetryAt),
		})
	}
	return json.Marshal(w)
}

func statusTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func statusOptionalTime(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := statusTime(t)
	return &s
}

func wireExit(e *StatusExit) *wireStatusExit {
	if e == nil {
		return nil
	}
	return &wireStatusExit{Code: e.Code, At: statusTime(e.At)}
}

// statusFirstLine trims a response body to its first line, for error text.
func statusFirstLine(s string) string {
	for i, r := range s {
		if r == '\n' || r == '\r' {
			return s[:i]
		}
	}
	return s
}

// ── Report building ───────────────────────────────────────────────────────────

// statusReport is the current snapshot. Called on the supervisor's single
// goroutine, so only the fields a Wait goroutine writes are read under the
// instance mutex.
func (d *Supervisor) statusReport() StatusReport {
	rep := StatusReport{
		Host:              d.hostname,
		SupervisorVersion: version.Current,
		StartedAt:         d.startedAt,
	}
	for _, inst := range d.instances {
		rep.Instances = append(rep.Instances, d.statusInstance(inst))
	}
	return rep
}

// statusInstance maps one instance's existing bookkeeping onto the five-state
// vocabulary. Order matters where two facts can be true at once: a live child is
// running whatever its desired state says; a fleet stop outranks a scheduled
// restart (the restart will never fire); and the reason names the first
// classification that explains why the instance is not simply running.
func (d *Supervisor) statusInstance(inst *Instance) StatusInstance {
	inst.mu.Lock()
	alive := inst.cmd != nil && !inst.exited
	childVersion := inst.childVersion
	exitErr := inst.exitErr
	exitAt := inst.exitAt
	aliveSince := inst.aliveSince
	dir := inst.stateDir
	inst.mu.Unlock()

	restartAt := inst.restartAt
	out := StatusInstance{
		Name:         inst.name,
		ChildVersion: childVersion,
		NextRetryAt:  restartAt,
	}
	if !exitAt.IsZero() {
		out.LastExit = &StatusExit{Code: statusExitCode(exitErr), At: exitAt}
	}

	switch {
	case alive:
		out.State = StatusRunning
		out.Since = aliveSince
		switch {
		case d.stopping:
			out.Reason = "supervisor stopping - draining at its iteration boundary"
		case inst.stopRequested:
			out.Reason = "stop requested - draining at its iteration boundary"
		}
	case d.stopping:
		out.State = StatusStopped
		out.Reason = "supervisor stopping"
		out.Since = exitAt
	case inst.halted:
		out.State = StatusHalted
		out.Reason = "HALT marker present"
		if dir != "" {
			out.Reason = "HALT marker present in " + dir
		}
		out.Since = exitAt
	case !restartAt.IsZero():
		out.State = StatusBackingOff
		out.Reason = fmt.Sprintf("restart scheduled in %s", inst.lastBackoff)
		out.Since = exitAt
	case inst.removed:
		out.State = StatusStopped
		out.Reason = "entry removed from the roster"
		out.Since = exitAt
	case inst.refused:
		out.State = StatusStopped
		out.Reason = "entry refused by the supervisor"
		out.Since = exitAt
	case inst.policyRefused:
		out.State = StatusStopped
		out.Reason = "refused: permission policy"
		out.Since = exitAt
	case inst.desired != RosterDesiredRunning:
		out.State = StatusStopped
		out.Reason = "desired state is stopped"
		out.Since = exitAt
	default:
		out.State = StatusExited
		out.Reason = "child exited and no restart is scheduled"
		out.Since = exitAt
	}
	return out
}

// statusExitCode renders a child exit for the wire: an orderly exit is 0, a
// signal is the negative code exec reports, and anything that is not an
// *exec.ExitError (a spawn that never became a process) is -1 rather than a
// fabricated zero.
func statusExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// publishStatus logs the fleet status surface (once per change) and queues a
// status report. Called after every child transition and at the end of every
// reconcile pass, so the plane sees each poll and each state change without
// waiting for the next poll.
func (d *Supervisor) publishStatus() {
	d.logFleetStatus()
	d.reportStatus()
}

// reportStatus queues one report, or does nothing when reporting is not wired.
// Fail-soft by contract: Send enqueues and returns.
func (d *Supervisor) reportStatus() {
	if d.status == nil {
		return
	}
	d.status.Send(d.statusReport())
}
