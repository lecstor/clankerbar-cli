//go:build !unix

package hook

import (
	"errors"
	"os"
	"time"
)

// staleLockAge is how old an O_EXCL lock file must be before it is treated as
// left by a crashed hook. A live hook holds it for at most the send bound, so a
// minute is comfortably clear.
const staleLockAge = time.Minute

// lockFile is the non-unix fallback: an exclusively-created file. There is no
// kernel lock release here, so a holder that dies without releasing strands the
// lock until staleLockAge has passed; the retry loop gives up long before that,
// which drops one event rather than blocking the session.
func lockFile(path string) (*os.File, bool, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err == nil {
		return file, true, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, false, err
	}
	if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > staleLockAge {
		if rmErr := os.Remove(path); rmErr == nil {
			return lockFile(path)
		}
	}
	return nil, false, nil
}

func unlockFile(file *os.File) {
	name := file.Name()
	_ = file.Close()
	_ = os.Remove(name)
}
