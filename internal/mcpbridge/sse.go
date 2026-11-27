package mcpbridge

// sse.go — the text/event-stream half of Streamable HTTP. The plane answers a
// request either with one JSON body or with an SSE stream; both carry the same
// JSON-RPC payloads, and both have to reach stdout one line at a time.

import (
	"bufio"
	"io"
	"strings"
)

// forEachSSEData reads an event stream and calls fn once per dispatched event
// with the event's joined data payload.
//
// One event may carry several `data:` lines; the SSE spec joins them with "\n",
// which JSON tolerates between tokens and which a multi-line JSON-RPC response
// relies on. A blank line dispatches the event, comments (":...") and the other
// fields (`event:`, `id:`, `retry:`) are ignored. A stream that ends without its
// closing blank line still delivers the last event rather than dropping a
// response that was already on the wire.
func forEachSSEData(r io.Reader, fn func([]byte) error) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var data []string

	flush := func() error {
		if len(data) == 0 {
			return nil
		}
		payload := strings.Join(data, "\n")
		data = nil
		if strings.TrimSpace(payload) == "" {
			return nil
		}
		return fn([]byte(payload))
	}

	for {
		line, err := readSSELine(br)
		if err != nil {
			if err == io.EOF {
				return flush()
			}
			return err
		}
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if field != "data" {
			continue
		}
		if found && strings.HasPrefix(value, " ") {
			value = value[1:]
		}
		data = append(data, value)
	}
}

// readSSELine reads one line, accepting all three line terminators the SSE spec
// allows: LF, CRLF and a bare CR. A trailing partial line at EOF is returned
// with no error; the following call reports EOF.
func readSSELine(br *bufio.Reader) (string, error) {
	var b strings.Builder
	for {
		c, err := br.ReadByte()
		if err != nil {
			if err == io.EOF && b.Len() > 0 {
				return b.String(), nil
			}
			return "", err
		}
		switch c {
		case '\n':
			return b.String(), nil
		case '\r':
			// CRLF is one terminator, not two lines. Peek cannot fail in a way
			// that matters here: if it does, the next ReadByte reports it.
			if next, perr := br.Peek(1); perr == nil && next[0] == '\n' {
				_, _ = br.ReadByte()
			}
			return b.String(), nil
		default:
			b.WriteByte(c)
		}
	}
}
