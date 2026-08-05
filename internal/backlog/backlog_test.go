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

// A real /api/backlog-summary response: a plain JSON object carrying the freshness
// snapshot plus the console-pause flag (CLA-76). Not paused.
const summaryJSON = `{"version":603,"counts":{"backlog":15,"ready":6,"in_progress":1,"in_review":0,"blocked":0,"parked":7,"done":99},"claimable":4,"openQuestions":2,"loopPaused":false}`

// The same shape with the console pause engaged.
const summaryPausedJSON = `{"version":604,"counts":{"ready":6,"in_progress":1},"claimable":4,"openQuestions":2,"loopPaused":true}`

func wantSummary(t *testing.T, got Summary, ver, ready, claimable, inProgress, openQ int, paused bool) {
	t.Helper()
	if got.Version != ver || got.Ready != ready || got.Claimable != claimable ||
		got.InProgress != inProgress || got.OpenQuestions != openQ || got.Paused != paused {
		t.Fatalf("summary mismatch: got %+v, want {Version:%d Ready:%d Claimable:%d InProgress:%d OpenQuestions:%d Paused:%t}",
			got, ver, ready, claimable, inProgress, openQ, paused)
	}
}

func TestParseSummary_Counts(t *testing.T) {
	got, err := parseSummary([]byte(summaryJSON))
	if err != nil {
		t.Fatalf("parseSummary: %v", err)
	}
	wantSummary(t, got, 603, 6, 4, 1, 2, false)
}

// The load-bearing field for CLA-130: loopPaused must parse into Summary.Paused.
func TestParseSummary_LoopPaused(t *testing.T) {
	got, err := parseSummary([]byte(summaryPausedJSON))
	if err != nil {
		t.Fatalf("parseSummary: %v", err)
	}
	wantSummary(t, got, 604, 6, 4, 1, 2, true)
	if !got.Paused {
		t.Fatal("loopPaused:true must set Summary.Paused")
	}
}

// A payload with no loopPaused field (e.g. an older plane) parses as not paused —
// the driver must never spuriously pause on a missing flag.
func TestParseSummary_MissingPauseIsFalse(t *testing.T) {
	got, err := parseSummary([]byte(`{"version":1,"counts":{"ready":0,"in_progress":0},"claimable":0,"openQuestions":0}`))
	if err != nil {
		t.Fatalf("parseSummary: %v", err)
	}
	if got.Paused {
		t.Fatal("a missing loopPaused field must parse as not paused")
	}
}

func TestParseSummary_Empty(t *testing.T) {
	if _, err := parseSummary(nil); err == nil {
		t.Fatal("expected error for empty body, got nil")
	}
}

func TestParseSummary_Garbage(t *testing.T) {
	if _, err := parseSummary([]byte("not json")); err == nil {
		t.Fatal("expected error for non-JSON body, got nil")
	}
}

// New with no creds must return the not-wired poller — the thing that reports
// ErrNotWired, so the thing that flips the loop into blind mode.
func TestNew_NotWiredWithoutCreds(t *testing.T) {
	cases := []struct{ url, key string }{
		{"", ""},
		{"https://clankerbar.com/api/backlog-summary", ""},
		{"", "secret"},
	}
	for _, c := range cases {
		p := New(c.url, c.key)
		if _, ok := p.(notWired); !ok {
			t.Fatalf("New(%q,%q): want notWired, got %T", c.url, c.key, p)
		}
		if _, err := p.Poll(context.Background()); !errors.Is(err, ErrNotWired) {
			t.Fatalf("New(%q,%q).Poll: want ErrNotWired, got %v", c.url, c.key, err)
		}
	}
}

// New with creds returns a real HTTP poller (never notWired) that GETs and parses
// live counts + pause from the endpoint with a Bearer key.
func TestNew_WiredPollsLiveCounts(t *testing.T) {
	var gotAuth, gotMethod, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, summaryPausedJSON)
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
	wantSummary(t, sum, 604, 6, 4, 1, 2, true)
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotAuth != "Bearer secret-key" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer secret-key")
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept header = %q, want application/json", gotAccept)
	}
}

// A live poll failure (server error) must map to an ordinary, non-fatal error — NOT
// ErrNotWired — so the loop backs off and retries rather than dropping into blind
// mode.
func TestPoll_ServerErrorIsNonFatalNotNotWired(t *testing.T) {
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

// An account key hitting this project-scoped route gets 400 `project_required`. That
// is a persistent wiring mismatch the harness sessions share (same account key), not a
// blip: it must map to the distinct ErrProjectRequired sentinel so the loop hard-stops
// loudly instead of blind-draining doomed sessions — NOT ErrNotWired (CLA-133, which
// reverses CLA-130's blind-drain mapping).
func TestPoll_ProjectRequiredMapsToProjectRequired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":"project_required","message":"needs a project-scoped API key"}}`)
	}))
	defer srv.Close()

	_, err := New(srv.URL, "account-key").Poll(context.Background())
	if !errors.Is(err, ErrProjectRequired) {
		t.Fatalf("project_required must map to ErrProjectRequired, got %v", err)
	}
	if errors.Is(err, ErrNotWired) {
		t.Fatalf("project_required must NOT map to ErrNotWired (would blind-drain doomed sessions), got %v", err)
	}
}

// A different 400 (some other bad-request code) is NOT the account-key case, so it
// stays an ordinary retryable error — never ErrNotWired.
func TestPoll_OtherBadRequestStaysRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":"something_else","message":"nope"}}`)
	}))
	defer srv.Close()

	_, err := New(srv.URL, "k").Poll(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, ErrNotWired) {
		t.Fatalf("a non-project_required 400 must stay retryable, not ErrNotWired: %v", err)
	}
}

// A revoked/wrong API key answers 401 or 403. That is a PERMANENT auth failure, not
// a transient blip — and NOT a blind-drain cue (the harness sessions carry the same
// bad key): it must map to the distinct ErrUnauthorized sentinel so the loop hard-
// stops loudly instead of blind-draining or idle-polling a dead key (CLA-132, which
// reverses CLA-131 finding #5's ErrNotWired mapping).
func TestPoll_AuthRejectedMapsToUnauthorized(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			_, _ = io.WriteString(w, `{"error":{"code":"unauthorized","message":"bad key"}}`)
		}))
		_, err := New(srv.URL, "revoked-key").Poll(context.Background())
		srv.Close()
		if !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("HTTP %d must map to ErrUnauthorized, got %v", code, err)
		}
		if errors.Is(err, ErrNotWired) {
			t.Fatalf("HTTP %d must NOT map to ErrNotWired (would blind-drain a dead key), got %v", code, err)
		}
	}
}

func TestNew_TrimsTrailingSlash(t *testing.T) {
	p, ok := New("https://clankerbar.com/api/backlog-summary/", "k").(*httpPoller)
	if !ok {
		t.Fatal("want *httpPoller")
	}
	if p.endpoint != "https://clankerbar.com/api/backlog-summary" {
		t.Errorf("endpoint = %q, want trailing slash trimmed", p.endpoint)
	}
}

// A plane that predates the slug-ful route (CLA-141) 404s it. The poller must fall
// back to the legacy slug-less route — permanently — instead of feeding the loop an
// idle-forever generic error against a URL the plane will never serve.
func TestPoll_SlugfulRoute404FallsBackToLegacy(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path != "/api/backlog-summary" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"version":7,"counts":{"ready":1,"in_progress":0},"claimable":1,"openQuestions":0,"loopPaused":false}`))
	}))
	defer srv.Close()

	p := New(srv.URL+"/api/projects/proj/backlog-summary", "clk_test")
	sum, err := p.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll() error = %v, want fallback success", err)
	}
	if sum.Claimable != 1 || sum.Version != 7 {
		t.Errorf("Poll() = %+v, want the legacy route's summary", sum)
	}

	// Permanent: the next poll goes straight to the legacy route, no 404 round-trip.
	if _, err := p.Poll(context.Background()); err != nil {
		t.Fatalf("second Poll() error = %v", err)
	}
	want := []string{"/api/projects/proj/backlog-summary", "/api/backlog-summary", "/api/backlog-summary"}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("paths = %v, want %v", paths, want)
		}
	}
}

// After the 404 fallback, an ACCOUNT key on the legacy route still surfaces the
// loud project_required hard stop — the fallback restores pre-upgrade behaviour,
// it does not swallow the misconfiguration.
func TestPoll_FallbackThenProjectRequiredStillHardStops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/backlog-summary" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"project_required","message":"nope"}}`))
	}))
	defer srv.Close()

	p := New(srv.URL+"/api/projects/proj/backlog-summary", "clk_test")
	_, err := p.Poll(context.Background())
	if !errors.Is(err, ErrProjectRequired) {
		t.Fatalf("Poll() error = %v, want ErrProjectRequired", err)
	}
}

// A 404 on the LEGACY route has no further fallback: it stays an ordinary
// (retryable) error, and the message names the endpoint so the cause is diagnosable.
func TestPoll_Legacy404IsOrdinaryErrorNamingTheEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	p := New(srv.URL+"/api/backlog-summary", "clk_test")
	_, err := p.Poll(context.Background())
	if err == nil || errors.Is(err, ErrNotWired) || errors.Is(err, ErrProjectRequired) {
		t.Fatalf("Poll() error = %v, want a plain retryable error", err)
	}
	if !strings.Contains(err.Error(), "/api/backlog-summary") {
		t.Errorf("error should name the endpoint; got %q", err.Error())
	}
}

// A redirect must not be able to move a request that is already carrying the
// bearer token to a cleartext destination (CLA-257). Go strips Authorization on a
// cross-host hop but FORWARDS it on an https -> http hop to the same host, so the
// scheme floor has to be re-checked per hop rather than only at config time.
func TestPollRefusesARedirectOffTheCredentialFloor(t *testing.T) {
	var hops int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops++
		http.Redirect(w, r, "http://attacker.example/api/backlog-summary", http.StatusFound)
	}))
	defer srv.Close()

	// The first hop is loopback http, which the floor allows; the redirect is not.
	_, err := New(srv.URL+"/api/backlog-summary", "key-123").Poll(context.Background())
	if err == nil {
		t.Fatal("Poll followed a redirect to a cleartext non-loopback host")
	}
	if !strings.Contains(err.Error(), "refusing redirect") {
		t.Errorf("Poll error = %v, want the redirect refusal", err)
	}
	if hops != 1 {
		t.Errorf("hops = %d, want the request to stop at the first response", hops)
	}
}
