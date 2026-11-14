package loop

// task_worktree_claim.go — the READ-ONLY pre-claim verdict on a task's existing
// worktree, so a deterministic local refusal is found BEFORE the claim: a claim
// that cannot be honoured costs a reclaim, and two consecutive ones park the
// task (CLA-847).

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// claimWorktreeRefusal is the pre-claim, READ-ONLY answer for one task: the
// deterministic reason the task's existing worktree cannot be prepared for a
// claim, or "" when taking the claim is worth it. It moves, fetches-to-reset and
// prunes nothing, so a refusal here happens BEFORE the claim and costs no
// reclaim — the park that two refused claims produce is what this exists to
// prevent.
//
// Only what can be proven is refused. Everything judged against origin is
// decided after one fetch; a fresh tree at a PR base the request does not carry
// is deferred to the post-claim prep, which resolves that base itself. A check
// that cannot run is not a refusal: the prep repeats the same decision with the
// same facts, and a transient failure must not starve the task.
//
// The origin facts it fetched are returned so the post-claim prep can reuse them
// instead of paying a second round-trip.
func claimWorktreeRefusal(ctx context.Context, req worktreeRequest, doneTip string) (string, originBranchFacts, error) {
	id := strings.TrimSpace(req.TaskID)
	if strings.TrimSpace(req.Checkout) == "" || strings.TrimSpace(req.Branch) == "" || len(id) < 8 || !isHex(id[:8]) {
		return "", originBranchFacts{}, nil
	}
	registered, err := taskWorktreeWorktrees(ctx, req.Checkout)
	if err != nil {
		return "", originBranchFacts{}, err
	}
	var main string
	if len(registered) > 0 {
		main = registered[0].path
	}
	dir := taskWorktreeDir(req, main)
	wt, found := taskWorktreeExisting(registered, dir, req.Branch)
	if !found {
		// A new tree is cut from origin, so its deterministic refusals are
		// origin facts too: proving them here keeps them off the reclaim.
		return claimWorktreeCreateRefusal(ctx, req, dir, doneTip)
	}
	switch {
	case main != "" && taskWorktreeSamePath(wt.path, main):
		return fmt.Sprintf("the branch %s is checked out in the main checkout %s", req.Branch, wt.path), originBranchFacts{}, nil
	case taskWorktreeSamePath(wt.path, req.Checkout):
		return fmt.Sprintf("the branch %s is checked out in %s, this run's own checkout", req.Branch, wt.path), originBranchFacts{}, nil
	case wt.detached:
		return fmt.Sprintf("worktree %s is detached, not on %s", wt.path, req.Branch), originBranchFacts{}, nil
	case wt.branch != req.Branch:
		return fmt.Sprintf("worktree %s is on %s, not %s", wt.path, wt.branch, req.Branch), originBranchFacts{}, nil
	}
	tracked, err := taskWorktreeTrackedSummary(ctx, wt.path)
	if err != nil {
		return "", originBranchFacts{}, err
	}
	if tracked != "" {
		return fmt.Sprintf("worktree %s has uncommitted changes (%s)", wt.path, tracked), originBranchFacts{}, nil
	}
	untracked, err := taskWorktreeUntracked(ctx, wt.path)
	if err != nil {
		return "", originBranchFacts{}, err
	}
	// Untracked files need origin to prove they are clearable, and a resumed or
	// close-out tree is synced against origin by the prep.
	var facts originBranchFacts
	if len(untracked) > 0 || req.Resume || req.CloseOut {
		if _, err := taskWorktreeGit(ctx, req.Checkout, "fetch", "origin", "--prune"); err != nil {
			return "", originBranchFacts{}, fmt.Errorf("%w in %s: %v", errWorktreeFetch, req.Checkout, err)
		}
		facts = readOriginBranchFetched(ctx, req.Checkout, req.Branch, doneTip)
	}
	remote := "origin/" + req.Branch
	onOrigin := taskWorktreeCommitishExists(ctx, wt.path, remote)
	if (req.Resume || req.CloseOut) && !onOrigin {
		return fmt.Sprintf("the branch %s is not on origin", req.Branch), facts, nil
	}
	if len(untracked) > 0 {
		if !onOrigin {
			// A fresh tree at the PR base may be safe to clear; the post-claim
			// prep resolves that base, so defer rather than guess.
			return "", facts, nil
		}
		ref, err := taskWorktreeClearedRef(ctx, req, wt.path)
		if err != nil {
			return "", facts, err
		}
		if ref == "" {
			return fmt.Sprintf("worktree %s has untracked files (%s) and %s",
				wt.path, taskWorktreeSummary(untracked), taskWorktreeUnclearableReason(ctx, req, wt.path)), facts, nil
		}
	}
	if req.Resume || req.CloseOut {
		out, err := taskWorktreeGit(ctx, wt.path, "rev-list", "--left-right", "--count", "HEAD..."+remote)
		if err != nil {
			return "", facts, err
		}
		ahead, behind, err := taskWorktreeCounts(out)
		if err != nil {
			return "", facts, err
		}
		if ahead > 0 && behind > 0 {
			return fmt.Sprintf("worktree %s has diverged from %s (%d local and %d remote commit(s))",
				wt.path, remote, ahead, behind), facts, nil
		}
	}
	return "", facts, nil
}

// taskWorktreeExisting finds the tree a prep would reuse: the one at the
// deterministic path, else one already checked out on the branch elsewhere.
func taskWorktreeExisting(list []registeredWorktree, dir, branch string) (registeredWorktree, bool) {
	for _, wt := range list {
		if taskWorktreeSamePath(wt.path, dir) {
			return wt, true
		}
	}
	return taskWorktreeOnBranch(list, branch)
}

// claimWorktreeCreateRefusal proves the CREATE path's deterministic refusals
// before the claim: each one fails identically inside prepareTaskWorktree, so
// finding it here costs no reclaim. A check whose facts this request does not
// carry is deferred — the prep repeats the decision after the claim.
func claimWorktreeCreateRefusal(ctx context.Context, req worktreeRequest, dir, doneTip string) (string, originBranchFacts, error) {
	if _, err := os.Stat(dir); err == nil {
		return fmt.Sprintf("%s already exists but is not a worktree of %s — refusing to reuse it", dir, req.Checkout), originBranchFacts{}, nil
	} else if !os.IsNotExist(err) {
		return "", originBranchFacts{}, err
	}
	if !req.Resume && !req.CloseOut && strings.TrimSpace(req.PRBase) == "" {
		// The caller resolved no base, so a missing origin/<base> cannot be
		// proven here; the prep resolves one after the claim.
		return "", originBranchFacts{}, nil
	}
	if _, err := taskWorktreeGit(ctx, req.Checkout, "fetch", "origin", "--prune"); err != nil {
		return "", originBranchFacts{}, fmt.Errorf("%w in %s: %v", errWorktreeFetch, req.Checkout, err)
	}
	facts := readOriginBranchFetched(ctx, req.Checkout, req.Branch, doneTip)
	if req.Resume || req.CloseOut {
		// taskWorktreeAddExisting needs the branch on origin and only
		// fast-forwards a local one.
		remote := "origin/" + req.Branch
		if !taskWorktreeCommitishExists(ctx, req.Checkout, remote) {
			return fmt.Sprintf("the branch %s is not on origin", req.Branch), facts, nil
		}
		reason, err := claimWorktreeLocalBranchRefusal(ctx, req.Checkout, req.Branch, remote)
		return reason, facts, err
	}
	// taskWorktreeAddNew cuts from the PR base and reuses a local branch only
	// when it holds nothing that base lacks.
	base := "origin/" + req.PRBase
	if !taskWorktreeCommitishExists(ctx, req.Checkout, base) {
		return fmt.Sprintf("the PR base %s is not on origin", req.PRBase), facts, nil
	}
	reason, err := claimWorktreeLocalBranchRefusal(ctx, req.Checkout, req.Branch, base)
	return reason, facts, err
}

// claimWorktreeLocalBranchRefusal names a local branch the create path would
// refuse to reset: one holding commits the branch would be cut from does not
// have. A missing local branch is not a refusal.
func claimWorktreeLocalBranchRefusal(ctx context.Context, checkout, branch, against string) (string, error) {
	if !taskWorktreeCommitishExists(ctx, checkout, "refs/heads/"+branch) {
		return "", nil
	}
	if _, err := taskWorktreeGit(ctx, checkout, "merge-base", "--is-ancestor", "refs/heads/"+branch, against); err != nil {
		if !negativeFact(err) {
			return "", err
		}
		return fmt.Sprintf("the local branch %s holds commits %s does not have", branch, against), nil
	}
	return "", nil
}
