package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// catalog is a tools/list result plus where it came from, written to the run
// directory so the exact schema a run served is still on disk afterwards.
type catalog struct {
	Source    string            `json:"source,omitempty"`
	FetchedAt string            `json:"fetchedAt,omitempty"`
	Tools     []json.RawMessage `json:"tools"`
}

// toolNames returns the catalog's tool names, in order.
func (c catalog) toolNames() []string {
	names := make([]string, 0, len(c.Tools))
	for _, raw := range c.Tools {
		var t struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &t) == nil && t.Name != "" {
			names = append(names, t.Name)
		}
	}
	return names
}

// loadCatalog reads a schema file. It accepts the rig's own catalog envelope,
// a bare {"tools":[...]} object, a saved tools/list response
// ({"result":{"tools":[...]}}), or a bare array of tools, so a file saved by
// hand from any of those shapes works without conversion.
func loadCatalog(path string) (catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return catalog{}, err
	}
	var c catalog
	var probe struct {
		Tools  []json.RawMessage `json:"tools"`
		Result *struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return catalog{}, fmt.Errorf("%s: not a tool catalog: %w", path, err)
	}
	switch {
	case probe.Result != nil && probe.Result.Tools != nil:
		c.Tools = probe.Result.Tools
	case probe.Tools != nil:
		c.Tools = probe.Tools
	default:
		// A bare array.
		if err := json.Unmarshal(data, &c.Tools); err != nil {
			return catalog{}, fmt.Errorf("%s: no tools array (expected {\"tools\":[...]}, a tools/list result, or a bare array)", path)
		}
	}
	if len(c.Tools) == 0 {
		return catalog{}, fmt.Errorf("%s: the tool catalog is empty", path)
	}
	// Keep the envelope's own provenance when it had one.
	var env struct {
		Source    string `json:"source"`
		FetchedAt string `json:"fetchedAt"`
	}
	_ = json.Unmarshal(data, &env)
	c.Source, c.FetchedAt = env.Source, env.FetchedAt
	return c, nil
}

// saveCatalog writes the catalog envelope. Raw tool objects are passed through
// untouched: the point of the rig's schema scenario is the bytes the plane
// (or a proposal) serves, so re-marshalling must not reorder or normalise them.
func saveCatalog(path string, c catalog) error {
	if c.FetchedAt == "" {
		c.FetchedAt = time.Now().UTC().Format(time.RFC3339)
	}
	data, err := json.MarshalIndent(struct {
		Source    string            `json:"source,omitempty"`
		FetchedAt string            `json:"fetchedAt,omitempty"`
		Tools     []json.RawMessage `json:"tools"`
	}{c.Source, c.FetchedAt, c.Tools}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

// fetchCatalog performs a read-only tools/list against an MCP Streamable HTTP
// endpoint: initialize, notifications/initialized, tools/list. It never calls
// a tool, so it cannot write to the backlog.
func fetchCatalog(ctx context.Context, endpoint, apiKey string) (catalog, error) {
	if apiKey == "" {
		return catalog{}, fmt.Errorf("fetching the plane's schemas needs CLANKERBAR_API_KEY (or --plane-key-file)")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	session := ""

	call := func(body map[string]any) (json.RawMessage, error) {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		// Cloudflare bans some non-browser signatures outright (error 1010);
		// a named, honest client is the one that gets through.
		req.Header.Set("User-Agent", "clankerbar-opencode-rig/0.1")
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		body2, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		if resp.StatusCode/100 != 2 {
			return nil, fmt.Errorf("%s: %s: %s", endpoint, resp.Status, truncate(string(body2), 500))
		}
		if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
			session = id
		}
		if len(body2) == 0 {
			return nil, nil
		}
		return responseJSON(body2)
	}

	init, err := call(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "clankerbar-opencode-rig", "version": "0.1"},
		},
	})
	if err != nil {
		return catalog{}, fmt.Errorf("initialize: %w", err)
	}
	if init == nil {
		return catalog{}, fmt.Errorf("initialize: empty response")
	}
	// The initialized notification is a notification: a server may answer 202
	// with no body, and its failure must not stop the read-only list.
	if _, err := call(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); err != nil {
		fmt.Fprintf(os.Stderr, "opencode-rig: notifications/initialized: %v (continuing)\n", err)
	}
	listed, err := call(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": map[string]any{}})
	if err != nil {
		return catalog{}, fmt.Errorf("tools/list: %w", err)
	}
	var msg struct {
		Result *struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(listed, &msg); err != nil {
		return catalog{}, fmt.Errorf("tools/list: %w", err)
	}
	if msg.Error != nil {
		return catalog{}, fmt.Errorf("tools/list: %d %s", msg.Error.Code, msg.Error.Message)
	}
	if msg.Result == nil || len(msg.Result.Tools) == 0 {
		return catalog{}, fmt.Errorf("tools/list returned no tools")
	}
	return catalog{Source: endpoint, Tools: msg.Result.Tools}, nil
}

// responseJSON extracts the JSON-RPC message from an MCP response body, which
// is either plain JSON or a one-or-more-event SSE stream. SSE is what the live
// plane answers with (content-type: text/event-stream).
func responseJSON(body []byte) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '{' {
		return append(json.RawMessage(nil), trimmed...), nil
	}
	// SSE: collect each event's data lines until a blank line, then keep the
	// first that parses as a JSON object.
	var data bytes.Buffer
	flush := func() (json.RawMessage, bool) {
		if data.Len() == 0 {
			return nil, false
		}
		line := bytes.TrimSpace(data.Bytes())
		data.Reset()
		if len(line) == 0 || line[0] != '{' {
			return nil, false
		}
		return append(json.RawMessage(nil), line...), true
	}
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 64<<10), 32<<20)
	for sc.Scan() {
		line := sc.Bytes()
		switch {
		case len(bytes.TrimSpace(line)) == 0:
			if msg, ok := flush(); ok {
				return msg, nil
			}
		case bytes.HasPrefix(line, []byte("data:")):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))))
		}
	}
	if msg, ok := flush(); ok {
		return msg, nil
	}
	return nil, fmt.Errorf("no JSON-RPC message in response: %s", truncate(string(body), 300))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// fetchSchemasCmd is the standalone entry point behind `fetch-schemas`.
func fetchSchemasCmd(args []string) int {
	fs := flagSet("fetch-schemas")
	url := fs.String("url", "https://clankerbar.com/mcp/clankerbar", "plane MCP endpoint")
	out := fs.String("out", "schemas.json", "where to write the catalog")
	keyFile := fs.String("plane-key-file", "", "read CLANKERBAR_API_KEY from this file")
	if err := parseFlags(fs, args); err != nil {
		return 2
	}
	key := os.Getenv("CLANKERBAR_API_KEY")
	if *keyFile != "" {
		b, err := os.ReadFile(expandHome(*keyFile))
		if err != nil {
			fmt.Fprintf(os.Stderr, "opencode-rig: %v\n", err)
			return 2
		}
		key = strings.TrimSpace(string(b))
	}
	cat, err := fetchCatalog(context.Background(), *url, key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "opencode-rig: %v\n", err)
		return 1
	}
	if err := saveCatalog(*out, cat); err != nil {
		fmt.Fprintf(os.Stderr, "opencode-rig: %v\n", err)
		return 1
	}
	fmt.Printf("wrote %d tools from %s to %s\n", len(cat.Tools), *url, *out)
	return 0
}
