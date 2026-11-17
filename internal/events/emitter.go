package events

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Defaults. Config exists so tests can shrink the intervals; production takes
// these.
const (
	defaultFlushEvery  = FlushInterval
	defaultSendTimeout = 5 * time.Second
	defaultCloseGrace  = 2 * time.Second
	defaultMaxBuffered = 512
)

// Config is one session's emitter wiring.
type Config struct {
	// Origin is the trusted plane origin (never a URL read off a checkout — the
	// caller takes it from the operator's config, as every credentialed call
	// does). The endpoint is <Origin>/api/runs/<runId>/events.
	Origin string
	// APIKey is the bearer token: the same CLANKERBAR_API_KEY the driver's other
	// plane calls use.
	APIKey string
	// Harness is the batch's `harness` field: "claude" or "opencode".
	Harness string

	// Transcript is the project's `events.transcript` dial. False — its zero
	// value — makes the transcript-tier methods no-ops, so an adapter calls them
	// unconditionally and the activity tier is unaffected. `events.transcript`
	// is only ever on beside `events.activity` (the plane refuses the pair
	// otherwise), which is why this flag does not gate the activity methods.
	Transcript bool

	// Seq is the per-run sequence counter. Nil builds a process-local one.
	Seq *SeqCounter

	// Client is the HTTP client. Nil gets a short-timeout client that refuses
	// redirects outright: the endpoint is exact, and a redirect is the one way a
	// bearer token could be sent somewhere it was not meant to go.
	Client *http.Client

	// Logf is where the emitter's own failure lines go. Nil is silent, which is
	// right for tests; production passes log.Printf.
	Logf func(string, ...any)

	// Now is the clock. Nil is time.Now.
	Now func() time.Time

	FlushEvery  time.Duration
	SendTimeout time.Duration
	CloseGrace  time.Duration
	MaxBuffered int
}

func (c *Config) defaults() {
	if c.FlushEvery <= 0 {
		c.FlushEvery = defaultFlushEvery
	}
	if c.SendTimeout <= 0 {
		c.SendTimeout = defaultSendTimeout
	}
	if c.CloseGrace <= 0 {
		c.CloseGrace = defaultCloseGrace
	}
	if c.MaxBuffered <= 0 {
		c.MaxBuffered = defaultMaxBuffered
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Client == nil {
		c.Client = &http.Client{
			Timeout: c.SendTimeout,
			// Refuse redirects. Go forwards Authorization on an https -> http hop
			// to the same host, and the endpoint is exact, so following one buys
			// nothing and can only move the key.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	if c.Seq == nil {
		c.Seq = NewSeqCounter(NewMemorySeqStore(), c.Logf)
	}
}

// Sink is what a harness adapter feeds. *Emitter implements it, and every method
// is safe on a nil receiver, so an adapter with events switched off calls them
// unconditionally and the method does nothing.
type Sink interface {
	ToolCall(tool, callID, target string)
	TodoChange(todos []TodoItem)
	ClankerbarMcpCall(tool, callID, target string)

	// ToolResult records one completed tool call's output (transcript tier).
	// dropOutput means the output must not leave the machine at all — a `.env*`
	// read — and the event then carries redacted:true and no output.
	ToolResult(callID string, ok bool, output string, dropOutput bool)

	// AssistantText records one assistant text block (transcript tier).
	AssistantText(text string)

	SetRunID(runID string)

	// TranscriptEnabled reports whether transcript-tier events are being sent
	// right now: the dial is on, and the plane has not stopped the tier for this
	// run. Adapters check it before doing transcript-only work (re-reading a
	// spilled tool result, say) that would otherwise be discarded.
	TranscriptEnabled() bool
}

var _ Sink = (*Emitter)(nil)

// Emitter buffers one session's activity events and posts them to the plane in
// batches. Adding an event never blocks and never fails: the buffer is bounded,
// the HTTP call runs on the emitter's own goroutine, and a send that is slow or
// refused is dropped with one bounded log line.
type Emitter struct {
	cfg Config

	mu          sync.Mutex
	pending     []event
	approxBytes int
	runID       string
	started     bool
	stopped     bool
	disabled    bool
	dropped     int
	dropNoted   bool
	failures    int

	// transcriptStopped is set for good when the plane reports that transcript
	// events were dropped: the 50 MB per-run cap was hit, or the project's
	// transcript dial is off. Activity keeps flowing — the cap is the tier's
	// only, and a runaway run must degrade rather than bill unbounded.
	transcriptStopped bool

	wake     chan struct{}
	done     chan struct{}
	finished chan struct{}
	closeOne sync.Once
}

// New builds an emitter. Its sender goroutine starts on the first enqueued
// event; an emitter that never sees one costs nothing. The caller MUST
// eventually Close it; adapters do so when the session ends.
func New(cfg Config) *Emitter {
	cfg.defaults()
	return &Emitter{
		cfg:      cfg,
		wake:     make(chan struct{}, 1),
		done:     make(chan struct{}),
		finished: make(chan struct{}),
	}
}

// SetRunID tells the emitter which run it is reporting for. Until it is known,
// events are buffered (bounded); a session that never claims sends nothing.
// The first non-empty id wins: an emitter serves exactly one session of one run.
func (e *Emitter) SetRunID(runID string) {
	if e == nil {
		return
	}
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return
	}
	e.mu.Lock()
	if e.runID != "" || e.stopped {
		e.mu.Unlock()
		return
	}
	e.runID = runID
	started := e.started
	e.mu.Unlock()
	if started {
		e.signal()
	}
}

// ToolCall records one ordinary tool call. target should already be extracted
// per tool shape; it is redacted, flattened to one line, then truncated.
func (e *Emitter) ToolCall(tool, callID, target string) {
	if e == nil {
		return
	}
	tool, callID = strings.TrimSpace(tool), strings.TrimSpace(callID)
	if !e.identifiersOK(tool, callID) {
		return
	}
	target, redacted := Redact(target)
	target = oneLine(target)
	target, truncated := truncateUTF8(target, MaxTargetBytes)
	e.enqueue(event{
		kind: TypeToolCall, at: e.timestamp(), tool: tool, callID: callID,
		target: target, truncated: truncated, redacted: redacted,
	})
}

// ClankerbarMcpCall records one clankerbar MCP call. tool is the BARE name
// (claim_task, not mcp__clankerbar__claim_task); target is the call's single
// most identifying argument and is omitted when empty.
func (e *Emitter) ClankerbarMcpCall(tool, callID, target string) {
	if e == nil {
		return
	}
	tool, callID = strings.TrimSpace(tool), strings.TrimSpace(callID)
	if !e.identifiersOK(tool, callID) {
		return
	}
	target, redacted := Redact(target)
	target = oneLine(target)
	target, truncated := truncateUTF8(target, MaxTargetBytes)
	e.enqueue(event{
		kind: TypeClankerbarMcpCall, at: e.timestamp(), tool: tool, callID: callID,
		target: target, truncated: truncated, redacted: redacted,
	})
}

// TodoChange records a full todo-list snapshot: capped at 50 items, each
// content redacted then cut to 256 bytes. Items with a status the plane would
// refuse are dropped (the event is marked truncated, because the snapshot is no
// longer the harness's whole answer). An EMPTY list is a real snapshot — the
// harness cleared its todos — and is sent as one; only an absent list sends
// nothing, which the mapping layer decides.
func (e *Emitter) TodoChange(todos []TodoItem) {
	if e == nil {
		return
	}
	if len(todos) == 0 {
		e.enqueue(event{kind: TypeTodoChange, at: e.timestamp(), todos: []TodoItem{}})
		return
	}
	out := make([]TodoItem, 0, len(todos))
	redacted, truncated := false, false
	for _, t := range todos {
		if len(out) >= MaxTodos {
			truncated = true
			break
		}
		if !validTodoStatus(t.Status) {
			truncated = true
			continue
		}
		content, red := Redact(t.Content)
		content, cut := truncateUTF8(content, MaxTodoContentBytes)
		redacted = redacted || red
		truncated = truncated || cut
		out = append(out, TodoItem{Content: content, Status: t.Status})
	}
	if len(out) == 0 {
		// Every item carried a status the plane refuses. Sending an empty list
		// would claim the harness has no todos, which is a different fact.
		return
	}
	e.enqueue(event{kind: TypeTodoChange, at: e.timestamp(), todos: out, truncated: truncated, redacted: redacted})
}

func validTodoStatus(s string) bool {
	return s == "pending" || s == "in_progress" || s == "completed"
}

// ToolResult records one completed tool call's output (transcript tier).
// callID joins it to the activity tier's tool_call. output is the flattened
// result text; dropOutput withholds it entirely — a `.env*` read — and the
// event then says `redacted: true` and carries no output at all.
//
// The order is the design note's: redact the FULL string, then truncate to the
// 64 KB wire cap. outputBytes is measured between the two (after redaction,
// before truncation), so with an output present `outputBytes > len(output)`
// means exactly "bytes were cut" and nothing else is being claimed. A withheld
// `.env*` output is identified by `redacted: true` and no output at all: its
// outputBytes is simply what was withheld.
func (e *Emitter) ToolResult(callID string, ok bool, output string, dropOutput bool) {
	if e == nil || !e.TranscriptEnabled() {
		return
	}
	callID = strings.TrimSpace(callID)
	if callID == "" || len(callID) > MaxCallIDBytes {
		// The plane refuses an over-cap identifier with the WHOLE batch; a
		// transcript event is never worth that.
		e.drop()
		return
	}
	ev := event{kind: TypeToolResult, at: e.timestamp(), callID: callID, ok: ok}
	if dropOutput {
		ev.dropOutput = true
		ev.redacted = true
		ev.outputBytes = len(output)
	} else {
		redactedText, redacted := Redact(output)
		cut, truncated := truncateUTF8(redactedText, MaxTranscriptBytes)
		ev.output = cut
		ev.outputBytes = len(redactedText)
		ev.redacted = redacted
		ev.truncated = truncated
		ev.fitTranscript(e.cfg.Harness)
	}
	e.enqueue(ev)
}

// AssistantText records one assistant text block (transcript tier). Consecutive
// blocks are separate events; the redact-then-truncate order and the textBytes
// rule are ToolResult's.
func (e *Emitter) AssistantText(text string) {
	if e == nil || !e.TranscriptEnabled() {
		return
	}
	redactedText, redacted := Redact(text)
	cut, truncated := truncateUTF8(redactedText, MaxTranscriptBytes)
	ev := event{
		kind: TypeAssistantText, at: e.timestamp(), text: cut,
		textBytes: len(redactedText), redacted: redacted, truncated: truncated,
	}
	ev.fitTranscript(e.cfg.Harness)
	e.enqueue(ev)
}

// TranscriptEnabled reports whether transcript-tier events are being sent right
// now: the dial is on, the run's channel is alive, and the plane has not stopped
// the tier. Nil-safe, like every other Sink method.
func (e *Emitter) TranscriptEnabled() bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg.Transcript && !e.transcriptStopped && !e.disabled && !e.stopped
}

// stopTranscript ends this run's transcript tier for this emitter's lifetime:
// queued transcript events are dropped and later ones are refused at enqueue.
// Activity is untouched.
//
// The plane has already appended the feed's `run_cap` marker itself — markers
// are plane-sourced, never emitted by a harness — and reports the dropped count
// on the batch's ACK. This is the CLI's half of the cooperation: stop paying to
// send what the plane will refuse, and say so once.
func (e *Emitter) stopTranscript(dropped int) {
	e.mu.Lock()
	if e.transcriptStopped {
		e.mu.Unlock()
		return
	}
	e.transcriptStopped = true
	kept := e.pending[:0]
	for _, q := range e.pending {
		if q.transcript() {
			dropped++
			continue
		}
		kept = append(kept, q)
	}
	e.pending = kept
	e.approxBytes = 0
	for _, q := range e.pending {
		e.approxBytes += q.approxBytes()
	}
	e.mu.Unlock()
	if e.cfg.Logf != nil {
		e.cfg.Logf("agent events: the plane stopped this run's transcript tier (%d transcript event(s) dropped — the 50 MB run cap or the transcript dial) — activity continues", dropped)
	}
}

// identifiersOK enforces the plane's identifier caps by dropping the event
// rather than sending it to be refused: a cut callId cannot join a call to its
// result, and an over-cap tool name is a mapping bug, not prose.
func (e *Emitter) identifiersOK(tool, callID string) bool {
	if tool == "" || callID == "" || len(tool) > MaxToolBytes || len(callID) > MaxCallIDBytes {
		e.drop()
		return false
	}
	return true
}

func (e *Emitter) timestamp() string {
	return e.cfg.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

func (e *Emitter) enqueue(ev event) {
	e.mu.Lock()
	if e.stopped || e.disabled {
		e.mu.Unlock()
		return
	}
	// A transcript event can lose the race with the plane's stop (the check in
	// ToolResult ran just before the ACK landed). Refuse it here too, or it
	// would be sent once more to be refused again.
	if ev.transcript() && e.transcriptStopped {
		e.mu.Unlock()
		return
	}
	if len(e.pending) >= e.cfg.MaxBuffered {
		e.dropped++
		note := !e.dropNoted
		e.dropNoted = true
		e.mu.Unlock()
		if note && e.cfg.Logf != nil {
			e.cfg.Logf("agent events: buffer full (%d events) — dropping further events for this session", e.cfg.MaxBuffered)
		}
		return
	}
	e.pending = append(e.pending, ev)
	e.approxBytes += ev.approxBytes()
	sizeWake := e.approxBytes >= MaxBatchBytes/2
	countWake := len(e.pending) >= MaxBatchEvents
	if !e.started {
		e.started = true
		go e.run()
	}
	e.mu.Unlock()
	if sizeWake || countWake {
		e.signal()
	}
}

func (e *Emitter) drop() {
	e.mu.Lock()
	e.dropped++
	e.mu.Unlock()
}

func (e *Emitter) signal() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// run is the sender goroutine: timer-driven, woken early when enough events are
// buffered. It ends on Close, after one bounded final flush.
func (e *Emitter) run() {
	ticker := time.NewTicker(e.cfg.FlushEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			e.flush(4, e.cfg.SendTimeout)
		case <-e.wake:
			e.flush(4, e.cfg.SendTimeout)
		case <-e.done:
			e.flush(1, e.cfg.SendTimeout)
			close(e.finished)
			return
		}
	}
}

// maxSendAttempts bounds a rate-limited batch: the first try plus two retries.
// Anything else (a 5xx, a timeout) is not retried at all — one send attempt per
// batch — so a slow plane cannot queue work behind its own latency.
const maxSendAttempts = 3

// A 429's wait is bounded: the plane's per-run budget refills over a minute, and
// a long honour of retry-after would only hold the run's send slot. Riding out a
// short burst is what this can afford.
const (
	defaultRetryWait = 200 * time.Millisecond
	maxRetryWait     = time.Second
)

// flush sends at most maxBatches batches. The whole send of one run is
// serialized across every emitter sharing the SeqCounter: the run's send lock is
// held from allocation through POST to commit, so a later-numbered batch can
// never overtake an earlier one (the plane would drop the earlier as a
// duplicate). A failed batch is dropped, but the sequence numbers are still
// committed, so the next batch does not collide with a send that may have
// landed.
func (e *Emitter) flush(maxBatches int, timeout time.Duration) {
	for i := 0; i < maxBatches; i++ {
		runID := e.currentRunID()
		if runID == "" {
			return
		}
		unlock := e.cfg.Seq.SendLock(runID)
		body, sentRun, maxSeq, n := e.takeBatch()
		if n == 0 {
			unlock()
			return
		}
		// Persist the allocation BEFORE the POST: a process that dies after the
		// batch lands but before the commit must leave the next process starting
		// above this batch, not reusing numbers the plane would drop.
		e.cfg.Seq.Persist(sentRun)
		res := e.send(sentRun, body, timeout)
		e.cfg.Seq.Commit(sentRun, maxSeq)
		unlock()
		if res.transcriptDropped > 0 {
			// The plane hit the run's 50 MB transcript cap (or the project's
			// transcript dial is off) and appended its own `run_cap` marker.
			// Stop the tier here; activity in this and later batches continues.
			e.stopTranscript(res.transcriptDropped)
		}
		if res.stop {
			e.disable()
			return
		}
		if res.err != nil {
			e.noteFailure(res.err)
			return
		}
		e.noteSuccess()
	}
}

// sendResult is one batch send's outcome. retryable marks a rate limit: the
// batch is worth re-sending once the plane's budget refills. stop means this
// run's channel is closed for good. transcriptDropped is how many transcript
// events the plane discarded in this batch — the 50 MB run cap, or the tier's
// dial being off — and non-zero means the tier is over for this run.
type sendResult struct {
	err               error
	stop              bool
	retryable         bool
	retryAfter        time.Duration
	transcriptDropped int
}

// send posts one batch, retrying a rate limit a bounded number of times. The
// body carries seq numbers already allocated and is re-sent unchanged: the plane
// drops any seq it has already accepted, so a retry cannot double a batch.
func (e *Emitter) send(runID string, body []byte, timeout time.Duration) sendResult {
	var res sendResult
	for attempt := 1; ; attempt++ {
		res = e.post(runID, body, timeout)
		if !res.retryable || attempt >= maxSendAttempts {
			return res
		}
		wait := res.retryAfter
		if wait <= 0 {
			wait = defaultRetryWait
		}
		if wait > maxRetryWait {
			wait = maxRetryWait
		}
		select {
		case <-time.After(wait):
		case <-e.done:
			// Closing: holding the run's send slot for a retry nobody waits for
			// would only delay the next session.
			return res
		}
	}
}

// currentRunID reads the run this emitter reports for, or "" when it has none or
// has been stopped. The id is set once and never changes, so a lock taken on it
// covers the batch takeBatch builds.
func (e *Emitter) currentRunID() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.disabled {
		return ""
	}
	return e.runID
}

// takeBatch removes up to MaxBatchEvents events from the front of the buffer
// whose encoded body fits MaxBatchBytes, assigns their sequence numbers, and
// returns the ready-to-post body. A single event that cannot fit is dropped,
// never split.
func (e *Emitter) takeBatch() (body []byte, runID string, maxSeq uint64, n int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.disabled || e.runID == "" || len(e.pending) == 0 {
		return nil, "", 0, 0
	}
	runID = e.runID
	body, n, maxSeq, consumed, dropped := buildBatch(e.pending, runID, e.cfg.Harness, e.cfg.Seq)
	if consumed == 0 {
		return nil, "", 0, 0
	}
	e.dropped += dropped
	if consumed >= len(e.pending) {
		e.pending = nil
		e.approxBytes = 0
	} else {
		e.pending = append([]event(nil), e.pending[consumed:]...)
		e.approxBytes = 0
		for _, q := range e.pending {
			e.approxBytes += q.approxBytes()
		}
	}
	if n == 0 {
		return nil, "", 0, 0
	}
	return body, runID, maxSeq, n
}

// buildBatch is takeBatch's pure half: it walks pending in order, assigns
// sequence numbers, and stops adding once the encoded body would pass
// MaxBatchBytes (counting the comma each additional event adds) or the event
// count passes MaxBatchEvents. It returns how many events it consumed
// (including any it dropped as unsendable) so the caller can remove exactly
// that prefix.
func buildBatch(pending []event, runID, harness string, seq *SeqCounter) (body []byte, n int, maxSeq uint64, consumed, dropped int) {
	// The wrapper's own bytes, including the harness name, before any event.
	overhead := batchOverhead(harness)
	remaining := MaxBatchBytes - overhead

	raws := make([]json.RawMessage, 0, min(len(pending), MaxBatchEvents))
	consumed = 0
	for _, q := range pending {
		if len(raws) >= MaxBatchEvents {
			break
		}
		assigned := seq.Next(runID)
		raw, err := q.marshal(assigned)
		if err != nil {
			consumed++
			dropped++
			continue
		}
		// The encoder inserts one comma between adjacent events; count it, or
		// the body can pass MaxBatchBytes by up to MaxBatchEvents-1 bytes and
		// the plane refuses the whole batch with 413.
		need := len(raw)
		if len(raws) > 0 {
			need++
		}
		if need > remaining {
			if len(raws) == 0 {
				// One event over the whole body cap: it can never be sent.
				consumed++
				dropped++
				continue
			}
			break
		}
		raws = append(raws, raw)
		remaining -= need
		maxSeq = assigned
		consumed++
	}
	if len(raws) == 0 {
		return nil, 0, 0, consumed, dropped
	}
	encoded, err := json.Marshal(batch{Harness: harness, Events: raws})
	if err != nil {
		// Cannot happen (raw JSON messages), but never panic a session's parse
		// path over it: drop the whole candidate.
		return nil, 0, 0, consumed, dropped + len(raws)
	}
	return encoded, len(raws), maxSeq, consumed, dropped
}

// post sends one batch. stop means this run will not be sent again: the plane
// refused it for a reason that will not change (the dial is off, the run ended).
// retryable means a rate limit, which the caller retries a bounded number of
// times with the same body.
func (e *Emitter) post(runID string, body []byte, timeout time.Duration) sendResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	endpoint := strings.TrimRight(e.cfg.Origin, "/") + "/api/runs/" + url.PathEscape(runID) + "/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return sendResult{err: err}
	}
	req.Header.Set("Authorization", "Bearer "+e.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.cfg.Client.Do(req)
	if err != nil {
		return sendResult{err: err}
	}
	defer resp.Body.Close()
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(detail))
	if resp.StatusCode == http.StatusOK {
		// The ACK's dropped counts are how a stale emitter (or one that has
		// crossed the run's 50 MB transcript cap) learns to stop the tier. A
		// body that does not parse means "nothing was reported": the batch was
		// accepted, and the next one will tell us if there is a stop to make.
		var ack struct {
			Dropped struct {
				Transcript int `json:"transcript"`
			} `json:"dropped"`
		}
		_ = json.Unmarshal(detail, &ack)
		return sendResult{transcriptDropped: ack.Dropped.Transcript}
	}
	switch {
	case resp.StatusCode == http.StatusForbidden && strings.Contains(msg, "events_disabled"):
		return sendResult{err: fmt.Errorf("the project's events.activity dial is off (%s)", msg), stop: true}
	case resp.StatusCode == http.StatusConflict && strings.Contains(msg, "run_closed"):
		return sendResult{err: fmt.Errorf("the run has ended (%s)", msg), stop: true}
	case resp.StatusCode == http.StatusTooManyRequests:
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return sendResult{
			err:        fmt.Errorf("plane rate-limited the batch: %s", msg),
			retryable:  true,
			retryAfter: retryAfter(resp),
		}
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return sendResult{err: fmt.Errorf("plane answered %d: %s", resp.StatusCode, msg)}
}

// retryAfter reads the header in either form it may take (seconds or a date),
// and reports 0 when it names no usable wait.
func retryAfter(resp *http.Response) time.Duration {
	raw := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if secs, err := strconv.Atoi(raw); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if at, err := http.ParseTime(raw); err == nil {
		if d := time.Until(at); d > 0 {
			return d
		}
	}
	return 0
}

func (e *Emitter) disable() {
	e.mu.Lock()
	was := e.disabled
	e.disabled = true
	e.pending = nil
	e.approxBytes = 0
	e.mu.Unlock()
	if !was && e.cfg.Logf != nil {
		e.cfg.Logf("agent events: plane refused this run's channel — no further events will be sent for it")
	}
}

func (e *Emitter) noteFailure(err error) {
	e.mu.Lock()
	e.failures++
	n := e.failures
	e.mu.Unlock()
	// One line for the first failure (so the operator knows the channel is not
	// working), then every twentieth: a plane outage must not flood the daemon
	// log that is the rest of the diagnosis.
	if e.cfg.Logf != nil && (n == 1 || n%20 == 0) {
		e.cfg.Logf("agent events: batch send failed (%v) — dropped; the session continues (failure %d)", err, n)
	}
}

func (e *Emitter) noteSuccess() {
	e.mu.Lock()
	had := e.failures
	e.failures = 0
	e.mu.Unlock()
	if had > 0 && e.cfg.Logf != nil {
		e.cfg.Logf("agent events: batch send recovered after %d failure(s)", had)
	}
}

// Close stops the sender after one bounded final flush. It never fails and never
// blocks longer than CloseGrace; the session's own result does not depend on it.
// Events that never learned a run id are dropped here, with one line when there
// were any: a fresh session that never claimed has nothing to report against.
func (e *Emitter) Close() {
	if e == nil {
		return
	}
	e.closeOne.Do(func() {
		e.mu.Lock()
		e.stopped = true
		started := e.started
		e.mu.Unlock()
		if started {
			close(e.done)
			timer := time.NewTimer(e.cfg.CloseGrace)
			select {
			case <-e.finished:
				timer.Stop()
			case <-timer.C:
			}
		}
		e.mu.Lock()
		orphaned := len(e.pending)
		noRun := e.runID == ""
		dropped := e.dropped
		e.pending = nil
		e.approxBytes = 0
		e.mu.Unlock()
		if orphaned > 0 && e.cfg.Logf != nil {
			if noRun {
				e.cfg.Logf("agent events: dropped %d event(s) — this session never claimed a run", orphaned)
			} else {
				e.cfg.Logf("agent events: %d event(s) were still unsent when the session ended", orphaned)
			}
		}
		if dropped > 0 && e.cfg.Logf != nil {
			e.cfg.Logf("agent events: %d event(s) were dropped this session (buffer full or unsendable)", dropped)
		}
	})
}
