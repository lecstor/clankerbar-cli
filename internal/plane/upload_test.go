package plane

// CLA-581: the upload half of the plane client, pinned at the wire. The CLI
// tests drive the happy path end to end; these assert the request shapes the
// plane actually sees — the declaration's three scalars, the PUT's deliberate
// lack of an Authorization header, and the refusal rendering that keeps a 401
// from reading as a hash mismatch.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreateUpload_RequestShape(t *testing.T) {
	srv, got := serve(t, http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"assetId\":\"a-1\",\"uploadUrl\":\"https://plane.example/uploads/tok\",\"expiresAt\":\"2026-09-29T09:00:00.000Z\",\"curl\":\"curl ...\"}"}]}}`)

	ticket, err := NewUploadAPI(srv.URL+"/mcp/demo", "k-1").CreateUpload(context.Background(), CreateUploadRequest{
		ContentType: "image/png",
		SizeBytes:   1234,
		SHA256:      strings.Repeat("ab", 32),
		TaskID:      "CB-12",
	})
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	if ticket.AssetID != "a-1" || ticket.UploadURL != "https://plane.example/uploads/tok" {
		t.Errorf("ticket = %+v", ticket)
	}
	if ticket.AlreadyStored {
		t.Error("AlreadyStored = true for a response carrying an upload URL")
	}

	params, _ := got.body["params"].(map[string]any)
	if params["name"] != "create_upload" {
		t.Errorf("tool = %v, want create_upload", params["name"])
	}
	args, _ := params["arguments"].(map[string]any)
	if args["contentType"] != "image/png" || args["sizeBytes"] != float64(1234) || args["sha256"] != strings.Repeat("ab", 32) {
		t.Errorf("arguments = %+v, want the declaration's three scalars", args)
	}
	if args["taskId"] != "CB-12" {
		t.Errorf("taskId = %v, want CB-12", args["taskId"])
	}
}

func TestCreateUpload_OmittedTaskAndAlreadyStored(t *testing.T) {
	srv, got := serve(t, http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"assetId\":\"a-9\",\"status\":\"ready\",\"contentType\":\"image/png\",\"sizeBytes\":3,\"sha256\":\"x\"}"}]}}`)

	ticket, err := NewUploadAPI(srv.URL+"/mcp/demo", "k-1").CreateUpload(context.Background(), CreateUploadRequest{
		ContentType: "image/png",
		SizeBytes:   3,
		SHA256:      "x",
	})
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	if !ticket.AlreadyStored || ticket.AssetID != "a-9" {
		t.Errorf("ticket = %+v, want already-stored with a-9", ticket)
	}
	params, _ := got.body["params"].(map[string]any)
	args, _ := params["arguments"].(map[string]any)
	if _, present := args["taskId"]; present {
		t.Errorf("taskId was sent when none was given: %+v", args)
	}
}

func TestCreateUpload_RefusalIsAnError(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"{\"error\":{\"code\":\"unclaimed_project\",\"message\":\"Uploads require a claimed project\"}}"}]}}`
	srv, _ := serve(t, http.StatusOK, body)

	_, err := NewUploadAPI(srv.URL+"/mcp/demo", "k-1").CreateUpload(context.Background(), CreateUploadRequest{
		ContentType: "image/png",
		SizeBytes:   3,
		SHA256:      "x",
	})
	if err == nil || !strings.Contains(err.Error(), "unclaimed_project") {
		t.Fatalf("err = %v, want the plane's refusal carried through", err)
	}
}

// TestCreateUpload_UrlLessTicketIsNotStored: an answer carrying neither a status
// nor a URL is uninterpretable, and must NOT read as "already stored" — that
// would print a success for bytes the client never sent. AlreadyStored stays
// false, and the empty URL is then refused by PutUpload.
func TestCreateUpload_UrlLessTicketIsNotStored(t *testing.T) {
	srv, _ := serve(t, http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"assetId\":\"a-pending\"}"}]}}`)

	ticket, err := NewUploadAPI(srv.URL+"/mcp/demo", "k-1").CreateUpload(context.Background(), CreateUploadRequest{
		ContentType: "image/png",
		SizeBytes:   3,
		SHA256:      "x",
	})
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	if ticket.AlreadyStored {
		t.Error("AlreadyStored = true for a ticket with neither status:ready nor an upload URL")
	}
	if err := NewUploadAPI(srv.URL+"/mcp/demo", "k-1").PutUpload(context.Background(), ticket.UploadURL, strings.NewReader("x"), 1, "image/png"); err == nil {
		t.Error("PutUpload with the empty URL = nil, want a refusal")
	}
}

// TestCreateUpload_ZeroSizeRelaysThePlaneRefusal: a zero-byte file's size is a
// declaration the plane owns (`invalid_size`), so the client must send it and
// relay the refusal rather than invent a local message that misnames the fields
// the caller does have.
func TestCreateUpload_ZeroSizeRelaysThePlaneRefusal(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"{\"error\":{\"code\":\"invalid_size\",\"message\":\"sizeBytes must be a positive integer - the file's exact size in bytes.\"}}"}]}}`
	srv, got := serve(t, http.StatusOK, body)

	_, err := NewUploadAPI(srv.URL+"/mcp/demo", "k-1").CreateUpload(context.Background(), CreateUploadRequest{
		ContentType: "image/png",
		SizeBytes:   0,
		SHA256:      strings.Repeat("ab", 32),
	})
	if err == nil || !strings.Contains(err.Error(), "invalid_size") {
		t.Fatalf("err = %v, want the plane's invalid_size refusal carried through", err)
	}
	params, _ := got.body["params"].(map[string]any)
	args, _ := params["arguments"].(map[string]any)
	if args["sizeBytes"] != float64(0) {
		t.Errorf("sizeBytes sent = %v, want 0 (the declaration the plane rules on)", args["sizeBytes"])
	}
}

// TestPutUpload_NoKeyAndDeclaredLength: the token in the URL IS the auth, so
// the account key must not ride along; the length is declared so the route can
// refuse a mismatch before receiving a body.
func TestPutUpload_NoKeyAndDeclaredLength(t *testing.T) {
	type seen struct {
		method, contentType, auth string
		length                    int64
		body                      string
	}
	got := &seen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got.method, got.contentType, got.auth = r.Method, r.Header.Get("Content-Type"), r.Header.Get("Authorization")
		got.length, got.body = r.ContentLength, string(raw)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)

	body := "the file's bytes"
	err := NewUploadAPI(srv.URL+"/mcp/demo", "k-1").PutUpload(
		context.Background(), srv.URL+"/uploads/tok-1", strings.NewReader(body), int64(len(body)), "text/html")
	if err != nil {
		t.Fatalf("PutUpload: %v", err)
	}
	if got.method != http.MethodPut {
		t.Errorf("method = %s, want PUT", got.method)
	}
	if got.auth != "" {
		t.Errorf("Authorization = %q — the PUT route takes no key; the URL's token is the auth", got.auth)
	}
	if got.contentType != "text/html" {
		t.Errorf("Content-Type = %q", got.contentType)
	}
	if got.length != int64(len(body)) || got.body != body {
		t.Errorf("length/body = %d/%q, want %d/%q", got.length, got.body, len(body), body)
	}
}

func TestPutUpload_RefusalNamesTheCode(t *testing.T) {
	cases := []struct {
		status int
		body   string
		wants  []string
	}{
		{
			status: http.StatusBadRequest,
			body:   `{"error":{"code":"sha256_mismatch","message":"The uploaded bytes hash to aa, but the declaration said bb."}}`,
			wants:  []string{"HTTP 400", "sha256_mismatch", "declaration said bb"},
		},
		{
			// An auth failure must not be renderable as a mismatch.
			status: http.StatusUnauthorized,
			body:   `{"error":{"code":"unauthorized","message":"Sign in."}}`,
			wants:  []string{"HTTP 401", "unauthorized", "Sign in."},
		},
		{
			status: http.StatusBadGateway,
			body:   "upstream is on fire",
			wants:  []string{"HTTP 502", "upstream is on fire"},
		},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		}))
		err := NewUploadAPI(srv.URL+"/mcp/demo", "k-1").PutUpload(
			context.Background(), srv.URL+"/uploads/tok", strings.NewReader("x"), 1, "image/png")
		srv.Close()
		if err == nil {
			t.Fatalf("status %d: err = nil, want a refusal", tc.status)
		}
		for _, want := range tc.wants {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("status %d: err = %v, want it to carry %q", tc.status, err, want)
			}
		}
	}
}

func TestUploadReviewDeck_RequestShape(t *testing.T) {
	srv, got := serve(t, http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"deck\":{\"id\":\"d-1\",\"sizeBytes\":12},\"reviewUrl\":\"https://clankerbar.com/review/abc\"}"}]}}`)

	url, err := NewUploadAPI(srv.URL+"/mcp/demo", "k-1").UploadReviewDeck(context.Background(), "t-1", "a-1")
	if err != nil {
		t.Fatalf("UploadReviewDeck: %v", err)
	}
	if url != "https://clankerbar.com/review/abc" {
		t.Errorf("reviewURL = %q", url)
	}
	params, _ := got.body["params"].(map[string]any)
	if params["name"] != "upload_review_deck" {
		t.Errorf("tool = %v, want upload_review_deck", params["name"])
	}
	args, _ := params["arguments"].(map[string]any)
	if args["taskId"] != "t-1" || args["assetId"] != "a-1" {
		t.Errorf("arguments = %+v, want the task and its asset", args)
	}
	if _, present := args["html"]; present {
		t.Error("the asset form must not carry inline html")
	}
}

// TestUploadAPI_NotWired: a missing endpoint or key degrades to a named error,
// never a panic, and the not-wired value offers only the upload capability.
func TestUploadAPI_NotWired(t *testing.T) {
	api := NewUploadAPI("", "k")
	if _, err := api.CreateUpload(context.Background(), CreateUploadRequest{ContentType: "image/png", SizeBytes: 1, SHA256: "x"}); err != ErrNotWired {
		t.Errorf("CreateUpload err = %v, want ErrNotWired", err)
	}
	if err := api.PutUpload(context.Background(), "https://x/uploads/t", strings.NewReader("x"), 1, "image/png"); err != ErrNotWired {
		t.Errorf("PutUpload err = %v, want ErrNotWired", err)
	}
	if _, err := api.UploadReviewDeck(context.Background(), "t", "a"); err != ErrNotWired {
		t.Errorf("UploadReviewDeck err = %v, want ErrNotWired", err)
	}
}

type countingRoundTripper struct {
	calls int
	resp  func(*http.Request) (*http.Response, error)
}

func (c *countingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	c.calls++
	if c.resp != nil {
		return c.resp(req)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// TestPutUpload_RefusesUnsafeAndMismatchedURLs verifies PutUpload validates
// uploadURL through secureurl.Origin and pins it to the plane's origin before
// sending any HTTP request, file bytes, or URL capability token.
func TestPutUpload_RefusesUnsafeAndMismatchedURLs(t *testing.T) {
	var hits int
	otherSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(otherSrv.Close)

	cases := []struct {
		name      string
		endpoint  string
		uploadURL string
		wantSub   string
	}{
		{
			name:      "cleartext non-loopback http",
			endpoint:  "https://plane.example/mcp/demo",
			uploadURL: "http://plane.example/uploads/tok",
			wantSub:   "plain http",
		},
		{
			name:      "cloud metadata http",
			endpoint:  "https://plane.example/mcp/demo",
			uploadURL: "http://169.254.169.254/latest/meta-data",
			wantSub:   "plain http",
		},
		{
			name:      "non-http scheme file",
			endpoint:  "https://plane.example/mcp/demo",
			uploadURL: "file:///etc/passwd",
			wantSub:   "no scheme and host",
		},
		{
			name:      "non-http scheme ftp",
			endpoint:  "https://plane.example/mcp/demo",
			uploadURL: "ftp://plane.example/uploads/tok",
			wantSub:   `scheme "ftp"`,
		},
		{
			name:      "mismatched https origin",
			endpoint:  "https://plane.example/mcp/demo",
			uploadURL: "https://evil.example/uploads/tok",
			wantSub:   "does not match plane endpoint origin",
		},
		{
			name:      "mismatched loopback port",
			endpoint:  "http://127.0.0.1:1/mcp/demo",
			uploadURL: otherSrv.URL + "/uploads/tok",
			wantSub:   "does not match plane endpoint origin",
		},
		{
			name:      "https endpoint rejects loopback http upload",
			endpoint:  "https://plane.example/mcp/demo",
			uploadURL: otherSrv.URL + "/uploads/tok",
			wantSub:   "does not match plane endpoint origin",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &countingRoundTripper{}
			r := wiredClient(tc.endpoint, "k-1")
			r.upload.Transport = rt

			err := r.PutUpload(context.Background(), tc.uploadURL, strings.NewReader("secret-bytes"), 12, "image/png")
			if err == nil {
				t.Fatalf("PutUpload(%q) = nil, want refusal", tc.uploadURL)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %v, want substring %q", err, tc.wantSub)
			}
			if rt.calls != 0 {
				t.Errorf("RoundTrip calls = %d, want 0 (no HTTP request must be sent)", rt.calls)
			}
		})
	}
	if hits != 0 {
		t.Errorf("otherSrv received %d requests, want 0", hits)
	}
}

// TestPutUpload_AllowsValidHTTPSAndLoopbackOrigins verifies legitimate HTTPS
// uploads (including normalized default port 443) and local loopback dev URLs
// pass validation and reach the target.
func TestPutUpload_AllowsValidHTTPSAndLoopbackOrigins(t *testing.T) {
	t.Run("https same origin", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPut {
				t.Errorf("method = %s, want PUT", r.Method)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"ok":true}`)
		}))
		t.Cleanup(srv.Close)

		r := wiredClient(srv.URL+"/mcp/demo", "k-1")
		r.upload.Transport = srv.Client().Transport
		if err := r.PutUpload(context.Background(), srv.URL+"/uploads/tok", strings.NewReader("ok"), 2, "image/png"); err != nil {
			t.Fatalf("PutUpload(https): %v", err)
		}
	})

	t.Run("https default port 443 matches bare host", func(t *testing.T) {
		rt := &countingRoundTripper{}
		r := wiredClient("https://plane.example/mcp/demo", "k-1")
		r.upload.Transport = rt
		if err := r.PutUpload(context.Background(), "https://plane.example:443/uploads/tok", strings.NewReader("ok"), 2, "image/png"); err != nil {
			t.Fatalf("PutUpload(https:443): %v", err)
		}
		if rt.calls != 1 {
			t.Errorf("RoundTrip calls = %d, want 1", rt.calls)
		}
	})

	t.Run("loopback http same origin", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(srv.Close)

		if err := NewUploadAPI(srv.URL+"/mcp/demo", "k-1").PutUpload(
			context.Background(), srv.URL+"/uploads/tok", strings.NewReader("ok"), 2, "image/png"); err != nil {
			t.Fatalf("PutUpload(loopback): %v", err)
		}
	})
}

// TestPutUpload_RefusesCrossOriginAndDowngradeRedirects verifies a redirect
// from a valid same-origin upload URL to a different origin or cleartext host
// is refused without sending the body to the redirect target.
func TestPutUpload_RefusesCrossOriginAndDowngradeRedirects(t *testing.T) {
	var targetHits int
	otherSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(otherSrv.Close)

	redirectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/uploads/cross":
			http.Redirect(w, r, otherSrv.URL+"/steal", http.StatusTemporaryRedirect)
		case "/uploads/downgrade":
			http.Redirect(w, r, "http://plane.example/steal", http.StatusTemporaryRedirect)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(redirectSrv.Close)

	api := NewUploadAPI(redirectSrv.URL+"/mcp/demo", "k-1")
	if err := api.PutUpload(context.Background(), redirectSrv.URL+"/uploads/cross", strings.NewReader("secret"), 6, "image/png"); err == nil {
		t.Fatal("cross-origin redirect = nil, want refusal")
	}
	if targetHits != 0 {
		t.Errorf("cross-origin redirect target received %d hits, want 0", targetHits)
	}
	if err := api.PutUpload(context.Background(), redirectSrv.URL+"/uploads/downgrade", strings.NewReader("secret"), 6, "image/png"); err == nil {
		t.Fatal("cleartext downgrade redirect = nil, want refusal")
	}
}
