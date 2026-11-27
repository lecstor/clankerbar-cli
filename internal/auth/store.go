package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/secureurl"
)

// maxStoreBytes caps one read. The store is a handful of short entries; the cap
// keeps a fifo, /dev/zero or an endless file from being pulled into memory and
// echoed by an error.
const maxStoreBytes = 1 << 20

// canonicalOrigin is the map key the store uses: scheme://host with the default
// port and trailing dot dropped.
func canonicalOrigin(origin string) (string, error) {
	o, err := secureurl.Origin(strings.TrimSpace(origin))
	if err != nil {
		return "", err
	}
	return o, nil
}

// Load reads the store from its resolved path. A missing file is an empty store,
// not an error: not being logged in is the starting state.
func Load() (Store, error) {
	path, enabled, err := Path()
	if err != nil {
		return Store{}, err
	}
	if !enabled {
		return emptyStore(), nil
	}
	return loadFile(path)
}

func emptyStore() Store {
	return Store{Version: FileVersion, Hosts: map[string]Credential{}}
}

// loadFile reads and vets one credential file. It refuses a symlink, a
// non-regular file, a file another user owns, and a file group or other can
// read — each with the reason and, where relevant, the chmod that fixes it.
func loadFile(path string) (Store, error) {
	before, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return emptyStore(), nil
		}
		return Store{}, fmt.Errorf("%s: %w", path, err)
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return Store{}, fmt.Errorf("%s is a symlink - refusing to read credentials through one (delete the link and run `clankerbar login` again)", path)
	}
	if !before.Mode().IsRegular() {
		return Store{}, fmt.Errorf("%s is not a regular file", path)
	}
	if err := checkParent(path); err != nil {
		return Store{}, err
	}
	if err := checkSecureFile(path, before); err != nil {
		return Store{}, err
	}

	f, err := os.Open(path)
	if err != nil {
		return Store{}, fmt.Errorf("%s: %w", path, err)
	}
	defer f.Close()
	// The Lstat above judged the path; Stat judges what was actually opened. If
	// they differ, something replaced the file in the window between the two.
	after, err := f.Stat()
	if err != nil {
		return Store{}, fmt.Errorf("%s: %w", path, err)
	}
	if !os.SameFile(before, after) {
		return Store{}, fmt.Errorf("%s was replaced while it was being opened - refusing to read it", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxStoreBytes+1))
	if err != nil {
		return Store{}, fmt.Errorf("%s: %w", path, err)
	}
	if len(data) > maxStoreBytes {
		return Store{}, fmt.Errorf("%s is larger than %d bytes - not a credential store", path, maxStoreBytes)
	}
	var st Store
	if err := json.Unmarshal(data, &st); err != nil {
		return Store{}, fmt.Errorf("%s: not a credential store: %w", path, err)
	}
	if st.Version > FileVersion {
		return Store{}, fmt.Errorf("%s is version %d, and this build understands up to %d - upgrade clankerbar or remove the file and log in again", path, st.Version, FileVersion)
	}
	if st.Hosts == nil {
		st.Hosts = map[string]Credential{}
	}
	return st, nil
}

// Lookup returns the stored credential for origin and the file it came from.
func Lookup(origin string) (Credential, string, bool, error) {
	norm, err := canonicalOrigin(origin)
	if err != nil {
		return Credential{}, "", false, err
	}
	path, enabled, err := Path()
	if err != nil {
		return Credential{}, "", false, err
	}
	if !enabled {
		return Credential{}, "", false, nil
	}
	st, err := loadFile(path)
	if err != nil {
		return Credential{}, "", false, err
	}
	cred, ok := findHost(st, norm)
	return cred, path, ok, nil
}

// findHost matches the canonical origin case-insensitively. Origin host names
// are case-insensitive, and a store written by an older build (or by hand) must
// not duplicate an entry just because it capitalised the host differently.
func findHost(st Store, origin string) (Credential, bool) {
	if c, ok := st.Hosts[origin]; ok && strings.TrimSpace(c.Token) != "" {
		return c, true
	}
	for key, c := range st.Hosts {
		if strings.EqualFold(key, origin) && strings.TrimSpace(c.Token) != "" {
			return c, true
		}
	}
	return Credential{}, false
}

// Save stores cred under origin, creating the file (0700 directory, 0600 file)
// or replacing it atomically. It refuses when the store is disabled.
func Save(origin string, cred Credential) error {
	norm, err := canonicalOrigin(origin)
	if err != nil {
		return err
	}
	cred.Token = strings.TrimSpace(cred.Token)
	if cred.Token == "" {
		return errors.New("auth: refusing to store an empty token")
	}
	switch cred.Scope {
	case "", ScopeUser, ScopeProject:
	default:
		return fmt.Errorf("auth: %q is not a known credential scope (%q or %q)", cred.Scope, ScopeUser, ScopeProject)
	}
	if cred.SavedAt.IsZero() {
		cred.SavedAt = time.Now().UTC()
	}
	path, enabled, err := Path()
	if err != nil {
		return err
	}
	if !enabled {
		return fmt.Errorf("%w - unset %s (or point it at a real path) and run `clankerbar login` again", ErrStoreDisabled, EnvAuthFile)
	}
	st, err := loadFile(path)
	if err != nil {
		return err
	}
	for key := range st.Hosts {
		if strings.EqualFold(key, norm) {
			delete(st.Hosts, key)
		}
	}
	st.Version = FileVersion
	st.Hosts[norm] = cred
	return writeFile(path, st)
}

// Remove deletes one origin's entry. It reports whether anything was removed, so
// `logout` is idempotent and can say which case it was.
func Remove(origin string) (bool, error) {
	norm, err := canonicalOrigin(origin)
	if err != nil {
		return false, err
	}
	path, enabled, err := Path()
	if err != nil {
		return false, err
	}
	if !enabled {
		return false, nil
	}
	st, err := loadFile(path)
	if err != nil {
		return false, err
	}
	removed := false
	for key := range st.Hosts {
		if strings.EqualFold(key, norm) {
			delete(st.Hosts, key)
			removed = true
		}
	}
	if !removed {
		return false, nil
	}
	if len(st.Hosts) == 0 {
		return true, removeFile(path)
	}
	return true, writeFile(path, st)
}

// RemoveAll deletes the whole credential file. It reports how many entries were
// dropped, so an operator can tell `logout --all` from a no-op.
func RemoveAll() (int, error) {
	path, enabled, err := Path()
	if err != nil {
		return 0, err
	}
	if !enabled {
		return 0, nil
	}
	before, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return 0, fmt.Errorf("%s is a symlink - refusing to remove anything through one", path)
	}
	if !before.Mode().IsRegular() {
		return 0, fmt.Errorf("%s is not a regular file - refusing to remove it", path)
	}
	st, err := loadFile(path)
	if err != nil {
		return 0, err
	}
	n := len(st.Hosts)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return n, fmt.Errorf("%s: %w", path, err)
	}
	return n, nil
}

// removeFile drops the store once its last entry is gone, so "logged out" leaves
// no empty file behind.
func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// writeFile replaces path with st atomically: a 0600 temp file in the same
// directory, then a rename. A same-directory rename is atomic, so a concurrent
// `whoami` sees either the old document or the new one, never a half write.
func writeFile(path string, st Store) error {
	dir := filepath.Dir(path)
	if err := ensureDir(dir); err != nil {
		return err
	}
	if err := checkParent(path); err != nil {
		return err
	}
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink - refusing to write credentials through one", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s: %w", path, err)
	}

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the credential store: %w", err)
	}
	data = append(data, '\n')

	f, err := os.CreateTemp(dir, ".auth-*.tmp")
	if err != nil {
		return fmt.Errorf("create a temporary credential file in %s: %w", dir, err)
	}
	tmp := f.Name()
	abort := func(cause error) error {
		_ = f.Close()
		_ = os.Remove(tmp)
		return cause
	}
	// CreateTemp already uses 0600; setting it explicitly means a platform whose
	// default differs cannot quietly widen it.
	if err := f.Chmod(0o600); err != nil {
		return abort(fmt.Errorf("set %s to mode 0600: %w", tmp, err))
	}
	if _, err := f.Write(data); err != nil {
		return abort(fmt.Errorf("write %s: %w", tmp, err))
	}
	if err := f.Sync(); err != nil {
		return abort(fmt.Errorf("sync %s: %w", tmp, err))
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// ensureDir creates dir 0700, refusing a symlink at any component it walks. The
// components ABOVE the deepest existing ancestor are trusted, exactly as
// statedir draws the line: ~/.local is a symlink on plenty of machines, and
// refusing that layout would refuse the default store.
func ensureDir(dir string) error {
	var missing []string
	cur := dir
	for {
		fi, err := os.Lstat(cur)
		if err == nil {
			if fi.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%s is a symlink - refusing to keep credentials under one", cur)
			}
			if !fi.IsDir() {
				return fmt.Errorf("%s is not a directory", cur)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s: %w", cur, err)
		}
		missing = append(missing, cur)
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	for i := len(missing) - 1; i >= 0; i-- {
		name := missing[i]
		if err := os.Mkdir(name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create %s: %w", name, err)
		}
		fi, err := os.Lstat(name)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			return fmt.Errorf("%s was not a real directory after creating it - refusing to keep credentials there", name)
		}
	}
	return nil
}

// checkParent refuses a store whose directory is itself a symlink, or is
// writable by anyone but its owner: whoever can write the directory can replace
// the 0600 file whatever its own mode says. A sticky directory (a shared temp
// root) is allowed for the same reason config allows one.
func checkParent(path string) error {
	dir := filepath.Dir(path)
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("%s: %w", dir, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink - refusing to read credentials reached through one", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if !permissionBitsAreMeaningful {
		return nil
	}
	if m := fi.Mode(); m.Perm()&0o022 != 0 && m&os.ModeSticky == 0 {
		return fmt.Errorf("%s is writable by group or other (mode %04o) - anyone who can write the directory can replace the credential: chmod go-w %s", dir, m.Perm(), dir)
	}
	return nil
}

// checkSecureFile is the owner-only rule: no group/other bits, and owned by the
// user running this process.
func checkSecureFile(path string, fi os.FileInfo) error {
	if !permissionBitsAreMeaningful {
		return nil
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s is readable by group or other (mode %04o) - an API key must be owner-only: chmod 600 %s", path, perm, path)
	}
	if uid, ok := fileOwnerUID(fi); ok {
		if me := os.Geteuid(); uid != me {
			return fmt.Errorf("%s is owned by uid %d, not by you (uid %d) - its owner decides its contents whatever its mode says", path, uid, me)
		}
	}
	return nil
}
