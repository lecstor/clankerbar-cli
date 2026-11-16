package loop

// Agent event emission in the driver (CLA-603). The dials live in the project's
// EFFECTIVE run config (`events.activity`), which the loop already applies at the
// iteration boundary; the machine can only opt out (`--no-events` /
// CLANKERBAR_EVENTS=off). When the channel is on, every phase invocation gets a
// factory that builds a fresh, bounded emitter for the session it is about to
// run.
//
// The emitter's sequence numbers are per RUN, not per session. A phased task
// runs several sessions on the same run id, so a fresh counter per session would
// start at 1 again and every later phase's events would be dropped as duplicates.
// The counter is therefore shared through the driver and persisted in the state
// dir; a supervised restart resumes after the high-water mark.

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

// SetEventChannel wires the plane origin and bearer the event sender uses. The
// origin is the operator's trusted CredentialOrigin (CLA-257), never a host read
// off a checkout, and the key is the same CLANKERBAR_API_KEY every other plane
// call carries. Unset means the channel is unwired and no session emits.
func (d *Driver) SetEventChannel(origin, apiKey string) {
	d.eventOrigin = origin
	d.eventKey = apiKey
}

// eventSenderOn reports whether this target's sessions may emit: wired, the
// project dial is on, the machine has not opted out, and the adapter has a
// mapping (codex and copilot do not in v1).
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

// newEventEmitter builds one session's sink. Batching and the HTTP call run on
// the emitter's own goroutine; the parser only ever appends to a bounded buffer.
func (d *Driver) newEventEmitter(harnessName string) *events.Emitter {
	return events.New(events.Config{
		Origin:  d.eventOrigin,
		APIKey:  d.eventKey,
		Harness: harnessName,
		Seq:     d.eventSeqCounter(),
		Logf:    log.Printf,
	})
}

// eventSeqCounter is the process-wide per-run counter, built once. The lock
// matters because a review panel's members are built from more than one
// goroutine and each would otherwise race to create its own counter (and its own
// in-memory view of the high-water marks).
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

// stateSeqStore persists the counters in the daemon's state dir. A missing file
// is the ordinary first-run case (no error); every other read/write failure is
// returned to the counter, which degrades to per-process numbering rather than
// affecting the session.
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
		return nil, err
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
