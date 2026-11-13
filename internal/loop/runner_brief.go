package loop

import (
	"fmt"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// runnerBriefing is what a runner-claimed drain hands each phase session up front.
type runnerBriefing struct {
	task     plane.TaskBrief
	runID    string
	prep     worktreePrep // only on the phase the walk started at; zero otherwise
	baseSHA  string       // merge-base of origin/<prBase> and the tip; every phase, for its diffs
	start    bool         // this phase is the one the walk started at
	closeOut bool         // close-out claim (only ever true with start)
}

// runnerPrompt swaps a variant-resolved shipped brief for its runner-claimed
// counterpart. The runner owns the claim, the worktree and the lease, so the
// shipped text that tells a session to do those things would be wrong.
//
// Everything else is left alone: a custom prompt owns its own wording, a panel
// member does its own job, and the no-code and unverified variants still work in
// a resumed run (runner versions of those are a follow-up).
func runnerPrompt(prompt string, rb *runnerBriefing, panelMember bool) string {
	if rb == nil || panelMember {
		return prompt
	}
	return runnerBriefSwap(prompt, rb.closeOut)
}

// runnerBriefSwap is the one mapping from a shipped brief to its runner
// counterpart. It exists apart from runnerPrompt for the caller that must name a
// phase's runner brief WITHOUT a briefing: the done-report note a handoff
// successor is given is chosen from the captured phase, not from a live spawn.
func runnerBriefSwap(prompt string, closeOut bool) string {
	switch prompt {
	case config.BuiltinImplementBrief():
		if closeOut {
			return config.RunnerCloseOutBrief()
		}
		return config.RunnerImplementBrief()
	case config.ReviewOnlyBrief():
		return config.RunnerReviewOnlyBrief()
	case config.FixBrief():
		return config.RunnerFixBrief()
	case config.FixVerifyBrief():
		return config.RunnerFixVerifyBrief()
	case config.VerifyBrief():
		return config.RunnerVerifyBrief()
	}
	return prompt
}

// runnerTaskContext fills the block a runner-claimed session starts with. The
// prep facts are the START phase's only: the runner prepared the tree once,
// before the walk, so a later phase reporting them would describe a merge it
// did not see happen.
func runnerTaskContext(rb *runnerBriefing, phases []config.Phase, phaseIdx int, worktree, branch, prBase, tip string) config.TaskContext {
	c := config.TaskContext{
		Ref:         rb.task.Ref,
		Title:       rb.task.Title,
		TaskID:      rb.task.ID,
		RunID:       rb.runID,
		Detail:      rb.task.Detail,
		DoneWhen:    rb.task.DoneWhen,
		PhaseNumber: phaseIdx + 1,
		PhaseTotal:  len(phases),
		Worktree:    worktree,
		Branch:      branch,
		PRBase:      prBase,
		DiffBase:    diffBase(prBase, rb.baseSHA),
		Tip:         tip,
		CloseOut:    rb.closeOut,
	}
	if phaseIdx >= 0 && phaseIdx < len(phases) {
		c.PhaseName = phases[phaseIdx].Label(phaseIdx)
	}
	if rb.closeOut {
		c.MergeHint = rb.task.MergeHint
	}
	for _, d := range rb.task.Decisions {
		c.Decisions = append(c.Decisions, struct{ Question, Answer string }{Question: d.Question, Answer: d.Answer})
	}
	for _, q := range rb.task.Questions {
		c.Questions = append(c.Questions, struct {
			Body, Answer string
			Blocking     bool
		}{Body: q.Body, Answer: q.Answer, Blocking: q.Blocking})
	}
	if rb.start {
		c.Resumed = rb.task.HasWIP
		switch {
		case rb.prep.Merged:
			c.Merge = "merged"
		case rb.prep.UpToDate:
			c.Merge = "up-to-date"
		}
		c.Conflicts = rb.prep.Conflicts
	}
	c.RunnerNext = runnerNextSentence(rb, phases, phaseIdx)
	return c
}

// runnerNextSentence states what the runner does after this session, which is
// the one piece of the walk a session cannot infer from its own brief.
func runnerNextSentence(rb *runnerBriefing, phases []config.Phase, phaseIdx int) string {
	// A close-out ends the run whatever the configured sequence says: the
	// approved task is handed back with release_task, not carried on.
	if rb.closeOut {
		return "When this session ends, the close-out is over and the runner ends the run."
	}
	if phaseIdx+1 < len(phases) {
		return fmt.Sprintf("When this session ends, the runner checks this phase's evidence (the pushed branch, or the durable review record) and then starts phase %d of %d (%s).",
			phaseIdx+2, len(phases), phases[phaseIdx+1].Label(phaseIdx+1))
	}
	return "This is the last phase: when this session ends, the runner checks the hand-over and the run ends."
}
