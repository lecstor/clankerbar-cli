package plane

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestClaimConversationEmpty(t *testing.T) {
	srv, _ := serve(t, http.StatusOK, rcResult(`{"conversation":null}`))
	defer srv.Close()
	claim, err := NewConversationAPI(srv.URL+"/mcp/demo", "k").ClaimConversation(context.Background(), ConversationClaimOptions{})
	if err != nil {
		t.Fatalf("ClaimConversation: %v", err)
	}
	if claim != nil {
		t.Fatalf("claim = %+v, want nil for an empty claim", claim)
	}
}

func TestClaimConversationDecodesTheTurn(t *testing.T) {
	payload := `{
		"conversation":{"id":"c-1","title":"Queue","pendingCount":2},
		"messages":[{"id":"m-3","seq":3,"authorKind":"operator","authorSource":"console","authorName":"alice","at":"2026-11-18T10:00:00.000Z","body":"first"}],
		"context":[{"id":"m-2","seq":2,"authorKind":"planner","authorSource":"mcp_self","authorName":"planner","snippet":"earlier...","truncated":true}],
		"contextTotal":7,
		"subjects":[{"kind":"task","id":"t-1"}],
		"resume":{"harness":"claude","ref":"ref-1"},
		"leaseExpiresIn":1020,
		"claimToken":"c-1.secret",
		"note":"a body was truncated"
	}`
	srv, got := serve(t, http.StatusOK, rcResult(payload))
	defer srv.Close()
	claim, err := NewConversationAPI(srv.URL+"/mcp/demo", "k").ClaimConversation(context.Background(), ConversationClaimOptions{
		ConversationID: "c-1",
		Instance:       "host/planner",
		History:        "full",
	})
	if err != nil {
		t.Fatalf("ClaimConversation: %v", err)
	}
	if claim == nil {
		t.Fatal("claim is nil")
	}
	if claim.ID != "c-1" || claim.Title != "Queue" || claim.PendingCount != 2 {
		t.Errorf("conversation = %+v", claim)
	}
	if len(claim.Messages) != 1 || claim.Messages[0].Body != "first" || claim.Messages[0].Seq != 3 {
		t.Errorf("messages = %+v", claim.Messages)
	}
	if len(claim.Context) != 1 || claim.Context[0].Snippet != "earlier..." || !claim.Context[0].Truncated || claim.ContextTotal != 7 {
		t.Errorf("context = %+v total=%d", claim.Context, claim.ContextTotal)
	}
	if len(claim.Subjects) != 1 || !strings.Contains(string(claim.Subjects[0]), `"t-1"`) {
		t.Errorf("subjects = %s", claim.Subjects)
	}
	if claim.Resume == nil || claim.Resume.Harness != "claude" || claim.Resume.Ref != "ref-1" {
		t.Errorf("resume = %+v", claim.Resume)
	}
	if claim.LeaseExpiresIn != 1020 || claim.ClaimToken != "c-1.secret" || claim.Note == "" {
		t.Errorf("lease/token/note = %d/%q/%q", claim.LeaseExpiresIn, claim.ClaimToken, claim.Note)
	}
	params, _ := got.body["params"].(map[string]any)
	if params["name"] != "claim_conversation" {
		t.Fatalf("tool = %v, want claim_conversation", params["name"])
	}
	args, _ := params["arguments"].(map[string]any)
	if args["conversationId"] != "c-1" || args["instance"] != "host/planner" || args["history"] != "full" {
		t.Errorf("arguments = %+v, want the claim's three options", args)
	}
}

func TestClaimConversationRefusesAnAnonymisedClaim(t *testing.T) {
	srv, _ := serve(t, http.StatusOK, rcResult(`{"conversation":{"id":"c-1"},"messages":[]}`))
	defer srv.Close()
	_, err := NewConversationAPI(srv.URL+"/mcp/demo", "k").ClaimConversation(context.Background(), ConversationClaimOptions{})
	if err == nil || !strings.Contains(err.Error(), "claimToken") {
		t.Fatalf("err = %v, want a refusal naming the missing claim token", err)
	}
}

func TestClaimConversationSurfacesAToolRefusal(t *testing.T) {
	refusal := `{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"planner_not_configured: no roles.planner"}]}}`
	srv, _ := serve(t, http.StatusOK, refusal)
	defer srv.Close()
	claim, err := NewConversationAPI(srv.URL+"/mcp/demo", "k").ClaimConversation(context.Background(), ConversationClaimOptions{})
	if err == nil || !strings.Contains(err.Error(), "planner_not_configured") {
		t.Fatalf("err = %v, want the plane's refusal", err)
	}
	if claim != nil {
		t.Fatalf("claim = %+v alongside an error", claim)
	}
}

func TestReadConversationDecodesAPage(t *testing.T) {
	payload := `{
		"conversation":{"id":"c-1","title":"Queue"},
		"messages":[{"id":"m-1","seq":1,"authorKind":"operator","authorName":"alice","body":"oldest"}],
		"count":1,
		"hasMore":true,
		"nextBeforeSeq":1
	}`
	srv, got := serve(t, http.StatusOK, rcResult(payload))
	defer srv.Close()
	page, err := NewConversationAPI(srv.URL+"/mcp/demo", "k").ReadConversation(context.Background(), "c-1", 11, 10)
	if err != nil {
		t.Fatalf("ReadConversation: %v", err)
	}
	if len(page.Messages) != 1 || page.Messages[0].Seq != 1 || page.Messages[0].Body != "oldest" {
		t.Errorf("messages = %+v", page.Messages)
	}
	if !page.HasMore || page.NextBeforeSeq != 1 || page.Count != 1 {
		t.Errorf("page flags = %+v", page)
	}
	params, _ := got.body["params"].(map[string]any)
	if params["name"] != "read_conversation" {
		t.Fatalf("tool = %v, want read_conversation", params["name"])
	}
	args, _ := params["arguments"].(map[string]any)
	if args["conversationId"] != "c-1" || args["beforeSeq"] != float64(11) || args["limit"] != float64(10) {
		t.Errorf("arguments = %+v", args)
	}
}

func TestReadConversationRequiresAnID(t *testing.T) {
	_, err := NewConversationAPI("https://plane.example/mcp/demo", "k").ReadConversation(context.Background(), "", 0, 0)
	if err == nil {
		t.Fatal("ReadConversation accepted an empty conversation id")
	}
}

func TestConversationAPINotWired(t *testing.T) {
	api := NewConversationAPI("", "")
	if _, err := api.ClaimConversation(context.Background(), ConversationClaimOptions{}); !errors.Is(err, ErrNotWired) {
		t.Errorf("ClaimConversation err = %v, want ErrNotWired", err)
	}
	if _, err := api.ReadConversation(context.Background(), "c-1", 0, 0); !errors.Is(err, ErrNotWired) {
		t.Errorf("ReadConversation err = %v, want ErrNotWired", err)
	}
}
