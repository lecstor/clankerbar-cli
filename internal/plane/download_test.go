package plane

// CLA-1014: the download half of the plane client, pinned at the wire. The CLI
// tests drive the command end to end; these assert the request the plane sees —
// an authenticated GET that asks for the stored bytes verbatim — and the metadata
// the CLI verifies a download against (length, ETag hash, filename).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestDownload_RequestShapeAndStream(t *testing.T) {
	body := []byte("# a markdown document\n")
	sum := sha256.Sum256(body)
	hexSum := hex.EncodeToString(sum[:])

	type seen struct {
		method, path, auth, acceptEncoding string
	}
	got := &seen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path = r.Method, r.URL.Path
		got.auth, got.acceptEncoding = r.Header.Get("Authorization"), r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "text/markdown")
		w.Header().Set("Content-Disposition", `attachment; filename="notes.md"`)
		w.Header().Set("ETag", `"`+hexSum+`"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	stream, err := NewDownloadAPI(srv.URL+"/api/projects/demo/assets/a-1", "k-1").Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer stream.Body.Close()

	if got.method != http.MethodGet {
		t.Errorf("method = %s, want GET", got.method)
	}
	if got.path != "/api/projects/demo/assets/a-1" {
		t.Errorf("path = %q, want the asset route", got.path)
	}
	if got.auth != "Bearer k-1" {
		t.Errorf("Authorization = %q, want the account key as a bearer token", got.auth)
	}
	// Identity encoding is what makes the length and the ETag describe the very
	// bytes this process hashes; a transparently decompressed body would not.
	if got.acceptEncoding != "identity" {
		t.Errorf("Accept-Encoding = %q, want identity", got.acceptEncoding)
	}

	raw, err := io.ReadAll(stream.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(raw) != string(body) {
		t.Errorf("body = %q, want the plane's bytes verbatim", raw)
	}
	if stream.ContentType != "text/markdown" {
		t.Errorf("ContentType = %q", stream.ContentType)
	}
	if stream.ContentLength != int64(len(body)) {
		t.Errorf("ContentLength = %d, want %d", stream.ContentLength, len(body))
	}
	if stream.Filename != "notes.md" {
		t.Errorf("Filename = %q, want the Content-Disposition name", stream.Filename)
	}
	if stream.SHA256 != hexSum {
		t.Errorf("SHA256 = %q, want the ETag's lowercase hash %q", stream.SHA256, hexSum)
	}
}

// TestDownload_RefusalCarriesThePlanesWords: the CLI's job is to show what the
// plane said, not to paraphrase it. The route's auth failures are JSON and its
// one not-found is plain text, so both shapes must survive.
func TestDownload_RefusalCarriesThePlanesWords(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		headers map[string]string
		wants   []string
	}{
		{
			name:   "not found is the route's one plain answer",
			status: http.StatusNotFound,
			body:   "Not found",
			wants:  []string{"HTTP 404", "Not found"},
		},
		{
			name:    "an auth refusal keeps its code",
			status:  http.StatusUnauthorized,
			body:    `{"error":{"code":"unauthorized","message":"Invalid or revoked API key"}}`,
			headers: map[string]string{"Content-Type": "application/json"},
			wants:   []string{"HTTP 401", "unauthorized", "Invalid or revoked API key"},
		},
		{
			name:   "a partial body is not an asset",
			status: http.StatusPartialContent,
			body:   "partial",
			wants:  []string{"HTTP 206", "partial"},
		},
		{
			name:   "an empty refusal still names the status",
			status: http.StatusBadGateway,
			body:   "",
			wants:  []string{"HTTP 502"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(srv.Close)

			stream, err := NewDownloadAPI(srv.URL+"/api/projects/demo/assets/a-1", "k-1").Get(context.Background())
			if err == nil {
				stream.Body.Close()
				t.Fatal("err = nil, want the plane's refusal")
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to carry %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "k-1") {
				t.Errorf("err = %v, leaks the API key", err)
			}
		})
	}
}

func TestDownload_NotWired(t *testing.T) {
	for name, api := range map[string]DownloadAPI{
		"no url": NewDownloadAPI("", "k-1"),
		"no key": NewDownloadAPI("https://plane.example/api/projects/demo/assets/a-1", ""),
	} {
		if _, err := api.Get(context.Background()); !errors.Is(err, ErrNotWired) {
			t.Errorf("%s: err = %v, want ErrNotWired", name, err)
		}
	}
}

// A cleartext asset URL would put the account key on the wire in the open. It is
// refused before any request is made, the same floor every credentialed call has.
func TestDownload_RefusesCleartextNonLoopback(t *testing.T) {
	_, err := NewDownloadAPI("http://plane.example/api/projects/demo/assets/a-1", "k-1").Get(context.Background())
	if err == nil || !strings.Contains(err.Error(), "plain http") {
		t.Fatalf("err = %v, want the cleartext refusal", err)
	}
}

// TestDownload_RefusesCrossOriginRedirect: the bytes are file content and the
// request carries the account key; a redirect must not move either off the
// plane's origin.
func TestDownload_RefusesCrossOriginRedirect(t *testing.T) {
	var targetHits int
	otherSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(otherSrv.Close)

	redirectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, otherSrv.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirectSrv.Close)

	_, err := NewDownloadAPI(redirectSrv.URL+"/api/projects/demo/assets/a-1", "k-1").Get(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refusing redirect") {
		t.Fatalf("err = %v, want the redirect guard's refusal", err)
	}
	if targetHits != 0 {
		t.Errorf("cross-origin redirect target received %d hits, want 0", targetHits)
	}
}

func TestETagSHA256(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	for name, tc := range map[string]struct {
		etag string
		want string
	}{
		"strong quoted": {`"` + hash + `"`, hash},
		"strong bare":   {hash, hash},
		"uppercase":     {`"` + strings.Repeat("AB", 32) + `"`, hash},
		"weak":          {`W/"` + hash + `"`, ""},
		"md5 length":    {`"` + strings.Repeat("ab", 16) + `"`, ""},
		"not hex":       {`"` + strings.Repeat("zz", 32) + `"`, ""},
		"empty":         {"", ""},
		"quoted tag":    {`"v3"`, ""},
	} {
		if got := etagSHA256(tc.etag); got != tc.want {
			t.Errorf("%s: etagSHA256(%q) = %q, want %q", name, tc.etag, got, tc.want)
		}
	}
}

func TestDispositionFilename(t *testing.T) {
	for name, tc := range map[string]struct {
		header string
		want   string
	}{
		"plain":            {`attachment; filename="report.md"`, "report.md"},
		"unquoted":         {`attachment; filename=report.txt`, "report.txt"},
		"rfc5987":          {`attachment; filename*=UTF-8''r%C3%A9sum%C3%A9.pdf`, "résumé.pdf"},
		"inline":           {"inline", ""},
		"empty":            {"", ""},
		"unparseable":      {`attachment; filename="unterminated`, ""},
		"traversal is raw": {`attachment; filename="../../etc/passwd"`, "../../etc/passwd"},
	} {
		if got := dispositionFilename(tc.header); got != tc.want {
			t.Errorf("%s: dispositionFilename(%q) = %q, want %q", name, tc.header, got, tc.want)
		}
	}
}
