// Package backlog is the driver's own cheap, read-only view of the clankerbar
// backlog — used to decide whether there is claimable work before spending tokens
// on a harness session, to keep polling (and logging) while idle so the loop reacts
// when questions are answered, items are promoted, or new work is filed, and to
// learn when the operator has paused the run from the web console.
//
// This is a control-plane read (no agent, no tokens): a single authenticated GET of
// clankerbar's backlog-summary surface, which returns the same freshness snapshot
// the MCP `backlog` block carries — `{version, counts, claimable, openQuestions}` —
// PLUS a `loopPaused` boolean. Folding the pause flag into the same cheap read means
// the loop never needs a second call to learn it should stop spawning sessions.
//
// Two route forms exist (CLA-141); config.BacklogSummaryURL / ProjectSummaryURL
// decide which this poller is given:
//
//   - `/api/projects/<slug>/backlog-summary` — the project named in the PATH, so the
//     operator's ACCOUNT key works (membership-gated, like /mcp/<slug>). The form a
//     multi-project instance polls per project (CLA-142).
//   - `/api/backlog-summary` — legacy, slug-less: only a PROJECT-scoped key can
//     select the project, and an account key gets `400 project_required`.
package backlog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
// the poll hit the LEGACY slug-less `/api/backlog-summary` route with an
// ACCOUNT-scoped key, which that route cannot bind to a project. The remedy is a
// project SELECTOR, not a different key (decision 2026-07-29): give the loop a slug
// to name in the path — a `projects` list in the config, or an .mcp.json whose
// clankerbar URL is `/mcp/<slug>` — so it polls the slug-ful route (CLA-141/142). A
// project-scoped key also avoids it, but that is the CI-style setup, not the fix.
//
// Like ErrUnauthorized, and unlike ErrNotWired, this is NOT a blind-drain cue: the
// mismatch won't self-heal, and the sessions the loop would spawn burn tokens for
// nothing while the loop can't gate or see the console pause. The loop hard-stops
// loudly (see loop.Run) with the remedy above (CLA-133 made it a hard stop; CLA-142
// reworded the remedy).
var ErrProjectRequired = errors.New("backlog summary needs a project selector (400 project_required)")

type notWired struct{}

func (notWired) Poll(context.Context) (Summary, error) { return Summary{}, ErrNotWired }

// httpPoller GETs clankerbar's backlog-summary route (either form) with a Bearer
// API key. No agent, no tokens — just the live counts the loop gates on and the
// console-pause flag it honours, in one read.
type httpPoller struct {
	endpoint string // full summary URL, e.g. https://clankerbar.com/api/projects/<slug>/backlog-summary
	// legacy is the slug-less fallback for a plane that predates the slug-ful route
	// (CLA-141): a 404 on the slug-ful endpoint permanently switches to it, so an
	// upgraded CLI against an old plane degrades to the previous behaviour (works
	// with a project key; an account key then gets the loud project_required stop)
	// instead of idle-polling a 404 forever. "" when the endpoint is already legacy.
	legacy string
	apiKey string
	client *http.Client
}

// New builds a Poller. With no endpoint / API key it returns a not-wired poller,
// which reports ErrNotWired (and so puts the loop into blind mode). Given both, it
// returns a real HTTP poller that fetches live {version, counts, claimable,
// openQuestions, loopPaused} from the plane.
//
// summaryURL is the full summary-route URL — config.BacklogSummaryURL for the
// single default target, config.ProjectSummaryURL for each multi-project entry.
func New(summaryURL, apiKey string) Poller {
	if summaryURL == "" || apiKey == "" {
		return notWired{}
	}
	endpoint := strings.TrimRight(summaryURL, "/")
	return &httpPoller{
		endpoint: endpoint,
		legacy:   legacyFallbackURL(endpoint),
		apiKey:   apiKey,
		client:   &http.Client{Timeout: 15 * time.Second},
	}
}

// legacyFallbackURL returns the slug-less legacy summary URL for a slug-ful one
// (`/api/projects/<slug>/backlog-summary` → `/api/backlog-summary`, same origin),
// or "" when the URL is not the slug-ful form.
func legacyFallbackURL(summaryURL string) string {
	u, err := url.Parse(summaryURL)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "projects" && parts[3] == "backlog-summary" {
		return u.Scheme + "://" + u.Host + "/api/backlog-summary"
	}
	return ""
}

func (p *httpPoller) Poll(ctx context.Context) (Summary, error) {
	resp, body, err := p.get(ctx, p.endpoint)
	if err != nil {
		return Summary{}, err
	}
	if resp.StatusCode == http.StatusNotFound && p.legacy != "" {
		// The slug-ful route 404s — a plane that predates CLA-141. Fall back to the
		// legacy slug-less route PERMANENTLY (this poller), restoring pre-upgrade
		// behaviour: a project key keeps working; an account key gets the loud
		// project_required hard stop and its remedy — never an idle-forever 404 loop.
		p.endpoint, p.legacy = p.legacy, ""
		resp, body, err = p.get(ctx, p.endpoint)
		if err != nil {
			return Summary{}, err
		}
	}
	if resp.StatusCode != http.StatusOK {
		// An ACCOUNT key on the legacy slug-less route answers 400 `project_required`
		// — the route has no path slug to bind the key's project. A persistent wiring
		// mismatch, not a transient blip, and NOT a cue to blind-drain. Map it to
		// ErrProjectRequired (distinct from both an auth failure and a transient blip)
		// so the loop hard-stops loudly with the project-selector remedy (see the var).
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
		// Name the endpoint: a repeated non-OK here (e.g. a 404 with no fallback
		// left) idle-retries in the loop, and the URL is what makes that diagnosable.
		return Summary{}, fmt.Errorf("backlog summary: GET %s: HTTP %d: %s", p.endpoint, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return parseSummary(body)
}

// get performs one authenticated GET of a summary URL, returning the response
// (body already read and closed) so Poll can decide on fallback by status.
func (p *httpPoller) get(ctx context.Context, endpoint string) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, err
	}
	return resp, body, nil
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
