// Package statedir owns the one directory the DAEMON writes on its own account:
// the loop's control markers (STOP/HALT) and its per-iteration transcripts.
//
// It exists because that directory is the daemon's blind spot. The daemon runs
// OUTSIDE the confinement its own sessions run inside — claude gets
// `--permission-mode acceptEdits` plus a deny policy, codex `--sandbox
// workspace-write`, opencode a fail-closed `OPENCODE_PERMISSION` — so a session
// cannot write outside its workdir, but the daemon can write anywhere. Before
// CLA-259 the state dir sat at `<workdir>/.clankerbar-loop`, i.e. squarely
// inside the one tree the session IS allowed to write, with 0755/0644 modes and
// symlink-following writes. A session could therefore plant a symlink at a path
// the daemon was about to write and have the daemon truncate a file the session
// could not reach — the confinement defeated by proxy — or simply let an
// overnight `git add -A` commit the night's transcripts, which record whatever
// the harness printed, tokens included.
//
// Two properties close that, and both are enforced here rather than at each call
// site:
//
//  1. The directory is ours alone. Created 0700, tightened to 0700 if an older
//     run left it looser, refused outright if it is a symlink or owned by
//     somebody else. A `.gitignore` containing `*` is written into it, so even a
//     state dir an operator deliberately points back inside a repo can never be
//     staged by an agent's `git add -A`.
//
//  2. Nothing written here follows a symlink or reuses an existing inode. Every
//     write goes through an [os.Root] scoped to the directory (which refuses any
//     path resolving outside it) AND uses O_CREATE|O_EXCL, which refuses to open
//     an existing file OR symlink at all. Note that O_EXCL, not O_NOFOLLOW, is
//     what does the work: os.Root silently ignores O_NOFOLLOW in the flags it is
//     handed and will happily follow a symlink that stays inside the root.
//     Reads Lstat first and refuse a symlink themselves.
//
// The default location moved out of the workdir at the same time (see
// config.ResolveStateDir). This package still assumes nothing about where it is
// pointed: an operator may aim `state_dir` back into a repo, and everything above
// has to hold when they do.
package statedir

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// gitignoreBody is the self-ignoring-directory trick: a `.gitignore` whose only
// rule is `*` ignores every sibling AND itself, so the whole directory is
// invisible to `git add -A` without needing an entry in the repo's own
// .gitignore — which we do not own and cannot rely on.
const gitignoreBody = "*\n"

// maxMarkerBytes caps a control-marker read. A marker holds a one-line reason;
// the cap stops a large or endless file (a fifo, /dev/zero, a log someone
// pointed at us) from being pulled into memory and echoed into the daemon log.
const maxMarkerBytes = 4096

// ErrExists is returned by Create when something already occupies the name — a
// stale file, or a symlink planted there to redirect the write. Callers treat it
// as "do not write", never as "write somewhere else".
var ErrExists = errors.New("state dir: path already exists")

// Dir is an opened, verified state directory. Its zero value is not usable; get
// one from Open, and Close it when the process is done with it.
type Dir struct {
	root *os.Root
	path string
}

// Open creates (if needed) and verifies the state directory at path, then
// returns a handle whose writes cannot leave it.
//
// It is safe to call on a directory an older, looser version of clankerbar
// created: an existing 0755 state dir is tightened to 0700 rather than refused,
// because refusing would strand every existing installation on an error it can
// only fix by hand.
func Open(path string) (*Dir, error) {
	if path == "" {
		return nil, errors.New("state dir: empty path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("state dir %s: %w", path, err)
	}

	// Parents at 0700 too. On the XDG default this creates ~/.local/state and
	// friends, which the spec already wants at 0700; where they exist already
	// MkdirAll leaves their modes alone, which is the operator's business.
	if parent := filepath.Dir(abs); parent != abs {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return nil, fmt.Errorf("state dir %s: %w", abs, err)
		}
	}
	// Mkdir, not MkdirAll, for the final component: MkdirAll is a no-op on an
	// existing directory and would leave a mode we never chose, while Mkdir tells
	// us plainly whether this run created it.
	if err := os.Mkdir(abs, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("state dir %s: %w", abs, err)
	}

	if err := verify(abs); err != nil {
		return nil, err
	}

	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("state dir %s: %w", abs, err)
	}
	d := &Dir{root: root, path: abs}
	if err := d.ensureGitignore(); err != nil {
		root.Close()
		return nil, err
	}
	return d, nil
}

// verify refuses a state dir that is not a plain directory we own, and tightens
// one that is merely too permissive.
//
// Lstat, not Stat: a symlink is exactly what we are refusing, and Stat would
// report the target and pass.
func verify(abs string) error {
	fi, err := os.Lstat(abs)
	if err != nil {
		return fmt.Errorf("state dir %s: %w", abs, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("state dir %s is a symlink - refusing to write session transcripts through it; point state_dir at a real directory", abs)
	}
	if !fi.IsDir() {
		return fmt.Errorf("state dir %s is not a directory", abs)
	}
	if !permissionBitsAreMeaningful {
		return nil
	}
	if uid, ok := fileOwnerUID(fi); ok && uid != os.Getuid() {
		return fmt.Errorf("state dir %s is owned by uid %d, not by you (uid %d) - refusing: its contents are session transcripts", abs, uid, os.Getuid())
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		// An older clankerbar made this 0755, so tighten rather than refuse —
		// refusing would strand every existing install on an error only a manual
		// chmod clears.
		//
		// Clearing the group/other bits, NOT setting 0700: this only ever REMOVES
		// permission. An operator who deliberately made their state dir read-only
		// gets 0500 and a plain "not writable" further on, rather than having the
		// tool quietly hand itself write access to a directory they locked.
		tightened := perm &^ 0o077
		if err := os.Chmod(abs, tightened); err != nil {
			return fmt.Errorf("state dir %s is mode %04o and could not be tightened to %04o: %w", abs, perm, tightened, err)
		}
		fi, err = os.Lstat(abs)
		if err != nil {
			return fmt.Errorf("state dir %s: %w", abs, err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("state dir %s is still mode %04o after tightening - session transcripts must not be readable by other users", abs, fi.Mode().Perm())
		}
	}
	return nil
}

// Path is the absolute directory this Dir writes into.
func (d *Dir) Path() string { return d.path }

// Close releases the directory handle.
func (d *Dir) Close() error { return d.root.Close() }

// Create makes a NEW file called name inside the directory, 0600, and fails if
// anything is already there.
//
// O_EXCL is the load-bearing flag. A symlink planted at name makes this return
// ErrExists rather than truncating whatever it points at, and so does a stale
// regular file — both are "someone else decided what is at this path", which is
// never a thing to write a transcript into. name must be a bare filename.
func (d *Dir) Create(name string) (*os.File, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	f, err := d.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("%w: %s", ErrExists, filepath.Join(d.path, name))
		}
		return nil, err
	}
	return f, nil
}

// WriteFile creates name and writes data to it, with Create's guarantees.
func (d *Dir) WriteFile(name string, data []byte) error {
	f, err := d.Create(name)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ReadFile reads a file from the directory, refusing to follow a symlink and
// reading at most maxMarkerBytes.
func (d *Dir) ReadFile(name string) ([]byte, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	fi, err := d.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink - refusing to read a control marker through one", filepath.Join(d.path, name))
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", filepath.Join(d.path, name))
	}
	f, err := d.root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxMarkerBytes))
}

// Exists reports whether name is present, without following a symlink and
// without caring what it is.
func (d *Dir) Exists(name string) bool {
	if err := checkName(name); err != nil {
		return false
	}
	_, err := d.root.Lstat(name)
	return err == nil
}

// Remove deletes name from the directory. Removing a symlink removes the link,
// never its target, so this is safe on a planted marker.
func (d *Dir) Remove(name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	return d.root.Remove(name)
}

// ensureGitignore guarantees the self-ignoring `.gitignore`, replacing anything
// that is not already exactly it — including a symlink, which is how a session
// would neutralise the protection while leaving a file there for us to find.
func (d *Dir) ensureGitignore() error {
	const name = ".gitignore"
	if fi, err := d.root.Lstat(name); err == nil {
		if fi.Mode().IsRegular() {
			if b, err := d.ReadFile(name); err == nil && string(b) == gitignoreBody {
				return nil
			}
		}
		if err := d.root.Remove(name); err != nil {
			return fmt.Errorf("state dir %s: %w", filepath.Join(d.path, name), err)
		}
	}
	if err := d.WriteFile(name, []byte(gitignoreBody)); err != nil {
		return fmt.Errorf("state dir %s: %w", filepath.Join(d.path, name), err)
	}
	return nil
}

// checkName rejects anything that is not a bare filename. Every caller in this
// repo passes a constant or a generated name, so this is a guard against a
// future one passing something derived — `..`, a nested path, an absolute path —
// and quietly widening what the daemon writes.
//
// It is not merely tidy. GO-2026-4970 is a root escape in os.Root itself, via a
// symlink plus a TRAILING SLASH, fixed in go1.26.5 — so on any older toolchain
// the escape guard we lean on has a hole with a slash in it. Refusing every name
// containing a separator closes that from our side, independent of which Go
// built the binary.
func checkName(name string) error {
	if name == "" || name == "." || name == ".." ||
		strings.ContainsRune(name, filepath.Separator) ||
		strings.ContainsRune(name, '/') ||
		filepath.IsAbs(name) {
		return fmt.Errorf("state dir: %q is not a bare file name", name)
	}
	return nil
}
