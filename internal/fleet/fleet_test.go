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
	"fmt"
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
			Sessions        []struct {
				Phase   string `json:"phase"`
				Harness string `json:"harness"`
				Model   string `json:"model"`
			} `json:"sessions"`
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
			Sessions: []Session{
				{Phase: "implement", Harness: "opencode", Model: "opencode-go/deepseek-v4-flash"},
				// An unresolved model rides as "" — the harness default applied —
				// and must decode as present-but-empty, not vanish.
				{Phase: "review", Harness: "claude"},
			},
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
		// CLA-630: one entry per spawned session, in spawn order, with the
		// resolved model — empty when the harness default applied.
		if len(it.Sessions) != 2 {
			t.Fatalf("iteration sessions = %+v, want one entry per spawned session", it.Sessions)
		}
		if s := it.Sessions[0]; s.Phase != "implement" || s.Harness != "opencode" || s.Model != "opencode-go/deepseek-v4-flash" {
			t.Errorf("session[0] = %+v, want the implement session's harness and model", s)
		}
		if s := it.Sessions[1]; s.Phase != "review" || s.Harness != "claude" || s.Model != "" {
			t.Errorf("session[1] = %+v, want the review session with its unresolved model as the empty string", s)
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

// CLA-691: each session entry carries its normalised token breakdown and the
// harness-reported cost, and each of those fields is OMITTED when there is
// nothing to say — an older CLI's entry, and a session that died before its
// harness reported anything, must stay the pre-CLA-691 shape exactly, which is
// how the plane tells "no usage" from "a reported zero".
func TestMarshalReportCarriesPerSessionUsage(t *testing.T) {
	cost := 0.18
	zero := 0.0
	b, err := marshalReport(Report{
		Identity: Identity{Instance: "box-1"},
		State:    State{Kind: StateIteration},
		Iterations: []Iteration{{
			Outcome: OutcomeReleased,
			Sessions: []Session{
				{
					Phase: "review-1", Harness: "opencode", Model: "opencode-go/gpt-6-luna",
					Usage:         TokenUsage{Input: 120, Output: 30, Reasoning: 5, CacheRead: 4000, CacheWrite: 100},
					UsageReported: true, CostUSD: &cost, RunID: "run-1",
				},
				{
					// A harness that reported usage but genuinely spent nothing:
					// the report rides, every bucket is zero, and a cost of 0 is a
					// FIGURE (a pointer) rather than unknown.
					Phase: "verify", Harness: "opencode", Model: "opencode-go/gpt-6-luna",
					UsageReported: true, CostUSD: &zero,
				},
				{
					// Nothing reported at all — the pre-CLA-691 entry, untouched.
					Phase: "implement", Harness: "codex", Model: "gpt-5",
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("marshalReport: %v", err)
	}

	var got struct {
		Iterations []struct {
			Sessions []struct {
				Phase         string          `json:"phase"`
				UsageReported *bool           `json:"usageReported"`
				Usage         *map[string]int `json:"usage"`
				CostUSD       *float64        `json:"costUsd"`
				RunID         string          `json:"runId"`
			} `json:"sessions"`
		} `json:"iterations"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Iterations) != 1 || len(got.Iterations[0].Sessions) != 3 {
		t.Fatalf("wire shape lost a session: %s", b)
	}
	sessions := got.Iterations[0].Sessions

	if sessions[0].UsageReported == nil || !*sessions[0].UsageReported {
		t.Errorf("session[0].usageReported = %v, want true — the harness reported usage", sessions[0].UsageReported)
	}
	wantUsage := map[string]int{"input": 120, "output": 30, "reasoning": 5, "cacheRead": 4000, "cacheWrite": 100}
	if sessions[0].Usage == nil || !mapsEqual(*sessions[0].Usage, wantUsage) {
		t.Errorf("session[0].usage = %v, want %v", sessions[0].Usage, wantUsage)
	}
	if sessions[0].CostUSD == nil || *sessions[0].CostUSD != cost {
		t.Errorf("session[0].costUsd = %v, want %v", sessions[0].CostUSD, cost)
	}

	if sessions[1].CostUSD == nil || *sessions[1].CostUSD != 0 {
		t.Errorf("session[1].costUsd = %v, want a present 0 — a reported zero is a figure, not unknown", sessions[1].CostUSD)
	}
	if sessions[1].Usage == nil || len(*sessions[1].Usage) != 0 {
		t.Errorf("session[1].usage = %v, want an empty object: every bucket is zero and each key is omitted", sessions[1].Usage)
	}

	if sessions[2].UsageReported != nil || sessions[2].Usage != nil || sessions[2].CostUSD != nil {
		t.Errorf("session[2] = %+v, want the pre-CLA-691 shape with all three fields absent", sessions[2])
	}

	// The run id is what joins a session to the plane's per-run records (the
	// durable review record carries the run that wrote it), so it must survive —
	// and be absent, like the rest, for a session that held no claim.
	if sessions[0].RunID != "run-1" {
		t.Errorf("session[0].runId = %q, want run-1", sessions[0].RunID)
	}
	if sessions[2].RunID != "" {
		t.Errorf("session[2].runId = %q, want absent for a session that held no claim", sessions[2].RunID)
	}
}

func mapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
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

// CLA-677: each iteration carries the drain's dollar spend as `costUsd`. It is
// always present — deliberately UNLIKE `tokens`, which is omitempty — because a
// zero here is a figure (the harnesses reported no cost), not an unknown the
// plane should distinguish from an older CLI's missing key.
func TestMarshalReport_CarriesIterationCost(t *testing.T) {
	b, err := marshalReport(Report{
		Identity: Identity{Instance: "x"},
		Iterations: []Iteration{{
			Outcome: OutcomeReleased, DurationSeconds: 12.5, Tokens: 4321, CostUSD: 1.25,
		}},
	})
	if err != nil {
		t.Fatalf("marshalReport: %v", err)
	}
	var got struct {
		Iterations []struct {
			CostUSD *float64 `json:"costUsd"`
		} `json:"iterations"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Iterations) != 1 || got.Iterations[0].CostUSD == nil {
		t.Fatalf("iteration did not carry costUsd: %s", b)
	}
	if *got.Iterations[0].CostUSD != 1.25 {
		t.Errorf("costUsd = %v, want 1.25", *got.Iterations[0].CostUSD)
	}

	// A zero-cost iteration still carries the key: absent would read as "an
	// older CLI", which is a different fact.
	b, err = marshalReport(Report{
		Identity:   Identity{Instance: "x"},
		Iterations: []Iteration{{Outcome: OutcomeReleased}},
	})
	if err != nil {
		t.Fatalf("marshalReport: %v", err)
	}
	if !strings.Contains(string(b), `"costUsd":0`) {
		t.Errorf("a zero-cost iteration dropped costUsd: %s", b)
	}
}

// CLA-630: the exact wire shape the plane's parsePhaseSessions reads —
// sessions:[{phase,harness,model}] in spawn order, the model present even when
// empty (the plane stores "" as null, "not resolved"), and no key at all on an
// iteration that spawned nothing, so a pre-CLA-630 shape stays valid.
func TestMarshalReport_SessionsWireShape(t *testing.T) {
	b, err := marshalReport(Report{
		Identity: Identity{Instance: "x"},
		Iterations: []Iteration{{
			Outcome: OutcomeReleased,
			Sessions: []Session{
				{Phase: "implement", Harness: "opencode", Model: "opencode-go/deepseek-v4-flash"},
				{Phase: "review", Harness: "claude"}, // unresolved: rides as ""
			},
		}},
	})
	if err != nil {
		t.Fatalf("marshalReport: %v", err)
	}
	want := `"sessions":[{"phase":"implement","harness":"opencode","model":"opencode-go/deepseek-v4-flash"},{"phase":"review","harness":"claude","model":""}]`
	if !strings.Contains(string(b), want) {
		t.Errorf("report JSON does not carry the plane's sessions shape\nwant substring: %s\ngot: %s", want, b)
	}

	// An iteration with no sessions must not grow a sessions key: the field is
	// optional end to end, and older shapes stay byte-compatible.
	b, err = marshalReport(Report{
		Identity:   Identity{Instance: "x"},
		Iterations: []Iteration{{Outcome: OutcomeReleased, Phases: []string{"1"}}},
	})
	if err != nil {
		t.Fatalf("marshalReport: %v", err)
	}
	if strings.Contains(string(b), "sessions") {
		t.Errorf("an iteration that spawned no sessions grew a sessions key: %s", b)
	}
}

// CLA-703: a session's infrastructure cause rides the wire only when there is
// one. The pre-CLA-703 shape is byte-for-byte unchanged (pinned above); a
// no_mcp session adds `cause` so the durable phase_session record can carry the
// classification once the plane's parser stores it.
func TestMarshalReport_SessionCauseIsOptional(t *testing.T) {
	b, err := marshalReport(Report{
		Identity: Identity{Instance: "x"},
		Iterations: []Iteration{{
			Outcome: OutcomeReleased,
			Sessions: []Session{
				{Phase: "review", Harness: "opencode", Cause: CauseNoMCP},
			},
		}},
	})
	if err != nil {
		t.Fatalf("marshalReport: %v", err)
	}
	if !strings.Contains(string(b), `"cause":"no_mcp"`) {
		t.Errorf("a no_mcp session did not carry its cause: %s", b)
	}
	// An ordinary session must not grow the key.
	b, err = marshalReport(Report{
		Identity: Identity{Instance: "x"},
		Iterations: []Iteration{{
			Outcome:  OutcomeReleased,
			Sessions: []Session{{Phase: "implement", Harness: "opencode"}},
		}},
	})
	if err != nil {
		t.Fatalf("marshalReport: %v", err)
	}
	if strings.Contains(string(b), `"cause"`) {
		t.Errorf("an ordinary session grew a cause key: %s", b)
	}
}

// CLA-630: the plane refuses the WHOLE report (all-or-nothing, presence
// included) when one iteration carries more than its per-iteration session cap,
// so the reporter clamps at the same bound. The first entries in spawn order are
// kept: the configured phases lead the list, the tail is respawns.
func TestMarshalReport_ClampsSessionsPerIteration(t *testing.T) {
	var sessions []Session
	for i := 0; i < maxSessionsPerIteration+1; i++ {
		sessions = append(sessions, Session{
			Phase:   fmt.Sprintf("phase-%02d", i),
			Harness: "fake",
			Model:   "m",
		})
	}
	b, err := marshalReport(Report{
		Identity:   Identity{Instance: "x"},
		Iterations: []Iteration{{Outcome: OutcomeReleased, Sessions: sessions}},
	})
	if err != nil {
		t.Fatalf("marshalReport: %v", err)
	}
	var got struct {
		Iterations []struct {
			Sessions []Session `json:"sessions"`
		} `json:"iterations"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Iterations) != 1 {
		t.Fatalf("iterations = %+v, want one", got.Iterations)
	}
	gotSessions := got.Iterations[0].Sessions
	if len(gotSessions) != maxSessionsPerIteration {
		t.Fatalf("clamped to %d session(s), want %d", len(gotSessions), maxSessionsPerIteration)
	}
	for i, s := range gotSessions {
		if want := fmt.Sprintf("phase-%02d", i); s.Phase != want {
			t.Errorf("session[%d].phase = %q, want %q — the clamp keeps the first entries in spawn order", i, s.Phase, want)
		}
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
