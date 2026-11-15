//go:build unix

package harness

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
)

// SessionPrioritySupported reports whether this platform can start a session's
// process tree at a lowered scheduling priority. It is a build-tagged constant
// so config validation can refuse a dial that would otherwise be silently inert.
const SessionPrioritySupported = true

// sessionCommand builds the exec.Cmd for one session process, applying the
// invocation's scheduling priority to it.
//
// The priority is applied to the CHILD, not to this daemon: everything a session
// starts — test runners, builds, dev servers, MCP servers — is forked from the
// harness process and inherits the value, while the driver's own plane calls stay
// at its priority. The kernel then gives another process (a CI runner sharing the
// machine) the CPU when they contend, and gives the session the whole machine
// when nothing else wants it — what a nice value does that a CPU quota does not.
//
// nice(1) raises its own priority and then execs the target, so the child keeps
// one pid: the adapters' process-group kill, WaitDelay and Cancel all still see
// exactly the process they started.
//
// A value of 0 is the byte-for-byte old shape — a plain exec.CommandContext with
// no wrapper between the daemon and the harness.
func sessionCommand(ctx context.Context, niceValue int, name string, args ...string) (*exec.Cmd, error) {
	if niceValue <= 0 {
		return exec.CommandContext(ctx, name, args...), nil
	}
	// Resolve nice(1) here rather than letting exec fail on Start, so a machine
	// without it reports the missing dependency instead of a session that dies
	// with the harness's name in the error.
	bin, err := exec.LookPath("nice")
	if err != nil {
		return nil, fmt.Errorf("session_nice %d needs nice(1) on PATH: %w", niceValue, err)
	}
	argv := make([]string, 0, len(args)+3)
	argv = append(argv, "-n", strconv.Itoa(niceValue), name)
	argv = append(argv, args...)
	return exec.CommandContext(ctx, bin, argv...), nil
}
