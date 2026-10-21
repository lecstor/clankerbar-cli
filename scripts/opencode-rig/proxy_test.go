package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProxyForwardsAndRecordsBodyWithoutAuthorization(t *testing.T) {
	const secret = "provider-key-that-must-not-be-recorded"
	var gotPath, gotQuery, gotAuth string
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotAuth = r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: {\"chunk\":1}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	up, err := url.Parse(upstream.URL + "/provider/v1")
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	proxy, err := newRecordingProxy(up, dir)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(proxy)
	defer front.Close()

	body := []byte(`{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/chat/completions?trace=1", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	// The proxy forwards faithfully: same path suffix, query, auth and body.
	if gotPath != "/provider/v1/chat/completions" {
		t.Errorf("upstream path = %q", gotPath)
	}
	if gotQuery != "trace=1" {
		t.Errorf("upstream query = %q", gotQuery)
	}
	if gotAuth != "Bearer "+secret {
		t.Errorf("upstream Authorization = %q (the proxy must forward it)", gotAuth)
	}
	if !bytes.Equal(gotBody, body) {
		t.Errorf("upstream body = %s", gotBody)
	}
	if string(got) != "data: {\"chunk\":1}\n\ndata: [DONE]\n\n" {
		t.Errorf("client response = %q", got)
	}

	// The record: request body present, Authorization absent everywhere.
	requests, err := readRequests(filepath.Join(dir, "requests"))
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("recorded %d requests, want 1", len(requests))
	}
	r := requests[0]
	if r.Path != "/chat/completions" || r.Method != http.MethodPost {
		t.Errorf("recorded %s %s", r.Method, r.Path)
	}
	if !strings.Contains(string(r.Body), `"stream":true`) {
		t.Errorf("recorded body = %s", r.Body)
	}
	if _, ok := r.Headers["Authorization"]; ok {
		t.Error("recorded headers carry Authorization")
	}
	if !r.AuthOmit {
		t.Error("record does not state that Authorization was omitted")
	}
	assertNoSecretOnDisk(t, dir, secret)

	// The response is recorded too, so a streamed completion can be inspected.
	respFiles, _ := filepath.Glob(filepath.Join(dir, "responses", "*.sse"))
	if len(respFiles) != 1 {
		t.Fatalf("recorded %d response files, want 1", len(respFiles))
	}
	respBody, _ := os.ReadFile(respFiles[0])
	if !strings.Contains(string(respBody), "[DONE]") {
		t.Errorf("recorded response = %s", respBody)
	}
}

// TestProxyDoesNotRecordAnInboundAuthorizationHeaderUnderAnyName is the
// negative-space test: every byte written under the run directory is scanned,
// not just the request envelope.
func TestProxyDoesNotRecordAnInboundAuthorizationHeaderUnderAnyName(t *testing.T) {
	const secret = "sk-live-oc-rig-secret"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	up, _ := url.Parse(upstream.URL)
	dir := t.TempDir()
	proxy, err := newRecordingProxy(up, dir)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(proxy)
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Proxy-Authorization", "Bearer "+secret)
	req.Header.Set("Cookie", "session="+secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	assertNoSecretOnDisk(t, dir, secret)
}

func TestProxyPassesThroughUpstreamErrors(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"boom"}`, http.StatusTooManyRequests)
	}))
	defer upstream.Close()
	up, _ := url.Parse(upstream.URL)
	dir := t.TempDir()
	proxy, err := newRecordingProxy(up, dir)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(proxy)
	defer front.Close()

	resp, err := http.Post(front.URL+"/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", resp.StatusCode)
	}
}

// assertNoSecretOnDisk walks every file the proxy wrote and fails if the secret
// appears in any of them.
func assertNoSecretOnDisk(t *testing.T, dir, secret string) {
	t.Helper()
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(secret)) {
			t.Errorf("%s contains the provider secret", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestJoinURLPath(t *testing.T) {
	cases := []struct{ base, path, want string }{
		{"/provider/v1", "/chat/completions", "/provider/v1/chat/completions"},
		{"/provider/v1/", "/chat/completions", "/provider/v1/chat/completions"},
		{"", "/chat/completions", "/chat/completions"},
		{"/provider/v1", "", "/provider/v1"},
	}
	for _, c := range cases {
		if got := joinURLPath(c.base, c.path); got != c.want {
			t.Errorf("joinURLPath(%q,%q) = %q, want %q", c.base, c.path, got, c.want)
		}
	}
}

func TestSafeName(t *testing.T) {
	if got := safeName("/chat/completions"); got != "chat_completions" {
		t.Errorf("safeName = %q", got)
	}
	if got := safeName("/"); got != "root" {
		t.Errorf("safeName(root) = %q", got)
	}
}

// TestRecordedRequestIsValidJSON pins the artifact format the report reads.
func TestRecordedRequestIsValidJSON(t *testing.T) {
	dir := t.TempDir()
	p, err := newRecordingProxy(&url.URL{Scheme: "http", Host: "example.invalid"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(`{"model":"m"}`))
	if err := p.recordRequest(7, req, []byte(`{"model":"m"}`)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "requests", "0007-chat_completions.json"))
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("recorded envelope is not valid JSON: %v\n%s", err, data)
	}
	if env["body"].(map[string]any)["model"] != "m" {
		t.Errorf("body = %v", env["body"])
	}
}
