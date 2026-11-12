package plane

// claim_test.go — the runner's own claim surface: what next_task and
// claim_task serve, and which refusals the caller must tell apart. A lost race
// and a withheld task need opposite answers, so both classes are pinned by code
// rather than by message.
//
// Expected briefs are written on one line per pointer: a vertical list of keys
// is a gofmt column, and a test that fails the formatting gate is a test nobody
// runs.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestNextOffer(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name string
		body string
		want Offer
	}{
		{
			name: "takeover offer with its decline candidate",
			body: `{"next":{"id":"t-1","ref":"CLA-1","title":"Runner claims","detail":"The full detail.","doneWhen":"The client decodes it","status":"in_progress","repo":"lecstor/clankerbar-cli","category":"feature","branch":"clanker/t-1","hasWip":true,"requiresTakeover":true,"questions":[{"id":"q-1","body":"Which base?","answer":"staging","blocking":true}],"phasesDone":{"implement":{"tip":"abc1234","at":"2026-11-11T10:00:00.000Z"}}},"readyNext":{"id":"t-2","ref":"CLA-2","title":"Fresh work","status":"ready","branch":"clanker/t-2","hasWip":false,"requiresTakeover":false},"readyCount":2}`,
			want: Offer{
				Next:      &TaskBrief{ID: "t-1", Ref: "CLA-1", Title: "Runner claims", Detail: "The full detail.", DoneWhen: "The client decodes it", Status: "in_progress", Repo: "lecstor/clankerbar-cli", Category: "feature", Branch: "clanker/t-1", HasWIP: true, RequiresTakeover: true, Questions: []AnsweredQuestion{{Body: "Which base?", Answer: "staging", Blocking: true}}, PhasesDone: map[string]PhaseDone{"implement": {Tip: "abc1234", At: "2026-11-11T10:00:00.000Z"}}},
				ReadyNext: &TaskBrief{ID: "t-2", Ref: "CLA-2", Title: "Fresh work", Status: "ready", Branch: "clanker/t-2"},
			},
		},
		{
			name: "close-out offer carries the decisions and no takeover",
			body: `{"next":{"id":"t-3","ref":"CLA-3","title":"Merge it","status":"approved","branch":"clanker/t-3","hasWip":true,"awaitingMerge":true,"requiresTakeover":false,"decisions":[{"context":"Where do PRs go?","ruling":"staging"},{"elided":true,"rulingLine":"Use staging"}]},"readyCount":1}`,
			want: Offer{
				Next: &TaskBrief{ID: "t-3", Ref: "CLA-3", Title: "Merge it", Status: "approved", Branch: "clanker/t-3", HasWIP: true, AwaitingMerge: true, Decisions: []Decision{{Question: "Where do PRs go?", Answer: "staging"}, {Answer: "Use staging"}}},
			},
		},
		{
			name: "close-out offer carries the merge hint",
			body: `{"next":{"id":"t-6","ref":"CLA-6","title":"Merge it","status":"approved","branch":"clanker/t-6","hasWip":true,"awaitingMerge":true,"mergeHint":"Squash onto staging, then delete the branch."},"readyCount":1}`,
			want: Offer{Next: &TaskBrief{ID: "t-6", Ref: "CLA-6", Title: "Merge it", Status: "approved", Branch: "clanker/t-6", HasWIP: true, AwaitingMerge: true, MergeHint: "Squash onto staging, then delete the branch."}},
		},
		{
			name: "plain ready offer",
			body: `{"next":{"id":"t-4","ref":"CLA-4","title":"Plain work","status":"ready","branch":"clanker/t-4","hasWip":false,"requiresTakeover":false},"readyCount":1}`,
			want: Offer{Next: &TaskBrief{ID: "t-4", Ref: "CLA-4", Title: "Plain work", Status: "ready", Branch: "clanker/t-4"}},
		},
		{
			name: "empty queue",
			body: `{"next":null,"readyCount":0}`,
			want: Offer{},
		},
		{
			name: "bare task object",
			body: `{"id":"t-5","ref":"CLA-5","title":"Bare","status":"ready","branch":"clanker/t-5"}`,
			want: Offer{Next: &TaskBrief{ID: "t-5", Ref: "CLA-5", Title: "Bare", Status: "ready", Branch: "clanker/t-5"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := serve(t, http.StatusOK, mcpTextBody(t, tc.body))
			got, err := mustClient(t, srv).NextOffer(ctx)
			if err != nil {
				t.Fatalf("NextOffer: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("offer = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The arguments are an empty OBJECT: the plane's next_task schema is
// `inputSchema: {}`, and a null `arguments` is refused (see PeekNextTask).
func TestNextOfferAsksWithAnEmptyObject(t *testing.T) {
	srv, got := serve(t, http.StatusOK, mcpTextBody(t, `{"next":null}`))

	if _, err := mustClient(t, srv).NextOffer(context.Background()); err != nil {
		t.Fatalf("NextOffer: %v", err)
	}
	if name := toolName(t, got.body); name != "next_task" {
		t.Errorf("tool = %q, want next_task", name)
	}
	if args := toolArgs(t, got.body); len(args) != 0 {
		t.Errorf("arguments = %+v, want an empty object", args)
	}
}

// Only an answered, non-superseded question is something to work to: an open
// one, a declined one, or one a later answer replaced is not.
func TestWireQuestionsKeepsOnlyAnsweredUnsuperseded(t *testing.T) {
	cases := []struct {
		name string
		wire questionList
		want []AnsweredQuestion
	}{
		{name: "open question dropped", wire: questionList{{Body: "Which base?", Status: "open"}}, want: []AnsweredQuestion{}},
		{name: "superseded answered dropped", wire: questionList{{Body: "Which base?", Answer: "main", Status: "answered", Superseded: true}}, want: []AnsweredQuestion{}},
		{name: "dismissed dropped", wire: questionList{{Body: "Ship Friday?", Answer: "no", Status: "dismissed"}}, want: []AnsweredQuestion{}},
		{name: "answered kept", wire: questionList{{Body: "Which base?", Answer: "staging", Status: "answered", Blocking: true}}, want: []AnsweredQuestion{{Body: "Which base?", Answer: "staging", Blocking: true}}},
		{name: "status-absent answered kept", wire: questionList{{Body: "Which base?", Answer: "staging"}}, want: []AnsweredQuestion{{Body: "Which base?", Answer: "staging"}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := wireQuestions(tc.wire); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("questions = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestClaimTask(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name     string
		body     string
		takeover bool
		want     ClaimedTask
	}{
		{
			name:     "fresh claim takes the synthesised branch and the credential",
			body:     `{"task":{"id":"t-1","ref":"CLA-1","title":"Runner claims","detail":"The full detail.","doneWhen":"It decodes","status":"in_progress","repo":"lecstor/clankerbar-cli","category":"feature","branch":null,"phasesDone":{"implement":{"tip":"abc1234","at":"2026-11-11T10:00:00.000Z"}}},"branch":"clanker/t-1","hasWip":false,"questions":[],"decisions":[{"context":"Where do PRs go?","ruling":"staging"},{"elided":true,"rulingLine":"Use staging"}],"run":{"id":"uuid-1.secret-1","leaseExpiresAt":"2026-11-12T01:00:00.000Z"}}`,
			takeover: false,
			want: ClaimedTask{
				Task:           TaskBrief{ID: "t-1", Ref: "CLA-1", Title: "Runner claims", Detail: "The full detail.", DoneWhen: "It decodes", Status: "in_progress", Repo: "lecstor/clankerbar-cli", Category: "feature", Branch: "clanker/t-1", Decisions: []Decision{{Question: "Where do PRs go?", Answer: "staging"}, {Answer: "Use staging"}}, PhasesDone: map[string]PhaseDone{"implement": {Tip: "abc1234", At: "2026-11-11T10:00:00.000Z"}}},
				RunID:          "uuid-1.secret-1",
				LeaseExpiresAt: "2026-11-12T01:00:00.000Z",
			},
		},
		{
			name:     "takeover claim keeps the recorded WIP",
			body:     `{"task":{"id":"t-2","ref":"CLA-2","title":"Resume","status":"in_progress","branch":"clanker/t-2"},"branch":"clanker/t-2","hasWip":true,"run":{"id":"uuid-2.secret-2"}}`,
			takeover: true,
			want:     ClaimedTask{Task: TaskBrief{ID: "t-2", Ref: "CLA-2", Title: "Resume", Status: "in_progress", Branch: "clanker/t-2", HasWIP: true}, RunID: "uuid-2.secret-2"},
		},
		{
			name:     "close-out claim, and an absent phasesDone stays nil",
			body:     `{"task":{"id":"t-3","ref":"CLA-3","title":"Merge it","status":"approved"},"branch":"clanker/t-3","hasWip":false,"awaitingMerge":true,"run":{"id":"uuid-3.secret-3","leaseExpiresAt":"2026-11-12T02:00:00.000Z"}}`,
			takeover: false,
			want:     ClaimedTask{Task: TaskBrief{ID: "t-3", Ref: "CLA-3", Title: "Merge it", Status: "approved", Branch: "clanker/t-3", AwaitingMerge: true}, RunID: "uuid-3.secret-3", LeaseExpiresAt: "2026-11-12T02:00:00.000Z"},
		},
		{
			name:     "close-out claim carries the top-level merge hint",
			body:     `{"task":{"id":"t-5","ref":"CLA-5","title":"Merge it","status":"approved"},"branch":"clanker/t-5","awaitingMerge":true,"mergeHint":"Squash onto staging.","run":{"id":"uuid-5.secret-5"}}`,
			takeover: false,
			want:     ClaimedTask{Task: TaskBrief{ID: "t-5", Ref: "CLA-5", Title: "Merge it", Status: "approved", Branch: "clanker/t-5", AwaitingMerge: true, MergeHint: "Squash onto staging."}, RunID: "uuid-5.secret-5"},
		},
		{
			name:     "bare task object",
			body:     `{"id":"t-4","ref":"CLA-4","title":"Bare","status":"in_progress","branch":"clanker/t-4","hasWip":true,"run":{"id":"uuid-4.secret-4"}}`,
			takeover: false,
			want:     ClaimedTask{Task: TaskBrief{ID: "t-4", Ref: "CLA-4", Title: "Bare", Status: "in_progress", Branch: "clanker/t-4", HasWIP: true}, RunID: "uuid-4.secret-4"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := serve(t, http.StatusOK, mcpTextBody(t, tc.body))
			got, err := mustClient(t, srv).ClaimTask(ctx, tc.want.Task.ID, tc.takeover)
			if err != nil {
				t.Fatalf("ClaimTask: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("claimed = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// takeover is sent only when it is asked for: its absence is the fresh claim.
func TestClaimTaskSendsTakeoverOnlyWhenAsked(t *testing.T) {
	ctx := context.Background()
	body := `{"task":{"id":"t-1"},"branch":"clanker/t-1","run":{"id":"uuid-1.secret-1"}}`

	t.Run("fresh claim", func(t *testing.T) {
		srv, got := serve(t, http.StatusOK, mcpTextBody(t, body))
		if _, err := mustClient(t, srv).ClaimTask(ctx, "t-1", false); err != nil {
			t.Fatalf("ClaimTask: %v", err)
		}
		if name := toolName(t, got.body); name != "claim_task" {
			t.Errorf("tool = %q, want claim_task", name)
		}
		args := toolArgs(t, got.body)
		if args["taskId"] != "t-1" {
			t.Errorf("taskId = %v, want t-1", args["taskId"])
		}
		if _, ok := args["takeover"]; ok {
			t.Errorf("a fresh claim carried takeover: %+v", args)
		}
	})

	t.Run("takeover", func(t *testing.T) {
		srv, got := serve(t, http.StatusOK, mcpTextBody(t, body))
		if _, err := mustClient(t, srv).ClaimTask(ctx, "t-1", true); err != nil {
			t.Fatalf("ClaimTask: %v", err)
		}
		if args := toolArgs(t, got.body); args["takeover"] != true {
			t.Errorf("arguments = %+v, want takeover=true", args)
		}
	})
}

func TestClaimTaskRefusesAnAnswerWithNoRun(t *testing.T) {
	srv, _ := serve(t, http.StatusOK, mcpTextBody(t, `{"task":{"id":"t-1"},"branch":"clanker/t-1"}`))

	if _, err := mustClient(t, srv).ClaimTask(context.Background(), "t-1", false); err == nil {
		t.Fatal("a claim with no run id must be an error: nothing could be signed with it")
	}
}

func TestClaimTaskRefusesAnEmptyTaskIDLocally(t *testing.T) {
	if _, err := mustClient(t, nil).ClaimTask(context.Background(), "", false); err == nil {
		t.Error("ClaimTask with an empty taskId must be refused before any request")
	}
}

// refusalBody is the plane's answer to a refused tool call: HTTP 200 with
// result.isError and the ToolError payload carrying a machine-readable code.
func refusalBody(t *testing.T, code, message string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]map[string]string{"error": {"code": code, "message": message}})
	if err != nil {
		t.Fatalf("marshal refusal: %v", err)
	}
	return `{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":` + mustJSONString(string(payload)) + `}]}}`
}

// The codes are grouped by the answer they demand: lost means take another
// task, not claimable means stop offering this one.
func TestClaimTaskRefusalClasses(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name string
		code string
		want error
	}{
		{name: "claim lost", code: "claim_lost", want: ErrClaimLost},
		{name: "not ready", code: "not_ready", want: ErrClaimLost},
		{name: "lease live", code: "lease_live", want: ErrClaimLost},
		{name: "not stale", code: "not_stale", want: ErrClaimLost},
		{name: "not found", code: "not_found", want: ErrClaimLost},
		{name: "not claimable", code: "not_claimable", want: ErrNotClaimable},
		{name: "human only", code: "human_only", want: ErrNotClaimable},
		{name: "is container", code: "is_container", want: ErrNotClaimable},
		{name: "reclaim bound exhausted", code: "reclaim_bound_exhausted", want: ErrNotClaimable},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := serve(t, http.StatusOK, refusalBody(t, tc.code, "the plane says "+tc.code))
			_, err := mustClient(t, srv).ClaimTask(ctx, "t-1", true)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			// The plane's own message is what tells the caller what to do next.
			if !strings.Contains(err.Error(), "the plane says "+tc.code) {
				t.Errorf("err = %v, want the plane's message carried", err)
			}
			other := ErrNotClaimable
			if tc.want == ErrNotClaimable {
				other = ErrClaimLost
			}
			if errors.Is(err, other) {
				t.Errorf("err = %v also matches %v; the classes must stay apart", err, other)
			}
		})
	}
}

// A refusal this build does not name, and a JSON-RPC level failure, are passed
// through untouched: classifying them would invent an answer.
func TestClaimTaskRefusalsItCannotClassifyPassThrough(t *testing.T) {
	ctx := context.Background()

	t.Run("unnamed code", func(t *testing.T) {
		srv, _ := serve(t, http.StatusOK, refusalBody(t, "internal_error", "boom"))
		_, err := mustClient(t, srv).ClaimTask(ctx, "t-1", false)
		if err == nil || !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "internal_error") {
			t.Fatalf("err = %v, want the plane's refusal carried through", err)
		}
		if errors.Is(err, ErrClaimLost) || errors.Is(err, ErrNotClaimable) {
			t.Errorf("err = %v was classified into a claim outcome it does not belong to", err)
		}
	})

	t.Run("json-rpc error", func(t *testing.T) {
		srv, _ := serve(t, http.StatusOK, `{"jsonrpc":"2.0","id":1,"error":{"message":"boom"}}`)
		_, err := mustClient(t, srv).ClaimTask(ctx, "t-1", false)
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("err = %v, want the transport failure carried through", err)
		}
		if errors.Is(err, ErrClaimLost) || errors.Is(err, ErrNotClaimable) {
			t.Errorf("err = %v was classified into a claim outcome it does not belong to", err)
		}
	})
}

func TestTaskClaimerNotWired(t *testing.T) {
	var claimer TaskClaimer = notWired{}
	ctx := context.Background()

	if _, err := claimer.NextOffer(ctx); !errors.Is(err, ErrNotWired) {
		t.Errorf("NextOffer err = %v, want ErrNotWired", err)
	}
	if _, err := claimer.ClaimTask(ctx, "t-1", true); !errors.Is(err, ErrNotWired) {
		t.Errorf("ClaimTask err = %v, want ErrNotWired", err)
	}
}
