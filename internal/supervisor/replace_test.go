package supervisor

// Phase 5c (daemon-supervisor): the supervisor replaces itself in place once
// its children are drained. The tests drive the same fake-daemon + fake-plane
// harness the roll uses: the REPLACE marker is written into the cache dir
// (through the Replace command or directly), and the supervisor honours it at
// its next poll — drain every child at its iteration boundary, then exec the
// launch path. The exec is intercepted through the ExecInPlace hook: the test
// records the call (so the drain-before-exec ordering can be pinned in the
// log) and returns an error (so the supervisor resumes the loop and the fleet
// is respawned — the same shape a failed exec takes in production).

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/version"
)

// execCall is one recorded ExecInPlace invocation: what the replacement would
// have exec'd.
type execCall struct {
	bin  string
	argv []string
	env  []string
}

// execRecorder is a concurrency-safe record of ExecInPlace calls: the hook
// runs on the supervisor goroutine while the test polls.
type execRecorder struct {
	mu    sync.Mutex
	calls []execCall
}

func (r *execRecorder) hook() func(bin string, argv []string, env []string) error {
	return func(bin string, argv []string, env []string) error {
		r.mu.Lock()
		r.calls = append(r.calls, execCall{bin: bin, argv: argv, env: env})
		r.mu.Unlock()
		return errors.New("exec refused by the test")
	}
}

func (r *execRecorder) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *execRecorder) first() execCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[0]
}

// The drain-before-exec gate: a REPLACE marker lands while two children run;
// the supervisor writes STOP to BOTH, waits for BOTH to drain at their
// iteration boundary, and only then invokes the exec — the ordering pinned in
// the log. The hook's failure resumes the loop: the children are respawned
// (from the launch path), the consumed marker does not re-trigger, and the
// fleet keeps serving on the old supervisor.
func TestReplaceDrainsEveryChildBeforeTheExec(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "sleep")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("daemon-one", ""), runEntry("daemon-two", "")})
	srv := plane.serve(t)

	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := testOptions(t, cacheDir, srv)
	o.LaunchVersion = func() (string, error) { return version.Current, nil }
	o.VerifyTimeout = 5 * time.Second
	rec := &execRecorder{}
	o.ExecInPlace = rec.hook()
	done := runSupervise(ctx, o)

	a := entryStateDir(cacheDir, "daemon-one")
	b := entryStateDir(cacheDir, "daemon-two")
	waitFor(t, 5*time.Second, "both children to come up", func() bool {
		return countRuns(t, a) == 1 && countRuns(t, b) == 1
	})

	if err := Replace(context.Background(), o); err != nil {
		t.Fatalf("Replace returned %v, want nil", err)
	}
	waitFor(t, 5*time.Second, "the exec to be invoked after the drain", func() bool {
		return rec.len() == 1
	})

	// The gate: BOTH children got their STOP and drained BEFORE the exec.
	text := buf.String()
	iExec := strings.Index(text, "replace: the fleet is drained - replacing myself in place")
	if iExec < 0 {
		t.Fatalf("no replacement line in the log:\n%s", text)
	}
	for _, want := range []string{
		"replace: REPLACE honoured - writing STOP to every child",
		"daemon-one: STOP written to",
		"daemon-two: STOP written to",
	} {
		i := strings.Index(text, want)
		if i < 0 {
			t.Errorf("log does not contain %q:\n%s", want, text)
			continue
		}
		if i > iExec {
			t.Errorf("%q logged after the replacement (%d > %d) - the drain did not precede the exec:\n%s", want, i, iExec, text)
		}
	}

	// What would have been exec'd: the launch binary, the supervisor's own
	// argv and environment.
	call := rec.first()
	if call.bin != o.Binary {
		t.Errorf("exec binary = %q, want %q", call.bin, o.Binary)
	}
	if !reflect.DeepEqual(call.argv, os.Args) {
		t.Errorf("exec argv = %v, want the supervisor's own %v", call.argv, os.Args)
	}
	if len(call.env) == 0 {
		t.Error("exec env is empty, want the supervisor's environment")
	}
	// The marker was consumed: the replacement cannot re-trigger.
	if _, err := os.Lstat(filepath.Join(cacheDir, replaceMarkerName)); !os.IsNotExist(err) {
		t.Errorf("the REPLACE marker was not consumed; stat err = %v", err)
	}

	// The exec failed (the hook's contract): the supervisor resumes the loop
	// and the drained children are respawned — the fleet keeps serving on the
	// old supervisor, and the consumed marker never re-triggers.
	waitFor(t, 5*time.Second, "the fleet to be respawned after the failed exec", func() bool {
		return countRuns(t, a) == 2 && countRuns(t, b) == 2
	})
	time.Sleep(300 * time.Millisecond) // several polls with no marker
	if got := rec.len(); got != 1 {
		t.Errorf("exec invoked %d time(s), want 1 - the consumed marker must not re-trigger", got)
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

// A child that never drains HALTS the replacement: the drain hits
// VerifyTimeout, the exec is never invoked, the marker is consumed so nothing
// re-triggers, and the log names the stuck child — the supervisor never
// replaces itself over a fleet that is not drained.
func TestReplaceHaltsWithoutExecWhenAChildNeverDrains(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "stubborn")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("stuck", "")})
	srv := plane.serve(t)

	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := testOptions(t, cacheDir, srv)
	o.LaunchVersion = func() (string, error) { return version.Current, nil }
	o.VerifyTimeout = 300 * time.Millisecond
	rec := &execRecorder{}
	o.ExecInPlace = rec.hook()
	done := runSupervise(ctx, o)

	state := entryStateDir(cacheDir, "stuck")
	waitFor(t, 5*time.Second, "the stubborn child to come up", func() bool {
		return countRuns(t, state) == 1
	})

	writeMarker(t, cacheDir, version.Current)
	waitFor(t, 5*time.Second, "the halted replacement to be logged", func() bool {
		text := buf.String()
		return strings.Contains(text, "did not drain within") && strings.Contains(text, "stuck")
	})
	// The gate held: no exec, the marker is gone, and the stuck child was
	// never killed or respawned — it is still running its original spawn.
	if got := rec.len(); got != 0 {
		t.Fatalf("exec invoked %d time(s), want 0 — the replacement must not happen over an undrained fleet", got)
	}
	if _, err := os.Lstat(filepath.Join(cacheDir, replaceMarkerName)); !os.IsNotExist(err) {
		t.Errorf("the REPLACE marker was not consumed; stat err = %v", err)
	}
	if got := countRuns(t, state); got != 1 {
		t.Errorf("spawns = %d, want 1 — the stuck child must not be respawned by the halted replacement", got)
	}
	// A STOP did land (the drain's write) — the child just never honours it.
	if _, err := os.Lstat(filepath.Join(state, "STOP")); err != nil {
		t.Errorf("no STOP was written to the stuck child: %v", err)
	}

	// The fleet stop after a halted replacement still has to wait for the
	// wedged child; kill it so the test leaves no orphan, exactly as the
	// stuck-stop test does.
	cancel()
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
}

// The replacement is scoped to local placement (Decision 7): the drain acts
// on the supervisor's own instances, which reconcile builds from local
// entries only — a remote entry never becomes an instance, so it is never
// touched by the replacement, and the local child drains and the exec fires
// exactly as if the remote entry did not exist.
func TestReplaceTouchesLocalChildrenOnly(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "sleep")
	plane := &fakePlane{}
	plane.set([]RosterEntry{
		runEntry("local", ""),
		{Name: "remote", DesiredState: RosterDesiredRunning, Placement: RosterPlacementRemote,
			Projects: []RosterProject{{Slug: "acme"}}},
	})
	srv := plane.serve(t)

	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := testOptions(t, cacheDir, srv)
	o.LaunchVersion = func() (string, error) { return version.Current, nil }
	o.VerifyTimeout = 5 * time.Second
	rec := &execRecorder{}
	o.ExecInPlace = rec.hook()
	done := runSupervise(ctx, o)

	localState := entryStateDir(cacheDir, "local")
	remoteState := entryStateDir(cacheDir, "remote")
	waitFor(t, 5*time.Second, "the local child to come up", func() bool {
		return countRuns(t, localState) == 1
	})
	waitFor(t, 5*time.Second, "the remote entry to be ignored", func() bool {
		return strings.Contains(buf.String(), `ignoring entry "remote"`)
	})

	writeMarker(t, cacheDir, version.Current)
	waitFor(t, 5*time.Second, "the exec to be invoked after the local drain", func() bool {
		return rec.len() == 1
	})
	// The local child drained (STOP written, then respawned by the failed
	// exec's resume); the remote entry has no state dir at all — it was never
	// an instance, so the replacement never touched it.
	if _, err := os.Lstat(remoteState); !os.IsNotExist(err) {
		t.Errorf("a state dir appeared for the remote entry; stat err = %v", err)
	}
	waitFor(t, 5*time.Second, "the local child to be respawned after the failed exec", func() bool {
		return countRuns(t, localState) == 2
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

// A marker a supervisor starts later finds STALE and discards: a REPLACE
// written before this process began is not a request this process owes, so
// the fleet is not drained and nothing is exec'd — a leftover marker must not
// make a fresh supervisor replace itself.
func TestReplaceDiscardsAMarkerOlderThanTheSupervisor(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "sleep")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("daemon-one", "")})
	srv := plane.serve(t)

	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	// The marker is written BEFORE the supervisor starts — with an explicitly
	// PAST timestamp, because the staleness gate's one-second slack (the same
	// RFC3339-truncation slack the roll's beacon wait uses) must not read a
	// marker written in the supervisor's own start second as fresh: it can
	// only be a leftover, and the stale gate must discard it.
	old := []byte(fmt.Sprintf("supervisor replace requested at %s by %s\n",
		time.Now().Add(-2*time.Second).UTC().Format(time.RFC3339), version.Current))
	if err := os.WriteFile(filepath.Join(cacheDir, replaceMarkerName), old, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := testOptions(t, cacheDir, srv)
	o.LaunchVersion = func() (string, error) { return version.Current, nil }
	o.VerifyTimeout = 5 * time.Second
	rec := &execRecorder{}
	o.ExecInPlace = rec.hook()
	done := runSupervise(ctx, o)

	state := entryStateDir(cacheDir, "daemon-one")
	waitFor(t, 5*time.Second, "the stale marker to be discarded and the child to come up", func() bool {
		return strings.Contains(buf.String(), "discarding a REPLACE marker") && countRuns(t, state) == 1
	})
	time.Sleep(300 * time.Millisecond) // several polls past the discard
	if got := rec.len(); got != 0 {
		t.Errorf("exec invoked %d time(s), want 0 - a stale marker must not trigger a replacement", got)
	}
	if _, err := os.Lstat(filepath.Join(state, "STOP")); err == nil {
		t.Error("a STOP was written to a child under a stale marker - the fleet must not be drained")
	}
	if got := countRuns(t, state); got != 1 {
		t.Errorf("spawns = %d, want 1 - the fleet must keep running untouched", got)
	}
	if _, err := os.Lstat(filepath.Join(cacheDir, replaceMarkerName)); !os.IsNotExist(err) {
		t.Errorf("the stale marker was not removed; stat err = %v", err)
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

// The honour-time re-probe is a gate of its own: a marker requesting one
// version is refused when the launch binary reports another — the operator's
// install must have landed, and a marker for a swap that did not happen must
// not drain a healthy fleet. The refusal consumes the marker, so the fixed
// install makes re-running the command the whole recovery.
func TestReplaceRefusesWhenTheLaunchBinaryDoesNotMatchTheMarker(t *testing.T) {
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
	o := testOptions(t, cacheDir, srv)
	o.LaunchVersion = func() (string, error) { return version.Current, nil }
	o.VerifyTimeout = 5 * time.Second
	rec := &execRecorder{}
	o.ExecInPlace = rec.hook()
	done := runSupervise(ctx, o)

	state := entryStateDir(cacheDir, "daemon-one")
	waitFor(t, 5*time.Second, "the child to come up", func() bool { return countRuns(t, state) == 1 })

	// A marker requesting a build the launch path does not carry: written
	// after the supervisor started, so the staleness gate does not apply and
	// the version gate is what must refuse.
	writeMarker(t, cacheDir, "1.0.0-stale")
	waitFor(t, 5*time.Second, "the version refusal to be logged", func() bool {
		text := buf.String()
		return strings.Contains(text, "refusing") && strings.Contains(text, "1.0.0-stale")
	})
	time.Sleep(300 * time.Millisecond)
	if got := rec.len(); got != 0 {
		t.Errorf("exec invoked %d time(s), want 0 - a mismatched marker must not trigger a replacement", got)
	}
	if _, err := os.Lstat(filepath.Join(state, "STOP")); err == nil {
		t.Error("a STOP was written to a child under a refused marker - the fleet must not be drained")
	}
	if got := countRuns(t, state); got != 1 {
		t.Errorf("spawns = %d, want 1 - the fleet must keep running untouched", got)
	}
	if _, err := os.Lstat(filepath.Join(cacheDir, replaceMarkerName)); !os.IsNotExist(err) {
		t.Errorf("the refused marker was not consumed; stat err = %v", err)
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

// The command's pre-flight refuses a replacement whose launch path still
// carries another version: the operator forgot to install the new build (or
// is running the command from a copy), and a marker for that swap would drain
// a healthy fleet for nothing.
func TestReplaceCommandRefusesWhenTheLaunchPathCarriesAnotherVersion(t *testing.T) {
	cacheDir := t.TempDir()
	o := Options{
		Binary:         os.Args[0],
		RosterCacheDir: cacheDir,
		LaunchVersion: func() (string, error) {
			return "0.0.0-stale", nil
		},
	}
	err := Replace(context.Background(), o)
	if err == nil {
		t.Fatal("Replace returned nil, want a refusal naming the launch binary version")
	}
	for _, want := range []string{"launch binary", "0.0.0-stale", "install the new build"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not contain %q", err, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(cacheDir, replaceMarkerName)); !os.IsNotExist(err) {
		t.Errorf("a refused replacement wrote a marker; stat err = %v", err)
	}
}

// A replacement request on a machine with no supervisor state is refused: no
// supervisor has run here, so there is nothing to replace — a marker would
// sit until some future supervisor discarded it as stale.
func TestReplaceCommandRefusesWithoutSupervisorState(t *testing.T) {
	o := Options{
		Binary:         os.Args[0],
		RosterCacheDir: filepath.Join(t.TempDir(), "missing"),
		LaunchVersion:  func() (string, error) { return version.Current, nil },
	}
	err := Replace(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "no supervisor has run") {
		t.Fatalf("Replace without supervisor state returned %v, want a refusal naming the missing state", err)
	}
}

// The command's happy path: the marker is written into the supervisor's cache
// dir carrying the requesting version, and re-running the command refreshes
// it (a fresh timestamp is what keeps the marker honourable to a supervisor
// that had already discarded an older one).
func TestReplaceCommandWritesTheMarker(t *testing.T) {
	cacheDir := t.TempDir()
	o := Options{
		Binary:         os.Args[0],
		RosterCacheDir: cacheDir,
		LaunchVersion:  func() (string, error) { return version.Current, nil },
	}
	if err := Replace(context.Background(), o); err != nil {
		t.Fatalf("Replace returned %v, want nil", err)
	}
	m := readReplaceMarker(cacheDir)
	if m == nil || m.version != version.Current {
		t.Fatalf("marker = %+v, want version %s", m, version.Current)
	}
	if err := Replace(context.Background(), o); err != nil {
		t.Fatalf("re-running Replace returned %v, want nil (an existing marker is refreshed)", err)
	}
	m = readReplaceMarker(cacheDir)
	if m == nil || m.version != version.Current {
		t.Fatalf("marker after refresh = %+v, want version %s", m, version.Current)
	}
}

func TestParseReplaceMarker(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		wantV   string
		wantErr bool
	}{
		{"valid", "supervisor replace requested at 2026-08-31T04:00:00Z by 1.2.3\n", "1.2.3", false},
		{"valid no trailing newline", "supervisor replace requested at 2026-08-31T04:00:00Z by 0.13.3-8-gabc123", "0.13.3-8-gabc123", false},
		{"bad prefix", "roll requested at 2026-08-31T04:00:00Z by 1.2.3\n", "", true},
		{"missing version", "supervisor replace requested at 2026-08-31T04:00:00Z by \n", "", true},
		{"missing separator", "supervisor replace requested at 2026-08-31T04:00:00Z 1.2.3\n", "", true},
		{"bad timestamp", "supervisor replace requested at yesterday by 1.2.3\n", "", true},
		{"empty", "", "", true},
	} {
		v, at, err := parseReplaceMarker([]byte(tc.in))
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: parseReplaceMarker(%q) = %q, want an error", tc.name, tc.in, v)
			}
			continue
		}
		if err != nil || v != tc.wantV || at.IsZero() {
			t.Errorf("%s: parseReplaceMarker(%q) = %q, %v, %v; want %q", tc.name, tc.in, v, at, err, tc.wantV)
		}
	}
}

// writeMarker is the tests' marker write, failing the test on error.
func writeMarker(t *testing.T, dir, v string) {
	t.Helper()
	if err := writeReplaceMarker(dir, v); err != nil {
		t.Fatalf("writeReplaceMarker(%s, %s): %v", dir, v, err)
	}
}
