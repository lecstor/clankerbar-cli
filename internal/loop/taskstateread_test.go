package loop

import (
	"context"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// taskReadReleaser is a fakeReleaser that can also read the queue head and one
// task's holder state — the shape the real mcpReleaser has since CLA-437 and
// CLA-636. `state` answers TaskState per task id; a missing id decodes to a zero
// TaskState (empty status, no holder, no branch), like the plane's nulls.
type taskReadReleaser struct {
	fakeReleaser
	next     plane.NextTask
	state    map[string]plane.TaskState
	stateErr error
	asked    []string // task ids TaskState was called with, in order
	// CLA-637: the round close the review→fix seam performs, and an error a
	// test can inject to drive the close-failure refusal.
	closes   []int
	closeErr error
}

func (r *taskReadReleaser) PeekNextTask(context.Context) (plane.NextTask, error) {
	return r.next, nil
}

func (r *taskReadReleaser) CloseReviewRound(_ context.Context, _, _ string, round int) error {
	if r.closeErr != nil {
		return r.closeErr
	}
	r.closes = append(r.closes, round)
	return nil
}

func (r *taskReadReleaser) TaskState(_ context.Context, taskID string) (plane.TaskState, error) {
	r.asked = append(r.asked, taskID)
	if r.stateErr != nil {
		return plane.TaskState{}, r.stateErr
	}
	return r.state[taskID], nil
}

// taskReadDriver builds the two-phase driver the plane-record evidence tests
// drive: phase 1's session ends cleanly holding the task, and get_task answers
// whatever the test put in `state`.
func taskReadDriver(t *testing.T, h *fakeAdapter, rel *taskReadReleaser) *Driver {
	t.Helper()
	cfg := fastCfg()
	cfg.Phases = twoPhases()
	cfg.Prompt = ""
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: rel}})
	openTestStateDir(t, d)
	// Same stub the other seam tests use: a worktree-shaped workdir and a
	// BranchPushed check that passes. Tests that exercise the evidence gate
	// override this.
	d.cfg.WorkDir = t.TempDir()
	d.newVerifier = func(string, bool) deliveryVerifier { return passVerifier() }
	return d
}
