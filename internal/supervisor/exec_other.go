//go:build !unix

package supervisor

import "errors"

// execInPlace is unsupported off-unix: there is no exec(2) to replace the
// process image with. The launchd/systemd fallback is the documented
// replacement for operators on such platforms — the unit owns the restart —
// and the drain path in replace_drain.go is what the marker honours here.
func execInPlace(bin string, argv []string, env []string) error {
	return errors.New("replacing the supervisor in place is not supported on this platform - run it under launchd/systemd (the documented fallback) and restart the unit after installing the new build at the launch path")
}

// replaceNow is the phase-5c replacement off-unix: with no exec(2) the
// documented path is the unit restart, and the marker honours it by draining
// the fleet first (replace_drain.go).
func (d *Supervisor) replaceNow() {
	d.replaceByDrain()
}

// waitPid is unsupported off-unix: adoption never runs here, because there is
// no exec(2) for children to survive.
func waitPid(pid int) error {
	return errors.New("waiting on an adopted child is not supported on this platform")
}

// probeExited is unsupported off-unix, for the same reason: no adopted child
// exists to probe.
func probeExited(pid int) (bool, error) {
	return false, nil
}

// processAlive is unsupported off-unix, for the same reason.
func processAlive(pid int) bool {
	return false
}
