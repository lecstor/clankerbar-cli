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

// ErrQuestionNotFiled wraps a park that moved the task to `parked` but then
// failed to file the question that raises it to the operator. The task IS
// parked — it will not be retried by a clanker — so the caller must not report
// "the park failed, the task is left to the next claim" (which is false) but
// "parked, and the question is missing" (which the operator needs to know)
// (review finding).
var ErrQuestionNotFiled = errors.New("task parked but the question was not filed")

// Releaser hands a claim back to the queue.
type Releaser interface {
	// Release ends the run holding taskID as `released` (never `failed`) and
	// returns a no-WIP task to `ready`, so the next iteration can claim it
	// immediately instead of waiting out a dead lease. It sends `release: true`,
	// the plane's hand-an-unfinished-claim-back primitive (CLA-246), which also
	// leaves the reclaim budget untouched.
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

// Heartbeat renews an active claim's lease. The loop calls it periodically
// while a session holds a claim, so the 30-minute lease does not expire
// mid-session (CLA-358).
type Heartbeat interface {
	Heartbeat(ctx context.Context, runID string) error
}

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

// Recorder records a work-in-progress branch on a task - the hand-off record
// that turns a redo into a resume (CLA-314).
//
// It is deliberately NOT part of Releaser, and the difference is the whole point:
// a release moves the task to `ready` and ends the run holding it, while this
// carries no status at all. Plane-side, `updateStatus` is what clears a holder,
// and a call with no status never reaches it - so recording a branch cannot
// settle a claim, cannot hand a task back, and cannot revert one that has already
// reached review. That is what makes it safe on the CLA-262 untrusted path, where
// handing the claim back is forbidden precisely because a settle we never saw may
// be in the bytes that never arrived.
//
// A separate interface, like Attester, so a not-wired plane or a test double
// degrades to "the work is pushed and the log says where" rather than failing to
// compile.
type Recorder interface {
	// RecordBranch writes branch onto taskID as its work-in-progress hand-off.
	// It must only be called for a branch that is really ON THE REMOTE: the whole
	// meaning of the field is that fetchable work exists, and a branch living on
	// one laptop sends the next clanker - routinely on another machine - to fetch
	// nothing.
	RecordBranch(ctx context.Context, taskID, runID, branch string) error
}

// ParkAPI is the optional interface a Releaser may implement to let the driver
// declare a failure the SESSION cannot: a phase that dies producing nothing is
// dead, so nobody is left to move the task — the driver has to (CLA-386).
//
// It is the one write the driver makes about a task's STATUS, and it is kept
// deliberately narrow. Release returns a held claim to the queue; this parks a
// task that should reach the operator rather than be retried by the next clanker
// — the two consecutive-dead-phase case. The park files an OPEN question
// alongside the status write, so the operator actually sees the park: a
// recorded decision is born answered and raises nothing (CLA-395). The question
// is deliberately a non-blocking `clarification`, not a blocking `decision`:
// the task is already parked, so there is nothing left to block (and a blocking
// question would un-block to `ready` on any answer, handing the task straight
// back to the fleet the guard just withheld it from), and `decision` questions
// are the project's STANDING DECISIONS — filed as a ruling it would push real
// standing decisions out of that window and read as project-wide law.
//
// A separate interface, like Recorder, so a not-wired plane or a test double
// degrades to a loud log line rather than failing to compile.
type ParkAPI interface {
	// Park moves taskID to `parked` with outcome. Signed with runID. The
	// outcome is the standalone record for when the question insert below fails:
	// it must say what happened and that the call is the operator's.
	Park(ctx context.Context, taskID, runID, outcome string) error

	// AskQuestion files an OPEN question on taskID — the thing that makes a
	// parked task reach the operator instead of vanishing into the Done tab.
	// The caller passes blocking and kind explicitly so the dead-phase park can
	// file a non-blocking clarification; the wire shape is asserted in tests.
	AskQuestion(ctx context.Context, taskID, body string, options []string, blocking bool, kind string) error

	// AskProjectQuestion files an OPEN question at PROJECT level — no taskId —
	// the shape a fleet-wide event raises (CLA-396). The fleet dead-phase
	// counter tripping is not one task's triage but a project-level condition
	// ("the provider is broken right now"), so the question must not be pinned
	// to the bystander task that happened to be in flight. It is always
	// non-blocking: there is no task to block, and the loop's pause — not a
	// blocking flag — is what stops the work. The kind is the caller's call,
	// so the driver can file a project-level decision.
	AskProjectQuestion(ctx context.Context, body string, options []string, kind string) error
}

type notWired struct{}

func (notWired) Release(context.Context, string, string) error    { return ErrNotWired }
func (notWired) Heartbeat(context.Context, string) error          { return ErrNotWired }
func (notWired) PeekNextTask(context.Context) (NextTask, error)   { return NextTask{}, ErrNotWired }
func (notWired) TaskRepo(context.Context, string) (string, error) { return "", ErrNotWired }
func (notWired) TaskState(context.Context, string) (TaskState, error) {
	return TaskState{}, ErrNotWired
}

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
	// `release: true` is the plane's hand-an-unfinished-claim-back primitive
	// (CLA-246): it returns a no-WIP task to the claimable queue WITHOUT charging
	// it a reclaim, and — unlike moving it with a status — records the run as
	// `released`, never `failed`. It cannot be combined with `status` or
	// `delivery` (a bare release IS the call; the plane refuses the combination),
	// and no `outcome` is sent: the session may have written one, and this call is
	// a handback, not a report.
	return r.call(ctx, "update_task", map[string]any{
		"taskId":  taskID,
		"runId":   runID,
		"release": true,
	})
}

// RecordBranch writes a work-in-progress branch onto a task, so a session that
// died before it could hand over still leaves a hand-off behind (CLA-314).
//
// No `status`, no `outcome`, no `delivery`. That is not minimalism, it is the
// safety property: this call has to be legal on a session whose stream could not
// be read whole, where the driver may be wrong about whether the task is still
// held. A `branch` on its own revises a record; it cannot move a task, cannot
// clear a holder, and cannot post `ready` over something already in review.
//
// `runId` signs it. The plane credits the run while it is the task's most recent
// one - which it still is after the session has ended - and refuses a superseded
// one outright, which is the correct answer: a later run already owns the task
// and its own branch record is the current one.
func (r *mcpReleaser) RecordBranch(ctx context.Context, taskID, runID, branch string) error {
	if taskID == "" || runID == "" {
		return errors.New("record branch: taskId and runId are both required")
	}
	if branch == "" {
		return errors.New("record branch: refusing to record an empty branch")
	}
	return r.call(ctx, "update_task", map[string]any{
		"taskId": taskID,
		"runId":  runID,
		"branch": branch,
	})
}

// Park parks a task the driver has decided should reach the operator (CLA-386).
// Unlike Release — which returns a held claim to the ready queue — this moves
// the task to `parked`, the status a human reviews, because a task that has
// killed two consecutive sessions must not be offered to a third.
//
// The OUTCOME is the standalone record: the park commits before any question is
// filed (the caller files it with AskQuestion), so if that insert fails this
// text is all the operator gets, and it must still say what happened and that
// the call is theirs — exactly as the plane's own escalateExhausted reasons
// about its ordering (CLA-186).
func (r *mcpReleaser) Park(ctx context.Context, taskID, runID, outcome string) error {
	if taskID == "" || runID == "" {
		return errors.New("park: taskId and runId are both required")
	}
	return r.call(ctx, "update_task", map[string]any{
		"taskId":  taskID,
		"runId":   runID,
		"status":  "parked",
		"outcome": outcome,
	})
}

// AskQuestion files an OPEN question on a task (CLA-395). It is what makes a
// park reach the operator: a recorded decision is born answered and raises no
// open question, so a task parked with only a decision never surfaces anywhere.
//
// blocking and kind are passed through deliberately — the dead-phase park files
// a non-blocking `clarification`, and the caller's choice is asserted in tests
// rather than buried in the client. multiSelect is not sent: the plane defaults
// it to false (single pick), and the park's options are one-of-four.
//
// A failure here wraps ErrQuestionNotFiled: the task IS parked — it will not be
// retried — so the caller must report "parked, and the question is missing"
// rather than "the park failed".
func (r *mcpReleaser) AskQuestion(ctx context.Context, taskID, body string, options []string, blocking bool, kind string) error {
	if taskID == "" || body == "" {
		return errors.New("ask question: taskId and body are both required")
	}
	args := map[string]any{
		"taskId":   taskID,
		"body":     body,
		"blocking": blocking,
		"kind":     kind,
	}
	if len(options) > 0 {
		args["options"] = options
	}
	if err := r.call(ctx, "ask_question", args); err != nil {
		return fmt.Errorf("%w: %v", ErrQuestionNotFiled, err)
	}
	return nil
}

// AskProjectQuestion files an OPEN question with NO taskId — the project-level
// sibling of AskQuestion (CLA-396). A fleet trip is not one task's triage: the
// dead-phase counter tripping across tasks means the provider or harness is
// broken right now, and pinning the question to whichever task was in flight
// would make the operator answer about a bystander. The plane's ask_question
// takes taskId optionally, so omitting it is the wire shape; non-blocking is
// hard-coded because there is no task to block (the loop's own pause is the
// enforcement, and the driver clears it when this question is answered).
//
// A failure here wraps ErrQuestionNotFiled, exactly like AskQuestion: the
// caller must report "paused, and the question is missing" rather than "the
// pause failed".
func (r *mcpReleaser) AskProjectQuestion(ctx context.Context, body string, options []string, kind string) error {
	if body == "" {
		return errors.New("ask project question: body is required")
	}
	args := map[string]any{
		"body":     body,
		"blocking": false,
		"kind":     kind,
	}
	if len(options) > 0 {
		args["options"] = options
	}
	if err := r.call(ctx, "ask_question", args); err != nil {
		return fmt.Errorf("%w: %v", ErrQuestionNotFiled, err)
	}
	return nil
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

// call performs one MCP `tools/call` and reports whether it succeeded. Callers
// that also need the result's text payload (the task reads) use callText
// directly; this is the fire-and-forget form the writes use.
func (r *mcpReleaser) call(ctx context.Context, tool string, args map[string]any) error {
	_, err := r.callText(ctx, tool, args)
	return err
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

// Heartbeat renews a live claim's lease. It is the driver-side renewal (CLA-358):
// the loop calls it periodically while a session lives, so a >30-minute session
// does not have its lease expire and become a stale take-over offer.
func (r *mcpReleaser) Heartbeat(ctx context.Context, runID string) error {
	if runID == "" {
		return errors.New("heartbeat: runId is required")
	}
	return r.call(ctx, "heartbeat", map[string]any{
		"runId": runID,
	})
}
