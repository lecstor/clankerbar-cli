package mcpbridge

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseRPCMessage(t *testing.T) {
	cases := []struct {
		name         string
		raw          string
		id           string // "" means absent
		method       string
		keyless      bool
		batch        bool
		notification bool
	}{
		{
			name: "initialize", raw: `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
			id: "1", method: "initialize", keyless: true,
		},
		{
			name: "tools call", raw: `{"jsonrpc":"2.0","id":"abc","method":"tools/call","params":{}}`,
			id: `"abc"`, method: "tools/call",
		},
		{
			name: "notification", raw: `{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			method: "notifications/initialized", keyless: true, notification: true,
		},
		{
			name: "batch", raw: `[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`,
			batch: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := parseRPCMessage([]byte(tc.raw))
			if err != nil {
				t.Fatalf("parseRPCMessage: %v", err)
			}
			if got := string(msg.id); got != tc.id {
				t.Errorf("id = %q, want %q", got, tc.id)
			}
			if msg.method != tc.method {
				t.Errorf("method = %q, want %q", msg.method, tc.method)
			}
			if msg.keyless != tc.keyless {
				t.Errorf("keyless = %v, want %v", msg.keyless, tc.keyless)
			}
			if msg.batch != tc.batch {
				t.Errorf("batch = %v, want %v", msg.batch, tc.batch)
			}
			if msg.isNotification() != tc.notification {
				t.Errorf("isNotification = %v, want %v", msg.isNotification(), tc.notification)
			}
		})
	}
}

func TestParseRPCMessageRejectsInvalidJSON(t *testing.T) {
	for _, raw := range []string{"not json", `{"id":1,`, ""} {
		if _, err := parseRPCMessage([]byte(raw)); err == nil {
			t.Errorf("parseRPCMessage(%q) succeeded", raw)
		}
	}
}

func TestParseRPCMessageAcceptsArrayParams(t *testing.T) {
	// JSON-RPC permits params to be an array; MCP does not use that shape, but
	// it must not turn an otherwise routable message into a parse error.
	msg, err := parseRPCMessage([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":[]}`))
	if err != nil {
		t.Fatalf("parseRPCMessage: %v", err)
	}
	if string(msg.id) != "1" || msg.method != "tools/call" {
		t.Errorf("message = %+v", msg)
	}
}

func TestRPCErrorPreservesID(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		{`7`, `7`},
		{`"req-1"`, `"req-1"`},
		{`{"nested":true}`, `{"nested":true}`},
		{"", `null`},
	}
	for _, tc := range cases {
		line := rpcError(json.RawMessage(tc.id), codeUnauthenticated, "run `clankerbar login`")
		var got struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Error   struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(line, &got); err != nil {
			t.Fatalf("rpcError is not JSON: %v\n%s", err, line)
		}
		if got.JSONRPC != "2.0" {
			t.Errorf("jsonrpc = %q", got.JSONRPC)
		}
		if string(got.ID) != tc.want {
			t.Errorf("id = %s, want %s", got.ID, tc.want)
		}
		if got.Error.Code != codeUnauthenticated || !strings.Contains(got.Error.Message, "clankerbar login") {
			t.Errorf("error = %+v", got.Error)
		}
	}
}

func TestCompactJSONMakesExactlyOneLine(t *testing.T) {
	pretty := "{\n  \"jsonrpc\": \"2.0\",\n  \"id\": 1\n}\n"
	got := string(compactJSON([]byte(pretty)))
	if got != `{"jsonrpc":"2.0","id":1}` {
		t.Errorf("compactJSON = %q", got)
	}
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("compactJSON left a line break: %q", got)
	}
}
