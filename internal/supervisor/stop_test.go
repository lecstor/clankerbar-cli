package supervisor

// writeStop's two outcomes, and what the operator is told about each. A STOP
// that is ALREADY in the child's state dir is a stop already pending — the
// marker the operator wrote by hand drains the child at its iteration boundary
// exactly as a fresh one would — so reporting it as "the child keeps running;
// stop it by hand" sends someone to fix a child that is about to stop on its
// own (CLA-765). Any OTHER failure to write the marker is still a real one and
// keeps the loud line.

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	(&Supervisor{}).writeStop(writeStopInstance(root), dir)

	got := buf.String()
	want := "STOP already present in " + dir + " - draining at its iteration boundary"
	if !strings.Contains(got, want) {
		t.Errorf("log = %q, want it to contain %q", got, want)
	}
	if strings.Contains(got, "stop it by hand") {
		t.Errorf("log = %q, must not tell the operator to stop the child by hand when a STOP is already pending", got)
	}
	body, err := os.ReadFile(stopPath)
	if err != nil {
		t.Fatalf("read the STOP back: %v", err)
	}
	if string(body) != handWritten {
		t.Errorf("STOP body = %q, want the existing marker untouched (%q)", body, handWritten)
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
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	(&Supervisor{}).writeStop(writeStopInstance(root), dir)

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
