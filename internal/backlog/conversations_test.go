package backlog

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The planner's wake poll (studio-chat.md §4): one entry, no bodies, and the same
// auth vocabulary the summary poll uses.

func servePending(t *testing.T, status int, body string) (*httptest.Server, *http.Header) {
	t.Helper()
	got := &http.Header{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = r.Header.Clone()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func TestPollPendingDecodesTheOneEntry(t *testing.T) {
	srv, header := servePending(t, http.StatusOK,
		`{"pending":{"id":"c-1","pendingCount":3,"lastMessageAt":"2026-11-18T10:00:00.000Z"}}`)
	poll := NewConversationPoller(srv.URL+"/api/projects/demo/conversations/pending", "k-1")
	got, err := poll.PollPending(context.Background())
	if err != nil {
		t.Fatalf("PollPending: %v", err)
	}
	if got == nil || got.ID != "c-1" || got.PendingCount != 3 || got.LastMessageAt.IsZero() {
		t.Fatalf("pending = %+v", got)
	}
	if header.Get("Authorization") != "Bearer k-1" {
		t.Errorf("Authorization = %q", header.Get("Authorization"))
	}
}

func TestPollPendingNullIsIdle(t *testing.T) {
	srv, _ := servePending(t, http.StatusOK, `{"pending":null}`)
	poll := NewConversationPoller(srv.URL+"/pending", "k")
	got, err := poll.PollPending(context.Background())
	if err != nil {
		t.Fatalf("PollPending: %v", err)
	}
	if got != nil {
		t.Fatalf("pending = %+v, want nil for an idle project", got)
	}
}

func TestPollPendingClassifiesPermanentFailures(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   error
	}{
		{http.StatusUnauthorized, `{"error":{"code":"x"}}`, ErrUnauthorized},
		{http.StatusForbidden, `{"error":{"code":"x"}}`, ErrUnauthorized},
		// A project-scoped key used against the wrong project must stop the
		// planner loudly, not retry as a blip.
		{http.StatusBadRequest, `{"error":{"code":"project_required"}}`, ErrProjectRequired},
		{http.StatusTooManyRequests, `{"error":{"code":"x"}}`, ErrConversationPollRateLimited},
	}
	for _, tc := range cases {
		srv, _ := servePending(t, tc.status, tc.body)
		_, err := NewConversationPoller(srv.URL+"/pending", "k").PollPending(context.Background())
		if !errors.Is(err, tc.want) {
			t.Errorf("HTTP %d err = %v, want %v", tc.status, err, tc.want)
		}
	}
}

// An undecodable 200 is an error (not an idle answer), and a decoded entry with
// no id names nothing claimable, so it is the idle answer.
func TestPollPendingDecodeAndEmptyID(t *testing.T) {
	srv, _ := servePending(t, http.StatusOK, `not json`)
	if _, err := NewConversationPoller(srv.URL+"/pending", "k").PollPending(context.Background()); err == nil {
		t.Error("an undecodable 200 was accepted as an idle answer")
	}

	srv, _ = servePending(t, http.StatusOK, `{"pending":{"id":"","pendingCount":2}}`)
	got, err := NewConversationPoller(srv.URL+"/pending", "k").PollPending(context.Background())
	if err != nil || got != nil {
		t.Errorf("pending = %+v, err = %v; an entry with no id must read as idle", got, err)
	}
}

func TestConversationPollerNotWired(t *testing.T) {
	if _, err := NewConversationPoller("", "").PollPending(context.Background()); !errors.Is(err, ErrNotWired) {
		t.Errorf("err = %v, want ErrNotWired", err)
	}
}
