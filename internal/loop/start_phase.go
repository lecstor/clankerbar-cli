package loop

// Choosing which phase a freshly claimed task starts at. Every fact below is
// already on the plane or one ls-remote plus git away, so the decision is pure
// and the caller only has to gather the facts. The rows are the start-phase
// table in docs/runner-owned-mechanics.md ("Implementation session decisions").

import (
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// startFacts is what the start-phase table reads, as the caller finds it.
type startFacts struct {
	// CloseOut marks a claim of an approved-awaiting-merge task: the job is the
	// close-out, which runs implement with the close-out brief and no prBase
	// merge.
	CloseOut bool
	// HasWIP is the plane's branch signal. Branch is always set (the plane
	// synthesises a name), so this flag is the one to trust.
	HasWIP bool
	Branch string
	// NoCodeDelivered is the task's delivery.noCode declaration.
	NoCodeDelivered bool
	// OriginKnown is false when ls-remote failed: nothing about the branch on
	// origin can be decided, which is why a WIP claim aborts.
	OriginKnown bool
	// OnOrigin is whether Branch exists on origin.
	OnOrigin bool
	// Tip is origin's tip of Branch, empty when it is not on origin.
	Tip string
	// ImplementDoneTip is phasesDone["implement"].tip, empty when the plane
	// carries no marker.
	ImplementDoneTip string
	// ImplementDoneIsAncestor is the caller's git answer to "is
	// ImplementDoneTip Tip itself or an ancestor of it": ancestry is the one
	// fact no plane read can supply.
	ImplementDoneIsAncestor bool
	// Reviews is the task's durable review records from get_task.
	Reviews []plane.ReviewRecordState
	// OpenMustIDs is every open MUST, by finding id, on a released (State
	// "open") record only: fill it with openMustIDs, because the plane's own
	// openMustIds also lists a void record's findings.
	OpenMustIDs []string
	// OpenMustAtTip is whether a released (State "open") record that reviewed
	// Tip itself carries an open MUST. Only that selects fix: an older record's
	// MUST beside a clean record at Tip does not.
	OpenMustAtTip bool
}

// startChoice is the phase to start at and what the start needs from the caller.
type startChoice struct {
	// Index is the position in phases to begin the walk at.
	Index int
	// Abort means the claim can be neither honoured nor resumed: hand it back
	// and end the drain as transient.
	Abort bool
	// Branchless marks a start whose checkpoint carries no branch — the no-code
	// review, which has no tip to check out.
	Branchless bool
	// CloseOut marks a close-out claim, which uses the close-out brief and no
	// prBase merge.
	CloseOut bool
	// Checkpoint is the claim the start resumes. Index 0 has no claim to carry,
	// so it is never filled there.
	Checkpoint struct{ Branch, Tip string }
	// Why is one log line naming the row that decided it.
	Why string
}

// chooseStartPhase is the whole table. Rows are evaluated in priority order, so
// an overlap resolves the way the table says rather than by accident of ordering
// inside a switch.
func chooseStartPhase(phases []config.Phase, f startFacts) startChoice {
	if f.CloseOut {
		c := startAtName(phases, f, config.ImplementPhaseName, true,
			"close-out claim: implement with the close-out brief")
		c.CloseOut = true
		return c
	}
	// Without origin the recorded branch's state is unknowable, so resuming or
	// redoing it are both guesses: only the caller can end this cleanly.
	if !f.OriginKnown && f.HasWIP {
		return startChoice{Abort: true, Why: "origin unreadable while the task has a recorded branch: hand the claim back and end the drain as transient"}
	}
	// An unphased or single-phase run has no positions to choose between; its
	// one session carries its own prompt and is never a built-in phase.
	if len(phases) < 2 {
		return startChoice{Why: "single-phase sequence: there is no start phase to choose"}
	}
	switch {
	case !f.HasWIP && f.NoCodeDelivered:
		return startAtName(phases, f, config.ReviewPhaseName, false,
			"no WIP and a no-code delivery: branchless review")
	case !f.HasWIP:
		return startAtName(phases, f, config.ImplementPhaseName, true, "no WIP: implement")
	case !f.OnOrigin:
		return startAtName(phases, f, config.ImplementPhaseName, true,
			"the recorded branch is not on origin: implement")
	}

	atTip := countedAtTip(f.Reviews, f.Tip)
	open := len(f.OpenMustIDs) > 0
	switch {
	case f.OpenMustAtTip:
		return startAtName(phases, f, config.FixPhaseName, true,
			"open MUSTs on the released record at the tip: fix")
	case open:
		// The findings outlived the tip they were raised on — whether the fix is
		// pushed or the record at the tip is clean and did not re-raise them —
		// so the fix is not this session's job: whoever adjudicates the MUSTs is
		// the verifier when the sequence has a fix->verify pair, else the review.
		if i := verifyAfterFix(phases); i >= 0 {
			return startAtIndex(f, i, true, "open MUSTs with none on the record at the tip: verify")
		}
		return startAtName(phases, f, config.ReviewPhaseName, true,
			"open MUSTs with none on the record at the tip and no fix->verify pair: review")
	case atTip:
		return startAtIndex(f, handOverIndex(phases), true,
			"a released record at the tip with no open MUST: hand over to the owner of the terminal step")
	}
	if f.ImplementDoneTip != "" && f.ImplementDoneIsAncestor {
		return startAtName(phases, f, config.ReviewPhaseName, true,
			"implement marked done at an ancestor of the tip with no counted record at it: review")
	}
	return startAtName(phases, f, config.ImplementPhaseName, true,
		"WIP with no implement marker and no records: implement")
}

// startAtName resolves a phase name to its position. An absent phase falls back
// to 0 — there is nothing else to run — and the Why says which phase was
// missing, so a mis-shaped sequence shows in the log instead of silently
// running the wrong brief.
func startAtName(phases []config.Phase, f startFacts, name string, withBranch bool, why string) startChoice {
	i := phaseIndex(phases, name)
	if i < 0 {
		return startChoice{Why: "start at 0: the sequence has no " + name + " phase (" + why + ")"}
	}
	return startAtIndex(f, i, withBranch, why)
}

// startAtIndex fills the choice for a known position. Index 0 never carries a
// checkpoint: the phase there takes its own claim, so there is nothing to
// resume.
func startAtIndex(f startFacts, i int, withBranch bool, why string) startChoice {
	c := startChoice{Index: i, Why: why}
	if i == 0 {
		return c
	}
	c.Branchless = !withBranch
	c.Checkpoint.Tip = f.Tip
	if withBranch {
		c.Checkpoint.Branch = f.Branch
	}
	return c
}

// phaseIndex is the FIRST phase with this name. An expanded review panel is
// several consecutive phases named review and one position to start at — its
// first member — so a later member is never a start candidate.
func phaseIndex(phases []config.Phase, name string) int {
	for i, ph := range phases {
		if ph.Name == name {
			return i
		}
	}
	return -1
}

// handOverIndex is who owns the terminal step: the verify phase of a fix->verify
// pair, else the last phase. It is an index, not a name, because the last phase
// of a custom sequence need not have one.
func handOverIndex(phases []config.Phase) int {
	if i := verifyAfterFix(phases); i >= 0 {
		return i
	}
	return len(phases) - 1
}

// verifyAfterFix is the position of the verify phase immediately following a
// fix phase — the pair drain_phases.go repeats (its fixIdx). Keyed on NAMES
// because phase selection runs before EffectivePhases resolves prompts, and the
// resolved-prompt predicate isVerifyPhase cannot answer for a hand-built phase.
func verifyAfterFix(phases []config.Phase) int {
	at := -1
	for i := 1; i < len(phases); i++ {
		if phases[i].Name == config.VerifyPhaseName && phases[i-1].Name == config.FixPhaseName {
			at = i
		}
	}
	return at
}

// countedAtTip reports whether a released (State "open") record reviewed this
// exact tip. A sealed record's findings are withheld and a void record is an
// aborted round, so neither can hand work over. An empty tip matches nothing: a
// record with no reviewed tip is not evidence about a branch, and treating the
// two empties as equal would hand over work nobody reviewed.
func countedAtTip(records []plane.ReviewRecordState, tip string) bool {
	if tip == "" {
		return false
	}
	for _, r := range records {
		if r.State == "open" && r.ReviewedTip == tip {
			return true
		}
	}
	return false
}

// openMustIDs is the startFacts.OpenMustIDs list: open MUSTs on released records
// only, in record then finding order. The plane's openMustIds is not filtered by
// state, so a void record's MUSTs would otherwise count as work to do.
func openMustIDs(records []plane.ReviewRecordState) []string {
	var ids []string
	for _, r := range records {
		if r.State != "open" {
			continue
		}
		for _, f := range r.Findings {
			if f.Severity == "must" && f.Status == "open" {
				ids = append(ids, f.ID)
			}
		}
	}
	return ids
}

// openMustAtTip reports whether a released record reviewed this exact tip and
// carries an open MUST itself. Only that record's own findings count: an older
// record's MUST is not evidence that the tip needs fixing, because a review at
// the tip that found it clean did not re-raise it. Sealed records are withheld
// findings and void records are aborted rounds, so neither can speak.
func openMustAtTip(records []plane.ReviewRecordState, tip string) bool {
	if tip == "" {
		return false
	}
	for _, r := range records {
		if r.State != "open" || r.ReviewedTip != tip {
			continue
		}
		if len(r.Findings) == 0 {
			// A read trimmed to counts still knows the round raised MUSTs.
			if r.Counts.Must > 0 {
				return true
			}
			continue
		}
		for _, f := range r.Findings {
			if f.Severity == "must" && f.Status == "open" {
				return true
			}
		}
	}
	return false
}
