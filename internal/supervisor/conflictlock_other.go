//go:build !unix

package supervisor

import (
	"errors"
	"os"
)

// lockConflictSlotFile is the non-unix fallback: there is no flock(2) here, so the
// slot is an exclusively-created file (O_EXCL) removed on release. It is still
// atomic between processes — the create decides the race — but it has NO
// crash recovery: a holder that dies without releasing strands the slot until an
// operator removes the file, which the stand-down line names. The fleet's releases
// are darwin/linux, so this is the source-build path only.
func lockConflictSlotFile(path string) (*os.File, bool, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return file, true, nil
}

// unlockConflictSlotFile closes and removes the file, which is what frees the slot
// on this platform.
func unlockConflictSlotFile(file *os.File) {
	name := file.Name()
	_ = file.Close()
	_ = os.Remove(name)
}
