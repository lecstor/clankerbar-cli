package config

// reviewmethod.go — how a review phase produces its findings.
//
// The shipped method is one single-pass session: read the diff, write the
// record. `review_method: "maps-session"` replaces that session's turns with
// the maps+notes technique (CLA-864): the driver maps the diff into M/F items,
// sends them in chunks, and the session writes notes per item before a final
// findings turn turns those notes into the record. The technique is a runner
// feature through and through — the maps are built by the DRIVER in the worktree
// the runner prepared — so a harness that cannot continue one session across
// turns cannot run it, and Validate refuses rather than silently running only
// the first turn.

import (
	"fmt"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// The review methods a phase or panel member may name. An empty value is the
// shipped single-pass review, so an untouched config is unchanged; naming
// "single-pass" explicitly is accepted so a config can state it.
const (
	ReviewMethodSinglePass  = "single-pass"
	ReviewMethodMapsSession = "maps-session"
)

// The maps-session defaults, matching the benchmarked profile (CLA-864). A zero
// field selects these, which is what lets an operator write only the knob they
// care about.
const (
	// DefaultMapsSessionChunkSize is how many items one notes turn receives.
	DefaultMapsSessionChunkSize = 25
	// DefaultMapsSessionFlowMax is the flow map's item cap. The change map has
	// none: a reviewer that misses a changed region misses it entirely.
	DefaultMapsSessionFlowMax = 40
)

// MapsSessionOptions configures the maps-session review method. Every field has
// a zero-value default, so `"maps_session": {}` is a valid profile.
type MapsSessionOptions struct {
	// ChunkSize is how many map items one notes turn carries. Zero selects
	// DefaultMapsSessionChunkSize.
	ChunkSize int `json:"chunk_size,omitempty"`

	// NotesModel names the model the notes turns run on. Empty inherits the
	// phase's model. It exists so a stronger model can take the per-item reading
	// and a cheaper one write the findings (or the reverse) — the choice is the
	// operator's, never this package's.
	NotesModel string `json:"notes_model,omitempty"`

	// FindingsModel names the model the final findings turn runs on. Empty
	// inherits the phase's model, like NotesModel; when the phase names no model
	// either, it inherits the resolved notes stage model, since that is the model
	// the live session is already on (see StageModels).
	FindingsModel string `json:"findings_model,omitempty"`

	// FlowMax caps the flow map's items (uses beyond the cap are summarised in
	// an unnumbered line). Zero selects DefaultMapsSessionFlowMax.
	FlowMax int `json:"flow_max,omitempty"`
}

// Resolved returns a copy with the defaults filled and model names trimmed.
func (o MapsSessionOptions) Resolved() MapsSessionOptions {
	if o.ChunkSize == 0 {
		o.ChunkSize = DefaultMapsSessionChunkSize
	}
	if o.FlowMax == 0 {
		o.FlowMax = DefaultMapsSessionFlowMax
	}
	o.NotesModel = strings.TrimSpace(o.NotesModel)
	o.FindingsModel = strings.TrimSpace(o.FindingsModel)
	return o
}

// StageModels resolves the model each stage runs on for a phase whose resolved
// model is phaseModel: a stage's own model wins, an empty one inherits the
// phase's, and a findings stage with neither runs on the notes stage's model —
// a live session cannot return to the harness's own default mid-conversation, so
// the notes model is what the findings turn actually produces. The runner and
// doctor both read this one function so the record and the preflight cannot
// disagree about which model wrote the findings.
func (o MapsSessionOptions) StageModels(phaseModel string) (notes, findings string) {
	notes = o.NotesModel
	if notes == "" {
		notes = phaseModel
	}
	findings = o.FindingsModel
	if findings == "" {
		if phaseModel != "" {
			findings = phaseModel
		} else {
			findings = notes
		}
	}
	return notes, findings
}

// UsesMapsSession reports whether this phase asked for the maps-session method.
func (p Phase) UsesMapsSession() bool { return p.ReviewMethod == ReviewMethodMapsSession }

// MapsSessionDefaults returns this phase's maps-session options with the
// defaults filled in. It is total: a phase that names no options object gets
// the benchmark profile, which is what the method's zero value means.
func (p Phase) MapsSessionDefaults() MapsSessionOptions {
	o := MapsSessionOptions{}
	if p.MapsSession != nil {
		o = *p.MapsSession
	}
	return o.Resolved()
}

// validateReviewMethods checks every effective step that names a review method:
// the method is known, the step can run a turn sequence, and the options are
// not inert. It reads the EFFECTIVE sequence so a panel member's own method and
// inherited options are judged as they will run.
func (c *Config) validateReviewMethods() error {
	for i, ph := range c.EffectivePhases() {
		if ph.ReviewMethod == "" && ph.MapsSession == nil {
			continue
		}
		step := reviewMethodStep(ph, i)
		switch ph.ReviewMethod {
		case "", ReviewMethodSinglePass:
			if ph.MapsSession != nil {
				return fmt.Errorf("%s: `maps_session` is set but `review_method` is not \"%s\" — the options would be inert; set `review_method` or remove `maps_session`",
					step, ReviewMethodMapsSession)
			}
			continue
		case ReviewMethodMapsSession:
		default:
			return fmt.Errorf("%s: unknown review_method %q (want: %q or %q, or omit it for the single-pass default)",
				step, ph.ReviewMethod, ReviewMethodSinglePass, ReviewMethodMapsSession)
		}
		// The method only means anything on a REVIEW phase: its prompt is what
		// the maps section is appended to, and a non-review step has no review
		// record for the findings turn to write. A verification round is
		// review-SHAPED but adjudicates prior findings over a fix delta, so it is
		// explicitly out: its done report is the verifier's, not a fresh review's.
		if !ReviewShaped(ph.Prompt) || VerifyShaped(ph.Prompt) {
			hint := ""
			if ph.Name == ReviewPhaseName {
				hint = " (this phase runs the FINAL combined review-and-fix brief; the maps-session turns are the review-only gate's, so put a phase after the review)"
			}
			return fmt.Errorf("%s: review_method %q needs a review phase, and this step's brief is not one of the shipped review briefs — the maps-session turns end in a review record, which this step does not write%s",
				step, ReviewMethodMapsSession, hint)
		}
		h := c.HarnessFor(ph)
		caps, known := harness.CapabilitiesOf(h)
		if !known {
			// validatePhaseHarnesses/validatePanelMembers already refused an
			// unknown harness; keep this total rather than crashing.
			return fmt.Errorf("%s: review_method %q needs a harness that can run a turn sequence, and %q is not a registered harness",
				step, ReviewMethodMapsSession, h)
		}
		if !caps.RunsTurnSequences {
			return fmt.Errorf("%s: review_method %q runs its notes and findings turns in ONE session, which harness %q cannot continue — it refuses a multi-turn invocation rather than silently dropping every turn after the first; run this review on a harness that can continue a session, or leave the method at the single-pass default",
				step, ReviewMethodMapsSession, h)
		}
		if n := ph.MapsSessionDefaults().ChunkSize; n <= 0 {
			return fmt.Errorf("%s: maps_session.chunk_size must be positive, got %d", step, n)
		}
		if n := ph.MapsSessionDefaults().FlowMax; n <= 0 {
			return fmt.Errorf("%s: maps_session.flow_max must be positive, got %d", step, n)
		}
	}
	return nil
}

// reviewMethodStep labels a step in a review-method refusal: a configured phase
// by its position and label, an expanded panel member by its label (its
// position is the member's, not the operator's).
func reviewMethodStep(ph Phase, i int) string {
	if ph.IsPanelMember() {
		return fmt.Sprintf("review_panel member %q", ph.Label(i))
	}
	return fmt.Sprintf("phases[%d] (%q)", ph.ConfiguredIndex(), ph.Label(i))
}
