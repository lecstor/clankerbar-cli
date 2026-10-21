package supervisor

// writeStop's outcomes, and what the operator is told about each. A STOP that
// is ALREADY in the child's state dir is a stop already pending — the marker
// the operator wrote by hand drains the child at its iteration boundary
// exactly as a fresh one would — so reporting it as "the child keeps running;
// stop it by hand" sends someone to fix a child that is about to stop on its
// own (CLA-765). Occupying the NAME is not enough: a symlink or a directory at
// STOP also makes WriteFile report ErrExists, and the daemon refuses to read
// either, so those keep the loud line (CLA-765 review findings 1.review-1.1,
// 1.review-2.1, 1.review-3.1). Any OTHER failure to write the marker is still
// a real one and keeps the loud line too.

import (
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

// writeStopFixture returns a real state dir (opened the way the daemon opens
// it, so it carries the sentinel and .gitignore the next Open adopts) under a
// workdir that is its session root.
func writeStopFixture(t *testing.T) (root, dir string) {
	t.Helper()
	root = t.TempDir()
	dir = filepath.Join(root, "state")
	st, err := statedir.Open(dir, root)
	if err != nil {
		t.Fatalf("prepare state dir: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close state dir: %v", err)
	}
	return root, dir
}

func writeStopInstance(root string) *Instance {
	return &Instance{name: "clanker2", cfg: &config.Config{WorkDir: root}}
}

// captureLog redirects the standard logger into buf for the duration of one
// test.
func captureLog(t *testing.T, buf *lockedBuffer) {
	t.Helper()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
}

// A marker already present is a pending stop, logged as one, and it is left
// byte-for-byte alone: its body is the record of who asked first.
func TestWriteStopTreatsAnExistingMarkerAsPending(t *testing.T) {
	root, dir := writeStopFixture(t)
	const handWritten = "operator: wrote this by hand to apply a config change\n"
	stopPath := filepath.Join(dir, "STOP")
	if err := os.WriteFile(stopPath, []byte(handWritten), 0o600); err != nil {
		t.Fatalf("write the hand-made STOP: %v", err)
	}

	var buf lockedBuffer
	captureLog(t, &buf)

	if got := (&Supervisor{}).writeStop(writeStopInstance(root), dir); got != stopPending {
		t.Errorf("writeStop = %v, want stopPending", got)
	}

	got := buf.String()
	want := "STOP already present in " + dir + ` (requested by "operator: wrote this by hand to apply a config change")`
	if !strings.Contains(got, want) {
		t.Errorf("log = %q, want it to contain %q", got, want)
	}
	if strings.Contains(got, "stop it by hand") {
		t.Errorf("log = %q, must not tell the operator to stop the child by hand when a STOP is already pending", got)
	}
	if strings.Contains(got, "draining at its iteration boundary") {
		t.Errorf("log = %q, must not assert a marker of unknown provenance will drain the child", got)
	}
	body, err := os.ReadFile(stopPath)
	if err != nil {
		t.Fatalf("read the STOP back: %v", err)
	}
	if string(body) != handWritten {
		t.Errorf("STOP body = %q, want the existing marker untouched (%q)", body, handWritten)
	}
}

// A symlink planted at STOP is not a marker, whatever it points at: the daemon
// refuses to read a control marker through one, so treating the ErrExists as
// pending would promise a drain that never comes. The loud failure line is the
// operator's only signal, and the link (and whatever it points at) is left
// intact.
func TestWriteStopRefusesASymlinkAtStop(t *testing.T) {
	root, dir := writeStopFixture(t)
	target := filepath.Join(root, "elsewhere")
	const targetBody = "not a marker\n"
	if err := os.WriteFile(target, []byte(targetBody), 0o600); err != nil {
		t.Fatalf("write the symlink target: %v", err)
	}
	stopPath := filepath.Join(dir, "STOP")
	if err := os.Symlink(target, stopPath); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	var buf lockedBuffer
	captureLog(t, &buf)

	if got := (&Supervisor{}).writeStop(writeStopInstance(root), dir); got != stopFailed {
		t.Errorf("writeStop = %v, want stopFailed", got)
	}

	got := buf.String()
	if !strings.Contains(got, "cannot write STOP into "+dir+":") {
		t.Errorf("log = %q, want the failed-write line naming %s", got, dir)
	}
	if !strings.Contains(got, "stop it by hand") {
		t.Errorf("log = %q, want the failure to keep telling the operator to step in", got)
	}
	if strings.Contains(got, "already present") {
		t.Errorf("log = %q, a symlink at STOP is not an already-pending stop", got)
	}
	// The refused write must not have replaced the link, nor written through it.
	fi, lerr := os.Lstat(stopPath)
	if lerr != nil {
		t.Errorf("lstat STOP: %v, want the planted symlink", lerr)
	} else if fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("STOP mode = %v, want the planted symlink", fi.Mode())
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != targetBody {
		t.Errorf("symlink target = %q (err = %v), want it untouched (%q)", body, err, targetBody)
	}
}

// A directory at STOP is likewise not a marker: the daemon's openRegular
// refuses anything non-regular, so the supervisor keeps the loud line.
func TestWriteStopRefusesADirectoryAtStop(t *testing.T) {
	root, dir := writeStopFixture(t)
	stopPath := filepath.Join(dir, "STOP")
	if err := os.Mkdir(stopPath, 0o700); err != nil {
		t.Fatalf("plant a directory at STOP: %v", err)
	}

	var buf lockedBuffer
	captureLog(t, &buf)

	if got := (&Supervisor{}).writeStop(writeStopInstance(root), dir); got != stopFailed {
		t.Errorf("writeStop = %v, want stopFailed", got)
	}

	got := buf.String()
	if !strings.Contains(got, "cannot write STOP into "+dir+":") {
		t.Errorf("log = %q, want the failed-write line naming %s", got, dir)
	}
	if !strings.Contains(got, "stop it by hand") {
		t.Errorf("log = %q, want the failure to keep telling the operator to step in", got)
	}
	if strings.Contains(got, "already present") {
		t.Errorf("log = %q, a directory at STOP is not an already-pending stop", got)
	}
	fi, lerr := os.Lstat(stopPath)
	if lerr != nil {
		t.Errorf("lstat STOP: %v, want the planted directory", lerr)
	} else if !fi.IsDir() {
		t.Errorf("STOP mode = %v, want the planted directory", fi.Mode())
	}
}

// A failed write must not be recorded as a requested stop: stopRequested would
// suppress every later attempt, leaving the status card promising a drain that
// is not happening. The next poll retries the write instead
// (CLA-765 review finding 1.review-1.3).
func TestEnsureStoppedRetriesAfterAFailedWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the write refusal this test relies on is a POSIX permission bit")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this test relies on")
	}
	root, dir := writeStopFixture(t)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("make the state dir read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	inst := writeStopInstance(root)
	inst.cmd = &exec.Cmd{}
	inst.aliveSince = time.Now().Add(-time.Minute)
	inst.stateDir = dir

	var buf lockedBuffer
	captureLog(t, &buf)

	d := &Supervisor{}
	d.ensureStopped(inst)
	if inst.stopRequested {
		t.Fatal("stopRequested = true after a failed write, want false so the next poll retries")
	}

	// The obstruction goes away: the next poll must write the marker and only
	// then record the stop as requested.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("make the state dir writable again: %v", err)
	}
	d.ensureStopped(inst)
	if !inst.stopRequested {
		t.Fatal("stopRequested = false after a successful retry, want true")
	}
	if _, err := os.Lstat(filepath.Join(dir, "STOP")); err != nil {
		t.Errorf("STOP after the retry: %v, want the marker written", err)
	}
}

// Any other failure to put the marker down is still a failed stop: the child
// will NOT drain, and the operator has to know.
func TestWriteStopStillLogsAGenuineFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the write refusal this test relies on is a POSIX permission bit")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this test relies on")
	}
	root, dir := writeStopFixture(t)
	// The dir stays a valid state dir (sentinel and .gitignore already there, so
	// Open succeeds) but the owner may no longer create files in it.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("make the state dir read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	var buf lockedBuffer
	captureLog(t, &buf)

	if got := (&Supervisor{}).writeStop(writeStopInstance(root), dir); got != stopFailed {
		t.Errorf("writeStop = %v, want stopFailed", got)
	}

	got := buf.String()
	if !strings.Contains(got, "cannot write STOP into "+dir+":") {
		t.Errorf("log = %q, want the failed-write line naming %s", got, dir)
	}
	if !strings.Contains(got, "stop it by hand") {
		t.Errorf("log = %q, want the failed stop to keep telling the operator to step in", got)
	}
	if strings.Contains(got, "already present") {
		t.Errorf("log = %q, a failed write is not an already-pending stop", got)
	}
	if _, err := os.Lstat(filepath.Join(dir, "STOP")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("STOP exists after a failed write (Lstat err = %v), want none", err)
	}
}
