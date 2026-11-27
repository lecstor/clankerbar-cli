//go:build !unix

package auth

import "os"

// Windows and friends. Go synthesises a mode from the read-only attribute here,
// so the permission bits describe the platform rather than the file and
// enforcing them would refuse every store. The symlink refusal and the atomic
// 0600 temp-file write still hold.
const permissionBitsAreMeaningful = false

func fileOwnerUID(os.FileInfo) (int, bool) { return 0, false }
