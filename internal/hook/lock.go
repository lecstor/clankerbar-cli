package hook

import (
	"os"
	"path/filepath"
	"time"
)

// acquireLock takes an exclusive lock on path, retrying until wait elapses. It
// reports whether the lock was taken; on failure the caller drops its event
// rather than blocking the session past the stated bound.
//
// The lock is held across the HTTP send, not just the counter update: the plane
// drops a seq at or below its high-water mark, so reserving in lock order is not
// enough — arrival order must match too.
func acquireLock(path string, wait time.Duration) (func(), bool) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false
	}
	deadline := time.Now().Add(wait)
	for {
		file, held, err := lockFile(path)
		if err != nil {
			return nil, false
		}
		if held {
			return func() { unlockFile(file) }, true
		}
		if !time.Now().Before(deadline) {
			return nil, false
		}
		time.Sleep(10 * time.Millisecond)
	}
}
