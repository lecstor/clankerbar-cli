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
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	// lands even if the model works past its brief. 0 = defer upward: the
	// top-level Config.MaxTurns, then the built-in default (see
	// Config.EffectivePhases). It is never "uncapped" — CLA-343 measured the
	// unphased run that reading produced (1093 turns / 285.9M tokens, nothing
	// able to stop it), so the cap chain always resolves to a number.
	MaxTurns int `json:"max_turns"`

	// MaxWallClock caps this phase's SESSION by elapsed time (harness
	// permitting), as the backstop for a harness whose CLI takes no turn flag —
	// opencode is that harness today, where MaxTurns never reaches the process
	// and a phase boundary rests on the prompt alone. 0 = defer upward to
	// Config.MaxSessionWallClock; if that is unset too the cap is OFF.
	//
	// Unlike MaxTurns it has no built-in default, deliberately: turns are a
	// harness-independent unit of work, while a wall-clock number that would be a
	// runaway detector for one model/provider is a routine session for another,
	// and clankerbar cannot know which the operator has. A default here would cut
	// honest sessions off in the middle rather than catch a runaway.
	MaxWallClock Duration `json:"max_wall_clock"`

	// Tier names a bucket in Config.Models — which of the operator's models this
	// phase's session runs on. Empty = the run-wide Model, and that empty = the
	// harness default, so an untouched config is untouched behaviour.
	//
	// It is a bucket NAME, never a model alias: putting "opus" here does not pin
	// opus, it looks for a bucket called "opus". Keeping the two apart is what
	// lets a tier policy be RECOMMENDED — see the README, where a phase producing
	// a durable artifact is worth the strong bucket — while the models themselves
	// stay the operator's, since clankerbar cannot know which ones they have or
	// what they cost.
	//
	// Nothing is tiered by DEFAULT. defaults() sets no phases at all and the
	// built-in briefs carry prompts only, so an untouched config runs every phase
	// on the run's model. The policy is advice in prose, never a shipped
	// assignment.
	Tier string `json:"tier"`

	// Harness selects which coding-agent CLI runs this phase. Empty = the
	// run-wide Config.Harness, so an untouched config runs every phase on one
	// harness exactly as before.
	//
	// It exists because the phases a task splits into are not equally demanding.
	// The implement phase can run on a cheap provider-agnostic backend; the
	// review phase runs the adversarial gate the repo's workflow mandates, which
	// wants the harness carrying the subagent machinery. Nothing in the phase seam
	// resists this: each phase is already a FRESH session, seeded from the
	// observed claim through the {{taskId}}/{{runId}} placeholders and
	// Invocation.ResumeClaim, so there is no session state to carry across a
	// harness boundary — only a claim, and the claim lives on the plane.
	//
	// What a mixed sequence DOES change is that the harness-shaped fields stop
	// being run-wide: a claude `config_dir` handed to opencode, or a claude model
	// alias handed to opencode's `--model`, is a session that dies at startup.
	// That is what Config.Harnesses is for, and why Validate refuses a phase
	// naming a harness with no block of its own.
	Harness string `json:"harness"`
}

// HarnessConfig is one harness's own invocation fields, for a run whose phases
// are not all on the same harness (CLA-366).
//
// Every field here has a run-wide twin on Config, and the twins are NOT shared
// across harnesses: they describe the top-level harness alone. That is the whole
// reason this type exists rather than a deeper fallback chain. The fields are
// harness dialects wearing generic names — `config_dir` is CLAUDE_CONFIG_DIR or
// OPENCODE_CONFIG_DIR or CODEX_HOME, `mcp_config_path` is a Claude-shaped
// `.mcp.json` for one adapter and an opencode-schema config for another (see
// harness.MCPConfigUse, which records that opencode REFUSES TO START on the
// Claude-shaped file), `model` is an alias only one provider has. So "inherit
// whatever the top level said" is not a helpful default but a guaranteed startup
// failure with the operator's own config as the cause. Better an empty field,
// which the adapter falls back on and `doctor` can talk about.
type HarnessConfig struct {
	// Model is this harness's default model alias, and the fallback its tiers
	// resolve to. Empty = the harness's own default.
	Model string `json:"model"`

	// Models is this harness's tier map (bucket name -> model alias), the
	// per-harness twin of Config.Models. A phase's Tier resolves against the map
	// belonging to the harness that phase runs on, because a bucket NAME is
	// portable across harnesses and the alias inside it is not — which is what
	// lets one `"tier": "strong"` mean opus here and something else there.
	Models map[string]string `json:"models"`

	// ConfigDir pins this harness's config dir (CLAUDE_CONFIG_DIR /
	// OPENCODE_CONFIG_DIR / CODEX_HOME) so a headless session loads the same
	// skills, plugins and auth as the interactive one.
	ConfigDir string `json:"config_dir"`

	// MCPConfigPath points this harness's sessions at the clankerbar MCP server,
	// in THAT harness's schema. Held to the same trusted-origin check as the
	// top-level field, because it points the same account key at a host.
	MCPConfigPath string `json:"mcp_config_path"`

	// SettingsPath is the extra settings file (Claude Code's --settings) carrying
	// the headless permission policy. Claude-specific; other adapters ignore it.
	SettingsPath string `json:"settings_path"`
}

// Empty reports whether this block declares nothing at all — the shape that is
// indistinguishable from having no block, and which Validate refuses for the
// same reason. Written out rather than compared against the zero value because
// the Models map makes the struct incomparable.
func (h HarnessConfig) Empty() bool {
	return h.Model == "" && len(h.Models) == 0 && h.ConfigDir == "" && h.MCPConfigPath == "" && h.SettingsPath == ""
}

// HandoffMarker is the exact line a session puts in its FINAL message to hand
// the rest of its job to a fresh successor session (CLA-352). Everything after
// the marker line is the successor's prompt; the driver respawns on it with the
// same run-continuity rules as a phase resume.
//
// The syntax had to be improbable in ordinary prose and easy to pin in a test,
// and it must not resemble tool-call framing markup: the web repo's
// docs/text-integrity.md records agents destroying their own output merely by
// EMITTING such tags, so an angle-bracket or tag-shaped marker would put the
// defect this feature rides on into the feature itself. A plain punctuated
// uppercase line does neither.
const HandoffMarker = "=====CLANKERBAR HANDOFF====="

// HandoffPromptMax bounds the successor prompt a handoff block may carry, in
// bytes. A session-authored respawn prompt is self-directed prompt injection,
// and the size cap is one of the non-negotiable guards on it: an over-cap
// prompt is refused (logged, normal path) rather than truncated, because a
// truncated brief is a successor confidently working half an instruction. "A
// few KB" is the spec; a genuine continuation prompt — decisions made, state
// verified, exact next steps — fits in far less.
const HandoffPromptMax = 4096

// HandoffPreamble is driver-authored framing prepended to a handoff respawn's
// prompt (CLA-352) — the successor never gets the emitting session's own
// prompt bare. A phase resume (reviewPhaseName, above) works only because its
// brief spells out "do not claim, call heartbeat first": a session-authored
// prompt cannot be trusted to carry that instruction forward on its own, and
// without it a successor calls next_task/claim_task on a fresh task, or its
// run's lease lapses because nothing tells it to hold it. Placed FIRST, not
// appended, so the driver's own constraints bound the prompt that follows
// rather than being one more thing a self-authored prompt could bury or
// contradict — the same reasoning behind refusing to truncate an over-cap
// prompt rather than editing it.
const HandoffPreamble = "You are resuming run " + PhaseRunPlaceholder + " on task " + PhaseTaskPlaceholder +
	", handed to you by the previous session on this task via a self-chosen handoff. Do NOT call next_task, and " +
	"do NOT claim anything: call heartbeat(\"" + PhaseRunPlaceholder + "\") first to resume the run, then stay " +
	"within this phase's scope. What follows is the previous session's own continuation prompt:\n\n"

// reviewTerminalStep is the review phase's LAST instruction, kept as a constant so
// its position can be pinned by a test and not only its wording.
//
// Position is the property that failed (CLA-384). The old brief said "Then push,
// and hand the task to in_review" - the instruction was PRESENT, as a trailing
// clause on a long sentence about scoping the follow-up re-verification. Three of
// four review phases in one evening did the work and ended without it. The
// implement brief, whose equivalent step is emphatic, names its call and its
// arguments, and comes last, did not fail once over the same evening. So this is
// written in that shape and pinned to that place.
//
// CLA-353: the terminal sequence is push, then open a PR, then update_task - in
// that order, and now all three live here rather than "push" sitting one sentence
// earlier. An overnight forensic drain found `gh pr create` appearing ZERO times
// across four review-phase transcripts, and two tasks committed straight onto
// `staging` because a session's cwd was already sitting there and nothing told it
// that was wrong - so a task closed at in_review with no PR, and the close-out
// flow's "merge its PR into staging" had nothing to merge. CLA-384 made
// update_task the phase's emphatic final call, which if the PR step were bolted on
// earlier would make it LESS likely to survive, not more - a step mentioned before
// an emphatic final call loses to it. So the PR step is folded into this same
// terminal block, before the update_task sentence, not left in the paragraph above.
const reviewTerminalStep = " Then COMMIT and PUSH the fixes. Open a PR targeting the repo's integration " +
	"branch (staging) if none exists yet for this branch - push, then the PR, in that order, before the " +
	"hand-off below. FINALLY, and this is the step that ENDS the phase: hand the task over with " +
	"update_task(taskId, runId, status: \"in_review\", outcome: ...), where the outcome MUST carry a " +
	"**Tests** section saying what you actually verified - without one the plane REFUSES the call, so a " +
	"session that leaves it out has not handed anything over. Ending this session while you still hold the " +
	"task is this phase FAILING, not finishing: the work you pushed then gets rediscovered and paid for a " +
	"second time by whoever takes it over. The ONLY exception is a declared handoff."

// HandoffContinuation is appended, by the driver, to a handoff respawn's prompt
// (CLA-353) so a phase's own terminal step survives a session-authored hand-off.
// HandoffPreamble carries the "resume, don't claim" contract forward on every
// handoff; the ORIGINAL phase brief's own instructions do not — a handoff
// respawn's prompt is HandoffPreamble plus the predecessor's self-authored
// nextPrompt alone, so anything the built-in brief said is otherwise gone the
// moment a session hands off instead of finishing itself. For the review phase
// that includes reviewTerminalStep - the PR-then-update_task sequence CLA-353
// exists to make land - so a handoff mid-review must not be a way to lose it.
// Empty for a phase with no such step: the implement phase stops rather than
// hands its task to in_review, so it has none to carry forward.
func HandoffContinuation(phaseName string) string {
	if phaseName == reviewPhaseName {
		return "\n\nThe phase's terminal step is unchanged by handing off:" + reviewTerminalStep
	}
	return ""
}

// handoffGuidance rides on every built-in phase brief (CLA-352): when a session
// may hand the rest of its job to a fresh successor, and how. The trigger is
// deliberately EVENT-shaped — a session cannot measure its own context (there
// is no token counter in its view), so "hand off when you feel big" is not
// implementable; a pivot is an event the session can recognise.
const handoffGuidance = " HANDOFF (most tasks need zero): if you reach a genuine pivot mid-session - " +
	"exploration finished and implementation about to start, or one distinct sub-goal landed and the next " +
	"beginning - you may hand the REST of this session's job to a fresh successor session that starts without " +
	"your accumulated context. First put durable state where it belongs: discovered constraints in repo docs, " +
	"commit/branch state on the task record. Then end your FINAL message with a line that is exactly " +
	HandoffMarker + " followed by the successor's starting prompt, written the way an operator's continuation " +
	"prompt reads: decisions made, state verified, exact next steps - nothing about the journey, nothing that " +
	"already lives in the repo or on the task, and under 4KB. The successor resumes this same run under this " +
	"same brief's scope, so do NOT settle, release or hand back the task first."

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
		"Ending there is this task going to plan, not the task being abandoned." + handoffGuidance,

	reviewPhaseName: "You are PHASE 2 of 2 on task " + PhaseTaskPlaceholder + ", which an earlier session has already " +
		"implemented, committed and pushed. You are RESUMING that run, not starting a new one: do not call " +
		"next_task, and do not claim anything. Call heartbeat(\"" + PhaseRunPlaceholder + "\") to resume the run, " +
		"then get_task with includeDecisions: true to re-read the bar and the standing decisions. An empty branch " +
		"field on the task is a FAILED hand-off to report, not work to silently adopt and implement. Work in the " +
		"worktree for the branch recorded on the task, and never commit to the integration branch (staging) - a " +
		"session whose cwd is already the main checkout sitting on staging is not a decision to commit where you " +
		"are, it is this failure mode. Read the diff on that branch. Then run the adversarial review gate, fix " +
		"what it finds, and re-verify SCOPED to those fixes: brief the follow-up reviewer with the findings you " +
		"fixed, by name, and point it at the fix commits (or, if not yet committed, the fix diff) and the " +
		"regression surface they touch - not at the whole diff, whose full pass already happened. A full second " +
		"pass is the exception you state a reason for (a fix that had to reach outside its own area), never the " +
		"default." + reviewTerminalStep + handoffGuidance,
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
}

// Budget is the "leave headroom / don't run away" circuit breaker. No harness
// exposes headless quota introspection (see the design memo), so this is a
// self-accounted, operator-tuned cap — simple and reliable over
// percentage-accurate, by design. Zero-valued fields are disabled.
type Budget struct {
	MaxTokens    int      `json:"max_tokens"`     // cumulative tokens across iterations
	MaxCostUSD   float64  `json:"max_cost_usd"`   // cumulative $ across iterations
	MaxWallClock Duration `json:"max_wall_clock"` // stop after this much elapsed

	// MaxSessionTokens kills a single SESSION in flight when its own cumulative
	// usage crosses this ceiling — the runaway detector, distinct from the
	// run-wide breaker above, which only checks BETWEEN sessions and so cannot
	// see a single huge session coming (CLA-343: the 285.9M runaway was 3.8x
	// its run's whole 75M ceiling). 0 = defer to SessionTokenCeiling's default.
	MaxSessionTokens int `json:"max_session_tokens"`

	// PerHarness are optional per-harness spend ceilings, keyed by harness name,
	// each counted over ONLY that harness's own sessions (CLA-367).
	//
	// The dials above are one ceiling over every session a run spends, which is
	// the right shape while a run drives one harness and the wrong one the moment
	// it drives two. `max_tokens` is calibrated against a subscription plan — 75M
	// is a sane week of Claude and roughly $2 on a DeepSeek-class backend, which
	// would end a drain after a task or two. `max_cost_usd` is the meaningful dial
	// for a metered backend and a meaningless one for a session billed to a
	// subscription, which reports a price nobody is charged (CLA-289). Neither
	// dial can hold a number that means the same thing on both sides, so a
	// mixed-harness run gets one block per harness, each measured in the unit that
	// harness understands.
	//
	// Any block that trips stops the WHOLE run. These are circuit breakers, not
	// per-harness quotas to be juggled: a run that carried on with one harness
	// switched off would be a shape nobody configured, and the phase that needed
	// it would fail every iteration.
	//
	// The dials above keep working exactly as they always have, over the run's
	// whole spend, and a config setting none of these behaves byte for byte as
	// before. Set both and both apply — the global ceiling over everything, the
	// per-harness one over its own sessions, whichever is reached first.
	//
	// Wall clock is deliberately absent: it measures the run, not a harness, and a
	// run has one clock however many harnesses it spends it on. So is
	// max_session_tokens, which is a runaway detector on a single session rather
	// than an accumulating ceiling.
	PerHarness map[string]HarnessBudget `json:"per_harness"`
}

// HarnessBudget is one harness's own spend ceiling inside a run's Budget — see
// Budget.PerHarness for why a mixed-harness run needs one per side. Zero-valued
// fields are disabled, as in Budget.
type HarnessBudget struct {
	MaxTokens  int     `json:"max_tokens"`   // cumulative tokens across THIS harness's sessions
	MaxCostUSD float64 `json:"max_cost_usd"` // cumulative $ across THIS harness's sessions
}

// CountsSpend reports whether this block bounds spend at all — the question
// Budget.CountsSpend answers, asked of one harness's block.
func (h HarnessBudget) CountsSpend() bool { return h.MaxTokens > 0 || h.MaxCostUSD > 0 }

// ExceededBy names the dimension of this block that tripped, or "" if none has.
//
// The harness name leads the string for the reason Budget.ExceededBy names the
// dimension at all: in a run carrying two ceilings, "cost $2.05 ≥ $2.00" does not
// say which side stopped it.
func (h HarnessBudget) ExceededBy(harness string, tokens int, costUSD float64) string {
	switch {
	case h.MaxTokens > 0 && tokens >= h.MaxTokens:
		return fmt.Sprintf("%s tokens %d ≥ %d", harness, tokens, h.MaxTokens)
	case h.MaxCostUSD > 0 && costUSD >= h.MaxCostUSD:
		return fmt.Sprintf("%s cost $%.2f ≥ $%.2f", harness, costUSD, h.MaxCostUSD)
	}
	return ""
}

// ForHarness is the block configured for a harness, and whether there is one. A
// missing block is the zero HarnessBudget, disabled in every dimension, so a
// caller that does not care which it got may use the value and drop the flag.
func (b Budget) ForHarness(name string) (HarnessBudget, bool) {
	hb, ok := b.PerHarness[name]
	return hb, ok
}

// ExceededByHarness names the per-harness dimension that tripped for one
// harness's own accumulated spend, or "" if none has. It consults ONLY that
// harness's block — the dials above are Budget.ExceededBy's business, and a
// caller enforcing both asks both.
func (b Budget) ExceededByHarness(name string, tokens int, costUSD float64) string {
	hb, ok := b.PerHarness[name]
	if !ok {
		return ""
	}
	return hb.ExceededBy(name, tokens, costUSD)
}

// CountsSpendFor reports whether a session run on this harness is under a spend
// ceiling — its own block's, or the run-wide one.
//
// This is the question CLA-262's side effects turn on (see CountsSpend): a
// session whose spend cannot be measured breaks a promise only where a promise
// was made. In a mixed-harness run the promise follows the harness whose breaker
// is set, so an unreadable opencode session stops a run carrying an opencode
// block while an unreadable claude session does not — unless a global dial covers
// them both, which is what every pre-CLA-367 config has.
func (b Budget) CountsSpendFor(name string) bool {
	if b.CountsSpend() {
		return true
	}
	hb, _ := b.ForHarness(name)
	return hb.CountsSpend()
}

// The per-session ceiling's defaults, in the order the resolution chain falls
// through (SessionTokenCeiling).
const (
	// sessionTokenCeilingMultiplier derives the ceiling from budget.max_tokens
	// when the operator set one. 2x is defensible because the runaway this
	// exists to stop ran 3.8x its run's ceiling: a detector at 2x catches that
	// class of runaway while sitting far above what a large task can plausibly
	// cost (~66M measured, CLA-309). It is a DETECTOR, deliberately not a
	// tighter budget.
	sessionTokenCeilingMultiplier = 2

	// sessionTokenFloor is the ceiling when the operator set no budget ceiling
	// at all. Anchors (CLA-343): the largest measured legitimate session was
	// 66.2M tokens (370 turns, CLA-309); the runaway was 285.9M. 150M is ~2.3x
	// the largest legitimate session and would have stopped the runaway at just
	// over half its spend.
	sessionTokenFloor = 150_000_000
)

// SessionTokenCeilingFor resolves the per-session runaway ceiling for a session
// on this harness: the operator's own dial, else 2x the run's max_tokens, else
// 2x that harness's own per_harness max_tokens, else the floor.
//
// The per-harness rung exists because CLA-367 tells an operator to move claude's
// token ceiling out of the global dial and into its own block — and without it,
// doing exactly that would silently LOOSEN the runaway detector the global dial
// was deriving: per_harness.claude.max_tokens=20M would give a 150M floor rather
// than the 40M the old shape gave, 7.5x the run's own ceiling. A detector that
// gets weaker when you follow the documented migration is the CLA-343 bug
// reappearing by another door.
//
// The global dial still wins over the per-harness one where both are set: it
// bounds the whole run, so it is the tighter promise about any one session.
func (b Budget) SessionTokenCeilingFor(harness string) int {
	if b.MaxSessionTokens > 0 {
		return b.MaxSessionTokens
	}
	if b.MaxTokens > 0 {
		return sessionTokenCeilingMultiplier * b.MaxTokens
	}
	if hb, ok := b.ForHarness(harness); ok && hb.MaxTokens > 0 {
		return sessionTokenCeilingMultiplier * hb.MaxTokens
	}
	return sessionTokenFloor
}

// SessionTokenCeiling resolves the per-session runaway ceiling: the operator's
// own dial, else 2x the run's max_tokens, else the floor. There is deliberately
// no "disabled": the whole point of CLA-343 is that nothing was able to stop the
// 285.9M session, so a ceiling that can be left unset by accident is the bug.
//
// This is the harness-blind form, kept for callers with no harness in hand; a
// caller that knows which harness the session runs on asks
// SessionTokenCeilingFor, which can also see a per-harness token ceiling.
func (b Budget) SessionTokenCeiling() int {
	if b.MaxSessionTokens > 0 {
		return b.MaxSessionTokens
	}
	if b.MaxTokens > 0 {
		return sessionTokenCeilingMultiplier * b.MaxTokens
	}
	return sessionTokenFloor
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
//
// Each phase's MaxTurns is resolved here too: the phase's own cap, else the
// top-level cap, else the built-in default. That resolution is what makes an
// unphased run bounded — before CLA-343 it fell through to the zero value, and
// one session ran 1093 turns / 285.9M tokens with no cap anywhere in the chain.
func (c *Config) EffectivePhases() []Phase {
	phases := c.Phases
	if len(phases) == 0 {
		phases = []Phase{{Prompt: c.Prompt}}
	}
	out := make([]Phase, len(phases))
	for i, ph := range phases {
		if ph.Prompt == "" {
			ph.Prompt = builtinPhasePrompts[ph.Name]
		}
		ph.MaxTurns = c.resolveMaxTurns(ph.MaxTurns)
		ph.MaxWallClock = c.resolveWallClock(ph.MaxWallClock)
		// Resolve the harness here too, so every phase the driver iterates NAMES
		// the harness it runs on and nothing downstream has to re-derive it from
		// the config. The synthesized single phase of an unphased run gets the
		// run-wide harness by the same rule, which is what it always ran on.
		ph.Harness = c.HarnessFor(ph)
		out[i] = ph
	}
	return out
}

// resolveMaxTurns is the turn-cap chain: the phase's own cap wins, then the
// top-level cap, then the built-in default. Nothing resolves to zero — see
// Phase.MaxTurns and DefaultMaxTurns for why "0 = uncapped" is gone.
func (c *Config) resolveMaxTurns(phase int) int {
	if phase > 0 {
		return phase
	}
	if c.MaxTurns > 0 {
		return c.MaxTurns
	}
	return DefaultMaxTurns
}

// resolveWallClock is the session wall-clock chain: the phase's own cap wins,
// then the run-wide one, then nothing. Unlike resolveMaxTurns this CAN resolve
// to zero, and zero means off — the dial ships disabled because no default
// number would be honest across harnesses (see Phase.MaxWallClock).
func (c *Config) resolveWallClock(phase Duration) Duration {
	if phase > 0 {
		return phase
	}
	return c.MaxSessionWallClock
}

// ZeroSpendAttemptBound is the effective bound on consecutive no-usage attempts:
// the operator's own, else the built-in default. Like the turn cap, nothing
// resolves to zero - the bound is a backstop, and "off" is not a value it takes.
func (c *Config) ZeroSpendAttemptBound() int {
	if c.MaxZeroSpendAttempts > 0 {
		return c.MaxZeroSpendAttempts
	}
	return DefaultMaxZeroSpendAttempts
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

// ModelForTier resolves one phase's tier to the model alias its session runs on.
//
// It is TOTAL — there is no input for which it fails — because the fallback chain
// is the design and not an error path: an unset tier, a tier the operator's map
// does not name, and a tier mapped to a blank string all resolve to the run-wide
// Model, and a blank Model resolves to "" (the harness's own default). That is
// what keeps a config written before tiers existed behaving exactly as it did.
//
// Everything is trimmed on the way through, which is the load-bearing half: a
// tier or a `"model": " "` that is blank-but-not-empty must resolve to the same
// nothing an absent one does, or a whitespace alias reaches the child's model
// flag and the session dies on an alias no provider has.
//
// `ok` is false only when a tier was NAMED and resolved to nothing — the typo
// case. The caller says so in the iteration log rather than refusing: a mistyped
// bucket should cost a log line and a run on the default model, not a stopped
// unattended run at 3am. It is deliberately NOT reported for an unset tier,
// which is the ordinary case and not a mistake.
func (c *Config) ModelForTier(tier string) (model string, ok bool) {
	return c.ModelForPhase(Phase{Tier: tier})
}

// HarnessFor names the harness one phase runs on: the phase's own, else the
// run-wide one. Total, and the answer for an unphased run is the run-wide
// harness, which is what every caller wants.
func (c *Config) HarnessFor(ph Phase) string {
	if h := strings.TrimSpace(ph.Harness); h != "" {
		return h
	}
	return c.Harness
}

// SessionFor resolves one harness's invocation fields.
//
// The run-wide fields fill the gaps for the TOP-LEVEL harness only. For any
// other harness the block stands alone: every field in it is a dialect of that
// harness (see HarnessConfig), so inheriting the top level's would hand a claude
// config dir and a Claude-shaped `.mcp.json` to opencode, which does not ignore
// the difference — it refuses to start. An unconfigured harness therefore
// resolves to empty fields, and each adapter falls back to its own ambient
// defaults, which is a session that may lack tools rather than one that dies on
// somebody else's config. Validate refuses the unconfigured case up front; this
// stays total so nothing downstream has a second error path.
func (c *Config) SessionFor(name string) HarnessConfig {
	hc := c.Harnesses[name]
	if name != c.Harness {
		return hc
	}
	if hc.Model == "" {
		hc.Model = c.Model
	}
	if hc.Models == nil {
		hc.Models = c.Models
	}
	if hc.ConfigDir == "" {
		hc.ConfigDir = c.ConfigDir
	}
	if hc.MCPConfigPath == "" {
		hc.MCPConfigPath = c.MCPConfigPath
	}
	if hc.SettingsPath == "" {
		hc.SettingsPath = c.SettingsPath
	}
	return hc
}

// ModelForPhase resolves one phase's tier to the model alias its session runs
// on, against the tier map of the harness THAT phase runs on.
//
// Same total fallback chain as the single-harness case it replaces — unset tier,
// unknown bucket, and a bucket mapped to blank all resolve to that harness's
// default model, and an empty default means the harness's own — with the
// per-harness lookup being the whole difference. A bucket name is portable and
// the alias in it is not, so "strong" on a claude phase and "strong" on an
// opencode phase are two different aliases under one policy word.
//
// `ok` is false only when a tier was NAMED and resolved to nothing, exactly as
// before: the caller logs it and runs on the default rather than stopping an
// unattended run over a typo. A phase on a harness with no tier map at all
// therefore reports the typo case for any tier it names, which is honest — the
// bucket really does resolve to nothing there.
func (c *Config) ModelForPhase(ph Phase) (model string, ok bool) {
	hc := c.SessionFor(c.HarnessFor(ph))
	dflt := strings.TrimSpace(hc.Model)
	t := strings.TrimSpace(ph.Tier)
	if t == "" {
		return dflt, true
	}
	if alias := strings.TrimSpace(hc.Models[t]); alias != "" {
		return alias, true
	}
	return dflt, false
}

// ResolveMCPConfig picks the MCP config file one session gets, given the harness
// it runs on and the project scope it runs in.
//
// The file carries TWO facts at once — which project (its `/mcp/<slug>` URL) and
// which schema (the harness's) — so the rule has to satisfy both, and it lives
// here because two callers must agree on it: the driver building an invocation
// and `doctor` reporting on what that invocation will be. A preflight that
// resolved this differently from the loop would certify a file no session ever
// reads, which is the exact failure MCPConfigUse was added to end.
//
// projectMCP / projectPerHarness are the project's own fields (empty and nil for
// a single-project run). Precedence: the project's file FOR THIS HARNESS, else
// the project's file when this is the top-level harness — it is that harness's
// schema and no other's — else the harness block's own path.
func (c *Config) ResolveMCPConfig(harnessName, projectMCP string, projectPerHarness map[string]string) string {
	if p := projectPerHarness[harnessName]; p != "" {
		return p
	}
	if harnessName == c.Harness && projectMCP != "" {
		return projectMCP
	}
	return c.SessionFor(harnessName).MCPConfigPath
}

// PhaseHarnesses lists every harness this config DECLARES — the run-wide one and
// every phase's — deduplicated, with the run-wide one first and the rest in
// sequence order.
//
// Callers are the ones that must reason about ALL of them rather than the
// top-level name alone: `doctor` checking binaries, config dirs and permission
// policy, and Validate checking that each is registered and configured. Reporting
// on `harness` alone is how a mixed-harness config earns a green preflight and
// then dies on its second phase.
//
// **Declared, not spawned** — and the difference matters, so pick deliberately.
// A declared harness must be USABLE whether or not today's phases reach it, which
// is why every caller above wants the run-wide one included. For the other
// question — what will this run actually do — use SpawnedHarnesses below, which
// omits a run-wide harness that every phase overrides. Asking this one there
// produces confidently false statements about a harness no session runs on.
func (c *Config) PhaseHarnesses() []string {
	out := []string{c.Harness}
	seen := map[string]bool{c.Harness: true}
	for _, ph := range c.Phases {
		h := c.HarnessFor(ph)
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// SpawnedHarnesses lists every harness this config will actually START A SESSION
// on: the run-wide one for an unphased run, and otherwise exactly the harnesses
// its phases resolve to. Deduplicated, in sequence order.
//
// It differs from PhaseHarnesses in one shape, and the difference is the whole
// reason it exists: when EVERY phase names its own harness, the run-wide
// `harness` field is DECLARED but never spawned - `Validate` does not refuse
// that, and the only two spawn sites in the driver (the phase's Invoke and its
// supervised-wait Probe) are both phase-driven. PhaseHarnesses seeds c.Harness
// unconditionally, which is right for the questions IT answers - is every
// declared harness registered and configured, is its binary on the PATH - since
// a declared harness must be usable whether or not today's phases reach it.
//
// It is wrong for any question of the form "what is true of the sessions this run
// will actually run", which is every inert-dial judgement in `doctor`'s budget
// check. Asked of PhaseHarnesses, those get three separate false statements out
// of a config whose phases all override: a live cost ceiling called INERT because
// a never-spawned codex is cost-blind; an unreachable per_harness block for that
// codex counted as a live ceiling; and an inert `max_session_wall_clock` passed
// over in silence because a never-spawned opencode would have honoured it.
//
// So: ask PhaseHarnesses what is CONFIGURED, and SpawnedHarnesses what will RUN.
func (c *Config) SpawnedHarnesses() []string {
	// No phases means the synthesized single phase of EffectivePhases, which runs
	// on the run-wide harness - so there the two are the same answer.
	if len(c.Phases) == 0 {
		return []string{c.Harness}
	}
	var out []string
	seen := map[string]bool{}
	for _, ph := range c.Phases {
		h := c.HarnessFor(ph)
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
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
		c.harnessFromFlag = true
	}
	if o.Model != "" {
		c.Model = o.Model
		c.modelFromFlag = true
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

	// Per-harness blocks get exactly the same path treatment as their run-wide
	// twins, and for the same reasons: a `~` that never expanded and a relative
	// path resolved against whatever cwd the process had are bugs that do not
	// care which block the field sat in. Written back through the map because
	// HarnessConfig is a value type.
	for name, hc := range c.Harnesses {
		hc.ConfigDir = underWorkDir(expandHome(hc.ConfigDir), c.WorkDir)
		hc.MCPConfigPath = underWorkDir(expandHome(hc.MCPConfigPath), c.WorkDir)
		hc.SettingsPath = underWorkDir(expandHome(hc.SettingsPath), c.WorkDir)
		c.Harnesses[name] = hc
	}

	// Validate against the harness registry (not a hand-kept switch) so the accepted
	// set can never drift from what is actually registered — an unregistered value is
	// rejected here before harness.Get is consulted, and a newly registered adapter is
	// accepted automatically. harness does not import config, so there is no cycle.
	if !harness.Known(c.Harness) {
		return fmt.Errorf("unknown harness %q (want: %s)", c.Harness, strings.Join(harness.Names(), ", "))
	}
	// A per-harness block keyed by a name no adapter answers to is a ceiling that
	// can never trip: nothing is ever charged to it, so the run is unbounded on
	// exactly the side the operator meant to bound. Refuse it here, against the
	// same registry as `harness` above, rather than at 3am with an inert breaker.
	// Names are checked in sorted order so the message does not depend on map
	// iteration. A negative ceiling INSIDE a block is doctor's finding, not this
	// one, exactly as for the dials beside it — see checkBudget.
	for _, name := range slices.Sorted(maps.Keys(c.Budget.PerHarness)) {
		if !harness.Known(name) {
			return fmt.Errorf("budget.per_harness[%q]: unknown harness (want: %s)", name, strings.Join(harness.Names(), ", "))
		}
	}
	if c.MaxTurns < 0 {
		return errors.New("max_turns is negative")
	}
	if c.Budget.MaxSessionTokens < 0 {
		return errors.New("max_session_tokens is negative")
	}
	// A negative wall-clock cap would reach exec as an already-expired deadline,
	// killing every session the instant it started — a config typo that looks
	// like a broken harness. Refused here, where it reads as the typo it is.
	if c.MaxSessionWallClock < 0 {
		return errors.New("max_session_wall_clock is negative")
	}
	// Refused rather than clamped: a negative here would read as "set" in the
	// config and resolve to the default anyway, which is the silent-inert shape
	// doctor's negative-budget check exists to stop.
	if c.MaxZeroSpendAttempts < 0 {
		return errors.New("max_zero_spend_attempts is negative")
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
	// A per-harness config and a single-harness FLAG are two answers to the same
	// question, and the flag's premise is the one that has stopped being true.
	// Refused rather than resolved, exactly as `prompt` alongside `phases` is:
	// --harness moves which harness SessionFor treats as top-level, so the other
	// one silently starts inheriting the run-wide fields that belong to this one
	// (a claude model alias reaching another harness's --model is the failure the
	// whole per-harness block exists to make impossible), and --model applies to
	// one phase's sessions while the rest quietly ignore it.
	if c.harnessFromFlag || c.modelFromFlag {
		if mixed := len(c.Harnesses) > 0 || len(c.PhaseHarnesses()) > 1; mixed {
			flag := "--harness"
			if !c.harnessFromFlag {
				flag = "--model"
			}
			return fmt.Errorf("%s was given, but this config selects harnesses per phase (`harnesses` / a phase `harness`): "+
				"the flag assumes one harness for the whole run, so it would re-point which harness the run-wide fields "+
				"belong to — edit the config's `harnesses` block instead", flag)
		}
	}

	// Every harness a phase names has to exist and, when it is not the top-level
	// one, has to be configured. Checked before the claim-tracking rule below,
	// which looks a phase's harness up in the registry.
	for i, ph := range c.Phases {
		h := c.HarnessFor(ph)
		if !harness.Known(h) {
			return fmt.Errorf("phases[%d] (%q): unknown harness %q (want: %s)",
				i, ph.Label(i), h, strings.Join(harness.Names(), ", "))
		}
		// A phase on some OTHER harness inherits none of the run-wide
		// harness-shaped fields, by design (see HarnessConfig), so with no block
		// of its own it would spawn with no config dir, no MCP config and no
		// settings — a session with no clankerbar tools, which looks from the log
		// like a model that declined the work. Refuse here, where the operator can
		// see it, rather than at 3am.
		if h == c.Harness {
			continue
		}
		hc, ok := c.Harnesses[h]
		if !ok || hc.Empty() {
			// An EMPTY block is the same failure as a missing one, and it has to be
			// said that way: the promise this rule makes is that no session spawns
			// with none of its harness's fields, and `"harnesses": {"opencode": {}}`
			// spawns exactly that while satisfying a presence-of-key check.
			return fmt.Errorf("phases[%d] (%q) runs on harness %q, but `harnesses.%s` is missing or empty: "+
				"a phase on a harness other than the run's `harness` inherits none of config_dir / mcp_config_path / "+
				"settings_path / model, because each is that harness's own dialect — declare them under `harnesses.%s`",
				i, ph.Label(i), h, h, h)
		}
	}
	// Phases rest on EVERY phase's adapter observing the session's claim, not only
	// the claiming one's.
	//
	// The first phase has to OBSERVE a claim, or there is nothing to hand on. A
	// later phase is handed one through Invocation.ResumeClaim — but seeding it is
	// the adapter's job (harness.newSessionResult), and an adapter that does not
	// track claims does not seed either: it returns a zero Result.Claim whatever
	// it was given. That is not merely a lost capability. `drainPhase` records a
	// checkpoint only for a phase that ends holding the task, so a non-tracking
	// phase in the middle ends every sequence early with a log line that reads
	// like an ordinary finish, and a non-tracking phase at the END leaves
	// `drainPhases` carrying its predecessor's claim into the handback — which can
	// post `ready` over a task that phase has just landed at in_review.
	//
	// So the rule is per phase, and the message names the phase, because in a
	// mixed sequence "the harness" is no longer one thing.
	if len(c.Phases) > 1 {
		for i, ph := range c.Phases {
			h := c.HarnessFor(ph)
			caps, known := harness.CapabilitiesOf(h)
			if !known || caps.TracksClaims {
				continue
			}
			return fmt.Errorf("phases[%d] (%q) runs on harness %q, which does not observe the session's task claim — "+
				"a phase either claims the task or resumes one it is handed, and this harness can do neither, so the "+
				"sequence would end early or hand the task back over its own work (only the claim-tracking harnesses "+
				"can run a phase in a sequence: use one, or drop `phases`)",
				i, ph.Label(i), h)
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
		if ph.MaxWallClock < 0 {
			return fmt.Errorf("phases[%d]: max_wall_clock is negative", i)
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
	// Each per-harness MCP config points the SAME account key at a host, so it is
	// held to the same trusted origin. Sorted so a config with two bad blocks
	// reports the same one every time rather than whichever the map iterated
	// first — an error message that changes between runs on unchanged input reads
	// as a flaky check.
	topSlug := slugFromMCPURL(mcpURLFromConfig(c.MCPConfigPath))
	for _, name := range sortedKeys(c.Harnesses) {
		hc := c.Harnesses[name]
		if err := c.checkMCPConfigOrigins(hc.MCPConfigPath, "harnesses."+name+".mcp_config_path"); err != nil {
			return err
		}
		// The single-project split-brain, refused the same way the multi-project
		// one is. With no `projects` block the poll's slug comes from
		// `mcp_config_path` alone (see BacklogEndpoint), while this harness's
		// sessions work whatever ITS file names — so two files whose slugs disagree
		// gate on one project's counts and claim, work and hand back another's. It
		// is a two-file typo away in exactly the shape the README documents, and
		// nothing downstream would notice.
		//
		// Only when both files name a slug: a file that names none (or that this
		// cannot parse) constrains nothing, which is the same latitude the
		// per-project check allows.
		if topSlug != "" && len(c.Projects) == 0 {
			if got := slugFromMCPURL(mcpURLFromConfig(hc.MCPConfigPath)); got != "" && got != topSlug {
				return fmt.Errorf("harnesses.%s.mcp_config_path names /mcp/%s, but the run polls /mcp/%s (from mcp_config_path) — "+
					"the poll would gate on one project while this harness's sessions claim and work another",
					name, got, topSlug)
			}
		}
		if err := refuseInsecureMode(hc.SettingsPath, groupOtherWrite); err != nil {
			return fmt.Errorf("harnesses.%s.settings_path: %w - it is the allow/deny policy the unattended session is gated by: chmod go-w %s",
				name, err, hc.SettingsPath)
		}
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
		// The per-harness files get every check the project's own file just had:
		// they are the same account key pointed at the same kind of host, and the
		// same split-brain is available through any one of them.
		for _, name := range sortedKeys(p.MCPConfigPaths) {
			path := underWorkDir(expandHome(p.MCPConfigPaths[name]), effectiveWorkDir)
			p.MCPConfigPaths[name] = path
			label := fmt.Sprintf("projects[%d].mcp_config_paths.%s", i, name)
			if err := c.checkMCPConfigOrigins(path, label); err != nil {
				return err
			}
			if fromMCP := slugFromMCPURL(mcpURLFromConfig(path)); fromMCP != "" && fromMCP != p.Slug {
				return fmt.Errorf("%s: slug %q does not match that file, which names /mcp/%s — the poll would gate on one project while sessions work another", label, p.Slug, fromMCP)
			}
		}
	}
	// A phase on a harness other than the run's needs its own MCP config PER
	// PROJECT, because that file carries both the schema (the harness's) and the
	// slug (the project's). Falling back to the single
	// `harnesses.<name>.mcp_config_path` would point every project's phase at one
	// project's queue, which is the split-brain the slug check above refuses when
	// it is written down — so refuse it here too, where it would otherwise be
	// arrived at by omission.
	for i, ph := range c.Phases {
		h := c.HarnessFor(ph)
		if h == c.Harness {
			continue
		}
		for j := range c.Projects {
			if c.Projects[j].MCPConfigPaths[h] == "" {
				return fmt.Errorf("phases[%d] (%q) runs on harness %q, but projects[%d] (%q) declares no `mcp_config_paths.%s`: "+
					"that file carries both the harness's schema and the project's /mcp/<slug>, so one per project is needed "+
					"(the top-level `harnesses.%s.mcp_config_path` names a single project and cannot serve them all)",
					i, ph.Label(i), h, j, c.Projects[j].Slug, h, h)
			}
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

// sortedKeys is map iteration made deterministic, for the checks whose ERROR
// depends on which entry is reached first. A validation message that names a
// different block on each run of the same file reads as a flaky check rather
// than a config with two problems.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
