// Package logfile writes a process's OWN log lines to a size-bounded,
// self-rotating file: the daemon's decisions (a session's spawn, exit code,
// classification and why the run ended) and the supervisor's, one file per
// process, beside the state that process owns.
//
// It exists because the daemon's own lines used to have nowhere durable to go
// (CLA-701). They reached the operator's terminal and nothing else, so the one
// record that says what the driver DECIDED was gone as soon as the terminal's
// scrollback wrapped — while the session's stream, which is what displaced it,
// was already kept in full in the iteration log. A monitoring agent cannot read
// an operator's terminal, and a fleet's worth of terminals is not a log store.
//
// Rotation is size-based and deliberately small: keep at most `keep` files of
// about `maxBytes` in total, newest first (`NAME`, `NAME.1`, ...
// `NAME.<keep-1>`). The
// daemon is a long-lived process by design — it runs until a STOP marker or a
// budget ceiling — so an unrotated file would eventually be the largest thing
// in the state dir.
//
// Nothing here is allowed to become control flow. A log that cannot be opened,
// rotated or written degrades the daemon's observability and nothing else: the
// caller treats Open's error as a warning, and Write's error as garbage the
// standard logger swallows.
package logfile

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	// DefaultMaxBytes is the size one file reaches before it rotates. The
	// daemon's own lines are sparse (one per session boundary, plus warns), so
	// this is weeks of history rather than hours.
	DefaultMaxBytes int64 = 8 << 20 // 8 MiB
	// DefaultKeep is how many rotated files are kept beside the current one.
	// 3 x 8 MiB caps a log directory's growth at 24 MiB per process.
	DefaultKeep = 3
)

// File is one open, rotating log. Its zero value is not usable; get one from
// Open. It is safe for concurrent use — the standard logger serialises its own
// writes, but a process may also write here from more than one goroutine.
type File struct {
	max  int64
	keep int

	mu   sync.Mutex
	f    *os.File
	size int64
	path string

	// warned records that a rotation failure has been reported, so a broken
	// directory does not turn every line into a second line about itself.
	warned bool
}

// Open appends to NAME in dir, creating dir (0700) and the file (0600) when
// absent, and returns a writer that rotates once the file reaches maxBytes.
// maxBytes <= 0 takes DefaultMaxBytes; keep <= 0 takes DefaultKeep.
//
// A SYMLINK at NAME is removed rather than followed — the same rule the state
// dir enforces for its own files, applied here because an explicit state_dir
// inside a workdir is a supported configuration, so a session may be able to
// plant one. Removing a link never touches what it points at.
func Open(dir, name string, maxBytes int64, keep int) (*File, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if keep <= 0 {
		keep = DefaultKeep
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("log %s: %w", filepath.Join(dir, name), err)
	}
	l := &File{max: maxBytes, keep: keep, path: filepath.Join(dir, name)}
	if err := l.reopen(true); err != nil {
		return nil, err
	}
	return l, nil
}

// Path is the absolute path of the current file.
func (l *File) Path() string { return l.path }

// Install opens NAME in dir and points the standard logger at stderr plus that
// file, returning a func that restores the previous output and closes it. It is
// the whole wiring a long-lived process needs: its own lines keep reaching the
// terminal they always did, and gain a durable, size-bounded home.
func Install(dir, name string) (restore func(), err error) {
	f, err := Open(dir, name, 0, 0)
	if err != nil {
		return nil, err
	}
	prev := log.Writer()
	log.SetOutput(io.MultiWriter(os.Stderr, f))
	return func() {
		log.SetOutput(prev)
		_ = f.Close()
	}, nil
}

// Write appends p, rotating first when the record would take the file past its
// bound. A rotation that fails is reported (once) and the bytes are appended to
// the file that is still open, so a full or read-only directory loses the
// bound, never the line.
func (l *File) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return 0, errors.New("log: file is closed")
	}
	if l.size > 0 && l.size+int64(len(p)) > l.max {
		l.rotate()
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

// Close closes the current file.
func (l *File) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// rotate shifts NAME -> NAME.1 -> NAME.2 ... and starts a fresh NAME, keeping
// at most keep files in total (so keep = 1 means the file is truncated).
// Errors are reported through warnf rather than failing the write: the open
// handle is left usable, appending, and the next rotation attempt tries again.
func (l *File) rotate() {
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
	if l.keep <= 1 {
		if err := os.Remove(l.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			l.warnf("cannot truncate %s (%v)", l.path, err)
		}
		if err := l.reopen(false); err != nil {
			l.warnf("cannot reopen %s (%v)", l.path, err)
		}
		return
	}
	ok := true
	// Oldest first, so a failure part-way does not shred the newest files.
	_ = os.Remove(l.rotated(l.keep - 1))
	for i := l.keep - 2; i >= 1; i-- {
		if err := os.Rename(l.rotated(i), l.rotated(i+1)); err != nil && !errors.Is(err, os.ErrNotExist) {
			ok = false
			l.warnf("cannot rotate %s (%v)", l.rotated(i), err)
			break
		}
	}
	if ok {
		if err := os.Rename(l.path, l.rotated(1)); err != nil && !errors.Is(err, os.ErrNotExist) {
			l.warnf("cannot rotate %s (%v)", l.path, err)
		}
	}
	// Reopen for appending whatever happened: if the rename failed, the old file
	// is still there and the write continues in it.
	if err := l.reopen(false); err != nil {
		l.warnf("cannot reopen %s (%v)", l.path, err)
	}
}

// reopen opens the current file for appending, recording its size. When
// create is true a symlink at the name is removed first (see Open); on a
// rotation the name has just been renamed away, so there is nothing to clear.
func (l *File) reopen(create bool) error {
	if create {
		if fi, err := os.Lstat(l.path); err == nil {
			if fi.Mode()&os.ModeSymlink != 0 {
				if err := os.Remove(l.path); err != nil {
					return fmt.Errorf("log %s: %w", l.path, err)
				}
			} else if fi.IsDir() {
				return fmt.Errorf("log %s: a directory is in the way", l.path)
			}
		}
	}
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("log %s: %w", l.path, err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("log %s: %w", l.path, err)
	}
	l.f = f
	l.size = fi.Size()
	return nil
}

// rotated is the path of the i-th rotated file; i is 1-based, and 0 is the
// current file's own path.
func (l *File) rotated(i int) string {
	if i == 0 {
		return l.path
	}
	return fmt.Sprintf("%s.%d", l.path, i)
}

// warnf reports a rotation problem to a side channel without recursing into
// the logger that owns this writer (a log failure must not log through the
// logger that just failed). Stderr is the same channel the daemon's earlier
// output went to, and it is only reached once per failure streak.
func (l *File) warnf(format string, args ...any) {
	if l.warned {
		return
	}
	l.warned = true
	_, _ = fmt.Fprintf(os.Stderr, "clankerbar: logfile: "+format+"\n", args...)
}

// checkName rejects anything that is not a bare filename, matching the state
// dir's own rule: a caller must not be able to walk out of the directory with
// a name that arrived from somewhere else.
func checkName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("log: %q is not a bare filename", name)
	}
	if filepath.Base(name) != name {
		return fmt.Errorf("log: %q is not a bare filename", name)
	}
	return nil
}
