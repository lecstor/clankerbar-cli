//go:build unix

package harness

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
)

// SessionPrioritySupported reports whether this platform can start a session's process tree at a
// lowered scheduling priority.
const SessionPrioritySupported = true

// sessionCommand builds the exec.Cmd for one session process, applying the invocation's scheduling
// priority to it.
func sessionCommand(ctx context.Context, niceValue int, name string, args ...string) (*exec.Cmd, error) {
	if niceValue < 0 {
		return nil, fmt.Errorf("session_nice %d is negative: a session can only be lowered below the daemon, never raised above it", niceValue)
	}
	if niceValue == 0 {
		return exec.CommandContext(ctx, name, args...), nil
	}
	// Resolve BOTH binaries with the DAEMON's PATH.
	harness, err := exec.LookPath(name)
	if err != nil {
		return nil, err
	}
	// Resolving nice(1) here rather than letting exec fail on Start reports a machine without it as
	// the missing dependency it is, rather than as a harness that died.
	bin, err := exec.LookPath("nice")
	if err != nil {
		return nil, fmt.Errorf("session_nice %d needs nice(1) on PATH: %w", niceValue, err)
	}
	argv := make([]string, 0, len(args)+3)
	argv = append(argv, "-n", strconv.Itoa(niceValue), harness)
	argv = append(argv, args...)
	return exec.CommandContext(ctx, bin, argv...), nil
}
