package supervisor

// Phase 5c (daemon-supervisor) after CLA-878: the supervisor replaces itself in
// place WITHOUT draining the fleet. The outgoing image writes a handoff naming
// its live children and execs at once; the incoming image adopts each live pid,
// reaps it itself, and rolls older-version children onto the new binary
// through the RESTART roll at their own boundaries.
//
// The tests drive the outgoing half directly on a Supervisor whose instances
// hold real child processes started by the test (no exec.Cmd.Wait goroutine:
// the adopted reaper must be the only waiter, exactly as after exec(2)), then
// run a second Supervisor over the same cache dir. Because the test process is
// the supervisor's stand-in, os.Getpid() is unchanged across the "exec", so the
// handoff pid gate accepts it — and a handoff written for another pid is
// exercised explicitly.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/statedir"
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

// hook records the call and returns an error — the shape of a failed exec, in
// which the old image keeps running untouched.
func (r *execRecorder) hook() func(bin string, argv []string, env []string) error {
	return r.resultHook(errors.New("exec refused by the test"))
}

// succeedHook records the call and returns nil — the seam that stands in for a
// successful exec(2): in production this call never returns.
func (r *execRecorder) succeedHook() func(bin string, argv []string, env []string) error {
	return r.resultHook(nil)
}

func (r *execRecorder) resultHook(result error) func(bin string, argv []string, env []string) error {
	return func(bin string, argv []string, env []string) error {
		r.mu.Lock()
		r.calls = append(r.calls, execCall{bin: bin, argv: argv, env: env})
		r.mu.Unlock()
		return result
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

// stagedChild is one child the outgoing supervisor holds: a real process with
// the materialized config and beacon a supervisor-spawned child would have,
// started by the test and deliberately not waited on.
type stagedChild struct {
	name     string
	pid      int
	stateDir string
	inst     *Instance
	cmd      *exec.Cmd
}

// newOutgoingSupervisor builds the supervisor stand-in the replacement runs
// on: the same options, cache dir and hostname a running Supervise would use,
// with no loop. checkReplace needs exactly these.
func newOutgoingSupervisor(t *testing.T, o Options, cacheDir string) *Supervisor {
	t.Helper()
	host, _ := os.Hostname()
	return &Supervisor{
		o:         o,
		ctx:       context.Background(),
		hostname:  host,
		cacheDir:  cacheDir,
		startedAt: time.Now(),
		exits:     make(chan exitEvent, 16),
	}
}

// stageReplaceableChild starts one fake daemon exactly as spawn would (same
// materialized config, same version env) but leaves it un-waited, so the
// adopted reaper is the process's only Wait after the "exec". mode is the
// per-instance SCRIPT_MODE the fake daemon reads: "sleep" is idle, "busy" is a
// child mid-session that ignores RESTART until its BOUNDARY file appears.
func stageReplaceableChild(t *testing.T, d *Supervisor, name, mode, v string) *stagedChild {
	t.Helper()
	e := runEntry(name, "")
	cfg, sd, err := d.buildConfig(&e)
	if err != nil {
		t.Fatalf("%s: buildConfig: %v", name, err)
	}
	inst := &Instance{
		name:         name,
		entry:        &e,
		cfg:          cfg,
		stateDir:     sd,
		desired:      RosterDesiredRunning,
		childVersion: v,
		aliveSince:   time.Now().Add(-time.Minute),
	}
	cfgPath, err := d.materializeConfig(inst)
	if err != nil {
		t.Fatalf("%s: materializeConfig: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(sd, "SCRIPT_MODE"), []byte(mode+"\n"), 0o600); err != nil {
		t.Fatalf("%s: SCRIPT_MODE: %v", name, err)
	}
	cmd := exec.Command(d.o.Binary, "run", "-c", cfgPath)
	cmd.Env = append(os.Environ(), childVersionEnv+"="+v)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("%s: start: %v", name, err)
	}
	inst.cmd = cmd
	d.instances = append(d.instances, inst)
	sc := &stagedChild{name: name, pid: cmd.Process.Pid, stateDir: sd, inst: inst, cmd: cmd}
	// Best effort: a child a test neither adopted nor stopped must not outlive
	// the test. A reaped pid makes this a no-op (or a harmless ESRCH).
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return sc
}

func waitForBeaconVersion(t *testing.T, dir, want string) {
	t.Helper()
	waitFor(t, 5*time.Second, fmt.Sprintf("a beacon reporting %s in %s", want, dir), func() bool {
		b := readLocalBeacon(dir)
		return b != nil && b.Version == want
	})
}

// waitForLog is waitFor with the captured log on failure: adoption failures are
// almost always visible in the supervisor's own lines.
func waitForLog(t *testing.T, timeout time.Duration, what string, buf *lockedBuffer, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s\nlog:\n%s", what, buf.String())
}

// The contract: a replace execs at once with no STOP written to any child. The
// incoming image adopts both children by pid; the idle child is rolled onto the
// new version first, while the busy child keeps its pid and reports running
// until its own boundary, then comes back on the new version.
func TestReplaceAdoptsLiveChildrenInsteadOfDraining(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "scripted")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("daemon-busy", ""), runEntry("daemon-idle", "")})
	srv := plane.serve(t)

	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := testOptions(t, cacheDir, srv)
	o.LaunchVersion = func() (string, error) { return version.Current, nil }
	o.VersionOf = func() string { return version.Current } // respawns are the new image
	o.VerifyTimeout = 5 * time.Second
	rec := &execRecorder{}
	o.ExecInPlace = rec.succeedHook()

	out := newOutgoingSupervisor(t, o, cacheDir)
	busy := stageReplaceableChild(t, out, "daemon-busy", "busy", "1.0.0")
	idle := stageReplaceableChild(t, out, "daemon-idle", "sleep", "1.0.0")
	waitForBeaconVersion(t, busy.stateDir, "1.0.0")
	waitForBeaconVersion(t, idle.stateDir, "1.0.0")

	writeMarker(t, cacheDir, version.Current)
	out.checkReplace()

	if got := rec.len(); got != 1 {
		t.Fatalf("exec invoked %d time(s), want 1", got)
	}
	// No child was signalled or stopped: the swap is adoption, not a drain.
	for _, sc := range []*stagedChild{busy, idle} {
		for _, marker := range []string{"STOP", "RESTART"} {
			if markerPresent(sc.stateDir, marker) {
				t.Errorf("%s: %s was written during the replace - the fleet must not be signalled", sc.name, marker)
			}
		}
		if !processAlive(sc.pid) {
			t.Errorf("%s (pid %d) is no longer alive after the replace", sc.name, sc.pid)
		}
	}
	// The handoff carries every live child and the writer's identity.
	h := readReplaceHandoff(cacheDir)
	if h == nil {
		t.Fatalf("no readable handoff file in %s", cacheDir)
	}
	if h.SupervisorPID != os.Getpid() {
		t.Errorf("handoff supervisor pid = %d, want %d", h.SupervisorPID, os.Getpid())
	}
	if h.Version != version.Current {
		t.Errorf("handoff version = %q, want %q", h.Version, version.Current)
	}
	if len(h.Children) != 2 {
		t.Fatalf("handoff children = %d, want 2: %+v", len(h.Children), h.Children)
	}
	byName := map[string]replaceHandoffChild{}
	for _, c := range h.Children {
		byName[c.Name] = c
	}
	for _, sc := range []*stagedChild{busy, idle} {
		c, ok := byName[sc.name]
		if !ok {
			t.Errorf("handoff does not name %s", sc.name)
			continue
		}
		if c.PID != sc.pid {
			t.Errorf("%s: handoff pid = %d, want %d", sc.name, c.PID, sc.pid)
		}
		if c.Version != "1.0.0" {
			t.Errorf("%s: handoff version = %q, want 1.0.0", sc.name, c.Version)
		}
		if want := filepath.Join(sc.stateDir, statedir.MaterializedConfigName); c.ConfigPath != want {
			t.Errorf("%s: handoff config path = %q, want %q", sc.name, c.ConfigPath, want)
		}
		if c.SpawnedAt.IsZero() {
			t.Errorf("%s: handoff spawn time is zero", sc.name)
		}
	}
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

	// The incoming image: adopt, keep the children running, roll idle first.
	done := runSupervise(ctx, o)
	waitForLog(t, 5*time.Second, "both children to be adopted", &buf, func() bool {
		text := buf.String()
		return strings.Contains(text, "replace: adopted daemon-busy (pid "+fmt.Sprint(busy.pid)) &&
			strings.Contains(text, "replace: adopted daemon-idle (pid "+fmt.Sprint(idle.pid))
	})
	waitFor(t, 5*time.Second, "the idle child to be rolled before the busy one", func() bool {
		return strings.Contains(buf.String(), "replace: rolling adopted daemon-busy")
	})
	if !strings.Contains(buf.String(), "replace: rolling adopted daemon-idle") {
		t.Fatalf("the idle child was never rolled:\n%s", buf.String())
	}
	waitForBeaconVersion(t, idle.stateDir, version.Current)
	// The busy child kept its pid and stayed running on the old version while
	// the idle one came back.
	if !processAlive(busy.pid) {
		t.Fatalf("the busy child (pid %d) was signalled or reaped by the idle roll", busy.pid)
	}
	if b := readLocalBeacon(busy.stateDir); b == nil || b.Version != "1.0.0" {
		t.Fatalf("busy beacon = %+v, want still 1.0.0 before its boundary", b)
	}
	var busyRunning bool
	for _, rep := range plane.statusReports(t) {
		if inst := runningInstance(rep, "daemon-busy"); inst != nil &&
			inst.State == StatusRunning && inst.ChildVersion == "1.0.0" {
			busyRunning = true
		}
	}
	if !busyRunning {
		t.Error("no status report showed the busy child running with its adopted pid/version")
	}
	if !strings.Contains(buf.String(), fmt.Sprintf("daemon-busy: running (pid %d, version 1.0.0)", busy.pid)) {
		t.Errorf("the fleet listing did not show the busy child running with its pid:\n%s", buf.String())
	}
	// Ordering is pinned: the idle child's next beacon was seen before the
	// busy child was so much as asked to restart.
	text := buf.String()
	iIdle := strings.Index(text, "replace: rolling adopted daemon-idle")
	iIdleDone := strings.Index(text, "roll: daemon-idle: the next beacon reports")
	iBusy := strings.Index(text, "replace: rolling adopted daemon-busy")
	if iIdle < 0 || iIdleDone < 0 || iBusy < 0 || !(iIdle < iIdleDone && iIdleDone < iBusy) {
		t.Errorf("the adopted roll did not move the idle child first:\n%s", text)
	}

	// The busy child reaches its boundary: it now honours the RESTART and
	// comes back on the new version.
	if err := os.WriteFile(filepath.Join(busy.stateDir, "BOUNDARY"), []byte("boundary\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForBeaconVersion(t, busy.stateDir, version.Current)
	waitFor(t, 5*time.Second, "the busy child to be respawned on the new version", func() bool {
		return countRuns(t, busy.stateDir) == 2
	})
	// The idle child was never stopped or restarted a second time.
	if got := countRuns(t, idle.stateDir); got != 2 {
		t.Errorf("idle spawns = %d, want 2 (initial + roll)", got)
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

// A child that exits between the handoff write and the new image's wait is a
// zombie still owned by this process: the adopted wait4 reaps it at once,
// reports a normal exit, and the reconcile respawns the instance.
func TestReplaceReapsAChildThatExitsInTheExecWindow(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "scripted")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("daemon-window", "")})
	srv := plane.serve(t)

	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := testOptions(t, cacheDir, srv)
	o.LaunchVersion = func() (string, error) { return version.Current, nil }
	o.VersionOf = func() string { return version.Current }
	o.VerifyTimeout = 5 * time.Second
	o.ExecInPlace = (&execRecorder{}).succeedHook()

	out := newOutgoingSupervisor(t, o, cacheDir)
	child := stageReplaceableChild(t, out, "daemon-window", "sleep", "1.0.0")
	waitForBeaconVersion(t, child.stateDir, "1.0.0")

	// The handoff is written while the child is still live, then the child dies
	// before the incoming image starts: exactly the exec-window race.
	n, err := out.writeReplaceHandoff()
	if err != nil || n != 1 {
		t.Fatalf("writeReplaceHandoff = %d, %v; want 1, nil", n, err)
	}
	if err := syscall.Kill(child.pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // let the signal land; nobody reaps it
	if !processAlive(child.pid) {
		// A zombie still counts as alive; if it does not, the test cannot
		// exercise the zombie path and should say so rather than pass.
		t.Fatalf("child pid %d is gone before adoption - the zombie window was missed", child.pid)
	}

	done := runSupervise(ctx, o)
	waitFor(t, 5*time.Second, "the dead-in-window child to be adopted and reaped", func() bool {
		text := buf.String()
		return strings.Contains(text, "replace: adopted daemon-window") &&
			strings.Contains(text, "daemon-window: exited unexpectedly") &&
			strings.Contains(text, "signal")
	})
	waitFor(t, 5*time.Second, "the reaped instance to be respawned on the new version", func() bool {
		return countRuns(t, child.stateDir) == 2 && readLocalBeacon(child.stateDir) != nil &&
			readLocalBeacon(child.stateDir).Version == version.Current
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

// A handoff pid that is not our child (reused, or already reaped) comes back
// ECHILD: the instance reads as exited and the next reconcile respawns it.
func TestReplaceTreatsANonChildPIDAsExited(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "scripted")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("daemon-ghost", "")})
	srv := plane.serve(t)

	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := testOptions(t, cacheDir, srv)
	o.VersionOf = func() string { return version.Current }
	o.VerifyTimeout = 5 * time.Second
	state := entryStateDir(cacheDir, "daemon-ghost")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	// Our own pid is alive but is not our child: wait4 answers ECHILD.
	if err := writeReplaceHandoffFile(cacheDir, replaceHandoff{
		SupervisorPID: os.Getpid(),
		Version:       "1.0.0",
		WrittenAt:     time.Now().UTC(),
		Children: []replaceHandoffChild{{
			Name:       "daemon-ghost",
			PID:        os.Getpid(),
			ConfigPath: filepath.Join(state, statedir.MaterializedConfigName),
			Version:    "1.0.0",
			SpawnedAt:  time.Now().Add(-time.Minute),
		}},
	}); err != nil {
		t.Fatal(err)
	}

	done := runSupervise(ctx, o)
	waitFor(t, 5*time.Second, "the non-child pid to read as an exit", func() bool {
		text := buf.String()
		return strings.Contains(text, "replace: adopted daemon-ghost") &&
			strings.Contains(text, "is not this supervisor's child")
	})
	waitFor(t, 5*time.Second, "the instance to be respawned", func() bool {
		return countRuns(t, state) == 1
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

// A handoff written by an earlier process is stale: the pid gate discards it,
// no child is adopted, and the roster spawns the fleet afresh.
func TestReplaceDiscardsAHandoffFromAnotherSupervisor(t *testing.T) {
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
	o.VersionOf = func() string { return version.Current }
	state := entryStateDir(cacheDir, "daemon-one")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	stalePID := os.Getpid() + 1
	if err := writeReplaceHandoffFile(cacheDir, replaceHandoff{
		SupervisorPID: stalePID,
		Version:       "1.0.0",
		WrittenAt:     time.Now().UTC(),
		Children: []replaceHandoffChild{{
			Name:       "daemon-one",
			PID:        os.Getpid(),
			ConfigPath: filepath.Join(state, statedir.MaterializedConfigName),
			Version:    "1.0.0",
			SpawnedAt:  time.Now().Add(-time.Minute),
		}},
	}); err != nil {
		t.Fatal(err)
	}

	done := runSupervise(ctx, o)
	waitFor(t, 5*time.Second, "the stale handoff to be discarded", func() bool {
		return strings.Contains(buf.String(), fmt.Sprintf("discarding a handoff file written by supervisor pid %d", stalePID))
	})
	if _, err := os.Lstat(filepath.Join(cacheDir, replaceHandoffName)); !os.IsNotExist(err) {
		t.Errorf("the stale handoff was not consumed; stat err = %v", err)
	}
	waitFor(t, 5*time.Second, "the roster to spawn the child afresh", func() bool {
		return countRuns(t, state) == 1
	})
	if strings.Contains(buf.String(), "replace: adopted daemon-one") {
		t.Errorf("a stale handoff was adopted:\n%s", buf.String())
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

// An exec that fails leaves the old image and its children untouched: no child
// is signalled, the handoff is removed so no later process can adopt it, and
// the fleet keeps serving.
func TestReplaceExecFailureKeepsTheOldImageAndItsChildren(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "scripted")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("daemon-one", "")})
	srv := plane.serve(t)

	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	o := testOptions(t, cacheDir, srv)
	o.LaunchVersion = func() (string, error) { return version.Current, nil }
	o.VerifyTimeout = 5 * time.Second
	rec := &execRecorder{}
	o.ExecInPlace = rec.hook() // returns an error: the exec fails

	out := newOutgoingSupervisor(t, o, cacheDir)
	child := stageReplaceableChild(t, out, "daemon-one", "sleep", "1.0.0")
	exited := &Instance{name: "daemon-backoff", desired: RosterDesiredRunning, exited: true, childVersion: "1.0.0"}
	out.instances = append(out.instances, exited)
	waitForBeaconVersion(t, child.stateDir, "1.0.0")

	writeMarker(t, cacheDir, version.Current)
	out.checkReplace()

	if got := rec.len(); got != 1 {
		t.Fatalf("exec invoked %d time(s), want 1", got)
	}
	if !processAlive(child.pid) {
		t.Errorf("the child (pid %d) was touched by a failed exec", child.pid)
	}
	for _, marker := range []string{"STOP", "RESTART"} {
		if markerPresent(child.stateDir, marker) {
			t.Errorf("%s was written despite the failed exec", marker)
		}
	}
	if _, err := os.Lstat(filepath.Join(cacheDir, replaceHandoffName)); !os.IsNotExist(err) {
		t.Errorf("the handoff survived a failed exec; stat err = %v", err)
	}
	if got := out.statusInstance(out.instances[0]).State; got != StatusRunning {
		t.Errorf("the old image reports the child %q, want running", got)
	}
	// Only the live child is in the handoff; a child in backoff is not.
	h := readReplaceHandoff(cacheDir)
	if h != nil {
		t.Errorf("a handoff is still readable after the failed exec: %+v", h)
	}
	text := buf.String()
	for _, want := range []string{
		"replace: handoff written for 1 live child(ren)",
		"replace: the replacement exec failed",
		"the supervisor keeps running with its children untouched",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("log does not contain %q:\n%s", want, text)
		}
	}
}

// The replacement is scoped to local placement (Decision 7): only the
// supervisor's own instances are handed to the new image; a remote entry never
// becomes an instance and its state dir is never created.
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
	o.ExecInPlace = rec.hook() // failure: the old image resumes and respawns nothing
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
	waitFor(t, 5*time.Second, "the exec to be invoked", func() bool { return rec.len() == 1 })
	h := readReplaceHandoff(cacheDir)
	// The handoff has already been removed by the failed exec; the log line
	// names the count, and the remote state dir is the durable evidence.
	if !strings.Contains(buf.String(), "replace: handoff written for 1 live child(ren)") {
		t.Errorf("handoff did not carry exactly the local child:\n%s", buf.String())
	}
	if h != nil {
		t.Errorf("handoff survived the failed exec: %+v", h)
	}
	if _, err := os.Lstat(remoteState); !os.IsNotExist(err) {
		t.Errorf("a state dir appeared for the remote entry; stat err = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(localState, "STOP")); err == nil {
		t.Error("a STOP was written to the local child - a replace must not drain")
	}
	if got := countRuns(t, localState); got != 1 {
		t.Errorf("local spawns = %d, want 1 - a failed exec must not respawn the child", got)
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

// A marker a supervisor starts later finds STALE and discards: a REPLACE
// written before this process began is not a request this process owes, so no
// child is handed off and nothing is exec'd — a leftover marker must not make a
// fresh supervisor replace itself.
func TestReplaceDiscardsAMarkerOlderThanTheSupervisor(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "sleep")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("daemon-one", "")})

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

	srv := plane.serve(t)
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
	if markerPresent(state, "STOP") {
		t.Error("a STOP was written to a child under a stale marker - the fleet must not be signalled")
	}
	if _, err := os.Lstat(filepath.Join(cacheDir, replaceHandoffName)); !os.IsNotExist(err) {
		t.Errorf("a stale marker wrote a handoff file; stat err = %v", err)
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
// not exec over a healthy fleet. The refusal consumes the marker, so the fixed
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
	if markerPresent(state, "STOP") {
		t.Error("a STOP was written to a child under a refused marker - the fleet must not be signalled")
	}
	if _, err := os.Lstat(filepath.Join(cacheDir, replaceHandoffName)); !os.IsNotExist(err) {
		t.Errorf("a refused marker wrote a handoff file; stat err = %v", err)
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
// is running the command from a copy), and a marker for that swap would exec
// over a healthy fleet for nothing.
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

// The handoff reader refuses a symlink and an unreadable body: a handoff is a
// control file like every other in the state surface.
func TestReadReplaceHandoffRefusesUntrustedFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, []byte(`{"supervisorPid":1,"children":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, replaceHandoffName)); err != nil {
		t.Fatal(err)
	}
	if h := readReplaceHandoff(dir); h != nil {
		t.Fatalf("readReplaceHandoff followed a symlink: %+v", h)
	}
	_ = os.Remove(filepath.Join(dir, replaceHandoffName))
	if err := os.WriteFile(filepath.Join(dir, replaceHandoffName), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if h := readReplaceHandoff(dir); h != nil {
		t.Fatalf("readReplaceHandoff accepted an unreadable body: %+v", h)
	}
	if err := os.WriteFile(filepath.Join(dir, replaceHandoffName), []byte(`{"supervisorPid":1,"children":[{"name":"x","pid":0}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if h := readReplaceHandoff(dir); h != nil {
		t.Fatalf("readReplaceHandoff accepted a pid-less child: %+v", h)
	}
}

// writeMarker is the tests' marker write, failing the test on error.
func writeMarker(t *testing.T, dir, v string) {
	t.Helper()
	if err := writeReplaceMarker(dir, v); err != nil {
		t.Fatalf("writeReplaceMarker(%s, %s): %v", dir, v, err)
	}
}
