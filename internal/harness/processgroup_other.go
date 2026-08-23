//go:build !unix

package harness

import "os/exec"

// setupProcessGroup is a no-op on non-unix platforms; process groups are
// a POSIX mechanism and have no direct equivalent elsewhere.
func setupProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup is a no-op on non-unix platforms.
func killProcessGroup(cmd *exec.Cmd) {}
