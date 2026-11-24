package harness

import (
	"errors"
	"os/exec"
)

// classifyWait folds cmd.Wait's error onto res and returns what Invoke should propagate: nil once
// the death is accounted for, the raw error when claude plainly failed to run at all. Extracted so
// the driver-kill guards below can be driven directly: their true side needs a marker raised in the
// window between the child's exit and its reap, which no stub can schedule deterministically.
func (c claude) classifyWait(waitErr error, res *Result) error {
	if ee, ok := waitErr.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
		res.ExitSignal = exitSignal(ee)
		return nil
	}
	// A driver kill is load-bearing, not belt-and-braces: raised after the child exited and was
	// reaped, cmd.Cancel's Process.Kill reads os.ErrProcessDone, os/exec injects no error for it,
	// and Wait returns ErrWaitDelay from the surviving inherited pipe exactly as an unkilled clean
	// exit would. The guards keep that death off the clean-end path; its exit code is the kill's,
	// not a verdict.
	if c.TokenCeilingHit(*res) || res.ToolLoop != "" || res.IdleKilled != "" {
		if waitErr != nil {
			res.ExitCode = -1
		}
		return nil
	}
	if errors.Is(waitErr, exec.ErrWaitDelay) {
		// The session exited 0 and only an inherited pipe outlived it: os/exec guarantees ErrWaitDelay
		// means a successful exit, so this is the finished session, never an invoke error (CLA-430,
		// symmetric with opencode's CLA-414 arm). ExitCode stays 0 for the driver's success path.
		return nil
	}
	if waitErr != nil {
		return waitErr // couldn't run claude at all
	}
	return nil
}
