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
//  3. The PATH TO the directory is checked, not just its last component. A
//     symlink at any component above it relocates the whole thing, and an
//     unguarded os.MkdirAll would CREATE the escape target through the link — no
//     race required. Open therefore never calls MkdirAll on the raw path: it
//     picks an anchor no session can have tampered with (see anchorFor) and
//     creates every component below it through an [os.Root] on that anchor,
//     refusing a symlink it meets on the way.
//
//  4. The directory is only ADOPTED if it is ours. Points 1 and 3 change a
//     directory's mode and drop a `.gitignore` of `*` into it, which is fine on a
//     directory we made and destructive on one an operator mis-pointed us at (a
//     repo root, say). So an existing directory must carry our sentinel file, be
//     empty, or hold nothing but files we write; otherwise Open refuses and
//     touches nothing. Same principle as config.vetTrustedFile: refuse, never
//     silently rewrite the operator's filesystem.
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

// sentinelName is how a state directory says it is ours. Open writes it into
// every directory it adopts, and requires it (or an otherwise-empty directory)
// before it will chmod one or rewrite its .gitignore.
//
// Without it `doctor` — a command whose whole promise is that it only LOOKS —
// would clobber a tracked .gitignore and tighten the mode of any directory
// `state_dir` happened to name.
const sentinelName = ".clankerbar-statedir"

const sentinelBody = "clankerbar loop state dir: iteration transcripts and STOP/HALT markers.\n" +
	"Created and owned by clankerbar; safe to delete while no loop is running.\n"

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
// sessionRoots are the directories spawned sessions run in — every tree the
// confinement lets a session write, i.e. every tree in which a symlink on the
// way to the state dir may have been planted rather than put there by the
// operator (config.Config.SessionWorkDirs). Passing none is safe but weaker: the
// path is still built through an [os.Root], so nothing we CREATE can be
// redirected, but an already-existing symlinked component outside them is taken
// as the operator's own filesystem layout. Above a session root we do not
// second-guess it — `/var` is a symlink on macOS, and `~/.local` is one on plenty
// of machines. That is the same line config.vetTrustedFile draws when it checks
// a file's immediate parent and not the whole ancestor chain.
//
// It is safe to call on a directory an older, looser version of clankerbar
// created: an existing 0755 state dir is tightened to 0700 rather than refused,
// because refusing would strand every existing installation on an error it can
// only fix by hand. It is NOT safe to point at a directory that is somebody
// else's — see (*Dir).checkAdoptable.
func Open(path string, sessionRoots ...string) (*Dir, error) {
	if path == "" {
		return nil, errors.New("state dir: empty path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("state dir %s: %w", path, err)
	}

	anchor, rel, err := anchorFor(abs, sessionRoots)
	if err != nil {
		return nil, err
	}
	// One handle on the anchor for the whole of Open: every path below it is
	// resolved through this root, so no component of it can be swapped for a
	// symlink out from under us between the check and the use.
	anchorRoot, err := os.OpenRoot(anchor)
	if err != nil {
		return nil, fmt.Errorf("state dir %s: %w", abs, err)
	}
	defer anchorRoot.Close()

	if err := makeChain(anchorRoot, anchor, abs, rel); err != nil {
		return nil, err
	}
	fi, err := verify(anchorRoot, rel, abs)
	if err != nil {
		return nil, err
	}

	root, err := anchorRoot.OpenRoot(rel)
	if err != nil {
		return nil, fmt.Errorf("state dir %s: %w", abs, err)
	}
	d := &Dir{root: root, path: abs}

	// Adoption is decided BEFORE anything is written or chmodded, and reads only.
	// A refusal here must leave the directory exactly as it was found.
	if err := d.checkAdoptable(); err != nil {
		root.Close()
		return nil, err
	}
	if err := tighten(anchorRoot, rel, abs, fi); err != nil {
		root.Close()
		return nil, err
	}
	if err := d.ensureSentinel(); err != nil {
		root.Close()
		return nil, err
	}
	if err := d.ensureGitignore(); err != nil {
		root.Close()
		return nil, err
	}
	return d, nil
}

// anchorFor picks the directory the state dir is built from, and the path of the
// state dir relative to it. Everything at or below the anchor is created and
// inspected through an [os.Root] on it; everything above is taken on trust.
//
// Inside a session root, trust is exactly what we do not have: pick the session
// root itself, so every component between it and the state dir is ours to check.
// Elsewhere, pick the deepest ancestor that already exists — nothing below it
// exists yet, so there is nothing there to have been planted, and anchoring at
// the deepest EXISTING one means a symlink the operator deliberately put
// somewhere above (`~/.local` -> a data volume, `/var` on macOS) is resolved
// normally instead of being refused.
func anchorFor(abs string, sessionRoots []string) (anchor, rel string, err error) {
	if r := enclosingSessionRoot(abs, sessionRoots); r != "" {
		anchor = r
	} else {
		anchor = deepestExistingAncestor(abs)
	}
	rel, err = filepath.Rel(anchor, abs)
	if err != nil || rel == "." || rel == "" || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("state dir %s: cannot be created (no directory to create it in)", abs)
	}
	return anchor, rel, nil
}

// enclosingSessionRoot is the longest session root that strictly contains abs,
// or "" if none does.
//
// A root is matched both as given and as EvalSymlinks resolves it, because
// `state_dir` and `workdir` may name the same tree by different routes (macOS
// hands out /var/... temp dirs that really live under /private/var) and a
// lexical miss there would silently downgrade to the trusting branch.
func enclosingSessionRoot(abs string, roots []string) string {
	best := ""
	for _, r := range roots {
		if r == "" {
			continue
		}
		candidates := []string{r}
		if resolved, err := filepath.EvalSymlinks(r); err == nil && resolved != r {
			candidates = append(candidates, resolved)
		}
		for _, cand := range candidates {
			cand, err := filepath.Abs(cand)
			if err != nil || !strings.HasPrefix(abs, cand+string(filepath.Separator)) || len(cand) <= len(best) {
				continue
			}
			if fi, err := os.Stat(cand); err != nil || !fi.IsDir() {
				continue
			}
			best = cand
		}
	}
	return best
}

// deepestExistingAncestor walks up from abs to the first directory that is
// already there. Stat, not Lstat: a symlinked ancestor outside every session
// root is the operator's own layout, and resolving it is the point.
func deepestExistingAncestor(abs string) string {
	dir := filepath.Dir(abs)
	for {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}

// makeChain creates every missing component of rel under root, 0700, refusing a
// symlink at any component it did not create.
//
// This is what os.MkdirAll cannot do. MkdirAll resolves symlinks in the prefix
// and creates through them, so a link at any intermediate component silently
// relocates the state dir AND gets its target created — the escape needs no race
// and leaves `doctor` reporting PASS about a directory outside the workdir. Here
// each component is Lstat'd through the root before the next one is touched, and
// os.Root refuses on its own account any resolution that leaves the anchor.
//
// The final component is left to verify, which already has the message for a
// symlink or a non-directory sitting where the state dir should be.
func makeChain(root *os.Root, anchor, abs, rel string) error {
	parts := strings.Split(rel, string(filepath.Separator))
	cum := ""
	for i, part := range parts {
		if cum == "" {
			cum = part
		} else {
			cum += string(filepath.Separator) + part
		}
		last := i == len(parts)-1
		fi, err := root.Lstat(cum)
		if err == nil {
			if last {
				continue
			}
			here := filepath.Join(anchor, cum)
			if fi.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("state dir %s is reached through the symlink %s - refusing: a link anywhere on the path moves the whole directory, and whoever can write %s then chooses where the daemon's transcripts land. Replace it with a real directory, or point state_dir somewhere a session cannot write", abs, here, here)
			}
			if !fi.IsDir() {
				return fmt.Errorf("state dir %s cannot be created: %s is not a directory", abs, here)
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("state dir %s: %w", abs, err)
		}
		// Mkdir, not MkdirAll, and one component at a time: MkdirAll is a no-op on
		// an existing directory and would leave a mode we never chose.
		if err := root.Mkdir(cum, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("state dir %s: %w", abs, err)
		}
	}
	return nil
}

// verify refuses a state dir that is not a plain directory we own. It reads only
// — tightening a mode is tighten's job, and must not happen before the caller has
// established the directory is ours to change.
//
// Lstat, not Stat: a symlink is exactly what we are refusing, and Stat would
// report the target and pass.
func verify(root *os.Root, rel, abs string) (os.FileInfo, error) {
	fi, err := root.Lstat(rel)
	if err != nil {
		return nil, fmt.Errorf("state dir %s: %w", abs, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("state dir %s is a symlink - refusing to write session transcripts through it; point state_dir at a real directory", abs)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("state dir %s is not a directory", abs)
	}
	if permissionBitsAreMeaningful {
		if uid, ok := fileOwnerUID(fi); ok && uid != os.Getuid() {
			return nil, fmt.Errorf("state dir %s is owned by uid %d, not by you (uid %d) - refusing: its contents are session transcripts", abs, uid, os.Getuid())
		}
	}
	return fi, nil
}

// tighten clears any group/other bits an older, looser clankerbar left on the
// directory.
func tighten(root *os.Root, rel, abs string, fi os.FileInfo) error {
	if !permissionBitsAreMeaningful {
		return nil
	}
	perm := fi.Mode().Perm()
	if perm&0o077 == 0 {
		return nil
	}
	// An older clankerbar made this 0755, so tighten rather than refuse —
	// refusing would strand every existing install on an error only a manual
	// chmod clears.
	//
	// Clearing the group/other bits, NOT setting 0700: this only ever REMOVES
	// permission. An operator who deliberately made their state dir read-only
	// gets 0500 and a plain "not writable" further on, rather than having the
	// tool quietly hand itself write access to a directory they locked.
	tightened := perm &^ 0o077
	if err := root.Chmod(rel, tightened); err != nil {
		return fmt.Errorf("state dir %s is mode %04o and could not be tightened to %04o: %w", abs, perm, tightened, err)
	}
	after, err := root.Lstat(rel)
	if err != nil {
		return fmt.Errorf("state dir %s: %w", abs, err)
	}
	if after.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("state dir %s is still mode %04o after tightening - session transcripts must not be readable by other users", abs, after.Mode().Perm())
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

// checkAdoptable is the gate in front of every change Open makes to a directory
// it did not create this run: the chmod and the `.gitignore` rewrite.
//
// Both are right on our own directory and destructive on somebody else's. Point
// `state_dir` at a repo root — the plausible misconfiguration — and without this
// `doctor`, a command an operator runs precisely BECAUSE it only looks, would
// replace a tracked `.gitignore` with `*` and drop the directory to 0700, leaving
// a modified file for the next unattended `git add -A` to commit.
//
// So we adopt only what is plainly ours: our sentinel, an empty directory, or a
// directory holding nothing but files we write (which is what a state dir from
// before the sentinel existed looks like). Anything else is refused by name, and
// nothing on disk is touched.
func (d *Dir) checkAdoptable() error {
	if fi, err := d.root.Lstat(sentinelName); err == nil && fi.Mode().IsRegular() {
		return nil
	}
	f, err := d.root.Open(".")
	if err != nil {
		return fmt.Errorf("state dir %s: %w", d.path, err)
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return fmt.Errorf("state dir %s: %w", d.path, err)
	}
	for _, name := range names {
		if d.isOurArtifact(name) {
			continue
		}
		return fmt.Errorf("state dir %s already exists and holds %s, which clankerbar did not write - refusing to adopt it: adopting means rewriting its .gitignore to `*` (hiding everything in it from git) and taking its mode to 0700. Point state_dir at a new or empty directory, or move that content aside", d.path, name)
	}
	return nil
}

// isOurArtifact reports whether one entry of an existing directory is a file
// clankerbar itself writes. The set is small and closed on purpose — it is the
// evidence for "this directory is a state dir", so a name we do not write is
// evidence it is something else and adoption stops.
func (d *Dir) isOurArtifact(name string) bool {
	switch {
	case name == sentinelName, name == "STOP", name == "HALT":
		return true
	case strings.HasPrefix(name, "iteration-") && strings.HasSuffix(name, ".log"):
		return true
	case strings.HasPrefix(name, ".doctor-write-probe-"):
		return true
	case name == ".gitignore":
		// Ours if it already carries the self-ignoring body. Ours too if it is not
		// a regular file: a symlink there is something planted to neutralise the
		// protection, not operator content, and ensureGitignore removes the link
		// and never its target.
		fi, err := d.root.Lstat(name)
		if err != nil || !fi.Mode().IsRegular() {
			return true
		}
		b, err := d.ReadFile(name)
		return err == nil && string(b) == gitignoreBody
	}
	return false
}

// ensureSentinel writes the marker that makes the next run's adoption check a
// one-line answer instead of an inventory.
func (d *Dir) ensureSentinel() error {
	if fi, err := d.root.Lstat(sentinelName); err == nil {
		if fi.Mode().IsRegular() {
			return nil
		}
		if err := d.root.Remove(sentinelName); err != nil {
			return fmt.Errorf("state dir %s: %w", filepath.Join(d.path, sentinelName), err)
		}
	}
	if err := d.WriteFile(sentinelName, []byte(sentinelBody)); err != nil {
		return fmt.Errorf("state dir %s: %w", filepath.Join(d.path, sentinelName), err)
	}
	return nil
}

// ensureGitignore guarantees the self-ignoring `.gitignore`, replacing anything
// that is not already exactly it — including a symlink, which is how a session
// would neutralise the protection while leaving a file there for us to find.
//
// Only ever reached past checkAdoptable, so "replace what is there" is a
// statement about our own directory, not about a file an operator wrote.
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
