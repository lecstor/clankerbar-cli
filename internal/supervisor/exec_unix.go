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
}

// processAlive reports whether pid names a live process. EPERM counts as
// alive: the process exists and belongs to somebody else.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
