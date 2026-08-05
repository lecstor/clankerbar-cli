//go:build !unix

package config

import "os"

// Windows and friends. Go synthesises 0666 (or 0444 for a read-only file) for an
// ordinary file here, so the permission bits describe the platform rather than the
// file: enforcing them would refuse every config and every @path secret, making
// the tool unusable rather than safer. The releases are darwin/linux
// (.goreleaser.yaml), so this is the source-build path, and it says plainly that
// the mode-based guarantees do not hold on it.
const permissionBitsAreMeaningful = false

func fileOwnerUID(os.FileInfo) (int, bool) { return 0, false }
