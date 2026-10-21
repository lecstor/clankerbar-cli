package supervisor

// The fleet's conflict-session slot (CLA-759).
//
// One conflict session may run at a time. `conflictBusy` alone cannot hold that
// bound: it is one atomic per Supervisor VALUE, so two supervisors — two processes
// on one machine, or two instances in one process — each start their own session
// beside the other, spending two paid sessions on two different tasks at once.
//
// The slot is therefore a SHARED, durable lease, not a process-local flag:
//
//   - it lives at one path, derived from the supervisor's state root
//     (`<cache dir>/conflict-session.lock`), which every supervisor on the fleet's
//     state root uses — so all of them contend for the same slot, and an operator
//     who puts the fleet's state root on storage shared between machines extends
//     that to those machines too;
//   - acquisition is an exclusive advisory lock on that file (flock on unix), taken
//     NON-blocking, so a second supervisor learns immediately that the slot is held
//     and simply waits for the next poll;
//   - release is the lock's release, and the kernel releases it when the holding
//     process exits — that is the stale-holder recovery: a supervisor that crashed,
//     was killed, or was rolled mid-session cannot strand the slot, so there is no
//     lease to expire and no timer to get wrong;
//   - the holder writes a one-line record into the file (host, pid, task, PR), so an
//     operator (or the next supervisor's log line) can see WHO holds it.
//
// The slot is taken BEFORE the plane claim and before the session is launched, and
// released when the session ends, so a task that has to wait costs neither a plane
// write nor a provider token.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// errConflictSlotHeld is "another supervisor holds the fleet's conflict-session
// slot". It is a normal, expected answer — the task waits for the next poll — and
// never an error to report as a failure.
var errConflictSlotHeld = errors.New("another supervisor holds the fleet's conflict-session slot")

// conflictSlot is a held slot. Its zero value is not valid; acquireConflictSlot
// returns one only after the lock was really taken.
type conflictSlot struct {
	file *os.File
	path string
}

// conflictSlotPath is the file the fleet's slot is held on, or "" when this
// supervisor has no state root to put it in (a hand-built Supervisor in a test).
// An empty path makes the slot UNACQUIRABLE: the bound fails closed, because a
// supervisor that cannot prove it is alone must not start a paid session.
func (d *Supervisor) conflictSlotPath() string {
	dir := strings.TrimSpace(d.cacheDir)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "conflict-session.lock")
}

// acquireConflictSlot takes the fleet's slot for one session. It returns
// errConflictSlotHeld when another supervisor holds it. record is the one-line
// holder description written into the file for an operator's benefit; it is
// BEST-EFFORT and never affects whether the slot is held.
func (d *Supervisor) acquireConflictSlot(record string) (*conflictSlot, error) {
	path := d.conflictSlotPath()
	if path == "" {
		return nil, errors.New("no state root to hold the fleet's conflict-session slot on (state_dir/XDG_STATE_HOME is unset)")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("prepare the conflict-session slot's directory: %w", err)
	}
	file, held, err := lockConflictSlotFile(path)
	if err != nil {
		return nil, err
	}
	if !held {
		return nil, errConflictSlotHeld
	}
	slot := &conflictSlot{file: file, path: path}
	slot.writeRecord(record)
	return slot, nil
}

// release hands the slot back. Safe on a nil slot, so a caller can defer it before
// it knows whether acquisition succeeded.
func (s *conflictSlot) release() {
	if s == nil || s.file == nil {
		return
	}
	unlockConflictSlotFile(s.file)
	s.file = nil
}

// writeRecord replaces the file's contents with the holder's one-line record. The
// holder owns the file exclusively, so this cannot race another writer; a failure
// only costs the diagnostic, never the slot.
func (s *conflictSlot) writeRecord(record string) {
	record = strings.TrimSpace(record)
	if record == "" {
		return
	}
	line := fmt.Sprintf("%s acquired %s\n", record, time.Now().UTC().Format(time.RFC3339))
	if err := s.file.Truncate(0); err != nil {
		return
	}
	if _, err := s.file.WriteAt([]byte(line), 0); err != nil {
		return
	}
}

// conflictHolder is what the slot's record says, as far as it can be read without
// taking the lock. Used only to make a stand-down line actionable.
func conflictSlotHolder(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
	return line
}
