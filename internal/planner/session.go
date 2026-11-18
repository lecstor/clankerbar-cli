package planner

import (
	"encoding/json"
	"log"
	"strings"
	"sync"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/plane"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

// The planner's local session store (studio-chat.md §3).
//
// It exists for ONE fact the plane cannot hold: how many tokens this machine has
// accumulated in a conversation's resumed session. The session REF and HARNESS
// are the plane's — the claim's `resume` pointer is the binding of record — and
// the store only says "this is what I have spent on that handle", so the role's
// max_session_tokens can end a session by re-priming it.
//
// A store that is missing, unreadable or stale is therefore harmless: the plane's
// pointer still resumes the session (the harness may well still have it), the
// accumulator restarts at zero, and the only cost is a bound that starts
// counting again. That is why a corrupt file is logged and replaced rather than
// refused.
const SessionStoreName = "planner-sessions.json"

// sessionEntry is one conversation's local session state, keyed in the file by
// `<project>\x00<conversationId>`.
type sessionEntry struct {
	Harness string `json:"harness"`
	Ref     string `json:"ref"`
	Tokens  int    `json:"tokens"`
}

// SessionStore is the in-memory view of the store file, written through on every
// change. The planner is single-threaded, but tests and any future concurrency
// read it from more than one goroutine, so the mutex is not decorative.
type SessionStore struct {
	mu      sync.Mutex
	dir     *statedir.Dir // nil = memory-only (tests)
	entries map[string]sessionEntry
}

// SessionDecision is resume-or-prime for one turn, with the accumulator to
// carry.
type SessionDecision struct {
	Resume bool
	Ref    string
	// Tokens is the accumulated spend of the session being resumed; it becomes
	// the base the turn's own report is added to.
	Tokens int
}

// sessionKey identifies one conversation within one project.
func sessionKey(project, conversationID string) string {
	return project + "\x00" + conversationID
}

// OpenSessionStore loads the store from dir. A nil dir yields an in-memory store
// (the unit tests); a real one reads the file when present and starts empty when
// it cannot be trusted. The returned closer releases the state-dir handle and is
// always safe to call.
func OpenSessionStore(dir *statedir.Dir) (*SessionStore, func() error, error) {
	s := &SessionStore{dir: dir, entries: map[string]sessionEntry{}}
	if dir != nil && dir.Exists(SessionStoreName) {
		b, err := dir.ReadFile(SessionStoreName)
		if err != nil {
			log.Printf("planner: could not read %s (%v) - starting with no accumulated session spend", SessionStoreName, err)
		} else if err := json.Unmarshal(b, &s.entries); err != nil {
			log.Printf("planner: %s is not valid JSON (%v) - starting with no accumulated session spend", SessionStoreName, err)
			s.entries = map[string]sessionEntry{}
		}
	}
	closeFn := func() error { return nil }
	if dir != nil {
		closeFn = dir.Close
	}
	return s, closeFn, nil
}

// NewMemorySessionStore is the test and caller-supplied form: no file, no state
// dir.
func NewMemorySessionStore() *SessionStore {
	return &SessionStore{entries: map[string]sessionEntry{}}
}

// Decide resolves this turn's session against the plane's resume pointer:
//
//   - no pointer, a different harness, or an empty ref -> PRIME. The plane only
//     offers a pointer when this instance owns the conversation's session; a
//     harness change means the ref belongs to a CLI this turn is not running.
//   - a pointer and a spent budget -> PRIME. This is the re-prime that keeps a
//     long conversation bounded (roles.planner.max_session_tokens, else the
//     run budget's).
//   - otherwise RESUME, carrying the accumulator this process recorded for that
//     exact handle. A local entry naming a DIFFERENT ref (or none at all, after a
//     restart) still resumes the plane's handle: the plane's pointer is the
//     binding of record, and the harness may still hold the session. The
//     accumulator restarts at zero in that case, because this process has no
//     observation of it.
func (s *SessionStore) Decide(key, harness string, resume *plane.ConversationResume, maxTokens int) SessionDecision {
	if resume == nil {
		return SessionDecision{}
	}
	ref := strings.TrimSpace(resume.Ref)
	if ref == "" || strings.TrimSpace(resume.Harness) != harness {
		return SessionDecision{}
	}
	s.mu.Lock()
	local, ok := s.entries[key]
	s.mu.Unlock()
	tokens := 0
	if ok && local.Harness == harness && local.Ref == ref {
		tokens = local.Tokens
	}
	if maxTokens > 0 && tokens >= maxTokens {
		return SessionDecision{}
	}
	return SessionDecision{Resume: true, Ref: ref, Tokens: tokens}
}

// Record writes what the turn's session ended as. A turn that could not name a
// ref (a harness with no session ids) writes nothing: there is no handle to
// resume and no accumulator worth carrying.
func (s *SessionStore) Record(key, harness, ref string, tokens int) {
	if strings.TrimSpace(ref) == "" {
		return
	}
	s.mu.Lock()
	s.entries[key] = sessionEntry{Harness: harness, Ref: ref, Tokens: tokens}
	s.mu.Unlock()
	s.save()
}

// save writes the store through atomically. A write failure is logged and
// dropped: losing the accumulator costs a bound that starts over, never a turn.
func (s *SessionStore) save() {
	if s.dir == nil {
		return
	}
	s.mu.Lock()
	b, err := json.Marshal(s.entries)
	s.mu.Unlock()
	if err != nil {
		return // a map of strings and ints cannot fail to marshal
	}
	if err := s.dir.WriteFileAtomic(SessionStoreName, b); err != nil {
		log.Printf("planner: could not write %s (%v) - session spend will restart after a restart", SessionStoreName, err)
	}
}

// stateDir resolves and opens the planner's state dir, which is the same one a
// drain uses (markers and logs shared per workdir) with the session store added.
// An explicitly configured dir is opened with the same session-root guard.
func (p *Planner) stateDir(base *config.Config) (*statedir.Dir, error) {
	dir := strings.TrimSpace(p.stateDirPath)
	if dir == "" {
		abs, err := base.ResolveStateDir()
		if err != nil {
			return nil, err
		}
		dir = abs
	}
	return statedir.Open(dir, base.SessionWorkDirs()...)
}
