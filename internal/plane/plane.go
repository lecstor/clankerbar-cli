// Package plane is the driver's small WRITE client for the clankerbar control
// plane. It is deliberately separate from internal/backlog, which is the cheap
// read-only view and says so in its own doc comment.
//
// Today it makes exactly one call: handing back a task the harness session was
// still holding when it ended (CLA-242). Without it, a session that dies mid-task
// — killed by a usage limit, or simply finishing its turn with work unfinished —
// leaves a 30-minute lease ticking down with nobody heartbeating it. The plane
// eventually sweeps that up, but it charges the task a reclaim to do so, and the
// reclaim budget is only two before the task is parked for the operator.
//
// The transport is MCP's JSON-RPC over HTTP, POSTed to the same `/mcp/<slug>`
// endpoint the harness sessions use, with the operator's API key as a bearer
// token. clankerbar's MCP server is stateless, so there is no initialize
// handshake to perform; a response comes back either as a plain JSON body or as
// a single SSE frame, and both are accepted.
package plane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/secureurl"
)

// ErrNotWired means no endpoint or API key was configured, so there is nothing to
// release against. The driver treats this as "skip the release", not as a failure
// — exactly as backlog.ErrNotWired degrades the poll to blind mode rather than
// stopping the run.
var ErrNotWired = errors.New("plane writes not wired")

// Releaser hands a claim back to the queue.
type Releaser interface {
	// Release ends the run holding taskID and returns the task to `ready`, so the
	// next iteration can claim it immediately instead of waiting out a dead lease.
	//
	// It must only be called for a task with NO work-in-progress branch recorded.
	// Releasing one that HAS a branch would be actively worse than doing nothing:
	// `requiresTakeover` is computed only for an `in_progress` task with a dead
	// lease, so moving the task to `ready` discards the very hint that warns the
	// next clanker there is pushed work to pick up. That trap is on the record —
	// it is what standing decision 15 (CLA-191) was written about. The WIP case
	// wants a plane-side primitive that preserves the handoff; until that exists,
	// the driver leaves those tasks to expire.
	Release(ctx context.Context, taskID, runID string) error
}

// Attester records the driver's own verdict on a delivery a session declared.
//
// `delivery.mergeVerified` is an attestation the plane STORES and does not
// confirm — it is the clanker saying "I ran `git merge-base --is-ancestor`". The
// driver is the one process in the loop that actually can, so when it has run that
// check it writes down what it found (CLA-253), true or false. It writes nothing
// when the check could not run: an absent attestation is honest, a fabricated one
// is the exact failure this exists to catch.
//
// It is a SEPARATE interface from Releaser on purpose. The driver type-asserts for
// it, so a Releaser that cannot attest (a not-wired plane, a test double) degrades
// to warn-only rather than failing to compile or failing to run.
type Attester interface {
	AttestMergeVerified(ctx context.Context, taskID, runID string, d Delivery, verified bool) error
}

// Delivery is the declaration being attested to, echoed back exactly as the
// session made it. The whole thing is resent, not just the two fields the check
// used: `update_task` takes `delivery` as an object, and a partial one risks
// dropping whatever the session put in the fields this driver did not name.
type Delivery struct {
	Commit            string
	IntegrationBranch string
	PR                string
}

type notWired struct{}

func (notWired) Release(context.Context, string, string) error { return ErrNotWired }

// New builds a Releaser. Missing either the endpoint or the key yields a
// not-wired one, so an operator running without a configured plane is degraded
// rather than broken.
//
// mcpURL is the project-scoped MCP endpoint (`https://…/mcp/<slug>`).
func New(mcpURL, apiKey string) Releaser {
	if mcpURL == "" || apiKey == "" {
		return notWired{}
	}
	return &mcpReleaser{
		endpoint: strings.TrimRight(mcpURL, "/"),
		apiKey:   apiKey,
		client:   &http.Client{Timeout: 20 * time.Second, CheckRedirect: noDowngradeRedirect},
	}
}

type mcpReleaser struct {
	endpoint string
	apiKey   string
	client   *http.Client
}

func (r *mcpReleaser) Release(ctx context.Context, taskID, runID string) error {
	if taskID == "" || runID == "" {
		return errors.New("release: taskId and runId are both required")
	}
	// `status: ready` is what returns the task to the claimable queue. It clears
	// the holder and — unlike the plane's own expiry sweep — does not charge the
	// task a reclaim, which is the whole point of releasing rather than going
	// quiet. `runId` signs the write, so the revision is credited to this run.
	//
	// No `outcome` is sent on purpose: the session may have written one, and this
	// call is a handback, not a report. Clobbering its words would lose the only
	// trace of what actually happened.
	return r.call(ctx, "update_task", map[string]any{
		"taskId": taskID,
		"runId":  runID,
		"status": "ready",
	})
}

// AttestMergeVerified writes the driver's verdict onto the task's delivery record.
//
// The declaration is echoed back exactly as the session made it, so the stored
// delivery stays internally consistent: an attestation has to name the thing it is
// about, or a later reader cannot tell what was checked. No `status` and no
// `outcome` are sent — this revises a record, it does not move the task or
// overwrite the session's own words.
//
// It runs AFTER the session ended, which is later than any other write here. The
// plane credits a `runId` while it is the task's most recent run — which it still
// is once the task is handed to review and the lock released — so this is a
// supported write rather than a stolen one. If the plane refuses it anyway, the
// caller logs and carries on: the loud log already carries the finding.
func (r *mcpReleaser) AttestMergeVerified(ctx context.Context, taskID, runID string, d Delivery, verified bool) error {
	if taskID == "" || runID == "" {
		return errors.New("attest: taskId and runId are both required")
	}
	if d.Commit == "" || d.IntegrationBranch == "" {
		return errors.New("attest: the delivery being attested must name a commit and an integration branch")
	}
	delivery := map[string]any{
		"commit":            d.Commit,
		"integrationBranch": d.IntegrationBranch,
		"mergeVerified":     verified,
	}
	if d.PR != "" {
		delivery["pr"] = d.PR
	}
	return r.call(ctx, "update_task", map[string]any{
		"taskId":   taskID,
		"runId":    runID,
		"delivery": delivery,
	})
}

// call performs one MCP `tools/call` and reports whether it succeeded.
func (r *mcpReleaser) call(ctx context.Context, tool string, args map[string]any) error {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.apiKey)
	req.Header.Set("Content-Type", "application/json")
	// Streamable HTTP may answer with either shape; ask for both and parse either.
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d: %s", tool, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return checkResult(tool, raw)
}

// checkResult decodes a tools/call response and turns a transport-level or
// tool-level failure into an error. Both matter: MCP reports a refused tool call
// as a 200 with `result.isError`, not as an HTTP status.
func checkResult(tool string, raw []byte) error {
	payload := sseData(raw)
	var rpc struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(payload, &rpc); err != nil {
		return fmt.Errorf("%s: decode response: %w", tool, err)
	}
	if rpc.Error != nil {
		return fmt.Errorf("%s: %s", tool, rpc.Error.Message)
	}
	if rpc.Result.IsError {
		var msg strings.Builder
		for _, c := range rpc.Result.Content {
			msg.WriteString(c.Text)
		}
		return fmt.Errorf("%s refused: %s", tool, strings.TrimSpace(msg.String()))
	}
	return nil
}

// sseData pulls the JSON payload out of an SSE frame, or returns the body
// unchanged when it is already plain JSON.
func sseData(raw []byte) []byte {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		return trimmed
	}
	for _, line := range bytes.Split(trimmed, []byte("\n")) {
		if after, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:")); ok {
			return bytes.TrimSpace(after)
		}
	}
	return trimmed
}

// noDowngradeRedirect refuses a redirect that would put the bearer token on the
// wire in cleartext. Go already strips Authorization when a redirect changes
// host, but it FORWARDS it on an https -> http hop to the SAME host — which is
// exactly the cleartext exposure the credential-origin rule exists to prevent
// (CLA-257), arriving by a route config validation cannot see.
func noDowngradeRedirect(req *http.Request, _ []*http.Request) error {
	if _, err := secureurl.Origin(req.URL.String()); err != nil {
		return fmt.Errorf("refusing redirect: %w", err)
	}
	return nil
}
