// Package backlog is the driver's own cheap, read-only view of the clankerbar
// backlog — used to decide whether there is claimable work before spending tokens
// on a harness session, to keep polling (and logging) while idle so the loop reacts
// when questions are answered, items are promoted, or new work is filed, and to
// learn when the operator has paused the run from the web console.
//
// This is a control-plane read (no agent, no tokens): a single authenticated GET of
// clankerbar's project-scoped `/api/backlog-summary` route (CLA-76), which returns
// the same freshness snapshot the MCP `backlog` block carries — `{version, counts,
// claimable, openQuestions}` — PLUS a `loopPaused` boolean. Folding the pause flag
// into the same cheap read means the loop never needs a second call to learn it
// should stop spawning sessions. This route (unlike the MCP `get_backlog_summary`
// tool) needs a PROJECT-scoped API key: it carries no project slug in its path, so
// the key alone selects the project.
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
	Claimable     int  // dep-unblocked ready — the count that means "work to do now"
	InProgress    int
	OpenQuestions int
	Paused        bool // console-driven loop pause (CLA-76): stop spawning, keep polling
}

// Poller reads the backlog summary cheaply.
type Poller interface {
	Poll(ctx context.Context) (Summary, error)
}

// ErrNotWired means the driver cannot do its cheap project-scoped read, so the loop
// cannot gate on live counts (or honour the console pause) and falls back to blind
// draining. It is returned when creds are absent, and also when the configured key
// is an ACCOUNT key that the project-scoped `/api/backlog-summary` route rejects
// (see Poll) — either way there is no usable live read, so blind drain (which still
// makes progress) beats idle-polling a permanent failure. A wired poller reports
// transient failures as ordinary (retryable) errors, never ErrNotWired, so the loop
// never mistakes a blip for "no endpoint".
var ErrNotWired = errors.New("backlog polling not wired")

type notWired struct{}

func (notWired) Poll(context.Context) (Summary, error) { return Summary{}, ErrNotWired }

// httpPoller GETs clankerbar's project-scoped `/api/backlog-summary` route with a
// Bearer API key. No agent, no tokens — just the live counts the loop gates on and
// the console-pause flag it honours, in one read.
type httpPoller struct {
	endpoint string // full /api/backlog-summary URL, e.g. https://clankerbar.com/api/backlog-summary
	apiKey   string
	client   *http.Client
}

// New builds a Poller. With no endpoint / API key it returns a not-wired poller,
// which reports ErrNotWired (and so puts the loop into blind mode). Given both, it
// returns a real HTTP poller that fetches live {version, counts, claimable,
// openQuestions, loopPaused} from the plane.
//
// summaryURL is the full `/api/backlog-summary` URL (see config.BacklogSummaryURL):
// the project-scoped read surface CLA-76 built for this driver.
func New(summaryURL, apiKey string) Poller {
	if summaryURL == "" || apiKey == "" {
		return notWired{}
	}
	return &httpPoller{
		endpoint: strings.TrimRight(summaryURL, "/"),
		apiKey:   apiKey,
		client:   &http.Client{Timeout: 15 * time.Second},
	}
}

func (p *httpPoller) Poll(ctx context.Context) (Summary, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint, nil)
	if err != nil {
		return Summary{}, err
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Accept", "application/json")

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
		// An ACCOUNT key can't drive this project-scoped route: the route carries no
		// project slug (an account key selects its project via the /mcp/<slug> path),
		// so it answers 400 `project_required`. That is a persistent wiring mismatch,
		// not a transient blip — treat it like "not wired" so the loop drains blind
		// (still makes progress) instead of idle-polling a 400 forever. Console pause
		// and live count-gating require a project-scoped key.
		if resp.StatusCode == http.StatusBadRequest && errorCode(body) == "project_required" {
			return Summary{}, ErrNotWired
		}
		return Summary{}, fmt.Errorf("backlog summary: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return parseSummary(body)
}

// parseSummary decodes the `/api/backlog-summary` JSON body into a Summary. The
// route returns a plain JSON object (no MCP JSON-RPC / SSE envelope): {version,
// counts:{ready,in_progress,...}, claimable, openQuestions, loopPaused}.
func parseSummary(body []byte) (Summary, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return Summary{}, errors.New("backlog summary: empty response")
	}
	var payload struct {
		Version int `json:"version"`
		Counts  struct {
			Ready      int `json:"ready"`
			InProgress int `json:"in_progress"`
		} `json:"counts"`
		Claimable     int  `json:"claimable"`
		OpenQuestions int  `json:"openQuestions"`
		LoopPaused    bool `json:"loopPaused"`
	}
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return Summary{}, fmt.Errorf("decode backlog summary: %w", err)
	}
	return Summary{
		Version:       payload.Version,
		Ready:         payload.Counts.Ready,
		Claimable:     payload.Claimable,
		InProgress:    payload.Counts.InProgress,
		OpenQuestions: payload.OpenQuestions,
		Paused:        payload.LoopPaused,
	}, nil
}

// errorCode pulls the `error.code` string out of the plane's JSON error shape
// (`{"error":{"code","message"}}`), or "" if the body is not that shape.
func errorCode(body []byte) string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	return e.Error.Code
}
