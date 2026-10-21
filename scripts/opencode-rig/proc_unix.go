//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup puts the session in its own process group, so a kill
// reaches opencode AND the stub server it spawned. Without it, killing opencode
// can leave the MCP child (and anything it spawned) holding the run's pipes.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup asks the whole group to stop. SIGTERM first: opencode can
// flush its session state and the stub can finish its log line.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}

// forceKillProcessGroup is the grace period's end.
func forceKillProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
