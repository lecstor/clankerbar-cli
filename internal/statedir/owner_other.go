//go:build !unix

package statedir

import "os"

// Windows and friends. Go synthesises a mode from the read-only attribute here,
// so the permission bits describe the platform rather than the directory and
// enforcing them would refuse every state dir. The releases are darwin/linux
// (.goreleaser.yaml), so this is the source-build path, and it says plainly that
// the mode- and owner-based guarantees do not hold on it. The symlink refusal
// and the O_EXCL writes still do.
const permissionBitsAreMeaningful = false

func fileOwnerUID(os.FileInfo) (int, bool) { return 0, false }
