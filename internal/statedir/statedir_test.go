package statedir

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tempStateDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "state")
}

func mustOpen(t *testing.T, path string, sessionRoots ...string) *Dir {
	t.Helper()
	d, err := Open(path, sessionRoots...)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// --- creation ----------------------------------------------------------------

// The directory holds session transcripts, so no other user may list or read it.
func TestOpenCreatesOwnerOnlyDirectory(t *testing.T) {
	path := tempStateDir(t)
	mustOpen(t, path)

	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Errorf("state dir mode: got %04o, want 0700", got)
	}
}

// The self-ignoring-directory trick. We do not own the repo's .gitignore and
// cannot rely on it, so the state dir ignores itself — an agent's `git add -A`
// in a workdir can never stage a transcript.
func TestOpenWritesSelfIgnoringGitignore(t *testing.T) {
	path := tempStateDir(t)
	mustOpen(t, path)

	b, err := os.ReadFile(filepath.Join(path, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "*\n" {
		t.Errorf(".gitignore body: got %q, want %q", b, "*\n")
	}
	fi, err := os.Lstat(filepath.Join(path, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf(".gitignore mode: got %04o, want 0600", got)
	}
}

// A .gitignore replaced with a symlink is how the protection would be
// neutralised while leaving a file there for a careless check to find.
func TestOpenReplacesASubvertedGitignore(t *testing.T) {
	path := tempStateDir(t)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	decoy := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(decoy, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(decoy, filepath.Join(path, ".gitignore")); err != nil {
		t.Fatal(err)
	}

	mustOpen(t, path)

	fi, err := os.Lstat(filepath.Join(path, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error(".gitignore is still a symlink")
	}
	if b, _ := os.ReadFile(filepath.Join(path, ".gitignore")); string(b) != "*\n" {
		t.Errorf(".gitignore body: got %q", b)
	}
	if b, _ := os.ReadFile(decoy); string(b) != "keep me" {
		t.Errorf("the symlink target was written through: got %q", b)
	}
}

// An existing state dir from before CLA-259 is 0755. Tighten it rather than
// refuse: refusing strands every install on an error only a manual chmod clears.
func TestOpenTightensALooseExistingDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this test relies on")
	}
	path := tempStateDir(t)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	mustOpen(t, path)

	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Errorf("loose dir after Open: got %04o, want 0700", got)
	}
}

// Tightening removes permission; it never grants it.
func TestOpenNeverWidensPermissions(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this test relies on")
	}
	path := tempStateDir(t)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o700) })

	if _, err := Open(path); err == nil {
		t.Error("Open on a read-only state dir: want an error, got nil")
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o500 {
		t.Errorf("read-only dir after Open: got %04o, want 0500 (group/other cleared, owner-write NOT granted)", got)
	}
}

// A symlink where the state dir should be is somebody choosing which directory
// the daemon writes its transcripts into.
func TestOpenRefusesASymlinkedStateDir(t *testing.T) {
	real := filepath.Join(t.TempDir(), "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "state")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(link); err == nil {
		t.Fatal("Open on a symlinked state dir: want an error, got nil")
	} else if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error should say symlink, got %v", err)
	}
}

func TestOpenRefusesANonDirectory(t *testing.T) {
	path := tempStateDir(t)
	if err := os.WriteFile(path, []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Error("Open on a regular file: want an error, got nil")
	}
}

// --- writes ------------------------------------------------------------------

func TestCreateMakesOwnerOnlyFiles(t *testing.T) {
	path := tempStateDir(t)
	d := mustOpen(t, path)

	f, err := d.Create("iteration.log")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	fi, err := os.Lstat(filepath.Join(path, "iteration.log"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("log mode: got %04o, want 0600", got)
	}
}

// The defect this package exists for: a symlink planted at a name the daemon is
// about to write used to make the daemon truncate the target — a file the
// session that planted it could not reach itself.
func TestCreateRefusesAPlantedSymlinkAndLeavesTheTargetIntact(t *testing.T) {
	path := tempStateDir(t)
	d := mustOpen(t, path)

	victim := filepath.Join(t.TempDir(), "authorized_keys")
	const body = "ssh-ed25519 AAAA... operator@laptop\n"
	if err := os.WriteFile(victim, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(path, "iteration.log")); err != nil {
		t.Fatal(err)
	}

	f, err := d.Create("iteration.log")
	if err == nil {
		f.Close()
		t.Fatal("Create through a planted symlink: want an error, got nil")
	}
	if !errors.Is(err, ErrExists) {
		t.Errorf("want ErrExists, got %v", err)
	}
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("the symlink target was written through: got %q, want %q", got, body)
	}
}

// A symlink that stays INSIDE the state dir is the case os.Root alone does not
// catch — it follows those happily, and it ignores O_NOFOLLOW when you pass it.
// O_EXCL is what actually refuses.
func TestCreateRefusesASymlinkThatStaysInsideTheDirectory(t *testing.T) {
	path := tempStateDir(t)
	d := mustOpen(t, path)

	if err := d.WriteFile("earlier.log", []byte("earlier transcript")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("earlier.log", filepath.Join(path, "iteration.log")); err != nil {
		t.Fatal(err)
	}

	if f, err := d.Create("iteration.log"); err == nil {
		f.Close()
		t.Fatal("Create through an in-directory symlink: want an error, got nil")
	}
	if b, _ := os.ReadFile(filepath.Join(path, "earlier.log")); string(b) != "earlier transcript" {
		t.Errorf("earlier log was truncated through the symlink: got %q", b)
	}
}

func TestCreateRefusesAnExistingFile(t *testing.T) {
	d := mustOpen(t, tempStateDir(t))
	if err := d.WriteFile("iteration.log", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if f, err := d.Create("iteration.log"); err == nil {
		f.Close()
		t.Fatal("Create over an existing file: want an error, got nil")
	}
	b, err := d.ReadFile("iteration.log")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "first" {
		t.Errorf("existing file was truncated: got %q", b)
	}
}

func TestWritesCannotEscapeTheDirectory(t *testing.T) {
	base := t.TempDir()
	d := mustOpen(t, filepath.Join(base, "state"))

	for _, name := range []string{"../escaped.log", "/tmp/escaped.log", "sub/escaped.log", "..", "."} {
		if f, err := d.Create(name); err == nil {
			f.Close()
			t.Errorf("Create(%q): want an error, got nil", name)
		}
	}
	if _, err := os.Lstat(filepath.Join(base, "escaped.log")); err == nil {
		t.Error("a write escaped the state dir")
	}
}

// --- reads -------------------------------------------------------------------

func TestReadFileRefusesASymlinkedMarker(t *testing.T) {
	path := tempStateDir(t)
	d := mustOpen(t, path)

	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("sk-live-hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(path, "STOP")); err != nil {
		t.Fatal(err)
	}

	if b, err := d.ReadFile("STOP"); err == nil {
		t.Fatalf("ReadFile through a symlink: want an error, got %q", b)
	}
}

func TestReadFileIsCapped(t *testing.T) {
	d := mustOpen(t, tempStateDir(t))
	if err := d.WriteFile("HALT", []byte(strings.Repeat("x", maxMarkerBytes*3))); err != nil {
		t.Fatal(err)
	}
	b, err := d.ReadFile("HALT")
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != maxMarkerBytes {
		t.Errorf("read %d bytes, want the %d-byte cap", len(b), maxMarkerBytes)
	}
}

// Exists must not follow a symlink either: a dangling one is still "somebody put
// something at this name".
func TestExistsSeesASymlinkWithoutFollowingIt(t *testing.T) {
	path := tempStateDir(t)
	d := mustOpen(t, path)
	if err := os.Symlink(filepath.Join(t.TempDir(), "nothing-here"), filepath.Join(path, "STOP")); err != nil {
		t.Fatal(err)
	}
	if !d.Exists("STOP") {
		t.Error("Exists on a dangling symlink: got false, want true")
	}
}

// Consuming a STOP marker must remove the link, never whatever it points at.
func TestRemoveDeletesTheLinkNotItsTarget(t *testing.T) {
	path := tempStateDir(t)
	d := mustOpen(t, path)

	victim := filepath.Join(t.TempDir(), "keep")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(path, "STOP")); err != nil {
		t.Fatal(err)
	}

	if err := d.Remove("STOP"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(victim); err != nil {
		t.Errorf("the symlink target was removed: %v", err)
	}
}

// GO-2026-4970 is a root escape in os.Root via a symlink plus a trailing slash,
// fixed only in go1.26.5. checkName refuses every separator, so the shape cannot
// be formed from our side whatever toolchain built the binary.
func TestNamesWithSeparatorsAreRefusedRegardlessOfToolchain(t *testing.T) {
	d := mustOpen(t, tempStateDir(t))
	for _, name := range []string{"link/", "link/.", "a/b/", "./x", "sub/", "/"} {
		if err := checkName(name); err == nil {
			t.Errorf("checkName(%q): want an error, got nil", name)
		}
		if f, err := d.Create(name); err == nil {
			f.Close()
			t.Errorf("Create(%q): want an error, got nil", name)
		}
		if _, err := d.ReadFile(name); err == nil {
			t.Errorf("ReadFile(%q): want an error, got nil", name)
		}
	}
}

// --- the path TO the directory -------------------------------------------------

// The escape the final-component check missed. `verify` Lstats the last
// component only; os.MkdirAll resolves symlinks in everything above it AND
// creates through them, so a link a session plants at ANY intermediate component
// relocates the whole state dir — with no race — and doctor reports PASS about a
// directory outside the workdir. Transcripts then land there.
func TestOpenRefusesASymlinkOnTheParentChain(t *testing.T) {
	base := t.TempDir()
	workdir := filepath.Join(base, "repo")
	sensitive := filepath.Join(base, "sensitive")
	for _, d := range []string{workdir, sensitive} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// What a session can do inside the tree it is confined to.
	if err := os.Symlink(sensitive, filepath.Join(workdir, "sub")); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(workdir, "sub", "state")

	_, err := Open(state, workdir)
	if err == nil {
		t.Fatal("Open through a symlinked parent: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error should name the symlink, got %v", err)
	}
	// The point of refusing BEFORE the MkdirAll: the escape target is never made.
	if _, err := os.Lstat(filepath.Join(sensitive, "state")); !os.IsNotExist(err) {
		t.Errorf("%s was created through the symlink", filepath.Join(sensitive, "state"))
	}
}

// The same escape one level deeper, where the planted link's target does not
// exist yet either — MkdirAll would have built the whole chain outside the
// workdir.
func TestOpenRefusesASymlinkSeveralComponentsUpTheChain(t *testing.T) {
	base := t.TempDir()
	workdir := filepath.Join(base, "repo")
	if err := os.MkdirAll(workdir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.Symlink(outside, filepath.Join(workdir, "a")); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(workdir, "a", "b", "c", "state")

	if _, err := Open(state, workdir); err == nil {
		t.Fatal("Open through a symlinked ancestor: want an error, got nil")
	}
	if _, err := os.Lstat(outside); !os.IsNotExist(err) {
		t.Errorf("%s was created through the symlink", outside)
	}
}

// A state dir nested inside the workdir is explicitly supported, and a chain of
// real directories under it must still be created.
func TestOpenCreatesANestedInWorkdirStateDir(t *testing.T) {
	workdir := t.TempDir()
	state := filepath.Join(workdir, "sub", "deeper", "state")

	d := mustOpen(t, state, workdir)

	if d.Path() != state {
		t.Errorf("Path() = %q, want %q", d.Path(), state)
	}
	for _, p := range []string{filepath.Join(workdir, "sub"), filepath.Join(workdir, "sub", "deeper"), state} {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if !fi.IsDir() {
			t.Errorf("%s is not a directory", p)
		}
	}
}

// Above the workdir we do not second-guess the operator's filesystem: `/var` is a
// symlink on macOS and `~/.local` is one on plenty of machines, so refusing every
// symlinked ancestor would be a denial rather than a defence. Nothing a session
// can write is up there.
func TestOpenFollowsASymlinkedAncestorOutsideAnySessionRoot(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(base, "link", "clankerbar", "loop", "slug")

	mustOpen(t, state, filepath.Join(base, "some-workdir"))

	if fi, err := os.Lstat(filepath.Join(real, "clankerbar", "loop", "slug")); err != nil {
		t.Fatalf("state dir was not created under the link's target: %v", err)
	} else if !fi.IsDir() {
		t.Error("state dir is not a directory")
	}
}

// --- adoption ------------------------------------------------------------------

// `doctor` is a command an operator runs BECAUSE it only looks. Pointed at a
// pre-existing directory — a repo root is the plausible misconfiguration — it
// used to replace a tracked .gitignore with `*` and drop the mode to 0700,
// leaving a modification an unattended drain then commits.
func TestOpenRefusesADirectoryThatIsNotAStateDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitignore := filepath.Join(dir, ".gitignore")
	body := "node_modules/\ndist/\n.env\n"
	if err := os.WriteFile(gitignore, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := Open(dir)
	if err == nil {
		t.Fatal("Open on somebody else's directory: want an error, got nil")
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("error should name the path, got %v", err)
	}

	if b, _ := os.ReadFile(gitignore); string(b) != body {
		t.Errorf(".gitignore was rewritten: got %q, want %q", b, body)
	}
	if fi, err := os.Lstat(dir); err != nil {
		t.Fatal(err)
	} else if os.Geteuid() != 0 && fi.Mode().Perm() != 0o755 {
		t.Errorf("directory mode was changed: got %04o, want 0755", fi.Mode().Perm())
	}
	if _, err := os.Lstat(filepath.Join(dir, sentinelName)); !os.IsNotExist(err) {
		t.Error("a sentinel was written into a directory we refused")
	}
}

// The marker that makes the next run's adoption check a one-line answer.
func TestOpenWritesTheSentinel(t *testing.T) {
	path := tempStateDir(t)
	mustOpen(t, path)

	fi, err := os.Lstat(filepath.Join(path, sentinelName))
	if err != nil {
		t.Fatalf("sentinel: %v", err)
	}
	if !fi.Mode().IsRegular() {
		t.Error("sentinel is not a regular file")
	}
	if got := fi.Mode().Perm(); os.Geteuid() != 0 && got != 0o600 {
		t.Errorf("sentinel mode: got %04o, want 0600", got)
	}
}

// A directory carrying the sentinel is ours whatever else is in it — including a
// .gitignore somebody rewrote, which is the case ensureGitignore exists for.
func TestOpenAdoptsADirectoryCarryingTheSentinel(t *testing.T) {
	path := tempStateDir(t)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, sentinelName), []byte(sentinelBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, ".gitignore"), []byte("not ours\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	mustOpen(t, path)

	if b, _ := os.ReadFile(filepath.Join(path, ".gitignore")); string(b) != gitignoreBody {
		t.Errorf(".gitignore in our own dir was not restored: got %q", b)
	}
}

// A state dir written before the sentinel existed holds only files we write, so
// it is adopted and marked rather than refused. Nobody is stranded on an error
// they cannot clear.
func TestOpenAdoptsAPreSentinelStateDir(t *testing.T) {
	path := tempStateDir(t)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		".gitignore":                        gitignoreBody,
		"iteration-20260101-d1-a0-abcd.log": "transcript\n",
		"STOP":                              "operator\n",
		// CLA-461: a dir holding a pending ctl request stays adoptable - the
		// daemon reading the request is the next thing to open this directory.
		"RESTART":     "ctl\n",
		"RESTART_NOW": "ctl --now\n",
		"RELOAD":      "ctl\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(path, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	mustOpen(t, path)

	if _, err := os.Lstat(filepath.Join(path, sentinelName)); err != nil {
		t.Errorf("a pre-sentinel state dir was not marked: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(path, "STOP")); string(b) != "operator\n" {
		t.Errorf("adoption disturbed an existing marker: got %q", b)
	}
	for _, name := range []string{"RESTART", "RESTART_NOW", "RELOAD"} {
		if b, _ := os.ReadFile(filepath.Join(path, name)); string(b) != "ctl\n" && name != "RESTART_NOW" {
			t.Errorf("adoption disturbed %s: got %q", name, b)
		}
	}
}

// An empty directory is adoptable: it is what `mkdir -p` leaves, and there is
// nothing in it to destroy.
func TestOpenAdoptsAnEmptyDirectory(t *testing.T) {
	path := tempStateDir(t)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	mustOpen(t, path)
	if _, err := os.Lstat(filepath.Join(path, sentinelName)); err != nil {
		t.Errorf("an empty directory was not adopted: %v", err)
	}
}

// Nested workdirs are legal in a multi-project config, and anchoring at the
// DEEPEST enclosing one would be an escape: a session running in the outer
// workdir can replace the inner one with a symlink, and anchoring at the link
// resolves it before anything is inspected. Anchoring at the shallowest root
// makes that same component an intermediate one, which makeChain refuses.
func TestOpenAnchorsAtTheShallowestSessionRoot(t *testing.T) {
	base := t.TempDir()
	outer := filepath.Join(base, "repo")
	inner := filepath.Join(outer, "sub")
	escape := filepath.Join(base, "escape")
	for _, d := range []string{outer, escape} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// What a session in the OUTER workdir can do to the inner one.
	if err := os.Symlink(escape, inner); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(inner, "state")

	if _, err := Open(state, outer, inner); err == nil {
		t.Fatal("Open anchored at a swapped inner workdir: want an error, got nil")
	}
	if _, err := os.Lstat(filepath.Join(escape, "state")); !os.IsNotExist(err) {
		t.Errorf("%s was created through the swapped workdir", filepath.Join(escape, "state"))
	}
}

// `doctor` (checkStateDir) has warned about a state dir that IS a session
// workdir since CLA-283, and that WARN is only honest if Open still succeeds -
// a FAIL here would silently turn "WARN, loop still runs" into "gates doctor &&
// run". TestEnclosingSessionRootMatchesTheRootItself is what pins the anchor
// choice CLA-284 actually changed; this pins the observable contract Open owes
// its caller once that choice is made.
func TestOpenAdoptsAStateDirThatIsExactlyASessionWorkdir(t *testing.T) {
	workdir := t.TempDir()

	d, err := Open(workdir, workdir)
	if err != nil {
		t.Fatalf("Open(workdir, workdir): %v", err)
	}
	defer func() { _ = d.Close() }()

	if _, err := os.Lstat(filepath.Join(workdir, sentinelName)); err != nil {
		t.Errorf("state dir was not adopted: sentinel missing: %v", err)
	}
}

// enclosingSessionRoot at abs == cand: the equality case CLA-284 fixes. Before,
// HasPrefix(abs, cand+"/") required cand to be a STRICT prefix, so a state dir
// exactly equal to a session root matched nothing and fell through to the
// trusting branch (see TestOpenAdoptsAStateDirThatIsExactlyASessionWorkdir for
// what that costs).
func TestEnclosingSessionRootMatchesTheRootItself(t *testing.T) {
	root := t.TempDir()

	if got := enclosingSessionRoot(root, []string{root}); got != root {
		t.Errorf("enclosingSessionRoot(root, [root]) = %q, want %q", got, root)
	}
}

// Pinned so a rewrite to filepath.Rel does not reintroduce the class of bug this
// package already handles correctly: a sibling directory that merely SHARES a
// string prefix with a session root ("/a/workdir-2" against "/a/workdir") is not
// inside it, and no session spawned in the root can reach it.
func TestEnclosingSessionRootDoesNotMatchASiblingSharingAPrefix(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workdir")
	sibling := filepath.Join(base, "workdir-2")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}

	if got := enclosingSessionRoot(sibling, []string{root}); got != "" {
		t.Errorf("enclosingSessionRoot(sibling, [root]) = %q, want \"\"", got)
	}
}

// The equality case participates in the shortest-root tie-break, not just
// strict containment: a state dir equal to the INNER of two nested session
// roots is enclosed by both (Rel(outer, inner) is the subpath "sub"; Rel(inner,
// inner) is "."), and the shallower OUTER root must still win the
// len(cand) >= len(best) comparison across both orderings - otherwise inner's
// own name stops being a checked component and becomes the anchor itself,
// which is exactly the escape the doc comment above walks through. Together
// with TestEnclosingSessionRootMatchesTheRootItself (which confirms inner is a
// candidate at all), this pins that inner loses the tie-break rather than never
// competing in it.
func TestEnclosingSessionRootPrefersTheOuterRootWhenStateDirIsTheInnerRootItself(t *testing.T) {
	base := t.TempDir()
	outer := filepath.Join(base, "repo")
	inner := filepath.Join(outer, "sub")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatal(err)
	}

	for _, roots := range [][]string{{outer, inner}, {inner, outer}} {
		if got := enclosingSessionRoot(inner, roots); got != outer {
			t.Errorf("enclosingSessionRoot(inner, %v) = %q, want %q", roots, got, outer)
		}
	}
}

// state_dir set to literally BE the inner workdir, which a session in the
// OUTER workdir has swapped for a symlink. Distinct coverage from
// TestOpenAnchorsAtTheShallowestSessionRoot, which swaps the same inner root
// but names a STATE DIR UNDER IT - refused there by makeChain's intermediate-
// component check. Here inner is never a tie-break candidate at all (Lstat
// sees a symlink, not a directory, before any length comparison runs), so
// anchorFor falls back to the parent and the refusal comes from Open's own
// final-component symlink check instead.
func TestOpenRefusesASymlinkedSessionRootNamedAsTheStateDir(t *testing.T) {
	base := t.TempDir()
	outer := filepath.Join(base, "repo")
	inner := filepath.Join(outer, "sub")
	escape := filepath.Join(base, "escape")
	for _, d := range []string{outer, escape} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(escape, inner); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(inner, outer, inner); err == nil {
		t.Fatal("Open anchored at a swapped inner workdir named as state_dir itself: want an error, got nil")
	}
	if _, err := os.Lstat(filepath.Join(escape, sentinelName)); !os.IsNotExist(err) {
		t.Errorf("%s was created through the swapped workdir", filepath.Join(escape, sentinelName))
	}
}

// deepestExistingAncestor returns "/" for abs == "/" (filepath.Dir("/") == "/"),
// which used to make anchorFor's rel == "." arm land there by an accident of
// arithmetic rather than by the equality case this task adds. Guarded
// separately so allowing rel == "." for a real state-dir-equals-workdir case
// does not also open the filesystem root as a would-be state dir - reachable in
// practice through an unvalidated state_dir config value (config.ResolveStateDir
// does not reject it).
func TestAnchorForRefusesTheFilesystemRoot(t *testing.T) {
	if _, _, err := anchorFor("/", nil); err == nil {
		t.Fatal(`anchorFor("/", nil): want an error, got nil`)
	}
}

// `state_dir` back inside a workdir is supported, so the confined session can
// write the state dir — and a DIRECTORY squatting on one of the two names we
// insist on owning cannot simply be removed. Left unhandled that wedges every
// later `run` and `doctor` alike on a bare `removeat: directory not empty`,
// which is a denial of the daemon driven from inside the sandbox, and one
// `doctor` cannot diagnose because it fails the same way.
func TestOpenSaysWhatToDoAboutADirectorySquattingOnOurNames(t *testing.T) {
	for _, name := range []string{".gitignore", sentinelName} {
		t.Run(name, func(t *testing.T) {
			path := tempStateDir(t)
			if err := os.MkdirAll(filepath.Join(path, name, "child"), 0o700); err != nil {
				t.Fatal(err)
			}

			_, err := Open(path)
			if err == nil {
				t.Fatal("Open with a non-empty directory in the way: want an error, got nil")
			}
			if !strings.Contains(err.Error(), filepath.Join(path, name)) {
				t.Errorf("error should name the path in the way, got %v", err)
			}
			if !strings.Contains(err.Error(), "rm -rf") {
				t.Errorf("error should say how to clear it, got %v", err)
			}
			if _, err := os.Lstat(filepath.Join(path, name, "child")); err != nil {
				t.Errorf("the directory in the way was deleted: %v", err)
			}
		})
	}
}

// The state dir is exactly the directory an operator opens to read a transcript,
// and on darwin that leaves a .DS_Store behind — invisible in the tool that made
// it. Refusing on one would stop the daemon over a file the operator cannot see.
func TestOpenAdoptsDespiteAFileManagerDropping(t *testing.T) {
	path := tempStateDir(t)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		".gitignore":                        gitignoreBody,
		".DS_Store":                         "\x00\x01",
		"iteration-20260101-d1-a0-abcd.log": "transcript\n",
	} {
		if err := os.WriteFile(filepath.Join(path, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	mustOpen(t, path)

	if _, err := os.Lstat(filepath.Join(path, sentinelName)); err != nil {
		t.Errorf("a state dir with a .DS_Store in it was not adopted: %v", err)
	}
}

// A refusal has to list EVERY offending name. Half of them are dotfiles the
// operator cannot see in a file manager, and reporting one at a time turns
// clearing the directory into a guessing game one round trip deep.
func TestOpenNamesEveryForeignEntryItRefusesOver(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Open(dir)
	if err == nil {
		t.Fatal("Open on somebody else's directory: want an error, got nil")
	}
	for _, name := range []string{"src", "README.md"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error should name %s, got %v", name, err)
		}
	}
}
