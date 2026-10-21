package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// stubOptions is what the stdio server needs. It is passed in, never read from
// the environment, so the smoke test drives exactly the same server the rig's
// config spawns.
type stubOptions struct {
	Scenario string
	Out      string
	Catalog  catalog
	MaxCalls int // 0 = no cap
}

// stubCall is one line of stub-calls.jsonl: the whole tools/call, the top-level
// argument keys (the padding signal, and the report's "keys sent"), and what
// the stub answered.
type stubCall struct {
	Seq       int             `json:"seq"`
	At        string          `json:"at"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Keys      []string        `json:"argumentKeys"`
	IsError   bool            `json:"isError"`
	Result    string          `json:"result"`
}

// serveStub runs the stdio MCP server until stdin closes. It logs every call
// and never writes anywhere but its own run directory: there is no plane client
// in this binary at all.
func serveStub(ctx context.Context, in io.Reader, out io.Writer, opts stubOptions) error {
	scenario, ok := scenarios()[opts.Scenario]
	if !ok {
		return fmt.Errorf("unknown scenario %q", opts.Scenario)
	}
	if err := os.MkdirAll(opts.Out, 0o755); err != nil {
		return err
	}
	logPath := filepath.Join(opts.Out, "stub-calls.jsonl")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("stub log: %w", err)
	}
	defer logf.Close()
	// A start marker, so the rig can tell "opencode never spawned the stub" from
	// "the stub spawned and the model never called a tool".
	start := time.Now()
	if err := os.WriteFile(filepath.Join(opts.Out, "stub-started"), []byte(start.UTC().Format(time.RFC3339Nano)+"\n"), 0o644); err != nil {
		return err
	}

	var mu sync.Mutex
	calls := 0
	w := bufio.NewWriter(out)
	defer w.Flush()
	send := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		w.Write(append(b, '\n'))
		w.Flush()
	}
	reply := func(id json.RawMessage, result any) {
		send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	replyErr := func(id json.RawMessage, code int, msg string) {
		send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
	}

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
			continue
		}
		hasID := len(msg.ID) > 0 && string(msg.ID) != "null"

		switch msg.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			if p.ProtocolVersion == "" {
				p.ProtocolVersion = "2025-03-26"
			}
			reply(msg.ID, map[string]any{
				"protocolVersion": p.ProtocolVersion,
				"capabilities": map[string]any{
					"tools":     map[string]any{"listChanged": false},
					"resources": map[string]any{"listChanged": false},
					"prompts":   map[string]any{"listChanged": false},
				},
				"serverInfo": map[string]any{"name": "clankerbar-opencode-rig-stub", "version": "0.1"},
			})

		case "notifications/initialized", "notifications/cancelled", "logging/setLevel":
			// Notifications are one-way; no reply.

		case "ping":
			reply(msg.ID, map[string]any{})

		case "tools/list":
			tools := opts.Catalog.Tools
			if tools == nil {
				tools = []json.RawMessage{}
			}
			reply(msg.ID, map[string]any{"tools": tools})

		case "resources/list":
			reply(msg.ID, map[string]any{"resources": []any{}})

		case "prompts/list":
			reply(msg.ID, map[string]any{"prompts": []any{}})

		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			rawArgs := json.RawMessage("{}")
			if len(msg.Params) > 0 {
				var envelope struct {
					Arguments json.RawMessage `json:"arguments"`
				}
				if json.Unmarshal(msg.Params, &envelope) == nil && len(envelope.Arguments) > 0 {
					rawArgs = envelope.Arguments
				}
			}

			mu.Lock()
			calls++
			n := calls
			mu.Unlock()

			var res toolResult
			capped := opts.MaxCalls > 0 && n > opts.MaxCalls
			if capped {
				// Stop the model rather than let it loop: the rig kills the
				// session when this marker lands, and the call is still logged.
				_ = os.WriteFile(filepath.Join(opts.Out, "stub-capped"), []byte(fmt.Sprintf("%d\n", n)), 0o644)
				res = toolError("rig_tool_cap", fmt.Sprintf(
					"rig: tool-call cap of %d reached; this run is being stopped. Nothing was written.", opts.MaxCalls))
			} else if fn := scenario.Respond; fn != nil {
				res = fn(p.Name, p.Arguments)
			} else {
				res = defaultToolResult(p.Name)
			}

			entry := stubCall{
				Seq:       n,
				At:        time.Now().UTC().Format(time.RFC3339Nano),
				Name:      p.Name,
				Arguments: rawArgs,
				Keys:      sortedKeys(p.Arguments),
				IsError:   res.IsError,
				Result:    res.Text,
			}
			if line, err := json.Marshal(entry); err == nil {
				logf.Write(append(line, '\n'))
			}
			reply(msg.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": res.Text}},
				"isError": res.IsError,
			})

		default:
			if hasID {
				replyErr(msg.ID, -32601, "method not found: "+msg.Method)
			}
		}
	}
	return sc.Err()
}

// sortedKeys returns a map's keys sorted, the report's stable "keys sent" view.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// stubCmd is the stdio entry point opencode spawns from the generated config.
func stubCmd(args []string) int {
	fs := flagSet("stub")
	scenario := fs.String("scenario", "padding", "scenario whose canned responses to serve")
	out := fs.String("out", "", "run directory for the call log")
	schemas := fs.String("schemas", "", "tool catalog file")
	maxCalls := fs.Int("max-calls", 0, "cap tool calls (0 = no cap)")
	if err := parseFlags(fs, args); err != nil {
		return 2
	}
	if *out == "" || *schemas == "" {
		fmt.Fprintln(os.Stderr, "opencode-rig stub: --out and --schemas are required")
		return 2
	}
	cat, err := loadCatalog(*schemas)
	if err != nil {
		fmt.Fprintf(os.Stderr, "opencode-rig stub: %v\n", err)
		return 1
	}
	if err := serveStub(context.Background(), os.Stdin, os.Stdout, stubOptions{
		Scenario: *scenario,
		Out:      *out,
		Catalog:  cat,
		MaxCalls: *maxCalls,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "opencode-rig stub: %v\n", err)
		return 1
	}
	return 0
}
