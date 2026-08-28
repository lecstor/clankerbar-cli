package supervisor

// The account-scoped roster (phase 3a of docs/proposals/daemon-supervisor.md in
// the clankerbar repo, merged as the plane's GET /api/daemon-roster): the
// declared instances and their desired state, polled by the supervisor and
// reconciled on every poll. The plane refuses bad entries at write time — a
// forbidden key, a remote placement, an unknown harness — and this file carries
// the supervisor's own copies of those gates, because the supervisor must never
// trust the plane to have enforced its own rules. The one-directional merge is
// an ALLOWLIST here, not a filter: a roster entry carrying a key that may not
// reach the machine layer or the run config is refused loudly and named in the
// log, never silently dropped — a silent drop is how a daemon ends up looking
// configured and not being.
//
// The client is deliberately the cheap periodic GET the proposal chose over a
// held socket: one HTTP request per poll, no connection state, nothing a
// reconnect can lose (a missed poll is caught by the next one, which is the
// whole "desired state, not commands" argument).
//
// The cache is the offline half: every successful poll writes the entries to
// disk (last-known-good roster), and a cold start that cannot reach the plane
// reconciles from the cache instead of starting nothing. The per-instance
// materialized configs (phase 2b) sit beside it — see rosterStateDir.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/secureurl"
)

// RosterProject is one project a roster entry drives, as the plane serves it.
// primaryRepo is what the phase-2a workdir derivation derives from when a
// machine root is stated; it may be empty on the wire (a project with no repo),
// which then refuses only a derivation that needs it.
type RosterProject struct {
	Slug        string `json:"slug"`
	PrimaryRepo string `json:"primaryRepo"`
}

// RosterEntry is one declared instance as the account-scoped poll serves it.
type RosterEntry struct {
	Name         string            `json:"name"`
	DesiredState string            `json:"desiredState"`
	Placement    string            `json:"placement"`
	Overrides    map[string]json.RawMessage `json:"overrides"`
	Projects     []RosterProject   `json:"projects"`
}

// RosterDesiredRunning / RosterDesiredStopped are the two desired states the
// plane serves. Anything else is refused loudly — the supervisor reconciles
// against named state, never a default.
const (
	RosterDesiredRunning = "running"
	RosterDesiredStopped = "stopped"
)

// RosterPlacementLocal / RosterPlacementRemote are the two placements an entry
// may carry. Remote is not implemented (Decision 7) and the plane refuses it at
// write time; the supervisor still must not fail on one, so it ignores it —
// with a log line, not an error.
const (
	RosterPlacementLocal  = "local"
	RosterPlacementRemote = "remote"
)

// rosterMachineLayerKeys are the keys that can never come from the plane
// (proposal, "what can never come from the cloud"): they name paths, secrets
// and the permission-policy trust boundary on THIS machine, and a plane that
// could set them could hand a daemon a permissive policy or a wrong workdir.
// Mirrors the plane's own ROSTER_MACHINE_LAYER_KEYS so the two gates agree.
var rosterMachineLayerKeys = map[string]bool{
	"env":             true,
	"settings_path":   true,
	"workdir":         true,
	"mcp_config_path": true,
}

// rosterRunConfigKeys are the run-config document's keys: project policy,
// never per-instance (Decision 1 — only `harness` and its per-harness block
// may be overridden on a roster entry). Mirrors the plane's
// ROSTER_RUN_CONFIG_KEYS.
var rosterRunConfigKeys = map[string]bool{
	"$schema_version":        true,
	"model":                  true,
	"models":                 true,
	"prompt":                 true,
	"phases":                 true,
	"max_turns":              true,
	"max_session_wall_clock": true,
	"backlog_url":            true,
	"budget":                 true,
	"escalation":             true,
	"transitions":            true,
	"allow_local_mcp_servers": true,
	"notes":                  true,
}

// checkEntry is the supervisor's own copy of the plane's write-time gate: an
// entry must carry a known desired state and placement, at least one project,
// and an overrides document holding nothing but `harness` and `harnesses`. A
// refusal NAMES the entry and the offending key, with the class that makes the
// key forbidden — the log line is the supervisor refusing loudly rather than
// silently dropping a key it does not apply.
func checkEntry(e *RosterEntry) error {
	if e.Name == "" {
		return errors.New("entry carries no name")
	}
	if e.DesiredState != RosterDesiredRunning && e.DesiredState != RosterDesiredStopped {
		return fmt.Errorf("entry %q: unknown desired state %q (want running or stopped)", e.Name, e.DesiredState)
	}
	if e.Placement != RosterPlacementLocal && e.Placement != RosterPlacementRemote {
		return fmt.Errorf("entry %q: unknown placement %q (want local or remote)", e.Name, e.Placement)
	}
	if len(e.Projects) == 0 {
		return fmt.Errorf("entry %q: names no project - a declared instance drives at least one", e.Name)
	}
	for _, p := range e.Projects {
		if strings.TrimSpace(p.Slug) == "" {
			return fmt.Errorf("entry %q: names a project with no slug", e.Name)
		}
	}
	for key := range e.Overrides {
		if rosterMachineLayerKeys[key] {
			return fmt.Errorf("entry %q: carries machine-layer key %q - env, settings_path, workdir and mcp_config_path can never come from the plane; they stay in the supervisor's local config", e.Name, key)
		}
		if rosterRunConfigKeys[key] {
			return fmt.Errorf("entry %q: carries run-config key %q - that is project policy, never per-instance; only harness and its per-harness block may be overridden on a roster entry", e.Name, key)
		}
		if key != "harness" && key != "harnesses" {
			return fmt.Errorf("entry %q: carries unknown key %q - only harness and its per-harness block are permitted per-instance overrides", e.Name, key)
		}
	}
	return nil
}

// RosterClient fetches the account-scoped roster. It is the supervisor's only
// plane surface; the daemons' own channels are untouched (Decision 2).
type RosterClient struct {
	endpoint string
	apiKey   string
	client   *http.Client
}

// NewRosterClient builds the poll client. A missing endpoint or key is a
// not-wired client whose Fetch answers ErrNotWired — the caller (the cli
// layer) already refuses to run a supervisor without the account key, so this
// is the same degrade-don't-panic shape every other plane client uses.
func NewRosterClient(endpoint, apiKey string) *RosterClient {
	if endpoint == "" || apiKey == "" {
		return &RosterClient{} // Fetch returns ErrNotWired
	}
	return &RosterClient{
		endpoint: strings.TrimRight(endpoint, "/"),
		apiKey:   apiKey,
		client:   &http.Client{Timeout: 15 * time.Second, CheckRedirect: noDowngradeRedirect},
	}
}

// ErrNotWired means no endpoint or API key was configured. The supervisor
// treats it as a hard start error, not a degraded mode: a supervisor without
// the account key cannot see the roster at all, and running children on no
// desired state is exactly the un-steered fleet this phase exists to end.
var ErrNotWired = errors.New("roster poll not wired (no endpoint or account key)")

// Fetch performs one poll of the account-scoped roster. A transport failure, a
// non-200 answer, or an undecodable body is an error — the caller decides what
// to reconcile against (the last-known-good roster), because an empty answer
// must never read as "nothing declared".
func (c *RosterClient) Fetch(ctx context.Context) ([]RosterEntry, error) {
	if c == nil || c.endpoint == "" || c.apiKey == "" {
		return nil, ErrNotWired
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("roster: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("roster: GET %s: %w", c.endpoint, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("roster: GET %s: %w", c.endpoint, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("roster: GET %s: HTTP %d: %s", c.endpoint, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Entries []RosterEntry `json:"entries"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("roster: decode %s: %w", c.endpoint, err)
	}
	return payload.Entries, nil
}

// noDowngradeRedirect refuses a redirect that would put the bearer token on the
// wire in cleartext — the same rule every other authenticated client here
// follows (CLA-257): Go strips Authorization on a host change but forwards it
// on an https -> http hop to the SAME host.
func noDowngradeRedirect(req *http.Request, _ []*http.Request) error {
	if _, err := secureurl.Origin(req.URL.String()); err != nil {
		return fmt.Errorf("refusing redirect: %w", err)
	}
	return nil
}

// rosterCacheName is the last-known-good roster file the supervisor writes
// after every successful poll and reconciles from when the plane is
// unreachable at start. It holds the raw entries as served — desired state
// included — so an offline start honours "stopped" exactly as a warm one does.
const rosterCacheName = "roster.json"

// rosterCacheDir is where the supervisor's own state lives: the cached roster
// and one per-instance state dir (see rosterStateDir). It sits under the loop
// state root beside the daemons' own per-workdir state dirs.
func rosterCacheDir() (string, error) {
	root, err := config.StateRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "roster"), nil
}

// rosterStateDir is one instance's state dir, keyed by the entry name so it is
// stable across reconciles and restarts (the materialized config cache has to
// survive both) and unambiguous between instances (two entries never share a
// workdir-derived hash). The name is sanitised for the filesystem and hashed so
// two names that sanitise alike — `a/b` and `a b` — still land apart.
func rosterStateDir(dir, name string) string {
	sum := sha256.Sum256([]byte(name))
	return filepath.Join(dir, sanitizeSlug(name)+"-"+hex.EncodeToString(sum[:8]))
}

// sanitizeSlug reduces an instance name to something plainly safe as one path
// component: no separators, no dot-prefix, no surprises from a name the plane
// declared. Mirrors config's own stateSlug sanitizer (the two cannot share it
// without exporting, and the rules are small enough to keep in lockstep).
func sanitizeSlug(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" || out == "." || out == ".." {
		out = "instance"
	}
	if len(out) > 60 {
		out = out[:60]
	}
	return out
}

// writeCachedRoster persists the last-known-good roster. A failure is logged,
// never fatal: the cache is a convenience for an offline start, and a poll that
// reached the plane has already done the real work in memory.
func writeCachedRoster(dir string, entries []RosterEntry) {
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("roster: cannot create the cache dir %s (%v) - offline starts will have nothing to reconcile from", dir, err)
		return
	}
	data, err := json.Marshal(entries)
	if err != nil {
		log.Printf("roster: cannot encode the cached roster (%v)", err)
		return
	}
	path := filepath.Join(dir, rosterCacheName)
	// Remove-then-create through O_EXCL, like the materialized-config write: a
	// symlink planted at the name is removed, never followed.
	if _, err := os.Lstat(path); err == nil {
		if err := os.Remove(path); err != nil {
			log.Printf("roster: cannot replace %s (%v)", path, err)
			return
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		log.Printf("roster: cannot write %s (%v)", path, err)
		return
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		log.Printf("roster: cannot write %s (%v)", path, err)
		return
	}
	if err := f.Close(); err != nil {
		log.Printf("roster: cannot write %s (%v)", path, err)
	}
}

// loadCachedRoster reads the last-known-good roster, or returns nil when there
// is nothing usable: an absent file, or one that fails to decode — a corrupt
// cache must not wedge a cold start, and the next successful poll regenerates
// it.
func loadCachedRoster(dir string) []RosterEntry {
	if dir == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, rosterCacheName))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("roster: cannot read the cached roster (%v)", err)
		}
		return nil
	}
	var entries []RosterEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		log.Printf("roster: cached roster is corrupt (%v) - a successful poll will regenerate it", err)
		return nil
	}
	return entries
}