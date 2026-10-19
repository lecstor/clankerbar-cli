package logfile

import (
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
