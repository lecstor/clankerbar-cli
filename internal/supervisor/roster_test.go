package supervisor

// The roster half of phase 3b: the poll client, the supervisor's own copy of
// the plane's write-time gate (the allowlist), and the last-known-good cache
// that makes an unreachable plane a cold start rather than an outage.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testRosterEntries() []RosterEntry {
	return []RosterEntry{
		{
			Name:         "daemon-one",
			DesiredState: RosterDesiredRunning,
			Placement:    RosterPlacementLocal,
			Projects:     []RosterProject{{Slug: "acme", PrimaryRepo: "acme/widgets"}},
		},
		{
			Name:         "daemon-two",
			DesiredState: RosterDesiredStopped,
			Placement:    RosterPlacementLocal,
			Projects:     []RosterProject{{Slug: "beta", PrimaryRepo: "beta/gadgets"}},
		},
	}
}

// The poll client decodes the account-scoped payload: entries with their
// projects, and an entry's overrides document. (An ABSENT overrides document
// decodes to a nil map — the reconcile treats nil and empty alike, since both
// read as "no override".)
func TestRosterFetchDecodesEntries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon-roster" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"entries": [
			{"name": "daemon-one", "desiredState": "running", "placement": "local",
			 "projects": [{"slug": "acme", "primaryRepo": "acme/widgets"}]},
			{"name": "daemon-two", "desiredState": "stopped", "placement": "local",
			 "overrides": {"harness": "claude"}, "projects": [{"slug": "beta"}]}
		]}`))
	}))
	defer srv.Close()

	entries, err := NewRosterClient(srv.URL+"/api/daemon-roster", "test-key").Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if entries[0].Name != "daemon-one" || entries[0].DesiredState != RosterDesiredRunning {
		t.Fatalf("entry[0] = %+v", entries[0])
	}
	if entries[0].Projects[0].PrimaryRepo != "acme/widgets" {
		t.Fatalf("entry[0].Projects = %+v", entries[0].Projects)
	}
	if entries[1].Overrides == nil || string(entries[1].Overrides["harness"]) != `"claude"` {
		t.Fatalf("entry[1].Overrides = %+v", entries[1].Overrides)
	}
}

// A non-200 answer is an error naming the endpoint, never an empty roster: an
// empty answer must not read as "nothing declared".
func TestRosterFetchNon200IsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, err := NewRosterClient(srv.URL+"/api/daemon-roster", "k").Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch on a 500 returned nil, want an error")
	}
	if !strings.Contains(err.Error(), "HTTP 500") || !strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("error does not name the status and endpoint: %v", err)
	}
}

// A not-wired client (no endpoint or key) is an explicit error, not a silent
// empty roster — a supervisor without the account key must not reconcile
// against nothing.
func TestRosterFetchNotWired(t *testing.T) {
	for _, c := range []*RosterClient{nil, {}, NewRosterClient("", "k"), NewRosterClient("https://plane/api/daemon-roster", "")} {
		if _, err := c.Fetch(context.Background()); !errors.Is(err, ErrNotWired) {
			t.Fatalf("Fetch on %+v = %v, want ErrNotWired", c, err)
		}
	}
}

// The allowlist refuses each forbidden class loudly, naming the entry AND the
// key: machine-layer keys, run-config keys, and anything else outside the two
// permitted overrides. Everything allowed passes.
func TestCheckEntryAllowlist(t *testing.T) {
	bad := []struct {
		name string
		key  string
		want string
	}{
		{"env", "env", "machine-layer key"},
		{"settings", "settings_path", "machine-layer key"},
		{"workdir", "workdir", "machine-layer key"},
		{"mcp", "mcp_config_path", "machine-layer key"},
		{"model", "model", "run-config key"},
		{"models", "models", "run-config key"},
		{"budget", "budget", "run-config key"},
		{"phases", "phases", "run-config key"},
		{"max_turns", "max_turns", "run-config key"},
		{"mystery", "prompt", "run-config key"},
		{"nonsense", "nope", "unknown key"},
	}
	for _, tc := range bad {
		e := &RosterEntry{Name: "entry-" + tc.name, DesiredState: RosterDesiredRunning, Placement: RosterPlacementLocal,
			Projects: []RosterProject{{Slug: "acme"}}, Overrides: map[string]json.RawMessage{tc.key: json.RawMessage(`1`)}}
		err := checkEntry(e)
		if err == nil {
			t.Errorf("%s: checkEntry accepted %q, want a refusal", tc.name, tc.key)
			continue
		}
		if !strings.Contains(err.Error(), "entry-"+tc.name) {
			t.Errorf("%s: refusal does not name the entry: %v", tc.name, err)
		}
		if !strings.Contains(err.Error(), tc.key) {
			t.Errorf("%s: refusal does not name the key %q: %v", tc.name, tc.key, err)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: refusal does not say the class %q: %v", tc.name, tc.want, err)
		}
	}

	good := &RosterEntry{Name: "fine", DesiredState: RosterDesiredRunning, Placement: RosterPlacementLocal,
		Projects: []RosterProject{{Slug: "acme", PrimaryRepo: "acme/widgets"}},
		Overrides: map[string]json.RawMessage{
			"harness":   json.RawMessage(`"claude"`),
			"harnesses": json.RawMessage(`{"opencode": {"model": "sonnet"}}`),
		}}
	if err := checkEntry(good); err != nil {
		t.Fatalf("checkEntry refused an allowed entry: %v", err)
	}
}

// Shape refusals the plane guarantees at write time but the supervisor does
// not trust it to have enforced: bad desired state, bad placement, no projects.
func TestCheckEntryShapeRefusals(t *testing.T) {
	cases := []struct {
		name string
		e    RosterEntry
		want string
	}{
		{"no name", RosterEntry{DesiredState: RosterDesiredRunning, Placement: RosterPlacementLocal, Projects: []RosterProject{{Slug: "a"}}}, "no name"},
		{"bad desired", RosterEntry{Name: "x", DesiredState: "paused", Placement: RosterPlacementLocal, Projects: []RosterProject{{Slug: "a"}}}, "unknown desired state"},
		{"bad placement", RosterEntry{Name: "x", DesiredState: RosterDesiredRunning, Placement: "cloud", Projects: []RosterProject{{Slug: "a"}}}, "unknown placement"},
		{"no projects", RosterEntry{Name: "x", DesiredState: RosterDesiredRunning, Placement: RosterPlacementLocal}, "names no project"},
		{"no slug", RosterEntry{Name: "x", DesiredState: RosterDesiredRunning, Placement: RosterPlacementLocal, Projects: []RosterProject{{Slug: " "}}}, "no slug"},
	}
	for _, tc := range cases {
		err := checkEntry(&tc.e)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: checkEntry = %v, want a refusal containing %q", tc.name, err, tc.want)
		}
	}
}

// The cached roster round-trips: what a successful poll wrote is what a cold
// offline start reconciles from — desired state included, so "stopped" stays
// stopped offline.
func TestRosterCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeCachedRoster(dir, testRosterEntries())
	got := loadCachedRoster(dir)
	if len(got) != 2 {
		t.Fatalf("loaded %d entries, want 2", len(got))
	}
	if got[1].DesiredState != RosterDesiredStopped {
		t.Fatalf("loaded entry[1].DesiredState = %q, want stopped (the cache carries desired state)", got[1].DesiredState)
	}
}

// A missing cache reads as nothing; a corrupt one reads as nothing and says
// so — a bad cache must not wedge a cold start, and the next successful poll
// regenerates it.
func TestRosterCacheCorruptOrMissingReadsAsNothing(t *testing.T) {
	if got := loadCachedRoster(t.TempDir()); len(got) != 0 {
		t.Fatalf("missing cache loaded %d entries, want 0", len(got))
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, rosterCacheName), []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadCachedRoster(dir); len(got) != 0 {
		t.Fatalf("corrupt cache loaded %d entries, want 0", len(got))
	}
}

// The cache write replaces a previous one through a same-directory temp and a
// rename, so a symlink planted at the name is replaced, never followed.
func TestRosterCacheReplacesSafely(t *testing.T) {
	dir := t.TempDir()
	writeCachedRoster(dir, testRosterEntries())
	writeCachedRoster(dir, testRosterEntries()[:1])
	got := loadCachedRoster(dir)
	if len(got) != 1 {
		t.Fatalf("loaded %d entries after a replacement write, want 1", len(got))
	}
	if _, err := os.Lstat(filepath.Join(dir, rosterCacheName+".tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the atomic write left its temp file behind (err = %v)", err)
	}
}

// A planted symlink must be swapped for our file, not followed: the rename
// replaces the directory entry, so the link's target is never written.
func TestRosterCacheReplacesAPlantedSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, rosterCacheName)); err != nil {
		t.Fatal(err)
	}

	writeCachedRoster(dir, testRosterEntries())

	if b, err := os.ReadFile(victim); err != nil || string(b) != "untouched" {
		t.Fatalf("the symlink target was written through (body = %q, err = %v)", b, err)
	}
	if fi, err := os.Lstat(filepath.Join(dir, rosterCacheName)); err != nil {
		t.Fatalf("roster.json is missing after the replacement: %v", err)
	} else if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("roster.json is still a symlink after the replacement")
	}
	if got := loadCachedRoster(dir); len(got) != 2 {
		t.Fatalf("loaded %d entries, want the freshly written 2", len(got))
	}
}

// A replacement must be atomic for a concurrent cold-start reader: it sees the
// old roster or the new one, never a missing or half-written file. The old
// remove-then-create write had exactly that window.
func TestRosterCacheReplaceIsAtomic(t *testing.T) {
	dir := t.TempDir()
	writeCachedRoster(dir, testRosterEntries())
	path := filepath.Join(dir, rosterCacheName)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	bad := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			b, err := os.ReadFile(path)
			if err != nil {
				select {
				case bad <- fmt.Errorf("read during replace: %w", err):
				default:
				}
				return
			}
			var entries []RosterEntry
			if err := json.Unmarshal(b, &entries); err != nil {
				select {
				case bad <- fmt.Errorf("decode during replace: %w (%q)", err, b):
				default:
				}
				return
			}
			if len(entries) == 0 {
				select {
				case bad <- fmt.Errorf("empty roster during replace"):
				default:
				}
				return
			}
		}
	}()
	for i := 0; i < 200; i++ {
		entries := testRosterEntries()
		if i%2 == 1 {
			entries = entries[:1]
		}
		writeCachedRoster(dir, entries)
	}
	close(stop)
	wg.Wait()
	select {
	case err := <-bad:
		t.Fatal(err)
	default:
	}
}

// Instance state dirs are stable across reconciles (keyed by the entry name,
// not the workdir) and distinct between names that sanitise alike.
func TestRosterStateDirStableAndDistinct(t *testing.T) {
	dir := t.TempDir()
	a1 := rosterStateDir(dir, "Jasons-MBP/clanker1")
	a2 := rosterStateDir(dir, "Jasons-MBP/clanker1")
	if a1 != a2 {
		t.Fatalf("state dir not stable for one name: %s vs %s", a1, a2)
	}
	b := rosterStateDir(dir, "Jasons-MBP/clanker2")
	if a1 == b {
		t.Fatalf("two entries share a state dir: %s", a1)
	}
	c := rosterStateDir(dir, "Jasons-MBP clanker1")
	if a1 == c {
		t.Fatalf("names that sanitise alike share a state dir: %s", a1)
	}
	if !strings.HasPrefix(a1, dir+string(filepath.Separator)) {
		t.Fatalf("state dir %s is not under the cache dir %s", a1, dir)
	}
}
