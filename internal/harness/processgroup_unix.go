//go:build unix

package harness

import (
	"os/exec"
	"syscall"
)

func setupProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}

// killProcessGroup sends SIGKILL to the negative PID (the whole process group), so descendants that
// survived the direct-child kill are also terminated. Descendants get no chance to clean up (a
// grandchild build leaves partial artefacts) - accepted, rather than spend opencode's 5s WaitDelay
// budget on a graceful shutdown we cannot bound.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil && cmd.Process.Pid > 0 {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
