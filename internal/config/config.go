// Package config holds the loop's runtime configuration: a file (JSON for now —
// TOML is the likely final format, matching Codex's own config) overlaid with
// explicit command-line flags.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// defaultBacklogURL is the base the driver reads backlog counts from when the
// operator sets no backlog_url. Kept as a named constant so BacklogSummaryURL can
// tell an explicit backlog_url apart from this default (explicit config overrides).
const defaultBacklogURL = "https://clankerbar.com"

// Config is the resolved loop configuration. The comments here are the source of
// truth for each knob until the README/docs catch up.
type Config struct {
	// Harness selects the coding-agent CLI to drive (e.g. "claude", "codex",
	// "opencode"). Validated against the harness registry (harness.Known), so the
	// accepted set is exactly what is registered — see Validate.
	Harness string `json:"harness"`

	// Model is a harness-specific model alias to pin (e.g. "opus"). Empty = the
	// harness default.
	Model string `json:"model"`

	// Prompt is what each fresh session is asked to do. Default: drain the
	// clankerbar backlog. This is the drain instruction, not a per-task prompt —
	// the backlog is the source of tasks.
	Prompt string `json:"prompt"`

	// WorkDir is where the harness runs (its repo checkout). Empty = current dir.
	WorkDir string `json:"workdir"`

	// ConfigDir pins the harness config dir (CLAUDE_CONFIG_DIR / CODEX_HOME) so a
	// headless session loads the same skills, plugins, and auth as the interactive
	// one. Empty inherits the ambient environment — declare it for an unattended
	// daemon / cron, whose bare environment would otherwise have none of them.
	ConfigDir string `json:"config_dir"`

	// MCPConfigPath points the harness at the clankerbar MCP server. Claude Code
	// takes it as --mcp-config (NOT auto-discovered in -p mode); Codex merges it
	// into config.toml [mcp_servers]. See the adapters.
	MCPConfigPath string `json:"mcp_config_path"`

	// MaxIterations stops the loop after N respawns. 0 = run until the backlog is
	// dry (a HALT marker / "no work" result) or the loop is stopped.
	MaxIterations int `json:"max_iterations"`

	// PollInterval is how often, while paused on a usage limit, the loop re-probes
	// to catch an early reset. 0 = a built-in default (see loop.supervisedWait).
	PollInterval Duration `json:"poll_interval"`

	// IdlePollInterval is how often, when the backlog has no claimable work, the
	// loop re-checks (and logs) instead of exiting — so it reacts to answered
	// questions, promotions, and newly filed work. 0 = a built-in default.
	IdlePollInterval Duration `json:"idle_poll_interval"`

	// BacklogURL is the clankerbar base URL the driver reads backlog counts from
	// (cheap, no tokens). The project-scoped API key comes from CLANKERBAR_API_KEY
	// in the environment, never the config file.
	BacklogURL string `json:"backlog_url"`

	// MaxRetries bounds consecutive transient-failure retries within one drain
	// before the loop gives up. 0 = never give up (keep retrying at the backoff
	// ceiling until the API recovers) — the right default for a persistent daemon.
	MaxRetries int `json:"max_retries"`

	// RetryCap ceilings the exponential backoff between transient retries
	// (30s → 60s → 120s → ..., capped here). 0 = a built-in default (300s).
	RetryCap Duration `json:"retry_cap"`

	// Budget is the circuit breaker / headroom knob. See Budget.
	Budget Budget `json:"budget"`

	// StateDir holds the loop's control markers (STOP/HALT) and per-iteration
	// logs. Empty = "<workdir>/.clankerbar-loop".
	StateDir string `json:"state_dir"`

	// SettingsPath points the harness at an extra settings file (Claude Code's
	// --settings) carrying the headless permission policy — the allow/deny rules
	// that gate what an unattended run may call, since there is no human to prompt
	// and no interactive auto-mode classifier. It MERGES with (does not replace)
	// the config-dir's own settings, and deny rules win — so this file's job is to
	// grant the few tools the run needs and to deny the exfil vectors the ambient
	// allowlist leaves open. Claude-specific; other harnesses ignore it. ~ expands.
	SettingsPath string `json:"settings_path"`

	// Projects declares the backlogs a single loop instance drives — one entry per
	// clankerbar project (CLA-142: one account key, many queues). Empty = the
	// original single-project mode, driven by the top-level fields, exactly as
	// before. When set, the driver polls each project's
	// `/api/projects/<slug>/backlog-summary` and spawns each drain session in that
	// project's workdir, round-robin over whichever queues have claimable work.
	// The account key in CLANKERBAR_API_KEY covers every project the operator is a
	// member of; per-project keys are never needed.
	Projects []Project `json:"projects"`

	// Env is extra environment for the spawned harness process, as KEY=VALUE
	// pairs. The child already inherits the loop's own environment, so this is for
	// the unattended case (cron / launchd / systemd) where there is no interactive
	// shell to export into — e.g. supplying CLAUDE_CODE_OAUTH_TOKEN when auth lives
	// in a shell alias rather than the config dir.
	//
	// A value of the form "@path" is replaced by the contents of that file
	// (trimmed; a leading ~ is expanded). Keep a secret in a 0600 file and point at
	// it here rather than inlining it — mirroring CLANKERBAR_API_KEY, which is read
	// from the environment, never this config file.
	Env map[string]string `json:"env"`

	source string   // path the config was loaded from, for diagnostics
	env    []string // resolved KEY=VALUE pairs (built in Validate)
}

// Project is one backlog a multi-project instance drives (CLA-142).
type Project struct {
	// Slug is the clankerbar project slug — the `<slug>` in `/mcp/<slug>` and in
	// `/api/projects/<slug>/backlog-summary`. Required, unique per entry.
	Slug string `json:"slug"`

	// WorkDir is where this project's drain sessions run. Its `.mcp.json` should
	// name `/mcp/<slug>` so the sessions reach the right project's tools. Empty =
	// the top-level workdir.
	WorkDir string `json:"workdir"`

	// MCPConfigPath overrides which .mcp.json this project's sessions are pointed
	// at. Empty = `<workdir>/.mcp.json` when that file exists, else the top-level
	// mcp_config_path.
	MCPConfigPath string `json:"mcp_config_path"`
}

// Budget is the "leave headroom / don't run away" circuit breaker. No harness
// exposes headless quota introspection (see the design memo), so this is a
// self-accounted, operator-tuned cap — simple and reliable over
// percentage-accurate, by design. Zero-valued fields are disabled.
type Budget struct {
	MaxTokens    int      `json:"max_tokens"`     // cumulative tokens across iterations
	MaxCostUSD   float64  `json:"max_cost_usd"`   // cumulative $ across iterations
	MaxWallClock Duration `json:"max_wall_clock"` // stop after this much elapsed
}

// Exceeded reports whether any enabled budget dimension has been reached.
func (b Budget) Exceeded(tokens int, costUSD float64, elapsed time.Duration) bool {
	return b.ExceededBy(tokens, costUSD, elapsed) != ""
}

// ExceededBy names the dimension that tripped, or "" if none has.
//
// Which dial stopped a run is the first thing an operator asks, and reporting
// all three figures side by side answers it wrongly: a run under a wall-clock
// ceiling alone still prints a token count and a dollar figure, which read as the
// cause. Naming the dimension — and the ceiling it crossed — is the difference
// between "why did it stop at $148" and "it ran for 10h23m against an 8h cap".
func (b Budget) ExceededBy(tokens int, costUSD float64, elapsed time.Duration) string {
	switch {
	case b.MaxTokens > 0 && tokens >= b.MaxTokens:
		return fmt.Sprintf("tokens %d ≥ %d", tokens, b.MaxTokens)
	case b.MaxCostUSD > 0 && costUSD >= b.MaxCostUSD:
		return fmt.Sprintf("cost $%.2f ≥ $%.2f", costUSD, b.MaxCostUSD)
	case b.MaxWallClock > 0 && elapsed >= b.MaxWallClock.Duration():
		return fmt.Sprintf("wall clock %s ≥ %s", elapsed.Round(time.Second), b.MaxWallClock.Duration())
	}
	return ""
}

// Deadline is when the wall-clock ceiling will be reached for a run that began at
// start, or the zero time if no wall-clock ceiling is set.
func (b Budget) Deadline(start time.Time) time.Time {
	if b.MaxWallClock <= 0 {
		return time.Time{}
	}
	return start.Add(b.MaxWallClock.Duration())
}

// Remaining is how much of the wall-clock ceiling is left after elapsed, and
// whether a ceiling is set at all.
//
// Callers must pass the SAME elapsed the breaker is given (ExceededBy), so a
// decision taken mid-drain cannot disagree with the breaker's own verdict
// between drains. Deriving a wall-clock deadline is what let them disagree:
// Deadline keeps start's monotonic reading while ExceededBy counts monotonic
// elapsed, and a suspended machine advances the one and freezes the other.
func (b Budget) Remaining(elapsed time.Duration) (time.Duration, bool) {
	if b.MaxWallClock <= 0 {
		return 0, false
	}
	return b.MaxWallClock.Duration() - elapsed, true
}

func defaults() *Config {
	return &Config{
		Harness:    "claude",
		Prompt:     "Work the backlog.",
		BacklogURL: defaultBacklogURL,
	}
}

// Load reads config from path (or a discovered default), layered over defaults.
// An explicit path that cannot be read is an error; a missing default file is not.
func Load(path string) (*Config, error) {
	cfg := defaults()
	p := path
	if p == "" {
		p = discover()
	}
	if p == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if path == "" && errors.Is(err, os.ErrNotExist) {
			return cfg, nil // discovered default absent — fine
		}
		return nil, err
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	cfg.source = p
	return cfg, nil
}

func discover() string {
	candidates := []string{"clankerbar.json"}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", "clankerbar", "config.json"))
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// Overrides carries explicit flag values; zero values are left untouched.
type Overrides struct {
	Harness          string
	Model            string
	WorkDir          string
	ConfigDir        string
	MaxIterations    int
	PollInterval     time.Duration
	IdlePollInterval time.Duration
}

// ApplyFlagOverrides layers non-zero flag values over the loaded config.
func (c *Config) ApplyFlagOverrides(o Overrides) {
	if o.Harness != "" {
		c.Harness = o.Harness
	}
	if o.Model != "" {
		c.Model = o.Model
	}
	if o.WorkDir != "" {
		c.WorkDir = o.WorkDir
	}
	if o.ConfigDir != "" {
		c.ConfigDir = o.ConfigDir
	}
	if o.MaxIterations != 0 {
		c.MaxIterations = o.MaxIterations
	}
	if o.PollInterval != 0 {
		c.PollInterval = Duration(o.PollInterval)
	}
	if o.IdlePollInterval != 0 {
		c.IdlePollInterval = Duration(o.IdlePollInterval)
	}
}

// expandHome expands a leading ~ (Go does not do this for us).
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// Validate normalizes path fields and checks the resolved config is runnable.
func (c *Config) Validate() error {
	c.ConfigDir = expandHome(c.ConfigDir)
	c.WorkDir = expandHome(c.WorkDir)
	c.MCPConfigPath = expandHome(c.MCPConfigPath)
	c.SettingsPath = expandHome(c.SettingsPath)
	c.StateDir = expandHome(c.StateDir)

	// Validate against the harness registry (not a hand-kept switch) so the accepted
	// set can never drift from what is actually registered — an unregistered value is
	// rejected here before harness.Get is consulted, and a newly registered adapter is
	// accepted automatically. harness does not import config, so there is no cycle.
	if !harness.Known(c.Harness) {
		return fmt.Errorf("unknown harness %q (want: %s)", c.Harness, strings.Join(harness.Names(), ", "))
	}
	if c.Prompt == "" {
		return errors.New("prompt is empty")
	}

	// Default mcp_config_path to <workdir>/.mcp.json when that file exists. Claude's
	// -p mode does NOT auto-discover .mcp.json, so without this a bare `clankerbar
	// run` from a workdir that carries one would spawn sessions with no clankerbar
	// tools at all — and the poller could derive no slug. Explicit config still wins.
	if c.MCPConfigPath == "" {
		c.MCPConfigPath = discoverMCPConfig(c.WorkDir)
	}

	// Multi-project entries: slug required and unique; paths normalized; each
	// project's mcp config defaults to its own workdir's .mcp.json (falling back to
	// the top-level one at invocation time — see loop.Target).
	seen := make(map[string]bool, len(c.Projects))
	for i := range c.Projects {
		p := &c.Projects[i]
		if p.Slug == "" {
			return fmt.Errorf("projects[%d]: slug is required", i)
		}
		if seen[p.Slug] {
			return fmt.Errorf("projects: duplicate slug %q", p.Slug)
		}
		seen[p.Slug] = true
		p.WorkDir = expandHome(p.WorkDir)
		p.MCPConfigPath = expandHome(p.MCPConfigPath)
		if p.MCPConfigPath == "" {
			p.MCPConfigPath = discoverMCPConfig(p.WorkDir)
		}
		// The slug decides which queue is POLLED; the .mcp.json decides which
		// project the sessions WORK. If they disagree, the loop would gate on one
		// project while draining another — a silent split-brain. Refuse it here.
		if fromMCP := slugFromMCPURL(mcpURLFromConfig(p.MCPConfigPath)); fromMCP != "" && fromMCP != p.Slug {
			return fmt.Errorf("projects[%d]: slug %q does not match its .mcp.json, which names /mcp/%s — the poll would gate on one project while sessions work another", i, p.Slug, fromMCP)
		}
	}

	resolved, err := resolveEnv(c.Env)
	if err != nil {
		return err
	}
	c.env = resolved
	return nil
}

// discoverMCPConfig returns <workdir>/.mcp.json if that file exists, else "".
func discoverMCPConfig(workdir string) string {
	base := workdir
	if base == "" {
		base = "."
	}
	p := filepath.Join(base, ".mcp.json")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// resolveEnv turns the env map into sorted KEY=VALUE pairs, reading "@path"
// values from disk so a secret needn't be inlined in the config file. Sorting
// keeps the child's environment deterministic across runs.
func resolveEnv(m map[string]string) ([]string, error) {
	if len(m) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		v := m[k]
		if strings.HasPrefix(v, "@") {
			path := expandHome(strings.TrimPrefix(v, "@"))
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("env %s: %w", k, err)
			}
			v = strings.TrimSpace(string(data))
		}
		out = append(out, k+"="+v)
	}
	return out, nil
}

// EnvSlice returns the resolved extra environment (KEY=VALUE) for the harness,
// populated by Validate. Nil when no env is configured.
func (c *Config) EnvSlice() []string { return c.env }

// ResolveStateDir returns where control markers and logs live.
func (c *Config) ResolveStateDir() string {
	if c.StateDir != "" {
		return c.StateDir
	}
	base := c.WorkDir
	if base == "" {
		base = "."
	}
	return filepath.Join(base, ".clankerbar-loop")
}

// Source is the file the config was loaded from ("" if none).
func (c *Config) Source() string { return c.source }

// BacklogEndpoint returns the full, project-scoped MCP URL the driver's own
// backlog poll should hit — the same `/mcp/<project>` endpoint the harness uses.
//
// BacklogURL defaults to a bare base (`https://clankerbar.com`), which the plane
// rejects without the project slug in the path. The exact URL already lives in the
// harness .mcp.json (MCPConfigPath), so we reuse it — keeping the cheap poll and
// the harness pointed at the same plane. An explicit backlog_url that already names
// a `/mcp/<project>` path wins; otherwise we derive from .mcp.json.
//
// Returns "" when no usable project-scoped endpoint can be resolved (a bare base
// and no .mcp.json url). That is deliberate: New("") yields a not-wired poller, so
// the loop falls into blind drain — which still makes progress — rather than
// retrying forever against a slug-less base the plane can only reject.
func (c *Config) BacklogEndpoint() string {
	if strings.Contains(c.BacklogURL, "/mcp/") {
		return c.BacklogURL
	}
	return mcpURLFromConfig(c.MCPConfigPath)
}

// ProjectEndpoint returns one configured project's MCP endpoint — the same
// `/mcp/<slug>` URL that project's sessions are pointed at. Resolved from the
// project's own .mcp.json, falling back to the config's, exactly as the harness
// invocation resolves its own, so a write the driver makes lands on the same
// project its sessions are working.
func (c *Config) ProjectEndpoint(p Project) string {
	path := p.MCPConfigPath
	if path == "" {
		path = c.MCPConfigPath
	}
	if u := mcpURLFromConfig(path); u != "" {
		return u
	}
	return c.BacklogEndpoint()
}

// BacklogSummaryURL returns the URL of the driver's cheap backlog read: the
// plane's `GET .../backlog-summary` surface, which returns {version, counts,
// claimable, openQuestions, loopPaused} in one authenticated call (counts to gate
// on, plus the console-driven pause the driver honours).
//
// Two forms exist (CLA-141), and which one we return decides which key kinds work:
//
//   - `/api/projects/<slug>/backlog-summary` — project named in the PATH, so the
//     operator's ACCOUNT key works (membership-gated, exactly like /mcp/<slug>);
//     a project key works too, for its own slug. Returned whenever a slug can be
//     derived from the resolved MCP endpoint (an .mcp.json naming /mcp/<slug>).
//   - `/api/backlog-summary` — the legacy slug-less route, where only a
//     project-scoped key can select the project. The fallback when no slug is
//     derivable, so pre-CLA-141 setups keep working unchanged.
//
// Origin precedence (explicit config overrides, per the README): an explicitly set
// backlog_url — one that differs from the default base — wins, so an operator who
// points backlog_url at a self-hosted plane is honoured even when .mcp.json names a
// different origin. Only when backlog_url is left at the default do we fall back to
// the resolved MCP endpoint's origin (so a self-hosted plane wired solely through
// .mcp.json is still honoured), and finally to the default base's own origin.
// Returns "" when no origin can be resolved (New("") then yields a not-wired,
// blind poller).
func (c *Config) BacklogSummaryURL() string {
	endpoint := c.BacklogEndpoint()
	origin := c.summaryOrigin(endpoint)
	if origin == "" {
		return ""
	}
	if slug := slugFromMCPURL(endpoint); slug != "" {
		return projectSummaryPath(origin, slug)
	}
	return origin + "/api/backlog-summary"
}

// ProjectSummaryURL returns the slug-ful summary URL for one configured project
// (CLA-142): `<origin>/api/projects/<slug>/backlog-summary`. The origin follows the
// same precedence as BacklogSummaryURL, using this project's own .mcp.json for the
// self-hosted-plane fallback; the default base guarantees a non-empty result.
func (c *Config) ProjectSummaryURL(p Project) string {
	origin := c.summaryOrigin(mcpURLFromConfig(p.MCPConfigPath))
	if origin == "" {
		return ""
	}
	return projectSummaryPath(origin, p.Slug)
}

// summaryOrigin resolves the plane origin for a summary read: explicit backlog_url
// wins, then the given MCP endpoint's origin, then the (default) backlog_url base.
func (c *Config) summaryOrigin(mcpEndpoint string) string {
	if c.BacklogURL != "" && c.BacklogURL != defaultBacklogURL {
		if origin := originOf(c.BacklogURL); origin != "" {
			return origin
		}
	}
	if origin := originOf(mcpEndpoint); origin != "" {
		return origin
	}
	return originOf(c.BacklogURL)
}

func projectSummaryPath(origin, slug string) string {
	return origin + "/api/projects/" + url.PathEscape(slug) + "/backlog-summary"
}

// slugFromMCPURL extracts the `<slug>` from an `/mcp/<slug>` MCP endpoint URL, or
// "" when the path is bare `/mcp` (a pre-CLA-99 project-key endpoint) or not an
// MCP path at all.
func slugFromMCPURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 2 && parts[0] == "mcp" && parts[1] != "" {
		return parts[1]
	}
	return ""
}

// originOf returns the scheme://host of a URL, or "" if it cannot be parsed or lacks
// a scheme/host.
func originOf(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// mcpURLFromConfig reads a Claude-shaped .mcp.json and returns the clankerbar MCP
// server URL (or the first http server's URL), or "" if the file is absent or has
// no usable url. Best-effort: any read/parse failure yields "".
func mcpURLFromConfig(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(expandHome(path))
	if err != nil {
		return ""
	}
	var f struct {
		MCPServers map[string]struct {
			URL string `json:"url"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return ""
	}
	if s, ok := f.MCPServers["clankerbar"]; ok && s.URL != "" {
		return s.URL
	}
	for _, s := range f.MCPServers {
		if s.URL != "" {
			return s.URL
		}
	}
	return ""
}
