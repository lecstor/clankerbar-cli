// Package config holds the loop's runtime configuration: a JSON file (TOML is the likely final
// format) overlaid with explicit command-line flags. Rationale: docs/config-rationale.md.
package config

const defaultBacklogURL = "https://clankerbar.com"

const DefaultCloneURLBase = "https://github.com/"

const defaultPrompt = "Work the next backlog item."

const DefaultMaxTurns = 400

const DefaultMaxZeroSpendAttempts = 3

const DefaultMaxRepeatedToolFailures = 5

const MaxInstanceNameLen = 100

const (
	ImplementPhaseName = "implement"
	ReviewPhaseName    = "review"
	FixPhaseName       = "fix"
	VerifyPhaseName    = "verify"
)

const ReviewRoundCap = 3

type Config struct {
	Role string `json:"role,omitempty"`

	Planner *RoleAssignment `json:"-"`

	Harness string `json:"harness"`

	Model string `json:"model"`

	// Models is the operator's own tier map: bucket name -> harness-specific model alias, e.g.
	// {"strong": "opus", "standard": "sonnet", "cheap": "haiku"}. A phase names a BUCKET (Phase.Tier),
	// never an alias.
	Models map[string]string `json:"models"`

	Harnesses map[string]HarnessConfig `json:"harnesses"`

	// Prompt is what each fresh session is asked to do. One task is the default for two reasons, and
	// only the second is unconditional.
	Prompt string `json:"prompt"`

	Phases []Phase `json:"phases"`

	Conflict *ConflictStep `json:"conflict,omitempty"`

	// ReviewPanel is the ordered list of independent reviewers a built-in review phase expands into.
	ReviewPanel []ReviewPanelMember `json:"review_panel,omitempty"`

	// ReviewPanelConcurrency caps how many of a review panel's members run AT ONCE.
	ReviewPanelConcurrency int `json:"review_panel_concurrency,omitempty"`

	reviewPanelSet bool

	WorkDir string `json:"workdir"`

	ProjectRoot bool `json:"project_root,omitempty"`

	// CloneURLBase is the URL PREFIX the supervisor clones a project's missing repos from:
	// <base><owner>/<name>.git. Empty means the built-in DefaultCloneURLBase (`https://github.com/`),
	// so a config that never mentions the key clones exactly as it always did.
	CloneURLBase string `json:"clone_url_base,omitempty"`

	ConfigDir string `json:"config_dir"`

	MCPConfigPath string `json:"mcp_config_path"`

	// InstanceName names this daemon on the console's Fleet page: presence beacons are keyed per
	// (project, instance name), so this is how two daemons driving the same project stay
	// distinguishable. The plane caps the name at 100 characters — Validate refuses longer here, so
	// a too-long name fails loudly at startup instead of silently dropping every beacon.
	InstanceName string `json:"instance_name"`

	MaxIterations int `json:"max_iterations"`

	MaxTurns int `json:"max_turns"`

	MaxSessionWallClock Duration `json:"max_session_wall_clock"`

	PollInterval Duration `json:"poll_interval"`

	IdlePollInterval Duration `json:"idle_poll_interval"`

	PauseWindows *PauseWindows `json:"pause_windows,omitempty"`

	// RouteSchedules are NAMED pricing schedules a route can be attached to, keyed by schedule name.
	RouteSchedules map[string]*RouteSchedule `json:"route_schedules,omitempty"`

	RouteWindows map[string]string `json:"route_windows,omitempty"`

	// BacklogURL is the clankerbar base URL the driver reads backlog counts from (cheap, no tokens).
	BacklogURL string `json:"backlog_url"`

	// MaxRetries bounds consecutive transient-failure retries within one drain before the loop gives
	// up. 0 = never give up (keep retrying at the backoff ceiling until the API recovers) — the
	// right default for a persistent daemon.
	MaxRetries int `json:"max_retries"`

	RetryCap Duration `json:"retry_cap"`

	MaxZeroSpendAttempts int `json:"max_zero_spend_attempts"`

	// MaxRepeatedToolFailures ends a session whose SAME tool returns the SAME error on this many
	// consecutive tool calls. Unset = the built-in default (DefaultMaxRepeatedToolFailures); 0 turns
	// the check off; negative is refused.
	MaxRepeatedToolFailures *int `json:"max_repeated_tool_failures,omitempty"`

	SessionIdle map[string]IdleBounds `json:"session_idle,omitempty"`

	SessionNice int `json:"session_nice,omitempty"`

	EchoSessions bool `json:"echo_sessions,omitempty"`

	Budget Budget `json:"budget"`

	StateDir string `json:"state_dir"`

	SettingsPath string `json:"settings_path"`

	Repos map[string]string `json:"repos"`

	PrimaryRepo string `json:"primary_repo"`

	// Escalation holds the mechanical review-tier escalation rules. The CLI evaluates them at
	// review-spawn time; the plane holds only the execution-config reference to them.
	Escalation Escalation `json:"escalation"`

	AllowUncheckedPR bool `json:"allow_unchecked_pr"`

	HealthURL string `json:"health_url"`

	IntegrationBranch string `json:"integration_branch"`

	PRBase map[string]string `json:"pr_base"`

	// AllowLocalMCPServers names the MCP server entries an operator means to run from a DISCOVERED
	// `<workdir>/.mcp.json` — the file Validate adopts by default when mcp_config_path is empty.
	AllowLocalMCPServers []string `json:"allow_local_mcp_servers"`

	Events RunConfigEvents `json:"-"`

	NoEvents bool `json:"-"`

	// Projects declares the backlogs a single loop instance drives — one entry per clankerbar
	// project. The account key in CLANKERBAR_API_KEY covers every project the operator is a member of;
	// per-project keys are never needed.
	Projects []Project `json:"projects"`

	// Env is extra environment for the spawned harness process.
	Env EnvMap `json:"env"`

	// EnvAllowlist, when non-empty, is the COMPLETE set of inherited environment variable NAMES a
	// spawned session receives. Variables not named here are dropped from the harness process, so the
	// session cannot read the tokens that happen to sit in the shell that started the supervisor.
	EnvAllowlist []string `json:"env_allowlist,omitempty"`

	source string // path the config was loaded from, for diagnostics

	mcpDiscovered bool

	harnessFromFlag bool
	modelFromFlag   bool

	workDirImplicit bool
}

type Project struct {
	Slug string `json:"slug"`

	WorkDir string `json:"workdir"`

	MCPConfigPath string `json:"mcp_config_path"`

	mcpDiscovered bool

	// MCPConfigPaths is this project's MCP config PER HARNESS (harness name -> path), for a run whose
	// phases are not all on one harness. MCPConfigPath above selects the PROJECT (its `/mcp/<slug>`
	// URL is what the poll and the sessions agree on), while the file's SCHEMA belongs to a harness
	// — opencode refuses to start on Claude's `mcpServers`.
	MCPConfigPaths map[string]string `json:"mcp_config_paths"`

	// Env is this project's extra session environment, overlaid on top of the top-level env map for
	// THIS project's sessions only — see Config.Env for what an entry holds.
	Env EnvMap `json:"env"`

	EnvPerHarness map[string]EnvMap `json:"env_per_harness"`

	EnvAllowlist []string `json:"env_allowlist,omitempty"`

	// EnvAllowlistPerHarness is this project's allowlist PER HARNESS, the most specific level of the
	// four: it wins over EnvAllowlist for sessions on that harness only.
	EnvAllowlistPerHarness map[string][]string `json:"env_allowlist_per_harness,omitempty"`

	// AllowUncheckedPR is this project's CLA-310 opt-out, overriding the top-level field of the same
	// name for this project only. See Config.AllowUncheckedPR for what it does and why the default
	// refuses.
	AllowUncheckedPR bool `json:"allow_unchecked_pr"`

	// HealthURL is this project's deployment health endpoint (`/health`), overriding the top-level
	// field of the same name for this project only.
	HealthURL string `json:"health_url"`

	// IntegrationBranch is this project's integration branch, overriding the top-level field of the
	// same name for this project only.
	IntegrationBranch string `json:"integration_branch"`

	PRBase map[string]string `json:"pr_base"`

	AllowLocalMCPServers []string `json:"allow_local_mcp_servers"`

	Repos map[string]string `json:"repos"`

	// PrimaryRepo is this project's no-repo-on-the-task fallback, overriding the top-level field of
	// the same name for this project only when non-empty.
	PrimaryRepo string `json:"primary_repo"`
}
