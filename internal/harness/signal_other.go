//go:build !unix

package harness

import "os/exec"

// exitSignal reports no signal on a non-unix platform, where the child's wait
// status carries no POSIX signal. This is the honest "not a signalled exit",
// not a gap: the loop only ever renders the signal when it is non-zero.
func exitSignal(*exec.ExitError) int { return 0 }

// SignalName falls back to the bare number where the conventional names are
// not known.
func SignalName(int) string { return "" }
