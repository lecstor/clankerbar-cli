package supervisor

// The supervisor's tests drive it against a FAKE daemon and a FAKE plane: the
// test binary re-invoked as the child (TestMain -> helperMain) behaves per the
// mode recorded in the CLANKERBAR_SUPER_MODE environment variable, and the
// account-scoped roster is served by a local httptest server whose entries the
// test can flip between polls (that is the console flip the doneWhen describes).
//
// The fake daemon mirrors the real daemon's load-bearing behaviours and nothing
// else:
//
//   - it creates its own state dir (0700) at startup,
//   - it consumes a STOP marker found AT STARTUP without stopping (the
//     CLA-491 behaviour: a leftover marker must not kill the next run),
//   - in sleep modes it polls for STOP and exits when it appears (consuming it,
//     like the real loop does at its iteration boundary),
//   - it counts its own runs by writing iteration-<n>.log files — the same
//     artifact names the real daemon writes, so the supervisor's statedir.Open
//     keeps adopting the dir.
//
// The MODE travels in an env var, not the config file, because the child no
// longer reads the operator's config: since phase 2b it runs on the
// supervisor's MATERIALIZED config, which carries only real config fields — a
// test-only key would not survive materialization. The state dir still comes
// from the config file, and the materialized one pins it, so a fake that
// starts at all is evidence the generated config carried the state dir.
//
// The modes: "sleep" (exit on STOP), "sleep-seen" (exit on STOP, leave the
// marker and drop a STOP-SEEN file as proof the marker landed), "crash" (exit
// 1 immediately), "halt" (write HALT, exit 1), "sleep-no-dir" (never create
// the state dir: the daemon-stuck-before-startup shape) and "stubborn" (never
// exit, whatever lands in the state dir: the drain-stuck shape). No test runs
// instances in different modes at once, so a single process-wide variable is
// enough.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/loop"
	"github.com/lecstor/clankerbar-cli/internal/teststate"
	"github.com/lecstor/clankerbar-cli/internal/version"
)

const helperEnv = "CLANKERBAR_SUPER_HELPER"

// helperModeEnv carries the fake daemon's mode (see the file comment for why
// it is an env var and not a config key).
const helperModeEnv = "CLANKERBAR_SUPER_MODE"

// TestMain doubles as the child-process entry point: with the helper env set,
// the test binary behaves as the fake daemon instead of running tests. The
// suite itself runs under teststate.Isolate (required of every package whose
// tests can reach internal/config, enforced mechanically): XDG_STATE_HOME is
// pointed at a fresh temp dir so nothing here can write into the operator's
// real loop state root, and the post-run guard fails the binary if anything
// leaked there.
func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		helperMain()
		os.Exit(0) // unreachable — helperMain exits
	}
	os.Exit(teststate.Isolate(m))
}

// helperMain is the fake daemon. Args are exactly what the supervisor spawns:
// <test binary> run -c <config>.
func helperMain() {
	args := os.Args[1:]
	cfgPath := ""
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-c" {
			cfgPath = args[i+1]
			break
		}
	}
	if cfgPath == "" {
		fmt.Fprintln(os.Stderr, "fake daemon: no -c <config> in args:", args)
		os.Exit(9)
	}
	var spec struct {
		StateDir string `json:"state_dir"`
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake daemon: read config:", err)
		os.Exit(9)
	}
	if err := json.Unmarshal(data, &spec); err != nil || spec.StateDir == "" {
		fmt.Fprintln(os.Stderr, "fake daemon: bad config:", err)
		os.Exit(9)
	}
	mode := os.Getenv(helperModeEnv)

	// The real daemon creates its own state dir at startup — except
	// "sleep-no-dir", which deliberately does not, so the supervisor faces a
	// child that is alive but has no state dir to write STOP into.
	if mode != "sleep-no-dir" {
		if err := os.MkdirAll(spec.StateDir, 0o700); err != nil {
			fmt.Fprintln(os.Stderr, "fake daemon: state dir:", err)
			os.Exit(9)
		}
		// Count existing runs, then write this run's iteration log — the real
		// daemon's artifact naming, so the supervisor adopts the dir.
		entries, _ := os.ReadDir(spec.StateDir)
		n := 0
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "iteration-") && strings.HasSuffix(e.Name(), ".log") {
				n++
			}
		}
		name := fmt.Sprintf("iteration-%d.log", n+1)
		if err := os.WriteFile(filepath.Join(spec.StateDir, name), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "fake daemon: iteration log:", err)
			os.Exit(9)
		}
		// The local beacon (phase 5b): what the roll verifies against. The
		// supervisor passes the version it recorded for THIS spawn in
		// CLANKERBAR_CHILD_VERSION; the fake reports it back the way the real
		// daemon reports its own build.
		v := os.Getenv(childVersionEnv)
		if v == "" {
			v = version.Current
		}
		beaconBody, err := json.Marshal(map[string]any{
			"version": v,
			"state":   "idle",
			"at":      time.Now().UTC().Format(time.RFC3339),
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake daemon: beacon:", err)
			os.Exit(9)
		}
		if err := os.WriteFile(filepath.Join(spec.StateDir, loop.LocalBeaconName), beaconBody, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "fake daemon: beacon:", err)
			os.Exit(9)
		}
		// The CLA-491 startup consumption: a STOP found here predates this process
		// and is eaten WITHOUT stopping.
		if _, err := os.Lstat(filepath.Join(spec.StateDir, "STOP")); err == nil {
			_ = os.Remove(filepath.Join(spec.StateDir, "STOP"))
		}
	}

	stopPath := filepath.Join(spec.StateDir, "STOP")
	switch mode {
	case "crash":
		os.Exit(1)
	case "halt":
		if err := os.WriteFile(filepath.Join(spec.StateDir, "HALT"), []byte("halted by test\n"), 0o600); err != nil {
			os.Exit(9)
		}
		os.Exit(1)
	case "sleep", "sleep-seen":
		for {
			if _, err := os.Lstat(stopPath); err == nil {
				if mode == "sleep" {
					_ = os.Remove(stopPath)
				} else {
					_ = os.WriteFile(filepath.Join(spec.StateDir, "STOP-SEEN"), []byte("marker landed\n"), 0o600)
				}
				os.Exit(0)
			}
			// The phase-5b roll: the real daemon drains and re-execs in place;
			// the fake simulates the restart as an EXIT, so the supervisor
			// respawns the child from the (possibly swapped) launch path — the
			// same landing the roll's beacon verify watches for.
			if _, err := os.Lstat(filepath.Join(spec.StateDir, loop.MarkerRestart)); err == nil {
				_ = os.Remove(filepath.Join(spec.StateDir, loop.MarkerRestart))
				os.Exit(0)
			}
			time.Sleep(20 * time.Millisecond)
		}
	case "sleep-no-dir":
		// Alive forever, never opening the state dir: a real daemon wedged
		// (or dead) before its statedir.Open. The supervisor's STOP lands in
		// the dir (which the supervisor itself created for the generated
		// config) but is never consumed, so the drain waits and logs.
		for {
			time.Sleep(50 * time.Millisecond)
		}
	case "stubborn":
		// Alive forever with a state dir, ignoring every marker: a daemon
		// wedged mid-drain, whose STOP never lands at an iteration boundary.
		for {
			time.Sleep(50 * time.Millisecond)
		}
	default:
		fmt.Fprintln(os.Stderr, "fake daemon: unknown mode", mode)
		os.Exit(9)
	}
}

// --- test scaffolding -------------------------------------------------------

// fakePlane serves the account-scoped roster over httptest. The served
// entries are mutable (set) so a test can flip desired state between polls —
// the console flip — and the plane can be put down (setDown) to exercise the
// offline paths.
type fakePlane struct {
	mu      sync.Mutex
	entries []RosterEntry
	down    bool
}

func (p *fakePlane) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		down := p.down
		entries := p.entries
		p.mu.Unlock()
		if down {
			http.Error(w, "plane unreachable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"entries": entries})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (p *fakePlane) set(entries []RosterEntry) {
	p.mu.Lock()
	p.entries = entries
	p.mu.Unlock()
}

func (p *fakePlane) setDown(down bool) {
	p.mu.Lock()
	p.down = down
	p.mu.Unlock()
}

// testOptions returns supervisor options pointed at the test binary as the
// child and the fake plane as the roster, with fast intervals so the tests do
// not wait on production defaults. HOME is isolated so the machine-layer
// config discovery in buildConfig never reads the operator's real config.
func testOptions(t *testing.T, cacheDir string, srv *httptest.Server) Options {
	t.Helper()
	t.Setenv(helperEnv, "1") // children must re-enter as the fake daemon
	t.Setenv("HOME", t.TempDir())
	return Options{
		Binary:            os.Args[0],
		RosterURL:         srv.URL + "/api/daemon-roster",
		APIKey:            "test-key",
		RosterCacheDir:    cacheDir,
		PollInterval:      50 * time.Millisecond,
		BackoffBase:       50 * time.Millisecond,
		BackoffCap:        200 * time.Millisecond,
		BackoffResetAfter: time.Second,
		SettleBeforeStop:  10 * time.Millisecond,
	}
}

// setHelperMode sets the fake daemon's mode for the children of one run. The
// mode is process-wide, so one run uses one mode (see the file comment).
func setHelperMode(t *testing.T, mode string) {
	t.Helper()
	t.Setenv(helperModeEnv, mode)
}

// runEntry builds a local running roster entry driving one project. repo is
// the project's primary_repo (empty when no workdir derivation is wanted).
func runEntry(name, repo string) RosterEntry {
	return RosterEntry{
		Name:         name,
		DesiredState: RosterDesiredRunning,
		Placement:    RosterPlacementLocal,
		Projects:     []RosterProject{{Slug: "acme", PrimaryRepo: repo}},
	}
}

// stopEntry builds a local stopped roster entry driving one project.
func stopEntry(name string) RosterEntry {
	return RosterEntry{
		Name:         name,
		DesiredState: RosterDesiredStopped,
		Placement:    RosterPlacementLocal,
		Projects:     []RosterProject{{Slug: "acme"}},
	}
}

// entryStateDir is the state dir the supervisor derives for one roster entry:
// the same derivation the implementation uses, so tests know where the fake
// daemon writes its iteration logs and where STOP lands.
func entryStateDir(cacheDir, name string) string {
	return rosterStateDir(cacheDir, name)
}

// countRuns reports how many iteration-*.log files the fake has written into
// stateDir — i.e. how many times that instance has been spawned.
func countRuns(t *testing.T, stateDir string) int {
	t.Helper()
	entries, err := os.ReadDir(stateDir)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "iteration-") && strings.HasSuffix(e.Name(), ".log") {
			n++
		}
	}
	return n
}

// waitFor polls cond until it holds or the timeout passes, then fails.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// runSupervise starts Supervise in a goroutine and returns a channel carrying
// its error (nil on a clean fleet stop).
func runSupervise(ctx context.Context, o Options) <-chan error {
	done := make(chan error, 1)
	go func() { done <- Supervise(ctx, o) }()
	return done
}

// lockedBuffer is a concurrency-safe log sink: the supervisor goroutine
// writes log lines into it while the test goroutine polls them.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// --- tests ------------------------------------------------------------------

// The supervisor starts exactly one child per running roster entry, and
// nothing for a stopped one.
func TestSpawnPerRosterEntry(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "sleep")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("daemon-one", ""), runEntry("daemon-two", ""), stopEntry("daemon-three")})
	srv := plane.serve(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, cacheDir, srv))

	a := entryStateDir(cacheDir, "daemon-one")
	b := entryStateDir(cacheDir, "daemon-two")
	c := entryStateDir(cacheDir, "daemon-three")
	waitFor(t, 5*time.Second, "both running instances to come up", func() bool {
		return countRuns(t, a) == 1 && countRuns(t, b) == 1
	})
	if got := countRuns(t, a) + countRuns(t, b); got != 2 {
		t.Fatalf("total spawns = %d, want exactly 2 (one per running entry)", got)
	}
	if got := countRuns(t, c); got != 0 {
		t.Fatalf("stopped entry spawned %d time(s), want 0", got)
	}

	cancel()
	waitFor(t, 5*time.Second, "the supervisor to return after the stop", func() bool {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Supervise returned %v, want nil", err)
			}
			return true
		default:
			return false
		}
	})
	if got := countRuns(t, a) + countRuns(t, b); got != 2 {
		t.Fatalf("total spawns after stop = %d, want 2 — a deliberate stop must not restart anything", got)
	}
}

// A child that exits unexpectedly is restarted, and the respawn rate backs off
// (the ladder doubles to the cap rather than spinning at the base interval).
func TestUnexpectedExitRestartWithBackoff(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "crash")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("crashy", "")})
	srv := plane.serve(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, cacheDir, srv))

	state := entryStateDir(cacheDir, "crashy")
	waitFor(t, 5*time.Second, "the crashing child to have been respawned at least twice", func() bool {
		return countRuns(t, state) >= 3
	})
	before := countRuns(t, state)
	time.Sleep(500 * time.Millisecond)
	after := countRuns(t, state)
	if after < before {
		t.Fatalf("spawn count went backwards: %d -> %d", before, after)
	}
	// At the 200ms cap, 500ms buys at most ~3 more spawns (+50, +150, +350),
	// and a 4th only if the sleep overruns ~550ms under CI load; at the flat
	// 50ms base the same window buys ~10. The bound sits wide of the cap's
	// worst case (6 tolerates a sleep that overruns to ~1s) and still far
	// below what a non-doubling ladder produces.
	if extra := after - before; extra > 6 {
		t.Fatalf("%d extra spawns in 500ms — the backoff is not doubling (cap %s, base %s)", extra, 200*time.Millisecond, 50*time.Millisecond)
	}

	cancel()
	waitFor(t, 5*time.Second, "the supervisor to return after the stop", func() bool {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Supervise returned %v, want nil", err)
			}
			return true
		default:
			return false
		}
	})
}

// Flipping desired state in the console lands on the machine within one poll
// and with no operator command: a running entry's child is stopped (STOP
// written, child drains, no respawn), and flipping back brings it up again.
func TestDesiredFlipWithinOnePoll(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "sleep-seen")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("daemon-one", "")})
	srv := plane.serve(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, cacheDir, srv))

	state := entryStateDir(cacheDir, "daemon-one")
	waitFor(t, 5*time.Second, "the running child to come up", func() bool { return countRuns(t, state) == 1 })

	// The console flip: the entry is now desired stopped. The STOP marker must
	// land and the child must drain within one poll — no operator command.
	plane.set([]RosterEntry{stopEntry("daemon-one")})
	waitFor(t, 5*time.Second, "STOP to land and the child to drain", func() bool {
		_, err := os.Lstat(filepath.Join(state, "STOP-SEEN"))
		return err == nil
	})
	if got := countRuns(t, state); got != 1 {
		t.Fatalf("spawns after the flip to stopped = %d, want 1 — a stopped child must not be restarted", got)
	}

	// Flip back: the next poll brings the child up again.
	plane.set([]RosterEntry{runEntry("daemon-one", "")})
	waitFor(t, 5*time.Second, "the child to come back up after the flip back to running", func() bool {
		return countRuns(t, state) == 2
	})

	cancel()
	waitFor(t, 5*time.Second, "the supervisor to return after the stop", func() bool {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Supervise returned %v, want nil", err)
			}
			return true
		default:
			return false
		}
	})
}

// Reconciliation is idempotent: repeated polls with unchanged state spawn
// nothing and write nothing — no respawn, no STOP marker.
func TestReconcileIsIdempotent(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "sleep")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("daemon-one", ""), runEntry("daemon-two", ""), stopEntry("daemon-three")})
	srv := plane.serve(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, cacheDir, srv))

	a := entryStateDir(cacheDir, "daemon-one")
	b := entryStateDir(cacheDir, "daemon-two")
	c := entryStateDir(cacheDir, "daemon-three")
	waitFor(t, 5*time.Second, "both running instances to come up", func() bool {
		return countRuns(t, a) == 1 && countRuns(t, b) == 1
	})

	// Let several polls pass over an unchanged roster: nothing may spawn and
	// nothing may be written — no STOP marker anywhere.
	time.Sleep(6 * 50 * time.Millisecond) // six polls at the test interval
	if got := countRuns(t, a) + countRuns(t, b); got != 2 {
		t.Fatalf("total spawns after six unchanged polls = %d, want 2 — reconcile is not idempotent", got)
	}
	for _, dir := range []string{a, b, c} {
		if _, err := os.Lstat(filepath.Join(dir, "STOP")); err == nil {
			t.Fatalf("an unchanged roster wrote STOP into %s — reconcile is not idempotent", dir)
		}
	}

	cancel()
	waitFor(t, 5*time.Second, "the supervisor to return after the stop", func() bool {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Supervise returned %v, want nil", err)
			}
			return true
		default:
			return false
		}
	})
}

// The fleet-wide drain: cancelling the context (the signal path) writes a STOP
// marker into every child's own state dir, every child exits, none restart.
func TestFleetWideDrainOnSignal(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "sleep-seen")
	plane := &fakePlane{}
	names := []string{"d1", "d2", "d3"}
	entries := make([]RosterEntry, 0, len(names))
	for _, n := range names {
		entries = append(entries, runEntry(n, ""))
	}
	plane.set(entries)
	srv := plane.serve(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, cacheDir, srv))

	states := make([]string, 0, len(names))
	for _, n := range names {
		states = append(states, entryStateDir(cacheDir, n))
	}
	for _, s := range states {
		waitFor(t, 5*time.Second, "all children to come up", func() bool { return countRuns(t, s) == 1 })
	}
	cancel()
	waitFor(t, 5*time.Second, "the supervisor to return after the stop", func() bool {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Supervise returned %v, want nil", err)
			}
			return true
		default:
			return false
		}
	})
	for _, s := range states {
		if _, err := os.Lstat(filepath.Join(s, "STOP-SEEN")); err != nil {
			t.Errorf("no STOP marker reached %s — the fleet-wide drain missed an instance", s)
		}
		if got := countRuns(t, s); got != 1 {
			t.Errorf("spawns in %s = %d, want 1 — the drain must not restart", s, got)
		}
	}
}

// HALT is the operator's per-instance stop switch: a child that exits with a
// HALT marker in its state dir is left stopped, not respawned.
func TestHaltLeavesInstanceStopped(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "halt")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("halted", "")})
	srv := plane.serve(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, cacheDir, srv))

	state := entryStateDir(cacheDir, "halted")
	waitFor(t, 5*time.Second, "the halting child to run once", func() bool { return countRuns(t, state) == 1 })
	// Six times the base backoff: if the HALT were ignored, a respawn would
	// have happened long before this.
	time.Sleep(300 * time.Millisecond)
	if got := countRuns(t, state); got != 1 {
		t.Fatalf("spawns = %d, want 1 — a child that exited under HALT must not be restarted", got)
	}
	cancel()
	waitFor(t, 5*time.Second, "the supervisor to return after the stop", func() bool {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Supervise returned %v, want nil", err)
			}
			return true
		default:
			return false
		}
	})
}

// A roster with nothing local on it is a clean no-op, not an error and not an
// idle hang.
func TestEmptyRosterReturnsCleanly(t *testing.T) {
	cacheDir := t.TempDir()
	plane := &fakePlane{}
	plane.set(nil)
	srv := plane.serve(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, cacheDir, srv))
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Supervise over an empty roster returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Supervise over an empty roster did not return")
	}
}

// A supervisor whose context is ALREADY cancelled must return cleanly, not
// deadlock: nothing was spawned, so no Wait goroutine exists and no exit
// event will ever arrive. (Regression: the pre-cancelled path used to hand
// to stopAll, which counted every never-spawned instance as live and then
// blocked forever on the exit channel.)
func TestPreCancelledContextReturnsCleanly(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "sleep")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("a", "")})
	srv := plane.serve(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before Supervise runs
	done := runSupervise(ctx, testOptions(t, cacheDir, srv))
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Supervise on a pre-cancelled ctx returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Supervise on a pre-cancelled ctx did not return - stopAll waits for children that were never spawned")
	}
	if got := countRuns(t, entryStateDir(cacheDir, "a")); got != 0 {
		t.Fatalf("spawns = %d, want 0 - a pre-cancelled supervisor must not spawn children", got)
	}
}

// A fleet stop that can make no progress must SAY so instead of hanging
// silently: a child stuck before its state dir exists (nothing to write STOP
// into) and a child that ignores STOP forever (the marker lands, the drain
// never ends) both leave the stop waiting by design — the supervisor never
// kills — and the wait must be loud.
func TestStopLogsWhenTheFleetCannotDrain(t *testing.T) {
	cacheDir := t.TempDir()
	cases := []struct {
		name string
		mode string
	}{
		{"child stuck before its state dir exists", "sleep-no-dir"},
		{"child that ignores STOP", "stubborn"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setHelperMode(t, tc.mode)
			plane := &fakePlane{}
			plane.set([]RosterEntry{runEntry("stuck", "")})
			srv := plane.serve(t)

			var buf lockedBuffer
			log.SetOutput(&buf)
			defer log.SetOutput(os.Stderr)

			o := testOptions(t, cacheDir, srv)
			o.StopLogEvery = 100 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			done := runSupervise(ctx, o)

			waitFor(t, 5*time.Second, "the child to be spawned", func() bool {
				return strings.Contains(buf.String(), "spawned (pid")
			})
			cancel()
			waitFor(t, 5*time.Second, "the stuck stop to log what it waits for", func() bool {
				return strings.Contains(buf.String(), "stop still waiting")
			})
			// The stop is stuck by construction; kill the child so the test
			// leaves no orphan, and verify the supervisor then finishes.
			if pid := spawnPid(t, buf.String()); pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
			waitFor(t, 5*time.Second, "the supervisor to return once the child is gone", func() bool {
				select {
				case err := <-done:
					if err != nil {
						t.Fatalf("Supervise returned %v, want nil", err)
					}
					return true
				default:
					return false
				}
			})
		})
	}
}

// spawnPid pulls the child pid out of a captured supervisor log line
// ("<name>: spawned (pid 123, <path>, version <v>)"), or 0 when no spawn is
// logged.
func spawnPid(t *testing.T, log string) int {
	t.Helper()
	const marker = "spawned (pid "
	i := strings.Index(log, marker)
	if i < 0 {
		return 0
	}
	rest := log[i+len(marker):]
	j := strings.Index(rest, ",")
	if j < 0 {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(rest[:j]))
	if err != nil {
		return 0
	}
	return pid
}

// The workdir derivation gate (phase 2a): with a machine root stated, an
// instance whose derived workdir fails the fail-closed conditions is refused —
// no child is ever spawned for it, the path tried is reported, and the rest of
// the fleet keeps running. The repo the entry's project names is what the
// derivation derives from.
func TestWorkdirDerivationFailureSpawnsNothing(t *testing.T) {
	root := t.TempDir()
	// The good instance's derived workdir must be a real checkout of the repo
	// its project names; the ghost instance's derived path will simply not
	// exist.
	makeCheckout(t, filepath.Join(root, "widgets"), "acme/widgets")

	cacheDir := t.TempDir()
	setHelperMode(t, "sleep")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("good", "acme/widgets"), runEntry("ghost", "acme/ghost")})
	srv := plane.serve(t)

	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	o := testOptions(t, cacheDir, srv)
	o.WorkdirRoot = root
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, o)

	goodState := entryStateDir(cacheDir, "good")
	ghostState := entryStateDir(cacheDir, "ghost")
	// The derivable instance derives, spawns, and runs.
	waitFor(t, 5*time.Second, "the derivable instance to come up", func() bool {
		return countRuns(t, goodState) == 1
	})
	// The refused instance must NEVER spawn, however long the fleet runs.
	time.Sleep(300 * time.Millisecond)
	if got := countRuns(t, ghostState); got != 0 {
		t.Fatalf("spawns for the refused instance = %d, want 0 — a failed derivation must spawn nothing", got)
	}
	// The refusal is loud and names the path that was tried.
	if logText := buf.String(); !strings.Contains(logText, filepath.Join(root, "ghost")) {
		t.Fatalf("the refusal log does not name the path tried %q:\n%s", filepath.Join(root, "ghost"), logText)
	}

	cancel()
	waitFor(t, 5*time.Second, "the supervisor to return after the stop", func() bool {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Supervise returned %v, want nil", err)
			}
			return true
		default:
			return false
		}
	})
	if got := countRuns(t, ghostState); got != 0 {
		t.Fatalf("spawns for the refused instance after the stop = %d, want 0", got)
	}
}

// With no machine root stated, derivation is OFF and every valid entry is
// supervised on its base config's own workdir — the phase-1 behaviour,
// unchanged.
func TestWorkdirDerivationOffWithoutRoot(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "sleep")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("daemon", "acme/ghost")})
	srv := plane.serve(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, cacheDir, srv))

	state := entryStateDir(cacheDir, "daemon")
	// The ghost repo has no checkout anywhere, yet the instance runs: without
	// a stated root there is nothing to derive, so the base config's own
	// workdir governs, exactly as before phase 2a.
	waitFor(t, 5*time.Second, "the instance to come up", func() bool { return countRuns(t, state) == 1 })
	cancel()
	waitFor(t, 5*time.Second, "the supervisor to return after the stop", func() bool {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Supervise returned %v, want nil", err)
			}
			return true
		default:
			return false
		}
	})
}

// A roster entry with placement remote is ignored without error: no child is
// ever spawned for it, the rest of the fleet keeps running, and the ignore is
// said once per entry (Decision 7 — the plane refuses them at write time; the
// supervisor still must not fail on one).
func TestRemotePlacementIgnoredWithoutError(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "sleep")
	plane := &fakePlane{}
	plane.set([]RosterEntry{
		runEntry("local-one", ""),
		{Name: "remote-one", DesiredState: RosterDesiredRunning, Placement: RosterPlacementRemote,
			Projects: []RosterProject{{Slug: "acme"}}},
	})
	srv := plane.serve(t)

	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, cacheDir, srv))

	localState := entryStateDir(cacheDir, "local-one")
	remoteState := entryStateDir(cacheDir, "remote-one")
	waitFor(t, 5*time.Second, "the local instance to come up", func() bool {
		return countRuns(t, localState) == 1
	})
	// The remote instance never spawns, and the ignore is logged (once).
	time.Sleep(300 * time.Millisecond)
	if got := countRuns(t, remoteState); got != 0 {
		t.Fatalf("spawns for the remote-placed instance = %d, want 0 — remote placement is ignored", got)
	}
	if logText := buf.String(); !strings.Contains(logText, `ignoring entry "remote-one"`) {
		t.Fatalf("the ignore is not logged:\n%s", logText)
	}

	cancel()
	waitFor(t, 5*time.Second, "the supervisor to return after the stop", func() bool {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Supervise returned %v, want nil", err)
			}
			return true
		default:
			return false
		}
	})
}

// A roster entry carrying a key that may not come from the plane is refused
// loudly at reconcile: the refusal names the entry AND the key, no child is
// ever spawned for it, and the rest of the fleet keeps running. Each refusal
// class is its own entry — machine-layer keys, run-config keys, and unknown
// keys.
func TestRefusedEntryNamedLoudlySpawnsNothing(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "sleep")
	plane := &fakePlane{}
	bad := []struct {
		name string
		key  string
		val  string
	}{
		{"bad-env", "env", `{"FOO": "bar"}`},
		{"bad-settings", "settings_path", `"/tmp/nope.json"`},
		{"bad-workdir", "workdir", `"/tmp/nope"`},
		{"bad-mcp", "mcp_config_path", `"/tmp/nope.json"`},
		{"bad-model", "model", `"opus"`},
		{"bad-budget", "budget", `{"per_session": 1}`},
		{"bad-mystery", "nope", `1`},
	}
	entries := []RosterEntry{runEntry("good", "")}
	for _, b := range bad {
		entries = append(entries, RosterEntry{
			Name: b.name, DesiredState: RosterDesiredRunning, Placement: RosterPlacementLocal,
			Projects:  []RosterProject{{Slug: "acme"}},
			Overrides: map[string]json.RawMessage{b.key: json.RawMessage(b.val)},
		})
	}
	plane.set(entries)
	srv := plane.serve(t)

	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, cacheDir, srv))

	goodState := entryStateDir(cacheDir, "good")
	waitFor(t, 5*time.Second, "the good instance to come up", func() bool {
		return countRuns(t, goodState) == 1
	})
	time.Sleep(300 * time.Millisecond)
	for _, b := range bad {
		if got := countRuns(t, entryStateDir(cacheDir, b.name)); got != 0 {
			t.Errorf("refused entry %q spawned %d time(s), want 0", b.name, got)
		}
		if logText := buf.String(); !strings.Contains(logText, b.name) || !strings.Contains(logText, b.key) {
			t.Errorf("the refusal of %q is not logged naming both the entry and the key %q:\n%s", b.name, b.key, logText)
		}
	}

	cancel()
	waitFor(t, 5*time.Second, "the supervisor to return after the stop", func() bool {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Supervise returned %v, want nil", err)
			}
			return true
		default:
			return false
		}
	})
}

// A cold start with the plane unreachable reconciles from the cached roster on
// disk: the running entry's child comes up, the stopped entry stays stopped,
// and the log says what it started from.
func TestColdStartFromCachedRosterWhenPlaneUnreachable(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "sleep")
	// A previous run's successful poll left the last-known-good roster.
	writeCachedRoster(cacheDir, []RosterEntry{runEntry("daemon-one", ""), stopEntry("daemon-two")})

	plane := &fakePlane{}
	plane.setDown(true) // the plane is unreachable at start
	srv := plane.serve(t)

	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, cacheDir, srv))

	a := entryStateDir(cacheDir, "daemon-one")
	b := entryStateDir(cacheDir, "daemon-two")
	waitFor(t, 5*time.Second, "the cached running entry to come up", func() bool {
		return countRuns(t, a) == 1
	})
	if got := countRuns(t, b); got != 0 {
		t.Fatalf("cached stopped entry spawned %d time(s), want 0", got)
	}
	if logText := buf.String(); !strings.Contains(logText, "starting from the cached roster") {
		t.Fatalf("the cold-start fallback is not logged:\n%s", logText)
	}

	cancel()
	waitFor(t, 5*time.Second, "the supervisor to return after the stop", func() bool {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Supervise returned %v, want nil", err)
			}
			return true
		default:
			return false
		}
	})
}

// A warm supervisor whose plane goes down keeps reconciling against the
// last-known-good roster: the running children stay up, nothing respawns, and
// the log says the plane is unreachable.
func TestPlaneLossKeepsFleetRunning(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "sleep")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("daemon-one", "")})
	srv := plane.serve(t)

	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, cacheDir, srv))

	state := entryStateDir(cacheDir, "daemon-one")
	waitFor(t, 5*time.Second, "the child to come up", func() bool { return countRuns(t, state) == 1 })

	plane.setDown(true) // the plane goes away mid-run
	waitFor(t, 5*time.Second, "the plane loss to be logged", func() bool {
		return strings.Contains(buf.String(), "plane unreachable") &&
			strings.Contains(buf.String(), "last-known-good roster")
	})
	// Several polls against the last-known-good roster: the child stays up,
	// nothing respawns, no STOP is written.
	time.Sleep(6 * 50 * time.Millisecond)
	if got := countRuns(t, state); got != 1 {
		t.Fatalf("spawns after plane loss = %d, want 1 — the fleet must keep running from the last-known-good roster", got)
	}
	if _, err := os.Lstat(filepath.Join(state, "STOP")); err == nil {
		t.Fatal("a STOP marker was written while the plane was merely unreachable")
	}

	cancel()
	waitFor(t, 5*time.Second, "the supervisor to return after the stop", func() bool {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Supervise returned %v, want nil", err)
			}
			return true
		default:
			return false
		}
	})
}

// The permission-policy gate (phase 2c): an instance whose effective
// settings_path names a file that does not exist is refused at reconcile — no
// child is ever spawned for it, the refusal names the daemon and the path
// tried, and the rest of the fleet keeps running. The policy is a MACHINE-LAYER
// value, so the base config carries it per harness: the good entry runs on
// claude (whose policy exists), the ghost entry overrides harness to opencode
// (whose per-harness policy is missing).
func TestPermissionPolicyAbsentSpawnsNothing(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "sleep")
	plane := &fakePlane{}
	plane.set([]RosterEntry{
		runEntry("good", ""),
		{Name: "ghost", DesiredState: RosterDesiredRunning, Placement: RosterPlacementLocal,
			Projects: []RosterProject{{Slug: "acme"}},
			Overrides: map[string]json.RawMessage{
				"harness": json.RawMessage(`"opencode"`),
			}},
	})
	srv := plane.serve(t)

	policy := filepath.Join(t.TempDir(), "headless.json")
	if err := os.WriteFile(policy, []byte(`{"permissions": {"allow": []}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "headless-missing.json")

	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	o := testOptions(t, cacheDir, srv)
	// The machine layer: loaded under the isolated HOME (so defaults fill the
	// prompt and the rest), then the per-harness policies overlaid — claude's
	// exists, opencode's does not.
	base, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	base.SettingsPath = policy
	base.Harnesses = map[string]config.HarnessConfig{
		"opencode": {SettingsPath: missing},
	}
	o.BaseCfg = base
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, o)

	goodState := entryStateDir(cacheDir, "good")
	ghostState := entryStateDir(cacheDir, "ghost")
	// The instance whose policy file exists spawns and runs.
	waitFor(t, 5*time.Second, "the instance with a policy file to come up", func() bool {
		return countRuns(t, goodState) == 1
	})
	// The refused instance must NEVER spawn, however long the fleet runs.
	time.Sleep(300 * time.Millisecond)
	if got := countRuns(t, ghostState); got != 0 {
		t.Fatalf("spawns for the refused instance = %d, want 0 — a missing permission policy must spawn nothing", got)
	}
	// The refusal is loud and names the daemon and the policy path that was
	// tried.
	logText := buf.String()
	if !strings.Contains(logText, missing) {
		t.Fatalf("the refusal log does not name the policy path tried %q:\n%s", missing, logText)
	}
	if !strings.Contains(logText, "ghost") {
		t.Fatalf("the refusal log does not name the refused daemon:\n%s", logText)
	}

	cancel()
	waitFor(t, 5*time.Second, "the supervisor to return after the stop", func() bool {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Supervise returned %v, want nil", err)
			}
			return true
		default:
			return false
		}
	})
	if got := countRuns(t, ghostState); got != 0 {
		t.Fatalf("spawns for the refused instance after the stop = %d, want 0", got)
	}
}

// The machine layer is the common single-harness config: no `harnesses:` key,
// so Harnesses is nil and Clone keeps it nil. A roster entry whose permitted
// `harnesses` override names a block over that shape used to panic in
// buildConfig with the same "assignment to entry in nil map" the ApplyRunConfig
// guard (CLA-474) fixes - the same root cause one path over: the override loop
// wrote straight into the nil map.
func TestBuildConfigHarnessesOverrideAllocatesNilBase(t *testing.T) {
	d := &Supervisor{cacheDir: t.TempDir()}
	d.o.BaseCfg = &config.Config{Harness: "claude", Prompt: "Work the next backlog item."}
	entry := RosterEntry{
		Name:         "one",
		DesiredState: RosterDesiredRunning,
		Placement:    RosterPlacementLocal,
		Projects:     []RosterProject{{Slug: "acme"}},
		Overrides: map[string]json.RawMessage{
			"harnesses": json.RawMessage(`{"opencode":{"model":"sonnet"}}`),
		},
	}
	cfg, _, err := d.buildConfig(&entry)
	if err != nil {
		t.Fatalf("buildConfig: %v", err)
	}
	if got := cfg.Harnesses["opencode"]; got.Model != "sonnet" {
		t.Errorf("opencode block model = %q, want the override (buildConfig must allocate the nil map)", got.Model)
	}
}

// The spawn-time gate (phase 2c): a policy file deleted AFTER the instance was
// admitted refuses the next respawn — the running child is untouched (it
// already has its policy loaded), but the child-start gate must not start a
// new one without the file. Once the file is restored, the backoff retry
// respawns the child.
func TestPermissionPolicyRemovedAfterStartRefusesRespawn(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "sleep")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("daemon-one", "")})
	srv := plane.serve(t)

	policy := filepath.Join(t.TempDir(), "headless.json")
	if err := os.WriteFile(policy, []byte(`{"permissions": {"allow": []}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	o := testOptions(t, cacheDir, srv)
	base, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	base.SettingsPath = policy
	o.BaseCfg = base
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, o)

	state := entryStateDir(cacheDir, "daemon-one")
	waitFor(t, 5*time.Second, "the child to come up", func() bool { return countRuns(t, state) == 1 })
	// Take the policy away, then kill the child so the supervisor must respawn.
	if err := os.Remove(policy); err != nil {
		t.Fatal(err)
	}
	pid := spawnPid(t, buf.String())
	if pid <= 0 {
		t.Fatalf("no spawned pid in the log:\n%s", buf.String())
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	// The respawn must be refused, naming the daemon and the policy path. The
	// refusal can be spoken by either gate — the poll's re-resolve ("refused:")
	// or the spawn itself ("refusing to start:") — so accept either wording;
	// what must hold either way is that the path is named and no child starts.
	waitFor(t, 5*time.Second, "the refused respawn to be logged", func() bool {
		logText := buf.String()
		return strings.Contains(logText, policy) &&
			(strings.Contains(logText, "refused:") || strings.Contains(logText, "refusing to start"))
	})
	if got := countRuns(t, state); got != 1 {
		t.Fatalf("spawns after the policy file was removed = %d, want 1 — the gate must not start a child without its policy", got)
	}
	// Restore the policy file: the next backoff retry starts the child again.
	if err := os.WriteFile(policy, []byte(`{"permissions": {"allow": []}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "the child to be respawned once the policy file is back", func() bool {
		return countRuns(t, state) == 2
	})

	cancel()
	waitFor(t, 5*time.Second, "the supervisor to return after the stop", func() bool {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Supervise returned %v, want nil", err)
			}
			return true
		default:
			return false
		}
	})
}

// The spawn-time gate (phase 2c) refuses a policy that goes missing from an
// EDITED base config, even when the previous config's policy file still
// exists: the daemon must not be respawned on the last materialized config
// under a policy the operator has replaced. The base config is the machine
// layer, so the edit lands on it; the backoff retry picks up the restored file
// without a supervisor restart.
func TestPermissionPolicyEditedToAbsentRefusesRespawn(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "sleep")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("daemon-one", "")})
	srv := plane.serve(t)

	oldPolicy := filepath.Join(t.TempDir(), "headless-old.json")
	if err := os.WriteFile(oldPolicy, []byte(`{"permissions": {"allow": []}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	newPolicy := filepath.Join(t.TempDir(), "headless-new.json") // named, never created

	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	o := testOptions(t, cacheDir, srv)
	// The machine layer is the operator's config FILE: buildConfig re-reads
	// the base's source on every build, so the edit lands on disk — the shape
	// an operator's edit takes, and the only race-free way to change what a
	// RUNNING supervisor reads (mutating the in-memory base would race the
	// poll loop's re-read).
	basePath := filepath.Join(t.TempDir(), "config.json")
	writeBase := func(settingsPath string) {
		t.Helper()
		if err := os.WriteFile(basePath, []byte(fmt.Sprintf(`{"settings_path": %q}`, settingsPath)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeBase(oldPolicy)
	base, err := config.Load(basePath)
	if err != nil {
		t.Fatal(err)
	}
	o.BaseCfg = base
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, o)

	state := entryStateDir(cacheDir, "daemon-one")
	waitFor(t, 5*time.Second, "the child to come up", func() bool { return countRuns(t, state) == 1 })
	// Edit the machine layer to name a policy file that does not exist. The
	// OLD policy file stays on disk, so the last materialized config would
	// pass the gate — the refusal must come from the re-resolve, and it must
	// NOT fall back to that config.
	writeBase(newPolicy)
	pid := spawnPid(t, buf.String())
	if pid <= 0 {
		t.Fatalf("no spawned pid in the log:\n%s", buf.String())
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "the refused respawn to be logged", func() bool {
		logText := buf.String()
		return strings.Contains(logText, newPolicy) &&
			(strings.Contains(logText, "refused:") || strings.Contains(logText, "refusing to start"))
	})
	// No child may spawn on the old materialized config, however long the
	// backoff retries run against the still-absent file.
	time.Sleep(300 * time.Millisecond)
	if got := countRuns(t, state); got != 1 {
		t.Fatalf("spawns after the source named an absent policy = %d, want 1 — the daemon must not fall back to the last materialized config", got)
	}
	// Restore the policy file: the next backoff retry starts the child again.
	if err := os.WriteFile(newPolicy, []byte(`{"permissions": {"allow": []}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "the child to be respawned once the policy file is back", func() bool {
		return countRuns(t, state) == 2
	})

	cancel()
	waitFor(t, 5*time.Second, "the supervisor to return after the stop", func() bool {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Supervise returned %v, want nil", err)
			}
			return true
		default:
			return false
		}
	})
}

// backoffDelay is the pure schedule: double from the base up to the cap, and
// reset to the base after a healthy run.
func TestBackoffDelay(t *testing.T) {
	const (
		base       = 2 * time.Second
		cap        = 60 * time.Second
		resetAfter = 2 * time.Minute
	)
	cases := []struct {
		name   string
		prev   time.Duration
		uptime time.Duration
		want   time.Duration
	}{
		{"first crash", 0, 100 * time.Millisecond, base},
		{"doubles", base, 100 * time.Millisecond, 4 * time.Second},
		{"doubles again", 4 * time.Second, 100 * time.Millisecond, 8 * time.Second},
		{"caps", 30 * time.Second, 100 * time.Millisecond, cap},
		{"stays capped", cap, 100 * time.Millisecond, cap},
		{"healthy run resets", cap, resetAfter + time.Second, base},
		{"healthy run from zero", 0, resetAfter, base},
	}
	for _, tc := range cases {
		if got := backoffDelay(tc.prev, tc.uptime, base, cap, resetAfter); got != tc.want {
			t.Errorf("%s: backoffDelay(%v, %v) = %v, want %v", tc.name, tc.prev, tc.uptime, got, tc.want)
		}
	}
}

// spawnEnv must drop a pre-existing CLANKERBAR_CHILD_VERSION from the parent
// environment: getenv returns the FIRST match for a duplicated key, so an
// operator env carrying the reserved name would otherwise shadow the
// supervisor's recorded value in the child.
func TestSpawnEnvDropsAPreExistingChildVersion(t *testing.T) {
	t.Setenv(childVersionEnv, "operator-stale")
	t.Setenv("KEEP_ME", "yes")
	env := spawnEnv("1.2.3")
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, childVersionEnv+"=operator-stale") {
		t.Fatalf("spawnEnv kept a pre-existing %s: %v", childVersionEnv, env)
	}
	if !strings.Contains(joined, childVersionEnv+"=1.2.3") {
		t.Fatalf("spawnEnv lost the supervisor's value: %v", env)
	}
	if !strings.Contains(joined, "KEEP_ME=yes") {
		t.Fatalf("spawnEnv dropped an unrelated variable: %v", env)
	}
	if got := len(env); got != len(os.Environ()) {
		t.Fatalf("spawnEnv changed the variable count: %d entries -> %d", len(os.Environ()), got)
	}
}
