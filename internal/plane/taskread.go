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
)

// The driver's small READ surface over the same MCP endpoint its writes use.
//
// It exists so the dispatcher knows a task's repo BEFORE it spawns a harness
// (agent-rule-scoping piece 3): a session's working directory is fixed at spawn
// time, but the task it will claim is only known to the plane — a fresh phase
// has not claimed anything yet, and a resumed phase knows only the task ID its
// predecessor's claim carried. So the driver asks: next_task for the queue head
// a fresh phase would claim, get_task for the exact task a resumed phase
// continues. Both responses carry the task's `repo` field, which is what the
// loop resolves to a checkout.
//
// Both are best-effort by contract: a read that fails degrades the spawn to the
// fallback chain (primary repo, then the legacy workdir), never to a failed
// run — the plane being briefly unreachable must not cost an iteration, when
// the session it would have spawned is itself about to need the plane anyway.

// NextTask is what the queue-head peek learned. Repo is the task's `repo`
// field, empty when the task carries none (or there is nothing queued).
type NextTask struct {
	TaskID string
	Repo   string
}

// TaskPeeker reads which task a FRESH phase would claim, so its repo can steer
// the spawn cwd. A zero NextTask with a nil error means "nothing claimable, or
// the head names no repo" — both resolve through the caller's fallback chain.
type TaskPeeker interface {
	PeekNextTask(ctx context.Context) (NextTask, error)
}

// TaskRepoSource reads one task's repo, for a RESUMED phase that already knows
// its task ID. Empty repo, nil error = the task carries none.
type TaskRepoSource interface {
	TaskRepo(ctx context.Context, taskID string) (string, error)
}

// PeekNextTask calls the plane's next_task — the same read a session makes to
// find work, which is a read and not a claim: it holds nothing, so the peek
// cannot race a session out of work. It may hand back a stale-lease takeover
// candidate; that is still the task the spawned session will be offered first,
// so its repo is the right cwd to start in.
//
// The arguments are an EMPTY OBJECT, not nil: the plane's next_task schema is
// `inputSchema: {}` and MCP servers reject a null `arguments` against an object
// schema (observed live as "Invalid input / expected: record / params.arguments"
// on every daemon, which sent the peek to the primary-repo fallback and
// silently defeated CLA-437's repo routing, CLA-448).
func (r *mcpReleaser) PeekNextTask(ctx context.Context) (NextTask, error) {
	raw, err := r.callText(ctx, "next_task", map[string]any{})
	if err != nil {
		return NextTask{}, err
	}
	var payload struct {
		Next *struct {
			ID   string `json:"id"`
			Repo any    `json:"repo"`
		} `json:"next"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return NextTask{}, fmt.Errorf("next_task: decode response: %w", err)
	}
	if payload.Next == nil || payload.Next.ID == "" {
		return NextTask{}, nil
	}
	return NextTask{TaskID: payload.Next.ID, Repo: repoString(payload.Next.Repo)}, nil
}

// TaskRepo calls get_task for one task and returns its `repo` field. The
// response is accepted in both envelopes the plane has used: wrapped
// ({"task": {...}}) and bare (the task object itself).
func (r *mcpReleaser) TaskRepo(ctx context.Context, taskID string) (string, error) {
	if taskID == "" {
		return "", errors.New("task repo: taskId is required")
	}
	raw, err := r.callText(ctx, "get_task", map[string]any{"taskId": taskID})
	if err != nil {
		return "", err
	}
	var payload struct {
		Task *struct {
			Repo any `json:"repo"`
		} `json:"task"`
		Repo any `json:"repo"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("get_task: decode response: %w", err)
	}
	if payload.Task != nil {
		return repoString(payload.Task.Repo), nil
	}
	return repoString(payload.Repo), nil
}

// repoString narrows the decoded `repo` field, which the plane sends as a
// string or null. A non-string is treated as absent rather than an error: the
// field's meaning is "which repo this task targets", and no value it can carry
// turns a spawn into a failure — that is resolution's job, on a real string.
func repoString(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// callText is call for callers who need the result's text payload, not just its
// success: the two task-read methods decode JSON out of the tool's content.
func (r *mcpReleaser) callText(ctx context.Context, tool string, args map[string]any) ([]byte, error) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d: %s", tool, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if err := checkResult(tool, raw); err != nil {
		return nil, err
	}
	return resultText(sseData(raw)), nil
}

// resultText concatenates a tools/call result's text contents — where the
// plane's task reads carry their JSON payload. Empty when the shape is not what
// the writers here expect; the JSON decode upstream reports that as its own
// error, so this stays silent.
func resultText(payload []byte) []byte {
	var rpc struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(payload, &rpc); err != nil {
		return nil
	}
	var b strings.Builder
	for _, c := range rpc.Result.Content {
		b.WriteString(c.Text)
	}
	return []byte(b.String())
}
