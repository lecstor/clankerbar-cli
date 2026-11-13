package loop

// The origin half of the start-phase facts: the fetch and the questions the
// start-phase table asks of it. A WIP task's read is the drain's one fetch — the
// worktree prep is told so with worktreeRequest.Fetched.

import (
	"context"
	"errors"
	"os/exec"
	"strings"
)

// originBranchFacts is what origin says about the task's branch.
type originBranchFacts struct {
	Known          bool // the fetch and the reads worked, so the other fields are facts
	OnOrigin       bool
	Tip            string // origin/<branch>, empty when not on origin
	DoneIsAncestor bool   // doneTip is Tip or an ancestor of it
}

// readOriginBranch asks origin about branch and where doneTip sits relative to
// it. Known=false means git could not answer, so NOTHING below it is a fact: the
// caller must not read an empty field as "not on origin".
func readOriginBranch(ctx context.Context, checkout, branch, doneTip string) originBranchFacts {
	if _, err := taskWorktreeGit(ctx, checkout, "fetch", "origin", "--prune"); err != nil {
		return originBranchFacts{}
	}
	return readOriginBranchFetched(ctx, checkout, branch, doneTip)
}

// readOriginBranchFetched is readOriginBranch's questions after a fetch the
// caller already ran: the pre-claim worktree check fetches once and hands the
// facts on, so a resumed claim does not pay a second round-trip.
func readOriginBranchFetched(ctx context.Context, checkout, branch, doneTip string) originBranchFacts {
	f := originBranchFacts{Known: true}
	if branch == "" {
		// No recorded branch: nothing about origin can be asked.
		return f
	}
	out, err := taskWorktreeGit(ctx, checkout, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch)
	if err != nil {
		if !negativeFact(err) {
			return originBranchFacts{}
		}
		return f
	}
	if strings.TrimSpace(out) == "" {
		return f
	}
	f.OnOrigin = true
	f.Tip = strings.TrimSpace(out)
	switch {
	case doneTip == "":
	case doneTip == f.Tip:
		f.DoneIsAncestor = true
	default:
		if _, err := taskWorktreeGit(ctx, checkout, "merge-base", "--is-ancestor", doneTip, f.Tip); err != nil {
			if !negativeFact(err) {
				return originBranchFacts{}
			}
			return f
		}
		f.DoneIsAncestor = true
	}
	return f
}

// negativeFact reports whether err is git's plain "this is not so" exit (status
// 1). Every other failure — a missing repository, an unknown object — says
// nothing about the branch, and reading it as a negative would resume or redo the
// recorded work on a guess.
func negativeFact(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == 1
}
