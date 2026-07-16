// Package backlog is the driver's own cheap, read-only view of the clankerbar
// backlog — used to decide whether there is claimable work before spending tokens
// on a harness session, and to keep polling (and logging) while idle so the loop
// reacts when questions are answered, items are promoted, or new work is filed.
//
// This is a control-plane read (no agent, no tokens), mirroring the freshness
// `backlog` block / get_backlog_summary that clankerbar already exposes.
package backlog

import (
	"context"
	"errors"
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
// live counts and falls back to blind draining.
var ErrNotWired = errors.New("backlog polling not wired")

type notWired struct{}

func (notWired) Poll(context.Context) (Summary, error) { return Summary{}, ErrNotWired }

// New builds a Poller. With no base URL / API key it returns a not-wired poller.
//
// TODO: real HTTP client. The driver holds the project-scoped CLANKERBAR_API_KEY,
// so a single authenticated GET against a lightweight summary endpoint (or the MCP
// get_backlog_summary) returns {version, counts}. Endpoint shape is the open
// decision in docs/proposals/looping.md ("Idle, don't exit").
func New(baseURL, apiKey string) Poller {
	if baseURL == "" || apiKey == "" {
		return notWired{}
	}
	return notWired{} // TODO: return httpPoller{baseURL, apiKey}
}
