package harness

// Console -> agent delivery for opencode (CLA-911): a message is steered into
// the live session by POSTing to the private server's session API. These tests
// drive the delivery goroutine against a fake plane inbox and a fake opencode
// server; the real binary is exercised by the conformance suite, not here.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/inbox"
)

// fakePlane is the plane's run inbox: per-run messages, an ACK watermark and a
// request log. Delivery is offered exactly as the real plane offers it —
// messages after the cursor that the watermark does not already cover.
type fakePlane struct {
	mu     sync.Mutex
	msgs   map[string][]inbox.Message
	acked  map[string]int64
	ackLog map[string][]int64
	polls  map[string]int
	ends   map[string]bool
}

func newFakePlane() *fakePlane {
	return &fakePlane{
		msgs:   map[string][]inbox.Message{},
		acked:  map[string]int64{},
		ackLog: map[string][]int64{},
		polls:  map[string]int{},
		ends:   map[string]bool{},
	}
}

// add appends messages to a run, numbered from the next seq.
func (p *fakePlane) add(runID string, bodies ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seq := int64(len(p.msgs[runID]))
	for _, body := range bodies {
		seq++
		p.msgs[runID] = append(p.msgs[runID], inbox.Message{
			Seq: seq, ID: fmt.Sprintf("%s-%d", runID, seq), Body: body,
			At: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC),
		})
	}
}

func (p *fakePlane) acks(runID string) []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int64(nil), p.ackLog[runID]...)
}

func (p *fakePlane) watermark(runID string) int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.acked[runID]
}

func (p *fakePlane) pollCount(runID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.polls[runID]
}

func (p *fakePlane) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		path := r.URL.Path
		if !strings.HasPrefix(path, "/api/runs/") {
			http.NotFound(w, r)
			return
		}
		rest := strings.TrimPrefix(path, "/api/runs/")
		if strings.HasSuffix(rest, "/inbox/ack") {
			runID := strings.TrimSuffix(rest, "/inbox/ack")
			var body struct {
				Seq int64 `json:"seq"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Seq > p.acked[runID] {
				p.acked[runID] = body.Seq
			}
			p.ackLog[runID] = append(p.ackLog[runID], body.Seq)
			fmt.Fprint(w, `{"acked":1}`)
			return
		}
		if !strings.HasSuffix(rest, "/inbox") {
			http.NotFound(w, r)
			return
		}
		runID := strings.TrimSuffix(rest, "/inbox")
		p.polls[runID]++
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		wire := make([]map[string]any, 0, len(p.msgs[runID]))
		for _, m := range p.msgs[runID] {
			if m.Seq > after && m.Seq > p.acked[runID] {
				wire = append(wire, map[string]any{
					"id": m.ID, "seq": m.Seq, "at": m.At.Format(time.RFC3339), "body": m.Body,
				})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages": wire, "next": after, "ended": p.ends[runID],
		})
	}
}

// fakeOpencode is the private server's session API: it records every prompt and
// answers the queued statuses (the last one repeats), so a test can make one
// delivery fail and the next succeed.
type fakeOpencode struct {
	mu       sync.Mutex
	posts    []promptPost
	statuses []int
}

type promptPost struct {
	Session  string
	Text     string
	Delivery string
	User     string
	Pass     string
}

func (f *fakeOpencode) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/api/session/") || !strings.HasSuffix(r.URL.Path, "/prompt") {
			http.NotFound(w, r)
			return
		}
		sid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/session/"), "/prompt")
		var body struct {
			Text     string `json:"text"`
			Delivery string `json:"delivery"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		user, pass, _ := r.BasicAuth()
		f.mu.Lock()
		status := http.StatusOK
		if len(f.statuses) > 0 {
			status = f.statuses[0]
			f.statuses = f.statuses[1:]
		}
		f.posts = append(f.posts, promptPost{Session: sid, Text: body.Text, Delivery: body.Delivery, User: user, Pass: pass})
		f.mu.Unlock()
		if status != http.StatusOK {
			http.Error(w, "refused", status)
			return
		}
		fmt.Fprint(w, `{"ok":true}`)
	}
}

func (f *fakeOpencode) delivered() []promptPost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]promptPost(nil), f.posts...)
}

// opencodeTestServer is an httptest server wrapped as the adapter's private
// server: only url and password are ever read by the delivery path.
func opencodeTestServer(t *testing.T, f *fakeOpencode) *opencodeServer {
	t.Helper()
	ts := httptest.NewServer(f.handler())
	t.Cleanup(ts.Close)
	return &opencodeServer{url: ts.URL, password: "pw"}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// startTestDelivery builds a delivery for runIDs and gives it a live target.
// The returned inbox's goroutine is stopped when the test ends.
func startTestDelivery(t *testing.T, planeURL string, runIDs chan string, srv *opencodeServer, sid string) *opencodeInbox {
	t.Helper()
	d := newOpencodeInbox(context.Background(), nil, InboxConfig{
		Origin: planeURL, APIKey: "clk_test", PollInterval: 5 * time.Millisecond,
	}, runIDs)
	// Not a production cadence: the retry path must be observable in a test.
	d.retry = 5 * time.Millisecond
	d.attach(srv)
	d.setSession(sid)
	t.Cleanup(d.close)
	return d
}

// Several messages are steered in the plane's order, one at a time, and each is
// ACKed only after its 2xx.
func TestOpencodeDeliversMessagesInOrder(t *testing.T) {
	plane := newFakePlane()
	plane.add("run.cred", "one", "two", "three")
	planeTS := httptest.NewServer(plane.handler())
	defer planeTS.Close()

	oc := &fakeOpencode{}
	srv := opencodeTestServer(t, oc)
	runIDs := make(chan string, 4)
	runIDs <- "run.cred"
	startTestDelivery(t, planeTS.URL, runIDs, srv, "ses_abc")

	waitUntil(t, "three delivered prompts", func() bool { return len(oc.delivered()) >= 3 })

	posts := oc.delivered()
	if len(posts) != 3 {
		t.Fatalf("posts = %d (%+v), want 3", len(posts), posts)
	}
	for i, want := range []string{"one", "two", "three"} {
		got := posts[i]
		if got.Text != want {
			t.Errorf("post %d text = %q, want %q — the plane's order is the delivery order", i, got.Text, want)
		}
		if got.Delivery != "steer" {
			t.Errorf("post %d delivery = %q, want steer", i, got.Delivery)
		}
		if got.Session != "ses_abc" {
			t.Errorf("post %d session = %q, want ses_abc", i, got.Session)
		}
		if got.User != "opencode" || got.Pass != "pw" {
			t.Errorf("post %d auth = %q:%q, want the private server's basic auth", i, got.User, got.Pass)
		}
	}
	if acks := plane.acks("run.cred"); len(acks) != 3 || acks[0] != 1 || acks[1] != 2 || acks[2] != 3 {
		t.Errorf("acks = %v, want 1,2,3 after the deliveries", acks)
	}
}

// A non-2xx is not a delivery: the message stays un-ACKed and a fresh poller is
// offered it again.
func TestOpencodeDoesNotAckAFailedPromptAndRedelivers(t *testing.T) {
	plane := newFakePlane()
	plane.add("run.cred", "retry me")
	planeTS := httptest.NewServer(plane.handler())
	defer planeTS.Close()

	oc := &fakeOpencode{statuses: []int{http.StatusInternalServerError}}
	srv := opencodeTestServer(t, oc)
	runIDs := make(chan string, 4)
	runIDs <- "run.cred"
	startTestDelivery(t, planeTS.URL, runIDs, srv, "ses_abc")

	waitUntil(t, "the message to be redelivered and ACKed", func() bool {
		return len(oc.delivered()) >= 2 && plane.watermark("run.cred") == 1
	})

	posts := oc.delivered()
	if posts[0].Text != "retry me" || posts[1].Text != "retry me" {
		t.Fatalf("posts = %+v, want the same message twice", posts)
	}
	acks := plane.acks("run.cred")
	if len(acks) == 0 {
		t.Fatal("the redelivered message was never ACKed")
	}
	// The failed POST must not have ACKed: only the successful redelivery did,
	// and it ACKs seq 1 exactly once.
	for _, seq := range acks {
		if seq != 1 {
			t.Errorf("acked %d, want only the delivered seq 1", seq)
		}
	}
}

// A run id that changes mid-session restarts the poll with a FRESH cursor: the
// new run's first message is delivered even though the previous run's cursor
// already moved past seq 1.
func TestOpencodeRestartsThePollOnANewRunID(t *testing.T) {
	plane := newFakePlane()
	plane.add("run.first", "first run message")
	plane.add("run.second", "second run message")
	planeTS := httptest.NewServer(plane.handler())
	defer planeTS.Close()

	oc := &fakeOpencode{}
	srv := opencodeTestServer(t, oc)
	runIDs := make(chan string, 4)
	runIDs <- "run.first"
	startTestDelivery(t, planeTS.URL, runIDs, srv, "ses_abc")

	waitUntil(t, "the first run's message", func() bool { return plane.watermark("run.first") == 1 })

	runIDs <- "run.second"
	waitUntil(t, "the second run's message on a fresh cursor", func() bool {
		return plane.watermark("run.second") == 1
	})

	posts := oc.delivered()
	if len(posts) != 2 || posts[0].Text != "first run message" || posts[1].Text != "second run message" {
		t.Fatalf("posts = %+v, want the first run's message then the second's", posts)
	}
	if polls := plane.pollCount("run.second"); polls == 0 {
		t.Error("the second run was never polled")
	}
}

// A session with no private server (a probe, or one whose config declares no
// clankerbar MCP server) runs no delivery goroutine and polls nothing.
func TestOpencodeWithoutAPrivateServerDeliversNothing(t *testing.T) {
	plane := newFakePlane()
	plane.add("run.cred", "never delivered")
	planeTS := httptest.NewServer(plane.handler())
	defer planeTS.Close()

	runIDs := make(chan string, 1)
	runIDs <- "run.cred"
	d := newOpencodeInbox(context.Background(), nil, InboxConfig{
		Origin: planeTS.URL, APIKey: "k", PollInterval: 5 * time.Millisecond,
	}, runIDs)

	// Nothing ever attaches: no server, no target, no goroutine.
	time.Sleep(50 * time.Millisecond)
	d.mu.Lock()
	started := d.started
	d.mu.Unlock()
	if started {
		t.Error("the delivery goroutine started with no private server")
	}
	if polls := plane.pollCount("run.cred"); polls != 0 {
		t.Errorf("polled the inbox %d times with no private server, want 0", polls)
	}
	if acks := plane.acks("run.cred"); len(acks) != 0 {
		t.Errorf("acked %v with no private server, want nothing", acks)
	}

	done := make(chan struct{})
	go func() { d.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("close blocked on a goroutine that should never have started")
	}
}

// A resumed process starts its own private server: delivery follows the live
// one, and a message that arrives while no process is live waits instead of
// being posted to the stopped server.
func TestOpencodeDeliveryFollowsTheLiveServer(t *testing.T) {
	plane := newFakePlane()
	plane.add("run.cred", "before the resume")
	planeTS := httptest.NewServer(plane.handler())
	defer planeTS.Close()

	first := &fakeOpencode{}
	second := &fakeOpencode{}
	firstSrv := opencodeTestServer(t, first)
	secondSrv := opencodeTestServer(t, second)
	runIDs := make(chan string, 4)
	runIDs <- "run.cred"
	d := startTestDelivery(t, planeTS.URL, runIDs, firstSrv, "ses_abc")

	waitUntil(t, "the first process's message", func() bool { return plane.watermark("run.cred") == 1 })

	// The first process ended: its server is about to be stopped.
	d.detach(firstSrv)
	plane.add("run.cred", "while no process was live")
	time.Sleep(50 * time.Millisecond)
	if posts := second.delivered(); len(posts) != 0 {
		t.Fatalf("the resumed process's server got %d posts before it was attached: %+v", len(posts), posts)
	}
	if got := plane.watermark("run.cred"); got != 1 {
		t.Fatalf("watermark = %d while no process was live, want 1 — the queued message must wait", got)
	}

	// The resurrection's continuation is attached, with the same opencode
	// session id (it continues the same session).
	d.attach(secondSrv)
	d.setSession("ses_abc")
	waitUntil(t, "the queued message on the resumed process's server", func() bool {
		return plane.watermark("run.cred") == 2
	})
	posts := second.delivered()
	if len(posts) != 1 || posts[0].Text != "while no process was live" {
		t.Fatalf("resumed process posts = %+v, want the queued message", posts)
	}
	if posts[0].Session != "ses_abc" {
		t.Errorf("resumed process post session = %q, want the continued session", posts[0].Session)
	}
	if posts := first.delivered(); len(posts) != 1 {
		t.Errorf("the stopped server got %d posts, want only the one it delivered before stopping", len(posts))
	}
}

// The stream is where the session id appears; the parser must hand it to the
// delivery target or no message could ever be posted.
func TestOpencodeParseAnnouncesTheSessionToDelivery(t *testing.T) {
	d := newOpencodeInbox(context.Background(), nil, InboxConfig{}, nil)
	p := newOpencodeParse(Invocation{}, nil)
	p.inbox = d
	p.line([]byte(`{"type":"step_start","sessionID":"ses_abc","part":{"id":"prt_1","type":"step-start"}}`))

	d.mu.Lock()
	sid := d.sid
	d.mu.Unlock()
	if sid != "ses_abc" {
		t.Fatalf("delivery session = %q, want the stream's sessionID", sid)
	}
}

// Through the real adapter path: an invocation that names no clankerbar MCP
// server starts no private server, so it must not start delivery either.
func TestOpencodeRunSessionWithoutAMCPServerStartsNoDelivery(t *testing.T) {
	opencodeStub(t, `#!/bin/sh
printf '%s\n' '{"type":"step_start","sessionID":"ses_x","part":{"id":"prt_1","type":"step-start"}}'
printf '%s\n' '{"type":"text","sessionID":"ses_x","part":{"id":"prt_2","type":"text","text":"done"}}'
`)
	plane := newFakePlane()
	plane.add("run.cred", "never delivered")
	planeTS := httptest.NewServer(plane.handler())
	defer planeTS.Close()

	runIDs := make(chan string, 1)
	runIDs <- "run.cred"
	d := newOpencodeInbox(context.Background(), nil, InboxConfig{
		Origin: planeTS.URL, APIKey: "k", PollInterval: 5 * time.Millisecond,
	}, runIDs)
	defer d.close()

	in := Invocation{Prompt: "Work.", Console: nil}
	if _, err := (opencode{}).runSession(context.Background(), in, opencodeArgs(in), d); err != nil {
		t.Fatalf("runSession: %v", err)
	}
	d.mu.Lock()
	started := d.started
	d.mu.Unlock()
	if started {
		t.Error("delivery started for a session with no private server")
	}
	if polls := plane.pollCount("run.cred"); polls != 0 {
		t.Errorf("polled the inbox %d times, want 0", polls)
	}
}
