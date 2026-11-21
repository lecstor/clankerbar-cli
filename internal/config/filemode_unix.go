//go:build unix

package config

import (
	"os"
	"syscall"
)

// permissionBitsAreMeaningful says whether a file's mode is worth refusing over. On unix it is the
// whole basis of the trust checks in vetTrustedFile.
const permissionBitsAreMeaningful = true

// fileOwnerUID returns the owning uid of a stat'd file.
func fileOwnerUID(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
