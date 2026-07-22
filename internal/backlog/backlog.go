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
// draining. It is returned only for the genuinely benign gap: creds are absent, or
// no summary endpoint is configured, so there is nothing to poll — blind drain (which
// still makes progress) beats idle-polling a permanent no-op. A wired poller reports
// transient failures as ordinary (retryable) errors, never ErrNotWired, so the loop
// never mistakes a blip for "no endpoint".
//
// Two failures are deliberately NOT ErrNotWired, because both are operator
// misconfigurations the harness sessions share and cannot self-heal — blind-draining
// either would just burn doomed sessions. A 401/403 auth rejection maps to
// ErrUnauthorized; a `400 project_required` (an account-scoped key on a project-scoped
// route) maps to ErrProjectRequired. Both are loud hard stops (see loop.Run).
var ErrNotWired = errors.New("backlog polling not wired")

// ErrUnauthorized means the plane rejected the driver's API key with 401/403 — a
// revoked, wrong, or malformed CLANKERBAR_API_KEY. Unlike ErrNotWired this is NOT a
// cue to blind-drain: the harness sessions the loop would spawn use the SAME key, so
// draining blind just burns sessions that also can't reach the plane. An auth failure
// is a persistent operator misconfiguration, not a transient blip that self-heals, so
// the loop treats it as a loud hard stop (see loop.Run) — exit non-zero and name the
// key — rather than idle-polling or blind-draining a dead credential (CLA-132).
var ErrUnauthorized = errors.New("backlog auth rejected (401/403)")

// ErrProjectRequired means the plane rejected the read with `400 project_required`:
// the configured CLANKERBAR_API_KEY is ACCOUNT-scoped, but the project-scoped
// `/api/backlog-summary` route (no project slug in its path) needs a PROJECT-scoped
// key. Like ErrUnauthorized, and unlike ErrNotWired, this is NOT a blind-drain cue:
// the harness sessions the loop spawns carry the SAME account key, so they can't do
// project-scoped MCP work (`next_task`/`claim_task`/…) either — blind-draining just
// burns doomed sessions against a wiring mismatch that won't self-heal. The loop
// hard-stops loudly (see loop.Run) and tells the operator to set a project-scoped key
// (CLA-133, which reverses CLA-130's ErrNotWired mapping).
var ErrProjectRequired = errors.New("backlog needs a project-scoped API key (400 project_required)")

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
		// not a transient blip — and NOT a cue to blind-drain: the harness sessions the
		// loop spawns carry the SAME account key, so they can't do project-scoped MCP
		// work either. Map it to ErrProjectRequired (distinct from both an auth failure
		// and a transient blip) so the loop hard-stops loudly and the operator switches
		// to a project-scoped key (CLA-133, reversing CLA-130's blind-drain mapping).
		if resp.StatusCode == http.StatusBadRequest && errorCode(body) == "project_required" {
			return Summary{}, ErrProjectRequired
		}
		// A revoked/wrong API key answers 401/403. That is a PERMANENT auth failure,
		// not a transient blip — and NOT a cue to blind-drain: the harness sessions the
		// loop spawns carry the SAME key, so blind draining just burns sessions that
		// also can't reach the plane. Map it to ErrUnauthorized (distinct from both the
		// project_required wiring mismatch and a transient blip) so the loop hard-stops
		// loudly and the operator fixes the key (CLA-132).
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return Summary{}, ErrUnauthorized
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
