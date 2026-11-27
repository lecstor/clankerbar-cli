package mcpbridge

import (
	"errors"
	"strings"
	"testing"
)

func collectSSE(t *testing.T, input string) []string {
	t.Helper()
	var got []string
	if err := forEachSSEData(strings.NewReader(input), maxResponseBytes, func(payload []byte) error {
		got = append(got, string(payload))
		return nil
	}); err != nil {
		t.Fatalf("forEachSSEData: %v", err)
	}
	return got
}

func TestForEachSSEDataJoinsMultiLineEvents(t *testing.T) {
	input := strings.Join([]string{
		": keepalive comment",
		"event: message",
		"id: 7",
		`data: {"jsonrpc":"2.0","id":1,"result":null}`,
		"",
		"retry: 10",
		`data: {"jsonrpc":"2.0",`,
		`data: "id":2,"result":null}`,
		"",
	}, "\r\n")

	got := collectSSE(t, input)
	want := []string{
		`{"jsonrpc":"2.0","id":1,"result":null}`,
		"{\"jsonrpc\":\"2.0\",\n\"id\":2,\"result\":null}",
	}
	if len(got) != len(want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestForEachSSEDataBareCRAndEOFFlush(t *testing.T) {
	// A bare CR is a line terminator per the SSE spec, and a stream that ends
	// without its closing blank line still delivers the last payload.
	got := collectSSE(t, "data: one\r\rdata: two\r\rdata: tail")
	want := []string{"one", "two", "tail"}
	if len(got) != len(want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestForEachSSEDataSkipsEmptyEvents(t *testing.T) {
	if got := collectSSE(t, "data:\n\nevent: ping\n\ndata: \n\n"); len(got) != 0 {
		t.Errorf("empty events produced payloads: %#v", got)
	}
}

func TestForEachSSEDataStripsOneLeadingSpace(t *testing.T) {
	got := collectSSE(t, "data:  two spaces\n\n")
	if len(got) != 1 || got[0] != " two spaces" {
		t.Errorf("payload = %#v, want one retained space", got)
	}
}

func TestForEachSSEDataPropagatesErrors(t *testing.T) {
	sentinel := errors.New("stop")
	err := forEachSSEData(strings.NewReader("data: one\n\n"), maxResponseBytes, func([]byte) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the callback's error", err)
	}
}

// A hostile stream cannot make the relay buffer without limit: both one long
// line and a multi-line event are bounded, with the same wording as the JSON
// path's runaway guard.
func TestForEachSSEDataBoundsEventSize(t *testing.T) {
	t.Run("one long line", func(t *testing.T) {
		input := "data: " + strings.Repeat("x", 64) + "\n\n"
		err := forEachSSEData(strings.NewReader(input), 32, func([]byte) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "exceeded 32 bytes") {
			t.Fatalf("err = %v, want a size refusal", err)
		}
	})

	t.Run("multi-line event", func(t *testing.T) {
		input := "data: " + strings.Repeat("x", 60) + "\ndata: " + strings.Repeat("y", 60) + "\n\n"
		err := forEachSSEData(strings.NewReader(input), 100, func([]byte) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "exceeded 100 bytes") {
			t.Fatalf("err = %v, want a size refusal", err)
		}
	})

	t.Run("under the bound still dispatches", func(t *testing.T) {
		got := collectSSE(t, "data: two\n\ndata: lines\n\n")
		if len(got) != 2 || got[0] != "two" || got[1] != "lines" {
			t.Errorf("events = %#v", got)
		}
	})
}
