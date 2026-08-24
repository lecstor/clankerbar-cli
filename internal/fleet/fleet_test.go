package fleet

// Tests for the reporter itself (CLA-466): the wire shape matches what the
// plane's parseFleetReport reads, Send never blocks on a wedged endpoint, an
// outage is logged once per streak with a recovery line naming what was lost,
// Close lands synchronously, and Probe distinguishes wired / bad key / old
// plane / unreachable without writing anything.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewWithoutCredsIsNotWired(t *testing.T) {
	for _, tc := range []struct{ endpoint, key string }{
		{"", ""},
		{"https://plane.example/api/projects/x/fleet/report", ""},
		{"", "k"},
	} {
		r := New(tc.endpoint, tc.key)
		if _, ok := r.(notWired); !ok {
			t.Errorf("New(%q, %q) = %T, want a notWired reporter", tc.endpoint, tc.key, r)
		}
		// Both methods must be safe to call: reporting is never load-bearing.
		r.Send(Report{Identity: Identity{Instance: "x"}})
		r.Close(Report{})
	}
}

func TestSendPostsThePlaneShape(t *testing.T) {
	type arrived struct {
		Instance       string `json:"instance"`
		Host           string `json:"host"`
		Version        string `json:"version"`
		ConfigIdentity string `json:"configIdentity"`
		State          struct {
			Kind    string `json:"kind"`
			N       int    `json:"n"`
			TaskRef string `json:"taskRef"`
			Phase   string `json:"phase"`
		} `json:"state"`
		Iterations []struct {
			TaskID          string   `json:"taskId"`
			TaskRef         string   `json:"taskRef"`
			Phases          []string `json:"phases"`
			Outcome         string   `json:"outcome"`
			DurationSeconds float64  `json:"durationSeconds"`
			Tokens          int      `json:"tokens"`
		} `json:"iterations"`
	}
	got := make(chan arrived, 1)
	auth := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth <- r.Header.Get("Authorization")
		var a arrived
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &a)
		got <- a
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	rep := New(srv.URL, "key-9")
	rep.Send(Report{
		Identity: Identity{Instance: "box-1", Host: "h1", Version: "v1.2.3", ConfigIdentity: "cafe"},
		State:    State{Kind: StateIteration, N: 2, TaskRef: "CLA-466", Phase: "review"},
		Iterations: []Iteration{{
			TaskID: "uuid-1", TaskRef: "CLA-466", Phases: []string{"implement", "review"},
			Outcome: OutcomeReleased, DurationSeconds: 12.5, Tokens: 4321,
		}},
	})

	select {
	case a := <-got:
		if a.Instance != "box-1" || a.Host != "h1" || a.Version != "v1.2.3" || a.ConfigIdentity != "cafe" {
			t.Errorf("identity = %+v", a)
		}
		if a.State.Kind != "iteration" || a.State.N != 2 || a.State.TaskRef != "CLA-466" || a.State.Phase != "review" {
			t.Errorf("state = %+v", a.State)
		}
		if len(a.Iterations) != 1 {
			t.Fatalf("iterations = %+v, want one row", a.Iterations)
		}
		it := a.Iterations[0]
		if it.TaskID != "uuid-1" || it.TaskRef != "CLA-466" || it.Outcome != "released" ||
			it.DurationSeconds != 12.5 || it.Tokens != 4321 || len(it.Phases) != 2 {
			t.Errorf("iteration row = %+v", it)
		}
		// The field names above are exactly what parseFleetReport reads; if this
		// decode succeeded, the plane's validator would too. The auth header is
		// the bearer key.
		if got2 := <-auth; got2 != "Bearer key-9" {
			t.Errorf("Authorization = %q", got2)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no report arrived")
	}
}

func TestMarshalReportDefaultsAndGuards(t *testing.T) {
	b, err := marshalReport(Report{Identity: Identity{Instance: "x"}})
	if err != nil {
		t.Fatalf("marshalReport: %v", err)
	}
	if !strings.Contains(string(b), `"kind":"idle"`) {
		t.Errorf("an empty state must marshal as idle, got %s", b)
	}
	if _, err := marshalReport(Report{}); err == nil {
		t.Error("an unnamed instance must be refused, not upserted as \"\"")
	}
	if _, err := marshalReport(Report{
		Identity:   Identity{Instance: "x"},
		Iterations: []Iteration{{Outcome: "done"}},
	}); err == nil {
		t.Error("an outcome outside the four-word vocabulary must be refused client-side: it would 400 the whole report plane-side")
	}
}

func TestSendNeverBlocksWhenThePlaneHangs(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	rep := New(srv.URL, "k")
	start := time.Now()
	for i := 0; i < queueCap+5; i++ { // more than the queue holds: drops must happen, not waits
		rep.Send(Report{Identity: Identity{Instance: "x"}})
	}
	elapsed := time.Since(start)
	if elapsed > time.Second {
		t.Errorf("%d Sends took %s — Send must enqueue and return, never await a wedged plane", queueCap+5, elapsed)
	}
}

func TestCloseLandsBeforeItReturns(t *testing.T) {
	arrived := make(chan struct{})
	var once sync.Once
	signal := func() { once.Do(func() { close(arrived) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		signal()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	rep := New(srv.URL, "k")
	rep.Send(Report{Identity: Identity{Instance: "x"}}) // occupy the pump briefly
	rep.Close(Report{Identity: Identity{Instance: "x"}, State: State{Kind: StateStopping}})
	select {
	case <-arrived:
	default:
		t.Fatal("Close returned before the final beacon was delivered")
	}
}

// The contract that makes console silence meaningful: after Close, the stopping
// beacon is the LAST POST the reporter makes. Everything still queued lands
// before it — including the iteration record a run that ends right at a drain
// boundary would otherwise lose — and a Send after Close is a no-op. The plane
// is wedged on the first post to force the queue to back up, then released:
// the pump delivers that one, Close takes the lock, delivers the rest newest
// first, then stopping — and nothing arrives after it.
func TestCloseEndsTheReporterWithStoppingLast(t *testing.T) {
	type wire struct {
		State struct {
			Kind string `json:"kind"`
		} `json:"state"`
		Iterations []struct {
			Outcome string `json:"outcome"`
		} `json:"iterations"`
	}
	var mu sync.Mutex
	var posts []string // one "kind[:outcome]" per request, in arrival order
	var n int
	firstStarted := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var a wire
		_ = json.Unmarshal(body, &a)
		mu.Lock()
		entry := a.State.Kind
		if len(a.Iterations) > 0 {
			entry += ":" + a.Iterations[0].Outcome
		}
		posts = append(posts, entry)
		i := n
		n++
		mu.Unlock()
		if i == 0 { // the pump's first POST is wedged until the test lets go
			close(firstStarted)
			<-release
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	rep := New(srv.URL, "k")
	rep.Send(Report{Identity: Identity{Instance: "x"}, State: State{Kind: StateIdle}}) // r1 — the pump takes and wedges on it
	<-firstStarted
	rep.Send(Report{Identity: Identity{Instance: "x"}, State: State{Kind: StateIdle}}) // r2
	rep.Send(Report{Identity: Identity{Instance: "x"}, State: State{Kind: StateIdle}}) // r3
	rep.Send(Report{Identity: Identity{Instance: "x"}, State: State{Kind: StateIdle},  // r4 — the boundary's iteration record
		Iterations: []Iteration{{TaskID: "t-1", TaskRef: "CLA-1", Outcome: OutcomeReleased, DurationSeconds: 1, Tokens: 3}}})
	rep.Send(Report{Identity: Identity{Instance: "x"}, State: State{Kind: StateIdle}}) // r5
	close(release)
	rep.Close(Report{Identity: Identity{Instance: "x"}, State: State{Kind: StateStopping}})

	mu.Lock()
	defer mu.Unlock()
	if len(posts) != 6 {
		t.Fatalf("arrival = %v, want the pump's r1 + a 4-report flush + stopping (6 posts)", posts)
	}
	if posts[len(posts)-1] != "stopping" {
		t.Errorf("last arrival = %q, want stopping — nothing may post after it:\n%v", posts[len(posts)-1], posts)
	}
	recordLanded := false
	for _, p := range posts {
		if p == "idle:released" {
			recordLanded = true
		}
	}
	if !recordLanded {
		t.Errorf("the queued iteration record never landed — a run ending at the boundary must not lose it:\n%v", posts)
	}
}

func TestSendAfterCloseIsInert(t *testing.T) {
	arrived := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		arrived <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	rep := New(srv.URL, "k")
	rep.Close(Report{Identity: Identity{Instance: "x"}, State: State{Kind: StateStopping}})
	select {
	case <-arrived:
	default:
		t.Fatal("the closing beacon itself did not arrive")
	}
	for i := 0; i < 3; i++ {
		rep.Send(Report{Identity: Identity{Instance: "x"}, State: State{Kind: StateIdle}})
	}
	select {
	case <-arrived:
		t.Fatal("a Send after Close still posted — Close must leave the reporter inert")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestProbeDistinguishesTheFourAnswers(t *testing.T) {
	t.Run("400 empty-body refusal means wired", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"error":{"code":"invalid_request","message":"instance is required"}}`, http.StatusBadRequest)
		}))
		t.Cleanup(srv.Close)
		if err := Probe(context.Background(), srv.URL, "k"); err != nil {
			t.Errorf("Probe = %v, want nil (reachable + routed + key accepted)", err)
		}
	})
	t.Run("401 means rejected key", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{}`, http.StatusUnauthorized)
		}))
		t.Cleanup(srv.Close)
		err := Probe(context.Background(), srv.URL, "k")
		var pe *ProbeError
		if !errors.As(err, &pe) || pe.Status != http.StatusUnauthorized {
			t.Errorf("Probe = %v, want ProbeError 401", err)
		}
	})
	t.Run("404 means a plane that predates fleets", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.NotFound(w, new(http.Request))
		}))
		t.Cleanup(srv.Close)
		err := Probe(context.Background(), srv.URL, "k")
		var pe *ProbeError
		if !errors.As(err, &pe) || pe.Status != http.StatusNotFound {
			t.Errorf("Probe = %v, want ProbeError 404", err)
		}
	})
	t.Run("unreachable is a plain error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
		url := srv.URL
		srv.Close() // dead port
		err := Probe(context.Background(), url, "k")
		var pe *ProbeError
		if err == nil || errors.As(err, &pe) {
			t.Errorf("Probe = %v, want a transport error, not a status", err)
		}
	})
	t.Run("unconfigured is refused before any dial", func(t *testing.T) {
		if err := Probe(context.Background(), "", "k"); err == nil {
			t.Error("no endpoint: want an error")
		}
		if err := Probe(context.Background(), "https://plane.example/api/projects/x/fleet/report", ""); err == nil {
			t.Error("no key: want an error")
		}
	})
}

// syncBuffer is a log.Writer safe to read while the reporter's background pump
// keeps writing — the race detector is right about plain strings.Builder here.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog redirects the package logger into a buffer for the duration of the
// test. Restores on cleanup.
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	old := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(old) })
	return buf
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestReporter_OutageLogsOnceThenRecoveryNamesTheLosses(t *testing.T) {
	var mu sync.Mutex
	fail := true
	served := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		wasFail := fail
		served++
		mu.Unlock()
		if wasFail {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	buf := captureLog(t)
	rep := New(srv.URL, "k")

	const outageReports = 6
	for i := 0; i < outageReports; i++ {
		rep.Send(Report{Identity: Identity{Instance: "x"}})
	}
	waitFor(t, "every outage report to be served", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return served >= outageReports
	})
	waitFor(t, "the first failure line", func() bool {
		return strings.Count(buf.String(), "fleet report failed and was dropped") == 1
	})

	mu.Lock()
	fail = false
	mu.Unlock()
	rep.Send(Report{Identity: Identity{Instance: "x"}, State: State{Kind: StateIdle}})
	waitFor(t, "the recovery line", func() bool {
		return strings.Contains(buf.String(), "fleet reporting recovered after")
	})

	if n := strings.Count(buf.String(), "fleet report failed and was dropped"); n != 1 {
		t.Errorf("%d failure lines for a whole outage, want exactly one:\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), "recovered after 6 failed") {
		t.Errorf("recovery line does not name all %d silently-dropped failures:\n%s", outageReports, buf.String())
	}
}
