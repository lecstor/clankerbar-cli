// Package config holds the loop's runtime configuration: a file (JSON for now —
// TOML is the likely final format, matching Codex's own config) overlaid with
// explicit command-line flags.
package config

// defaultBacklogURL is the base the driver reads backlog counts from when the
// operator sets no backlog_url — and, since CLA-257, the default TRUSTED ORIGIN:
// the only host the account-scoped API key is sent to unless the operator names
// another one in their own config file.
const defaultBacklogURL = "https://clankerbar.com"

// DefaultCloneURLBase is the URL prefix the fleet's derived clones are made
// from when the operator sets no clone_url_base: <base><owner>/<name>.git.
// Keeping the default HTTPS is deliberate — a config that says nothing about
// the clone transport keeps the behaviour it had before the key existed.
const DefaultCloneURLBase = "https://github.com/"

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

// DefaultMaxTurns bounds a session that sets no phase cap and no top-level cap.
//
// Calibrated against the two measured anchors from the CLA-343 audit: the
// largest legitimate session on record ran 370 turns (CLA-309, 66.2M tokens),
// and the runaway this exists to stop ran 1093 turns (285.9M tokens). 400 sits
// between them — above the largest legitimate session with room to spare (8%
// headroom is deliberate, not generous, because the cost of a false positive is
// a scruffy salvage commit and a re-queued task), and at less than half the
// runaway. It is a RUNAWAY DETECTOR, not a budget: the salvage (CLA-314)
// commits and pushes whatever a cut-off session left, so a hit costs a scruffy
// commit, not work.
const DefaultMaxTurns = 400

// DefaultMaxZeroSpendAttempts bounds consecutive attempts within one drain that
// ended without the harness reporting ANY usage (see Config.MaxZeroSpendAttempts).
//
// 3 is deliberately small, and small is the point: a spend ceiling cannot see an
// attempt that reported nothing, so every one of these is a paid session the
// breaker is blind to, and the loop's other backstop - `max_retries: 0`, never
// give up - is exactly the setting that turns three of them into three thousand.
//
// What actually reaches this bound is narrower than it sounds, and worth naming
// so nobody tunes it against the wrong failure: an attempt has to be classified
// TRANSIENT (anything else ends the phase on the first one) and die before its
// harness accounts for anything - a stream that never gets past the handshake, a
// connection reset early in the session, an overloaded API turning it away. A
// real blip that reaches the model is followed by a session that reports, which
// resets the count, so the ladder an operator tuned `retry_cap` for is untouched;
// and a usage-limit pause is counted neither way (see loop.drainPhase). Raise it
// if a harness of yours legitimately dies silently more often than that.
const DefaultMaxZeroSpendAttempts = 3

// DefaultMaxRepeatedToolFailures is how many CONSECUTIVE calls of one tool may
// return the same error before the harness kills the session (see
// Config.MaxRepeatedToolFailures, CLA-739).
//
// 5 because the measured loop is nothing like the ordinary shape. The incident
// sessions made 65 to 316 identical refused calls each; a working session that
// trips over the same refusal retries it once or twice and moves on. And the
// largest legitimate run of identical failures in that incident's own logs was
// 4: a session refused `external_directory` on four shell calls in a row, which
// then recovered and carried on. So 5 sits just above what a session that is
// still working does, and every call past it re-sends the whole context.
const DefaultMaxRepeatedToolFailures = 5

// MaxInstanceNameLen is the plane's cap on a daemon instance name (CLA-465's
// report bounds): longer names are refused by Validate rather than beached as
// silently-dropped beacons.
const MaxInstanceNameLen = 100

// The built-in phase names, as constants because Validate reasons about them: a
// sequence that ENDS on the implement brief can never reach review. Exported so
// the driver can name the implement phase for the fleet dead-phase counter
// (CLA-396): only a successful IMPLEMENT phase resets it, because only the
// implement phase runs the harness that can exhibit the zero-usage death.
const (
	ImplementPhaseName = "implement"
	ReviewPhaseName    = "review"
	// FixPhaseName is the phase that APPLIES a review's findings. It is a
	// separate name from ReviewPhaseName on purpose: the review phase's terminal
	// step (PR, in_review) is injected by name, so a fix phase that reused the
	// name would inherit review-only or review-and-fix wording it did not ask
	// for. See reviewOnlyTerminalStep and the built-in briefs below.
	FixPhaseName = "fix"
	// VerifyPhaseName is the fix-verification round (CLA-638): it re-reviews the
	// fix delta, adjudicates the prior round's open MUSTs by their plane finding
	// ids, and carries the terminal step only when no MUST remains open. It is a
	// review-shaped phase — it writes a sealed durable review record like the
	// review gate — and it must be the LAST phase of the sequence, because the
	// driver repeats the fix→verify pair after it until a round finds no open
	// MUST or the round cap parks the task.
	VerifyPhaseName = "verify"
)

// ReviewRoundCap is the hard round cap per review cycle (B8): a human verdict
// starts a new cycle, a takeover or reclaim does not, and a void round does not
// count. The plane derives the count from its non-void records and serves it as
// `reviews.counted`; the driver refuses to spawn another fix after the third
// round. It is a constant rather than a config knob on purpose: "never one more
// round" is the stopping rule, not a tuning dial, and a config value of 0 would
// be a sequence with no bound at all.
const ReviewRoundCap = 3

// Config is the resolved loop configuration. The comments here are the source of
// truth for each knob until the README/docs catch up.
type Config struct {
	// Harness selects the coding-agent CLI to drive (e.g. "claude", "codex",
	// "opencode"). Validated against the harness registry (harness.Known), so the
	// accepted set is exactly what is registered — see Validate.
	Harness string `json:"harness"`

	// Model is a harness-specific model alias to pin (e.g. "opus"). Empty = the
	// harness default. It is the run-wide default, and the fallback every tier
	// resolves to when it names nothing — see Models and ModelForTier.
	Model string `json:"model"`

	// Models is the operator's own tier map: bucket name -> harness-specific model
	// alias, e.g. {"strong": "opus", "standard": "sonnet", "cheap": "haiku"}. A
	// phase names a BUCKET (Phase.Tier), never an alias.
	//
	// The indirection is the whole point. clankerbar drives three harnesses and
	// cannot know which models an operator has, nor which is cheaper — that would
	// need a price table, which goes stale silently and in the direction that
	// costs money. So it never learns what models exist; it only learns that the
	// operator bucketed them, and ships a policy over the buckets.
	//
	// Everything here is OPTIONAL and every path falls back rather than refusing:
	// no map, an unnamed bucket, or a bucket mapped to a blank string all resolve
	// to Model, and Model empty resolves to the harness default. A mistyped tier
	// therefore costs a line in the iteration log, not a stopped run.
	Models map[string]string `json:"models"`

	// Harnesses carries the per-harness invocation fields for a run whose phases
	// are not all on one harness (CLA-366): harness name -> HarnessConfig. Empty
	// on every single-harness config, which is every config written before this
	// existed.
	//
	// Keyed by harness NAME, deliberately, so it composes with the other
	// per-harness maps rather than nesting inside one of them — the budget's own
	// per-harness breakers are keyed the same way, and neither has to know about
	// the other.
	//
	// A block for the TOP-LEVEL harness is optional and its gaps are filled from
	// the run-wide twins (Model, Models, ConfigDir, MCPConfigPath, SettingsPath),
	// because those fields have always described that harness. A block for any
	// OTHER harness is required and is filled from nothing — see HarnessConfig
	// for why inheriting a harness dialect across harnesses is worse than an
	// empty field.
	Harnesses map[string]HarnessConfig `json:"harnesses"`

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
	// has: one session, asked Prompt. Two or more phases mean a task that reaches
	// its checkpoint and stops there is only HALF finished until the next session
	// runs, so switching it on before the sequence has been watched end to end
	// would turn a driver bug into tasks that silently never reach review. An
	// operator opts in per config, and the shipped briefs are one line each:
	//
	//	"phases": [{"name": "implement"}, {"name": "review"}, {"name": "fix"}]
	//
	// The three-phase shape is the shipped one: `review` with a phase after it is
	// the REVIEW-ONLY gate (it records findings and changes nothing), and `fix`
	// is the last phase and owns the PR plus in_review. A FINAL `review` keeps the
	// historical review-and-fix brief, so an existing two-phase config still
	// finishes its tasks; the mode is chosen by position, not by a second name.
	//
	// Naming a phase and leaving its prompt empty takes the built-in brief for
	// that name; set `prompt` to override it, and `max_turns` to cap it.
	//
	// Phases and Prompt are mutually exclusive — Validate refuses both, rather
	// than silently preferring one and leaving an operator to wonder which of
	// two prompts their sessions are actually being handed.
	Phases []Phase `json:"phases"`

	// Conflict is the supervisor's conflict-only session step (CLA-759): the
	// harness and model for the short session it starts when CLA-758's
	// deterministic close-out stands down on a CONFLICTING approved task. It is
	// NOT a phase — it runs outside any task's sequence, for one task the
	// supervisor picked, at most one at a time fleet-wide — so it is its own
	// block rather than another entry in `phases`.
	//
	// ABSENT/partial = the fleet's IMPLEMENT step's harness and model, which is
	// the documented default: resolving a conflict is implementation work that
	// wants the model already trusted with the task's code. See ConflictStep.
	//
	// A pointer so an unset block is omitted from Identity(), exactly as
	// ReviewPanel's `omitempty` keeps an untouched config's identity unchanged.
	Conflict *ConflictStep `json:"conflict,omitempty"`

	// ReviewPanel is the ordered list of independent reviewers a built-in review
	// phase expands into (CLA-639). There is NO default and NO preset: the CLI
	// cannot know which harnesses, providers or models an operator has, so it
	// never names one (decision a0b1373b).
	//
	// ABSENT = one reviewer, exactly as before this feature: the config's own
	// review phase runs once, on the harness/model the config already gives it,
	// with the historical brief, the historical `review-1` role and the legacy
	// single-review fix path — byte for byte.
	//
	// SET = those members, and only those: any number, the same model allowed
	// more than once (each entry is an independent run). A member's `name` is
	// only its log label; the reviewer role the durable record is keyed on is
	// positional (`review-1`..`review-N`, counted over the review-shaped phases),
	// so naming a member never changes which record it writes. The panel applies
	// to a BUILT-IN review phase (no prompt of its own) that is not the last
	// phase — the read-only gate. A final review phase (the historical
	// review-and-fix brief) never expands, and a review phase with its own prompt
	// owns its own contract; Validate refuses the combination rather than letting
	// the panel silently not apply.
	//
	// An explicitly EMPTY array (`"review_panel": []`) is a half-edited config
	// rather than an opt-out, and Validate refuses it — the opt-out is to remove
	// the key. Read through Config.EffectivePanel.
	//
	// The panel hangs off the review PHASE, not the sequence, because that is
	// what it multiplies: `phases` still reads [implement, review, fix], the
	// config still has one review, and the driver is what fans it out.
	//
	// INTERIM placement (B10, A7): the panel is local config because phases are,
	// until CLA-598 moves the sequence onto the plane and the state-machine
	// reshape (CLA-454) carries panel membership as plane phase state. Nothing
	// here should be read as the durable shape — the durable review RECORD is
	// the plane's (CLA-636); this is only which reviewers run.
	ReviewPanel []ReviewPanelMember `json:"review_panel,omitempty"`

	// ReviewPanelConcurrency caps how many of a review panel's members run AT
	// ONCE (CLA-697). The members are independent by design, so the default is
	// all of them: 0 (or an absent key) means "the whole panel", and a value
	// larger than the panel means the same. 1 is the historical behaviour — the
	// members run one after another in the task's own worktree — and is the
	// fallback for an operator who wants the old sequence, or whose machine or
	// provider cannot take N sessions at once.
	//
	// Each concurrent member gets its own DETACHED worktree at the reviewed tip
	// (created and removed by the driver), because members may run builds or
	// probes: one shared checkout would let two reviewers write the same file.
	// A member's detached worktree is discarded when it ends — there is nothing
	// to salvage from a review-only session, and the fix phase owns the real
	// worktree.
	//
	// Read through Config.EffectivePanelConcurrency. It only applies when
	// `review_panel` expands; a single reviewer never fans out.
	ReviewPanelConcurrency int `json:"review_panel_concurrency,omitempty"`

	// reviewPanelSet records that `review_panel` was an EXPLICIT key — written
	// by the config file at Load, or carried by a stored run-config document at
	// ApplyRunConfig — so Validate can tell two shapes apart that the slice
	// alone cannot: an operator who wrote `"review_panel": []` (a panel of none
	// — refused, because it is almost certainly a half-edited config) and a
	// config that never mentioned the key (nil, the single reviewer). An overlay
	// REPLACES the pipeline section, so it rewrites this flag from the
	// document's own key presence rather than inheriting the file's.
	reviewPanelSet bool

	// WorkDir is where the harness runs (its repo checkout). Empty = current dir.
	WorkDir string `json:"workdir"`

	// ProjectRoot marks every workdir in this file as a FLEET PROJECT ROOT
	// (CLA-678): `<machine root>/<project slug>`, holding that project's clones
	// (<project root>/<repo name>) and its worktrees directory
	// (<project root>/worktrees). The supervisor's workdir derivation sets it
	// on the config it materializes; set it by hand to adopt the layout without
	// the supervisor.
	//
	// It is what decides a session's permission SCOPE. With it, a session
	// starting in one of the project's checkouts reaches its whole project —
	// every repo under the project root and every worktree in it — and nothing
	// outside; without it, the scope stays the session's own checkout, exactly
	// as before. So a config whose workdir is a developer's multi-repo parent
	// (`~/dev`, say) must NOT set it: the flag is an assertion about the
	// layout, and granting a whole directory tree on a guess is the wrong
	// direction to be wrong in.
	//
	// `omitempty` is load-bearing: Config.Identity() hashes the marshalled
	// form (see ReviewPanel for the same care), so the field must NOT alter the
	// identity of a config that does not adopt the layout — an unset flag is
	// the legacy shape, and a daemon's beacon must not report a config change
	// just because this field exists. Set it, and the identity does change,
	// correctly: the config now means something different.
	ProjectRoot bool `json:"project_root,omitempty"`

	// CloneURLBase is the URL PREFIX the supervisor clones a project's missing
	// repos from: <base><owner>/<name>.git. Empty means the built-in
	// DefaultCloneURLBase (`https://github.com/`), so a config that never
	// mentions the key clones exactly as it always did.
	//
	// It exists because a derived fleet clone's origin is otherwise always
	// HTTPS, which makes a session's branch pushes depend on a GitHub token
	// riding in its environment (the `gh auth token` scoping on the clone).
	// An SSH prefix — `git@github-personal:` for an ssh host alias, or
	// `git@github.com:` — makes the clone authenticate with the key alone and
	// skips the token scoping entirely. Any other mirror prefix works too;
	// it is a prefix, not a GitHub-specific setting.
	//
	// The value is a PREFIX, so it has to end where the owner begins: a URL
	// base ends with `/` (`https://github.com/`, `file:///srv/mirrors/`) and
	// an scp-style ssh base ends with `:` (`git@github-personal:`). Anything
	// else is refused by Validate rather than concatenated into a broken URL.
	//
	// `omitempty` is load-bearing for the same reason as ProjectRoot: an unset
	// key must not change a daemon's identity, and a set one correctly does.
	CloneURLBase string `json:"clone_url_base,omitempty"`

	// ConfigDir pins the harness config dir (CLAUDE_CONFIG_DIR / CODEX_HOME) so a
	// headless session loads the same skills, plugins, and auth as the interactive
	// one. Empty inherits the ambient environment — declare it for an unattended
	// daemon / cron, whose bare environment would otherwise have none of them.
	ConfigDir string `json:"config_dir"`

	// MCPConfigPath points the harness at the clankerbar MCP server. Claude Code
	// takes it as --mcp-config (NOT auto-discovered in -p mode); Codex merges it
	// into config.toml [mcp_servers]. See the adapters.
	MCPConfigPath string `json:"mcp_config_path"`

	// InstanceName names this daemon on the console's Fleet page (CLA-466):
	// presence beacons are keyed per (project, instance name), so this is how two
	// daemons driving the same project stay distinguishable. Empty resolves to
	// the machine's hostname plus the config file's basename (see
	// ResolveInstanceName) - unique per running daemon, so co-located daemons
	// with distinct config files no longer collapse into one Fleet row (CLA-501).
	//
	// The plane caps the name at 100 characters — Validate refuses longer here,
	// so a too-long name fails loudly at startup instead of silently dropping
	// every beacon. The resolved default is truncated to the same cap.
	InstanceName string `json:"instance_name"`

	// MaxIterations stops the loop after N respawns. 0 = no iteration ceiling:
	// the loop runs until a STOP/HALT marker or a signal stops it (or a budget
	// ceiling is reached); an empty queue idle-polls rather than exiting — the
	// same falsehood-corrected shape CLA-290 applied to doctor's no-ceiling
	// detail, kept in lockstep because this comment is where the old lie
	// survived longest.
	MaxIterations int `json:"max_iterations"`

	// MaxTurns is the run-wide turn cap any phase inherits when it sets none of
	// its own (see Phase.MaxTurns for the resolution chain). It exists because
	// the cap used to be reachable ONLY through per-phase config, so an unphased
	// run — the default — had no cap at all: one measured session ran 1093 turns
	// / 285.9M tokens with nothing able to interrupt it (CLA-343). 0 = defer
	// upward to the built-in default; "uncapped" is not a value this config
	// accepts.
	MaxTurns int `json:"max_turns"`

	// MaxSessionWallClock is the run-wide per-SESSION wall-clock cap any phase
	// inherits when it sets none of its own (see Phase.MaxWallClock). It is the
	// turn cap's stand-in for a harness whose CLI takes no turn flag: under
	// opencode, Invocation.MaxTurns reaches nothing, so without this a session
	// that works past its brief has no backstop at all and the CLA-314 salvage —
	// which assumes a phase CAN be cut off — never gets its chance.
	//
	// 0 = OFF, and that is the default: see Phase.MaxWallClock for why this dial
	// alone among the backstops ships without a built-in number.
	//
	// Measured per SESSION, never across the run: Budget.MaxWallClock is the
	// run-wide elapsed ceiling and counts the hours a run spends WAITING OUT a
	// usage limit, which is exactly why it cannot double as a runaway detector.
	// A session's own clock starts when its process does.
	MaxSessionWallClock Duration `json:"max_session_wall_clock"`

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

	// MaxZeroSpendAttempts bounds consecutive attempts within one drain that
	// ended without the harness reporting any usage at all. 0 = the built-in
	// default (DefaultMaxZeroSpendAttempts).
	//
	// It is the ceiling's blind spot made bounded. A spend ceiling can only stop
	// spend it is told about, and an attempt that dies before its harness reports
	// usage tells it nothing - so under `max_retries: 0` and a token- or
	// cost-only budget, the retry ladder is: attempt, zero spend, back off at
	// retry_cap, attempt, forever, with only max_wall_clock able to end it
	// (CLA-288). An attempt that DOES report resets the count, including one that
	// honestly reports zero, so this never counts a session that merely did
	// nothing.
	MaxZeroSpendAttempts int `json:"max_zero_spend_attempts"`

	// MaxRepeatedToolFailures ends a session whose SAME tool returns the SAME
	// error on this many consecutive tool calls (CLA-739). The harness kills the
	// session mid-stream, and the driver classifies it as dead with a cause
	// naming the tool, the count and the error. Unset = the built-in default
	// (DefaultMaxRepeatedToolFailures); 0 turns the check off; negative is
	// refused.
	//
	// A POINTER, unlike max_zero_spend_attempts, because 0 here is a real
	// setting (off) and not "use the default", so unset has to be a third state.
	// And `omitempty`, because the config's marshalled form is its identity (see
	// EchoSessions): adding the knob must not change an existing daemon's.
	//
	// Enforced by the harnesses that observe tool results as they stream -
	// claude and opencode. Inert under codex.
	MaxRepeatedToolFailures *int `json:"max_repeated_tool_failures,omitempty"`

	// EchoSessions mirrors each session's output stream to the daemon's own
	// stderr while it runs, as well as to its iteration log. Default FALSE
	// (CLA-701): the stream is the whole harness transcript — opencode emits
	// one JSON event per line, many carrying tool output inline — so echoing it
	// buries the daemon's own lines (spawn, exit, classification, why the run
	// ended) under thousands of rows of scrollback, and the iteration log keeps
	// it in full anyway. Turn it on when watching one session live is worth
	// more than the terminal being readable: `"echo_sessions": true`.
	//
	// `omitempty` is load-bearing for the same reason as ProjectRoot: the
	// config's marshalled form is its identity, and the default must not change
	// a daemon's identity merely because this field now exists.
	EchoSessions bool `json:"echo_sessions,omitempty"`

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

	// Repos maps the repos a project's tasks may name to their local checkouts,
	// so a session starts in the TASK's repo rather than in the multi-repo parent
	// (agent-rule-scoping pieces 2 and 3). Keyed by repo identity — either the
	// full "owner/name" form the task carries or just its bare name — with the
	// checkout path as the value (~ and relative paths both expand; a relative
	// path resolves against the workdir). Resolution order, fallbacks and the
	// loud repo_not_found failure are ResolveCheckout's, which is the only thing
	// that should interpret this map.
	//
	// Declaring repos also widens each session's permission policy to cover every
	// declared checkout regardless of cwd, so a two-repo project does not wall
	// sessions started in one repo off the other. The grants themselves stay in
	// local config — this map holds paths, never credentials.
	//
	// In single-project mode set it here; in multi-project mode set it per
	// project (see Project.Repos), whose non-empty map replaces the top-level
	// one for that project.
	Repos map[string]string `json:"repos"`

	// PrimaryRepo names the repo a session starts in when its task carries none:
	// the fallback that keeps a fresh phase out of the multi-repo parent until
	// the plane returns a repo for every task. It names a `repos` key, or any
	// identity resolvable by the same steps; with exactly one repo declared and
	// no primary named, that one is primary implicitly. Empty = no fallback, so
	// a task without a repo fails the iteration with repo_not_found rather than
	// start somewhere unconsidered — unless NO repos are configured at all, in
	// which case this whole feature is off and the legacy workdir behaviour runs.
	PrimaryRepo string `json:"primary_repo"`

	// Escalation holds the mechanical review-tier escalation rules (CLA-379).
	// Config-owned: a glob-to-tier mapping (path_rules) and a category-to-tier
	// mapping (category_rules). The CLI evaluates them at review-spawn time; the
	// plane holds only the execution-config reference to them. Empty = no
	// escalation — the review phase keeps its configured tier unchanged.
	Escalation Escalation `json:"escalation"`

	// AllowUncheckedPR opts this project out of CLA-310's empty-check-rollup
	// refusal: a delivery whose PR carries NO checks is logged as a WARNING
	// instead of refused. It exists for repos with NO CI at all, where refusing
	// every delivery would wedge the driver shut; both clankerbar repos have
	// CI, so for them the default (refuse) costs nothing.
	//
	// The mergeability half of the gate is NEVER relaxed by it: a CONFLICTING
	// PR is refused either way, because a conflicted PR is exactly the state
	// that produces an empty rollup, and silence-reads-as-pass is the bug the
	// gate exists to kill. The safe state is the default; the loose state is
	// this visible, operator-owned line, which `doctor` reports so it is seen
	// before it fires. In single-project mode set it here; in multi-project
	// mode set it per project (see Project.AllowUncheckedPR), which overrides
	// the top-level value for that project only.
	AllowUncheckedPR bool `json:"allow_unchecked_pr"`

	// HealthURL is the deployment health endpoint (`/health`) doctor's deploy_lag
	// check reads to learn WHICH commit is running out there - CLA-322. The plane
	// stamps version.commit at BUILD time, so "is commit X live?" is one request
	// and a string compare; this URL names where to ask. It is an operator-set
	// value with no derived default, because a deployment can live anywhere
	// (staging, production, a self-hosted plane) and deriving it from backlog_url
	// would answer about whichever environment the guess happened to hit. Empty =
	// not monitored: the check says so once, quietly, and moves on.
	//
	// The endpoint is read WITHOUT credentials - /health is public - so unlike
	// backlog_url it is held only to "parses as an absolute URL", not to the
	// bearer-token TLS floor.
	HealthURL string `json:"health_url"`

	// IntegrationBranch is the branch deployments are built from and promoted
	// along - the branch whose tip a healthy deployed build should be an ancestor
	// of. Doctor's deploy_lag check compares /health's version.commit against its
	// REMOTE tip. Default: staging (the convention both of this project's repos
	// use); override where your project integrates elsewhere. It deliberately does
	// NOT come from delivery.Claim.IntegrationBranch: that is a per-session
	// declaration of where one commit landed, while this is standing per-project
	// configuration about how releases flow.
	//
	// It is NOT what the phase briefs call the PR base (CLA-608): a repo can
	// deploy from one branch while its PRs target another (lecstor/makespdf
	// deploys staging but merges task branches into integration), and a repo in
	// a driven project can have no integration branch at all (the makesPDF
	// plugin repos carry only main). See PRBase for the override that names a
	// PR base explicitly; without one, each repo's PR base is resolved against
	// the origin remote at phase-spawn time.
	IntegrationBranch string `json:"integration_branch"`

	// PRBase maps a repo identity ("owner/name", or its bare name) to the branch
	// that repo's PRs TARGET - the branch a task branch forks from and merges
	// back into, substituted into the phase briefs as {{prBase}} (CLA-608).
	//
	// It exists because the briefs used to hard-code "(staging)" for every repo
	// of every project: the makesPDF plugins have no staging branch (their PRs
	// all target main), and lecstor/makespdf and lecstor/clankerbar merge into
	// integration - so a session was told to open its PR against a branch its
	// repo does not have, and the setting raised itself as a decision on every
	// task. An entry here wins outright, no probe: name the branch when the
	// convention is not derivable. Without an entry the driver resolves per
	// repo at phase-spawn time: the project's integration_branch when it
	// exists on the origin remote, else origin's default branch (see
	// loop.Driver.prBaseFor).
	//
	// This is deliberately NOT integration_branch renamed: that one keeps
	// meaning the branch whose tip doctor's deploy_lag check judges, so a repo
	// that deploys staging and merges integration needs both to stay truthful.
	PRBase map[string]string `json:"pr_base"`

	// AllowLocalMCPServers names the MCP server entries an operator means to run
	// from a DISCOVERED `<workdir>/.mcp.json` — the file Validate adopts by
	// default when mcp_config_path is empty. Any local-command entry whose name
	// is on this list is accepted from a discovered file; every other command
	// entry in one is refused (CLA-266: such an entry starts a process at session
	// init, before any permission rule applies, and a workdir default is not
	// where that decision belongs). It reaches nothing else: opencode-schema
	// policy keys (`permission`, `plugin`, `agent`) in a discovered file are
	// refused regardless of this list — naming servers never approves policy.
	//
	// The safe state is the default (refuse); the loose state is this visible,
	// operator-owned list, which `doctor` reports. A file the operator NAMES via
	// mcp_config_path needs no entry here — naming it IS the statement, and its
	// local servers are disclosed by doctor's WARN as they always were.
	//
	// In single-project mode set it here; in multi-project mode set it per
	// project (see Project.AllowLocalMCPServers) to replace the top-level list
	// for that project only.
	AllowLocalMCPServers []string `json:"allow_local_mcp_servers"`

	// Projects declares the backlogs a single loop instance drives — one entry per
	// clankerbar project (CLA-142: one account key, many queues). Empty = the
	// original single-project mode, driven by the top-level fields, exactly as
	// before. When set, the driver polls each project's
	// `/api/projects/<slug>/backlog-summary` and spawns each drain session in that
	// project's workdir, round-robin over whichever queues have claimable work.
	// The account key in CLANKERBAR_API_KEY covers every project the operator is a
	// member of; per-project keys are never needed.
	Projects []Project `json:"projects"`

	// Env is extra environment for the spawned harness process. Each entry's
	// value is either a literal string or an object {"fromCommand": "..."} whose
	// stdout becomes the value, resolved FRESH AT EVERY SPAWN so a rotated token
	// reaches the next session without a daemon restart (CLA-462). The child
	// already inherits the loop's own environment, so this is for the unattended
	// case: with GH_TOKEN declared here — per project if you drive several — the
	// cb-run wrapper stops being load-bearing and launching the binary directly
	// is safe.
	//
	// The map overlays at FOUR levels in most-specific-wins order (the same
	// precedence ResolveMCPConfig applies): this top-level map first, then
	// `harnesses.<name>.env`, then `projects[].env`, then that project's
	// `env_per_harness.<name>`. A value of the form "@path" is replaced by the
	// contents of that file (trimmed; a leading ~ is expanded) — keep a secret in
	// a 0600 file and point at it here rather than inlining it. That 0600 is
	// ENFORCED, not advice: a file any other local account can read refuses the
	// session spawn, as it always refused Validate.
	//
	// A fromCommand whose execution fails, times out (10s), or produces empty
	// output REFUSES the session spawn with a log line naming the variable —
	// fail closed, because a session missing its declared env is the incident,
	// not a degraded mode. `doctor` verifies every declared command resolves
	// before a run starts.
	Env EnvMap `json:"env"`

	// EnvAllowlist, when non-empty, is the COMPLETE set of inherited environment
	// variable NAMES a spawned session receives (CLA-714). Variables not named
	// here are dropped from the harness process, so the session cannot read the
	// tokens that happen to sit in the shell that started the supervisor.
	//
	// Unset is the default and keeps today's behaviour exactly: the harness
	// inherits the daemon's whole environment. An explicitly EMPTY list is
	// refused by Validate as a half-edit, so the only way to say "everything" is
	// to omit the key. The list is about INHERITANCE only — the `env` maps above
	// are still added on top of it at every level, which is how GH_TOKEN and
	// CLAUDE_CODE_OAUTH_TOKEN arrive. A var may therefore be in the allowlist,
	// in an `env` block, or in both; a `fromCommand` may read a variable that is
	// NOT in the allowlist, because it runs in the daemon, before the filter.
	//
	// It composes at the same four levels as `env` (top level, harness, project,
	// project-per-harness) but as a WHOLE LIST, not per key: the most specific
	// level that declares one wins entirely. Restate a broader list where you
	// narrow it; an intersection would make a level's own entry silently
	// ineffective. `doctor` FAILS when the list in force omits a variable the
	// fleet needs (HOME, PATH, CLANKERBAR_API_KEY, and CLANKERBAR_WORKDIR_ROOT
	// when the workdir derivation is on), because a session missing one of those
	// fails later and elsewhere.
	//
	// `omitempty` is load-bearing for the same reason as ProjectRoot: an unset
	// key must not change a daemon's identity, and a set one correctly does.
	EnvAllowlist []string `json:"env_allowlist,omitempty"`

	source string // path the config was loaded from, for diagnostics

	// harnessFromFlag / modelFromFlag record that --harness / --model overrode
	// the file. Both flags say "the run has ONE harness and this is it", which a
	// mixed-harness config contradicts: --harness would re-point which harness
	// SessionFor treats as top-level, so the other one would start inheriting the
	// run-wide claude fields it must never see, and --model would silently apply
	// to one phase's sessions and not the rest. Validate refuses rather than
	// resolving either; see there.
	harnessFromFlag bool
	modelFromFlag   bool

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

	// MCPConfigPaths is this project's MCP config PER HARNESS (harness name ->
	// path), for a run whose phases are not all on one harness (CLA-366). Empty
	// on every single-harness config.
	//
	// It exists because the two axes multiply. MCPConfigPath above selects the
	// PROJECT (its `/mcp/<slug>` URL is what the poll and the sessions agree on),
	// while the file's SCHEMA belongs to a harness — opencode refuses to start on
	// Claude's `mcpServers`. One file cannot be both for two harnesses, so a
	// multi-project mixed-harness run needs one per pair, and Validate refuses the
	// combination rather than letting every project's opencode phase quietly share
	// whichever single file `harnesses.<name>.mcp_config_path` named — which would
	// poll one queue and work another, the split-brain the slug check below exists
	// to prevent.
	//
	// Each entry is held to the same origin and slug checks as MCPConfigPath.
	MCPConfigPaths map[string]string `json:"mcp_config_paths"`

	// Env is this project's extra session environment, overlaid on top of the
	// top-level env map for THIS project's sessions only — see Config.Env for
	// what an entry holds. This is where a project's GH_TOKEN belongs when one
	// daemon drives several backlogs: each project declares its own source, so
	// no session ever carries another project's credential.
	Env EnvMap `json:"env"`

	// EnvPerHarness is this project's env PER HARNESS (harness name -> env map),
	// the most specific layer of the four-level overlay: it wins per key over
	// projects[].env, which wins over `harnesses.<name>.env`, which wins over
	// the top level. Empty on every config that needs no per-pair override.
	EnvPerHarness map[string]EnvMap `json:"env_per_harness"`

	// EnvAllowlist is this project's inherited-env allowlist, replacing the
	// top-level (and any harness-level) one wholly for this project's sessions —
	// see Config.EnvAllowlist. It is how one daemon driving several backlogs
	// keeps a credential one project needs out of another project's sessions.
	EnvAllowlist []string `json:"env_allowlist,omitempty"`

	// EnvAllowlistPerHarness is this project's allowlist PER HARNESS, the most
	// specific level of the four: it wins over EnvAllowlist for sessions on that
	// harness only. Empty on every config that needs no per-pair override.
	EnvAllowlistPerHarness map[string][]string `json:"env_allowlist_per_harness,omitempty"`

	// AllowUncheckedPR is this project's CLA-310 opt-out, overriding the
	// top-level field of the same name for this project only. See
	// Config.AllowUncheckedPR for what it does and why the default refuses.
	AllowUncheckedPR bool `json:"allow_unchecked_pr"`

	// HealthURL is this project's deployment health endpoint (`/health`),
	// overriding the top-level field of the same name for this project only.
	// See Config.HealthURL. Each project is a distinct backlog with its own
	// deployment, so in multi-project mode each one names where ITS plane
	// answers.
	HealthURL string `json:"health_url"`

	// IntegrationBranch is this project's integration branch, overriding the
	// top-level field of the same name for this project only. See
	// Config.IntegrationBranch for what it means; the default is staging.
	IntegrationBranch string `json:"integration_branch"`

	// PRBase is this project's repo -> PR-base-branch map, REPLACING the
	// top-level map of the same name for this project when it declares
	// anything; an entry that sets none inherits the top-level one. See
	// Config.PRBase for what the map holds and how an unmatched repo resolves.
	PRBase map[string]string `json:"pr_base"`

	// AllowLocalMCPServers is this project's CLA-266 allowlist, REPLACING the
	// top-level list of the same name for this project's discovered MCP config
	// when it names anything; an entry that sets none inherits the top-level
	// one. See Config.AllowLocalMCPServers and AllowLocalMCPServersFor.
	AllowLocalMCPServers []string `json:"allow_local_mcp_servers"`

	// Repos is this project's repo -> checkout map, REPLACING the top-level map
	// of the same name for this project when it declares anything; an entry that
	// sets none inherits the top-level one. See Config.Repos and ReposFor for
	// what the map does and how it resolves.
	Repos map[string]string `json:"repos"`

	// PrimaryRepo is this project's no-repo-on-the-task fallback, overriding the
	// top-level field of the same name for this project only when non-empty. See
	// Config.PrimaryRepo and PrimaryRepoFor.
	PrimaryRepo string `json:"primary_repo"`
}
