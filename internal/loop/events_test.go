package loop

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/events"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

func eventTestDriver(eventsOn, noEvents bool) *Driver {
	cfg := &config.Config{
		Harness:    "claude",
		Prompt:     "work",
		BacklogURL: "https://clankerbar.com",
		Events:     config.RunConfigEvents{Activity: eventsOn},
		NoEvents:   noEvents,
	}
	d := &Driver{cfg: cfg}
	d.SetEventChannel("https://clankerbar.com", "clk_test")
	return d
}

func TestInvocationSetsEventSinkOnlyWhenEveryGateIsOpen(t *testing.T) {
	cases := []struct {
		name    string
		driver  *Driver
		harness string
		probe   bool
		want    bool
	}{
		{"dial on and wired", eventTestDriver(true, false), "claude", false, true},
		{"dial off", eventTestDriver(false, false), "claude", false, false},
		{"machine opted out", eventTestDriver(true, true), "claude", false, false},
		{"probe", eventTestDriver(true, false), "claude", true, false},
		{"codex is out of v1", eventTestDriver(true, false), "codex", false, false},
		{"copilot has no mapped stream", eventTestDriver(true, false), "copilot", false, false},
		{"opencode", eventTestDriver(true, false), "opencode", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in, err := tc.driver.invocationOn(Target{}, tc.harness, tc.probe)
			if err != nil {
				t.Fatalf("invocationOn: %v", err)
			}
			if got := in.NewEventSink != nil; got != tc.want {
				t.Fatalf("NewEventSink set = %v, want %v", got, tc.want)
			}
			if in.NewEventSink != nil {
				in.NewEventSink().Close()
			}
		})
	}
}

func TestInvocationWithoutEventWiringIsInert(t *testing.T) {
	d := eventTestDriver(true, false)
	d.SetEventChannel("", "")
	in, err := d.invocationOn(Target{}, "claude", false)
	if err != nil {
		t.Fatalf("invocationOn: %v", err)
	}
	if in.NewEventSink != nil {
		t.Fatal("an unwired channel built a sink")
	}
}

func TestEventSeqCounterIsProcessWide(t *testing.T) {
	d := eventTestDriver(true, false)
	first := d.eventSeqCounter()
	second := d.eventSeqCounter()
	if first != second {
		t.Fatal("the sequence counter was rebuilt; concurrent sessions would collide")
	}
}

func TestEventSinkUsesTheConfiguredOriginAndKey(t *testing.T) {
	// The driver's wiring has to reach the emitter: the origin is the plane's,
	// the bearer is the operator's key, and the path is the run's ingest URL.
	type captured struct{ auth, path string }
	hit := make(chan captured, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		hit <- captured{auth: r.Header.Get("Authorization"), path: r.URL.Path}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"accepted":1}`)
	}))
	defer srv.Close()

	d := eventTestDriver(true, false)
	d.SetEventChannel(srv.URL, "clk_wired")
	in, err := d.invocationOn(Target{}, "claude", false)
	if err != nil {
		t.Fatalf("invocationOn: %v", err)
	}
	if in.NewEventSink == nil {
		t.Fatal("no sink with every gate open")
	}
	sink := in.NewEventSink()
	sink.SetRunID("uuid-1.secret")
	sink.ToolCall("Bash", "call-1", "ls")
	sink.Close()

	select {
	case got := <-hit:
		if got.auth != "Bearer clk_wired" {
			t.Errorf("Authorization = %q", got.auth)
		}
		if got.path != "/api/runs/uuid-1.secret/events" {
			t.Errorf("path = %q", got.path)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the emitter built by the driver never reached the plane")
	}
}

func TestStateSeqStoreRoundTrip(t *testing.T) {
	st, err := statedir.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open state dir: %v", err)
	}
	defer st.Close()
	store := stateSeqStore{st: st}

	if got, err := store.Load(); err != nil || len(got) != 0 {
		t.Fatalf("fresh Load = %v, %v; want empty and no error", got, err)
	}
	runs := map[string]events.RunSeq{
		"run-1": {Seq: 7, At: time.Now().UTC()},
	}
	if err := store.Save(runs); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got["run-1"].Seq != 7 {
		t.Fatalf("loaded seq = %d, want 7", got["run-1"].Seq)
	}
}

func TestStateSeqStoreRepairsACorruptFile(t *testing.T) {
	st, err := statedir.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open state dir: %v", err)
	}
	defer st.Close()
	if err := st.WriteFile(statedir.RunEventSeqName, []byte("{not json")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	store := stateSeqStore{st: st}
	got, err := store.Load()
	if err != nil || len(got) != 0 {
		t.Fatalf("Load of a corrupt file = %v, %v; want empty and no error so the next Save repairs it", got, err)
	}
	if err := store.Save(map[string]events.RunSeq{"run-1": {Seq: 2, At: time.Now().UTC()}}); err != nil {
		t.Fatalf("Save after a corrupt read: %v", err)
	}
	got, err = store.Load()
	if err != nil || got["run-1"].Seq != 2 {
		t.Fatalf("repaired Load = %v, %v", got, err)
	}
}

// The state dir's adoption check is a closed set of names it recognises as ours;
// a new artifact missing from it would make the next daemon (or doctor) refuse
// to open a directory this one wrote.
func TestStateDirAdoptsTheEventSequenceFile(t *testing.T) {
	dir := t.TempDir()
	st, err := statedir.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.WriteFile(statedir.RunEventSeqName, []byte("{}\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	st2, err := statedir.Open(dir)
	if err != nil {
		t.Fatalf("re-open a state dir holding %s: %v", statedir.RunEventSeqName, err)
	}
	defer st2.Close()
	if _, err := os.Stat(filepath.Join(dir, statedir.RunEventSeqName)); err != nil {
		t.Fatalf("the counter file is gone: %v", err)
	}
}
