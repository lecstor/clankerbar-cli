//go:build !unix

package harness

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
)

// SessionPrioritySupported reports whether this platform can start a session's process tree at a
// lowered scheduling priority. A non-unix platform has no nice(1)/setpriority, so config validation
// refuses a positive session_nice there rather than letting the dial be silently inert.
const SessionPrioritySupported = false

// sessionCommand builds the exec.Cmd for one session process.
func sessionCommand(ctx context.Context, niceValue int, name string, args ...string) (*exec.Cmd, error) {
	if niceValue < 0 {
		return nil, fmt.Errorf("session_nice %d is negative: a session can only be lowered below the daemon, never raised above it", niceValue)
	}
	if niceValue > 0 {
		return nil, fmt.Errorf("session_nice %d is unsupported on %s: no nice(1) to lower a session's scheduling priority with", niceValue, runtime.GOOS)
	}
	return exec.CommandContext(ctx, name, args...), nil
}
