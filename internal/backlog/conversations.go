package backlog

import (
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

// The planner loop's 2-second wake poll (studio-chat.md §4).
//
// This is deliberately NOT an MCP tool call: a claim every 2 s would be a MODEL
// round trip over its whole accumulated context, the failure token-budget §5
// prices. The plane serves one entry — the oldest CLAIMABLE conversation, no
// bodies — and the loop only wakes a model once it has something to claim. A
// poll that finds nothing spawns no process and spends nothing.
//
// It shares the backlog poller's auth and error vocabulary on purpose: both are
// the driver's own project-scoped reads with the same key, and a planner must
// react to a revoked key exactly as a worker does.

// PendingConversation is the one entry the poll returns.
type PendingConversation struct {
	ID            string
	PendingCount  int
	LastMessageAt time.Time
}

// ConversationPoller reads the pending-conversation wake signal.
//
// A nil PendingConversation with a nil error means the plane answered
// `{pending: null}`: nothing is claimable right now. That is the ordinary idle
// answer, not a failure, and it covers both "no pending conversation" and "this
// project's resolved config has no planner".
type ConversationPoller interface {
	PollPending(ctx context.Context) (*PendingConversation, error)
}

type conversationNotWired struct{}

func (conversationNotWired) PollPending(context.Context) (*PendingConversation, error) {
	return nil, ErrNotWired
}

// NewConversationPoller builds the planner's pending poll. With no endpoint or
// API key it returns the not-wired poller, which reports ErrNotWired and lets
// the planner idle rather than poll a URL it cannot derive.
//
// endpoint is the full pending-route URL — config.ConversationPendingURL for
// the single-project target, config.ProjectConversationPendingURL per project.
func NewConversationPoller(endpoint, apiKey string) ConversationPoller {
	if endpoint == "" || apiKey == "" {
		return conversationNotWired{}
	}
	return &httpConversationPoller{
		endpoint: strings.TrimRight(endpoint, "/"),
		apiKey:   apiKey,
		client:   &http.Client{Timeout: 15 * time.Second, CheckRedirect: secureurl.NoDowngradeRedirect},
	}
}

type httpConversationPoller struct {
	endpoint string
	apiKey   string
	client   *http.Client
}

func (p *httpConversationPoller) PollPending(ctx context.Context) (*PendingConversation, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("conversation pending: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Same two permanent failures as the summary poll: a rejected key and a
		// project mismatch must stop the planner loudly, not be retried as blips.
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, ErrUnauthorized
		}
		if resp.StatusCode == http.StatusBadRequest && errorCode(body) == "project_required" {
			return nil, ErrProjectRequired
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, ErrConversationPollRateLimited
		}
		return nil, fmt.Errorf("conversation pending: GET %s: HTTP %d: %s", p.endpoint, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Pending *struct {
			ID            string `json:"id"`
			PendingCount  int    `json:"pendingCount"`
			LastMessageAt string `json:"lastMessageAt"`
		} `json:"pending"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("conversation pending: decode: %w", err)
	}
	if payload.Pending == nil || payload.Pending.ID == "" {
		return nil, nil
	}
	out := &PendingConversation{ID: payload.Pending.ID, PendingCount: payload.Pending.PendingCount}
	if at, err := time.Parse(time.RFC3339, payload.Pending.LastMessageAt); err == nil {
		out.LastMessageAt = at
	}
	return out, nil
}

// ErrConversationPollRateLimited means the plane refused the pending poll with
// 429. It exists so the planner can slow down quietly instead of logging a
// failed request every 2 seconds: the next tick is the retry, and one rate limit
// is not a broken loop.
var ErrConversationPollRateLimited = errors.New("conversation poll rate limited (429)")
