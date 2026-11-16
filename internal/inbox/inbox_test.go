package inbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPollReadsTheRunInboxWithTheRunCredential(t *testing.T) {
	var gotPath, gotAuth, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotQuery = r.URL.Path, r.Header.Get("Authorization"), r.URL.RawQuery
		fmt.Fprint(w, `{"runId":"abc","messages":[
			{"id":"m1","seq":1,"at":"2026-11-16T12:00:00.000Z","body":"first"},
			{"id":"m2","seq":2,"at":"2026-11-16T12:00:01.000Z","body":"second"}
		],"next":2,"ended":false}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL+"/", "clk_key", nil)
	page, err := c.Poll(context.Background(), "abc.sec.ret", 0)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if gotPath != "/api/runs/abc.sec.ret/inbox" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer clk_key" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotQuery != "after=0" {
		t.Errorf("query = %q", gotQuery)
	}
	if len(page.Messages) != 2 || page.Messages[0].Body != "first" || page.Messages[1].Seq != 2 {
		t.Fatalf("messages = %+v", page.Messages)
	}
	if page.Messages[0].At.IsZero() {
		t.Error("the message timestamp was not parsed")
	}
	if page.Ended {
		t.Error("ended = true for a running run")
	}
}

func TestPollSendsTheCursor(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		fmt.Fprint(w, `{"messages":[],"next":7,"ended":true}`)
	}))
	defer srv.Close()

	page, err := NewClient(srv.URL, "k", nil).Poll(context.Background(), "run", 7)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if gotQuery != "after=7" {
		t.Errorf("query = %q, want after=7", gotQuery)
	}
	if !page.Ended {
		t.Error("ended = false for an ended run")
	}
}

func TestPollSurfacesThePlaneErrorCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"run_not_found"}}`)
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "k", nil).Poll(context.Background(), "run", 0)
	var api apiError
	if !errors.As(err, &api) {
		t.Fatalf("err = %v, want apiError", err)
	}
	if api.status != 404 || api.code != "run_not_found" {
		t.Errorf("api = %+v", api)
	}
}

func TestAckPostsTheWatermark(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		fmt.Fprint(w, `{"acked":2,"through":3}`)
	}))
	defer srv.Close()

	if err := NewClient(srv.URL, "clk_key", nil).Ack(context.Background(), "run-1", 3); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if gotPath != "/api/runs/run-1/inbox/ack" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer clk_key" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotBody["seq"] != 3 {
		t.Errorf("body = %v", gotBody)
	}
}

// The poller's two promises: order, and once per seq even when the plane keeps offering the
// same message because the ACK never landed.
func TestPollerDeliversInOrderAndExactlyOnce(t *testing.T) {
	var mu sync.Mutex
	acked := int64(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ack") {
			var body struct {
				Seq int64 `json:"seq"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			acked = body.Seq
			mu.Unlock()
			fmt.Fprint(w, `{"acked":1}`)
			return
		}
		// The ACK is deliberately ignored: every poll re-offers both messages, so only the
		// poller's own cursor can keep delivery exactly-once.
		fmt.Fprint(w, `{"messages":[
			{"id":"m1","seq":1,"at":"2026-11-16T12:00:00Z","body":"one"},
			{"id":"m2","seq":2,"at":"2026-11-16T12:00:01Z","body":"two"}
		],"next":2,"ended":false}`)
	}))
	defer srv.Close()

	p := NewPoller(Config{Origin: srv.URL, APIKey: "k", RunID: "run", Interval: 5 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu2 sync.Mutex
	var got []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx, func(m Message) error {
			mu2.Lock()
			got = append(got, m.Body)
			mu2.Unlock()
			return nil
		})
	}()

	deadline := time.After(2 * time.Second)
	for {
		mu2.Lock()
		n := len(got)
		mu2.Unlock()
		if n >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("delivered %v after 2s", got)
		case <-time.After(5 * time.Millisecond):
		}
	}
	// Several more poll cycles re-offer both messages; none may be written again.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	mu2.Lock()
	defer mu2.Unlock()
	if len(got) != 2 {
		t.Fatalf("delivered %v, want each message exactly once", got)
	}
	if got[0] != "one" || got[1] != "two" {
		t.Fatalf("delivery order = %v", got)
	}
	mu.Lock()
	if acked != 2 {
		t.Errorf("acked watermark = %d, want 2", acked)
	}
	mu.Unlock()
}

func TestPollerSurvivesPollFailuresAndRecovers(t *testing.T) {
	var mu sync.Mutex
	fail := true
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ack") {
			fmt.Fprint(w, `{}`)
			return
		}
		mu.Lock()
		calls++
		f := fail
		mu.Unlock()
		if f {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"code":"unavailable"}}`)
			return
		}
		fmt.Fprint(w, `{"messages":[{"id":"m1","seq":1,"at":"2026-11-16T12:00:00Z","body":"recovered"}],"next":1,"ended":false}`)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := NewPoller(Config{Origin: srv.URL, APIKey: "k", RunID: "run", Interval: 5 * time.Millisecond})

	got := make(chan string, 4)
	go p.Run(ctx, func(m Message) error { got <- m.Body; return nil })

	time.Sleep(40 * time.Millisecond)
	mu.Lock()
	fail = false
	mu.Unlock()

	select {
	case body := <-got:
		if body != "recovered" {
			t.Fatalf("body = %q", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the poller never recovered after a failed poll")
	}
	mu.Lock()
	if calls < 2 {
		t.Errorf("poll calls = %d, want at least 2 (fail then recover)", calls)
	}
	mu.Unlock()
}

func TestPollerStopsWhenTheSessionInputCloses(t *testing.T) {
	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ack") {
			fmt.Fprint(w, `{}`)
			return
		}
		polls++
		fmt.Fprint(w, `{"messages":[{"id":"m1","seq":1,"at":"2026-11-16T12:00:00Z","body":"x"}],"next":1,"ended":false}`)
	}))
	defer srv.Close()

	p := NewPoller(Config{Origin: srv.URL, APIKey: "k", RunID: "run", Interval: 5 * time.Millisecond})
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(context.Background(), func(Message) error { return errors.New("stdin closed") })
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after the deliverer refused")
	}
	if polls > 2 {
		t.Errorf("polled %d times after the input closed", polls)
	}
}
