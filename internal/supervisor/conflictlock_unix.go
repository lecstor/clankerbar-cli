//go:build unix

package supervisor

import (
	"errors"
	"os"
	"syscall"
)

// lockConflictSlotFile takes an exclusive advisory lock on path WITHOUT blocking,
// creating the file if it does not exist. It reports (file, false, nil) when
// another open file description already holds the lock.
//
// flock is the right primitive here rather than a lease file with an expiry:
//
//   - it is ATOMIC (one syscall decides the race, so two supervisors can never both
//     come back holding it, however they interleave);
//   - it is PER OPEN FILE DESCRIPTION, so a second acquisition inside the SAME
//     process contends exactly like another process does — which is what makes two
//     Supervisor values in one process honour the same bound;
//   - the kernel drops the lock when the holding process exits, so a crashed or
//     killed session-supervisor NEVER strands the fleet's slot. That is the
//     stale-holder recovery, and it cannot be got wrong by a clock, a pid that was
//     reused, or a lease that was not renewed in time.
func lockConflictSlotFile(path string) (*os.File, bool, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
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

// unlockConflictSlotFile releases the lock and closes the file. Closing alone
// would release it; the explicit unlock keeps the intent readable.
func unlockConflictSlotFile(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}
