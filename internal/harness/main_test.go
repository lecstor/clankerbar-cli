package harness

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// fakeMCPDirEnv switches the test binary into a fake stdio MCP server instead
// of running tests. The opencode conformance suite points an MCP config's
// `command` at this binary with the variable set, so the fake needs nothing
// installed beyond Go (no python, no node).
const fakeMCPDirEnv = "CLANKERBAR_FAKE_MCP_DIR"

func TestMain(m *testing.M) {
	if dir := os.Getenv(fakeMCPDirEnv); dir != "" {
		runFakeMCP(dir)
		os.Exit(0)
	}
	// The conformance suite execs the opencode 2.x binary through the real
	// adapter, which runs whatever `opencode` PATH resolves. PATH is set here,
	// once, rather than per test with t.Setenv, so the slow error cases can run
	// in parallel.
	if os.Getenv(conformanceEnv) != "" {
		if bin := conformanceBin(); bin != "" {
			os.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
		}
	}
	os.Exit(m.Run())
}

// runFakeMCP serves newline-delimited JSON-RPC on stdin/stdout: initialize,
// tools/list (claim_task, get_backlog_summary) and tools/call. claim_task
// answers with the task/run ids the claim observer reads, and every call is
// appended to calls.jsonl in dir so a test can see what reached the server.
func runFakeMCP(dir string) {
	tools := []map[string]any{
		{"name": "claim_task", "description": "Claim a task", "inputSchema": map[string]any{
			"type": "object", "properties": map[string]any{"taskId": map[string]any{"type": "string"}}, "required": []string{"taskId"},
		}},
		{"name": "get_backlog_summary", "description": "Summary", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}},
	}
	out := bufio.NewWriter(os.Stdout)
	send := func(v any) {
		b, _ := json.Marshal(v)
		out.Write(append(b, '\n'))
		out.Flush()
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil {
			continue
		}
		reply := func(result any) {
			send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		}
		switch msg.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			json.Unmarshal(msg.Params, &p)
			if p.ProtocolVersion == "" {
				p.ProtocolVersion = "2024-11-05"
			}
			reply(map[string]any{
				"protocolVersion": p.ProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fake-clankerbar", "version": "0.0.1"},
			})
		case "tools/list":
			reply(map[string]any{"tools": tools})
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			json.Unmarshal(msg.Params, &p)
			if f, err := os.OpenFile(filepath.Join(dir, "calls.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
				f.Write(append(msg.Params, '\n'))
				f.Close()
			}
			text := `{"summary":"empty"}`
			if p.Name == "claim_task" {
				text = fmt.Sprintf(`{"task":{"id":%q,"ref":"CLA-1"},"run":{"id":"run-1"}}`, p.Arguments["taskId"])
			}
			reply(map[string]any{"content": []map[string]any{{"type": "text", "text": text}}})
		default:
			if len(msg.ID) > 0 {
				reply(map[string]any{})
			}
		}
	}
}
