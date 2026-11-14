//go:build unix

package supervisor

import (
	"errors"
	"fmt"
	"syscall"
)

// execInPlace replaces this process image with the binary at the fleet's
// launch path, invoked with the same arguments (argv[0] re-pointed at the
// binary) and the same environment — the same re-exec the daemon's own
// restart performs (cli.restartSelf), so the operator's launch arrangement is
// preserved across the replacement. On success this call never returns: the
// fresh supervisor starts from the same command line, with the same unit
// watching the same pid. An error leaves the OLD supervisor running, which
// the caller logs and resumes the loop from.
func execInPlace(bin string, argv []string, env []string) error {
	return syscall.Exec(bin, append([]string{bin}, argv[1:]...), env)
}

// replaceNow is the phase-5c replacement on a platform with exec(2): hand the
// live children to the incoming image, then exec at once.
func (d *Supervisor) replaceNow() {
	d.replaceByAdoption()
}

// waitPid waits for a child inherited across exec(2) to end, because the
// exec.Cmd.Wait goroutine that would have reaped it died with the old image.
// A zero exit returns nil; a non-zero exit or a signal returns an error with
// the status the status surface reports. ECHILD is wrapped so the caller can
// tell "not our child" (a reused pid) from a real wait failure.
func waitPid(pid int) error {
	var ws syscall.WaitStatus
	for {
		_, err := syscall.Wait4(pid, &ws, 0, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			if errors.Is(err, syscall.ECHILD) {
				return fmt.Errorf("%w: %v", errNotAChild, err)
			}
			return err
		}
		return waitStatusErr(ws)
	}
}

// probeExited reaps a child that has ALREADY exited, without blocking: the
// exec-window case, where the handoff names a child that died before the new
// image got to wait on it. reaped is false while the child still runs, and the
// caller then blocks in waitPid. ECHILD is deliberately left to waitPid to
// report (as errNotAChild), so a reused or foreign pid keeps that same path.
func probeExited(pid int) (reaped bool, err error) {
	var ws syscall.WaitStatus
	for {
		got, werr := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
		if errors.Is(werr, syscall.EINTR) {
			continue
		}
		if werr != nil {
			return false, nil
		}
		if got == 0 {
			return false, nil
		}
		return true, waitStatusErr(ws)
	}
}

// waitStatusErr renders wait4's status the way exec.ExitError does: a zero
// exit is nil, a non-zero exit keeps its code, and a signal is code -1.
func waitStatusErr(ws syscall.WaitStatus) error {
	switch {
	case ws.Exited():
		if code := ws.ExitStatus(); code != 0 {
			return &adoptedExitError{code: code, msg: fmt.Sprintf("exit status %d", code)}
		}
		return nil
	case ws.Signaled():
		return &adoptedExitError{code: -1, msg: fmt.Sprintf("signal: %s", ws.Signal())}
	default:
		return nil
	}
}

// processAlive reports whether pid names a live process. EPERM counts as
// alive: the process exists and belongs to somebody else.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
