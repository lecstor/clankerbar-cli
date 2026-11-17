package events

import (
	"encoding/json"
	"fmt"
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
	bodies := c.waitFor(t, 1)
	for _, raw := range decodeBatch(t, bodies[0]).Events {
		var ev struct {
			Type   string  `json:"type"`
			Output *string `json:"output"`
			Text   string  `json:"text"`
		}
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatal(err)
		}
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
