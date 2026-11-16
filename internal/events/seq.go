package events

import (
	"strings"
	"sync"
	"time"
)

// The emitter's sequence numbers are per RUN, from 1, and they are the plane's
// idempotency key: a (run, seq) it already holds is dropped. A phased task runs
// several sessions on the SAME run (the claim's run id is carried across the
// seam), so a fresh counter per session would restart at 1 and every later
// phase's events would be dropped as duplicates. The counter therefore lives
// per run and is persisted, so a supervised restart or a crash resumes after the
// high-water mark rather than replaying the numbers.

// RunSeq is one run's counter as persisted: the highest sequence number assigned
// so far, and when it was last used (so an old entry can be pruned).
type RunSeq struct {
	Seq uint64    `json:"seq"`
	At  time.Time `json:"at"`
}

// SeqStore is where the per-run counters survive a process. Load's error is not
// fatal — a counter that cannot be read starts empty (the plane drops any
// duplicate) — and Save's is logged and ignored, because the event channel must
// never fail a session over bookkeeping.
type SeqStore interface {
	Load() (map[string]RunSeq, error)
	Save(map[string]RunSeq) error
}

// MemorySeqStore is the process-local store: the fallback when no state dir is
// open (tests, and a Driver built by hand), and what keeps a single process's
// concurrent panel sessions on one counter.
type MemorySeqStore struct {
	mu   sync.Mutex
	runs map[string]RunSeq
}

// NewMemorySeqStore returns an empty in-memory store.
func NewMemorySeqStore() *MemorySeqStore {
	return &MemorySeqStore{runs: map[string]RunSeq{}}
}

func (m *MemorySeqStore) Load() (map[string]RunSeq, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]RunSeq, len(m.runs))
	for k, v := range m.runs {
		out[k] = v
	}
	return out, nil
}

func (m *MemorySeqStore) Save(runs map[string]RunSeq) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs = make(map[string]RunSeq, len(runs))
	for k, v := range runs {
		m.runs[k] = v
	}
	return nil
}

// maxPersistedRuns bounds the counter file. One daemon sees one run per task per
// drain, so a long-lived process would otherwise grow the file without limit;
// the oldest entries are dropped first, and a run old enough to be evicted is
// long past the point where its emitter is alive.
const maxPersistedRuns = 256

// SeqCounter reserves monotonically increasing per-run sequence numbers and
// persists the high-water marks. Safe for concurrent use: a review panel runs
// several sessions on the same run at once.
type SeqCounter struct {
	mu     sync.Mutex
	store  SeqStore
	runs   map[string]RunSeq
	loaded bool
	now    func() time.Time
	logf   func(string, ...any)
	readOK bool
}

// NewSeqCounter builds a counter over store (nil means process-local) and loads
// what the store holds. A load failure is logged once and treated as an empty
// counter: losing the mark can cost a duplicate (which the plane drops), never a
// failed session.
func NewSeqCounter(store SeqStore, logf func(string, ...any)) *SeqCounter {
	if store == nil {
		store = NewMemorySeqStore()
	}
	c := &SeqCounter{store: store, now: time.Now, logf: logf}
	c.load()
	return c
}

func (c *SeqCounter) load() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loadLocked()
}

func (c *SeqCounter) loadLocked() {
	c.loaded = true
	c.runs = map[string]RunSeq{}
	loaded, err := c.store.Load()
	if err != nil {
		if c.logf != nil {
			c.logf("agent events: could not read the per-run sequence counter (%v) — starting from 1; the plane drops any duplicates", err)
		}
		return
	}
	for k, v := range loaded {
		c.runs[k] = v
	}
	c.readOK = true
}

// seqKey is the run id WITHOUT its credential secret: the sequence is per run
// (the bare uuid), and the state dir has no need to hold a second copy of the
// secret part.
func seqKey(runID string) string {
	if i := strings.IndexByte(runID, '.'); i > 0 {
		return runID[:i]
	}
	return runID
}

// Next reserves the next sequence number for runID.
func (c *SeqCounter) Next(runID string) uint64 {
	if c == nil || runID == "" {
		return 0
	}
	key := seqKey(runID)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		c.loadLocked()
	}
	cur := c.runs[key]
	cur.Seq++
	cur.At = c.now().UTC()
	c.runs[key] = cur
	return cur.Seq
}

// Commit records that seq was the highest number handed to the plane for runID.
// Called after every send attempt — including a failed one: the batch is spent
// either way (it was dropped), and reusing its numbers after a timeout whose
// batch DID land would make the plane drop the new events instead.
func (c *SeqCounter) Commit(runID string, seq uint64) {
	if c == nil || runID == "" || seq == 0 {
		return
	}
	key := seqKey(runID)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		c.loadLocked()
	}
	cur := c.runs[key]
	if seq > cur.Seq {
		cur.Seq = seq
	}
	cur.At = c.now().UTC()
	c.runs[key] = cur
	c.saveLocked()
}

func (c *SeqCounter) saveLocked() {
	if !c.readOK {
		// The initial load failed. Saving now would overwrite whatever the file
		// held with a map built from nothing, turning a transient read fault into
		// data loss. Leave it alone; the channel still works.
		return
	}
	out := make(map[string]RunSeq, len(c.runs))
	for k, v := range c.runs {
		out[k] = v
	}
	if len(out) > maxPersistedRuns {
		type kv struct {
			key string
			at  time.Time
		}
		all := make([]kv, 0, len(out))
		for k, v := range out {
			all = append(all, kv{k, v.At})
		}
		// Oldest first; drop the excess.
		for i := 0; i < len(all); i++ {
			for j := i + 1; j < len(all); j++ {
				if all[j].at.Before(all[i].at) {
					all[i], all[j] = all[j], all[i]
				}
			}
		}
		for _, e := range all[:len(all)-maxPersistedRuns] {
			delete(out, e.key)
		}
	}
	if err := c.store.Save(out); err != nil && c.logf != nil {
		c.logf("agent events: could not persist the per-run sequence counter (%v) — a restart may replay sequence numbers the plane then drops", err)
	}
}
