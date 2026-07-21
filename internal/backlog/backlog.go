// Package backlog is the driver's own cheap, read-only view of the clankerbar
// backlog — used to decide whether there is claimable work before spending tokens
// on a harness session, and to keep polling (and logging) while idle so the loop
// reacts when questions are answered, items are promoted, or new work is filed.
//
// This is a control-plane read (no agent, no tokens): a single authenticated
// call to clankerbar's `get_backlog_summary` MCP tool, mirroring the freshness
// `backlog` block that clankerbar already exposes.
package backlog

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
)

// Summary is a point-in-time view of the queue.
type Summary struct {
	Version       int // per-project monotonic counter; bumps on every write
	Ready         int
	Claimable     int // dep-unblocked ready — the count that means "work to do now"
	InProgress    int
	OpenQuestions int
}

// Poller reads the backlog summary cheaply.
type Poller interface {
	Poll(ctx context.Context) (Summary, error)
}

// ErrNotWired means no backlog endpoint is configured, so the loop cannot gate on
// live counts and falls back to blind draining. It is returned ONLY when creds are
// absent — a wired poller reports transient failures as ordinary (retryable)
// errors, never ErrNotWired, so the loop never mistakes a blip for "no endpoint".
var ErrNotWired = errors.New("backlog polling not wired")

type notWired struct{}

func (notWired) Poll(context.Context) (Summary, error) { return Summary{}, ErrNotWired }

// httpPoller calls clankerbar's `get_backlog_summary` MCP tool over HTTP: a single
// JSON-RPC `tools/call` with a Bearer API key. No agent, no tokens — just the live
// counts the loop gates on.
type httpPoller struct {
	endpoint string // full project-scoped MCP endpoint, e.g. https://clankerbar.com/mcp/<project>
	apiKey   string
	client   *http.Client
}

// New builds a Poller. With no endpoint / API key it returns a not-wired poller,
// which is the ONLY thing that reports ErrNotWired (and so the only thing that puts
// the loop into blind mode). Given both, it returns a real HTTP poller that fetches
// live {version, counts, claimable, openQuestions} from the plane.
//
// baseURL is expected to be the full project-scoped MCP endpoint (the same
// `/mcp/<project>` URL the harness uses, resolved from the operator's .mcp.json).
func New(baseURL, apiKey string) Poller {
	if baseURL == "" || apiKey == "" {
		return notWired{}
	}
	return &httpPoller{
		endpoint: strings.TrimRight(baseURL, "/"),
		apiKey:   apiKey,
		client:   &http.Client{Timeout: 15 * time.Second},
	}
}

// summaryRequest is the JSON-RPC call for the read-only summary tool. Stateless:
// the plane answers a bare tools/call without an initialize handshake or session.
const summaryRequest = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_backlog_summary","arguments":{}}}`

func (p *httpPoller) Poll(ctx context.Context) (Summary, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, strings.NewReader(summaryRequest))
	if err != nil {
		return Summary{}, err
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")
	// The MCP HTTP transport may answer either as JSON or as an SSE frame; accept both.
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := p.client.Do(req)
	if err != nil {
		return Summary{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Summary{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Summary{}, fmt.Errorf("backlog summary: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return parseSummary(body)
}

// parseSummary decodes a get_backlog_summary response body into a Summary. It
// accepts both a plain JSON-RPC body and an SSE frame whose `data:` line carries
// the envelope, then unwraps the tool's text content (itself a JSON document).
func parseSummary(body []byte) (Summary, error) {
	env, err := extractJSONRPC(body)
	if err != nil {
		return Summary{}, err
	}

	var rpc struct {
		Result *struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(env, &rpc); err != nil {
		return Summary{}, fmt.Errorf("decode backlog summary envelope: %w", err)
	}
	if rpc.Error != nil {
		return Summary{}, fmt.Errorf("backlog summary rpc error %d: %s", rpc.Error.Code, rpc.Error.Message)
	}
	if rpc.Result == nil || len(rpc.Result.Content) == 0 || rpc.Result.Content[0].Text == "" {
		return Summary{}, errors.New("backlog summary: empty tool result")
	}
	if rpc.Result.IsError {
		return Summary{}, fmt.Errorf("backlog summary tool error: %s", rpc.Result.Content[0].Text)
	}

	var payload struct {
		Version int `json:"version"`
		Counts  struct {
			Ready      int `json:"ready"`
			InProgress int `json:"in_progress"`
		} `json:"counts"`
		Claimable     int `json:"claimable"`
		OpenQuestions int `json:"openQuestions"`
	}
	if err := json.Unmarshal([]byte(rpc.Result.Content[0].Text), &payload); err != nil {
		return Summary{}, fmt.Errorf("decode backlog summary payload: %w", err)
	}
	return Summary{
		Version:       payload.Version,
		Ready:         payload.Counts.Ready,
		Claimable:     payload.Claimable,
		InProgress:    payload.Counts.InProgress,
		OpenQuestions: payload.OpenQuestions,
	}, nil
}

// extractJSONRPC pulls the JSON-RPC envelope out of an MCP HTTP response, which is
// either a plain JSON object or a text/event-stream frame carrying the envelope on
// a `data:` line. The last `data:` line wins (an SSE stream may precede the payload
// with other events).
func extractJSONRPC(body []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, errors.New("backlog summary: empty response")
	}
	if trimmed[0] == '{' {
		return trimmed, nil
	}
	var data []byte
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("data:")) {
			if d := bytes.TrimSpace(line[len("data:"):]); len(d) > 0 {
				data = d
			}
		}
	}
	if len(data) == 0 {
		return nil, errors.New("backlog summary: no JSON-RPC data in response")
	}
	return data, nil
}
