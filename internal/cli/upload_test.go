package cli

// CLA-581: the byte-free upload commands, driven end to end against a fake
// plane. The fake stands in for both halves of the real one — the MCP endpoint
// that mints the upload URL, and the token-authenticated PUT route that
// receives the bytes — because the contract under test spans both: stdout is
// exactly the reference, the bytes go up from the file (never through a tool
// argument), and every refusal gets a named exit.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakePlane is a stand-in for the control plane's upload half: one MCP route
// posting JSON-RPC tools/call, one PUT route receiving the bytes. Every request
// is recorded in order, so the tests can assert both shapes and sequence.
type fakePlane struct {
	srv *httptest.Server

	mu     sync.Mutex
	events []string
	calls  []fakeMCPCall
	puts   []fakePut

	createUploadBody  string
	createUploadError bool
	deckBody          string
	deckError         bool

	putStatus   int
	putBodyText string
}

type fakeMCPCall struct {
	name string
	args map[string]any
	path string
	auth string
}

type fakePut struct {
	path        string
	contentType string
	auth        string
	length      int64
	body        []byte
}

func newFakePlane(t *testing.T) *fakePlane {
	t.Helper()
	p := &fakePlane{putStatus: http.StatusOK, putBodyText: `{"ok":true}`}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/mcp/"):
			raw, _ := io.ReadAll(r.Body)
			var rpc struct {
				Params struct {
					Name      string         `json:"name"`
					Arguments map[string]any `json:"arguments"`
				} `json:"params"`
			}
			_ = json.Unmarshal(raw, &rpc)

			p.mu.Lock()
			p.events = append(p.events, "mcp:"+rpc.Params.Name)
			p.calls = append(p.calls, fakeMCPCall{name: rpc.Params.Name, args: rpc.Params.Arguments, path: r.URL.Path, auth: r.Header.Get("Authorization")})
			text, isErr := p.createUploadBody, p.createUploadError
			if rpc.Params.Name == "upload_review_deck" {
				text, isErr = p.deckBody, p.deckError
			}
			p.mu.Unlock()

			result := map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
			if isErr {
				result["isError"] = true
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})

		case r.Method == http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			p.mu.Lock()
			p.events = append(p.events, "put:"+r.URL.Path)
			p.puts = append(p.puts, fakePut{
				path:        r.URL.Path,
				contentType: r.Header.Get("Content-Type"),
				auth:        r.Header.Get("Authorization"),
				length:      r.ContentLength,
				body:        body,
			})
			status, text := p.putStatus, p.putBodyText
			p.mu.Unlock()
			w.WriteHeader(status)
			_, _ = io.WriteString(w, text)

		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.srv.Close)

	// Defaults name the fake's own URL, so a ticket is always PUTtable.
	p.createUploadBody = fmt.Sprintf(`{"assetId":"asset-1","uploadUrl":%q,"expiresAt":"2026-09-29T09:00:00.000Z","curl":"curl ..."}`, p.srv.URL+"/uploads/tok-1")
	p.deckBody = `{"deck":{"id":"deck-1","sizeBytes":12},"reviewUrl":"https://clankerbar.com/review/abc","note":"Deck uploaded."}`
	return p
}

func (p *fakePlane) snapshot() ([]string, []fakeMCPCall, []fakePut) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.events...), append([]fakeMCPCall(nil), p.calls...), append([]fakePut(nil), p.puts...)
}

// writeUploadConfig plants a config naming the fake plane's MCP endpoint, the
// shape a single-project daemon config has.
func writeUploadConfig(t *testing.T, endpoint string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"backlog_url":%q}`, endpoint)), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func writeUploadFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// testKey is distinctive enough that a substring check means something: the
// requirement is that the account key appears NOWHERE, success or failure, and
// a one-letter key would make `strings.Contains` trivially true everywhere.
const testKey = "test-key-do-not-print"

// assertNoKey fails when the account key appears in the command's output or in
// its returned error. The happy-path tests pin stdout already; this is the same
// requirement applied to every refusal, where an interpolated key would
// otherwise ship unremarked.
func assertNoKey(t *testing.T, err error, stdout, stderr *bytes.Buffer) {
	t.Helper()
	for _, s := range []string{stdout.String(), stderr.String()} {
		if strings.Contains(s, testKey) {
			t.Errorf("the API key appears in output: %q", s)
		}
	}
	if err != nil && strings.Contains(err.Error(), testKey) {
		t.Errorf("the API key appears in the error: %v", err)
	}
}

// TestUploadStdoutCarriesOnlyTheAssetRef is the compose-in-a-script contract:
// `IMG=$(clankerbar upload shot.png)` must capture the reference and nothing
// else, while the human line goes to stderr.
func TestUploadStdoutCarriesOnlyTheAssetRef(t *testing.T) {
	plane := newFakePlane(t)
	t.Setenv("CLANKERBAR_API_KEY", testKey)
	img := writeUploadFile(t, "shot.png", []byte("\x89PNG\r\n\x1a\n not really a png"))
	cfg := writeUploadConfig(t, plane.srv.URL+"/mcp/acme")

	var stdout, stderr bytes.Buffer
	if err := uploadRun(context.Background(), []string{"-c", cfg, img}, &stdout, &stderr); err != nil {
		t.Fatalf("upload: %v", err)
	}

	if got, want := stdout.String(), "asset:asset-1\n"; got != want {
		t.Errorf("stdout = %q, want exactly %q", got, want)
	}
	if stderr.Len() == 0 {
		t.Error("stderr carries no human line")
	}
	if strings.Contains(stderr.String(), testKey) || strings.Contains(stdout.String(), testKey) {
		t.Error("the API key appears in the command's output")
	}

	events, calls, puts := plane.snapshot()
	if len(calls) != 1 || calls[0].name != "create_upload" {
		t.Fatalf("MCP calls = %+v, want exactly one create_upload", calls)
	}
	if calls[0].path != "/mcp/acme" {
		t.Errorf("MCP path = %q, want the config's /mcp/acme", calls[0].path)
	}
	if calls[0].auth != "Bearer "+testKey {
		t.Errorf("MCP Authorization = %q, want the CLANKERBAR_API_KEY bearer", calls[0].auth)
	}
	if got := calls[0].args["contentType"]; got != "image/png" {
		t.Errorf("declared contentType = %v, want image/png", got)
	}
	if got := calls[0].args["sizeBytes"]; got != float64(len("\x89PNG\r\n\x1a\n not really a png")) {
		t.Errorf("declared sizeBytes = %v", got)
	}
	if got := calls[0].args["sha256"]; got != sha256Hex([]byte("\x89PNG\r\n\x1a\n not really a png")) {
		t.Errorf("declared sha256 = %v", got)
	}
	if _, present := calls[0].args["taskId"]; present {
		t.Error("taskId sent without --task")
	}

	if len(puts) != 1 {
		t.Fatalf("PUTs = %+v, want exactly one", puts)
	}
	if puts[0].path != "/uploads/tok-1" {
		t.Errorf("PUT path = %q, want the ticket's URL", puts[0].path)
	}
	if got := string(puts[0].body); got != "\x89PNG\r\n\x1a\n not really a png" {
		t.Errorf("PUT body = %q, want the file's bytes verbatim", got)
	}
	if puts[0].contentType != "image/png" {
		t.Errorf("PUT Content-Type = %q", puts[0].contentType)
	}
	if puts[0].length != int64(len(puts[0].body)) {
		t.Errorf("PUT Content-Length = %d, want %d", puts[0].length, len(puts[0].body))
	}
	// The URL's token IS the auth; the account key must not travel to a route
	// that deliberately accepts no other credential.
	if puts[0].auth != "" {
		t.Errorf("PUT carried Authorization %q, want none", puts[0].auth)
	}

	if len(events) != 2 || events[0] != "mcp:create_upload" || events[1] != "put:/uploads/tok-1" {
		t.Errorf("event order = %v, want declaration before PUT", events)
	}
}

func TestContentTypeFor(t *testing.T) {
	for name, want := range map[string]string{
		"shot.png":     "image/png",
		"photo.jpg":    "image/jpeg",
		"photo.JPEG":   "image/jpeg",
		"anim.gif":     "image/gif",
		"drawing.webp": "image/webp",
		"icon.svg":     "image/svg+xml",
		"clip.mp4":     "video/mp4",
		"clip.webm":    "video/webm",
		"deck.html":    "text/html",
		"deck.htm":     "text/html",
	} {
		got, ok := contentTypeFor(name)
		if !ok || got != want {
			t.Errorf("contentTypeFor(%q) = %q, %v; want %q, true", name, got, ok, want)
		}
	}
	if got, ok := contentTypeFor("archive.xyz"); ok {
		t.Errorf("contentTypeFor(archive.xyz) = %q, want unknown", got)
	}
	if got, ok := contentTypeFor("noextension"); ok {
		t.Errorf("contentTypeFor(noextension) = %q, want unknown", got)
	}
}

// TestUploadTypeOverride: --type is the escape hatch for an extension the table
// does not know, and it must reach both the declaration and the PUT.
func TestUploadTypeOverride(t *testing.T) {
	plane := newFakePlane(t)
	t.Setenv("CLANKERBAR_API_KEY", "k")
	blob := writeUploadFile(t, "blob.bin", []byte("bytes"))
	cfg := writeUploadConfig(t, plane.srv.URL+"/mcp/acme")

	var stdout, stderr bytes.Buffer
	if err := uploadRun(context.Background(), []string{"-c", cfg, "--type", "image/webp", blob}, &stdout, &stderr); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if stdout.String() != "asset:asset-1\n" {
		t.Errorf("stdout = %q", stdout.String())
	}
	_, calls, puts := plane.snapshot()
	if calls[0].args["contentType"] != "image/webp" {
		t.Errorf("declared contentType = %v, want the --type override", calls[0].args["contentType"])
	}
	if puts[0].contentType != "image/webp" {
		t.Errorf("PUT Content-Type = %q, want the --type override", puts[0].contentType)
	}
}

// TestUploadTaskAttribution: --task rides create_upload as attribution, and is
// absent when not given.
func TestUploadTaskAttribution(t *testing.T) {
	plane := newFakePlane(t)
	t.Setenv("CLANKERBAR_API_KEY", "k")
	img := writeUploadFile(t, "shot.png", []byte("png"))
	cfg := writeUploadConfig(t, plane.srv.URL+"/mcp/acme")

	var stdout, stderr bytes.Buffer
	if err := uploadRun(context.Background(), []string{"-c", cfg, "--task", "CB-12", img}, &stdout, &stderr); err != nil {
		t.Fatalf("upload: %v", err)
	}
	_, calls, _ := plane.snapshot()
	if calls[0].args["taskId"] != "CB-12" {
		t.Errorf("taskId = %v, want CB-12", calls[0].args["taskId"])
	}
}

// TestUploadProjectOverride: --project picks the slug the MCP endpoint scopes
// to, so the command can target a project the local config does not name.
func TestUploadProjectOverride(t *testing.T) {
	plane := newFakePlane(t)
	t.Setenv("CLANKERBAR_API_KEY", "k")
	img := writeUploadFile(t, "shot.png", []byte("png"))
	cfg := writeUploadConfig(t, plane.srv.URL+"/mcp/acme")

	var stdout, stderr bytes.Buffer
	if err := uploadRun(context.Background(), []string{"-c", cfg, "--project", "other", img}, &stdout, &stderr); err != nil {
		t.Fatalf("upload: %v", err)
	}
	_, calls, _ := plane.snapshot()
	if calls[0].path != "/mcp/other" {
		t.Errorf("MCP path = %q, want /mcp/other", calls[0].path)
	}
}

// TestUploadAlreadyStoredSkipsThePut: identical bytes are one asset, so the
// plane answers with a ready asset and no URL — no PUT, same stdout reference.
func TestUploadAlreadyStoredSkipsThePut(t *testing.T) {
	plane := newFakePlane(t)
	plane.createUploadBody = `{"assetId":"asset-9","status":"ready","contentType":"image/png","sizeBytes":3,"sha256":"abc","note":"already stored"}`
	t.Setenv("CLANKERBAR_API_KEY", "k")
	img := writeUploadFile(t, "shot.png", []byte("png"))
	cfg := writeUploadConfig(t, plane.srv.URL+"/mcp/acme")

	var stdout, stderr bytes.Buffer
	if err := uploadRun(context.Background(), []string{"-c", cfg, img}, &stdout, &stderr); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if stdout.String() != "asset:asset-9\n" {
		t.Errorf("stdout = %q, want the stored asset's reference", stdout.String())
	}
	events, _, puts := plane.snapshot()
	if len(puts) != 0 {
		t.Errorf("PUTs = %+v, want none for already-stored bytes", puts)
	}
	if len(events) != 1 || events[0] != "mcp:create_upload" {
		t.Errorf("events = %v, want the declaration and nothing else", events)
	}
}

// TestUploadUrlLessTicketIsNotASuccess: a declaration answer carrying neither a
// status nor a URL is uninterpretable. Reading it as already-stored would print
// `asset:<id>` and exit 0 for bytes that were never sent; it must fail instead,
// with nothing on stdout.
func TestUploadUrlLessTicketIsNotASuccess(t *testing.T) {
	plane := newFakePlane(t)
	plane.createUploadBody = `{"assetId":"asset-pending"}`
	t.Setenv("CLANKERBAR_API_KEY", testKey)
	img := writeUploadFile(t, "shot.png", []byte("png"))
	cfg := writeUploadConfig(t, plane.srv.URL+"/mcp/acme")

	var stdout, stderr bytes.Buffer
	err := uploadRun(context.Background(), []string{"-c", cfg, img}, &stdout, &stderr)
	if err == nil {
		t.Fatal("err = nil, want a refusal for a ticket with no upload URL")
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", stdout.String())
	}
	assertNoKey(t, err, &stdout, &stderr)
	if _, _, puts := plane.snapshot(); len(puts) != 0 {
		t.Errorf("PUTs = %+v, want none (there is no URL to PUT to)", puts)
	}
}

// TestUploadEmptyFileRelaysThePlaneRefusal: a zero-byte file is a declaration
// the plane rules on (invalid_size); the CLI sends it and relays the plane's
// words instead of inventing a local refusal that misnames the fields.
func TestUploadEmptyFileRelaysThePlaneRefusal(t *testing.T) {
	plane := newFakePlane(t)
	plane.createUploadError = true
	plane.createUploadBody = `{"error":{"code":"invalid_size","message":"sizeBytes must be a positive integer - the file's exact size in bytes."}}`
	t.Setenv("CLANKERBAR_API_KEY", testKey)
	empty := writeUploadFile(t, "empty.png", nil)
	cfg := writeUploadConfig(t, plane.srv.URL+"/mcp/acme")

	var stdout, stderr bytes.Buffer
	err := uploadRun(context.Background(), []string{"-c", cfg, empty}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "invalid_size") {
		t.Fatalf("err = %v, want the plane's invalid_size refusal carried through", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", stdout.String())
	}
	assertNoKey(t, err, &stdout, &stderr)
	_, calls, _ := plane.snapshot()
	if len(calls) != 1 || calls[0].name != "create_upload" {
		t.Fatalf("MCP calls = %+v, want exactly one create_upload", calls)
	}
	if calls[0].args["sizeBytes"] != float64(0) {
		t.Errorf("declared sizeBytes = %v, want 0 sent to the plane", calls[0].args["sizeBytes"])
	}
}

// Each error exit gets a named reason on stderr (via main's log.Fatal) and,
// where the failure precedes any network call, must not touch the plane.
func TestUploadErrorExits(t *testing.T) {
	plane := newFakePlane(t)
	t.Setenv("CLANKERBAR_API_KEY", testKey)
	cfg := writeUploadConfig(t, plane.srv.URL+"/mcp/acme")
	png := writeUploadFile(t, "shot.png", []byte("png"))
	unknown := writeUploadFile(t, "archive.xyz", []byte("?"))
	dirAsFile := filepath.Join(t.TempDir(), "dir.png")
	if err := os.Mkdir(dirAsFile, 0o700); err != nil {
		t.Fatal(err)
	}

	t.Run("unreadable file", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		err := uploadRun(context.Background(), []string{"-c", cfg, filepath.Join(t.TempDir(), "gone.png")}, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), "gone.png") {
			t.Fatalf("err = %v, want one naming the missing file", err)
		}
		if stdout.Len() != 0 {
			t.Errorf("stdout = %q, want nothing", stdout.String())
		}
		assertNoKey(t, err, &stdout, &stderr)
	})

	t.Run("directory is not a file", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		err := uploadRun(context.Background(), []string{"-c", cfg, dirAsFile}, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v, want a not-a-file refusal", err)
		}
		assertNoKey(t, err, &stdout, &stderr)
	})

	t.Run("unknown type without --type", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		err := uploadRun(context.Background(), []string{"-c", cfg, unknown}, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), "--type") {
			t.Fatalf("err = %v, want a refusal naming --type", err)
		}
		assertNoKey(t, err, &stdout, &stderr)
	})

	t.Run("missing file beats the unknown extension", func(t *testing.T) {
		// The file is unreadable AND untypeable; the reason must be the one
		// --type cannot fix.
		var stdout, stderr bytes.Buffer
		missing := filepath.Join(t.TempDir(), "gone.xyz")
		err := uploadRun(context.Background(), []string{"-c", cfg, missing}, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), "gone.xyz") {
			t.Fatalf("err = %v, want one naming the missing file", err)
		}
		if strings.Contains(err.Error(), "pass --type") {
			t.Errorf("err = %v, want the unreadable file blamed, not the extension", err)
		}
		assertNoKey(t, err, &stdout, &stderr)
	})

	t.Run("missing api key", func(t *testing.T) {
		t.Setenv("CLANKERBAR_API_KEY", "")
		var stdout, stderr bytes.Buffer
		err := uploadRun(context.Background(), []string{"-c", cfg, png}, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), "CLANKERBAR_API_KEY") {
			t.Fatalf("err = %v, want a refusal naming CLANKERBAR_API_KEY", err)
		}
		assertNoKey(t, err, &stdout, &stderr)
	})

	// Every subtest above failed before the plane was touched.
	if events, _, _ := plane.snapshot(); len(events) != 0 {
		t.Errorf("the plane was called on an error exit: %v", events)
	}
}

// TestUploadPlaneRefusalPassesThrough: the plane owns the allowlist and the
// caps, so its refusal text is what the operator sees, not a paraphrase.
func TestUploadPlaneRefusalPassesThrough(t *testing.T) {
	plane := newFakePlane(t)
	plane.createUploadError = true
	plane.createUploadBody = `{"error":{"code":"unsupported_content_type","message":"Content type 'application/pdf' is not allowed. Allowed: image/png, text/html."}}`
	t.Setenv("CLANKERBAR_API_KEY", testKey)
	doc := writeUploadFile(t, "doc.pdf", []byte("%PDF"))
	cfg := writeUploadConfig(t, plane.srv.URL+"/mcp/acme")

	var stdout, stderr bytes.Buffer
	err := uploadRun(context.Background(), []string{"-c", cfg, "--type", "application/pdf", doc}, &stdout, &stderr)
	if err == nil {
		t.Fatal("err = nil, want the plane's refusal")
	}
	for _, want := range []string{"unsupported_content_type", "Content type 'application/pdf' is not allowed."} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to carry %q verbatim", err, want)
		}
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing on a refused upload", stdout.String())
	}
	assertNoKey(t, err, &stdout, &stderr)
	_, _, puts := plane.snapshot()
	if len(puts) != 0 {
		t.Errorf("PUTs = %+v, want none after a refused declaration", puts)
	}
}

// TestUploadPutRefusalNamesItsCause: the PUT route's code and message are
// carried through, so a hash mismatch, an expired token and an auth failure
// cannot be mistaken for one another.
func TestUploadPutRefusalNamesItsCause(t *testing.T) {
	plane := newFakePlane(t)
	t.Setenv("CLANKERBAR_API_KEY", testKey)
	img := writeUploadFile(t, "shot.png", []byte("png"))
	cfg := writeUploadConfig(t, plane.srv.URL+"/mcp/acme")

	cases := []struct {
		name   string
		status int
		body   string
		wants  []string
	}{
		{
			name:   "hash mismatch",
			status: http.StatusBadRequest,
			body:   `{"error":{"code":"sha256_mismatch","message":"The uploaded bytes hash to aa, but the declaration said bb. Nothing was stored."}}`,
			wants:  []string{"sha256_mismatch", "Nothing was stored."},
		},
		{
			name:   "expired token",
			status: http.StatusBadRequest,
			body:   `{"error":{"code":"invalid_token","message":"This upload URL is not valid any more — tokens are single-use and expire after 15 minutes."}}`,
			wants:  []string{"invalid_token", "expire after 15 minutes"},
		},
		{
			name:   "auth failure is not a mismatch",
			status: http.StatusUnauthorized,
			body:   `{"error":{"code":"unauthorized","message":"Sign in."}}`,
			wants:  []string{"HTTP 401", "unauthorized"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plane.mu.Lock()
			plane.putStatus = tc.status
			plane.putBodyText = tc.body
			plane.events = nil
			plane.mu.Unlock()

			var stdout, stderr bytes.Buffer
			err := uploadRun(context.Background(), []string{"-c", cfg, img}, &stdout, &stderr)
			if err == nil {
				t.Fatal("err = nil, want the PUT refusal")
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to carry %q", err, want)
				}
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want nothing on a failed PUT", stdout.String())
			}
			assertNoKey(t, err, &stdout, &stderr)
		})
	}
}

// TestUploadMultiProjectNeedsProject: a config that names several projects has
// no single answer, and the refusal names the flag that fixes it.
func TestUploadMultiProjectNeedsProject(t *testing.T) {
	plane := newFakePlane(t)
	t.Setenv("CLANKERBAR_API_KEY", testKey)
	img := writeUploadFile(t, "shot.png", []byte("png"))
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	body := fmt.Sprintf(`{"backlog_url":%q,"projects":[{"slug":"one"},{"slug":"two"}]}`, plane.srv.URL)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	err := uploadRun(context.Background(), []string{"-c", cfgPath, img}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "--project") {
		t.Fatalf("err = %v, want a refusal naming --project", err)
	}
	assertNoKey(t, err, &stdout, &stderr)
	if events, _, _ := plane.snapshot(); len(events) != 0 {
		t.Errorf("the plane was called without a project: %v", events)
	}
}

// TestDeckPrintsTheReviewURL: deck uploads the HTML as an asset, binds it to
// the task, and prints exactly the review URL for scripts to capture.
func TestDeckPrintsTheReviewURL(t *testing.T) {
	plane := newFakePlane(t)
	t.Setenv("CLANKERBAR_API_KEY", testKey)
	html := []byte("<!doctype html><title>deck</title>")
	deck := writeUploadFile(t, "deck.html", html)
	cfg := writeUploadConfig(t, plane.srv.URL+"/mcp/acme")

	var stdout, stderr bytes.Buffer
	if err := deckRun(context.Background(), []string{"-c", cfg, "--task", "CB-581", deck}, &stdout, &stderr); err != nil {
		t.Fatalf("deck: %v", err)
	}
	if got, want := stdout.String(), "https://clankerbar.com/review/abc\n"; got != want {
		t.Errorf("stdout = %q, want exactly the review URL %q", got, want)
	}
	if strings.Contains(stdout.String(), testKey) {
		t.Error("the API key appears in stdout")
	}
	assertNoKey(t, nil, &stdout, &stderr)

	events, calls, puts := plane.snapshot()
	if len(calls) != 2 {
		t.Fatalf("MCP calls = %+v, want create_upload then upload_review_deck", calls)
	}
	if calls[0].name != "create_upload" || calls[0].args["contentType"] != "text/html" || calls[0].args["taskId"] != "CB-581" {
		t.Errorf("create_upload = %+v, want text/html declared with task CB-581", calls[0])
	}
	if calls[1].name != "upload_review_deck" {
		t.Fatalf("second call = %q, want upload_review_deck", calls[1].name)
	}
	if calls[1].args["taskId"] != "CB-581" || calls[1].args["assetId"] != "asset-1" {
		t.Errorf("upload_review_deck args = %+v, want the task and the uploaded asset", calls[1].args)
	}
	if len(puts) != 1 || string(puts[0].body) != string(html) {
		t.Errorf("PUTs = %+v, want the deck's bytes", puts)
	}
	wantEvents := []string{"mcp:create_upload", "put:/uploads/tok-1", "mcp:upload_review_deck"}
	if strings.Join(events, ",") != strings.Join(wantEvents, ",") {
		t.Errorf("events = %v, want %v", events, wantEvents)
	}
}

func TestDeckErrorExits(t *testing.T) {
	plane := newFakePlane(t)
	t.Setenv("CLANKERBAR_API_KEY", testKey)
	cfg := writeUploadConfig(t, plane.srv.URL+"/mcp/acme")
	deck := writeUploadFile(t, "deck.html", []byte("<html></html>"))
	png := writeUploadFile(t, "shot.png", []byte("png"))

	t.Run("task is required", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		err := deckRun(context.Background(), []string{"-c", cfg, deck}, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), "--task") {
			t.Fatalf("err = %v, want a refusal naming --task", err)
		}
		assertNoKey(t, err, &stdout, &stderr)
	})

	t.Run("non-HTML file refused before any call", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		err := deckRun(context.Background(), []string{"-c", cfg, "--task", "CB-1", png}, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), "HTML") {
			t.Fatalf("err = %v, want a refusal naming HTML", err)
		}
		assertNoKey(t, err, &stdout, &stderr)
	})

	t.Run("missing file beats the HTML check", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		missing := filepath.Join(t.TempDir(), "gone.png")
		err := deckRun(context.Background(), []string{"-c", cfg, "--task", "CB-1", missing}, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), "gone.png") {
			t.Fatalf("err = %v, want one naming the missing file", err)
		}
		// The sentinel is the refusal's own phrase, not "HTML": the temp path
		// carries the subtest's name, which contains it.
		if strings.Contains(err.Error(), "deck expects") {
			t.Errorf("err = %v, want the unreadable file blamed, not the extension", err)
		}
		assertNoKey(t, err, &stdout, &stderr)
	})

	if events, _, _ := plane.snapshot(); len(events) != 0 {
		t.Errorf("the plane was called on an error exit: %v", events)
	}
}

// TestHashFileDeclaresTheBytes: the declaration is only as good as the hash and
// size, and they must be of the file as it sits on disk.
func TestHashFileDeclaresTheBytes(t *testing.T) {
	data := []byte("the exact bytes, nothing else")
	path := writeUploadFile(t, "blob.png", data)
	sha, size, err := hashFile(path)
	if err != nil {
		t.Fatalf("hashFile: %v", err)
	}
	if sha != sha256Hex(data) {
		t.Errorf("sha = %s, want %s", sha, sha256Hex(data))
	}
	if size != int64(len(data)) {
		t.Errorf("size = %d, want %d", size, len(data))
	}
}

// TestUploadPositionalIsRequired pins the "exactly one file" rule.
func TestUploadPositionalIsRequired(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := uploadRun(context.Background(), nil, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "exactly one file") {
		t.Fatalf("err = %v, want the exactly-one-file refusal", err)
	}
	var stdout2, stderr2 bytes.Buffer
	err = uploadRun(context.Background(), []string{"a.png", "b.png"}, &stdout2, &stderr2)
	if err == nil || !strings.Contains(err.Error(), "exactly one file") {
		t.Fatalf("err = %v, want the exactly-one-file refusal", err)
	}
}
