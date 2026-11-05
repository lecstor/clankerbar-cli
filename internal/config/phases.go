package config

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The review panel (CLA-639, design §5.4 / A4 / B10). When a config SETS
// `review_panel`, each built-in review phase expands into ONE review phase PER
// MEMBER, run against the same reviewed tip, each writing its own sealed durable
// record under its own reviewer role (`review-1`..`review-N`, by member
// position). The members run concurrently by default, in private detached
// worktrees; see ReviewPanelConcurrency. The round closes only once every member has recorded or been
// recorded as failed — a sealed round is what keeps a later member from reading
// an earlier member's findings, and a usage-limited or dead member therefore does
// not stop the round.
//
// OPT-IN, and shipped with NO model-naming default and NO preset: the CLI cannot
// know which harnesses, providers or models an operator has, so it never names
// one (decision a0b1373b, superseding the trial panel of c8e7509a). A config
// that sets no panel runs its one review phase as it always has — the historical
// brief, the historical `review-1` role, the legacy fix path, byte for byte. See
// Config.ReviewPanel.
//
// INTERIM, deliberately (B10): task-state-machine build phase 4 removes phase
// logic from the CLI and routes findings as an `agent_review -> implementing`
// plane transition, and panel membership becomes PLANE phase state (that is the
// CLA-454 reshape). Until then the panel is local config and the members run
// CONCURRENTLY by default (CLA-697), each in its own detached worktree at the
// reviewed tip; `review_panel_concurrency: 1` restores the sequential sequence

// PanelMemberLabel is the LOG LABEL of the member at position i: the name the
// operator gave it, or the deterministic `review-<i+1>` derived from its
// position. It names the member in daemon logs and iteration-log filenames; it
// is NOT the reviewer role the durable record is keyed on. Roles are positional
// (review-1..review-N), derived from the effective sequence by the driver, so
// naming a member cannot change which record it writes or which record the seam
// gate matches (CLA-639).
func PanelMemberLabel(members []ReviewPanelMember, i int) string {
	if i >= 0 && i < len(members) && strings.TrimSpace(members[i].Name) != "" {
		return strings.TrimSpace(members[i].Name)
	}
	return "review-" + strconv.Itoa(i+1)
}

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

	// Model pins this phase's session to a model alias directly, skipping the
	// tier map. It is an operator-facing key (operator decision 3eddb7a5) on
	// both the local file and the plane's stored document (CLA-598, mirrored by
	// the plane in CLA-722), and it is what lets a REVIEW PANEL read as the
	// harness/model pairs the operator wrote — see ReviewPanelMember, whose
	// expansion sets it too. Empty = fall through to Tier, then the harness's
	// default: an untouched config's resolution is unchanged.
	//
	// Unlike Tier it is an ALIAS, never a bucket name — the panel entries and the
	// documented config examples name `opencode-go/gpt-6-luna` and friends, not a
	// policy word. The two are deliberately not merged: keeping them apart is
	// what makes a typo visible as a model the provider refuses, rather than a
	// bucket that silently resolves to the harness default. A CONFIGURED phase
	// sets one of them, not both (Validate refuses both, so which one wins is
	// never ambiguous); a panel MEMBER may set both, and expansion lets the
	// alias win, exactly as before.
	//
	// It carries an ordered ROUTE LIST since CLA-782: a plain string is the
	// historical shape, and an array names the free route first and its paid
	// fallback after it. The session starts on the first route and the opencode
	// adapter may resume it on the next one when a provider usage limit denies
	// the first; ModelForPhase still reports the first route, so a single-route
	// (or string) phase resolves exactly as it did before.
	//
	// `omitempty` is load-bearing: Config.Identity() hashes the marshalled
	// config, so an unset model must not add a key and move every identity.
	Model ModelRoutes `json:"model,omitempty"`

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

	// PanelMember marks this phase as one reviewer of an expanded review panel
	// (CLA-639). It is set by EffectivePhases and NOT a config key: the operator
	// writes the panel, not its expansion, and a config that names a dozen
	// review phases by hand is a different thing (no panel) that the driver must
	// not mistake for one. Read through Phase.IsPanelMember.
	PanelMember bool `json:"-"`

	// PanelOptional marks this panel member as a TRIAL seat (CLA-784): the
	// operator's ReviewPanelMember.Optional, carried onto the expanded phase so
	// the driver can tell a seat the panel relies on from one it is evaluating.
	// Like PanelMember it is set by EffectivePhases and NOT a config key, so it
	// is invisible to Config.Identity(). Read through Phase.IsOptionalPanelMember.
	PanelOptional bool `json:"-"`

	// sourceIdx is the index in Config.Phases this phase was resolved from. It
	// equals the position for every phase except an expanded panel member, whose
	// source is the single `review` the operator wrote. Read through
	// Phase.ConfiguredIndex; zero-value use (a Phase built by hand) is its own
	// index, which is what all of them were before the panel existed.
	sourceIdx int
}

// ConfiguredIndex is the index, in Config.Phases, of the phase the operator
// actually wrote for this one. It differs from the effective position only for
// an expanded review-panel member: every member reports the single `review`
// phase it came from. The driver reads a phase's CONFIGURED prompt through it —
// a handoff continuation must match the brief the operator asked for, not the
// member's resolved copy of it.
func (p Phase) ConfiguredIndex() int { return p.sourceIdx }

// ReviewPanelMember is one reviewer in a review panel (CLA-639): a
// driver-spawned review phase of its own, on its own harness and model, run
// against the same reviewed tip as its siblings.
//
// A member is NOT a session: the driver still runs one review phase per member,
// concurrently up to Config.ReviewPanelConcurrency, and each writes its own
// sealed durable record under its own reviewer role (see Config.ReviewPanel).
// Two entries naming the same harness/model are two INDEPENDENT REVIEWS of the
// same revision — never a mistake to be deduplicated.
type ReviewPanelMember struct {
	// Name is the member's LOG LABEL only. Empty = the driver derives it from
	// the member's position: `review-1` .. `review-N`. It names the member in
	// daemon logs and iteration-log filenames; it is NOT the reviewer role the
	// durable record is keyed on. The role is always positional (`review-N`,
	// counted over the review-shaped phases of the sequence), so naming a member
	// cannot change which record it writes or which record the seam gate matches,
	// and a resumed or retried round maps a member to the same record either way.
	Name string `json:"name"`

	// Harness is the coding-agent CLI this member reviews on. Empty = the review
	// phase's own harness (which is itself the run-wide one when unset), so a
	// panel that names only models inherits the sequence's review harness.
	Harness string `json:"harness"`

	// Model pins this member's model alias, exactly as Phase.Model does — including
	// the ordered route-list form (CLA-782). Empty = the review phase's own model
	// (then its tier, then the harness default).
	Model ModelRoutes `json:"model"`

	// Tier resolves this member's model through the harness's tier map, for an
	// operator who keeps policy in buckets. Model wins when both are set: an
	// alias is the more specific statement.
	Tier string `json:"tier"`

	// Optional marks a TRIAL seat: a reviewer the operator added to evaluate,
	// not one the panel relies on (CLA-784). An optional member runs and records
	// exactly like any other, but its failure is that member's own result and
	// nothing more: whatever its session ends in — a transient blip, a usage
	// limit, a stop, a death, a tool-loop kill, a setup error — it does not end
	// the sequence, does not hand the panel's shared claim back, and does not
	// trigger the run's limit wait. The required members' records still close the
	// round and spawn the fix; a round where ONLY optional members recorded is no
	// review at all, and no fix runs.
	//
	// Default false, and `omitempty` is load-bearing: Config.Identity() hashes the
	// marshalled form, so a panel that does not write the key must hash exactly
	// what it hashed before the field existed (a trial seat is a deliberate
	// change to the panel, and the identity moving with it is correct).
	//
	// Validate refuses a panel whose members are ALL optional: with no required
	// seat there is no review a round could ever be said to have produced.
	Optional bool `json:"optional,omitempty"`
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
	// resolve to. Empty = the harness's own default. It accepts the ordered
	// route-list form (CLA-782): free route first, paid fallback after it.
	Model ModelRoutes `json:"model"`

	// Models is this harness's tier map (bucket name -> model alias), the
	// per-harness twin of Config.Models. A phase's Tier resolves against the map
	// belonging to the harness that phase runs on, because a bucket NAME is
	// portable across harnesses and the alias inside it is not — which is what
	// lets one `"tier": "strong"` mean opus here and something else there. Each
	// bucket's value also accepts the ordered route-list form (CLA-782).
	Models map[string]ModelRoutes `json:"models"`

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

	// Env is this harness's own extra session environment, overlaid ON TOP of the
	// top-level env map (per-key winner) — see Config.Env for what the map holds.
	// Unlike the fields above it does NOT inherit the top-level value only for the
	// run-wide harness: environment is not dialect-shaped, so every layer of the
	// overlay applies to every harness, with SessionEnv stacking them in
	// most-specific-wins order.
	Env EnvMap `json:"env"`

	// EnvAllowlist is this harness's inherited-env allowlist, replacing the
	// top-level one wholly for sessions on THIS harness — see Config.EnvAllowlist
	// for the semantics, the composition rule and the minimum `doctor` enforces.
	EnvAllowlist []string `json:"env_allowlist,omitempty"`
}

// Empty reports whether this block declares nothing at all — the shape that is
// indistinguishable from having no block, and which Validate refuses for the
// same reason. Written out rather than compared against the zero value because
// the Models map makes the struct incomparable.
func (h HarnessConfig) Empty() bool {
	return h.Model.Empty() && len(h.Models) == 0 && h.ConfigDir == "" && h.MCPConfigPath == "" && h.SettingsPath == "" && len(h.Env) == 0 && len(h.EnvAllowlist) == 0
}

// phaseNameRe is what a phase name may contain, because it becomes part of an
// iteration log's filename. It is the PLANE's own pattern verbatim
// (/^[a-z0-9]+(-[a-z0-9]+)*$/, RUN_CONFIG built-ins aside): a proposal is
// ratified by that validator, so a name this file accepts but the plane refuses
// (a double hyphen, a leading or trailing one) would be a config `propose-config`
// could never lift.
var phaseNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// panelMemberPositionRe matches the DERIVED member-label family (`review-N`),
// which is owned by member position (CLA-639). An explicit name from this family
// must equal the member's own derived label; otherwise the logs, the INCOMPLETE
// lines and the dead-phase tally would disagree about which member `review-2` is.
var panelMemberPositionRe = regexp.MustCompile(`^review-[0-9]+$`)

// The placeholders a phase prompt may carry, substituted by the driver from the
// claim the previous phase left held. {{branch}} names the recorded, verified
// hand-off branch (CLA-457): the implement phase's checkpoint is gated on it
// being real and on the origin remote, so the review brief can name it rather
// than asserting an unverified "an earlier session already implemented".
//
// {{prBase}} names the branch the TASK'S REPO's PRs target (CLA-608), resolved
// per repo at phase-spawn time (config.PRBaseFor, else loop.Driver.prBaseFor).
// Unlike the other three it is not a per-run fact and rides EVERY built-in
// phase, the implement phase included: the implement brief's resumed-work rule
// needs a branch to name, and that rule runs in phase 1, before any claim
// exists for the other placeholders to come from. It replaced literal
// "(staging)" text in these briefs — the literal was wrong for every repo
// whose PRs do not target staging, and the resolution above is what knows.
const (
	PhaseTaskPlaceholder   = "{{taskId}}"
	PhaseRunPlaceholder    = "{{runId}}"
	PhaseBranchPlaceholder = "{{branch}}"
	PhasePRBasePlaceholder = "{{prBase}}"

	// PhaseHarnessPlaceholder and PhaseModelPlaceholder name the phase's OWN
	// resolved harness and model. They are per-phase facts the driver knows and a
	// session cannot reliably report for itself ("what model are you?" is a guess
	// even when the provider exposes one), so the review record's attribution is
	// injected rather than remembered.
	PhaseHarnessPlaceholder = "{{harness}}"
	PhaseModelPlaceholder   = "{{model}}"

	// PhasePrevHarnessPlaceholder and PhasePrevModelPlaceholder name the phase
	// BEFORE this one — for the review, the implementer whose work is being
	// judged. Empty in phase 0, which has no predecessor, so a first-phase
	// prompt carrying them is refused rather than spawned with a literal.
	PhasePrevHarnessPlaceholder = "{{prevHarness}}"
	PhasePrevModelPlaceholder   = "{{prevModel}}"

	// PhaseNumberPlaceholder and PhaseTotalPlaceholder are this phase's 1-based
	// position and the sequence length. The briefs used to say "PHASE 2 of 2"
	// as text, which became a lie the moment a fix phase could follow: a
	// three-phase review is phase 2 of 3, and a brief that miscounts the
	// sequence is a session reasoning about a shape that is not there.
	PhaseNumberPlaceholder = "{{phaseNumber}}"
	PhaseTotalPlaceholder  = "{{phaseTotal}}"

	// ReviewerRolePlaceholder and ReviewedTipPlaceholder are the durable review
	// record's identity (CLA-637). The plane assigns the round and the cycle,
	// so neither is a placeholder: the driver reads them back off the record it
	// matches. The driver resolves the role from the phase and the reviewed tip
	// from the checkpoint it VERIFIED on the origin remote, so the session
	// records what the seam gate will match rather than inventing it. The role
	// is unique per review phase (`review-1`, ...) so a panel member each
	// appends its own record; the tip is the origin commit the reviewer must
	// check out and review.
	ReviewerRolePlaceholder = "{{reviewerRole}}"
	ReviewedTipPlaceholder  = "{{reviewedTip}}"
)

// ReviewFindingsCall is the call that updates findings on a closed record. The
// briefs carry this shape literally: there is no protocol-dependent wording left
// to select, because the plane this build ships with is the one that serves
// record_review and no older plane is supported.
const ReviewFindingsCall = "record_review(taskId, runId, {findingUpdates: [...]})"

// ReviewRecordHeading is the marker of the LEGACY block a review-only phase
// still records in the task's outcome for one release (CLA-637), so older
// readers keep seeing a review where they expect one. The durable
// per-reviewer record, written through record_review, is the interface the
// driver's review→fix seam gate reads now; this block is carried for
// compatibility, and the fix phase keeps it verbatim in its own outcome.
//
// It deliberately carries no round number. The round lives in the durable
// record, assigned by the plane; a hard-coded "round 1" would be false on any
// later run of the same task.
const ReviewRecordHeading = "## Review"

// expandsReviewPanel reports whether this config's sequence contains the shape
// a panel multiplies: a configured phase that is the built-in review brief (no
// prompt of its own) with a phase after it, i.e. the read-only gate. It is the
// same shape EffectivePhases expands, so validation and expansion cannot
// disagree about whether a panel applies.
func (c *Config) expandsReviewPanel() bool {
	for i, ph := range c.Phases {
		if ph.Prompt == "" && ph.Name == ReviewPhaseName && i < len(c.Phases)-1 {
			return true
		}
	}
	return false
}

// reviewPhaseHarness is the harness a panel member with none of its own
// inherits: the REVIEW phase's, which is itself the run-wide harness when that
// phase names none — exactly the chain EffectivePhases' panelMember copies into
// each member. The FIRST built-in review phase is the one that expands, so it is
// the one whose harness a member inherits; nothing here assumes a review phase
// exists (the caller checks).
func (c *Config) reviewPhaseHarness() string {
	for _, ph := range c.Phases {
		if ph.Prompt == "" && ph.Name == ReviewPhaseName {
			return c.HarnessFor(ph)
		}
	}
	return c.Harness
}

// EffectivePanel is the review panel this config actually runs: the operator's
// own list, and nothing else — an absent key (nil) means the historical single
// review, not a panel of one (decision a0b1373b). There is no trial panel and no
// preset: the CLI cannot know which harnesses, providers or models an operator
// has, so it never names one. A FILE that writes `"review_panel": []` is refused
// by Validate rather than silently meaning either thing (reviewPanelSet).
func (c *Config) EffectivePanel() []ReviewPanelMember {
	return c.ReviewPanel
}

// EffectivePanelConcurrency is how many members of a panel of n review AT ONCE
// (CLA-697): the operator's `review_panel_concurrency` cap, or the whole panel
// when the cap is absent, zero or larger than the panel. It never returns less
// than 1, and a panel of fewer than two members always reports 1 — there is
// nothing to fan out, and callers can use `1` as "the historical sequential
// path" without a second check.
//
// A negative cap is a config error refused by Validate; this method is the
// read path and stays total so a hand-built Config cannot make the driver
// spawn a negative number of sessions.
func (c *Config) EffectivePanelConcurrency(n int) int {
	if n < 2 {
		return 1
	}
	v := c.ReviewPanelConcurrency
	if v <= 0 || v > n {
		return n
	}
	return v
}

// panelMember is the review phase one panel member runs as: the configured
// review phase's knobs (its turn and wall-clock caps, its tier, its prompt)
// with the member's own identity, harness and model on top. Copying the phase
// rather than synthesizing a fresh one is what lets an operator cap the whole
// panel in one place — `{"name": "review", "max_turns": 80, "review_panel":
// [...]}` caps every member — while a member that names its own harness or
// model still wins where it does.
func (p Phase) panelMember(panel []ReviewPanelMember, i int) Phase {
	m := panel[i]
	ph := p
	ph.Name = PanelMemberLabel(panel, i)
	ph.PanelMember = true
	ph.PanelOptional = m.Optional
	if h := strings.TrimSpace(m.Harness); h != "" {
		ph.Harness = h
	}
	// Model and Tier are the same choice at two levels of indirection, and a
	// member states one of them: either is the more specific statement than
	// whatever the review phase carries, so setting one clears the other. An
	// empty member field leaves the review phase's own value in place (itself
	// empty on a config that resolved neither), which is the fall-through to
	// the harness default.
	if v := m.Model.List(); len(v) > 0 {
		ph.Model = ModelRoutes(v)
		ph.Tier = ""
	} else if t := strings.TrimSpace(m.Tier); t != "" {
		ph.Tier = t
		// The inherited model would otherwise WIN: ModelForPhase resolves Model
		// before Tier, so a member that asked for a bucket would silently run on
		// the review phase's pinned alias. The member's own tier is the more
		// specific statement, so it clears the inherited model too.
		ph.Model = nil
	}
	return ph
}

// IsPanelMember reports whether this phase is one reviewer of an expanded
// review panel (CLA-639) — the copy EffectivePhases makes, not the single
// `review` the operator wrote. The driver reads it to tell a panel member that
// may be skipped from a review phase whose record is the sequence's only gate.
func (p Phase) IsPanelMember() bool { return p.PanelMember }

// IsOptionalPanelMember reports whether this phase is a TRIAL seat of an
// expanded review panel (CLA-784): a reviewer whose failure is recorded against
// that member alone and never ends the sequence, hands back the panel's shared
// claim, or blocks the fix. False for every phase that is not a panel member.
func (p Phase) IsOptionalPanelMember() bool { return p.PanelMember && p.PanelOptional }

// EffectivePhases is the sequence a task runs as: the configured phases with
// their built-in prompts resolved and the review panel expanded, or — when none
// are configured — a single phase carrying Prompt.
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
	// The panel expands AFTER each phase's built-in brief is resolved (below),
	// because a member replaces the configured phase's identity with its own log
	// label (no built-in prompt answers to `review-1`): the member inherits the
	// resolved REVIEW brief and only its label, harness and model change.
	panel := c.EffectivePanel()
	out := make([]Phase, 0, len(phases)+len(panel))
	for srcIdx, ph := range phases {
		if ph.Prompt == "" {
			// The review phase's MODE is positional: a review with a phase after
			// it is the read-only gate, and a final review keeps the historical
			// review-and-fix brief so a two-phase config still finishes. See
			// reviewOnlyBrief and the builtinPhasePrompts comment.
			//
			// The fix phase's TERMINAL is positional for the same reason: a fix
			// followed by a verification round must not open the PR (CLA-638,
			// B12), because the verifier carries that terminal step only after it
			// finds no open MUST.
			switch {
			case ph.Name == ReviewPhaseName && srcIdx < len(phases)-1:
				ph.Prompt = reviewOnlyBrief
			case ph.Name == FixPhaseName && srcIdx+1 < len(phases) && phases[srcIdx+1].Name == VerifyPhaseName && phases[srcIdx+1].Prompt == "":
				ph.Prompt = fixVerifyBrief
			default:
				ph.Prompt = builtinPhasePrompts[ph.Name]
			}
		}
		ph.sourceIdx = srcIdx
		// CLA-639: a built-in review phase expands into one review phase PER
		// PANEL MEMBER, so every consumer of the effective sequence — the
		// driver's spawn loop, its seam, doctor — sees the reviewers that
		// actually run rather than a single review the driver would have to
		// remember to multiply. The panel is what the operator SET: an absent
		// `review_panel` expands nothing (the single reviewer, as before), a
		// phase carrying its own prompt is not expanded either (a custom review
		// prompt owns its own contract), and a FINAL review keeps the historical
		// combined brief — the position check is part of the expansion itself,
		// not only of Validate, so EffectivePhases can never multiply a final
		// review even when a caller skips validation. Validate refuses a
		// configured panel in either of those shapes rather than letting it
		// silently not apply.
		if len(panel) > 0 && srcIdx < len(c.Phases)-1 && c.Phases[srcIdx].Prompt == "" && ph.Name == ReviewPhaseName {
			for i := range panel {
				member := ph.panelMember(panel, i)
				member.sourceIdx = srcIdx
				out = append(out, member)
			}
			continue
		}
		out = append(out, ph)
	}
	// A verify phase that names neither a harness nor a tier nor a model would
	// otherwise run on the REVIEWER'S model — the one that raised the findings —
	// which A5 (throughput, not only dollars) says is the wrong default:
	// verification rounds are scoped and cheap, and a Claude subscription limit is
	// shared with the operator's interactive use. So it inherits the FIX phase's
	// harness and its model/tier pair instead (the shipped shape has fix on a
	// cheap non-Claude harness), and an explicit harness/model/tier on the verify
	// phase always wins. Configurable, never silent magic: the resolved values
	// are what the spawn line stamps.
	//
	// Keyed on the RESOLVED built-in brief, not the name: an operator's own
	// prompt under the name "verify" owns its own contract (the same convention
	// every other variant follows — verifyBrief is what the driver treats as a
	// verifier), so it must not silently inherit the fix phase's knobs.
	if len(c.Phases) > 1 {
		lastFix := -1
		for i := range out {
			if out[i].Name == FixPhaseName {
				lastFix = i
				continue
			}
			if out[i].Name != VerifyPhaseName || out[i].Prompt != verifyBrief || lastFix < 0 {
				continue
			}
			// A model alias is provider-specific: the model/tier pair crosses only
			// when the verifier ends up on the fix phase's harness (the tier is
			// dropped too, by the task's design, so the verifier resolves through
			// its own harness's default).
			sameHarness := out[i].Harness == "" || c.HarnessFor(out[i]) == c.HarnessFor(out[lastFix])
			if out[i].Harness == "" {
				out[i].Harness = out[lastFix].Harness
			}
			// Model and tier are one choice, and the verify phase's own statement
			// wins: inherit the fix phase's pair only when the verifier named
			// neither. Inheriting the MODEL beside a tier the verifier named
			// would make that tier inert, because ModelForPhase resolves Model
			// first — the same trap a panel member's tier has.
			if sameHarness && out[i].Model.Empty() && out[i].Tier == "" {
				out[i].Model = out[lastFix].Model
				out[i].Tier = out[lastFix].Tier
			}
		}
	}
	for i := range out {
		out[i].MaxTurns = c.resolveMaxTurns(out[i].MaxTurns)
		out[i].MaxWallClock = c.resolveWallClock(out[i].MaxWallClock)
		// Resolve the harness here too, so every phase the driver iterates NAMES
		// the harness it runs on and nothing downstream has to re-derive it from
		// the config. The synthesized single phase of an unphased run gets the
		// run-wide harness by the same rule, which is what it always ran on.
		out[i].Harness = c.HarnessFor(out[i])
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
