//go:build unix

package statedir

import (
	"os"
	"syscall"
)

// permissionBitsAreMeaningful says whether a directory's mode and owner are
// worth refusing over. On unix they are the whole basis of the checks in verify.
const permissionBitsAreMeaningful = true

// fileOwnerUID returns the owning uid of a stat'd file. The second result is
// false when the platform's FileInfo does not carry one, so callers treat
// "cannot tell" as "no opinion" rather than as a refusal.
func fileOwnerUID(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
