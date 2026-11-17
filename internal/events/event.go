// Package events is the CLI's half of the agent event channel (CLA-603,
// transcript tier CLA-887): the wire schema from
// docs/proposals/agent-event-channel.md (the plane's repository), CLI-side
// redaction, and the batched sender that posts to the plane's per-run ingest
// endpoint.
//
// Two tiers share one sender. The ACTIVITY tier carries tool names, one
// identifying string per call, and todo snapshots — no tool output and no
// assistant text. The TRANSCRIPT tier is a second, separate opt-in and adds a
// completed call's output and each assistant text block, both capped at 64 KB
// per event. Redaction runs BEFORE truncation and BEFORE anything leaves the
// machine, and it is best effort, not a guarantee: that is why the project's
// dials are the privacy boundary and this is only the second line.
//
// Sending is off the session's critical path. Callers hand events to an Emitter,
// which never blocks them: it buffers within a bound, a goroutine batches and
// posts, and a slow or failed send is dropped (with one bounded log line) rather
// than stalling or failing the agent session.
package events

import (
	"encoding/json"
	"math"
	"time"
	"unicode/utf8"
)

// The channel's event types (CLA-601's schema). The first three are the activity
// tier; the last two are the transcript tier and are sent only while its dial is
// on.
const (
	TypeToolCall          = "tool_call"
	TypeTodoChange        = "todo_change"
	TypeClankerbarMcpCall = "clankerbar_mcp_call"
	TypeToolResult        = "tool_result"
	TypeAssistantText     = "assistant_text"
)

// Wire caps from the design note. They are duplicated here because the CLI must
// truncate before sending: the plane truncates prose again defensively, but an
// emitter that sends its whole command line learns nothing and costs a round
// trip. The identifier caps are REFUSALS at the plane (a cut callId would break
// the join to its result), so over-cap identifiers are dropped here rather than
// sent to be refused.
const (
	MaxTargetBytes      = 512
	MaxTodoContentBytes = 256
	MaxTodos            = 50
	MaxToolBytes        = 128
	MaxCallIDBytes      = 256

	// MaxTranscriptBytes is the transcript tier's nominal per-event cap on
	// `output` and `text`. It is the R2 copy's cap: no layer keeps more. The
	// emitted prefix can be shorter when JSON escaping would push the event past
	// MaxBatchBytes (fitTranscript) — one event must always fit a batch — and
	// `outputBytes` / `textBytes` make both cuts explicit rather than silent.
	MaxTranscriptBytes = 64 << 10

	// MaxBatchEvents / MaxBatchBytes are the flush caps: whichever is reached
	// first. The body cap is the plane's whole-body limit, so one event that
	// would overflow flushes the batch before it rather than splitting it.
	MaxBatchEvents = 50
	MaxBatchBytes  = 256 << 10

	// FlushInterval is the cadence bound: a quiet session's last event still
	// reaches the plane within this window.
	FlushInterval = 2 * time.Second
)

// TodoItem is one item of a todo_change snapshot. The plane accepts exactly
// these three statuses, and the snapshot is the full current list rather than a
// diff.
type TodoItem struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

// ToolCallEvent is one harness tool call in the activity tier.
type ToolCallEvent struct {
	Seq       uint64 `json:"seq"`
	At        string `json:"at"`
	Type      string `json:"type"`
	Tool      string `json:"tool"`
	CallID    string `json:"callId"`
	Target    string `json:"target"`
	Truncated bool   `json:"truncated,omitempty"`
	Redacted  bool   `json:"redacted,omitempty"`
}

// ClankerbarMcpCallEvent is a clankerbar MCP call with the bare tool name. The
// call's full input is never sent; Target is the single most identifying
// argument, and is absent when the tool has none.
type ClankerbarMcpCallEvent struct {
	Seq       uint64 `json:"seq"`
	At        string `json:"at"`
	Type      string `json:"type"`
	Tool      string `json:"tool"`
	CallID    string `json:"callId"`
	Target    string `json:"target,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Redacted  bool   `json:"redacted,omitempty"`
}

// TodoChangeEvent is a full todo-list snapshot in the activity tier.
type TodoChangeEvent struct {
	Seq       uint64     `json:"seq"`
	At        string     `json:"at"`
	Type      string     `json:"type"`
	Todos     []TodoItem `json:"todos"`
	Truncated bool       `json:"truncated,omitempty"`
	Redacted  bool       `json:"redacted,omitempty"`
}

// ToolResultEvent is one completed tool call's output — TRANSCRIPT TIER ONLY.
// It is the counterpart of a ToolCallEvent, joined by CallID.
//
// Output is a POINTER so an empty-but-present output encodes as `"output":""`.
// The plane requires the key unless the output was withheld by redaction, and
// an omitted empty output would read as a redaction that never happened.
type ToolResultEvent struct {
	Seq         uint64  `json:"seq"`
	At          string  `json:"at"`
	Type        string  `json:"type"`
	CallID      string  `json:"callId"`
	OK          bool    `json:"ok"`
	Output      *string `json:"output,omitempty"`
	OutputBytes int     `json:"outputBytes"`
	Truncated   bool    `json:"truncated,omitempty"`
	Redacted    bool    `json:"redacted,omitempty"`
}

// AssistantTextEvent is one assistant text block — TRANSCRIPT TIER ONLY.
// Consecutive blocks are not merged: a viewer that wants paragraphs can join
// them, and merging would make the stored feed ranges lie about the source.
type AssistantTextEvent struct {
	Seq       uint64 `json:"seq"`
	At        string `json:"at"`
	Type      string `json:"type"`
	Text      string `json:"text"`
	TextBytes int    `json:"textBytes"`
	Truncated bool   `json:"truncated,omitempty"`
	Redacted  bool   `json:"redacted,omitempty"`
}

// batch is the request body the plane's POST /api/runs/<runId>/events accepts.
type batch struct {
	Harness string            `json:"harness"`
	Events  []json.RawMessage `json:"events"`
}

// event is the emitter's internal queued shape. One struct with a type switch
// keeps the buffer homogeneous while the wire shape stays per-type.
type event struct {
	kind      string
	at        string
	tool      string
	callID    string
	target    string
	todos     []TodoItem
	truncated bool
	redacted  bool

	// Transcript tier. outputBytes/textBytes are the lengths measured after
	// redaction and before truncation, so for a present output the difference
	// from len(output) / len(text) means exactly "bytes were cut". A withheld
	// `.env*` output has dropOutput set: no payload is sent and the count is
	// simply what was withheld.
	ok          bool
	output      string
	outputBytes int
	text        string
	textBytes   int
	dropOutput  bool
}

// transcript reports whether this event belongs to the transcript tier.
func (e event) transcript() bool {
	return e.kind == TypeToolResult || e.kind == TypeAssistantText
}

// wire renders the event with its per-run sequence number, in the exact shape
// the plane validates.
func (e event) wire(seq uint64) any {
	switch e.kind {
	case TypeToolCall:
		return ToolCallEvent{
			Seq: seq, At: e.at, Type: e.kind,
			Tool: e.tool, CallID: e.callID, Target: e.target,
			Truncated: e.truncated, Redacted: e.redacted,
		}
	case TypeClankerbarMcpCall:
		return ClankerbarMcpCallEvent{
			Seq: seq, At: e.at, Type: e.kind,
			Tool: e.tool, CallID: e.callID, Target: e.target,
			Truncated: e.truncated, Redacted: e.redacted,
		}
	case TypeTodoChange:
		return TodoChangeEvent{
			Seq: seq, At: e.at, Type: e.kind,
			Todos: e.todos, Truncated: e.truncated, Redacted: e.redacted,
		}
	case TypeToolResult:
		out := ToolResultEvent{
			Seq: seq, At: e.at, Type: e.kind,
			CallID: e.callID, OK: e.ok,
			OutputBytes: e.outputBytes,
			Truncated:   e.truncated, Redacted: e.redacted,
		}
		if !e.dropOutput {
			output := e.output
			out.Output = &output
		}
		return out
	case TypeAssistantText:
		return AssistantTextEvent{
			Seq: seq, At: e.at, Type: e.kind,
			Text: e.text, TextBytes: e.textBytes,
			Truncated: e.truncated, Redacted: e.redacted,
		}
	}
	return nil
}

// approxBytes is a cheap upper-bound estimate used to wake the sender early,
// not to size a batch. The exact body is measured when the batch is built.
func (e event) approxBytes() int {
	n := len(e.at) + len(e.kind) + len(e.tool) + len(e.callID) + len(e.target) + 48
	for _, t := range e.todos {
		n += len(t.Content) + len(t.Status) + 24
	}
	return n + len(e.output) + len(e.text) + 48
}

// marshal is json.Marshal for the tests and the batcher; an event always
// marshals (all fields are strings and slices).
func (e event) marshal(seq uint64) ([]byte, error) { return json.Marshal(e.wire(seq)) }

// batchOverhead is what the batch wrapper adds before any event's encoded
// bytes: the same figure buildBatch measures against.
func batchOverhead(harness string) int {
	return len(`{"harness":"","events":[]}`) + len(harness)
}

// fitTranscript shrinks a transcript event's payload until its JSON encoding
// fits a batch on its own. The 64 KB cap is on the raw bytes, but JSON expands
// control and HTML bytes to six (`\uXXXX`), so a cap-sized output can encode
// past MaxBatchBytes — and buildBatch drops a lone event that big whole, losing
// what the plane would have accepted. outputBytes/textBytes keep the
// after-redaction length, so the extra cut is already visible as `truncated`.
func (e *event) fitTranscript(harness string) {
	payload, text := e.output, false
	if e.kind == TypeAssistantText {
		payload, text = e.text, true
	}
	// The event with an empty payload, at the widest seq a batch can assign
	// (the seq is the only field not known here). The fit sets `truncated` on
	// the event it shrinks, so the probe carries it too; the free bytes left are
	// the payload's encoded budget.
	probe := *e
	probe.truncated = true
	if text {
		probe.text = ""
	} else {
		probe.output = ""
	}
	raw, err := probe.marshal(math.MaxUint64)
	if err != nil {
		return // unreachable: every field is a string, a bool or a slice
	}
	budget := MaxBatchBytes - batchOverhead(harness) - len(raw)
	if budget < 0 {
		budget = 0
	}
	kept, used := 0, 0
	for kept < len(payload) {
		r, size := utf8.DecodeRuneInString(payload[kept:])
		cost := jsonStringRuneBytes(r, size)
		if used+cost > budget {
			break
		}
		used += cost
		kept += size
	}
	if kept >= len(payload) {
		return
	}
	if text {
		e.text = payload[:kept]
	} else {
		e.output = payload[:kept]
	}
	e.truncated = true
}

// jsonStringRuneBytes is one rune's encoded length inside a JSON string, as
// encoding/json writes it: quotes and backslashes take two, so do the short
// control escapes (\n, \t, ...); other control bytes, <, >, & and the JS line
// separators U+2028/U+2029 take six (\uXXXX); an invalid byte becomes \ufffd.
// Everything else is its own UTF-8 length. Under-counting here would let an
// event encode past a batch, so the six-byte cases are the load-bearing ones.
func jsonStringRuneBytes(r rune, size int) int {
	switch {
	case r == utf8.RuneError && size == 1:
		return 6 // \ufffd
	case r == '"' || r == '\\':
		return 2
	case r == '\b' || r == '\f' || r == '\n' || r == '\r' || r == '\t':
		return 2
	case r < 0x20 || r == '<' || r == '>' || r == '&':
		return 6 // \u00XX
	case r == '\u2028' || r == '\u2029':
		return 6 // escaped unconditionally for JS safety
	}
	return size
}
