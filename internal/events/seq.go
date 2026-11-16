package events

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// The emitter's sequence numbers are per RUN, from 1, and are the plane's
// idempotency key: a (run, seq) it already holds is dropped. A phased task runs
// several sessions on the SAME run, so the counter is per run and persisted —
// a fresh counter per session would restart at 1 and every later phase's events
// would be dropped as duplicates, including across a supervised restart.

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

// maxPersistedRuns bounds the counter file, and minPersistedAge keeps the cap
// from evicting a mark that is still in use. One daemon sees one run per task
// per drain, so a long-lived process would otherwise grow the file without
// limit. An active emitter refreshes its run's mark on every batch, so a mark
// untouched for minPersistedAge has no live emitter; evicting one that does
// would restart that run at 1 after a restart, and the plane would drop its
// events as duplicates.
const (
	maxPersistedRuns = 256
	minPersistedAge  = time.Hour
)

// SeqCounter reserves monotonically increasing per-run sequence numbers,
// persists the high-water marks, and owns the per-run SEND lock. That lock is
// what makes the numbers an idempotency key: the plane drops any event at or
// below the run's high-water mark, so a later-numbered batch that overtakes an
// earlier one would silently discard the earlier session's events (the default
// review panel runs its members on one run in parallel). Emitters sharing a
// counter hold the run's lock from allocation through POST to commit, so
// allocation order is arrival order.
type SeqCounter struct {
	mu     sync.Mutex
	store  SeqStore
	runs   map[string]RunSeq
	saved  map[string]uint64 // highest seq already written to store, per key
	loaded bool
	now    func() time.Time
	logf   func(string, ...any)
	readOK bool

	sendMu    sync.Mutex
	sendLocks map[string]*sync.Mutex
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
	c.saved = map[string]uint64{}
	loaded, err := c.store.Load()
	if err != nil {
		if c.logf != nil {
			c.logf("agent events: could not read the per-run sequence counter (%v) — starting from 1; the plane drops any duplicates", err)
		}
		return
	}
	for k, v := range loaded {
		c.runs[k] = v
		c.saved[k] = v.Seq
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

// SendLock locks runID's send slot and returns the unlock function. The caller
// holds it across the WHOLE send — sequence allocation, the POST and the commit
// — never around only one of them: a lock around the POST alone would still let
// two batches allocate out of order.
func (c *SeqCounter) SendLock(runID string) (unlock func()) {
	if c == nil || runID == "" {
		return func() {}
	}
	key := seqKey(runID)
	c.sendMu.Lock()
	if c.sendLocks == nil {
		c.sendLocks = map[string]*sync.Mutex{}
	}
	m := c.sendLocks[key]
	if m == nil {
		m = &sync.Mutex{}
		c.sendLocks[key] = m
	}
	c.sendMu.Unlock()
	m.Lock()
	return m.Unlock
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

// Persist writes the current high-water marks to the store. The emitter calls it
// AFTER allocating a batch and BEFORE posting it: a process that dies between a
// landed POST and the commit must make the next process resume ABOVE the batch,
// not reuse numbers the plane would drop as duplicates. Gaps in the sequence are
// harmless — the plane's rule is seq > high-water, not seq == high-water+1.
func (c *SeqCounter) Persist(runID string) {
	if c == nil || runID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		c.loadLocked()
	}
	c.saveLocked()
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
	if cur.Seq > c.saved[key] {
		c.saveLocked()
	}
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
		sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
		evict := len(out) - maxPersistedRuns
		for _, e := range all {
			if evict == 0 {
				break
			}
			if c.now().Sub(e.at) < minPersistedAge {
				// Fresh enough to belong to a live emitter: evicting it would
				// make that run restart at 1, and the plane would drop its
				// events. The file stays over the cap until these age out.
				break
			}
			delete(out, e.key)
			evict--
		}
	}
	if err := c.store.Save(out); err != nil {
		if c.logf != nil {
			c.logf("agent events: could not persist the per-run sequence counter (%v) — a restart may replay sequence numbers the plane then drops", err)
		}
		return
	}
	c.saved = make(map[string]uint64, len(out))
	for k, v := range out {
		c.saved[k] = v.Seq
	}
}
