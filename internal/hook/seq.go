package hook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lecstor/clankerbar-cli/internal/events"
)

// seqFileStore persists the per-run high-water marks across hook processes.
// Each invocation is a new process, so an in-memory counter would restart at 1
// and every event after a session's first would be dropped by the plane as a
// duplicate.
//
// The file is read and written while the caller holds the lock beside it, so
// the read-modify-write is one critical section across processes. The content
// is counters only — no target, no todo text — and the run id is stored without
// its credential secret (events.SeqCounter's seqKey).
type seqFileStore struct{ path string }

func (s seqFileStore) Load() (map[string]events.RunSeq, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	var runs map[string]events.RunSeq
	if err := json.Unmarshal(raw, &runs); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	return runs, nil
}

func (s seqFileStore) Save(runs map[string]events.RunSeq) error {
	if runs == nil {
		runs = map[string]events.RunSeq{}
	}
	body, err := json.Marshal(runs)
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// temp+rename, so a process killed mid-write cannot leave a half file that
	// the next invocation fails to parse (which would restart the counter at 1).
	tmp, err := os.CreateTemp(dir, ".run-event-seq-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // a no-op once the rename below succeeded
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(body, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, s.path)
}
