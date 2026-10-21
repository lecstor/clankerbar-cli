package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// rigEvent is one line of opencode's `--format json` stream, reduced to the
// fields the report needs. Unknown fields are ignored on purpose: the stream is
// opencode's, not this rig's.
type rigEvent struct {
	Type      string    `json:"type"`
	SessionID string    `json:"sessionID"`
	Part      *rigPart  `json:"part"`
	Error     *rigError `json:"error"`
}

type rigPart struct {
	Type   string     `json:"type"`
	Text   string     `json:"text"`
	Reason string     `json:"reason"`
	Tool   string     `json:"tool"`
	ID     string     `json:"id"`
	Tokens *rigTokens `json:"tokens"`
	Cost   *float64   `json:"cost"`
	State  *rigState  `json:"state"`
}

type rigTokens struct {
	Input     int       `json:"input"`
	Output    int       `json:"output"`
	Reasoning int       `json:"reasoning"`
	Cache     *rigCache `json:"cache"`
}

type rigCache struct {
	Read  int `json:"read"`
	Write int `json:"write"`
}

type rigState struct {
	Status string          `json:"status"`
	Title  string          `json:"title"`
	Input  json.RawMessage `json:"input"`
	Output string          `json:"output"`
	Error  string          `json:"error"`
}

type rigError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Status  int    `json:"status"`
}

// capturedRequest is one recorded proxy request, read back from run.json's
// sibling requests/ directory.
type capturedRequest struct {
	Seq       int               `json:"seq"`
	Method    string            `json:"method"`
	Path      string            `json:"path"`
	Headers   map[string]string `json:"headers"`
	Body      json.RawMessage   `json:"body"`
	BodyText  string            `json:"bodyText"`
	AuthOmit  bool              `json:"authorizationOmitted"`
	Redacted  []string          `json:"redactedHeaders"`
	BodyBytes int               `json:"bodyBytes"`
}

// chatRequest is the OpenAI-compatible request shape the proxy sees.
type chatRequest struct {
	Model           string            `json:"model"`
	Messages        []json.RawMessage `json:"messages"`
	Tools           []json.RawMessage `json:"tools"`
	Stream          *bool             `json:"stream"`
	ReasoningEffort string            `json:"reasoning_effort"`
	Reasoning       *struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
	Provider *struct {
		ReasoningEffort string `json:"reasoning_effort"`
	} `json:"provider"`
}

// messageMeta is what the report prints about one provider message.
type messageMeta struct {
	Index      int
	Role       string
	ContentLen int
	ContentNil bool
	ToolCalls  int
	Extras     []extraField
}

type extraField struct {
	Name string
	Kind string
	Len  int
}

// readEvents reads opencode's JSONL event stream. A truncated final line (the
// session was killed) is reported as a count, not a failure: the run still has
// everything up to that line.
func readEvents(path string) ([]rigEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []rigEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var ev rigEvent
		if json.Unmarshal(sc.Bytes(), &ev) == nil {
			out = append(out, ev)
		}
	}
	return out, sc.Err()
}

// readStubCalls reads the stub's call log.
func readStubCalls(path string) ([]stubCall, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []stubCall
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var c stubCall
		if json.Unmarshal(sc.Bytes(), &c) == nil {
			out = append(out, c)
		}
	}
	return out, sc.Err()
}

// readRequests loads every captured request in sequence order.
func readRequests(dir string) ([]capturedRequest, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	out := make([]capturedRequest, 0, len(names))
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		var r capturedRequest
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// writeReport renders the report to path and is the only writer; the same text
// is printed to stdout by the caller.
func writeReport(path string, rec runRecord, events []rigEvent, calls []stubCall, requests []capturedRequest) error {
	return os.WriteFile(path, []byte(renderReport(rec, events, calls, requests)), 0o644)
}

// renderReport is the run's answer sheet: tool calls and the keys they sent,
// reasoning per step, and the assistant fields replayed on each provider
// request. It is deliberately plain Markdown and reads top-to-bottom.
func renderReport(rec runRecord, events []rigEvent, calls []stubCall, requests []capturedRequest) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	line("# opencode rig: %s", rec.Scenario)
	line("")
	line("- run: `%s`", rec.RunID)
	line("- model: `%s` (provider `%s`)", rec.Model, rec.Provider)
	line("- provider upstream: `%s` via `%s`", rec.Upstream, rec.ProxyURL)
	line("- tools: %d from `%s`", len(rec.Tools), schemaSourceLabel(rec.SchemaSource))
	line("- sandbox: `%s`", rec.Workdir)
	line("- deadline: %s; stub call cap: %d", rec.Deadline, rec.MaxToolCalls)
	line("- session: exit %d after %s (timed out: %v, capped: %v, stub started: %v)",
		rec.ExitCode, time.Duration(rec.DurationMS)*time.Millisecond, rec.TimedOut, rec.Capped, rec.StubStarted)
	line("")
	line("Prompt:")
	line("")
	line("```text")
	line("%s", strings.TrimSpace(rec.Prompt))
	line("```")
	line("")

	// ---- Tool calls -------------------------------------------------------
	line("## Tool calls")
	line("")
	if len(calls) == 0 {
		line("No tools/call reached the stub.")
		if !rec.StubStarted {
			line("")
			line("The stub never started: opencode did not spawn the MCP server at all (check `opencode-stderr.log`).")
		} else {
			line("")
			line("The stub started, so opencode connected; the model made no tool call in this run.")
		}
	} else {
		line("| # | tool | keys sent | result |")
		line("|---|------|-----------|--------|")
		for _, c := range calls {
			line("| %d | `%s` | %s | %s |", c.Seq, c.Name, renderKeys(c.Arguments), resultLabel(c))
		}
		line("")
		line("An empty value is printed as the model sent it: `key=null`, `key=false` or `key=\"\"`. A bare `key` means a non-empty value. That distinction is the padding signal.")
	}
	line("")

	// ---- Reasoning per step ----------------------------------------------
	line("## Reasoning per step")
	line("")
	var steps []*rigPart
	for _, ev := range events {
		if ev.Part != nil && ev.Part.Type == "step-finish" {
			p := ev.Part
			steps = append(steps, p)
		}
	}
	if len(steps) == 0 {
		line("No `step_finish` events were emitted. opencode 2.x emits one only for tool-call steps, and a run that ends on a plain answer emits none.")
	} else {
		line("| step | finish reason | input | output | reasoning | cache read | cost |")
		line("|------|---------------|-------|--------|-----------|------------|------|")
		for i, p := range steps {
			var in, out, reasoning, cache int
			cost := "—"
			if p.Tokens != nil {
				in, out, reasoning = p.Tokens.Input, p.Tokens.Output, p.Tokens.Reasoning
				if p.Tokens.Cache != nil {
					cache = p.Tokens.Cache.Read
				}
			}
			if p.Cost != nil {
				cost = fmt.Sprintf("%.6f", *p.Cost)
			}
			line("| %d | %s | %d | %d | %d | %d | %s |", i+1, p.Reason, in, out, reasoning, cache, cost)
		}
	}
	line("")

	// Model-side tool events, which should match the stub's calls. A model call
	// the stub never saw is a different bug from a stub call the model never
	// made, so both are visible.
	var toolEvents []*rigPart
	for _, ev := range events {
		if ev.Part != nil && ev.Part.Tool != "" {
			toolEvents = append(toolEvents, ev.Part)
		}
	}
	line("Model tool_use events: %d; stub calls: %d.", len(toolEvents), len(calls))
	line("")

	// ---- Provider requests ------------------------------------------------
	line("## Provider requests")
	line("")
	if len(requests) == 0 {
		line("No request reached the proxy. The session never got as far as a provider call (see `opencode-stderr.log`).")
		line("")
	} else {
		firstTools := ""
		toolsVary := false
		for _, req := range requests {
			var chat chatRequest
			_ = json.Unmarshal(req.Body, &chat)
			line("### #%d %s %s", req.Seq, req.Method, req.Path)
			line("")
			h := sha256.Sum256(req.Body)
			line("- body: %d bytes, sha256 `%s`", len(req.Body), hex.EncodeToString(h[:8]))
			line("- credentials: %s", credentialSummary(req))
			line("- model: `%s`; messages: %d; tools: %d; stream: %s",
				chat.Model, len(chat.Messages), len(chat.Tools), boolLabel(chat.Stream))
			if effort := reasoningEffort(chat); effort != "" {
				line("- reasoning effort: `%s`", effort)
			}
			if names := toolNamesJSON(chat.Tools); len(names) > 0 {
				th := sha256.Sum256(joinRaw(chat.Tools))
				if firstTools == "" {
					firstTools = hex.EncodeToString(th[:8])
				} else if firstTools != hex.EncodeToString(th[:8]) {
					toolsVary = true
				}
				line("- tool names: %s", strings.Join(names, ", "))
			}
			if len(chat.Messages) > 0 {
				line("")
				line("- messages:")
				for i, raw := range chat.Messages {
					m := messageMetaOf(i, raw)
					line("  - `[%d] %s`: content=%s, tool_calls=%d%s",
						m.Index, m.Role, contentLabel(m), m.ToolCalls, extraSummary(m.Extras))
				}
			}
			line("")
		}
		if firstTools != "" {
			if toolsVary {
				line("The tools array CHANGED between the requests that carried tools (see each tool sha256 above).")
			} else {
				line("The tools array was byte-identical in every request that carried tools (sha256 `%s`).", firstTools)
			}
			line("")
		}
	}

	// ---- Observations -----------------------------------------------------
	line("## Observations")
	line("")
	line("- %d tool call(s): %d refused (isError), %d ok.", len(calls), countErrors(calls), len(calls)-countErrors(calls))
	if lines := editTaskResidual(calls); len(lines) > 0 {
		line("- `edit_task` padding by call — a padded `false` on `container`/`humanOnly` is their CLEAR spelling, `repo=\"\"` clears routing and `null` is absence:")
		for _, l := range lines {
			line("  - %s", l)
		}
	}
	if len(requests) > 1 {
		replayed, assistantMsgs := replayStats(requests)
		line("- reasoning replay: %d of %d assistant message(s) in requests after the first carried a non-empty `reasoning_content`.",
			replayed, assistantMsgs)
	}
	var reasoningTotal int
	for _, ev := range events {
		if ev.Part != nil && ev.Part.Tokens != nil {
			reasoningTotal += ev.Part.Tokens.Reasoning
		}
	}
	line("- reasoning tokens reported on the event stream: %d.", reasoningTotal)
	if rec.Capped {
		line("- the stub call cap was reached; the session was stopped so a looping model could not keep spending.")
	}
	if rec.TimedOut {
		line("- the session hit the %s deadline and was killed.", rec.Deadline)
	}
	line("")

	// ---- Artifacts --------------------------------------------------------
	line("## Raw logs")
	line("")
	line("- `run.json` — resolved settings and outcome")
	line("- `opencode-config.json` — the source config passed as OPENCODE_CONFIG")
	line("- `opencode-config-content.json` — the generated OPENCODE_CONFIG_CONTENT")
	line("- `schemas.json` — the exact tool catalog the stub served")
	line("- `stub-calls.jsonl` — every tools/call with its arguments")
	line("- `requests/` — each request body (never the Authorization header)")
	line("- `responses/` — each provider response as received")
	line("- `opencode-events.jsonl` — the `--format json` event stream")
	line("- `opencode-stderr.log` — opencode's own log (`--print-logs`), captured per run")
	line("- isolated XDG dirs: `%s`", strings.Join(rec.IsolationDirs, "`, `"))
	line("")
	return b.String()
}

func schemaSourceLabel(source string) string {
	if source == "" {
		return "(unknown)"
	}
	if source == "live" {
		return "the live plane"
	}
	return source
}

// credentialSummary states what the record dropped for this request, by name, so
// the "never written to disk" claim is checkable rather than a blanket
// assurance about one header.
func credentialSummary(req capturedRequest) string {
	if len(req.Redacted) > 0 {
		return fmt.Sprintf("headers omitted from the record (forwarded to the provider, never written to disk): %s",
			strings.Join(req.Redacted, ", "))
	}
	if req.AuthOmit {
		// A record written before redactedHeaders existed.
		return "Authorization header: omitted from the record (forwarded to the provider, never written to disk)"
	}
	return "no credential header was sent on this request"
}

func boolLabel(b *bool) string {
	if b == nil {
		return "unset"
	}
	if *b {
		return "true"
	}
	return "false"
}

func resultLabel(c stubCall) string {
	if !c.IsError {
		if changed := changedLabel(c.Result); changed != "" {
			// The receipt's `changed` is the half of the residual the arguments
			// alone cannot answer: what the call actually DID.
			return "ok; changed: " + changed
		}
		return "ok"
	}
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(c.Result), &payload)
	if payload.Error.Code != "" {
		return "error `" + payload.Error.Code + "`"
	}
	return "error"
}

// changedLabel renders a task-write receipt's `changed` list, when the result
// carries one.
func changedLabel(result string) string {
	var payload struct {
		Changed []string `json:"changed"`
	}
	if json.Unmarshal([]byte(result), &payload) != nil || len(payload.Changed) == 0 {
		return ""
	}
	return strings.Join(payload.Changed, ", ")
}

// renderKeys prints a call's top-level arguments, marking the empty value the
// model actually sent. A padded `false` and a padded `""` are as much a signal
// as a null - they are the CLEAR spellings on container/humanOnly and repo - so
// each is printed as sent rather than collapsed to a bare key.
func renderKeys(raw json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || len(m) == 0 {
		return "—"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		switch v := m[k].(type) {
		case nil:
			parts = append(parts, "`"+k+"=null`")
		case bool:
			parts = append(parts, fmt.Sprintf("`%s=%t`", k, v))
		case string:
			if v == "" {
				parts = append(parts, "`"+k+`=""`+"`")
				continue
			}
			parts = append(parts, "`"+k+"`")
		default:
			parts = append(parts, "`"+k+"`")
		}
	}
	return strings.Join(parts, ", ")
}

// messageMetaOf extracts a message's role, content length, tool-call count and
// every field the rig does not recognise. Those extras are the answer to
// "was reasoning_content replayed?".
func messageMetaOf(i int, raw json.RawMessage) messageMeta {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return messageMeta{Index: i, Role: "?", ContentNil: true}
	}
	meta := messageMeta{Index: i, Role: "?"}
	if r, ok := m["role"]; ok {
		_ = json.Unmarshal(r, &meta.Role)
	}
	if c, ok := m["content"]; ok {
		if string(c) == "null" {
			meta.ContentNil = true
		} else if c[0] == '"' {
			var s string
			_ = json.Unmarshal(c, &s)
			meta.ContentLen = len(s)
		} else {
			meta.ContentLen = len(c)
		}
	} else {
		meta.ContentNil = true
	}
	if tc, ok := m["tool_calls"]; ok {
		var arr []json.RawMessage
		if json.Unmarshal(tc, &arr) == nil {
			meta.ToolCalls = len(arr)
		}
	}
	known := map[string]bool{
		"role": true, "content": true, "tool_calls": true, "name": true, "tool_call_id": true,
	}
	for k, v := range m {
		if known[k] {
			continue
		}
		kind := jsonKind(v)
		length := len(v)
		if kind == "string" {
			// Report the STRING's length, not its JSON encoding: an empty
			// reasoning_content is `""` (2 bytes) on the wire and must read as 0.
			var s string
			if json.Unmarshal(v, &s) == nil {
				length = len(s)
			}
		}
		meta.Extras = append(meta.Extras, extraField{Name: k, Kind: kind, Len: length})
	}
	sort.Slice(meta.Extras, func(a, b int) bool { return meta.Extras[a].Name < meta.Extras[b].Name })
	return meta
}

func jsonKind(v json.RawMessage) string {
	s := strings.TrimSpace(string(v))
	switch {
	case s == "null":
		return "null"
	case s == "":
		return "empty"
	case s[0] == '"':
		return "string"
	case s[0] == '{':
		return "object"
	case s[0] == '[':
		return "array"
	default:
		return "scalar"
	}
}

func contentLabel(m messageMeta) string {
	if m.ContentNil {
		return "null"
	}
	return fmt.Sprintf("%d chars", m.ContentLen)
}

func extraSummary(extras []extraField) string {
	if len(extras) == 0 {
		return ""
	}
	parts := make([]string, 0, len(extras))
	for _, e := range extras {
		parts = append(parts, fmt.Sprintf("`%s`=%s(%d)", e.Name, e.Kind, e.Len))
	}
	return ", extras: " + strings.Join(parts, ", ")
}

func reasoningEffort(c chatRequest) string {
	if c.ReasoningEffort != "" {
		return c.ReasoningEffort
	}
	if c.Reasoning != nil && c.Reasoning.Effort != "" {
		return c.Reasoning.Effort
	}
	if c.Provider != nil && c.Provider.ReasoningEffort != "" {
		return c.Provider.ReasoningEffort
	}
	return ""
}

// toolNamesJSON returns the function names in an OpenAI/Anthropic tools array.
func toolNamesJSON(tools []json.RawMessage) []string {
	var names []string
	for _, raw := range tools {
		var t struct {
			Name     string `json:"name"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if json.Unmarshal(raw, &t) == nil {
			if t.Function.Name != "" {
				names = append(names, t.Function.Name)
			} else if t.Name != "" {
				names = append(names, t.Name)
			}
		}
	}
	return names
}

func joinRaw(parts []json.RawMessage) []byte {
	out := []byte("[")
	for i, p := range parts {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, p...)
	}
	return append(out, ']')
}

func countErrors(calls []stubCall) int {
	n := 0
	for _, c := range calls {
		if c.IsError {
			n++
		}
	}
	return n
}

// editTaskResidual lists, per edit_task call, the padding signals the ruled
// residual is about, read from the arguments the model actually sent: a
// nullable enum sent as null (absence), `repo: ""` (the CLEAR) beside `repo:
// null` (absence), and the two flags whose padded `false` is their CLEAR
// spelling. What the call then CHANGED is the table's result column.
func editTaskResidual(calls []stubCall) []string {
	var out []string
	for _, c := range calls {
		if c.Name != "edit_task" {
			continue
		}
		var args map[string]any
		if json.Unmarshal(c.Arguments, &args) != nil {
			continue
		}
		var signals []string
		if v, ok := args["repo"]; ok {
			switch {
			case v == nil:
				signals = append(signals, "`repo=null` (absent)")
			case v == "":
				signals = append(signals, "`repo=\"\"` (CLEARS to primary)")
			}
		}
		for _, field := range []string{"container", "humanOnly"} {
			v, ok := args[field]
			if !ok {
				continue
			}
			switch {
			case v == nil:
				signals = append(signals, "`"+field+"=null` (leaves the flag)")
			case v == false:
				signals = append(signals, "`"+field+"=false` (CLEARS a set flag)")
			}
		}
		for _, field := range []string{"priority", "schedule"} {
			if v, ok := args[field]; ok && v == nil {
				signals = append(signals, "`"+field+"=null` (absent)")
			}
		}
		if len(signals) == 0 {
			continue
		}
		out = append(out, fmt.Sprintf("call %d: %s", c.Seq, strings.Join(signals, ", ")))
	}
	return out
}

// replayStats counts the assistant messages in requests after the first that
// carried a non-empty reasoning_content, and the total assistant messages in
// those requests.
func replayStats(requests []capturedRequest) (replayed, assistants int) {
	for i, req := range requests {
		if i == 0 {
			continue
		}
		var chat chatRequest
		if json.Unmarshal(req.Body, &chat) != nil {
			continue
		}
		for _, raw := range chat.Messages {
			var m map[string]json.RawMessage
			if json.Unmarshal(raw, &m) != nil {
				continue
			}
			if string(m["role"]) != `"assistant"` {
				continue
			}
			assistants++
			var s string
			if rc, ok := m["reasoning_content"]; ok && json.Unmarshal(rc, &s) == nil && s != "" {
				replayed++
			}
		}
	}
	return replayed, assistants
}
