package events

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// decodeTranscript decodes the one event of a single-event batch.
func decodeTranscript[T any](t *testing.T, bodies [][]byte, i int) T {
	t.Helper()
	b := decodeBatch(t, bodies[i])
	if len(b.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(b.Events))
	}
	var ev T
	if err := json.Unmarshal(b.Events[0], &ev); err != nil {
		t.Fatalf("decode event: %v (%s)", err, b.Events[0])
	}
	return ev
}

func TestEmitterToolResultCapsAt64KB(t *testing.T) {
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusOK, `{"accepted":1}`))
	defer srv.Close()
	e := newTranscriptEmitter(srv, nil, nil)
	e.SetRunID("run-1")

	output := strings.Repeat("x", MaxTranscriptBytes+4096)
	e.ToolResult("call-1", true, output, false)
	bodies := c.waitFor(t, 1)
	ev := decodeTranscript[ToolResultEvent](t, bodies, 0)
	if ev.Type != TypeToolResult || ev.CallID != "call-1" || !ev.OK {
		t.Fatalf("event = %+v", ev)
	}
	if ev.Output == nil || len(*ev.Output) != MaxTranscriptBytes {
		t.Fatalf("output length = %v, want %d", ev.Output, MaxTranscriptBytes)
	}
	if ev.OutputBytes != len(output) {
		t.Errorf("outputBytes = %d, want the ORIGINAL %d", ev.OutputBytes, len(output))
	}
	if !ev.Truncated {
		t.Error("truncated marker missing")
	}
	e.Close()
}

func TestEmitterTranscriptCapsDoNotSplitUTF8(t *testing.T) {
	// The cap is BYTES; a cut through a multi-byte rune must walk back, or the
	// JSON encoder rewrites it as U+FFFD and grows the string past the cap.
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusOK, `{"accepted":1}`))
	defer srv.Close()
	e := newTranscriptEmitter(srv, nil, nil)
	e.SetRunID("run-1")

	output := "a" + strings.Repeat("é", MaxTranscriptBytes/2+10)
	e.ToolResult("call-1", true, output, false)
	e.AssistantText(output)
	events := c.waitForEvents(t, 2)
	seen := map[string]bool{}
	for _, raw := range events {
		var ev struct {
			Type   string  `json:"type"`
			Output *string `json:"output"`
			Text   string  `json:"text"`
		}
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatal(err)
		}
		seen[ev.Type] = true
		got := ev.Text
		if ev.Output != nil {
			got = *ev.Output
		}
		if len(got) > MaxTranscriptBytes {
			t.Errorf("%s is %d bytes, over the cap", ev.Type, len(got))
		}
		if !utf8.ValidString(got) {
			t.Errorf("%s is not valid UTF-8 at the cap boundary", ev.Type)
		}
	}
	// Both halves must be checked: a split flush previously left assistant_text
	// unexamined while the test still passed.
	if !seen[TypeToolResult] || !seen[TypeAssistantText] {
		t.Fatalf("event types = %v, want both transcript types", seen)
	}
	e.Close()
}

func TestEmitterAssistantTextCapsAt64KB(t *testing.T) {
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusOK, `{"accepted":1}`))
	defer srv.Close()
	e := newTranscriptEmitter(srv, nil, nil)
	e.SetRunID("run-1")

	text := strings.Repeat("y", MaxTranscriptBytes+1)
	e.AssistantText(text)
	bodies := c.waitFor(t, 1)
	ev := decodeTranscript[AssistantTextEvent](t, bodies, 0)
	if ev.Type != TypeAssistantText || len(ev.Text) != MaxTranscriptBytes {
		t.Fatalf("event = %+v", ev)
	}
	if ev.TextBytes != len(text) {
		t.Errorf("textBytes = %d, want the ORIGINAL %d", ev.TextBytes, len(text))
	}
	if !ev.Truncated {
		t.Error("truncated marker missing")
	}
	e.Close()
}

func TestEmitterTranscriptByteCountsMeasureRedactedText(t *testing.T) {
	// outputBytes/textBytes are measured AFTER redaction and BEFORE truncation.
	// Plain ASCII has the same length either way, so no existing cap test can
	// tell the two rules apart: a long key past the 64 KB point makes the
	// redacted length differ from the input length, and that is what the counts
	// must report.
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusOK, `{"accepted":1}`))
	defer srv.Close()
	e := newTranscriptEmitter(srv, nil, nil)
	e.SetRunID("run-1")

	output := strings.Repeat("o", MaxTranscriptBytes+100) + " ghp_" + strings.Repeat("a", 36)
	text := strings.Repeat("t", MaxTranscriptBytes+100) + " sk-ant-" + strings.Repeat("b", 30)
	e.ToolResult("call-1", true, output, false)
	e.AssistantText(text)

	events := c.waitForEvents(t, 2)
	var result ToolResultEvent
	var assistant AssistantTextEvent
	for _, raw := range events {
		var kind struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &kind); err != nil {
			t.Fatal(err)
		}
		switch kind.Type {
		case TypeToolResult:
			if err := json.Unmarshal(raw, &result); err != nil {
				t.Fatal(err)
			}
		case TypeAssistantText:
			if err := json.Unmarshal(raw, &assistant); err != nil {
				t.Fatal(err)
			}
		}
	}

	// The redacted prefix plus " " plus the 10-byte marker.
	wantResultBytes := MaxTranscriptBytes + 100 + 1 + len(redactedMarker)
	if result.OutputBytes != wantResultBytes {
		t.Errorf("outputBytes = %d, want the redacted length %d (input was %d)", result.OutputBytes, wantResultBytes, len(output))
	}
	if result.OutputBytes == len(output) {
		t.Error("outputBytes measured the raw input, not the redacted text")
	}
	if !result.Truncated {
		t.Error("tool_result truncated marker missing")
	}
	wantTextBytes := MaxTranscriptBytes + 100 + 1 + len(redactedMarker)
	if assistant.TextBytes != wantTextBytes {
		t.Errorf("textBytes = %d, want the redacted length %d (input was %d)", assistant.TextBytes, wantTextBytes, len(text))
	}
	if assistant.TextBytes == len(text) {
		t.Error("textBytes measured the raw input, not the redacted text")
	}
	if !assistant.Truncated {
		t.Error("assistant_text truncated marker missing")
	}
	e.Close()
}

func TestFitTranscriptMatchesTheJSONEncoder(t *testing.T) {
	// The fit's byte arithmetic must agree with encoding/json, or an event can
	// still encode past a batch on content the estimate under-counted. Every
	// expanding shape is fed at the cap and the real encoder is the judge.
	payloads := map[string]string{
		"control":       strings.Repeat("\x01", MaxTranscriptBytes),
		"short escapes": strings.Repeat("\n\t\r", MaxTranscriptBytes/3),
		"html":          strings.Repeat("<>&", MaxTranscriptBytes/3),
		"js separators": strings.Repeat("\u2028\u2029", MaxTranscriptBytes/6),
		"quotes":        strings.Repeat(`"\`, MaxTranscriptBytes/2),
		"multibyte":     strings.Repeat("é", MaxTranscriptBytes/2),
		"invalid utf8":  strings.Repeat("\xff", MaxTranscriptBytes),
		"mixed":         strings.Repeat("a\x01<é\u2028", MaxTranscriptBytes/9),
	}
	for name, payload := range payloads {
		t.Run(name, func(t *testing.T) {
			ev := event{kind: TypeToolResult, callID: "c1", output: payload, outputBytes: len(payload)}
			ev.fitTranscript("claude")
			raw, err := ev.marshal(math.MaxUint64)
			if err != nil {
				t.Fatal(err)
			}
			if limit := MaxBatchBytes - batchOverhead("claude"); len(raw) > limit {
				t.Fatalf("encoded event = %d bytes, over the %d-byte batch budget", len(raw), limit)
			}
			if len(ev.output) > len(payload) {
				t.Fatalf("fit grew the payload: %d > %d", len(ev.output), len(payload))
			}
		})
	}
}

func TestEmitterTranscriptEventAlwaysFitsABatch(t *testing.T) {
	// A control byte encodes to six (`\u0001`), so 64 KB of them would encode
	// past MaxBatchBytes and buildBatch would drop the whole event as
	// unsendable — losing what the plane never refused. The cut is sized against
	// the encoded event, so it still arrives, shorter, with the after-redaction
	// count intact.
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusOK, `{"accepted":1}`))
	defer srv.Close()
	e := newTranscriptEmitter(srv, nil, nil)
	e.SetRunID("run-1")

	output := strings.Repeat("\x01", MaxTranscriptBytes)
	text := strings.Repeat("<", MaxTranscriptBytes)
	e.ToolResult("call-1", true, output, false)
	e.AssistantText(text)

	events := c.waitForEvents(t, 2)
	bodies, _, _ := c.snapshot()
	for _, body := range bodies {
		if len(body) > MaxBatchBytes {
			t.Errorf("batch is %d bytes, over MaxBatchBytes", len(body))
		}
	}
	seen := map[string]bool{}
	for _, raw := range events {
		var ev struct {
			Type        string  `json:"type"`
			Output      *string `json:"output"`
			Text        string  `json:"text"`
			OutputBytes int     `json:"outputBytes"`
			TextBytes   int     `json:"textBytes"`
			Truncated   bool    `json:"truncated"`
		}
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatal(err)
		}
		seen[ev.Type] = true
		got, want := ev.Text, ev.TextBytes
		if ev.Output != nil {
			got, want = *ev.Output, ev.OutputBytes
		}
		if want != MaxTranscriptBytes {
			t.Errorf("%s count = %d, want the after-redaction %d", ev.Type, want, MaxTranscriptBytes)
		}
		if len(got) >= MaxTranscriptBytes {
			t.Errorf("%s payload = %d bytes; the cut was not sized against the encoding", ev.Type, len(got))
		}
		if len(got) == 0 || !ev.Truncated {
			t.Errorf("%s payload = %d bytes, truncated = %v; want a shorter marked prefix", ev.Type, len(got), ev.Truncated)
		}
	}
	if !seen[TypeToolResult] || !seen[TypeAssistantText] {
		t.Fatalf("event types = %v, want both transcript types", seen)
	}
	e.Close()
}

func TestStopTranscriptDropsQueuedEventsAndRefusesLaterOnes(t *testing.T) {
	// The stop has two branches beyond the ACK itself: the queued transcript
	// events are filtered out of the buffer, and a transcript event that loses
	// the race with the ACK (enqueued after the stop) is refused. Activity
	// already queued keeps flowing.
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusOK, `{"accepted":1,"dropped":{"transcript":1}}`))
	defer srv.Close()
	var logs []string
	var logMu sync.Mutex
	e := newTranscriptEmitter(srv, nil, func(format string, args ...any) {
		logMu.Lock()
		logs = append(logs, fmt.Sprintf(format, args...))
		logMu.Unlock()
	})
	e.cfg.FlushEvery = time.Hour
	e.SetRunID("run-1")
	at := "2026-01-01T00:00:00.000Z"

	// Prime the buffer directly: the first flush must carry exactly one full
	// batch of activity filler and leave the transcript events queued.
	// Enqueueing 50 events would wake the sender and race this test's flush.
	e.mu.Lock()
	for i := 0; i < MaxBatchEvents; i++ {
		e.pending = append(e.pending, event{kind: TypeToolCall, at: at, tool: "Bash",
			callID: fmt.Sprintf("fill-%d", i), target: "ls"})
	}
	e.pending = append(e.pending,
		event{kind: TypeToolResult, at: at, callID: "queued-1", output: "one", outputBytes: 3},
		event{kind: TypeToolResult, at: at, callID: "queued-2", output: "two", outputBytes: 3},
		event{kind: TypeToolCall, at: at, tool: "Bash", callID: "kept", target: "ls"},
	)
	for _, q := range e.pending {
		e.approxBytes += q.approxBytes()
	}
	e.mu.Unlock()

	e.flush(1, time.Second)
	if e.TranscriptEnabled() {
		t.Fatal("the drop report did not stop the transcript tier")
	}
	e.mu.Lock()
	pending := append([]event(nil), e.pending...)
	e.mu.Unlock()
	if len(pending) != 1 || pending[0].callID != "kept" {
		t.Fatalf("pending after the stop = %+v, want only the queued activity call", pending)
	}

	// A transcript event arriving after the stop is refused before buffering.
	e.enqueue(event{kind: TypeToolResult, at: at, callID: "late", output: "late", outputBytes: 4})

	e.flush(1, time.Second)
	events := c.waitForEvents(t, MaxBatchEvents+1)
	if len(events) != MaxBatchEvents+1 {
		t.Fatalf("events sent = %d, want %d (the late transcript event must not be sent)", len(events), MaxBatchEvents+1)
	}
	var types []string
	for _, raw := range events {
		var ev struct {
			Type   string `json:"type"`
			CallID string `json:"callId"`
		}
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatal(err)
		}
		types = append(types, ev.Type)
		if ev.Type == TypeToolResult {
			t.Fatalf("a transcript event was sent after the stop: %+v", ev)
		}
	}
	if types[len(types)-1] != TypeToolCall {
		t.Fatalf("last event type = %q, want the queued activity call", types[len(types)-1])
	}
	e.Close()

	logMu.Lock()
	defer logMu.Unlock()
	if joined := strings.Join(logs, "\n"); !strings.Contains(joined, "3 transcript event(s) dropped") {
		t.Fatalf("the stop did not count the ACK's 1 plus the 2 queued events: %v", logs)
	}
}

func TestEmitterTranscriptRedactsBeforeTruncation(t *testing.T) {
	// A secret beyond the cap must not survive being "cut": redaction runs over
	// the full string first, and the redacted marker is what the cap then sees.
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusOK, `{"accepted":1}`))
	defer srv.Close()
	e := newTranscriptEmitter(srv, nil, nil)
	e.SetRunID("run-1")

	secret := "ghp_" + strings.Repeat("a", 36)
	output := strings.Repeat("o", MaxTranscriptBytes-20) + " " + secret
	text := strings.Repeat("t", MaxTranscriptBytes-20) + " sk-ant-" + strings.Repeat("b", 30)
	e.ToolResult("call-1", true, output, false)
	e.AssistantText(text)
	bodies := c.waitFor(t, 1)
	for _, body := range bodies {
		if strings.Contains(string(body), "ghp_") || strings.Contains(string(body), "sk-ant-") {
			t.Fatalf("a key reached the wire: %s", body)
		}
		if !strings.Contains(string(body), `"redacted":true`) {
			t.Fatalf("redacted marker missing: %s", body)
		}
	}
	e.Close()
}

func TestEmitterToolResultWithholdsEnvReadOutput(t *testing.T) {
	// `.env*`: the output is dropped entirely and the event says redacted. The
	// byte count still measures what was withheld, so a reader can see that
	// something was there.
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusOK, `{"accepted":1}`))
	defer srv.Close()
	e := newTranscriptEmitter(srv, nil, nil)
	e.SetRunID("run-1")

	output := "DATABASE_URL=postgres://user:pw@host/db\n"
	e.ToolResult("call-env", true, output, true)
	bodies := c.waitFor(t, 1)
	ev := decodeTranscript[ToolResultEvent](t, bodies, 0)
	if ev.Output != nil {
		t.Fatalf("output survived the .env drop: %q", *ev.Output)
	}
	if !ev.Redacted {
		t.Error("redacted marker missing on a withheld output")
	}
	if ev.OutputBytes != len(output) {
		t.Errorf("outputBytes = %d, want the withheld length %d", ev.OutputBytes, len(output))
	}
	if strings.Contains(string(bodies[0]), "DATABASE_URL") {
		t.Fatal("the .env output reached the wire")
	}
	e.Close()
}

func TestEmitterTranscriptOffSendsNoTranscriptEvents(t *testing.T) {
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusOK, `{"accepted":1}`))
	defer srv.Close()
	e := newTestEmitter(srv, nil, nil) // Transcript defaults to false
	e.SetRunID("run-1")

	if e.TranscriptEnabled() {
		t.Fatal("transcript read as enabled with the dial off")
	}
	e.ToolResult("call-1", true, "output", false)
	e.AssistantText("hello")
	e.ToolCall("Bash", "call-2", "ls")
	bodies := c.waitFor(t, 1)
	b := decodeBatch(t, bodies[0])
	if len(b.Events) != 1 {
		t.Fatalf("events = %d, want only the activity call", len(b.Events))
	}
	var ev struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(b.Events[0], &ev)
	if ev.Type != TypeToolCall {
		t.Fatalf("type = %q, want %q", ev.Type, TypeToolCall)
	}
	e.Close()
}

func TestEmitterStopsTranscriptWhenThePlaneReportsDrops(t *testing.T) {
	// The ACK's dropped.transcript is how a run that crossed the 50 MB cap (or
	// whose transcript dial was switched off) learns to stop. The plane has
	// already appended its own run_cap marker; the CLI stops paying to send.
	c := newCapture()
	srv := httptest.NewServer(c.handler(http.StatusOK, `{"accepted":1,"dropped":{"transcript":1}}`))
	defer srv.Close()
	var logs []string
	var logMu sync.Mutex
	e := newTranscriptEmitter(srv, nil, func(format string, args ...any) {
		logMu.Lock()
		logs = append(logs, fmt.Sprintf(format, args...))
		logMu.Unlock()
	})
	e.cfg.FlushEvery = time.Hour // the test drives flush explicitly
	e.SetRunID("run-1")

	e.ToolResult("call-1", true, "big output", false)
	e.flush(1, time.Second)
	if e.TranscriptEnabled() {
		t.Fatal("the drop report did not stop the transcript tier")
	}

	// Activity still flows; further transcript events are refused at enqueue.
	e.ToolResult("call-2", true, "more output", false)
	e.ToolCall("Bash", "call-3", "ls")
	e.flush(1, time.Second)
	bodies, _, _ := c.snapshot()
	last := decodeBatch(t, bodies[len(bodies)-1])
	if len(last.Events) != 1 {
		t.Fatalf("last batch carries %d events, want only the activity call", len(last.Events))
	}
	var ev struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(last.Events[0], &ev)
	if ev.Type != TypeToolCall {
		t.Fatalf("type = %q, want %q", ev.Type, TypeToolCall)
	}
	e.Close()

	logMu.Lock()
	defer logMu.Unlock()
	joined := strings.Join(logs, "\n")
	if n := strings.Count(joined, "stopped this run's transcript tier"); n != 1 {
		t.Fatalf("the stop was said %d time(s), want once: %v", n, logs)
	}
}

func TestEmitterTranscriptAddAndCloseNeverBlock(t *testing.T) {
	// The transcript tier is the same sender: a slow plane must not make a
	// 64 KB tool result block the parser, and Close stays bounded.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer func() {
		close(release)
		srv.Close()
	}()
	e := newTranscriptEmitter(srv, nil, nil)
	e.SetRunID("run-1")

	output := strings.Repeat("z", MaxTranscriptBytes)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			e.ToolResult(fmt.Sprintf("call-%d", i), true, output, false)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("adding transcript events blocked behind a slow send")
	}
	start := time.Now()
	e.Close()
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Close blocked for %s", elapsed)
	}
}

func TestToolResultWireShapeDistinguishesEmptyFromWithheld(t *testing.T) {
	// The plane requires outputBytes always and `redacted:true` whenever output
	// is absent, so an empty-but-present output must NOT be omitted: an omitted
	// one reads as a redaction that never happened and refuses the whole batch.
	empty := event{kind: TypeToolResult, callID: "c1", output: "", outputBytes: 0}
	raw, err := empty.marshal(1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"output":""`) {
		t.Fatalf("an empty output was omitted: %s", raw)
	}
	if strings.Contains(string(raw), `"redacted"`) {
		t.Fatalf("an empty output was marked redacted: %s", raw)
	}

	withheld := event{kind: TypeToolResult, callID: "c1", dropOutput: true, redacted: true, outputBytes: 9}
	raw, err = withheld.marshal(1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"output"`) {
		t.Fatalf("a withheld output was sent: %s", raw)
	}
	if !strings.Contains(string(raw), `"redacted":true`) || !strings.Contains(string(raw), `"outputBytes":9`) {
		t.Fatalf("a withheld output lacks its markers: %s", raw)
	}
}
