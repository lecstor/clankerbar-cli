// Package config holds the loop's runtime configuration: a file (JSON for now —
// TOML is the likely final format, matching Codex's own config) overlaid with
// explicit command-line flags.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config is the resolved loop configuration. The comments here are the source of
// truth for each knob until the README/docs catch up.
type Config struct {
	// Harness selects the coding-agent CLI to drive: "claude" or "codex".
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

	source string // path the config was loaded from, for diagnostics
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
	switch {
	case b.MaxTokens > 0 && tokens >= b.MaxTokens:
		return true
	case b.MaxCostUSD > 0 && costUSD >= b.MaxCostUSD:
		return true
	case b.MaxWallClock > 0 && elapsed >= b.MaxWallClock.Duration():
		return true
	}
	return false
}

func defaults() *Config {
	return &Config{
		Harness:    "claude",
		Prompt:     "Work the backlog.",
		BacklogURL: "https://clankerbar.com",
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
	c.StateDir = expandHome(c.StateDir)

	switch c.Harness {
	case "claude", "codex":
	default:
		return fmt.Errorf("unknown harness %q (want: claude, codex)", c.Harness)
	}
	if c.Prompt == "" {
		return errors.New("prompt is empty")
	}
	return nil
}

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
