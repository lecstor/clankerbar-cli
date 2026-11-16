package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The stream-json input's turn accounting: every written user message is one outstanding
// turn, and the session ends only when a result retires the last one (CLA-606).
func TestSessionInputRetiresTurnsAndClosesOnTheLast(t *testing.T) {
	var buf bytes.Buffer
	in := newSessionInput(&closeBuffer{buf: &buf})

	if err := in.writeUser("prompt"); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	if err := in.writeUser("steer"); err != nil {
		t.Fatalf("write steer: %v", err)
	}
	if got := in.endTurn(); got {
		t.Fatal("the first result closed the input while a steering turn was outstanding")
	}
	if !in.endTurn() {
		t.Fatal("the last result did not close the input — the CLI would wait for stdin forever")
	}
	if err := in.writeUser("too late"); !errors.Is(err, errSessionInputClosed) {
		t.Fatalf("write after close = %v, want errSessionInputClosed", err)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("wrote %d lines, want 2", len(lines))
	}
	var msg userMessage
	if err := json.Unmarshal([]byte(lines[1]), &msg); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if msg.Type != "user" || msg.Message.Role != "user" || msg.Message.Content[0].Text != "steer" {
		t.Fatalf("second line = %+v", msg)
	}
}

// A write error marks the input dead, so the poller stops instead of retrying a broken pipe.
func TestSessionInputMarksItselfClosedOnAWriteError(t *testing.T) {
	in := newSessionInput(&failingWriter{})
	if err := in.writeUser("x"); err == nil {
		t.Fatal("a failing pipe returned nil")
	}
	if err := in.writeUser("y"); !errors.Is(err, errSessionInputClosed) {
		t.Fatalf("second write = %v, want errSessionInputClosed", err)
	}
	if !in.endTurn() {
		t.Fatal("endTurn on a broken input must report the session over, so delivery stops")
	}
}

// The argv no longer carries the prompt: it must travel on stdin (CLA-606).
func TestClaudeArgsUseStreamJSONInput(t *testing.T) {
	joined := strings.Join(claudeArgs(Invocation{Prompt: "Work the backlog."}), " ")
	if !strings.Contains(joined, "--input-format stream-json") {
		t.Errorf("args = %q, want --input-format stream-json", joined)
	}
	if strings.Contains(joined, "Work the backlog.") {
		t.Errorf("args = %q, want no prompt argument", joined)
	}
}

type closeBuffer struct {
	buf *bytes.Buffer
}

func (c *closeBuffer) Write(p []byte) (int, error) { return c.buf.Write(p) }
func (c *closeBuffer) Close() error                { return nil }

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }
func (failingWriter) Close() error              { return nil }
