package plane

// closeout_test.go — CLA-758's close-out surface. What matters on the wire: the
// claim's run credential is READ back (every later call signs with it), the release
// carries the driver marker and the check-green attestation so a failed merge is not
// charged to the CI-repair ladder, and the close sends the delivery and NO outcome —
// an absent outcome is what makes the plane inherit the reviewed text the approval
// was given for.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// toolArgs pulls params.arguments out of a captured request.
func toolArgs(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	params, ok := body["params"].(map[string]any)
	if !ok {
		t.Fatalf("request carries no params: %v", body)
	}
	args, ok := params["arguments"].(map[string]any)
	if !ok {
		t.Fatalf("call carries no arguments: %v", params)
	}
	return args
}

// toolName pulls params.name out of a captured request.
func toolName(t *testing.T, body map[string]any) string {
	t.Helper()
	params, _ := body["params"].(map[string]any)
	name, _ := params["name"].(string)
	return name
}

func TestProjectPolicyReadsTheEffectiveDial(t *testing.T) {
	srv, got := serve(t, http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"policy\":{\"authorityIntegrate\":\"human\",\"topology\":\"staging_then_promote\"},\"pendingProposal\":null}"}]}}`)

	policy, err := NewCloseOutAPI(srv.URL+"/mcp/demo", "k-1").ProjectPolicy(context.Background())
	if err != nil {
		t.Fatalf("ProjectPolicy: %v", err)
	}
	if policy.AuthorityIntegrate != "human" {
		t.Errorf("AuthorityIntegrate = %q, want human", policy.AuthorityIntegrate)
	}
	if name := toolName(t, got.body); name != "get_project_policy" {
		t.Errorf("tool = %q, want get_project_policy", name)
	}
}

func TestApprovedForCloseOutAsksForTheFullView(t *testing.T) {
	const tasks = `{"tasks":[{"id":"t-1","ref":"CLA-1","title":"x","branch":"clanker/x","repo":"acme/widgets"}],` +
		`"count":1,"total":3,"view":"full"}`
	srv, got := serve(t, http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":`+
		mustJSONString(tasks)+`}]}}`)

	queue, err := NewCloseOutAPI(srv.URL+"/mcp/demo", "k-1").ApprovedForCloseOut(context.Background())
	if err != nil {
		t.Fatalf("ApprovedForCloseOut: %v", err)
	}
	args := toolArgs(t, got.body)
	if args["status"] != "approved" || args["view"] != "full" {
		t.Errorf("arguments = %+v, want status=approved view=full (the branch and repo are only in the full view)", args)
	}
	if len(queue.Tasks) != 1 || queue.Tasks[0].ID != "t-1" || queue.Tasks[0].Branch != "clanker/x" || queue.Tasks[0].Repo != "acme/widgets" {
		t.Fatalf("queue = %+v, want the one approved task", queue)
	}
	if queue.Total != 3 {
		t.Errorf("Total = %d, want 3 (the cap must be visible to the caller)", queue.Total)
	}
}

func TestClaimCloseOutReturnsTheRunCredential(t *testing.T) {
	srv, got := serve(t, http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"task\":{\"id\":\"t-1\"},\"run\":{\"id\":\"uuid-1.secret-1\"}}"}]}}`)

	runID, err := NewCloseOutAPI(srv.URL+"/mcp/demo", "k-1").ClaimCloseOut(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("ClaimCloseOut: %v", err)
	}
	if runID != "uuid-1.secret-1" {
		t.Errorf("runID = %q, want the compound credential the plane returned", runID)
	}
	if name := toolName(t, got.body); name != "claim_task" {
		t.Errorf("tool = %q, want claim_task", name)
	}
	if args := toolArgs(t, got.body); args["taskId"] != "t-1" {
		t.Errorf("arguments = %+v, want taskId=t-1", args)
	}
}

func TestClaimCloseOutRefusesAnAnswerWithNoRun(t *testing.T) {
	srv, _ := serve(t, http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"task\":{\"id\":\"t-1\"}}"}]}}`)

	if _, err := NewCloseOutAPI(srv.URL+"/mcp/demo", "k-1").ClaimCloseOut(context.Background(), "t-1"); err == nil {
		t.Fatal("a claim with no run id must be an error: nothing could be signed with it")
	}
}

func TestReleaseCloseOutAttestsGreenAndTheDriver(t *testing.T) {
	srv, got := serve(t, http.StatusOK, okBody)

	if err := NewCloseOutAPI(srv.URL+"/mcp/demo", "k-1").ReleaseCloseOut(context.Background(), "t-1", "run-1", true); err != nil {
		t.Fatalf("ReleaseCloseOut: %v", err)
	}
	if name := toolName(t, got.body); name != "release_task" {
		t.Errorf("tool = %q, want release_task", name)
	}
	args := toolArgs(t, got.body)
	if args["driver"] != true || args["checkGreen"] != true {
		t.Errorf("arguments = %+v, want driver+checkGreen", args)
	}
	// A release moves nothing: no expiry, no status, no delivery, no outcome.
	for _, forbidden := range []string{"expire", "status", "delivery", "outcome"} {
		if _, ok := args[forbidden]; ok {
			t.Errorf("release carries %q, which it must not: %+v", forbidden, args)
		}
	}
}

func TestReleaseCloseOutSendsTheAttestationItWasGiven(t *testing.T) {
	srv, got := serve(t, http.StatusOK, okBody)

	if err := NewCloseOutAPI(srv.URL+"/mcp/demo", "k-1").ReleaseCloseOut(context.Background(), "t-1", "run-1", false); err != nil {
		t.Fatalf("ReleaseCloseOut: %v", err)
	}
	args := toolArgs(t, got.body)
	if args["checkGreen"] != false {
		t.Errorf("checkGreen = %v, want false: a failed MERGE must be counted by the repair ladder", args["checkGreen"])
	}
}

func TestReleaseCloseOutDoesNotRetryAnUnknownTool(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"MCP error -32602: Tool release_task not found"}],"isError":true}}`)
	}))
	t.Cleanup(srv.Close)

	err := NewCloseOutAPI(srv.URL+"/mcp/demo", "k-1").ReleaseCloseOut(context.Background(), "t-1", "run-1", true)
	if !errors.Is(err, ErrPlaneTooOld) {
		t.Fatalf("err = %v, want ErrPlaneTooOld", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want exactly 1: there is no fallback to retry with", calls)
	}
}

func TestCompleteCloseOutSendsTheDeliveryAndNoOutcome(t *testing.T) {
	srv, got := serve(t, http.StatusOK, okBody)

	err := NewCloseOutAPI(srv.URL+"/mcp/demo", "k-1").CompleteCloseOut(context.Background(), "t-1", "run-1", CloseOutDelivery{
		Commit:            "38e21749",
		PR:                "#12",
		IntegrationBranch: "staging",
		MergeVerified:     true,
	})
	if err != nil {
		t.Fatalf("CompleteCloseOut: %v", err)
	}
	if name := toolName(t, got.body); name != "complete_task" {
		t.Errorf("tool = %q, want complete_task", name)
	}
	args := toolArgs(t, got.body)
	if args["runId"] != "run-1" || args["taskId"] != "t-1" {
		t.Errorf("arguments = %+v, want the close signed by the claim's run", args)
	}
	if _, ok := args["status"]; ok {
		t.Errorf("complete_task has no status field, but the call carried one: %+v", args)
	}
	if _, ok := args["outcome"]; ok {
		t.Errorf("the close asserts an outcome; it must inherit the reviewed one: %+v", args)
	}
	delivery, ok := args["delivery"].(map[string]any)
	if !ok {
		t.Fatalf("no delivery block: %+v", args)
	}
	if delivery["commit"] != "38e21749" || delivery["pr"] != "#12" ||
		delivery["integrationBranch"] != "staging" || delivery["mergeVerified"] != true {
		t.Errorf("delivery = %+v, want the merge commit, PR, base and the verified attestation", delivery)
	}
}

func TestCompleteCloseOutNeedsACommitAndABase(t *testing.T) {
	api := NewCloseOutAPI("https://plane.example/mcp/demo", "k-1")
	if err := api.CompleteCloseOut(context.Background(), "t-1", "run-1", CloseOutDelivery{PR: "#1"}); err == nil {
		t.Error("a delivery with no commit must be refused")
	}
	if err := api.CompleteCloseOut(context.Background(), "t-1", "run-1", CloseOutDelivery{Commit: "abc"}); err == nil {
		t.Error("a delivery with no integration branch must be refused")
	}
}

func TestNewCloseOutAPINotWiredRefusesAndStaysNarrow(t *testing.T) {
	api := NewCloseOutAPI("", "")
	ctx := context.Background()

	if _, err := api.ProjectPolicy(ctx); !errors.Is(err, ErrNotWired) {
		t.Errorf("ProjectPolicy err = %v, want ErrNotWired", err)
	}
	if _, err := api.ApprovedForCloseOut(ctx); !errors.Is(err, ErrNotWired) {
		t.Errorf("ApprovedForCloseOut err = %v, want ErrNotWired", err)
	}
	if _, err := api.ClaimCloseOut(ctx, "t-1"); !errors.Is(err, ErrNotWired) {
		t.Errorf("ClaimCloseOut err = %v, want ErrNotWired", err)
	}
	if err := api.ReleaseCloseOut(ctx, "t-1", "r-1", true); !errors.Is(err, ErrNotWired) {
		t.Errorf("ReleaseCloseOut err = %v, want ErrNotWired", err)
	}
	if err := api.CompleteCloseOut(ctx, "t-1", "r-1", CloseOutDelivery{Commit: "a", IntegrationBranch: "b"}); !errors.Is(err, ErrNotWired) {
		t.Errorf("CompleteCloseOut err = %v, want ErrNotWired", err)
	}

	// Narrow on purpose: a close-out client must not be assertable into another
	// capability it never asked for.
	if r, ok := api.(Releaser); ok {
		t.Errorf("the not-wired close-out client satisfies Releaser (%T); a caller could release with it", r)
	}
}

// mustJSONString quotes a JSON document for embedding in an MCP text payload.
func mustJSONString(s string) string {
	raw, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(raw)
}
