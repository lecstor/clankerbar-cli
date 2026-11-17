//go:build unix

package hook

import (
	"errors"
	"os"
	"syscall"
)

// lockFile takes a non-blocking exclusive advisory lock on path. flock is the
// right primitive here: it is atomic, per open file description (so two
// invocations in one process contend too), and the kernel releases it when a
// killed hook process exits — no stale lease to time out.
func lockFile(path string) (*os.File, bool, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		return file, true, nil
	case errors.Is(err, syscall.EWOULDBLOCK):
		file.Close()
		return nil, false, nil
	default:
		file.Close()
		return nil, false, err
	}
}

func unlockFile(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}
