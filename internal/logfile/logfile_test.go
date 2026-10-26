package logfile

import (
	"bytes"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// Files that appear beside the current one, newest first, for the assertions
// that care about what survived rotation.
func siblings(t *testing.T, dir, name string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if e.Name() == name || strings.HasPrefix(e.Name(), name+".") {
			out = append(out, e.Name())
		}
	}
	return out
}

// A daemon that restarts appends to the log it already wrote: Open must not
// truncate, or every re-exec (RESTART/RESTART_NOW) would erase the record the
// log exists to keep.
func TestOpenAppendsAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	f, err := Open(dir, "daemon.log", 0, 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := f.Write([]byte("first run\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	f2, err := Open(dir, "daemon.log", 0, 0)
	if err != nil {
		t.Fatalf("Open again: %v", err)
	}
	if _, err := f2.Write([]byte("second run\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = f2.Close()

	if got := read(t, filepath.Join(dir, "daemon.log")); got != "first run\nsecond run\n" {
		t.Errorf("the second run must append, not truncate; got %q", got)
	}
}

// The bound is the point: a long-lived daemon must not grow one file forever.
func TestRotatesAtTheSizeBound(t *testing.T) {
	dir := t.TempDir()
	f, err := Open(dir, "daemon.log", 10, 3)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	writes := []string{"0123456789", "a", "b", "cdefghijk", "l", "m", "n"}
	for _, w := range writes {
		if _, err := f.Write([]byte(w)); err != nil {
			t.Fatalf("write %q: %v", w, err)
		}
	}
	// "0123456789" then "a" (rotate), "b", "cdefghijk" (rotate at 2+9 > 10),
	// "l" (fills the file to exactly 10), "m" (rotate), "n".
	if got := read(t, filepath.Join(dir, "daemon.log")); got != "mn" {
		t.Errorf("current file = %q, want %q", got, "mn")
	}
	if got := read(t, filepath.Join(dir, "daemon.log.1")); got != "cdefghijkl" {
		t.Errorf("daemon.log.1 = %q, want %q", got, "cdefghijkl")
	}
	if got := read(t, filepath.Join(dir, "daemon.log.2")); got != "ab" {
		t.Errorf("daemon.log.2 = %q, want %q", got, "ab")
	}
}

// keep is a total, not a count of rotated files: the oldest is dropped rather
// than accumulated, or the bound is a fiction.
func TestRotationKeepsAtMostKeepFiles(t *testing.T) {
	dir := t.TempDir()
	f, err := Open(dir, "daemon.log", 4, 2)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	for _, w := range []string{"aaaa", "b", "cccc", "d", "eeee"} {
		if _, err := f.Write([]byte(w)); err != nil {
			t.Fatalf("write %q: %v", w, err)
		}
	}
	got := siblings(t, dir, "daemon.log")
	if len(got) != 2 {
		t.Fatalf("keep=2 must leave 2 files total, got %v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "daemon.log.2")); !os.IsNotExist(err) {
		t.Errorf("keep=2 must not leave daemon.log.2 behind (err=%v)", err)
	}
}

// A symlink planted at the name is removed, never followed: an explicit
// state_dir inside a workdir is supported, so a session can reach one.
func TestSymlinkAtTheNameIsRemovedNotFollowed(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "target.txt")
	if err := os.WriteFile(target, []byte("do not touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "daemon.log")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	f, err := Open(dir, "daemon.log", 0, 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := f.Write([]byte("ours\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = f.Close()

	if got := read(t, target); got != "do not touch" {
		t.Errorf("the symlink target was written through: %q", got)
	}
	if got := read(t, link); got != "ours\n" {
		t.Errorf("the log file = %q, want %q", got, "ours\n")
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("the symlink must be replaced by a regular file (err=%v)", err)
	}
}

func TestOpenRefusesADirectoryInTheWay(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "daemon.log"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, "daemon.log", 0, 0); err == nil {
		t.Fatal("Open must refuse a directory where the log belongs")
	}
}

func TestOpenRefusesANonBareName(t *testing.T) {
	for _, name := range []string{"", ".", "..", "sub/daemon.log", "../daemon.log"} {
		if _, err := Open(t.TempDir(), name, 0, 0); err == nil {
			t.Errorf("Open must refuse the name %q", name)
		}
	}
}

// --- the review round's failure modes (CLA-701) ------------------------------
//
// warnf writes to the process's own stderr, so the assertions below read it back
// through a pipe. Nothing here asserts on a quiet stderr: the tests that do not
// care simply leave it alone.

// captureStderr redirects os.Stderr to a pipe for the rest of the test and
// returns a func that restores it and hands back everything written. Call it
// before the code under test, once, and not before a t.Fatal you want to read.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	prev := os.Stderr
	os.Stderr = w
	restored := false
	restore := func() string {
		if restored {
			return ""
		}
		restored = true
		os.Stderr = prev
		_ = w.Close()
		b, _ := io.ReadAll(r)
		_ = r.Close()
		return string(b)
	}
	t.Cleanup(func() { restore() })
	return restore
}

type errWriter struct{ err error }

func (e errWriter) Write([]byte) (int, error) { return 0, e.err }

// 1.review-1.1 / 1.review-3.3: a rotation whose reopen fails must not panic the
// daemon. The parent directory is renamed out from under an open log, so the
// rotation's rename finds nothing and the reopen has nowhere to create the file
// — the pointer the old code then dereferenced.
func TestAFailedRotationKeepsWritingAndNeverPanics(t *testing.T) {
	dir := t.TempDir()
	moved := filepath.Join(t.TempDir(), "moved")
	stderr := captureStderr(t)
	f, err := Open(dir, "daemon.log", 8, 3)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	if _, err := f.Write([]byte("first\n")); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := os.Rename(dir, moved); err != nil {
		t.Fatalf("rename the log's directory away: %v", err)
	}

	n, err := f.Write([]byte("second-line\n"))
	if err != nil {
		t.Fatalf("a rotation that could not reopen must lose the bound, not the line: %v", err)
	}
	if n != len("second-line\n") {
		t.Errorf("wrote %d bytes, want %d", n, len("second-line\n"))
	}
	if got := read(t, filepath.Join(moved, "daemon.log")); !strings.Contains(got, "second-line") {
		t.Errorf("the line written after the failed rotation is in no file: %q", got)
	}
	if out := stderr(); !strings.Contains(out, "cannot reopen") {
		t.Errorf("the failed reopen was not reported: %q", out)
	}
}

// The truncating mode (keep=1) cannot keep the rotated file — there is none —
// so it must reach the same place by the other route: with the name already
// gone, the handle that is still open is kept rather than dropped.
func TestATruncatingRotationWithTheNameGoneStillWrites(t *testing.T) {
	dir := t.TempDir()
	moved := filepath.Join(t.TempDir(), "moved")
	stderr := captureStderr(t)
	f, err := Open(dir, "daemon.log", 8, 1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	if _, err := f.Write([]byte("first\n")); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := os.Rename(dir, moved); err != nil {
		t.Fatalf("rename the log's directory away: %v", err)
	}

	if _, err := f.Write([]byte("second-line\n")); err != nil {
		t.Fatalf("a truncating rotation that could not reopen must keep the line: %v", err)
	}
	if got := read(t, filepath.Join(moved, "daemon.log")); !strings.Contains(got, "second-line") {
		t.Errorf("the line written after the failed rotation is in no file: %q", got)
	}
	if out := stderr(); !strings.Contains(out, "cannot reopen") {
		t.Errorf("the failed reopen was not reported: %q", out)
	}
}

// 1.review-3.2: the file is the sink that has to survive, so a failing terminal
// must not cost the durable copy. io.MultiWriter stops at the first error, which
// is exactly how a closed stderr used to silence the file too.
func TestTeeWritesToBothSinksWhenOneFails(t *testing.T) {
	failed := errors.New("stderr is gone")
	var file bytes.Buffer
	w := Tee(errWriter{err: failed}, &file)

	n, err := w.Write([]byte("a line\n"))
	if !errors.Is(err, failed) {
		t.Errorf("the failing sink must be reported; got %v", err)
	}
	if n != len("a line\n") {
		t.Errorf("wrote %d bytes, want %d — the sink that worked took the whole line", n, len("a line\n"))
	}
	if file.String() != "a line\n" {
		t.Errorf("the file sink got %q, want the whole line", file.String())
	}

	// Both sinks failing reports both, so a diagnostic naming one does not hide
	// the other.
	other := errors.New("the disk is full")
	_, err = Tee(errWriter{err: failed}, errWriter{err: other}).Write([]byte("x"))
	if !errors.Is(err, failed) || !errors.Is(err, other) {
		t.Errorf("both failures must be reported; got %v", err)
	}
}

// The same failure end to end: Install's own writer must keep the line when the
// terminal it writes to first cannot take it. The pipe's reader is closed, so
// every write to it fails with EPIPE — the shape a daemon whose parent closed its
// inherited stderr wears.
func TestInstallKeepsTheFileWhenStderrIsBroken(t *testing.T) {
	dir := t.TempDir()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	_ = r.Close() // no reader: writes to w fail
	prevStderr := os.Stderr
	os.Stderr = w
	restore, err := Install(dir, "daemon.log")
	if err != nil {
		os.Stderr = prevStderr
		t.Fatalf("Install: %v", err)
	}

	log.Print("the daemon's own line")

	restore()
	os.Stderr = prevStderr
	_ = w.Close()

	if got := read(t, filepath.Join(dir, "daemon.log")); !strings.Contains(got, "the daemon's own line") {
		t.Errorf("a broken stderr cost the log file the line; got %q", got)
	}
}

// 1.review-4.2 (should): the warning is once per failure STREAK, not once per
// process — a directory that breaks, recovers and breaks again is reported
// again. Before the reset there was no streak boundary and the second failure
// went unsaid for the rest of the process's life.
func TestAWarningIsRepeatedForANewFailureStreak(t *testing.T) {
	dir := t.TempDir()
	moved := filepath.Join(t.TempDir(), "moved")
	stderr := captureStderr(t)
	f, err := Open(dir, "daemon.log", 8, 3)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	if _, err := f.Write([]byte("first\n")); err != nil {
		t.Fatalf("first write: %v", err)
	}

	// First failure: the directory is gone, so the reopen has nowhere to go.
	if err := os.Rename(dir, moved); err != nil {
		t.Fatalf("rename away: %v", err)
	}
	if _, err := f.Write([]byte("second\n")); err != nil {
		t.Fatalf("write during the failure: %v", err)
	}
	// Recovery: the directory comes back, so the next rotation succeeds and the
	// streak ends.
	if err := os.Rename(moved, dir); err != nil {
		t.Fatalf("rename back: %v", err)
	}
	if _, err := f.Write([]byte("third\n")); err != nil {
		t.Fatalf("write after the recovery: %v", err)
	}
	// Second failure: a NEW streak, and it must be reported again.
	if err := os.Rename(dir, moved); err != nil {
		t.Fatalf("rename away again: %v", err)
	}
	if _, err := f.Write([]byte("fourth\n")); err != nil {
		t.Fatalf("write during the second failure: %v", err)
	}

	if n := strings.Count(stderr(), "cannot reopen"); n != 2 {
		t.Errorf("reported %d reopen failures, want 2 (one per failure streak)", n)
	}
}
