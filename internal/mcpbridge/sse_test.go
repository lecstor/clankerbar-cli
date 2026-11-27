package mcpbridge

import (
	"errors"
	"strings"
	"testing"
)

func collectSSE(t *testing.T, input string) []string {
	t.Helper()
	var got []string
	if err := forEachSSEData(strings.NewReader(input), func(payload []byte) error {
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
	err := forEachSSEData(strings.NewReader("data: one\n\n"), func([]byte) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the callback's error", err)
	}
}
