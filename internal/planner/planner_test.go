package planner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// The planner loop's behaviour, against a fake plane and a fake harness: the
// five promises studio-chat.md §3/§4 makes are each one test — no model when
// nothing is pending, one session per conversation, resume on the plane's
// pointer, re-prime on a missing or over-budget session, and no fallback when
// the role cannot be served.

type fakePoller struct {
	mu      sync.Mutex
	pending *backlog.PendingConversation
	err     error
	calls   int
}

func (f *fakePoller) PollPending(context.Context) (*backlog.PendingConversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.pending, f.err
}

type fakeConversations struct {
	mu       sync.Mutex
	claim    *plane.ConversationClaim
	claimErr error
	claims   []plane.ConversationClaimOptions
	pages    map[int]plane.ConversationPage
	reads    []int
}

func (f *fakeConversations) ClaimConversation(_ context.Context, opts plane.ConversationClaimOptions) (*plane.ConversationClaim, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims = append(f.claims, opts)
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	return f.claim, nil
}

func (f *fakeConversations) ReadConversation(_ context.Context, _ string, beforeSeq, _ int) (plane.ConversationPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, beforeSeq)
	return f.pages[beforeSeq], nil
}

type fakeRCfg struct {
	state *plane.RunConfigState
	err   error
}

func (f *fakeRCfg) RunConfig(context.Context) (*plane.RunConfigState, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.state, nil
}

// fakeAdapter embeds the interface so only the two methods the planner calls
// have to be implemented. Any other call would panic, which is the point: the
// planner must not need them.
type fakeAdapter struct {
	harness.Adapter
	caps    harness.Capabilities
	results []harness.Result
	errs    []error
	calls   []harness.Invocation
}

func (f *fakeAdapter) Invoke(_ context.Context, in harness.Invocation) (harness.Result, error) {
	f.calls = append(f.calls, in)
	i := len(f.calls) - 1
	var res harness.Result
	if i < len(f.results) {
		res = f.results[i]
	}
	var err error
	if i < len(f.errs) {
		err = f.errs[i]
	}
	// A real session-id harness (claude) runs under the handle it was given;
	// opencode may report its own, which a dedicated test covers.
	if res.SessionRef == "" {
		res.SessionRef = in.SessionRef
	}
	return res, err
}

func (f *fakeAdapter) Capabilities() harness.Capabilities {
	if f.caps == (harness.Capabilities{}) {
		return harness.Capabilities{TracksClaims: true, ResumesSessions: true}
	}
	return f.caps
}

type fakeFleet struct {
	mu     sync.Mutex
	sent   []fleet.Report
	closed []fleet.Report
}

func (f *fakeFleet) Send(r fleet.Report) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, r)
}

func (f *fakeFleet) Close(r fleet.Report) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, r)
}

func (f *fakeFleet) sentStates() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.sent))
	for _, r := range f.sent {
		out = append(out, r.State.Kind)
	}
	return out
}

func roleDoc(t *testing.T, role map[string]any) *plane.RunConfigState {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"roles": map[string]any{"planner": role}})
	if err != nil {
		t.Fatal(err)
	}
	return &plane.RunConfigState{Version: 1, Config: raw}
}

type fixture struct {
	planner *Planner
	poller  *fakePoller
	convs   *fakeConversations
	adapter *fakeAdapter
	fleet   *fakeFleet
	base    *config.Config
	rcfg    *fakeRCfg
}

func newFixture(t *testing.T, role map[string]any) *fixture {
	t.Helper()
	base := &config.Config{
		Harness:    "claude",
		Prompt:     "Work the next backlog item.",
		BacklogURL: "https://clankerbar.com",
		WorkDir:    t.TempDir(),
	}
	f := &fixture{
		poller:  &fakePoller{},
		convs:   &fakeConversations{pages: map[int]plane.ConversationPage{}},
		adapter: &fakeAdapter{},
		fleet:   &fakeFleet{},
		base:    base,
		rcfg:    &fakeRCfg{state: roleDoc(t, role)},
	}
	refN := 0
	p, err := New(Config{
		Base: base,
		Targets: []Target{{
			Name:          "demo",
			Poller:        f.poller,
			Conversations: f.convs,
			RCfg:          f.rcfg,
			Fleet:         f.fleet,
		}},
		StateDir: t.TempDir(),
		Instance: "host/planner",
		AdapterFor: func(name string) (harness.Adapter, error) {
			if name != "claude" {
				return nil, errors.New("unknown harness")
			}
			return f.adapter, nil
		},
		Console: io.Discard,
		Now:     time.Now,
		NewSessionRef: func() string {
			refN++
			return fmt.Sprintf("ref-%d", refN)
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// The memory store keeps each test independent of the filesystem; the file
	// store gets its own test.
	p.sessions = NewMemorySessionStore()
	f.planner = p
	return f
}

func pending(id string) *backlog.PendingConversation {
	return &backlog.PendingConversation{ID: id, PendingCount: 1, LastMessageAt: time.Now()}
}

func claim(id string, seq int, body string) *plane.ConversationClaim {
	return &plane.ConversationClaim{
		ID:           id,
		PendingCount: 1,
		Messages:     []plane.ConversationMessage{{ID: "m1", Seq: seq, AuthorKind: "operator", AuthorSource: "console", AuthorName: "alice", Body: body}},
		ClaimToken:   id + ".secret",
	}
}

func (f *fixture) lap(t *testing.T) {
	t.Helper()
	if err := f.planner.lap(context.Background()); err != nil {
		t.Fatalf("lap: %v", err)
	}
}

// The whole point of the poll/model split: nothing pending means no process, no
// claim and no spend.
func TestLapWithNothingPendingRunsNoModel(t *testing.T) {
	f := newFixture(t, map[string]any{"harness": "claude"})
	f.lap(t)
	if len(f.adapter.calls) != 0 {
		t.Fatalf("spawned %d session(s) with nothing pending", len(f.adapter.calls))
	}
	if len(f.convs.claims) != 0 {
		t.Fatalf("claimed %d conversation(s) with nothing pending", len(f.convs.claims))
	}
	if got := f.fleet.sentStates(); len(got) == 0 || got[len(got)-1] != fleet.StateIdle {
		t.Errorf("presence states = %v, want an idle beacon", got)
	}
}

// A pending turn is claimed, run as ONE session, and answered by the session
// itself: the planner never writes a reply.
func TestLapClaimsAndPrimesOneSession(t *testing.T) {
	f := newFixture(t, map[string]any{"harness": "claude", "prompt": "You are the planner."})
	f.poller.pending = pending("conv-1")
	f.convs.claim = claim("conv-1", 3, "please summarise the queue")
	f.adapter.results = []harness.Result{{Tokens: 1200}}

	f.lap(t)

	if len(f.convs.claims) != 1 {
		t.Fatalf("claims = %d, want 1", len(f.convs.claims))
	}
	got := f.convs.claims[0]
	if got.ConversationID != "conv-1" || got.Instance != "host/planner" || got.History != "full" {
		t.Errorf("claim options = %+v, want the polled conversation, this instance, full history", got)
	}
	if len(f.adapter.calls) != 1 {
		t.Fatalf("sessions = %d, want 1", len(f.adapter.calls))
	}
	inv := f.adapter.calls[0]
	if inv.ResumeSession {
		t.Error("a fresh conversation resumed a session")
	}
	if inv.SessionRef != "ref-1" {
		t.Fatalf("SessionRef = %q, want the minted ref-1", inv.SessionRef)
	}
	for _, want := range []string{"You are the planner.", "conv-1", "conv-1.secret", "please summarise the queue", "sessionRef: " + inv.SessionRef} {
		if !strings.Contains(inv.Prompt, want) {
			t.Errorf("prime prompt does not carry %q:\n%s", want, inv.Prompt)
		}
	}
	if entry := f.planner.sessions.entries[sessionKey("demo", "conv-1")]; entry.Ref != inv.SessionRef || entry.Tokens != 1200 {
		t.Errorf("session entry = %+v, want the primed ref with 1200 tokens", entry)
	}
	if inv.MaxTurns != config.DefaultMaxTurns {
		t.Errorf("MaxTurns = %d, want the default %d", inv.MaxTurns, config.DefaultMaxTurns)
	}
	if inv.MaxSessionWallClock != config.DefaultPlannerWallClock {
		t.Errorf("MaxSessionWallClock = %v, want %v", inv.MaxSessionWallClock, config.DefaultPlannerWallClock)
	}
	if len(f.adapter.calls[0].Prompt) == 0 {
		t.Fatal("empty prompt")
	}
}

// The second turn of the same conversation resumes the SAME session: the plane's
// pointer is followed, and the turn carries no history because the transcript
// already has it.
func TestSecondTurnResumesTheSameSession(t *testing.T) {
	f := newFixture(t, map[string]any{"harness": "claude"})
	f.poller.pending = pending("conv-1")
	f.convs.claim = claim("conv-1", 3, "first")
	f.convs.claim.Context = []plane.ConversationMessage{{Seq: 1, AuthorKind: "operator", AuthorName: "alice", Body: "earlier"}}
	f.adapter.results = []harness.Result{{SessionRef: "ref-1", Tokens: 1000}}
	f.lap(t)

	// The plane records the session on reply, so the next claim carries it.
	f.convs.claim = claim("conv-1", 5, "second")
	f.convs.claim.Resume = &plane.ConversationResume{Harness: "claude", Ref: "ref-1"}
	f.adapter.results = append(f.adapter.results, harness.Result{SessionRef: "ref-1", Tokens: 500})
	f.lap(t)

	if len(f.adapter.calls) != 2 {
		t.Fatalf("sessions = %d, want 2", len(f.adapter.calls))
	}
	second := f.adapter.calls[1]
	if !second.ResumeSession || second.SessionRef != "ref-1" {
		t.Fatalf("second turn = resume:%v ref:%q, want a resume of ref-1", second.ResumeSession, second.SessionRef)
	}
	if strings.Contains(second.Prompt, "## Conversation history") {
		t.Errorf("a resumed turn re-sent the history it already has:\n%s", second.Prompt)
	}
	if !strings.Contains(second.Prompt, "second") {
		t.Errorf("resume prompt does not carry the new message:\n%s", second.Prompt)
	}
	// The accumulator carried the first turn's spend.
	entry, ok := f.planner.sessions.entries[sessionKey("demo", "conv-1")]
	if !ok || entry.Tokens != 1500 || entry.Ref != "ref-1" {
		t.Errorf("session entry = %+v (present=%v), want ref-1 with 1500 tokens", entry, ok)
	}
}

// A harness that reports the resumed session gone re-primes from the plane's
// history within the SAME turn: the claim is still held, so no message is lost.
func TestMissingSessionReprimesInTheSameTurn(t *testing.T) {
	f := newFixture(t, map[string]any{"harness": "claude"})
	f.poller.pending = pending("conv-1")
	f.convs.claim = claim("conv-1", 7, "hello again")
	f.convs.claim.Resume = &plane.ConversationResume{Harness: "claude", Ref: "gone-ref"}
	f.convs.claim.Context = []plane.ConversationMessage{{Seq: 5, AuthorKind: "operator", AuthorName: "alice", Body: "the earlier message"}}
	f.planner.sessions.Record(sessionKey("demo", "conv-1"), "claude", "gone-ref", 800)
	f.adapter.results = []harness.Result{
		{SessionRef: "gone-ref", SessionMissing: true, ExitCode: 1},
		{SessionRef: "fresh-ref", Tokens: 300},
	}
	f.lap(t)

	if len(f.adapter.calls) != 2 {
		t.Fatalf("sessions = %d, want the failed resume plus the re-prime", len(f.adapter.calls))
	}
	if !f.adapter.calls[0].ResumeSession {
		t.Error("the first attempt did not resume the plane's pointer")
	}
	if f.adapter.calls[1].ResumeSession {
		t.Error("the re-prime resumed instead of starting fresh")
	}
	if f.adapter.calls[1].SessionRef == "gone-ref" || f.adapter.calls[1].SessionRef == "" {
		t.Errorf("re-prime SessionRef = %q, want a fresh handle", f.adapter.calls[1].SessionRef)
	}
	if !strings.Contains(f.adapter.calls[1].Prompt, "the earlier message") {
		t.Errorf("the re-prime prompt does not carry the plane's history:\n%s", f.adapter.calls[1].Prompt)
	}
	entry := f.planner.sessions.entries[sessionKey("demo", "conv-1")]
	if entry.Ref != "fresh-ref" || entry.Tokens != 300 {
		t.Errorf("session entry = %+v, want the fresh ref with only its own 300 tokens", entry)
	}
}

// The per-session token bound is what ends a long conversation's session: when
// the accumulated spend reaches it, the next turn primes a new session.
func TestSpentSessionBudgetReprimes(t *testing.T) {
	f := newFixture(t, map[string]any{"harness": "claude", "max_session_tokens": 1000})
	f.poller.pending = pending("conv-1")
	f.convs.claim = claim("conv-1", 7, "still there?")
	f.convs.claim.Resume = &plane.ConversationResume{Harness: "claude", Ref: "spent-ref"}
	f.planner.sessions.Record(sessionKey("demo", "conv-1"), "claude", "spent-ref", 1000)
	f.adapter.results = []harness.Result{{Tokens: 10}}
	f.lap(t)

	if len(f.adapter.calls) != 1 {
		t.Fatalf("sessions = %d, want one fresh prime", len(f.adapter.calls))
	}
	if f.adapter.calls[0].ResumeSession {
		t.Error("a spent session was resumed; the bound did nothing")
	}
	if f.adapter.calls[0].SessionRef == "" || f.adapter.calls[0].SessionRef == "spent-ref" {
		t.Errorf("re-prime SessionRef = %q, want a fresh handle", f.adapter.calls[0].SessionRef)
	}
	if !strings.Contains(f.adapter.calls[0].Prompt, "## Conversation history") {
		t.Error("the re-prime did not carry the history it must have lost")
	}
}

// A resume pointer naming another harness is not resumable: the session belongs
// to a CLI this turn is not running, so it primes.
func TestForeignHarnessResumePrimes(t *testing.T) {
	f := newFixture(t, map[string]any{"harness": "claude"})
	f.poller.pending = pending("conv-1")
	f.convs.claim = claim("conv-1", 2, "hi")
	f.convs.claim.Resume = &plane.ConversationResume{Harness: "opencode", Ref: "ses_1"}
	f.planner.sessions.Record(sessionKey("demo", "conv-1"), "opencode", "ses_1", 10)
	f.adapter.results = []harness.Result{{SessionRef: "fresh", Tokens: 1}}
	f.lap(t)
	if f.adapter.calls[0].ResumeSession {
		t.Error("resumed a session belonging to another harness")
	}
}

// D23's no-fallback rule at the loop boundary: a role whose harness this build
// cannot run is not replaced by the local default; nothing is claimed or spawned.
func TestUnservableRoleClaimsAndSpawnsNothing(t *testing.T) {
	f := newFixture(t, map[string]any{"harness": "chatgpt"})
	f.poller.pending = pending("conv-1")
	f.convs.claim = claim("conv-1", 1, "hi")
	f.lap(t)
	if len(f.convs.claims) != 0 {
		t.Errorf("claimed %d conversation(s) with an unservable role", len(f.convs.claims))
	}
	if len(f.adapter.calls) != 0 {
		t.Errorf("spawned %d session(s) with an unservable role", len(f.adapter.calls))
	}
}

// A project whose resolved document has no roles.planner has chat off: a stale
// pending poll (or a config change between poll and turn) must not claim.
func TestNoPlannerClaimsNothing(t *testing.T) {
	f := newFixture(t, map[string]any{"harness": "claude"})
	f.rcfg.state = &plane.RunConfigState{Version: 1, Config: json.RawMessage(`{}`)}
	f.poller.pending = pending("conv-1")
	f.convs.claim = claim("conv-1", 1, "hi")
	f.lap(t)
	if len(f.convs.claims) != 0 || len(f.adapter.calls) != 0 {
		t.Errorf("claims=%d sessions=%d with no planner configured", len(f.convs.claims), len(f.adapter.calls))
	}
}

// A harness with no session ids always primes: the role's session affinity
// cannot be honoured, and pretending to resume would be worse than re-priming.
func TestHarnessWithoutResumeAlwaysPrimes(t *testing.T) {
	f := newFixture(t, map[string]any{"harness": "claude"})
	f.adapter.caps = harness.Capabilities{TracksClaims: true, ResumesSessions: false}
	f.poller.pending = pending("conv-1")
	f.convs.claim = claim("conv-1", 2, "hi")
	f.convs.claim.Resume = &plane.ConversationResume{Harness: "claude", Ref: "ref-1"}
	f.adapter.results = []harness.Result{{Tokens: 5}}
	f.lap(t)
	if f.adapter.calls[0].ResumeSession {
		t.Error("an adapter that cannot resume was asked to")
	}
	for _, line := range []string{"Omit sessionRef", "sessionHarness: claude"} {
		if !strings.Contains(f.adapter.calls[0].Prompt, line) {
			t.Errorf("prompt does not carry %q for a no-session harness", line)
		}
	}
}

// The prime walks read_conversation backwards when the claim's first page does
// not reach the conversation's start.
func TestPrimePagesOlderHistory(t *testing.T) {
	f := newFixture(t, map[string]any{"harness": "claude"})
	f.poller.pending = pending("conv-1")
	claim := claim("conv-1", 21, "current question")
	claim.Context = []plane.ConversationMessage{{Seq: 11, AuthorKind: "planner", AuthorSource: "mcp_self", Body: "previous answer"}}
	f.convs.claim = claim
	f.convs.pages[11] = plane.ConversationPage{
		Messages: []plane.ConversationMessage{{Seq: 1, AuthorKind: "operator", AuthorName: "alice", Body: "the very first message"}},
		HasMore:  false, NextBeforeSeq: 1,
	}
	f.adapter.results = []harness.Result{{Tokens: 1}}
	f.lap(t)
	if len(f.convs.reads) != 1 || f.convs.reads[0] != 11 {
		t.Fatalf("history reads = %v, want one page strictly before seq 11", f.convs.reads)
	}
	prompt := f.adapter.calls[0].Prompt
	for _, want := range []string{"the very first message", "previous answer", "current question"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prime prompt is missing %q:\n%s", want, prompt)
		}
	}
	if strings.Index(prompt, "the very first message") > strings.Index(prompt, "previous answer") {
		t.Error("history is not ordered oldest to newest")
	}
}

// Never a fallback model: a tier that resolves to nothing means no session.
func TestUnresolvableTierSpawnsNothing(t *testing.T) {
	f := newFixture(t, map[string]any{"harness": "claude", "tier": "mystery"})
	f.poller.pending = pending("conv-1")
	f.convs.claim = claim("conv-1", 1, "hi")
	f.lap(t)
	if len(f.convs.claims) != 0 || len(f.adapter.calls) != 0 {
		t.Errorf("claims=%d sessions=%d with an unresolvable tier", len(f.convs.claims), len(f.adapter.calls))
	}
}

// Every beacon carries the planner role; shutdown sends a stopping beacon.
func TestBeaconsCarryThePlannerRole(t *testing.T) {
	f := newFixture(t, map[string]any{"harness": "claude"})
	f.lap(t)
	f.planner.closeBeacons()
	f.fleet.mu.Lock()
	defer f.fleet.mu.Unlock()
	if len(f.fleet.sent) == 0 {
		t.Fatal("no presence beacon")
	}
	if f.fleet.sent[0].Role != config.RolePlanner {
		t.Errorf("beacon role = %q, want planner", f.fleet.sent[0].Role)
	}
	if len(f.fleet.closed) != 1 || f.fleet.closed[0].State.Kind != fleet.StateStopping {
		t.Errorf("close beacons = %+v, want one stopping", f.fleet.closed)
	}
}

// Run starts the poll loop, beacons, and ends cleanly on cancellation.
func TestRunPollsAndStopsOnCancel(t *testing.T) {
	f := newFixture(t, map[string]any{"harness": "claude"})
	f.planner.poll = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.planner.Run(ctx) }()
	deadline := time.After(2 * time.Second)
	for {
		f.poller.mu.Lock()
		calls := f.poller.calls
		f.poller.mu.Unlock()
		if calls >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Run never polled")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if len(f.fleet.closed) != 1 {
		t.Errorf("stopping beacons = %d, want 1", len(f.fleet.closed))
	}
}

// A 429 slows the poll quietly; it does not end the run.
func TestRateLimitedPollKeepsTheLoopAlive(t *testing.T) {
	f := newFixture(t, map[string]any{"harness": "claude"})
	f.poller.err = backlog.ErrConversationPollRateLimited
	f.lap(t)
	if err := f.planner.lap(context.Background()); err != nil {
		t.Fatalf("lap returned %v on a rate limit, want it to keep polling", err)
	}
}

// A rejected key ends the run: the sessions would carry the same key, so idling
// would only hide a permanent misconfiguration.
func TestUnauthorizedPollEndsTheRun(t *testing.T) {
	f := newFixture(t, map[string]any{"harness": "claude"})
	f.poller.err = backlog.ErrUnauthorized
	if err := f.planner.lap(context.Background()); !errors.Is(err, backlog.ErrUnauthorized) {
		t.Fatalf("lap = %v, want the auth failure", err)
	}
}
