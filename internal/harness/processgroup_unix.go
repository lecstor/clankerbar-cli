//go:build unix

package harness

import (
	"os/exec"
	"syscall"
)

// setupProcessGroup sets the command to start in its own process group,
// so a group signal reaches descendants (grandchildren holding inherited
// file descriptors, background bash commands, MCP servers) rather than
// only the direct child.
func setupProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}

// killProcessGroup sends SIGKILL to the negative PID (the whole process
// group), so descendants that survived the direct-child kill are also
// terminated. It is a no-op when the process has not yet started.
//
// Straight SIGKILL with no SIGTERM grace period is deliberate: the direct
// child already got SIGKILL from CommandContext's default cancel, so the
// subtree had no grace before this change either, and a half-dead tree is
// exactly what holds pipes open past the kill. Descendants get no chance to
// clean up (a grandchild build leaves partial artefacts) - accepted, rather
// than spend opencode's 5s WaitDelay budget on a graceful shutdown we cannot
// bound.
func killProcessGroup(cmd *exec.Cmd) {
	// Pid > 0 matters: -pid with pid == 0 is kill(0), which POSIX addresses
	// to the CALLER's process group - SIGKILL to clankerbar-cli itself and
	// everything sharing its group.
	if cmd.Process != nil && cmd.Process.Pid > 0 {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
