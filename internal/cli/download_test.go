package cli

// CLA-1014: `clankerbar download`, driven end to end against a fake plane's
// asset route. The contract under test spans the transport (an authenticated GET
// for the project's asset URL) and the filesystem (stdout is the file, or
// exactly the path written, with nothing partial or misplaced left behind).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// fakeAssetPlane serves the one route a download uses:
// GET /api/projects/<slug>/assets/<assetId>. Everything but the body and the
// status is a header the tests can set, because the CLI's verification and
// naming decisions are driven entirely by what the response says.
type fakeAssetPlane struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []assetRequest

	body        []byte
	contentType string
	// filename is the Content-Disposition name; "" writes no header at all.
	filename string
	// disposition, when non-empty, is the whole Content-Disposition header
	// verbatim. Some forms (an RFC 5987 %0A) cannot be produced by quoting
	// filename as a plain parameter.
	disposition string
	// etag is the whole ETag header (quotes included); "" writes none.
	etag string
	// declaredLength, when >= 0, overrides the Content-Length header. A value
	// larger than the body simulates a truncated transfer.
	declaredLength int
	// noLength omits Content-Length entirely and flushes the header before the
	// body, so the response is really chunked (ContentLength == -1).
	noLength bool

	status     int
	statusBody string
}

type assetRequest struct {
	method, path, auth, acceptEncoding string
}

func newFakeAssetPlane(t *testing.T) *fakeAssetPlane {
	t.Helper()
	p := &fakeAssetPlane{
		body:           []byte("body"),
		contentType:    "text/markdown",
		filename:       "notes.md",
		status:         http.StatusOK,
		declaredLength: -1,
	}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.requests = append(p.requests, assetRequest{
			method:         r.Method,
			path:           r.URL.Path,
			auth:           r.Header.Get("Authorization"),
			acceptEncoding: r.Header.Get("Accept-Encoding"),
		})
		p.mu.Unlock()

		p.mu.Lock()
		body, contentType, filename, disposition, etag := p.body, p.contentType, p.filename, p.disposition, p.etag
		status, statusBody, declared, noLength := p.status, p.statusBody, p.declaredLength, p.noLength
		p.mu.Unlock()

		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, statusBody)
			return
		}
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		if disposition != "" {
			w.Header().Set("Content-Disposition", disposition)
		} else if filename != "" {
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
		}
		length := len(body)
		if declared >= 0 {
			length = declared
		}
		if !noLength {
			w.Header().Set("Content-Length", fmt.Sprint(length))
		}
		if etag != "" {
			w.Header().Set("ETag", etag)
		}
		w.WriteHeader(http.StatusOK)
		if noLength {
			// Flush before writing so net/http cannot infer a length from a
			// small buffered body: the response must really be chunked.
			w.(http.Flusher).Flush()
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// setEtagFromBody makes the plane behave like the real route: a strong ETag
// carrying the sha256 of the stored bytes.
func (p *fakeAssetPlane) setEtagFromBody() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.etag = `"` + sha256Hex(p.body) + `"`
}

func (p *fakeAssetPlane) snapshot() []assetRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]assetRequest(nil), p.requests...)
}

// assetConfig plants the same single-project config the daemon uses; the
// download URL is derived from its origin and slug.
func assetConfig(t *testing.T, p *fakeAssetPlane) string {
	t.Helper()
	return writeUploadConfig(t, p.srv.URL+"/mcp/acme")
}

// TestDownloadStdoutCarriesTheFileAndNothingElse: when -o is omitted the bytes
// are the whole of stdout, so `clankerbar download asset:x > f` produces exactly
// the file. The human line goes to stderr.
func TestDownloadStdoutCarriesTheFileAndNothingElse(t *testing.T) {
	plane := newFakeAssetPlane(t)
	plane.body = []byte("# handoff\n\nread this first\n")
	plane.setEtagFromBody()
	t.Setenv("CLANKERBAR_API_KEY", testKey)
	cfg := assetConfig(t, plane)

	var stdout, stderr bytes.Buffer
	if err := downloadRun(context.Background(), []string{"-c", cfg, "asset-1"}, &stdout, &stderr); err != nil {
		t.Fatalf("download: %v", err)
	}

	if got := stdout.String(); got != string(plane.body) {
		t.Errorf("stdout = %q, want the plane's bytes and nothing else", got)
	}
	if stderr.Len() == 0 {
		t.Error("stderr carries no human line")
	}
	if strings.Contains(stderr.String(), testKey) || strings.Contains(stdout.String(), testKey) {
		t.Error("the API key appears in the command's output")
	}

	reqs := plane.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("requests = %+v, want exactly one", reqs)
	}
	if reqs[0].method != http.MethodGet {
		t.Errorf("method = %q, want GET", reqs[0].method)
	}
	if reqs[0].path != "/api/projects/acme/assets/asset-1" {
		t.Errorf("path = %q, want the project's asset route", reqs[0].path)
	}
	if reqs[0].auth != "Bearer "+testKey {
		t.Errorf("Authorization = %q, want the CLANKERBAR_API_KEY bearer", reqs[0].auth)
	}
	if reqs[0].acceptEncoding != "identity" {
		t.Errorf("Accept-Encoding = %q, want identity (the stored bytes, verified by length and hash)", reqs[0].acceptEncoding)
	}
}

// TestDownloadToFile: -o writes the bytes and stdout carries exactly the path,
// so `FILE=$(clankerbar download asset:x -o dir/)` is usable.
func TestDownloadToFile(t *testing.T) {
	plane := newFakeAssetPlane(t)
	plane.body = []byte("%PDF-1.7 document")
	plane.contentType = "application/pdf"
	plane.filename = "brief.pdf"
	plane.setEtagFromBody()
	t.Setenv("CLANKERBAR_API_KEY", "k")
	cfg := assetConfig(t, plane)
	outDir := t.TempDir()

	t.Run("explicit file path", func(t *testing.T) {
		dest := filepath.Join(outDir, "saved.bin")
		var stdout, stderr bytes.Buffer
		if err := downloadRun(context.Background(), []string{"-c", cfg, "-o", dest, "asset-1"}, &stdout, &stderr); err != nil {
			t.Fatalf("download: %v", err)
		}
		if got := stdout.String(); got != dest+"\n" {
			t.Errorf("stdout = %q, want exactly the written path %q", got, dest)
		}
		raw, err := os.ReadFile(dest)
		if err != nil {
			t.Fatalf("read downloaded file: %v", err)
		}
		if !bytes.Equal(raw, plane.body) {
			t.Errorf("file = %q, want the plane's bytes", raw)
		}
	})

	t.Run("existing directory takes the plane's filename", func(t *testing.T) {
		dir := filepath.Join(outDir, "docs")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if err := downloadRun(context.Background(), []string{"-c", cfg, "--out", dir, "asset-1"}, &stdout, &stderr); err != nil {
			t.Fatalf("download: %v", err)
		}
		want := filepath.Join(dir, "brief.pdf")
		if got := stdout.String(); got != want+"\n" {
			t.Errorf("stdout = %q, want exactly %q", got, want)
		}
		if raw, err := os.ReadFile(want); err != nil || !bytes.Equal(raw, plane.body) {
			t.Errorf("read %s = %q, %v; want the downloaded bytes", want, raw, err)
		}
	})

	t.Run("an existing file is replaced atomically", func(t *testing.T) {
		dest := filepath.Join(outDir, "replace.md")
		if err := os.WriteFile(dest, []byte("old content"), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if err := downloadRun(context.Background(), []string{"-c", cfg, "-o", dest, "asset-1"}, &stdout, &stderr); err != nil {
			t.Fatalf("download: %v", err)
		}
		if raw, _ := os.ReadFile(dest); !bytes.Equal(raw, plane.body) {
			t.Errorf("file = %q, want the replacement bytes", raw)
		}
		// Replacing a file must not widen its permissions: cp preserves mode,
		// and a 0600 destination is often someone's private note.
		if fi, err := os.Stat(dest); err != nil {
			t.Fatalf("stat replaced file: %v", err)
		} else if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("replaced file mode = %o, want the existing 0600 preserved", got)
		}
	})
}

// TestDownloadNameComesFromThePlaneSafely: a Content-Disposition name is a
// suggestion, and never a path. A traversal attempt falls back to the asset id,
// because the response must not choose where a file lands.
func TestDownloadNameComesFromThePlaneSafely(t *testing.T) {
	cases := []struct {
		name        string
		filename    string
		disposition string
		wantSuffix  string
	}{
		{"traversal is refused", "../../etc/passwd", "", "asset-1"},
		{"nested path is refused", "sub/doc.md", "", "asset-1"},
		{"absolute path is refused", "/etc/passwd", "", "asset-1"},
		{"missing header falls back to the id", "", "", "asset-1"},
		{"a plain name is used", "handoff.md", "", "handoff.md"},
		// Go's mime decoder turns a percent-encoded %0A into a real newline, so
		// the suggestion would put a newline in the path printed on stdout.
		{"a control character is refused", "", `attachment; filename*=UTF-8''na%0Ame.md`, "asset-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plane := newFakeAssetPlane(t)
			plane.filename = tc.filename
			plane.disposition = tc.disposition
			plane.setEtagFromBody()
			t.Setenv("CLANKERBAR_API_KEY", "k")
			cfg := assetConfig(t, plane)
			dir := t.TempDir()

			var stdout, stderr bytes.Buffer
			if err := downloadRun(context.Background(), []string{"-c", cfg, "-o", dir, "asset-1"}, &stdout, &stderr); err != nil {
				t.Fatalf("download: %v", err)
			}
			want := filepath.Join(dir, tc.wantSuffix)
			if got := stdout.String(); got != want+"\n" {
				t.Errorf("stdout = %q, want %q", got, want)
			}
			if _, err := os.Stat(want); err != nil {
				t.Errorf("expected file %s: %v", want, err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Errorf("directory holds %d entries, want exactly the downloaded file", len(entries))
			}
			// The traversal target's parent must not have been created anywhere.
			if _, err := os.Stat(filepath.Join(dir, "sub")); !os.IsNotExist(err) {
				t.Errorf("a nested path was created under the download directory: %v", err)
			}
		})
	}
}

// TestDownloadSha256Verification: --sha256 is an integrity gate, not decoration.
// The plane's ETag is checked before anything is written; the streamed bytes are
// checked again, so a plane that omits the header still cannot slip a bad file
// through — and a failure leaves no file at all.
func TestDownloadSha256Verification(t *testing.T) {
	body := []byte("the exact document bytes")
	good := sha256Hex(body)
	bad := strings.Repeat("0", 64)

	t.Run("matching hash succeeds even with no ETag", func(t *testing.T) {
		plane := newFakeAssetPlane(t)
		plane.body = body
		plane.etag = ""
		t.Setenv("CLANKERBAR_API_KEY", "k")
		cfg := assetConfig(t, plane)
		dir := t.TempDir()

		var stdout, stderr bytes.Buffer
		if err := downloadRun(context.Background(), []string{"-c", cfg, "-o", dir, "--sha256", strings.ToUpper(good), "asset-1"}, &stdout, &stderr); err != nil {
			t.Fatalf("download: %v", err)
		}
		if raw, err := os.ReadFile(filepath.Join(dir, "notes.md")); err != nil || !bytes.Equal(raw, body) {
			t.Errorf("file = %q, %v; want the verified bytes", raw, err)
		}
	})

	t.Run("ETag mismatch refuses before writing", func(t *testing.T) {
		plane := newFakeAssetPlane(t)
		plane.body = body
		plane.setEtagFromBody()
		t.Setenv("CLANKERBAR_API_KEY", "k")
		cfg := assetConfig(t, plane)
		dir := t.TempDir()

		var stdout, stderr bytes.Buffer
		err := downloadRun(context.Background(), []string{"-c", cfg, "-o", dir, "--sha256", bad, "asset-1"}, &stdout, &stderr)
		if err == nil {
			t.Fatal("err = nil, want a hash refusal")
		}
		for _, want := range []string{"ETag", good, bad} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want it to name %q", err, want)
			}
		}
		if stdout.Len() != 0 {
			t.Errorf("stdout = %q, want nothing", stdout.String())
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 0 {
			t.Errorf("download directory holds %d entries, want none after a refusal", len(entries))
		}
	})

	t.Run("streamed mismatch without an ETag writes no file", func(t *testing.T) {
		plane := newFakeAssetPlane(t)
		plane.body = body
		plane.etag = ""
		t.Setenv("CLANKERBAR_API_KEY", "k")
		cfg := assetConfig(t, plane)
		dir := t.TempDir()

		var stdout, stderr bytes.Buffer
		err := downloadRun(context.Background(), []string{"-c", cfg, "-o", dir, "--sha256", bad, "asset-1"}, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), "--sha256") || !strings.Contains(err.Error(), "nothing was written") {
			t.Fatalf("err = %v, want the streamed-hash refusal", err)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 0 {
			t.Errorf("download directory holds %d entries, want none (the temp file is removed)", len(entries))
		}
	})

	t.Run("stdout mismatch says the bytes were already written", func(t *testing.T) {
		plane := newFakeAssetPlane(t)
		plane.body = body
		plane.etag = ""
		t.Setenv("CLANKERBAR_API_KEY", "k")
		cfg := assetConfig(t, plane)

		var stdout, stderr bytes.Buffer
		err := downloadRun(context.Background(), []string{"-c", cfg, "--sha256", bad, "asset-1"}, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), "already written to stdout") {
			t.Fatalf("err = %v, want a refusal that says the bytes reached stdout", err)
		}
		if !bytes.Equal(stdout.Bytes(), body) {
			t.Errorf("stdout = %q, want the streamed bytes", stdout.String())
		}
	})

	t.Run("malformed hash is refused before any request", func(t *testing.T) {
		plane := newFakeAssetPlane(t)
		t.Setenv("CLANKERBAR_API_KEY", "k")
		cfg := assetConfig(t, plane)

		for _, bad := range []string{"abc", strings.Repeat("z", 64)} {
			var stdout, stderr bytes.Buffer
			err := downloadRun(context.Background(), []string{"-c", cfg, "--sha256", bad, "asset-1"}, &stdout, &stderr)
			if err == nil || !strings.Contains(err.Error(), "--sha256") {
				t.Fatalf("--sha256 %q: err = %v, want a refusal naming the flag", bad, err)
			}
		}
		if reqs := plane.snapshot(); len(reqs) != 0 {
			t.Errorf("requests = %+v, want none before the flag is valid", reqs)
		}
	})
}

// TestDownloadPlainDownloadVerifiesThePlaneETag: with no --sha256 the plane's
// own strong ETag is still an integrity gate, in both output modes. Every other
// test that sets an ETag sets it to the body's hash, so this is what pins the
// check a plain download relies on: an ETag naming different bytes must refuse.
func TestDownloadPlainDownloadVerifiesThePlaneETag(t *testing.T) {
	body := []byte("the bytes the plane actually sent")
	etag := sha256Hex([]byte("some other asset's bytes"))
	if etag == sha256Hex(body) {
		t.Fatal("test setup: the ETag must not describe the body")
	}

	t.Run("stdout", func(t *testing.T) {
		plane := newFakeAssetPlane(t)
		plane.body = body
		plane.etag = `"` + etag + `"`
		t.Setenv("CLANKERBAR_API_KEY", "k")
		cfg := assetConfig(t, plane)

		var stdout, stderr bytes.Buffer
		err := downloadRun(context.Background(), []string{"-c", cfg, "asset-1"}, &stdout, &stderr)
		if err == nil {
			t.Fatal("err = nil, want a refusal after the bytes arrived")
		}
		for _, want := range []string{"ETag", etag, "already written to stdout"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want it to name %q", err, want)
			}
		}
		if !bytes.Equal(stdout.Bytes(), body) {
			t.Errorf("stdout = %q, want the unverifiable bytes that were already written", stdout.String())
		}
	})

	t.Run("file", func(t *testing.T) {
		plane := newFakeAssetPlane(t)
		plane.body = body
		plane.etag = `"` + etag + `"`
		t.Setenv("CLANKERBAR_API_KEY", "k")
		cfg := assetConfig(t, plane)
		dir := t.TempDir()

		var stdout, stderr bytes.Buffer
		err := downloadRun(context.Background(), []string{"-c", cfg, "-o", dir, "asset-1"}, &stdout, &stderr)
		if err == nil {
			t.Fatal("err = nil, want the ETag refusal")
		}
		for _, want := range []string{"ETag", etag, "nothing was written"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want it to name %q", err, want)
			}
		}
		if stdout.Len() != 0 {
			t.Errorf("stdout = %q, want nothing", stdout.String())
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("download directory holds %d entries, want no destination and no temp file", len(entries))
		}
	})
}

// errReader fails the read after whatever the MultiReader has already yielded,
// standing in for a connection that dies mid-body.
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// TestDownloadStdoutPartialTransportError: a mid-stream transport failure leaves
// the delivered bytes on stdout, and the error must say so rather than read like
// a failed read of nothing.
func TestDownloadStdoutPartialTransportError(t *testing.T) {
	stream := &plane.AssetStream{
		Body: io.NopCloser(io.MultiReader(
			strings.NewReader("half a file"),
			errReader{errors.New("connection reset")},
		)),
		ContentLength: -1,
	}
	var stdout, stderr bytes.Buffer
	err := downloadToStdout("asset-1", stream, "", &stdout, &stderr)
	if err == nil {
		t.Fatal("err = nil, want the transport failure")
	}
	for _, want := range []string{"connection reset", "partial file", fmt.Sprintf("%d bytes", len("half a file"))} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to name %q", err, want)
		}
	}
	if stdout.String() != "half a file" {
		t.Errorf("stdout = %q, want the bytes delivered before the failure", stdout.String())
	}
}

// TestDownloadWithoutADeclaredLength: a chunked response declares no length, so
// the length check must be skipped rather than compared against -1.
func TestDownloadWithoutADeclaredLength(t *testing.T) {
	plane := newFakeAssetPlane(t)
	plane.body = []byte("chunked bytes with no declared length")
	plane.setEtagFromBody()
	plane.noLength = true
	t.Setenv("CLANKERBAR_API_KEY", "k")
	cfg := assetConfig(t, plane)
	dir := t.TempDir()

	var stdout, stderr bytes.Buffer
	if err := downloadRun(context.Background(), []string{"-c", cfg, "-o", dir, "asset-1"}, &stdout, &stderr); err != nil {
		t.Fatalf("download: %v", err)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "notes.md")); err != nil || !bytes.Equal(raw, plane.body) {
		t.Errorf("file = %q, %v; want the downloaded bytes", raw, err)
	}
}

// TestDownloadNameRefusesUnsafeCandidates pins the refusal when neither the
// plane's suggestion nor the asset id can be a bare filename.
func TestDownloadNameRefusesUnsafeCandidates(t *testing.T) {
	if _, err := downloadName("sub/doc.md", "a/b"); err == nil || !strings.Contains(err.Error(), "-o") {
		t.Fatalf("err = %v, want a refusal naming -o", err)
	}
	if got, err := downloadName("sub/doc.md", "asset-1"); err != nil || got != "asset-1" {
		t.Errorf("downloadName = %q, %v; want the asset id fallback", got, err)
	}
}

// TestDownloadRefusesNonRegularDestinations: a device or FIFO is not a file a
// download may write. The -o resolution refuses a device outright, and the write
// path refuses the name it picks inside a directory (the second guard, hit when
// a planted FIFO sits where the plane's filename lands).
func TestDownloadRefusesNonRegularDestinations(t *testing.T) {
	fake := newFakeAssetPlane(t)
	fake.setEtagFromBody()
	t.Setenv("CLANKERBAR_API_KEY", "k")
	cfg := assetConfig(t, fake)

	t.Run("-o is a device", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		err := downloadRun(context.Background(), []string{"-c", cfg, "-o", os.DevNull, "asset-1"}, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v, want a non-regular-file refusal", err)
		}
	})

	t.Run("the chosen name is a device", func(t *testing.T) {
		// Call the write directly: resolveDownloadTarget only inspects -o, so a
		// name inside an existing directory reaches this guard.
		target := downloadTarget{dir: filepath.Dir(os.DevNull), name: filepath.Base(os.DevNull), path: os.DevNull}
		_, err := writeDownloadFile(target, &plane.AssetStream{Body: io.NopCloser(strings.NewReader("x"))}, "")
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v, want the write path's non-regular-file refusal", err)
		}
	})
}

// TestDownloadPlaneErrorsPassThrough: the route's own words reach the operator,
// and a refusal touches neither stdout nor the disk.
func TestDownloadPlaneErrorsPassThrough(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		statusBody string
		wants      []string
	}{
		{
			name:       "not found",
			status:     http.StatusNotFound,
			statusBody: "Not found",
			wants:      []string{"HTTP 404", "Not found"},
		},
		{
			name:       "unauthorized",
			status:     http.StatusUnauthorized,
			statusBody: `{"error":{"code":"unauthorized","message":"Invalid or revoked API key"}}`,
			wants:      []string{"HTTP 401", "unauthorized", "Invalid or revoked API key"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plane := newFakeAssetPlane(t)
			plane.status = tc.status
			plane.statusBody = tc.statusBody
			t.Setenv("CLANKERBAR_API_KEY", testKey)
			cfg := assetConfig(t, plane)
			dir := t.TempDir()

			var stdout, stderr bytes.Buffer
			err := downloadRun(context.Background(), []string{"-c", cfg, "-o", dir, "asset-1"}, &stdout, &stderr)
			if err == nil {
				t.Fatal("err = nil, want the plane's refusal")
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to carry %q", err, want)
				}
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want nothing on a refusal", stdout.String())
			}
			if strings.Contains(err.Error(), testKey) {
				t.Errorf("err = %v, leaks the API key", err)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				t.Errorf("download directory holds %d entries, want none after a refusal", len(entries))
			}
		})
	}
}

// TestDownloadTruncatedBodyWritesNothing: a connection that ends mid-file is a
// failed download, not a short one. The temporary file is removed and the
// destination never appears.
func TestDownloadTruncatedBodyWritesNothing(t *testing.T) {
	plane := newFakeAssetPlane(t)
	plane.body = []byte("only part of this arrives")
	plane.setEtagFromBody()
	plane.declaredLength = len(plane.body) + 100
	t.Setenv("CLANKERBAR_API_KEY", "k")
	cfg := assetConfig(t, plane)
	dir := t.TempDir()

	var stdout, stderr bytes.Buffer
	err := downloadRun(context.Background(), []string{"-c", cfg, "-o", dir, "asset-1"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("err = nil, want a truncated-transfer failure")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("download directory holds %d entries, want none after a truncated transfer", len(entries))
	}
}

// TestDownloadRefusesSymlinkDestinations: writing through a link is how a planted
// name turns a download into a write somewhere else. Both shapes — an explicit
// file path and a filename joined inside a directory — refuse it.
func TestDownloadRefusesSymlinkDestinations(t *testing.T) {
	plane := newFakeAssetPlane(t)
	plane.body = []byte("bytes")
	plane.filename = "notes.md"
	plane.setEtagFromBody()
	t.Setenv("CLANKERBAR_API_KEY", "k")
	cfg := assetConfig(t, plane)

	t.Run("explicit path", func(t *testing.T) {
		dir := t.TempDir()
		outside := filepath.Join(dir, "outside.txt")
		if err := os.WriteFile(outside, []byte("untouched"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link.md")
		if err := os.Symlink(outside, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}

		var stdout, stderr bytes.Buffer
		err := downloadRun(context.Background(), []string{"-c", cfg, "-o", link, "asset-1"}, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("err = %v, want a symlink refusal", err)
		}
		if raw, _ := os.ReadFile(outside); string(raw) != "untouched" {
			t.Errorf("symlink target = %q, want it untouched", raw)
		}
	})

	t.Run("name inside a directory", func(t *testing.T) {
		dir := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside.txt")
		if err := os.WriteFile(outside, []byte("untouched"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(dir, "notes.md")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}

		var stdout, stderr bytes.Buffer
		err := downloadRun(context.Background(), []string{"-c", cfg, "-o", dir, "asset-1"}, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("err = %v, want a symlink refusal", err)
		}
		if raw, _ := os.ReadFile(outside); string(raw) != "untouched" {
			t.Errorf("symlink target = %q, want it untouched", raw)
		}
	})
}

// TestDownloadTargetResolution pins the -o rules: a non-existent path is a file,
// a trailing separator is a missing directory, and a device is not a destination.
func TestDownloadTargetResolution(t *testing.T) {
	t.Run("a missing directory is named as one", func(t *testing.T) {
		dir := t.TempDir()
		plane := newFakeAssetPlane(t)
		t.Setenv("CLANKERBAR_API_KEY", "k")
		cfg := assetConfig(t, plane)

		var stdout, stderr bytes.Buffer
		err := downloadRun(context.Background(), []string{"-c", cfg, "-o", filepath.Join(dir, "missing") + "/", "asset-1"}, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), "not an existing directory") {
			t.Fatalf("err = %v, want a missing-directory refusal", err)
		}
	})

	t.Run("a missing parent directory is refused, not created", func(t *testing.T) {
		dir := t.TempDir()
		plane := newFakeAssetPlane(t)
		t.Setenv("CLANKERBAR_API_KEY", "k")
		cfg := assetConfig(t, plane)
		dest := filepath.Join(dir, "nope", "file.md")

		var stdout, stderr bytes.Buffer
		err := downloadRun(context.Background(), []string{"-c", cfg, "-o", dest, "asset-1"}, &stdout, &stderr)
		if err == nil {
			t.Fatal("err = nil, want an open failure naming the missing parent")
		}
		if _, statErr := os.Stat(filepath.Join(dir, "nope")); !os.IsNotExist(statErr) {
			t.Errorf("the missing parent directory was created")
		}
	})
}

// TestDownloadRequiresTheKeyAndAProject: both refusals happen before any network
// call, and each names the thing that fixes it.
func TestDownloadRequiresTheKeyAndAProject(t *testing.T) {
	plane := newFakeAssetPlane(t)

	t.Run("missing api key", func(t *testing.T) {
		t.Setenv("CLANKERBAR_API_KEY", "")
		cfg := assetConfig(t, plane)
		var stdout, stderr bytes.Buffer
		err := downloadRun(context.Background(), []string{"-c", cfg, "asset-1"}, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), "CLANKERBAR_API_KEY") {
			t.Fatalf("err = %v, want a refusal naming CLANKERBAR_API_KEY", err)
		}
	})

	t.Run("multi-project needs --project", func(t *testing.T) {
		t.Setenv("CLANKERBAR_API_KEY", "k")
		cfgPath := filepath.Join(t.TempDir(), "config.json")
		body := fmt.Sprintf(`{"backlog_url":%q,"projects":[{"slug":"one"},{"slug":"two"}]}`, plane.srv.URL)
		if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		err := downloadRun(context.Background(), []string{"-c", cfgPath, "asset-1"}, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), "--project") {
			t.Fatalf("err = %v, want a refusal naming --project", err)
		}
	})

	if reqs := plane.snapshot(); len(reqs) != 0 {
		t.Errorf("requests = %+v, want none before the target is resolved", reqs)
	}
}

// TestDownloadProjectOverride: --project picks the slug in the asset URL, so a
// user key can read a project the local config does not name.
func TestDownloadProjectOverride(t *testing.T) {
	plane := newFakeAssetPlane(t)
	plane.setEtagFromBody()
	t.Setenv("CLANKERBAR_API_KEY", "k")
	cfg := assetConfig(t, plane)

	var stdout, stderr bytes.Buffer
	if err := downloadRun(context.Background(), []string{"-c", cfg, "--project", "other", "asset-9"}, &stdout, &stderr); err != nil {
		t.Fatalf("download: %v", err)
	}
	reqs := plane.snapshot()
	if len(reqs) != 1 || reqs[0].path != "/api/projects/other/assets/asset-9" {
		t.Errorf("requests = %+v, want /api/projects/other/assets/asset-9", reqs)
	}
}

// TestDownloadAcceptsTheAssetReferenceSpelling: upload prints `asset:<id>`, so
// the copy-pasted reference must work; the route still gets the bare id.
func TestDownloadAcceptsTheAssetReferenceSpelling(t *testing.T) {
	plane := newFakeAssetPlane(t)
	plane.setEtagFromBody()
	t.Setenv("CLANKERBAR_API_KEY", "k")
	cfg := assetConfig(t, plane)

	var stdout, stderr bytes.Buffer
	if err := downloadRun(context.Background(), []string{"-c", cfg, "asset:asset-1"}, &stdout, &stderr); err != nil {
		t.Fatalf("download: %v", err)
	}
	reqs := plane.snapshot()
	if len(reqs) != 1 || reqs[0].path != "/api/projects/acme/assets/asset-1" {
		t.Errorf("requests = %+v, want the bare id in the asset route", reqs)
	}
}

// TestDownloadPositionalIsRequired pins the exactly-one-asset-id rule.
func TestDownloadPositionalIsRequired(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := downloadRun(context.Background(), nil, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "exactly one asset id") {
		t.Fatalf("err = %v, want the exactly-one-asset-id refusal", err)
	}
	stdout.Reset()
	stderr.Reset()
	if err := downloadRun(context.Background(), []string{"a", "b"}, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "exactly one asset id") {
		t.Fatalf("err = %v, want the exactly-one-asset-id refusal", err)
	}
}

func TestParseExpectedSHA256(t *testing.T) {
	sum := sha256.Sum256([]byte("x"))
	hexSum := hex.EncodeToString(sum[:])
	for name, tc := range map[string]struct {
		in      string
		want    string
		wantErr bool
	}{
		"empty":     {"", "", false},
		"lowercase": {hexSum, hexSum, false},
		"uppercase": {strings.ToUpper(hexSum), hexSum, false},
		"padded":    {"  " + hexSum + "\n", hexSum, false},
		"short":     {"abcd", "", true},
		"non-hex":   {strings.Repeat("g", 64), "", true},
	} {
		got, err := parseExpectedSHA256(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: err = nil, want a refusal", name)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: parseExpectedSHA256(%q) = %q, %v; want %q", name, tc.in, got, err, tc.want)
		}
	}
}

func TestIsBareFileName(t *testing.T) {
	for name, want := range map[string]bool{
		"notes.md":               true,
		"a b.txt":                true,
		".hidden":                true,
		"":                       false,
		".":                      false,
		"..":                     false,
		"a/b":                    false,
		`a\b`:                    false,
		"/abs":                   false,
		"na\nme.md":              false,
		"esc\x1b[31m":            false,
		"nul\x00.md":             false,
		strings.Repeat("a", 256): false,
	} {
		if got := isBareFileName(name); got != want {
			t.Errorf("isBareFileName(%q) = %v, want %v", name, got, want)
		}
	}
}
