package config

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// PanelMemberLabel is the LOG LABEL of the member at position i: the name the operator gave it, or
// the deterministic `review-<i+1>` derived from its position. Roles are positional
// (review-1..review-N), derived from the effective sequence by the driver, so naming a member
// cannot change which record it writes or which record the seam gate matches.
func PanelMemberLabel(members []ReviewPanelMember, i int) string {
	if i >= 0 && i < len(members) && strings.TrimSpace(members[i].Name) != "" {
		return strings.TrimSpace(members[i].Name)
	}
	return "review-" + strconv.Itoa(i+1)
}

// Phase is one session in a task's sequence. The point is a context reset the MODEL cannot perform
// for itself.
type Phase struct {
	Name string `json:"name"`

	// Prompt is what this phase's session is asked to do. They are the whole reason a later phase can
	// RESUME a run instead of claiming a new task, and they are only available once a phase has held a
	// claim — so they are meaningless in the first phase.
	Prompt string `json:"prompt"`

	MaxTurns int `json:"max_turns"`

	MaxWallClock Duration `json:"max_wall_clock"`

	// Tier names a bucket in Config.Models — which of the operator's models this phase's session
	// runs on.
	Tier string `json:"tier"`

	// Model pins this phase's session to a model alias directly, skipping the tier map. Unlike Tier it
	// is an ALIAS, never a bucket name — the panel entries and the documented config examples name
	// `opencode-go/gpt-6-luna` and friends, not a policy word.
	Model ModelRoutes `json:"model,omitempty"`

	// Harness selects which coding-agent CLI runs this phase.
	Harness string `json:"harness"`

	// ReviewMethod names how this phase produces its review findings. Only a review phase may set it
	// (Validate refuses otherwise), and only a harness that can continue one session across turns can
	// run it (opencode today).
	ReviewMethod string `json:"review_method,omitempty"`

	// MapsSession configures the maps-session method. A pointer with `omitempty` so an unset key does
	// not move Config.Identity for a config that never adopts the method.
	MapsSession *MapsSessionOptions `json:"maps_session,omitempty"`

	PanelMember bool `json:"-"`

	PanelOptional bool `json:"-"`

	sourceIdx int
}

// ConfiguredIndex is the index, in Config.Phases, of the phase the operator actually wrote for this
// one. The driver reads a phase's CONFIGURED prompt through it — a handoff continuation must
// match the brief the operator asked for, not the member's resolved copy of it.
func (p Phase) ConfiguredIndex() int { return p.sourceIdx }

// ReviewPanelMember is one reviewer in a review panel: a driver-spawned review phase of its own, on
// its own harness and model, run against the same reviewed tip as its siblings. Two entries naming
// the same harness/model are two INDEPENDENT REVIEWS of the same revision — never a mistake to be
// deduplicated.
type ReviewPanelMember struct {
	// Name is the member's LOG LABEL only.
	Name string `json:"name"`

	// Harness is the coding-agent CLI this member reviews on. Empty = the review phase's own harness
	// (which is itself the run-wide one when unset), so a panel that names only models inherits the
	// sequence's review harness.
	Harness string `json:"harness"`

	Model ModelRoutes `json:"model"`

	Tier string `json:"tier"`

	// Optional marks a TRIAL seat: a reviewer the operator added to evaluate, not one the panel relies
	// on. The required members' records still close the round and spawn the fix; a round where ONLY
	// optional members recorded is no review at all, and no fix runs.
	Optional bool `json:"optional,omitempty"`

	ReviewMethod string `json:"review_method,omitempty"`

	// MapsSession overrides the phase's maps-session options for this member. Nil inherits the phase's
	// block, so a member that only tunes the options keeps the inherited method and profile.
	MapsSession *MapsSessionOptions `json:"maps_session,omitempty"`
}

// HarnessConfig is one harness's own invocation fields, for a run whose phases are not all on the
// same harness.
type HarnessConfig struct {
	Model ModelRoutes `json:"model"`

	Models map[string]ModelRoutes `json:"models"`

	ConfigDir string `json:"config_dir"`

	MCPConfigPath string `json:"mcp_config_path"`

	SettingsPath string `json:"settings_path"`

	// Env is this harness's own extra session environment, overlaid ON TOP of the top-level env map
	// (per-key winner) — see Config.Env for what the map holds.
	Env EnvMap `json:"env"`

	EnvAllowlist []string `json:"env_allowlist,omitempty"`
}

// Empty reports whether this block declares nothing at all — the shape that is indistinguishable
// from having no block, and which Validate refuses for the same reason. Written out rather than
// compared against the zero value because the Models map makes the struct incomparable.
func (h HarnessConfig) Empty() bool {
	return h.Model.Empty() && len(h.Models) == 0 && h.ConfigDir == "" && h.MCPConfigPath == "" && h.SettingsPath == "" && len(h.Env) == 0 && len(h.EnvAllowlist) == 0
}

var phaseNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

var panelMemberPositionRe = regexp.MustCompile(`^review-[0-9]+$`)

const (
	PhaseTaskPlaceholder   = "{{taskId}}"
	PhaseRunPlaceholder    = "{{runId}}"
	PhaseBranchPlaceholder = "{{branch}}"
	PhasePRBasePlaceholder = "{{prBase}}"

	// PhaseBasePlaceholder names the commit a brief's `git diff` starts from: the merge-base of
	// origin/<prBase> and the worktree tip, as the runner computed it during worktree prep, or
	// origin/<prBase> when no SHA was available. Never a bare local <prBase>: the local base branch
	// can be far behind origin (the MAK-362 review's was 124 commits), and a diff from it carries
	// unrelated files.
	PhaseBasePlaceholder = "{{baseSha}}"

	// PhaseHarnessPlaceholder and PhaseModelPlaceholder name the phase's OWN resolved harness and
	// model. They are per-phase facts the driver knows and a session cannot reliably report for itself
	// ("what model are you?" is a guess even when the provider exposes one), so the review record's
	// attribution is injected rather than remembered.
	PhaseHarnessPlaceholder = "{{harness}}"
	PhaseModelPlaceholder   = "{{model}}"

	// PhasePrevHarnessPlaceholder and PhasePrevModelPlaceholder name the phase BEFORE this one — for
	// the review, the implementer whose work is being judged. Empty in phase 0, which has no
	// predecessor, so a first-phase prompt carrying them is refused rather than spawned with a
	// literal.
	PhasePrevHarnessPlaceholder = "{{prevHarness}}"
	PhasePrevModelPlaceholder   = "{{prevModel}}"

	PhaseNumberPlaceholder = "{{phaseNumber}}"
	PhaseTotalPlaceholder  = "{{phaseTotal}}"

	// ReviewerRolePlaceholder and ReviewedTipPlaceholder are the durable review record's identity. The
	// role is unique per review phase (`review-1`, ...) so a panel member each appends its own record;
	// the tip is the origin commit the reviewer must check out and review.
	ReviewerRolePlaceholder = "{{reviewerRole}}"
	ReviewedTipPlaceholder  = "{{reviewedTip}}"
)

const ReviewFindingsCall = "record_review(taskId, runId, {findingUpdates: [...]})"

// ReviewRecordHeading is the marker of the LEGACY block a review-only phase still records in the
// task's outcome for one release, so older readers keep seeing a review where they expect one.
const ReviewRecordHeading = "## Review"

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
// (decision a0b1373b). There is no trial panel and no preset: the CLI cannot know which harnesses,
// providers or models an operator has, so it never names one.
func (c *Config) EffectivePanel() []ReviewPanelMember {
	return c.ReviewPanel
}

// EffectivePanelConcurrency is how many members of a panel of n review AT ONCE: the operator's
// `review_panel_concurrency` cap, or the whole panel when the cap is absent, zero or larger than
// the panel. A negative cap is a config error refused by Validate; this method is the read path and
// stays total so a hand-built Config cannot make the driver spawn a negative number of sessions.
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
	if v := strings.TrimSpace(m.ReviewMethod); v != "" {
		ph.ReviewMethod = v
		ph.MapsSession = m.MapsSession
	} else if m.MapsSession != nil {
		ph.MapsSession = m.MapsSession
	}
	return ph
}

// IsPanelMember reports whether this phase is one reviewer of an expanded review panel — the copy
// EffectivePhases makes, not the single `review` the operator wrote. The driver reads it to tell a
// panel member that may be skipped from a review phase whose record is the sequence's only gate.
func (p Phase) IsPanelMember() bool { return p.PanelMember }

func (p Phase) IsOptionalPanelMember() bool { return p.PanelMember && p.PanelOptional }

// EffectivePhases is the sequence a task runs as: the configured phases with their built-in prompts
// resolved and the review panel expanded, or — when none are configured — a single phase
// carrying Prompt. The unphased case is expressed as one phase on purpose, so the driver has
// exactly ONE shape to run and the old behaviour is not a second code path that can rot untested.
func (c *Config) EffectivePhases() []Phase {
	phases := c.Phases
	if len(phases) == 0 {
		phases = []Phase{{Prompt: c.Prompt}}
	}
	panel := c.EffectivePanel()
	out := make([]Phase, 0, len(phases)+len(panel))
	for srcIdx, ph := range phases {
		if ph.Prompt == "" {
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
			sameHarness := out[i].Harness == "" || c.HarnessFor(out[i]) == c.HarnessFor(out[lastFix])
			if out[i].Harness == "" {
				out[i].Harness = out[lastFix].Harness
			}
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
