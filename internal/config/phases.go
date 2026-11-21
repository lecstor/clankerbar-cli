package config

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func PanelMemberLabel(members []ReviewPanelMember, i int) string {
	if i >= 0 && i < len(members) && strings.TrimSpace(members[i].Name) != "" {
		return strings.TrimSpace(members[i].Name)
	}
	return "review-" + strconv.Itoa(i+1)
}

// Phase is one session in a task's sequence.
type Phase struct {
	Name string `json:"name"`

	// Prompt is what this phase's session is asked to do.
	Prompt string `json:"prompt"`

	MaxTurns int `json:"max_turns"`

	// MaxWallClock caps this phase's SESSION by elapsed time (harness permitting), as the backstop for
	// a harness whose CLI takes no turn flag — opencode is that harness today, where MaxTurns never
	// reaches the process and a phase boundary rests on the prompt alone.
	MaxWallClock Duration `json:"max_wall_clock"`

	// Tier names a bucket in Config.Models — which of the operator's models this phase's session
	// runs on.
	Tier string `json:"tier"`

	// Model pins this phase's session to a model alias directly, skipping the tier map.
	Model ModelRoutes `json:"model,omitempty"`

	Harness string `json:"harness"`

	// ReviewMethod names how this phase produces its review findings.
	ReviewMethod string `json:"review_method,omitempty"`

	// MapsSession configures the maps-session method.
	MapsSession *MapsSessionOptions `json:"maps_session,omitempty"`

	PanelMember bool `json:"-"`

	PanelOptional bool `json:"-"`

	sourceIdx int
}

// ConfiguredIndex is the index, in Config.Phases, of the phase the operator actually wrote for this
// one.
func (p Phase) ConfiguredIndex() int { return p.sourceIdx }

// ReviewPanelMember is one reviewer in a review panel: a driver-spawned review phase of its own, on
// its own harness and model, run against the same reviewed tip as its siblings.
type ReviewPanelMember struct {
	// Name is the member's LOG LABEL only.
	Name string `json:"name"`

	// Harness is the coding-agent CLI this member reviews on.
	Harness string `json:"harness"`

	Model ModelRoutes `json:"model"`

	Tier string `json:"tier"`

	// Optional marks a TRIAL seat: a reviewer the operator added to evaluate, not one the panel relies
	// on.
	Optional bool `json:"optional,omitempty"`

	ReviewMethod string `json:"review_method,omitempty"`

	// MapsSession overrides the phase's maps-session options for this member.
	MapsSession *MapsSessionOptions `json:"maps_session,omitempty"`
}

// HarnessConfig is one harness's own invocation fields, for a run whose phases are not all on the
// same harness.
type HarnessConfig struct {
	Model ModelRoutes `json:"model"`

	Models map[string]ModelRoutes `json:"models"`

	ConfigDir string `json:"config_dir"`

	// MCPConfigPath points this harness's sessions at the clankerbar MCP server, in THAT harness's
	// schema.
	MCPConfigPath string `json:"mcp_config_path"`

	SettingsPath string `json:"settings_path"`

	// Env is this harness's own extra session environment, overlaid ON TOP of the top-level env map
	// (per-key winner) — see Config.Env for what the map holds.
	Env EnvMap `json:"env"`

	EnvAllowlist []string `json:"env_allowlist,omitempty"`
}

// Empty reports whether this block declares nothing at all — the shape that is indistinguishable
// from having no block, and which Validate refuses for the same reason.
func (h HarnessConfig) Empty() bool {
	return h.Model.Empty() && len(h.Models) == 0 && h.ConfigDir == "" && h.MCPConfigPath == "" && h.SettingsPath == "" && len(h.Env) == 0 && len(h.EnvAllowlist) == 0
}

var phaseNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// panelMemberPositionRe matches the DERIVED member-label family (`review-N`), which is owned by
// member position.
var panelMemberPositionRe = regexp.MustCompile(`^review-[0-9]+$`)

const (
	PhaseTaskPlaceholder   = "{{taskId}}"
	PhaseRunPlaceholder    = "{{runId}}"
	PhaseBranchPlaceholder = "{{branch}}"
	PhasePRBasePlaceholder = "{{prBase}}"

	// PhaseBasePlaceholder names the commit a brief's `git diff` starts from: the merge-base of
	// origin/<prBase> and the worktree tip, as the runner computed it during worktree prep, or
	// origin/<prBase> when no SHA was available.
	PhaseBasePlaceholder = "{{baseSha}}"

	// PhaseHarnessPlaceholder and PhaseModelPlaceholder name the phase's OWN resolved harness and
	// model.
	PhaseHarnessPlaceholder = "{{harness}}"
	PhaseModelPlaceholder   = "{{model}}"

	// PhasePrevHarnessPlaceholder and PhasePrevModelPlaceholder name the phase BEFORE this one — for
	// the review, the implementer whose work is being judged.
	PhasePrevHarnessPlaceholder = "{{prevHarness}}"
	PhasePrevModelPlaceholder   = "{{prevModel}}"

	PhaseNumberPlaceholder = "{{phaseNumber}}"
	PhaseTotalPlaceholder  = "{{phaseTotal}}"

	// ReviewerRolePlaceholder and ReviewedTipPlaceholder are the durable review record's identity.
	ReviewerRolePlaceholder = "{{reviewerRole}}"
	ReviewedTipPlaceholder  = "{{reviewedTip}}"
)

// ReviewFindingsCall is the call that updates findings on a closed record.
const ReviewFindingsCall = "record_review(taskId, runId, {findingUpdates: [...]})"

// ReviewRecordHeading is the marker of the LEGACY block a review-only phase still records in the
// task's outcome for one release, so older readers keep seeing a review where they expect one.
const ReviewRecordHeading = "## Review"

// expandsReviewPanel reports whether this config's sequence contains the shape a panel multiplies:
// a configured phase that is the built-in review brief (no prompt of its own) with a phase after
// it, i.e. the read-only gate.
func (c *Config) expandsReviewPanel() bool {
	for i, ph := range c.Phases {
		if ph.Prompt == "" && ph.Name == ReviewPhaseName && i < len(c.Phases)-1 {
			return true
		}
	}
	return false
}

func (c *Config) reviewPhaseHarness() string {
	for _, ph := range c.Phases {
		if ph.Prompt == "" && ph.Name == ReviewPhaseName {
			return c.HarnessFor(ph)
		}
	}
	return c.Harness
}

// EffectivePanel is the review panel this config actually runs: the operator's own list, and
// nothing else — an absent key (nil) means the historical single review, not a panel of one
// (decision a0b1373b).
func (c *Config) EffectivePanel() []ReviewPanelMember {
	return c.ReviewPanel
}

// EffectivePanelConcurrency is how many members of a panel of n review AT ONCE: the operator's
// `review_panel_concurrency` cap, or the whole panel when the cap is absent, zero or larger than
// the panel.
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

func (p Phase) panelMember(panel []ReviewPanelMember, i int) Phase {
	m := panel[i]
	ph := p
	ph.Name = PanelMemberLabel(panel, i)
	ph.PanelMember = true
	ph.PanelOptional = m.Optional
	if h := strings.TrimSpace(m.Harness); h != "" {
		ph.Harness = h
	}
	if v := m.Model.List(); len(v) > 0 {
		ph.Model = ModelRoutes(v)
		ph.Tier = ""
	} else if t := strings.TrimSpace(m.Tier); t != "" {
		ph.Tier = t
		ph.Model = nil
	}
	// The method is the same kind of per-member statement as the model: the member's own wins, an
	// empty one inherits the phase's (copied above), and a member that sets only the options keeps the
	// inherited method so a panel can tune one seat's chunking without restating `maps-session`.
	if v := strings.TrimSpace(m.ReviewMethod); v != "" {
		ph.ReviewMethod = v
		ph.MapsSession = m.MapsSession
	} else if m.MapsSession != nil {
		ph.MapsSession = m.MapsSession
	}
	return ph
}

// IsPanelMember reports whether this phase is one reviewer of an expanded review panel — the copy
// EffectivePhases makes, not the single `review` the operator wrote.
func (p Phase) IsPanelMember() bool { return p.PanelMember }

// IsOptionalPanelMember reports whether this phase is a TRIAL seat of an expanded review panel: a
// reviewer whose failure is recorded against that member alone and never ends the sequence, hands
// back the panel's shared claim, or blocks the fix.
func (p Phase) IsOptionalPanelMember() bool { return p.PanelMember && p.PanelOptional }

// EffectivePhases is the sequence a task runs as: the configured phases with their built-in prompts
// resolved and the review panel expanded, or — when none are configured — a single phase
// carrying Prompt.
func (c *Config) EffectivePhases() []Phase {
	phases := c.Phases
	if len(phases) == 0 {
		phases = []Phase{{Prompt: c.Prompt}}
	}
	// The panel expands AFTER each phase's built-in brief is resolved (below), because a member
	// replaces the configured phase's identity with its own log label (no built-in prompt answers to
	// `review-1`): the member inherits the resolved REVIEW brief and only its label, harness and model
	// change.
	panel := c.EffectivePanel()
	out := make([]Phase, 0, len(phases)+len(panel))
	for srcIdx, ph := range phases {
		if ph.Prompt == "" {
			// The review phase's MODE is positional: a review with a phase after it is the read-only gate,
			// and a final review keeps the historical review-and-fix brief so a two-phase config still
			// finishes.
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
	// A verify phase that names neither a harness nor a tier nor a model would otherwise run on the
	// REVIEWER'S model — the one that raised the findings — which A5 (throughput, not only
	// dollars) says is the wrong default: verification rounds are scoped and cheap, and a Claude
	// subscription limit is shared with the operator's interactive use.
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
			// A model alias is provider-specific: the model/tier pair crosses only when the verifier ends up
			// on the fix phase's harness (the tier is dropped too, by the task's design, so the verifier
			// resolves through its own harness's default).
			sameHarness := out[i].Harness == "" || c.HarnessFor(out[i]) == c.HarnessFor(out[lastFix])
			if out[i].Harness == "" {
				out[i].Harness = out[lastFix].Harness
			}
			// Model and tier are one choice, and the verify phase's own statement wins: inherit the fix
			// phase's pair only when the verifier named neither.
			if sameHarness && out[i].Model.Empty() && out[i].Tier == "" {
				out[i].Model = out[lastFix].Model
				out[i].Tier = out[lastFix].Tier
			}
		}
	}
	for i := range out {
		out[i].MaxTurns = c.resolveMaxTurns(out[i].MaxTurns)
		out[i].MaxWallClock = c.resolveWallClock(out[i].MaxWallClock)
		out[i].Harness = c.HarnessFor(out[i])
	}
	return out
}

func BuiltinPhaseNames() []string {
	names := make([]string, 0, len(builtinPhasePrompts))
	for n := range builtinPhasePrompts {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (p Phase) Label(i int) string {
	if p.Name != "" {
		return p.Name
	}
	return strconv.Itoa(i + 1)
}
