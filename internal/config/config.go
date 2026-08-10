// Package config holds the loop's runtime configuration: a file (JSON for now —
// TOML is the likely final format, matching Codex's own config) overlaid with
// explicit command-line flags.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/secureurl"
)

// defaultBacklogURL is the base the driver reads backlog counts from when the
// operator sets no backlog_url — and, since CLA-257, the default TRUSTED ORIGIN:
// the only host the account-scoped API key is sent to unless the operator names
// another one in their own config file.
const defaultBacklogURL = "https://clankerbar.com"

// defaultPrompt asks each session for exactly ONE task.
//
// The wording is not casual: the served protocol
// (clankerbar.com/skills/clankerbar.md) defines "work the next backlog item" as
// "means exactly one - run the loop once, finish that task, and stop", against
// "work the backlog", which it defines as drain the whole ready queue. This
// string is read by an agent that has read that document, so a paraphrase is a
// behaviour change. It is pinned by TestDefaultPromptAsksForOneTask, which is
// there because the previous default was the DRAIN phrase and nothing failed.
const defaultPrompt = "Work the next backlog item."

// The built-in phase names, as constants because Validate reasons about them: a
// sequence that ENDS on the implement brief can never reach review.
const (
	implementPhaseName = "implement"
	reviewPhaseName    = "review"
)

// Phase is one session in a task's sequence. A task runs as an ordered list of
// them, each a separate harness process, so context resets between them.
//
// The point is a context reset the MODEL cannot perform for itself. A session's
// context grows monotonically and is re-read on every turn: one measured task
// (CLA-309, 2026-08-11) spent 66.2M tokens over 370 turns, and its last four
// deciles were 53% of that purely because each of those turns re-read ~250k. The
// served protocol already tells a session to shed at a safe checkpoint, and says
// in the same breath that the rule is a no-op if the harness cannot discard
// context. Inside a `claude -p` run there is no tool and no slash command a model
// can use to discard its own context, so for the agent that rule is dead letter.
//
// What was evaluated and NOT chosen, since the driver does have options here
// (claude 2.1.226): `--autocompact <auto|tokens>` compacts at a THRESHOLD, and
// `--continue` / `--resume` / `--fork-session` restore a session rather than
// shedding one. Compaction lands wherever the threshold falls, which is the
// specific thing the protocol warns about — a summarised context can garble the
// bar the work is judged against, and the session will not know. A phase boundary
// lands at a point where everything load-bearing is already durable elsewhere
// (code pushed, bar and decisions on the plane), so the next phase re-reads them
// from source instead of trusting a summary. Autocompact is also Claude-only,
// where a process boundary is a thing every harness has. Worth revisiting: the
// two compose, and autocompact inside a long phase costs nothing to try.
//
// Why phases and not a better-worded rule: "your job this session is
// implementation only, then stop" is a SCOPE instruction, and scope is the kind
// an agent follows reliably. The driver decides the boundary; the model only has
// to respect its brief. MaxTurns is the backstop for when it does not, and the
// existing salvage (CLA-314) commits and pushes whatever a cut-off phase left.
//
// The cost being paid: a session's fixed startup — re-reading the served
// protocol and the repo's agent guide — is paid per PHASE rather than per task.
// That is real, and it is why phases are opt-in and why splitting thin is a bad
// trade: the saving falls off per extra cut while the ramp does not.
type Phase struct {
	// Name selects a built-in prompt when Prompt is empty, and labels the phase
	// in the iteration log. See builtinPhasePrompts.
	Name string `json:"name"`

	// Prompt is what this phase's session is asked to do. Empty means "use the
	// built-in prompt for Name". Two placeholders are substituted by the driver
	// from the claim the previous phase left held: {{taskId}} and {{runId}}.
	// They are the whole reason a later phase can RESUME a run instead of
	// claiming a new task, and they are only available once a phase has held a
	// claim — so they are meaningless in the first phase.
	Prompt string `json:"prompt"`

	// MaxTurns caps the session's turns (harness permitting) so the boundary
	// lands even if the model works past its brief. 0 = uncapped.
	MaxTurns int `json:"max_turns"`
}

// builtinPhasePrompts are the shipped briefs, selected by phase name.
//
// The split is implement, then review-and-fix, and that grouping is deliberate:
// the reviewing session dispatches the review itself, so it holds the findings
// while it fixes them. A third "fix" phase would hold neither the implementation
// nor the review context and would re-acquire both — which is why the repo's own
// workflow puts implementation and fix in ONE actor and the review in a separate
// read-only one. Splitting where that workflow already splits is the whole idea.
var builtinPhasePrompts = map[string]string{
	implementPhaseName: "Work the next backlog item. This session is PHASE 1 of 2, and its scope is implementation ONLY: " +
		"claim the task, work it in a worktree, self-verify, then COMMIT, PUSH, and record the branch with " +
		"update_task(taskId, runId, branch). Then STOP and end the session. Do NOT run the review gate, and do NOT " +
		"move the task to in_review — a second session resumes this same run from that checkpoint and does both. " +
		"Ending there is this task going to plan, not the task being abandoned.",

	reviewPhaseName: "You are PHASE 2 of 2 on task " + PhaseTaskPlaceholder + ", which an earlier session has already " +
		"implemented, committed and pushed. You are RESUMING that run, not starting a new one: do not call " +
		"next_task, and do not claim anything. Call heartbeat(\"" + PhaseRunPlaceholder + "\") to resume the run, " +
		"then get_task with includeDecisions: true to re-read the bar and the standing decisions, and read the diff " +
		"on the branch recorded on the task. Then run the adversarial review gate, fix what it finds, re-verify, " +
		"push, and hand the task to in_review.",
}

// phaseNameRe is what a phase name may contain, because it becomes part of an
// iteration log's filename.
var phaseNameRe = regexp.MustCompile(`^[a-z0-9-]+$`)

// The placeholders a phase prompt may carry, substituted by the driver from the
// claim the previous phase left held.
const (
	PhaseTaskPlaceholder = "{{taskId}}"
	PhaseRunPlaceholder  = "{{runId}}"
)

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

	// Prompt is what each fresh session is asked to do. Default: work ONE task.
	// It is not a per-task prompt - the backlog is the source of tasks - but it
	// does decide how much a session takes on, and the served protocol reads the
	// wording precisely: "work the next backlog item" means exactly one task,
	// while "work the backlog" means drain the whole ready queue in that session.
	//
	// One task is the default for two reasons, and only the second is unconditional.
	//
	// Context: each session starts fresh, so per-session cost stays bounded however
	// long the queue is, where a draining session CAN accumulate every task it
	// touches into one context. "Can", not "does" - a repo whose agent guide tells
	// the session to dispatch each task to a subagent keeps the orchestrator thin,
	// so this argument is contingent on the target repo's own guidance.
	//
	// Control latency: this one always holds. The console pause, HALT,
	// max_iterations and the budget breaker are all evaluated at the top of
	// loop.Run's iteration, and none of them can interrupt a session that is
	// already running (STOP is additionally honoured inside waitOrStop, but only
	// while the driver is between or backing off from sessions, never mid-session).
	// So the iteration IS the operator's control granularity: one task per
	// iteration means a pause takes effect after the current task instead of after
	// the whole queue.
	//
	// The trade being bought: a session's fixed startup cost - re-reading the
	// served protocol and the repo's agent guide - is now paid per task rather
	// than amortised across a drain.
	Prompt string `json:"prompt"`

	// Phases splits ONE task across several sessions, so its context resets at a
	// checkpoint the model cannot reach itself. See Phase for why this exists.
	//
	// OPT-IN, and deliberately so. Left empty, a task runs exactly as it always
	// has: one session, asked Prompt. Two phases mean a task that reaches its
	// checkpoint and stops there is only HALF finished until the next session
	// runs, so switching it on before the sequence has been watched end to end
	// would turn a driver bug into tasks that silently never reach review. An
	// operator opts in per config, and the shipped briefs are one line each:
	//
	//	"phases": [{"name": "implement"}, {"name": "review"}]
	//
	// Naming a phase and leaving its prompt empty takes the built-in brief for
	// that name; set `prompt` to override it, and `max_turns` to cap it.
	//
	// Phases and Prompt are mutually exclusive — Validate refuses both, rather
	// than silently preferring one and leaving an operator to wonder which of
	// two prompts their sessions are actually being handed.
	Phases []Phase `json:"phases"`

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
	// logs. Empty = an XDG state path OUTSIDE the workdir, keyed to it — see
	// ResolveStateDir. Setting it back inside the workdir is allowed and is the
	// operator's call to make; the hardening in internal/statedir holds either way.
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
	// from the environment, never this config file. That 0600 is ENFORCED, not
	// advice: a file any other local account can read is refused (see resolveEnv).
	Env map[string]string `json:"env"`

	source string   // path the config was loaded from, for diagnostics
	env    []string // resolved KEY=VALUE pairs (built in Validate)

	// workDirImplicit records that WorkDir arrived empty and Validate filled it in
	// from the cwd. The value is then absolute like any other, so nothing
	// downstream can tell the difference — but the state dir hangs off it, and an
	// operator diagnosing "doctor says no STOP marker but the loop will not start"
	// needs to be told the two processes were started in different places.
	workDirImplicit bool
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

// CountsSpend reports whether this budget bounds SPEND — tokens or dollars — as
// opposed to only the wall clock.
//
// The distinction matters when a session's spend cannot be measured at all (a
// stream the supervisor could not read whole, CLA-262): a run under a spend
// ceiling has been promised something the driver can no longer deliver, while a
// run under a wall-clock ceiling alone still has its ceiling intact, because the
// clock does not depend on anything the child said.
func (b Budget) CountsSpend() bool { return b.MaxTokens > 0 || b.MaxCostUSD > 0 }

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

// EffectivePhases is the sequence a task runs as: the configured phases with
// their built-in prompts resolved, or — when none are configured — a single
// phase carrying Prompt.
//
// The unphased case is expressed as one phase on purpose, so the driver has
// exactly ONE shape to run and the old behaviour is not a second code path that
// can rot untested. "No phases" and "one phase" are the same thing downstream.
func (c *Config) EffectivePhases() []Phase {
	if len(c.Phases) == 0 {
		return []Phase{{Prompt: c.Prompt}}
	}
	out := make([]Phase, len(c.Phases))
	for i, ph := range c.Phases {
		if ph.Prompt == "" {
			ph.Prompt = builtinPhasePrompts[ph.Name]
		}
		out[i] = ph
	}
	return out
}

// BuiltinPhaseNames lists the shipped phase briefs, sorted so the error naming
// them is stable.
func BuiltinPhaseNames() []string {
	names := make([]string, 0, len(builtinPhasePrompts))
	for n := range builtinPhasePrompts {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Label names a phase for logs and log filenames: its name, or its 1-based
// position when it has none.
func (p Phase) Label(i int) string {
	if p.Name != "" {
		return p.Name
	}
	return strconv.Itoa(i + 1)
}

func defaults() *Config {
	return &Config{
		Harness:    "claude",
		Prompt:     defaultPrompt,
		BacklogURL: defaultBacklogURL,
	}
}

// cwdConfigName is the file that used to be auto-discovered from the process
// working directory, ahead of the operator's own config. It no longer is - see
// refuseImplicitWorkDirConfig - but the name is still recognised so its presence
// can be refused loudly rather than ignored silently.
const cwdConfigName = "clankerbar.json"

// homeConfigRelPath is the one config file this tool discovers on its own,
// relative to the user's home directory.
var homeConfigRelPath = filepath.Join(".config", "clankerbar", "config.json")

// Load reads config from path (or a discovered default), layered over defaults.
// An explicit path that cannot be read is an error; a missing default file is not.
//
// DISCOVERY IS HOME-ONLY (CLA-260). An explicit --config is honoured wherever it
// points, including into the working directory; what is gone is the implicit
// candidate. See refuseImplicitWorkDirConfig.
func Load(path string) (*Config, error) {
	cfg := defaults()
	p := path
	if p == "" {
		if err := refuseImplicitWorkDirConfig(); err != nil {
			return nil, err
		}
		p = discover()
	}
	if p == "" {
		return cfg, nil
	}
	data, err := readOwnerOnly(p, groupOtherWrite)
	if err != nil {
		if path == "" && errors.Is(err, os.ErrNotExist) {
			return cfg, nil // discovered default absent — fine
		}
		if errors.Is(err, errInsecureMode) {
			return nil, fmt.Errorf("%w: anyone who can write it owns the prompt, the permission policy and the child environment of the next unattended run - chmod go-w %s", err, p)
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
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	p := filepath.Join(home, homeConfigRelPath)
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// refuseImplicitWorkDirConfig refuses to run when a `clankerbar.json` is sitting
// in the process working directory and no --config was given.
//
// It used to be the FIRST discovery candidate, stat'd as a relative path against
// whatever cwd the process happened to have, ahead of the operator's own
// ~/.config file, with no ownership or provenance check. Everything in it is
// load-bearing for an unattended run: `prompt` is the entire instruction the
// fresh session gets, `settings_path` is the headless allow/deny policy,
// `env` is arbitrary environment for the child (including secrets read off
// disk), and `backlog_url` is the one origin the account-scoped API key may be
// sent to (CLA-257 - a fix that rests on this file being the operator's).
//
// The working directory is exactly where that trust does not hold. It is a
// checkout, which may have been cloned from anywhere, and the sessions this
// daemon spawns run there WITH EDIT PERMISSION - so a session can write the file
// that owns the NEXT run's prompt, policy and environment. Config is read once at
// startup, so the damage lands on tomorrow's cron, not on the run that wrote it.
//
// REFUSED, NOT IGNORED. Ignoring it would silently fall back to the home config
// or to bare defaults, which for an operator who genuinely relied on cwd
// discovery means an unattended loop running a different prompt against a
// different backlog and saying nothing - the same class of silent-wrong-config
// this closes. A refusal costs the honest operator one flag they are already
// shown (`-c ./clankerbar.json`), and turns the hostile case into a stopped
// daemon a human looks at rather than a captured one nobody does.
func refuseImplicitWorkDirConfig() error {
	fi, err := os.Stat(cwdConfigName)
	if err != nil || fi.IsDir() {
		return nil
	}
	shown := cwdConfigName
	if abs, err := filepath.Abs(cwdConfigName); err == nil {
		shown = abs
	}
	return fmt.Errorf(
		"refusing to auto-load %s: a config file in the working directory is no longer discovered implicitly - it decides the prompt, the permission policy, the child environment and the API key's destination for every session this loop spawns, and the working directory is a checkout the sessions themselves can write. Name it if it is yours (--config %s), or move it to ~/%s",
		shown, cwdConfigName, homeConfigRelPath,
	)
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

// Permission bits that disqualify a file this loop is about to TRUST.
//
//   - groupOtherWrite: someone other than the owner can REWRITE it. Fatal for a
//     config file, whose contents choose what an unattended session is told to do
//     and what it is allowed to call.
//   - groupOtherAccess: someone other than the owner can READ it (write implied).
//     Fatal for a secret - the whole point of the `@path` indirection.
const (
	groupOtherWrite  os.FileMode = 0o022
	groupOtherAccess os.FileMode = 0o077
)

// errInsecureMode is the sentinel every mode refusal wraps, so callers can add
// their own "and here is why that matters" without re-deriving the mode.
var errInsecureMode = errors.New("insecure file mode")

// readOwnerOnly reads a file the loop is about to trust, refusing it when anyone
// but the owner (or root) could have decided its contents.
//
// The file's own mode is taken from the OPEN FILE HANDLE, not from a separate
// os.Stat of the path, so there is no window between the mode that was checked
// and the bytes that were read: a file swapped after the check is a different
// inode, and this reads the one it vetted. Symlinks are followed (the target's
// mode is what matters), which is deliberate - an operator pointing at
// ~/.secrets/token through a symlink is normal.
func readOwnerOnly(path string, forbid os.FileMode) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err := vetTrustedFile(path, fi, forbid); err != nil {
		return nil, err
	}
	return io.ReadAll(f)
}

// vetTrustedFile is the three questions a mode check has to answer to mean what
// it says. Only the first is about the file's own bits:
//
//  1. Can group or other CHANGE (or, for a secret, READ) it?
//  2. Is it OWNED by someone else? A 0600 file is unreadable by a peer, so under a
//     normal uid this is nearly self-enforcing - but a root daemon reading a
//     user-owned config is exactly the case where mode alone says "fine" and the
//     file is under someone else's control.
//  3. Can group or other REPLACE it by writing its directory? A 0600 token in a
//     0777 directory can be unlinked and recreated by any local account, which
//     defeats (1) entirely. This checks the immediate parent only, not the whole
//     ancestor chain - a world-writable /home is its own, larger problem, and
//     OpenSSH's StrictModes draws the line in the same place. The sticky bit is
//     the documented exception (/tmp), where only an entry's owner may remove it.
func vetTrustedFile(path string, fi os.FileInfo, forbid os.FileMode) error {
	if !permissionBitsAreMeaningful {
		// Go synthesises a fixed mode for ordinary files on such platforms, so the
		// bits carry no information and enforcing them would refuse every config
		// the tool has - a denial, not a defence.
		return nil
	}
	if perm := fi.Mode().Perm(); perm&forbid != 0 {
		verb := "writable by group or other"
		if forbid&groupOtherAccess == groupOtherAccess {
			verb = "readable by group or other"
		}
		return fmt.Errorf("%w: %s is %s (mode %04o)", errInsecureMode, path, verb, perm)
	}
	if uid, ok := fileOwnerUID(fi); ok {
		if me := os.Geteuid(); uid != me && uid != 0 {
			return fmt.Errorf("%w: %s is owned by uid %d, not by you (uid %d) or root - its owner decides its contents whatever its mode says", errInsecureMode, path, uid, me)
		}
	}
	dir := filepath.Dir(path)
	if dfi, err := os.Stat(dir); err == nil {
		if m := dfi.Mode(); m.Perm()&groupOtherWrite != 0 && m&os.ModeSticky == 0 {
			return fmt.Errorf("%w: %s sits in %s, which is writable by group or other (mode %04o) - anyone who can write the directory can replace the file whatever the file's own mode is", errInsecureMode, path, dir, m.Perm())
		}
	}
	return nil
}

// refuseInsecureMode vets a file the loop hands ONWARD rather than reads, so the
// same rule covers a path whose contents are another program's business.
//
// Only an insecure-mode verdict is returned: absence and unreadability belong to
// whichever check already reports them (for settings_path, `doctor`'s permissions
// check, which says what a missing policy file means far better than a config
// error could).
func refuseInsecureMode(path string, forbid os.FileMode) error {
	if path == "" {
		return nil
	}
	if _, err := readOwnerOnly(path, forbid); errors.Is(err, errInsecureMode) {
		return err
	}
	return nil
}

// underWorkDir resolves a RELATIVE path the way the spawned harness will: against
// the session's working directory, not against the daemon's.
//
// The two are routinely different, and every one of these paths is handed to the
// child verbatim while `cmd.Dir` is the workdir (harness/claude.go), so a
// relative value used to be read by US against one directory and by the CHILD
// against another. That is not a cosmetic mismatch: `mcp_config_path: ".mcp.json"`
// with a workdir elsewhere made checkMCPConfigOrigins vet a file that did not
// exist (absent is the one benign case, so the gate passed) while the session
// loaded the checkout's file with its own origins and its own `Authorization`
// headers - the CLA-257 property defeated by a relative path, with `doctor` green
// throughout. Resolving here means the file that is VETTED is provably the file
// that is USED.
//
// An empty workdir means the child inherits our cwd, so relative already means
// the same thing to both and is left alone.
func underWorkDir(p, workdir string) string {
	if p == "" || workdir == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(workdir, p)
}

// Validate normalizes path fields and checks the resolved config is runnable.
func (c *Config) Validate() error {
	c.ConfigDir = expandHome(c.ConfigDir)
	c.WorkDir = expandHome(c.WorkDir)
	c.MCPConfigPath = expandHome(c.MCPConfigPath)
	c.SettingsPath = expandHome(c.SettingsPath)
	c.StateDir = expandHome(c.StateDir)

	// The workdir becomes absolute HERE, once, before anything is derived from it
	// — the same treatment MCPConfigPath/SettingsPath/ConfigDir get on the next
	// three lines. It used to stay as written and be resolved at each point of use
	// against whatever cwd that process had, so one config file described
	// different runs: `clankerbar run` from the repo and `clankerbar doctor` from
	// ~ hashed to two DIFFERENT state dirs (ResolveStateDir -> stateSlug). Doctor
	// then reported "no stop markers" about a directory the running loop had never
	// opened, and a stray state dir accumulated at every cwd anyone invoked from.
	// Before CLA-259 the same cwd-dependence was at least visible as
	// `./.clankerbar-loop`; a hashed name under ~/.local/state hides it.
	//
	// An empty workdir still means "where the daemon was started" — that is the
	// directory the child inherits either way. It is just pinned now, not re-read.
	// Sticky: Validate is not guaranteed to be called only once, and a second call
	// sees the absolute value the first one wrote — it would then answer
	// "configured" about a workdir nobody configured.
	if c.WorkDir == "" {
		c.workDirImplicit = true
	}
	absWorkDir, err := filepath.Abs(c.WorkDir)
	if err != nil {
		return fmt.Errorf("workdir %s: %w", c.WorkDir, err)
	}
	c.WorkDir = absWorkDir

	c.MCPConfigPath = underWorkDir(c.MCPConfigPath, c.WorkDir)
	c.SettingsPath = underWorkDir(c.SettingsPath, c.WorkDir)
	c.ConfigDir = underWorkDir(c.ConfigDir, c.WorkDir)

	// Validate against the harness registry (not a hand-kept switch) so the accepted
	// set can never drift from what is actually registered — an unregistered value is
	// rejected here before harness.Get is consulted, and a newly registered adapter is
	// accepted automatically. harness does not import config, so there is no cycle.
	if !harness.Known(c.Harness) {
		return fmt.Errorf("unknown harness %q (want: %s)", c.Harness, strings.Join(harness.Names(), ", "))
	}
	if len(c.Phases) == 0 && c.Prompt == "" {
		return errors.New("prompt is empty")
	}
	// Two answers to the same question. A phased task is asked its PHASE's
	// prompt and never this one, so a config carrying both is refused rather
	// than resolved — an operator who set one and forgot the other gets told,
	// instead of wondering which of two prompts their sessions were handed.
	//
	// "Set" means "differs from the built-in default": defaults() fills Prompt
	// before the file is layered over it, so an untouched Prompt is
	// indistinguishable from an absent one by the time we get here. The residue
	// is that writing the default string out by hand alongside phases is
	// accepted silently, which costs nothing — it is the same prompt, unused.
	if len(c.Phases) > 0 && c.Prompt != "" && c.Prompt != defaultPrompt {
		return errors.New("config sets both `prompt` and `phases`; a phased task is asked its phase's prompt " +
			"and never `prompt`, so remove one of them")
	}
	// Phases rest entirely on the adapter observing the session's claim: the
	// handback across a seam, the salvage, and the delivery check all gate on
	// Result.Claim.Held(). An adapter that never populates it would not fail —
	// it would implement and push and then stop after phase 1 on EVERY task,
	// reported by a log line that reads like an ordinary early finish. Refuse
	// here instead, where an operator can see it, rather than at 3am.
	if len(c.Phases) > 1 {
		if caps, ok := harness.CapabilitiesOf(c.Harness); ok && !caps.TracksClaims {
			return fmt.Errorf("harness %q cannot run `phases`: it does not observe the session's task claim, "+
				"so a phase could not hand its task to the next one (only the claim-tracking harnesses can: use one, or drop `phases`)",
				c.Harness)
		}
	}
	for i, ph := range c.Phases {
		if ph.Prompt == "" && builtinPhasePrompts[ph.Name] == "" {
			return fmt.Errorf("phases[%d]: no `prompt`, and %q is not a built-in phase name (built-ins: %s)",
				i, ph.Name, strings.Join(BuiltinPhaseNames(), ", "))
		}
		// The name reaches a log FILENAME, and statedir refuses one it does not
		// like — which would cost the phase its iteration log entirely, the one
		// artifact an operator has to debug the sequence with.
		if ph.Name != "" && !phaseNameRe.MatchString(ph.Name) {
			return fmt.Errorf("phases[%d]: name %q must be lowercase letters, digits and hyphens (it becomes part of the iteration log filename)", i, ph.Name)
		}
		if ph.MaxTurns < 0 {
			return fmt.Errorf("phases[%d]: max_turns is negative", i)
		}
		// The resume placeholders are filled from the claim the PREVIOUS phase left
		// held, so the first phase has nothing to fill them from. The driver leaves
		// an unfilled one standing rather than blanking it — a literal {{runId}} in
		// a log is a misconfiguration announcing itself — but there is no reason to
		// spend a session discovering that when the config says it up front.
		//
		// Checked against the RESOLVED prompt, not the configured one: the usual way
		// to get this wrong is `phases: [{"name": "review"}, …]`, whose configured
		// prompt is empty and whose built-in brief is full of placeholders.
		if i == 0 {
			effective := ph.Prompt
			if effective == "" {
				effective = builtinPhasePrompts[ph.Name]
			}
			for _, pl := range []string{PhaseTaskPlaceholder, PhaseRunPlaceholder} {
				if strings.Contains(effective, pl) {
					return fmt.Errorf("phases[0] (%q): its prompt carries %s, but the FIRST phase has no previous claim to fill it from — it claims a task of its own, so a resume brief cannot go first",
						ph.Label(0), pl)
				}
			}
		}
	}
	// A sequence whose last phase is the implement brief has no path to in_review:
	// every task would stop half-finished, forever. Cheap to catch here, expensive
	// to discover a night later.
	if n := len(c.Phases); n > 0 {
		lastPh := c.Phases[n-1]
		if lastPh.Prompt == "" && lastPh.Name == implementPhaseName {
			return fmt.Errorf("phases[%d]: the sequence ends on the %q brief, which tells its session to stop at the checkpoint — nothing would ever hand a task to review (add a %q phase, or give this one its own prompt)",
				n-1, implementPhaseName, reviewPhaseName)
		}
	}

	// Default mcp_config_path to <workdir>/.mcp.json when that file exists. Claude's
	// -p mode does NOT auto-discover .mcp.json, so without this a bare `clankerbar
	// run` from a workdir that carries one would spawn sessions with no clankerbar
	// tools at all — and the poller could derive no slug. Explicit config still wins.
	if c.MCPConfigPath == "" {
		c.MCPConfigPath = discoverMCPConfig(c.WorkDir)
	}

	// Where the account-scoped API key is allowed to go, settled once, here
	// (CLA-257). backlog_url is the operator's own statement of it and is held to
	// the TLS floor; the workdir's .mcp.json is untrusted input and may not name a
	// different host. Refusing at Validate means `doctor` reports it as a failed
	// config check and `run` never starts — neither one makes a credentialed
	// request to the host the file named.
	// An empty backlog_url used to mean "take the origin from .mcp.json"; with that
	// road closed it would mean "no origin at all", which is a silent blind drain
	// for a config that merely omitted a field. Fill it, so a validated config
	// always has exactly one trusted origin to check the rest against.
	if c.BacklogURL == "" {
		c.BacklogURL = defaultBacklogURL
	}
	if _, err := secureurl.Origin(c.BacklogURL); err != nil {
		return fmt.Errorf("backlog_url: %w", err)
	}
	if err := c.checkMCPConfigOrigins(c.MCPConfigPath, "mcp_config_path"); err != nil {
		return err
	}

	// The settings file IS the permission policy - the allow/deny rules that are
	// the only thing gating what an unattended session may call, since there is no
	// human to prompt. Holding the config file to a mode check and not this one
	// would leave the shorter route to the same capture open: rewrite the policy
	// rather than the config that names it.
	if err := refuseInsecureMode(c.SettingsPath, groupOtherWrite); err != nil {
		return fmt.Errorf("settings_path: %w - it is the allow/deny policy the unattended session is gated by: chmod go-w %s", err, c.SettingsPath)
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
		if p.WorkDir != "" {
			// Absolute for the same reason the top-level workdir is, and so
			// SessionWorkDirs can be compared against a state dir path lexically.
			// Left empty when empty: that means "the top-level workdir", which is
			// already absolute, and filling it in here would hide the fallback.
			abs, err := filepath.Abs(p.WorkDir)
			if err != nil {
				return fmt.Errorf("projects[%d].workdir %s: %w", i, p.WorkDir, err)
			}
			p.WorkDir = abs
		}
		p.MCPConfigPath = expandHome(p.MCPConfigPath)
		// Against the workdir the SESSIONS for this project get - its own, falling
		// back to the top-level one, exactly as loop.Driver.invocation resolves it.
		// See underWorkDir for why a relative path may not be left to the daemon's cwd.
		effectiveWorkDir := p.WorkDir
		if effectiveWorkDir == "" {
			effectiveWorkDir = c.WorkDir
		}
		p.MCPConfigPath = underWorkDir(p.MCPConfigPath, effectiveWorkDir)
		if p.MCPConfigPath == "" {
			p.MCPConfigPath = discoverMCPConfig(effectiveWorkDir)
		}
		if err := c.checkMCPConfigOrigins(p.MCPConfigPath, fmt.Sprintf("projects[%d].mcp_config_path", i)); err != nil {
			return err
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
//
// An `@path` file must be owner-only (CLA-260). The indirection exists for one
// reason - holding a credential out of the config file - and the doc comment on
// Env has always told operators to keep it at 0600, but nothing checked, so a
// `chmod 644` token file was accepted in silence and every local account could
// read the key that drives the whole backlog. Refused rather than warned: a WARN
// in an overnight log is read after the fact, if at all, and the fix is one
// chmod.
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
			data, err := readOwnerOnly(path, groupOtherAccess)
			if err != nil {
				if errors.Is(err, errInsecureMode) {
					return nil, fmt.Errorf("env %s: %w - an @path secret must be readable only by you: chmod 600 %s", k, err, path)
				}
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

// legacyStateDirName is where the state dir used to live, relative to the
// workdir. Kept only so `doctor` and the loop can point an operator at a
// leftover one — nothing reads markers from it (see LegacyStateDir).
const legacyStateDirName = ".clankerbar-loop"

// ResolveStateDir returns the absolute path where control markers and iteration
// logs live.
//
// It defaults OUTSIDE the workdir (CLA-259), to
// `$XDG_STATE_HOME/clankerbar/loop/<slug>` — `~/.local/state/...` when that
// variable is unset. It used to be `<workdir>/.clankerbar-loop`, which put the
// daemon's own writes inside the one tree its spawned sessions are permitted to
// write, and that placement was the root of three separate defects rather than a
// detail: transcripts a session could read or commit, and paths a session could
// pre-plant a symlink at to make the daemon truncate a file outside the
// confinement the adapters impose on the session. Moving it removes the class
// instead of guarding three symptoms — the session cannot reach the directory at
// all.
//
// The slug is `<workdir basename>-<hash of its absolute path>`. The hash keeps
// two checkouts that share a basename apart; the basename keeps the directory
// recognisable when an operator goes looking for a transcript. It is derived
// from the cleaned absolute path and nothing else — deliberately NOT from
// EvalSymlinks, because a symlinked workdir that resolves differently once its
// target exists would silently move an operator's STOP marker out from under
// them.
//
// An explicit state_dir always wins, including one pointed back inside the
// workdir: that is a supported thing to want, and internal/statedir keeps its
// guarantees there too.
func (c *Config) ResolveStateDir() (string, error) {
	if c.StateDir != "" {
		abs, err := filepath.Abs(c.StateDir)
		if err != nil {
			return "", fmt.Errorf("state_dir %s: %w", c.StateDir, err)
		}
		return abs, nil
	}
	abs, err := c.absWorkDir()
	if err != nil {
		return "", err
	}
	home, err := stateHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "clankerbar", "loop", stateSlug(abs)), nil
}

// LegacyStateDir is the pre-CLA-259 `<workdir>/.clankerbar-loop`, returned only
// when it still exists on disk AND is not where we are actually writing. It is
// reported, never read: honouring a STOP marker there would hand a spawned
// session the daemon's stop switch back, which is half of what moving the
// directory bought. Empty means there is nothing to tell the operator about.
func (c *Config) LegacyStateDir() string {
	abs, err := c.absWorkDir()
	if err != nil {
		return ""
	}
	legacy := filepath.Join(abs, legacyStateDirName)
	if current, err := c.ResolveStateDir(); err != nil || current == legacy {
		return ""
	}
	if _, err := os.Lstat(legacy); err != nil {
		return ""
	}
	return legacy
}

// absWorkDir is the workdir as an absolute, cleaned path.
//
// Validate already made WorkDir absolute (see the comment there — resolving it
// per use against the caller's cwd is what gave one config file two state dirs).
// The fallback is only for a Config that was never validated, which is tests and
// nothing else.
func (c *Config) absWorkDir() (string, error) {
	base := c.WorkDir
	if base == "" {
		base = "."
	}
	abs, err := filepath.Abs(base)
	if err != nil {
		return "", fmt.Errorf("workdir %s: %w", base, err)
	}
	return abs, nil
}

// WorkDirIsImplicit reports whether the workdir was taken from the directory
// this process started in rather than configured. Validate pins it either way,
// so the only thing that still varies between two invocations of the SAME config
// is this case — which is worth saying out loud, because the state dir's name is
// a hash and the divergence is otherwise invisible.
func (c *Config) WorkDirIsImplicit() bool { return c.workDirImplicit }

// SessionWorkDirs is every directory a spawned session actually runs in,
// absolute, resolved the way the loop resolves them.
//
// It is the CONFINEMENT boundary expressed as paths: a session may write
// anywhere under one of these and nowhere else, so these are exactly the trees
// in which a symlink is something that may have been planted rather than
// something the operator laid out. statedir.Open takes them for that reason.
func (c *Config) SessionWorkDirs() []string {
	if len(c.Projects) == 0 {
		abs, err := c.absWorkDir()
		if err != nil {
			return nil
		}
		return []string{abs}
	}
	out := make([]string, 0, len(c.Projects))
	for _, p := range c.Projects {
		dir := p.WorkDir
		if dir == "" {
			dir = c.WorkDir
		}
		if abs, err := filepath.Abs(dir); err == nil {
			out = append(out, abs)
		}
	}
	return out
}

// stateHome is $XDG_STATE_HOME, or ~/.local/state. A relative XDG_STATE_HOME is
// ignored rather than honoured: the spec says the variable must hold an absolute
// path, and a relative one would resolve against whatever cwd the daemon happens
// to have been started in — the exact ambiguity underWorkDir exists to stamp out.
func stateHome() (string, error) {
	if v := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(v) {
		return filepath.Clean(v), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate a home directory for the loop's state dir (set state_dir, or XDG_STATE_HOME): %w", err)
	}
	return filepath.Join(home, ".local", "state"), nil
}

// stateSlug names one workdir's state directory: a readable basename plus a hash
// of the full path, so it is both recognisable and unambiguous.
func stateSlug(absWorkDir string) string {
	sum := sha256.Sum256([]byte(absWorkDir))
	return sanitizeSlug(filepath.Base(absWorkDir)) + "-" + hex.EncodeToString(sum[:8])
}

// sanitizeSlug reduces a directory basename to something plainly safe as one
// path component: no separators, no dot-prefix, no surprises from a checkout
// named by someone else.
func sanitizeSlug(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), ".-")
	if len(out) > 40 {
		out = out[:40]
	}
	if out == "" {
		return "workdir"
	}
	return out
}

// Source is the file the config was loaded from ("" if none).
func (c *Config) Source() string { return c.source }

// BacklogEndpoint returns the full, project-scoped MCP URL the driver's own
// backlog poll should hit — the same `/mcp/<project>` endpoint the harness uses.
//
// BacklogURL defaults to a bare base (`https://clankerbar.com`), which the plane
// rejects without the project slug in the path. The slug lives in the harness
// .mcp.json (MCPConfigPath), so we take it from there — but ONLY the slug. The
// origin always comes from CredentialOrigin, because this URL carries the API key
// (CLA-257): a file inside the workdir may say which project, never which host.
// An explicit backlog_url that already names a `/mcp/<project>` path wins outright.
//
// Returns "" when no usable project-scoped endpoint can be resolved (a bare base
// and no slug). That is deliberate: New("") yields a not-wired poller, so the loop
// falls into blind drain — which still makes progress — rather than retrying
// forever against a slug-less base the plane can only reject.
func (c *Config) BacklogEndpoint() string {
	origin := c.CredentialOrigin()
	if origin == "" {
		return ""
	}
	if strings.Contains(c.BacklogURL, "/mcp/") {
		return c.BacklogURL
	}
	raw := mcpURLFromConfig(c.MCPConfigPath)
	if slug := slugFromMCPURL(raw); slug != "" {
		return mcpPath(origin, slug)
	}
	// A pre-CLA-99 file names a bare `/mcp` with no slug to lift out. Take its
	// path verbatim, but only once its origin is proven to BE the trusted one —
	// which Validate has already refused the file for otherwise. Dropping to ""
	// here instead would silently disable the driver's one write (the CLA-242
	// release-on-interrupt) for those setups: a security fix quietly deleting an
	// unrelated feature.
	if secureurl.SameOrigin(origin, raw) {
		return raw
	}
	return ""
}

// ProjectEndpoint returns one configured project's MCP endpoint — the same
// `/mcp/<slug>` URL that project's sessions are pointed at — so a write the driver
// makes lands on the same project its sessions are working.
//
// The slug is the project's own declared `slug`, which Validate has already
// cross-checked against its .mcp.json; the origin is CredentialOrigin. Neither
// half is read off the workdir's file (CLA-257).
func (c *Config) ProjectEndpoint(p Project) string {
	origin := c.CredentialOrigin()
	if origin == "" || p.Slug == "" {
		return c.BacklogEndpoint()
	}
	return mcpPath(origin, p.Slug)
}

func mcpPath(origin, slug string) string {
	return origin + "/mcp/" + url.PathEscape(slug)
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
// The origin is CredentialOrigin — `backlog_url`, the operator's own config, or
// the default base — and nothing else. Before CLA-257 it fell back to the origin
// named by the workdir's .mcp.json whenever backlog_url was left at its default,
// which is the normal setup, so a committed file in a cloned repo could redirect
// this credentialed GET to any host over plain http. A self-hosted plane is still
// reachable; it just has to be named in `backlog_url`. Only the SLUG still comes
// from .mcp.json, and it only chooses a path on an origin we already trust.
//
// Returns "" when no origin can be resolved (New("") then yields a not-wired,
// blind poller).
func (c *Config) BacklogSummaryURL() string {
	origin := c.CredentialOrigin()
	if origin == "" {
		return ""
	}
	if slug := slugFromMCPURL(c.BacklogEndpoint()); slug != "" {
		return projectSummaryPath(origin, slug)
	}
	return origin + "/api/backlog-summary"
}

// ProjectSummaryURL returns the slug-ful summary URL for one configured project
// (CLA-142): `<origin>/api/projects/<slug>/backlog-summary`, on the same trusted
// CredentialOrigin as every other credentialed call.
func (c *Config) ProjectSummaryURL(p Project) string {
	origin := c.CredentialOrigin()
	if origin == "" {
		return ""
	}
	return projectSummaryPath(origin, p.Slug)
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

// mcpURLFromConfig reads a harness MCP config and returns the clankerbar MCP
// server URL, or "" if the file is absent or names no such server.
//
// Callers use this for the PATH — a project slug — never for an origin (see
// origin.go). The old "else the first http server's URL" fallback is gone with
// it: it made an unrelated MCP entry (a docs server, a browser driver) speak for
// clankerbar, and picked which one by map iteration order. What is left is the
// entry named `clankerbar`, else whichever entry is handed CLANKERBAR_API_KEY —
// the two ways a file can actually mean "this is the clankerbar server".
//
// A read/parse failure yields "", which is safe HERE and only here: Validate has
// already refused such a file outright (checkMCPConfigOrigins fails closed), so
// this is never reached with one.
func mcpURLFromConfig(path string) string {
	servers, err := readMCPServers(path)
	if err != nil {
		return ""
	}
	for _, s := range servers {
		if s.name == "clankerbar" {
			return s.url
		}
	}
	for _, s := range servers {
		if s.usesKey {
			return s.url
		}
	}
	return ""
}
