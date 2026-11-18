package planner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/plane"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

// The session store's two jobs (studio-chat.md §3): carry the accumulated spend
// across turns, and make the spend bound end a session. The ref itself is the
// plane's pointer; the store only ever adds the figure.

func TestSessionStorePersistsAcrossOpens(t *testing.T) {
	path := t.TempDir()
	key := sessionKey("demo", "conv-1")

	dir, err := statedir.Open(path)
	if err != nil {
		t.Fatalf("statedir.Open: %v", err)
	}
	store, closeStore, err := OpenSessionStore(dir)
	if err != nil {
		t.Fatalf("OpenSessionStore: %v", err)
	}
	store.Record(key, "claude", "ref-1", 4242)
	if err := closeStore(); err != nil {
		t.Fatalf("close: %v", err)
	}

	dir2, err := statedir.Open(path)
	if err != nil {
		t.Fatalf("reopen state dir: %v", err)
	}
	defer dir2.Close()
	store2, close2, err := OpenSessionStore(dir2)
	if err != nil {
		t.Fatalf("OpenSessionStore: %v", err)
	}
	defer close2()

	got := store2.Decide(key, "claude", &plane.ConversationResume{Harness: "claude", Ref: "ref-1"}, 10000)
	if !got.Resume || got.Ref != "ref-1" || got.Tokens != 4242 {
		t.Errorf("decision after restart = %+v, want a resume of ref-1 with 4242 tokens", got)
	}
}

func TestSessionStoreCorruptFileStartsEmpty(t *testing.T) {
	path := t.TempDir()
	dir, err := statedir.Open(path)
	if err != nil {
		t.Fatalf("statedir.Open: %v", err)
	}
	if err := dir.WriteFile(SessionStoreName, []byte("{not json")); err != nil {
		t.Fatalf("write: %v", err)
	}
	store, closeStore, err := OpenSessionStore(dir)
	if err != nil {
		t.Fatalf("OpenSessionStore: %v", err)
	}
	defer closeStore()
	got := store.Decide(sessionKey("demo", "c"), "claude", &plane.ConversationResume{Harness: "claude", Ref: "r"}, 0)
	if !got.Resume || got.Tokens != 0 {
		t.Errorf("decision = %+v, want a resume with no accumulated spend", got)
	}
}

func TestSessionStoreDecide(t *testing.T) {
	key := sessionKey("demo", "c")
	cases := []struct {
		name      string
		local     *sessionEntry
		resume    *plane.ConversationResume
		maxTokens int
		want      SessionDecision
	}{
		{name: "no pointer", want: SessionDecision{}},
		{name: "empty ref", resume: &plane.ConversationResume{Harness: "claude"}, want: SessionDecision{}},
		{name: "other harness", resume: &plane.ConversationResume{Harness: "opencode", Ref: "r"}, want: SessionDecision{}},
		{
			name:   "matching handle resumes with its accumulator",
			local:  &sessionEntry{Harness: "claude", Ref: "r", Tokens: 100},
			resume: &plane.ConversationResume{Harness: "claude", Ref: "r"},
			want:   SessionDecision{Resume: true, Ref: "r", Tokens: 100},
		},
		{
			name:      "spent budget primes",
			local:     &sessionEntry{Harness: "claude", Ref: "r", Tokens: 100},
			resume:    &plane.ConversationResume{Harness: "claude", Ref: "r"},
			maxTokens: 100,
			want:      SessionDecision{},
		},
		{
			name:      "zero is uncapped",
			local:     &sessionEntry{Harness: "claude", Ref: "r", Tokens: 1 << 30},
			resume:    &plane.ConversationResume{Harness: "claude", Ref: "r"},
			maxTokens: 0,
			want:      SessionDecision{Resume: true, Ref: "r", Tokens: 1 << 30},
		},
		{
			name:   "a different local handle still resumes the plane's, with no accumulator",
			local:  &sessionEntry{Harness: "claude", Ref: "old", Tokens: 900},
			resume: &plane.ConversationResume{Harness: "claude", Ref: "r"},
			want:   SessionDecision{Resume: true, Ref: "r"},
		},
		{
			name:   "an unknown handle (fresh process) resumes with no accumulator",
			resume: &plane.ConversationResume{Harness: "claude", Ref: "r"},
			want:   SessionDecision{Resume: true, Ref: "r"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewMemorySessionStore()
			if tc.local != nil {
				s.entries[key] = *tc.local
			}
			if got := s.Decide(key, "claude", tc.resume, tc.maxTokens); got != tc.want {
				t.Errorf("Decide = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestSessionStoreRecordIgnoresEmptyRef(t *testing.T) {
	s := NewMemorySessionStore()
	s.Record(sessionKey("demo", "c"), "claude", "", 10)
	if len(s.entries) != 0 {
		t.Errorf("recorded an entry with no handle: %+v", s.entries)
	}
}

func TestSessionStoreFileIsOwnerOnly(t *testing.T) {
	path := t.TempDir()
	dir, err := statedir.Open(path)
	if err != nil {
		t.Fatalf("statedir.Open: %v", err)
	}
	store, closeStore, err := OpenSessionStore(dir)
	if err != nil {
		t.Fatalf("OpenSessionStore: %v", err)
	}
	store.Record(sessionKey("demo", "c"), "claude", "r", 1)
	if err := closeStore(); err != nil {
		t.Fatalf("close: %v", err)
	}
	fi, err := os.Stat(filepath.Join(path, SessionStoreName))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Errorf("store mode = %o, want 600 (it carries conversation ids and spend)", mode)
	}
}
