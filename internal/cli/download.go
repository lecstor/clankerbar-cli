package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/spf13/pflag"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// download (CLA-1014): the read half of the byte-free path upload opened. An
// agent (or a human) runs `clankerbar download <assetId>` and gets the file back
// on stdout or on disk, streamed by this process, so the bytes never cross a
// model or a tool argument.
//
// The stdout contract mirrors upload's: when the bytes go to stdout, stdout
// carries the file and NOTHING else (the human line goes to stderr). When they
// go to a file, stdout carries exactly the path written, so
// `FILE=$(clankerbar download asset:... -o docs/)` yields a path to read.

type downloadFlags struct {
	cfgPath string
	project string
	out     string
	sha256  string
}

func newDownloadFlagSet(f *downloadFlags) *pflag.FlagSet {
	fs := newFlagSet("download")
	fs.StringVarP(&f.cfgPath, "config", "c", "", "config file naming the plane and project (default: ~/.config/clankerbar/config.json)")
	fs.StringVar(&f.project, "project", "", "clankerbar project slug (default: the config's project)")
	fs.StringVarP(&f.out, "out", "o", "", "write the bytes to this path instead of stdout; an existing directory writes the plane's filename inside it, and stdout then carries the path written")
	fs.StringVar(&f.sha256, "sha256", "", "verify the bytes hash to this 64-character hex sha256; a mismatch refuses the download")
	return fs
}

// Download is `clankerbar download <assetId>`: fetch an uploaded asset, stream
// it to stdout (the default) or a file.
func Download(ctx context.Context, args []string) error {
	return downloadRun(ctx, args, os.Stdout, os.Stderr)
}

func downloadRun(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	var f downloadFlags
	fs := newDownloadFlagSet(&f)
	assetID, err := parseOnePositional(fs, args, "clankerbar download needs exactly one asset id: download <assetId> [-o <path>] [--project <slug>] [--sha256 <hex>]")
	if err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil
		}
		return err
	}
	assetID = strings.TrimSpace(assetID)
	// `upload` prints `asset:<id>` and that is the reference pasted into prose, so
	// accept it verbatim; the route takes the bare id.
	assetID = strings.TrimPrefix(assetID, "asset:")
	if assetID == "" {
		return errors.New("clankerbar download needs an asset id: download <assetId> [-o <path>]")
	}

	expected, err := parseExpectedSHA256(f.sha256)
	if err != nil {
		return err
	}

	api, err := downloadAPI(f.cfgPath, f.project, assetID)
	if err != nil {
		return err
	}
	stream, err := api.Get(ctx)
	if err != nil {
		return fmt.Errorf("download asset %q: %w", assetID, err)
	}
	defer stream.Body.Close()

	// The plane's ETag is the hash of the stored bytes. Comparing it with the
	// caller's expectation BEFORE writing anything turns a wrong id, a stale
	// reference or a proxy substitution into a refusal with an empty output,
	// rather than a file whose hash only fails once it is already on disk.
	if expected != "" && stream.SHA256 != "" && stream.SHA256 != expected {
		return fmt.Errorf("download asset %q: the plane's ETag says sha256 %s, but --sha256 expects %s - refusing to write bytes that are not the ones requested", assetID, stream.SHA256, expected)
	}

	if f.out == "" {
		return downloadToStdout(assetID, stream, expected, stdout, stderr)
	}
	return downloadToFile(assetID, stream, expected, f.out, stdout, stderr)
}

// downloadToStdout streams the asset straight to stdout, hashing as it goes when
// there is something to verify against. A verification failure can only be
// reported AFTER the bytes have been emitted — stdout cannot be unwritten — and
// the error says so, so a caller does not mistake a printed file for a good one.
func downloadToStdout(assetID string, stream *plane.AssetStream, expected string, stdout, stderr io.Writer) error {
	w := io.Writer(stdout)
	var hasher hash.Hash
	if expected != "" || stream.SHA256 != "" {
		hasher = sha256.New()
		w = io.MultiWriter(stdout, hasher)
	}
	n, err := io.Copy(w, stream.Body)
	if err != nil {
		if n > 0 {
			return fmt.Errorf("download asset %q: %w - %d bytes were already written to stdout, so stdout holds a partial file", assetID, err, n)
		}
		return fmt.Errorf("download asset %q: %w", assetID, err)
	}
	if err := integrityError(stream, hasher, expected, n); err != nil {
		return fmt.Errorf("download asset %q: %w - the bytes were already written to stdout, so they are not trustworthy", assetID, err)
	}
	fmt.Fprintf(stderr, "downloaded asset:%s (%s, %d bytes) to stdout\n", assetID, contentTypeOrUnknown(stream.ContentType), n)
	return nil
}

// downloadToFile writes the asset to the path -o names, through an [os.Root]
// scoped to the destination directory: nothing this process creates can resolve
// outside it, and a symlink at the destination is refused rather than followed.
//
// The bytes go to a same-directory temporary file and are renamed into place only
// after every check passes, so a truncated transfer or a hash mismatch leaves no
// file that looks like a completed download.
func downloadToFile(assetID string, stream *plane.AssetStream, expected, out string, stdout, stderr io.Writer) error {
	target, err := resolveDownloadTarget(out, stream.Filename, assetID)
	if err != nil {
		return err
	}
	n, err := writeDownloadFile(target, stream, expected)
	if err != nil {
		return fmt.Errorf("download asset %q: %w", assetID, err)
	}
	fmt.Fprintln(stdout, target.path)
	fmt.Fprintf(stderr, "downloaded asset:%s (%s, %d bytes) to %s\n", assetID, contentTypeOrUnknown(stream.ContentType), n, target.path)
	return nil
}

// downloadTarget is a resolved file destination: dir is pinned as an os.Root and
// name is a bare filename inside it.
type downloadTarget struct {
	dir  string
	name string
	path string
}

// resolveDownloadTarget decides where a file-mode download lands. out is the
// user's -o verbatim; serverName is the plane's Content-Disposition suggestion,
// treated as a suggestion and never as a path.
//
// A directory gets the plane's filename (sanitised to a bare name). Any other
// path is written exactly as given. A symlink AT the destination is refused:
// Stat would follow it and hide the very link a planted name uses to redirect a
// download, so the Lstat check is deliberate.
func resolveDownloadTarget(out, serverName, assetID string) (downloadTarget, error) {
	if fi, err := os.Stat(out); err == nil && fi.IsDir() {
		name, err := downloadName(serverName, assetID)
		if err != nil {
			return downloadTarget{}, err
		}
		return newDownloadTarget(out, name), nil
	}
	if fi, err := os.Lstat(out); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return downloadTarget{}, fmt.Errorf("%q is a symlink - refusing to write through it; pass the real file path, or an existing directory to name the file from the plane", out)
	}
	if fi, err := os.Stat(out); err == nil && !fi.Mode().IsRegular() {
		return downloadTarget{}, fmt.Errorf("%q is not a regular file", out)
	}
	dir, name := filepath.Split(out)
	if dir == "" {
		dir = "."
	}
	if name == "" {
		return downloadTarget{}, fmt.Errorf("%q ends in a path separator but is not an existing directory", out)
	}
	if name == "." || name == ".." || !isBareFileName(name) {
		return downloadTarget{}, fmt.Errorf("%q is not a file path - pass a file name, or an existing directory to write the plane's filename into", out)
	}
	return newDownloadTarget(dir, name), nil
}

func newDownloadTarget(dir, name string) downloadTarget {
	return downloadTarget{dir: dir, name: name, path: filepath.Join(dir, name)}
}

// downloadName picks the bare filename a directory download uses: the plane's
// filename when it is safe as one, otherwise the asset id. A server-chosen name
// must never become a path segment.
func downloadName(serverName, assetID string) (string, error) {
	for _, candidate := range []string{serverName, assetID} {
		if candidate != "" && isBareFileName(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("cannot name the downloaded file safely from the plane's %q or asset id %q - pass -o <file>", serverName, assetID)
}

// isBareFileName reports whether name is a single path component this CLI is
// willing to create. Both separators are rejected regardless of platform: a
// response is not allowed to choose a path, and a name that means one thing on
// Linux must not mean something else on Windows. Control characters are refused
// too: a filename is printed and logged as one line, and Go's mime decoder will
// turn a percent-encoded %0A into a real newline.
func isBareFileName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	if filepath.IsAbs(name) || filepath.Base(name) != name {
		return false
	}
	// NAME_MAX is 255 on the filesystems this runs on, and the plane already
	// bounds its suggestions to 120. A longer name is refused rather than
	// silently truncated into a different file.
	return len(name) <= 255
}

// writeDownloadFile streams the body into a same-directory temporary file, then
// renames it to target.name once every check has passed. Returns the bytes
// written.
func writeDownloadFile(target downloadTarget, stream *plane.AssetStream, expected string) (int64, error) {
	root, err := os.OpenRoot(target.dir)
	if err != nil {
		return 0, fileError(target.dir, err)
	}
	defer root.Close()

	// Replacing an existing file preserves its permissions the way cp does; a
	// new file takes the usual 0644 less the umask. The in-flight temp starts
	// 0600 when it will replace something, so a partial download is never more
	// readable than the destination it is about to become.
	var destPerm os.FileMode
	exists := false
	if fi, err := root.Lstat(target.name); err == nil {
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			return 0, fmt.Errorf("%s is a symlink - refusing to write through it; remove it or choose another -o path", target.path)
		case fi.IsDir():
			return 0, fmt.Errorf("%s is a directory", target.path)
		case !fi.Mode().IsRegular():
			return 0, fmt.Errorf("%s is not a regular file", target.path)
		}
		destPerm = fi.Mode().Perm()
		exists = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, fileError(target.path, err)
	}

	tmp, err := tempDownloadName()
	if err != nil {
		return 0, err
	}
	createMode := os.FileMode(0o644)
	if exists {
		createMode = 0o600
	}
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, createMode)
	if err != nil {
		return 0, fileError(filepath.Join(target.dir, tmp), err)
	}
	discard := func() {
		f.Close()
		root.Remove(tmp)
	}
	if exists {
		if err := f.Chmod(destPerm); err != nil {
			discard()
			return 0, fileError(filepath.Join(target.dir, tmp), err)
		}
	}

	w := io.Writer(f)
	var hasher hash.Hash
	if expected != "" || stream.SHA256 != "" {
		hasher = sha256.New()
		w = io.MultiWriter(f, hasher)
	}
	n, err := io.Copy(w, stream.Body)
	if err != nil {
		discard()
		return 0, err
	}
	if err := f.Close(); err != nil {
		root.Remove(tmp)
		return 0, fileError(filepath.Join(target.dir, tmp), err)
	}
	if err := integrityError(stream, hasher, expected, n); err != nil {
		root.Remove(tmp)
		return 0, fmt.Errorf("%w (nothing was written)", err)
	}
	if err := root.Rename(tmp, target.name); err != nil {
		root.Remove(tmp)
		return 0, fileError(target.path, err)
	}
	return n, nil
}

// tempDownloadName is a fresh dot-file name for the in-flight half of a download.
// It never derives from the destination, so a long or hostile destination name
// cannot push it over NAME_MAX.
func tempDownloadName() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("download: generate a temporary name: %w", err)
	}
	return ".clankerbar-download-" + hex.EncodeToString(b[:]), nil
}

// integrityError compares what arrived with what was promised: the declared
// length first (a truncated body is a transport failure, not a hash problem), then
// the caller's --sha256, then the plane's own ETag. hasher is nil when there was
// nothing to verify against.
func integrityError(stream *plane.AssetStream, hasher hash.Hash, expected string, written int64) error {
	if stream.ContentLength >= 0 && written != stream.ContentLength {
		return fmt.Errorf("the plane declared %d bytes but %d arrived", stream.ContentLength, written)
	}
	if hasher == nil {
		return nil
	}
	got := hex.EncodeToString(hasher.Sum(nil))
	if expected != "" && got != expected {
		return fmt.Errorf("the received bytes hash to %s, but --sha256 expects %s", got, expected)
	}
	if stream.SHA256 != "" && got != stream.SHA256 {
		return fmt.Errorf("the received bytes hash to %s, but the plane's ETag says %s", got, stream.SHA256)
	}
	return nil
}

// parseExpectedSHA256 normalises --sha256: empty means "no expected hash",
// otherwise it must be exactly 64 hex characters. Case is normalised so an
// uppercase hash from a checksum list verifies rather than failing a comparison
// that has nothing to do with the bytes.
func parseExpectedSHA256(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if len(raw) != 64 {
		return "", fmt.Errorf("--sha256 must be the 64-character hex sha256 to verify - %q has %d characters", raw, len(raw))
	}
	sum := strings.ToLower(raw)
	if _, err := hex.DecodeString(sum); err != nil {
		return "", fmt.Errorf("--sha256 %q is not hexadecimal: %w", raw, err)
	}
	return sum, nil
}

// contentTypeOrUnknown keeps the human line honest when a response carries no
// Content-Type at all.
func contentTypeOrUnknown(contentType string) string {
	if contentType == "" {
		return "unknown type"
	}
	return contentType
}

// downloadAPI resolves the config, the target project and the account key, and
// returns the download surface pointed at the asset's URL on the configured
// origin. It relies on commandTarget for the slug and the origin: pickSlug never
// returns an empty slug with a nil error, and Validate pins a non-empty
// CredentialOrigin, so a resolved target always has a URL.
func downloadAPI(cfgPath, project, assetID string) (plane.DownloadAPI, error) {
	key, cfg, slug, err := commandTarget(cfgPath, project, "--project")
	if err != nil {
		return nil, err
	}
	url := cfg.ProjectAssetURL(config.Project{Slug: slug}, assetID)
	return plane.NewDownloadAPI(url, key), nil
}
