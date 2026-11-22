//go:build !unix

package config

import "os"

// Windows and friends.
const permissionBitsAreMeaningful = false

func fileOwnerUID(os.FileInfo) (int, bool) { return 0, false }
