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
// priority to it. A negative value is REFUSED: it would ask to raise the session above the daemon,
// which config validation does not allow and which would fail for an unprivileged daemon anyway.
func sessionCommand(ctx context.Context, niceValue int, name string, args ...string) (*exec.Cmd, error) {
	if niceValue < 0 {
		return nil, fmt.Errorf("session_nice %d is negative: a session can only be lowered below the daemon, never raised above it", niceValue)
	}
	if niceValue == 0 {
		return exec.CommandContext(ctx, name, args...), nil
	}
	// Resolve BOTH binaries with the DAEMON's PATH. nice(1) would otherwise resolve the harness
	// through the CHILD's PATH, and a session env allowlist that drops PATH (an operator's declared
	// scope) would then fail to start a harness that resolves perfectly well for the daemon - a
	// behaviour change the wrapper must not introduce.
	harness, err := exec.LookPath(name)
	if err != nil {
		return nil, err
	}
	bin, err := exec.LookPath("nice")
	if err != nil {
		return nil, fmt.Errorf("session_nice %d needs nice(1) on PATH: %w", niceValue, err)
	}
	argv := make([]string, 0, len(args)+3)
	argv = append(argv, "-n", strconv.Itoa(niceValue), harness)
	argv = append(argv, args...)
	return exec.CommandContext(ctx, bin, argv...), nil
}
