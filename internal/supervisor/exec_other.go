//go:build !unix

package supervisor

import "errors"

// execInPlace is unsupported off-unix: there is no exec(2) to replace the
// process image with. The launchd/systemd fallback is the documented
// replacement for operators on such platforms — the unit owns the restart.
func execInPlace(bin string, argv []string, env []string) error {
	return errors.New("replacing the supervisor in place is not supported on this platform - run it under launchd/systemd (the documented fallback) and restart the unit after installing the new build at the launch path")
}
