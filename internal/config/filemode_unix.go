//go:build unix

package config

import (
	"os"
	"syscall"
)

// permissionBitsAreMeaningful says whether a file's mode is worth refusing over.
// On unix it is the whole basis of the trust checks in vetTrustedFile.
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
