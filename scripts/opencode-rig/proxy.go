package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// recordingProxy is an HTTP pass-through in front of the real provider. It
// records every request body (and the response as received) into the run
// directory, then forwards the request unchanged apart from hop-by-hop headers.
//
// Two rules are load-bearing:
//
//   - The Authorization header is NEVER written to disk. It is forwarded to the
//     provider and omitted from the recorded headers; the record states that it
//     was omitted so a reader knows the absence is deliberate.
//   - The response is streamed, and flushed per chunk. A provider answering SSE
//     must reach opencode as it arrives, or the session stalls until the whole
//     completion is buffered - which changes the timing under test.
//
// The listener is created before the session starts (port 0: a free port, never
// a hard-coded one), so "wait until the proxy is listening" is a fact rather
// than a sleep.
type recordingProxy struct {
	upstream *url.URL
	dir      string
	client   *http.Client
	seq      atomic.Int64
}

// secretHeaders are never recorded: an Authorization value is the provider key,
// and a cookie/Proxy-Authorization is the same class of thing.
var secretHeaders = map[string]bool{
	"Authorization":       true,
	"Proxy-Authorization": true,
	"Cookie":              true,
	"Set-Cookie":          true,
}

func newRecordingProxy(upstream *url.URL, dir string) (*recordingProxy, error) {
	if upstream == nil || upstream.Scheme == "" || upstream.Host == "" {
		return nil, fmt.Errorf("proxy: need an absolute upstream URL")
	}
	for _, sub := range []string{"requests", "responses"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	return &recordingProxy{
		upstream: upstream,
		dir:      dir,
		client: &http.Client{
			// No overall timeout: a model completion can legitimately run for
			// minutes. The rig's own deadline kills the session (and the
			// request with it).
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConnsPerHost:   4,
				IdleConnTimeout:       90 * time.Second,
				ResponseHeaderTimeout: 5 * time.Minute,
			},
		},
	}, nil
}

// listen binds 127.0.0.1:0 and returns the listener and its base URL. Binding
// port 0 is the only correct way to pick a port: a fixed port can be occupied
// by a stray local server that would then receive requests carrying the
// provider key.
func listen() (net.Listener, string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	return ln, "http://" + ln.Addr().String(), nil
}

func (p *recordingProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/__rig/health" {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "ok")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
	if err != nil {
		http.Error(w, "rig proxy: request body: "+err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	seq := int(p.seq.Add(1))

	// Record before forwarding: if the provider hangs or dies, the exact bytes
	// the client sent are still on disk.
	if err := p.recordRequest(seq, r, body); err != nil {
		fmt.Fprintf(os.Stderr, "opencode-rig proxy: recording request %d: %v\n", seq, err)
	}

	target := *p.upstream
	target.Path = joinURLPath(p.upstream.Path, r.URL.Path)
	target.RawQuery = r.URL.RawQuery
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, "rig proxy: "+err.Error(), http.StatusBadGateway)
		return
	}
	copyHeaders(req.Header, r.Header)
	// Ask for an uncompressed response so the response log is readable. The
	// recorded request headers still show exactly what opencode sent.
	req.Header.Del("Accept-Encoding")
	req.Host = target.Host
	req.ContentLength = int64(len(body))

	resp, err := p.client.Do(req)
	if err != nil {
		p.recordProxyError(seq, err)
		http.Error(w, "rig proxy: upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	rec, _ := os.Create(p.responsePath(seq, resp.Header.Get("Content-Type")))
	if rec != nil {
		defer rec.Close()
	}
	// Flush per chunk so SSE stays live. The tee writes the raw upstream bytes
	// to the response log and the same bytes to the client.
	dst := io.Writer(w)
	if rec != nil {
		dst = io.MultiWriter(w, rec)
	}
	buf := make([]byte, 32<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
			flush(w)
		}
		if rerr == io.EOF {
			return
		}
		if rerr != nil {
			return
		}
	}
}

// recordRequest writes one request envelope. The body is embedded as the exact
// bytes the client sent (never re-indented or re-marshalled), so the reported
// size and hash are the wire values and a schema difference is visible byte for
// byte. The Authorization header is omitted, and the record says so.
func (p *recordingProxy) recordRequest(seq int, r *http.Request, body []byte) error {
	headers := map[string]string{}
	for k, v := range r.Header {
		if secretHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		headers[k] = strings.Join(v, ", ")
	}
	var b bytes.Buffer
	b.WriteString("{\n")
	fmt.Fprintf(&b, "  \"seq\": %d,\n", seq)
	at, _ := json.Marshal(time.Now().UTC().Format(time.RFC3339Nano))
	fmt.Fprintf(&b, "  \"at\": %s,\n", at)
	method, _ := json.Marshal(r.Method)
	fmt.Fprintf(&b, "  \"method\": %s,\n", method)
	path, _ := json.Marshal(r.URL.Path)
	fmt.Fprintf(&b, "  \"path\": %s,\n", path)
	if r.URL.RawQuery != "" {
		q, _ := json.Marshal(r.URL.RawQuery)
		fmt.Fprintf(&b, "  \"query\": %s,\n", q)
	}
	h, _ := json.Marshal(headers)
	fmt.Fprintf(&b, "  \"headers\": %s,\n", h)
	fmt.Fprintf(&b, "  \"authorizationOmitted\": true,\n")
	fmt.Fprintf(&b, "  \"bodyBytes\": %d,\n", len(body))
	b.WriteString("  \"body\": ")
	if len(body) > 0 && json.Valid(body) {
		b.Write(bytes.TrimSpace(body))
	} else {
		s, _ := json.Marshal(string(body))
		b.Write(s)
	}
	b.WriteString("\n}\n")
	return os.WriteFile(p.requestPath(seq, r.URL.Path), b.Bytes(), 0o644)
}

func (p *recordingProxy) requestPath(seq int, path string) string {
	return filepath.Join(p.dir, "requests", fmt.Sprintf("%04d-%s.json", seq, safeName(path)))
}

func (p *recordingProxy) responsePath(seq int, contentType string) string {
	ext := ".txt"
	if strings.Contains(contentType, "json") {
		ext = ".json"
	} else if strings.Contains(contentType, "event-stream") {
		ext = ".sse"
	}
	return filepath.Join(p.dir, "responses", fmt.Sprintf("%04d%s", seq, ext))
}

func (p *recordingProxy) recordProxyError(seq int, err error) {
	_ = os.WriteFile(filepath.Join(p.dir, "responses", fmt.Sprintf("%04d-error.txt", seq)),
		[]byte(err.Error()+"\n"), 0o644)
}

// safeName turns a URL path into a file-name fragment.
func safeName(path string) string {
	path = strings.Trim(path, "/")
	if path == "" {
		return "root"
	}
	repl := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "?", "_", "*", "_")
	return truncate(repl.Replace(path), 80)
}

// joinURLPath joins a base path with an incoming path without doubling or
// eating separators: upstream "/provider/v1" + "/chat/completions".
func joinURLPath(base, p string) string {
	switch {
	case base == "":
		return p
	case p == "":
		return base
	}
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(p, "/")
}

// hopHeaders are connection-scoped and must not be forwarded.
var hopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Trailers":            true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		if hopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// flush pushes buffered response bytes to the client without requiring the
// handler to own a concrete ResponseWriter type.
func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
