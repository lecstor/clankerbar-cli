package mcpbridge

// rpc.go — the small amount of JSON-RPC the bridge must understand to route a
// message: its id (absent means a notification, which has no response), its
// method (some are keyless discovery), and the protocol version an initialize
// negotiates.

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// JSON-RPC error codes this bridge synthesizes. -32000..-32099 is the
// implementation-defined server-error range, which is where a failure to
// authenticate belongs; -32603 is the standard internal error; -32700 is the
// standard parse error.
const (
	codeUnauthenticated = -32001
	codeInternal        = -32603
	codeParse           = -32700
)

// rpcMessage is the routing view of one incoming line.
type rpcMessage struct {
	id       json.RawMessage // nil when the message carries no id
	method   string
	keyless  bool
	batch    bool   // a JSON array of requests: forwarded whole, responses relayed
	protocol string // initialize's params.protocolVersion, when present
}

// isNotification reports whether the message carries no id and is therefore
// owed no response.
func (m rpcMessage) isNotification() bool { return len(m.id) == 0 && !m.batch }

// wantsResponse reports whether a reply (or a synthesized error) belongs on
// stdout for this message.
func (m rpcMessage) wantsResponse() bool { return !m.isNotification() }

// keylessMethods may be forwarded with no credential. Discovery is what a
// harness does before an operator has logged in, and the plane serves it
// anonymously; everything else needs a token.
var keylessMethods = map[string]bool{
	"initialize":                true,
	"notifications/initialized": true,
	"tools/list":                true,
	"resources/list":            true,
	"prompts/list":              true,
}

// errInvalidJSON is the parse error a line that is not JSON produces.
var errInvalidJSON = errors.New("invalid JSON-RPC message on stdin")

// parseRPCMessage reads the routing facts out of one line.
func parseRPCMessage(raw []byte) (rpcMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return rpcMessage{}, errInvalidJSON
	}
	if trimmed[0] == '[' {
		// A batch is forwarded untouched: the plane's JSON-RPC layer is the
		// only thing that should split it, and it is what knows which members
		// are notifications.
		return rpcMessage{batch: true}, nil
	}
	var probe struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return rpcMessage{}, errInvalidJSON
	}
	msg := rpcMessage{
		id:      probe.ID,
		method:  probe.Method,
		keyless: keylessMethods[probe.Method],
	}
	// params may be an object or (per JSON-RPC) an array; only an object can
	// carry initialize's protocolVersion, and an array must not make an
	// otherwise valid request unparseable here.
	if len(probe.Params) > 0 && probe.Params[0] == '{' {
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if err := json.Unmarshal(probe.Params, &params); err == nil {
			msg.protocol = params.ProtocolVersion
		}
	}
	return msg, nil
}

// rpcError builds one JSON-RPC error response, preserving the client's id
// byte-for-byte (a numeric id stays a number). An absent id becomes JSON null:
// the only response a parse failure can carry is the null-id form.
func rpcError(id json.RawMessage, code int, message string) []byte {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	payload := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{JSONRPC: "2.0", ID: id}
	payload.Error.Code = code
	payload.Error.Message = message
	out, err := json.Marshal(payload)
	if err != nil {
		// Marshalling this fixed shape cannot fail; the fallback keeps a botched
		// error from becoming no answer at all.
		return []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"clankerbar: cannot encode an error response"}}`)
	}
	return out
}

// compactJSON reduces a payload to exactly one line without changing its
// meaning. A body that is not valid JSON (which is never forwarded) falls back
// to whitespace-collapsed text so it still cannot introduce a second line.
func compactJSON(raw []byte) []byte {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err == nil {
		return buf.Bytes()
	}
	return []byte(strings.Join(strings.Fields(string(raw)), " "))
}
