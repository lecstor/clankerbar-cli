//go:build !unix

package cli

import "errors"

// restartSelf has no syscall.Exec on this platform. The marker semantics still
// hold — the daemon stops cleanly and says why — so a restart degrades to a
// stop the operator (or their wrapper) relaunches, rather than failing silently.
func restartSelf(argv []string) error {
	return errors.New("restart requested, but re-exec is not supported on this platform - the loop has stopped; start it again by hand")
}
