//go:build unix

package harness

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// priorityOf reads a live process's scheduling priority from the KERNEL. argv is
// what the code claims it did; this is what the machine did.
//
// Only differences between two processes are asserted, never an absolute: the
// kernel reports 20-nice, and the process running the test may itself be niced
// by whatever started it.
func priorityOf(t *testing.T, pid int) int {
	t.Helper()
	prio, err := syscall.Getpriority(syscall.PRIO_PROCESS, pid)
	if err != nil {
		t.Fatalf("getpriority(%d): %v", pid, err)
	}
	return prio
}

// sessionTree starts a fake harness that forks a long-lived grandchild (the
// shape of a test runner or a build the session starts), and returns the
// kernel's priority for the child and the grandchild, read while both are still
// alive. The two pids come back over the child's stdout, so the test never
// guesses at a pid. Priorities are read before this function's cleanup kills
// the tree.
func sessionTree(t *testing.T, niceValue int) (child, grandchild int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd, err := sessionCommand(ctx, niceValue, "sh", "-c", "echo $$; sleep 30 & echo $!; wait")
	if err != nil {
		t.Fatalf("sessionCommand(%d): %v", niceValue, err)
	}
	setupProcessGroup(cmd)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		killProcessGroup(cmd)
		_ = cmd.Wait()
	}()

	sc := bufio.NewScanner(out)
	var pids []int
	for len(pids) < 2 && sc.Scan() {
		pid, err := strconv.Atoi(strings.TrimSpace(sc.Text()))
		if err != nil {
			t.Fatalf("the stub printed %q, want a pid", sc.Text())
		}
		pids = append(pids, pid)
	}
	if len(pids) < 2 {
		t.Fatalf("the stub printed %d pid(s), want 2 (child and grandchild)", len(pids))
	}
	return priorityOf(t, pids[0]), priorityOf(t, pids[1])
}

// The dial's whole claim: the priority lands on the process the adapter starts
// AND is inherited by what that process forks, so a session's test runners and
// builds run low without any toolchain opting in.
func TestSessionCommandLowersTheTreePriority(t *testing.T) {
	self := priorityOf(t, 0)
	plainChild, plainGrandchild := sessionTree(t, 0)
	if plainChild != self {
		t.Errorf("sessionCommand(0) started the child at %d, want the daemon's own %d — the default must be unchanged", plainChild, self)
	}
	if plainGrandchild != self {
		t.Errorf("a session with no session_nice forked a grandchild at %d, want the daemon's own %d", plainGrandchild, self)
	}

	// A niced process reports a SMALLER value (getpriority is 20-nice), so the
	// drop is self minus the session's figure.
	const lowered = 10
	child, grandchild := sessionTree(t, lowered)
	if got := self - child; got != lowered {
		t.Errorf("the session process runs %d below the daemon, want %d", got, lowered)
	}
	if got := self - grandchild; got != lowered {
		t.Errorf("the session's forked grandchild runs %d below the daemon, want %d — the priority is not inherited", got, lowered)
	}
}

// The default must be the OLD spawn, not a wrapper that happens to be a no-op:
// no extra process between the daemon and the harness, so pid, argv and error
// classification are exactly what they were before the dial existed.
func TestSessionCommandZeroIsThePlainSpawn(t *testing.T) {
	cmd, err := sessionCommand(context.Background(), 0, "sh", "-c", "true")
	if err != nil {
		t.Fatalf("sessionCommand(0): %v", err)
	}
	if base := filepath.Base(cmd.Path); base != "sh" {
		t.Errorf("sessionCommand(0) resolves to %q, want sh itself", cmd.Path)
	}
	if len(cmd.Args) != 3 || cmd.Args[0] != "sh" || cmd.Args[2] != "true" {
		t.Errorf("sessionCommand(0) argv = %q, want the plain command", cmd.Args)
	}
}

// A positive value REQUIRES nice(1); without it the session must fail to start
// with a message naming the setting, not die looking like a broken harness.
func TestSessionCommandRefusesNiceWhenItIsMissing(t *testing.T) {
	// A PATH holding the harness but no nice(1): the refusal must come from our
	// own lookup, and name session_nice.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writing stub: %v", err)
	}
	t.Setenv("PATH", dir)
	_, err := sessionCommand(context.Background(), 5, "sh", "-c", "true")
	if err == nil {
		t.Fatal("sessionCommand with a positive session_nice and no nice(1) returned no error")
	}
	if !strings.Contains(err.Error(), "session_nice 5") || !strings.Contains(err.Error(), "nice(1)") {
		t.Errorf("error = %q, want it to name session_nice and nice(1)", err)
	}
}

// The harness is resolved with the DAEMON's PATH, so a session env allowlist
// that drops PATH from the child's environment cannot break the spawn. Before
// the wrapper existed the binary was resolved by the daemon for the same reason;
// passing a bare name to nice(1) would move that resolution into the child.
func TestSessionCommandResolvesTheHarnessWithTheDaemonsPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fakeharness"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writing stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cmd, err := sessionCommand(context.Background(), 3, "fakeharness")
	if err != nil {
		t.Fatalf("sessionCommand: %v", err)
	}
	// The child sees a PATH that contains nothing at all.
	cmd.Env = []string{"PATH=" + t.TempDir()}
	if err := cmd.Run(); err != nil {
		t.Fatalf("the wrapped harness could not start with the child's PATH empty: %v", err)
	}
}

// The ADAPTER must hand the invocation's value to the spawn. A ratchet can force
// every spawn through sessionCommand, but not that the value passed is in.Nice;
// this runs a real claude-shaped stub and reads the priority back from the
// grandchild it forked, which is the process a session's tests would run in.
func TestClaudeInvokeAppliesTheSessionPriority(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	// The grandchild's streams go to /dev/null so it cannot hold the adapter's
	// pipe open past its parent's exit; the parent then exits cleanly.
	script := "#!/bin/sh\nsleep 30 >/dev/null 2>&1 &\necho $! > " + pidFile + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	const lowered = 7
	if _, err := (claude{}).Invoke(context.Background(), Invocation{Prompt: "work", Nice: lowered, Console: io.Discard}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	var pid int
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			if pid, err = strconv.Atoi(strings.TrimSpace(string(data))); err != nil || pid <= 0 {
				t.Fatalf("the stub wrote %q, want a pid", data)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stub never wrote %s: %v", pidFile, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	if got := priorityOf(t, 0) - priorityOf(t, pid); got != lowered {
		t.Errorf("the session's forked grandchild runs %d below the daemon, want %d — Invocation.Nice did not reach the spawn", got, lowered)
	}
}
