package mcpbridge

// mcpbridge.go — the bridge itself. stdin carries newline-delimited JSON-RPC
// requests and notifications; every one is POSTed to the plane's Streamable
// HTTP MCP endpoint, and whatever comes back (one JSON body or an SSE stream)
// is relayed to stdout one JSON-RPC payload per line.
//
// stdout discipline is the contract: Out receives JSON-RPC lines and nothing
// else, because that pipe belongs to the harness's MCP client. Diagnostics,
// warnings and errors go to Err (stderr) where a human or a log can see them
// without corrupting the protocol.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"

	"github.com/lecstor/clankerbar-cli/internal/auth"
	"github.com/lecstor/clankerbar-cli/internal/secureurl"
)

const (
	// maxLineBytes bounds one stdin line. A tools/call can carry a whole file
	// or a long prompt as an argument, so the scanner gets room well past a
	// dense tool definition; 4 MiB is the floor the design names.
	maxLineBytes = 4 << 20

	// maxResponseBytes bounds one JSON body or one SSE event payload. It is a
	// runaway guard, not a protocol limit: the plane's largest tool results are
	// well under it.
	maxResponseBytes = 64 << 20

	// maxErrorBody bounds the server text quoted in a synthesized error.
	maxErrorBody = 4 << 10

	// DefaultMaxInflight caps concurrent requests forwarded to the plane. MCP
	// clients pipeline; the cap keeps a misbehaving one from opening unbounded
	// connections while still allowing real concurrency.
	DefaultMaxInflight = 16
)

// Config is one bridge run.
type Config struct {
	// Target is the Streamable HTTP MCP endpoint, e.g.
	// https://clankerbar.com/mcp or https://clankerbar.com/mcp/<slug>.
	Target string
	// Origin is the credential origin auth.Resolve answers for.
	Origin string
	// In is the newline-delimited JSON-RPC input (os.Stdin).
	In io.Reader
	// Out receives newline-delimited JSON-RPC responses and nothing else
	// (os.Stdout).
	Out io.Writer
	// Err receives diagnostics, warnings and errors (os.Stderr).
	Err io.Writer
	// MaxInflight caps concurrent requests; 0 means DefaultMaxInflight.
	MaxInflight int
}

// Run bridges stdin to the plane until stdin ends or ctx is cancelled.
//
// It returns nil on EOF (after draining in-flight requests) and on
// SIGINT/SIGTERM (ctx cancellation, which main's signal context delivers), so
// the process exits 0. A read failure that leaves the stream unusable is
// returned as an error.
func Run(ctx context.Context, cfg Config) error {
	if strings.TrimSpace(cfg.Target) == "" {
		return errors.New("mcpbridge: no target URL")
	}
	if strings.TrimSpace(cfg.Origin) == "" {
		return errors.New("mcpbridge: no credential origin")
	}
	if cfg.In == nil || cfg.Out == nil {
		return errors.New("mcpbridge: stdin and stdout are both required")
	}
	if cfg.Err == nil {
		cfg.Err = io.Discard
	}
	if cfg.MaxInflight <= 0 {
		cfg.MaxInflight = DefaultMaxInflight
	}
	b := &bridge{
		cfg:      cfg,
		client:   &http.Client{CheckRedirect: sameOriginRedirect(cfg.Origin)},
		inflight: make(chan struct{}, cfg.MaxInflight),
	}

	done := make(chan error, 1)
	go func() { done <- b.readLoop(ctx) }()

	select {
	case <-ctx.Done():
		// The signal path exits promptly: in-flight requests carry this same
		// context, so they unwind on their own and the process is about to end.
		return nil
	case err := <-done:
		// readLoop has returned, so no further goroutine can be added; draining
		// here is what guarantees a response already on the wire is written
		// before the process exits.
		b.wg.Wait()
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
}

// bridge is the state one Run holds.
type bridge struct {
	cfg    Config
	client *http.Client

	// inflight is the concurrency bound: a token is taken before a goroutine is
	// started and returned when it finishes.
	inflight chan struct{}
	wg       sync.WaitGroup

	outMu sync.Mutex // one whole line at a time on stdout

	protoMu      sync.Mutex
	protoVersion string

	warnOnce sync.Once // the no-credential hint is said once, not once per request
}

// readLoop scans stdin for messages and hands each to a worker.
func (b *bridge) readLoop(ctx context.Context) error {
	sc := bufio.NewScanner(b.cfg.In)
	sc.Buffer(make([]byte, 64*1024), maxLineBytes)
	for sc.Scan() {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		msg, err := parseRPCMessage(raw)
		if err != nil {
			b.emit(rpcError(nil, codeParse, "clankerbar: "+err.Error()))
			continue
		}
		// The version is recorded before the worker starts, so an initialize
		// and the requests a client sends straight after it agree on it even
		// while the initialize response is still in flight.
		if msg.method == "initialize" && msg.protocol != "" {
			b.setProtocolVersion(msg.protocol)
		}
		line := append([]byte(nil), raw...)
		if !b.dispatch(ctx, line, msg) {
			return nil
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	return nil
}

// dispatch starts one worker, blocking while the in-flight cap is reached.
// It reports whether the read loop should continue.
func (b *bridge) dispatch(ctx context.Context, raw []byte, msg rpcMessage) bool {
	select {
	case b.inflight <- struct{}{}:
	case <-ctx.Done():
		return false
	}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer func() { <-b.inflight }()
		b.handle(ctx, raw, msg)
	}()
	return true
}

// handle resolves the credential and forwards one message. The token is
// resolved per request, so `clankerbar login` takes effect without restarting
// the bridge.
func (b *bridge) handle(ctx context.Context, raw []byte, msg rpcMessage) {
	res, err := auth.Resolve(b.cfg.Origin)
	if err != nil {
		b.fail(msg, fmt.Errorf("read the credential store: %w (run `clankerbar login`)", err))
		return
	}
	if res.Token == "" && !msg.keyless && !msg.batch {
		hint := fmt.Sprintf("clankerbar: not authenticated for %s - run `clankerbar login` (or set %s)", b.cfg.Origin, auth.EnvAPIKey)
		if msg.isNotification() {
			b.warnAuthHint(hint)
			return
		}
		b.emit(rpcError(msg.id, codeUnauthenticated, hint))
		b.warnAuthHint(hint)
		return
	}
	b.post(ctx, raw, msg, res.Token)
}

// post makes the one HTTP request a message becomes and relays its answer.
func (b *bridge) post(ctx context.Context, raw []byte, msg rpcMessage, token string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.cfg.Target, bytes.NewReader(raw))
	if err != nil {
		b.fail(msg, fmt.Errorf("build the request: %w", err))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	// The header belongs on requests AFTER initialize; the initialize request
	// carries the version in its body instead.
	if v := b.protocolVersion(); v != "" && msg.method != "initialize" {
		req.Header.Set("MCP-Protocol-Version", v)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return // cancelled shutdown, not a client-visible failure
		}
		b.fail(msg, fmt.Errorf("call %s: %w", b.cfg.Target, err))
		return
	}
	defer resp.Body.Close()

	// A notification's success is an empty 202/204; there is nothing to relay.
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent {
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		b.failStatus(msg, resp.StatusCode, body)
		return
	}
	if isEventStream(resp.Header.Get("Content-Type")) {
		b.forwardSSE(ctx, msg, resp.Body)
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		b.fail(msg, fmt.Errorf("read the response: %w", err))
		return
	}
	if len(body) > maxResponseBytes {
		b.fail(msg, fmt.Errorf("the response exceeded %d bytes", maxResponseBytes))
		return
	}
	b.forwardPayload(msg, body)
}

// forwardPayload relays one JSON body. Empty bodies (a bare 200 on a
// notification) produce nothing; a notification never produces a line even if
// the server answers it.
func (b *bridge) forwardPayload(msg rpcMessage, payload []byte) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || !msg.wantsResponse() {
		return
	}
	if !json.Valid(trimmed) {
		b.fail(msg, errors.New("the plane returned a response that is not JSON"))
		return
	}
	b.observeResponse(trimmed)
	b.emit(compactJSON(trimmed))
}

// forwardSSE relays every JSON payload an event stream dispatches. If a request
// stream closes without one, the client gets an error instead of hanging.
func (b *bridge) forwardSSE(ctx context.Context, msg rpcMessage, body io.Reader) {
	emitted := 0
	err := forEachSSEData(body, func(payload []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		trimmed := bytes.TrimSpace(payload)
		if len(trimmed) == 0 {
			return nil
		}
		if !json.Valid(trimmed) {
			b.warnf("ignoring a non-JSON event-stream payload")
			return nil
		}
		if !msg.wantsResponse() {
			return nil
		}
		b.observeResponse(trimmed)
		b.emit(compactJSON(trimmed))
		emitted++
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		if msg.wantsResponse() && emitted == 0 {
			b.fail(msg, fmt.Errorf("read the event stream: %w", err))
		} else {
			b.warnf("read the event stream: %v", err)
		}
		return
	}
	if msg.wantsResponse() && emitted == 0 {
		b.fail(msg, errors.New("the plane closed the event stream without a response"))
	}
}

// fail turns a transport-level failure into the JSON-RPC error the client is
// waiting for, preserving its id. A notification has no one to answer, so the
// failure is only reported on stderr.
func (b *bridge) fail(msg rpcMessage, err error) {
	text := "clankerbar: " + err.Error()
	if !msg.wantsResponse() {
		b.warnf("%s", text)
		return
	}
	b.emit(rpcError(msg.id, codeInternal, text))
}

// failStatus maps an HTTP failure to a JSON-RPC error. An authentication
// refusal gets the implementation-defined code and the one hint that fixes it.
func (b *bridge) failStatus(msg rpcMessage, status int, body []byte) {
	var text string
	code := codeInternal
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		code = codeUnauthenticated
		text = fmt.Sprintf("clankerbar: %s rejected the credential (HTTP %d %s) - run `clankerbar login` (or replace %s)",
			b.cfg.Origin, status, http.StatusText(status), auth.EnvAPIKey)
	} else {
		text = fmt.Sprintf("clankerbar: %s returned HTTP %d %s", b.cfg.Origin, status, http.StatusText(status))
		if snippet := strings.TrimSpace(string(body)); snippet != "" {
			text += ": " + snippet
		}
	}
	if !msg.wantsResponse() {
		b.warnf("%s", text)
		return
	}
	b.emit(rpcError(msg.id, code, text))
}

// observeResponse records the protocol version an initialize response
// negotiated, so later requests carry it in MCP-Protocol-Version.
func (b *bridge) observeResponse(payload []byte) {
	var probe struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return
	}
	if v := strings.TrimSpace(probe.Result.ProtocolVersion); v != "" {
		b.setProtocolVersion(v)
	}
}

func (b *bridge) protocolVersion() string {
	b.protoMu.Lock()
	defer b.protoMu.Unlock()
	return b.protoVersion
}

func (b *bridge) setProtocolVersion(v string) {
	b.protoMu.Lock()
	defer b.protoMu.Unlock()
	b.protoVersion = v
}

// emit writes exactly one line, under a mutex so concurrent responses never
// interleave mid-line.
func (b *bridge) emit(line []byte) {
	if len(line) == 0 {
		return
	}
	b.outMu.Lock()
	defer b.outMu.Unlock()
	if _, err := b.cfg.Out.Write(line); err != nil {
		return // the client closed the pipe; stdin's EOF ends the run
	}
	_, _ = b.cfg.Out.Write([]byte("\n"))
}

// warnf writes one diagnostic line to stderr. It never touches stdout.
func (b *bridge) warnf(format string, args ...any) {
	fmt.Fprintf(b.cfg.Err, "clankerbar mcp: "+format+"\n", args...)
}

// warnAuthHint says the login hint once for the whole run, so a keyless
// harness's polling does not flood stderr.
func (b *bridge) warnAuthHint(hint string) {
	b.warnOnce.Do(func() { b.warnf("%s", hint) })
}

// isEventStream reports whether a Content-Type names text/event-stream, with or
// without parameters.
func isEventStream(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "text/event-stream")
	}
	return strings.EqualFold(mt, "text/event-stream")
}

// sameOriginRedirect refuses a redirect that would move the bearer token off
// the origin it was resolved for. Go's own client strips Authorization on a
// host change but forwards it on an https -> http downgrade of the same host,
// and either way a cross-origin hop is not a destination this bridge names.
func sameOriginRedirect(origin string) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if !secureurl.SameOrigin(origin, req.URL.String()) {
			return fmt.Errorf("refusing a redirect off %s to %s: the credential never leaves its origin",
				origin, secureurl.RedactUserinfo(req.URL.String()))
		}
		return nil
	}
}
