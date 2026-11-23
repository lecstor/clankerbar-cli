//go:build !unix

package harness

import "os/exec"

func setupProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup is a no-op on non-unix platforms.
func killProcessGroup(cmd *exec.Cmd) {}
