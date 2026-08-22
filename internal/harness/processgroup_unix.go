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
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
