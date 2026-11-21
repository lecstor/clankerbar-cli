package harness

import (
	"encoding/json"
	"errors"
	"io"
	"sync"
)

// errSessionInputClosed is what a delivery gets after the session's stdin was closed: the run is
// over, so the message stays queued on the plane for the next process.
var errSessionInputClosed = errors.New("session stdin is closed")

// sessionInput serialises the stream-json user messages a claude session reads from stdin.
type sessionInput struct {
	mu      sync.Mutex
	w       io.WriteCloser
	pending int
	closed  bool
}

func newSessionInput(w io.WriteCloser) *sessionInput {
	return &sessionInput{w: w}
}

// userMessage is Claude Code's stream-json input shape for a user turn. The type descriptions in
// the CLI spell out that a client may write exactly one StdinMessage per line, that user messages
// start turns, and that initialize is optional (the first user message initialises with defaults)
// — so no handshake line is needed.
type userMessage struct {
	Type    string `json:"type"`
	Message struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

func marshalUserMessage(text string) ([]byte, error) {
	msg := userMessage{Type: "user"}
	msg.Message.Role = "user"
	msg.Message.Content = []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{{Type: "text", Text: text}}
	return json.Marshal(msg)
}

// writeUser writes one user turn.
func (s *sessionInput) writeUser(text string) error {
	line, err := marshalUserMessage(text)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errSessionInputClosed
	}
	if _, err := s.w.Write(append(line, '\n')); err != nil {
		s.closed = true
		return err
	}
	s.pending++
	return nil
}

// endTurn retires one outstanding turn and reports whether that was the LAST one. True means the
// session is done: the caller must stop delivery (the stream is closed inside this call).
func (s *sessionInput) endTurn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return true
	}
	if s.pending > 0 {
		s.pending--
	}
	if s.pending == 0 {
		s.closeLocked()
		return true
	}
	return false
}

// close closes the pipe if it is still open. Idempotent; safe from any goroutine.
func (s *sessionInput) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeLocked()
}

func (s *sessionInput) closeLocked() {
	if s.closed {
		return
	}
	s.closed = true
	_ = s.w.Close()
}
