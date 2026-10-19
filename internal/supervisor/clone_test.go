package supervisor

// The clone's install step (CLA-678): a completed clone is moved into the
// derived path only when that path is still absent. git clone runs for minutes,
// so a directory that appears while it runs belongs to whoever created it and
// must be left untouched — the fail-closed posture the derivation keeps for
// every other path it does not own.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installClone moves a completed clone into place when nothing is there.
func TestInstallCloneMovesIntoAnAbsentPath(t *testing.T) {
	base := t.TempDir()
	tmp := filepath.Join(base, "widgets.clone-1")
	if err := os.MkdirAll(filepath.Join(tmp, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "widgets")
	if err := installClone(tmp, dir, "acme/widgets"); err != nil {
		t.Fatalf("installClone: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dir, ".git")); err != nil || !fi.IsDir() {
		t.Fatalf("the clone was not installed at %s: %v", dir, err)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("the temporary clone still exists after install: %v", err)
	}
}

// A destination that raced in while git ran is NEVER replaced: not an empty
// directory, not a file, not a symlink. os.Rename alone would refuse the
// directory but silently replace the file and the symlink, so each shape is
// pinned here.
func TestInstallCloneRefusesARacedInDestination(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "widgets")

	// An empty directory that appeared during the clone.
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(base, "widgets.clone-2")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	err := installClone(tmp, dir, "acme/widgets")
	if err == nil {
		t.Fatal("installClone replaced a directory that appeared during the clone")
	}
	if !strings.Contains(err.Error(), "appeared") {
		t.Errorf("err %q does not say the destination appeared during the clone", err)
	}
	if entries, readErr := os.ReadDir(dir); readErr != nil || len(entries) != 0 {
		t.Fatalf("the raced-in directory was modified: %v %v", entries, readErr)
	}

	// A file at the destination: os.Rename WOULD replace this one, so the
	// check is what stops it.
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("someone else's file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := installClone(tmp, dir, "acme/widgets"); err == nil {
		t.Fatal("installClone replaced a file that appeared during the clone")
	}
	if b, readErr := os.ReadFile(dir); readErr != nil || string(b) != "someone else's file" {
		t.Fatalf("the raced-in file was modified: %q %v", b, readErr)
	}

	// A symlink at the destination: likewise refused, and its target untouched.
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "outside")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, dir); err != nil {
		t.Fatal(err)
	}
	if err := installClone(tmp, dir, "acme/widgets"); err == nil {
		t.Fatal("installClone replaced a symlink that appeared during the clone")
	}
	if fi, err := os.Lstat(dir); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the raced-in symlink was replaced: %v", err)
	}
	if entries, readErr := os.ReadDir(target); readErr != nil || len(entries) != 0 {
		t.Fatalf("the symlink's target was modified: %v %v", entries, readErr)
	}
}
