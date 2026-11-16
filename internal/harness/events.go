package harness

import (
	"encoding/json"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/events"
)

// The per-harness normalizers for the agent event channel's activity tier
// (CLA-603): tool_call, todo_change and clankerbar_mcp_call. tool_result and
// assistant_text are the transcript follow-up's. codex and copilot are not
// mapped in v1 — their Capabilities report EmitsEvents false — and the design
// table is docs/agent-events.md.
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

// noteClaudeToolEvent maps one claude `assistant`/tool_use block onto the
// activity tier. The tool name is kept as claude reports it (Bash, Read, ...);
// only the clankerbar namespace is normalized, exactly as the mapping table
// says.
func noteClaudeToolEvent(sink events.Sink, name, callID string, input json.RawMessage) {
	if sink == nil {
		return
	}
	switch {
	case name == "TodoWrite":
		if todos, ok := todoItemsFromInput(input); ok {
			sink.TodoChange(todos)
		}
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
// a tool carrying none of them sends no target. That is deliberate: a status
// line is better with an empty target than with a dump of an arbitrary input
// object, which is exactly what the "never the raw input JSON" rule forbids.
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
