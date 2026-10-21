//go:build windows

package main

import "os/exec"

// Windows has no process groups in the unix sense; killing the top process is
// the best available bound, and the rig is a developer tool aimed at the unix
// hosts the fleet runs on.
func configureProcessGroup(cmd *exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func forceKillProcessGroup(cmd *exec.Cmd) { killProcessGroup(cmd) }
