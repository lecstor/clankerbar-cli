package harness

import (
	"encoding/json"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/events"
)

// The per-harness normalizers for the agent event channel (CLA-603, transcript
// tier CLA-887): tool_call, todo_change, clankerbar_mcp_call, tool_result and
// assistant_text. codex and copilot are not mapped in v1 — their Capabilities
// report EmitsEvents false — and the design table is docs/agent-events.md.
//
// The one shared rule: a target is ONE extracted string per tool shape, never
// the raw input JSON.

// closeEvents flushes and stops this session's agent-event sink. Nil-safe and
// idempotent; the adapters call it on every path that built a Result.
func (r *Result) closeEvents() {
	if r == nil || r.events == nil {
		return
	}
	r.events.Close()
	r.events = nil
}

// NoteClaudeToolEvent maps one claude tool use onto the activity tier. The two
// callers see the same shape: the driver's `assistant`/tool_use stream block,
// and an interactive session's PostToolUse hook payload (CLA-605). The tool name
// is kept as claude reports it (Bash, Read, ...); only the clankerbar namespace
// is normalized, exactly as the mapping table says.
func NoteClaudeToolEvent(sink events.Sink, name, callID string, input json.RawMessage) {
	if sink == nil {
		return
	}
	switch {
	case name == "TodoWrite":
		if todos, ok := todoItemsFromInput(input); ok {
			sink.TodoChange(todos)
		}
	case name == "TaskCreate" || name == "TaskUpdate":
		// Per-item todo tools. The activity tier carries the whole-list snapshot
		// (TodoWrite), and the design note drops these until a harness change
		// makes them primary (CLA-601) — sending one as a tool_call would put a
		// different fact on the wire than the list.
	case strings.HasPrefix(name, clankerbarToolPrefix):
		sink.ClankerbarMcpCall(strings.TrimPrefix(name, clankerbarToolPrefix), callID, clankerbarTarget(input))
	case name != "":
		sink.ToolCall(name, callID, toolTarget(name, input))
	}
}

// noteOpencodeToolEvent maps one terminal opencode tool part onto the activity
// tier. `todowrite` is the todo list tool; `clankerbar_<tool>` is an MCP call
// whose namespace is normalized to the bare name; everything else is an
// ordinary tool call under opencode's own spelling.
func noteOpencodeToolEvent(sink events.Sink, tool, callID string, input json.RawMessage) {
	if sink == nil || tool == "" {
		return
	}
	switch {
	case strings.EqualFold(tool, "todowrite"):
		if todos, ok := todoItemsFromInput(input); ok {
			sink.TodoChange(todos)
		}
	case strings.HasPrefix(tool, "clankerbar_"):
		sink.ClankerbarMcpCall(strings.TrimPrefix(tool, "clankerbar_"), callID, clankerbarTarget(input))
	default:
		sink.ToolCall(tool, callID, toolTarget(tool, input))
	}
}

// noteClaudeAssistantText maps one claude `assistant` text block onto the
// transcript tier. A whitespace-only block is NOT an event: it is the stream's
// framing, not something the model said.
func noteClaudeAssistantText(sink events.Sink, text string) {
	if sink == nil || strings.TrimSpace(text) == "" {
		return
	}
	sink.AssistantText(text)
}

// noteOpencodeAssistantText is the opencode half: one `text` part is one block.
func noteOpencodeAssistantText(sink events.Sink, text string) {
	if sink == nil || strings.TrimSpace(text) == "" {
		return
	}
	sink.AssistantText(text)
}

// noteOpencodeToolResult maps one TERMINAL opencode tool part onto the
// transcript tier. opencode delivers the arguments and the result together, so
// this needs no correlation state. output is `state.output`, or the error text
// when the harness marked the call failed; a `.env*` read's output is withheld
// (see envReadTarget).
func noteOpencodeToolResult(sink events.Sink, tool, callID string, input json.RawMessage, ok bool, output string) {
	if sink == nil || !sink.TranscriptEnabled() || tool == "" || callID == "" {
		return
	}
	sink.ToolResult(callID, ok, output, envReadTarget(tool, toolTarget(tool, input)))
}

// noteClaudeToolResult maps one claude `user`/tool_result block onto the
// transcript tier. The stream reports a result on a LATER event than the call,
// which is why the `.env*` decision (drop) is taken from the call id at
// tool_use time (Result.noteEnvRead).
//
// A result Claude Code spilled to a file is rehydrated before sending — the
// model saw the output, not the pointer, and the transcript is a record of the
// session. The path is honoured under persistedOutputPath's own guard; if the
// read fails, the envelope text is what we have and is sent.
func noteClaudeToolResult(sink events.Sink, toolUseID string, isError bool, content json.RawMessage, drop bool) {
	if sink == nil || !sink.TranscriptEnabled() || toolUseID == "" {
		return
	}
	text := toolResultText(content)
	if !drop {
		if path, ok := persistedOutputPath(text, toolUseID); ok {
			if b, err := readSpilled(path); err == nil {
				text = toolResultText(json.RawMessage(b))
			}
		}
	}
	sink.ToolResult(toolUseID, !isError, text, drop)
}

// noteEnvRead records that a claude call read a `.env*` path, so the transcript
// tier withholds its output when the result arrives. Only remembered while the
// tier is on: it is transcript-only state, and the map is dropped entry-by-entry
// as results land.
func (r *Result) noteEnvRead(tool, callID string, input json.RawMessage) {
	if r == nil || r.events == nil || callID == "" || !r.events.TranscriptEnabled() {
		return
	}
	if !envReadTarget(tool, toolTarget(tool, input)) {
		return
	}
	if r.envReadCalls == nil {
		r.envReadCalls = map[string]bool{}
	}
	r.envReadCalls[callID] = true
}

// takeEnvRead returns and forgets the `.env*` verdict for a call.
func (r *Result) takeEnvRead(callID string) bool {
	drop := r.envReadCalls[callID]
	delete(r.envReadCalls, callID)
	return drop
}

// envReadTarget reports whether a call is a READ of a `.env*` path, whose output
// the design note says must be dropped entirely. The family is deliberately
// limited to read-shaped tools: the verdict is taken from the call's extracted
// target (a path), not from parsing a shell command, and a Bash `cat .env` is
// the activity dial's residue, not something this rule can see.
func envReadTarget(tool, target string) bool {
	if !strings.EqualFold(tool, "read") {
		return false
	}
	base := strings.TrimSpace(target)
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	return strings.HasPrefix(base, ".env")
}

// todoItemsFromInput reads the whole-list snapshot from a todo tool's input.
// The design drops per-harness decoration (activeForm and friends): one string
// per item is what the status line reads. An absent or unparseable list yields
// no event rather than an empty snapshot — an empty list is a real state the
// harness reports, a missing field is not.
func todoItemsFromInput(input json.RawMessage) ([]events.TodoItem, bool) {
	var args struct {
		Todos []struct {
			Content string `json:"content"`
			Status  string `json:"status"`
		} `json:"todos"`
	}
	if json.Unmarshal(input, &args) != nil || args.Todos == nil {
		return nil, false
	}
	out := make([]events.TodoItem, 0, len(args.Todos))
	for _, t := range args.Todos {
		out = append(out, events.TodoItem{Content: t.Content, Status: t.Status})
	}
	return out, true
}

// clankerbarTarget is the call's single most identifying argument: the task id
// for task tools, the question id for questions, absent when the tool has
// neither (heartbeat, list_tasks, record_decision). The full input is never
// sent in either tier.
func clankerbarTarget(input json.RawMessage) string {
	var args map[string]json.RawMessage
	if json.Unmarshal(input, &args) != nil {
		return ""
	}
	for _, key := range []string{"taskId", "questionId"} {
		var s string
		if raw, ok := args[key]; ok && json.Unmarshal(raw, &s) == nil && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// toolTarget extracts the one identifying string from a tool call's input, by
// tool shape. Both harnesses' spellings of the same shape are listed together:
// claude's Read/Edit take `file_path`, opencode's read/edit take `filePath` (and
// some versions `path`), and a shell tool takes `command` on both.
//
// Unknown tools fall back to the first recognisable among the common keys, and
// a tool carrying none of them sends an EMPTY target (the wire key is required
// and always present, not omitted). That is deliberate: a status line is better
// with an empty target than with a dump of an arbitrary input object, which is
// exactly what the "never the raw input JSON" rule forbids.
func toolTarget(tool string, input json.RawMessage) string {
	var args map[string]json.RawMessage
	if json.Unmarshal(input, &args) != nil {
		return ""
	}
	for _, key := range targetKeys(tool) {
		var s string
		if raw, ok := args[key]; ok && json.Unmarshal(raw, &s) == nil && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// targetKeys is the per-shape priority list. Command first for exec-shaped
// tools; the path family first for file-shaped ones. Lower-case matching makes
// the two dialects' capitalisation (Bash vs bash) irrelevant.
func targetKeys(tool string) []string {
	switch strings.ToLower(tool) {
	case "bash", "shell", "exec", "command":
		return []string{"command"}
	case "read", "write", "edit", "multiedit", "notebookread", "notebookedit", "patch", "apply_patch":
		return []string{"file_path", "filePath", "notebook_path", "notebookPath", "path"}
	case "glob", "grep", "ls", "list", "search":
		return []string{"path", "pattern", "query"}
	case "webfetch", "fetch":
		return []string{"url"}
	case "websearch":
		return []string{"query"}
	case "task", "agent":
		return []string{"description", "prompt"}
	}
	return []string{"command", "file_path", "filePath", "notebook_path", "notebookPath", "path", "url", "query", "pattern", "description"}
}
