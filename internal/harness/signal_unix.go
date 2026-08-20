//go:build unix

package harness

import (
	"os/exec"
	"syscall"
)

// exitSignal returns the number of the signal a finished child died on, or 0
// for a normal exit. The wait status only knows a signal on platforms with a
// POSIX-ish WaitStatus (darwin, linux); elsewhere exitSignal always reports
// none, which is the honest "not a signalled exit".
//
// It exists as a function rather than an inline type-assertion so the unix-only
// syscall decoding stays behind the build tag, and an adapter's ExitError
// branch stays one line.
func exitSignal(ee *exec.ExitError) int {
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return int(ws.Signal())
	}
	return 0
}

// SignalName renders a signal number for a log line. The common ones get their
// conventional names — the whole point of recording the signal is to tell an
// OOM/SIGKILL from a SIGSEGV crash at a glance — and anything else falls back
// to the bare number.
func SignalName(n int) string {
	switch syscall.Signal(n) {
	case syscall.SIGHUP:
		return "SIGHUP"
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGQUIT:
		return "SIGQUIT"
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGSEGV:
		return "SIGSEGV"
	case syscall.SIGABRT:
		return "SIGABRT"
	case syscall.SIGBUS:
		return "SIGBUS"
	case syscall.SIGPIPE:
		return "SIGPIPE"
	}
	return ""
}
