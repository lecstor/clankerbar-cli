package backlog

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A real get_backlog_summary response, as the MCP HTTP transport returns it: an
// SSE frame whose `data:` line carries the JSON-RPC envelope, whose tool content is
// itself a JSON document.
const sseFixture = "event: message\n" +
	`data: {"result":{"content":[{"type":"text","text":"{\n  \"version\": 603,\n  \"counts\": {\n    \"backlog\": 15,\n    \"ready\": 6,\n    \"in_progress\": 1,\n    \"in_review\": 0,\n    \"blocked\": 0,\n    \"parked\": 7,\n    \"done\": 99\n  },\n  \"claimable\": 4,\n  \"openQuestions\": 2\n}"}]},"jsonrpc":"2.0","id":1}` + "\n"

// The same envelope delivered as a plain JSON body (the transport may answer either way).
const jsonFixture = `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"version\":42,\"counts\":{\"ready\":3,\"in_progress\":2},\"claimable\":3,\"openQuestions\":0}"}]}}`

func wantSummary(t *testing.T, got Summary, ver, ready, claimable, inProgress, openQ int) {
	t.Helper()
	if got.Version != ver || got.Ready != ready || got.Claimable != claimable ||
		got.InProgress != inProgress || got.OpenQuestions != openQ {
		t.Fatalf("summary mismatch: got %+v, want {Version:%d Ready:%d Claimable:%d InProgress:%d OpenQuestions:%d}",
			got, ver, ready, claimable, inProgress, openQ)
	}
}

func TestParseSummary_SSE(t *testing.T) {
	got, err := parseSummary([]byte(sseFixture))
	if err != nil {
		t.Fatalf("parseSummary(SSE): %v", err)
	}
	wantSummary(t, got, 603, 6, 4, 1, 2)
}

func TestParseSummary_PlainJSON(t *testing.T) {
	got, err := parseSummary([]byte(jsonFixture))
	if err != nil {
		t.Fatalf("parseSummary(JSON): %v", err)
	}
	wantSummary(t, got, 42, 3, 3, 2, 0)
}

func TestParseSummary_ToolError(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"boom"}]}}`
	if _, err := parseSummary([]byte(body)); err == nil {
		t.Fatal("expected error for isError result, got nil")
	}
}

func TestParseSummary_RPCError(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"unauthorized"}}`
	if _, err := parseSummary([]byte(body)); err == nil {
		t.Fatal("expected error for rpc error, got nil")
	}
}

func TestParseSummary_Empty(t *testing.T) {
	if _, err := parseSummary(nil); err == nil {
		t.Fatal("expected error for empty body, got nil")
	}
}

// New with no creds must return the not-wired poller — the only thing that reports
// ErrNotWired, so the only thing that flips the loop into blind mode.
func TestNew_NotWiredWithoutCreds(t *testing.T) {
	cases := []struct{ base, key string }{
		{"", ""},
		{"https://clankerbar.com/mcp/x", ""},
		{"", "secret"},
	}
	for _, c := range cases {
		p := New(c.base, c.key)
		if _, ok := p.(notWired); !ok {
			t.Fatalf("New(%q,%q): want notWired, got %T", c.base, c.key, p)
		}
		if _, err := p.Poll(context.Background()); !errors.Is(err, ErrNotWired) {
			t.Fatalf("New(%q,%q).Poll: want ErrNotWired, got %v", c.base, c.key, err)
		}
	}
}

// New with creds returns a real HTTP poller (never notWired) that fetches and
// parses live counts from the endpoint.
func TestNew_WiredPollsLiveCounts(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseFixture)
	}))
	defer srv.Close()

	p := New(srv.URL, "secret-key")
	if _, ok := p.(*httpPoller); !ok {
		t.Fatalf("New with creds: want *httpPoller, got %T", p)
	}

	sum, err := p.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	wantSummary(t, sum, 603, 6, 4, 1, 2)
	if gotAuth != "Bearer secret-key" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer secret-key")
	}
	if !strings.Contains(gotBody, "get_backlog_summary") {
		t.Errorf("request body missing tool name: %q", gotBody)
	}
}

// A live poll failure (server error) must map to an ordinary, non-fatal error —
// NOT ErrNotWired — so the loop backs off and retries rather than dropping into
// blind mode.
func TestPoll_ErrorIsNonFatalNotNotWired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := New(srv.URL, "secret-key").Poll(context.Background())
	if err == nil {
		t.Fatal("expected error from failing poll, got nil")
	}
	if errors.Is(err, ErrNotWired) {
		t.Fatalf("poll error must not be ErrNotWired (would trigger blind mode): %v", err)
	}
}

func TestBacklogEndpointTrimsTrailingSlash(t *testing.T) {
	p, ok := New("https://clankerbar.com/mcp/proj/", "k").(*httpPoller)
	if !ok {
		t.Fatal("want *httpPoller")
	}
	if p.endpoint != "https://clankerbar.com/mcp/proj" {
		t.Errorf("endpoint = %q, want trailing slash trimmed", p.endpoint)
	}
}
