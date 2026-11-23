//go:build !unix

package harness

import "os/exec"

func exitSignal(*exec.ExitError) int { return 0 }

// SignalName falls back to the bare number where the conventional names are not known.
func SignalName(int) string { return "" }
