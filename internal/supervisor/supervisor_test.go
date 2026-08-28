package supervisor

// The supervisor's tests drive it against a FAKE daemon: the test binary
// re-invoked as the child (TestMain -> helperMain), behaving per the mode
// recorded in the CLANKERBAR_SUPER_MODE environment variable. The fake mirrors
// the real daemon's load-bearing behaviours and nothing else:
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
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/teststate"
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
			time.Sleep(20 * time.Millisecond)
		}
	case "sleep-no-dir":
		// Alive forever, no state dir: a real daemon wedged (or dead) before
		// its statedir.Open. The supervisor can never write STOP to it.
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

// testOptions returns supervisor options pointed at the test binary as the
// child, with fast intervals so the tests do not wait on production defaults.
func testOptions(t *testing.T, dir string) Options {
	t.Helper()
	t.Setenv(helperEnv, "1") // children must re-enter as the fake daemon
	return Options{
		ConfigDir:         dir,
		Binary:            os.Args[0],
		BackoffBase:       50 * time.Millisecond,
		BackoffCap:        200 * time.Millisecond,
		BackoffResetAfter: time.Second,
		SettleBeforeStop:  10 * time.Millisecond,
	}
}

// writeInstanceConfig drops one fake-daemon config file into dir. The MODE is
// carried in the process env (helperModeEnv), inherited by every child: since
// phase 2b the child reads the supervisor's MATERIALIZED config, which carries
// only real config fields — so a test-only key would not survive
// materialization, and the mode has to travel outside the file.
func writeInstanceConfig(t *testing.T, dir, name, mode, stateDir string) {
	t.Helper()
	t.Setenv(helperModeEnv, mode)
	body := fmt.Sprintf(`{"harness": "claude", "state_dir": %q}`, stateDir)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeInstanceConfigRepo is writeInstanceConfig with a primary_repo declared,
// so the workdir derivation (phase 2a) has a repo to derive from.
func writeInstanceConfigRepo(t *testing.T, dir, name, mode, stateDir, primary string) {
	t.Helper()
	t.Setenv(helperModeEnv, mode)
	body := fmt.Sprintf(`{"harness": "claude", "state_dir": %q, "primary_repo": %q}`, stateDir, primary)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
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

// The supervisor starts exactly one child per instance config, and ignores the
// other JSON that shares the config dir.
func TestSpawnPerFile(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(t.TempDir(), "state-a")
	b := filepath.Join(t.TempDir(), "state-b")
	writeInstanceConfig(t, dir, "a.json", "sleep", a)
	writeInstanceConfig(t, dir, "b.json", "sleep", b)
	// Not instance configs: an MCP config, a headless permission policy, and
	// unparseable JSON. None may spawn a child.
	os.WriteFile(filepath.Join(dir, "mcp.json"), []byte(`{"mcp": {"cb": {"type": "remote", "url": "https://plane/mcp/slug"}}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "headless.json"), []byte(`{"permissions": {"allow": ["Bash(go test:*)"]}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "broken.json"), []byte(`{not json`), 0o644)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, dir))

	waitFor(t, 5*time.Second, "both instances to come up", func() bool {
		return countRuns(t, a) == 1 && countRuns(t, b) == 1
	})
	if got := countRuns(t, a) + countRuns(t, b); got != 2 {
		t.Fatalf("total spawns = %d, want exactly 2 (one per instance config)", got)
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
	dir := t.TempDir()
	state := filepath.Join(t.TempDir(), "state")
	writeInstanceConfig(t, dir, "crashy.json", "crash", state)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, dir))

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

// A child the supervisor told to stop is NOT restarted: the STOP marker lands,
// the child drains (exits) at its boundary, and the supervisor waits for it.
func TestDeliberateStopNoRestart(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(t.TempDir(), "state")
	writeInstanceConfig(t, dir, "daemon.json", "sleep", state)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, dir))

	waitFor(t, 5*time.Second, "the child to come up", func() bool { return countRuns(t, state) == 1 })
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
	if got := countRuns(t, state); got != 1 {
		t.Fatalf("spawns = %d, want 1 — a child told to stop must not be restarted", got)
	}
}

// The fleet-wide drain: cancelling the context (the signal path) writes a STOP
// marker into every child's own state dir, every child exits, none restart.
func TestFleetWideDrainOnSignal(t *testing.T) {
	dir := t.TempDir()
	states := []string{
		filepath.Join(t.TempDir(), "s1"),
		filepath.Join(t.TempDir(), "s2"),
		filepath.Join(t.TempDir(), "s3"),
	}
	for i, s := range states {
		writeInstanceConfig(t, dir, fmt.Sprintf("d%d.json", i+1), "sleep-seen", s)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, dir))

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
	dir := t.TempDir()
	state := filepath.Join(t.TempDir(), "state")
	writeInstanceConfig(t, dir, "halted.json", "halt", state)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, dir))

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

// A config dir with nothing superviseable in it is a clean no-op, not an
// error and not an idle hang.
func TestEmptyConfigDirReturnsCleanly(t *testing.T) {
	dir := t.TempDir() // empty
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, dir))
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Supervise on an empty dir returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Supervise on an empty dir did not return")
	}
}

// A supervisor whose context is ALREADY cancelled must return cleanly, not
// deadlock: nothing was spawned, so no Wait goroutine exists and no exit
// event will ever arrive. (Regression: the pre-cancelled path used to hand
// to stopAll, which counted every never-spawned instance as live and then
// blocked forever on the exit channel.)
func TestPreCancelledContextReturnsCleanly(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(t.TempDir(), "state")
	writeInstanceConfig(t, dir, "a.json", "sleep", state)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before Supervise runs
	done := runSupervise(ctx, testOptions(t, dir))
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Supervise on a pre-cancelled ctx returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Supervise on a pre-cancelled ctx did not return - stopAll waits for children that were never spawned")
	}
	if got := countRuns(t, state); got != 0 {
		t.Fatalf("spawns = %d, want 0 - a pre-cancelled supervisor must not spawn children", got)
	}
}

// A fleet stop that can make no progress must SAY so instead of hanging
// silently: a child stuck before its state dir exists (nothing to write STOP
// into) and a child that ignores STOP forever (the marker lands, the drain
// never ends) both leave the stop waiting by design — the supervisor never
// kills — and the wait must be loud.
func TestStopLogsWhenTheFleetCannotDrain(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		mode string
	}{
		{"child stuck before its state dir exists", "sleep-no-dir"},
		{"child that ignores STOP", "stubborn"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "state")
			writeInstanceConfig(t, dir, "stuck.json", tc.mode, state)

			var buf lockedBuffer
			log.SetOutput(&buf)
			defer log.SetOutput(os.Stderr)

			o := testOptions(t, dir)
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
// ("<name>: spawned (pid 123, <path>)"), or 0 when no spawn is logged.
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
// the fleet keeps running.
func TestWorkdirDerivationFailureSpawnsNothing(t *testing.T) {
	root := t.TempDir()
	// The good instance's derived workdir must be a real checkout of the repo
	// its config names; the ghost instance's derived path will simply not exist.
	makeCheckout(t, filepath.Join(root, "widgets"), "acme/widgets")

	dir := t.TempDir()
	goodState := filepath.Join(t.TempDir(), "state-good")
	ghostState := filepath.Join(t.TempDir(), "state-ghost")
	writeInstanceConfigRepo(t, dir, "good.json", "sleep", goodState, "acme/widgets")
	writeInstanceConfigRepo(t, dir, "ghost.json", "sleep", ghostState, "acme/ghost")

	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	o := testOptions(t, dir)
	o.WorkdirRoot = root
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, o)

	// The good instance derives, spawns, and runs.
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

// With no machine root stated, derivation is OFF and every valid config is
// supervised on its own declared workdir — the phase-1 behaviour, unchanged.
func TestWorkdirDerivationOffWithoutRoot(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(t.TempDir(), "state")
	writeInstanceConfigRepo(t, dir, "daemon.json", "sleep", state, "acme/ghost")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, dir)) // no WorkdirRoot

	// The ghost repo has no checkout anywhere, yet the instance runs: without
	// a stated root there is nothing to derive, so the config's own workdir
	// governs, exactly as before phase 2a.
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

// enumerate keeps exactly the parseable, valid instance configs and reports
// the skips as log lines.
func TestEnumerateSkipsNonConfigs(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(t.TempDir(), "state-a")
	writeInstanceConfig(t, dir, "a.json", "sleep", a)
	os.WriteFile(filepath.Join(dir, "mcp.json"), []byte(`{"mcp": {}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "policy.json"), []byte(`{"permissions": {}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "broken.json"), []byte(`{`), 0o644)
	// A config that would fail validation (unknown harness) is skipped too.
	os.WriteFile(filepath.Join(dir, "bad-harness.json"), []byte(`{"harness": "no-such-harness", "state_dir": "/tmp/x"}`), 0o644)

	d := &Supervisor{o: testOptions(t, dir).withDefaults()}
	if err := d.enumerate(); err != nil {
		t.Fatal(err)
	}
	if len(d.instances) != 1 {
		t.Fatalf("enumerate kept %d instance(s), want 1 — only a.json is a valid instance config", len(d.instances))
	}
	if d.instances[0].path != filepath.Join(dir, "a.json") {
		t.Fatalf("kept %s, want a.json", d.instances[0].path)
	}
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

// The supervisor sorts nothing itself; enumeration comes back in the glob's
// sorted order, so two instances are stable and distinct.
func TestEnumerateSortsInstances(t *testing.T) {
	dir := t.TempDir()
	writeInstanceConfig(t, dir, "b.json", "sleep", filepath.Join(t.TempDir(), "b"))
	writeInstanceConfig(t, dir, "a.json", "sleep", filepath.Join(t.TempDir(), "a"))

	d := &Supervisor{o: testOptions(t, dir).withDefaults()}
	if err := d.enumerate(); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, inst := range d.instances {
		names = append(names, filepath.Base(inst.path))
	}
	sort.Strings(names)
	got := make([]string, len(d.instances))
	for i, inst := range d.instances {
		got[i] = filepath.Base(inst.path)
	}
	if strings.Join(got, ",") != strings.Join(names, ",") {
		t.Fatalf("instances not in sorted order: %v", got)
	}
}
