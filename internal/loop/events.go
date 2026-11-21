package loop

// Agent event emission in the driver.

import (
	"encoding/json"
	"errors"
	"log"
	"os"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/events"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

// SetEventChannel wires the plane origin and bearer the event sender uses.
func (d *Driver) SetEventChannel(origin, apiKey string) {
	d.eventOrigin = origin
	d.eventKey = apiKey
}

// eventSenderOn reports whether this target's sessions may emit: wired, the project activity dial
// is on, the machine has not opted out, and the adapter has a mapping (codex and copilot do not in
// v1). The transcript dial rides on the sink this gates, and is only ever on beside activity (the
// plane refuses the pair at save time).
func (d *Driver) eventSenderOn(tc *config.Config, harnessName string) bool {
	if d.eventOrigin == "" || d.eventKey == "" {
		return false
	}
	if tc.NoEvents || !tc.Events.Activity {
		return false
	}
	caps, ok := harness.CapabilitiesOf(harnessName)
	return ok && caps.EmitsEvents
}

// newEventEmitter builds one session's sink. Batching and the HTTP call run on the emitter's own
// goroutine; the parser only ever appends to a bounded buffer.
func (d *Driver) newEventEmitter(harnessName string, transcript bool) *events.Emitter {
	return events.New(events.Config{
		Origin:     d.eventOrigin,
		APIKey:     d.eventKey,
		Harness:    harnessName,
		Transcript: transcript,
		Seq:        d.eventSeqCounter(),
		Logf:       log.Printf,
	})
}

// eventSeqCounter is the process-wide per-run counter, built once. The lock matters because a
// review panel's members are built from more than one goroutine and each would otherwise race to
// create its own counter (and its own in-memory view of the high-water marks).
func (d *Driver) eventSeqCounter() *events.SeqCounter {
	d.eventMu.Lock()
	defer d.eventMu.Unlock()
	if d.eventSeq == nil {
		var store events.SeqStore = events.NewMemorySeqStore()
		if d.state != nil {
			store = stateSeqStore{st: d.state}
		}
		d.eventSeq = events.NewSeqCounter(store, log.Printf)
	}
	return d.eventSeq
}

// stateSeqStore persists the counters in the daemon's state dir. A missing file is the ordinary
// first-run case (no error); every other read/write failure is returned to the counter, which
// degrades to per-process numbering rather than affecting the session.
type stateSeqStore struct{ st *statedir.Dir }

func (s stateSeqStore) Load() (map[string]events.RunSeq, error) {
	raw, err := s.st.ReadFile(statedir.RunEventSeqName)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var runs map[string]events.RunSeq
	if err := json.Unmarshal(raw, &runs); err != nil {
		// The file is already unusable. Treating it as empty lets the next Save rewrite it; returning the
		// parse error would leave the counter unable to persist anything, so every restart would replay
		// sequence numbers the plane drops.
		return nil, nil
	}
	return runs, nil
}

func (s stateSeqStore) Save(runs map[string]events.RunSeq) error {
	if runs == nil {
		runs = map[string]events.RunSeq{}
	}
	body, err := json.Marshal(runs)
	if err != nil {
		return err
	}
	return s.st.WriteFileAtomic(statedir.RunEventSeqName, append(body, '\n'))
}
